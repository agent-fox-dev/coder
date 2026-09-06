package core

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// AuditKind names one audited operation.
type AuditKind string

const (
	AuditSessionStart AuditKind = "session_start"
	AuditSessionEnd   AuditKind = "session_end"
	AuditToolCall     AuditKind = "tool_call"
	AuditSkillsLoaded AuditKind = "skills_loaded"
)

// AuditEvent is the record REQ-OBS-04 and REQ-OBS-05 require.
//
// It is a distinct type from the REQ-OBS-06 event taxonomy on purpose. Those
// events describe what a UI should render and are emitted at streaming
// cadence; this is the durable record of what the agent DID, at one event per
// operation, and the two have different consumers, retention and volume.
type AuditEvent struct {
	Kind      AuditKind
	Timestamp time.Time
	SessionID string

	// Tool call (REQ-OBS-05).
	ToolName  string
	ToolUseID string
	// ServerName is the MCP server a qualified tool came from, empty for a
	// local tool. It is derived from the REQ-SEC-08 name prefix rather than
	// plumbed separately, so it is right for any tool that follows the
	// convention and empty rather than wrong for one that does not.
	ServerName string
	// ArgumentsHash is a SHA-256 of the argument bytes.
	//
	// REQ-OBS-05 says HASH, not arguments, and the distinction is the whole
	// design. Tool arguments routinely carry file contents, credentials and
	// personal data, and an audit trail is precisely the artifact that gets
	// shipped to a log aggregator and retained for years. A hash gives
	// correlation — the same call twice, the same call across sessions —
	// without turning the audit log into the largest copy of the data it
	// describes.
	ArgumentsHash string
	IsError       bool
	ElapsedMS     int64

	// Skills loaded (REQ-OBS-04).
	Skills []string

	// Session end.
	Usage      Usage
	StopReason RunStopReason
	// Error is the run's terminal error, if any. Its text is the SDK's own;
	// no credential reaches it (NFR-SEC-01, REQ-AUTH-07).
	Error string
}

// HashArguments is REQ-OBS-05's "arguments hash".
//
// A HASH, never the arguments. Tool arguments routinely carry file contents,
// credentials and personal data, and an audit trail is precisely the artifact
// that gets shipped to a log aggregator and retained for years. The hash gives
// correlation — the same call twice, the same call across sessions — without
// making the audit log the largest copy of the data it describes.
//
// It is taken over the RAW argument bytes, so two calls that differ only in
// the key order the model authored hash differently. That is deliberate and
// matches REQ-CACHE-01: on wires that carry arguments as a JSON string they
// are genuinely different calls.
//
// It lives in core because both the batch executor and the MCP client need it,
// and neither should import the other.
func HashArguments(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
