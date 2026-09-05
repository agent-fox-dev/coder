package provider_test

import (
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

func envOf(pairs map[string]string) provider.Env {
	return provider.Env{Getenv: func(k string) string { return pairs[k] }}
}

var anthropicTable = provider.VendorAuth{
	Vars: []provider.EnvVar{
		{Name: "ANTHROPIC_AUTH_TOKEN", Scheme: provider.SchemeBearer},
		{Name: "ANTHROPIC_OAUTH_TOKEN", Scheme: provider.SchemeBearer},
		{Name: "ANTHROPIC_API_KEY", Scheme: provider.SchemeAPIKey},
	},
	BaseURLVar: "ANTHROPIC_BASE_URL",
}

// TestAuthTokenIsSentAsBearerNotAsAPIKey is the reason REQ-AUTH-03 is an
// ORDERED TABLE and not a `<VENDOR>_API_KEY` convention.
//
// ANTHROPIC_AUTH_TOKEN and ANTHROPIC_API_KEY carry the credential under
// DIFFERENT headers. Sending either under the other's name is a 401 whose body
// says nothing about which variable was picked, so the failure reads as "my
// key is wrong" and the key is fine.
func TestAuthTokenIsSentAsBearerNotAsAPIKey(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		wantHeader string
		wantValue  string
		wantSource string
	}{
		{"auth token wins and rides as Bearer",
			map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_API_KEY": "key"},
			"Authorization", "Bearer tok", "ANTHROPIC_AUTH_TOKEN"},
		{"oauth token outranks the api key",
			map[string]string{"ANTHROPIC_OAUTH_TOKEN": "oa", "ANTHROPIC_API_KEY": "key"},
			"Authorization", "Bearer oa", "ANTHROPIC_OAUTH_TOKEN"},
		{"api key rides as x-api-key",
			map[string]string{"ANTHROPIC_API_KEY": "key"},
			"x-api-key", "key", "ANTHROPIC_API_KEY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := provider.ResolveAuth(anthropicTable, envOf(c.env))
			if a.State != provider.CredentialResolved {
				t.Fatalf("state = %v, want resolved", a.State)
			}
			if a.Source != c.wantSource {
				t.Fatalf("source = %q, want %q", a.Source, c.wantSource)
			}
			got, ok := a.Headers[c.wantHeader]
			if !ok || got == nil || *got != c.wantValue {
				t.Fatalf("headers = %v, want %s: %q", a.Headers, c.wantHeader, c.wantValue)
			}
		})
	}
}

// TestAnEmptyOverrideFallsThroughRatherThanMasking pins the half-sentence in
// REQ-AUTH-03 that is easy to read past.
//
// A caller building a scoped override map from a partially-filled config
// writes "" for the variables it has no value for. If an empty override
// masked, that blanks out a perfectly good ambient key and the resulting 401
// names no cause.
func TestAnEmptyOverrideFallsThroughRatherThanMasking(t *testing.T) {
	env := provider.Env{
		Override: map[string]string{"ANTHROPIC_API_KEY": ""},
		Getenv:   func(k string) string { return map[string]string{"ANTHROPIC_API_KEY": "real"}[k] },
	}
	a := provider.ResolveAuth(anthropicTable, env)
	if a.APIKey != "real" {
		t.Fatalf("key = %q, want the process value: an EMPTY override falls through", a.APIKey)
	}
}

func TestOverrideIsConsultedBeforeGetenv(t *testing.T) {
	env := provider.Env{
		Override: map[string]string{"ANTHROPIC_API_KEY": "scoped"},
		Getenv:   func(k string) string { return map[string]string{"ANTHROPIC_API_KEY": "process"}[k] },
	}
	if a := provider.ResolveAuth(anthropicTable, env); a.APIKey != "scoped" {
		t.Fatalf("key = %q, want the scoped override", a.APIKey)
	}
}

// TestAmbientIsDistinctFromNone is REQ-AUTH-04's third state.
//
// A deployment on an instance role has no key the SDK can read and a transport
// that will authenticate anyway. Two-valued credential state fails it at a
// pre-flight check that a plain key would have passed, and blames the wrong
// thing.
func TestAmbientIsDistinctFromNone(t *testing.T) {
	table := anthropicTable
	table.Ambient = func(e provider.Env) bool { return e.Has("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") }

	none := provider.ResolveAuth(table, envOf(nil))
	if none.State != provider.CredentialNone {
		t.Fatalf("state = %v with nothing set, want none", none.State)
	}
	amb := provider.ResolveAuth(table, envOf(map[string]string{
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/creds"}))
	if amb.State != provider.CredentialAmbient {
		t.Fatalf("state = %v with an instance credential chain, want ambient: a bool "+
			"cannot express this and every ADC deployment fails pre-flight", amb.State)
	}
	if amb.APIKey != "" {
		t.Fatal("an ambient credential is not readable; APIKey must stay empty")
	}
}

func TestBaseURLAloneIsConfigurationNotACredential(t *testing.T) {
	a := provider.ResolveAuth(anthropicTable, envOf(map[string]string{
		"ANTHROPIC_BASE_URL": "https://gw.example.com/"}))
	if a.BaseURL != "https://gw.example.com" {
		t.Fatalf("base url = %q, want the trailing slash trimmed", a.BaseURL)
	}
	if a.State != provider.CredentialAmbient {
		t.Fatalf("state = %v, want ambient: a gateway URL means the vendor is configured "+
			"and the credential is not ours to read", a.State)
	}
}

// TestCredentialsCannotBePrintedByAccident is NFR-SEC-01 at the boundary
// REQ-AUTH-07 names.
//
// The ways a credential actually escapes are %v in a log line, %w in an error
// and a struct dump in a test failure. A redaction helper nobody remembers to
// call prevents none of them; a String method on the type that holds the
// credential prevents all three.
func TestCredentialsCannotBePrintedByAccident(t *testing.T) {
	a := provider.ResolveAuth(anthropicTable, envOf(map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-super-secret-value-1234"}))

	dump := a.String()
	if strings.Contains(dump, "super-secret") {
		t.Fatalf("ModelAuth.String() leaked the credential: %s", dump)
	}
	if !strings.Contains(dump, "sk-a") || !strings.Contains(dump, "1234") {
		t.Fatalf("redaction must keep the first and last 4 characters for "+
			"recognisability: %s", dump)
	}
}

func TestShortSecretsAreRedactedEntirely(t *testing.T) {
	// The literal "first 4 and last 4" rule leaks a 9-character secret down to
	// one unknown character. The point of the rule is recognisability, and a
	// secret too short to be recognised is redacted whole.
	if got := provider.Redact("abcdefghi"); got != "***" {
		t.Fatalf("Redact(9 chars) = %q, want ***", got)
	}
	if got := provider.Redact(""); got != "" {
		t.Fatalf("Redact(\"\") = %q, want empty", got)
	}
}

// ---------------------------------------------------------------- headers

// TestNilHeaderValueSuppressesAProviderDefault is REQ-AUTH-02's third state,
// and the case it exists for.
//
// A gateway that authenticates with its own header needs the upstream
// x-api-key turned OFF. No string value expresses that: "" sends an empty
// header, which is not the same as sending none.
func TestNilHeaderValueSuppressesAProviderDefault(t *testing.T) {
	key := "sk-live"
	plan := provider.HeaderPlan{
		Auth:    map[string]*string{"x-api-key": &key},
		Request: map[string]*string{"x-api-key": nil}, // deletion marker
	}
	if v, ok := plan.Merge()["X-Api-Key"]; ok {
		t.Fatalf("x-api-key = %q; a present-nil at a higher layer must SUPPRESS the "+
			"provider default, not be ignored", v)
	}
}

// TestHeaderPrecedenceIsLowestToHighest pins REQ-SEC-13.4's four layers.
func TestHeaderPrecedenceIsLowestToHighest(t *testing.T) {
	s := func(v string) *string { return &v }
	plan := provider.HeaderPlan{
		Attribution: map[string]*string{"x-thing": s("attribution"), "x-attr-only": s("kept")},
		Auth:        map[string]*string{"x-thing": s("auth")},
		Model:       map[string]*string{"x-thing": s("model")},
		Request:     map[string]*string{"x-thing": s("request")},
	}
	got := plan.Merge()
	if got["X-Thing"] != "request" {
		t.Fatalf("x-thing = %q, want the caller's value to win over model, auth and "+
			"attribution", got["X-Thing"])
	}
	if got["X-Attr-Only"] != "kept" {
		t.Fatal("a lower layer's header nobody overrode must survive the merge")
	}
}

// TestALowLayerNilIsOverriddenByAHigherValue is the direction that
// filter-nils-per-layer gets right and merge-then-filter must not break.
func TestALowLayerNilIsOverriddenByAHigherValue(t *testing.T) {
	s := func(v string) *string { return &v }
	plan := provider.HeaderPlan{
		Auth:    map[string]*string{"x-thing": nil},
		Request: map[string]*string{"x-thing": s("on")},
	}
	if got := plan.Merge()["X-Thing"]; got != "on" {
		t.Fatalf("x-thing = %q, want \"on\": a nil at a LOW layer that a higher layer "+
			"overrides sends the value", got)
	}
}

// TestAttributionHasASingleKillSwitch is REQ-SEC-13.2. The default being ON is
// exactly why it must be disclosed and switchable.
func TestAttributionHasASingleKillSwitch(t *testing.T) {
	on := provider.AttributionLayer(nil, envOf(nil))
	if len(on) == 0 {
		t.Fatal("attribution defaults to on")
	}
	off := false
	if got := provider.AttributionLayer(&off, envOf(nil)); len(got) != 0 {
		t.Fatalf("AgentConfig.Attribution = false must disable every attribution header, got %v", got)
	}
	if got := provider.AttributionLayer(nil, envOf(map[string]string{
		"AGENTKIT_TELEMETRY": "0"})); len(got) != 0 {
		t.Fatalf("AGENTKIT_TELEMETRY=0 must disable every attribution header, got %v", got)
	}
}

// TestNoAttributionHeaderCarriesRequestContent is REQ-SEC-13.3, enforced
// mechanically rather than by review: every value must be a constant, so no
// future edit can slip a session id or a workspace path in.
func TestNoAttributionHeaderCarriesRequestContent(t *testing.T) {
	m := &core.Model{ID: "m", Provider: "v"}
	auth := provider.ModelAuth{}
	opts := core.RequestOptions{SessionID: "session-should-never-appear"}
	plan := provider.PlanFor(m, auth, opts, nil, envOf(nil))

	for name, v := range plan.Attribution {
		if v == nil {
			continue
		}
		if strings.Contains(*v, "session") || strings.Contains(*v, "/") && strings.Contains(*v, "home") {
			t.Fatalf("attribution header %q carries request context: %q", name, *v)
		}
		if want := provider.AttributionHeaders[name]; *v != want {
			t.Fatalf("attribution header %q = %q, want the enumerated constant %q",
				name, *v, want)
		}
	}
	if len(provider.AttributionNames()) != len(provider.AttributionHeaders) {
		t.Fatal("AttributionNames must enumerate the complete set (REQ-SEC-13.1)")
	}
}
