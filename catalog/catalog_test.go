package catalog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// testCatalogJSON is a purpose-built catalog for the REQ-CAT-02 matching
// rules. It is deliberately NOT the shipped catalog: the rules it exercises
// (an ambiguous bare id, an OpenRouter-style row whose own id contains a
// slash) are configurations the shipped catalog must NOT have, and pinning
// them here lets the shipped catalog stay usable while the rules stay tested.
const testCatalogJSON = `{
  "schema_version": 1,
  "catalog_version": "test-1",
  "vendors": {
    "anthropic": {
      "api": "anthropic-messages",
      "base_url": "https://api.anthropic.com",
      "headers": {"anthropic-version": "2023-06-01"},
      "default_model": "claude-sonnet-4-5",
      "models": {
        "claude-opus-4-5": {"name": "Claude Opus 4.5", "context_window": 200000, "max_tokens": 64000,
          "cost": {"input": 5, "output": 25}, "input": ["text","image"], "reasoning": true,
          "thinking_level_map": {"off": "disabled", "minimal": null, "low": "low", "medium": "medium", "high": "high"}},
        "claude-sonnet-4-5": {"name": "Claude Sonnet 4.5", "context_window": 200000, "max_tokens": 64000,
          "cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75},
          "compat": {"beta_headers": ["fine-grained-tool-streaming"]},
          "input": ["text","image"], "reasoning": true,
          "thinking_level_map": {"off": "disabled", "low": "4096", "medium": "16384", "high": "32768"}}
      }
    },
    "openai": {
      "api": "openai-completions", "base_url": "https://api.openai.com/v1", "default_model": "gpt-4o",
      "models": {
        "gpt-4o": {"context_window": 128000, "max_tokens": 16384, "cost": {"input": 2.5, "output": 10}}
      }
    },
    "azure": {
      "api": "openai-completions", "base_url": "https://example.openai.azure.com", "default_model": "gpt-4o",
      "models": {
        "gpt-4o": {"context_window": 128000, "max_tokens": 16384, "cost": {"input": 2.5, "output": 10}}
      }
    },
    "openrouter": {
      "api": "openai-completions", "base_url": "https://openrouter.ai/api/v1",
      "default_model": "anthropic/claude-opus-4-5",
      "models": {
        "anthropic/claude-opus-4-5": {"context_window": 200000, "max_tokens": 64000, "cost": {"input": 5, "output": 25}},
        "deepseek-ai/DeepSeek-V3": {"context_window": 64000, "max_tokens": 8192, "cost": {"input": 0.3, "output": 0.9}},
        "openai/o4-preview": {"context_window": 200000, "max_tokens": 100000, "cost": {"input": 2, "output": 8}}
      }
    }
  }
}`

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := Parse([]byte(testCatalogJSON))
	if err != nil {
		t.Fatalf("test catalog does not parse: %v", err)
	}
	return c
}

func strp(s string) *string { return &s }

func TestEmbeddedCatalogPopulatesTheREQPROV10Descriptor(t *testing.T) {
	c := Default()

	opus, err := c.ResolveModel("anthropic/claude-opus-4-5")
	if err != nil {
		t.Fatalf("resolve opus: %v", err)
	}
	if opus.API != core.APIAnthropicMessages {
		t.Errorf("API = %q, want %q", opus.API, core.APIAnthropicMessages)
	}
	if opus.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", opus.Provider)
	}
	if opus.BaseURL == "" {
		t.Error("BaseURL is empty; it is inherited from the vendor and no request can be built without it")
	}
	if got := opus.Headers["anthropic-version"]; got == nil || *got == "" {
		t.Errorf("Headers[anthropic-version] = %v, want the vendor-inherited version header", got)
	}
	if opus.ContextWindow != 200000 || opus.MaxTokens != 64000 {
		t.Errorf("window/max = %d/%d, want 200000/64000", opus.ContextWindow, opus.MaxTokens)
	}
	if opus.Cost.Input != 5 || opus.Cost.Output != 25 {
		t.Errorf("cost = %v/%v per 1M, want 5/25", opus.Cost.Input, opus.Cost.Output)
	}
	if !opus.SupportsImages() {
		t.Error("SupportsImages() = false; REQ-CAT-05 would replace every image block")
	}
	if !opus.Reasoning {
		t.Error("Reasoning = false")
	}
	if opus.Cloned {
		t.Error("a catalog hit must not be marked Cloned")
	}

	o3, err := c.ResolveModel("o3")
	if err != nil {
		t.Fatalf("resolve o3: %v", err)
	}
	if o3.API != core.APIOpenAICompletions {
		t.Errorf("o3 API = %q", o3.API)
	}
	var compat map[string]any
	if err := json.Unmarshal(o3.Compat, &compat); err != nil {
		t.Fatalf("o3 compat is not an object: %v", err)
	}
	if compat["max_tokens_field"] != "max_completion_tokens" {
		t.Errorf("o3 compat.max_tokens_field = %v, want max_completion_tokens", compat["max_tokens_field"])
	}
	// Present-and-null: REQ-PROV-15's "explicitly unsupported".
	w, present := o3.ThinkingLevelMap[core.ThinkingOff]
	if !present {
		t.Error("o3 thinking map has no entry for off; the snapshot recorded it as present-null on purpose")
	}
	if w != nil {
		t.Errorf("o3 thinking map off = %q, want null (this model cannot stop reasoning)", *w)
	}
}

// TestCorruptCatalogPanicsOnFirstUseNotAtInit pins ruling P-15, which
// reconciles REQ-CAT-01 ("panics at init") with NFR-SEC-05 ("no init() global
// mutation"): building the accessor must be silent, the FIRST USE must panic,
// and every later use must panic with the same value. A wrong implementation
// that panics in init() cannot be tested at all, and one that parses eagerly
// per call would produce a fresh (non-identical) panic value each time.
func TestCorruptCatalogPanicsOnFirstUseNotAtInit(t *testing.T) {
	corrupt := []byte(`{"schema_version": 1, "vendors": {`)

	get := onceCatalog(corrupt) // must not panic: this is the "init" moment

	first := recoverPanic(t, get)
	if first == nil {
		t.Fatal("first use of a corrupt catalog did not panic; REQ-CAT-01 forbids a silently empty catalog")
	}
	msg, ok := first.(string)
	if !ok || !strings.Contains(msg, "corrupt") {
		t.Fatalf("panic value = %#v, want a string naming the corruption", first)
	}

	second := recoverPanic(t, get)
	if second != first {
		t.Fatalf("second use panicked with %#v, want the identical value %#v; sync.OnceValue must\n"+
			"re-panic with the stored value, not re-parse", second, first)
	}
}

func TestGoodCatalogNeverPanics(t *testing.T) {
	get := onceCatalog([]byte(testCatalogJSON))
	if p := recoverPanic(t, func() *Catalog { return get() }); p != nil {
		t.Fatalf("valid catalog panicked: %v", p)
	}
	if got := get().Version(); got != "test-1" {
		t.Errorf("Version() = %q, want test-1", got)
	}
	// Parsed ONCE, not once per call: the accessor memoizes the value, which
	// is the same property that makes the corrupt case re-panic with the
	// stored value rather than re-deriving one (P-15).
	if get() != get() {
		t.Error("the catalog was re-parsed; onceCatalog must memoize")
	}
	if Default() != Default() {
		t.Error("Default() re-parses the embedded catalog on every call")
	}
}

func recoverPanic(t *testing.T, f func() *Catalog) (p any) {
	t.Helper()
	defer func() { p = recover() }()
	f()
	return nil
}

func TestParseRejectsCorruptCatalogs(t *testing.T) {
	// Each row is one way a hand-edited catalog goes wrong during the
	// REQ-CAT-06 regeneration. Every one of them would otherwise produce a
	// catalog that loads and lies.
	tests := []struct {
		name string
		json string
		want string // substring of the error
	}{
		{"not json", `{`, "unexpected end"},
		{"future schema version", `{"schema_version":2,"vendors":{}}`, "this build understands 1"},
		{"no vendors", `{"schema_version":1,"vendors":{}}`, "declares no vendors"},
		{"vendor with no models", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"m","models":{}}}}`, "declares no models"},
		{"missing default_model", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","models":{"m":{}}}}}`, "default_model"},
		{"dangling default_model", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"nope","models":{"m":{}}}}}`, "not a model of this vendor"},
		{"vendor id with slash", `{"schema_version":1,"vendors":{"a/b":{"api":"faux","base_url":"x","default_model":"m","models":{"m":{}}}}}`, "may not contain a slash"},
		{"row id disagrees with key", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"m","models":{"m":{"id":"other"}}}}}`, "keyed"},
		{"no api anywhere", `{"schema_version":1,"vendors":{"v":{"base_url":"x","default_model":"m","models":{"m":{}}}}}`, "no default api"},
		{"no base_url anywhere", `{"schema_version":1,"vendors":{"v":{"api":"faux","default_model":"m","models":{"m":{}}}}}`, "no default base_url"},
		{"negative price", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"m","models":{"m":{"cost":{"input":-1}}}}}}`, "cost.input"},
		{"negative window", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"m","models":{"m":{"context_window":-1}}}}}`, "context_window"},
		{"misspelled thinking level", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"m","models":{"m":{"reasoning":true,"thinking_level_map":{"higth":"high"}}}}}}`, "unknown thinking level"},
		{"thinking without reasoning", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"m","models":{"m":{"thinking_level_map":{"high":"high"}}}}}}`, "reasoning"},
		{"unsorted cost tiers", `{"schema_version":1,"vendors":{"v":{"api":"faux","base_url":"x","default_model":"m","models":{"m":{"cost":{"input":1,"tiers":[{"threshold":200000},{"threshold":100000}]}}}}}}`, "ascending"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json))
			if err == nil {
				t.Fatalf("Parse accepted a corrupt catalog")
			}
			if !errors.Is(err, ErrCorruptCatalog) {
				t.Errorf("error does not match ErrCorruptCatalog: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestUnknownAPIStringLoads: REQ-PROV-09 lets a third party register a new API
// value via a BackendPlugin, so a catalog row naming an API this build has
// never heard of must LOAD. Only an empty API is corrupt.
func TestUnknownAPIStringLoads(t *testing.T) {
	c, err := Parse([]byte(`{"schema_version":1,"vendors":{"acme":{"api":"acme-chat-v9",
		"base_url":"https://acme.example","default_model":"m","models":{"m":{}}}}}`))
	if err != nil {
		t.Fatalf("Parse rejected an unknown api string: %v", err)
	}
	m, err := c.ResolveModel("acme/m")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.API != core.API("acme-chat-v9") {
		t.Errorf("API = %q, want the row's own value verbatim", m.API)
	}
}

// TestResolvedModelIsADeepCopy: the catalog is a process-wide singleton and
// REQ-CAT-03 invites callers to "override any inherited field". Without a deep
// copy, one caller's override silently rewrites every later resolution in the
// process.
func TestResolvedModelIsADeepCopy(t *testing.T) {
	c := testCatalog(t)

	first, err := c.ResolveModel("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	first.MaxTokens = 1
	first.Headers["anthropic-version"] = strp("tampered")
	first.Headers["x-added"] = strp("1")
	first.Input[0] = "tampered"
	first.Compat[0] = ' '
	first.ThinkingLevelMap[core.ThinkingHigh] = nil
	first.Cost.Input = 999

	second, err := c.ResolveModel("anthropic/claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if second.MaxTokens != 64000 {
		t.Errorf("MaxTokens = %d, want 64000", second.MaxTokens)
	}
	if got := second.Headers["anthropic-version"]; got == nil || *got != "2023-06-01" {
		t.Errorf("Headers[anthropic-version] = %v, want 2023-06-01", got)
	}
	if _, added := second.Headers["x-added"]; added {
		t.Error("a header added to a resolved model leaked back into the catalog")
	}
	if second.Input[0] != "text" {
		t.Errorf("Input[0] = %q, want text", second.Input[0])
	}
	if !json.Valid(second.Compat) {
		t.Errorf("Compat was mutated through the shared backing array: %s", second.Compat)
	}
	if !SupportsThinkingLevel(second, core.ThinkingHigh) {
		t.Error("ThinkingLevelMap was mutated through the shared map")
	}
	if second.Cost.Input != 3 {
		t.Errorf("Cost.Input = %v, want 3", second.Cost.Input)
	}
}

// TestEmbeddedCatalogHasNoAmbiguousBareIDs: an ambiguous bare id is a correct
// resolution outcome (an error) but a bad shipping decision — it makes a
// plain, obvious spec unusable. This is the invariant that keeps the SHIPPED
// catalog honest while TestAmbiguousBareIDIsAnErrorNotAGuess tests the rule.
func TestEmbeddedCatalogHasNoAmbiguousBareIDs(t *testing.T) {
	c := Default()
	for _, vendor := range c.Vendors() {
		for _, id := range c.Models(vendor) {
			if _, err := c.ResolveModel(id); err != nil {
				t.Errorf("bare id %q (vendor %q) does not resolve: %v", id, vendor, err)
			}
		}
	}
}

func TestCatalogAccessors(t *testing.T) {
	c := Default()
	if c.Version() == "" {
		t.Error("Version() is empty; REQ-CAT-01 versions the catalog separately from the SDK")
	}
	if !strings.Contains(c.Note(), "REQ-CAT-06") {
		t.Error("Note() does not carry the regeneration ritual")
	}
	if got := c.Vendors(); len(got) != 2 || got[0] != "anthropic" || got[1] != "openai" {
		t.Errorf("Vendors() = %v, want sorted [anthropic openai]", got)
	}
	if !c.KnownVendor("openai") || c.KnownVendor("deepseek-ai") {
		t.Error("KnownVendor is the predicate REQ-CAT-02 rule 1 turns on")
	}
	if c.DefaultModelID("anthropic") == "" {
		t.Error("anthropic has no default_model; REQ-CAT-03 has nothing to clone")
	}
	if c.DefaultModelID("nope") != "" || c.Models("nope") != nil {
		t.Error("accessors must report nothing for an unknown vendor")
	}
	// Vendors() must hand out a copy, not the index's own slice.
	vs := c.Vendors()
	vs[0] = "tampered"
	if c.Vendors()[0] != "anthropic" {
		t.Error("Vendors() aliases internal state")
	}
}
