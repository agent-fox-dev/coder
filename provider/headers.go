package provider

import (
	"net/http"
	"sort"

	"github.com/agentfox/agentkit-go/core"
)

// This file is REQ-SEC-13.4's header precedence and REQ-AUTH-02's deletion
// marker, in one place, because they only work together.

// AttributionHeaders is REQ-SEC-13.1's ENUMERATED set: the complete list of
// headers AgentKit sends that identify AgentKit itself.
//
// It is a package-level var read as data, not a scatter of AddHeader calls, so
// that "the complete per-provider set is enumerated in a dedicated
// documentation section" has something to enumerate, and so a change to it is
// a visible diff rather than a line buried in a request builder.
//
// REQ-SEC-13.3 constrains the VALUES: no session identifier, no workspace
// path, no user identity, no prompt text. Everything here is a constant.
var AttributionHeaders = map[string]string{
	"x-agentkit-version": Version,
	"user-agent":         "agentkit-go/" + Version,
}

// Version is the SDK version reported in attribution headers.
const Version = "0.1.0"

// AttributionNames returns the attribution header names, sorted. It exists so
// the disclosure section can be generated from the code rather than kept in
// sync with it by hand.
func AttributionNames() []string {
	out := make([]string, 0, len(AttributionHeaders))
	for k := range AttributionHeaders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HeaderPlan holds the four precedence layers of REQ-SEC-13.4, lowest first.
//
// Every layer is map[string]*string because REQ-AUTH-02's third state — a
// present NIL, meaning "suppress the default of this name" — has to survive
// merging. Collapsing to map[string]string at any layer destroys it, and the
// gateway case it exists for (turn off the upstream x-api-key so the gateway's
// own key rides in a different header) then becomes inexpressible.
type HeaderPlan struct {
	Attribution map[string]*string
	Auth        map[string]*string
	Model       map[string]*string
	Request     map[string]*string
}

// AttributionLayer builds the lowest layer, honouring REQ-SEC-13.2's single
// kill switch: AgentConfig.Attribution == false, or AGENTKIT_TELEMETRY=0.
//
// The env check is second, not first: an explicit false in code and an
// explicit 0 in the environment both disable, and neither can re-enable what
// the other turned off.
func AttributionLayer(on *bool, env Env) map[string]*string {
	if on != nil && !*on {
		return nil
	}
	if v := env.Get("AGENTKIT_TELEMETRY"); v == "0" || v == "false" {
		return nil
	}
	out := make(map[string]*string, len(AttributionHeaders))
	for k, v := range AttributionHeaders {
		val := v
		out[k] = &val
	}
	return out
}

// Merge folds the layers lowest to highest and drops every name whose winning
// value is nil.
//
// Overwrite-then-drop is exactly REQ-AUTH-02's semantics and it is worth being
// explicit about why: a nil at a LOW layer that a higher layer overwrites with
// a value sends the value, and a nil at a HIGH layer suppresses whatever the
// lower layers set. Filtering nils per layer instead of after the merge gets
// the first case right and the second — the one the requirement exists for —
// silently wrong.
func (p HeaderPlan) Merge() map[string]string {
	merged := map[string]*string{}
	for _, layer := range []map[string]*string{p.Attribution, p.Auth, p.Model, p.Request} {
		for k, v := range layer {
			merged[http.CanonicalHeaderKey(k)] = v
		}
	}
	out := make(map[string]string, len(merged))
	for k, v := range merged {
		if v == nil {
			continue // REQ-AUTH-02 deletion marker
		}
		out[k] = *v
	}
	return out
}

// Apply writes the merged headers onto a request, replacing rather than
// appending: Set, not Add. A provider default plus a caller override must
// produce one header, not two, and net/http sends both when they are added.
func (p HeaderPlan) Apply(r *http.Request) {
	for k, v := range p.Merge() {
		r.Header.Set(k, v)
	}
}

// PlanFor assembles the four layers for one request.
func PlanFor(m *core.Model, auth ModelAuth, opts core.RequestOptions, attribution *bool, env Env) HeaderPlan {
	return HeaderPlan{
		Attribution: AttributionLayer(attribution, env),
		Auth:        auth.Headers,
		Model:       m.Headers,
		Request:     opts.Headers,
	}
}
