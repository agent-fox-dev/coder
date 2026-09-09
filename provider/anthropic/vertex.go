package anthropic

import (
	"errors"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// This file is NFR-COMPAT-05 for the Anthropic Messages wire: the SAME
// implementation serves Anthropic direct and Claude on Vertex AI, and moving
// between them is a config change rather than a provider swap.
//
// It mirrors provider/google's Vertex support deliberately. The requirement
// already has an answer in this codebase; a second, differently-shaped answer
// to it would be the divergence.

const (
	// VertexAPIVersion is the version this deployment requires IN THE BODY.
	//
	// Vertex has no anthropic-version HEADER: the version moved into the body
	// as `anthropic_version` and the value is its own string, unrelated to
	// APIVersion. Sending the direct API's dated version here is a 400, and
	// sending nothing is too.
	VertexAPIVersion = "vertex-2023-10-16"

	// VertexGlobalLocation is the location used when none is configured. The
	// global endpoint is the one that needs no regional host prefix, so it is
	// the only safe default for a deployment that named a project and nothing
	// else.
	VertexGlobalLocation = "global"

	// vertexHostSuffix identifies a Vertex AI endpoint. Every Vertex host is
	// either this or "<location>-" + this.
	vertexHostSuffix = "aiplatform.googleapis.com"
)

// The environment variables of the Vertex deployment. They are named here, as
// constants, because three separate things read them — the deployment switch,
// the credential table's ambient detector and the base URL resolution — and a
// literal in each is three places to make the same typo.
const (
	// BaseURLVar overrides the base URL for BOTH deployments.
	BaseURLVar = "ANTHROPIC_BASE_URL"
	// VertexEnableVar is the flag an operator sets to choose this deployment.
	// It is read for TRUTH, not presence: `=0` means off.
	VertexEnableVar = "CLAUDE_CODE_USE_VERTEX"
	// VertexProjectVar names the GCP project. Unlike GOOGLE_CLOUD_PROJECT it
	// is set by nothing except a decision to run Claude on Vertex, which is
	// why it may select the deployment (ruling L-12).
	VertexProjectVar = "ANTHROPIC_VERTEX_PROJECT_ID"
	// VertexRegionVar is Vertex's own region variable.
	VertexRegionVar = "CLOUD_ML_REGION"
	// VertexBaseURLVar points at a proxy in front of Vertex. It beats
	// BaseURLVar when this deployment is on: the two name different upstreams
	// and a machine can carry both.
	VertexBaseURLVar = "ANTHROPIC_VERTEX_BASE_URL"
)

// Vertex is the Vertex AI deployment's coordinates. The zero value is the
// Anthropic-direct (API-key) deployment.
//
// NFR-COMPAT-05 requires that switching between deployments is "only a config
// change, not a provider swap". The two differ in exactly four things — host,
// path, where the version lives, and whether the body names the model — and
// this type carries the second. ResolveVertex derives it from configuration
// alone; VendorAuth.Ambient handles the credential, reporting an ADC
// deployment as REQ-AUTH-04's ambient state rather than as "no key".
type Vertex struct {
	Project  string
	Location string
}

// On reports whether the Vertex path shape applies. A project is the whole
// signal: the Vertex path cannot be spelled without one, and ResolveVertex
// refuses to return a selected deployment without one.
func (v Vertex) On() bool { return v.Project != "" }

// BaseURL is the endpoint for this deployment's location. The global endpoint
// has no regional prefix; every other location does.
func (v Vertex) BaseURL() string {
	if v.Location == "" || v.Location == VertexGlobalLocation {
		return "https://" + vertexHostSuffix
	}
	return "https://" + v.Location + "-" + vertexHostSuffix
}

// Path returns the request path for a model.
//
// On Vertex the model id is a URL SEGMENT and the body must not name it, which
// is why the request's model field is omitzero rather than required:
//
//	direct: /v1/messages
//	Vertex: /v1/projects/{p}/locations/{l}/publishers/anthropic/models/{id}:streamRawPredict
//
// The streaming verb is `:streamRawPredict`; `:rawPredict` is the unary one.
// Both carry the same SSE grammar as the direct API, which is what lets one
// decoder serve both.
func (v Vertex) Path(m *core.Model, stream bool) string {
	if !v.On() {
		return "/v1/messages"
	}
	verb := ":rawPredict"
	if stream {
		verb = ":streamRawPredict"
	}
	id := ""
	if m != nil {
		id = m.ID
	}
	// A "publishers/anthropic/models/" prefix carried on a Vertex catalog row
	// would otherwise be doubled.
	id = strings.TrimPrefix(id, "publishers/anthropic/models/")
	return "/v1/projects/" + v.Project + "/locations/" + v.Location +
		"/publishers/anthropic/models/" + id + verb
}

// Path is the Anthropic-direct path shape. It is the zero Vertex, kept as a
// function because it is the shape every non-Vertex caller wants.
func Path(m *core.Model, stream bool) string { return Vertex{}.Path(m, stream) }

// VertexSelected reports whether the ENVIRONMENT names this deployment.
//
// It is exported because VendorAuth.Ambient is the pre-flight answer to the
// reported bug — a Vertex deployment has no readable key and must not resolve
// to "no credential" — and an embedder writing its own pre-flight needs the
// same predicate.
//
// Ruling L-12: unlike Gemini's L-7, an environment variable MAY select this
// deployment, because these variables are not ambient. CLAUDE_CODE_USE_VERTEX
// and ANTHROPIC_VERTEX_PROJECT_ID are set by nothing except an operator
// choosing Claude on Vertex; GOOGLE_CLOUD_PROJECT, which is set on every GCE
// and Cloud Run box, is NOT consulted here and can only supply a project once
// something else has selected the deployment.
func VertexSelected(env provider.Env) bool {
	if envOn(env.Get(VertexEnableVar)) {
		return true
	}
	if env.Has(VertexProjectVar) {
		return true
	}
	return isVertexHost(env.Get(VertexBaseURLVar)) || isVertexHost(env.Get(BaseURLVar))
}

// ResolveVertex decides the deployment from configuration alone.
//
// base is the resolved base URL, so a Vertex host configured anywhere — the
// catalog row, Options.BaseURL, either base URL variable — selects the
// deployment on its own.
//
// A selected deployment with no resolvable project is an ERROR rather than a
// silent fallback to the direct path: /v1/messages on a Vertex host is a 404
// several layers down, and the message that produces names neither Vertex nor
// the missing project.
func ResolveVertex(base, project, location string, env provider.Env) (Vertex, error) {
	if project == "" && !isVertexHost(base) && !VertexSelected(env) {
		return Vertex{}, nil
	}
	if project == "" {
		// GOOGLE_CLOUD_PROJECT and CLOUDSDK_CORE_PROJECT SUPPLY a project;
		// per ruling L-12 they never select the deployment.
		project = firstEnv(env, VertexProjectVar, "GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT")
	}
	if project == "" {
		return Vertex{}, errors.New("anthropic: the Vertex AI deployment needs a project: " +
			"set Options.VertexProject or " + VertexProjectVar)
	}
	if location == "" {
		location = firstEnv(env, VertexRegionVar, "GOOGLE_CLOUD_LOCATION", "CLOUDSDK_COMPUTE_REGION")
	}
	if location == "" {
		// A regional host already says which region it is; demanding a second
		// config field to repeat it makes the switch two changes instead of
		// one, and a mismatched pair is a 404.
		location = locationFromHost(base)
	}
	if location == "" {
		location = VertexGlobalLocation
	}
	return Vertex{Project: project, Location: location}, nil
}

// deploymentBase is the base URL the deployment CHECK sees. It mirrors
// provider.ResolveBaseURL's precedence for the layers visible without a
// context — the credential store is not one of them, because the switch is
// decided before the store is read and a store that carries a Vertex host is
// not how a deployment is named.
func deploymentBase(m *core.Model, configured string, env provider.Env) string {
	base := configured
	if m != nil && m.BaseURL != "" {
		base = m.BaseURL
	}
	if u := env.Get(BaseURLVar); u != "" {
		base = u
	}
	if u := env.Get(VertexBaseURLVar); u != "" {
		base = u
	}
	return strings.TrimRight(base, "/")
}

// vertexAuth NARROWS a resolved credential to what this deployment can use.
//
// A bearer token is kept: that is the Google OAuth access token, however it
// arrived — ANTHROPIC_AUTH_TOKEN, a Credentials store, or an Options.HTTPClient
// that adds its own header (in which case there is nothing here to keep and
// the ambient state is the correct answer).
//
// An x-api-key is DROPPED, and this is the security half of the change rather
// than tidiness. A workstation that used to call Anthropic directly still has
// ANTHROPIC_API_KEY set; forwarding it would hand a first-party credential to
// a third party in a header Vertex has no use for. Dropping it leaves
// REQ-AUTH-04's ambient state, which is the truth: this process holds no
// readable credential for this endpoint and the transport may still have one.
func vertexAuth(auth provider.ModelAuth) provider.ModelAuth {
	if auth.Headers != nil && auth.Headers["Authorization"] != nil {
		return auth
	}
	delete(auth.Headers, "x-api-key")
	auth.APIKey = ""
	if auth.State == provider.CredentialResolved {
		auth.State = provider.CredentialAmbient
	}
	// Source names where a credential came from, and none did: the token, if
	// there is one, is the transport's. "credential-store" sets the same
	// precedent for a source that is not an environment variable.
	auth.Source = "vertex-adc"
	return auth
}

// envOn reads a FLAG variable for truth rather than presence.
//
// Env.Has is presence, which is right for a variable whose value is the
// configuration (a key, a URL). A flag's value IS its grammar: an operator who
// writes CLAUDE_CODE_USE_VERTEX=0 to turn the deployment off must not select
// it by having written the variable at all.
func envOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

func firstEnv(env provider.Env, names ...string) string {
	for _, n := range names {
		if v := env.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// hostOf is deliberately string surgery rather than net/url parsing: the base
// URL may be a bare host, and a parse failure must not decide a deployment.
func hostOf(base string) string {
	h := base
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/:"); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func isVertexHost(base string) bool {
	h := hostOf(base)
	return h == vertexHostSuffix || strings.HasSuffix(h, "-"+vertexHostSuffix)
}

// locationFromHost reads the region out of a regional Vertex host.
func locationFromHost(base string) string {
	h := hostOf(base)
	if s, ok := strings.CutSuffix(h, "-"+vertexHostSuffix); ok {
		return s
	}
	return ""
}
