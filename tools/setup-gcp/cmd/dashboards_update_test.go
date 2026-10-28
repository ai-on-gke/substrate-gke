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
	"fmt"
	"io/fs"
	"net"
	"sync"
	"testing"

	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ai-on-gke/substrate-gke/tools/setup-gcp/dashboards"
)

// fakeDashboards is an in-process Cloud Monitoring DashboardsService. Like
// the real one, UpdateDashboard fails with Aborted unless the request carries
// the dashboard's current etag, and every write changes the etag.
type fakeDashboards struct {
	dashboardpb.UnimplementedDashboardsServiceServer

	mu         sync.Mutex
	dashboards map[string]*dashboardpb.Dashboard // by name
	order      []string
	version    int
	// listEtag, when set, is the etag ListDashboards reports instead of the
	// current one, as when its result is stale.
	listEtag string
	// bumpBeforeUpdate counts updates before which something else changes
	// the dashboard, so the etag the caller just read is out of date.
	bumpBeforeUpdate int
	updates          []string
}

func newFakeDashboards(t *testing.T) *fakeDashboards {
	t.Helper()
	f := &fakeDashboards{dashboards: map[string]*dashboardpb.Dashboard{}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	dashboardpb.RegisterDashboardsServiceServer(srv, f)
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

// add stores a dashboard called name with the given displayName.
func (f *fakeDashboards) add(name, displayName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	f.dashboards[name] = &dashboardpb.Dashboard{Name: name, DisplayName: displayName, Etag: fmt.Sprint(f.version)}
	f.order = append(f.order, name)
}

func (f *fakeDashboards) ListDashboards(context.Context, *dashboardpb.ListDashboardsRequest) (*dashboardpb.ListDashboardsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &dashboardpb.ListDashboardsResponse{}
	for _, name := range f.order {
		d := proto.Clone(f.dashboards[name]).(*dashboardpb.Dashboard)
		if f.listEtag != "" {
			d.Etag = f.listEtag
		}
		resp.Dashboards = append(resp.Dashboards, d)
	}
	return resp, nil
}

func (f *fakeDashboards) GetDashboard(_ context.Context, req *dashboardpb.GetDashboardRequest) (*dashboardpb.Dashboard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.dashboards[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such dashboard")
	}
	return proto.Clone(d).(*dashboardpb.Dashboard), nil
}

func (f *fakeDashboards) UpdateDashboard(_ context.Context, req *dashboardpb.UpdateDashboardRequest) (*dashboardpb.Dashboard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := req.GetDashboard()
	cur, ok := f.dashboards[d.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such dashboard")
	}
	if f.bumpBeforeUpdate > 0 {
		f.bumpBeforeUpdate--
		f.version++
		cur.Etag = fmt.Sprint(f.version)
	}
	if d.GetEtag() != cur.GetEtag() {
		return nil, status.Error(codes.Aborted, "The supplied etag is not up to date.")
	}
	f.version++
	stored := proto.Clone(d).(*dashboardpb.Dashboard)
	stored.Etag = fmt.Sprint(f.version)
	f.dashboards[d.GetName()] = stored
	f.updates = append(f.updates, d.GetName())
	return proto.Clone(stored).(*dashboardpb.Dashboard), nil
}

func (f *fakeDashboards) CreateDashboard(_ context.Context, req *dashboardpb.CreateDashboardRequest) (*dashboardpb.Dashboard, error) {
	f.mu.Lock()
	name := fmt.Sprintf("projects/p/dashboards/created-%d", len(f.order))
	f.mu.Unlock()
	f.add(name, req.GetDashboard().GetDisplayName())
	return f.GetDashboard(context.Background(), &dashboardpb.GetDashboardRequest{Name: name})
}

// displayNames returns the displayName of each embedded dashboard.
func displayNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, file := range dashboardFiles {
		data, err := fs.ReadFile(dashboards.FS, file)
		if err != nil {
			t.Fatal(err)
		}
		d := &dashboardpb.Dashboard{}
		if err := protojson.Unmarshal(data, d); err != nil {
			t.Fatal(err)
		}
		names = append(names, d.GetDisplayName())
	}
	return names
}

// ai-on-gke/substrate-gke#69: an out-of-date etag from ListDashboards, or a
// change between reading the etag and updating, must not abort bootstrap.
func TestCreateMonitoringDashboardsRefreshesTheEtag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		listEtag string
		bumps    int
	}{
		{"stale list etag", "stale", 0},
		{"changed before the update", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDashboards(t)
			for i, name := range displayNames(t) {
				f.add(fmt.Sprintf("projects/p/dashboards/%d", i), name)
			}
			f.listEtag, f.bumpBeforeUpdate = tc.listEtag, tc.bumps
			if err := createMonitoringDashboards(t.Context(), &Config{ProjectID: "p"}); err != nil {
				t.Fatalf("createMonitoringDashboards: %v", err)
			}
			if len(f.updates) != len(dashboardFiles) || len(f.order) != len(dashboardFiles) {
				t.Errorf("updates = %v, dashboards = %v; want each existing dashboard updated and none created", f.updates, f.order)
			}
		})
	}
}

// An update that keeps losing the race gives up with the error rather than
// retrying forever.
func TestCreateMonitoringDashboardsGivesUpOnRepeatedAborts(t *testing.T) {
	f := newFakeDashboards(t)
	f.add("projects/p/dashboards/0", displayNames(t)[0])
	f.bumpBeforeUpdate = dashboardUpdateAttempts
	err := createMonitoringDashboards(t.Context(), &Config{ProjectID: "p"})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("createMonitoringDashboards = %v, want Aborted after %d attempts", err, dashboardUpdateAttempts)
	}
}

// Dashboards that share a displayName are all updated, not just the last one
// ListDashboards returned.
func TestCreateMonitoringDashboardsUpdatesDuplicates(t *testing.T) {
	f := newFakeDashboards(t)
	name := displayNames(t)[0]
	f.add("projects/p/dashboards/a", name)
	f.add("projects/p/dashboards/b", name)
	if err := createMonitoringDashboards(t.Context(), &Config{ProjectID: "p"}); err != nil {
		t.Fatal(err)
	}
	updated := map[string]bool{}
	for _, u := range f.updates {
		updated[u] = true
	}
	if !updated["projects/p/dashboards/a"] || !updated["projects/p/dashboards/b"] {
		t.Errorf("updates = %v, want both dashboards named %q", f.updates, name)
	}
}
