package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// FetchResponseCap is REQ-TOOL-07's 512 KB response cap.
//
// It is enforced with a LimitReader plus a one-byte probe rather than by
// trusting Content-Length: a server that lies about the length, or sends none
// at all under chunked encoding, would otherwise stream unbounded bytes into
// the model's context and the process's memory.
const FetchResponseCap = 512 << 10

// FetchMaxRedirects is REQ-TOOL-07's 5-hop limit.
const FetchMaxRedirects = 5

// FetchOptions configures the fetch_url tool.
type FetchOptions struct {
	// AllowHTTP is REQ-SEC-09's opt-in (`tools.allow_http`). Off by default.
	AllowHTTP bool
	// Guard is the SSRF guard. Nil builds one from AllowHTTP.
	Guard *SSRFGuard
	// DefaultTimeout applies when the call supplies no timeout_s.
	DefaultTimeout time.Duration
}

// FetchTool is REQ-TOOL-07.
//
// It is NOT in the default set (see All): an embedder registers it through
// ToolPolicy.CustomTools. That placement is the requirement, and the reason is
// that a tool which makes outbound requests on the model's behalf is a
// different risk class from one that reads a file inside a workspace root.
//
// REQ-SEC-01's path containment does not apply here and neither does the
// workspace root; the boundary for this tool is the SSRF guard and the scheme
// check, and the interceptor of REQ-SEC-03 above them.
func FetchTool(opts FetchOptions) core.Tool {
	guard := opts.Guard
	if guard == nil {
		guard = &SSRFGuard{AllowHTTP: opts.AllowHTTP}
	}
	timeout := opts.DefaultTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	client := &http.Client{
		Transport: guard.Transport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= FetchMaxRedirects {
				return fmt.Errorf("tools: stopped after %d redirects", FetchMaxRedirects)
			}
			if err := checkScheme(req.URL, guard.AllowHTTP); err != nil {
				// Per-hop scheme re-validation. Without it an https URL
				// redirects to http and the guard's HTTPS-only promise holds
				// for exactly one hop.
				return err
			}
			// Caller-supplied headers are DROPPED on a cross-host redirect.
			// net/http already strips Authorization and Cookie, but a caller's
			// own X-Api-Key is not sensitive to it — and an open redirect is
			// how that key reaches somebody else's server.
			if via[0].URL.Host != req.URL.Host {
				for name := range req.Header {
					if !hopSafeHeader(name) {
						req.Header.Del(name)
					}
				}
			}
			return nil
		},
	}

	return core.Tool{
		Name: "fetch_url",
		Description: "Fetch a URL over HTTPS and return its body. Private, loopback, " +
			"link-local and reserved addresses are refused.",
		Builtin: true,
		InputSchema: schema.Object(
			schema.Prop("url", schema.String("Absolute https:// URL")),
			schema.Opt("method", schema.Enum("HTTP method", "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE")),
			schema.Opt("headers", schema.Object().Describe("Request headers")),
			schema.Opt("body", schema.String("Request body")),
			schema.Opt("timeout_s", schema.Int("Request timeout in seconds")),
			schema.Opt("as_text", schema.Bool("Extract readable text from an HTML response")),
		),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				URL      string            `json:"url"`
				Method   string            `json:"method"`
				Headers  map[string]string `json:"headers"`
				Body     string            `json:"body"`
				TimeoutS int               `json:"timeout_s"`
				AsText   bool              `json:"as_text"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}

			u, err := url.Parse(a.URL)
			if err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if err := checkScheme(u, guard.AllowHTTP); err != nil {
				return core.ErrResult("scheme_not_allowed", err.Error())
			}

			method := strings.ToUpper(a.Method)
			if method == "" {
				method = http.MethodGet
			}

			d := timeout
			if a.TimeoutS > 0 {
				d = time.Duration(a.TimeoutS) * time.Second
			}
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()

			var body io.Reader
			if a.Body != "" {
				body = strings.NewReader(a.Body)
			}
			req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
			if err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			for k, v := range a.Headers {
				req.Header.Set(k, v)
			}

			start := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				if errors.Is(err, ErrBlockedAddress) {
					return core.ErrResult("address_not_allowed", err.Error())
				}
				if errors.Is(err, ErrSchemeNotAllowed) {
					return core.ErrResult("scheme_not_allowed", err.Error())
				}
				if ctx.Err() != nil {
					return core.ErrResult("timeout", err.Error())
				}
				return core.ErrResult("request_failed", err.Error())
			}
			defer resp.Body.Close()

			// Read one byte past the cap so truncation is DETECTED rather than
			// inferred from a body that happens to be exactly 512 KB.
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, FetchResponseCap+1))
			if rerr != nil {
				return core.ErrResult("request_failed", rerr.Error())
			}
			truncated := len(raw) > FetchResponseCap
			if truncated {
				raw = raw[:FetchResponseCap]
			}

			text := string(raw)
			contentType := resp.Header.Get("Content-Type")
			if a.AsText && strings.Contains(strings.ToLower(contentType), "html") {
				text = HTMLToText(text)
			}

			headers := map[string]any{}
			for k, v := range resp.Header {
				headers[k] = strings.Join(v, ", ")
			}
			data := map[string]any{
				"status":       resp.StatusCode,
				"url":          resp.Request.URL.String(),
				"content_type": contentType,
				"headers":      headers,
				"body":         text,
				"truncated":    truncated,
			}
			r := core.OKResult(data)
			r.Metadata = &core.ToolMetadata{
				Truncated: truncated, TotalBytes: int64(len(raw)),
				DurationMS: time.Since(start).Milliseconds(),
			}
			if truncated {
				r.Metadata.TruncatedBy = string(TruncatedByBytes)
			}
			return r
		},
	}
}

func checkScheme(u *url.URL, allowHTTP bool) error {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if allowHTTP {
			return nil
		}
		return fmt.Errorf("%w (got %s)", ErrSchemeNotAllowed, u)
	}
	return fmt.Errorf("%w (got scheme %q)", ErrSchemeNotAllowed, u.Scheme)
}

// hopSafeHeader names the headers that may survive a cross-host redirect.
func hopSafeHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept", "Accept-Encoding", "Accept-Language", "User-Agent", "Content-Type":
		return true
	}
	return false
}

// HTMLToText is REQ-TOOL-07's optional extraction.
//
// It is a deliberately small tag stripper, not a parser: script and style
// CONTENT is dropped, tags are removed, a handful of entities are decoded and
// whitespace is collapsed. A real extractor needs a dependency, and REQ-GO-11
// makes that a decision rather than a default — so this is documented as
// approximate rather than sold as readability extraction.
func HTMLToText(s string) string {
	s = dropElement(s, "script")
	s = dropElement(s, "style")

	var b strings.Builder
	inTag := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '<':
			inTag = true
		case s[i] == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteByte(s[i])
		}
	}

	out := b.String()
	for _, e := range [][2]string{
		{"&nbsp;", " "}, {"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"},
		{"&quot;", "\""}, {"&#39;", "'"}, {"&apos;", "'"},
	} {
		out = strings.ReplaceAll(out, e[0], e[1])
	}
	return strings.Join(strings.Fields(out), " ")
}

// dropElement removes an element and its content, case-insensitively.
func dropElement(s, tag string) string {
	lower := strings.ToLower(s)
	open, closing := "<"+tag, "</"+tag
	for {
		i := strings.Index(lower, open)
		if i < 0 {
			return s
		}
		j := strings.Index(lower[i:], closing)
		if j < 0 {
			return s[:i]
		}
		end := i + j
		if k := strings.Index(lower[end:], ">"); k >= 0 {
			end += k + 1
		}
		s = s[:i] + s[end:]
		lower = strings.ToLower(s)
	}
}
