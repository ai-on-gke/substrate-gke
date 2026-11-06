# Federating actor JWTs with Google Cloud

Google Cloud Workload Identity Federation (WIF) can verify Substrate actor JWTs in two ways:
- Services you run on Google Cloud can verify actor JWTs injected with `actorJwt`.
- Actors can call Google Cloud APIs directly — such as Cloud Storage and Vertex AI — by having [`credprovider-token-exchange`](https://github.com/agent-substrate-plugins/credprovider-token-exchange) exchange an actor JWT at Google Cloud STS for a federated access token.

See upstream's [Federating actor JWTs](https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/actor-jwt-federation.md) and [Actor JWTs](https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/actor-jwts.md) for the generic issuer setup and token claims.

## 1. Make the issuer keys available to Workload Identity Federation

Choose one of two ways to give WIF the cluster's signing keys:

- **Public issuer.** Follow [Serve the issuer publicly](https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/actor-jwt-federation.md#serve-the-issuer-publicly) to expose `Service/idp` in `ate-system` behind a publicly trusted certificate and redeploy `ate-system` with `--actor-jwt-issuer=https://<domain>`.
  - On GKE Gateway API, `Service/idp` already sets `appProtocol: https` on port `443` (`name: https`); attach a `HealthCheckPolicy` probing `/healthz` over `HTTPS` (`/` returns `404`). On GKE Ingress, annotate `Service/idp` with `cloud.google.com/app-protocols: '{"https":"HTTPS"}'` and set `requestPath: /healthz` in a `BackendConfig`.
  - Cloud Load Balancing does not validate backend certificates by default on HTTPS backends, so `Service/idp`'s `servicedns.podcert.ate.dev/identity` certificate needs no custom CA on the load balancer.
- **Private cluster (uploaded JWKS).** Keep your cluster's `--actor-jwt-issuer` (including the default `https://idp.ate-system.svc`), export `/openid/v1/jwks`, and pass `--jwk-json-path=jwks.json` when creating the OIDC provider:

  ```bash
  kubectl port-forward -n ate-system svc/idp 8443:443 &
  PF_PID=$!
  curl -sk https://127.0.0.1:8443/openid/v1/jwks > jwks.json
  kill "${PF_PID}"
  ```

  Re-upload `jwks.json` with `gcloud iam workload-identity-pools providers update-oidc --jwk-json-path=jwks.json` whenever you add or remove a signing key (`kubectl ate admin add-jwt-key` / `remove-jwt-key`).

## 2. Create the Workload Identity Pool and OIDC provider

Google Cloud WIF verifies Substrate's default `ES256` keys directly. Enable the Security Token Service API (`iam.googleapis.com` is already enabled by `setup-gcp`), then create a pool and OIDC provider:

```bash
gcloud services enable sts.googleapis.com --project="${PROJECT_ID}"

gcloud iam workload-identity-pools create substrate-actors \
  --project="${PROJECT_ID}" \
  --location=global

gcloud iam workload-identity-pools providers create-oidc substrate \
  --project="${PROJECT_ID}" \
  --location=global \
  --workload-identity-pool=substrate-actors \
  --issuer-uri="${ISSUER_URI}" \
  --attribute-condition="assertion.sub.startsWith('actor/')" \
  --attribute-mapping="google.subject=assertion.sub.extract('actor/{actor}'),attribute.atespace=assertion['actor.ate.dev'].atespace"
```

Add `--jwk-json-path=jwks.json` if you exported the key set in step 1.

- `assertion.sub.startsWith('actor/')` restricts the provider to actor tokens.
- `assertion.sub.extract('actor/{actor}')` strips the `actor/` prefix so `<atespace>/<actor>` (at most 127 bytes across two 63-byte DNS-1123 labels) fits Google Cloud's 127-byte `google.subject` limit; the full `sub` can reach 133 bytes.
- Omitting `--allowed-audiences` accepts the provider's default audience:
  `//iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/substrate-actors/providers/substrate`.

## 3. Deploy `credprovider-token-exchange`

[`credprovider-token-exchange`](https://github.com/agent-substrate-plugins/credprovider-token-exchange) mints an actor JWT through `ate-api-server`, exchanges it at `https://sts.googleapis.com/v1/token` for a federated Google Cloud access token, and caches the token per `(atespace, actor, profile)`.

1. Apply `credprovider-token-exchange-config` in `ate-system`, replacing `PROJECT_NUMBER` and listing the atespaces allowed to use the `gcp` profile:

   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: credprovider-token-exchange-config
     namespace: ate-system
   data:
     config.yaml: |
       providerName: sts
       profiles:
       - name: gcp
         tokenUrl: https://sts.googleapis.com/v1/token
         audience: //iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/substrate-actors/providers/substrate
         scopes: ["https://www.googleapis.com/auth/cloud-platform"]
       policy:
         atespaces:
           demo: ["gcp"]
   ```

   The provider reads `config.yaml` only at startup; run `kubectl -n ate-system rollout restart deployment/credprovider-token-exchange` after editing it.

2. Clone [`agent-substrate-plugins/credprovider-token-exchange`](https://github.com/agent-substrate-plugins/credprovider-token-exchange) and deploy it with [`ko`](https://ko.build):

   ```bash
   KO_DOCKER_REPO="${KO_DOCKER_REPO}" make deploy
   ```

   Ensure cluster nodes have `roles/artifactregistry.reader` on the `KO_DOCKER_REPO` repository. When system nodes are cordoned onto `ate-system-infra` (`SEPARATE_SYSTEM_NODES=true`, the default), add `nodeSelector: {ate.dev/workloadType: ate-system-infra}` and a matching `NoSchedule` toleration to `Deployment/credprovider-token-exchange`.

3. From your Substrate checkout, allow the provider's SPIFFE ID to call `Control.MintActorJwt` on `ate-api-server` and register the `sts` provider on `atenet` alongside `k8s.io`:

   ```bash
   go run ./cmd/ate-setup deploy apiserver --config <record> \
     --egress-gateway-extra-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/credprovider-token-exchange

   go run ./cmd/ate-setup deploy atenet --config <record> \
     --credential-provider='[{"name":"k8s.io"},{"name":"sts","address":"credprovider-token-exchange.ate-system.svc:50051"}]'
   ```

   `<record>` is `~/.cache/ate-setup/installs/<context>.yaml`; pass any secrets that install records omit (such as database connection strings) again, and keep any existing SPIFFE IDs or credential providers in the two flags.

## 4. Example: Call Cloud Storage and Vertex AI from an actor

1. Enable Vertex AI (`storage.googleapis.com` is already enabled by `setup-gcp`) and grant IAM roles directly to the actor's federated principal (`subject/<atespace>/<actor>`):

   ```bash
   gcloud services enable aiplatform.googleapis.com --project="${PROJECT_ID}"

   PRINCIPAL="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/substrate-actors/subject/demo/my-agent"

   gcloud storage buckets add-iam-policy-binding "gs://${BUCKET}" \
     --member="${PRINCIPAL}" \
     --role=roles/storage.objectViewer

   gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
     --member="${PRINCIPAL}" \
     --role=roles/aiplatform.user
   ```

   To grant every actor in the `demo` atespace instead, bind `principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/substrate-actors/attribute.atespace/demo`.

2. Create an `ActorTemplate` that mounts the egress gateway trust bundle (required for `https` credential injection; see [MITM Egress Interception](https://github.com/agent-substrate/substrate/blob/v1.0.0/docs/guides/egress-trust-bundle.md)), then create and resume the actor:

   ```yaml
   # template.yaml
   metadata:
     atespace: demo
     name: gcp-caller
   containers:
   - name: agent
     image: registry.example.com/my-agent@sha256:<digest>
     env:
     - name: SSL_CERT_FILE
       value: /run/ate/egress-trust-bundle/trust-bundle.pem
     volumeMounts:
     - name: system-info
       mountPath: /run/ate/egress-trust-bundle
   volumes:
   - name: system-info
     systemInfo:
       dataSources:
       - trustBundle:
           names:
           - egress-mitm.ate.dev
           - system-roots.ate.dev
           path: trust-bundle.pem
   resources:
     limits:
       cpuMillicores: 1000
       memoryBytes: 536870912
   ```

   ```bash
   kubectl ate create atespace demo
   kubectl ate create actor-template -f template.yaml
   kubectl ate create actor my-agent --template=gcp-caller -a demo
   kubectl ate resume actor my-agent -a demo
   ```

3. Attach an `EgressPolicy` that replaces the actor's `Authorization` header with a federated token from `ate-secret://sts/token-exchange/gcp`:

   ```yaml
   # policy.yaml
   rules:
   - https:
       hostnames:
       - storage.googleapis.com
       - us-central1-aiplatform.googleapis.com
       effects:
         replaceHeaders:
         - header: Authorization
           prefix: "Bearer "
           credentialUri: ate-secret://sts/token-exchange/gcp
   ```

   ```bash
   kubectl ate create egress-policy my-agent -a demo -f policy.yaml
   ```

   `EgressPolicy` wildcards only match a complete leftmost label (such as `*.googleapis.com`, never `*-aiplatform.googleapis.com`); list the exact endpoints your actor calls.

4. Inside the actor, send any placeholder `Authorization` header — the egress gateway replaces it with `Bearer <federated-access-token>` before forwarding the request:

   ```bash
   # List objects in Cloud Storage
   curl -sS -H 'Authorization: Bearer placeholder' \
     "https://storage.googleapis.com/storage/v1/b/${BUCKET}/o"

   # Generate content with Vertex AI
   curl -sS -X POST \
     -H 'Authorization: Bearer placeholder' \
     -H 'Content-Type: application/json' \
     "https://us-central1-aiplatform.googleapis.com/v1/projects/${PROJECT_ID}/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent" \
     -d '{"contents":[{"role":"user","parts":[{"text":"Hello from Substrate"}]}]}'
   ```

   Google Cloud client libraries work the same way when given static placeholder credentials (such as `oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "placeholder"})` in Go or `google.oauth2.credentials.Credentials("placeholder")` in Python) instead of Application Default Credentials, which would otherwise query the blocked GCE metadata server.
