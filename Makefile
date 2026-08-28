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

.PHONY: run doctor dry-run build test fmt verify

# Launch the interactive installer.
run:
	cd installer && go run .

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

# Refresh the vendored substrate snapshot:
#   make vendor-substrate SUBSTRATE=/path/to/substrate REF=main
SUBSTRATE ?= ../substrate
REF ?= main
.PHONY: vendor-substrate
vendor-substrate:
	hack/vendor-substrate.sh $(SUBSTRATE) $(REF)
