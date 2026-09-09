# Change request 01 — Support Claude on Vertex AI

Status: implemented
Surfaces: `provider/anthropic`, `docs/PROVIDERS.md`, `examples/README.md`

## The report

> Running in an environment where `CLAUDE_CODE_USE_VERTEX=1` is set, the coder
> refuses to work.

## What actually happens

`CLAUDE_CODE_USE_VERTEX=1` is the flag a Claude-on-Vertex deployment sets. Such
a deployment has **no `ANTHROPIC_API_KEY`**: it authenticates with a Google
OAuth access token minted from Application Default Credentials, exactly like
the ADC deployment `NFR-COMPAT-05` already describes for Gemini.

Two things then go wrong, in this order.

1. **Pre-flight refuses the run.** `anthropic.VendorAuth` has no `Ambient`
   detector, so `ResolveAuth` returns `CredentialNone`, and the pre-flight
   every example shares —

   ```go
   if auth.State != provider.CredentialNone { return nil }
   return fmt.Errorf("no credential for vendor %q: set one of %s", …)
   ```

   — ends the run before a request is built. This is REQ-AUTH-04's stated
   failure mode verbatim: *"a deployment using an instance role, ADC or a
   workload identity has no key the SDK can read and a transport that will
   nonetheless authenticate. Modelling credentials as a bool fails every such
   deployment at a pre-flight check that a plain key would have passed, and
   the failure names the wrong cause."* The message says the vendor is
   unconfigured. It is configured; it is configured for a deployment the
   adapter does not know exists.

2. **Nothing downstream would have worked either.** `provider/anthropic`
   hardcodes one deployment: `POST {base}/v1/messages`, the version in the
   `anthropic-version` header, the model id in the body. Vertex is a different
   host, a different path, a different verb, and moves the version into the
   body while removing the model from it. Even with a hand-supplied bearer
   token, every request would 404.

There is also a **credential-leak hazard** in the naive fix. A workstation that
has both a leftover `ANTHROPIC_API_KEY` and `CLAUDE_CODE_USE_VERTEX=1` — the
ordinary state of a machine that used to call Anthropic directly — would send
that Anthropic key as `x-api-key` to `*.aiplatform.googleapis.com`, i.e. hand a
first-party credential to a third party in a header they have no use for. A
deployment switch that silently exfiltrates the old deployment's key is worse
than the refusal it replaces.

## Goals

- `CLAUDE_CODE_USE_VERTEX=1` names a *configured* vendor, not an unconfigured
  one: pre-flight passes on the ambient state REQ-AUTH-04 already models.
- The Vertex deployment reaches the wire correctly — host, path, verb, body.
- Moving between Anthropic direct and Vertex is a **config change, not a
  provider swap** (NFR-COMPAT-05), served by the one `anthropic-messages`
  implementation, as Gemini's two deployments already are.
- An Anthropic API key is never sent to a Vertex endpoint.

## Non-goals

- **Bedrock** (`CLAUDE_CODE_USE_BEDROCK`). It is a third deployment of the same
  wire with a different signing scheme (SigV4), which is a subsystem rather
  than a path shape. Out of scope here; see *Remaining* below.
- **Minting Google credentials.** This module has zero dependencies (`go.mod`
  names none) and will not grow an `oauth2/google` one to mint an ADC token.
  Token acquisition stays where REQ-AUTH-04 puts it: with the transport. Three
  supported ways are documented below.
- **A Vertex model-id translation table.** Vertex names Claude models with a
  dated suffix (`claude-sonnet-5@20260401`). The catalog is not an allowlist
  (REQ-CAT-03), so such an id resolves by cloning the vendor default row and
  reaches the URL segment verbatim. A hardcoded mapping would rot the day a
  model ships and is exactly what REQ-CAT-03 exists to avoid.

## Design

Mirror `provider/google` — the same requirement (NFR-COMPAT-05) already has an
answer in this codebase, and a second answer to it would be the divergence.

### The deployment switch

`anthropic.Vertex{Project, Location}` carries the coordinates; `ResolveVertex`
derives it from configuration alone. The deployment is on when **any** of:

| Signal | Why it is explicit enough |
|---|---|
| `Options.VertexProject` | code said so |
| `CLAUDE_CODE_USE_VERTEX` truthy | a flag variable whose only purpose is this |
| `ANTHROPIC_VERTEX_PROJECT_ID` set | names Anthropic-on-Vertex and nothing else |
| the resolved base URL is an `aiplatform.googleapis.com` host | the host is the deployment |

This is a deliberate departure from ruling **L-7**, which forbids ambient GCP
environment from selecting Vertex for Gemini, and the difference is the reason
for the ruling rather than an exception to it. L-7's hazard is that
`GOOGLE_CLOUD_PROJECT` is set on every GCE and Cloud Run box, so honouring it
would move deployments that never asked. `CLAUDE_CODE_USE_VERTEX` and
`ANTHROPIC_VERTEX_PROJECT_ID` are set by nothing except an operator choosing
this deployment. `GOOGLE_CLOUD_PROJECT` keeps L-7's treatment here too: it can
*supply the project* once something else has selected Vertex, and it can never
*select* it. Recorded as ruling L-12.

`CLAUDE_CODE_USE_VERTEX=0` must not select it. `Env.Has` is presence, and a
flag variable's whole grammar is its value, so the check is truthiness
(`0`/`false`/`no`/`off` are off).

A selected deployment with no resolvable project is an **error naming the
missing project**, not a silent fallback to `/v1/messages` — which would be a
404 several layers down whose message mentions neither Vertex nor the project.

### Resolution order

- **Project:** `Options.VertexProject` → `ANTHROPIC_VERTEX_PROJECT_ID` →
  `GOOGLE_CLOUD_PROJECT` → `CLOUDSDK_CORE_PROJECT`.
- **Location:** `Options.VertexLocation` → `CLOUD_ML_REGION` →
  `GOOGLE_CLOUD_LOCATION` → `CLOUDSDK_COMPUTE_REGION` → the region named by a
  regional host → `global`.
- **Base URL:** `ANTHROPIC_VERTEX_BASE_URL` (when Vertex is on) →
  `ANTHROPIC_BASE_URL` / credential store → catalog row → the regional Vertex
  host for the resolved location.

A regional host already says its region, so it is read out of the host rather
than demanded twice; a mismatched pair is a 404.

### The wire delta

Four differences, and only four:

| | Anthropic direct | Vertex |
|---|---|---|
| Host | `api.anthropic.com` | `{location-}aiplatform.googleapis.com` (no prefix for `global`) |
| Path | `/v1/messages` | `/v1/projects/{p}/locations/{l}/publishers/anthropic/models/{id}:streamRawPredict` |
| Version | `anthropic-version: 2023-06-01` header | `anthropic_version: "vertex-2023-10-16"` in the body |
| Model | `model` in the body | a URL segment; the body field must be **absent** |

Everything else — the message model, tools, thinking, cache control, the SSE
event grammar, salvage, billing — is byte-identical, which is precisely why
this is one implementation and not two. `request.Model` becomes `omitzero` so
the field disappears rather than going out empty; a direct request always names
a model, so no golden moves.

### Credentials

`VendorAuth` gains an `Ambient` detector that reports the Vertex deployment,
which is the one-line fix for the reported symptom. It also gains
`ANTHROPIC_VERTEX_BASE_URL` as a `DiscoveryOnly` row: a base URL is
configuration, never a bearer token.

On the Vertex path the resolved auth is **narrowed** before it is sent:

- an `Authorization: Bearer` credential is kept — that is the access token,
  however it arrived (`ANTHROPIC_AUTH_TOKEN`, a `Credentials` store, an
  `Options.HTTPClient` that adds its own);
- an `x-api-key` credential is **dropped**, and the state falls back to
  `ambient`. An Anthropic key is not a Vertex credential and Vertex has no
  field for one; sending it would leak it to a third party for no benefit.

The three supported ways to authenticate, none of which adds a dependency:

```bash
# 1. A token in the environment.
export CLAUDE_CODE_USE_VERTEX=1 ANTHROPIC_VERTEX_PROJECT_ID=my-project CLOUD_ML_REGION=us-east5
export ANTHROPIC_AUTH_TOKEN="$(gcloud auth print-access-token)"
```

```go
// 2. An ADC-authenticating transport, owned by the application.
client, _ := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
anthropic.Provider(anthropic.Options{HTTPClient: client, VertexProject: "my-project"})

// 3. A credential store, for a long-running process (REQ-AUTH-05/06 refresh).
anthropic.Provider(anthropic.Options{Credentials: creds})
```

## Test plan

Offline, no key, no network (NFR-TEST-01), through the real request path:

- `TestSwitchingToVertexIsAConfigChangeNotAProviderSwap` — one config change
  per case, asserting the URL: direct default, `Options` project, the flag
  variable, `ANTHROPIC_VERTEX_BASE_URL`, a regional host, `global`.
- `TestTheVertexDeploymentIsDiscoveredAsAnAmbientCredential` — the reported
  bug: pre-flight must not see `CredentialNone`.
- `TestTheFlagVariableSetToZeroDoesNotSelectVertex`.
- `TestAnAnthropicAPIKeyIsNeverSentToTheVertexEndpoint` — and its converse,
  that a bearer token survives.
- `TestTheVertexBodyOmitsTheModelAndCarriesTheAnthropicVersion`, plus the
  header's absence.
- `TestTheVertexPathShapeIsSpelledOut`, `TestResolveVertexReadsTheRegionOutOfTheHost`,
  and the missing-project error.

## Documentation

`docs/PROVIDERS.md` (the `anthropic-messages` endpoint row, the
`vertex-2023-10-16` pin, ruling L-12), `docs/GAPS.md` (one row under the
provider audit), `examples/README.md` (the credential and base-URL tables).

## Remaining

- **Bedrock.** Same shape of gap, different subsystem: SigV4 request signing.
- **Per-model region overrides** (`VERTEX_REGION_CLAUDE_*`). One location per
  provider value today; a second value is the workaround.
- **No vendor capture.** Ruling L-3 applies unchanged — the Vertex request
  shape is pinned by tests this repository authored, not by a capture.
