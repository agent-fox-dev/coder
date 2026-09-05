package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/jsonx"
)

// The message and content codec.
//
// core declares the canonical types but ships no JSON codec for them, and it
// cannot: core.Message and core.ContentBlock are sealed interfaces, so
// encoding/json can marshal them by dynamic dispatch but has no way to
// unmarshal one. The log format is machinery, so the codec lives here.
//
// Every encode path builds a jsonx.OrderedObject rather than a map, for the
// reason jsonx exists: encoding/json sorts map keys unconditionally, and this
// file is the one place where a session written by one build is read by
// another. Scalars that came off the wire — tool_use.input above all — are
// carried through as verbatim bytes and never laundered through float64
// (NFR-TEST-03 d).

// ---------------------------------------------------------------- helpers

func objValue(o jsonx.OrderedObject) jsonx.OrderedValue {
	return jsonx.OrderedValue{Kind: jsonx.KindObject, Object: o}
}

func arrValue(a []jsonx.OrderedValue) jsonx.OrderedValue {
	if a == nil {
		a = []jsonx.OrderedValue{}
	}
	return jsonx.OrderedValue{Kind: jsonx.KindArray, Array: a}
}

func boolValue(b bool) jsonx.OrderedValue {
	s := "false"
	if b {
		s = "true"
	}
	return jsonx.OrderedValue{Kind: jsonx.KindBool, Scalar: json.RawMessage(s)}
}

func intValue(v int64) jsonx.OrderedValue {
	return jsonx.OrderedValue{Kind: jsonx.KindNumber, Scalar: json.RawMessage(fmt.Sprint(v))}
}

func floatValue(v float64) jsonx.OrderedValue {
	b, err := json.Marshal(v)
	if err != nil { // NaN/Inf; a cost that cannot be encoded is recorded as 0
		b = []byte("0")
	}
	return jsonx.OrderedValue{Kind: jsonx.KindNumber, Scalar: b}
}

// setString adds key only when s is non-empty. Absent and "" are the same
// thing for every string on a message, so emitting empties would bloat every
// line without carrying information.
func setString(o *jsonx.OrderedObject, key, s string) {
	if s != "" {
		o.Set(key, jsonx.OVString(s))
	}
}

func setBool(o *jsonx.OrderedObject, key string, b bool) {
	if b {
		o.Set(key, boolValue(true))
	}
}

func setTime(o *jsonx.OrderedObject, key string, t time.Time) {
	if !t.IsZero() {
		o.Set(key, jsonx.OVString(t.Format(time.RFC3339Nano)))
	}
}

func getString(o jsonx.OrderedObject, key string) string {
	v, ok := o.Get(key)
	if !ok || v.Kind != jsonx.KindString {
		return ""
	}
	s, _ := v.Any().(string)
	return s
}

func getBool(o jsonx.OrderedObject, key string) bool {
	v, ok := o.Get(key)
	if !ok || v.Kind != jsonx.KindBool {
		return false
	}
	b, _ := v.Any().(bool)
	return b
}

func getInt(o jsonx.OrderedObject, key string) (int64, bool) {
	v, ok := o.Get(key)
	if !ok || v.Kind != jsonx.KindNumber {
		return 0, false
	}
	n, err := json.Number(v.Scalar).Int64()
	if err != nil {
		f, ferr := json.Number(v.Scalar).Float64()
		if ferr != nil {
			return 0, false
		}
		return int64(f), true
	}
	return n, true
}

func getFloat(o jsonx.OrderedObject, key string) (float64, bool) {
	v, ok := o.Get(key)
	if !ok || v.Kind != jsonx.KindNumber {
		return 0, false
	}
	f, err := json.Number(v.Scalar).Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}

func getTime(o jsonx.OrderedObject, key string) time.Time {
	s := getString(o, key)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func getObject(o jsonx.OrderedObject, key string) (jsonx.OrderedObject, bool) {
	v, ok := o.Get(key)
	if !ok || v.Kind != jsonx.KindObject {
		return nil, false
	}
	return v.Object, true
}

func getArray(o jsonx.OrderedObject, key string) ([]jsonx.OrderedValue, bool) {
	v, ok := o.Get(key)
	if !ok || v.Kind != jsonx.KindArray {
		return nil, false
	}
	return v.Array, true
}

// rest collects every key not in known, in source order, so a message written
// by a newer build survives a decode/encode cycle by an older one
// (REQ-SESS-05.2 applied below the entry level).
//
// Position is NOT preserved: unknown keys are re-emitted after the modelled
// ones. Byte-exactness is delivered one level up by Entry.Raw passthrough
// (P-57), which is the only mechanism that can preserve interleaving.
func rest(o jsonx.OrderedObject, known ...string) jsonx.OrderedObject {
	set := make(map[string]struct{}, len(known))
	for _, k := range known {
		set[k] = struct{}{}
	}
	var out jsonx.OrderedObject
	for _, m := range o {
		if _, ok := set[m.Key]; ok {
			continue
		}
		out = append(out, jsonx.Member{Key: m.Key, Value: m.Value.Clone()})
	}
	return out
}

func appendRest(o *jsonx.OrderedObject, unknown jsonx.OrderedObject) {
	for _, m := range unknown {
		if o.Index(m.Key) >= 0 {
			continue // a modelled key always wins over a stale unknown copy
		}
		o.Set(m.Key, m.Value.Clone())
	}
}

// ---------------------------------------------------------------- usage

var usageFields = []struct {
	key   string
	field core.UsageField
	get   func(core.Usage) int64
}{
	{"input_tokens", core.UsageInputTokens, func(u core.Usage) int64 { return u.InputTokens }},
	{"output_tokens", core.UsageOutputTokens, func(u core.Usage) int64 { return u.OutputTokens }},
	{"reasoning_tokens", core.UsageReasoningTokens, func(u core.Usage) int64 { return u.ReasoningTokens }},
	{"cache_read_tokens", core.UsageCacheReadTokens, func(u core.Usage) int64 { return u.CacheReadTokens }},
	{"cache_write_tokens", core.UsageCacheWriteTokens, func(u core.Usage) int64 { return u.CacheWriteTokens }},
	{"cache_write_1h_tokens", core.UsageCacheWrite1hTokens, func(u core.Usage) int64 { return u.CacheWrite1hTokens }},
	{"total_tokens", core.UsageTotalTokens, func(u core.Usage) int64 { return u.TotalTokens }},
}

// encodeUsage emits only the fields the provider actually reported.
// Presence is the whole point (REQ-PROV-16.4): an explicit 0 and an absent
// field mean different things to REQ-GO-15's anchor rule, and a codec that
// emits every field unconditionally destroys the distinction on the way to
// disk. That is why this is a hand-rolled projection over Usage.Set and not
// json.Marshal with omitzero.
func encodeUsage(u core.Usage) jsonx.OrderedValue {
	var o jsonx.OrderedObject
	for _, f := range usageFields {
		if u.Has(f.field) {
			o.Set(f.key, intValue(f.get(u)))
		}
	}
	if u.Has(core.UsageCostUSD) {
		o.Set("cost_usd", floatValue(u.CostUSD))
	}
	setString(&o, "billed_model", u.BilledModel)
	return objValue(o)
}

func decodeUsage(o jsonx.OrderedObject) core.Usage {
	var u core.Usage
	for _, f := range usageFields {
		if n, ok := getInt(o, f.key); ok {
			u.SetField(f.field, n)
		}
	}
	if c, ok := getFloat(o, "cost_usd"); ok {
		u.SetCost(c)
	}
	u.BilledModel = getString(o, "billed_model")
	return u
}

// ---------------------------------------------------------------- content

func encodeBlock(b core.ContentBlock) jsonx.OrderedValue {
	var o jsonx.OrderedObject
	switch v := b.(type) {
	case core.TextBlock:
		o.Set("type", jsonx.OVString(string(core.BlockText)))
		o.Set("text", jsonx.OVString(v.Text))
	case core.ThinkingBlock:
		o.Set("type", jsonx.OVString(string(core.BlockThinking)))
		o.Set("thinking", jsonx.OVString(v.Thinking))
		// signature and redacted are opaque provenance REQ-PROV-11 rule 4
		// keys on; they are emitted whenever set and never inspected here.
		setString(&o, "signature", v.Signature)
		setBool(&o, "redacted", v.Redacted)
	case core.ToolUseBlock:
		o.Set("type", jsonx.OVString(string(core.BlockToolUse)))
		o.Set("id", jsonx.OVString(v.ID))
		o.Set("name", jsonx.OVString(v.Name))
		o.Set("input", encodeToolInput(v))
		setString(&o, "thought_signature", v.ThoughtSignature)
	case core.ToolResultBlock:
		o.Set("type", jsonx.OVString(string(core.BlockToolResult)))
		o.Set("tool_use_id", jsonx.OVString(v.ToolUseID))
		o.Set("content", encodeContent(v.Content))
		setBool(&o, "is_error", v.IsError)
	case core.ImageBlock:
		o.Set("type", jsonx.OVString(string(core.BlockImage)))
		o.Set("data", jsonx.OVString(v.Data))
		o.Set("mime_type", jsonx.OVString(v.MimeType))
	case core.RawBlock:
		// A block type this build does not model, re-emitted verbatim.
		return jsonx.OVRaw(v.Raw)
	default:
		o.Set("type", jsonx.OVString(string(b.BlockType())))
	}
	return objValue(o)
}

// encodeToolInput prefers the model's own argument bytes over the ordered
// form, per the P-13 precedence rule: Input wins when present, InputOrder is
// only the regeneration path. jsonx round-trips the bytes with key order and
// numeric literals intact, so nothing is laundered either way.
func encodeToolInput(v core.ToolUseBlock) jsonx.OrderedValue {
	if len(v.Input) > 0 {
		return jsonx.OVRaw(v.Input)
	}
	if v.InputOrder != nil {
		return objValue(v.InputOrder.Clone())
	}
	return objValue(jsonx.OrderedObject{})
}

func encodeContent(c core.Content) jsonx.OrderedValue {
	out := make([]jsonx.OrderedValue, 0, len(c))
	for _, b := range c {
		out = append(out, encodeBlock(b))
	}
	return arrValue(out)
}

func decodeBlock(v jsonx.OrderedValue) core.ContentBlock {
	if v.Kind != jsonx.KindObject {
		return rawBlockOf("", v)
	}
	o := v.Object
	switch core.BlockType(getString(o, "type")) {
	case core.BlockText:
		return core.TextBlock{Text: getString(o, "text")}
	case core.BlockThinking:
		return core.ThinkingBlock{
			Thinking:  getString(o, "thinking"),
			Signature: getString(o, "signature"),
			Redacted:  getBool(o, "redacted"),
		}
	case core.BlockToolUse:
		b := core.ToolUseBlock{
			ID:               getString(o, "id"),
			Name:             getString(o, "name"),
			ThoughtSignature: getString(o, "thought_signature"),
		}
		in, ok := o.Get("input")
		if !ok || in.Kind != jsonx.KindObject {
			b.Input = json.RawMessage("{}")
			b.InputOrder = jsonx.OrderedObject{}
			return b
		}
		raw, err := in.MarshalJSON()
		if err != nil {
			raw = []byte("{}")
		}
		// Input and InputOrder come from ONE decode pass, as core.NewToolUse
		// requires: InputOrder is the object jsonx already built, and Input is
		// its byte rendering. Re-parsing would be a second pass and a second
		// chance to disagree.
		b.Input = raw
		b.InputOrder = in.Object.Clone()
		return b
	case core.BlockToolResult:
		b := core.ToolResultBlock{
			ToolUseID: getString(o, "tool_use_id"),
			IsError:   getBool(o, "is_error"),
		}
		if arr, ok := getArray(o, "content"); ok {
			b.Content = decodeContentArray(arr)
		}
		return b
	case core.BlockImage:
		return core.ImageBlock{Data: getString(o, "data"), MimeType: getString(o, "mime_type")}
	}
	return rawBlockOf(getString(o, "type"), v)
}

func rawBlockOf(typ string, v jsonx.OrderedValue) core.RawBlock {
	raw, err := v.MarshalJSON()
	if err != nil {
		raw = []byte("null")
	}
	return core.RawBlock{Type: typ, Raw: raw}
}

func decodeContentArray(arr []jsonx.OrderedValue) core.Content {
	out := make(core.Content, 0, len(arr))
	for _, v := range arr {
		out = append(out, decodeBlock(v))
	}
	return out
}

// ---------------------------------------------------------------- messages

const (
	msgKeyRole = "role"
	msgKeyTime = "timestamp"
)

var (
	userKnown      = []string{msgKeyRole, "content", msgKeyTime}
	assistantKnown = []string{
		msgKeyRole, "content", "stop_reason", "raw_stop_reason", "error_message",
		"usage", "provider", "api", "model", "response_model", "response_id",
		"thinking_level", msgKeyTime,
	}
	toolResultKnown = []string{
		msgKeyRole, "tool_use_id", "tool_name", "content", "is_error",
		"added_tool_names", "usage", msgKeyTime,
	}
)

// encodeMessage renders one canonical message. The role discriminates; there
// is no shared superset shape, because REQ-LOOP-02 makes tool_result a
// first-class role whose fields exist on no other message.
func encodeMessage(m core.Message) jsonx.OrderedValue {
	var o jsonx.OrderedObject
	switch v := m.(type) {
	case core.UserMessage:
		o.Set(msgKeyRole, jsonx.OVString(string(core.RoleUser)))
		o.Set("content", encodeContent(v.Content))
		setTime(&o, msgKeyTime, v.Timestamp)
		appendRest(&o, v.Unknown)
	case core.AssistantMessage:
		o.Set(msgKeyRole, jsonx.OVString(string(core.RoleAssistant)))
		o.Set("content", encodeContent(v.Content))
		setString(&o, "stop_reason", string(v.StopReason))
		setString(&o, "raw_stop_reason", v.RawStopReason)
		setString(&o, "error_message", v.ErrorMessage)
		if v.Usage.Reported() {
			o.Set("usage", encodeUsage(v.Usage))
		}
		// Provenance is not optional: REQ-PROV-11 rule 1 computes same_model
		// over exactly (provider, api, model), and a log that drops api is
		// P-4 — every replayed ThinkingBlock silently downgraded to text on
		// the first post-resume request.
		setString(&o, "provider", v.Provider)
		setString(&o, "api", string(v.API))
		setString(&o, "model", v.Model)
		setString(&o, "response_model", v.ResponseModel)
		setString(&o, "response_id", v.ResponseID)
		setString(&o, "thinking_level", string(v.ThinkingLevel))
		setTime(&o, msgKeyTime, v.Timestamp)
		appendRest(&o, v.Unknown)
	case core.ToolResultMessage:
		o.Set(msgKeyRole, jsonx.OVString(string(core.RoleToolResult)))
		o.Set("tool_use_id", jsonx.OVString(v.ToolUseID))
		setString(&o, "tool_name", v.ToolName)
		o.Set("content", encodeContent(v.Content))
		setBool(&o, "is_error", v.IsError)
		if len(v.AddedToolNames) > 0 {
			names := make([]jsonx.OrderedValue, len(v.AddedToolNames))
			for i, n := range v.AddedToolNames {
				names[i] = jsonx.OVString(n)
			}
			o.Set("added_tool_names", arrValue(names))
		}
		if v.Usage != nil {
			o.Set("usage", encodeUsage(*v.Usage))
		}
		setTime(&o, msgKeyTime, v.Timestamp)
		appendRest(&o, v.Unknown)
	default:
		o.Set(msgKeyRole, jsonx.OVString(string(m.Role())))
	}
	return objValue(o)
}

var errNotAMessage = fmt.Errorf("session: entry payload is not a message object")

func decodeMessage(v jsonx.OrderedValue) (core.Message, error) {
	if v.Kind != jsonx.KindObject {
		return nil, errNotAMessage
	}
	o := v.Object
	var content core.Content
	if arr, ok := getArray(o, "content"); ok {
		content = decodeContentArray(arr)
	}
	switch core.Role(getString(o, msgKeyRole)) {
	case core.RoleUser:
		return core.UserMessage{
			Content:   content,
			Timestamp: getTime(o, msgKeyTime),
			Unknown:   rest(o, userKnown...),
		}, nil
	case core.RoleAssistant:
		m := core.AssistantMessage{
			Content:       content,
			StopReason:    core.StopReason(getString(o, "stop_reason")),
			RawStopReason: getString(o, "raw_stop_reason"),
			ErrorMessage:  getString(o, "error_message"),
			Provider:      getString(o, "provider"),
			API:           core.API(getString(o, "api")),
			Model:         getString(o, "model"),
			ResponseModel: getString(o, "response_model"),
			ResponseID:    getString(o, "response_id"),
			ThinkingLevel: core.ThinkingLevel(getString(o, "thinking_level")),
			Timestamp:     getTime(o, msgKeyTime),
			Unknown:       rest(o, assistantKnown...),
		}
		if u, ok := getObject(o, "usage"); ok {
			m.Usage = decodeUsage(u)
		}
		return m, nil
	case core.RoleToolResult:
		m := core.ToolResultMessage{
			ToolUseID: getString(o, "tool_use_id"),
			ToolName:  getString(o, "tool_name"),
			Content:   content,
			IsError:   getBool(o, "is_error"),
			Timestamp: getTime(o, msgKeyTime),
			Unknown:   rest(o, toolResultKnown...),
		}
		if arr, ok := getArray(o, "added_tool_names"); ok {
			for _, n := range arr {
				if s, ok := n.Any().(string); ok {
					m.AddedToolNames = append(m.AddedToolNames, s)
				}
			}
		}
		if u, ok := getObject(o, "usage"); ok {
			usage := decodeUsage(u)
			m.Usage = &usage
		}
		return m, nil
	}
	return nil, errNotAMessage
}
