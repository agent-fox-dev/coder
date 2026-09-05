package agentkit

import (
	"context"
	"sync"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// This file is REQ-CACHE-08, -09 and -11: what an operator can see about the
// three caching levels.
//
// The requirement's own justification is the point of it: an operator needs to
// see "when a session lost its prompt cache and why". A cost spike with no
// explanation is indistinguishable from a price change, and the usual response
// to one is to turn caching off.

// CacheStats is REQ-CACHE-08's aggregate over the session lifetime, extended
// by REQ-CACHE-11.
type CacheStats struct {
	// Hits and Misses are Level 2, the in-process dedup LRU.
	Hits   int
	Misses int
	// ProviderCacheReadTokens and ProviderCacheWriteTokens are Level 1, as
	// REPORTED by the provider. They are never estimated: REQ-GO-15 forbids
	// treating a re-estimate as a measurement, and a savings figure computed
	// from a token estimate is a guess wearing a dollar sign.
	ProviderCacheReadTokens  int64
	ProviderCacheWriteTokens int64
	// EstimatedSavingsUSD is what the cached tokens would have cost at full
	// input price, minus what they did cost, plus the full cost of every
	// response a Level 2 hit avoided re-requesting. The second term is exact
	// rather than estimated — the avoided response was priced when it was
	// stored.
	EstimatedSavingsUSD float64

	// REQ-CACHE-11: why a session lost its prefix.
	DeferredToolPromotions int
	PrefixInvalidations    int
	// LastInvalidation names the most recent cause, so the operator has
	// something to act on rather than a count.
	LastInvalidation string
}

// CacheMeter accumulates CacheStats. It is safe for concurrent use and is held
// by the Agent, not by a package-level global: two agents in one process have
// separate caches and must have separate numbers.
type CacheMeter struct {
	mu sync.Mutex
	s  CacheStats
	// priced remembers what each cached response cost when it was stored, so a
	// hit can credit the exact amount rather than an average.
	priced map[string]float64
}

func NewCacheMeter() *CacheMeter { return &CacheMeter{priced: map[string]float64{}} }

// Stats returns a snapshot.
func (m *CacheMeter) Stats() CacheStats {
	if m == nil {
		return CacheStats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

// ObserveTurn folds one completed turn's reported usage into the Level 1
// counters and their savings.
//
// Savings are computed against the model's OWN rates through the same
// arithmetic that priced the turn (REQ-PROV-05), not against a nominal input
// price: a request that crossed a pricing tier saved tier money, and a
// hand-rolled `cacheRead * inputRate` here would disagree with the bill.
func (m *CacheMeter) ObserveTurn(model *core.Model, u core.Usage) {
	if m == nil || model == nil || !u.Reported() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.ProviderCacheReadTokens += u.CacheReadTokens
	m.s.ProviderCacheWriteTokens += u.CacheWriteTokens

	if u.CacheReadTokens == 0 {
		return
	}
	// What the same request would have cost with the cached portion billed as
	// ordinary input.
	uncached := u
	uncached.InputTokens += u.CacheReadTokens
	uncached.CacheReadTokens = 0
	m.s.EstimatedSavingsUSD += provider.ComputeCost(model, uncached) - provider.ComputeCost(model, u)
}

// ObserveSync folds a REQ-CACHE-06 tool-prefix reconciliation.
func (m *CacheMeter) ObserveSync(rep provider.SyncReport) {
	if m == nil || !rep.Invalidated {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.PrefixInvalidations++
	m.s.LastInvalidation = rep.Reason
}

// ObserveDeferredSplit folds a REQ-CACHE-10 partition. Only the safety-valve
// promotion is counted: it is the case that costs the cache.
func (m *CacheMeter) ObserveDeferredSplit(s provider.DeferredSplit) {
	if m == nil || !s.Promoted {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.DeferredToolPromotions++
	if m.s.LastInvalidation == "" {
		m.s.LastInvalidation = "every tool was deferred; all promoted back (REQ-CACHE-10 safety valve)"
	}
}

func (m *CacheMeter) hit(fp string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Hits++
	m.s.EstimatedSavingsUSD += m.priced[fp]
}

func (m *CacheMeter) miss(fp string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Misses++
}

func (m *CacheMeter) price(fp string, cost float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.priced == nil {
		m.priced = map[string]float64{}
	}
	m.priced[fp] = cost
}

// ---------------------------------------------------------------- REQ-CACHE-09

// cacheNote is how the caching middleware tells the tracing middleware what
// happened, without either one knowing about the other.
//
// REQ-CACHE-09 puts cache.hit / cache.tier / cache.fingerprint on the MODEL
// CALL span, and those two concerns sit at different points in one chain. A
// direct call would couple them; a context-carried note leaves both usable
// alone. The ORDERING it requires is real and is documented on
// TracingMiddleware: tracing must be registered so that it wraps caching,
// which — since the LAST registered middleware is outermost — means
// registering tracing after caching.
type cacheNote struct {
	Hit         bool
	Tier        string
	Fingerprint string
}

type cacheNoteKey struct{}

func withCacheNote(ctx context.Context) (context.Context, *cacheNote) {
	n := &cacheNote{}
	return context.WithValue(ctx, cacheNoteKey{}, n), n
}

func noteFrom(ctx context.Context) *cacheNote {
	n, _ := ctx.Value(cacheNoteKey{}).(*cacheNote)
	return n
}
