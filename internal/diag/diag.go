// Package diag holds the non-fatal report type shared by every subsystem that
// loads LOCALLY AUTHORED content — skill manifests, plugin manifests, the TOML
// subset underneath both.
//
// It is separate from the error types those packages return because the
// distinction is load-bearing (REQ-SKILL-10, REQ-PLUGIN-08): authored content
// is decoded leniently, so almost everything that goes wrong is something the
// embedder may want to log rather than something that stops the load. Only
// SeverityError means a thing was not loaded.
package diag

import "fmt"

// Severity distinguishes a diagnostic that skipped something from one that
// rejected it outright.
type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Diagnostic is a non-fatal report from parsing or discovery.
type Diagnostic struct {
	// Path is the file the diagnostic concerns, empty when it is about a byte
	// stream the caller supplied directly.
	Path     string
	Line     int
	Severity Severity
	Message  string
}

func (d Diagnostic) String() string {
	switch {
	case d.Path != "" && d.Line > 0:
		return fmt.Sprintf("%s:%d: %s: %s", d.Path, d.Line, d.Severity, d.Message)
	case d.Path != "":
		return fmt.Sprintf("%s: %s: %s", d.Path, d.Severity, d.Message)
	case d.Line > 0:
		return fmt.Sprintf("line %d: %s: %s", d.Line, d.Severity, d.Message)
	}
	return fmt.Sprintf("%s: %s", d.Severity, d.Message)
}

// BOMPrefix is the UTF-8 byte order mark, written as an escape because a
// literal one in Go source is a compile error.
const BOMPrefix = "\ufeff"
