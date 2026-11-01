package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestReviewImportActionsFenceFormerScopeAndHideCheckpoint(t *testing.T) {
	for _, action := range []string{actionKeyImportStatus, actionKeyImportCancel, actionKeyImportStart} {
		t.Run(action, func(t *testing.T) {
			p, _ := historyPlugin(t, retainedProtocolFixture{list: func(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				if q.SnapshotID == "former-org-snapshot" {
					return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.NotFound, "snapshot no longer authorized")
				}
				return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "current-org-snapshot", SnapshotUpperBound: 0}, nil
			}})
			p.importWait = func(context.Context) bool { return false }
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				return []byte(`{"entries":[]}`), nil
			}
			old := historicalImportState{
				SchemaVersion: importSchemaVersion, WorkspaceID: "ws", RunID: "former-org-run",
				SnapshotID: "former-org-snapshot", SnapshotUpperBound: 99, Status: importStatusFailed,
				Phase: importPhaseDaily, NextDay: "1999-01-01", DaysTotal: 3, DaysDone: 1,
				SessionsFound: 130, SessionsImported: 87, SessionsMissing: 43, SessionsSkipped: 3,
				LastError: "private-transcript failed", StartedAt: "private-date",
				Intervals: []historicalImportInterval{{Start: "1999-01-01", End: "1999-01-03"}},
			}
			require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", old, true))
			response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: action, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"}})
			require.NoError(t, err)
			wantStatus := 200
			if action == actionKeyImportStart {
				wantStatus = 202
			}
			require.Equal(t, wantStatus, response.Status)
			for _, secret := range []string{"private-", "former-org", "1999", "130", "87", "43", "snapshot_id", "run_id", "intervals", "next_day"} {
				require.NotContains(t, string(response.Body), secret)
			}
			var view map[string]any
			require.NoError(t, json.Unmarshal(response.Body, &view))
			require.Equal(t, float64(0), view["sessions_imported"])
			require.Equal(t, float64(0), view["sessions_missing"])
			require.Equal(t, float64(0), view["sessions_skipped"])
			require.Equal(t, float64(0), view["days_total"])
		})
	}
}

func TestReviewImportStatusSameScopeReturnsNarrowViewAfterRestart(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage})
	p.importWait = func(context.Context) bool { return false }
	state := historicalImportState{
		SchemaVersion: importSchemaVersion, RunID: "private-run", SnapshotID: "fixed", SnapshotUpperBound: 17,
		Status: importStatusCancelled, Phase: importPhaseDaily, NextDay: "2026-10-05",
		DaysTotal: 5, DaysDone: 3, DaySecondsTotal: 90,
		SessionsFound: 130, SessionsImported: 87, SessionsMissing: 40, SessionsSkipped: 3,
		Intervals: []historicalImportInterval{{Start: "2026-10-05", End: "2026-10-09"}},
	}
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", state, true))
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(host)
	restarted.importWait = func(context.Context) bool { return false }
	t.Cleanup(restarted.Close)
	response, err := restarted.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionKeyImportStatus, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"}})
	require.NoError(t, err)
	require.Equal(t, 200, response.Status)
	var view map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &view))
	require.Equal(t, float64(130), view["sessions_found"])
	require.Equal(t, float64(87), view["sessions_imported"])
	require.Equal(t, float64(40), view["sessions_missing"])
	require.Equal(t, float64(3), view["sessions_skipped"])
	require.Equal(t, float64(5), view["days_total"])
	require.Equal(t, float64(3), view["days_done"])
	require.Equal(t, "2026-10-05", view["current_day"])
	require.Equal(t, float64(60), view["eta_seconds"], "three completed days average 30s across two remaining days")
	for _, field := range []string{"snapshot_id", "run_id", "intervals", "next_day", "scan_cursor", "day_attempts", "day_seconds_total"} {
		require.NotContains(t, view, field)
	}
}

func TestReviewImportErasureDiscardsCheckpointBeforeResume(t *testing.T) {
	p, _ := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.NotFound, "scope erased")
	}})
	old := historicalImportState{SchemaVersion: importSchemaVersion, SnapshotID: "erased-snapshot", RunID: "erased-run", SessionsImported: 87, SessionsMissing: 43, Status: importStatusRunning}
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", old, true))
	response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionKeyImportStatus, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"}})
	require.NoError(t, err)
	require.Equal(t, 200, response.Status)
	require.NotContains(t, string(response.Body), "erased")
	require.NotContains(t, string(response.Body), "87")
	require.NotContains(t, string(response.Body), "43")
	_, found, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.False(t, found)
	require.ErrorIs(t, p.saveHistoricalImportState(context.Background(), "ws", old), context.Canceled, "an in-flight erased run must not recreate the checkpoint")
}

func TestReviewRevokedTargetCannotReuseAcceptedReceiptOrRebindCumulativeUsage(t *testing.T) {
	revoked := false
	writes, validations := 0, 0
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		if q.CurrentTarget != nil && q.CurrentTarget.SubjectBinding == testTarget().SubjectBinding {
			validations++
			if revoked {
				return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.NotFound, "old scope revoked")
			}
		}
		target := testTarget()
		snapshot := "old-scope"
		if revoked {
			target.SubjectBinding, snapshot = "new-scope-binding", "new-scope"
		}
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: snapshot, SnapshotUpperBound: 17, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
	}, report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		writes++
		return acceptedBatch(batch), nil
	}})
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":7}]}`)
	require.NoError(t, p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response))
	revoked = true
	for attempt := 0; attempt < 2; attempt++ {
		err := p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response)
		require.Error(t, err, "a cached accepted receipt cannot suppress current authorization")
		require.Contains(t, err.Error(), "scope")
	}
	require.Equal(t, 1, validations, "a revoked exact binding is checked once, then quarantined across retries")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	host.sessions[0].State = "RUNNING"
	host.sessions[0].UpdatedAt = now.Format(time.RFC3339)
	p.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":7}]}`), nil)
	require.NoError(t, p.saveCollectorCheckpoint(context.Background(), host, collectorCheckpoint{Initialized: true, Pending: map[string]collectorPending{"session-1": {TaskID: "task-1", WorkspaceID: "ws", ACPSessionID: "transcript-1", SubjectBinding: testTarget().SubjectBinding, State: "RUNNING"}}}))
	require.NoError(t, p.collectOnce(context.Background()), "a revoked session is skipped without aborting the pass")
	skipped, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.True(t, skipped.Pending["session-1"].Skipped)
	require.Equal(t, 1, writes, "old cumulative amounts must never enter the new scope")
}
