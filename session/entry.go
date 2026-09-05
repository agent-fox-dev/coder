package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/jsonx"
)

// Envelope keys, in the order REQ-SESS-01 prints them.
const (
	keyID       = "id"
	keyParentID = "parent_id"
	keyType     = "type"
	keyTime     = "timestamp"
)

// headerType is the value of the header line's "type" key. It is deliberately
// a value in the same key namespace as an entry type, so line 1 is
// self-identifying even when a file is concatenated or truncated at the front.
const headerType = "session"

// ErrNotAnEntry is returned by DecodeEntry for a line that is not a JSON
// object. It is a damage class, not a programming error: the loader catches it
// and records a repair.
var ErrNotAnEntry = errors.New("session: line is not a JSON object")

// EncodeHeader renders line 1 of the log in REQ-SESS-01's exact key order:
// type, version, id, timestamp, cwd.
func EncodeHeader(h core.SessionHeader) ([]byte, error) {
	var o jsonx.OrderedObject
	o.Set(keyType, jsonx.OVString(headerType))
	o.Set("version", intValue(int64(h.Version)))
	o.Set(keyID, jsonx.OVString(h.ID))
	ts := h.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	o.Set(keyTime, jsonx.OVString(ts.Format(time.RFC3339Nano)))
	o.Set("cwd", jsonx.OVString(h.CWD))
	return o.MarshalJSON()
}

// DecodeHeader parses line 1. A line that is not an object, or whose "type" is
// not "session", is not a header; the caller treats that as REPAIR_MISSING_HEADER
// and reconsiders the line as an entry.
func DecodeHeader(line []byte) (core.SessionHeader, error) {
	v, err := jsonx.DecodeOrdered(line)
	if err != nil {
		return core.SessionHeader{}, err
	}
	if v.Kind != jsonx.KindObject {
		return core.SessionHeader{}, ErrNotAnEntry
	}
	o := v.Object
	if getString(o, keyType) != headerType {
		return core.SessionHeader{}, fmt.Errorf("session: line 1 has type %q, want %q",
			getString(o, keyType), headerType)
	}
	h := core.SessionHeader{
		ID:        getString(o, keyID),
		Timestamp: getTime(o, keyTime),
		CWD:       getString(o, "cwd"),
	}
	if n, ok := getInt(o, "version"); ok {
		h.Version = int(n)
	}
	return h, nil
}

// payloadKey names the object key holding an entry's type-specific payload.
// It is the entry type itself, so the shape is self-describing and an unknown
// type needs no special case on the read side.
func payloadKey(t core.EntryType) string { return string(t) }

// EncodeEntry renders one entry as one line, without the terminating newline.
//
// When e.Raw is present it is the authority (P-57). Raw passthrough is not
// only for unknown entry types: decode-then-re-encode of a MODELLED entry also
// reorders keys and rewrites numeric literals (1e3 -> 1000, and a 64-bit
// integer loses its low bit through float64), so NFR-TEST-03(d) and
// REQ-SESS-05.2 together force verbatim retention for EVERY entry. Only the
// envelope fields are patched, and only where the value actually differs, so
// a re-emitted entry is byte-identical whenever its identity is unchanged.
func EncodeEntry(e core.Entry) ([]byte, error) {
	if len(e.Raw) > 0 {
		if b, err := patchEnvelope(e); err == nil {
			return b, nil
		}
		// Raw is not decodable (it was hand-set, or came from a damaged
		// line). Fall through and rebuild from the modelled fields rather
		// than write bytes we cannot parse back.
	}
	return buildEntry(e)
}

func patchEnvelope(e core.Entry) ([]byte, error) {
	v, err := jsonx.DecodeOrdered(e.Raw)
	if err != nil {
		return nil, err
	}
	if v.Kind != jsonx.KindObject {
		return nil, ErrNotAnEntry
	}
	o := v.Object.Clone()
	if got := getString(o, keyID); got != string(e.ID) {
		o.Set(keyID, jsonx.OVString(string(e.ID)))
	}
	// An absent parent_id and an empty one both mean "root". Leaving an absent
	// key absent is what keeps a foreign writer's line byte-identical.
	if got := core.EntryID(getString(o, keyParentID)); got != e.ParentID {
		o.Set(keyParentID, jsonx.OVString(string(e.ParentID)))
	}
	if got := getString(o, keyType); got != string(e.Type) {
		o.Set(keyType, jsonx.OVString(string(e.Type)))
	}
	if !getTime(o, keyTime).Equal(e.Timestamp) {
		if e.Timestamp.IsZero() {
			o.Delete(keyTime)
		} else {
			o.Set(keyTime, jsonx.OVString(e.Timestamp.Format(time.RFC3339Nano)))
		}
	}
	return o.MarshalJSON()
}

func buildEntry(e core.Entry) ([]byte, error) {
	var o jsonx.OrderedObject
	o.Set(keyID, jsonx.OVString(string(e.ID)))
	o.Set(keyParentID, jsonx.OVString(string(e.ParentID)))
	o.Set(keyType, jsonx.OVString(string(e.Type)))
	setTime(&o, keyTime, e.Timestamp)

	switch e.Type {
	case core.EntryMessage:
		if e.Message == nil || e.Message.Message == nil {
			return nil, fmt.Errorf("session: message entry %q has no message", e.ID)
		}
		o.Set(payloadKey(e.Type), encodeMessage(e.Message.Message))
	case core.EntryModelChange:
		if e.ModelChange == nil {
			return nil, fmt.Errorf("session: model_change entry %q has no payload", e.ID)
		}
		var p jsonx.OrderedObject
		p.Set("provider", jsonx.OVString(e.ModelChange.Provider))
		// api is load-bearing, not decoration: see P-4 and ModelChangeEntry's
		// doc comment. Emitted even when empty so its absence in an older log
		// is distinguishable from a build that never wrote it.
		p.Set("api", jsonx.OVString(string(e.ModelChange.API)))
		p.Set("model_id", jsonx.OVString(e.ModelChange.ModelID))
		o.Set(payloadKey(e.Type), objValue(p))
	case core.EntryThinkingLevelChange:
		if e.ThinkingChange == nil {
			return nil, fmt.Errorf("session: thinking_level_change entry %q has no payload", e.ID)
		}
		var p jsonx.OrderedObject
		p.Set("thinking_level", jsonx.OVString(string(e.ThinkingChange.Level)))
		o.Set(payloadKey(e.Type), objValue(p))
	case core.EntryCompaction:
		if e.Compaction == nil {
			return nil, fmt.Errorf("session: compaction entry %q has no payload", e.ID)
		}
		var p jsonx.OrderedObject
		p.Set("summary", jsonx.OVString(e.Compaction.Summary))
		p.Set("first_kept_entry_id", jsonx.OVString(string(e.Compaction.FirstKeptEntryID)))
		setString(&p, "previous_summary", e.Compaction.PreviousSummary)
		o.Set(payloadKey(e.Type), objValue(p))
	case core.EntryCustomMessage:
		if e.Custom == nil {
			return nil, fmt.Errorf("session: custom_message entry %q has no payload", e.ID)
		}
		var p jsonx.OrderedObject
		p.Set("kind", jsonx.OVString(e.Custom.Kind))
		p.Set("content", encodeContent(e.Custom.Content))
		o.Set(payloadKey(e.Type), objValue(p))
	case core.EntryBranchSummary:
		if e.BranchSummary == nil {
			return nil, fmt.Errorf("session: branch_summary entry %q has no payload", e.ID)
		}
		var p jsonx.OrderedObject
		p.Set("summary", jsonx.OVString(e.BranchSummary.Summary))
		p.Set("from_leaf_id", jsonx.OVString(string(e.BranchSummary.FromLeafID)))
		p.Set("fork_point_id", jsonx.OVString(string(e.BranchSummary.ForkPointID)))
		o.Set(payloadKey(e.Type), objValue(p))
	default:
		// An entry of a type this build does not model, with no Raw to fall
		// back on, carries nothing we could write. Refusing is better than
		// writing a payload-less line that a later reader would keep forever.
		return nil, fmt.Errorf("session: cannot encode unknown entry type %q without Raw", e.Type)
	}
	return o.MarshalJSON()
}

// DecodeEntry parses one line into an Entry, retaining the line's bytes in
// Raw. An unknown entry type decodes to an Entry with no payload struct and a
// populated Raw, which is exactly what REQ-SESS-05.2 requires: retained
// verbatim and re-emitted on write.
func DecodeEntry(line []byte) (core.Entry, error) {
	v, err := jsonx.DecodeOrdered(line)
	if err != nil {
		return core.Entry{}, err
	}
	if v.Kind != jsonx.KindObject {
		return core.Entry{}, ErrNotAnEntry
	}
	o := v.Object
	e := core.Entry{
		ID:        core.EntryID(getString(o, keyID)),
		ParentID:  core.EntryID(getString(o, keyParentID)),
		Type:      core.EntryType(getString(o, keyType)),
		Timestamp: getTime(o, keyTime),
		Raw:       append(json.RawMessage(nil), line...),
	}
	p, hasPayload := o.Get(payloadKey(e.Type))
	if !hasPayload {
		// No payload under the type key. The entry is still retained whole.
		return e, nil
	}
	switch e.Type {
	case core.EntryMessage:
		m, err := decodeMessage(p)
		if err != nil {
			// A message entry whose payload will not decode keeps its bytes
			// and contributes no message. Dropping the line would lose the
			// branch structure that hangs off its id.
			return e, nil
		}
		e.Message = &core.MessageEntry{Message: m}
	case core.EntryModelChange:
		if p.Kind == jsonx.KindObject {
			e.ModelChange = &core.ModelChangeEntry{
				Provider: getString(p.Object, "provider"),
				API:      core.API(getString(p.Object, "api")),
				ModelID:  getString(p.Object, "model_id"),
			}
		}
	case core.EntryThinkingLevelChange:
		if p.Kind == jsonx.KindObject {
			e.ThinkingChange = &core.ThinkingLevelChangeEntry{
				Level: core.ThinkingLevel(getString(p.Object, "thinking_level")),
			}
		}
	case core.EntryCompaction:
		if p.Kind == jsonx.KindObject {
			e.Compaction = &core.CompactionEntry{
				Summary:          getString(p.Object, "summary"),
				FirstKeptEntryID: core.EntryID(getString(p.Object, "first_kept_entry_id")),
				PreviousSummary:  getString(p.Object, "previous_summary"),
			}
		}
	case core.EntryCustomMessage:
		if p.Kind == jsonx.KindObject {
			c := &core.CustomMessageEntry{Kind: getString(p.Object, "kind")}
			if arr, ok := getArray(p.Object, "content"); ok {
				c.Content = decodeContentArray(arr)
			}
			e.Custom = c
		}
	case core.EntryBranchSummary:
		if p.Kind == jsonx.KindObject {
			e.BranchSummary = &core.BranchSummaryEntry{
				Summary:     getString(p.Object, "summary"),
				FromLeafID:  core.EntryID(getString(p.Object, "from_leaf_id")),
				ForkPointID: core.EntryID(getString(p.Object, "fork_point_id")),
			}
		}
	}
	return e, nil
}

// NewMessageEntry builds an unsaved message entry. ID and ParentID are
// assigned by the store on Append.
func NewMessageEntry(m core.Message) core.Entry {
	return core.Entry{Type: core.EntryMessage, Message: &core.MessageEntry{Message: m}}
}

// NewModelChangeEntry builds an unsaved model_change entry. It takes the
// provenance TRIPLE because REQ-PROV-11 rule 1 needs all three (P-4).
func NewModelChangeEntry(provider string, api core.API, modelID string) core.Entry {
	return core.Entry{
		Type:        core.EntryModelChange,
		ModelChange: &core.ModelChangeEntry{Provider: provider, API: api, ModelID: modelID},
	}
}

// NewThinkingLevelEntry builds an unsaved thinking_level_change entry.
func NewThinkingLevelEntry(l core.ThinkingLevel) core.Entry {
	return core.Entry{
		Type:           core.EntryThinkingLevelChange,
		ThinkingChange: &core.ThinkingLevelChangeEntry{Level: l},
	}
}

// NewCompactionEntry builds an unsaved compaction entry (REQ-SESS-04). The
// summarized entries are not removed; firstKept anchors the kept tail.
func NewCompactionEntry(summary string, firstKept core.EntryID, previous string) core.Entry {
	return core.Entry{
		Type: core.EntryCompaction,
		Compaction: &core.CompactionEntry{
			Summary:          summary,
			FirstKeptEntryID: firstKept,
			PreviousSummary:  previous,
		},
	}
}

// NewCustomMessageEntry builds an unsaved custom_message entry.
func NewCustomMessageEntry(kind string, c core.Content) core.Entry {
	return core.Entry{
		Type:   core.EntryCustomMessage,
		Custom: &core.CustomMessageEntry{Kind: kind, Content: c},
	}
}

// NewBranchSummaryEntry builds an unsaved branch_summary entry (REQ-SESS-07).
// The summary text is caller-supplied: generating it would mean a second
// out-of-band model call with its own failure taxonomy, which nothing
// specifies (P-55). ForkFrom writes no entry of its own.
func NewBranchSummaryEntry(summary string, fromLeaf, forkPoint core.EntryID) core.Entry {
	return core.Entry{
		Type: core.EntryBranchSummary,
		BranchSummary: &core.BranchSummaryEntry{
			Summary:     summary,
			FromLeafID:  fromLeaf,
			ForkPointID: forkPoint,
		},
	}
}
