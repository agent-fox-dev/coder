package tools

import (
	"fmt"
	"strings"
)

// REQ-TOOL-09's two-limit table. Two limits compose on every result — a
// line/entry limit and a byte limit — and the result records WHICH one fired,
// because "truncated" alone does not tell the model what to change.
const (
	DefaultByteLimit = 50 * 1024 // 50 KB, every tool

	ReadLineLimit   = 2000
	SearchMatchCap  = 100
	SearchLineChars = 500
	FindResultCap   = 1000
	ListEntryCap    = 500

	// The DEFAULTS for find_files and list_files sit well under their caps.
	// A model that asks for "everything" almost never wants a thousand paths
	// in its context; it wants enough to orient and a marker that names the
	// call for more. The PRD's table puts the default at the cap; the cap is
	// kept, and a caller that wants it asks for it (limit=1000 / limit=500).
	FindResultDefault = 200
	ListEntryDefault  = 200
)

// TruncatedBy names which limit fired. It is a closed enum: the 500-char
// per-line cap in search gets its OWN marker rather than a third value, so the
// enum stays closed and a consumer switching on it cannot miss a case
// (ruling P-44).
type TruncatedBy string

const (
	TruncatedByLines TruncatedBy = "lines"
	TruncatedByBytes TruncatedBy = "bytes"
)

// Marker strings. These are MODEL-VISIBLE and therefore part of the contract,
// pinned by golden tests. REQ-TOOL-09b: a truncation the model cannot act on
// costs a turn, so every marker names the exact next call that retrieves the
// remainder.

// ReadMarker is emitted when a read is line-truncated.
//
// The offsets are 1-BASED (ruling P-21). The PRD's parameter table says
// "offset int (default 0)" while its own example marker says "Use offset=2001"
// after showing lines 1-2000 — which is only correct if offsets are 1-based.
// A 0-based reading re-reads line 2000 on every continuation.
func ReadMarker(shown, total int) string {
	return fmt.Sprintf("[Showing lines 1-%d of %d. Use offset=%d to continue.]",
		shown, total, shown+1)
}

// ReadOffsetMarker is the continuation form.
func ReadOffsetMarker(from, to, total int) string {
	if to >= total {
		return fmt.Sprintf("[Showing lines %d-%d of %d.]", from, to, total)
	}
	return fmt.Sprintf("[Showing lines %d-%d of %d. Use offset=%d to continue.]",
		from, to, total, to+1)
}

// FindMarker is emitted when a find hits its result cap.
//
// REQ-TOOL-09b: the marker must name a call that WORKS. The tool clamps
// `limit` to FindResultCap, so "Use limit=2000" at a limit of 1000 names a call
// the tool would clamp straight back to the same result — a truncation the
// model cannot act on, which is the thing the requirement exists to prevent.
// At the cap the marker says so and advises narrowing instead.
func FindMarker(limit int) string {
	return capMarker("results", "limit", limit, FindResultCap,
		"refine the pattern or search a narrower path")
}

// ListMarker is emitted when a listing hits its entry cap.
func ListMarker(limit int) string {
	return capMarker("entries", "limit", limit, ListEntryCap,
		"list a subdirectory, or use find_files with a pattern")
}

// SearchMarker is emitted when a search hits its match cap. It names the
// parameter search_files actually takes, `max_matches`, not find_files'
// `limit` (REQ-TOOL-09b).
func SearchMarker(limit int) string {
	return capMarker("matches", "max_matches", limit, SearchMatchCap,
		"refine the pattern, narrow the path or use file_glob")
}

// SearchBytesMarker is emitted when a search hits the 50 KB byte cap before
// its match cap (REQ-TOOL-09). There is no offset to continue from, so the
// call it names is a narrower one.
func SearchBytesMarker(shown int, limit int) string {
	return fmt.Sprintf("[Showing %d matches; %s limit reached. Refine the pattern, "+
		"narrow the path or file_glob, or reduce context_lines.]", shown, humanBytes(int64(limit)))
}

// capMarker is the shared shape: below the cap it names a larger call, at the
// cap it says the cap is the maximum and names the alternative.
func capMarker(noun, param string, limit, max int, alternative string) string {
	if limit >= max {
		return fmt.Sprintf("[%d %s limit reached, which is the maximum %s; %s]",
			max, noun, param, alternative)
	}
	next := limit * 2
	if next > max {
		next = max
	}
	return fmt.Sprintf("[%d %s limit reached. Use %s=%d for more, or %s]",
		limit, noun, param, next, alternative)
}

// LongLineMarker names a shell workaround for a single line too large to
// return, rather than failing opaquely (REQ-TOOL-09c).
//
// It names `execute` deliberately: `execute` is not path-contained
// (REQ-SEC-01), so it can reach a line — or a spill file — that the file tools
// cannot (ruling P-45).
//
// The path is single-quoted for the shell: a file called `my file.txt` or
// `$HOME.txt` would otherwise name a command that does not read that file.
func LongLineMarker(line int, size int64, limit int, path string) string {
	return fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use execute: sed -n '%dp' %s | head -c %d]",
		line, humanBytes(size), humanBytes(int64(limit)), line, ShellQuote(path), limit)
}

// ShellQuote wraps s in single quotes, the one quoting form under which a
// POSIX shell interprets nothing. An embedded single quote is spelled '\”,
// which closes the quote, escapes one quote, and reopens it.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
