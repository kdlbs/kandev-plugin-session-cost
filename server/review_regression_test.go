package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestReportDoesNotJoinCancelledProcess(t *testing.T) {
	c := newReportCoordinator()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			<-release
			return nil, ctx.Err()
		}
		return []byte(`{"entries":[]}`), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.run(ctx, resolvedCommand{Argv: []string{"fixture"}}, run); first <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-first, context.Canceled)
	waiting := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		_, err := c.run(&doneSignalContext{Context: context.Background(), signal: waiting}, resolvedCommand{Argv: []string{"fixture"}}, run)
		second <- err
	}()
	<-waiting
	close(release)
	require.NoError(t, <-second)
	require.Equal(t, int32(2), calls.Load())
}

func TestRejectedBatchIncludesEveryAcceptedModel(t *testing.T) {
	usage := &usageRecordingReader{writeStatus: "conflict", items: []pluginsdk.SessionUsageMeasurement{
		{SourceRecordID: "transcript", UsageIdentity: "lifetime:transcript", Model: "a", CostSubcents: ptr(int64(100))},
		{SourceRecordID: "transcript", UsageIdentity: "lifetime:transcript", Model: "b", CostSubcents: ptr(int64(200))},
	}}
	p := newPlugin()
	p.SetHost(&usageRecordingHost{fakeHost: &fakeHost{}, usage: usage})
	response := sessionCostResponse{ACPSessionID: "transcript", Models: []sessionModel{{Model: "a", Cost: 9}, {Model: "b", Cost: 9}}}
	err := p.persistSessionUsage(context.Background(), "ws", "task", "session", &response)
	var rejected *usageWriteRejectedError
	require.ErrorAs(t, err, &rejected)
	require.True(t, rejected.complete)
	require.Len(t, rejected.measurements, 2)
}

func TestDatedCollectionRotatesTerminalWork(t *testing.T) {
	sessions := []pluginsdk.Session{{ID: "active", State: "RUNNING"}, {ID: "terminal", State: "COMPLETED"}}
	pending := map[string]collectorPending{
		"active": {Dates: []string{"2026-10-05"}}, "terminal": {Dates: []string{"2026-10-01"}},
	}
	require.Equal(t, "2026-10-05", nextCollectionDate(pending, sessions))
	require.Equal(t, "2026-10-01", nextCollectionDate(pending, sessions, "2026-10-05"))
	require.Equal(t, "2026-10-05", nextCollectionDate(pending, sessions, "2026-10-01"))
}

func TestImportCancellationCannotBecomeFailure(t *testing.T) {
	p := newPlugin()
	p.SetHost(&fakeHost{})
	ctx := context.Background()
	require.NoError(t, p.saveHistoricalImportState(ctx, "ws", historicalImportState{Status: importStatusCancelled}))
	p.finishHistoricalImport("ws", importStatusFailed, context.Canceled)
	state, _, err := p.loadHistoricalImportState(ctx, "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCancelled, state.Status)
	require.Empty(t, state.LastError)
}

func TestHistoricalImportResumesPendingDate(t *testing.T) {
	usage := &usageRecordingReader{}
	host := &usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{
		{ID: "session", TaskID: "task", ACPSessionID: "transcript", StartedAt: "2026-09-30T10:00:00Z", UpdatedAt: "2026-10-01T10:00:00Z", State: "COMPLETED"},
	}}, usage: usage}
	p := newPlugin()
	p.SetHost(host)
	defer stopPluginWorkers(p)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{Status: importStatusRunning, Cursor: "interrupted-page", Dates: []string{"2026-10-01"}}))
	var dates []string
	p.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "--since" {
				dates = append(dates, args[i+1])
			}
		}
		return []byte(`{"entries":[{"sessionId":"transcript","model":"a","cost":1}]}`), nil
	}
	p.runHistoricalImport(context.Background(), "ws")
	require.Equal(t, []string{"2026-10-01"}, dates)
	require.Len(t, usage.items, 1)
	require.Equal(t, "2026-10-01", usage.items[0].SourceDate)
}

func TestImportWaitHonorsIntervalAndCancellation(t *testing.T) {
	p := newPlugin()
	p.SetHost(&fakeHost{config: map[string]any{configKeyInterval: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan bool, 1)
	go func() { finished <- p.waitImportBudget(ctx) }()
	select {
	case <-finished:
		t.Fatal("import bypassed its interval budget")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	require.False(t, <-finished)
}

func TestSourceDateAndTimezoneShareTZOverride(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	require.Equal(t, "Asia/Tokyo", sourceTimezone())
	require.Equal(t, "2026-10-01", collectionDate(time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC)))
	p := newPlugin()
	err := p.persistSessionUsageWithCoverage(context.Background(), "ws", "task", "session", &sessionCostResponse{}, usageCoverage{Date: "2026-10-01"})
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
}
