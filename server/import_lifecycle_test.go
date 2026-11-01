package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

// importLifecycleHost adds state-write fault injection while delegating every
// other Host surface to the retained usage fixture.
type importLifecycleHost struct {
	*retainedFixtureHost
	mu      sync.Mutex
	failSet func(scope, id, key string, value map[string]any) error
}

func (h *importLifecycleHost) SetState(ctx context.Context, scope, id, key string, value map[string]any) error {
	h.mu.Lock()
	hook := h.failSet
	h.mu.Unlock()
	if hook != nil {
		if err := hook(scope, id, key, value); err != nil {
			return err
		}
	}
	return h.retainedFixtureHost.SetState(ctx, scope, id, key, value)
}

func (h *importLifecycleHost) failOnce(predicate func(string, map[string]any) bool) {
	var fired atomic.Bool
	h.mu.Lock()
	h.failSet = func(_ string, _ string, key string, value map[string]any) error {
		if fired.Load() || !predicate(key, value) {
			return nil
		}
		fired.Store(true)
		return errors.New("injected state write failure")
	}
	h.mu.Unlock()
}

func newImportLifecycleHost(fixture *importEngineFixture, collect bool) *importLifecycleHost {
	return &importLifecycleHost{retainedFixtureHost: &retainedFixtureHost{
		usageRecordingHost: &usageRecordingHost{
			fakeHost: &fakeHost{config: map[string]any{configKeyCollect: collect}},
			usage:    &usageRecordingReader{},
		},
		reader: fixture.reader(),
	}}
}

func newImportLifecyclePlugin(t *testing.T, host *importLifecycleHost, fixture *importEngineFixture, start bool) *plugin {
	t.Helper()
	p := newPlugin()
	p.lookPath = noLookPath
	p.run = fixture.run
	p.importWait = func(context.Context) bool { return true }
	if start {
		p.SetHost(host)
	} else {
		p.UnimplementedPlugin.SetHost(host)
	}
	t.Cleanup(p.Close)
	return p
}

func importStateOf(t *testing.T, p *plugin, workspace string) historicalImportState {
	t.Helper()
	state, found, err := p.loadHistoricalImportState(context.Background(), workspace)
	require.NoError(t, err)
	require.True(t, found)
	return state
}

func TestImportLifecycleResumesRunningImportAfterRestart(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-03"),
	}}
	host := newImportLifecycleHost(fixture, true)
	seed := newImportLifecyclePlugin(t, host, fixture, false)
	require.NoError(t, seed.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "restart-run",
		SnapshotID: "engine-snapshot", SnapshotUpperBound: 99, Phase: importPhaseDaily,
		Intervals: []historicalImportInterval{{Start: "2026-10-01", End: "2026-10-03"}},
		DaysTotal: 3, DaysDone: 1, NextDay: "2026-10-02", DaySecondsTotal: 30, DayAttempts: 1,
		SessionsFound: 1, SessionsImported: 1, Undated: true,
	}, true))
	require.NoError(t, seed.writeImportQueue(context.Background(), []string{"ws"}))
	seed.Close()

	restarted := newImportLifecyclePlugin(t, host, fixture, true)
	require.Eventually(t, func() bool {
		state, found, err := restarted.loadHistoricalImportState(context.Background(), "ws")
		return err == nil && found && state.Status == importStatusCompleted
	}, 2*time.Second, 5*time.Millisecond)

	require.Equal(t, []string{"2026-10-02", "2026-10-03"}, fixture.reports, "completed days are never repeated")
	state := importStateOf(t, restarted, "ws")
	require.Equal(t, 3, state.DaysDone)
	require.Equal(t, 3, state.DaysTotal)
	require.Zero(t, state.DayAttempts)
	require.Equal(t, "restart-run", state.RunID, "the interrupted run identity is reused")
}

func TestImportLifecycleSecondWorkspaceQueuesWithoutCancellingFirst(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
	}}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fixture.onReport = func(ctx context.Context) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	host := newImportLifecycleHost(fixture, true)
	p := newImportLifecyclePlugin(t, host, fixture, false)

	first, err := p.startHistoricalImport(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Equal(t, 202, first.Status)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the first import never started its report")
	}
	running := importStateOf(t, p, "ws-1")
	require.Equal(t, importStatusRunning, running.Status)

	second, err := p.startHistoricalImport(context.Background(), "ws-2")
	require.NoError(t, err)
	require.Equal(t, 202, second.Status)
	require.NotContains(t, string(second.Body), "ws-1", "a queued status never reveals the workspace ahead")
	require.Equal(t, importStatusQueued, importStateOf(t, p, "ws-2").Status)

	stillRunning := importStateOf(t, p, "ws-1")
	require.Equal(t, importStatusRunning, stillRunning.Status, "starting another workspace leaves the active run alone")

	close(release)
	require.Eventually(t, func() bool {
		state, found, err := p.loadHistoricalImportState(context.Background(), "ws-1")
		return err == nil && found && state.Status == importStatusCompleted
	}, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		state, found, err := p.loadHistoricalImportState(context.Background(), "ws-2")
		return err == nil && found && state.Status == importStatusCompleted
	}, 2*time.Second, 5*time.Millisecond)
	firstState := importStateOf(t, p, "ws-1")
	require.Equal(t, firstState.DaysTotal, firstState.DaysDone, "the first import finished every active day")
}

func TestImportLifecycleShutdownDoesNotWriteTerminalStatus(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
	}}
	started := make(chan struct{})
	var once sync.Once
	fixture.onReport = func(ctx context.Context) {
		once.Do(func() { close(started) })
		<-ctx.Done()
	}
	host := newImportLifecycleHost(fixture, true)
	p := newImportLifecyclePlugin(t, host, fixture, false)
	_, err := p.startHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the import never started its report")
	}
	p.Close()

	state := importStateOf(t, p, "ws")
	require.NotEqual(t, importStatusCancelled, state.Status, "shutdown is not a user cancellation")
	require.NotEqual(t, importStatusFailed, state.Status)
	require.NotEqual(t, importStatusCompleted, state.Status)
}

func TestImportLifecycleReportFailureStopsAfterThreeAttemptsAndResumeKeepsPosition(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-02"),
	}}
	var failures atomic.Int32
	fixture.reportErr = func() error {
		if failures.Add(1) <= maxImportDayAttempts {
			return errors.New("tokscale timed out")
		}
		return nil
	}
	host := newImportLifecycleHost(fixture, true)
	p := newImportLifecyclePlugin(t, host, fixture, false)
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
		SnapshotID: "engine-snapshot", SnapshotUpperBound: 99, Phase: importPhaseDaily,
		Intervals: []historicalImportInterval{{Start: "2026-10-01", End: "2026-10-02"}},
		DaysTotal: 2, DaysDone: 1, NextDay: "2026-10-02", SessionsFound: 1, SessionsImported: 1, Undated: true,
	}, true))
	p.runHistoricalImport(context.Background(), "ws")
	failed := importStateOf(t, p, "ws")
	require.Equal(t, importStatusFailed, failed.Status)
	require.Equal(t, 3, failed.DayAttempts)
	require.Equal(t, "2026-10-02", failed.NextDay, "the failed day is kept for Resume")
	require.Equal(t, 1, failed.DaysDone)

	response, err := p.startHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, 202, response.Status)
	require.Eventually(t, func() bool {
		state, found, err := p.loadHistoricalImportState(context.Background(), "ws")
		return err == nil && found && state.Status == importStatusCompleted
	}, 2*time.Second, 5*time.Millisecond)
	resumed := importStateOf(t, p, "ws")
	require.Equal(t, 2, resumed.DaysDone, "Resume continues from the failed day")
	require.Zero(t, resumed.DayAttempts, "a successful day resets the failure counter")
}

func TestImportLifecycleDisabledKeepsPositionAndResumes(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-02"),
	}}
	host := newImportLifecycleHost(fixture, true)
	var once sync.Once
	fixture.onReport = func(context.Context) {
		once.Do(func() { host.config[configKeyCollect] = false })
	}
	p := newImportLifecyclePlugin(t, host, fixture, false)
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
		SnapshotID: "engine-snapshot", SnapshotUpperBound: 99, Phase: importPhaseDaily,
		Intervals: []historicalImportInterval{{Start: "2026-10-01", End: "2026-10-02"}},
		DaysTotal: 2, NextDay: "2026-10-01", SessionsFound: 1,
	}, true))
	p.runHistoricalImport(context.Background(), "ws")
	disabled := importStateOf(t, p, "ws")
	require.Equal(t, importStatusDisabled, disabled.Status)
	require.Equal(t, "2026-10-01", disabled.NextDay, "the disabled run keeps its position")
	require.Zero(t, disabled.DaysDone)

	blocked, err := p.startHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, 409, blocked.Status)
	require.Equal(t, importStatusDisabled, importStateOf(t, p, "ws").Status)

	host.config[configKeyCollect] = true
	response, err := p.startHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, 202, response.Status)
	require.Eventually(t, func() bool {
		state, found, err := p.loadHistoricalImportState(context.Background(), "ws")
		return err == nil && found && state.Status == importStatusCompleted
	}, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, 2, importStateOf(t, p, "ws").DaysDone)
}

func TestImportLifecycleStateFailureSettlesFailed(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
	}}
	host := newImportLifecycleHost(fixture, true)
	host.failOnce(func(key string, value map[string]any) bool {
		return key == importStateKey && value["phase"] == importPhaseDaily && value["status"] == importStatusRunning
	})
	p := newImportLifecyclePlugin(t, host, fixture, false)
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}, true))
	p.runHistoricalImport(context.Background(), "ws")
	state := importStateOf(t, p, "ws")
	require.Equal(t, importStatusFailed, state.Status, "an interrupted checkpoint write settles instead of staying running")
	require.NotEqual(t, importStatusRunning, state.Status)
}

func TestImportLifecycleComposerWaiterSurvivesImportCancellation(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
	}}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	fixture.onReport = func(ctx context.Context) {
		calls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	host := newImportLifecycleHost(fixture, true)
	host.sessions = []pluginsdk.Session{{ID: "session-0", TaskID: "task-0", ACPSessionID: "transcript-0"}}
	p := newImportLifecyclePlugin(t, host, fixture, false)

	_, err := p.startHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the import never started its lifetime report")
	}

	// The composer shares the import's lifetime report instead of starting a
	// second tokscale process.
	pending, err := p.responsiveSessionUsage(context.Background(), "ws", "task-0", "session-0", true)
	require.NoError(t, err)
	require.Equal(t, reportStatePending, pending.ReportState)
	require.Equal(t, int32(1), calls.Load(), "the composer joined the shared report")
	require.Eventually(t, func() bool {
		p.reportMu.Lock()
		reports := p.reports
		p.reportMu.Unlock()
		reports.mu.Lock()
		defer reports.mu.Unlock()
		return reports.inFlight != nil && reports.inFlight.waiters >= 2
	}, time.Second, time.Millisecond, "the composer must become a waiter before the import is cancelled")

	// Cancelling the import removes only its waiter; the composer keeps the
	// shared process alive and receives the completed result.
	_, err = p.cancelHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	close(release)
	waitForReport(t, p)

	p.toolbar.mu.Lock()
	snapshot := p.toolbar.snapshot
	p.toolbar.mu.Unlock()
	require.NotNil(t, snapshot, "the composer received the shared report result")
	require.NotEmpty(t, snapshot.entries)
	require.Equal(t, int32(1), calls.Load(), "no second report was started: %v", fixture.reports)

	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-0", "session-0", false)
	require.NoError(t, err)
	require.NotEqual(t, reportStateFailed, response.ReportState)
	require.NotEqual(t, reportErrorTimeout, response.ReportError)
}

func TestImportLifecycleRefusedStartKeepsFinishedState(t *testing.T) {
	for _, status := range []string{importStatusCompleted, importStatusCancelled} {
		t.Run(status, func(t *testing.T) {
			fixture := &importEngineFixture{pageSize: 10}
			host := newImportLifecycleHost(fixture, false)
			p := newImportLifecyclePlugin(t, host, fixture, false)
			require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
				SchemaVersion: importSchemaVersion, Status: status, RunID: "finished-run",
				Phase: importPhaseCompleted, SnapshotID: "engine-snapshot", SnapshotUpperBound: 99,
			}, true))
			response, err := p.startHistoricalImport(context.Background(), "ws")
			require.NoError(t, err)
			require.Equal(t, 409, response.Status)
			state := importStateOf(t, p, "ws")
			require.Equal(t, status, state.Status, "a refused start must not replace a finished checkpoint")
			require.Equal(t, "finished-run", state.RunID)
			require.Equal(t, importPhaseCompleted, state.Phase)
		})
	}
}

func TestImportLifecycleDisabledCompletedPhaseStartsNewRun(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
	}}
	host := newImportLifecycleHost(fixture, true)
	p := newImportLifecyclePlugin(t, host, fixture, false)
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusDisabled, RunID: "stale-run",
		Phase: importPhaseCompleted, SnapshotID: "engine-snapshot", SnapshotUpperBound: 99,
	}, true))
	response, err := p.startHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, 202, response.Status)
	require.Eventually(t, func() bool {
		state, found, err := p.loadHistoricalImportState(context.Background(), "ws")
		return err == nil && found && state.Status == importStatusCompleted
	}, 2*time.Second, 5*time.Millisecond)
	state := importStateOf(t, p, "ws")
	require.NotEqual(t, "stale-run", state.RunID)
	require.Equal(t, importPhaseCompleted, state.Phase)
}

func TestImportStatusDoesNotCreateSnapshotBeforeWorker(t *testing.T) {
	calls := 0
	p, _ := historyPlugin(t, retainedProtocolFixture{
		list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
			calls++
			return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "created-by-poll", SnapshotUpperBound: 1}, nil
		},
		report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
			return acceptedBatch(batch), nil
		},
	})
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusPreparing, Phase: importPhaseScanning,
	}, true))
	response, err := p.handleHistoricalImportAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionKeyImportStatus, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"},
	})
	require.NoError(t, err)
	require.Equal(t, 200, response.Status)
	require.Zero(t, calls, "a status poll must not create a discovery snapshot")
	state := importStateOf(t, p, "ws")
	require.Empty(t, state.SnapshotID)
}

func TestImportLifecycleTransientConfigFailureRetries(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
	}}
	host := newImportLifecycleHost(fixture, true)
	calls := 0
	host.fakeHost.configFn = func(context.Context) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("config read unavailable")
		}
		return map[string]any{configKeyCollect: true}, nil
	}
	p := newImportLifecyclePlugin(t, host, fixture, false)
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-config",
		SnapshotID: "engine-snapshot", SnapshotUpperBound: 99, Phase: importPhaseDaily,
		Intervals: []historicalImportInterval{{Start: "2026-10-01", End: "2026-10-01"}},
		DaysTotal: 1, NextDay: "2026-10-01", SessionsFound: 1,
	}, true))
	p.runHistoricalImport(context.Background(), "ws")
	state := importStateOf(t, p, "ws")
	require.Equal(t, importStatusCompleted, state.Status, "a transient config read must not stop the import as disabled")
	require.Greater(t, calls, 1)
}
