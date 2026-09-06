package openairesponses

import (
	"encoding/json"
	"fmt"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/wire"
)

// wholeResponse is the non-streaming body.
type wholeResponse struct {
	ID          string     `json:"id"`
	Model       string     `json:"model"`
	Status      string     `json:"status"`
	ServiceTier string     `json:"service_tier"`
	Output      []wireItem `json:"output"`
	Usage       *wireUsage `json:"usage"`
	Incomplete  *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *wireError `json:"error"`
}

// DecodeResponse decodes a non-streaming Responses body.
//
// It exists so the cross-provider conformance suite can drive this wire's
// whole-response path alongside the other four, and so REQ-PROV-01's
// derivation has something to compare against: the streaming path is the
// primitive, and a second parser that disagrees with it is exactly what the
// conformance suite is for.
func DecodeResponse(m *core.Model, body []byte, lookup func(string) *core.Model) (*core.AssistantMessage, error) {
	if err := wire.Guard(body, wire.Limits{}); err != nil {
		return nil, fmt.Errorf("openai-responses: %w", err)
	}
	var w wholeResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai-responses: decoding response: %w", err)
	}

	out := &core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
		ResponseID: w.ID, ResponseModel: w.Model,
	}
	for _, it := range w.Output {
		switch it.Type {
		case "message":
			var text string
			for _, c := range it.Content {
				text += c.Text
			}
			if text != "" {
				out.Content = append(out.Content, core.TextBlock{Text: text})
			}
		case "reasoning":
			var summary string
			for _, sp := range it.Summary {
				summary += sp.Text
			}
			sig := EncodeThinkingSignature(it.ID, it.EncryptedContent)
			if summary == "" && sig == "" {
				continue
			}
			out.Content = append(out.Content, core.ThinkingBlock{Thinking: summary, Signature: sig})
		case "function_call":
			args, _ := provider.SalvageJSON([]byte(it.Arguments))
			blk, err := core.NewToolUse(JoinID(it.CallID, it.ID), it.Name, args)
			if err != nil {
				continue
			}
			out.Content = append(out.Content, blk)
		}
	}

	status := w.Status
	if w.Incomplete != nil && w.Incomplete.Reason != "" {
		status = w.Incomplete.Reason
	}
	hasTools := len(core.ExtractToolUse(out)) > 0
	out.StopReason = MapStatus(status, hasTools)
	out.RawStopReason = status
	if w.Error != nil && w.Error.Message != "" {
		out.StopReason = core.StopReasonError
		out.ErrorMessage = w.Error.Message
	}

	if w.Usage != nil {
		w.Usage.Into(&out.Usage)
	}
	billModel, billed := provider.BillingModel(m, w.Model, lookup)
	out.Usage.BilledModel = billed
	if out.Usage.Reported() {
		out.Usage.SetCost(provider.ComputeCost(billModel, out.Usage))
	}
	return out, nil
}
