package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/agentfox/agentkit-go/wire"
)

// ServeStdio runs the server over this process's stdin/stdout
// (REQ-MCP-SERVER-02's stdio mode).
//
// Nothing here writes to stdout other than the protocol. A stray log line on
// stdout is indistinguishable from a frame, and the client's decoder is
// poisoned by the first malformed one — so a host's own logging belongs on
// stderr, and ServerOptions.Warnf is how it gets there.
func (s *Server) ServeStdio(ctx context.Context) error {
	tr := NewPipeTransport(os.Stdin, os.Stdout, s.opts.Limits)
	// Closing stdin unblocks the read loop when ctx ends. Without it Serve
	// stays parked in Receive until the parent process closes the pipe, which
	// is precisely the shutdown that cancellation was supposed to trigger.
	stop := context.AfterFunc(ctx, func() { _ = tr.Close() })
	defer stop()

	err := s.Serve(ctx, tr)
	if err != nil && (errors.Is(err, ErrTransportClosed) || ctx.Err() != nil) {
		// A close we asked for is not a failure to report.
		return ctx.Err()
	}
	return err
}

// HTTPShutdownGrace bounds how long ListenAndServeHTTP waits for in-flight
// requests once the context ends.
const HTTPShutdownGrace = 5 * time.Second

// ListenAndServeHTTP runs the HTTP mode on addr until ctx ends.
//
// It returns nil on a clean shutdown. http.ErrServerClosed is the expected
// outcome of a shutdown we asked for, and passing it up as an error makes
// every caller write the same check.
func (s *Server) ListenAndServeHTTP(ctx context.Context, addr string, opts HTTPOptions) error {
	h, err := s.HTTPHandler(opts)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: h,
		// A client that opens a connection and sends nothing otherwise holds a
		// slot indefinitely. These are the cheapest defence and cost a
		// well-behaved client nothing.
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	stop := context.AfterFunc(ctx, func() {
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), HTTPShutdownGrace)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	defer stop()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Run starts whichever mode ServerModeConfig selects (REQ-MCP-SERVER-01/02).
//
// A disabled config returns nil without listening: REQ-MCP-SERVER-01 says
// server mode is off by default, so "not enabled" is a successful no-op and
// not an error a host has to special-case.
//
// lookupEnv resolves ServerModeConfig.APIKeyEnv; nil uses os.Getenv. It is a
// parameter because REQ-AUTH-03 puts an explicit environment ahead of the
// process's own, and because a test should not have to set a real variable to
// exercise the authenticated path.
func (s *Server) Run(ctx context.Context, cfg ServerModeConfig, lookupEnv func(string) string) error {
	if !cfg.Enabled {
		return nil
	}
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}

	switch cfg.Transport {
	case "", "stdio":
		return s.ServeStdio(ctx)
	case "http":
		if cfg.APIKeyEnv == "" {
			return fmt.Errorf("mcp: http server mode needs api_key_env naming the "+
				"variable that holds the key: %w", ErrNoAPIKey)
		}
		key := lookupEnv(cfg.APIKeyEnv)
		if key == "" {
			// Named but unset is the dangerous case: the config LOOKS
			// authenticated, so nobody re-reads it. Refusing to start is the
			// only outcome that cannot be mistaken for a working deployment.
			return fmt.Errorf("mcp: %s is empty or unset, so http server mode has no "+
				"credential to check: %w", cfg.APIKeyEnv, ErrNoAPIKey)
		}
		port := cfg.Port
		if port <= 0 {
			return fmt.Errorf("mcp: http server mode needs a port")
		}
		// Loopback, not 0.0.0.0. A default that binds every interface turns a
		// config file's `port = 8080` into an internet-facing service, and the
		// host that wants that can say so with an explicit address.
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		return s.ListenAndServeHTTP(ctx, addr, HTTPOptions{APIKey: key})
	}
	return fmt.Errorf("mcp: unknown server transport %q (want \"stdio\" or \"http\")", cfg.Transport)
}

// DefaultLimits is what a host gets when it does not choose.
func DefaultLimits() wire.Limits { return wire.Defaults() }
