package mcp

import (
	"fmt"
	"strings"
)

// ResourceTemplate is a parameterised resource (REQ-MCP-SERVER-05).
//
// The requirement's own examples are templated —
// `nightshift://issues/{number}/triage-report`,
// `nightshift://sessions/{id}/audit-log` — so exact-URI registration cannot
// express them at all. A host would otherwise have to enumerate every issue it
// has ever seen as a separate resource.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitzero"`
	Description string `json:"description,omitzero"`
	MimeType    string `json:"mimeType,omitzero"`
}

func (t ResourceTemplate) Validate() error {
	if t.URITemplate == "" {
		return errMissing("uriTemplate")
	}
	return nil
}

// ResourceTemplatesListResult answers resources/templates/list.
type ResourceTemplatesListResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitzero"`
}

// uriPattern is a compiled template.
type uriPattern struct {
	// parts alternate literal, variable, literal, ... A variable part has a
	// non-empty name.
	parts []patternPart
	vars  []string
}

type patternPart struct {
	literal string
	name    string
	// greedy marks {+var}, RFC 6570's reserved expansion, which may span '/'.
	// Plain {var} may not: a template like `x://{a}/{b}` with a
	// slash-swallowing first variable matches everything with the second
	// variable empty, which is not a match, it is a bug that looks like one.
	greedy bool
}

// compileTemplate parses `scheme://a/{var}/b` and `{+path}`.
func compileTemplate(tmpl string) (*uriPattern, error) {
	p := &uriPattern{}
	rest := tmpl
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			if rest != "" {
				p.parts = append(p.parts, patternPart{literal: rest})
			}
			break
		}
		close := strings.IndexByte(rest[open:], '}')
		if close < 0 {
			return nil, fmt.Errorf("mcp: unterminated variable in template %q", tmpl)
		}
		close += open

		if open > 0 {
			p.parts = append(p.parts, patternPart{literal: rest[:open]})
		}
		name := rest[open+1 : close]
		greedy := false
		if strings.HasPrefix(name, "+") {
			greedy, name = true, name[1:]
		}
		if name == "" {
			return nil, fmt.Errorf("mcp: empty variable name in template %q", tmpl)
		}
		p.parts = append(p.parts, patternPart{name: name, greedy: greedy})
		p.vars = append(p.vars, name)
		rest = rest[close+1:]
	}
	if len(p.vars) == 0 {
		return nil, fmt.Errorf("mcp: template %q has no variables; register it as a "+
			"plain resource instead", tmpl)
	}
	return p, nil
}

// match extracts the variables from a concrete URI, or reports no match.
//
// Matching is LEFT TO RIGHT against the following literal, so a variable is
// bounded by what comes after it rather than by a regexp's backtracking. A
// variable may not be empty: `x://issues//report` is not an issue whose number
// is the empty string, and treating it as one hands a handler an id it will
// look up and fail on somewhere less obvious.
func (p *uriPattern) match(uri string) (map[string]string, bool) {
	vars := make(map[string]string, len(p.vars))
	rest := uri

	for i, part := range p.parts {
		if part.name == "" {
			if !strings.HasPrefix(rest, part.literal) {
				return nil, false
			}
			rest = rest[len(part.literal):]
			continue
		}

		// Find where this variable ends: at the next literal, or at the end.
		var value string
		if i+1 < len(p.parts) {
			next := p.parts[i+1].literal
			idx := strings.Index(rest, next)
			if idx < 0 {
				return nil, false
			}
			value, rest = rest[:idx], rest[idx:]
		} else {
			value, rest = rest, ""
		}
		if value == "" {
			return nil, false
		}
		if !part.greedy && strings.Contains(value, "/") {
			return nil, false
		}
		vars[part.name] = value
	}
	if rest != "" {
		return nil, false
	}
	return vars, true
}
