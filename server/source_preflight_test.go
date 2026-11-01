package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sourceTaskTestHost struct {
	*retainedFixtureHost
	tasks pluginsdk.TaskReader
}

func (h sourceTaskTestHost) Tasks() pluginsdk.TaskReader { return h.tasks }

type sourceTaskTestReader struct {
	pluginsdk.TaskReader
	get func(context.Context, string) (*pluginsdk.Task, error)
}

func (r sourceTaskTestReader) Get(ctx context.Context, id string) (*pluginsdk.Task, error) {
	return r.get(ctx, id)
}

func TestCurrentSourceTargetTreatsMissingTaskAsIneligibleAndBoundsLookup(t *testing.T) {
	for _, tc := range []struct {
		name string
		get  func() (*pluginsdk.Task, error)
	}{
		{name: "not found", get: func() (*pluginsdk.Task, error) {
			return nil, status.Error(codes.NotFound, "task no longer exists")
		}},
		{name: "empty row", get: func() (*pluginsdk.Task, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage})
			p.hostRead = 20 * time.Millisecond
			p.UnimplementedPlugin.SetHost(sourceTaskTestHost{
				retainedFixtureHost: host,
				tasks: sourceTaskTestReader{get: func(ctx context.Context, _ string) (*pluginsdk.Task, error) {
					if _, ok := ctx.Deadline(); !ok {
						t.Error("task lookup must have a bounded Host read context")
					}
					return tc.get()
				}},
			})
			_, err := p.currentSourceTarget(context.Background(), "", "task-1", "session-1", "transcript-1")
			require.ErrorIs(t, err, errUsageSourceUnavailable)
		})
	}
}

func TestCurrentSourceTargetStopsStalledTaskLookupAtHostReadDeadline(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage})
	p.hostRead = 20 * time.Millisecond
	p.UnimplementedPlugin.SetHost(sourceTaskTestHost{
		retainedFixtureHost: host,
		tasks: sourceTaskTestReader{get: func(ctx context.Context, _ string) (*pluginsdk.Task, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := p.currentSourceTarget(ctx, "", "task-1", "session-1", "transcript-1")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 450*time.Millisecond, "the plugin Host-read budget must bound the task lookup")
}

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
