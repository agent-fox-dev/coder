package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/schema"
)

// These pin the REQ-PROV-12 profile: inferred from Provider+BaseURL, then
// overridden key by key from the catalog row, with an unknown key failing
// loudly rather than being ignored — which is how every override in the
// catalog was silently lost.

func model(vendor, id, base string) *core.Model {
	return &core.Model{
		ID: id, API: openai.API, Provider: vendor, BaseURL: base,
		ContextWindow: 128000, MaxTokens: 4096, Input: []string{"text"},
	}
}

func body(t *testing.T, m *core.Model, req core.Request) map[string]any {
	t.Helper()
	r, _, err := openai.BuildRequest(m, req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func userReq() core.Request {
	return core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}}}
}

// ---- REQ-PROV-12 inference

func TestTheProfileIsInferredFromTheHost(t *testing.T) {
	cases := []struct {
		name  string
		m     *core.Model
		check func(t *testing.T, c openai.Compat)
	}{
		{"api.openai.com keeps the default", model("openai", "gpt-4o", "https://api.openai.com/v1"),
			func(t *testing.T, c openai.Compat) {
				if c != openai.DefaultCompat() {
					t.Fatalf("profile = %+v, want the default", c)
				}
			}},
		{"deepseek", model("deepseek", "deepseek-chat", "https://api.deepseek.com"),
			func(t *testing.T, c openai.Compat) {
				if !c.UseMaxTokens || c.SupportsStore || c.SupportsDeveloperRole || c.ThinkingFormat != "deepseek" {
					t.Fatalf("profile = %+v, want max_tokens, no store, no developer role, deepseek thinking", c)
				}
			}},
		{"together by host alone", model("some-gateway", "llama", "https://api.together.xyz/v1"),
			func(t *testing.T, c openai.Compat) {
				if !c.UseMaxTokens || c.SupportsStrictTools || c.SupportsLongCacheRetention ||
					c.SupportsReasoningEffort || c.SupportsStore {
					t.Fatalf("profile = %+v, want the Together row of the REQ-PROV-12 table", c)
				}
			}},
		{"openrouter anthropic route", model("openrouter", "anthropic/claude-sonnet-4-5", "https://openrouter.ai/api/v1"),
			func(t *testing.T, c openai.Compat) {
				if c.SupportsStore || !c.SupportsDeveloperRole || c.CacheControlFormat != "anthropic" {
					t.Fatalf("profile = %+v, want no store, developer role, anthropic cache_control", c)
				}
			}},
		{"openrouter other route", model("openrouter", "meta-llama/llama-3", "https://openrouter.ai/api/v1"),
			func(t *testing.T, c openai.Compat) {
				if c.SupportsStore || c.SupportsDeveloperRole || c.CacheControlFormat != "" {
					t.Fatalf("profile = %+v, want neither store nor developer role", c)
				}
			}},
		{"xai", model("xai", "grok", "https://api.x.ai/v1"),
			func(t *testing.T, c openai.Compat) {
				if c.SupportsReasoningEffort || c.SupportsStore {
					t.Fatalf("profile = %+v, want no reasoning_effort and no store", c)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := openai.CompatFor(tc.m)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, c)
		})
	}
}

func TestTheCatalogRowOverridesTheInferredProfileKeyByKey(t *testing.T) {
	m := model("deepseek", "deepseek-chat", "https://api.deepseek.com")
	m.Compat = json.RawMessage(`{"use_max_tokens": false}`)
	c, err := openai.CompatFor(m)
	if err != nil {
		t.Fatal(err)
	}
	if c.UseMaxTokens {
		t.Fatal("the row said use_max_tokens:false and the inferred profile won")
	}
	if c.SupportsStore {
		t.Fatal("a row override of one key must leave the inferred value of another alone")
	}
}

// TestAnUnknownCompatKeyFailsLoudly is what stops the original bug recurring:
// the catalog rows were written in a vocabulary this struct did not read, and
// json.Unmarshal ignored every key without a sound.
func TestAnUnknownCompatKeyFailsLoudly(t *testing.T) {
	m := model("openai", "o3", "https://api.openai.com/v1")
	m.Compat = json.RawMessage(`{"max_tokens_field": "max_completion_tokens"}`)
	if _, err := openai.CompatFor(m); !errors.Is(err, openai.ErrUnknownCompatKey) {
		t.Fatalf("err = %v, want ErrUnknownCompatKey", err)
	}
	// Through Stream it is a pre-closed error stream (REQ-PROV-04), not a
	// request sent with the override silently dropped.
	s := openai.Provider(openai.Options{}).Stream(context.Background(), m, userReq(), core.ProviderStreamOptions{})
	msg := s.Result()
	if msg.StopReason != core.StopReasonError || !strings.Contains(msg.ErrorMessage, "max_tokens_field") {
		t.Fatalf("stop=%q err=%q, want an error naming the unknown key", msg.StopReason, msg.ErrorMessage)
	}
}

// TestEveryCatalogRowIsUnderstood keeps the embedded catalog and this profile
// in one vocabulary: a row this struct cannot decode is a row whose overrides
// never happen.
func TestEveryCatalogRowIsUnderstood(t *testing.T) {
	c := catalog.Default()
	n := 0
	for _, vendor := range c.Vendors() {
		for _, id := range c.Models(vendor) {
			m, err := c.ResolveModel(vendor + "/" + id)
			if err != nil {
				t.Fatal(err)
			}
			if m.API != openai.API {
				continue
			}
			if _, err := openai.CompatFor(m); err != nil {
				t.Errorf("%s/%s: %v", vendor, id, err)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no openai-completions rows in the catalog; the test checked nothing")
	}
}

// ---- wired flags

func TestARowThatRejectsTemperatureOmitsIt(t *testing.T) {
	m := model("openai", "o3", "https://api.openai.com/v1")
	m.Compat = json.RawMessage(`{"use_max_tokens": false, "supports_temperature": false}`)
	temp, topP, max := 0.2, 0.9, 512
	req := userReq()
	req.Temperature, req.TopP, req.MaxTokens = &temp, &topP, &max
	got := body(t, m, req)
	if _, ok := got["temperature"]; ok {
		t.Fatalf("temperature was sent to a row that rejects it: %v", got)
	}
	if _, ok := got["top_p"]; ok {
		t.Fatalf("top_p was sent to a row that rejects it: %v", got)
	}
	if got["max_completion_tokens"] != float64(512) || got["max_tokens"] != nil {
		t.Fatalf("max tokens field = %v / %v, want max_completion_tokens only", got["max_completion_tokens"], got["max_tokens"])
	}
}

func TestAnUntrustedFinishReasonIsInferredFromContent(t *testing.T) {
	whole := `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"edit","arguments":"{}"}}]},` +
		`"finish_reason":"length"}]}`
	trusted := model("openai", "gpt-4o", "https://api.openai.com/v1")
	msg, err := openai.DecodeResponse(trusted, []byte(whole), nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != core.StopReasonLength {
		t.Fatalf("stop = %q on a trusted server, want length", msg.StopReason)
	}
	untrusted := model("openai", "gpt-4o", "https://api.openai.com/v1")
	untrusted.Compat = json.RawMessage(`{"supports_finish_reason": false}`)
	msg, err = openai.DecodeResponse(untrusted, []byte(whole), nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != core.StopReasonToolUse {
		t.Fatalf("stop = %q, want tool_use inferred from the content when finish_reason cannot be trusted", msg.StopReason)
	}
	if msg.RawStopReason != "length" {
		t.Fatalf("raw stop reason = %q, want the server's own string preserved", msg.RawStopReason)
	}
}

func TestCacheControlAndItsTTLFollowTheProfile(t *testing.T) {
	long := core.CacheRetentionLong
	req := core.Request{
		System:   []core.ContentBlock{core.TextBlock{Text: "sys"}},
		Messages: userReq().Messages,
		Options:  core.RequestOptions{CacheRetention: &long},
	}
	ccOf := func(got map[string]any, i int) map[string]any {
		msgs := got["messages"].([]any)
		parts, ok := msgs[i].(map[string]any)["content"].([]any)
		if !ok {
			return nil
		}
		cc, _ := parts[len(parts)-1].(map[string]any)["cache_control"].(map[string]any)
		return cc
	}

	or := model("openrouter", "anthropic/claude-sonnet-4-5", "https://openrouter.ai/api/v1")
	got := body(t, or, req)
	if cc := ccOf(got, 0); cc == nil || cc["ttl"] != "1h" {
		t.Fatalf("system cache_control = %v, want ephemeral with ttl 1h on an OpenRouter anthropic route", cc)
	}
	if cc := ccOf(got, 1); cc == nil {
		t.Fatalf("last user message carries no cache_control: %v", got["messages"])
	}

	or.Compat = json.RawMessage(`{"supports_long_cache_retention": false}`)
	got = body(t, or, req)
	if cc := ccOf(got, 0); cc == nil || cc["ttl"] != nil {
		t.Fatalf("cache_control = %v, want ephemeral WITHOUT ttl where the gateway rejects it", cc)
	}

	plain := model("openai", "gpt-4o", "https://api.openai.com/v1")
	raw, _ := json.Marshal(body(t, plain, req))
	if strings.Contains(string(raw), "cache_control") {
		t.Fatalf("cache_control emitted on a wire that has no such field: %s", raw)
	}
}

// ---- REQ-PROV-15

func TestReasoningEffortIsClampedNeverPassedThrough(t *testing.T) {
	low, medium, high, max := "low", "medium", "high", "max"
	ladder := func(entries map[core.ThinkingLevel]*string) *core.Model {
		m := model("openai", "o3", "https://api.openai.com/v1")
		m.Reasoning, m.ThinkingLevelMap = true, entries
		return m
	}
	at := func(m *core.Model, lvl core.ThinkingLevel) any {
		req := userReq()
		req.ThinkingLevel = lvl
		return body(t, m, req)["reasoning_effort"]
	}

	plain := ladder(map[core.ThinkingLevel]*string{core.ThinkingLow: &low, core.ThinkingMedium: &medium, core.ThinkingHigh: &high})
	if got := at(plain, core.ThinkingXHigh); got != "high" {
		t.Fatalf("reasoning_effort = %v for xhigh on a plain reasoning model, want high (clamped DOWN); "+
			"passing xhigh through is a 400", got)
	}
	optIn := ladder(map[core.ThinkingLevel]*string{core.ThinkingHigh: &high, core.ThinkingXHigh: nil, core.ThinkingMax: &max})
	if got := at(optIn, core.ThinkingXHigh); got != "max" {
		t.Fatalf("reasoning_effort = %v for xhigh on {xhigh:null,max:max}, want max (clamped UP)", got)
	}
	noMap := model("openai", "gpt-4o", "https://api.openai.com/v1")
	if got := at(noMap, core.ThinkingHigh); got != nil {
		t.Fatalf("reasoning_effort = %v on a model with no map, want the key omitted", got)
	}
}

// ---- REQ-TOOL-03

func TestStrictRidesOnlyOnTheRewrittenSchema(t *testing.T) {
	m := model("openai", "gpt-4o", "https://api.openai.com/v1")
	strict := func(mode core.StrictMode, s *schema.Schema) core.Request {
		req := userReq()
		req.Tools = []core.ToolWire{{Name: "find", InputSchema: s,
			ConstrainedSampling: &core.ConstrainedSampling{Type: core.ConstrainJSONSchema, Strict: mode}}}
		return req
	}
	fn := func(got map[string]any) map[string]any {
		return got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	}

	optional := schema.Object(schema.Prop("pattern", schema.String()), schema.Opt("limit", schema.Int()))
	f := fn(body(t, m, strict(core.StrictPrefer, optional)))
	if f["strict"] != true {
		t.Fatalf("function = %v, want strict:true on a schema the rewrite accepts", f)
	}
	params := f["parameters"].(map[string]any)
	if params["additionalProperties"] != false {
		t.Fatalf("parameters = %v, want the object closed", params)
	}
	if req, _ := params["required"].([]any); len(req) != 2 {
		t.Fatalf("required = %v, want every property listed", req)
	}
	limit := params["properties"].(map[string]any)["limit"].(map[string]any)
	if _, widened := limit["anyOf"]; !widened {
		t.Fatalf("limit = %v, want the formerly-optional property widened to nullable so the model can still omit it", limit)
	}

	withRef := schema.Object(schema.Prop("q", schema.String().WithExtra("$ref", "#/$defs/q")))
	f = fn(body(t, m, strict(core.StrictPrefer, withRef)))
	if _, present := f["strict"]; present {
		t.Fatalf("function = %v: $ref cannot be rewritten, and prefer falls back to unconstrained", f)
	}
	if !strings.Contains(string(mustJSON(t, f["parameters"])), "$ref") {
		t.Fatal("the unconstrained fallback must send the caller's own schema")
	}

	msg := openai.Provider(openai.Options{}).Stream(context.Background(), m,
		strict(core.StrictRequire, withRef), core.ProviderStreamOptions{}).Result()
	if msg.StopReason != core.StopReasonError || !strings.Contains(msg.ErrorMessage, "$ref") {
		t.Fatalf("require on a $ref schema: stop=%q err=%q, want a pre-closed error stream naming $ref",
			msg.StopReason, msg.ErrorMessage)
	}

	// Gated on the profile: a host that cannot emit strict cannot satisfy
	// require either, and prefer sends unconstrained.
	together := model("together", "llama", "https://api.together.xyz/v1")
	f = fn(body(t, together, strict(core.StrictPrefer, optional)))
	if _, present := f["strict"]; present {
		t.Fatalf("function = %v: the Together profile does not support strict tools", f)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
