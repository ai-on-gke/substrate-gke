// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sync"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// fakeGCP is an in-process GKE ClusterManager and Resource Manager Projects
// server. It records the mutating calls it receives so tests can assert what
// setup-gcp did to a project, without touching GCP.
type fakeGCP struct {
	containerpb.UnimplementedClusterManagerServer
	resourcemanagerpb.UnimplementedProjectsServer

	mu sync.Mutex
	// cluster is the cluster GetCluster returns; nil means NotFound.
	cluster *containerpb.Cluster
	// clusterCalls lists the mutating ClusterManager RPCs, in order.
	clusterCalls []string

	// policy is the project's stored IAM policy, conditions included.
	policy *iampb.Policy
	// requestedVersions records each GetIamPolicy's requested policy version.
	requestedVersions []int32
	// written records each policy passed to SetIamPolicy.
	written []*iampb.Policy
}

// newFakeGCP starts the fake and points clientOptions at it for the test.
func newFakeGCP(t *testing.T) *fakeGCP {
	t.Helper()
	f := &fakeGCP{policy: &iampb.Policy{Etag: []byte("etag-1")}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	containerpb.RegisterClusterManagerServer(srv, f)
	resourcemanagerpb.RegisterProjectsServer(srv, f)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	orig := clientOptions
	clientOptions = []option.ClientOption{
		option.WithEndpoint("passthrough:///bufnet"),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
	t.Cleanup(func() { clientOptions = orig })
	return f
}

func (f *fakeGCP) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clusterCalls = append(f.clusterCalls, call)
}

func (f *fakeGCP) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.clusterCalls...)
}

func doneOp(name string) *containerpb.Operation {
	return &containerpb.Operation{Name: name, Status: containerpb.Operation_DONE}
}

func (f *fakeGCP) GetCluster(_ context.Context, _ *containerpb.GetClusterRequest) (*containerpb.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cluster == nil {
		return nil, status.Error(codes.NotFound, "cluster not found")
	}
	return proto.Clone(f.cluster).(*containerpb.Cluster), nil
}

func (f *fakeGCP) DeleteCluster(_ context.Context, _ *containerpb.DeleteClusterRequest) (*containerpb.Operation, error) {
	f.record("DeleteCluster")
	f.mu.Lock()
	f.cluster = nil
	f.mu.Unlock()
	return doneOp("op-delete"), nil
}

func (f *fakeGCP) CreateCluster(_ context.Context, req *containerpb.CreateClusterRequest) (*containerpb.Operation, error) {
	f.record("CreateCluster")
	f.mu.Lock()
	f.cluster = proto.Clone(req.GetCluster()).(*containerpb.Cluster)
	f.mu.Unlock()
	return doneOp("op-create"), nil
}

func (f *fakeGCP) UpdateCluster(_ context.Context, _ *containerpb.UpdateClusterRequest) (*containerpb.Operation, error) {
	f.record("UpdateCluster")
	return doneOp("op-update"), nil
}

func (f *fakeGCP) GetOperation(_ context.Context, req *containerpb.GetOperationRequest) (*containerpb.Operation, error) {
	return doneOp(req.GetName()), nil
}

// GetIamPolicy follows IAM's documented versioning
// (https://cloud.google.com/iam/docs/policies#versions): a request for
// version 3 gets the policy with its conditions. A request for a lower
// version, or none, gets a version 1 view in which each conditional binding's
// role name has "_withcond_<hash>" appended and the condition removed.
func (f *fakeGCP) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	version := req.GetOptions().GetRequestedPolicyVersion()
	f.requestedVersions = append(f.requestedVersions, version)

	p := proto.Clone(f.policy).(*iampb.Policy)
	hasConditions := false
	for _, b := range p.Bindings {
		if b.Condition != nil {
			hasConditions = true
		}
	}
	switch {
	case !hasConditions:
		p.Version = 1
	case version >= 3:
		p.Version = 3
	default:
		p.Version = 1
		for _, b := range p.Bindings {
			if b.Condition != nil {
				sum := sha256.Sum256([]byte(b.Condition.GetExpression()))
				b.Role += "_withcond_" + hex.EncodeToString(sum[:10])
				b.Condition = nil
			}
		}
	}
	return p, nil
}

func (f *fakeGCP) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := proto.Clone(req.GetPolicy()).(*iampb.Policy)
	f.written = append(f.written, p)
	f.policy = proto.Clone(p).(*iampb.Policy)
	return p, nil
}
