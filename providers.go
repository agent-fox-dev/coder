package agentkit

import "github.com/agentfox/agentkit-go/core"

// DefaultProviders returns a fresh registry holding the first-party wire API
// implementations (REQ-PROV-09).
//
// It is a PURE FUNCTION returning a new map, not a package-level registry
// populated by init(). That matters twice over: NFR-SEC-05 forbids init()-time
// global mutation outright, and a global would additionally have to be frozen
// against late registration races. With no global there is nothing to race and
// nothing to freeze — strictly better than the mutable-default OQ-12 weighs
// (ruling C13).
//
// The cost is that a caller wanting the defaults must say so. That cost is
// paid once, in NewAgent, and it is why AgentConfig.Providers being nil means
// "the defaults" rather than "no providers".
//
// v1 registers no providers here; the provider packages register themselves
// into a registry the caller passes, so importing agentkit does not drag
// net/http into a consumer that only wants the loop. Use RegisterDefaults, or
// build the registry explicitly:
//
//	reg := agentkit.DefaultProviders()
//	reg.Register(anthropic.Provider())
//	cfg.Providers = reg
func DefaultProviders() core.ProviderRegistry { return core.ProviderRegistry{} }

// RegisterDefaults installs providers into cfg, creating the registry if
// needed. It is the one-line form for the common case.
func RegisterDefaults(cfg *core.AgentConfig, ps ...core.APIProvider) {
	if cfg.Providers == nil {
		cfg.Providers = DefaultProviders()
	}
	for _, p := range ps {
		cfg.Providers.Register(p)
	}
}
