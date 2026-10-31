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

func TestResponsiveActionRefreshPollsAndPersistsOverSavedUsage(t *testing.T) {
	usage := &usageRecordingReader{items: []pluginsdk.SessionUsageMeasurement{{
		WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
		SourceRecordID: "transcript-1", UsageIdentity: "lifetime:transcript-1", Model: "model-a",
		InputTokens: int64Ptr(1), TotalTokens: int64Ptr(1), CostSubcents: int64Ptr(100),
		CollectedAt: "2026-09-01T00:00:00Z",
	}}}
	host := &usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")}}, usage: usage}
	p := newPlugin()
	p.lookPath = noLookPath
	// Exercise the authenticated action separately from the periodic collector.
	p.UnimplementedPlugin.SetHost(host)
	defer p.Close()
	release := make(chan struct{})
	var calls atomic.Int64
	p.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		calls.Add(1)
		select {
		case <-release:
			return []byte(`{"entries":[{"sessionId":"transcript-1","model":"model-a","input":100,"output":5,"cost":1.25}]}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	request := &pluginsdk.PluginActionRequest{ActionKey: actionKeySessionUsage, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "session-1"}, Body: []byte(`{"refresh":true}`)}
	first, err := p.HandleAction(context.Background(), request)
	require.NoError(t, err)
	pending := decode(t, first.Body)
	require.Equal(t, reportStatePending, pending.ReportState)
	require.True(t, pending.Saved)
	require.Equal(t, int64(1), pending.Input)
	request.Body = []byte(`{"refresh":false}`)
	poll, err := p.HandleAction(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, reportStatePending, decode(t, poll.Body).ReportState)
	close(release)
	waitForReport(t, p)
	ready, err := p.HandleAction(context.Background(), request)
	require.NoError(t, err)
	result := decode(t, ready.Body)
	require.Equal(t, reportStateReady, result.ReportState)
	require.True(t, result.Saved)
	require.Equal(t, int64(100), result.Input)
	require.InDelta(t, 1.25, result.Cost, 0.00001)
	require.Equal(t, int64(1), calls.Load())
	usage.mu.Lock()
	require.Equal(t, 1, usage.upserts)
	usage.mu.Unlock()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(ready.Body, &payload))
	require.NotEmpty(t, payload["last_refresh"])
	require.WithinDuration(t, time.Now(), p.now(), time.Second)
}

func TestResponsiveActionCanonicalReadbackFailureSurvivesReadyReport(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code          codes.Code
		rejectWrite   bool
		wantSaved     bool
		wantInput     int64
		wantState     string
		wantError     string
		compatibility string
	}{
		{name: "invalid_query", code: codes.InvalidArgument, wantState: "failed", wantError: "invalid_query"},
		{name: "denied", code: codes.PermissionDenied, wantState: "failed", wantError: "access_denied"},
		{name: "not_found", code: codes.NotFound, wantState: "failed", wantError: "access_denied"},
		{name: "unauthenticated", code: codes.Unauthenticated, wantState: "failed", wantError: "access_denied"},
		{name: "unsupported", code: codes.Unimplemented, wantState: "failed", wantError: "unavailable", compatibility: "upgrade_required"},
		{name: "rejected_write", code: codes.OK, rejectWrite: true, wantSaved: true, wantInput: 2, wantState: "failed", wantError: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
				writes.Add(1)
				result := acceptedBatch(batch)
				if tc.rejectWrite {
					for i := range result.Results {
						result.Results[i].Status = pluginsdk.UsageObservationStatusConflict
					}
				}
				return result, nil
			}})
			host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				if writes.Load() > 0 && tc.code != codes.OK {
					return nil, nil, status.Error(tc.code, "canonical readback failed")
				}
				return canonicalFixture(2), canonicalReadyPage("accepted-selection.1", ""), nil
			}
			release := make(chan struct{})
			var runs atomic.Int32
			p.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
				runs.Add(1)
				select {
				case <-release:
					return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"cost":9}]}`), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			request := &pluginsdk.PluginActionRequest{ActionKey: actionKeySessionUsage, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1"}, Body: []byte(`{"refresh":true}`)}
			pending, err := p.HandleAction(context.Background(), request)
			require.NoError(t, err)
			require.Equal(t, reportStatePending, decode(t, pending.Body).ReportState)
			require.Equal(t, int64(2), decode(t, pending.Body).Input)
			close(release)
			waitForReport(t, p)
			request.Body = []byte(`{"refresh":false}`)
			completed, err := p.HandleAction(context.Background(), request)
			require.NoError(t, err)
			require.Equal(t, 200, completed.Status)
			response := decode(t, completed.Body)
			require.Equal(t, tc.wantState, response.ReportState, "a ready source report cannot make a failed canonical readback or admission ready")
			require.Equal(t, tc.wantError, response.ReportError)
			require.Equal(t, tc.compatibility, response.Compatibility)
			require.Equal(t, tc.wantSaved, response.Saved)
			require.Equal(t, tc.wantSaved, response.Found)
			require.Equal(t, tc.wantInput, response.Input)
			require.False(t, response.CanonicalPending)
			if tc.wantSaved {
				require.Equal(t, "accepted-selection.1", response.ProjectionGeneration)
				require.True(t, response.Stale)
				require.InDelta(t, 0.02, response.Cost, 1e-12)
			} else {
				require.False(t, response.CostKnown)
				require.Empty(t, response.ProjectionGeneration)
				require.Empty(t, response.Models)
			}
			require.Equal(t, int32(1), writes.Load())
			require.Equal(t, int32(1), runs.Load())
		})
	}
}
