package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// Call is one provider HTTP request with the whole cross-cutting pipeline
// attached: credential resolution (REQ-AUTH-03), header precedence and
// deletion markers (REQ-SEC-13.4, REQ-AUTH-02), caller transport injection and
// retry bounds (REQ-PROV-18), and the transport retry policy (REQ-PROV-13).
//
// It exists because that pipeline is identical for every wire API and none of
// it is where the interesting differences live. Four copies would be four
// places to forget the deletion marker, and the symptom of forgetting it is a
// 401 from a gateway rather than a test failure.
type Call struct {
	Method string
	URL    string
	Body   []byte
	// Headers are the wire protocol's own defaults (content-type, a version
	// header). They sit at the AUTH layer, so RequestOptions.Headers can still
	// override or suppress them.
	Headers     map[string]string
	Auth        ModelAuth
	Model       *core.Model
	Options     core.RequestOptions
	Attribution *bool
	Env         Env
	Client      *http.Client
	Retry       RetryPolicy
}

func (c Call) Do(ctx context.Context) (*http.Response, error) {
	hc := c.Client
	if t := c.Options.Transport; t != nil {
		// Caller-injected transport wins outright (REQ-PROV-18). This is also
		// the seam every offline test uses.
		hc = &http.Client{Transport: t}
	}

	p := c.Retry
	if v := c.Options.MaxRetries; v != nil {
		p.MaxRetries = *v
	}
	if v := c.Options.MaxRetryDelayMs; v != nil {
		p.MaxRetryDelay = time.Duration(*v) * time.Millisecond
	}

	newReq := func() (*http.Request, error) {
		r, err := http.NewRequest(c.Method, c.URL, bytes.NewReader(c.Body))
		if err != nil {
			return nil, err
		}
		plan := PlanFor(c.Model, c.Auth, c.Options, c.Attribution, c.Env)
		// A COPY: setDefault must not write through to the ModelAuth's map,
		// which is shared across every request of this stream.
		auth := make(map[string]*string, len(plan.Auth)+len(c.Headers))
		for k, v := range plan.Auth {
			auth[k] = v
		}
		for k, v := range c.Headers {
			if _, ok := auth[k]; !ok {
				val := v
				auth[k] = &val
			}
		}
		plan.Auth = auth
		plan.Apply(r)
		return r, nil
	}
	return Do(ctx, hc, newReq, p)
}

// ResolveBaseURL applies the precedence every provider needs: the environment
// override (a corporate gateway) beats the catalog row, which beats the
// provider's compiled-in default.
//
// The environment wins because it is the layer the operator controls without
// rebuilding or re-authoring a catalog.
func ResolveBaseURL(m *core.Model, auth ModelAuth, fallback string) string {
	base := fallback
	if m != nil && m.BaseURL != "" {
		base = m.BaseURL
	}
	if auth.BaseURL != "" {
		base = auth.BaseURL
	}
	return strings.TrimRight(base, "/")
}

// StatusError renders a non-2xx response as text the SEMANTIC retry layer can
// classify (REQ-PROV-14).
//
// The status CODE is always included, because that allowlist matches bare
// "429"/"500"/"503" strings and a gateway returning a 503 with an empty body
// would otherwise produce an unclassifiable message. detail extracts the
// provider's own prose; a nil extractor falls back to the raw body.
func StatusError(prefix string, resp *http.Response, detail func([]byte) string) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := strings.TrimSpace(string(body))
	if detail != nil {
		if s := detail(body); s != "" {
			text = s
		}
	}
	if text == "" {
		text = http.StatusText(resp.StatusCode)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", prefix, resp.StatusCode, text)
}

// JSONErrorDetail is the extractor for the shape almost every vendor uses:
// {"error": {"type": ..., "message": ...}} or {"error": "text"}.
func JSONErrorDetail(body []byte) string {
	var obj struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(obj.Error, &s) == nil {
		return s
	}
	var e struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(obj.Error, &e) != nil {
		return ""
	}
	kind := e.Type
	if kind == "" {
		kind = e.Status
	}
	switch {
	case kind != "" && e.Message != "":
		return kind + ": " + e.Message
	case e.Message != "":
		return e.Message
	}
	return kind
}

// AbortText is the one error string that means "the caller stopped this", and
// it is matched by name rather than by error identity because it also has to
// survive a round trip through an AssistantMessage's ErrorMessage field.
const AbortText = "Request was aborted"

// TransportErrorText renders a transport failure for the semantic classifier,
// preserving the underlying text where "getaddrinfo", "connection reset" and
// "unexpected EOF" live.
//
// It takes BOTH contexts a request runs under, because they mean different
// things when they fire (REQ-PROV-18): caller is the ctx the caller passed to
// Stream, and its cancellation is an abort — terminal, never retried
// (REQ-LOOP-09, REQ-PROV-14). req is the per-request ctx derived from it by
// RequestOptions.TimeoutMs; its deadline expiring with the caller still alive
// is a transient failure the semantic layer MAY retry. A classifier that saw
// only the derived ctx reported every timeout as an abort, which is how a
// slow gateway silently disabled retries.
func TransportErrorText(prefix string, caller, req context.Context, err error) string {
	if text, ok := cancellationText(prefix, caller, req, err); ok {
		return text
	}
	return prefix + ": " + err.Error()
}

// StreamErrorText is TransportErrorText for a failure raised while the
// response body was being decoded. The error text already carries the
// provider's prefix (or is one of the shared sentinels the REQ-PROV-14
// allowlist matches verbatim), so it is passed through unprefixed; only the
// two cancellation channels are classified.
func StreamErrorText(prefix string, caller, req context.Context, err error) string {
	if text, ok := cancellationText(prefix, caller, req, err); ok {
		return text
	}
	return err.Error()
}

func cancellationText(prefix string, caller, req context.Context, err error) (string, bool) {
	if caller.Err() != nil {
		return AbortText, true
	}
	if req.Err() != nil && errors.Is(req.Err(), context.DeadlineExceeded) {
		// "timeout" is on the REQ-PROV-14 allowlist; the wording is what
		// makes the expiry retryable rather than merely reported.
		return prefix + ": request timeout (RequestOptions.TimeoutMs elapsed): " + err.Error(), true
	}
	return "", false
}
