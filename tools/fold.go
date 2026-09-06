package tools

import "strings"

// REQ-TOOL-04d's whitespace-tolerant fallback.
//
// The problem it solves: a model often sees a file through a lossy render —
// a web UI that curls quotes, a terminal that pads lines, a paste that turned
// a hyphen into an en dash. It then supplies an `old_string` that is right in
// every way a human would judge and wrong byte-for-byte, and the edit is
// rejected as "not found" with nothing to act on.
//
// Two things keep this from becoming a licence to guess:
//
//  1. It runs ONLY after exact matching has failed for the whole batch. An
//     exact match is never overridden by a folded one.
//  2. It matches at LINE granularity and splices whole original lines back, so
//     every line the edit did not touch keeps its exact bytes — including the
//     smart quotes and trailing spaces that made the fold necessary. The fold
//     is a matching key, never an output.
//
// The requirement flags the cost explicitly: NFKC is not in the standard
// library, so this is a hand-rolled fold restricted to the confusable set
// rather than the full normal form. That is the deliberate payment against
// REQ-GO-11 — the alternative is a nested module for one function, and the
// characters below are the ones that actually appear in rendered text.

// confusables maps a character to its ASCII fold. Anything absent is left
// alone: a fold that guessed at unlisted runes would start changing meaning
// rather than presentation.
var confusables = map[rune]string{
	// Quotation marks. The single commonest cause of a failed match: almost
	// every rich-text surface curls quotes on the way out.
	'‘': "'", '’': "'", '‚': "'", '‛': "'",
	'“': `"`, '”': `"`, '„': `"`, '‟': `"`,
	'′': "'", '″': `"`,
	'«': `"`, '»': `"`, '‹': "'", '›': "'",

	// Dashes and minus. A hyphen that became an en dash reads identically at
	// most font sizes.
	'‐': "-", '‑': "-", '‒': "-", '–': "-",
	'—': "-", '―': "-", '−': "-", '﹘': "-",
	'﹣': "-", '－': "-",

	// Spaces. Every one of these renders as a gap and none of them is 0x20.
	// Written as escapes, not literals: a table of invisible characters is
	// unreviewable, and one of them (U+FEFF) is not even legal in Go source.
	'\u00a0': " ", // no-break space
	'\u1680': " ", // ogham space mark
	'\u2000': " ", // en quad
	'\u2001': " ", // em quad
	'\u2002': " ", // en space
	'\u2003': " ", // em space
	'\u2004': " ", // three-per-em
	'\u2005': " ", // four-per-em
	'\u2006': " ", // six-per-em
	'\u2007': " ", // figure space
	'\u2008': " ", // punctuation space
	'\u2009': " ", // thin space
	'\u200a': " ", // hair space
	'\u202f': " ", // narrow no-break
	'\u205f': " ", // medium mathematical
	'\u3000': " ", // ideographic space

	// Zero-width characters fold to NOTHING. They are invisible, so a model
	// transcribing what it saw cannot reproduce them and cannot know they are
	// there.
	'\u200b': "", // zero-width space
	'\u200c': "", // zero-width non-joiner
	'\u200d': "", // zero-width joiner
	'\u2060': "", // word joiner
	'\ufeff': "", // BOM / zero-width no-break space

	'ﬀ': "ff", 'ﬁ': "fi", 'ﬂ': "fl", 'ﬃ': "ffi", 'ﬄ': "ffl",
	'…': "...",
}

// FoldConfusables rewrites a string to its ASCII fold.
//
// Exported because the same key has to be computed on both sides of a match,
// and a caller assembling its own diff wants the identical function rather
// than an approximation of it.
func FoldConfusables(s string) string {
	// Fast path: most content is plain ASCII with no folding to do, and
	// building a new string for it would be pure waste on every edit.
	if isPlainASCII(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if to, ok := confusables[r]; ok {
			b.WriteString(to)
			continue
		}
		// Fullwidth ASCII (FF01..FF5E) maps onto 21..7E. This is the one
		// NFKC range worth folding wholesale rather than listing.
		if r >= '！' && r <= '～' {
			b.WriteRune(r - 0xFEE0)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isPlainASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// FoldLine is the per-line match key: confusables folded, then TRAILING
// whitespace trimmed.
//
// Trailing only. Leading whitespace is indentation and is semantic in Go,
// Python, YAML and Makefiles alike; trimming it would let an edit match a line
// at the wrong nesting level, which is precisely the kind of silent
// mis-application REQ-TOOL-04c's uniqueness rule exists to prevent.
func FoldLine(s string) string {
	return strings.TrimRight(FoldConfusables(s), " \t")
}

// foldLines is FoldLine over a split block.
func foldLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = FoldLine(l)
	}
	return out
}

// findFoldedBlock locates the folded needle inside the folded haystack, at
// line granularity, and reports how many times it occurs.
//
// It returns the FIRST match's line range. The caller rejects on count > 1, so
// the range is only used when the match is unique.
func findFoldedBlock(hay, needle []string) (start, end, count int) {
	if len(needle) == 0 || len(needle) > len(hay) {
		return 0, 0, 0
	}
	first := -1
	for i := 0; i+len(needle) <= len(hay); i++ {
		ok := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		count++
		if first < 0 {
			first = i
		}
	}
	if first < 0 {
		return 0, 0, 0
	}
	return first, first + len(needle), count
}
