package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func canonicalFixture(input int64) []pluginsdk.SessionUsageMeasurement {
	return []pluginsdk.SessionUsageMeasurement{{WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1", InputTokens: ptr(input), CostSubcents: ptr(input * 100), Currency: "USD", Coverage: "complete"}}
}

func TestUsageHistoryProjectionRetainsHostLastCompleteDuringPending(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	preparing := false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if preparing {
			return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "complete-generation", ProjectionPending: true}, nil
		}
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "complete-generation"}, nil
	}
	first, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Equal(t, int64(2), first.Input)
	preparing = true
	pending, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Equal(t, int64(2), pending.Input, "incomplete rows cannot replace the completed saved selection")
	require.True(t, pending.CanonicalPending)
	require.True(t, pending.Stale)
	require.Equal(t, reportStatePending, pending.ReportState)
	encoded, err := json.Marshal(pending)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"projection_generation":"complete-generation"`)
	require.NotContains(t, string(encoded), `"pending_projection_generation"`)
}

func TestUsageHistoryProjectionPreparingWithoutRowsDoesNotStartTokscale(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, &pluginsdk.PageInfo{ProjectionStatus: "preparing", ProjectionGeneration: "opaque-7", ProjectionPending: true}, nil
	}
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"entries":[]}`), nil
	}
	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	p.Close()
	require.Zero(t, calls.Load())
	require.True(t, response.CanonicalPending)
	require.False(t, response.Found)
	require.False(t, response.CostKnown)
	require.Equal(t, reportStatePending, response.ReportState)
}

func TestHistoricalTrustedRetainedDatesWithoutLiveSession(t *testing.T) {
	t.Setenv("TZ", "UTC")
	target := testTarget()
	target.SessionStartedAt = optionalString("2026-10-25T12:00:00Z")
	target.SessionEndedAt = optionalString("2026-10-26T12:00:00Z")
	var dates []string
	var writes []pluginsdk.UsageObservationV1
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 1, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
	}, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		writes = append(writes, b.Observations...)
		return acceptedBatch(b), nil
	}})
	host.sessions = nil
	host.sessionsFn = func(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, error) {
		t.Fatal("trusted retained dates required live session lookup")
		return nil, nil
	}
	p.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		date := "lifetime"
		for i, arg := range args {
			if arg == "--since" {
				date = args[i+1]
			}
		}
		dates = append(dates, date)
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":1}]}`), nil
	}
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, Status: importStatusRunning}))
	p.runHistoricalImport(context.Background(), "ws")
	require.Equal(t, []string{"2026-10-25", "2026-10-26"}, dates)
	require.Len(t, writes, 2)
	for i, item := range writes {
		require.Equal(t, dates[i], *item.Coverage.SourceDate)
	}
}

func TestUsageHistoryTokscaleMessageCountIsNotKandevTurns(t *testing.T) {
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":1,"cost":1,"messageCount":9}]}`)
	require.Zero(t, response.Turns)
	require.Zero(t, response.CostPerTurn)
	require.Nil(t, response.TokenAmounts["turns"])
}

func TestUsageHistoryProjectionRejectsMixedPageGenerations(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "complete"}, nil
	}
	_, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	host.usage.canonicalFn = func(_ context.Context, _ pluginsdk.SessionUsageFilter, page pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if page.Cursor == "" {
			return canonicalFixture(99), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g-next", HasMore: true, NextCursor: "second"}, nil
		}
		return canonicalFixture(100), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g-changed"}, nil
	}
	response, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Zero(t, response.Input)
	require.False(t, response.Found)
	require.False(t, response.Saved)
	require.False(t, response.CostKnown)
	require.True(t, response.CanonicalPending)
	require.Empty(t, response.ProjectionGeneration)
	host.usage.canonicalFn = func(_ context.Context, _ pluginsdk.SessionUsageFilter, page pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if page.Cursor == "" {
			return canonicalFixture(3), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "90071992547409931234", HasMore: true, NextCursor: "second"}, nil
		}
		return canonicalFixture(4), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "90071992547409931234"}, nil
	}
	response, found = p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Equal(t, int64(7), response.Input)
	require.False(t, response.CanonicalPending)
	require.Equal(t, "90071992547409931234", response.ProjectionGeneration)
}

func TestUsageHistoryProjectionLagAndRestartKeepOnlyCompleteSelection(t *testing.T) {
	for _, info := range []pluginsdk.PageInfo{
		{ProjectionStatus: "ready", ProjectionGeneration: "last-complete", ProjectionPending: true},
		{ProjectionStatus: "preparing", ProjectionGeneration: "g-next"},
		{ProjectionStatus: "unknown", ProjectionGeneration: "g-next"},
	} {
		t.Run(info.ProjectionStatus, func(t *testing.T) {
			p, host := historyPlugin(t, retainedProtocolFixture{})
			host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "last-complete"}, nil
			}
			_, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
			require.True(t, found)
			host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				if info.ProjectionStatus == "ready" {
					return canonicalFixture(2), &info, nil
				}
				return canonicalFixture(999), &info, nil
			}
			restarted := newPlugin()
			restarted.UnimplementedPlugin.SetHost(host)
			defer restarted.Close()
			response, found := restarted.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
			require.True(t, found)
			require.True(t, response.CanonicalPending)
			if info.ProjectionStatus == "ready" {
				require.Equal(t, int64(2), response.Input)
				require.True(t, response.Saved)
				require.Equal(t, "last-complete", response.ProjectionGeneration)
			} else {
				require.Zero(t, response.Input)
				require.False(t, response.Found)
				require.False(t, response.Saved)
				require.False(t, response.CostKnown)
				require.Empty(t, response.ProjectionGeneration)
			}
		})
	}
}

func TestUsageHistoryProjectionCompletedEmptySelectionReplacesOldAmount(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g1"}, nil
	}
	_, _ = p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g2"}, nil
	}
	response, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.False(t, response.Found)
	require.False(t, response.CanonicalPending)
	require.False(t, response.CostKnown)
	require.Equal(t, "g2", response.ProjectionGeneration)
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(999), &pluginsdk.PageInfo{ProjectionStatus: "preparing", ProjectionGeneration: "g3", ProjectionPending: true}, nil
	}
	response, found = p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.False(t, response.Found)
	require.True(t, response.CanonicalPending)
	require.Empty(t, response.ProjectionGeneration)
	require.False(t, response.Saved)
}

func TestUsageHistoryProjectionRefreshWaitsForCompletedReadback(t *testing.T) {
	var admitted bool
	p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		admitted = true
		return acceptedBatch(b), nil
	}})
	complete := false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if complete {
			return canonicalFixture(7), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g2"}, nil
		}
		if admitted {
			return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g1", ProjectionPending: true}, nil
		}
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g1"}, nil
	}
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"messageCount":19}]}`), nil
	}
	first, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, int64(2), first.Input)
	require.Equal(t, reportStatePending, first.ReportState)
	waitForReport(t, p)
	lag, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	require.True(t, admitted)
	require.Equal(t, int64(2), lag.Input)
	require.Equal(t, "g1", lag.ProjectionGeneration)
	require.True(t, lag.CanonicalPending)
	again, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	require.Equal(t, int64(2), again.Input)
	require.Equal(t, reportStatePending, again.ReportState)
	require.Equal(t, int32(1), calls.Load())
	complete = true
	ready, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	require.Equal(t, int64(7), ready.Input)
	require.Equal(t, "g2", ready.ProjectionGeneration)
	require.False(t, ready.CanonicalPending)
	require.Equal(t, reportStateReady, ready.ReportState)
	require.Zero(t, ready.Turns)
}

func TestUsageHistoryProjectionConcurrentOlderReadCannotReplaceNewerCompletion(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return canonicalFixture(1), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "earlier-request"}, nil
		}
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "later-request"}, nil
	}
	finished := make(chan *sessionCostResponse, 1)
	go func() {
		result, _ := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
		finished <- result
	}()
	<-started
	newest, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Equal(t, int64(2), newest.Input)
	close(release)
	earlier := <-finished
	require.Zero(t, earlier.Input)
	require.False(t, earlier.Found)
	require.False(t, earlier.Saved)
	require.False(t, earlier.CostKnown)
	require.True(t, earlier.CanonicalPending)
	require.Empty(t, earlier.ProjectionGeneration)
}

func TestHistoricalTrustedRangesPreserveUnknownAndCoverageEnd(t *testing.T) {
	t.Setenv("TZ", "Europe/Lisbon")
	for _, tt := range []struct {
		name   string
		target pluginsdk.UsageCollectionTargetV1
		want   []string
	}{
		{"source interval", pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", CoverageStart: optionalString("2026-10-24T23:00:00Z"), CoverageEnd: optionalString("2026-10-26T00:00:00Z")}, []string{"2026-10-25"}},
		{"missing end", pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", SessionStartedAt: optionalString("2026-10-25T12:00:00Z")}, nil},
		{"invalid", pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", SessionStartedAt: optionalString("invalid"), SessionEndedAt: optionalString("2026-10-25T12:00:00Z")}, nil},
		{"reversed", pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", CoverageStart: optionalString("2026-10-26T00:00:00Z"), CoverageEnd: optionalString("2026-10-25T12:00:00Z")}, nil},
		{"partial session with source interval", pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", SessionStartedAt: optionalString("2026-10-01T00:00:00Z"), CoverageStart: optionalString("2026-10-24T23:00:00Z"), CoverageEnd: optionalString("2026-10-26T00:00:00Z")}, []string{"2026-10-25"}},
	} {
		t.Run(tt.name, func(t *testing.T) { require.Equal(t, tt.want, trustedTargetDateRange(tt.target)) })
	}
}

func TestHistoricalTrustedLongIntervalQueuesOneDay(t *testing.T) {
	t.Setenv("TZ", "UTC")
	target := testTarget()
	target.CoverageStart = optionalString("2000-01-01T00:00:00Z")
	target.CoverageEnd = optionalString("2099-01-01T00:00:00Z")
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 1, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
	}})
	host.sessions = nil
	p.importWait = func(context.Context) bool { return false }
	p.run = modelsRunner([]byte(`{"entries":[]}`), nil)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, Status: importStatusRunning}))
	p.runHistoricalImport(context.Background(), "ws")
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCancelled, state.Status)
	require.Equal(t, []string{"2000-01-02"}, state.Dates)
	require.Equal(t, []string{"2000-01-01", "2098-12-31"}, state.PageDates[target.SubjectBinding])
	require.Zero(t, state.Processed)
}

func TestUsageHistoryProjectionReadyEmptyAllowsManualCalculationWhenDisabled(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.config[configKeyCollect] = false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g-empty"}, nil
	}
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","cost":1,"messageCount":9}]}`), nil
	}
	first, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	require.Equal(t, reportStatePending, first.ReportState)
	waitForReport(t, p)
	ready, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	require.True(t, ready.Found)
	require.False(t, ready.Saved)
	require.InDelta(t, 1, ready.Cost, 1e-9)
	require.Zero(t, ready.Turns)
	require.Equal(t, int32(1), calls.Load())
	require.Zero(t, host.usage.upserts)
}

func TestUsageHistoryProjectionDeniedReadDoesNotReplayCachedSelection(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g1"}, nil
	}
	_, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, nil, status.Error(codes.PermissionDenied, "access revoked")
	}
	response, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.False(t, found)
	require.Nil(t, response)
}

func TestUsageHistoryProjectionUncachedPreparingRowsRemainUnavailable(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(999), &pluginsdk.PageInfo{ProjectionStatus: "preparing", ProjectionGeneration: "uncompleted", ProjectionPending: true}, nil
	}
	response, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.False(t, response.Found)
	require.False(t, response.CostKnown)
	require.False(t, response.Saved)
	require.Zero(t, response.Input)
	require.Empty(t, response.ProjectionGeneration)
	require.Equal(t, "uncompleted", response.PendingProjectionGeneration)
}

func TestUsageHistoryProjectionDisabledRefreshKeepsCompletedSelectionDuringLag(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g1"}, nil
	}
	_, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	host.config[configKeyCollect] = false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(2), &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "g1", ProjectionPending: true}, nil
	}
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"entries":[]}`), nil
	}
	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.True(t, response.Saved)
	require.True(t, response.CanonicalPending)
	require.Equal(t, int64(2), response.Input)
	require.Zero(t, calls.Load())
}

func TestUsageHistoryCanonicalKandevTurnsRemainAvailable(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		rows := canonicalFixture(3)
		rows[0].Turns = ptr(3)
		return rows, &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: "canonical-turns"}, nil
	}
	response, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Equal(t, int64(3), response.Turns)
	require.Equal(t, "3", *response.TokenAmounts["turns"])
	require.InDelta(t, 0.01, response.CostPerTurn, 1e-12)
}
