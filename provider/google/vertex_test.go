package google_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/google"
)

// ------------------------------------------------------------ test transport

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// sseOK is a complete, minimal generateContent stream. A well-formed response
// matters even when the test only inspects the request: a malformed one makes
// the provider retry and the transport observe the same request twice.
func sseOK() *http.Response {
	const body = "data: {\"candidates\":[{\"content\":{\"role\":\"model\"," +
		"\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	return &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(body))}
}

// callURL drives one request through the provider and returns the URL the
// transport saw. No network, no key (NFR-TEST-01).
func callURL(t *testing.T, opts google.Options, m *core.Model, env map[string]string) string {
	t.Helper()
	var got string
	req := core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Options: core.RequestOptions{
			Env: env,
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				got = r.URL.String()
				return sseOK(), nil
			}),
		},
	}
	if opts.Getenv == nil {
		opts.Getenv = func(string) string { return "" }
	}
	google.Provider(opts).Stream(context.Background(), m, req, core.ProviderStreamOptions{}).Result()
	return got
}

// TestSwitchingToVertexIsAConfigChangeNotAProviderSwap is NFR-COMPAT-05.
//
// The requirement is not "Vertex works" — it is that the SAME wire API
// implementation serves both deployments and that moving between them touches
// configuration only. Each case below changes one piece of config and nothing
// else; the body shape is identical in all of them, which is why there is no
// second provider.
func TestSwitchingToVertexIsAConfigChangeNotAProviderSwap(t *testing.T) {
	const vertexPath = "/v1/projects/proj-1/locations/us-central1/publishers/google/" +
		"models/gemini-x:streamGenerateContent?alt=sse"

	cases := []struct {
		name string
		opts google.Options
		env  map[string]string
		want string
	}{
		{
			name: "the default is the API-key deployment",
			opts: google.Options{},
			env:  map[string]string{"GEMINI_API_KEY": "k"},
			want: "https://generativelanguage.googleapis.com/v1beta/models/" +
				"gemini-x:streamGenerateContent?alt=sse",
		},
		{
			name: "a project on Options is the whole switch, host included",
			opts: google.Options{VertexProject: "proj-1", VertexLocation: "us-central1"},
			want: "https://us-central1-aiplatform.googleapis.com" + vertexPath,
		},
		{
			// The ADC deployment: no key at all, coordinates from the
			// environment the instance already carries. This must not regress
			// REQ-AUTH-04 — an ambient credential is not "no credential".
			name: "a Vertex base URL takes its project and region from the environment",
			opts: google.Options{},
			env: map[string]string{
				"GOOGLE_GEMINI_BASE_URL":         "https://us-central1-aiplatform.googleapis.com",
				"GOOGLE_CLOUD_PROJECT":           "proj-1",
				"GOOGLE_APPLICATION_CREDENTIALS": "/var/run/adc.json",
			},
			want: "https://us-central1-aiplatform.googleapis.com" + vertexPath,
		},
		{
			// GOOGLE_CLOUD_PROJECT is set on every GCE and Cloud Run instance
			// and is also what VendorAuth.Ambient reads. If it flipped the
			// deployment, every AI Studio API-key deployment that happens to
			// run on Google Cloud would start sending Vertex paths.
			name: "the ambient environment alone does not flip the deployment",
			opts: google.Options{},
			env: map[string]string{
				"GEMINI_API_KEY":       "k",
				"GOOGLE_CLOUD_PROJECT": "proj-1",
			},
			want: "https://generativelanguage.googleapis.com/v1beta/models/" +
				"gemini-x:streamGenerateContent?alt=sse",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := callURL(t, c.opts, model(), c.env); got != c.want {
				t.Errorf("request URL =\n  %s\nwant\n  %s", got, c.want)
			}
		})
	}
}

// TestTheVertexPathShapeIsSpelledOut pins both path shapes directly, including
// the non-streaming verb the streaming test cannot reach.
func TestTheVertexPathShapeIsSpelledOut(t *testing.T) {
	v := google.Vertex{Project: "p", Location: "europe-west4"}
	if got, want := v.Path(model(), false),
		"/v1/projects/p/locations/europe-west4/publishers/google/models/gemini-x:generateContent"; got != want {
		t.Errorf("Vertex Path(non-stream) = %q, want %q", got, want)
	}
	if got, want := v.Path(model(), true),
		"/v1/projects/p/locations/europe-west4/publishers/google/models/"+
			"gemini-x:streamGenerateContent?alt=sse"; got != want {
		t.Errorf("Vertex Path(stream) = %q; without alt=sse the endpoint answers with an "+
			"incremental JSON array and an SSE reader waits forever", got)
	}
	if got, want := google.Path(model(), false), "/v1beta/models/gemini-x:generateContent"; got != want {
		t.Errorf("the zero Vertex must keep the AI Studio shape: got %q, want %q", got, want)
	}
	if got := (google.Vertex{Location: "global"}).BaseURL(); got != "https://aiplatform.googleapis.com" {
		t.Errorf("global BaseURL = %q; the global endpoint carries no regional prefix", got)
	}
}

// TestResolveVertexReadsTheRegionOutOfTheHost: a regional host already says
// which region it is, so requiring a second config field to repeat it would
// make the switch two changes instead of one — and a mismatched pair is a 404.
func TestResolveVertexReadsTheRegionOutOfTheHost(t *testing.T) {
	env := provider.Env{Override: map[string]string{"GOOGLE_CLOUD_PROJECT": "p"}}

	v, err := google.ResolveVertex("https://europe-west4-aiplatform.googleapis.com", "", "", env)
	if err != nil {
		t.Fatal(err)
	}
	if v.Project != "p" || v.Location != "europe-west4" {
		t.Fatalf("ResolveVertex = %+v, want {p europe-west4}", v)
	}

	// The global host names no region; "global" is the only location that
	// endpoint serves.
	v, err = google.ResolveVertex("https://aiplatform.googleapis.com", "", "", env)
	if err != nil {
		t.Fatal(err)
	}
	if v.Location != google.VertexGlobalLocation {
		t.Fatalf("location = %q, want %q", v.Location, google.VertexGlobalLocation)
	}

	// No project anywhere is a configuration error with a message that says
	// so, not an AI Studio path sent to a Vertex host and a 404 three layers
	// down that mentions neither.
	if _, err := google.ResolveVertex("https://aiplatform.googleapis.com", "", "",
		provider.Env{Override: map[string]string{}, Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("a Vertex host with no project must be an error")
	}

	if v, err := google.ResolveVertex(google.DefaultBaseURL, "", "",
		provider.Env{Getenv: func(string) string { return "" }}); err != nil || v.On() {
		t.Fatalf("ResolveVertex(default base) = %+v, %v; want the zero deployment", v, err)
	}
}
