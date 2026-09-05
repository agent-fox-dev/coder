// Package catalog embeds AgentKit's model catalog and turns a model spec
// string into the core.Model descriptor of REQ-PROV-10.
//
// The catalog supplies exactly the metadata no provider API returns and no
// pass-through can synthesize: wire API, base URL, context window, pricing,
// reasoning support, modality support and compatibility profile. Every one of
// those is load-bearing before the first byte is sent — the max_tokens clamp
// (REQ-CAT-04), the budget gate (REQ-PROV-05) and the thinking clamp
// (REQ-PROV-15) all read it — which is why "just pass the model string
// through" is not an option (NFR-COMPAT-03, PRD Appendix A #11).
//
// The catalog is NOT an allowlist. An unknown model id under a known vendor
// clones that vendor's default row (REQ-CAT-03), so a model that ships after
// this snapshot works the day it ships. Nothing in the resolution path rejects
// a model id merely for being absent from the catalog; the only rejections are
// "which vendor?" (unresolvable) and "which of these two?" (ambiguous).
//
// # Layering
//
// catalog imports core and nothing else in the module (plan §1.3). It holds no
// mutable package-level state: Default is a sync.OnceValue over the embedded
// bytes and every returned *core.Model is a deep copy, so a caller who edits a
// resolved descriptor cannot corrupt the process-wide catalog.
package catalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/agentfox/agentkit-go/core"
)

// SchemaVersion is the catalog file format this build understands.
//
// The catalog is versioned separately from the SDK (REQ-CAT-01), so a caller
// may hand Parse a file newer than this binary. A file that declares a
// different schema_version is rejected rather than parsed on a guess: the
// failure mode of guessing is a silently wrong price or context window, which
// surfaces as a 400 deep into a long session (REQ-CAT-04's stated hazard) or
// as a mis-billed run, and neither is detectable from the result.
const SchemaVersion = 1

//go:embed catalog.json
var embeddedCatalog []byte

// Catalog is one parsed, validated, immutable model catalog.
//
// All lookup state is precomputed at Parse time, so resolution is map lookups
// and never a scan. A *Catalog is safe for concurrent use; nothing mutates it
// after Parse returns.
type Catalog struct {
	version string
	note    string

	vendors     map[string]*vendorRow
	vendorIDs   []string           // sorted, for deterministic error messages
	byCanonical map[string]*entry  // "vendor/id"
	byBareID    map[string][]entry // id -> every vendor carrying it, sorted by vendor
}

// entry is one catalog row bound to the vendor that declared it.
type entry struct {
	vendor string
	model  core.Model
}

// canonical is the unambiguous name of a row: "<vendor>/<id>".
func (e entry) canonical() string { return e.vendor + "/" + e.model.ID }

// --- file format -----------------------------------------------------------
//
// The wire format is DELIBERATELY not core.Model. The catalog is versioned
// separately from the SDK (REQ-CAT-01), so its file format is an interface
// with a future version of itself, not a mirror of an internal struct: rows
// inherit api/base_url/headers from their vendor (so a vendor-wide base URL
// change is one edit, not six), and a row may carry a human `note` that has no
// place on a runtime descriptor.

type catalogFile struct {
	SchemaVersion  int                   `json:"schema_version"`
	CatalogVersion string                `json:"catalog_version"`
	Note           string                `json:"note"`
	Vendors        map[string]*vendorRow `json:"vendors"`
}

type vendorRow struct {
	Name    string             `json:"name"`
	API     core.API           `json:"api"`
	BaseURL string             `json:"base_url"`
	Headers map[string]*string `json:"headers"`
	// DefaultModel names the row cloned for an unknown id under this vendor
	// (REQ-CAT-03). It is the most representative current row, not the
	// cheapest and not the largest: the clone is a documented guess, and the
	// guess that is wrong least often is the middle of the vendor's range.
	DefaultModel string               `json:"default_model"`
	Models       map[string]*modelRow `json:"models"`

	id string // the map key, filled in by Parse
}

type modelRow struct {
	ID               string                         `json:"id"` // optional; must equal the key when present
	Name             string                         `json:"name"`
	API              core.API                       `json:"api"`      // inherited from the vendor when empty
	BaseURL          string                         `json:"base_url"` // inherited from the vendor when empty
	Headers          map[string]*string             `json:"headers"`  // inherited from the vendor when nil
	Compat           json.RawMessage                `json:"compat"`
	ContextWindow    int                            `json:"context_window"`
	MaxTokens        int                            `json:"max_tokens"`
	Cost             core.Cost                      `json:"cost"`
	Input            []string                       `json:"input"`
	Reasoning        bool                           `json:"reasoning"`
	ThinkingLevelMap map[core.ThinkingLevel]*string `json:"thinking_level_map"`
	Note             string                         `json:"note"`
}

// --- parsing ---------------------------------------------------------------

// ErrCorruptCatalog is the sentinel every Parse failure matches.
var ErrCorruptCatalog = errors.New("agentkit: model catalog is corrupt")

// ParseError names the exact row that failed validation. A catalog is edited
// by hand during the REQ-CAT-06 regeneration ritual, so "corrupt" without a
// path is a message that costs an hour.
type ParseError struct {
	Path   string // "vendors.anthropic.models.claude-opus-4-5.cost.input", or "" for whole-file failures
	Reason string
}

func (e *ParseError) Error() string {
	if e.Path == "" {
		return "agentkit: model catalog is corrupt: " + e.Reason
	}
	return "agentkit: model catalog is corrupt at " + e.Path + ": " + e.Reason
}

// Is reports ErrCorruptCatalog so callers can classify without type-asserting.
func (e *ParseError) Is(target error) bool { return target == ErrCorruptCatalog }

func badf(path, format string, args ...any) error {
	return &ParseError{Path: path, Reason: fmt.Sprintf(format, args...)}
}

// Parse decodes and validates a catalog document (REQ-CAT-01's "overridable by
// the caller"). It is the only constructor; there is no partially-valid
// Catalog, because a catalog that loaded "mostly" is one whose missing row
// becomes a sibling clone with a plausible, wrong context window.
func Parse(data []byte) (*Catalog, error) {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, badf("", "%v", err)
	}
	if f.SchemaVersion != SchemaVersion {
		return nil, badf("schema_version", "have %d, this build understands %d",
			f.SchemaVersion, SchemaVersion)
	}
	if len(f.Vendors) == 0 {
		return nil, badf("vendors", "catalog declares no vendors; an empty catalog is\n"+
			"indistinguishable at every call site from a catalog that failed to load")
	}

	c := &Catalog{
		version:     f.CatalogVersion,
		note:        f.Note,
		vendors:     make(map[string]*vendorRow, len(f.Vendors)),
		byCanonical: make(map[string]*entry),
		byBareID:    make(map[string][]entry),
	}

	for vid, v := range f.Vendors {
		vpath := "vendors." + vid
		if v == nil {
			return nil, badf(vpath, "null vendor")
		}
		if vid == "" {
			return nil, badf("vendors", "empty vendor id")
		}
		// A vendor id containing a slash would make "vendor/id" ambiguous to
		// split, and REQ-CAT-02.1 splits at the FIRST slash. Reject it here
		// rather than resolve wrongly later.
		if strings.ContainsAny(vid, "/ \t\n") {
			return nil, badf(vpath, "vendor id may not contain a slash or whitespace")
		}
		if len(v.Models) == 0 {
			return nil, badf(vpath+".models", "vendor declares no models; it can never be\n"+
				"a clone template, so a known-vendor id under it would resolve to nothing")
		}
		if v.DefaultModel == "" {
			return nil, badf(vpath+".default_model", "missing; REQ-CAT-03 has no sibling to clone without it")
		}
		if _, ok := v.Models[v.DefaultModel]; !ok {
			return nil, badf(vpath+".default_model", "names %q, which is not a model of this vendor", v.DefaultModel)
		}
		v.id = vid
		c.vendors[vid] = v
		c.vendorIDs = append(c.vendorIDs, vid)

		for mid, m := range v.Models {
			mpath := vpath + ".models." + mid
			if m == nil {
				return nil, badf(mpath, "null model row")
			}
			if mid == "" {
				return nil, badf(vpath+".models", "empty model id")
			}
			if m.ID != "" && m.ID != mid {
				return nil, badf(mpath+".id", "row declares id %q but is keyed %q", m.ID, mid)
			}
			model, err := m.toModel(vid, mid, v, mpath)
			if err != nil {
				return nil, err
			}
			e := entry{vendor: vid, model: model}
			ce := e
			c.byCanonical[e.canonical()] = &ce
			c.byBareID[mid] = append(c.byBareID[mid], e)
		}
	}

	sort.Strings(c.vendorIDs)
	for id := range c.byBareID {
		es := c.byBareID[id]
		sort.Slice(es, func(i, j int) bool { return es[i].vendor < es[j].vendor })
	}
	return c, nil
}

// toModel projects one file row onto the REQ-PROV-10 descriptor, inheriting
// vendor-level defaults, and validates it.
func (m *modelRow) toModel(vendorID, modelID string, v *vendorRow, path string) (core.Model, error) {
	out := core.Model{
		ID:               modelID,
		Name:             m.Name,
		API:              m.API,
		Provider:         vendorID,
		BaseURL:          m.BaseURL,
		Headers:          m.Headers,
		Compat:           m.Compat,
		ContextWindow:    m.ContextWindow,
		MaxTokens:        m.MaxTokens,
		Cost:             m.Cost,
		Input:            m.Input,
		Reasoning:        m.Reasoning,
		ThinkingLevelMap: m.ThinkingLevelMap,
	}
	if out.Name == "" {
		out.Name = modelID
	}
	if out.API == "" {
		out.API = v.API
	}
	if out.BaseURL == "" {
		out.BaseURL = v.BaseURL
	}
	if out.Headers == nil {
		out.Headers = v.Headers
	}

	// The API string is checked for emptiness ONLY, never against the known
	// core.API constants. REQ-PROV-09 lets a third party register a new API
	// value via a BackendPlugin, and a catalog row naming that API must load
	// in a build that has never heard of it. An empty API, by contrast, can
	// only ever dispatch to nothing.
	if out.API == "" {
		return core.Model{}, badf(path+".api", "missing, and vendor %q declares no default api", vendorID)
	}
	if out.BaseURL == "" {
		return core.Model{}, badf(path+".base_url", "missing, and vendor %q declares no default base_url", vendorID)
	}
	if out.ContextWindow < 0 {
		return core.Model{}, badf(path+".context_window", "negative (%d)", out.ContextWindow)
	}
	if out.MaxTokens < 0 {
		return core.Model{}, badf(path+".max_tokens", "negative (%d)", out.MaxTokens)
	}
	if len(m.Compat) > 0 && !json.Valid(m.Compat) {
		return core.Model{}, badf(path+".compat", "not valid JSON")
	}
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"input", out.Cost.Input}, {"output", out.Cost.Output},
		{"cache_read", out.Cost.CacheRead}, {"cache_write", out.Cost.CacheWrite},
	} {
		if f.v < 0 {
			return core.Model{}, badf(path+".cost."+f.name, "negative (%v)", f.v)
		}
	}
	prev := 0
	for i, t := range out.Cost.Tiers {
		if t.Threshold <= prev {
			return core.Model{}, badf(fmt.Sprintf("%s.cost.tiers[%d].threshold", path, i),
				"must be positive and strictly ascending (got %d after %d)", t.Threshold, prev)
		}
		prev = t.Threshold
	}

	// A typo'd thinking level ("higth") is the single most expensive thing a
	// hand-edited catalog can contain: it reads as "unsupported", so the clamp
	// silently picks a different level and nothing ever errors. Reject unknown
	// keys at load time — this is the REQ-CAT-06 diff's other half.
	supported := 0
	for lvl, wire := range out.ThinkingLevelMap {
		if !knownThinkingLevel(lvl) {
			return core.Model{}, badf(path+".thinking_level_map",
				"unknown thinking level %q (known: off, minimal, low, medium, high, xhigh, max)", string(lvl))
		}
		if wire != nil && lvl != core.ThinkingOff {
			supported++
		}
	}
	if supported > 0 && !out.Reasoning {
		return core.Model{}, badf(path+".reasoning",
			"false, but thinking_level_map supports %d level(s) above off; a clamp would\n"+
				"select a level the budget and provenance paths believe cannot exist", supported)
	}
	return out, nil
}

func knownThinkingLevel(l core.ThinkingLevel) bool {
	for _, k := range core.ThinkingLevelOrder {
		if k == l {
			return true
		}
	}
	return false
}

// --- the default (embedded) catalog ----------------------------------------

// defaultCatalog parses the embedded bytes at most once.
//
// REQ-CAT-01 says a corrupt catalog "panics at init". NFR-SEC-05 forbids
// package init() from mutating global state, and an init() panic is also
// untestable and unrecoverable: it takes down every binary that merely imports
// the package, including the test binary that would have reported the problem.
// Ruling P-15 reconciles the two — sync.OnceValue, panicking on FIRST USE.
// The observable contract is identical (nothing ever sees a silently empty
// catalog), the failure is attributable to the call that needed the catalog,
// and the corrupt case becomes an ordinary table test: sync.OnceValue stores
// the recovered panic value and re-panics with THE SAME value on every
// subsequent call, so "panics identically the second time" is assertable.
var defaultCatalog = onceCatalog(embeddedCatalog)

// onceCatalog is shared by Default and by the corrupt-catalog test, so the
// test exercises the real panic-on-first-use machinery rather than a lookalike.
func onceCatalog(data []byte) func() *Catalog {
	return sync.OnceValue(func() *Catalog {
		c, err := Parse(data)
		if err != nil {
			panic("agentkit/catalog: embedded model catalog failed to load: " + err.Error())
		}
		return c
	})
}

// Default returns the embedded catalog, parsing it on first use.
//
// It panics if the embedded catalog is corrupt (REQ-CAT-01 as amended by
// ruling P-15) and panics identically on every later call. It never returns a
// silently empty catalog.
func Default() *Catalog { return defaultCatalog() }

// --- accessors -------------------------------------------------------------

// Version is the catalog document's own version, which moves independently of
// the SDK's (REQ-CAT-01).
func (c *Catalog) Version() string { return c.version }

// Note is the catalog document's maintenance note, including the REQ-CAT-06
// regeneration ritual.
func (c *Catalog) Note() string { return c.note }

// Vendors lists known vendor ids in sorted order.
func (c *Catalog) Vendors() []string { return append([]string(nil), c.vendorIDs...) }

// KnownVendor reports whether id names a vendor. This is REQ-CAT-02 rule 1's
// predicate: it decides whether the segment before the first slash of a spec
// is a vendor prefix or just part of an OpenRouter-style model id.
func (c *Catalog) KnownVendor(id string) bool {
	_, ok := c.vendors[id]
	return ok
}

// Models lists the model ids of one vendor in sorted order. It returns nil for
// an unknown vendor.
func (c *Catalog) Models(vendor string) []string {
	v, ok := c.vendors[vendor]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v.Models))
	for id := range v.Models {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// DefaultModelID is the id of the row REQ-CAT-03 clones for an unknown model
// under this vendor. It returns "" for an unknown vendor.
func (c *Catalog) DefaultModelID(vendor string) string {
	v, ok := c.vendors[vendor]
	if !ok {
		return ""
	}
	return v.DefaultModel
}

// cloneModel deep-copies a descriptor.
//
// Every model handed to a caller goes through this, hit or clone. REQ-CAT-03
// says callers "may override any inherited field", and the catalog is a
// process-wide singleton: without the copy, one caller raising MaxTokens or
// deleting a header would change every later resolution in the process.
func cloneModel(m core.Model) core.Model {
	out := m
	if m.Headers != nil {
		h := make(map[string]*string, len(m.Headers))
		for k, v := range m.Headers {
			if v == nil {
				// Present-nil is REQ-AUTH-02's deletion marker, not "absent";
				// it must survive the copy as a present key.
				h[k] = nil
				continue
			}
			s := *v
			h[k] = &s
		}
		out.Headers = h
	}
	if m.Compat != nil {
		out.Compat = append(json.RawMessage(nil), m.Compat...)
	}
	if m.Input != nil {
		out.Input = append([]string(nil), m.Input...)
	}
	if m.ThinkingLevelMap != nil {
		tm := make(map[core.ThinkingLevel]*string, len(m.ThinkingLevelMap))
		for k, v := range m.ThinkingLevelMap {
			if v == nil {
				// Present-null: "explicitly unsupported" (REQ-PROV-15). It is
				// runtime-identical to absent (ruling P-28) but must survive
				// the copy so the REQ-CAT-06 diff can still see it.
				tm[k] = nil
				continue
			}
			s := *v
			tm[k] = &s
		}
		out.ThinkingLevelMap = tm
	}
	if m.Cost.Tiers != nil {
		out.Cost.Tiers = append([]core.CostTier(nil), m.Cost.Tiers...)
	}
	return out
}
