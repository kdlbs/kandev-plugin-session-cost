package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

// importEngineFixture serves one immutable snapshot in pages and records every
// source report and admitted batch.
type importEngineFixture struct {
	targets    []pluginsdk.UsageCollectionTargetV1
	pageSize   int
	batches    []pluginsdk.UsageObservationBatchV1
	reports    []string
	onReport   func(context.Context)
	reportErr  func() error
	dedupeRows bool
}

func (f *importEngineFixture) reader() pluginsdk.UsageHistoryReader {
	return retainedProtocolFixture{list: f.list, report: f.report}
}

func (f *importEngineFixture) list(_ context.Context, q pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
	if q.CurrentTarget != nil {
		return pluginsdk.UsageCollectionTargetPageV1{Targets: f.targets}, nil
	}
	if q.Limit == 1 {
		return pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "engine-snapshot", SnapshotUpperBound: 99}, nil
	}
	offset := 0
	if q.Cursor != "" {
		var err error
		offset, err = strconv.Atoi(q.Cursor)
		if err != nil {
			return pluginsdk.UsageCollectionTargetPageV1{}, err
		}
	}
	end := offset + f.pageSize
	if end > len(f.targets) {
		end = len(f.targets)
	}
	page := pluginsdk.UsageCollectionTargetPageV1{SnapshotID: "engine-snapshot", SnapshotUpperBound: 99, Targets: f.targets[offset:end]}
	if end < len(f.targets) {
		page.HasMore, page.NextCursor = true, strconv.Itoa(end)
	}
	return page, nil
}

func (f *importEngineFixture) report(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
	f.batches = append(f.batches, batch)
	return acceptedBatch(batch), nil
}

func (f *importEngineFixture) run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	scope := "lifetime"
	for i, arg := range args {
		switch arg {
		case "--version":
			return []byte("tokscale 4.15.1\n"), nil
		case "--since":
			scope = args[i+1]
		}
	}
	f.reports = append(f.reports, scope)
	if f.reportErr != nil {
		if err := f.reportErr(); err != nil {
			return nil, err
		}
	}
	if f.onReport != nil {
		f.onReport(ctx)
	}
	rows := make([]map[string]any, 0, len(f.targets))
	for i, target := range f.targets {
		if target.ExternalSessionID == nil {
			continue
		}
		rows = append(rows, map[string]any{
			"sessionId": *target.ExternalSessionID, "model": "model-a",
			"input": i + 1, "output": 1, "total": i + 2, "cost": 0.01,
		})
	}
	if f.dedupeRows && len(rows) > 1 {
		rows = rows[:1]
	}
	// Rows for sessions outside the index must never be written.
	rows = append(rows, map[string]any{"sessionId": "foreign-transcript", "model": "model-a", "input": 999, "cost": 9.9})
	return json.Marshal(map[string]any{"entries": rows})
}

func (f *importEngineFixture) observations() []pluginsdk.UsageObservationV1 {
	var all []pluginsdk.UsageObservationV1
	for _, batch := range f.batches {
		all = append(all, batch.Observations...)
	}
	return all
}

func engineTarget(index int, first, last string) pluginsdk.UsageCollectionTargetV1 {
	target := pluginsdk.UsageCollectionTargetV1{
		SourceScopeStatus: "eligible", SubjectBinding: fmt.Sprintf("binding-%d", index),
		TaskID: fmt.Sprintf("task-%d", index), SessionID: optionalString(fmt.Sprintf("session-%d", index)),
		ExternalSessionID: optionalString(fmt.Sprintf("transcript-%d", index)), SubjectKind: "session",
	}
	if first != "" {
		target.SessionStartedAt = optionalString(first + "T00:00:00Z")
		target.SessionEndedAt = optionalString(last + "T00:00:00Z")
	}
	return target
}

func importEnginePlugin(t *testing.T, fixture *importEngineFixture) *plugin {
	t.Helper()
	p, _ := historyPlugin(t, fixture.reader())
	p.lookPath = noLookPath
	p.run = fixture.run
	p.importWait = func(context.Context) bool { return true }
	return p
}

func TestImportEngineRunsOneLifetimeAndOneReportPerActiveDay(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 4}
	for i := 0; i < 10; i++ {
		switch i % 3 {
		case 0:
			fixture.targets = append(fixture.targets, engineTarget(i, "2026-10-01", "2026-10-01"))
		case 1:
			fixture.targets = append(fixture.targets, engineTarget(i, "2026-10-02", "2026-10-02"))
		default:
			fixture.targets = append(fixture.targets, engineTarget(i, "2026-10-01", "2026-10-03"))
		}
	}
	p := importEnginePlugin(t, fixture)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(context.Background(), "ws")

	require.Equal(t, []string{"lifetime", "2026-10-01", "2026-10-02", "2026-10-03"}, fixture.reports,
		"one lifetime report and exactly one report per active day")
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCompleted, state.Status)
	require.Equal(t, importPhaseCompleted, state.Phase)
	require.Equal(t, 10, state.SessionsFound)
	require.Equal(t, 10, state.SessionsImported)
	require.Zero(t, state.SessionsMissing)
	require.Zero(t, state.SessionsSkipped)
	require.Equal(t, 3, state.DaysTotal)
	require.Equal(t, 3, state.DaysDone)
	require.Len(t, state.Intervals, 1)
	require.Equal(t, "2026-10-01", state.Intervals[0].Start)
	require.Equal(t, "2026-10-03", state.Intervals[0].End)

	observations := fixture.observations()
	for _, item := range observations {
		require.NotContains(t, item.SubjectBinding, "foreign", "rows outside the index are ignored")
	}
	// Each session writes its lifetime totals once; dated usage follows the
	// session's trusted source range.
	lifetime, dated := 0, 0
	for _, item := range observations {
		if item.Coverage == nil {
			lifetime++
			continue
		}
		dated++
	}
	require.Equal(t, 10, lifetime)
	require.Equal(t, 16, dated, "4 one-day, 3 one-day and 3 three-day sessions")
}

func TestImportEngineSkipsIneligibleTargetAndKeepsCounting(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
		{SourceScopeStatus: "unknown", SubjectBinding: "binding-unsafe", TaskID: "task-unsafe", SessionID: optionalString("session-unsafe"), ExternalSessionID: optionalString("transcript-unsafe")},
		engineTarget(1, "2026-10-02", "2026-10-02"),
	}}
	p := importEnginePlugin(t, fixture)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(context.Background(), "ws")

	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCompleted, state.Status, "an ineligible session never asks for an upgrade")
	require.Equal(t, 3, state.SessionsFound)
	require.Equal(t, 2, state.SessionsImported)
	require.Equal(t, 1, state.SessionsSkipped)
	for _, item := range fixture.observations() {
		require.NotEqual(t, "binding-unsafe", item.SubjectBinding)
	}
}

func TestImportEngineReclassifiesTargetThatBecomesIneligible(t *testing.T) {
	t.Setenv("TZ", "UTC")
	first := engineTarget(0, "2026-10-01", "2026-10-01")
	second := engineTarget(1, "2026-10-01", "2026-10-01")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{first, second}}
	p := importEnginePlugin(t, fixture)
	revoked := false
	fixture.onReport = func(context.Context) {
		if revoked {
			return
		}
		revoked = true
		fixture.targets[1].SourceScopeStatus, fixture.targets[1].SubjectBinding = "mixed", "binding-revoked"
	}
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(context.Background(), "ws")

	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCompleted, state.Status)
	require.Equal(t, 2, state.SessionsFound)
	require.Equal(t, 1, state.SessionsImported)
	require.Equal(t, 1, state.SessionsSkipped)
	for _, item := range fixture.observations() {
		require.NotEqual(t, second.SubjectBinding, item.SubjectBinding, "a revoked target never reaches a write")
	}
}

func TestImportEngineRerunLeavesRevisionsUnchanged(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
	}}
	p := importEnginePlugin(t, fixture)
	ctx := context.Background()
	require.NoError(t, p.saveHistoricalImportState(ctx, "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(ctx, "ws")
	firstBatches := len(fixture.batches)
	require.NotZero(t, firstBatches)

	fixture.batches = nil
	fixture.reports = nil
	require.NoError(t, p.writeHistoricalImportState(ctx, "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-2",
	}, true))
	p.runHistoricalImport(ctx, "ws")
	require.Empty(t, fixture.batches, "unchanged transcripts are not resubmitted, so revisions cannot increase")
	require.Equal(t, []string{"lifetime", "2026-10-01"}, fixture.reports)
}

func TestImportEngineSingleReportSubmissionCarriesNoForeignRows(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "2026-10-01", "2026-10-01"),
		engineTarget(1, "2026-10-01", "2026-10-01"),
	}}
	p := importEnginePlugin(t, fixture)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(context.Background(), "ws")
	observations := fixture.observations()
	require.Len(t, observations, 4, "one lifetime and one dated observation per session")
	for _, item := range observations {
		require.NotContains(t, item.RawSourceLabels["client"], "foreign")
		require.NotContains(t, item.SubjectBinding, "foreign")
	}
}

func TestImportEngineIndexesSharedTranscriptPerSession(t *testing.T) {
	t.Setenv("TZ", "UTC")
	first := engineTarget(0, "2026-10-01", "2026-10-01")
	second := engineTarget(1, "2026-10-03", "2026-10-03")
	second.ExternalSessionID = first.ExternalSessionID
	fixture := &importEngineFixture{pageSize: 10, dedupeRows: true, targets: []pluginsdk.UsageCollectionTargetV1{first, second}}
	p := importEnginePlugin(t, fixture)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(context.Background(), "ws")

	require.Equal(t, []string{"lifetime", "2026-10-01", "2026-10-03"}, fixture.reports,
		"both sessions sharing one transcript keep their active days")
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, 2, state.SessionsFound)
	require.Equal(t, 2, state.SessionsImported)
	require.Zero(t, state.SessionsMissing)
	require.Zero(t, state.SessionsSkipped)
	require.Equal(t, 2, state.DaysTotal)
	require.Equal(t, 2, state.DaysDone)
	require.Len(t, state.Intervals, 2)

	bindings := map[string]int{}
	for _, item := range fixture.observations() {
		bindings[item.SubjectBinding]++
	}
	require.Equal(t, 2, bindings[first.SubjectBinding])
	require.Equal(t, 2, bindings[second.SubjectBinding])
}

func TestImportEngineOpenSessionRangeExtendsToToday(t *testing.T) {
	t.Setenv("TZ", "UTC")
	target := engineTarget(0, "2026-10-02", "")
	target.SessionEndedAt = nil
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{target}}
	p := importEnginePlugin(t, fixture)
	p.now = func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(context.Background(), "ws")

	require.Equal(t, []string{"lifetime", "2026-10-02", "2026-10-03", "2026-10-04"}, fixture.reports)
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, 3, state.DaysTotal)
	require.Equal(t, 3, state.DaysDone)
	require.False(t, state.Undated, "an open session with a known start has a daily range")
}

func TestImportEngineMarksOnlyLifetimeTargetUndated(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fixture := &importEngineFixture{pageSize: 10, targets: []pluginsdk.UsageCollectionTargetV1{
		engineTarget(0, "", ""),
	}}
	p := importEnginePlugin(t, fixture)
	require.NoError(t, p.saveHistoricalImportState(context.Background(), "ws", historicalImportState{
		SchemaVersion: importSchemaVersion, Status: importStatusRunning, RunID: "run-1",
	}))
	p.runHistoricalImport(context.Background(), "ws")
	state, _, err := p.loadHistoricalImportState(context.Background(), "ws")
	require.NoError(t, err)
	require.Equal(t, importStatusCompleted, state.Status)
	require.True(t, state.Undated, "a target with no known date range is undated")
	require.Zero(t, state.DaysTotal)
}
