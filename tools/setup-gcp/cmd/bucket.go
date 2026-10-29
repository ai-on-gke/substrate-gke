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
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/storage"
	"github.com/spf13/cobra"
	"google.golang.org/api/googleapi"
)

// createBuckets creates the snapshot bucket and the micro-VM asset bucket.
func createBuckets(ctx context.Context, cfg *Config) error {
	if err := createBucket(ctx, cfg, cfg.BucketName); err != nil {
		return err
	}
	return createBucket(ctx, cfg, cfg.assetBucketName())
}

func createBucket(ctx context.Context, cfg *Config, name string) error {
	if err := resolveProject(ctx, cfg); err != nil {
		return err
	}

	client, err := storage.NewClient(ctx, restClientOptions...)
	if err != nil {
		return err
	}
	defer client.Close()

	bucket := client.Bucket(name)
	slog.Info("Checking if Bucket exists", slog.String("bucket", name))
	attrs, err := bucket.Attrs(ctx)
	if err != nil {
		if !errors.Is(err, storage.ErrBucketNotExist) {
			return fmt.Errorf("getting bucket %s: %w", name, err)
		}

		slog.Info("Bucket does not exist. Creating...", slog.String("bucket", name))
		err = bucket.Create(ctx, cfg.ProjectID, &storage.BucketAttrs{
			Location: cfg.Region,
			UniformBucketLevelAccess: storage.UniformBucketLevelAccess{
				Enabled: true,
			},
		})
		if err != nil {
			return fmt.Errorf("create bucket %s: %w", name, err)
		}
		return nil
	}

	slog.Info("Bucket exists. Checking attributes...", slog.String("bucket", name))

	// Ensure the bucket belongs to the correct project.
	// GCS bucket names are globally unique, so it's possible this bucket belongs to a different project.
	projectNum := strconv.FormatUint(attrs.ProjectNumber, 10)
	if projectNum != cfg.ProjectNumber {
		return fmt.Errorf("bucket %s belongs to project number %s, but expected %s (it may be owned by another GCP project)", name, projectNum, cfg.ProjectNumber)
	}

	// Ensure the bucket is in the correct region.
	if !strings.EqualFold(attrs.Location, cfg.Region) {
		return fmt.Errorf("bucket %s is in location %s, but expected %s", name, attrs.Location, cfg.Region)
	}

	slog.Info("Bucket is in the correct project and region. Checking uniform-bucket-level-access setting...", slog.String("bucket", name))
	if !attrs.UniformBucketLevelAccess.Enabled {
		slog.Info("Updating uniform-bucket-level-access", slog.String("bucket", name))
		_, err = bucket.Update(ctx, storage.BucketAttrsToUpdate{
			UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{
				Enabled: true,
			},
		})
		if err != nil {
			return fmt.Errorf("update bucket %s ubla: %w", name, err)
		}
	} else {
		slog.Info("uniform-bucket-level-access is already correct", slog.String("bucket", name))
	}

	return nil
}

// bucketGrant is a role on a bucket for an IAM member.
type bucketGrant struct {
	bucket string
	member string
	role   string
}

// bucketGrants are the roles setup-gcp grants on the two buckets.
// ate-api-server copies snapshots for tags and deletes the ones nothing refers
// to, so it gets objectAdmin, plus bucketViewer to resolve the bucket. The
// transfer service account gets the same: the credential exchange downscopes
// its token to each actor's prefixes for every snapshot transfer. atelet reads
// micro-VM sandbox assets from the asset bucket with its own identity, and
// gets nothing on the snapshot bucket: every snapshot transfer runs on a
// per-actor token from the credential exchange (agent-substrate/substrate
// threat model T-08 and T-36).
func bucketGrants(cfg *Config) []bucketGrant {
	apiServer := workloadIdentityPrincipal(cfg, "ate-api-server")
	transfer := transferServiceAccountMember(cfg)
	return []bucketGrant{
		{bucket: cfg.BucketName, member: apiServer, role: "roles/storage.objectAdmin"},
		{bucket: cfg.BucketName, member: apiServer, role: "roles/storage.bucketViewer"},
		{bucket: cfg.BucketName, member: transfer, role: "roles/storage.objectAdmin"},
		{bucket: cfg.BucketName, member: transfer, role: "roles/storage.bucketViewer"},
		{bucket: cfg.assetBucketName(), member: workloadIdentityPrincipal(cfg, "atelet"), role: "roles/storage.objectViewer"},
	}
}

func createIamPolicyBindings(ctx context.Context, cfg *Config) error {
	client, err := storage.NewClient(ctx, restClientOptions...)
	if err != nil {
		return err
	}
	defer client.Close()

	// Every policy is read before any is written, so a missing bucket fails
	// the run before it grants anything. Version 3, so conditional bindings
	// come back intact and are written back unchanged (see
	// conditionalPolicyVersion).
	names := []string{cfg.BucketName, cfg.assetBucketName()}
	policies := make([]*iam.Policy3, len(names))
	for i, name := range names {
		policy, err := client.Bucket(name).IAM().V3().Policy(ctx)
		if gerr := (*googleapi.Error)(nil); errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
			return fmt.Errorf("bucket %s does not exist, run `create bucket` first: %w", name, err)
		}
		if err != nil {
			return fmt.Errorf("get bucket %s iam policy: %w", name, err)
		}
		policies[i] = policy
	}

	grants := bucketGrants(cfg)
	for i, name := range names {
		handle := client.Bucket(name).IAM().V3()
		policy := policies[i]
		// A service account created moments earlier can take a while to
		// propagate, and granting to it too soon fails with "does not
		// exist". The policy is re-read on each attempt, so an etag conflict
		// also resolves.
		for attempt := 1; ; attempt++ {
			if !addGrants(policy, name, grants) {
				slog.Info("IAM policy is already correct", slog.String("bucket", name))
				break
			}
			slog.Info("Setting IAM policy for bucket", slog.String("bucket", name))
			err := handle.SetPolicy(ctx, policy)
			if err == nil {
				break
			}
			if attempt >= 6 {
				return fmt.Errorf("set bucket %s iam policy: %w", name, err)
			}
			slog.Warn("Setting bucket IAM policy failed, retrying...", slog.String("bucket", name), slog.Int("attempt", attempt), slog.Any("err", err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(transferRetryDelay):
			}
			if policy, err = handle.Policy(ctx); err != nil {
				return fmt.Errorf("get bucket %s iam policy: %w", name, err)
			}
		}
	}
	return nil
}

// addGrants adds the grants on the named bucket that policy lacks, and
// reports whether it added any.
func addGrants(policy *iam.Policy3, name string, grants []bucketGrant) bool {
	changed := false
	for _, g := range grants {
		if g.bucket != name {
			continue
		}
		if !addBinding(&policy.Bindings, g.role, g.member) {
			continue
		}
		slog.Info("Adding role to member", slog.String("bucket", name), slog.String("role", g.role), slog.String("member", g.member))
		changed = true
	}
	return changed
}

func validateBucketFlags(ctx context.Context, cfg *Config) error {
	if cfg.BucketName == "" {
		return errors.New("--name is required")
	}
	if err := checkAssetBucketName(cfg); err != nil {
		return err
	}
	return resolveProject(ctx, cfg)
}

// maxBucketNameLength is the longest bucket name GCS accepts.
const maxBucketNameLength = 63

// checkAssetBucketName fails when the asset bucket's name, the snapshot
// bucket's plus a suffix, is longer than GCS allows.
func checkAssetBucketName(cfg *Config) error {
	if n := len(cfg.assetBucketName()); n > maxBucketNameLength {
		return fmt.Errorf("bucket name %q is too long: the asset bucket %q would have %d characters, and GCS allows %d", cfg.BucketName, cfg.assetBucketName(), n, maxBucketNameLength)
	}
	return nil
}

var bucketCmd = &cobra.Command{
	Use:   "bucket",
	Short: "Create the GCS snapshot bucket and the micro-VM asset bucket",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateBucketFlags(cmd.Context(), &cfg); err != nil {
			return err
		}
		return createBuckets(cmd.Context(), &cfg)
	},
}

func init() {
	createCmd.AddCommand(bucketCmd)
	bucketCmd.Flags().StringVar(&cfg.BucketName, "name", getEnv("BUCKET_NAME", ""), "Name of the GCS snapshot bucket. The asset bucket is named NAME-assets [env: BUCKET_NAME]")
}
