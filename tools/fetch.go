package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

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

// fetchHeaderValueCap bounds each returned header value. The headers that
// reach the model are a curated set (see returnedHeaders), and even those are
// capped: a Location or Cache-Control of several kilobytes is a payload, not
// metadata.
const fetchHeaderValueCap = 256

// returnedHeaders is the set of response headers the tool hands to the model.
// Everything else — cookies, CSP policies, server fingerprints, the several
// kilobytes of tracking headers a CDN adds — is dropped: the model cannot act
// on them, they cost context on every fetch, and a server that wants to feed
// the model text has the body for that.
var returnedHeaders = []string{
	"content-type", "content-length", "location", "last-modified", "etag", "cache-control",
}

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

			contentType := resp.Header.Get("Content-Type")
			headers := map[string]any{}
			for _, k := range returnedHeaders {
				if v := resp.Header.Get(k); v != "" {
					if len(v) > fetchHeaderValueCap {
						v = v[:fetchHeaderValueCap]
					}
					headers[k] = v
				}
			}
			data := map[string]any{
				"status":       resp.StatusCode,
				"url":          resp.Request.URL.String(),
				"content_type": contentType,
				"headers":      headers,
				"truncated":    truncated,
			}
			r := core.OKResult(data)
			r.Metadata = &core.ToolMetadata{
				Truncated: truncated, TotalBytes: int64(len(raw)),
				DurationMS: time.Since(start).Milliseconds(),
			}
			if truncated {
				r.Metadata.TruncatedBy = string(TruncatedByBytes)
				// The cut can land inside a multi-byte character; the partial
				// rune is dropped so a truncated text body is still text.
				raw = trimIncompleteRune(raw)
			}

			// A body that is not text is not returned as text. An image, a
			// zip or a PDF handed to the model as a string is several hundred
			// KB of mojibake it cannot read; it is told what arrived and how
			// big it was instead. The decision is made on the content type
			// where the server states one, and on the bytes where it does not
			// — a text/plain that is not valid UTF-8 is not text either.
			if !textualContentType(contentType) || !utf8.Valid(raw) {
				data["binary"] = true
				data["bytes"] = len(raw)
				return r
			}
			text := string(raw)
			if a.AsText && strings.Contains(strings.ToLower(contentType), "html") {
				text = HTMLToText(text)
			}
			data["body"] = text
			return r
		},
	}
}

// textualContentType reports whether a Content-Type names something the model
// can read as text. An absent or unparseable type is treated as textual and
// left to the UTF-8 check.
func textualContentType(ct string) bool {
	if strings.TrimSpace(ct) == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return true
	}
	typ, sub, _ := strings.Cut(strings.ToLower(mt), "/")
	switch {
	case typ == "text":
		return true
	case strings.HasSuffix(sub, "+json"), strings.HasSuffix(sub, "+xml"):
		return true
	}
	switch sub {
	case "json", "xml", "javascript", "ecmascript", "x-www-form-urlencoded",
		"x-ndjson", "ld+json", "graphql", "yaml", "x-yaml", "toml", "sql":
		return typ == "application"
	}
	return false
}

// trimIncompleteRune drops a trailing partial UTF-8 sequence left by a byte
// cut, so a body truncated at exactly the cap is still valid text.
func trimIncompleteRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if !utf8.FullRune(b[i:]) {
			return b[:i]
		}
		break
	}
	return b
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

// dropElement removes an element and its content, case-insensitively, in ONE
// forward pass over the input.
//
// The previous version re-lowercased and re-spliced the whole string per
// element removed, which on a page with a few thousand inline scripts is
// quadratic in the 512 KB the tool allows. It also matched `<script` as a
// prefix, so `<scripts>` and `<scriptlet>` were dropped too; the tag name
// must now END where a name ends — at whitespace, `>` or `/`.
func dropElement(s, tag string) string {
	open, closing := "<"+tag, "</"+tag
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if !tagAt(s, i, open) {
			b.WriteByte(s[i])
			i++
			continue
		}
		// Inside the element: scan forward to its closing tag.
		end := -1
		for j := i + len(open); j+len(closing) <= len(s); j++ {
			if s[j] == '<' && tagAt(s, j, closing) {
				end = len(s)
				if k := strings.IndexByte(s[j:], '>'); k >= 0 {
					end = j + k + 1
				}
				break
			}
		}
		if end < 0 {
			break // unclosed: the element runs to the end of the input
		}
		i = end
	}
	return b.String()
}

// tagAt reports whether s[i:] starts with tag (case-insensitively) as a WHOLE
// tag name, i.e. followed by whitespace, `>`, `/` or the end of input.
func tagAt(s string, i int, tag string) bool {
	if len(s)-i < len(tag) || !strings.EqualFold(s[i:i+len(tag)], tag) {
		return false
	}
	if i+len(tag) == len(s) {
		return true
	}
	switch s[i+len(tag)] {
	case ' ', '\t', '\n', '\r', '\f', '>', '/':
		return true
	}
	return false
}
