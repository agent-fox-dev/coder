package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/session"
)

// deferring is a provider that accepts a background submission: it answers
// the first call with a RECEIPT and no content, and redeems that receipt on
// FetchDeferred. It is the executable form of REQ-PROV-19's contract.
type deferring struct {
	handle    core.DeferredHandle
	redeemed  int
	sawWindow time.Duration
}

func (d *deferring) provider() core.APIProvider {
	return core.APIProvider{
		API: testAPI,
		Stream: func(ctx context.Context, m *core.Model, req core.Request, _ core.ProviderStreamOptions) *core.EventStream {
			st := core.NewEventStream(core.StreamOptions{})
			msg := core.AssistantMessage{
				StopReason: core.StopReasonStop,
				Content:    core.Content{core.TextBlock{Text: "immediate"}},
				Provider:   m.Provider, API: m.API, Model: m.ID,
			}
			if req.Deferred != nil {
				d.sawWindow = req.Deferred.Window
				h := d.handle
				msg = core.AssistantMessage{
					StopReason: core.StopReasonDeferred,
					Deferred:   &h,
					Provider:   m.Provider, API: m.API, Model: m.ID,
				}
			}
			st.Push(core.MessageEndEvent{Message: msg})
			st.End(core.StreamResult{Message: &msg})
			return st
		},
		FetchDeferred: func(ctx context.Context, m *core.Model, h core.DeferredHandle, _ core.ProviderStreamOptions) *core.EventStream {
			d.redeemed++
			st := core.NewEventStream(core.StreamOptions{})
			msg := core.AssistantMessage{
				StopReason: core.StopReasonStop,
				Content:    core.Content{core.TextBlock{Text: "the deferred answer for " + h.ID}},
				Provider:   m.Provider, API: m.API, Model: m.ID,
			}
			st.Push(core.MessageEndEvent{Message: msg})
			st.End(core.StreamResult{Message: &msg})
			return st
		},
	}
}

func testHandle() core.DeferredHandle {
	return core.DeferredHandle{
		Provider: "test", API: testAPI, ModelID: "test-model", ID: "job-1",
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Data:      json.RawMessage(`{"queue":"batch"}`),
	}
}

// TestTheCapabilityIsProbedNotAssumed is REQ-PROV-19's first clause: a caller
// asks whether the wire supports deferral before submitting, because a
// provider that ignores the field answers immediately.
func TestTheCapabilityIsProbedNotAssumed(t *testing.T) {
	d := &deferring{handle: testHandle()}
	a := newTestAgent(t, nil, func(c *core.AgentConfig) {
		c.Providers = core.ProviderRegistry{testAPI: d.provider()}
	})
	if !a.SupportsDeferred() {
		t.Fatal("a provider registering FetchDeferred must probe as supporting it")
	}

	plain := &scripted{}
	b := newTestAgent(t, plain, nil)
	if b.SupportsDeferred() {
		t.Fatal("a provider with no FetchDeferred must probe as NOT supporting it; " +
			"REQ-PROV-19 says the capability is probed, never assumed")
	}
}

// TestADeferredRunEndsWithItsReceipt: the run ends CLEAN holding a handle,
// not as an error and not as an empty completion (REQ-PROV-19, OQ-11).
func TestADeferredRunEndsWithItsReceipt(t *testing.T) {
	d := &deferring{handle: testHandle()}
	a := newTestAgent(t, nil, func(c *core.AgentConfig) {
		c.Providers = core.ProviderRegistry{testAPI: d.provider()}
		c.RequestOptions.Deferred = &core.DeferredRequest{Window: 24 * time.Hour}
	})

	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("a deferred submission is a clean end, not an error: %v", err)
	}
	if res.StopReason != core.RunStopDeferred {
		t.Fatalf("StopReason = %q, want deferred", res.StopReason)
	}
	if d.sawWindow != 24*time.Hour {
		t.Fatalf("the provider saw window %v; RequestOptions.Deferred must reach Request.Deferred", d.sawWindow)
	}
	h, ok := res.DeferredHandle()
	if !ok || h.ID != "job-1" {
		t.Fatalf("no handle on the result (%v, %v); the receipt is the only way back to the answer", h, ok)
	}
}

// TestADeferredStopWithNoHandleIsStillAnError: the empty-completion failure
// OQ-11 exists to foreclose. A stop reason with nothing to redeem is not a
// clean end.
func TestADeferredStopWithNoHandleIsStillAnError(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{{StopReason: core.StopReasonDeferred}}}
	a := newTestAgent(t, s, nil)
	_, err := a.Run(context.Background(), "go")
	if !errors.Is(err, core.ErrDeferredUnsupported) {
		t.Fatalf("err = %v, want ErrDeferredUnsupported", err)
	}
}

// TestRedeemingAHandleAppendsTheAnswer: one call, one attempt, appended to
// history — and no poller anywhere.
func TestRedeemingAHandleAppendsTheAnswer(t *testing.T) {
	d := &deferring{handle: testHandle()}
	a := newTestAgent(t, nil, func(c *core.AgentConfig) {
		c.Providers = core.ProviderRegistry{testAPI: d.provider()}
		c.RequestOptions.Deferred = &core.DeferredRequest{Window: time.Hour}
	})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := res.DeferredHandle()

	before := a.History().Len()
	msg, err := a.RedeemDeferred(context.Background(), h)
	if err != nil {
		t.Fatalf("RedeemDeferred: %v", err)
	}
	if d.redeemed != 1 {
		t.Fatalf("the provider was called %d times; redemption is one attempt, never a poll loop", d.redeemed)
	}
	if !strings.Contains(msg.Content.Text(), "job-1") {
		t.Fatalf("answer = %q", msg.Content.Text())
	}
	if a.History().Len() != before+1 {
		t.Fatal("the redeemed answer must be appended to history")
	}
	if !a.Idle() {
		t.Fatal("the run slot must be released after redemption")
	}
}

// TestAnExpiredOrForeignHandleIsRefusedBeforeTheWire: two failures that must
// not become network calls — an expired receipt, and one issued for another
// model. Redeeming the latter would collect someone else's answer.
func TestAnExpiredOrForeignHandleIsRefusedBeforeTheWire(t *testing.T) {
	d := &deferring{handle: testHandle()}
	a := newTestAgent(t, nil, func(c *core.AgentConfig) {
		c.Providers = core.ProviderRegistry{testAPI: d.provider()}
	})

	expired := testHandle()
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := a.RedeemDeferred(context.Background(), expired); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want an expiry refusal", err)
	}

	foreign := testHandle()
	foreign.ModelID = "some-other-model"
	if _, err := a.RedeemDeferred(context.Background(), foreign); err == nil {
		t.Fatal("a handle issued for another model must be refused")
	}
	if d.redeemed != 0 {
		t.Fatal("neither refusal may reach the wire")
	}
	// A zero ExpiresAt means the provider stated no expiry, not "expired at
	// the epoch" — the same distinction credential expiry draws.
	never := testHandle()
	never.ExpiresAt = time.Time{}
	if _, err := a.RedeemDeferred(context.Background(), never); err != nil {
		t.Fatalf("a handle with no stated expiry must be redeemable: %v", err)
	}
}

// TestAHandleSurvivesTheProcess is REQ-PROV-19's durability clause: the
// receipt is serialized into the session log like any other entry, so a
// submission outlives the binary that made it.
func TestAHandleSurvivesTheProcess(t *testing.T) {
	h := testHandle()
	msg := core.AssistantMessage{
		StopReason: core.StopReasonDeferred,
		Deferred:   &h,
		Provider:   "test", API: testAPI, Model: "test-model",
	}
	raw, err := session.EncodeMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"job-1"`) || !strings.Contains(string(raw), `"batch"`) {
		t.Fatalf("the handle did not reach the log: %s", raw)
	}

	back, err := session.DecodeMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.(core.AssistantMessage)
	if !ok || got.Deferred == nil {
		t.Fatalf("the handle did not survive the round trip: %#v", back)
	}
	if got.Deferred.ID != h.ID || got.Deferred.ModelID != h.ModelID || got.Deferred.API != h.API {
		t.Fatalf("handle = %+v, want %+v", *got.Deferred, h)
	}
	if !got.Deferred.ExpiresAt.Equal(h.ExpiresAt) {
		t.Fatalf("expiry = %v, want %v", got.Deferred.ExpiresAt, h.ExpiresAt)
	}
	// Provider-opaque bytes are replayed unmodified, the same rule that
	// governs thinking signatures.
	if strings.TrimSpace(string(got.Deferred.Data)) != `{"queue":"batch"}` {
		t.Fatalf("data = %s, want the bytes as issued", got.Deferred.Data)
	}
}

// TestRedemptionRoutesOnTheHandlesOwnAPI: a session whose model changed after
// the submission (REQ-SESS-03) must still redeem against the wire holding the
// answer, not the wire it happens to be on now.
func TestRedemptionRoutesOnTheHandlesOwnAPI(t *testing.T) {
	d := &deferring{handle: testHandle()}
	reg := core.ProviderRegistry{testAPI: d.provider()}
	h := testHandle()
	h.API = core.API("some-other-wire")
	st := reg.FetchDeferred(context.Background(), testModel(), h, core.ProviderStreamOptions{})
	if err := st.Err(); !errors.Is(err, core.ErrDeferredUnsupportedAPI) {
		t.Fatalf("err = %v, want ErrDeferredUnsupportedAPI", err)
	}
	if d.redeemed != 0 {
		t.Fatal("a handle for an unregistered wire must not reach this provider")
	}
}
