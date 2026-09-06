package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// This file is REQ-AUTH-05, -06 and -07.
//
// The failure REQ-AUTH-06 describes is worth restating because it is not
// obvious and it is expensive: without a double-checked refresh, N concurrent
// turns arriving on an expired token each POST the same refresh token, the
// provider rotates it N times, and N-1 turns are left holding a credential the
// provider has already invalidated. The session does not fail cleanly — it
// fails N-1 times out of N, intermittently, and looks like a flaky provider.

// Credential is what a store holds for one vendor.
//
// It carries both shapes because a vendor can use either and some use both: a
// long-lived API key, or an OAuth pair with an expiry. BaseURL and Headers are
// here for the same reason they are on ModelAuth (REQ-AUTH-01) — a gateway may
// carry the credential in the URL or in a header of its own choosing.
type Credential struct {
	APIKey       string
	AccessToken  string
	RefreshToken string
	// ExpiresAt is zero for a credential that does not expire. A zero value is
	// NOT "expired at the epoch": treating it that way refreshes an API key
	// that has no refresh flow, on every single turn.
	ExpiresAt time.Time
	BaseURL   string
	Headers   map[string]*string
	// Scheme decides how a token rides on the wire when this credential is
	// converted to a ModelAuth.
	Scheme AuthScheme
}

// String is redacted. REQ-AUTH-07 puts the redaction boundary HERE, at the
// type that holds the secret, rather than at the log call sites — because the
// ways a credential actually escapes are `%v` in a log line, `%w` in an error
// and a struct dump in a test failure, and none of those consult a helper
// somebody remembered to call.
func (c Credential) String() string {
	return fmt.Sprintf("Credential{key:%s access:%s refresh:%s expires:%s base_url:%s}",
		Redact(c.APIKey), Redact(c.AccessToken), Redact(c.RefreshToken),
		expiryText(c.ExpiresAt), c.BaseURL)
}

func expiryText(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// Empty reports whether this credential carries nothing usable.
func (c Credential) Empty() bool {
	return c.APIKey == "" && c.AccessToken == "" && c.BaseURL == "" && len(c.Headers) == 0
}

// NeedsRefresh implements REQ-AUTH-06's validity FLOOR.
//
// The floor is why this is not `now.After(ExpiresAt)`. A token with forty
// seconds left passes a bare expiry check and then expires in flight — after
// the request is sent, so the failure arrives as a 401 on a turn that was
// already paid for. The floor refreshes it before it is spent.
func (c Credential) NeedsRefresh(now time.Time, floor time.Duration) bool {
	if c.ExpiresAt.IsZero() {
		return false // a credential with no expiry has no refresh flow
	}
	return !now.Add(floor).Before(c.ExpiresAt)
}

// Auth converts a stored credential into REQ-AUTH-01's ModelAuth.
func (c Credential) Auth() ModelAuth {
	a := ModelAuth{BaseURL: c.BaseURL, Headers: map[string]*string{}, Source: "credential-store"}
	for k, v := range c.Headers {
		a.Headers[k] = v
	}
	token := c.AccessToken
	if token == "" {
		token = c.APIKey
	}
	if token != "" {
		a.APIKey = token
		a.State = CredentialResolved
		switch c.Scheme {
		case SchemeBearer:
			a.Headers["Authorization"] = strp("Bearer " + token)
		default:
			a.Headers["x-api-key"] = strp(token)
		}
		return a
	}
	if c.BaseURL != "" || len(c.Headers) > 0 {
		a.State = CredentialAmbient
	}
	return a
}

// CredentialStore is the APPLICATION-OWNED storage interface (REQ-AUTH-05).
//
// It is deliberately just load and save. Serialization is not the
// application's problem to re-solve — Credentials provides it — and an
// interface that also demanded correct per-vendor locking would be an
// interface every embedder implements slightly wrong.
type CredentialStore interface {
	Load(ctx context.Context, vendorID string) (Credential, error)
	Save(ctx context.Context, vendorID string, c Credential) error
}

// MemoryStore is an in-process CredentialStore, for tests and for embedders
// with nothing to persist to.
type MemoryStore struct {
	mu sync.Mutex
	by map[string]Credential
}

func (m *MemoryStore) Load(_ context.Context, vendorID string) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.by[vendorID], nil
}

func (m *MemoryStore) Save(_ context.Context, vendorID string, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string]Credential{}
	}
	m.by[vendorID] = c
	return nil
}

// Credentials wraps a store with REQ-AUTH-05's serialization.
type Credentials struct {
	store CredentialStore
	locks *keyedLocks
}

func NewCredentials(store CredentialStore) *Credentials {
	if store == nil {
		store = &MemoryStore{}
	}
	return &Credentials{store: store, locks: newKeyedLocks()}
}

// Get reads without taking the vendor lock.
//
// Reading under the lock would serialize every turn behind every refresh, and
// the overwhelmingly common case — a valid token — needs no coordination at
// all.
func (c *Credentials) Get(ctx context.Context, vendorID string) (Credential, error) {
	return c.store.Load(ctx, vendorID)
}

// ErrCredentialUnchanged lets a Modify callback decline to write.
var ErrCredentialUnchanged = errors.New("agentkit: credential unchanged")

// Modify is the ONLY write path (REQ-AUTH-05), serialized per vendor id.
//
// The callback receives the CURRENT value, read inside the lock. That is what
// makes a double-checked refresh possible at all: a caller that read before
// taking the lock cannot tell whether someone else has since written.
//
// Returning ErrCredentialUnchanged commits nothing and is not an error to the
// caller. Without it, "check and decide not to write" would have to be
// expressed as a write of the same value, which races with a concurrent
// refresh in exactly the way this is meant to prevent.
func (c *Credentials) Modify(ctx context.Context, vendorID string,
	fn func(Credential) (Credential, error)) (Credential, error) {

	unlock := c.locks.lock(vendorID)
	defer unlock()

	cur, err := c.store.Load(ctx, vendorID)
	if err != nil {
		return Credential{}, err
	}
	next, err := fn(cur)
	if errors.Is(err, ErrCredentialUnchanged) {
		return cur, nil
	}
	if err != nil {
		return Credential{}, err
	}
	if err := c.store.Save(ctx, vendorID, next); err != nil {
		return Credential{}, err
	}
	return next, nil
}

// Refresher exchanges an expiring credential for a fresh one.
type Refresher func(ctx context.Context, current Credential) (Credential, error)

// RefreshOptions configures EnsureFresh.
type RefreshOptions struct {
	// Floor is REQ-AUTH-06's validity floor. Zero means 5 minutes.
	Floor time.Duration
	// Timeout bounds the refresh call. Zero means 15 seconds.
	//
	// It exists BECAUSE the refresh holds the per-vendor lock: a refresh
	// endpoint that hangs would otherwise block every turn for that vendor for
	// as long as the caller's context allows, which on a long-running agent is
	// forever.
	Timeout time.Duration
	Now     func() time.Time
}

func (o RefreshOptions) withDefaults() RefreshOptions {
	if o.Floor == 0 {
		o.Floor = 5 * time.Minute
	}
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// EnsureFresh is REQ-AUTH-06: a DOUBLE-CHECKED refresh inside Modify.
//
// Both checks are load-bearing and they do different jobs. The first, outside
// the lock, keeps the common case — a valid token — from serializing turns
// that have nothing to coordinate. The second, inside the lock, is the
// requirement: a turn that blocked while another refreshed observes the NEW
// token and returns it, instead of refreshing again and rotating the provider's
// refresh token out from under the turn that just succeeded.
func (c *Credentials) EnsureFresh(ctx context.Context, vendorID string,
	refresh Refresher, opts RefreshOptions) (Credential, error) {

	o := opts.withDefaults()

	cur, err := c.Get(ctx, vendorID)
	if err != nil {
		return Credential{}, err
	}
	if !cur.NeedsRefresh(o.Now(), o.Floor) {
		return cur, nil
	}
	if refresh == nil {
		return cur, fmt.Errorf("agentkit: credential for %q needs refresh and no Refresher was supplied", vendorID)
	}

	return c.Modify(ctx, vendorID, func(inside Credential) (Credential, error) {
		// THE second check. Removing it is the defect the requirement names.
		if !inside.NeedsRefresh(o.Now(), o.Floor) {
			return inside, ErrCredentialUnchanged
		}
		rctx, cancel := context.WithTimeout(ctx, o.Timeout)
		defer cancel()
		return refresh(rctx, inside)
	})
}

// ResolveAuthWith consults a credential store first and falls back to the
// REQ-AUTH-03 environment table.
//
// The store wins because it is the layer that can hold a refreshed OAuth
// token; the environment is a static fallback and cannot.
func ResolveAuthWith(ctx context.Context, vendorID string, creds *Credentials,
	table VendorAuth, env Env) (ModelAuth, error) {

	if creds != nil {
		c, err := creds.Get(ctx, vendorID)
		if err != nil {
			return ModelAuth{}, err
		}
		if !c.Empty() {
			return c.Auth(), nil
		}
	}
	return ResolveAuth(table, env), nil
}

// ---------------------------------------------------------------- keyed locks

// keyedLocks is REQ-AUTH-05's REFERENCE-COUNTED per-key mutex.
//
// Refcounting is what keeps the map bounded. A map of mutexes that never
// deletes grows with the number of distinct vendor ids seen — unbounded in any
// deployment where the vendor id is derived from a request — and a map that
// deletes without counting hands two goroutines two different mutexes for the
// same key, which is not a lock at all.
type keyedLocks struct {
	mu sync.Mutex
	by map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func newKeyedLocks() *keyedLocks { return &keyedLocks{by: map[string]*keyedLock{}} }

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	l := k.by[key]
	if l == nil {
		l = &keyedLock{}
		k.by[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.by, key)
		}
		k.mu.Unlock()
	}
}

// held reports how many keys currently have a live lock. Test-facing.
func (k *keyedLocks) held() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.by)
}

// LiveLocks reports how many vendor locks are currently allocated, so a test
// can assert the refcounting actually releases them.
func (c *Credentials) LiveLocks() int { return c.locks.held() }
