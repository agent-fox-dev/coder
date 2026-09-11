package session

import "github.com/agentfox/agentkit-go/core"

// EventJSON encodes an event as REQ-OBS-06c's discriminated union: a `type`
// member plus exactly the fields that variant carries. Messages inside the
// event are rendered by this package's codec — the one lossless encoder — so a
// consumer bridging the stream to a socket sees the same message shape it
// would read back from a session log.
func EventJSON(e core.Event) ([]byte, error) {
	return core.MarshalEvent(e, EncodeMessage)
}
