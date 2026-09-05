package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This file is REQ-PROV-13, the TRANSPORT retry layer.
//
// It is one of the two independent layers of REQ-PROV-06, and the split is not
// redundancy. This layer sees status codes, `x-should-retry` and `Retry-After`
// and nothing else; the semantic layer (RetryMiddleware, REQ-PROV-14) sees a
// completed AssistantMessage and classifies its prose. Neither subsumes the
// other: a truncated SSE body is a 200 here and an error there, and an
// out-of-credit 429 is retryable here and denylisted there.

// ErrRetryDelayTooLong is REQ-PROV-13's typed abandonment.
//
// A server-dictated delay above the ceiling is NOT clamped and NOT slept. A
// `Retry-After: 3600` is not a transient throttle — it is the server saying
// come back in an hour — and sleeping an hour inside a request holds a
// connection, a goroutine and the caller's deadline hostage to a wait that was
// never going to be worth it. Failing immediately with a typed error lets the
// caller decide; clamping to 60s guarantees a second failure.
var ErrRetryDelayTooLong = errors.New("agentkit: server-dictated retry delay exceeds the ceiling")

// RetryPolicy configures Do.
type RetryPolicy struct {
	// MaxRetries counts RETRIES, not attempts: 0 means a single attempt, which
	// is the REQ-PROV-13 default. Retry policy is owned above the transport.
	MaxRetries int
	// MaxRetryDelay is the ceiling above which a server-dictated delay is
	// abandoned rather than slept. Zero means 60s.
	MaxRetryDelay time.Duration
	BaseDelay     time.Duration // zero means 500ms
	MaxDelay      time.Duration // zero means 8s

	// Sleep, Rand and Now are injectable so the whole policy is testable
	// without waiting and without a clock (NFR-TEST-04).
	Sleep func(ctx context.Context, d time.Duration) error
	Rand  func() float64
	Now   func() time.Time
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxRetryDelay == 0 {
		p.MaxRetryDelay = 60 * time.Second
	}
	if p.BaseDelay == 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay == 0 {
		p.MaxDelay = 8 * time.Second
	}
	if p.Sleep == nil {
		p.Sleep = sleepCtx
	}
	if p.Rand == nil {
		p.Rand = rand.Float64
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Do issues an HTTP request with REQ-PROV-13's retry policy.
//
// newReq is a FACTORY, not a request. A retried request needs a fresh body
// reader, and a *http.Request whose body has been consumed replays as empty —
// which the server answers with a 400 that looks nothing like the 503 that
// caused the retry.
func Do(ctx context.Context, hc *http.Client, newReq func() (*http.Request, error), p RetryPolicy) (*http.Response, error) {
	p = p.withDefaults()
	if hc == nil {
		hc = http.DefaultClient
	}

	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req.WithContext(ctx))

		if err != nil {
			// A transport error carries no headers, so status logic cannot
			// apply. Context cancellation is terminal: retrying a cancelled
			// request cannot succeed and turns one abort into MaxRetries+1.
			if ctx.Err() != nil {
				return nil, err
			}
			if attempt >= p.MaxRetries {
				return nil, err
			}
			if serr := p.Sleep(ctx, backoffDelay(p, attempt)); serr != nil {
				return nil, serr
			}
			continue
		}

		retry, serverDelay, derr := shouldRetry(resp, p)
		if derr != nil {
			drain(resp)
			return nil, derr
		}
		if !retry || attempt >= p.MaxRetries {
			return resp, nil
		}

		d := backoffDelay(p, attempt)
		if serverDelay > 0 {
			// A server-dictated delay WINS over computed backoff. The server
			// knows when its window reopens; our exponential curve is a guess.
			d = serverDelay
		}
		drain(resp)
		if serr := p.Sleep(ctx, d); serr != nil {
			return nil, serr
		}
	}
}

// shouldRetry applies the status policy and reads the server-dictated delay.
//
// The returned error is the REQ-PROV-13 abandonment: a delay above the ceiling
// fails the request outright instead of being clamped.
func shouldRetry(resp *http.Response, p RetryPolicy) (bool, time.Duration, error) {
	delay, ok := serverDelay(resp.Header, p.Now())
	if ok && delay > p.MaxRetryDelay {
		return false, 0, fmt.Errorf("%w: server asked for %s, ceiling is %s (status %d)",
			ErrRetryDelayTooLong, delay, p.MaxRetryDelay, resp.StatusCode)
	}

	// x-should-retry OVERRIDES the status logic ENTIRELY, in BOTH directions.
	// A 200 with `x-should-retry: true` is retried and a 503 with
	// `x-should-retry: false` is not — an override that only ever adds retries
	// implements half the requirement, and the half it drops is the one that
	// stops a client hammering a server that has already said not to.
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("x-should-retry"))) {
	case "true":
		return true, delay, nil
	case "false":
		return false, 0, nil
	}

	switch {
	case resp.StatusCode == 408, resp.StatusCode == 409, resp.StatusCode == 429:
		return true, delay, nil
	case resp.StatusCode >= 500:
		return true, delay, nil
	}
	return false, 0, nil
}

// serverDelay reads the dictated delay in REQ-PROV-13's order: retry-after-ms,
// then Retry-After as seconds, then Retry-After as an HTTP date.
func serverDelay(h http.Header, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		// A date already in the past means "retry now", which is a dictated
		// delay of zero — not an absent header that falls back to backoff.
		return 0, true
	}
	return 0, false
}

// backoffDelay is min(500ms * 2^attempt, 8s) with up to 25% DOWNWARD jitter.
//
// Jitter never lengthens a delay. Upward jitter lets a retry storm drift past
// the caller's deadline, so the same policy that was meant to smooth load
// converts a recoverable throttle into a timeout.
func backoffDelay(p RetryPolicy, attempt int) time.Duration {
	d := p.BaseDelay
	for i := 0; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	return time.Duration(float64(d) * (1 - 0.25*p.Rand()))
}

// drain reads and closes a response body being discarded, so the connection
// returns to the pool instead of being torn down. A retry loop that closes
// without draining opens a fresh TCP connection per attempt, which is the
// opposite of what backing off is for.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
