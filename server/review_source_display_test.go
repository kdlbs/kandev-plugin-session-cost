package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

type sourceContextTestHost struct{ *retainedFixtureHost }

func (h sourceContextTestHost) Tasks() pluginsdk.TaskReader { return collectorTaskReader{} }

func TestReviewManualRefreshKeepsNewScopeNativeValuesOverOldCumulativeSource(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage})
	host.config[configKeyCollect] = false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		rows := canonicalFixture(2)
		rows[0].CostSubcents = nil // A missing native price is not permission to expose old transcript amounts.
		return rows, canonicalReadyPage("new-org-native-publication", ""), nil
	}
	var runs atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		runs.Add(1)
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"cost":99}]}`), nil
	}
	for _, responsive := range []bool{false, true} {
		var response *sessionCostResponse
		var err error
		if responsive {
			response, err = p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
		} else {
			response, err = p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
		}
		require.NoError(t, err)
		require.True(t, response.Saved)
		require.Equal(t, int64(2), response.Input)
		require.False(t, response.CostKnown)
		require.Equal(t, "new-org-native-publication", response.ProjectionGeneration)
	}
	require.Zero(t, runs.Load())
}

func TestReviewRawPathsRequireCurrentSupportedTranscriptContext(t *testing.T) {
	for _, path := range []string{"responsive", "synchronous", "legacy"} {
		t.Run(path, func(t *testing.T) {
			p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				target := testTarget()
				target.ExternalSessionID = nil
				return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "current-scope", SnapshotUpperBound: 1, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
			}})
			p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
			host.config[configKeyCollect] = false
			host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				return nil, canonicalReadyPage("empty-current", ""), nil
			}
			var runs atomic.Int32
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				runs.Add(1)
				return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"cost":99}]}`), nil
			}
			var response *sessionCostResponse
			var err error
			switch path {
			case "responsive":
				response, err = p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
			case "synchronous":
				response, err = p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
			case "legacy":
				var body []byte
				body, err = p.sessionCost(context.Background(), "task-1", "session-1")
				if err == nil {
					value := decode(t, body)
					response = &value
				}
			}
			require.NoError(t, err)
			require.False(t, response.Found)
			require.False(t, response.CostKnown)
			require.Equal(t, "source_context_unavailable", response.Compatibility)
			require.Zero(t, runs.Load())
		})
	}
}

func TestReviewSupportedHomogeneousManualSourceStillWorks(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage})
	host.config[configKeyCollect] = false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, canonicalReadyPage("empty-homogeneous-scope", ""), nil
	}
	var runs atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		runs.Add(1)
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":7,"cost":1}]}`), nil
	}
	_, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	waitForReport(t, p)
	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	require.True(t, response.Found)
	require.False(t, response.Saved)
	require.Equal(t, int64(7), response.Input)
	require.InDelta(t, 1, response.Cost, 1e-12)
	require.Equal(t, int32(1), runs.Load())
}
