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

.PHONY: run doctor dry-run teardown build test fmt verify

# Launch the interactive installer.
run:
	cd installer && go run .

# Delete the GCP resources a previous install created. The values are your
# wizard answers; the installer's exit summary prints this exact invocation.
teardown:
	./tools/cleanup-gcp --project "$(PROJECT_ID)" --cluster "$(CLUSTER_NAME)" --location "$(CLUSTER_LOCATION)" --bucket "$(BUCKET_NAME)"

# Run the preflight checks only.
doctor:
	cd installer && go run . --doctor

# Walk the full wizard without touching GCP.
dry-run:
	cd installer && go run . --dry-run

build:
	cd installer && go build -o ../bin/substrate-gke-installer .

test:
	cd installer && go test ./...

fmt:
	cd installer && gofmt -w .

verify:
	cd installer && test -z "$$(gofmt -l .)" && go vet ./...

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
# and the install then die mid-bootstrap. Run this after bumping the pin.
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
	echo "substrate-pin-check: MinGoVersion $$pinned matches substrate@$$commit"
