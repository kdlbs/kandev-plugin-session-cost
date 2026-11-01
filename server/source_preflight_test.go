package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestComposerIneligibleSessionUsesSafeAttributionCopy(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		target := testTarget()
		target.SourceScopeStatus, target.ExternalSessionID = "unknown", nil
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "composer", SnapshotUpperBound: 1, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
	}, report: func(context.Context, pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		t.Fatal("an ineligible composer source never writes")
		return pluginsdk.UsageObservationBatchResultV1{}, nil
	}})
	host.usage.items = []pluginsdk.SessionUsageMeasurement{{
		WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
		Model: "model-a", InputTokens: ptr(7), CostSubcents: ptr(250), Coverage: "complete",
	}}
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("an ineligible composer source never calculates")
		return nil, nil
	}

	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, reportStateFailed, response.ReportState)
	require.Equal(t, "source_ineligible", response.ReportError)
	require.Equal(t, "source_context_unavailable", response.Compatibility)
	require.True(t, response.Saved, "saved usage stays available for the ineligible session")
	require.True(t, response.Stale)
	require.Equal(t, int64(7), response.Input)
	require.NotContains(t, response.Error, "Upgrade")
}

func TestComposerScopeChangeUsesSafeAttributionCopy(t *testing.T) {
	saved := &sessionCostResponse{Found: true, Input: 11, Cost: 0.5, Models: []sessionModel{{Model: "model-a", Input: 11}}}
	response := newPlugin().unavailableSource(saved, "session-1", 1, 10, errUsageScopeChanged)
	require.Equal(t, reportStateFailed, response.ReportState)
	require.Equal(t, "source_ineligible", response.ReportError)
	require.Equal(t, "source_context_unavailable", response.Compatibility)
	require.True(t, response.Stale)
	require.Equal(t, int64(11), response.Input)
	require.NotContains(t, response.Error, "Upgrade")
}

func TestComposerTransientHostFailureKeepsGenericSourceContextCopy(t *testing.T) {
	response := newPlugin().unavailableSource(nil, "session-1", 1, 10, errors.New("host read timed out"))
	require.Equal(t, reportStateFailed, response.ReportState)
	require.Equal(t, "source_context", response.ReportError)
	require.Equal(t, "source_context_unavailable", response.Compatibility)
}

func TestComposerUnsupportedHostStillAsksForUpgrade(t *testing.T) {
	response := newPlugin().unavailableSource(nil, "session-1", 1, 10, errUsageHistoryUnsupported)
	require.Equal(t, reportStateFailed, response.ReportState)
	require.Equal(t, reportErrorUnavailable, response.ReportError)
	require.Equal(t, "upgrade_required", response.Compatibility)
}

func TestComposerSynchronousPathPreservesIneligibleClassification(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		target := testTarget()
		target.SourceScopeStatus = "mixed"
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "composer", SnapshotUpperBound: 1, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
	}})
	host.usage.items = []pluginsdk.SessionUsageMeasurement{{
		WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
		Model: "model-a", InputTokens: ptr(3), Coverage: "complete",
	}}
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("an ineligible composer source never calculates")
		return nil, nil
	}

	response, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, reportStateFailed, response.ReportState)
	require.Equal(t, "source_ineligible", response.ReportError)
	require.True(t, response.Saved)
	require.Equal(t, int64(3), response.Input)
}
