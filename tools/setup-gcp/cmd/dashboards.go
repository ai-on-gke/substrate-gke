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
	"log/slog"
	"os"
	"path/filepath"
	"time"

	dashboard "cloud.google.com/go/monitoring/dashboard/apiv1"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"github.com/spf13/cobra"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ai-on-gke/substrate-gke/tools/setup-gcp/dashboards"
)

var dashboardFiles = []string{
	"ate-grpc-dashboard.json",
	"ate-e2e-latency-dashboard.json",
	"ate-snapshot-dashboard.json",
}

// dashboardUpdateAttempts bounds how often an update is retried after
// Cloud Monitoring rejects its etag as out of date.
const dashboardUpdateAttempts = 3

// dashboardCallAttempts bounds how often one Monitoring call is retried after
// a transient failure (ai-on-gke/substrate-gke#115). The client gives each
// call a 30 s deadline and retries nothing but Unavailable on reads, so one
// slow call surfaced as DeadlineExceeded and failed the last step of a
// bootstrap whose other eight had succeeded.
const dashboardCallAttempts = 4

// dashboardRetryDelay is the wait before the second attempt at a Monitoring
// call; it doubles on each further attempt. A var only so tests can shorten
// it.
var dashboardRetryDelay = 2 * time.Second

// isTransientDashboardError reports whether repeating the same call may
// succeed: its deadline passed, or the service was unavailable or answered
// Unknown, the codes the generated client itself retries on reads. Aborted is
// not transient here; updateDashboard handles it by rereading the etag.
func isTransientDashboardError(err error) bool {
	s, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch s.Code() {
	case codes.DeadlineExceeded, codes.Unavailable, codes.Unknown:
		return true
	}
	return false
}

// retryDashboardCall runs call until it succeeds, fails with an error a retry
// would not fix, or dashboardCallAttempts are used up. Each attempt is its own
// client call, so each gets the client's per-call deadline. action names the
// call in the log.
func retryDashboardCall[T any](ctx context.Context, action string, call func() (T, error)) (T, error) {
	delay := dashboardRetryDelay
	for attempt := 1; ; attempt++ {
		v, err := call()
		if err == nil || !isTransientDashboardError(err) || attempt >= dashboardCallAttempts || ctx.Err() != nil {
			return v, err
		}
		slog.Warn(action+" did not complete, retrying",
			slog.String("attempt", fmt.Sprintf("%d/%d", attempt, dashboardCallAttempts)),
			slog.Any("err", err))
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// createMonitoringDashboards creates or updates each dashboard in
// dashboardFiles. It is idempotent: dashboards are matched by displayName and
// updated in place, because CreateDashboard always creates a new dashboard (so
// calling it repeatedly would produce duplicates). A project can still hold
// several dashboards with one displayName, for example from runs before this
// matching existed; each of them is updated.
func createMonitoringDashboards(ctx context.Context, cfg *Config) error {
	client, err := dashboard.NewDashboardsClient(ctx, clientOptions...)
	if err != nil {
		return fmt.Errorf("create dashboards client: %w", err)
	}
	defer client.Close()

	parent := "projects/" + cfg.ProjectID

	// Index existing dashboards by displayName to decide create vs update.
	existing, err := retryDashboardCall(ctx, "Listing dashboards", func() (map[string][]*dashboardpb.Dashboard, error) {
		existing := map[string][]*dashboardpb.Dashboard{}
		it := client.ListDashboards(ctx, &dashboardpb.ListDashboardsRequest{Parent: parent})
		for {
			d, err := it.Next()
			if err == iterator.Done {
				return existing, nil
			}
			if err != nil {
				return nil, err
			}
			existing[d.GetDisplayName()] = append(existing[d.GetDisplayName()], d)
		}
	})
	if err != nil {
		return fmt.Errorf("list dashboards: %w", err)
	}

	// An empty DashboardDir means the definitions embedded in the binary.
	var files fs.FS = dashboards.FS
	dir := "embedded"
	if cfg.DashboardDir != "" {
		files = os.DirFS(cfg.DashboardDir)
		dir = cfg.DashboardDir
	}

	for _, file := range dashboardFiles {
		path := filepath.Join(dir, file)
		data, err := fs.ReadFile(files, file)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		d := &dashboardpb.Dashboard{}
		if err := protojson.Unmarshal(data, d); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		if matches := existing[d.GetDisplayName()]; len(matches) > 0 {
			if len(matches) > 1 {
				slog.Warn("Several dashboards share this display name; updating each of them",
					slog.String("displayName", d.GetDisplayName()),
					slog.Int("count", len(matches)))
			}
			for _, cur := range matches {
				slog.Info("Updating dashboard",
					slog.String("displayName", d.GetDisplayName()),
					slog.String("name", cur.GetName()),
					slog.String("file", filepath.Base(path)))
				if err := updateDashboard(ctx, client, cur.GetName(), d); err != nil {
					return fmt.Errorf("update dashboard %q (%s): %w", d.GetDisplayName(), cur.GetName(), err)
				}
			}
		} else {
			slog.Info("Creating dashboard",
				slog.String("displayName", d.GetDisplayName()),
				slog.String("file", filepath.Base(path)))
			// A create whose deadline passed may still have landed, in
			// which case the retry leaves a duplicate; the next run
			// updates both (see above), which beats failing bootstrap.
			_, err := retryDashboardCall(ctx, "Creating dashboard "+d.GetDisplayName(), func() (*dashboardpb.Dashboard, error) {
				return client.CreateDashboard(ctx, &dashboardpb.CreateDashboardRequest{Parent: parent, Dashboard: d})
			})
			if err != nil {
				return fmt.Errorf("create dashboard %q: %w", d.GetDisplayName(), err)
			}
		}
	}
	return nil
}

// updateDashboard replaces the dashboard called name with def's content.
// The etag from ListDashboards can be out of date by the time of the update
// (ai-on-gke/substrate-gke#69), so it reads the current etag with
// GetDashboard first, and reads it again and retries when the update still
// fails with Aborted because something changed the dashboard in between.
// Each call is retried on transient failures by retryDashboardCall.
func updateDashboard(ctx context.Context, client *dashboard.DashboardsClient, name string, def *dashboardpb.Dashboard) error {
	var err error
	for range dashboardUpdateAttempts {
		var cur *dashboardpb.Dashboard
		cur, err = retryDashboardCall(ctx, "Reading dashboard "+name, func() (*dashboardpb.Dashboard, error) {
			return client.GetDashboard(ctx, &dashboardpb.GetDashboardRequest{Name: name})
		})
		if err != nil {
			return fmt.Errorf("get current etag: %w", err)
		}
		d := proto.Clone(def).(*dashboardpb.Dashboard)
		d.Name = cur.GetName()
		d.Etag = cur.GetEtag()
		_, err = retryDashboardCall(ctx, "Updating dashboard "+name, func() (*dashboardpb.Dashboard, error) {
			return client.UpdateDashboard(ctx, &dashboardpb.UpdateDashboardRequest{Dashboard: d})
		})
		if status.Code(err) != codes.Aborted {
			return err
		}
	}
	return err
}

var dashboardsCmd = &cobra.Command{
	Use:   "dashboards",
	Short: "Create Cloud Monitoring dashboards",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if err := resolveProjectID(ctx, &cfg); err != nil {
			return err
		}
		return createMonitoringDashboards(ctx, &cfg)
	},
}

func init() {
	createCmd.AddCommand(dashboardsCmd)
	dashboardsCmd.Flags().StringVar(&cfg.DashboardDir, "dir", getEnv("DASHBOARD_DIR", ""), "Directory containing dashboard JSON files; empty = the definitions built into this binary [env: DASHBOARD_DIR]")
}
