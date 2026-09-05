package catalog

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestSlashPrefixHonouredOnlyForAKnownVendor is REQ-CAT-02 rule 1, both ways.
// The two rows are the whole rule: the prefix is honoured because "anthropic"
// is in the vendor set, and it is NOT honoured for "deepseek-ai" — which is
// not a fact about the string but a fact about the catalog. An implementation
// that treats any leading segment as a vendor fails row 2; one that never
// honours a prefix fails row 1.
func TestSlashPrefixHonouredOnlyForAKnownVendor(t *testing.T) {
	c := testCatalog(t)

	t.Run("known vendor prefix resolves by vendor", func(t *testing.T) {
		r, err := c.Resolve("anthropic/claude-opus-4-5")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if r.Model.Provider != "anthropic" || r.Model.ID != "claude-opus-4-5" {
			t.Fatalf("resolved %s/%s, want anthropic/claude-opus-4-5", r.Model.Provider, r.Model.ID)
		}
		if r.Kind != MatchVendorAndID {
			t.Errorf("Kind = %s, want vendor+id", r.Kind)
		}
	})

	t.Run("unknown leading segment is part of the model id", func(t *testing.T) {
		r, err := c.Resolve("deepseek-ai/DeepSeek-V3")
		if err != nil {
			t.Fatalf("an OpenRouter-style id must match whole, got: %v", err)
		}
		if r.Model.Provider != "openrouter" {
			t.Fatalf("Provider = %q, want openrouter", r.Model.Provider)
		}
		if r.Model.ID != "deepseek-ai/DeepSeek-V3" {
			t.Fatalf("ID = %q, want the whole spec; the prefix is not a vendor", r.Model.ID)
		}
		if r.Kind != MatchBareID {
			t.Errorf("Kind = %s, want bare-id", r.Kind)
		}
		if r.Model.Cloned {
			t.Error("this is a real catalog row, not a clone")
		}
	})
}

// TestVendorAndIDOutranksBareID pins the ORDER of REQ-CAT-02 rule 2. The test
// catalog contains an OpenRouter row whose own model id is literally
// "anthropic/claude-opus-4-5", so a bare-id-first implementation resolves this
// spec to OpenRouter's base URL and API — a working request to the wrong
// vendor, at the wrong price.
func TestVendorAndIDOutranksBareID(t *testing.T) {
	c := testCatalog(t)
	r, err := c.Resolve("anthropic/claude-opus-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if r.Model.Provider != "anthropic" {
		t.Fatalf("Provider = %q, want anthropic; bare-id matching outranked vendor+id", r.Model.Provider)
	}
	if r.Model.API != core.APIAnthropicMessages {
		t.Errorf("API = %q, want anthropic-messages", r.Model.API)
	}

	// And the canonical name of that OpenRouter row still resolves to it.
	or, err := c.Resolve("openrouter/anthropic/claude-opus-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if or.Model.Provider != "openrouter" || or.Model.ID != "anthropic/claude-opus-4-5" {
		t.Fatalf("canonical resolve gave %s/%s", or.Model.Provider, or.Model.ID)
	}
	if or.Kind != MatchExactCanonical {
		t.Errorf("Kind = %s, want exact-canonical", or.Kind)
	}
}

// TestAmbiguousBareIDIsAnErrorNotAGuess is REQ-CAT-02 rule 2's last sentence.
// Both candidates share an API and a context window, so a guess would produce
// a request that succeeds against the wrong endpoint with the wrong
// credentials — the failure is silent, which is why the rule is "error".
func TestAmbiguousBareIDIsAnErrorNotAGuess(t *testing.T) {
	c := testCatalog(t)

	m, err := c.ResolveModel("gpt-4o")
	if err == nil {
		t.Fatalf("resolved an ambiguous bare id to %s/%s instead of erroring", m.Provider, m.ID)
	}
	if !errors.Is(err, ErrAmbiguousModel) {
		t.Fatalf("error = %v, want ErrAmbiguousModel", err)
	}
	msg := err.Error()
	for _, want := range []string{"azure/gpt-4o", "openai/gpt-4o"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name candidate %q", msg, want)
		}
	}

	// Naming either vendor resolves it: ambiguity is about the spec, never a
	// rejection of the model.
	for _, spec := range []string{"azure/gpt-4o", "openai/gpt-4o"} {
		if _, err := c.ResolveModel(spec); err != nil {
			t.Errorf("%s: %v", spec, err)
		}
	}
}

// TestSiblingCloneInheritsEverythingButIDAndName is REQ-CAT-03. It compares
// every exported field of core.Model reflectively rather than listing the six
// the requirement names, so a field added to the descriptor later cannot be
// silently dropped from the clone.
func TestSiblingCloneInheritsEverythingButIDAndName(t *testing.T) {
	c := testCatalog(t)

	template, err := c.ResolveModel("anthropic/claude-sonnet-4-5") // the vendor's default_model
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Resolve("anthropic/claude-opus-9-9-20991231")
	if err != nil {
		t.Fatalf("an unknown id under a KNOWN vendor must resolve, not error: %v", err)
	}
	clone := r.Model

	if r.Kind != MatchSiblingClone {
		t.Errorf("Kind = %s, want sibling-clone", r.Kind)
	}
	if clone.ID != "claude-opus-9-9-20991231" {
		t.Errorf("ID = %q, want the requested id", clone.ID)
	}
	if clone.Name != "claude-opus-9-9-20991231" {
		t.Errorf("Name = %q, want the requested id", clone.Name)
	}
	if !clone.Cloned || clone.ClonedFrom != "anthropic/claude-sonnet-4-5" {
		t.Errorf("Cloned/ClonedFrom = %v/%q, want true/anthropic/claude-sonnet-4-5", clone.Cloned, clone.ClonedFrom)
	}

	swapped := map[string]bool{"ID": true, "Name": true, "Cloned": true, "ClonedFrom": true}
	tv, cv := reflect.ValueOf(*template), reflect.ValueOf(*clone)
	rt := tv.Type()
	inherited := 0
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() || swapped[f.Name] {
			continue
		}
		inherited++
		if !reflect.DeepEqual(tv.Field(i).Interface(), cv.Field(i).Interface()) {
			t.Errorf("field %s not inherited: template %v, clone %v",
				f.Name, tv.Field(i).Interface(), cv.Field(i).Interface())
		}
	}
	// REQ-CAT-03 names six inherited fields; if the descriptor ever shrinks
	// below that the reflective sweep would pass vacuously.
	if inherited < 6 {
		t.Fatalf("only %d fields compared; the sweep is not covering the descriptor", inherited)
	}

	// The clone must be independent of the template AND of the catalog.
	clone.Headers["anthropic-version"] = strp("tampered")
	again, err := c.ResolveModel("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Headers["anthropic-version"]; got == nil || *got != "2023-06-01" {
		t.Errorf("editing a clone changed the catalog row: %v", got)
	}
}

// TestCloneWarningIsSurfacedOnTheValueAndAsText: REQ-CAT-03's "with a
// warning". The durable half (Cloned/ClonedFrom) travels with the descriptor
// into the session log and the budget gate; the rendered half is for display.
// Neither is a log line — see the Resolution doc comment.
func TestCloneWarningIsSurfacedOnTheValueAndAsText(t *testing.T) {
	c := testCatalog(t)

	r, err := c.Resolve("openai/gpt-5-nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if r.Warning == "" {
		t.Fatal("a sibling clone produced no warning")
	}
	for _, want := range []string{"gpt-5-nonexistent", "openai/gpt-4o", "guess"} {
		if !strings.Contains(r.Warning, want) {
			t.Errorf("warning %q does not mention %q", r.Warning, want)
		}
	}

	hit, err := c.Resolve("openai/gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if hit.Warning != "" {
		t.Errorf("a catalog hit warned: %q", hit.Warning)
	}
	if hit.Model.Cloned || hit.Model.ClonedFrom != "" {
		t.Error("a catalog hit is marked as a clone")
	}
}

// TestUnknownVendorIsAConfigurationError, and TestBareUnknownIDBlamesTheVendor
// below, are the two halves of REQ-CAT-03's last sentence: "nothing in the
// resolution path may reject a model ID solely because it is absent from the
// catalog". Both errors here are about the VENDOR — under any known vendor the
// same id would have resolved.
func TestUnknownVendorIsAConfigurationError(t *testing.T) {
	c := testCatalog(t)

	_, err := c.ResolveModel("acme/super-model-1")
	if !errors.Is(err, ErrUnknownVendor) {
		t.Fatalf("error = %v, want ErrUnknownVendor", err)
	}
	if !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "known vendors") {
		t.Errorf("error %q should name the bad vendor and the known ones", err)
	}

	// The same id under a known vendor is fine — proving the id was never the
	// objection.
	if _, err := c.ResolveModel("anthropic/super-model-1"); err != nil {
		t.Errorf("known vendor + unknown id must clone, got %v", err)
	}
}

func TestBareUnknownIDBlamesTheVendorNotTheModel(t *testing.T) {
	c := testCatalog(t)

	_, err := c.ResolveModel("llama-3-70b")
	if !errors.Is(err, ErrUnresolvedModel) {
		t.Fatalf("error = %v, want ErrUnresolvedModel", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "prefix it with a vendor") {
		t.Errorf("error %q must name the fix; the model id is not the objection", msg)
	}
	if strings.Contains(msg, "not in the catalog") {
		t.Errorf("error %q rejects the id for being absent, which REQ-CAT-03 forbids", msg)
	}
}

func TestEmptySpecIsAnError(t *testing.T) {
	c := testCatalog(t)
	for _, spec := range []string{"", "   "} {
		if _, err := c.ResolveModel(spec); !errors.Is(err, ErrEmptySpec) {
			t.Errorf("Resolve(%q) error = %v, want ErrEmptySpec", spec, err)
		}
	}
}

// TestResolutionExposesTheResolvedModel is REQ-CAT-07: the caller can inspect
// which catalog row (or clone) was used.
func TestResolutionExposesTheResolvedModel(t *testing.T) {
	r, err := Resolve("anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if r.ResolvedModel() != r.Model {
		t.Error("ResolvedModel() must return the descriptor actually used")
	}
	if r.ResolvedModel().ID != "claude-haiku-4-5" {
		t.Errorf("ID = %q", r.ResolvedModel().ID)
	}

	m, err := ResolveModel("claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "anthropic" {
		t.Errorf("package-level ResolveModel gave provider %q", m.Provider)
	}
}

func TestMatchKindString(t *testing.T) {
	for k, want := range map[MatchKind]string{
		MatchExactCanonical: "exact-canonical",
		MatchVendorAndID:    "vendor+id",
		MatchBareID:         "bare-id",
		MatchSiblingClone:   "sibling-clone",
		MatchKind(99):       "MatchKind(99)",
	} {
		if got := k.String(); got != want {
			t.Errorf("MatchKind(%d).String() = %q, want %q", uint8(k), got, want)
		}
	}
}

// A spec that names a known vendor with an empty id ("anthropic/") must not
// clone a model whose id is the empty string.
func TestVendorPrefixWithNoIDDoesNotCloneAnEmptyModel(t *testing.T) {
	c := testCatalog(t)
	if m, err := c.ResolveModel("anthropic/"); err == nil {
		t.Fatalf("resolved to id %q; an empty model id is not a model", m.ID)
	}
}
