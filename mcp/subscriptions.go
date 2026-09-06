package mcp

import (
	"context"
	"fmt"
)

// Subscribe opens a subscriptions/listen stream (REQ-MCP-SERVER-06.5's client
// half).
//
// 2026-07-28 replaced the standalone GET stream with this. The difference that
// matters to a caller: notifications are OPT-IN by type, and a server must not
// send a type that was not named — so a client that wants its tool cache
// invalidated has to ask for it, where the old model delivered whatever the
// server chose to send.
//
// The call blocks until the subscription ends, so it is normally run on its
// own goroutine. Cancelling ctx ends it.
func (c *ServerConnection) Subscribe(ctx context.Context, filter SubscriptionFilter) error {
	if !filter.Any() {
		return fmt.Errorf("mcp: %s: a subscription must opt in to at least one "+
			"notification type", c.cfg.Name)
	}
	defer func() {
		c.mu.Lock()
		c.subscribed = false
		c.mu.Unlock()
	}()

	var res SubscriptionsListenResult
	err := c.call(ctx, MethodSubscriptionsListen,
		SubscriptionsListenParams{Notifications: filter}, &res)
	if err != nil {
		return fmt.Errorf("mcp: %s: subscriptions/listen: %w", c.cfg.Name, err)
	}
	return nil
}

// Subscribed reports whether the server has acknowledged a subscription.
//
// Subscribe blocks for the life of the stream, so a caller that starts it on a
// goroutine has no other way to know when it is live — and acting before then
// (registering a tool, say) is a race that loses silently.
func (c *ServerConnection) Subscribed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subscribed
}

// SubscribeToolChanges is the common case: keep the tool cache honest.
//
// It exists because the tool cache is the one piece of client state that goes
// stale silently. Without a subscription the cache is bounded only by the
// server's ttlMs hint, and a server that sends a long hint and then changes
// its tools leaves the model calling tools that no longer exist.
func (c *ServerConnection) SubscribeToolChanges(ctx context.Context) error {
	return c.Subscribe(ctx, SubscriptionFilter{ToolsListChanged: true})
}
