# Erratum: the Vertex deployment signals are ranked, not OR-ed

**Relates to:** `docs/prd/01-support-claude-on-vertex-ai.md`, ruling L-12,
NFR-COMPAT-05, REQ-AUTH-03/04.
**Status:** implemented 2026-09-10.

## What the code did

`VertexSelected` was a disjunction. Any one of a truthy `CLAUDE_CODE_USE_VERTEX`,
the *presence* of `ANTHROPIC_VERTEX_PROJECT_ID`, or a Vertex host in either
base URL variable selected the deployment, and nothing could unselect it.

## The failure

Reported from the field: an operator who had tried Claude on Vertex went back
to the direct API the way the flag's own grammar suggests —

```bash
unset CLAUDE_CODE_USE_VERTEX
export ANTHROPIC_API_KEY=sk-ant-…
```

— and every request still went to
`https://aiplatform.googleapis.com/v1/projects/…/locations/global/publishers/anthropic/models/claude-sonnet-5:streamRawPredict`,
because `ANTHROPIC_VERTEX_PROJECT_ID` was still exported. `vertexAuth` then did
its job and withheld the Anthropic key from a Google endpoint, ADC had nothing
to put in its place, and the run died on Vertex's 401:

```
Request is missing required authentication credential. … CREDENTIALS_MISSING
```

Rendered under this package's `anthropic:` prefix, that reads as an Anthropic
authentication failure on a machine whose `ANTHROPIC_API_KEY` is perfectly
good. Nothing in the message names Vertex's *selection*, the project, or the
variable to unset, and `CLAUDE_CODE_USE_VERTEX=0` would not have helped either:
reading the flag for truth only stopped the flag from selecting the deployment,
so on this machine there was no way to spell "not Vertex" at all.

## Why L-12's reasoning was incomplete

L-12 is right that these variables are not ambient — nothing but a decision to
run Claude on Vertex sets them, unlike L-7's `GOOGLE_CLOUD_PROJECT`. What it
missed is that they are **sticky**: the decision is made once and unmade once,
and the variable that survives an incomplete unmaking silently keeps routing
traffic. "Not set by accident" and "still true" are different claims.

`ANTHROPIC_VERTEX_PROJECT_ID` is also the weaker of the two by its own name. It
carries the deployment's *coordinates*; `CLAUDE_CODE_USE_VERTEX` is the
*decision*. An `ANTHROPIC_API_KEY` in the same environment is an unambiguous
statement about a deployment that the project variable is not, and it is
useless on the Vertex path — it is dropped before the request is sent.

## What the code does now

`VertexSelectedBy` ranks the signals and returns the one that decided, so the
answer can be quoted back in an error:

| Rank | Signal | Effect |
|---|---|---|
| 1 | `Options.VertexProject` | selects — the embedding program said so |
| 1 | `CLAUDE_CODE_USE_VERTEX` truthy | selects — the decision variable |
| 1 | an `aiplatform.googleapis.com` host reached through `ANTHROPIC_BASE_URL`, `Options.BaseURL` or the catalog row | selects — that base URL is read by *both* deployments, so it is the endpoint the request is going to, and `/v1/messages` sent there is a 404 whatever else is set |
| 2 | `CLAUDE_CODE_USE_VERTEX` read as false | **vetoes** every remaining environment signal |
| 3 | `ANTHROPIC_VERTEX_PROJECT_ID` set | selects **only** when no `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_OAUTH_TOKEN` is set |
| 3 | an `aiplatform.googleapis.com` host reached only through `ANTHROPIC_VERTEX_BASE_URL` | the same — it is a Vertex-only variable that names no endpoint at all while the deployment is off, so it is a leftover of exactly the same kind |

Rank 3 is what the reported environment hit. Rank 2 is what the operator would
reasonably have reached for next.

Nothing that worked before stops working: a real Vertex box has no Anthropic
key — that is the premise of the deployment, and of the ambient credential
state REQ-AUTH-04 added for it — so `ANTHROPIC_VERTEX_PROJECT_ID` alone still
selects it, and REQ-AUTH-04 still reports `CredentialAmbient` rather than
"unconfigured".

## The second half: the failure is now legible

A 401 or 403 from the Vertex deployment now carries the deployment, its
coordinates, the setting that selected it, and both ways out:

```
anthropic: HTTP 401: … [Claude on Vertex AI: project p, location global,
selected by ANTHROPIC_VERTEX_PROJECT_ID. This deployment authenticates with
Google Application Default Credentials, not ANTHROPIC_API_KEY — which is set,
and is never sent to a Google endpoint. To use the Anthropic API directly
instead, unset ANTHROPIC_VERTEX_PROJECT_ID or set CLAUDE_CODE_USE_VERTEX=0.]
```

The note is attached to the Vertex deployment only; a bad-key 401 from
`api.anthropic.com` is left to say what it says.

## Tests

`provider/anthropic/vertex_test.go`: the leftover project variable, the
leftover Vertex proxy, the veto across all five falsy spellings, explicit
configuration outranking the veto, a Vertex host on `ANTHROPIC_BASE_URL` not
being treated as a leftover, the project variable still selecting on its own,
and the 401 note being present on Vertex and absent on the direct deployment.
