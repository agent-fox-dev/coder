package plugins

import (
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/internal/toml"
)

// ParseConfig reads the `[plugins]` section of config.toml: `paths`
// (REQ-PLUGIN-05) and `disabled` (REQ-PLUGIN-07).
//
// Config is LOCALLY AUTHORED, so it decodes leniently (REQ-SEC-12.5): an
// unknown key or a value of the wrong type is a diagnostic and the section
// still loads. The opposite of the wire package's rule, and the difference is
// who wrote the bytes. It mirrors mcp.ParseConfig so the two sections of one
// file are read by one parser with one set of rules.
func ParseConfig(path string, src []byte) (Config, []Diagnostic, error) {
	root, diags, err := toml.ParseTOML(src)
	if err != nil {
		return Config{}, diags, fmt.Errorf("plugins: %s: %w", path, err)
	}

	var cfg Config
	tbl, ok := root.Sub("plugins")
	if !ok {
		// No section is the ordinary case for an embedder with no plugins;
		// it is not a diagnostic.
		return cfg, diags, nil
	}

	list := func(key string) []string {
		v, ok := tbl.Get(key)
		if !ok {
			return nil
		}
		if v.Kind != toml.KindStringArray {
			diags = append(diags, Diagnostic{Path: path, Line: v.Line, Severity: SeverityWarning,
				Message: fmt.Sprintf("[plugins] %s must be an array of strings; ignored", key)})
			return nil
		}
		var out []string
		for _, s := range v.Array {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	cfg.Paths = list("paths")
	cfg.Disabled = list("disabled")

	for _, k := range tbl.Keys() {
		switch k {
		case "paths", "disabled":
		default:
			diags = append(diags, Diagnostic{Path: path, Severity: SeverityWarning,
				Message: fmt.Sprintf("[plugins] unknown key %q; known keys are paths, disabled", k)})
		}
	}
	return cfg, diags, nil
}
