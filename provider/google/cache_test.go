package google_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/schema"
)

// bigSystem is a system prompt over §6.2a's 32k-token threshold. The estimate
// is the 4:1 heuristic, so this is ~34k tokens.
func bigSystem() []core.ContentBlock {
	return []core.ContentBlock{core.TextBlock{Text: strings.Repeat("context ", 17408)}}
}

func cacheRequest(t *testing.T, rt http.RoundTripper) core.Request {
	t.Helper()
	return core.Request{
		System:   bigSystem(),
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Tools: []core.ToolWire{{Name: "read_file", Description: "Read a file",
			InputSchema: schema.Object(schema.Prop("path", schema.String("path")))}},
		Options: core.RequestOptions{
			Env:       map[string]string{"GEMINI_API_KEY": "k"},
			Transport: rt,
		},
	}
}

func isCacheCreate(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/cachedContents") }

// TestTheAgentLoopNeverWaitsForCachedContentCreation is NFR-PERF-08's
// structural half, restored from design guidance now that CachedContent ships.
//
// "Creation runs on a background goroutine before the first model call. The
// agent loop must not block waiting for it — it falls back to uncached
// operation if creation has not completed."
//
// The creation call below NEVER RETURNS. An implementation that creates the
// resource and then sends the request — the obvious one, and the one that
// makes the first turn of every session two serial round trips — hangs here
// until the test binary's timeout kills it, with no assertion message at all.
// A pass means the model request went out, uncached, while creation was still
// in flight.
func TestTheAgentLoopNeverWaitsForCachedContentCreation(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) }) // release the parked goroutine

	reached := make(chan struct{}, 1)
	var body []byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if isCacheCreate(r) {
			reached <- struct{}{}
			<-blocked
			return nil, io.ErrUnexpectedEOF
		}
		body, _ = io.ReadAll(r.Body)
		return sseOK(), nil
	})

	p := google.Provider(google.Options{
		Getenv:       func(string) string { return "" },
		ContextCache: &google.ContextCacheOptions{},
	})
	st := p.Stream(context.Background(), model(), cacheRequest(t, rt), core.ProviderStreamOptions{})
	st.Result()
	if err := st.Err(); err != nil {
		t.Fatalf("the turn failed while creation was in flight: %v", err)
	}

	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("creation never started; NFR-PERF-08 requires it to be launched " +
			"before the first model call, not after it")
	}

	var w struct {
		CachedContent     string          `json:"cachedContent"`
		SystemInstruction json.RawMessage `json:"systemInstruction"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	if w.CachedContent != "" {
		t.Errorf("cachedContent = %q on a turn whose resource does not exist yet", w.CachedContent)
	}
	if len(w.SystemInstruction) == 0 {
		t.Error("the fallback is UNCACHED operation, which means the system prompt still " +
			"rides in the request; withholding it here sends the model no instructions at all")
	}
}

// TestACreatedCachedContentIsReferencedAndThePrefixWithheld is §6.2a Level 1's
// Gemini arm: once the resource exists it is referenced, and the fields it
// carries are withheld — sending both is rejected outright.
func TestACreatedCachedContentIsReferencedAndThePrefixWithheld(t *testing.T) {
	created := make(chan string, 1)
	var createBody, second []byte
	turn := 0

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if isCacheCreate(r) {
			createBody, _ = io.ReadAll(r.Body)
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"name":"cachedContents/abc123"}`))}, nil
		}
		turn++
		if turn == 2 {
			second, _ = io.ReadAll(r.Body)
		}
		return sseOK(), nil
	})

	p := google.Provider(google.Options{
		Getenv: func(string) string { return "" },
		ContextCache: &google.ContextCacheOptions{
			OnCreate: func(name string, err error) {
				if err != nil {
					t.Errorf("creation failed: %v", err)
				}
				created <- name
			},
		},
	})

	req := cacheRequest(t, rt)
	p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("creation never completed")
	}
	p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()

	var c struct {
		Model             string          `json:"model"`
		TTL               string          `json:"ttl"`
		SystemInstruction json.RawMessage `json:"systemInstruction"`
		Tools             json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(createBody, &c); err != nil {
		t.Fatal(err)
	}
	if c.Model != "models/gemini-x" {
		t.Errorf("creation model = %q, want the models/ resource name", c.Model)
	}
	if c.TTL != "3600s" {
		t.Errorf("ttl = %q, want 3600s: context_cache_ttl defaults to 60 minutes (§6.2a)", c.TTL)
	}
	if len(c.SystemInstruction) == 0 || len(c.Tools) == 0 {
		t.Error("the resource must carry everything the request then withholds")
	}

	var w struct {
		CachedContent     string          `json:"cachedContent"`
		SystemInstruction json.RawMessage `json:"systemInstruction"`
		Tools             json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(second, &w); err != nil {
		t.Fatal(err)
	}
	if w.CachedContent != "cachedContents/abc123" {
		t.Fatalf("cachedContent = %q on the turn after creation, want the resource name",
			w.CachedContent)
	}
	if len(w.SystemInstruction) != 0 || len(w.Tools) != 0 {
		t.Error("a request may not carry cachedContent AND the fields the resource holds")
	}
}

// TestTheContextCacheIsOffByDefaultAndBelowTheThreshold: the feature costs a
// request and a provider-side resource, so it is opt-in, and even opted in it
// does not fire under §6.2a's 32k-token threshold, where implicit caching
// already applies at no cost.
func TestTheContextCacheIsOffByDefaultAndBelowTheThreshold(t *testing.T) {
	for _, c := range []struct {
		name   string
		opts   google.Options
		system []core.ContentBlock
	}{
		{"off by default", google.Options{}, bigSystem()},
		{"below the threshold", google.Options{ContextCache: &google.ContextCacheOptions{}},
			[]core.ContentBlock{core.TextBlock{Text: "you are a helpful assistant"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			creates := make(chan struct{}, 4)
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if isCacheCreate(r) {
					creates <- struct{}{}
					return &http.Response{StatusCode: 200,
						Header: http.Header{"Content-Type": []string{"application/json"}},
						Body:   io.NopCloser(strings.NewReader(`{"name":"cachedContents/x"}`))}, nil
				}
				return sseOK(), nil
			})
			opts := c.opts
			opts.Getenv = func(string) string { return "" }
			req := cacheRequest(t, rt)
			req.System = c.system
			google.Provider(opts).Stream(context.Background(), model(), req,
				core.ProviderStreamOptions{}).Result()

			select {
			case <-creates:
				t.Fatal("a CachedContent resource was created")
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

// TestAnExpiredCachedContentIsForgottenAndRecreated is the local half of the
// resource's lifetime. The service deletes a CachedContent when its TTL runs
// out and says nothing; an entry that never expired here kept referencing it,
// and every turn after the TTL was a 400 for a cache the caller had opted into
// an hour earlier. An expired entry reads as absent and creation runs AGAIN —
// which a sync.Once cannot do.
func TestAnExpiredCachedContentIsForgottenAndRecreated(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := make(chan string, 4)
	var creates int
	var bodies [][]byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if isCacheCreate(r) {
			creates++
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"name":"cachedContents/c` +
					strings.Repeat("x", creates) + `","expireTime":"` +
					now.Add(10*time.Minute).Format(time.RFC3339) + `"}`))}, nil
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		return sseOK(), nil
	})
	p := google.Provider(google.Options{
		Getenv: func(string) string { return "" },
		Now:    func() time.Time { return now },
		ContextCache: &google.ContextCacheOptions{
			OnCreate: func(name string, err error) { created <- name },
		},
	})
	req := cacheRequest(t, rt)
	turn := func() {
		t.Helper()
		p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	}
	wait := func() string {
		t.Helper()
		select {
		case n := <-created:
			return n
		case <-time.After(2 * time.Second):
			t.Fatal("creation never completed")
			return ""
		}
	}
	cachedContentOf := func(i int) string {
		var w struct {
			CachedContent string `json:"cachedContent"`
		}
		_ = json.Unmarshal(bodies[i], &w)
		return w.CachedContent
	}

	turn()
	first := wait()
	turn()
	if cachedContentOf(1) != first {
		t.Fatalf("turn 2 referenced %q, want the created resource %q", cachedContentOf(1), first)
	}

	// The service's expireTime passes.
	now = now.Add(11 * time.Minute)
	turn()
	if cachedContentOf(2) != "" {
		t.Fatalf("turn 3 referenced %q after the resource's expireTime; the service has "+
			"deleted it and the request is a 400", cachedContentOf(2))
	}
	second := wait()
	if second == first || second == "" {
		t.Fatalf("no second creation ran after expiry (got %q, first was %q)", second, first)
	}
	turn()
	if cachedContentOf(3) != second {
		t.Fatalf("turn 4 referenced %q, want the recreated resource %q", cachedContentOf(3), second)
	}
	if creates != 2 {
		t.Fatalf("%d creations, want exactly 2: one per lifetime, never one per turn", creates)
	}
}

// TestACachedContentTheServiceRefusesIsClearedAndTheTurnRetriedOnce: a
// resource deleted under us — or one the service will not pair with this
// request — is a 4xx on a request that was otherwise fine. The entry is
// cleared so the next turn recreates, and THIS turn goes out again without
// the reference rather than failing for a cache it never needed.
func TestACachedContentTheServiceRefusesIsClearedAndTheTurnRetriedOnce(t *testing.T) {
	created := make(chan string, 4)
	var bodies [][]byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if isCacheCreate(r) {
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"name":"cachedContents/gone"}`))}, nil
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		if strings.Contains(string(b), "cachedContents/gone") {
			return &http.Response{StatusCode: 400, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"CachedContent not found"}}`))}, nil
		}
		return sseOK(), nil
	})
	p := google.Provider(google.Options{
		Getenv: func(string) string { return "" },
		ContextCache: &google.ContextCacheOptions{
			OnCreate: func(name string, err error) { created <- name },
		},
	})
	req := cacheRequest(t, rt)
	p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("creation never completed")
	}

	msg := p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	if msg.StopReason == core.StopReasonError {
		t.Fatalf("the turn failed instead of being retried without the refused reference: %s",
			msg.ErrorMessage)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d model requests, want 3: the first turn, the refused cached one, and its "+
			"retry", len(bodies))
	}
	var retry struct {
		CachedContent     string          `json:"cachedContent"`
		SystemInstruction json.RawMessage `json:"systemInstruction"`
	}
	if err := json.Unmarshal(bodies[2], &retry); err != nil {
		t.Fatal(err)
	}
	if retry.CachedContent != "" {
		t.Fatalf("the retry still referenced %q", retry.CachedContent)
	}
	if len(retry.SystemInstruction) == 0 {
		t.Fatal("the retry withheld the system prompt the resource was supposed to carry")
	}

	// The entry is cleared: the next turn goes out uncached and creation
	// runs again, rather than referencing the refused resource forever.
	p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	var third struct {
		CachedContent string `json:"cachedContent"`
	}
	if err := json.Unmarshal(bodies[3], &third); err != nil {
		t.Fatal(err)
	}
	if third.CachedContent != "" {
		t.Fatalf("the turn after the refusal still referenced %q", third.CachedContent)
	}
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("no recreation was started after the service refused the resource")
	}
}
