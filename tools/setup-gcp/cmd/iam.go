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
	"errors"
	"log/slog"
	"slices"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"github.com/spf13/cobra"
)

// conditionalPolicyVersion is the IAM policy version that represents
// conditional role bindings. Reading a policy at a lower version renames each
// conditional role to "<role>_withcond_<hash>" and drops its condition, so a
// read-modify-write at that version would destroy those bindings
// (https://cloud.google.com/iam/docs/policies#versions).
const conditionalPolicyVersion = 3

// projectPolicyRequest reads a project's IAM policy at
// conditionalPolicyVersion.
func projectPolicyRequest(resource string) *iampb.GetIamPolicyRequest {
	return &iampb.GetIamPolicyRequest{
		Resource: resource,
		Options:  &iampb.GetPolicyOptions{RequestedPolicyVersion: conditionalPolicyVersion},
	}
}

// addBinding grants role to member without a condition, in a project or
// bucket policy's bindings, and reports whether it changed them. Conditional
// bindings are left alone and do not count as a grant: their condition may
// not hold for what Substrate accesses, and adding a member to one would
// subject that member to it.
func addBinding(bindings *[]*iampb.Binding, role, member string) bool {
	for _, b := range *bindings {
		if b.Condition != nil {
			continue
		}
		if b.Role == role {
			if slices.Contains(b.Members, member) {
				return false // Member already has this role
			}
			b.Members = append(b.Members, member)
			return true
		}
	}
	// Role not found, add a new binding
	*bindings = append(*bindings, &iampb.Binding{
		Role:    role,
		Members: []string{member},
	})
	return true
}

// grantGkeNodePermissions creates the node service account, grants it its
// project role, and lets it pull images from the image repository. Nodes get
// no project-wide image pull role.
func grantGkeNodePermissions(ctx context.Context, cfg *Config) error {
	if err := ensureNodeServiceAccount(ctx, cfg); err != nil {
		return err
	}
	return grantNodeImagePull(ctx, cfg, []string{nodeGSAEmail(cfg)})
}

// grantAteletPermissions lets atelet pull actor images. Its storage roles are
// on the buckets (see bucketGrants); atelet gets nothing on the project.
func grantAteletPermissions(ctx context.Context, cfg *Config) error {
	// atelet's principal is keyed to the project number.
	if err := resolveProject(ctx, cfg); err != nil {
		return err
	}
	warnStaleAteletProjectGrants(ctx, cfg)
	return grantAteletImagePull(ctx, cfg)
}

// staleAteletProjectRoles are the project-level roles older setup-gcp
// releases granted atelet. setup-gcp only adds bindings, so a re-run leaves
// them in place.
var staleAteletProjectRoles = []string{"roles/storage.objectAdmin", "roles/artifactregistry.reader"}

// staleAteletSnapshotBucketRoles are the snapshot bucket roles older
// setup-gcp releases granted atelet (--atelet-snapshot-access, removed).
var staleAteletSnapshotBucketRoles = []string{"roles/storage.objectAdmin", "roles/storage.bucketViewer"}

// heldRoles returns which of roles bindings grant member without a condition.
func heldRoles(bindings []*iampb.Binding, member string, roles []string) []string {
	var held []string
	for _, b := range bindings {
		if b.GetCondition() == nil && slices.Contains(roles, b.GetRole()) && slices.Contains(b.GetMembers(), member) && !slices.Contains(held, b.GetRole()) {
			held = append(held, b.GetRole())
		}
	}
	return held
}

// warnStaleAteletProjectGrants warns when atelet still holds project-level
// roles an older setup-gcp granted, so upgraders learn they keep them. It only
// reads the policy, and a failed read is not an error: the check is advisory.
func warnStaleAteletProjectGrants(ctx context.Context, cfg *Config) {
	client, err := resourcemanager.NewProjectsClient(ctx, clientOptions...)
	if err != nil {
		slog.Debug("Skipping the check for atelet's old project grants", slog.Any("err", err))
		return
	}
	defer client.Close()
	policy, err := client.GetIamPolicy(ctx, projectPolicyRequest("projects/"+cfg.ProjectID))
	if err != nil {
		slog.Debug("Skipping the check for atelet's old project grants", slog.Any("err", err))
		return
	}
	if held := heldRoles(policy.GetBindings(), workloadIdentityPrincipal(cfg, "atelet"), staleAteletProjectRoles); len(held) > 0 {
		slog.Warn("atelet still holds project-level roles an older setup-gcp granted, which Substrate v0.6.0 and later do not need. setup-gcp does not remove them; run tools/setup-gcp/teardown.sh --revoke-atelet-permissions",
			slog.String("project", cfg.ProjectID), slog.Any("roles", held))
	}
}

// warnStaleAteletSnapshotBucketGrants warns when the snapshot bucket policy
// still grants atelet the roles --atelet-snapshot-access used to add.
func warnStaleAteletSnapshotBucketGrants(cfg *Config, bindings []*iampb.Binding) {
	if held := heldRoles(bindings, workloadIdentityPrincipal(cfg, "atelet"), staleAteletSnapshotBucketRoles); len(held) > 0 {
		slog.Warn("atelet still holds snapshot bucket roles an older setup-gcp granted, which Substrate v0.6.0 and later do not need. setup-gcp does not remove them; see \"Moving to per-actor snapshot credentials\" in tools/setup-gcp/README.md for the commands",
			slog.String("bucket", cfg.BucketName), slog.Any("roles", held))
	}
}

// validateIamFlags checks every flag the command needs before any of the
// grants run. Each grant is a separate SetIamPolicy call, so a flag validated
// partway through the sequence reports a usage error only after the earlier
// grants have already been written to the project.
func validateIamFlags(ctx context.Context, cfg *Config, bucketBindings bool) error {
	if err := resolveProject(ctx, cfg); err != nil {
		return err
	}
	if bucketBindings && cfg.BucketName == "" {
		return errors.New("--bucket is required for bucket bindings")
	}
	if bucketBindings {
		return checkAssetBucketName(cfg)
	}
	return nil
}

var iamCmd = &cobra.Command{
	Use:   "iam",
	Short: "Create IAM bindings",
	RunE: func(cmd *cobra.Command, args []string) error {
		gkeNodes, _ := cmd.Flags().GetBool("gke-nodes")
		atelet, _ := cmd.Flags().GetBool("atelet")
		transferAccount, _ := cmd.Flags().GetBool("transfer-service-account")
		bucketBindings, _ := cmd.Flags().GetBool("bucket-bindings")
		warnRemovedAteletSnapshotAccessEnv()

		if err := validateIamFlags(cmd.Context(), &cfg, bucketBindings); err != nil {
			return err
		}

		if gkeNodes {
			if err := grantGkeNodePermissions(cmd.Context(), &cfg); err != nil {
				return err
			}
		}
		if atelet {
			if err := grantAteletPermissions(cmd.Context(), &cfg); err != nil {
				return err
			}
		}
		// Before the bucket bindings, which grant the account: IAM refuses a
		// binding for an account that does not exist.
		if transferAccount {
			if err := setupTransferServiceAccount(cmd.Context(), &cfg, workloadIdentityPrincipal(&cfg, "ate-api-server")); err != nil {
				return err
			}
		}
		if bucketBindings {
			if err := createIamPolicyBindings(cmd.Context(), &cfg); err != nil {
				return err
			}
		}
		return nil
	},
}

func init() {
	createCmd.AddCommand(iamCmd)
	iamCmd.Flags().StringVar(&cfg.BucketName, "bucket", getEnv("BUCKET_NAME", ""), "GCS bucket name (required for bucket bindings) [env: BUCKET_NAME]")
	iamCmd.Flags().Bool("gke-nodes", true, "Create the node service account, grant it its project role, and grant or print its image repository read access")
	addNodeGSANameFlag(iamCmd)
	iamCmd.Flags().Bool("atelet", true, "Grant or print atelet's read access to the image repositories")
	addTransferGSANameFlag(iamCmd)
	iamCmd.Flags().Bool("transfer-service-account", true, "Create the snapshot transfer service account and let ate-api-server create tokens for it")
	iamCmd.Flags().Bool("bucket-bindings", true, "Grant ate-api-server and the snapshot transfer service account access to the snapshot bucket, and atelet read access to the asset bucket")
}
