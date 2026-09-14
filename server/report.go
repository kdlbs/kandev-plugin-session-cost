package main

import (
	"context"
	"sync"
)

type reportCoordinator struct {
	mu       sync.Mutex
	inFlight *reportCall
}

type reportCall struct {
	done    chan struct{}
	scope   string
	entries []sessionModelEntry
	err     error
}

func newReportCoordinator() *reportCoordinator {
	return &reportCoordinator{}
}

// run coalesces concurrent callers around one bounded tokscale process. Each
// waiter may stop waiting without cancelling the shared process.
func (c *reportCoordinator) run(ctx context.Context, cmd resolvedCommand, runner runner) ([]sessionModelEntry, error) {
	return c.runScoped(ctx, cmd, "lifetime", runner)
}

// runScoped serializes reports with different command scopes while allowing
// callers for the same scope to share one result. A dated historical query
// must never receive the lifetime report that happens to be in flight.
func (c *reportCoordinator) runScoped(ctx context.Context, cmd resolvedCommand, scope string, runner runner, extraArgs ...string) ([]sessionModelEntry, error) {
	for {
		c.mu.Lock()
		if c.inFlight == nil {
			call := &reportCall{done: make(chan struct{}), scope: scope}
			c.inFlight = call
			c.mu.Unlock()
			// The command is shared by all waiters. Do not give it the first
			// webhook/action request context: a disconnected waiter must not
			// cancel a report that another caller is using. The coordinator still
			// bounds the subprocess independently of every request.
			runCtx, cancel := context.WithTimeout(context.Background(), reportTimeout)
			call.entries, call.err = runSessionModels(runCtx, cmd, runner, extraArgs...)
			cancel()
			c.mu.Lock()
			c.inFlight = nil
			close(call.done)
			c.mu.Unlock()
			return call.entries, call.err
		}
		call := c.inFlight
		c.mu.Unlock()
		select {
		case <-call.done:
			if call.scope == scope {
				return call.entries, call.err
			}
			// A different scope finished. Loop so this caller starts its
			// own report while preserving the one-process invariant.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
