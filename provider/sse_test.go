package provider_test

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/provider"
)

func readAllSSE(t *testing.T, body string) ([]provider.SSEEvent, error) {
	t.Helper()
	r := provider.NewSSEReader(strings.NewReader(body), 0)
	var out []provider.SSEEvent
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func TestSSEDispatchesOnBlankLines(t *testing.T) {
	evs, err := readAllSSE(t, "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != "a" || string(evs[0].Data) != "1" ||
		evs[1].Type != "b" || string(evs[1].Data) != "2" {
		t.Fatalf("events = %+v", evs)
	}
}

// TestMultipleDataLinesJoinWithNewline is the SSE grammar rule that a
// naive "last data line wins" reader silently drops. Anthropic does not use
// multi-line data today; a gateway in front of it may.
func TestMultipleDataLinesJoinWithNewline(t *testing.T) {
	evs, err := readAllSSE(t, "data: {\"a\":\ndata:  1}\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if !json.Valid(evs[0].Data) {
		t.Fatalf("joined payload is not valid JSON: %q", evs[0].Data)
	}
}

func TestCommentsAndKeepalivesAreIgnored(t *testing.T) {
	evs, err := readAllSSE(t, ": keep-alive\n\n:ping\nevent: x\ndata: 1\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "x" {
		t.Fatalf("events = %+v, want just the real one", evs)
	}
}

func TestExactlyOneLeadingSpaceIsStripped(t *testing.T) {
	evs, err := readAllSSE(t, "data:  two spaces\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if string(evs[0].Data) != " two spaces" {
		t.Fatalf("data = %q, want one leading space preserved", evs[0].Data)
	}
}

// TestATruncatedFinalEventIsDiscardedNotDispatched is the behaviour that keeps
// a truncation from being misdiagnosed.
//
// Dispatching a half-accumulated event hands the decoder half a line of JSON,
// and the parse error that follows names a malformed payload — sending the
// reader after a bug in the provider's serializer instead of after a dropped
// connection.
func TestATruncatedFinalEventIsDiscardedNotDispatched(t *testing.T) {
	evs, err := readAllSSE(t, "event: a\ndata: 1\n\nevent: b\ndata: {\"partial\"")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want only the complete one", evs)
	}
}

func TestOversizedEventIsRejectedBeforeItIsAccumulated(t *testing.T) {
	body := "data: " + strings.Repeat("x", 5000) + "\n\n"
	r := provider.NewSSEReader(strings.NewReader(body), 1024)
	if _, err := r.Next(); !errors.Is(err, provider.ErrSSEEventTooLarge) {
		t.Fatalf("err = %v, want ErrSSEEventTooLarge: a provider response is bytes we "+
			"did not produce and must be bounded before it is allocated", err)
	}
}

// TestTruncationErrorTextMatchesTheRetryAllowlist is the coupling between the
// two retry layers, made explicit.
//
// A truncated SSE body is a 200 with a complete-looking prefix, so the
// transport layer cannot see it. Only the semantic classifier can, and it
// matches on this exact phrase. Changing the string silently disables the
// retry for the commonest streaming failure there is.
func TestTruncationErrorTextMatchesTheRetryAllowlist(t *testing.T) {
	if !strings.Contains(provider.ErrSSETruncated.Error(), "stream ended before message_stop") {
		t.Fatalf("ErrSSETruncated = %q; RetryMiddleware's allowlist matches "+
			"\"stream ended before message_stop\"", provider.ErrSSETruncated)
	}
}
