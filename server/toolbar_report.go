package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	defaultReportTimeout   = 120 * time.Second
	defaultReportFreshness = 30 * time.Second
	defaultFailureCooldown = 10 * time.Second
	reportShutdownWait     = 3 * time.Second
)

const (
	reportStateReady   = "ready"
	reportStatePending = "pending"
	reportStateFailed  = "failed"

	reportErrorTimeout     = "timeout"
	reportErrorUnavailable = "unavailable"
	reportErrorFailed      = "failed"
)

type usageTotals struct {
	Found     bool
	Cost      float64
	Input     int64
	Output    int64
	CacheRead int64
	Turns     int64
	Models    []sessionModel
}

type reportSnapshot struct {
	entries      []sessionModelEntry
	generatedAt  time.Time
	bySession    map[string]usageTotals
	sessionOrder []string
	tokscale     InstallStatus
}

type reportFailure struct {
	code string
	at   time.Time
}

type reportView struct {
	state    string
	errCode  string
	stale    bool
	snapshot *reportSnapshot
}

type toolbarReportCoordinator struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	load   func(context.Context, resolvedCommand) ([]sessionModelEntry, error)
	now    func() time.Time

	timeout   time.Duration
	freshness time.Duration
	cooldown  time.Duration

	active     bool
	activeDone chan struct{}
	snapshot   *reportSnapshot
	failure    *reportFailure
	closed     bool
}

func newToolbarReportCoordinator(load func(context.Context, resolvedCommand) ([]sessionModelEntry, error), now func() time.Time) *toolbarReportCoordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &toolbarReportCoordinator{
		ctx:       ctx,
		cancel:    cancel,
		load:      load,
		now:       now,
		timeout:   defaultReportTimeout,
		freshness: defaultReportFreshness,
		cooldown:  defaultFailureCooldown,
	}
}

func (c *toolbarReportCoordinator) Request(cmd resolvedCommand, refresh bool) reportView {
	now := c.now()
	c.mu.Lock()
	if !c.closed && !c.active && c.shouldStart(now, refresh) {
		c.startLocked(cmd)
	}

	view := reportView{snapshot: c.snapshot}
	switch {
	case c.active:
		view.state = reportStatePending
		view.stale = c.snapshot != nil
	case c.failure != nil:
		view.state = reportStateFailed
		view.errCode = c.failure.code
		view.stale = c.snapshot != nil
	case c.snapshot != nil:
		view.state = reportStateReady
	default:
		view.state = reportStateFailed
		view.errCode = reportErrorUnavailable
	}
	c.mu.Unlock()
	return view
}

func (c *toolbarReportCoordinator) shouldStart(now time.Time, refresh bool) bool {
	if refresh {
		return true
	}
	if c.failure != nil && now.Sub(c.failure.at) < c.cooldown {
		return false
	}
	if c.snapshot == nil {
		return true
	}
	return now.Sub(c.snapshot.generatedAt) >= c.freshness
}

func (c *toolbarReportCoordinator) startLocked(cmd resolvedCommand) {
	c.active = true
	c.failure = nil
	done := make(chan struct{})
	c.activeDone = done
	ctx, cancel := context.WithTimeout(c.ctx, c.timeout)
	started := c.now()
	go func() {
		defer cancel()
		entries, err := c.load(ctx, cmd)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		finished := c.now()
		var snapshot *reportSnapshot
		var failure *reportFailure
		if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
			failure = &reportFailure{code: classifyReportError(ctx, err), at: finished}
			log.Printf("session-cost report failed: code=%s duration=%s", failure.code, finished.Sub(started).Round(time.Millisecond))
		} else if err == nil {
			snapshot = buildReportSnapshot(entries, cmd, finished)
		}

		c.mu.Lock()
		if !c.closed {
			if failure != nil {
				c.failure = failure
			} else if snapshot != nil {
				c.snapshot = snapshot
				c.failure = nil
			}
		}
		c.active = false
		c.activeDone = nil
		close(done)
		c.mu.Unlock()
	}()
}

func (c *toolbarReportCoordinator) Close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
	}
	done := c.activeDone
	c.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(reportShutdownWait):
		log.Printf("session-cost report shutdown exceeded %s", reportShutdownWait)
	}
}

func classifyReportError(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return reportErrorTimeout
	}
	if errors.Is(err, exec.ErrNotFound) {
		return reportErrorUnavailable
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return reportErrorUnavailable
	}
	return reportErrorFailed
}

func buildReportSnapshot(entries []sessionModelEntry, cmd resolvedCommand, generatedAt time.Time) *reportSnapshot {
	snapshot := &reportSnapshot{
		entries:     entries,
		generatedAt: generatedAt,
		bySession:   make(map[string]usageTotals),
		tokscale: InstallStatus{
			Command:   commandDisplay(cmd),
			Source:    cmd.Source,
			Installed: true,
		},
	}
	for _, entry := range entries {
		totals, exists := snapshot.bySession[entry.SessionID]
		if !exists {
			snapshot.sessionOrder = append(snapshot.sessionOrder, entry.SessionID)
		}
		totals.Found = true
		totals.Cost += entry.Cost
		totals.Input += entry.Input
		totals.Output += entry.Output
		totals.CacheRead += entry.CacheRead
		totals.Turns += entry.MessageCount
		totals.Models = append(totals.Models, sessionModel{
			Model:     entry.Model,
			Input:     entry.Input,
			Output:    entry.Output,
			CacheRead: entry.CacheRead,
			Cost:      entry.Cost,
		})
		snapshot.bySession[entry.SessionID] = totals
	}
	return snapshot
}

func (s *reportSnapshot) usageFor(acpSessionID string) usageTotals {
	var usage usageTotals
	for _, sessionID := range s.sessionOrder {
		totals := s.bySession[sessionID]
		if !sessionMatches(sessionID, acpSessionID) {
			continue
		}
		usage.Found = true
		usage.Cost += totals.Cost
		usage.Input += totals.Input
		usage.Output += totals.Output
		usage.CacheRead += totals.CacheRead
		usage.Turns += totals.Turns
		usage.Models = append(usage.Models, totals.Models...)
	}
	if usage.Models == nil {
		usage.Models = []sessionModel{}
	}
	return usage
}
