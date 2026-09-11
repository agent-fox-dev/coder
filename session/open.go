package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// OpenOrCreate opens or creates a session log and returns the resume state.
//
// It is the front door for a persisted agent, and it exists so the ONLY way to
// build one is the correct way. REQ-SESS-02 requires the recovered model and
// reasoning level to be CONSTRUCTION INPUTS, not fields patched onto a built
// agent — a distinction that is easy to state and easy to violate, so
// agentkit.NewAgent rejects a non-empty store outright (ErrSessionNotEmpty)
// and this plus agentkit.NewAgentFromSession is what you reach for instead.
func OpenOrCreate(path string, opts Options) (*Store, *Resume, error) {
	store, loaded, err := Open(path, opts)
	if errors.Is(err, fs.ErrNotExist) {
		// A session that does not exist yet is the ordinary first-run case,
		// not an error the caller should have to distinguish.
		cwd, _ := os.Getwd()
		// The id and timestamp are left for Create to fill from opts.NewID
		// and opts.Now when the caller supplied them, exactly as Create does
		// on its own: those hooks exist so a golden can pin a whole file
		// byte-for-byte (NFR-TEST-08), and a front door that overrode them
		// made the header unpinnable through the one path an embedder is
		// told to use. The "sess_" prefix and wall-clock default are kept for
		// the caller that supplied neither.
		h := core.SessionHeader{Version: core.SessionLogVersion, CWD: cwd}
		if opts.NewID == nil {
			h.ID = sessionID()
		}
		if opts.Now == nil {
			h.Timestamp = time.Now()
		}
		store, err = Create(path, h, opts)
		if err != nil {
			return nil, nil, err
		}
		return store, &Resume{Path: path, Header: store.Header(),
			History: core.NewConversationHistory()}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	branch, err := store.Branch(store.Head())
	if err != nil {
		return nil, nil, err
	}
	r := Fold(store.Header(), branch)
	r.Path = path
	r.LoadRepairs = loaded.Repairs
	return store, &r, nil
}

// sessionID is the default session id: "sess_" over eight random bytes.
// rand.Read from crypto/rand cannot fail on any supported platform; since
// Go 1.24 it panics rather than returning an error.
func sessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
}
