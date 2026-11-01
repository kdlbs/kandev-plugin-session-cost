package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isolationFixtures returns one eligible and one ineligible target for the two
// live sessions the isolation tests collect.
func isolationFixtures() []pluginsdk.UsageCollectionTargetV1 {
	eligible := testTarget()
	eligible.SessionStartedAt = optionalString("2026-11-01T00:00:00Z")
	eligible.SessionEndedAt = optionalString("2026-11-01T12:00:00Z")
	ineligible := pluginsdk.UsageCollectionTargetV1{
		SourceScopeStatus: "unknown", SubjectBinding: "binding:session-2", TaskID: "task-2",
		SessionID: optionalString("session-2"), ExternalSessionID: optionalString("transcript-2"), SubjectKind: "session",
	}
	return []pluginsdk.UsageCollectionTargetV1{eligible, ineligible}
}

func isolationSessions() []pluginsdk.Session {
	return []pluginsdk.Session{
		{ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", State: "RUNNING"},
		{ID: "session-2", TaskID: "task-2", ACPSessionID: "transcript-2", State: "RUNNING"},
	}
}

type isolationObserver struct {
	writes  []pluginsdk.UsageObservationV1
	reports atomic.Int32
}

func (o *isolationObserver) reader(targets *[]pluginsdk.UsageCollectionTargetV1) pluginsdk.UsageHistoryReader {
	return retainedProtocolFixture{
		list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
			return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "isolation", SnapshotUpperBound: 1, Targets: *targets}, nil
		},
		report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
			o.writes = append(o.writes, batch.Observations...)
			return acceptedBatch(batch), nil
		},
	}
}

func (o *isolationObserver) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	for _, arg := range args {
		if arg == "--version" {
			return []byte("tokscale 4.15.1\n"), nil
		}
	}
	o.reports.Add(1)
	return []byte(`{"entries":[
		{"sessionId":"transcript-1","model":"model-a","input":4,"total":4,"cost":0.1},
		{"sessionId":"transcript-2","model":"model-a","input":9,"total":9,"cost":0.9}
	]}`), nil
}

func TestCollectorIsolationIneligibleSessionDoesNotBlockEligibleSession(t *testing.T) {
	t.Setenv("TZ", "UTC")
	targets := isolationFixtures()
	observer := &isolationObserver{}
	p, host := historyPlugin(t, observer.reader(&targets))
	p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
	p.now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	host.sessions = isolationSessions()
	host.config[configKeyCollect] = true
	p.run = observer.run

	require.NoError(t, p.collectOnce(context.Background()))
	require.Equal(t, int32(2), observer.reports.Load(), "lifetime and dated reports still run for the eligible session")
	require.NotEmpty(t, observer.writes)
	for _, item := range observer.writes {
		require.Equal(t, testTarget().SubjectBinding, item.SubjectBinding, "an ineligible transcript is never written")
	}

	checkpoint, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.False(t, checkpoint.Pending["session-1"].Skipped)
	require.True(t, checkpoint.Pending["session-2"].Skipped, "the ineligible session stays queued for a later pass")
}

func TestCollectorIsolationSkippedActiveSessionIsRechecked(t *testing.T) {
	t.Setenv("TZ", "UTC")
	targets := isolationFixtures()
	observer := &isolationObserver{}
	p, host := historyPlugin(t, observer.reader(&targets))
	p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
	p.now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	host.sessions = isolationSessions()
	host.config[configKeyCollect] = true
	p.run = observer.run

	require.NoError(t, p.collectOnce(context.Background()))
	require.Equal(t, int32(2), observer.reports.Load())
	targets[1].SourceScopeStatus = "eligible"
	require.NoError(t, p.collectOnce(context.Background()))

	checkpoint, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.False(t, checkpoint.Pending["session-2"].Skipped, "a now-eligible session is collected by a later pass")
	require.Equal(t, "binding:session-2", checkpoint.Pending["session-2"].SubjectBinding)
	var wroteSecond bool
	for _, item := range observer.writes {
		if item.SubjectBinding == "binding:session-2" {
			wroteSecond = true
		}
	}
	require.True(t, wroteSecond)
}

func TestCollectorIsolationOnlySkippedSessionsRunNoReport(t *testing.T) {
	targets := isolationFixtures()[1:]
	observer := &isolationObserver{}
	p, host := historyPlugin(t, observer.reader(&targets))
	p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
	host.sessions = isolationSessions()[1:]
	host.config[configKeyCollect] = true
	p.run = observer.run

	require.NoError(t, p.collectOnce(context.Background()))
	require.Zero(t, observer.reports.Load(), "a pass with no eligible session never starts tokscale")
	require.Empty(t, observer.writes)
	checkpoint, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.True(t, checkpoint.Pending["session-2"].Skipped)
}

func TestCollectorIsolationRemovesUnboundPendingWithoutLiveSession(t *testing.T) {
	targets := isolationFixtures()
	observer := &isolationObserver{}
	p, host := historyPlugin(t, observer.reader(&targets))
	p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
	host.sessions = nil
	host.config[configKeyCollect] = true
	p.run = observer.run
	require.NoError(t, host.SetState(context.Background(), collectorStateScope, collectorStateID, collectorStateKey, map[string]any{
		"initialized": true,
		"pending": map[string]any{
			"deleted": map[string]any{"task_id": "task-gone", "acp_session_id": "transcript-gone", "state": "RUNNING"},
		},
	}))

	require.NoError(t, p.collectOnce(context.Background()))
	checkpoint, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.NotContains(t, checkpoint.Pending, "deleted", "an entry with no binding and no live session cannot be collected")
	require.Zero(t, observer.reports.Load())
}

func TestCollectorIsolationRemovesTerminalSkippedSession(t *testing.T) {
	targets := isolationFixtures()
	observer := &isolationObserver{}
	p, host := historyPlugin(t, observer.reader(&targets))
	p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
	host.sessions = nil
	host.config[configKeyCollect] = true
	p.run = observer.run
	require.NoError(t, host.SetState(context.Background(), collectorStateScope, collectorStateID, collectorStateKey, map[string]any{
		"initialized": true,
		"pending": map[string]any{
			"session-2": map[string]any{
				"task_id": "task-2", "workspace_id": "ws", "acp_session_id": "transcript-2",
				"subject_binding": "binding:session-2", "state": "COMPLETED", "skipped": true,
			},
		},
	}))

	require.NoError(t, p.collectOnce(context.Background()))
	checkpoint, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.NotContains(t, checkpoint.Pending, "session-2", "a terminal skipped session cannot become eligible again")
	require.Zero(t, observer.reports.Load())
}

func TestCollectorIsolationTransientHostErrorAbortsAndKeepsSavedWork(t *testing.T) {
	observer := &isolationObserver{}
	p, host := historyPlugin(t, retainedProtocolFixture{
		list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
			return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.Unavailable, "host read timed out")
		},
		report: func(context.Context, pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
			return pluginsdk.UsageObservationBatchResultV1{}, errors.New("no write expected")
		},
	})
	p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
	host.sessions = isolationSessions()[:1]
	host.config[configKeyCollect] = true
	p.run = observer.run
	require.NoError(t, host.SetState(context.Background(), collectorStateScope, collectorStateID, collectorStateKey, map[string]any{
		"initialized": true,
		"pending": map[string]any{
			"session-1": map[string]any{
				"task_id": "task-1", "workspace_id": "ws", "acp_session_id": "transcript-1",
				"subject_binding": testTarget().SubjectBinding, "state": "RUNNING",
				"dates": []string{"2026-11-01"},
			},
		},
	}))

	err := p.collectOnce(context.Background())
	require.Equal(t, codes.Unavailable, status.Code(err), "only per-session errors are isolated")
	checkpoint, loadErr := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, loadErr)
	require.False(t, checkpoint.Pending["session-1"].Skipped)
	require.Equal(t, []string{"2026-11-01"}, checkpoint.Pending["session-1"].Dates)
	require.Zero(t, observer.reports.Load())
}
