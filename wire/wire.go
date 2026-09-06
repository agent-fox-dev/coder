// Package wire is REQ-SEC-11 and REQ-SEC-12: the bounded, strict decoder for
// bytes AgentKit did not produce.
//
// Every other decoder in this module reads data we or the model authored — a
// session log, a tool's arguments, a catalog file — and can afford to be
// lenient. This one reads what a peer sent: an MCP server we spawned, a
// gateway in front of a provider, a client connecting to the AgentKit MCP
// server. The difference is not tone. A lenient decoder on that surface turns
// a malformed message into an allocation the peer chose, a duplicate key into
// a value the peer chose, and a deep nest into a stack the peer chose.
//
// WHY NOT encoding/json. REQ-SEC-11.5 states it plainly: encoding/json
// satisfies none of rules 1-4 and silently accepts duplicate keys. json.Decoder
// with Token() could be driven to enforce depth and container bounds, but
// duplicate keys are invisible through it — Token() reports both — and the
// unescaping is not reachable without re-implementing it anyway. So the scanner
// here is hand-rolled: it is one pass, it allocates nothing it was not asked
// to, and every bound is checked before the allocation it guards.
package wire

import (
	"errors"
	"fmt"
)

// Limits are REQ-SEC-11's bounds. The zero value means Defaults.
type Limits struct {
	// MaxMessageBytes rejects the whole message. Default 16 MiB.
	MaxMessageBytes int64
	// MaxContainerLen bounds array elements and object members. Default 1e6.
	MaxContainerLen int
	// MaxDepth bounds nesting. Default 64.
	MaxDepth int
}

// Defaults are the table in REQ-SEC-11.
func Defaults() Limits {
	return Limits{MaxMessageBytes: 16 << 20, MaxContainerLen: 1_000_000, MaxDepth: 64}
}

// WithDefaults fills the zero fields. It is exported because a caller that
// bounds a read BEFORE handing bytes to Parse — an SSE event, an HTTP body —
// needs the same number Parse will use, and re-deriving it is how two bounds
// drift apart.
func (l Limits) WithDefaults() Limits { return l.withDefaults() }

func (l Limits) withDefaults() Limits {
	d := Defaults()
	if l.MaxMessageBytes <= 0 {
		l.MaxMessageBytes = d.MaxMessageBytes
	}
	if l.MaxContainerLen <= 0 {
		l.MaxContainerLen = d.MaxContainerLen
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	return l
}

// Rule names which contract a rejection violated. It is a closed set so a
// caller can branch on it without matching on message text.
type Rule string

const (
	RuleMessageBytes Rule = "message_bytes"
	RuleContainerLen Rule = "container_len"
	RuleDepth        Rule = "depth"
	RuleDuplicateKey Rule = "duplicate_key"
	RuleSyntax       Rule = "syntax"
	RuleUnknownField Rule = "unknown_field"
	RuleType         Rule = "type"
	RuleRange        Rule = "range"
	RuleValidator    Rule = "validator"
)

// Error is a rejection. It names the rule and the path, because "invalid
// JSON" from a peer is not something an operator can act on.
type Error struct {
	Rule Rule
	Path string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	at := e.Path
	if at == "" {
		at = "$"
	}
	return fmt.Sprintf("wire: %s at %s: %s", e.Rule, at, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrRejected matches every rejection this package produces, so a transport
// can implement REQ-SEC-11.4's poisoning without enumerating rules.
var ErrRejected = errors.New("wire: message rejected")

func (e *Error) Is(target error) bool { return target == ErrRejected }

func failf(rule Rule, path, format string, args ...any) error {
	return &Error{Rule: rule, Path: path, Msg: fmt.Sprintf(format, args...)}
}
