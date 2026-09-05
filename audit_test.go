package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

type auditLog struct {
	mu     sync.Mutex
	events []core.AuditEvent
}

func (l *auditLog) add(e core.AuditEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *auditLog) of(kind core.AuditKind) []core.AuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []core.AuditEvent
	for _, e := range l.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// TestEveryToolCallIsAudited is REQ-OBS-05 through the real batch executor.
func TestEveryToolCallIsAudited(t *testing.T) {
	log := &auditLog{}
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "echo", `{"v":"one"}`),
			toolUse(t, "c2", "mcp__github__create_issue", `{"v":"two"}`)),
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.SessionID = "sess-1"
		c.ParallelTools = true
		c.Hooks.OnAudit = log.add
	})
	for _, n := range []string{"echo", "mcp__github__create_issue"} {
		if err := a.RegisterTool(echoTool(n, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	calls := log.of(core.AuditToolCall)
	if len(calls) != 2 {
		t.Fatalf("%d tool-call audit events, want 2", len(calls))
	}
	byID := map[string]core.AuditEvent{}
	for _, e := range calls {
		byID[e.ToolUseID] = e
		if e.SessionID != "sess-1" {
			t.Fatalf("audit event carries session %q, want sess-1", e.SessionID)
		}
		if e.ArgumentsHash == "" {
			t.Fatal("REQ-OBS-05 requires an arguments hash on every tool call")
		}
	}
	if byID["c1"].ServerName != "" {
		t.Fatalf("a LOCAL tool reported server %q; deriving the field from the "+
			"REQ-SEC-08 prefix must leave it empty rather than guess", byID["c1"].ServerName)
	}
	if byID["c2"].ServerName != "github" {
		t.Fatalf("server_name = %q, want github from the mcp__github__ prefix",
			byID["c2"].ServerName)
	}
}

// TestTheAuditTrailHashesArgumentsRatherThanRecordingThem is REQ-OBS-05's word
// "hash", taken literally.
//
// An audit trail is precisely the artifact that gets shipped to a log
// aggregator and retained for years, and tool arguments routinely carry file
// contents, credentials and personal data. A hash gives correlation without
// making the audit log the largest copy of the data it describes.
func TestTheAuditTrailHashesArgumentsRatherThanRecordingThem(t *testing.T) {
	log := &auditLog{}
	const secret = "hunter2-the-actual-password"
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "echo", `{"v":"`+secret+`"}`)),
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.Hooks.OnAudit = log.add })
	if err := a.RegisterTool(echoTool("echo", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	for _, e := range log.of(core.AuditToolCall) {
		blob, _ := json.Marshal(e)
		if strings.Contains(string(blob), secret) {
			t.Fatalf("the audit event carries the argument VALUE: %s", blob)
		}
		if !strings.HasPrefix(e.ArgumentsHash, "sha256:") {
			t.Fatalf("arguments hash = %q, want a labelled digest", e.ArgumentsHash)
		}
	}

	// The same call hashes the same way, so an auditor can correlate; a
	// different one does not.
	same := ArgumentsHash([]byte(`{"a":1}`))
	if same != ArgumentsHash([]byte(`{"a":1}`)) {
		t.Fatal("the hash must be stable, or it correlates nothing")
	}
	if same == ArgumentsHash([]byte(`{"a":2}`)) {
		t.Fatal("different arguments must hash differently")
	}
	if ArgumentsHash(nil) != "" {
		t.Fatal("no arguments means no hash, not the hash of nothing")
	}
}

func TestMCPServerOf(t *testing.T) {
	cases := map[string]string{
		"mcp__github__create_issue": "github",
		"mcp__db__query":            "db",
		"read_file":                 "",
		"mcp__malformed":            "",
		"mcp__":                     "",
		"":                          "",
		// A LOCAL tool whose name happens to contain the separator. Without
		// the prefix check this reports a server called "my", inventing an MCP
		// origin for a tool that has none — and an audit trail that attributes
		// a local call to a remote server is worse than one that omits the
		// field.
		"my__local__tool": "",
		"__leading":       "",
	}
	for in, want := range cases {
		if got := MCPServerOf(in); got != want {
			t.Errorf("MCPServerOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSessionStartAndEndFireOnEveryExit is REQ-OBS-03.
//
// The "every exit" half is the point. A hook that fires only on a clean run is
// worse than none: an auditor cannot tell a session that ended badly from one
// still running, which is the case they most need to see.
func TestSessionStartAndEndFireOnEveryExit(t *testing.T) {
	t.Run("clean run", func(t *testing.T) {
		log := &auditLog{}
		s := &scripted{turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop}}}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			c.Hooks.OnSessionStart = log.add
			c.Hooks.OnSessionEnd = log.add
		})
		if _, err := a.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		assertOneEach(t, log)
		if end := log.of(core.AuditSessionEnd)[0]; end.StopReason != core.RunStopEndTurn {
			t.Fatalf("stop reason = %q, want end_turn", end.StopReason)
		}
	})

	t.Run("turn limit with ErrorOnLimit", func(t *testing.T) {
		log := &auditLog{}
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "echo", `{"v":"x"}`)),
			assistantWithTools(core.StopReasonToolUse, toolUse(t, "c2", "echo", `{"v":"x"}`)),
		}}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			c.StopPolicy = StopAfterTurns(1)
			c.ErrorOnLimit = true
			c.Hooks.OnSessionStart = log.add
			c.Hooks.OnSessionEnd = log.add
		})
		if err := a.RegisterTool(echoTool("echo", nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Run(context.Background(), "go"); err == nil {
			t.Fatal("want ErrMaxTurns")
		}
		assertOneEach(t, log)
		end := log.of(core.AuditSessionEnd)[0]
		if end.Error == "" {
			t.Fatal("a session that ended in an error must say so in the audit record")
		}
	})

	t.Run("aborted run", func(t *testing.T) {
		log := &auditLog{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &scripted{}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			c.Hooks.OnSessionStart = log.add
			c.Hooks.OnSessionEnd = log.add
		})
		a.Run(ctx, "go")
		assertOneEach(t, log)
	})
}

func assertOneEach(t *testing.T, log *auditLog) {
	t.Helper()
	if n := len(log.of(core.AuditSessionStart)); n != 1 {
		t.Fatalf("%d session_start events, want exactly 1", n)
	}
	if n := len(log.of(core.AuditSessionEnd)); n != 1 {
		t.Fatalf("%d session_end events, want exactly 1", n)
	}
}

// TestLoadedSkillsAreAudited is REQ-OBS-04.
func TestLoadedSkillsAreAudited(t *testing.T) {
	log := &auditLog{}
	s := &scripted{}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.Hooks.OnAudit = log.add })
	a.AuditSkills([]string{"code-review", "release-notes"})

	got := log.of(core.AuditSkillsLoaded)
	if len(got) != 1 {
		t.Fatalf("%d skills events, want 1", len(got))
	}
	if strings.Join(got[0].Skills, ",") != "code-review,release-notes" {
		t.Fatalf("skills = %v, want both names", got[0].Skills)
	}
}

// TestToolSpansCarryTheRequiredAttributes is REQ-OBS-02.
func TestToolSpansCarryTheRequiredAttributes(t *testing.T) {
	rec := &spanRecorder{}
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "ok_tool", `{"v":"x"}`),
			toolUse(t, "c2", "bad_tool", `{"v":"y"}`)),
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.Tracer = rec })
	if err := a.RegisterTool(echoTool("ok_tool", nil)); err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterTool(core.Tool{
		Name: "bad_tool", Description: "fails", InputSchema: emptySchema(),
		Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("nope")
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	byTool := map[string]map[string]any{}
	for _, sp := range rec.spans {
		if n, ok := sp["tool_name"].(string); ok {
			byTool[n] = sp
		}
	}
	if len(byTool) != 2 {
		t.Fatalf("%d tool spans, want one per call: %v", len(byTool), rec.spans)
	}
	for name, attrs := range byTool {
		for _, k := range []string{"tool_name", "tool_use_id", "is_error", "elapsed_ms"} {
			if _, ok := attrs[k]; !ok {
				t.Fatalf("span for %s is missing %q (REQ-OBS-02)", name, k)
			}
		}
	}
	if byTool["ok_tool"]["is_error"] != false {
		t.Fatalf("ok_tool span: is_error = %v, want false", byTool["ok_tool"]["is_error"])
	}
	if byTool["bad_tool"]["is_error"] != true {
		t.Fatalf("bad_tool span: is_error = %v, want true", byTool["bad_tool"]["is_error"])
	}
}

// TestAPanickingAuditHookDoesNotTakeTheRunWithIt: hooks are Axis 2 —
// observation, never interception (REQ-OBS-07) — so a hook must not be able to
// change the outcome, and a panic is the loudest way to change one.
func TestAPanickingAuditHookDoesNotTakeTheRunWithIt(t *testing.T) {
	var reported int
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop}}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Hooks.OnAudit = func(core.AuditEvent) { panic("a badly written sink") }
		c.Hooks.OnError = func(error) { reported++ }
	})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("a panicking observer must not fail the run: %v", err)
	}
	if res.FinalText() != "done" {
		t.Fatalf("final text = %q, want the model's answer", res.FinalText())
	}
	if reported == 0 {
		t.Fatal("the panic must be surfaced through OnError, not swallowed")
	}
}
