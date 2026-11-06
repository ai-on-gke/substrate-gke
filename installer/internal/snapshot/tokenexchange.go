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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/ai-on-gke/substrate-gke/installer/internal/execx"
	"github.com/ai-on-gke/substrate-gke/installer/internal/gkeoverlay"
	"github.com/ai-on-gke/substrate-gke/installer/internal/state"
)

const (
	TokenExchangeRepo           = "https://github.com/agent-substrate-plugins/credprovider-token-exchange.git"
	TokenExchangeServiceAddress = "credprovider-token-exchange.ate-system.svc:50051"
	TokenExchangeSPIFFEID       = "spiffe://cluster.local/ns/ate-system/sa/credprovider-token-exchange"
	TokenExchangeDeployLine     = "Building and deploying credprovider-token-exchange..."

	tokenExchangeEndpoint = "https://sts.googleapis.com/v1/token"
	tokenExchangeScope    = "https://www.googleapis.com/auth/cloud-platform"
	tokenExchangeKo       = "go run github.com/google/ko@v0.19.1"
)

const tokenExchangeSystemInfraOverlay = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../credprovider-token-exchange/config
patches:
  - target:
      kind: Deployment
      name: credprovider-token-exchange
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: credprovider-token-exchange
      spec:
        template:
          metadata:
            labels:
              ate.dev/workloadType: ate-system-infra
          spec:
            nodeSelector:
              ate.dev/workloadType: ate-system-infra
            tolerations:
            - key: ate.dev/workloadType
              operator: Equal
              value: ate-system-infra
              effect: NoSchedule
            topologySpreadConstraints:
            - maxSkew: 1
              topologyKey: kubernetes.io/hostname
              whenUnsatisfiable: ScheduleAnyway
              labelSelector:
                matchLabels:
                  ate.dev/workloadType: ate-system-infra
              matchLabelKeys:
              - app`

type credentialProviderEntry struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
}

func unmarshalStrict(raw string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	_, err := dec.Token()
	if err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// TokenExchangeCredentialProviderJSON merges the sts token-exchange provider
// into an ATE_CREDENTIAL_PROVIDER JSON value.
func TokenExchangeCredentialProviderJSON(baseJSON string) (string, error) {
	raw := strings.TrimSpace(baseJSON)
	if raw == "" {
		raw = defaultCredentialProvider
	}
	var entries []credentialProviderEntry
	if strings.HasPrefix(raw, "[") {
		if err := unmarshalStrict(raw, &entries); err != nil || len(entries) == 0 {
			return "", fmt.Errorf("invalid ATE_CREDENTIAL_PROVIDER %q", raw)
		}
		for _, e := range entries {
			if e.Enabled != nil || e.Name == "" || (e.Name != "k8s.io" && e.Address == "") {
				return "", fmt.Errorf("invalid ATE_CREDENTIAL_PROVIDER %q", raw)
			}
		}
	} else {
		var single credentialProviderEntry
		if err := unmarshalStrict(raw, &single); err != nil {
			return "", fmt.Errorf("invalid ATE_CREDENTIAL_PROVIDER %q: %w", raw, err)
		}
		if single.Enabled != nil {
			if *single.Enabled || single.Name != "" || single.Address != "" {
				return "", fmt.Errorf("invalid ATE_CREDENTIAL_PROVIDER %q", raw)
			}
		} else if single.Name == "" || (single.Name != "k8s.io" && single.Address == "") {
			return "", fmt.Errorf("invalid ATE_CREDENTIAL_PROVIDER %q", raw)
		} else {
			entries = append(entries, single)
		}
	}

	added := credentialProviderEntry{Name: state.TokenExchangeProvider, Address: TokenExchangeServiceAddress}
	if i := slices.IndexFunc(entries, func(e credentialProviderEntry) bool { return e.Name == added.Name }); i >= 0 {
		entries[i] = added
	} else {
		entries = append(entries, added)
	}
	out, err := json.Marshal(entries)
	return string(out), err
}

// TokenExchangeExtraSPIFFEIDs appends TokenExchangeSPIFFEID to a
// comma-separated ATE_EGRESS_GATEWAY_EXTRA_SPIFFE_IDS value unless present.
func TokenExchangeExtraSPIFFEIDs(exported string) string {
	var ids []string
	for _, raw := range strings.Split(exported, ",") {
		if id := strings.TrimSpace(raw); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if !slices.Contains(ids, TokenExchangeSPIFFEID) {
		ids = append(ids, TokenExchangeSPIFFEID)
	}
	return strings.Join(ids, ",")
}

// RenderTokenExchangeConfigMap renders ConfigMap/credprovider-token-exchange-config.
func RenderTokenExchangeConfigMap(st *state.Setup) string {
	atespaces := "      atespaces: {}"
	if len(st.TokenExchangeAtespaces) > 0 {
		var rows []string
		for _, a := range st.TokenExchangeAtespaces {
			rows = append(rows, fmt.Sprintf("        %q: [%q]", a, state.TokenExchangeProfile))
		}
		atespaces = "      atespaces:\n" + strings.Join(rows, "\n")
	}
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: credprovider-token-exchange-config
  namespace: ate-system
data:
  config.yaml: |
    providerName: %q
    profiles:
    - name: %q
      tokenUrl: %q
      audience: %s
      scopes: [%q]
    policy:
%s`, state.TokenExchangeProvider, state.TokenExchangeProfile, tokenExchangeEndpoint, strconv.Quote(st.TokenExchangeAudience), tokenExchangeScope, atespaces)
}

func (b *Builder) controlPlaneEnvWithTokenExchange(st *state.Setup) []string {
	credJSON, err := TokenExchangeCredentialProviderJSON(CredentialProvider())
	if err != nil {
		credJSON, _ = TokenExchangeCredentialProviderJSON(defaultCredentialProvider)
	}
	return slices.Concat(
		b.env(st),
		[]string{
			"ATE_CREDENTIAL_PROVIDER=" + credJSON,
			"ATE_EGRESS_GATEWAY_EXTRA_SPIFFE_IDS=" + TokenExchangeExtraSPIFFEIDs(os.Getenv("ATE_EGRESS_GATEWAY_EXTRA_SPIFFE_IDS")),
		},
		ingressAllowlistEnv(st),
		dataplaneEnv(),
	)
}

// DeployTokenExchange builds and deploys credprovider-token-exchange, then
// redeploys ate-api-server and atenet with the plugin's SPIFFE ID and address.
func (b *Builder) DeployTokenExchange(st *state.Setup) execx.Spec {
	_, argv := imageArgs(st)
	koRepo := ShellQuote(st.TokenExchangeBuildRepository())

	lines := append([]string{}, kubeconfigLines(st.ProjectID, st.ClusterName, st.Zone)...)
	lines = append(lines,
		`TMP_DIR=$(mktemp -d)`,
		`trap 'rm -rf "${TMP_DIR}"' EXIT`,
		`trap 'exit 130' INT TERM`,
		`git clone --depth 1 `+TokenExchangeRepo+` "${TMP_DIR}/credprovider-token-exchange"`,
	)
	if st.Prebuilt() && b.SetupGCP != "" {
		lines = append(lines, fmt.Sprintf("KO_DOCKER_REPO=%s CREATE_ARTIFACT_REPOSITORY=%t go -C %s run . create image-pull",
			koRepo, st.TokenExchangeCreatesRepository(), ShellQuote(b.SetupGCP)))
	}
	// VERSION="" overrides b.env(st)'s Substrate version so ko's .ko.yaml stamps
	// the plugin's own revision rather than Substrate's release tag.
	lines = append(lines,
		strings.TrimSuffix(cordonLines(st), "\n"),
		`printf '%s\n' `+ShellQuote(RenderTokenExchangeConfigMap(st))+` | kubectl apply -f -`,
		`echo `+ShellQuote(TokenExchangeDeployLine),
		`KUSTOMIZE_DIR="${TMP_DIR}/credprovider-token-exchange/config"`,
		`if [ "${ATE_INSTALL_CORDON_SYSTEM_INFRA_COMPONENTS}" = true ]; then`,
		`  mkdir -p "${TMP_DIR}/overlay"`,
		`  printf '%s\n' `+ShellQuote(tokenExchangeSystemInfraOverlay)+` > "${TMP_DIR}/overlay/kustomization.yaml"`,
		`  KUSTOMIZE_DIR="${TMP_DIR}/overlay"`,
		`fi`,
		fmt.Sprintf(`(cd "${TMP_DIR}/credprovider-token-exchange" && kubectl kustomize "${KUSTOMIZE_DIR}" | KO_DOCKER_REPO=%s KO_DEFAULTPLATFORMS=%s VERSION="" %s apply --base-import-paths -f -)`,
			koRepo, targetPlatform, tokenExchangeKo),
		`kubectl rollout status deployment/credprovider-token-exchange -n ate-system --timeout=180s`,
		b.withOverlay("go run ./cmd/ate-setup deploy apiserver"+argv+"\ngo run ./cmd/ate-setup deploy atenet"+argv+placementLines(st)),
	)

	classes := []string{state.SandboxGVisor}
	if st.MicroVMActive() {
		classes = append(classes, state.SandboxMicroVM)
	}

	sim := append(b.fetchSimLines(), "Cloning into 'credprovider-token-exchange'...")
	if st.Prebuilt() {
		sim = append(sim, "Step 2/2: Granting GKE Node image pull permissions...")
	}
	cordonSim := SeparateSystemNodesLine
	if st.SharedSystemNodes {
		cordonSim = SharedSystemNodesLine
	}
	sim = append(sim,
		cordonSim,
		"configmap/credprovider-token-exchange-config configured",
		TokenExchangeDeployLine,
		"Building plugins.go.ate.dev/credprovider-token-exchange/cmd/credprovider-token-exchange for "+targetPlatform,
		`deployment "credprovider-token-exchange" successfully rolled out`,
		gkeoverlay.NotNeededLine,
		"[step]: deploy_ate_apiserver",
		`deployment "ate-api-server" successfully rolled out`,
		"[step]: deploy_atenet",
		`deployment "atenet-egress" successfully rolled out`,
	)

	return execx.Spec{
		Label:            "deploy credprovider-token-exchange",
		Display:          "deploy credprovider-token-exchange and register it with ate-api-server and atenet",
		DisplayIsSummary: true,
		Argv:             b.inTree(strings.Join(lines, "\n")),
		Env:              append(b.controlPlaneEnvWithTokenExchange(st), sandboxClassesEnv(classes...)),
		SimLines:         sim,
	}
}
