package google_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/schema"
)

// ---------------------------------------------------------------- fixtures

func mk(t *testing.T, id, name, args string) core.ToolUseBlock {
	t.Helper()
	b, err := core.NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func model() *core.Model {
	return &core.Model{ID: "gemini-x", API: google.API, Provider: "google", MaxTokens: 4096}
}

// transcript is the canonical shape the asymmetry tests share: a user turn, an
// assistant turn emitting THREE parallel tool calls, and three separate
// ToolResultMessages (REQ-LOOP-02).
func transcript(t *testing.T) core.Messages {
	t.Helper()
	return core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "check three files"}}},
		core.AssistantMessage{
			Content: core.Content{
				core.TextBlock{Text: "Reading them now."},
				mk(t, "call_a", "read_file", `{"path":"a.go"}`),
				mk(t, "call_b", "read_file", `{"path":"b.go"}`),
				mk(t, "call_c", "read_file", `{"path":"c.go"}`),
			},
			StopReason: core.StopReasonToolUse,
			Provider:   "google", API: google.API, Model: "gemini-x",
		},
		core.ToolResultMessage{ToolUseID: "call_a", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of a"}}},
		core.ToolResultMessage{ToolUseID: "call_b", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of b"}}},
		core.ToolResultMessage{ToolUseID: "call_c", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of c"}}},
	}
}

func request(t *testing.T) core.Request {
	t.Helper()
	return core.Request{
		System:   []core.ContentBlock{core.TextBlock{Text: "You are a code reader."}},
		Messages: transcript(t),
		Tools: []core.ToolWire{{
			Name: "read_file", Description: "Read a file",
			InputSchema: schema.Object(schema.Prop("path", schema.String("path"))),
		}},
	}
}

// body builds the wire body and returns it both as raw JSON and decoded into
// the minimal shape these tests assert over.
type wire struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text         string `json:"text"`
			FunctionCall *struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				Name     string          `json:"name"`
				Response json.RawMessage `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"contents"`
	SystemInstruction *struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction"`
	Tools []struct {
		FunctionDeclarations []struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	Model            *string `json:"model"`
	GenerationConfig *struct {
		Temperature    *float64 `json:"temperature"`
		ThinkingConfig *struct {
			ThinkingBudget  *int  `json:"thinkingBudget"`
			IncludeThoughts *bool `json:"includeThoughts"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
}

func build(t *testing.T, m *core.Model, r core.Request) ([]byte, wire) {
	t.Helper()
	body, _, err := google.BuildRequest(m, r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return raw, w
}

// ------------------------------------------------------------------- shape

// TestAssistantTurnsUseTheRoleModelAndNeverAssistant pins the silent failure
// named in the package doc: "assistant" is not rejected by Gemini, it is
// accepted and misattributed, so no 400 ever reveals the bug.
func TestAssistantTurnsUseTheRoleModelAndNeverAssistant(t *testing.T) {
	raw, w := build(t, model(), request(t))

	var roles []string
	for _, c := range w.Contents {
		roles = append(roles, c.Role)
	}
	if strings.Join(roles, ",") != "user,model,user" {
		t.Fatalf("content roles = %v, want [user model user]", roles)
	}
	if strings.Contains(string(raw), `"assistant"`) {
		t.Fatalf("the role \"assistant\" reached the Gemini wire; it is accepted and "+
			"misattributed rather than rejected, so nothing else would catch it.\nbody: %s", raw)
	}
}

// TestToolResultsBecomeFunctionResponsePartsInOneUserContent is the Gemini
// half of REQ-LOOP-02, and the third data point the requirement needs: the
// same canonical transcript that Anthropic packs as tool_result BLOCKS and
// OpenAI cannot pack at all becomes functionResponse PARTS of a USER content.
func TestToolResultsBecomeFunctionResponsePartsInOneUserContent(t *testing.T) {
	_, w := build(t, model(), request(t))

	var carriers, parts int
	var names []string
	for _, c := range w.Contents {
		n := 0
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				n++
				names = append(names, p.FunctionResponse.Name)
			}
		}
		if n > 0 {
			carriers++
			parts = n
			if c.Role != google.RoleUser {
				t.Errorf("functionResponse parts must ride on a %q content, got %q",
					google.RoleUser, c.Role)
			}
		}
	}
	if carriers != 1 {
		t.Errorf("%d contents carry functionResponse parts, want exactly 1: Gemini groups a "+
			"parallel batch into ONE user content", carriers)
	}
	if parts != 3 {
		t.Errorf("the carrier holds %d functionResponse parts, want 3 — one per result", parts)
	}
	if strings.Join(names, ",") != "read_file,read_file,read_file" {
		t.Errorf("functionResponse names = %v", names)
	}
}

// TestFunctionResponseOrderMatchesFunctionCallOrder pins the consequence of
// this wire carrying no tool_call_id: results are paired to calls by position,
// so a reordering silently swaps the answers of two parallel calls to the same
// tool. Nothing downstream can detect it.
func TestFunctionResponseOrderMatchesFunctionCallOrder(t *testing.T) {
	_, w := build(t, model(), request(t))

	var callArgs, responses []string
	for _, c := range w.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				callArgs = append(callArgs, string(p.FunctionCall.Args))
			}
			if p.FunctionResponse != nil {
				responses = append(responses, string(p.FunctionResponse.Response))
			}
		}
	}
	wantCalls := []string{`{"path":"a.go"}`, `{"path":"b.go"}`, `{"path":"c.go"}`}
	if strings.Join(callArgs, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("functionCall args = %v, want %v", callArgs, wantCalls)
	}
	for i, want := range []string{"contents of a", "contents of b", "contents of c"} {
		if !strings.Contains(responses[i], want) {
			t.Errorf("functionResponse[%d] = %s, want it to answer %q — results are paired to "+
				"calls POSITIONALLY on this wire", i, responses[i], want)
		}
	}
}

// TestSystemPromptIsATopLevelSystemInstruction: not a message, and not a
// content with a role. Prepending it as a user content is accepted and makes
// the system prompt look like the first human turn.
func TestSystemPromptIsATopLevelSystemInstruction(t *testing.T) {
	_, w := build(t, model(), request(t))

	if w.SystemInstruction == nil {
		t.Fatal("systemInstruction is absent; the system prompt was dropped or turned into a message")
	}
	if got := w.SystemInstruction.Parts[0].Text; got != "You are a code reader." {
		t.Errorf("systemInstruction text = %q", got)
	}
	if w.SystemInstruction.Role != "" {
		t.Errorf("systemInstruction carries role %q; it takes parts and no role",
			w.SystemInstruction.Role)
	}
	for _, c := range w.Contents {
		for _, p := range c.Parts {
			if strings.Contains(p.Text, "You are a code reader") {
				t.Fatal("the system prompt also reached contents; it is a top-level field, not a message")
			}
		}
	}
}

// TestToolsAreOneWrapperObjectHoldingEveryDeclaration pins the nesting:
// tools:[{functionDeclarations:[...]}], not OpenAI's flat array of tools.
func TestToolsAreOneWrapperObjectHoldingEveryDeclaration(t *testing.T) {
	r := request(t)
	r.Tools = append(r.Tools, core.ToolWire{
		Name: "write_file", Description: "Write a file",
		InputSchema: schema.Object(schema.Prop("path", schema.String())),
	})
	_, w := build(t, model(), r)

	if len(w.Tools) != 1 {
		t.Fatalf("tools has %d entries, want exactly 1 wrapper object holding every "+
			"declaration; a flat array is a 400 on every tool-carrying request", len(w.Tools))
	}
	if n := len(w.Tools[0].FunctionDeclarations); n != 2 {
		t.Fatalf("functionDeclarations has %d entries, want 2", n)
	}
}

// TestModelIsNotABodyField: the model id is a URL segment on this API. A body
// field named model is ignored, so an implementation copied from the OpenAI
// shape talks to whichever model the URL happened to name.
func TestModelIsNotABodyField(t *testing.T) {
	_, w := build(t, model(), request(t))
	if w.Model != nil {
		t.Errorf("the body carries model=%q; Gemini takes it in the path", *w.Model)
	}
	if got := google.Path(model(), true); got != "/v1beta/models/gemini-x:streamGenerateContent?alt=sse" {
		t.Errorf("Path(stream) = %q; without alt=sse the endpoint answers with an incremental "+
			"JSON array and an SSE reader waits forever", got)
	}
	if got := google.Path(model(), false); got != "/v1beta/models/gemini-x:generateContent" {
		t.Errorf("Path(non-stream) = %q", got)
	}
}

// ------------------------------------------------------------ tool results

// TestFunctionResponseIsAlwaysAJSONObject: Gemini rejects a bare string in the
// response position, so `response: result.Text()` — the obvious encoding —
// 400s on every tool call.
func TestFunctionResponseIsAlwaysAJSONObject(t *testing.T) {
	cases := []struct {
		name   string
		result core.ToolResultMessage
		want   string
	}{
		{"plain text is wrapped",
			core.ToolResultMessage{ToolUseID: "c1", ToolName: "t",
				Content: core.Content{core.TextBlock{Text: "contents of a"}}},
			`{"output":"contents of a"}`},
		{"an error is wrapped under error",
			core.ToolResultMessage{ToolUseID: "c1", ToolName: "t", IsError: true,
				Content: core.Content{core.TextBlock{Text: "boom"}}},
			`{"error":"boom"}`},
		{"a JSON object passes through verbatim, key order intact",
			core.ToolResultMessage{ToolUseID: "c1", ToolName: "t",
				Content: core.Content{core.TextBlock{Text: `{"zeta":1,"alpha":2}`}}},
			`{"zeta":1,"alpha":2}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs := core.Messages{
				core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
				core.AssistantMessage{Content: core.Content{mk(t, "c1", "t", `{}`)},
					StopReason: core.StopReasonToolUse},
				c.result,
			}
			_, w := build(t, model(), core.Request{Messages: msgs})

			var got json.RawMessage
			for _, cc := range w.Contents {
				for _, p := range cc.Parts {
					if p.FunctionResponse != nil {
						got = p.FunctionResponse.Response
					}
				}
			}
			if got == nil {
				t.Fatal("no functionResponse reached the wire")
			}
			var obj map[string]any
			if err := json.Unmarshal(got, &obj); err != nil {
				t.Fatalf("functionResponse.response = %s, which is not a JSON object: %v", got, err)
			}
			if string(got) != c.want {
				t.Errorf("functionResponse.response = %s, want %s", got, c.want)
			}
		})
	}
}

// TestFunctionResponseNameFallsBackToTheCallItAnswers: this wire pairs by
// NAME, and a result loaded from an older session log may carry none. Emitting
// an empty name is a 400 that a transcript-level test would never produce.
func TestFunctionResponseNameFallsBackToTheCallItAnswers(t *testing.T) {
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{mk(t, "call_1", "read_file", `{}`)},
			StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "call_1", // ToolName deliberately empty
			Content: core.Content{core.TextBlock{Text: "ok"}}},
	}
	_, w := build(t, model(), core.Request{Messages: msgs})

	for _, c := range w.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				if p.FunctionResponse.Name != "read_file" {
					t.Fatalf("functionResponse.name = %q, want %q recovered from the call it answers",
						p.FunctionResponse.Name, "read_file")
				}
				return
			}
		}
	}
	t.Fatal("no functionResponse reached the wire")
}

// TestToolArgumentBytesSurviveUnchanged pins REQ-PROV-17 / REQ-TOOL-12: the
// model's own argument bytes reach the args position verbatim, in the model's
// own key order. A decode-and-re-encode round trip would sort them and shift
// the prompt-cache prefix for the rest of the session.
func TestToolArgumentBytesSurviveUnchanged(t *testing.T) {
	const args = `{"zeta":1,"alpha":{"yankee":true,"bravo":[{"zulu":"z","charlie":0}]},"mid":"x"}`
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{mk(t, "call_1", "t", args)},
			StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "t",
			Content: core.Content{core.TextBlock{Text: "ok"}}},
	}
	raw, _ := build(t, model(), core.Request{Messages: msgs})
	if !strings.Contains(string(raw), args) {
		t.Fatalf("argument bytes were reordered on the Gemini wire.\nwant substring: %s\ngot: %s", args, raw)
	}
}

// TestUnansweredToolUseIsRepairedAtSendTime: a transcript loaded from disk
// ending on an unanswered tool_use — what every Ctrl-C leaves behind — must be
// made sendable by the PROVIDER, because the loop is not running when a
// session is loaded (REQ-PROV-11).
func TestUnansweredToolUseIsRepairedAtSendTime(t *testing.T) {
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{mk(t, "call_x", "read_file", `{}`)},
			StopReason: core.StopReasonToolUse,
			Provider:   "google", API: google.API, Model: "gemini-x"},
	}
	body, rep, err := google.BuildRequest(model(), core.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SyntheticResults != 1 {
		t.Fatalf("SyntheticResults = %d, want 1: an unanswered functionCall must be answered "+
			"at send time or the request 400s", rep.SyntheticResults)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "functionResponse") ||
		!strings.Contains(string(raw), "No result provided") {
		t.Fatalf("no synthetic functionResponse reached the wire: %s", raw)
	}
}

// ---------------------------------------------------------- schema dialect

// TestSchemaDialectUppercasesTypesAndStripsRejectedKeywords pins the
// translation whose absence is a 400 on every request carrying a tool.
func TestSchemaDialectUppercasesTypesAndStripsRejectedKeywords(t *testing.T) {
	s := schema.Object(
		schema.Prop("path", schema.String("the path")),
		schema.Opt("depth", schema.Int()),
		schema.Opt("ratio", schema.Number()),
		schema.Opt("flags", schema.Array(schema.Bool())),
		schema.Opt("kind", schema.Const("file")),
	).Closed().WithExtra("$schema", "https://json-schema.org/draft/2020-12/schema")

	raw, err := google.ConvertSchema(s)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	for _, want := range []string{`"OBJECT"`, `"STRING"`, `"INTEGER"`, `"NUMBER"`, `"ARRAY"`, `"BOOLEAN"`} {
		if !strings.Contains(got, want) {
			t.Errorf("type %s is missing; Gemini's type enum has no lowercase members.\ngot: %s", want, got)
		}
	}
	for _, forbidden := range []string{"additionalProperties", "$schema", "const",
		`"object"`, `"string"`, `"integer"`, `"array"`, `"boolean"`, `"number"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("%q survived into the Gemini schema dialect; it is rejected outright.\ngot: %s",
				forbidden, got)
		}
	}
	if !strings.Contains(got, `"required":["path"]`) {
		t.Errorf("required was lost: %s", got)
	}
	if !strings.Contains(got, `"propertyOrdering":["path","depth","ratio","flags","kind"]`) {
		t.Errorf("authored property order was lost: %s", got)
	}
}

// TestNullableIsAFlagNotATypeArray: the canonical encoder emits
// "type":["string","null"], which this dialect rejects.
func TestNullableIsAFlagNotATypeArray(t *testing.T) {
	raw, err := google.ConvertSchema(schema.String("maybe").Nullable_())
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `"type":"STRING"`) || !strings.Contains(got, `"nullable":true`) {
		t.Fatalf("nullable schema = %s, want a scalar type plus nullable:true", got)
	}
	if strings.Contains(got, "[") {
		t.Fatalf("a type array reached the Gemini dialect: %s", got)
	}
}

// TestToolParametersUseTheGeminiDialect: the conversion must be wired into
// BuildRequest, not merely available. Marshalling schema.Schema directly — the
// path both other providers take — emits the canonical dialect and 400s here.
func TestToolParametersUseTheGeminiDialect(t *testing.T) {
	r := request(t)
	r.Tools[0].InputSchema = schema.Object(schema.Prop("path", schema.String())).Closed()
	raw, w := build(t, model(), r)

	params := string(w.Tools[0].FunctionDeclarations[0].Parameters)
	if !strings.Contains(params, `"OBJECT"`) || strings.Contains(params, "additionalProperties") {
		t.Fatalf("tool parameters were not translated into the Gemini dialect: %s\nbody: %s", params, raw)
	}
}

// -------------------------------------------------------------- thinking

// TestThinkingOffEmitsAnExplicitZeroBudget is REQ-PROV-16.1 with teeth: zero
// is the ONLY value that disables Gemini thinking, so a bare int with
// omitempty/omitzero drops the field and the request that meant "no thinking"
// arrives as "dynamic thinking" — and is billed for it.
func TestThinkingOffEmitsAnExplicitZeroBudget(t *testing.T) {
	raw, w := build(t, model(), core.Request{
		Messages:      core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		ThinkingLevel: core.ThinkingOff,
	})
	if w.GenerationConfig == nil || w.GenerationConfig.ThinkingConfig == nil {
		t.Fatalf("thinkingConfig is absent for an explicit off: %s", raw)
	}
	b := w.GenerationConfig.ThinkingConfig.ThinkingBudget
	if b == nil || *b != 0 {
		t.Fatalf("thinkingBudget = %v, want an explicit 0", b)
	}
	if !strings.Contains(string(raw), `"thinkingBudget":0`) {
		t.Fatalf("the explicit zero budget was omitted from the payload: %s", raw)
	}
}

// TestThinkingOffOnAFamilyThatCannotDisableUsesTheFloor is REQ-PROV-15's
// Google clause: some families cannot disable thinking and take a floor level
// instead of a zero budget, so a blanket zero is a hard failure on exactly the
// models a user picks when they want reasoning.
func TestThinkingOffOnAFamilyThatCannotDisableUsesTheFloor(t *testing.T) {
	m := model()
	m.Compat = json.RawMessage(`{"can_disable_thinking":false,"min_thinking_budget":128}`)

	_, w := build(t, m, core.Request{
		Messages:      core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		ThinkingLevel: core.ThinkingOff,
	})
	b := w.GenerationConfig.ThinkingConfig.ThinkingBudget
	if b == nil || *b != 128 {
		t.Fatalf("thinkingBudget = %v, want the family floor 128: this family rejects a zero budget", b)
	}
}

// TestThinkingBudgetComesFromTheCatalogRow: ThinkingLevelMap is the per-model
// wire value REQ-PROV-15 requires and REQ-CAT-06 diffs; a hardcoded table
// would silently misprice every model whose row disagrees.
func TestThinkingBudgetComesFromTheCatalogRow(t *testing.T) {
	m := model()
	w12345 := "12345"
	m.ThinkingLevelMap = map[core.ThinkingLevel]*string{core.ThinkingHigh: &w12345}

	_, w := build(t, m, core.Request{
		Messages:      core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		ThinkingLevel: core.ThinkingHigh,
	})
	b := w.GenerationConfig.ThinkingConfig.ThinkingBudget
	if b == nil || *b != 12345 {
		t.Fatalf("thinkingBudget = %v, want 12345 from the model's ThinkingLevelMap", b)
	}
	if it := w.GenerationConfig.ThinkingConfig.IncludeThoughts; it == nil || !*it {
		t.Error("includeThoughts was not requested for a thinking turn; without it the turn " +
			"streams nothing until the answer begins")
	}
}

// TestUnsetThinkingEmitsNoThinkingConfig is the negative control: absent and
// zero are different requests (REQ-PROV-16.1), and without this an
// implementation that always stamps a budget would pass the tests above.
func TestUnsetThinkingEmitsNoThinkingConfig(t *testing.T) {
	raw, w := build(t, model(), core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
	})
	if w.GenerationConfig != nil && w.GenerationConfig.ThinkingConfig != nil {
		t.Fatalf("thinkingConfig was invented for a caller who set no level: %s", raw)
	}
}

// TestExplicitZeroTemperatureSurvives pins REQ-PROV-16 on generationConfig:
// omitempty on a bare float64 would drop it and silently use Gemini's default.
func TestExplicitZeroTemperatureSurvives(t *testing.T) {
	zero := 0.0
	raw, w := build(t, model(), core.Request{
		Messages:    core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Temperature: &zero,
	})
	if w.GenerationConfig == nil || w.GenerationConfig.Temperature == nil {
		t.Fatalf("an explicit temperature of 0 was dropped: %s", raw)
	}
	if *w.GenerationConfig.Temperature != 0 {
		t.Fatalf("temperature = %v, want 0", *w.GenerationConfig.Temperature)
	}
}

// ---------------------------------------------------------- finish reasons

// TestMapFinishReasonReportsToolUseForSTOPWithFunctionCalls is the case
// REQ-LOOP-01 exists for, on the provider that motivates it: Gemini returns
// STOP alongside functionCall parts. It must never report an error for an
// unknown or absent reason either (ruling P-41) — StopReasonError
// short-circuits the loop AND drops the turn in REQ-PROV-11 rule 2.
func TestMapFinishReasonReportsToolUseForSTOPWithFunctionCalls(t *testing.T) {
	cases := []struct {
		reason   string
		hasCalls bool
		want     core.StopReason
	}{
		{"STOP", true, core.StopReasonToolUse}, // the whole point
		{"STOP", false, core.StopReasonStop},
		{"MAX_TOKENS", false, core.StopReasonLength},
		{"MAX_TOKENS", true, core.StopReasonLength}, // truncated calls are not executable
		{"SAFETY", false, core.StopReasonRefusal},
		{"RECITATION", false, core.StopReasonRefusal},
		{"", true, core.StopReasonToolUse},
		{"", false, core.StopReasonStop},
		{"SOMETHING_NEW", false, core.StopReasonStop},
		{"MALFORMED_FUNCTION_CALL", false, core.StopReasonStop},
	}
	for _, c := range cases {
		got := google.MapFinishReason(c.reason, c.hasCalls)
		if got != c.want {
			t.Errorf("MapFinishReason(%q, %v) = %q, want %q", c.reason, c.hasCalls, got, c.want)
		}
		if got == core.StopReasonError {
			t.Errorf("MapFinishReason(%q, %v) reported an error; no finish reason may (ruling P-41)",
				c.reason, c.hasCalls)
		}
	}
}

// TestNormalizeToolCallIDIsInjectiveOverTheCasesRepairSees: rule 5 rewrites
// ids in the repaired view and rule 6 mints synthetic results keyed by them,
// so a normalizer that collapsed two ids would delete a result.
func TestNormalizeToolCallIDIsInjectiveOverTheCasesRepairSees(t *testing.T) {
	if got := google.NormalizeToolCallID("toolu_01ABC-xyz.1"); got != "toolu_01ABC-xyz.1" {
		t.Errorf("NormalizeToolCallID mangled an already-legal id: %q", got)
	}
	if got := google.NormalizeToolCallID("call/with spaces"); got != "call_with_spaces" {
		t.Errorf("NormalizeToolCallID(%q) = %q", "call/with spaces", got)
	}
	long := strings.Repeat("x", 100)
	if got := google.NormalizeToolCallID(long); len(got) != 64 {
		t.Errorf("id length = %d, want 64", len(got))
	}
	if got := google.NormalizeToolCallID(""); got != "" {
		t.Errorf("an empty id must stay empty, got %q", got)
	}
}
