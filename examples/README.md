# AgentKit examples

Fifteen examples, smallest first. The first twelve are small and
self-contained — most are a single `main.go` you can read top to bottom and
copy into your own project, and `testing` is a test file — and they
deliberately repeat their setup rather than sharing a helper package, so
nothing you need is in a file you have not opened. The last three,
[`issued`](issued), [`cleaner`](cleaner) and [`flatline`](flatline), are the
opposite on purpose: finished applications, which is what the others look like
once they stop being examples.

**Six of them need no API key at all**: `agentdemo`, `testing`, `plugins`,
`mcp`, `mcpserver` and `skills` do their real work before any model call, so you can run
them right now.

| Example | Run it | What it teaches |
|---|---|---|
| [`chat`](chat) | `go run ./examples/chat "your question"` | Model resolution, provider registration, cost accounting. Start here. |
| [`streaming`](streaming) | `go run ./examples/streaming "write a haiku"` | The event taxonomy, live token output, Ctrl-C abort from a signal handler. |
| [`codingagent`](codingagent) | `go run ./examples/codingagent --dir . "which files define the tool policy?"` | Built-in file/shell tools, a workspace root, and the `execute` authorization boundary. |
| [`session`](session) | `go run ./examples/session "pick a number"` then `go run ./examples/session "which number?"` | Durable append-only sessions. The second run answers from the first one's transcript, on disk. |
| [`delegation`](delegation) | `go run ./examples/delegation "which files define the agent loop?"` | Named specialists, per-child tool scoping, budget propagation. |

Then the ones that go deeper. `testing`, `plugins`, `mcp`, `mcpserver` and `skills` run
fully without a key:

| Example | Run it | What it teaches |
|---|---|---|
| [`testing`](testing) | `go test ./examples/testing/ -v` | **How to test the agent code *you* write.** A test file, not a program: scripting turns with `provider/faux`, asserting what was actually sent, and testing your handlers, interceptors and middleware offline. |
| [`customtools`](customtools) | `go run ./examples/customtools` | Writing tools well: the schema combinators, `Handler` vs `Execute`, argument repair, sequential execution, per-tool prompt guidelines, and a tool that ends the run. |
| [`plugins`](plugins) | `go run ./examples/plugins` | The four plugin categories, manifest discovery, load ordering, the `disabled` list, and why a plugin hook can only narrow what the host already allowed. |
| [`mcp`](mcp) | `go run ./examples/mcp` · `--serve` | Model Context Protocol both ways: consuming a server's tools under qualified names, and exposing your own over stdio. |
| [`mcpserver`](mcpserver) | `go run ./examples/mcpserver` · `-transport http -port 8722 -api-key-env MCP_API_KEY` · `-config agentkit.toml` | Standalone reference MCP server host: stdio and HTTP transports, API key authentication, and TOML configuration. |
| [`interactive`](interactive) | `go run ./examples/interactive` | Typing *while* the agent works: steering a running turn, queued follow-ups, out-of-band abort, phase and snapshot. |
| [`skills`](skills) | `go run ./examples/skills` | Repository- and user-authored prompt material: the three discovery tiers, the project trust gate, progressive disclosure and its escaping, context files, the tool-merge and mid-session activation seams, the subagent step. |

And three applications rather than demonstrations of a feature. The first two
are the halves of one workflow — `issued` turns a bug report into an issue,
`cleaner` turns that issue into a merged fix — and each is a slash-command
skill rebuilt as a program. The third rebuilds an orchestrator's simplest path:

| Example | Run it | What it teaches |
|---|---|---|
| [`issued`](issued) | `go run ./examples/issued "<a bug report>"` · `go test ./examples/issued/ -v` | **A whole application.** The read-only mandate becomes a tool policy, the issue template becomes a schema, "cite real files" becomes a check in a tool handler, and filing lives where no model output can reach it. Its test suite needs no key. |
| [`cleaner`](cleaner) | `go run ./examples/cleaner https://github.com/{owner}/{repo}/issues/{n}` · `go test ./examples/cleaner/` | **A whole application.** Two model phases with different tool scopes, structured hand-off through terminating tools, an application-specific authorization guard, verification the model cannot fake, and an end-to-end test of all of it against a scripted provider. Its test suite needs no key. |
| [`flatline`](flatline) | `cd examples/flatline && go run . --dir ~/src/widgets 3` · `go test ./...` | **A whole application, and a nested module.** agent-fox's `af code` for one spec pack with no dependencies: a session per task group with the spec rendered and scoped to it, memory carried from group to group, the pack's own test commands as gates, retries with the failure in the prompt, per-group squash landings, and an optional informational verifier — driven by the spec library it imports. Needs a sibling checkout of `agent-fox-dev/spec`; its test suite needs no key. |

There is also [`agentdemo`](agentdemo), which needs **no API key and no
network**: it drives the real loop against a scripted provider and prints
seven behaviours the specification originally got wrong. Run it first if you
want to see the machinery without spending anything.

```bash
go run ./examples/agentdemo
```

## Configuring an application

AgentKit reads no configuration file and has no global state. Everything is
either a field on `core.AgentConfig` or an environment variable consulted at
request time. There are exactly three things to get right.

### 1. A credential for the vendor you are calling

Each vendor has an **ordered** list of variables, not a single
`<VENDOR>_API_KEY` convention. The first one set wins, and the scheme differs
per variable — that is the whole reason the list is ordered rather than a
lookup.

| Vendor | Variables, in order | Sent as |
|---|---|---|
| `anthropic` | `ANTHROPIC_AUTH_TOKEN` | `Authorization: Bearer` |
| | `ANTHROPIC_OAUTH_TOKEN` | `Authorization: Bearer` |
| | `ANTHROPIC_API_KEY` | `x-api-key` |
| | on Vertex: a Google OAuth token, `ambient` when the transport holds it | `Authorization: Bearer` |
| `openai` | `OPENAI_API_KEY` | `Authorization: Bearer` |
| `google` | `GOOGLE_GENERATIVE_AI_API_KEY` | `x-goog-api-key` |
| | `GEMINI_API_KEY` | `x-goog-api-key` |
| | `GOOGLE_API_KEY` | `x-goog-api-key` |
| `ollama` | `OLLAMA_API_KEY` (usually unset — a local server needs none) | `Authorization: Bearer` |
| `openrouter` | `OPENROUTER_API_KEY` | `Authorization: Bearer` |
| `deepseek` | `DEEPSEEK_API_KEY` | `Authorization: Bearer` |
| `groq` | `GROQ_API_KEY` | `Authorization: Bearer` |
| `xai` | `XAI_API_KEY` | `Authorization: Bearer` |
| `together` | `TOGETHER_API_KEY` | `Authorization: Bearer` |
| `moonshot` | `MOONSHOT_API_KEY` | `Authorization: Bearer` |
| anything else | `<VENDOR>_API_KEY` | `Authorization: Bearer` |

The last row is a fallback for a vendor this build has never heard of. It is
not the convention — it is what happens when there is no table entry, and it
beats refusing to authenticate at all.

**Credentials have three states, not two.** A deployment using a cloud
instance role, Google ADC or a workload identity has *no key this process can
read* and a transport that will nonetheless authenticate. That is `ambient`,
and it must pass a pre-flight check that `none` fails — otherwise every
service-account deployment fails a check a plain key would have passed. The
examples' `checkCredentials` shows the correct test:

```go
auth := provider.ResolveAuth(anthropic.VendorAuth, provider.Env{})
if auth.State == provider.CredentialNone {
    // genuinely unconfigured
}
```

Setting only a base URL also yields `ambient`: the vendor is *discovered* but
not *authenticated*, which is exactly the state a gateway that authenticates
by URL leaves you in.

For a long-running process whose token expires, do not use environment
variables at all — supply a `provider.Credentials` store on the provider
options. It is consulted before the environment, and OAuth refresh is
serialized per vendor so that N concurrent turns arriving on an expired token
refresh once rather than N times.

### 2. A base URL, when you are not talking to the vendor directly

| Variable | Points at |
|---|---|
| `ANTHROPIC_BASE_URL` | a proxy or gateway in front of Anthropic |
| `ANTHROPIC_VERTEX_BASE_URL` | a proxy in front of Vertex; beats `ANTHROPIC_BASE_URL` when the Vertex deployment is on |
| `OPENAI_BASE_URL` | Azure OpenAI, a gateway, or any OpenAI-compatible server |
| `GOOGLE_GEMINI_BASE_URL` | Vertex AI, or a proxy |
| `OLLAMA_HOST` | your Ollama server (default `http://localhost:11434`) |
| `<VENDOR>_BASE_URL` | the same, for every other vendor in the table above |

A base URL swap is **not** sufficient on its own for an OpenAI-compatible
endpoint. Roughly a dozen named quirks differ between hosts — whether the
field is `max_tokens` or `max_completion_tokens`, whether `store` and the
`developer` role are accepted, how reasoning is replayed — and AgentKit
resolves those from a compatibility profile inferred from the vendor and the
host, overridable per model from the catalog. Point `OPENAI_BASE_URL` at Groq
and the profile follows.

**Google Vertex** is a config change rather than a provider swap. Set a
project — `Options.VertexProject`, else `GOOGLE_CLOUD_PROJECT` or
`CLOUDSDK_CORE_PROJECT` — and optionally a location
(`GOOGLE_CLOUD_LOCATION` / `CLOUDSDK_COMPUTE_REGION`, default `global`).
Note that these variables alone do *not* flip the deployment, because they are
set on every GCE and Cloud Run box: pass the project explicitly, or point
`GOOGLE_GEMINI_BASE_URL` at a Vertex host.

**Claude on Vertex** is the same kind of config change, on the same wire
implementation:

```bash
export CLAUDE_CODE_USE_VERTEX=1
export ANTHROPIC_VERTEX_PROJECT_ID=my-project
export CLOUD_ML_REGION=us-east5            # optional; default `global`
```

| Variable | Does |
|---|---|
| `CLAUDE_CODE_USE_VERTEX` | selects the deployment. Read for *truth*, not presence: `=0` is an explicit **off** that vetoes every other environment signal |
| `ANTHROPIC_VERTEX_PROJECT_ID` | the GCP project. It selects the deployment on its own **only when no `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_OAUTH_TOKEN` is set** |
| `CLOUD_ML_REGION` | the location; `GOOGLE_CLOUD_LOCATION` and `CLOUDSDK_COMPUTE_REGION` also work |
| `ANTHROPIC_VERTEX_BASE_URL` | a proxy in front of Vertex. Like the project variable, a Vertex host here selects the deployment only when no Anthropic-direct credential is set |

`GOOGLE_CLOUD_PROJECT` and `CLOUDSDK_CORE_PROJECT` may *supply* the project
once something else has selected the deployment; like the Gemini case above,
they never select it. `Options.VertexProject` / `Options.VertexLocation` are
the in-code equivalents, and a Vertex base URL selects the deployment too. A
selected deployment with no project anywhere is an error naming the project,
not a request sent to `api.anthropic.com` with a Vertex path.

**Going back to the direct API** is `unset CLAUDE_CODE_USE_VERTEX` *and*
`unset ANTHROPIC_VERTEX_PROJECT_ID` — or, if the project variable is set by
something you do not control, `export CLAUDE_CODE_USE_VERTEX=0`, which turns
the deployment off outright. Exporting an `ANTHROPIC_API_KEY` is enough on its
own when the project variable is the only thing left over: a key that only the
direct deployment can use outranks a project that only names coordinates. If a
request does reach Vertex without a credential, the 401 says so — it names the
project, the setting that selected the deployment, and both ways out, because
Google's own body names none of them.

Vertex authenticates with a Google OAuth access token, and this module has no
dependencies to mint one with. Three ways, none of which adds one:

```bash
# 1. A token in the environment. Refresh it yourself; it is short-lived.
export ANTHROPIC_AUTH_TOKEN="$(gcloud auth print-access-token)"
```

```go
// 2. An ADC-authenticating transport, owned by the application.
client, _ := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
anthropic.Provider(anthropic.Options{HTTPClient: client})

// 3. A credential store, for a long-running process — REQ-AUTH-05/06 gives
//    you serialized refresh, which the environment cannot.
anthropic.Provider(anthropic.Options{Credentials: creds})
```

With none of the three the credential state is `ambient`, which is the honest
answer: this process holds no readable credential and the transport may still
have one. That is also why pre-flight passes — a Vertex deployment is
configured, and a check that reported `none` would refuse the run and name the
wrong cause.

An `ANTHROPIC_API_KEY` left over from a direct deployment is **dropped**, not
forwarded: it is not a Vertex credential, and sending it would hand a
first-party secret to a third party.

Vertex names Claude models with a dated suffix (`claude-sonnet-5@20260401`).
The catalog is not an allowlist, so such an id resolves by cloning the vendor's
default row and reaches the URL verbatim:
`AGENTKIT_MODEL=anthropic/claude-sonnet-5@20260401`.

### 3. A model, resolved through the catalog

```go
model, err := catalog.ResolveModel("anthropic/claude-sonnet-5")
```

`ResolveModel` is the single entry point, and it is what supplies the wire
API, base URL, context window, pricing, reasoning support and compatibility
profile. The model-ID string carries none of that, which is why a
pass-through design cannot clamp `max_tokens`, cost a turn, or pick the right
request shape.

The catalog is **not an allowlist**. An unknown id under a *known* vendor
clones that vendor's default row with a warning, so a model released after
this build works without an SDK release. An unknown *vendor* is a
configuration error. A bare id that matches two vendors resolves to nothing
and errors rather than guessing.

Every example takes `AGENTKIT_MODEL` to override its default:

```bash
AGENTKIT_MODEL=openai/gpt-5.6-terra    go run ./examples/chat "hello"
AGENTKIT_MODEL=google/gemini-3.8-flash go run ./examples/chat "hello"
```

`catalog.Default().Vendors()` lists what the shipped snapshot knows — today
`anthropic`, `google` and `openai`. **A vendor with a provider is not
necessarily a vendor with catalog rows.** Ollama, OpenRouter, Groq, DeepSeek
and the rest have a wire implementation and a credential table (above) but no
rows here, so `AGENTKIT_MODEL=ollama/llama3.2` is an *unknown vendor* error
rather than a sibling clone. To reach one, build the `core.Model` descriptor
yourself, or supply your own catalog with `catalog.Parse` and resolve through
that — the shipped snapshot is data, versioned separately and overridable,
not a gate.

One caution about sibling-cloning, because it costs real money to miss: an
unknown id under a *known* vendor resolves, it does not validate. Ask for
`anthropic/claude-sonnet-4-5` today and you get a working descriptor cloned
from the current default row — and then the request fails at the vendor,
because that model is gone. Resolution succeeding means "AgentKit knows how
to build this request", never "this model exists".

### Other variables

| Variable | Effect |
|---|---|
| `AGENTKIT_TELEMETRY=0` | Disables every attribution header. AgentKit sends `x-agentkit-version` and `user-agent` to identify itself; neither carries a session id, workspace path, user identity or prompt content. `AgentConfig.Attribution = false` does the same in code. |

## Things every application has to decide

These are not defaults you can ignore — the library will stop you.

**A shell tool needs an authorization boundary.** Registering `execute`,
`run_command` or `powershell` with a nil `AgentConfig.BeforeToolCall` fails the
run with `core.ErrUnguardedExecute`, before any request is built. A headless
service would otherwise hand the model an unrestricted shell by omission.
Supply an interceptor — `guard.Restricted` is a replaceable starting
point — or pass `guard.AllowAll` to say in code that you meant it.
See [`codingagent`](codingagent).

**A stop policy is how a run ends.** `stop.AfterTurns` and `stop.OverBudget`
compose with `stop.Any`. Without one, a tool-using agent has no upper bound.
The budget check runs after each turn, so a run can overshoot by at most one
turn plus its tool batch; `middleware.Budget` is the pre-turn gate if you need
a hard ceiling.

**File tools are contained to a workspace root**, resolved through symlinks
before every read and re-checked immediately before every write. `execute` is
deliberately *not* contained — that is what the interceptor above is for.

**Project-local prompt material is untrusted by default.**
`AgentConfig.TrustProject` defaults to false, so skills and context files
under the working directory are not discovered, not listed and not named in
the system prompt. A cloned repository would otherwise author part of your
prompt by being the current directory. Establishing trust is an affirmative
act by your application. See [`skills`](skills).

## Troubleshooting

**`no credential for vendor "x"`** — nothing in that vendor's table is set,
and there is no ambient credential. The message names the variables.

**A 401 despite the pre-flight passing** — you have a base URL set but no key,
which is the `ambient` state: discovered, not authenticated. Set the key too.

**`agent is busy` (`ErrBusy`)** — `Run` or `Stream` was called while a turn was
in flight. Conflicting operations fail rather than queue, because a prompt
queued behind a running turn was written against a transcript that has since
changed. Retry, queue it yourself, or use `Steer`/`FollowUp` to deliver a
message into the *running* turn.

**`session store is not empty`** — you passed a non-empty store to `NewAgent`.
Resuming means folding the log and passing the recovered model and reasoning
level as construction inputs; use `NewAgentFromSession`. See
[`session`](session).

**Nothing streams** — a non-streaming provider emits no delta events at all.
Deltas are an optimization; the authoritative events always arrive.

## Compaction

`compaction.NewContextTransform` installs a context transform that summarizes a
growing transcript in place. Once a summary checkpoint exists it is always
re-applied; the threshold only decides whether to extend it. The naive
"compact when over threshold" reading oscillates.

The three applications install it — see `installCompaction` in
[`issued/triage.go`](issued/triage.go), [`cleaner/phases.go`](cleaner/phases.go)
and [`flatline/phases.go`](flatline/phases.go). The shape is the same in each:
make the `core.ConversationHistory` first, bind it into `CompactionDeps`
alongside `ModelSummarizer` and `ModelTurnSummarizer` over the registered
provider, set `cfg.TransformContext`, and construct the agent with
`agentkit.NewAgentWithHistory(cfg, history)` so the transform and the agent
share one history. None of the small examples set it up.
