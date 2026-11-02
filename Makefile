# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

.PHONY: run run-demo doctor dry-run teardown build test fmt verify screenshots

# Launch the interactive installer. It asks for the SPIFFE IDs of the clients
# the ingress router admits, and deploys no demo.
run:
	cd installer && go run .

# Launch the installer for a demo: it ends by deploying the counter demo, and
# allows the demo's driver pods on the ingress router on your behalf.
run-demo:
	cd installer && go run . --demo

# Delete the GCP resources a previous install created. The values are your
# wizard answers; the installer's exit summary prints this exact invocation.
teardown:
	./tools/cleanup-gcp --project "$(PROJECT_ID)" --cluster "$(CLUSTER_NAME)" --location "$(CLUSTER_LOCATION)" --bucket "$(BUCKET_NAME)" \
		$(if $(and $(filter command line,$(origin ARTIFACT_REGISTRY_REPOSITORY)),$(ARTIFACT_REGISTRY_REPOSITORY)),--repository "$(ARTIFACT_REGISTRY_REPOSITORY)") \
		$(if $(filter true,$(DELETE_REPOSITORY)),--delete-repository) \
		$(if $(filter true,$(KEEP_REPOSITORY)),--keep-repository) \
		$(if $(filter true,$(CLUSTER_SCOPED)),--cluster-scoped)

# Run the preflight checks only.
doctor:
	cd installer && go run . --doctor

# Walk the full wizard without touching GCP.
dry-run:
	cd installer && go run . --dry-run

build:
	cd installer && go build -o ../bin/substrate-gke-installer .

# Go modules in this repository. tools/setup-gcp is its own module so the
# installer does not pull in the GCP client libraries.
MODULES := installer tools/setup-gcp

test:
	for m in $(MODULES); do (cd $$m && go test ./...) || exit 1; done

fmt:
	for m in $(MODULES); do (cd $$m && gofmt -w .) || exit 1; done

verify:
	for m in $(MODULES); do (cd $$m && test -z "$$(gofmt -l .)" && go vet ./...) || exit 1; done

# Regenerate the README screenshots from the dry-run wizard. Needs freeze:
# go install github.com/charmbracelet/freeze@latest
screenshots:
	cd installer && go run ./cmd/uishots ../docs/screenshots
	@# Feed the capture on stdin rather than via `--execute "cat $$f"`. The
	@# --execute form runs the command under a pty and races its reader against
	@# it: it fails intermittently with "XML syntax error: invalid UTF-8" on a
	@# multi-byte glyph split across reads, sometimes hangs outright, and has
	@# been seen to report WROTE after emitting a vertically truncated frame.
	@# Redirecting the file is deterministic and produces byte-identical SVGs.
	for f in docs/screenshots/*.ans; do \
		freeze --window -o "$${f%.ans}.svg" < "$$f" && rm "$$f"; \
	done

PIN_FILE := installer/internal/snapshot/snapshot.go
# `\t` is a BSD grep extension that GNU grep does not honour, so match on
# leading whitespace instead — this has to work on Linux too.
pin = $(shell grep -E '^[[:space:]]+$(1) = ' $(PIN_FILE) | sed -E 's/.*"(.*)".*/\1/')

# The pinned upstream revision the installer builds from. To move to a newer
# Substrate, edit Commit (and MinGoVersion) in snapshot.go — the tree is
# fetched on demand, not vendored here.
.PHONY: substrate-pin
substrate-pin:
	@grep -E '^[[:space:]]+(RepoURL|Commit|MinGoVersion|ReleaseRepo|ReleaseVersion) ' $(PIN_FILE)

# MinGoVersion is a hand-maintained mirror of upstream's go.mod at Commit, and
# nothing else notices when the two drift — a stale value makes the doctor pass
# and the install then die mid-bootstrap. ReleaseVersion is the same kind of
# mirror of the tag naming Commit: pre-built installs pull its images and read
# the manifests from Commit. Run this after bumping the pin.
.PHONY: substrate-pin-check
substrate-pin-check:
	@commit='$(call pin,Commit)'; pinned='$(call pin,MinGoVersion)'; \
	upstream=$$(curl -fsSL "https://raw.githubusercontent.com/agent-substrate/substrate/$$commit/go.mod" \
		| awk '$$1 == "go" { print $$2; exit }'); \
	if [ -z "$$upstream" ]; then \
		echo "substrate-pin-check: could not read go.mod at $$commit" >&2; exit 1; \
	elif [ "$$pinned" != "$$upstream" ]; then \
		echo "substrate-pin-check: MinGoVersion is $$pinned but substrate@$$commit needs go $$upstream" >&2; exit 1; \
	fi; \
	echo "substrate-pin-check: MinGoVersion $$pinned matches substrate@$$commit"; \
	release='$(call pin,ReleaseVersion)'; \
	refs=$$(git ls-remote --tags https://github.com/agent-substrate/substrate.git "refs/tags/$$release" "refs/tags/$$release^{}"); \
	tagged=$$(printf '%s\n' "$$refs" | awk '$$2 ~ /\^\{\}$$/ { print $$1; found=1; exit } { first = first ? first : $$1 } END { if (!found) print first }'); \
	if [ -z "$$tagged" ]; then \
		echo "substrate-pin-check: no tag $$release in agent-substrate/substrate" >&2; exit 1; \
	elif [ "$$tagged" != "$$commit" ]; then \
		echo "substrate-pin-check: ReleaseVersion $$release names $$tagged, not Commit $$commit" >&2; exit 1; \
	fi; \
	echo "substrate-pin-check: ReleaseVersion $$release names substrate@$$commit"
