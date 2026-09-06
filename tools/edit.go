package tools

import (
	"fmt"
	"sort"
	"strings"
)

// Edit is one requested replacement.
type Edit struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// EditError is a rejection with the exact model-visible text the PRD pins.
// The strings are part of the contract: two conforming implementations that
// worded them differently would show the model different messages and get
// different self-correction behaviour.
type EditError struct {
	Phase string
	Index int
	Text  string
}

func (e *EditError) Error() string { return e.Text }

// ApplyEdits applies a batch of edits to content, or rejects the whole batch.
//
// The contract, from REQ-TOOL-04a-d:
//
//   - Every old_string matches against the ORIGINAL content, never against the
//     result of an earlier edit in the same call. This is what makes a batch
//     reviewable: the model reasoned about one file, not about a moving one.
//   - A non-unique old_string is a REJECTION, not a replace-all (REQ-TOOL-04c).
//     Multiplicity means the model has not identified a site; replacing all of
//     them is how an agent corrupts a file it was asked to touch once. There
//     is deliberately no {replaced: N} success shape.
//   - Rejections are evaluated in GLOBAL PHASE ORDER, not per edit (ruling
//     P-22). Phases 4 and 5 are inherently global, and per-edit ordering is a
//     real behavioural fork: it would report "not found" for edit 2 while a
//     phase-ordered implementation reports "not unique" for edit 1, showing
//     the model a different problem to fix.
//
// Returns the new content and the number of edits applied.
func ApplyEdits(content string, edits []Edit) (string, int, error) {
	if len(edits) == 0 {
		return "", 0, &EditError{Phase: "empty", Text: "No edits were provided."}
	}

	// ---- Phase 1: empty old_string.
	for i, e := range edits {
		if e.OldString == "" {
			return "", 0, &EditError{Phase: "empty_old_string", Index: i,
				Text: fmt.Sprintf("edits[%d].old_string is empty. Provide the exact text to replace.", i)}
		}
	}

	// ---- Phase 2: not found.
	//
	// REQ-TOOL-04d's fallback hangs off this phase, and only this phase. An
	// edit that was FOUND exactly is never re-matched leniently: the fold
	// exists to rescue a batch that would otherwise be rejected outright, not
	// to widen matching for one that already works.
	for i, e := range edits {
		if !strings.Contains(content, e.OldString) {
			if out, n, ok := applyEditsFolded(content, edits); ok {
				return out, n, nil
			}
			return "", 0, &EditError{Phase: "not_found", Index: i,
				Text: fmt.Sprintf("edits[%d]: the string to replace was not found in the file.", i)}
		}
	}

	// ---- Phase 3: not unique. The pinned wording.
	for i, e := range edits {
		if n := strings.Count(content, e.OldString); n > 1 {
			return "", 0, &EditError{Phase: "not_unique", Index: i,
				Text: fmt.Sprintf("Found %d occurrences of the string to replace. "+
					"The text must be unique. Please provide more context to make it unique.", n)}
		}
	}

	// ---- Phase 4: overlap. Sort matches by offset and reject when the
	// previous match's end passes the next one's start. Two edits that overlap
	// cannot both be applied against the original, and applying either alone
	// silently drops the other.
	type match struct {
		idx    int
		offset int
		length int
	}
	ms := make([]match, 0, len(edits))
	for i, e := range edits {
		ms = append(ms, match{idx: i, offset: strings.Index(content, e.OldString), length: len(e.OldString)})
	}
	sort.Slice(ms, func(a, b int) bool { return ms[a].offset < ms[b].offset })
	for k := 1; k < len(ms); k++ {
		prev, cur := ms[k-1], ms[k]
		if prev.offset+prev.length > cur.offset {
			i, j := prev.idx, cur.idx
			if i > j {
				i, j = j, i
			}
			return "", 0, &EditError{Phase: "overlap", Index: i,
				Text: fmt.Sprintf("edits[%d] and edits[%d] overlap. "+
					"Merge them into one edit or target disjoint regions.", i, j)}
		}
	}

	// ---- Apply. Because matches are disjoint and sorted, splicing right to
	// left keeps every remaining offset valid without recomputing anything.
	out := content
	for k := len(ms) - 1; k >= 0; k-- {
		m := ms[k]
		out = out[:m.offset] + edits[m.idx].NewString + out[m.offset+m.length:]
	}

	// ---- Phase 5: no-op. A batch that changes nothing is a rejection, not a
	// silent success: it means the model believes it edited something it did
	// not, and reporting success would let it move on.
	if out == content {
		return "", 0, &EditError{Phase: "noop",
			Text: "The edits would leave the file unchanged."}
	}
	return out, len(edits), nil
}

// LineEnding describes the convention a file uses.
type LineEnding string

const (
	LF   LineEnding = "lf"
	CRLF LineEnding = "crlf"
)

// NormalizeForEdit strips a leading BOM and converts CRLF to LF so matching
// works against what the model actually saw, and reports what it removed so
// Restore can put it back (REQ-TOOL-04d).
//
// The CRLF restoration is LOSSY for a mixed-ending file: if the original
// contains ANY CRLF the output is CRLF throughout (ruling P-24). That is a
// deliberate, reported choice — preserving per-line endings would require
// tracking them through every splice, and a file with mixed endings is
// already in a state no editor preserves faithfully.
func NormalizeForEdit(s string) (normalized string, bom bool, ending LineEnding) {
	ending = LF
	if strings.HasPrefix(s, "\ufeff") {
		bom = true
		s = strings.TrimPrefix(s, "\ufeff")
	}
	if strings.Contains(s, "\r\n") {
		ending = CRLF
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return s, bom, ending
}

// Restore re-applies what NormalizeForEdit removed.
func Restore(s string, bom bool, ending LineEnding) string {
	if ending == CRLF {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	if bom {
		s = "\ufeff" + s
	}
	return s
}

// applyEditsFolded is REQ-TOOL-04d's whitespace-tolerant pass.
//
// It runs only after exact matching has failed for the batch, and it reports
// failure by returning ok=false rather than its own error: the caller then
// emits the ORIGINAL phase-ordered rejection. A second, differently-worded
// error from a fallback the model never asked for would tell it to fix the
// wrong thing.
//
// Matching is per LINE BLOCK and the splice puts back whole ORIGINAL lines, so
// every line outside a matched block keeps its exact bytes — the curly quotes
// and trailing spaces that made the fold necessary in the first place survive
// untouched. The fold is only ever a key.
func applyEditsFolded(content string, edits []Edit) (string, int, bool) {
	lines := strings.Split(content, "\n")
	folded := foldLines(lines)

	type block struct {
		idx        int
		start, end int // line range, half-open
	}
	blocks := make([]block, 0, len(edits))

	for i, e := range edits {
		needle := foldLines(strings.Split(e.OldString, "\n"))
		start, end, count := findFoldedBlock(folded, needle)
		// Every edit must match, and match exactly once. A batch where the
		// fold rescues some edits and not others is not a batch the model
		// meant, and applying the subset would be the silent partial
		// application REQ-TOOL-04's phases exist to prevent.
		if count != 1 {
			return "", 0, false
		}
		blocks = append(blocks, block{idx: i, start: start, end: end})
	}

	sort.Slice(blocks, func(a, b int) bool { return blocks[a].start < blocks[b].start })
	for k := 1; k < len(blocks); k++ {
		if blocks[k-1].end > blocks[k].start {
			return "", 0, false // overlapping blocks: same rejection as an exact overlap
		}
	}

	// Splice right to left so earlier ranges stay valid.
	out := append([]string(nil), lines...)
	for k := len(blocks) - 1; k >= 0; k-- {
		b := blocks[k]
		replacement := strings.Split(edits[b.idx].NewString, "\n")
		out = append(out[:b.start], append(replacement, out[b.end:]...)...)
	}
	joined := strings.Join(out, "\n")
	if joined == content {
		return "", 0, false // no-op, rejected exactly as the exact path rejects one
	}
	return joined, len(edits), true
}
