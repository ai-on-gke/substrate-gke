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
	"io/fs"
	"testing"

	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ai-on-gke/substrate-gke/tools/setup-gcp/dashboards"
)

// TestEmbeddedDashboards checks that every file in dashboardFiles is embedded
// and parses as a dashboard, since bootstrap reads them from the binary by
// default.
func TestEmbeddedDashboards(t *testing.T) {
	for _, file := range dashboardFiles {
		data, err := fs.ReadFile(dashboards.FS, file)
		if err != nil {
			t.Fatalf("read embedded %s: %v", file, err)
		}
		d := &dashboardpb.Dashboard{}
		if err := protojson.Unmarshal(data, d); err != nil {
			t.Fatalf("parse embedded %s: %v", file, err)
		}
		if d.GetDisplayName() == "" {
			t.Errorf("embedded %s has no displayName", file)
		}
	}
}
