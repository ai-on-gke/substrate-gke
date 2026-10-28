// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upstreamTransferGSAName is agent-substrate/substrate's
// internal/installdefaults.TransferServiceAccountName (as of ed3f2e4f): the
// snapshot transfer service account name upstream's own copy of setup-gcp,
// hack/teardown.sh and hack/ate-dev-env.sh.example default to.
const upstreamTransferGSAName = "ate-snapshot-transfer"

// The snapshot transfer service account name is spelled out in three places
// in this repository, because tools/setup-gcp is a separate module and
// teardown.sh is shell: TransferGSAName here, setup-gcp's
// DefaultTransferGSAName, and teardown.sh's default. All three must match
// upstream's default, so a user who follows upstream's docs without setting
// SNAPSHOT_TRANSFER_GSA_NAME tears down and grants the account ate-setup is
// pointed at. If upstream changes its default, change all four constants.
func TestTransferGSANameMatchesSetupGCPAndUpstream(t *testing.T) {
	if TransferGSAName != upstreamTransferGSAName {
		t.Errorf("TransferGSAName = %q, upstream's default is %q", TransferGSAName, upstreamTransferGSAName)
	}
	for file, want := range map[string]string{
		"tools/setup-gcp/cmd/transferaccount.go": `const DefaultTransferGSAName = "` + TransferGSAName + `"`,
		"tools/setup-gcp/teardown.sh":            `${SNAPSHOT_TRANSFER_GSA_NAME:-` + TransferGSAName + `}`,
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("%s does not contain %s; the transfer service account names have drifted", file, want)
		}
	}
}
