package mcp

import (
	"context"
	"errors"
	"fmt"
)

// ListResources enumerates the server's concrete resources, following
// pagination (REQ-MCP-SERVER-05's client half).
//
// Resources are NOT cached the way tools are. The tool list is cached because
// it is re-sent in every prompt and is invalidated by an explicit
// notification; a resource list is read on demand and its contents can change
// without any notification at all, so serving a stale one would be a worse
// bargain than the round trip.
func (c *ServerConnection) ListResources(ctx context.Context) ([]Resource, error) {
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		return nil, ErrNotInitialized
	}

	var all []Resource
	cursor := ""
	for {
		var res ResourcesListResult
		if err := c.call(ctx, MethodResourcesList, ResourcesListParams{Cursor: cursor}, &res); err != nil {
			return nil, fmt.Errorf("mcp: %s: resources/list: %w", c.cfg.Name, err)
		}
		all = append(all, res.Resources...)
		if res.NextCursor == "" || res.NextCursor == cursor {
			// A server repeating its cursor would otherwise page forever.
			break
		}
		cursor = res.NextCursor
		if len(all) > 10_000 {
			c.warnf("server %q listed more than 10000 resources; stopping", c.cfg.Name)
			break
		}
	}
	return all, nil
}

// ListResourceTemplates enumerates the parameterised resources.
//
// A server that does not implement the method answers CodeMethodNotFound, and
// that is reported as an empty list rather than an error: templates are
// optional, and a client that treats "this server has none" as a failure
// cannot talk to the servers that have none.
func (c *ServerConnection) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		return nil, ErrNotInitialized
	}

	var res ResourceTemplatesListResult
	if err := c.call(ctx, MethodResourceTemplatesList, struct{}{}, &res); err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) && rpcErr.Code == CodeMethodNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("mcp: %s: resources/templates/list: %w", c.cfg.Name, err)
	}
	return res.ResourceTemplates, nil
}

// ReadResource fetches one resource by URI.
func (c *ServerConnection) ReadResource(ctx context.Context, uri string) (ResourcesReadResult, error) {
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		return ResourcesReadResult{}, ErrNotInitialized
	}

	var res ResourcesReadResult
	if err := c.call(ctx, MethodResourcesRead, ResourcesReadParams{URI: uri}, &res); err != nil {
		return ResourcesReadResult{}, fmt.Errorf("mcp: %s: resources/read %s: %w", c.cfg.Name, uri, err)
	}
	return res, nil
}
