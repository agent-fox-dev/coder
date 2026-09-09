package anthropic_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
)

// sent drives one request through the provider and returns what the transport
// saw. No network, no key, no process environment (NFR-TEST-01, NFR-TEST-04):
// the deployment is decided from the injected env alone.
func sent(t *testing.T, opts anthropic.Options, env map[string]string) *http.Request {
	t.Helper()
	var got *http.Request
	req := core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Options: core.RequestOptions{
			Env: env,
			Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				got = r.Clone(r.Context())
				if r.Body != nil {
					b, _ := io.ReadAll(r.Body)
					got.Body = io.NopCloser(strings.NewReader(string(b)))
				}
				return &http.Response{StatusCode: 200, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(streamFixture()))}, nil
			}),
		},
	}
	if opts.Getenv == nil {
		opts.Getenv = func(string) string { return "" }
	}
	anthropic.Provider(opts).Stream(context.Background(), testModel(), req,
		core.ProviderStreamOptions{}).Result()
	return got
}

// TestSwitchingToVertexIsAConfigChangeNotAProviderSwap is NFR-COMPAT-05.
//
// The requirement is not "Vertex works" — it is that the SAME wire API
// implementation serves both deployments and that moving between them touches
// configuration only. Each case changes one piece of config and nothing else.
func TestSwitchingToVertexIsAConfigChangeNotAProviderSwap(t *testing.T) {
	const path = "/v1/projects/proj-1/locations/us-east5/publishers/anthropic/" +
		"models/claude-test:streamRawPredict"

	cases := []struct {
		name string
		opts anthropic.Options
		env  map[string]string
		want string
	}{
		{
			name: "the default is the direct deployment",
			env:  map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"},
			want: "https://api.anthropic.com/v1/messages",
		},
		{
			name: "a project on Options is the whole switch, host included",
			opts: anthropic.Options{VertexProject: "proj-1", VertexLocation: "us-east5"},
			want: "https://us-east5-aiplatform.googleapis.com" + path,
		},
		{
			// The reported environment, spelled exactly: the flag plus the
			// coordinates, no key anywhere.
			name: "CLAUDE_CODE_USE_VERTEX selects it with the coordinates from the environment",
			env: map[string]string{
				"CLAUDE_CODE_USE_VERTEX":      "1",
				"ANTHROPIC_VERTEX_PROJECT_ID": "proj-1",
				"CLOUD_ML_REGION":             "us-east5",
			},
			want: "https://us-east5-aiplatform.googleapis.com" + path,
		},
		{
			// GOOGLE_CLOUD_PROJECT may SUPPLY a project once something else
			// has selected the deployment (ruling L-12).
			name: "the project may come from the ambient GCP environment",
			env: map[string]string{
				"CLAUDE_CODE_USE_VERTEX": "true",
				"GOOGLE_CLOUD_PROJECT":   "proj-1",
				"GOOGLE_CLOUD_LOCATION":  "us-east5",
			},
			want: "https://us-east5-aiplatform.googleapis.com" + path,
		},
		{
			name: "a Vertex base URL selects it and names its own region",
			env: map[string]string{
				"ANTHROPIC_BASE_URL":   "https://us-east5-aiplatform.googleapis.com",
				"GOOGLE_CLOUD_PROJECT": "proj-1",
			},
			want: "https://us-east5-aiplatform.googleapis.com" + path,
		},
		{
			// A proxy in front of Vertex: the path shape is Vertex's, the host
			// is the operator's, and it beats ANTHROPIC_BASE_URL because the
			// two name different upstreams.
			name: "ANTHROPIC_VERTEX_BASE_URL beats the general base URL",
			env: map[string]string{
				"CLAUDE_CODE_USE_VERTEX":      "1",
				"ANTHROPIC_VERTEX_PROJECT_ID": "proj-1",
				"CLOUD_ML_REGION":             "us-east5",
				"ANTHROPIC_BASE_URL":          "https://general.proxy.example.com",
				"ANTHROPIC_VERTEX_BASE_URL":   "https://vertex.proxy.example.com",
			},
			want: "https://vertex.proxy.example.com" + path,
		},
		{
			// No region named anywhere. The global endpoint is the only one
			// that needs no regional prefix, so it is the safe default.
			name: "no location falls back to the global endpoint",
			env: map[string]string{
				"CLAUDE_CODE_USE_VERTEX":      "1",
				"ANTHROPIC_VERTEX_PROJECT_ID": "proj-1",
			},
			want: "https://aiplatform.googleapis.com/v1/projects/proj-1/locations/global/" +
				"publishers/anthropic/models/claude-test:streamRawPredict",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := sent(t, c.opts, c.env)
			if r == nil {
				t.Fatal("no request was made")
			}
			if got := r.URL.String(); got != c.want {
				t.Errorf("request URL =\n  %s\nwant\n  %s", got, c.want)
			}
		})
	}
}

// TestTheVertexDeploymentIsDiscoveredAsAnAmbientCredential is REQ-AUTH-04 and
// the reported bug.
//
// A Vertex deployment has no key this process can read and a transport that
// will nonetheless authenticate. Resolving it to CredentialNone fails it at
// the pre-flight check every embedder writes — with a message saying the
// vendor is unconfigured, which is the wrong cause.
func TestTheVertexDeploymentIsDiscoveredAsAnAmbientCredential(t *testing.T) {
	none := func(string) string { return "" }
	cases := []struct {
		name string
		env  map[string]string
		want provider.CredentialState
	}{
		{"the reported environment", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"},
			provider.CredentialAmbient},
		{"the project variable alone", map[string]string{"ANTHROPIC_VERTEX_PROJECT_ID": "p"},
			provider.CredentialAmbient},
		{"a Vertex proxy", map[string]string{"ANTHROPIC_VERTEX_BASE_URL": "https://proxy.example.com"},
			provider.CredentialAmbient},
		{"nothing at all", map[string]string{}, provider.CredentialNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth := provider.ResolveAuth(anthropic.VendorAuth,
				provider.Env{Override: c.env, Getenv: none})
			if auth.State != c.want {
				t.Fatalf("credential state = %v, want %v; a pre-flight check that reads "+
					"CredentialNone refuses the run and names the wrong cause", auth.State, c.want)
			}
		})
	}
}

// TestTheFlagVariableSetToZeroDoesNotSelectVertex: a flag's value IS its
// grammar. Presence would make "turn it off" impossible to write.
func TestTheFlagVariableSetToZeroDoesNotSelectVertex(t *testing.T) {
	for _, off := range []string{"0", "false", "no", "off", "OFF"} {
		env := map[string]string{"CLAUDE_CODE_USE_VERTEX": off, "ANTHROPIC_API_KEY": "sk-ant-x"}
		if got := sent(t, anthropic.Options{}, env).URL.String(); got !=
			"https://api.anthropic.com/v1/messages" {
			t.Errorf("CLAUDE_CODE_USE_VERTEX=%q gave %s, want the direct deployment", off, got)
		}
	}
}

// TestAnAnthropicAPIKeyIsNeverSentToTheVertexEndpoint.
//
// A workstation that used to call Anthropic directly still has
// ANTHROPIC_API_KEY set. Forwarding it hands a first-party credential to a
// third party in a header they have no use for — a deployment switch that
// silently exfiltrates the old deployment's key.
func TestAnAnthropicAPIKeyIsNeverSentToTheVertexEndpoint(t *testing.T) {
	r := sent(t, anthropic.Options{}, map[string]string{
		"CLAUDE_CODE_USE_VERTEX":      "1",
		"ANTHROPIC_VERTEX_PROJECT_ID": "proj-1",
		"ANTHROPIC_API_KEY":           "sk-ant-leftover-from-the-old-deployment",
	})
	if got := r.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key = %q; an Anthropic key must never reach a Google endpoint", got)
	}
	if got := r.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q; an x-api-key credential must be dropped, not "+
			"re-scheme'd into a bearer token", got)
	}

	// The converse: a bearer token IS the Vertex credential — a gcloud access
	// token in ANTHROPIC_AUTH_TOKEN is the dependency-free way to authenticate.
	r = sent(t, anthropic.Options{}, map[string]string{
		"CLAUDE_CODE_USE_VERTEX":      "1",
		"ANTHROPIC_VERTEX_PROJECT_ID": "proj-1",
		"ANTHROPIC_AUTH_TOKEN":        "ya29.access-token",
	})
	if got := r.Header.Get("Authorization"); got != "Bearer ya29.access-token" {
		t.Fatalf("Authorization = %q, want the bearer token to survive", got)
	}
}

// TestTheVertexBodyOmitsTheModelAndCarriesTheAnthropicVersion pins the two
// body-shaped halves of the wire delta. Both are 400s when wrong, and the
// model field is the one an omitempty-style struct would get silently right
// and a required one would get silently wrong.
func TestTheVertexBodyOmitsTheModelAndCarriesTheAnthropicVersion(t *testing.T) {
	r := sent(t, anthropic.Options{VertexProject: "proj-1"}, nil)

	if got := r.Header.Get("anthropic-version"); got != "" {
		t.Errorf("anthropic-version header = %q; on Vertex the version lives in the body", got)
	}

	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if _, ok := body["model"]; ok {
		t.Errorf("body names a model: %s\nVertex names it in the URL and rejects the field", raw)
	}
	if got := body["anthropic_version"]; got != anthropic.VertexAPIVersion {
		t.Errorf("anthropic_version = %v, want %q", got, anthropic.VertexAPIVersion)
	}

	// The direct deployment is untouched by both changes.
	r = sent(t, anthropic.Options{}, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"})
	if got := r.Header.Get("anthropic-version"); got != anthropic.APIVersion {
		t.Errorf("direct anthropic-version = %q, want %q", got, anthropic.APIVersion)
	}
	raw, _ = io.ReadAll(r.Body)
	body = nil
	_ = json.Unmarshal(raw, &body)
	if body["model"] != "claude-test" {
		t.Errorf("direct body model = %v, want claude-test", body["model"])
	}
	if _, ok := body["anthropic_version"]; ok {
		t.Errorf("the direct body must not carry anthropic_version: %s", raw)
	}
}

// TestTheVertexPathShapeIsSpelledOut pins both shapes directly, including the
// unary verb the streaming path cannot reach.
func TestTheVertexPathShapeIsSpelledOut(t *testing.T) {
	v := anthropic.Vertex{Project: "p", Location: "europe-west1"}
	if got, want := v.Path(testModel(), true),
		"/v1/projects/p/locations/europe-west1/publishers/anthropic/models/"+
			"claude-test:streamRawPredict"; got != want {
		t.Errorf("Path(stream) = %q, want %q", got, want)
	}
	if got, want := v.Path(testModel(), false),
		"/v1/projects/p/locations/europe-west1/publishers/anthropic/models/"+
			"claude-test:rawPredict"; got != want {
		t.Errorf("Path(unary) = %q, want %q", got, want)
	}
	if got := anthropic.Path(testModel(), true); got != "/v1/messages" {
		t.Errorf("the zero Vertex must keep the direct shape: got %q", got)
	}
	if got := (anthropic.Vertex{Project: "p", Location: "global"}).BaseURL(); got !=
		"https://aiplatform.googleapis.com" {
		t.Errorf("global BaseURL = %q; the global endpoint carries no regional prefix", got)
	}
}

// TestResolveVertexReadsTheRegionOutOfTheHost: a regional host already says
// which region it is, so requiring a second config field to repeat it would
// make the switch two changes instead of one — and a mismatched pair is a 404.
func TestResolveVertexReadsTheRegionOutOfTheHost(t *testing.T) {
	none := func(string) string { return "" }
	env := provider.Env{Override: map[string]string{"GOOGLE_CLOUD_PROJECT": "p"}, Getenv: none}

	v, err := anthropic.ResolveVertex("https://europe-west1-aiplatform.googleapis.com", "", "", env)
	if err != nil {
		t.Fatal(err)
	}
	if v.Project != "p" || v.Location != "europe-west1" {
		t.Fatalf("ResolveVertex = %+v, want {p europe-west1}", v)
	}

	if v, err := anthropic.ResolveVertex(anthropic.DefaultBaseURL, "", "",
		provider.Env{Getenv: none}); err != nil || v.On() {
		t.Fatalf("ResolveVertex(default base) = %+v, %v; want the zero deployment", v, err)
	}
}

// TestASelectedVertexDeploymentWithNoProjectFailsSaying So. The alternative is
// the direct path on a Vertex host: a 404 several layers down whose message
// names neither Vertex nor the missing project.
func TestASelectedVertexDeploymentWithNoProjectFailsSayingSo(t *testing.T) {
	none := func(string) string { return "" }
	_, err := anthropic.ResolveVertex("", "", "",
		provider.Env{Override: map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}, Getenv: none})
	if err == nil {
		t.Fatal("a selected deployment with no project must be an error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_VERTEX_PROJECT_ID") {
		t.Fatalf("error = %q; it must name the variable that fixes it", err)
	}

	// And the failure reaches the caller through the stream, not as a silent
	// fallback to api.anthropic.com (REQ-PROV-04).
	var reached bool
	req := core.Request{Options: core.RequestOptions{
		Env: map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"},
		Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			reached = true
			return nil, io.EOF
		}),
	}}
	msg := anthropic.Provider(anthropic.Options{Getenv: none}).
		Stream(context.Background(), testModel(), req, core.ProviderStreamOptions{}).Result()
	if reached {
		t.Fatal("a misconfigured deployment must not send a request")
	}
	if msg == nil || msg.ErrorMessage == "" {
		t.Fatalf("result = %+v, want an error message", msg)
	}
}
