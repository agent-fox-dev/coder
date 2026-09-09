package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// TestBlockedAddressCoversEveryReservedRange is REQ-SEC-05's classification.
//
// The entries are not decoration. 169.254.169.254 is the cloud instance
// metadata endpoint and is the single highest-value target an SSRF has;
// 100.64/10 and 198.18/15 look routable to any check built on
// netip.Addr.IsPrivate, which covers only RFC1918 and fc00::/7.
func TestBlockedAddressCoversEveryReservedRange(t *testing.T) {
	blocked := []struct{ addr, why string }{
		{"127.0.0.1", "loopback"},
		{"::1", "IPv6 loopback"},
		{"0.0.0.0", "unspecified"},
		{"10.0.0.5", "RFC1918"},
		{"172.16.0.1", "RFC1918"},
		{"192.168.1.1", "RFC1918"},
		{"169.254.169.254", "cloud instance metadata"},
		{"fe80::1", "IPv6 link-local"},
		{"fd00::1", "IPv6 unique-local"},
		{"224.0.0.1", "multicast"},
		{"ff02::1", "IPv6 multicast"},
		{"100.64.0.1", "carrier-grade NAT"},
		{"192.0.0.1", "IETF protocol assignments"},
		{"192.0.2.1", "TEST-NET-1"},
		{"198.18.0.1", "benchmarking"},
		{"198.51.100.1", "TEST-NET-2"},
		{"203.0.113.1", "TEST-NET-3"},
		{"240.0.0.1", "reserved"},
		{"255.255.255.255", "broadcast"},
		{"2001:db8::1", "IPv6 documentation"},
		{"0.0.0.1", "this network (0.0.0.0/8)"},
		{"0.255.255.254", "this network (0.0.0.0/8)"},
		{"64:ff9b:1::1", "local-use NAT64"},
		{"::10.0.0.1", "IPv4-compatible IPv6 embedding a private address"},
		{"::169.254.169.254", "IPv4-compatible IPv6 embedding the metadata endpoint"},
		{"::8.8.8.8", "IPv4-compatible IPv6 (deprecated ::/96) even with a public embedded address"},
		{"::1.2.3.4", "IPv4-compatible IPv6"},
	}
	for _, c := range blocked {
		t.Run(c.addr, func(t *testing.T) {
			got, why := BlockedAddress(netip.MustParseAddr(c.addr))
			if !got {
				t.Fatalf("%s (%s) was permitted", c.addr, c.why)
			}
			if why == "" {
				t.Fatal("a refusal must say why, or the operator cannot tell a guard " +
					"from an outage")
			}
		})
	}

	// ::169.254.169.254 says WHY in terms of the embedded address: the
	// low 32 bits are unmapped and classified before the generic reason.
	if _, why := BlockedAddress(netip.MustParseAddr("::169.254.169.254")); !strings.Contains(why, "link-local") {
		t.Fatalf("an IPv4-compatible address must be classified by its embedded IPv4 address: %q", why)
	}

	for _, ok := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:4700::1111", "1::1"} {
		if blocked, why := BlockedAddress(netip.MustParseAddr(ok)); blocked {
			t.Fatalf("%s was refused as %q; a guard that blocks the public internet is "+
				"an outage, not a control", ok, why)
		}
	}
}

// TestIPv4MappedIPv6IsUnmappedBeforeClassification pins the bypass that the
// standard library does NOT close for us.
//
// netip's own predicates already unwrap a 4-in-6 address, so
// `::ffff:127.0.0.1` is IsLoopback and `::ffff:10.0.0.1` is IsPrivate without
// any help. netip.Prefix.Contains does not: a prefix and an address of
// different families never match, so every range in reservedRanges — carrier-
// grade NAT, the benchmarking block, the documentation blocks — is reachable
// through its IPv4-mapped spelling unless the address is unmapped first.
//
// The addresses below are exactly the ones covered by the table and by nothing
// else, which is what makes this test discriminate. A first attempt used
// ::ffff:169.254.169.254, and it passed with the unmapping removed.
func TestIPv4MappedIPv6IsUnmappedBeforeClassification(t *testing.T) {
	tableOnly := []string{
		"::ffff:100.64.0.1",  // carrier-grade NAT
		"::ffff:198.18.0.1",  // benchmarking
		"::ffff:192.0.2.1",   // TEST-NET-1
		"::ffff:203.0.113.1", // TEST-NET-3
		"::ffff:240.0.0.1",   // reserved for future use
	}
	for _, s := range tableOnly {
		a := netip.MustParseAddr(s)
		if blocked, _ := BlockedAddress(a); !blocked {
			t.Fatalf("%s was permitted. netip.Prefix.Contains never matches across "+
				"address families, so this range is reachable through its IPv4-mapped "+
				"spelling unless the address is unmapped before classification.", s)
		}
	}

	// And the ones the standard library already handles, for completeness.
	for _, s := range []string{"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1"} {
		if blocked, _ := BlockedAddress(netip.MustParseAddr(s)); !blocked {
			t.Fatalf("%s was permitted", s)
		}
	}
}

// TestTheGuardRefusesEveryResolvedAddressNotJustTheFirst is the rebinding
// pattern.
//
// A name resolving to one public address and one private one is the attack,
// not a coincidence. "Connect to the first permitted address" hands the
// attacker a retry loop: reorder the answer and the private one is first.
func TestTheGuardRefusesEveryResolvedAddressNotJustTheFirst(t *testing.T) {
	g := &SSRFGuard{
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("169.254.169.254"),
			}, nil
		},
		DialAddr: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("the guard dialled despite a blocked address in the answer")
			return nil, nil
		},
	}
	_, err := g.DialContext(context.Background(), "tcp", "rebind.example:443")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if !strings.Contains(err.Error(), "169.254.169.254") {
		t.Fatalf("the error must name the offending address: %v", err)
	}
}

func TestTheGuardPermitsAPublicAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	var dialled string
	g := &SSRFGuard{
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		DialAddr: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialled = addr
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(srv.URL, "http://"))
		},
	}
	conn, err := g.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("a public address must be permitted: %v", err)
	}
	_ = conn.Close()
	if !strings.HasPrefix(dialled, "93.184.216.34:") {
		t.Fatalf("dialled %q; the guard must dial the address it VALIDATED, not re-resolve "+
			"the name and open a second rebinding window", dialled)
	}
}

// ------------------------------------------------------------------ fetch_url

func fetchThrough(t *testing.T, srv *httptest.Server, opts FetchOptions, args map[string]any) core.ToolResult {
	t.Helper()
	if opts.Guard == nil {
		opts.Guard = &SSRFGuard{
			AllowHTTP: opts.AllowHTTP,
			Resolve: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			},
			DialAddr: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network,
					strings.TrimPrefix(srv.URL, "http://"))
			},
		}
	}
	in, _ := json.Marshal(args)
	return FetchTool(opts).Execute(context.Background(), in)
}

// TestHTTPIsRefusedByDefault is REQ-SEC-09.
func TestHTTPIsRefusedByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	res := fetchThrough(t, srv, FetchOptions{}, map[string]any{"url": "http://example.com/x"})
	if res.OK {
		t.Fatal("http:// must be refused unless AllowHTTP is set")
	}
	if res.Error != "scheme_not_allowed" {
		t.Fatalf("error = %q, want scheme_not_allowed", res.Error)
	}

	res = fetchThrough(t, srv, FetchOptions{AllowHTTP: true},
		map[string]any{"url": "http://example.com/x"})
	if !res.OK {
		t.Fatalf("AllowHTTP must permit it: %s %s", res.Error, res.Detail)
	}
}

func TestNonHTTPSchemesAreRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	for _, u := range []string{"file:///etc/passwd", "gopher://x/", "ftp://x/"} {
		if res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true}, map[string]any{"url": u}); res.OK {
			t.Fatalf("%s was fetched", u)
		}
	}
}

// TestALoopbackURLIsRefusedThroughTheRealGuard uses NO injected resolver: a
// literal 127.0.0.1 must be refused by the shipped path.
func TestALoopbackURLIsRefusedThroughTheRealGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "secret")
	}))
	defer srv.Close()

	in, _ := json.Marshal(map[string]any{"url": srv.URL})
	res := FetchTool(FetchOptions{AllowHTTP: true}).Execute(context.Background(), in)
	if res.OK {
		t.Fatalf("fetched a loopback URL: %v", res.Data)
	}
	if res.Error != "address_not_allowed" {
		t.Fatalf("error = %q, want address_not_allowed", res.Error)
	}
}

// TestARedirectToAPrivateAddressIsBlockedAtTheHop is REQ-TOOL-07's per-hop
// re-validation. The first hop is public and the second is the metadata
// endpoint — which is what an open redirect on a trusted host buys an attacker.
func TestARedirectToAPrivateAddressIsBlockedAtTheHop(t *testing.T) {
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "instance credentials")
	}))
	defer victim.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true},
		map[string]any{"url": "http://example.com/redirect"})
	if res.OK {
		t.Fatalf("followed a redirect to the metadata endpoint: %v", res.Data)
	}
}

// TestARedirectToHTTPIsBlockedWhenHTTPIsNotAllowed is REQ-SEC-09 across a
// redirect: without per-hop scheme re-validation the HTTPS-only promise holds
// for exactly one hop.
//
// The first hop is a REAL TLS server. An earlier version pointed an https URL
// at a plain-HTTP test server, so the request died in the handshake and the
// test passed without the redirect check ever running.
func TestARedirectToHTTPIsBlockedWhenHTTPIsNotAllowed(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/downgraded", http.StatusFound)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	g := &SSRFGuard{
		AllowHTTP:       false,
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		DialAddr: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, host)
		},
	}

	in, _ := json.Marshal(map[string]any{"url": "https://example.com/start"})
	res := FetchTool(FetchOptions{Guard: g}).Execute(context.Background(), in)
	if res.OK {
		t.Fatalf("a redirect from https to http was followed: %v", res.Data)
	}
	if !strings.Contains(res.Detail, "https") {
		t.Fatalf("the refusal must name the scheme rule, not look like a network "+
			"failure: %s / %s", res.Error, res.Detail)
	}
}

func TestRedirectsAreCappedAtFiveHops(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, fmt.Sprintf("http://example.com/%d", hops), http.StatusFound)
	}))
	defer srv.Close()

	res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true},
		map[string]any{"url": "http://example.com/0"})
	if res.OK {
		t.Fatal("an infinite redirect chain must be refused")
	}
	if hops > FetchMaxRedirects+1 {
		t.Fatalf("followed %d hops, want at most %d", hops, FetchMaxRedirects+1)
	}
}

// TestTheResponseIsCappedAt512KB is REQ-TOOL-07's cap, and it asserts the
// property that matters: the client stops READING, it does not read everything
// and then slice.
//
// The server streams 16 MB with no Content-Length and reports how much it
// managed to write. A capped client closes the connection early and the
// server's writes start failing well before the end; an uncapped one drains
// the whole thing into memory and the model's context. Slicing after an
// unbounded read produces an identical `body` and `truncated`, so an assertion
// on those alone cannot tell the two apart — an earlier version of this test
// could not.
func TestTheResponseIsCappedAt512KB(t *testing.T) {
	const total = 16 << 20
	wrote := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("x", 64<<10)
		n := 0
		for n < total {
			c, err := io.WriteString(w, chunk)
			n += c
			if err != nil {
				break
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		wrote <- n
	}))
	defer srv.Close()

	res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true},
		map[string]any{"url": "http://example.com/big"})
	if !res.OK {
		t.Fatalf("fetch failed: %s %s", res.Error, res.Detail)
	}
	body := res.Data["body"].(string)
	if len(body) != FetchResponseCap {
		t.Fatalf("body is %d bytes, want the %d cap", len(body), FetchResponseCap)
	}
	if res.Data["truncated"] != true {
		t.Fatal("truncation must be reported, or the model reasons over a fragment it " +
			"believes is whole")
	}

	select {
	case n := <-wrote:
		// The DISCRIMINATING bound is "not all of it": an uncapped client reads
		// exactly `total`, a capped one stops early. How MUCH earlier depends
		// on socket buffering and on how fast the runner notices the close, so
		// a tight second bound measures the machine rather than the code — an
		// earlier version asserted 4 MiB and flaked under -race at 4.26 MiB.
		if n >= total/2 {
			t.Fatalf("the server got %d of %d bytes out; the client read the whole body "+
				"into memory and only then truncated it", n, total)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server handler never finished")
	}
}

// TestCallerHeadersAreDroppedOnACrossHostRedirect: net/http strips
// Authorization and Cookie by itself, but a caller's own X-Api-Key means
// nothing to it — and an open redirect on a trusted host is how that key
// reaches somebody else's server.
//
// The two hops resolve to two DIFFERENT addresses and dial two different
// servers. An earlier version pointed both at the same one, so the second hop
// never happened and the assertion was skipped entirely.
func TestCallerHeadersAreDroppedOnACrossHostRedirect(t *testing.T) {
	got := make(chan http.Header, 1)
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		fmt.Fprint(w, "ok")
	}))
	defer final.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.example/x", http.StatusFound)
	}))
	defer first.Close()

	const firstIP, secondIP = "93.184.216.34", "93.184.216.35"
	g := &SSRFGuard{
		AllowHTTP: true,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "elsewhere.example" {
				return []netip.Addr{netip.MustParseAddr(secondIP)}, nil
			}
			return []netip.Addr{netip.MustParseAddr(firstIP)}, nil
		},
		DialAddr: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target := first
			if strings.HasPrefix(addr, secondIP+":") {
				target = final
			}
			return (&net.Dialer{}).DialContext(ctx, network,
				strings.TrimPrefix(target.URL, "http://"))
		},
	}

	in, _ := json.Marshal(map[string]any{
		"url":     "http://example.com/start",
		"headers": map[string]string{"X-Api-Key": "super-secret", "Accept": "text/plain"},
	})
	res := FetchTool(FetchOptions{Guard: g, AllowHTTP: true}).Execute(context.Background(), in)
	if !res.OK {
		t.Fatalf("the redirect chain must complete: %s %s", res.Error, res.Detail)
	}

	select {
	case h := <-got:
		if h.Get("X-Api-Key") != "" {
			t.Fatal("a caller-supplied credential header followed a cross-host redirect")
		}
		if h.Get("Accept") != "text/plain" {
			t.Fatalf("Accept = %q; dropping every header would break ordinary content "+
				"negotiation — only the ones that can carry a secret go", h.Get("Accept"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second hop never reached the other server; the test proves nothing")
	}
}

func TestHTMLToTextDropsScriptAndStyleContent(t *testing.T) {
	got := HTMLToText(`<html><head><style>body{color:red}</style>` +
		`<script>var secret = "leak";</script></head>` +
		`<body><h1>Title</h1><p>Some&nbsp;text &amp; more.</p></body></html>`)
	if strings.Contains(got, "color:red") || strings.Contains(got, "secret") {
		t.Fatalf("script/style content survived: %q", got)
	}
	if got != "Title Some text & more." {
		t.Fatalf("text = %q, want %q", got, "Title Some text & more.")
	}
}

func TestFetchIsNotInTheDefaultToolSet(t *testing.T) {
	all, err := All(Options{Workspace: mustWorkspace(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range all {
		if tl.Name == "fetch_url" {
			t.Fatal("REQ-TOOL-07 puts fetch_url OUTSIDE the default set: a tool that " +
				"makes outbound requests on the model's behalf is a different risk " +
				"class from one that reads a file inside a workspace root, and the " +
				"embedder should have to say the word")
		}
	}
}

func mustWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// ---- review fixes

// TestFetchReturnsOnlyCuratedHeadersEachCapped is S2. A CDN response carries
// several kilobytes of headers the model cannot act on; only the ones that
// describe the body come back, and none of those at unbounded length.
func TestFetchReturnsOnlyCuratedHeadersEachCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain")
		h.Set("ETag", strings.Repeat("e", 1000))
		h.Set("Cache-Control", "max-age=60")
		h.Set("Set-Cookie", "session=secret")
		h.Set("X-Powered-By", "something")
		h.Set("Content-Security-Policy", strings.Repeat("default-src 'self'; ", 100))
		_, _ = io.WriteString(w, "body")
	}))
	defer srv.Close()
	res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true}, map[string]any{"url": "http://example.com/"})
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	headers := res.Data["headers"].(map[string]any)
	for _, gone := range []string{"Set-Cookie", "set-cookie", "X-Powered-By", "x-powered-by",
		"Content-Security-Policy", "content-security-policy"} {
		if _, has := headers[gone]; has {
			t.Fatalf("%s reached the model; only the curated set is returned: %v", gone, headers)
		}
	}
	if headers["content-type"] != "text/plain" || headers["cache-control"] != "max-age=60" {
		t.Fatalf("curated headers missing: %v", headers)
	}
	if etag, _ := headers["etag"].(string); len(etag) != fetchHeaderValueCap {
		t.Fatalf("etag is %d bytes; each value is capped at %d", len(etag), fetchHeaderValueCap)
	}
}

// TestFetchRefusesAnOversizedHeaderBlock is S9's transport half: headers are
// read into memory before the body cap applies, so they need their own.
func TestFetchRefusesAnOversizedHeaderBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Filler", strings.Repeat("f", 200<<10))
		_, _ = io.WriteString(w, "body")
	}))
	defer srv.Close()
	res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true}, map[string]any{"url": "http://example.com/"})
	if res.OK {
		t.Fatal("a 200 KB header block was accepted; the transport must cap response headers")
	}
	if got := (&SSRFGuard{}).Transport().MaxResponseHeaderBytes; got != 64<<10 {
		t.Fatalf("MaxResponseHeaderBytes = %d, want 64 KiB", got)
	}
}

// TestFetchDoesNotReturnABinaryBodyAsText is S9. A PDF or an image handed to
// the model as a string is hundreds of kilobytes of mojibake it cannot read.
// The decision is by content type where one is stated and by the bytes where
// it is not; a text/plain that is not UTF-8 is not text either.
func TestFetchDoesNotReturnABinaryBodyAsText(t *testing.T) {
	cases := []struct {
		name, contentType string
		body              []byte
		binary            bool
	}{
		{"octet-stream", "application/octet-stream", []byte("plain ascii"), true},
		{"pdf", "application/pdf", []byte("%PDF-1.4"), true},
		{"png", "image/png", []byte("\x89PNG\r\n\x1a\n"), true},
		{"invalid utf-8 text", "text/plain", []byte("abc\xff\xfe"), true},
		{"json", "application/json", []byte(`{"a":1}`), false},
		{"json suffix", "application/problem+json; charset=utf-8", []byte(`{}`), false},
		{"xml", "application/xml", []byte(`<a/>`), false},
		{"html", "text/html; charset=utf-8", []byte(`<p>hi</p>`), false},
		{"no content type", "", []byte("just bytes"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.contentType != "" {
					w.Header().Set("Content-Type", c.contentType)
				} else {
					// Go sniffs a type for an unset header; an explicit empty
					// value is what "the server said nothing" looks like.
					w.Header()["Content-Type"] = nil
				}
				_, _ = w.Write(c.body)
			}))
			defer srv.Close()
			res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true}, map[string]any{"url": "http://example.com/x"})
			if !res.OK {
				t.Fatalf("%+v", res)
			}
			_, hasBody := res.Data["body"]
			if c.binary {
				if hasBody || res.Data["binary"] != true || res.Data["bytes"] != len(c.body) {
					t.Fatalf("a binary body must come back as {binary:true, bytes:N} with no body: %+v", res.Data)
				}
				if res.Data["content_type"] != c.contentType {
					t.Fatalf("content_type = %v", res.Data["content_type"])
				}
			} else if !hasBody || res.Data["body"] != string(c.body) {
				t.Fatalf("a text body must come back as text: %+v", res.Data)
			}
		})
	}
}

// TestATruncatedBodyIsCutOnARuneBoundary: the 512 KB cut can land inside a
// multi-byte character, which would make a text body look binary.
func TestATruncatedBodyIsCutOnARuneBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// "é" is two bytes; an odd number of leading ASCII bytes puts the cap
		// mid-rune.
		_, _ = io.WriteString(w, "a"+strings.Repeat("é", FetchResponseCap))
	}))
	defer srv.Close()
	res := fetchThrough(t, srv, FetchOptions{AllowHTTP: true}, map[string]any{"url": "http://example.com/"})
	if !res.OK || res.Data["truncated"] != true {
		t.Fatalf("%+v", res)
	}
	body, ok := res.Data["body"].(string)
	if !ok || res.Data["binary"] == true {
		t.Fatalf("a truncated UTF-8 body is still text: %+v", res.Data)
	}
	if len(body) != FetchResponseCap-1 || !strings.HasSuffix(body, "é") {
		t.Fatalf("body is %d bytes; the cut must drop the partial rune at the cap", len(body))
	}
}

// TestDropElementMatchesWholeTagNamesInOnePass is S6b. `<scripts>` is not
// `<script>`, and a page of many inline scripts must not cost quadratic time.
func TestDropElementMatchesWholeTagNamesInOnePass(t *testing.T) {
	in := `<scripts>keep one</scripts><SCRIPT type="x">drop</SCRIPT>` +
		`<script>drop too</script ><scriptlet>keep two</scriptlet><script>unclosed drop`
	if got := HTMLToText(in); got != "keep one keep two" {
		t.Fatalf("HTMLToText = %q, want %q", got, "keep one keep two")
	}

	var b strings.Builder
	for b.Len() < FetchResponseCap {
		b.WriteString("<script>x</script>t ")
	}
	start := time.Now()
	got := HTMLToText(b.String())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stripping %d scripts from 512 KB took %v; dropElement must be one forward pass", FetchResponseCap/20, elapsed)
	}
	if strings.Contains(got, "x") || !strings.HasPrefix(got, "t t t") {
		t.Fatalf("scripts survived or text was lost: %.40q", got)
	}
}
