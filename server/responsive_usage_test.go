package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
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
