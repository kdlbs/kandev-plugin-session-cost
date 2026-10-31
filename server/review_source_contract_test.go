package main

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

func eligibleTestTarget() pluginsdk.UsageCollectionTargetV1 {
	target := testTarget()
	target.SourceScopeStatus = "eligible"
	return target
}

func TestReviewSourceContractOnlyExplicitEligibleAllowsRawUsage(t *testing.T) {
	for _, scope := range []string{"", "unknown", "mixed", "ELIGIBLE"} {
		for _, path := range []string{"responsive", "synchronous", "legacy", "collector", "adapter"} {
			t.Run(scope+"/"+path, func(t *testing.T) {
				target := eligibleTestTarget()
				target.SourceScopeStatus = scope
				p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
					return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "current", SnapshotUpperBound: 1, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
				}})
				p.UnimplementedPlugin.SetHost(sdkBrokerHost(t, sourceContextTestHost{host}))
				host.config[configKeyCollect] = path == "collector"
				host.sessions[0].State = "RUNNING"
				host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
					return nil, canonicalReadyPage("allowed-new-org", ""), nil
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
				case "collector":
					err = p.collectOnce(context.Background())
				case "adapter":
					_, err = (tokscaleUsageAdapter{}).observations(target, parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","input":999}]}`), usageCoverage{})
				}
				if response != nil {
					require.NoError(t, err)
					require.False(t, response.Found)
					require.False(t, response.CostKnown)
					require.Equal(t, "source_context_unavailable", response.Compatibility)
				} else {
					require.Error(t, err)
				}
				require.Zero(t, runs.Load(), "live session metadata is not permission to read an unsafe aggregate")
			})
		}
	}
}

func TestReviewSourceContractCachedEligibilityCannotSuppressCurrentCheck(t *testing.T) {
	var unsafe atomic.Bool
	var reports atomic.Int32
	var queries atomic.Int32
	p, _ := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		queries.Add(1)
		target := eligibleTestTarget()
		if unsafe.Load() {
			target.SourceScopeStatus = "mixed"
			target.ExternalSessionID = nil
		}
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 17, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
	}, report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		reports.Add(1)
		return acceptedBatch(batch), nil
	}})
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":7}]}`)
	require.NoError(t, p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response))
	before := queries.Load()
	unsafe.Store(true)
	err := p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response)
	require.Error(t, err, "an accepted unchanged receipt cannot bypass a new unsafe source decision")
	require.Greater(t, queries.Load(), before)
	require.Equal(t, int32(1), reports.Load())
}

func TestReviewSourceContractRPCImportPreparationRepeatsOnlyInitialQuery(t *testing.T) {
	var mu sync.Mutex
	var queries []pluginsdk.UsageCollectionTargetQueryV1
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, query)
		if len(queries) <= 2 {
			return pluginsdk.UsageCollectionTargetPageV1{Preparing: true}, nil
		}
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "prepared", SnapshotUpperBound: 7, Targets: []pluginsdk.UsageCollectionTargetV1{eligibleTestTarget()}}, nil
	}})
	p.UnimplementedPlugin.SetHost(sdkBrokerHost(t, host))
	reader, err := usageHistory(p.Host())
	require.NoError(t, err)
	page, err := p.readUsageTargetPage(context.Background(), reader, pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: "ws", Limit: importPageSize}, 0)
	require.NoError(t, err)
	require.Equal(t, "eligible", page.Targets[0].SourceScopeStatus)
	mu.Lock()
	require.Len(t, queries, 3)
	for _, query := range queries {
		require.Empty(t, query.SnapshotID)
		require.Empty(t, query.Cursor)
		require.Equal(t, queries[0], query)
	}
	mu.Unlock()
}

func TestReviewSourceContractPointPreparationIsRejectedWithoutStartingSource(t *testing.T) {
	var calls atomic.Int32
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		calls.Add(1)
		return pluginsdk.UsageCollectionTargetPageV1{Preparing: true}, nil
	}})
	host.config[configKeyCollect] = false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, canonicalReadyPage("current-empty", ""), nil
	}
	var runs atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) { runs.Add(1); return nil, nil }
	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, "source_context_unavailable", response.Compatibility)
	require.Equal(t, reportStateFailed, response.ReportState)
	require.False(t, response.Found)
	require.False(t, response.CostKnown)
	require.Equal(t, int32(1), calls.Load(), "a point lookup cannot fall back to discovery preparation")
	require.Zero(t, runs.Load())
}

func TestReviewSourceContractImportPreparingNeverCertifiesEmptyMembership(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		return pluginsdk.UsageCollectionTargetPageV1{Preparing: true}, nil
	}})
	p.importWait = func(ctx context.Context) bool { <-ctx.Done(); return false }
	var runs atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) { runs.Add(1); return nil, nil }
	response, err := p.startHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, 202, response.Status)
	var view map[string]any
	require.NoError(t, json.Unmarshal(response.Body, &view))
	require.Equal(t, "preparing", view["status"])
	require.Zero(t, view["processed"])
	state, found, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, state.SnapshotID)
	require.Empty(t, state.PageTargets)
	require.Empty(t, state.Cursor)
	require.Zero(t, state.SnapshotUpperBound)
	status, err := p.handleHistoricalImportAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionKeyImportStatus, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws"}})
	require.NoError(t, err)
	require.Contains(t, string(status.Body), `"status":"preparing"`)
	canceled, err := p.cancelHistoricalImport(context.Background(), "ws")
	require.NoError(t, err)
	require.Contains(t, string(canceled.Body), `"status":"cancelled"`)
	require.Zero(t, runs.Load())
	_, found, err = host.GetState(context.Background(), importStateScope, "ws", importStateKey)
	require.NoError(t, err)
	require.True(t, found)
}

func TestReviewSourceContractRevocationDuringReportWithholdsRawSnapshot(t *testing.T) {
	for _, path := range []string{"responsive", "synchronous", "legacy"} {
		t.Run(path, func(t *testing.T) {
			var revoked atomic.Bool
			p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				target := eligibleTestTarget()
				if revoked.Load() {
					target.SourceScopeStatus, target.ExternalSessionID = "mixed", nil
				}
				return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 17, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
			}})
			p.UnimplementedPlugin.SetHost(sourceContextTestHost{host})
			host.config[configKeyCollect] = false
			host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				return nil, canonicalReadyPage("new-scope-empty", ""), nil
			}
			var runs atomic.Int32
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				runs.Add(1)
				revoked.Store(true)
				return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"cost":99}]}`), nil
			}
			var result *sessionCostResponse
			var err error
			switch path {
			case "responsive":
				_, err = p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
				require.NoError(t, err)
				waitForReport(t, p)
				result, err = p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
			case "synchronous":
				result, err = p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
			case "legacy":
				var body []byte
				body, err = p.sessionCost(context.Background(), "task-1", "session-1")
				if err == nil {
					value := decode(t, body)
					result = &value
				}
			}
			require.NoError(t, err)
			require.Equal(t, int32(1), runs.Load())
			require.Equal(t, "source_context_unavailable", result.Compatibility)
			require.False(t, result.Found)
			require.False(t, result.CostKnown)
			require.Zero(t, result.Input)
			require.Empty(t, result.Models)
		})
	}
}

func TestReviewSourceContractUnsafeImportCannotUseLiveDatesOrTranscript(t *testing.T) {
	for _, scope := range []string{"", "unknown", "mixed"} {
		t.Run(scope, func(t *testing.T) {
			target := eligibleTestTarget()
			target.SourceScopeStatus, target.ExternalSessionID = scope, nil
			// Even a legacy checkpoint's date fields cannot authorize a source.
			target.SessionStartedAt, target.SessionEndedAt = optionalString("2026-10-01T00:00:00Z"), optionalString("2026-10-02T00:00:00Z")
			require.Nil(t, trustedTargetDateRange(target))
			p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 17, Targets: []pluginsdk.UsageCollectionTargetV1{target}}, nil
			}})
			host.sessionsFn = func(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, error) {
				t.Fatal("unsafe source context was bypassed through live sessions")
				return nil, nil
			}
			var runs atomic.Int32
			p.run = func(context.Context, string, ...string) ([]byte, error) { runs.Add(1); return nil, nil }
			require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", historicalImportState{SchemaVersion: 2, Status: importStatusRunning, RunID: "unsafe-run"}, true))
			p.runHistoricalImport(context.Background(), "ws")
			state, found, err := p.loadHistoricalImportState(context.Background(), "ws")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, importStatusUnsupported, state.Status)
			require.Zero(t, state.Processed)
			require.Zero(t, state.Missing, "unavailable scope is not a measured missing-source result")
			require.Zero(t, runs.Load())
		})
	}
}

func TestReviewSourceContractPreparingRejectsMembershipAndHonorsCancellation(t *testing.T) {
	initial := pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: "ws", Limit: 1}
	for name, page := range map[string]pluginsdk.UsageCollectionTargetPageV1{
		"snapshot": {Preparing: true, SnapshotID: "invented"},
		"upper":    {Preparing: true, SnapshotUpperBound: 1},
		"target":   {Preparing: true, Targets: []pluginsdk.UsageCollectionTargetV1{eligibleTestTarget()}},
		"cursor":   {Preparing: true, NextCursor: "invented"},
		"has-more": {Preparing: true, HasMore: true},
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, validateTargetPage(initial, 0, page)) })
	}
	require.Error(t, validateTargetPage(pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: "ws", Limit: 1, SnapshotID: "frozen"}, 17, pluginsdk.UsageCollectionTargetPageV1{Preparing: true}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	p, _ := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		calls.Add(1)
		cancel()
		return pluginsdk.UsageCollectionTargetPageV1{Preparing: true}, nil
	}})
	_, err := p.resolveCurrentUsageTarget(ctx, "ws", "task-1", "session-1", "transcript-1")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(1), calls.Load())
}

func TestReviewSourceContractImportRestartsPreparationAndRevalidatesLegacyTarget(t *testing.T) {
	for _, legacyPage := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparing", true: "legacy-page"}[legacyPage], func(t *testing.T) {
			var preparations atomic.Int32
			var reports atomic.Int32
			p, host := historyPlugin(t, retainedProtocolFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				if query.CurrentTarget == nil && query.SnapshotID == "" && preparations.Add(1) <= 2 {
					return pluginsdk.UsageCollectionTargetPageV1{Preparing: true}, nil
				}
				return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "prepared", SnapshotUpperBound: 17, Targets: []pluginsdk.UsageCollectionTargetV1{eligibleTestTarget()}}, nil
			}, report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
				reports.Add(1)
				return acceptedBatch(batch), nil
			}})
			state := historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, RunID: "restart", Status: importStatusPreparing}
			if legacyPage {
				target := eligibleTestTarget()
				target.SourceScopeStatus = "" // A pre-field checkpoint must obtain new authority.
				state.Status, state.SnapshotID, state.SnapshotUpperBound = importStatusRunning, "prepared", 17
				state.PageTargets = []pluginsdk.UsageCollectionTargetV1{target}
				state.PageDates, state.Dates = map[string][]string{target.SubjectBinding: {""}}, []string{""}
			}
			require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", state, true))
			p.Close()
			restarted := migrationPlugin(t, host)
			restarted.importWait = func(context.Context) bool { return true }
			restarted.lookPath = noLookPath
			restarted.run = func(context.Context, string, ...string) ([]byte, error) {
				stored, found, err := restarted.loadHistoricalImportState(context.Background(), "ws")
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, "prepared", stored.SnapshotID)
				require.Len(t, stored.PageTargets, 1, "membership is frozen and checkpointed before external collection")
				require.Zero(t, stored.Processed)
				return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":7}]}`), nil
			}
			restarted.runHistoricalImport(context.Background(), "ws")
			stored, found, err := restarted.loadHistoricalImportState(context.Background(), "ws")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, importStatusCompleted, stored.Status)
			require.Equal(t, 1, stored.Processed)
			require.Zero(t, stored.Missing)
			require.Equal(t, int32(1), reports.Load())
			if legacyPage {
				require.Zero(t, preparations.Load(), "a retained page resumes through bounded eligibility lookup without archive rediscovery")
			} else {
				require.Equal(t, int32(3), preparations.Load())
			}
		})
	}
}
