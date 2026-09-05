package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// This file is §6.2a Level 3: REQ-CACHE-06 (serialize the tool list once per
// session) and REQ-CACHE-10 (deferred tool loading).
//
// Both exist to protect the same thing — the provider-side prompt cache
// prefix — and they attack it from opposite ends. REQ-CACHE-06 stops us paying
// to re-serialize a tool list that has not changed; REQ-CACHE-10 stops a tool
// that appeared mid-session from being prepended to the prefix, which would
// invalidate the entire cached history over one added tool.

// ---------------------------------------------------------------- REQ-CACHE-06

type prefixEntry struct {
	// ptr is the identity fast path. Tools are registered once and their
	// schema values are treated as immutable, so an unchanged pointer means an
	// unchanged schema and no re-marshal.
	ptr *schema.Schema
	raw json.RawMessage
	// hash is the correctness slow path, consulted only when the pointer
	// differs. Rebuilding an identical tool must not invalidate the prefix;
	// only a genuinely different schema may.
	hash string
}

// ToolPrefix is the per-session serialized tool list.
//
// It is a value on the session, never a package-level map: two agents in one
// process routinely hold tools of the same NAME and different schemas — that
// is what SubagentTool is for — and a shared cache keyed by name would serve
// one agent's schema to the other.
type ToolPrefix struct {
	mu sync.Mutex
	by map[string]prefixEntry
}

// SyncReport is what Sync learned. It feeds REQ-CACHE-11's counters.
type SyncReport struct {
	// Added names tools that appeared since the last Sync. An addition does
	// NOT invalidate (REQ-CACHE-06).
	Added []string
	// Invalidated is true when a tool was REMOVED or an existing tool's schema
	// CHANGED. Either rewrites the cached prefix and costs the whole
	// provider-side cache.
	Invalidated bool
	// Reason names what invalidated it, for the operator who has to explain a
	// cost spike (REQ-CACHE-11).
	Reason string
	// Marshalled counts schemas actually serialized on this call. Zero on a
	// steady-state turn is the whole point of the cache.
	Marshalled int
}

// Marshaller renders one tool's schema into its provider's dialect. It is a
// parameter rather than a fixed json.Marshal because the dialects differ:
// Gemini strips keywords and upper-cases types, and a cache keyed on the
// canonical form would hand a provider the wrong bytes.
type Marshaller func(*schema.Schema) (json.RawMessage, error)

// Sync reconciles against the cached prefix using plain JSON Schema.
func (p *ToolPrefix) Sync(tools []core.ToolWire) ([]json.RawMessage, SyncReport, error) {
	return p.SyncWith(tools, func(s *schema.Schema) (json.RawMessage, error) {
		return json.Marshal(s)
	})
}

// SyncWith reconciles the current tool list against the cached prefix and
// returns the serialized schemas, in the provider's own dialect, in order.
//
// A nil receiver is a working no-op that marshals every time. That is what
// lets a provider hold an optional prefix without branching at the call site,
// and what keeps BuildRequest usable from a test with no session behind it.
func (p *ToolPrefix) SyncWith(tools []core.ToolWire, marshal Marshaller) ([]json.RawMessage, SyncReport, error) {
	if p == nil {
		var rep SyncReport
		out := make([]json.RawMessage, len(tools))
		for i, t := range tools {
			raw, err := marshal(t.InputSchema)
			if err != nil {
				return nil, rep, err
			}
			rep.Marshalled++
			out[i] = raw
		}
		return out, rep, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.by == nil {
		p.by = map[string]prefixEntry{}
	}

	var rep SyncReport
	out := make([]json.RawMessage, len(tools))
	present := make(map[string]bool, len(tools))

	for i, t := range tools {
		present[t.Name] = true
		prev, known := p.by[t.Name]
		if known && prev.ptr == t.InputSchema {
			out[i] = prev.raw
			continue
		}

		raw, err := marshal(t.InputSchema)
		if err != nil {
			return nil, rep, err
		}
		rep.Marshalled++
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])

		switch {
		case !known:
			rep.Added = append(rep.Added, t.Name)
		case prev.hash != hash:
			rep.Invalidated = true
			if rep.Reason == "" {
				rep.Reason = "schema changed: " + t.Name
			}
		}
		p.by[t.Name] = prefixEntry{ptr: t.InputSchema, raw: raw, hash: hash}
		out[i] = raw
	}

	for name := range p.by {
		if !present[name] {
			rep.Invalidated = true
			if rep.Reason == "" {
				rep.Reason = "tool removed: " + name
			}
			delete(p.by, name)
		}
	}
	return out, rep, nil
}

// ---------------------------------------------------------------- REQ-CACHE-10

// DeferredSplit is SplitDeferredTools' partition.
type DeferredSplit struct {
	Immediate []core.ToolWire
	Deferred  []core.ToolWire
	// Promoted is true when the safety valve fired: every tool would have been
	// deferred, leaving no prefix to anchor against, so all were promoted back
	// and the cache wipe accepted. REQ-CACHE-11 reports this.
	Promoted bool
}

// IsDeferred reports whether a named tool is in the deferred set.
func (s DeferredSplit) IsDeferred(name string) bool {
	for _, t := range s.Deferred {
		if t.Name == name {
			return true
		}
	}
	return false
}

// SplitDeferredTools implements REQ-CACHE-10.
//
// A tool is deferred only if a tool result MARKED it as newly added and no
// assistant turn used it BEFORE that marker. The pass is forward and the
// decision is made at the marker: later usage cannot un-defer a tool.
//
// That ordering is the requirement and it is not obvious. A tool used after
// being added is exactly the normal case — a skill activates and the model
// calls its tool on the next turn — and un-deferring on later use would
// promote every deferred tool on the turn after it appeared, which is when
// promoting it is most expensive: the transcript is longest and the cache
// prefix most valuable.
func SplitDeferredTools(tools []core.ToolWire, history core.Messages) DeferredSplit {
	used := map[string]bool{}
	deferred := map[string]bool{}

	for _, m := range history {
		switch v := m.(type) {
		case core.AssistantMessage:
			for _, tu := range core.ExtractToolUse(&v) {
				used[tu.Name] = true
			}
		case core.ToolResultMessage:
			for _, name := range v.AddedToolNames {
				if !used[name] {
					deferred[name] = true
				}
			}
		}
	}

	var s DeferredSplit
	for _, t := range tools {
		if deferred[t.Name] {
			s.Deferred = append(s.Deferred, t)
			continue
		}
		s.Immediate = append(s.Immediate, t)
	}

	// Safety valve: with nothing immediate there is no prefix to anchor
	// references against, so the deferral buys nothing and costs correctness.
	if len(s.Immediate) == 0 && len(s.Deferred) > 0 {
		s.Immediate, s.Deferred, s.Promoted = s.Deferred, nil, true
	}
	return s
}
