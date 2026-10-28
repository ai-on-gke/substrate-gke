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
	"log/slog"
	"slices"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"github.com/spf13/cobra"
	iam "google.golang.org/api/iam/v1"
)

// DefaultNodeGSAName is the default name of the Google service account the
// nodes of a cluster this tool creates run as.
const DefaultNodeGSAName = "ate-gke-node"

const (
	// nodeServiceAccountRole is the least-privileged role GKE documents for
	// a node service account: logging, monitoring and node operation, and
	// nothing else in the project.
	nodeServiceAccountRole = "roles/container.defaultNodeServiceAccount"
)

// nodeAccountRetryDelay is the wait between attempts to grant a role to a
// service account that may not have propagated yet. A var only so tests can
// shorten it.
var nodeAccountRetryDelay = 10 * time.Second

// addNodeGSANameFlag registers the node service account's name on cmd.
func addNodeGSANameFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&cfg.NodeGSAName, "node-service-account-name", getEnv("NODE_GSA_NAME", DefaultNodeGSAName),
		"Name of the service account the nodes of a new cluster run as [env: NODE_GSA_NAME]")
}

func nodeGSAEmail(cfg *Config) string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", cfg.NodeGSAName, cfg.ProjectID)
}

// computeDefaultServiceAccount is the Compute Engine default service account,
// which GKE nodes run as when their pool names none.
func computeDefaultServiceAccount(cfg *Config) string {
	return cfg.ProjectNumber + "-compute@developer.gserviceaccount.com"
}

// ensureNodeServiceAccount creates the node service account unless it exists,
// and grants it nodeServiceAccountRole on the project. It replaces the Compute
// Engine default service account, which usually holds Editor on the project:
// root on a node can use whatever its service account holds.
func ensureNodeServiceAccount(ctx context.Context, cfg *Config) error {
	svc, err := iam.NewService(ctx, restClientOptions...)
	if err != nil {
		return fmt.Errorf("create iam client: %w", err)
	}
	email := nodeGSAEmail(cfg)
	resource := fmt.Sprintf("projects/%s/serviceAccounts/%s", cfg.ProjectID, email)
	_, err = svc.Projects.ServiceAccounts.Get(resource).Context(ctx).Do()
	switch {
	case err == nil:
		slog.Info("Node service account exists. Skipping create.", slog.String("gsa", email))
	case isNotFound(err):
		slog.Info("Creating node service account...", slog.String("gsa", email))
		_, err = svc.Projects.ServiceAccounts.Create("projects/"+cfg.ProjectID, &iam.CreateServiceAccountRequest{
			AccountId:      cfg.NodeGSAName,
			ServiceAccount: &iam.ServiceAccount{DisplayName: "Substrate GKE nodes"},
		}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("create service account %s: %w", email, err)
		}
	default:
		return fmt.Errorf("get service account %s: %w", email, err)
	}

	client, err := resourcemanager.NewProjectsClient(ctx, clientOptions...)
	if err != nil {
		return err
	}
	defer client.Close()
	return retryUntilPropagated(ctx, "Granting the node service account its project role", email, func() error {
		return grantNodeProjectRole(ctx, client, cfg, "serviceAccount:"+email)
	})
}

// grantNodeProjectRole grants member nodeServiceAccountRole on the project.
func grantNodeProjectRole(ctx context.Context, client *resourcemanager.ProjectsClient, cfg *Config, member string) error {
	resource := "projects/" + cfg.ProjectID
	policy, err := client.GetIamPolicy(ctx, projectPolicyRequest(resource))
	if err != nil {
		return fmt.Errorf("get project iam policy: %w", err)
	}
	if !addBinding(&policy.Bindings, nodeServiceAccountRole, member) {
		slog.Info("IAM policy already has the node service account role. Skipping update.", slog.String("project", cfg.ProjectID))
		return nil
	}
	slog.Info("Setting IAM policy (grant node service account role)...", slog.String("project", cfg.ProjectID), slog.String("member", member))
	// Version 3 keeps conditional bindings intact; the etag read with the
	// policy makes IAM reject the write if the policy changed in between.
	policy.Version = conditionalPolicyVersion
	if _, err := client.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: policy}); err != nil {
		return fmt.Errorf("set project iam policy: %w", err)
	}
	return nil
}

// retryUntilPropagated runs grant until it succeeds. A service account created
// moments earlier can take a while to propagate, and granting it a role too
// soon fails with "does not exist". grant re-reads the policy on each attempt,
// so an etag conflict also resolves.
func retryUntilPropagated(ctx context.Context, action, accounts string, grant func() error) error {
	for attempt := 1; ; attempt++ {
		err := grant()
		if err == nil {
			return nil
		}
		if attempt >= 6 {
			return err
		}
		slog.Warn(action+" failed, retrying...", slog.String("gsa", accounts), slog.Int("attempt", attempt), slog.Any("err", err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nodeAccountRetryDelay):
		}
	}
}

// clusterNodeServiceAccounts returns the service accounts the cluster's node
// pools run as, sorted and without duplicates. A pool that names none runs as
// the Compute Engine default service account. For a cluster this tool created
// that is just the node service account; a cluster the user brought may have
// others.
func clusterNodeServiceAccounts(cluster *containerpb.Cluster, cfg *Config) []string {
	var accounts []string
	for _, pool := range cluster.GetNodePools() {
		sa := pool.GetConfig().GetServiceAccount()
		if sa == "" || sa == "default" {
			sa = computeDefaultServiceAccount(cfg)
		}
		if !slices.Contains(accounts, sa) {
			accounts = append(accounts, sa)
		}
	}
	slices.Sort(accounts)
	return accounts
}

// readClusterNodeServiceAccounts reads the cluster and returns
// clusterNodeServiceAccounts for it.
func readClusterNodeServiceAccounts(ctx context.Context, cfg *Config) ([]string, error) {
	// A pool without a service account runs as the Compute Engine default,
	// whose name needs the project number.
	if err := resolveProject(ctx, cfg); err != nil {
		return nil, err
	}
	client, err := container.NewClusterManagerClient(ctx, clientOptions...)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", cfg.ProjectID, cfg.ClusterLocation, cfg.ClusterName)
	cluster, err := client.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("getting cluster: %w", err)
	}
	return clusterNodeServiceAccounts(cluster, cfg), nil
}
