// Package mcp implements the Model Context Protocol — client and server — on
// the Go standard library.
//
// # Why this is not built on mcp-go
//
// REQ-MCP-CLIENT-01 names github.com/mark3labs/mcp-go. It is not used, and the
// reason is a conflict inside the specification rather than a preference.
//
// REQ-SEC-11 says every decoder reading bytes AgentKit did not produce is
// bounded before it allocates, and names three surfaces explicitly: "MCP stdio
// NDJSON from spawned subprocesses, MCP HTTP/SSE and streamable-HTTP response
// bodies, and inbound requests to the AgentKit MCP server". Rule 5 of that
// requirement then says `encoding/json` satisfies none of rules 1-4 and that a
// bare `json.Decoder` on an untrusted stream is a conformance failure.
//
// A third-party MCP library owns the wire. Handing it these three surfaces
// means REQ-SEC-11 is either satisfied by that library — no general-purpose
// JSON-RPC implementation rejects duplicate keys, because JSON-RPC does not
// ask it to — or not satisfied at all, on the exact surfaces the requirement
// was written for. The two requirements cannot both hold.
//
// Three smaller facts point the same way. REQ-GO-06 already assigns JSON-RPC
// id generation and response correlation to AgentKit, which would be a strange
// thing to specify about somebody else's code. REQ-GO-11 confines third-party
// modules to a nested module to keep the root standard-library-only; with no
// third-party module there is nothing to confine, so the goal is met more
// directly than the mechanism designed to protect it. And REQ-GO-11.2 requires
// a nested module to depend on a TAGGED root release, which does not exist
// yet — so the specified path is not merely inconvenient, it is not currently
// buildable.
//
// The cost is real and worth stating: this is a partial implementation of a
// moving specification, and every capability it does not model is a capability
// AgentKit does not have. What it covers is the client and server subset the
// §6.7 and §6.8 requirements actually name.
//
// The PRD is amended in 0.3.5.
package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
)

// Version is the JSON-RPC version string every message carries.
const Version = "2.0"

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// The MCP-reserved sub-range. 2026-07-28 partitions the JSON-RPC
	// server-error space: -32000..-32019 stays implementation-defined and
	// -32020..-32099 belongs to the specification. These three are the only
	// ones it has allocated, and they were RENUMBERED from the draft's
	// -32001/-32003/-32004 — an implementation still sending the draft codes
	// is now colliding with somebody's private range.
	CodeHeaderMismatch = -32020
	// CodeMissingRequiredClientCapability is returned when a server needs a
	// capability the request's clientCapabilities did not declare. With the
	// handshake gone, capabilities arrive per request, so this is a per-request
	// answer rather than a connection that fails to open.
	CodeMissingRequiredClientCapability = -32021
	CodeUnsupportedProtocolVersion      = -32022
)

// ID is a JSON-RPC id: a string, a number, or absent.
//
// It is a struct rather than an `any` because the two forms are not
// interchangeable on the wire and a correlation map keyed on `any` silently
// treats the string "1" and the number 1 as different pending calls — which is
// exactly the confusion a peer would exploit to make a response land on the
// wrong request.
type ID struct {
	num   int64
	str   string
	isStr bool
	set   bool
}

// NumberID and StringID construct the two forms.
func NumberID(n int64) ID  { return ID{num: n, set: true} }
func StringID(s string) ID { return ID{str: s, isStr: true, set: true} }

// IsSet reports whether an id is present. A request without one is a
// NOTIFICATION and takes no response.
func (i ID) IsSet() bool { return i.set }

// Key is the correlation-map key. It encodes the TYPE as well as the value, so
// a string id and a numeric id that render the same never collide.
func (i ID) Key() string {
	if !i.set {
		return ""
	}
	if i.isStr {
		return "s:" + i.str
	}
	return "n:" + strconv.FormatInt(i.num, 10)
}

func (i ID) String() string { return i.Key() }

func (i ID) MarshalJSON() ([]byte, error) {
	if !i.set {
		return []byte("null"), nil
	}
	if i.isStr {
		return json.Marshal(i.str)
	}
	return []byte(strconv.FormatInt(i.num, 10)), nil
}

func (i *ID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*i = ID{}
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*i = StringID(v)
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// A float id is legal JSON-RPC and useless as a correlation key;
		// carrying it as its literal text keeps it distinguishable without
		// pretending it is an integer.
		*i = StringID(s)
		return nil
	}
	*i = NumberID(n)
	return nil
}

// Message is one JSON-RPC frame in either direction.
//
// Request, response and notification share a struct because a transport reads
// a frame before it knows which it is, and three types would mean decoding
// twice — on an untrusted stream, twice as much attack surface for no gain.
// IsRequest, IsResponse and IsNotification classify it.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id,omitzero"`
	Method  string          `json:"method,omitzero"`
	Params  json.RawMessage `json:"params,omitzero"`
	Result  json.RawMessage `json:"result,omitzero"`
	Error   *Error          `json:"error,omitzero"`
}

func (m *Message) IsRequest() bool      { return m.Method != "" && m.ID.IsSet() }
func (m *Message) IsNotification() bool { return m.Method != "" && !m.ID.IsSet() }
func (m *Message) IsResponse() bool     { return m.Method == "" }

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitzero"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("mcp: jsonrpc error %d: %s", e.Code, e.Message)
}

// Errorf builds an error object.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------- correlation

// correlator is REQ-GO-06: id generation and response correlation over a
// goroutine-safe map.
//
// Every waiter owns a BUFFERED channel of one. An unbuffered channel would
// make delivery depend on the waiter still being there — and the waiter that
// has given up on a timeout is precisely the case where a late response
// arrives — so the reader goroutine would block forever on a send nobody will
// ever receive, taking the whole connection with it.
type correlator struct {
	seq      atomic.Int64
	mu       sync.Mutex
	pending  map[string]chan *Message
	closed   bool
	closeErr error
}

func newCorrelator() *correlator {
	return &correlator{pending: map[string]chan *Message{}}
}

// next allocates an id and registers a waiter.
func (c *correlator) next() (ID, chan *Message, error) {
	id := NumberID(c.seq.Add(1))
	ch := make(chan *Message, 1)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ID{}, nil, c.closeErr
	}
	c.pending[id.Key()] = ch
	return id, ch, nil
}

// deliver routes a response to its waiter. An unmatched response is DROPPED
// rather than treated as an error: a peer echoing an id we never sent is
// misbehaving, and tearing the connection down over it would let any peer
// disconnect us at will.
func (c *correlator) deliver(m *Message) bool {
	c.mu.Lock()
	ch, ok := c.pending[m.ID.Key()]
	if ok {
		delete(c.pending, m.ID.Key())
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	ch <- m
	return true
}

// forget drops a waiter that gave up, so a timed-out call does not leak an
// entry for the life of the connection.
func (c *correlator) forget(id ID) {
	c.mu.Lock()
	delete(c.pending, id.Key())
	c.mu.Unlock()
}

// fail wakes every waiter with one error. It is how a torn-down transport
// surfaces as a failure on each in-flight call instead of as N hangs.
func (c *correlator) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed, c.closeErr = true, err
	for key, ch := range c.pending {
		ch <- &Message{JSONRPC: Version, Error: Errorf(CodeInternalError, "%v", err)}
		delete(c.pending, key)
	}
}

func (c *correlator) inFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}
