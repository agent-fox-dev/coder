package tools

import (
	"path/filepath"
	"strings"
)

// MatchGlob matches a path against a glob pattern.
//
// The dialect is a documented subset, not "whatever filepath.Match does". The
// PRD's fallback for the non-ripgrep path is "the Go standard library regexp
// package", which is not a fallback at all: filepath.Match cannot cross `/`
// with `**`, has no brace expansion, and does not do smart-case. A find tool
// that silently returned node_modules would be worse than no find tool.
//
// Implemented:
//
//	**        crosses directory separators
//	*         matches within one segment
//	?         one character within a segment
//	[abc]     character class, [!abc] negated
//	{a,b}     brace expansion, nested
//	smart-case: an all-lowercase pattern matches case-insensitively
//
// NOT implemented in v1, and named here so the gap is visible rather than
// discovered: extglob (`+(a|b)`), `**` as a bare path component with implicit
// descent semantics differing between fd and rg, and character equivalence
// classes.
func MatchGlob(pattern, path string) bool {
	path = filepath.ToSlash(path)
	smart := pattern == strings.ToLower(pattern)
	for _, p := range ExpandBraces(pattern) {
		hay, needle := path, p
		if smart {
			hay = strings.ToLower(hay)
		}
		if matchSegments(needle, hay) {
			return true
		}
		// A bare pattern with no separator also matches by basename, which is
		// what a user typing `*.go` means.
		if !strings.Contains(needle, "/") && matchSegments(needle, baseOf(hay)) {
			return true
		}
	}
	return false
}

func baseOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ExpandBraces expands {a,b} alternations, including nested ones.
func ExpandBraces(p string) []string {
	start := strings.IndexByte(p, '{')
	if start < 0 {
		return []string{p}
	}
	depth, end := 0, -1
	for i := start; i < len(p); i++ {
		switch p[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return []string{p} // unbalanced: treat literally
	}

	// Split the alternation on top-level commas only.
	body := p[start+1 : end]
	var alts []string
	d, last := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			d++
		case '}':
			d--
		case ',':
			if d == 0 {
				alts = append(alts, body[last:i])
				last = i + 1
			}
		}
	}
	alts = append(alts, body[last:])

	var out []string
	for _, a := range alts {
		for _, rest := range ExpandBraces(p[:start] + a + p[end+1:]) {
			out = append(out, rest)
		}
	}
	return out
}

// matchSegments matches with `**` crossing separators.
func matchSegments(pattern, name string) bool {
	// Fast path: no `**`.
	if !strings.Contains(pattern, "**") {
		ps := strings.Split(pattern, "/")
		ns := strings.Split(name, "/")
		if len(ps) != len(ns) {
			return false
		}
		for i := range ps {
			if !matchOne(ps[i], ns[i]) {
				return false
			}
		}
		return true
	}
	return matchDoubleStar(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchDoubleStar(ps, ns []string) bool {
	switch {
	case len(ps) == 0:
		return len(ns) == 0
	case ps[0] == "**":
		// `**` matches zero or more segments.
		for i := 0; i <= len(ns); i++ {
			if matchDoubleStar(ps[1:], ns[i:]) {
				return true
			}
		}
		return false
	case len(ns) == 0:
		return false
	case !matchOne(ps[0], ns[0]):
		return false
	}
	return matchDoubleStar(ps[1:], ns[1:])
}

// matchOne matches a single segment, supporting * ? and [class].
//
// The pattern is translated first: the glob dialect specified for find/search
// negates a character class with [!x], as gitignore and ripgrep do, while Go's
// filepath.Match uses [^x] and treats a leading ! as a literal. Without the
// translation, [!x]*.go silently matches nothing that starts with x — and
// silently matches a literal "!" — which is the kind of near-miss a find tool
// can carry for a long time before anyone notices.
func matchOne(pattern, s string) bool {
	ok, err := filepath.Match(translateClasses(pattern), s)
	return err == nil && ok
}

// translateClasses rewrites [! into [^ at the start of a character class only,
// leaving a ! anywhere else alone.
func translateClasses(p string) string {
	if !strings.Contains(p, "[!") {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			b.WriteByte(c)
			i++
			b.WriteByte(p[i])
			continue
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
			if i+1 < len(p) && p[i+1] == '!' {
				b.WriteByte('^')
				i++
			}
			continue
		case c == ']' && inClass:
			inClass = false
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ---------------------------------------------------------------- ignore
