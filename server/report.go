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
	done     chan struct{}
	scope    string
	entries  []sessionModelEntry
	err      error
	cancel   context.CancelFunc
	waiters  int
	finished bool
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
			runCtx, cancel := context.WithTimeout(context.Background(), reportTimeout)
			call := &reportCall{done: make(chan struct{}), scope: scope, cancel: cancel, waiters: 1}
			c.inFlight = call
			c.mu.Unlock()
			go c.execute(call, runCtx, cmd, runner, extraArgs...)
			return c.wait(ctx, call)
		}
		call := c.inFlight
		if call.scope == scope {
			call.waiters++
			c.mu.Unlock()
			return c.wait(ctx, call)
		}
		c.mu.Unlock()
		select {
		case <-call.done:
			// A different scope finished. Loop so this caller starts its
			// own report while preserving the one-process invariant.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *reportCoordinator) execute(call *reportCall, runCtx context.Context, cmd resolvedCommand, runner runner, extraArgs ...string) {
	call.entries, call.err = runSessionModels(runCtx, cmd, runner, extraArgs...)
	call.cancel()
	c.mu.Lock()
	call.finished = true
	if c.inFlight == call {
		c.inFlight = nil
	}
	close(call.done)
	c.mu.Unlock()
}

func (c *reportCoordinator) wait(ctx context.Context, call *reportCall) ([]sessionModelEntry, error) {
	select {
	case <-call.done:
		return call.entries, call.err
	case <-ctx.Done():
		c.mu.Lock()
		if call.waiters > 0 {
			call.waiters--
		}
		if call.waiters == 0 && !call.finished {
			call.cancel()
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}
