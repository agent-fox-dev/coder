package provider_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/provider"
)

// roundTripFunc is the offline seam. Every transport test drives a real
// http.Client through a canned RoundTripper, so the policy is exercised
// end-to-end with no network and no key.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// noWait is a policy whose sleeps are instant and whose jitter is fixed, so
// the tests assert on the DECISIONS rather than on the clock.
func noWait(maxRetries int, slept *[]time.Duration) provider.RetryPolicy {
	return provider.RetryPolicy{
		MaxRetries: maxRetries,
		Rand:       func() float64 { return 0 },
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
		Sleep: func(_ context.Context, d time.Duration) error {
			if slept != nil {
				*slept = append(*slept, d)
			}
			return nil
		},
	}
}

func post(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	}
}

func TestTransportDefaultsToASingleAttempt(t *testing.T) {
	// REQ-PROV-13: default MaxRetries is 0. Retry policy is owned above the
	// transport, and a hidden default here would compound with the semantic
	// layer's own budget — 4 x 4 attempts against one turn's cost ceiling.
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return respond(503, "overloaded", nil), nil
	})}

	resp, err := provider.Do(context.Background(), hc, post("http://x/y"), provider.RetryPolicy{
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Fatalf("made %d attempts with the default policy, want 1", got)
	}
}

func TestRetryableStatuses(t *testing.T) {
	for _, c := range []struct {
		status int
		want   int32 // total attempts with MaxRetries=1
	}{
		{200, 1}, {400, 1}, {401, 1}, {404, 1}, {422, 1},
		{408, 2}, {409, 2}, {429, 2}, {500, 2}, {502, 2}, {503, 2}, {524, 2},
	} {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			var calls atomic.Int32
			hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return respond(c.status, "", nil), nil
			})}
			resp, err := provider.Do(context.Background(), hc, post("http://x/y"), noWait(1, nil))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if got := calls.Load(); got != c.want {
				t.Fatalf("status %d made %d attempts, want %d", c.status, got, c.want)
			}
		})
	}
}

// TestShouldRetryHeaderOverridesInBothDirections is the half of REQ-PROV-13
// that an "add retries" reading drops.
//
// A 503 with x-should-retry:false must NOT be retried. An override that only
// ever adds retries implements the easy direction and leaves a client
// hammering a server that has explicitly said not to — which is how a partial
// outage becomes a total one.
func TestShouldRetryHeaderOverridesInBothDirections(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header string
		want   int32
	}{
		{"503 says do not retry", 503, "false", 1},
		{"200 says retry", 200, "true", 2},
		{"429 says do not retry", 429, "false", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return respond(c.status, "", map[string]string{"x-should-retry": c.header}), nil
			})}
			resp, err := provider.Do(context.Background(), hc, post("http://x/y"), noWait(1, nil))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if got := calls.Load(); got != c.want {
				t.Fatalf("%s: %d attempts, want %d", c.name, got, c.want)
			}
		})
	}
}

func TestServerDictatedDelayWinsOverBackoff(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"retry-after-ms is read first", map[string]string{
			"retry-after-ms": "1500", "Retry-After": "30"}, 1500 * time.Millisecond},
		{"Retry-After as seconds", map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{"Retry-After as an HTTP date", map[string]string{
			"Retry-After": time.Unix(1700000000, 0).UTC().Add(12 * time.Second).Format(http.TimeFormat)},
			12 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var slept []time.Duration
			hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return respond(429, "", c.headers), nil
			})}
			resp, err := provider.Do(context.Background(), hc, post("http://x/y"), noWait(1, &slept))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if len(slept) != 1 || slept[0] != c.want {
				t.Fatalf("slept %v, want exactly [%v]: a server-dictated delay wins over "+
					"computed backoff, and the read order is retry-after-ms > seconds > date",
					slept, c.want)
			}
		})
	}
}

// TestOverlongServerDelayIsAbandonedNotClamped is the requirement's sharpest
// edge, and the one every "clamp it and carry on" implementation gets wrong.
//
// A Retry-After of an hour is not a transient throttle. Clamping it to the 60s
// ceiling and sleeping guarantees a second failure after a minute of holding a
// connection and the caller's deadline; the typed error hands the decision
// back where it belongs.
func TestOverlongServerDelayIsAbandonedNotClamped(t *testing.T) {
	var slept []time.Duration
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return respond(429, "", map[string]string{"Retry-After": "3600"}), nil
	})}
	p := noWait(3, &slept)
	p.MaxRetryDelay = 60 * time.Second

	resp, err := provider.Do(context.Background(), hc, post("http://x/y"), p)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, provider.ErrRetryDelayTooLong) {
		t.Fatalf("err = %v, want ErrRetryDelayTooLong", err)
	}
	if len(slept) != 0 {
		t.Fatalf("slept %v; the request must be abandoned, not clamped to the ceiling "+
			"and slept", slept)
	}
}

func TestBackoffIsExponentialCappedAndJitteredDownwardOnly(t *testing.T) {
	var slept []time.Duration
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return respond(500, "", nil), nil
	})}
	p := noWait(6, &slept)
	p.Rand = func() float64 { return 0.99 } // maximum jitter

	resp, err := provider.Do(context.Background(), hc, post("http://x/y"), p)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	want := []time.Duration{500, 1000, 2000, 4000, 8000, 8000}
	if len(slept) != len(want) {
		t.Fatalf("slept %d times, want %d: %v", len(slept), len(want), slept)
	}
	for i, base := range want {
		b := base * time.Millisecond
		if slept[i] > b {
			t.Fatalf("attempt %d slept %v, LONGER than the %v base: jitter is downward only",
				i, slept[i], b)
		}
		if slept[i] < b*3/4 {
			t.Fatalf("attempt %d slept %v, below the 25%%-off floor of %v", i, slept[i], b*3/4)
		}
	}
}

// TestARetriedRequestGetsAFreshBody is why Do takes a factory.
//
// A *http.Request whose body has been consumed replays as an empty body. The
// server then answers the retry with a 400 that looks nothing like the 503
// that caused it, and the bug reads as "the provider rejects our schema".
func TestARetriedRequestGetsAFreshBody(t *testing.T) {
	var bodies []string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) < 2 {
			return respond(503, "", nil), nil
		}
		return respond(200, "ok", nil), nil
	})}

	resp, err := provider.Do(context.Background(), hc, func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "http://x/y", strings.NewReader(`{"hello":1}`))
	}, noWait(2, nil))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("bodies = %q; the retried request must carry the same payload as the first",
			bodies)
	}
}

func TestCancelledRequestIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.Canceled
	})}

	_, err := provider.Do(ctx, hc, post("http://x/y"), noWait(4, nil))
	if err == nil {
		t.Fatal("want an error from a cancelled request")
	}
	if got := calls.Load(); got > 1 {
		t.Fatalf("made %d attempts on a cancelled context, want at most 1: retrying a "+
			"cancellation turns one abort into five", got)
	}
}
