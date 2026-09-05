package difftest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// Scenario is one shared test case. Scenarios are shared ACROSS providers
// (NFR-TEST-06.1): a provider with no scenarios is untested regardless of its
// unit-test coverage, and the only way that statement means anything is if the
// scenario file knows nothing about which provider will render it.
type Scenario struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	System      string            `json:"system"`
	Messages    []ScenarioMessage `json:"messages"`
	Tools       []ScenarioTool    `json:"tools"`
	Config      ScenarioConfig    `json:"config"`
	// OrderSensitivePaths declares where object insertion order is observable
	// to the model or the provider (NFR-TEST-06.5).
	OrderSensitivePaths []string `json:"order_sensitive_paths"`

	dir string
}

type ScenarioConfig struct {
	Model         string   `json:"model"`
	MaxTokens     *int     `json:"max_tokens"`
	Temperature   *float64 `json:"temperature"`
	TopP          *float64 `json:"top_p"`
	ThinkingLevel string   `json:"thinking_level"`
	ToolChoice    string   `json:"tool_choice"`
	SessionID     string   `json:"session_id"`
	Retention     string   `json:"cache_retention"`
}

type ScenarioMessage struct {
	Role      string          `json:"role"` // user | assistant | tool_result
	Content   []ScenarioBlock `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
	ToolName  string          `json:"tool_name"`
	IsError   bool            `json:"is_error"`
	Provider  string          `json:"provider"`
	API       string          `json:"api"`
	Model     string          `json:"model"`
	Stop      string          `json:"stop_reason"`
	Added     []string        `json:"added_tool_names"`
	_         struct{}        `json:"-"`
}

type ScenarioBlock struct {
	Type      string          `json:"type"` // text | thinking | tool_use | image
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Redacted  bool            `json:"redacted"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	MimeType  string          `json:"mime_type"`
	Data      string          `json:"data"`
}

type ScenarioTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      ScenarioSchema `json:"schema"`
}

// ScenarioSchema is a small declarative subset, converted to a *schema.Schema.
//
// It is not raw JSON Schema on purpose. The harness must build the SAME
// schema.Schema value the shipped code would, so that each provider's own
// dialect translation is what is under test; handing a provider pre-rendered
// JSON Schema would skip the translation and compare it against itself.
type ScenarioSchema struct {
	Type       string                    `json:"type"`
	Desc       string                    `json:"description"`
	Properties map[string]ScenarioSchema `json:"properties"`
	Required   []string                  `json:"required"`
	Items      *ScenarioSchema           `json:"items"`
	Enum       []string                  `json:"enum"`
	Order      []string                  `json:"order"`
}

// LoadScenario decodes one scenario file with DisallowUnknownFields.
//
// NFR-TEST-06.6: an unmapped option is a HARD ERROR. Without it both arms
// ignore the unknown key and agree for the wrong reason — the harness reports
// PASS on a scenario neither side actually ran.
func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("difftest: %s: %w", path, err)
	}
	if s.Name == "" {
		s.Name = filepath.Base(filepath.Dir(path))
	}
	s.dir = filepath.Dir(path)
	return &s, nil
}

// LoadScenarios reads every <dir>/*/scenario.json.
func LoadScenarios(dir string) ([]*Scenario, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Scenario
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), "scenario.json")
		if _, err := os.Stat(p); err != nil {
			continue
		}
		s, err := LoadScenario(p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Reference returns the independently produced body for one wire API.
//
// NFR-TEST-06.3: the reference is produced by the vendor's own SDK at a pinned
// version or by recorded live traffic, and is NEVER hand-authored — a
// hand-authored expectation encodes the same mental model as the code under
// test, so the two agree precisely where both are wrong.
func (s *Scenario) Reference(api string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, "reference", api+".json"))
}

// Request builds the canonical request the scenario describes.
func (s *Scenario) Request() (core.Request, *core.Model, error) {
	m, err := catalog.ResolveModel(s.Config.Model)
	if err != nil {
		return core.Request{}, nil, fmt.Errorf("difftest: scenario %q: %w", s.Name, err)
	}

	req := core.Request{
		MaxTokens:     s.Config.MaxTokens,
		Temperature:   s.Config.Temperature,
		TopP:          s.Config.TopP,
		ThinkingLevel: core.ThinkingLevel(s.Config.ThinkingLevel),
		ToolChoice:    core.ToolChoice(s.Config.ToolChoice),
		Options:       core.RequestOptions{SessionID: s.Config.SessionID},
	}
	if s.System != "" {
		req.System = []core.ContentBlock{core.TextBlock{Text: s.System}}
	}
	for _, t := range s.Tools {
		req.Tools = append(req.Tools, core.ToolWire{
			Name: t.Name, Description: t.Description, InputSchema: buildSchema(t.Schema)})
	}
	for i, sm := range s.Messages {
		msg, err := sm.message()
		if err != nil {
			return core.Request{}, nil, fmt.Errorf("difftest: scenario %q message %d: %w", s.Name, i, err)
		}
		req.Messages = append(req.Messages, msg)
	}
	return req, m, nil
}

func (sm ScenarioMessage) message() (core.Message, error) {
	content, err := blocks(sm.Content)
	if err != nil {
		return nil, err
	}
	switch sm.Role {
	case "user":
		return core.UserMessage{Content: content}, nil
	case "assistant":
		return core.AssistantMessage{
			Content: content, Provider: sm.Provider, API: core.API(sm.API), Model: sm.Model,
			StopReason: core.StopReason(sm.Stop),
		}, nil
	case "tool_result":
		return core.ToolResultMessage{
			ToolUseID: sm.ToolUseID, ToolName: sm.ToolName, Content: content,
			IsError: sm.IsError, AddedToolNames: sm.Added,
		}, nil
	}
	return nil, fmt.Errorf("unknown role %q", sm.Role)
}

func blocks(bs []ScenarioBlock) (core.Content, error) {
	var out core.Content
	for _, b := range bs {
		switch b.Type {
		case "text":
			out = append(out, core.TextBlock{Text: b.Text})
		case "thinking":
			out = append(out, core.ThinkingBlock{
				Thinking: b.Thinking, Signature: b.Signature, Redacted: b.Redacted})
		case "tool_use":
			tu, err := core.NewToolUse(b.ID, b.Name, b.Input)
			if err != nil {
				return nil, err
			}
			out = append(out, tu)
		case "image":
			out = append(out, core.ImageBlock{MimeType: b.MimeType, Data: b.Data})
		default:
			return nil, fmt.Errorf("unknown block type %q", b.Type)
		}
	}
	return out, nil
}

func buildSchema(s ScenarioSchema) *schema.Schema {
	switch s.Type {
	case "object", "":
		names := s.Order
		if len(names) == 0 {
			for k := range s.Properties {
				names = append(names, k)
			}
		}
		var fields []schema.Field
		for _, name := range names {
			sub, ok := s.Properties[name]
			if !ok {
				continue
			}
			f := schema.Opt(name, buildSchema(sub))
			for _, r := range s.Required {
				if r == name {
					f = schema.Prop(name, buildSchema(sub))
				}
			}
			fields = append(fields, f)
		}
		return schema.Object(fields...).Describe(s.Desc)
	case "string":
		if len(s.Enum) > 0 {
			return schema.Enum(s.Desc, s.Enum...)
		}
		return schema.String(s.Desc)
	case "integer":
		return schema.Int(s.Desc)
	case "number":
		return schema.Number(s.Desc)
	case "boolean":
		return schema.Bool(s.Desc)
	case "array":
		var items *schema.Schema
		if s.Items != nil {
			items = buildSchema(*s.Items)
		} else {
			items = schema.String()
		}
		return schema.Array(items, s.Desc)
	}
	return schema.String(s.Desc)
}

// errCaptured aborts the provider before the first byte.
var errCaptured = errors.New("difftest: payload captured")

// Capture is NFR-TEST-06.2's capture arm: the payload is stored by OnPayload,
// which then returns an error so nothing is sent. No API key, no network.
//
// It drives the real Stream rather than calling BuildRequest, so what is
// captured is what the shipped dispatch path produces — including anything
// applied between building and sending.
func Capture(ctx context.Context, p core.APIProvider, m *core.Model, req core.Request) ([]byte, error) {
	var captured []byte
	req.Options.OnPayload = func(payload any, _ *core.Model) (any, error) {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		captured = b
		return nil, errCaptured
	}
	s := p.Stream(ctx, m, req, core.ProviderStreamOptions{})
	_ = s.Result()
	if captured == nil {
		return nil, fmt.Errorf("difftest: provider %q produced no payload; err=%v", p.API, s.Err())
	}
	return captured, nil
}
