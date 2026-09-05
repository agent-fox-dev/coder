package provider

import (
	"os"
	"strings"
)

// This file is REQ-AUTH-01..04 and the NFR-SEC-01 redaction boundary.
//
// It lives in `provider` rather than in each wire API because credential
// resolution is keyed by VENDOR while providers are keyed by wire API
// (REQ-PROV-09). One Anthropic-Messages implementation serves Anthropic
// direct, Bedrock-style gateways and OpenRouter's `anthropic/*` routes, and
// each of those resolves its credential differently. Putting the table in the
// wire API would force the vendor axis into the API axis, which is precisely
// the vendor-keyed registry REQ-PROV-09 prohibits.

// CredentialState is REQ-AUTH-04's THREE-valued credential state.
//
// The third value is the requirement. A deployment using an instance role, ADC
// or a workload identity has no key the SDK can read and a transport that will
// nonetheless authenticate. Modelling credentials as a bool ("have key" /
// "no key") fails every such deployment at a pre-flight check that a plain key
// would have passed, and the failure names the wrong cause.
type CredentialState uint8

const (
	// CredentialNone: nothing found, and nothing ambient to fall back to.
	CredentialNone CredentialState = iota
	// CredentialResolved: a usable credential is in hand.
	CredentialResolved
	// CredentialAmbient: no readable credential, but the transport can obtain
	// one. Pre-flight checks must treat this as configured.
	CredentialAmbient
)

func (s CredentialState) String() string {
	switch s {
	case CredentialResolved:
		return "resolved"
	case CredentialAmbient:
		return "ambient"
	}
	return "none"
}

// ModelAuth is REQ-AUTH-01's resolved auth. ANY of the three may carry the
// credential: a key, a set of headers, or a distinct base URL. The
// API-key-string-only model of the §7 sketch cannot express a gateway that
// authenticates by URL, and is replaced by this.
type ModelAuth struct {
	APIKey  string
	Headers map[string]*string
	BaseURL string
	State   CredentialState
	// Source names the environment variable the key came from. It is
	// diagnostic only and never carries the value.
	Source string
}

// String is redacted. It is defined so that the ordinary ways a credential
// leaks — %v in a log line, %w in an error, a struct dump in a test failure —
// cannot print one (NFR-SEC-01, REQ-AUTH-07).
func (a ModelAuth) String() string {
	return "ModelAuth{state:" + a.State.String() + " source:" + a.Source +
		" key:" + Redact(a.APIKey) + " base_url:" + a.BaseURL + "}"
}

// Redact implements NFR-SEC-01: first 4 and last 4 characters with *** in the
// middle.
//
// A short secret is redacted ENTIRELY rather than shown with its middle
// elided. The literal rule leaks a 9-character credential down to one unknown
// character; the point of the rule is recognisability, not a fixed shape.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) < 12 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

// AuthScheme says how a resolved value is carried on the wire. It is part of
// the table because it varies WITHIN one vendor: Anthropic sends
// ANTHROPIC_AUTH_TOKEN as `Authorization: Bearer` and ANTHROPIC_API_KEY as
// `x-api-key`, and sending either under the other's header is a 401
// (REQ-AUTH-03).
type AuthScheme uint8

const (
	SchemeAPIKey AuthScheme = iota // x-api-key style
	SchemeBearer                   // Authorization: Bearer
)

// EnvVar is one row of a vendor's ordered resolution table.
type EnvVar struct {
	Name   string
	Scheme AuthScheme
	// DiscoveryOnly marks a variable that participates in "is this vendor
	// configured?" while being unusable as a plain credential — REQ-AUTH-03's
	// "discovery and retrieval are distinct operations". A base URL pointing
	// at a corporate gateway is the common case: its presence means the vendor
	// is set up, and sending it as a bearer token is nonsense.
	DiscoveryOnly bool
}

// VendorAuth is one vendor's ordered table plus its ambient detector.
type VendorAuth struct {
	// Vars are consulted IN ORDER. First non-empty wins.
	Vars []EnvVar
	// Ambient reports whether a credential chain exists that the SDK cannot
	// read but the transport can (REQ-AUTH-04). Nil means none.
	Ambient func(Env) bool
	// BaseURLVar, when set and present, overrides the model's base URL.
	BaseURLVar string
}

// Env is REQ-AUTH-03's lookup: RequestOptions.Env is consulted BEFORE
// os.Getenv, and an empty override falls through rather than masking.
//
// Falling through is the deliberate half. A caller building a scoped override
// map from a partially-filled config would otherwise blank out a working
// ambient key by writing "" for a variable it had no value for, and the
// resulting 401 names no cause.
type Env struct {
	Override map[string]string
	// Getenv is injectable so tests need not mutate process state
	// (NFR-TEST-04). Nil means os.Getenv.
	Getenv func(string) string
}

func (e Env) Get(name string) string {
	if v := e.Override[name]; v != "" {
		return v
	}
	if e.Getenv != nil {
		return e.Getenv(name)
	}
	return os.Getenv(name)
}

// Has reports presence for DISCOVERY. It is deliberately the same lookup as
// Get: a variable that is present but empty is not configuration.
func (e Env) Has(name string) bool { return e.Get(name) != "" }

// ResolveAuth walks a vendor's ordered table and produces REQ-AUTH-01's
// ModelAuth. It never logs and never returns the raw value in an error.
func ResolveAuth(v VendorAuth, env Env) ModelAuth {
	auth := ModelAuth{State: CredentialNone, Headers: map[string]*string{}}

	if v.BaseURLVar != "" {
		if u := env.Get(v.BaseURLVar); u != "" {
			auth.BaseURL = strings.TrimRight(u, "/")
			// A base URL alone is configuration, not a credential: it makes
			// the vendor discovered without making it authenticated.
			auth.State = CredentialAmbient
			auth.Source = v.BaseURLVar
		}
	}

	for _, row := range v.Vars {
		val := env.Get(row.Name)
		if val == "" {
			continue
		}
		if row.DiscoveryOnly {
			if auth.State == CredentialNone {
				auth.State = CredentialAmbient
				auth.Source = row.Name
			}
			continue
		}
		auth.APIKey = val
		auth.State = CredentialResolved
		auth.Source = row.Name
		switch row.Scheme {
		case SchemeBearer:
			auth.Headers["Authorization"] = strp("Bearer " + val)
		default:
			auth.Headers["x-api-key"] = strp(val)
		}
		return auth
	}

	if auth.State != CredentialResolved && v.Ambient != nil && v.Ambient(env) {
		auth.State = CredentialAmbient
	}
	return auth
}

func strp(s string) *string { return &s }
