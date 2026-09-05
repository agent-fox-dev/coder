package catalog

import (
	"errors"
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/core"
)

// Resolution failures. Each is a distinct configuration mistake with a
// distinct fix, so each gets a sentinel; ResolveError.Is reports the right one.
var (
	// ErrEmptySpec: ResolveModel("") — there is no default model.
	ErrEmptySpec = errors.New("agentkit: empty model spec")

	// ErrUnknownVendor: the segment before the first slash is not a vendor of
	// this catalog and the whole string is not a model id either (REQ-CAT-03:
	// "an unknown vendor is a configuration error").
	ErrUnknownVendor = errors.New("agentkit: unknown vendor")

	// ErrAmbiguousModel: a bare id carried by two or more vendors. REQ-CAT-02
	// rule 2 is explicit that this "resolves to nothing — the SDK errors
	// rather than guessing". Guessing here picks a base URL, a price and a
	// context window; being wrong about any of them is silent.
	ErrAmbiguousModel = errors.New("agentkit: model id is ambiguous across vendors")

	// ErrUnresolvedModel: a spec with no vendor prefix that names no catalog
	// row. Note carefully what this is NOT: it is not "this model id is not in
	// the catalog", which REQ-CAT-03 forbids as a rejection reason. It is "no
	// vendor could be determined for it", and prefixing the spec with any
	// known vendor fixes it — the id itself is never the objection.
	ErrUnresolvedModel = errors.New("agentkit: cannot determine a vendor for model spec")
)

// ResolveError explains a failed resolution and names the fix.
type ResolveError struct {
	Spec string
	Err  error // one of the sentinels above

	// Candidates holds the canonical "vendor/id" names that matched, set only
	// for ErrAmbiguousModel. The message lists them so the caller can copy one.
	Candidates []string
	// Vendors holds the catalog's known vendor ids, set when naming one is the
	// fix.
	Vendors []string
}

func (e *ResolveError) Unwrap() error { return e.Err }

func (e *ResolveError) Error() string {
	switch {
	case errors.Is(e.Err, ErrEmptySpec):
		return "agentkit: empty model spec; pass a model id or \"vendor/model-id\""
	case errors.Is(e.Err, ErrAmbiguousModel):
		return fmt.Sprintf("agentkit: model id %q is ambiguous across vendors (%s); "+
			"name one explicitly rather than letting the SDK guess a base URL and a price",
			e.Spec, strings.Join(e.Candidates, ", "))
	case errors.Is(e.Err, ErrUnknownVendor):
		vendor, _, _ := strings.Cut(e.Spec, "/")
		return fmt.Sprintf("agentkit: unknown vendor %q in model spec %q, and %q is not a "+
			"model id either; known vendors: %s",
			vendor, e.Spec, e.Spec, strings.Join(e.Vendors, ", "))
	default:
		return fmt.Sprintf("agentkit: cannot determine a vendor for model spec %q; "+
			"prefix it with a vendor (%s) — an unknown id under a known vendor is fine "+
			"and clones that vendor's default row",
			e.Spec, strings.Join(e.Vendors, ", "))
	}
}

// MatchKind records which REQ-CAT-02 rule produced a Resolution. It exists so
// the precedence between the rules is observable — and therefore testable —
// rather than being an implementation detail that can silently reorder.
type MatchKind uint8

const (
	// MatchExactCanonical: the spec equalled a row's "vendor/id" exactly.
	MatchExactCanonical MatchKind = iota
	// MatchVendorAndID: the segment before the first slash named a vendor and
	// the remainder named one of its models.
	MatchVendorAndID
	// MatchBareID: the whole spec named exactly one model across all vendors.
	MatchBareID
	// MatchSiblingClone: a known vendor, an unknown id — REQ-CAT-03.
	MatchSiblingClone
)

func (k MatchKind) String() string {
	switch k {
	case MatchExactCanonical:
		return "exact-canonical"
	case MatchVendorAndID:
		return "vendor+id"
	case MatchBareID:
		return "bare-id"
	case MatchSiblingClone:
		return "sibling-clone"
	default:
		return fmt.Sprintf("MatchKind(%d)", uint8(k))
	}
}

// Resolution is the full result of a catalog lookup.
//
// # How REQ-CAT-03's "with a warning" is surfaced
//
// A sibling clone is a documented guess about a model nobody has measured, and
// the caller has to be able to see that. Three mechanisms were available; this
// package uses the first two and rejects the third:
//
//  1. On the value itself — Model.Cloned and Model.ClonedFrom. This is the
//     primary mechanism, because it is the only one that survives. The
//     descriptor outlives the resolve call: it is stored on AgentConfig, is
//     written into the session log's model-change entry (REQ-SESS-04), and is
//     read again by the budget gate and the clamp. A caller inspecting a model
//     six turns later can still tell the pricing was inherited.
//  2. Resolution.Warning — a rendered sentence, non-empty exactly when
//     Model.Cloned, for callers that want to show or log one.
//  3. NOT a log line, and NOT a package-level logger. AgentKit has no global
//     logger by construction (core.ProviderStreamOptions.Warnf is passed in,
//     never reached for), a log line is unobservable to a test and to a UI,
//     and a warning that only exists in a stream nobody read is the same as no
//     warning at all.
type Resolution struct {
	Model   *core.Model
	Kind    MatchKind
	Warning string // non-empty iff Model.Cloned
}

// ResolvedModel is REQ-CAT-07's accessor: the descriptor actually used,
// clone or not. The root package's Agent/Session exposes the same value by
// delegating here, so "which catalog row did this run use?" has one answer.
func (r Resolution) ResolvedModel() *core.Model { return r.Model }

// Resolve applies REQ-CAT-02's matching rules to spec.
//
// Rule 1 — a "provider/" prefix is honoured ONLY when the segment before the
// FIRST slash is a known vendor. OpenRouter-style ids contain slashes, so
// "deepseek-ai/DeepSeek-V3" must be matched whole as a model id rather than
// read as vendor "deepseek-ai". This is not a heuristic about the string: it
// is a lookup of the prefix in this catalog's vendor set.
//
// Rule 2 — matching proceeds exact-canonical, then vendor+id, then bare id,
// each accepted only when unambiguous. The order is load-bearing: a catalog
// that carries an OpenRouter row whose own id is literally
// "anthropic/claude-opus-4-5" must still resolve the spec
// "anthropic/claude-opus-4-5" to the Anthropic vendor, because vendor+id
// outranks bare id.
//
// A known vendor with an unknown id is not a failure — it is REQ-CAT-03's
// sibling clone. The only failures are "which vendor?" and "which of these?".
func (c *Catalog) Resolve(spec string) (Resolution, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Resolution{}, &ResolveError{Spec: spec, Err: ErrEmptySpec, Vendors: c.vendorIDs}
	}

	// Rule 2, pass 1: exact canonical "vendor/id".
	//
	// In a catalog whose vendor ids contain no slash (Parse enforces that)
	// this pass is a fast path that pass 2 would also satisfy, since pass 2
	// splits at the first slash and takes the whole remainder as the id. It is
	// kept because REQ-CAT-02 names it first and because it is the pass that
	// stays correct if a future format ever lets a row declare a canonical
	// name of its own.
	if e, ok := c.byCanonical[spec]; ok {
		return hit(*e, MatchExactCanonical), nil
	}

	// Rule 1 + rule 2 pass 2: vendor prefix, honoured only for a known vendor.
	prefix, rest, hasSlash := strings.Cut(spec, "/")
	if hasSlash && rest != "" && c.KnownVendor(prefix) {
		v := c.vendors[prefix]
		if e, ok := c.byCanonical[prefix+"/"+rest]; ok {
			return hit(*e, MatchVendorAndID), nil
		}
		return c.cloneSibling(v, rest), nil
	}

	// Rule 2 pass 3: bare id, across every vendor, accepted only when unique.
	if es := c.byBareID[spec]; len(es) == 1 {
		return hit(es[0], MatchBareID), nil
	} else if len(es) > 1 {
		cands := make([]string, 0, len(es))
		for _, e := range es {
			cands = append(cands, e.canonical())
		}
		return Resolution{}, &ResolveError{
			Spec: spec, Err: ErrAmbiguousModel, Candidates: cands, Vendors: c.vendorIDs,
		}
	}

	// Nothing matched. Distinguish "you named a vendor I do not have" from
	// "you named no vendor at all": the fixes differ, and neither error is a
	// rejection of the model id itself (REQ-CAT-03's final sentence).
	if hasSlash {
		return Resolution{}, &ResolveError{Spec: spec, Err: ErrUnknownVendor, Vendors: c.vendorIDs}
	}
	return Resolution{}, &ResolveError{Spec: spec, Err: ErrUnresolvedModel, Vendors: c.vendorIDs}
}

func hit(e entry, k MatchKind) Resolution {
	m := cloneModel(e.model)
	return Resolution{Model: &m, Kind: k}
}

// cloneSibling implements REQ-CAT-03: an unknown model id under a known vendor
// inherits that vendor's default row wholesale, swapping ONLY ID and Name.
//
// Everything else — API, BaseURL, Headers, ContextWindow, MaxTokens, Cost,
// Compat, Input and ThinkingLevelMap — is inherited verbatim, which is the
// whole point: NFR-COMPAT-03 requires a new model id to work without an SDK
// release, and every one of those fields is needed before the first byte is
// sent. The inherited profile is a documented guess, not a fact; the caller
// may override any field, which is safe because the returned descriptor is a
// deep copy.
func (c *Catalog) cloneSibling(v *vendorRow, id string) Resolution {
	base := c.byCanonical[v.id+"/"+v.DefaultModel] // Parse guarantees this exists
	m := cloneModel(base.model)
	m.ID = id
	m.Name = id // the only honest display name for a model nobody has described
	m.Cloned = true
	m.ClonedFrom = base.canonical()
	return Resolution{
		Model: &m,
		Kind:  MatchSiblingClone,
		Warning: fmt.Sprintf(
			"agentkit: model %q is not in the catalog (version %s); it inherits %q — "+
				"context window %d, max output %d, $%g/$%g per 1M in/out, api %s. "+
				"That profile is a guess, not a fact: override any field on the returned "+
				"Model if it is wrong.",
			v.id+"/"+id, c.version, m.ClonedFrom,
			m.ContextWindow, m.MaxTokens, m.Cost.Input, m.Cost.Output, m.API),
	}
}

// ResolveModel is REQ-CAT-02's single entry point against this catalog.
// AgentConfig.Model and AgentConfig.Provider are resolved through it before
// any request is built.
//
// A cloned model is a success, not an error: check Model.Cloned, or call
// Resolve for the rendered warning.
func (c *Catalog) ResolveModel(spec string) (*core.Model, error) {
	r, err := c.Resolve(spec)
	if err != nil {
		return nil, err
	}
	return r.Model, nil
}

// Resolve applies REQ-CAT-02's rules against the embedded catalog. It panics
// on first use if the embedded catalog is corrupt (REQ-CAT-01, ruling P-15).
func Resolve(spec string) (Resolution, error) { return Default().Resolve(spec) }

// ResolveModel is REQ-CAT-02's single entry point against the embedded
// catalog, re-exported at the root as agentkit.ResolveModel.
func ResolveModel(spec string) (*core.Model, error) { return Default().ResolveModel(spec) }
