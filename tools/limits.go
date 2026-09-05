package tools

import "fmt"

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
func FindMarker(limit int) string {
	return fmt.Sprintf("[%d results limit reached. Use limit=%d for more, or refine pattern]",
		limit, limit*2)
}

// ListMarker is emitted when a listing hits its entry cap.
func ListMarker(limit int) string {
	return fmt.Sprintf("[%d entries limit reached. Use limit=%d for more]", limit, limit*2)
}

// LongLineMarker names a shell workaround for a single line too large to
// return, rather than failing opaquely (REQ-TOOL-09c).
//
// It names `execute` deliberately: `execute` is not path-contained
// (REQ-SEC-01), so it can reach a line — or a spill file — that the file tools
// cannot (ruling P-45).
func LongLineMarker(line int, size int64, limit int, path string) string {
	return fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use execute: sed -n '%dp' %s | head -c %d]",
		line, humanBytes(size), humanBytes(int64(limit)), line, path, limit)
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
