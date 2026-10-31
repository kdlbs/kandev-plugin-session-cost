package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
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
			var runs atomic.Int32
			p, _ := historyPlugin(t, retainedProtocolFixture{list: func(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				if q.SnapshotID == "former-org-snapshot" {
					return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.NotFound, "snapshot no longer authorized")
				}
				return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "current-org-snapshot", SnapshotUpperBound: 0}, nil
			}})
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				runs.Add(1)
				return []byte(`{"entries":[]}`), nil
			}
			old := historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, WorkspaceID: "ws", RunID: "former-org-run", SnapshotID: "former-org-snapshot", SnapshotUpperBound: 99, Status: importStatusFailed, Processed: 87, Missing: 43, MissingSessionIDs: []string{"private-session"}, LastError: "private-transcript failed", StartedAt: "private-date", PageTargets: []pluginsdk.UsageCollectionTargetV1{{SubjectBinding: "private-binding", TaskID: "private-task", ExternalSessionID: optionalString("private-transcript")}}, PageDates: map[string][]string{"private-binding": {"1999-01-01"}}}
			require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", old, true))
			response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: action, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"}})
			require.NoError(t, err)
			for _, secret := range []string{"private-", "former-org", "1999", `"processed":87`, `"missing":43`, "page_targets", "snapshot_id", "run_id", "missing_session_ids"} {
				require.NotContains(t, string(response.Body), secret)
			}
			var view map[string]any
			require.NoError(t, json.Unmarshal(response.Body, &view))
			require.Equal(t, float64(0), view["processed"])
			require.Equal(t, float64(0), view["missing"])
			require.Zero(t, runs.Load())
		})
	}
}

func TestReviewImportStatusSameScopeReturnsNarrowViewAfterRestart(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage})
	state := historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, RunID: "private-run", SnapshotID: "fixed", SnapshotUpperBound: 17, Status: importStatusCancelled, Processed: 87, Missing: 43, MissingSessionIDs: []string{"private-session"}, PageTargets: []pluginsdk.UsageCollectionTargetV1{testTarget()}}
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", state, true))
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(host)
	t.Cleanup(restarted.Close)
	response, err := restarted.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionKeyImportStatus, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"}})
	require.NoError(t, err)
	var view map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &view))
	require.Equal(t, float64(87), view["processed"])
	require.Equal(t, float64(43), view["missing"])
	for _, field := range []string{"page_targets", "snapshot_id", "run_id", "missing_session_ids", "page_dates", "cursor"} {
		require.NotContains(t, view, field)
	}
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
	require.ErrorIs(t, p.collectOnce(context.Background()), errUsageScopeChanged)
	require.Equal(t, 1, writes, "old cumulative amounts must never enter the new scope")
}

func TestReviewImportErasureDiscardsCheckpointBeforeResume(t *testing.T) {
	p, _ := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.NotFound, "scope erased")
	}})
	old := historicalImportState{SchemaVersion: 2, SnapshotID: "erased-snapshot", RunID: "erased-run", Processed: 87, Missing: 43, Status: importStatusRunning, PageTargets: []pluginsdk.UsageCollectionTargetV1{testTarget()}}
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", old, true))
	response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionKeyImportStatus, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"}})
	require.NoError(t, err)
	require.NotContains(t, string(response.Body), "erased")
	require.NotContains(t, string(response.Body), "87")
	require.NotContains(t, string(response.Body), "43")
	_, found, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.False(t, found)
	require.ErrorIs(t, p.saveHistoricalImportState(context.Background(), "ws", old), context.Canceled, "an in-flight erased run must not recreate the checkpoint")
}
