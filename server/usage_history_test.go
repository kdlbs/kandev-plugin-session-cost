package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHistoricalMissingDiagnosticsDoNotRecountBeyondSample(t *testing.T) {
	state := historicalImportState{}
	for pass := 0; pass < 3; pass++ {
		for i := 0; i < 150; i++ {
			markMissingSession(&state, fmt.Sprintf("session-%03d", i))
		}
	}
	require.Equal(t, 150, state.Missing)
	require.Len(t, state.MissingSessionIDs, maxMissingSessionIDs)
}

func TestHistoricalLegacyDiagnosticsAreExplicitlyUnreliable(t *testing.T) {
	p := newPlugin()
	host := &fakeHost{}
	p.SetHost(host)
	require.NoError(t, host.SetState(context.Background(), importStateScope, "ws", importStateKey, map[string]any{
		"status": importStatusFailed, "missing": 300, "cursor": "mutable-offset", "processed": 200,
	}))
	state, found, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.True(t, found)
	response, err := importJSONResponse(200, state)
	require.NoError(t, err)
	require.Contains(t, string(response.Body), `"diagnostics_reliable":false`)
}

// The older assertions inspect the compatibility read DTO. Writes in these
// fixtures enter only through the retained SDK, then appear in canonical reads.
func (h *usageRecordingHost) UsageHistory() pluginsdk.UsageHistoryReader {
	return recordingRetainedHistory{h}
}

type recordingRetainedHistory struct{ host *usageRecordingHost }

func (r recordingRetainedHistory) ListTargets(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
	sessions := r.host.sessions
	if len(sessions) == 0 {
		sessions = []pluginsdk.Session{{ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1"}}
	}
	page := pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixture-snapshot", SnapshotUpperBound: 1}
	for _, session := range sessions {
		page.Targets = append(page.Targets, pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", SubjectBinding: "binding:" + session.ID, TaskID: session.TaskID, SessionID: optionalString(session.ID), ExternalSessionID: optionalString(session.ACPSessionID), SubjectKind: "session", ActorAttributionStatus: "unknown"})
	}
	if query.Limit > 0 && len(page.Targets) > int(query.Limit) {
		page.Targets = page.Targets[:query.Limit]
	}
	return fixtureCurrentTargetPage(query, page), nil
}

func (r recordingRetainedHistory) Report(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
	usage := r.host.usage
	usage.mu.Lock()
	defer usage.mu.Unlock()
	usage.upserts++
	response := pluginsdk.UsageObservationBatchResultV1{Status: pluginsdk.UsageReportStatusAccepted, CollectorNamespace: "fixture-plugin"}
	for _, item := range batch.Observations {
		receipt := pluginsdk.UsageObservationWriteResultV1{Operation: "observation", ItemID: item.ItemID, Status: pluginsdk.UsageObservationStatusAccepted, CurrentRevision: item.Revision, CurrentDigest: item.PayloadDigest}
		if usage.writeStatus != "" {
			receipt.Status = usage.writeStatus
			response.Results = append(response.Results, receipt)
			continue
		}
		sessionID := strings.TrimPrefix(item.SubjectBinding, "binding:")
		taskID, transcript := "task-1", "transcript-1"
		for _, session := range r.host.sessions {
			if session.ID == sessionID {
				taskID = session.TaskID
				transcript = session.ACPSessionID
			}
		}
		legacy := pluginsdk.SessionUsageMeasurement{ID: item.ItemID, WorkspaceID: "ws-1", TaskID: taskID, SessionID: sessionID, TranscriptID: transcript, SourceRecordID: transcript, SourceVersion: item.SourceVersion, Revision: item.Revision, PayloadDigest: item.PayloadDigest, Coverage: "complete", CollectedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if item.ModelID != nil {
			legacy.Model = *item.ModelID
		}
		if item.ModelProvider != nil {
			legacy.Provider = *item.ModelProvider
		}
		if item.Tokens != nil {
			legacy.InputTokens = item.Tokens.InputTokens
			legacy.OutputTokens = item.Tokens.OutputTokens
			legacy.CacheReadTokens = item.Tokens.CacheReadTokens
			legacy.CacheWriteTokens = item.Tokens.CacheWriteTokens
			legacy.ReasoningTokens = item.Tokens.ReasoningTokens
			legacy.TotalTokens = item.Tokens.TotalTokens
		}
		if item.Cost != nil {
			legacy.CostSubcents = item.Cost.Amount
			legacy.CostBasis = string(item.Cost.Basis)
			if item.Cost.Currency != nil {
				legacy.Currency = *item.Cost.Currency
			}
			legacy.CostCoverage = "complete"
			if item.Cost.Amount == nil {
				legacy.CostCoverage = "missing"
			}
		}
		legacy.UsageIdentity = "lifetime:" + transcript
		if item.Coverage != nil && item.Coverage.SourceDate != nil {
			legacy.SourceDate = *item.Coverage.SourceDate
			legacy.CoverageKey = legacy.SourceDate
			legacy.UsageIdentity = "date:" + transcript + ":" + legacy.SourceDate
			legacy.SourceTimezone = *item.Coverage.SourceTimezone
		}
		replaced := false
		for i, old := range usage.items {
			if old.ID == item.ItemID || (old.SourceRecordID == legacy.SourceRecordID && old.UsageIdentity == legacy.UsageIdentity && old.Model == legacy.Model && old.Provider == legacy.Provider) {
				usage.items[i] = legacy
				replaced = true
				break
			}
		}
		if !replaced {
			usage.items = append(usage.items, legacy)
		}
		response.Results = append(response.Results, receipt)
	}
	return response, nil
}

type retainedProtocolFixture struct {
	list   func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error)
	report func(context.Context, pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error)
}

func (f retainedProtocolFixture) ListTargets(ctx context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
	if f.list == nil {
		return targetPage(ctx, q)
	}
	page, err := f.list(ctx, q)
	return fixtureCurrentTargetPage(q, page), err
}

// This fixture models a Host implementing the new exact selector. Deliberate
// malformed/older-Host tests use exactHistoryFixture without this adapter.
func fixtureCurrentTargetPage(query pluginsdk.UsageCollectionTargetQueryV1, page pluginsdk.UsageCollectionTargetPageV1) pluginsdk.UsageCollectionTargetPageV1 {
	point := query.CurrentTarget
	if point == nil || page.CurrentTarget {
		return page
	}
	result := pluginsdk.UsageCollectionTargetPageV1{CurrentTarget: true, Preparing: page.Preparing}
	for _, target := range page.Targets {
		if (point.TaskID != "" && point.TaskID != target.TaskID) ||
			(point.SessionID != "" && (target.SessionID == nil || point.SessionID != *target.SessionID)) ||
			(point.SubjectBinding != "" && point.SubjectBinding != target.SubjectBinding) ||
			(point.ExternalSessionID != "" && target.ExternalSessionID != nil && point.ExternalSessionID != *target.ExternalSessionID) {
			continue
		}
		result.Targets = append(result.Targets, target)
	}
	return result
}
func (f retainedProtocolFixture) Report(ctx context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
	return f.report(ctx, b)
}

type retainedFixtureHost struct {
	*usageRecordingHost
	reader pluginsdk.UsageHistoryReader
}

func (h *retainedFixtureHost) UsageHistory() pluginsdk.UsageHistoryReader { return h.reader }
func historyPlugin(t *testing.T, reader pluginsdk.UsageHistoryReader) (*plugin, *retainedFixtureHost) {
	t.Helper()
	host := &retainedFixtureHost{usageRecordingHost: &usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")}}, usage: &usageRecordingReader{}}, reader: reader}
	p := newPlugin()
	p.lookPath = noLookPath
	p.UnimplementedPlugin.SetHost(host)
	p.importWait = func(context.Context) bool { return true }
	t.Cleanup(p.Close)
	return p, host
}
func testTarget() pluginsdk.UsageCollectionTargetV1 {
	return pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", SubjectBinding: "retained-core-binding", TaskID: "task-1", SessionID: optionalString("session-1"), ExternalSessionID: optionalString("transcript-1"), ActorAttributionStatus: "unknown"}
}
func targetPage(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
	return fixtureCurrentTargetPage(q, pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 17, Targets: []pluginsdk.UsageCollectionTargetV1{testTarget()}}), nil
}
func acceptedBatch(batch pluginsdk.UsageObservationBatchV1) pluginsdk.UsageObservationBatchResultV1 {
	result := pluginsdk.UsageObservationBatchResultV1{Status: "accepted", CollectorNamespace: "host-owned-source"}
	for _, item := range batch.Observations {
		result.Results = append(result.Results, pluginsdk.UsageObservationWriteResultV1{Operation: "observation", ItemID: item.ItemID, Status: "accepted", CurrentRevision: item.Revision, CurrentDigest: item.PayloadDigest})
	}
	return result
}
func parsedUsage(t *testing.T, payload string) *sessionCostResponse {
	t.Helper()
	var report sessionModelsReport
	require.NoError(t, json.Unmarshal([]byte(payload), &report))
	response := responseForEntries("session-1", "transcript-1", report.Entries, 1, 10, time.Now())
	return &response
}

func TestUsageHistoryAdapterPreservesExactNullableAndSourceDimensions(t *testing.T) {
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"exact/Model:ID","source":"codex","input":0,"output":9223372036854775807,"reasoning":2,"total":9223372036854775807,"cost":0.00005}]}`)
	rows, err := (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	require.Equal(t, "exact/Model:ID", *row.ModelID)
	require.Nil(t, row.ModelProvider)
	require.Nil(t, row.AgentClient, "session/model grouping does not prove a uniform client")
	require.Equal(t, int64(0), *row.Tokens.InputTokens)
	require.Equal(t, int64(math.MaxInt64), *row.Tokens.OutputTokens)
	require.Nil(t, row.Tokens.CacheReadTokens)
	require.Nil(t, row.Tokens.CacheWriteTokens)
	require.Equal(t, int64(1), *row.Cost.Amount)
	require.Equal(t, pluginsdk.UsageCostBasisEstimated, row.Cost.Basis)
	require.Nil(t, row.Cost.PricingProvenance)
	require.Nil(t, row.TokenSemantics)
	require.Nil(t, row.WorkIdentity)
	require.Equal(t, map[string]string{"source": "codex"}, row.RawSourceLabels)
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"output":null`)
	require.Contains(t, string(encoded), `"output":"9223372036854775807"`)
	require.Contains(t, string(encoded), `"cache_read":null`)
}

func TestUsageHistoryAdapterCostOverflowAndUnknownTotal(t *testing.T) {
	for _, cost := range []string{"-0.1", "922337203685477.5808"} {
		var entry sessionModelEntry
		require.Error(t, json.Unmarshal([]byte(`{"sessionId":"t","model":"m","cost":`+cost+`}`), &entry))
	}
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":20,"output":10,"cacheRead":4,"cacheWrite":0,"reasoning":3,"cost":922337203685477.5807}]}`)
	rows, err := (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{})
	require.NoError(t, err)
	require.Nil(t, rows[0].Tokens.TotalTokens, "unknown subset relationships cannot manufacture a total")
	require.Equal(t, int64(math.MaxInt64), *rows[0].Cost.Amount)
}

func TestUsageHistoryAdapterSourceDayUsesDSTHalfOpenInterval(t *testing.T) {
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":0}]}`)
	rows, err := (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{Date: "2026-10-25", Timezone: "Europe/Lisbon"})
	require.NoError(t, err)
	coverage := rows[0].Coverage
	require.Equal(t, "2026-10-25T00:00:00+01:00", *coverage.CoverageStart)
	require.Equal(t, "2026-10-26T00:00:00Z", *coverage.CoverageEnd)
	require.Equal(t, "2026-10-25", *coverage.SourceDate)
	require.Nil(t, coverage.OccurrenceAt)
	require.False(t, rows[0].Quality.CostCoverageComplete)
	require.False(t, rows[0].Quality.TokenCoverageComplete)
}

func TestUsageHistoryReplaysUncertainWriteAfterRestart(t *testing.T) {
	var batches []pluginsdk.UsageObservationBatchV1
	reader := retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, b)
		if len(batches) == 1 {
			return pluginsdk.UsageObservationBatchResultV1{}, status.Error(codes.Unavailable, "response lost after admission")
		}
		return acceptedBatch(b), nil
	}}
	p, host := historyPlugin(t, reader)
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":7,"total":7}]}`)
	require.Error(t, p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response))
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(host)
	defer restarted.Close()
	require.NoError(t, restarted.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response))
	require.Len(t, batches, 2)
	require.Equal(t, batches[0], batches[1])
	require.Equal(t, int64(1), batches[1].Observations[0].Revision)
	require.NoError(t, restarted.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response))
	require.Len(t, batches, 2)
}

func TestUsageHistoryDimensionCorrectionAndPartialAbsence(t *testing.T) {
	var batches []pluginsdk.UsageObservationBatchV1
	p, _ := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, b)
		return acceptedBatch(b), nil
	}})
	first := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","sourceRecordId":"row-1","model":"old","input":10},{"sessionId":"transcript-1","sourceRecordId":"other","model":"other","input":5}]}`)
	second := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","sourceRecordId":"row-1","model":"corrected","modelProvider":"openai","input":8}]}`)
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), first, usageCoverage{}))
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), second, usageCoverage{}))
	require.Len(t, batches, 2)
	require.Len(t, batches[1].Observations, 1)
	require.Empty(t, batches[1].Tombstones)
	require.Nil(t, batches[1].CompleteSetRevision)
	corrected := batches[1].Observations[0]
	require.Equal(t, int64(2), corrected.Revision)
	require.Equal(t, "corrected", *corrected.ModelID)
	for _, old := range batches[0].Observations {
		if old.SourceRecordID == "row-1" {
			require.Equal(t, old.ItemID, corrected.ItemID)
		}
	}
	third := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","sourceRecordId":"new-record","supersedesRecordId":"row-1","model":"replacement","input":8}]}`)
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), third, usageCoverage{}))
	require.Equal(t, corrected.ItemID, *batches[2].Observations[0].SupersedesItemID)
}

func TestUsageHistoryCanonicalReadbackNeverDisplaysUnselectedSubmission(t *testing.T) {
	for _, hasSaved := range []bool{false, true} {
		t.Run(fmt.Sprint(hasSaved), func(t *testing.T) {
			p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
				return acceptedBatch(b), nil
			}})
			if hasSaved {
				host.usage.items = []pluginsdk.SessionUsageMeasurement{{TaskID: "task-1", SessionID: "session-1", CostSubcents: ptr(200), InputTokens: ptr(2), Coverage: "complete"}}
			}
			p.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"cost":999}]}`), nil)
			response, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
			require.NoError(t, err)
			require.NotEqual(t, int64(999), response.Input)
			require.NotEqual(t, float64(999), response.Cost)
			if hasSaved {
				require.True(t, response.Saved)
				require.Equal(t, int64(2), response.Input)
			} else {
				require.False(t, response.Found)
				require.True(t, response.CanonicalPending)
				require.False(t, response.CostKnown)
			}
		})
	}
}

func TestUsageHistoryOlderHostUpgradeKeepsCanonicalValue(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.Unimplemented, "old host")
	}})
	host.usage.items = []pluginsdk.SessionUsageMeasurement{{TaskID: "task-1", SessionID: "session-1", CostSubcents: ptr(200), Coverage: "complete"}}
	p.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"m","cost":9}]}`), nil)
	response, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, "upgrade_required", response.Compatibility)
	require.True(t, response.Saved)
	require.InDelta(t, 0.02, response.Cost, 1e-9)
	require.Zero(t, host.usage.upserts)
}

func TestUsageHistoryBatchesAreBoundedAndValidateReceipts(t *testing.T) {
	var sizes []int
	p, _ := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		sizes = append(sizes, len(b.Observations))
		return acceptedBatch(b), nil
	}})
	var rows []pluginsdk.UsageObservationV1
	for i := 0; i < 451; i++ {
		rows = append(rows, pluginsdk.UsageObservationV1{SubjectBinding: "binding", ItemID: fmt.Sprint(i), SourceRecordID: fmt.Sprint(i), Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(math.MaxInt64)}})
	}
	require.NoError(t, p.reportUsageObservations(context.Background(), rows))
	require.Equal(t, []int{200, 200, 51}, sizes)
	p2, _ := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		r := acceptedBatch(b)
		r.Results[0].CurrentDigest = "wrong"
		return r, nil
	}})
	require.ErrorContains(t, p2.reportUsageObservations(context.Background(), rows[:1]), "receipt")
}

func TestHistoricalUsageHistoryFixedSnapshotMissingCount(t *testing.T) {
	var queries []pluginsdk.UsageCollectionTargetQueryV1
	reader := retainedProtocolFixture{list: func(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		if q.Limit == 1 {
			require.Equal(t, "fixed", q.SnapshotID)
			return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 150}, nil
		}
		queries = append(queries, q)
		offset := 0
		if q.Cursor != "" {
			var err error
			offset, err = strconv.Atoi(q.Cursor)
			require.NoError(t, err)
		}
		page := pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "fixed", SnapshotUpperBound: 150, HasMore: offset < 100, NextCursor: strconv.Itoa(offset + 50)}
		for i := offset; i < offset+50; i++ {
			page.Targets = append(page.Targets, pluginsdk.UsageCollectionTargetV1{SourceScopeStatus: "eligible", SubjectBinding: fmt.Sprint(i), TaskID: fmt.Sprint(i)})
		}
		return page, nil
	}}
	p, _ := historyPlugin(t, reader)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, Status: importStatusRunning}))
	p.runHistoricalImport(context.Background(), "ws")
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCompleted, state.Status)
	require.Equal(t, 150, state.Missing)
	require.Equal(t, 150, state.Processed)
	require.Len(t, state.MissingSessionIDs, 100)
	require.Empty(t, state.PageMissingIDs)
	require.Len(t, queries, 3)
	require.Empty(t, queries[0].SnapshotID)
	require.Equal(t, "fixed", queries[1].SnapshotID)
	require.Equal(t, "50", queries[1].Cursor)
	require.Equal(t, "100", queries[2].Cursor)
}

func TestHistoricalUsageHistoryResumeRetainsDeletedTargetAndPendingDate(t *testing.T) {
	t.Setenv("TZ", "UTC")
	var queries []pluginsdk.UsageCollectionTargetQueryV1
	var sourceQueries []pluginsdk.UsageCollectionTargetQueryV1
	var writes []pluginsdk.UsageObservationV1
	reader := retainedProtocolFixture{list: func(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		if q.Limit == 1 {
			require.Equal(t, "immutable", q.SnapshotID)
			return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "immutable", SnapshotUpperBound: 2}, nil
		}
		if q.CurrentTarget != nil {
			require.Empty(t, q.SnapshotID, "current source authorization must not trust checkpoint eligibility")
			require.Empty(t, q.Cursor)
			require.Equal(t, testTarget().SubjectBinding, q.CurrentTarget.SubjectBinding)
			sourceQueries = append(sourceQueries, q)
			return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "immutable", SnapshotUpperBound: 2, Targets: []pluginsdk.UsageCollectionTargetV1{testTarget()}}, nil
		}
		require.Equal(t, importPageSize, q.Limit)
		queries = append(queries, q)
		page := pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "immutable", SnapshotUpperBound: 2}
		if q.Cursor == "" {
			require.Empty(t, q.SnapshotID)
			page.Targets = []pluginsdk.UsageCollectionTargetV1{testTarget()}
			page.HasMore = true
			page.NextCursor = "after-first"
		} else {
			require.Equal(t, "immutable", q.SnapshotID)
			require.Equal(t, "after-first", q.Cursor)
			page.Targets = []pluginsdk.UsageCollectionTargetV1{{SourceScopeStatus: "eligible", SubjectBinding: "second-missing", TaskID: "deleted-task"}}
		}
		return page, nil
	}, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		writes = append(writes, b.Observations...)
		return acceptedBatch(b), nil
	}}
	p, host := historyPlugin(t, reader)
	host.sessions = []pluginsdk.Session{{ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", StartedAt: "2026-10-24T12:00:00Z", UpdatedAt: "2026-10-25T12:00:00Z", State: "COMPLETED"}}
	p.run = modelsRunner([]byte(`{"entries":[]}`), nil)
	p.importWait = func(context.Context) bool { return false }
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, Status: importStatusRunning, RunID: "run-1"}))
	p.runHistoricalImport(context.Background(), "ws")
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCancelled, state.Status)
	require.Zero(t, state.Processed)
	require.Zero(t, state.Missing)
	require.Equal(t, []string{"2026-10-25"}, state.Dates)
	require.Len(t, state.PageTargets, 1)
	// Deleting the live task/session must not cause mutable rediscovery of this page.
	host.sessionsFn = func(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, error) {
		t.Fatal("resumed page queried live sessions")
		return nil, nil
	}
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(host)
	defer restarted.Close()
	restarted.lookPath = noLookPath
	restarted.importWait = func(context.Context) bool { return true }
	restarted.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":3}]}`), nil)
	state.Status = importStatusRunning
	state.RunID = "run-2"
	require.NoError(t, restarted.writeHistoricalImportState(context.Background(), "ws", state, true))
	restarted.runHistoricalImport(context.Background(), "ws")
	state, _, err = restarted.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCompleted, state.Status)
	require.Equal(t, 2, state.Processed)
	require.Equal(t, 1, state.Missing)
	require.Len(t, queries, 2)
	require.Len(t, sourceQueries, 4, "each dated report obtains current source authorization before and after calculation")
	require.Len(t, writes, 1)
	require.Equal(t, testTarget().SubjectBinding, writes[0].SubjectBinding)
	require.Equal(t, "2026-10-25", *writes[0].Coverage.SourceDate)
}

func TestUsageHistoryRetainedBindingSurvivesLiveSessionDeletion(t *testing.T) {
	calls := 0
	p, host := historyPlugin(t, retainedProtocolFixture{list: func(ctx context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		calls++
		return targetPage(ctx, q)
	}, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		return acceptedBatch(b), nil
	}})
	target, err := p.resolveUsageTarget(context.Background(), "ws", "task-1", "session-1", "transcript-1")
	require.NoError(t, err)
	host.reader = retainedProtocolFixture{list: func(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		require.Empty(t, q.SnapshotID, "deletion corrections use the bounded retained-binding lookup")
		require.Empty(t, q.Cursor)
		require.Equal(t, &pluginsdk.UsageCollectionCurrentTargetQueryV1{TaskID: "task-1", SessionID: "session-1", ExternalSessionID: "transcript-1", SubjectBinding: target.SubjectBinding}, q.CurrentTarget)
		return pointTargetPage(target), nil
	}, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		return acceptedBatch(b), nil
	}}
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(host)
	defer restarted.Close()
	cached, err := restarted.resolveUsageTarget(context.Background(), "ws", "task-1", "session-1", "transcript-1")
	require.NoError(t, err)
	require.Equal(t, target, cached)
	require.Equal(t, 1, calls)
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":4}]}`)
	require.NoError(t, restarted.persistTargetUsage(context.Background(), cached, response, usageCoverage{}))
}

type fixtureUsageAdapter []pluginsdk.UsageObservationV1

func (f fixtureUsageAdapter) observations(target pluginsdk.UsageCollectionTargetV1, _ *sessionCostResponse, _ usageCoverage) ([]pluginsdk.UsageObservationV1, error) {
	rows := append([]pluginsdk.UsageObservationV1(nil), f...)
	for i := range rows {
		rows[i].SubjectBinding = target.SubjectBinding
	}
	return rows, nil
}
func TestUsageHistoryReplacementAdapterUsesGenericContract(t *testing.T) {
	var actual pluginsdk.UsageObservationV1
	p, _ := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		actual = b.Observations[0]
		return acceptedBatch(b), nil
	}})
	p.adapter = fixtureUsageAdapter{{ItemID: "independent-source-record", SourceRecordID: "provider-event-7", SourceVersion: "fixture-collector-1", WorkIdentity: &pluginsdk.UsageWorkIdentityV1{Scheme: "provider_account_thread", Authority: optionalString("account-7"), ID: "thread-7"}, ModelID: optionalString("exact-id"), AgentClient: optionalString("another-client"), Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(0)}, Cost: &pluginsdk.UsageCostV1{Basis: pluginsdk.UsageCostBasisUnknown}, Quality: &pluginsdk.UsageObservationQualityV1{TokenCoverageComplete: false}}}
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("generic adapter started tokscale")
		return nil, nil
	}
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), &sessionCostResponse{}, usageCoverage{}))
	require.Equal(t, testTarget().SubjectBinding, actual.SubjectBinding)
	require.Equal(t, "fixture-collector-1", actual.SourceVersion)
	require.Equal(t, "thread-7", actual.WorkIdentity.ID)
	require.Nil(t, actual.ModelProvider)
	require.Nil(t, actual.Cost.Amount)
}

func TestUsageHistoryRejectsUnsupportedVersionsAndMismatchedTargets(t *testing.T) {
	for _, failure := range []string{"rpc", "schema", "item"} {
		t.Run(failure, func(t *testing.T) {
			p, _ := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
				result := acceptedBatch(b)
				switch failure {
				case "rpc":
					return result, status.Error(codes.Unimplemented, "old host")
				case "schema":
					result.Status = "unsupported_version"
				case "item":
					result.Results[0].Status = "unsupported_version"
				}
				return result, nil
			}})
			response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":1}]}`)
			require.ErrorIs(t, p.persistTargetUsage(context.Background(), testTarget(), response, usageCoverage{}), errUsageHistoryUnsupported)
		})
	}
	_, err := usageHistory(&fakeHost{})
	require.ErrorIs(t, err, errUsageHistoryUnsupported)
}

func TestUsageHistoryDisabledOrCancelledCollectionDoesNotWrite(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(context.Context, pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		t.Fatal("disabled or cancelled write")
		return pluginsdk.UsageObservationBatchResultV1{}, nil
	}})
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":1}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, p.persistTargetUsage(ctx, testTarget(), response, usageCoverage{}), context.Canceled)
	host.config[configKeyCollect] = false
	require.ErrorIs(t, p.persistTargetUsage(context.Background(), testTarget(), response, usageCoverage{}), errUsageCollectionDisabled)
}

func TestCollectorUsageHistoryFinalizesRetainedTargetAfterDeletion(t *testing.T) {
	t.Setenv("TZ", "UTC")
	var observations []pluginsdk.UsageObservationV1
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		observations = append(observations, b.Observations...)
		return acceptedBatch(b), nil
	}})
	host.sessions = nil
	fixed := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return fixed }
	checkpoint := collectorCheckpoint{Initialized: true, LastScanAt: fixed.Format(time.RFC3339Nano), Pending: map[string]collectorPending{"session-1": {SubjectBinding: testTarget().SubjectBinding, WorkspaceID: "ws", TaskID: "task-1", ACPSessionID: "transcript-1", State: "RUNNING", UpdatedAt: fixed.Format(time.RFC3339Nano), Dates: []string{"2026-10-28"}}}}
	require.NoError(t, p.saveCollectorCheckpoint(context.Background(), host, checkpoint))
	p.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":1,"total":1,"cost":0.1}]}`), nil)
	for i := 0; i < 6; i++ {
		require.NoError(t, p.collectOnce(context.Background()))
	}
	require.Len(t, observations, 2, "lifetime and source day are each admitted once while final reports stabilize")
	for _, item := range observations {
		require.Equal(t, testTarget().SubjectBinding, item.SubjectBinding)
	}
	after, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.Empty(t, after.Pending)
}

func TestUsageHistoryConflictingSourceReceiptIsNotCanonical(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{list: targetPage, report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		result := acceptedBatch(b)
		current := b.Observations[0]
		current.Revision = 9
		current.Tokens = &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(777)}
		current.PayloadDigest, _ = current.ComputePayloadDigestV1()
		result.Results[0].Status = "conflict"
		result.Results[0].CurrentObservation = &current
		result.Results[0].CurrentRevision = current.Revision
		result.Results[0].CurrentDigest = current.PayloadDigest
		return result, nil
	}})
	host.usage.items = []pluginsdk.SessionUsageMeasurement{{TaskID: "task-1", SessionID: "session-1", InputTokens: ptr(2), Coverage: "complete"}}
	p.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999}]}`), nil)
	response, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, int64(2), response.Input)
	require.True(t, response.Saved)
	require.True(t, response.Stale)
}

func TestUsageHistoryDisplayAggregationDoesNotOverflowOrInventUnknowns(t *testing.T) {
	items := []pluginsdk.SessionUsageMeasurement{
		{Model: "m1", InputTokens: ptr(math.MaxInt64), OutputTokens: ptr(0), CostSubcents: ptr(1), Currency: "EUR", Coverage: "complete"},
		{Model: "m2", InputTokens: ptr(math.MaxInt64), CostSubcents: ptr(0), Currency: "USD", Coverage: "complete"},
	}
	response := responseFromSaved(items, "session", 1, 10)
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"input":"18446744073709551614"`)
	require.Contains(t, string(encoded), `"input":null`)
	require.Contains(t, string(encoded), `"output":null`)
	require.False(t, response.CostKnown, "non-USD costs cannot pass through the USD formatter")
	require.Equal(t, "mixed", response.Currency)
	var report sessionModelsReport
	require.NoError(t, json.Unmarshal([]byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":1,"cost":922337203685477.5807}]}`), &report))
	raw := responseForEntries("session-1", "transcript-1", report.Entries, 1, 10, time.Now())
	require.False(t, raw.CostKnown)
	require.Zero(t, raw.Cost)
	require.Equal(t, "9223372036854775807", *raw.Models[0].ExactCost)
}

func TestHistoricalCancellationDoesNotAllowOldCheckpointToReviveImport(t *testing.T) {
	p, _ := historyPlugin(t, retainedProtocolFixture{})
	state := historicalImportState{SchemaVersion: 2, Status: importStatusRunning, RunID: "old"}
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", state))
	cancelled := state
	cancelled.Status = importStatusCancelled
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", cancelled))
	require.ErrorIs(t, p.saveHistoricalImportState(context.Background(), "ws", state), context.Canceled)
	next := state
	next.RunID = "new"
	require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", next, true))
	p.finishHistoricalImportRun("ws", importStatusFailed, errors.New("old worker failed"), "old")
	require.ErrorIs(t, p.saveHistoricalImportState(context.Background(), "ws", state), context.Canceled)
	current, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, "new", current.RunID)
	require.Equal(t, importStatusRunning, current.Status)
}

func TestHistoricalUsageHistoryRejectsChangingSnapshot(t *testing.T) {
	p, _ := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "different", SnapshotUpperBound: 7}, nil
	}})
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{SchemaVersion: 2, DiagnosticsReliable: true, Status: importStatusRunning, SnapshotID: "fixed", SnapshotUpperBound: 7, Cursor: "keyset"}))
	p.runHistoricalImport(context.Background(), "ws")
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusFailed, state.Status)
	require.Contains(t, state.LastError, "changed the usage target snapshot")
	require.Zero(t, state.Processed)
}

func TestCollectorUsageHistoryDisableDuringReportPreservesPendingWork(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(context.Context, pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		t.Fatal("collection wrote after disablement")
		return pluginsdk.UsageObservationBatchResultV1{}, nil
	}})
	now := time.Now().UTC()
	checkpoint := collectorCheckpoint{Initialized: true, LastScanAt: now.Format(time.RFC3339Nano), Pending: map[string]collectorPending{"session-1": {SubjectBinding: testTarget().SubjectBinding, WorkspaceID: "ws", TaskID: "task-1", ACPSessionID: "transcript-1", State: "RUNNING", Dates: []string{collectionDate(now)}}}}
	host.sessions[0].State = "RUNNING"
	require.NoError(t, p.saveCollectorCheckpoint(context.Background(), host, checkpoint))
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		host.config[configKeyCollect] = false
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":10}]}`), nil
	}
	require.NoError(t, p.collectOnce(context.Background()))
	after, err := p.loadCollectorCheckpoint(context.Background(), host)
	require.NoError(t, err)
	require.Contains(t, after.Pending, "session-1")
	require.Equal(t, 0, after.Pending["session-1"].Attempts)
	require.Equal(t, checkpoint.Pending["session-1"].Dates, after.Pending["session-1"].Dates)
}

func TestUsageHistoryOrdinaryDuplicateModelGroupsNeedExplicitDecomposition(t *testing.T) {
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","provider":"provider-a","input":1},{"sessionId":"transcript-1","model":"m","provider":"provider-b","input":2}]}`)
	_, err := (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{})
	require.ErrorIs(t, err, errTokscaleMembership)
	// A producer that repeats one explicit identity for two rows is still
	// ambiguous; a dimension label cannot silently override that identity.
	response = parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","sourceRecordId":"same","model":"m","provider":"provider-a","input":1},{"sessionId":"transcript-1","sourceRecordId":"same","model":"m","provider":"provider-b","input":2}]}`)
	_, err = (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{})
	require.ErrorContains(t, err, "ambiguous duplicate")
}
