package provider_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/provider"
)

func expiringCreds(t *testing.T, in time.Duration) (*provider.Credentials, *provider.MemoryStore) {
	t.Helper()
	store := &provider.MemoryStore{}
	if err := store.Save(context.Background(), "acme", provider.Credential{
		AccessToken:  "old-access-token-value",
		RefreshToken: "the-one-refresh-token",
		ExpiresAt:    time.Now().Add(in),
		Scheme:       provider.SchemeBearer,
	}); err != nil {
		t.Fatal(err)
	}
	return provider.NewCredentials(store), store
}

// TestConcurrentTurnsRefreshExactlyOnce is REQ-AUTH-06, and it is the whole
// reason the requirement exists.
//
// Without the check INSIDE the lock, N concurrent turns each POST the same
// refresh token, the provider rotates it N times, and N-1 turns are left
// holding a credential the provider has already invalidated. The session does
// not fail cleanly: it fails N-1 times out of N, intermittently, and reads as
// a flaky provider.
func TestConcurrentTurnsRefreshExactlyOnce(t *testing.T) {
	const turns = 24
	creds, _ := expiringCreds(t, time.Minute) // inside the 5-minute floor

	var refreshes atomic.Int32
	refresh := func(_ context.Context, cur provider.Credential) (provider.Credential, error) {
		n := refreshes.Add(1)
		// A real refresh takes a network round trip; without one the race is
		// hard to lose even when the code is wrong.
		time.Sleep(20 * time.Millisecond)
		cur.AccessToken = fmt.Sprintf("fresh-token-%d", n)
		cur.RefreshToken = fmt.Sprintf("rotated-refresh-%d", n)
		cur.ExpiresAt = time.Now().Add(time.Hour)
		return cur, nil
	}

	var wg sync.WaitGroup
	got := make([]provider.Credential, turns)
	errs := make([]error, turns)
	for i := 0; i < turns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = creds.EnsureFresh(context.Background(), "acme", refresh,
				provider.RefreshOptions{})
		}(i)
	}
	wg.Wait()

	if n := refreshes.Load(); n != 1 {
		t.Fatalf("%d concurrent turns performed %d refreshes, want exactly 1. Each extra "+
			"one rotates the provider's refresh token and invalidates the token another "+
			"turn is already holding.", turns, n)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("turn %d failed: %v", i, err)
		}
		if got[i].AccessToken != "fresh-token-1" {
			t.Fatalf("turn %d holds %q; every turn must observe the ONE refreshed token",
				i, got[i].AccessToken)
		}
	}
}

// TestAValidTokenIsNotRefreshed keeps the fast path fast: a store hit and no
// lock at all.
func TestAValidTokenIsNotRefreshed(t *testing.T) {
	creds, _ := expiringCreds(t, time.Hour)
	var refreshes atomic.Int32
	_, err := creds.EnsureFresh(context.Background(), "acme",
		func(context.Context, provider.Credential) (provider.Credential, error) {
			refreshes.Add(1)
			return provider.Credential{}, nil
		}, provider.RefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 0 {
		t.Fatal("a token an hour from expiry must not be refreshed")
	}
}

// TestTheValidityFloorRefreshesBeforeExpiry is why NeedsRefresh is not a bare
// `now.After(ExpiresAt)`.
//
// A token with forty seconds left passes a bare expiry check and then expires
// IN FLIGHT — after the request is sent, so the 401 lands on a turn that was
// already paid for.
func TestTheValidityFloorRefreshesBeforeExpiry(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		left  time.Duration
		floor time.Duration
		want  bool
	}{
		{"expired", -time.Minute, 5 * time.Minute, true},
		{"forty seconds left, five minute floor", 40 * time.Second, 5 * time.Minute, true},
		{"four minutes left", 4 * time.Minute, 5 * time.Minute, true},
		{"six minutes left", 6 * time.Minute, 5 * time.Minute, false},
		{"an hour left", time.Hour, 5 * time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cred := provider.Credential{ExpiresAt: now.Add(c.left)}
			if got := cred.NeedsRefresh(now, c.floor); got != c.want {
				t.Fatalf("NeedsRefresh = %v, want %v", got, c.want)
			}
		})
	}
}

// TestACredentialWithNoExpiryIsNeverRefreshed: a zero ExpiresAt means "does
// not expire", not "expired at the epoch". Reading it the other way refreshes
// a plain API key — which has no refresh flow — on every single turn.
func TestACredentialWithNoExpiryIsNeverRefreshed(t *testing.T) {
	c := provider.Credential{APIKey: "sk-static-key-value"}
	if c.NeedsRefresh(time.Now(), time.Hour) {
		t.Fatal("a credential with no expiry has no refresh flow")
	}
}

// TestTheRefreshCarriesItsOwnTimeout is REQ-AUTH-06's last sentence, and the
// reason it is there: the refresh holds the per-vendor lock, so a hanging
// refresh endpoint blocks every turn for that vendor for as long as the
// caller's context allows — which on a long-running agent is forever.
func TestTheRefreshCarriesItsOwnTimeout(t *testing.T) {
	creds, _ := expiringCreds(t, time.Minute)

	var deadlineSeen bool
	_, err := creds.EnsureFresh(context.Background(), "acme",
		func(ctx context.Context, cur provider.Credential) (provider.Credential, error) {
			_, deadlineSeen = ctx.Deadline()
			<-ctx.Done()
			return provider.Credential{}, ctx.Err()
		}, provider.RefreshOptions{Timeout: 40 * time.Millisecond})

	if !deadlineSeen {
		t.Fatal("the refresh context must carry a deadline even when the caller's has none")
	}
	if err == nil {
		t.Fatal("a refresh that outran its timeout must fail rather than hold the lock")
	}
}

// TestPerVendorLocksDoNotSerializeDifferentVendors: the lock is keyed, and a
// global one would make every vendor wait on the slowest.
func TestPerVendorLocksDoNotSerializeDifferentVendors(t *testing.T) {
	store := &provider.MemoryStore{}
	ctx := context.Background()
	for _, v := range []string{"acme", "globex"} {
		if err := store.Save(ctx, v, provider.Credential{
			AccessToken: "old", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	creds := provider.NewCredentials(store)

	both := make(chan struct{})
	var entered sync.WaitGroup
	entered.Add(2)
	refresh := func(ctx context.Context, cur provider.Credential) (provider.Credential, error) {
		entered.Done()
		select {
		case <-both:
		case <-time.After(2 * time.Second):
			return provider.Credential{}, errors.New("the other vendor never entered")
		}
		cur.AccessToken = "fresh"
		cur.ExpiresAt = time.Now().Add(time.Hour)
		return cur, nil
	}

	go func() { entered.Wait(); close(both) }()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, v := range []string{"acme", "globex"} {
		wg.Add(1)
		go func(i int, v string) {
			defer wg.Done()
			_, errs[i] = creds.EnsureFresh(context.Background(), v, refresh,
				provider.RefreshOptions{Timeout: 3 * time.Second})
		}(i, v)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("vendor %d: %v; two vendors must refresh concurrently", i, err)
		}
	}
}

// TestVendorLocksAreReleased is the refcounting half of REQ-AUTH-05.
//
// A map of mutexes that never deletes grows with the number of distinct vendor
// ids seen; one that deletes without counting hands two goroutines two
// different mutexes for the same key, which is not a lock at all.
func TestVendorLocksAreReleased(t *testing.T) {
	creds := provider.NewCredentials(nil)
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		v := fmt.Sprintf("vendor-%d", i)
		if _, err := creds.Modify(ctx, v, func(c provider.Credential) (provider.Credential, error) {
			c.APIKey = "k"
			return c, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := creds.LiveLocks(); n != 0 {
		t.Fatalf("%d vendor locks still allocated after 200 modifies; the map grows "+
			"without bound in any deployment where the vendor id comes from a request", n)
	}
}

// TestAFailedModifyWritesNothing: Modify is the only write path, so a
// callback that fails must leave the stored value alone.
func TestAFailedModifyWritesNothing(t *testing.T) {
	creds, store := expiringCreds(t, time.Hour)
	ctx := context.Background()

	boom := errors.New("refresh endpoint said no")
	if _, err := creds.Modify(ctx, "acme", func(provider.Credential) (provider.Credential, error) {
		return provider.Credential{AccessToken: "half-written"}, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the callback's own error", err)
	}
	got, _ := store.Load(ctx, "acme")
	if got.AccessToken != "old-access-token-value" {
		t.Fatalf("stored token = %q; a failed Modify must commit nothing", got.AccessToken)
	}
}

// TestAStoredCredentialCannotBePrintedByAccident is REQ-AUTH-07: the redaction
// boundary is the TYPE, not the log call sites, because the ways a credential
// escapes are %v, %w and a struct dump in a test failure.
func TestAStoredCredentialCannotBePrintedByAccident(t *testing.T) {
	c := provider.Credential{
		APIKey:       "sk-ant-super-secret-key-material",
		AccessToken:  "ya29-super-secret-access-material",
		RefreshToken: "1//super-secret-refresh-material",
	}
	for _, rendered := range []string{
		c.String(),
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%s", c),
		fmt.Errorf("auth failed for %v", c).Error(),
	} {
		if strings.Contains(rendered, "super-secret") {
			t.Fatalf("a credential leaked through an ordinary format verb: %s", rendered)
		}
	}
}

func TestAStoredCredentialBecomesModelAuth(t *testing.T) {
	bearer := provider.Credential{AccessToken: "tok-abcdefghijkl", Scheme: provider.SchemeBearer}.Auth()
	if v := bearer.Headers["Authorization"]; v == nil || *v != "Bearer tok-abcdefghijkl" {
		t.Fatalf("headers = %v, want an Authorization bearer", bearer.Headers)
	}
	if bearer.State != provider.CredentialResolved {
		t.Fatalf("state = %v, want resolved", bearer.State)
	}

	key := provider.Credential{APIKey: "sk-abcdefghijkl"}.Auth()
	if v := key.Headers["x-api-key"]; v == nil || *v != "sk-abcdefghijkl" {
		t.Fatalf("headers = %v, want x-api-key", key.Headers)
	}

	gw := provider.Credential{BaseURL: "https://gw.example"}.Auth()
	if gw.State != provider.CredentialAmbient {
		t.Fatalf("state = %v, want ambient: a gateway URL is configuration the SDK "+
			"cannot read a key out of", gw.State)
	}
}

// TestTheStoreOutranksTheEnvironment: the store is the layer that can hold a
// refreshed OAuth token; the environment is static and cannot.
func TestTheStoreOutranksTheEnvironment(t *testing.T) {
	store := &provider.MemoryStore{}
	ctx := context.Background()
	_ = store.Save(ctx, "anthropic", provider.Credential{
		AccessToken: "refreshed-oauth-token", Scheme: provider.SchemeBearer})
	creds := provider.NewCredentials(store)

	env := envOf(map[string]string{"ANTHROPIC_API_KEY": "stale-env-key"})
	auth, err := provider.ResolveAuthWith(ctx, "anthropic", creds, anthropicTable, env)
	if err != nil {
		t.Fatal(err)
	}
	if auth.APIKey != "refreshed-oauth-token" {
		t.Fatalf("resolved %q, want the stored credential to win", auth.APIKey)
	}

	// An EMPTY store falls through to the environment rather than resolving to
	// nothing, so adding a store does not break a working env-based setup.
	empty := provider.NewCredentials(&provider.MemoryStore{})
	auth, err = provider.ResolveAuthWith(ctx, "anthropic", empty, anthropicTable, env)
	if err != nil {
		t.Fatal(err)
	}
	if auth.APIKey != "stale-env-key" {
		t.Fatalf("resolved %q, want the environment fallback", auth.APIKey)
	}
}
