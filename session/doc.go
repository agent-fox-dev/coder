// Package session implements the durable session log of REQ-SESS-01..09: an
// append-only JSONL file, a damage-tolerant loader, the branch tree, and the
// fold that turns a log back into agent construction inputs.
//
// core owns the vocabulary (core.Entry, core.SessionStore, core.SessionHeader);
// this package owns the machinery. Nothing here imports the root package, so
// the log format has no opinion about the loop.
//
// # File format (REQ-SESS-01)
//
// Line 1 is the session header:
//
//	{"type":"session","version":1,"id":"…","timestamp":"…","cwd":"…"}
//
// Every subsequent line is exactly one entry, one JSON value per line and
// nothing else:
//
//	{"id":"…","parent_id":"…","type":"message","timestamp":"…","message":{…}}
//
// The envelope keys come first in the order REQ-SESS-01 lists them; the
// type-specific payload hangs off a key named for the entry type
// ("message", "model_change", "thinking_level_change", "compaction",
// "custom_message", "branch_summary").
//
// # Two boundaries this package does not cross
//
// REQ-SESS-06: structural repair of the LOG is not semantic repair of the
// TRANSCRIPT. A loaded session commonly ends with an unanswered tool_use.
// That is a valid log and an invalid request, and reconciling it is the
// provider's send-time transform (REQ-PROV-11), not the loader's. Nothing in
// this package inserts a synthetic tool result; TestLoaderDoesNotRepairTranscript
// pins that.
//
// REQ-SESS-04/REQ-GO-12: a compaction is an entry, not a rewrite. Fold reports
// the checkpoint and leaves every summarized message in the message list.
// Applying the checkpoint — dropping the prefix, prepending the summary — is
// the root package's view, built fresh on every request.
package session
