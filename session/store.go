package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// Durability is REQ-SESS-09's "the store must document its durability level
// explicitly".
//
// The requirement offers three levels: buffered, fsync per entry, fsync per
// turn. The third is not implementable here (P-36): a SessionStore cannot see
// a turn without importing loop state, and importing loop state into the
// store inverts the dependency graph. Two levels ship, and Sync is the
// explicit escape hatch a caller uses to get fsync-per-turn from outside.
type Durability uint8

const (
	// DurabilityBuffered issues exactly one write(2) per entry and does not
	// fsync. "Buffered" means the OS page cache, NOT a user-space buffer:
	// REQ-SESS-09 requires that a session crashing during turn 1 still leave
	// the header and the user entry on disk, and a bufio.Writer loses both
	// when the process dies. So the data survives process death and does not
	// survive machine death.
	DurabilityBuffered Durability = iota

	// DurabilityPerEntry fsyncs after every entry. Correct for a session
	// whose loss would cost real work; roughly one disk flush per message.
	DurabilityPerEntry
)

// Options configures a Store.
type Options struct {
	// Durability defaults to DurabilityBuffered.
	Durability Durability

	// OnPersistError is REQ-SESS-08's mandated hook. Append also returns its
	// error and callers must not discard it; this exists for the path where
	// the SDK drives the store from loop events and the caller never sees the
	// return value. Silent failure is prohibited for an embeddable library.
	OnPersistError func(error)

	// NewID generates entry ids. Defaults to 16 random bytes, hex-encoded.
	// Injectable so a golden test can pin a whole file byte-for-byte.
	NewID func() core.EntryID

	// Now supplies entry timestamps. Defaults to time.Now().UTC().
	Now func() time.Time

	// FileMode is the mode for a newly created log. Defaults to 0o600: a
	// session log holds the full text of every prompt and tool result.
	FileMode os.FileMode
}

func (o Options) withDefaults() Options {
	if o.NewID == nil {
		o.NewID = randomID
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.FileMode == 0 {
		o.FileMode = 0o600
	}
	return o
}

func randomID() core.EntryID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never fails on any supported platform; falling
		// back to a timestamp keeps ids unique-enough rather than panicking
		// inside a persistence path.
		return core.EntryID(fmt.Sprintf("t%d", time.Now().UnixNano()))
	}
	return core.EntryID(hex.EncodeToString(b[:]))
}

// Store is the append-only JSONL SessionStore of REQ-SESS-01. It satisfies
// core.SessionStore and is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	opts Options

	path   string
	header core.SessionHeader
	// headerLine is the exact bytes to write for line 1. On a resumed store
	// it is empty, because the header is already in the file.
	headerLine []byte

	f *os.File
	// pendingNewline restores a terminator lost to a partial write before the
	// next entry is appended.
	pendingNewline bool
	closed         bool

	t *tree
}

var _ core.SessionStore = (*Store)(nil)

// Create prepares a new session log at path.
//
// It touches the filesystem NOT AT ALL. REQ-SESS-09 makes the FIRST FLUSH the
// moment of creation, with O_CREATE|O_EXCL, so an agent that is constructed
// and never run leaves nothing behind, and a collision with an existing file
// is reported through Append (REQ-SESS-08) rather than swallowed. The header
// and the first entry are written in the same flush, which is what makes
// "a session that crashes during turn 1 must still leave a header and the
// user entry on disk" true.
func Create(path string, h core.SessionHeader, opts Options) (*Store, error) {
	opts = opts.withDefaults()
	if h.Version == 0 {
		h.Version = core.SessionLogVersion
	}
	if h.ID == "" {
		h.ID = string(opts.NewID())
	}
	if h.Timestamp.IsZero() {
		h.Timestamp = opts.Now()
	}
	if h.CWD == "" {
		if wd, err := os.Getwd(); err == nil {
			h.CWD = wd
		}
	}
	line, err := EncodeHeader(h)
	if err != nil {
		return nil, err
	}
	return &Store{opts: opts, path: path, header: h, headerLine: line, t: newTree()}, nil
}

// Open resumes an existing log: it loads and repairs the file, applies the
// FILE-level part of the repair, and returns a store positioned to append.
//
// The file-level part is P-3, and it is the whole reason Open exists as
// something other than Load followed by opening for append. REQ-SESS-05.1
// says to discard a truncated final line, but after discarding it the
// O_APPEND offset still sits past the surviving partial bytes, so the next
// entry concatenates onto the partial and BOTH are lost — and the loader
// reports the same repair forever. Open truncates the file to the last
// newline offset the loader already computed, before the first append.
func Open(path string, opts Options) (*Store, *Loaded, error) {
	opts = opts.withDefaults()
	l, err := Load(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, opts.FileMode)
	if err != nil {
		return nil, l, err
	}
	if l.truncate {
		if err := f.Truncate(l.truncateTo); err != nil {
			f.Close()
			return nil, l, fmt.Errorf("session: truncating damaged tail: %w", err)
		}
		// The truncation must reach the disk before the next append, or a
		// second crash resurrects exactly the state this call just repaired.
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, l, fmt.Errorf("session: syncing truncation: %w", err)
		}
	}
	s := &Store{
		opts:           opts,
		path:           path,
		header:         l.Header,
		f:              f,
		pendingNewline: l.needsNewline,
		t:              l.t,
	}
	if l.needsHeader {
		// The file was emptied by repair (or was empty to begin with), so the
		// next flush must write the header again.
		line, err := EncodeHeader(l.Header)
		if err != nil {
			f.Close()
			return nil, l, err
		}
		s.headerLine = line
	}
	return s, l, nil
}

// Header returns line 1's decoded form.
func (s *Store) Header() core.SessionHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header
}

// Path is the log's location on disk.
func (s *Store) Path() string { return s.path }

// SetOnPersistError installs REQ-SESS-08's hook after construction.
func (s *Store) SetOnPersistError(fn func(error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.OnPersistError = fn
}

// Append durably records one entry parented at Head and advances Head.
//
// The store assigns ID (when empty), ParentID and Timestamp (when zero).
// ParentID is ALWAYS the current head: re-parenting is what branching is
// (REQ-SESS-07), and it goes through ForkFrom so that a caller cannot create a
// branch by accident with a stale entry value.
//
// It returns its error. REQ-SESS-08: marshal and write failures must not be
// discarded.
func (s *Store) Append(e core.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.fail(errors.New("session: append to a closed store"))
	}
	if e.ID == core.NullLeaf {
		e.ID = s.opts.NewID()
	}
	if s.t.has(e.ID) {
		return s.fail(fmt.Errorf("session: entry id %q is already in the log", e.ID))
	}
	e.ParentID = s.t.head
	if e.Timestamp.IsZero() {
		e.Timestamp = s.opts.Now()
	}
	line, err := EncodeEntry(e)
	if err != nil {
		return s.fail(err)
	}
	if err := s.write(line); err != nil {
		return s.fail(err)
	}
	// Raw becomes the bytes actually on disk, so a later re-emission of this
	// entry reproduces them exactly (P-57).
	e.Raw = line
	s.t.add(e)
	return nil
}

// write emits header (if unwritten) plus one line in a single write(2), so a
// process killed between them cannot exist.
func (s *Store) write(line []byte) error {
	buf := make([]byte, 0, len(s.headerLine)+len(line)+2)
	if s.pendingNewline {
		buf = append(buf, '\n')
	}
	if len(s.headerLine) > 0 {
		buf = append(buf, s.headerLine...)
		buf = append(buf, '\n')
	}
	if line != nil {
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if len(buf) == 0 {
		return nil
	}
	if err := s.ensureFile(); err != nil {
		return err
	}
	if _, err := s.f.Write(buf); err != nil {
		return err
	}
	s.pendingNewline = false
	s.headerLine = nil
	if s.opts.Durability == DurabilityPerEntry {
		return s.f.Sync()
	}
	return nil
}

// ensureFile performs REQ-SESS-09's first flush: O_CREATE|O_EXCL, which never
// clobbers an existing file at the session path. The collision surfaces as
// core.ErrSessionExists through Append.
func (s *Store) ensureFile() error {
	if s.f != nil {
		return nil
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, s.opts.FileMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", core.ErrSessionExists, s.path)
		}
		return err
	}
	s.f = f
	return nil
}

func (s *Store) fail(err error) error {
	if err != nil && s.opts.OnPersistError != nil {
		s.opts.OnPersistError(err)
	}
	return err
}

// Entries returns every entry in file order.
func (s *Store) Entries() []core.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.snapshot()
}

// Branch returns the root->leaf path (REQ-SESS-07).
func (s *Store) Branch(leafID core.EntryID) ([]core.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.branch(leafID)
}

// Leaves returns the divergent tips (REQ-SESS-07).
func (s *Store) Leaves() []core.EntryID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.leaves()
}

// Head is the active leaf (P-38). It is core.NullLeaf before the first entry
// and after ForkFrom(core.NullLeaf) — REQ-SESS-07's explicit null-leaf state.
func (s *Store) Head() core.EntryID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t.head
}

// ForkFrom repoints Head so the next Append becomes a second child of id.
// Nothing is rewritten and nothing is deleted: both branches stay in the same
// file (REQ-SESS-07). ForkFrom(core.NullLeaf) starts a new root.
func (s *Store) ForkFrom(id core.EntryID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != core.NullLeaf && !s.t.has(id) {
		return fmt.Errorf("%w: %q", ErrUnknownEntry, id)
	}
	s.t.head = id
	return nil
}

// Sync flushes the log to stable storage. It is the escape hatch REQ-SESS-09's
// third durability level would have been: the store cannot see a turn, so a
// caller wanting fsync-per-turn calls this at its own turn boundary (P-36).
//
// Sync on a store that has never flushed creates the file and writes the
// header, so a session is on disk from the moment its owner asks for it.
func (s *Store) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.fail(errors.New("session: sync on a closed store"))
	}
	if err := s.write(nil); err != nil {
		return s.fail(err)
	}
	if s.f == nil {
		return nil
	}
	return s.fail(s.f.Sync())
}

// Close syncs and closes the file. It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.f == nil {
		return nil
	}
	err := s.f.Sync()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	s.f = nil
	return s.fail(err)
}
