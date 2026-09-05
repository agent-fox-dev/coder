package tools

import (
	"fmt"
	"os"
	"strings"
)

// Accumulator is REQ-TOOL-15's bounded rolling accumulator.
//
// It retains a bounded HEAD and a bounded TAIL, discarding the middle as bytes
// arrive, so peak memory is roughly 2×cap regardless of how much a subprocess
// writes. The obvious alternative — buffer everything, truncate at the end —
// is quadratic in output size and out-of-memories on precisely the runaway
// command the cap exists to contain.
//
// It also lazily spills the COMPLETE output to a file on first overflow, so
// the full text stays available to the caller and the audit trail while only
// the bounded window reaches the model (REQ-TOOL-15.2).
type Accumulator struct {
	// Cap is the byte budget for the retained window.
	Cap int
	// Mode selects which end survives.
	Mode TruncateMode
	// SpillDir, when non-empty, enables the spill file.
	SpillDir string
	// SpillPrefix names the spill file.
	SpillPrefix string

	head []byte
	tail []byte
	// total is every byte ever written, including discarded ones.
	total int64

	spill     *os.File
	spillPath string
	spillErr  error
}

// TruncateMode selects which end of the stream survives truncation.
type TruncateMode int

const (
	// TruncateHead keeps the BEGINNING. Right for a file read or a listing,
	// where the interesting part is at the top.
	TruncateHead TruncateMode = iota
	// TruncateTail keeps the END. Right for `execute` (REQ-TOOL-09a): a
	// failing build puts its error at the end of the log, and head-truncation
	// preserves the banner and discards the failure.
	TruncateTail
	// TruncateMiddle keeps both ends and elides the centre.
	TruncateMiddle
)

// NewAccumulator returns an accumulator with the given budget and mode.
func NewAccumulator(capBytes int, mode TruncateMode) *Accumulator {
	if capBytes <= 0 {
		capBytes = DefaultByteLimit
	}
	return &Accumulator{Cap: capBytes, Mode: mode}
}

// Write implements io.Writer, so an Accumulator can be handed straight to
// exec.Cmd as its combined output sink.
func (a *Accumulator) Write(p []byte) (int, error) {
	a.total += int64(len(p))
	a.writeSpill(p)

	switch a.Mode {
	case TruncateHead:
		// Keep only the first Cap bytes; drop the rest on the floor.
		if room := a.Cap - len(a.head); room > 0 {
			if len(p) <= room {
				a.head = append(a.head, p...)
			} else {
				a.head = append(a.head, p[:room]...)
			}
		}
	case TruncateTail:
		a.tail = appendBounded(a.tail, p, a.Cap)
	case TruncateMiddle:
		half := a.Cap / 2
		if room := half - len(a.head); room > 0 {
			if len(p) <= room {
				a.head = append(a.head, p...)
				return len(p), nil
			}
			a.head = append(a.head, p[:room]...)
			p = p[room:]
		}
		a.tail = appendBounded(a.tail, p, half)
	}
	return len(p), nil
}

// appendBounded keeps at most n trailing bytes of dst+src without ever
// allocating more than n+len(src).
func appendBounded(dst, src []byte, n int) []byte {
	if len(src) >= n {
		out := make([]byte, n)
		copy(out, src[len(src)-n:])
		return out
	}
	if len(dst)+len(src) <= n {
		return append(dst, src...)
	}
	drop := len(dst) + len(src) - n
	return append(dst[drop:], src...)
}

func (a *Accumulator) writeSpill(p []byte) {
	if a.SpillDir == "" || a.spillErr != nil {
		return
	}
	if a.spill == nil {
		// Lazily created on FIRST write, so a command producing nothing leaves
		// no file behind.
		f, err := os.CreateTemp(a.SpillDir, a.SpillPrefix+"-*.log")
		if err != nil {
			a.spillErr = err
			return
		}
		a.spill, a.spillPath = f, f.Name()
	}
	if _, err := a.spill.Write(p); err != nil {
		a.spillErr = err
	}
}

// Total reports every byte written, including discarded ones.
func (a *Accumulator) Total() int64 { return a.total }

// Truncated reports whether anything was discarded.
func (a *Accumulator) Truncated() bool { return a.total > int64(len(a.head)+len(a.tail)) }

// SpillPath returns the spill file path, or "" if nothing spilled.
func (a *Accumulator) SpillPath() string {
	if a.spill == nil {
		return ""
	}
	return a.spillPath
}

// Close releases the spill file handle.
func (a *Accumulator) Close() error {
	if a.spill == nil {
		return nil
	}
	err := a.spill.Close()
	a.spill = nil
	return err
}

// String renders the retained window, with a marker naming what was elided.
//
// REQ-TOOL-09b: a truncation the model cannot act on costs a turn. Every
// marker therefore names the number of bytes elided and, where one exists, the
// spill path — which is reachable through `execute`, because `execute` is not
// path-contained (ruling P-45).
func (a *Accumulator) String() string {
	if !a.Truncated() {
		return string(a.head) + string(a.tail)
	}
	elided := a.total - int64(len(a.head)+len(a.tail))
	var b strings.Builder
	switch a.Mode {
	case TruncateHead:
		b.Write(a.head)
		b.WriteString("\n" + a.marker(elided))
	case TruncateTail:
		b.WriteString(a.marker(elided) + "\n")
		b.Write(a.tail)
	case TruncateMiddle:
		b.Write(a.head)
		b.WriteString("\n" + a.marker(elided) + "\n")
		b.Write(a.tail)
	}
	return b.String()
}

func (a *Accumulator) marker(elided int64) string {
	if p := a.SpillPath(); p != "" {
		return fmt.Sprintf("[%d bytes elided of %d total. Full output: %s]", elided, a.total, p)
	}
	return fmt.Sprintf("[%d bytes elided of %d total.]", elided, a.total)
}
