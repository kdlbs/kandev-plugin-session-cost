package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

type usageRecordingReader struct {
	mu          sync.Mutex
	items       []pluginsdk.SessionUsageMeasurement
	upserts     int
	listings    int
	writeStatus string
}

func (r *usageRecordingReader) UpsertBatch(_ context.Context, workspaceID string, items []pluginsdk.SessionUsageMeasurement) ([]pluginsdk.SessionUsageWriteResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts++
	if r.writeStatus != "" {
		results := make([]pluginsdk.SessionUsageWriteResult, len(items))
		for i, item := range items {
			results[i] = pluginsdk.SessionUsageWriteResult{
				SourceRecordID: item.SourceRecordID, UsageIdentity: item.UsageIdentity,
				Model: item.Model, Provider: item.Provider, Status: r.writeStatus,
			}
			for _, existing := range r.items {
				if existing.SourceRecordID == item.SourceRecordID && existing.UsageIdentity == item.UsageIdentity &&
					existing.Model == item.Model && existing.Provider == item.Provider {
					copy := existing
					results[i].Measurement = &copy
					break
				}
			}
		}
		return results, nil
	}
	for _, item := range items {
		item.WorkspaceID = workspaceID
		found := false
		for i := range r.items {
			if r.items[i].SourceRecordID == item.SourceRecordID && r.items[i].UsageIdentity == item.UsageIdentity && r.items[i].Model == item.Model && r.items[i].Provider == item.Provider {
				r.items[i] = item
				found = true
				break
			}
		}
		if !found {
			r.items = append(r.items, item)
		}
	}
	results := make([]pluginsdk.SessionUsageWriteResult, len(items))
	for i, item := range items {
		copy := item
		results[i] = pluginsdk.SessionUsageWriteResult{
			SourceRecordID: item.SourceRecordID, UsageIdentity: item.UsageIdentity,
			Model: item.Model, Provider: item.Provider, Status: "applied", Measurement: &copy,
		}
	}
	return results, nil
}

func (r *usageRecordingReader) List(_ context.Context, filter pluginsdk.SessionUsageFilter, page pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listings++
	items := make([]pluginsdk.SessionUsageMeasurement, 0, len(r.items))
	for _, item := range r.items {
		if len(filter.TaskIDs) > 0 && !containsString(filter.TaskIDs, item.TaskID) {
			continue
		}
		if len(filter.SessionIDs) > 0 && !containsString(filter.SessionIDs, item.SessionID) {
			continue
		}
		items = append(items, item)
	}
	start := 0
	if page.Cursor != "" {
		start = 1
	}
	if start >= len(items) {
		return []pluginsdk.SessionUsageMeasurement{}, &pluginsdk.PageInfo{}, nil
	}
	end := len(items)
	limit := int(page.Limit)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return items[start:end], &pluginsdk.PageInfo{HasMore: end < len(items), NextCursor: "next"}, nil
}

func (r *usageRecordingReader) ListCanonical(ctx context.Context, filter pluginsdk.SessionUsageFilter, page pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
	filter.Canonical = true
	return r.List(ctx, filter, page)
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

type usageRecordingHost struct {
	*fakeHost
	usage *usageRecordingReader
}

func (h *usageRecordingHost) Usage() pluginsdk.UsageReader { return h.usage }

type collectorHost struct {
	*usageRecordingHost
}

func (h *collectorHost) Tasks() pluginsdk.TaskReader { return collectorTaskReader{} }

type collectorTaskReader struct{}

func (collectorTaskReader) List(context.Context, pluginsdk.TaskFilter, pluginsdk.Page) ([]pluginsdk.Task, *pluginsdk.PageInfo, error) {
	return nil, nil, errors.New("collector task list is not used")
}

func (collectorTaskReader) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	return &pluginsdk.Task{ID: id, WorkspaceID: "ws-1"}, nil
}

func (collectorTaskReader) Create(context.Context, pluginsdk.CreateTaskInput) (*pluginsdk.Task, error) {
	return nil, errors.New("collector task create is not used")
}

func (collectorTaskReader) Update(context.Context, pluginsdk.UpdateTaskInput) (*pluginsdk.Task, error) {
	return nil, errors.New("collector task update is not used")
}

func (collectorTaskReader) Move(context.Context, pluginsdk.MoveTaskInput) (*pluginsdk.MoveTaskOutcome, error) {
	return nil, errors.New("collector task move is not used")
}

type doneSignalContext struct {
	context.Context
	signal chan struct{}
	once   sync.Once
}

func (c *doneSignalContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.signal) })
	return c.Context.Done()
}

func TestSessionUsageActionUsesSavedValuesBeforeTokscale(t *testing.T) {
	usage := &usageRecordingReader{items: []pluginsdk.SessionUsageMeasurement{{
		WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
		SourceRecordID: "transcript-1", UsageIdentity: "lifetime:transcript-1", Model: "model-a",
		InputTokens: int64Ptr(10), OutputTokens: int64Ptr(5), TotalTokens: int64Ptr(15),
		Turns: int64Ptr(1), CostSubcents: int64Ptr(1250), CollectedAt: "2026-09-13T10:00:00Z",
	}}}
	host := &usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")}}, usage: usage}
	called := 0
	p := newPlugin()
	p.lookPath = noLookPath
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		called++
		return nil, errors.New("tokscale must not run for a saved read")
	}
	p.SetHost(host)

	response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionKeySessionUsage,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "session-1"},
	})
	require.NoError(t, err)
	require.Equal(t, 200, response.Status)
	var body sessionCostResponse
	require.NoError(t, json.Unmarshal(response.Body, &body))
	require.True(t, body.Saved)
	require.InDelta(t, 0.125, body.Cost, 1e-9)
	require.Equal(t, int64(15), body.Total)
	require.Equal(t, 0, called)
}

func TestSessionUsageRefreshRetainsSavedValueOnFailure(t *testing.T) {
	usage := &usageRecordingReader{items: []pluginsdk.SessionUsageMeasurement{{
		WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
		SourceRecordID: "transcript-1", UsageIdentity: "lifetime:transcript-1", Model: "model-a",
		InputTokens: int64Ptr(10), OutputTokens: int64Ptr(5), TotalTokens: int64Ptr(15),
		CostSubcents: int64Ptr(1250), CollectedAt: "2026-09-13T10:00:00Z",
	}}}
	host := &usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{"collect_statistics": true}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")}}, usage: usage}
	p := newPlugin()
	p.lookPath = noLookPath
	p.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("report unavailable") }
	p.SetHost(host)

	response, err := p.sessionUsage(context.Background(), "ws-1", "task-1", "session-1", true)
	require.NoError(t, err)
	require.True(t, response.Saved)
	require.True(t, response.Stale)
	require.NotEmpty(t, response.Error)
	require.InDelta(t, 0.125, response.Cost, 1e-9)
}

func TestSessionUsageRefreshUsesCanonicalValueAfterRejectedWrite(t *testing.T) {
	usage := &usageRecordingReader{
		writeStatus: "conflict",
		items: []pluginsdk.SessionUsageMeasurement{{
			WorkspaceID: "ws-1", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
			SourceRecordID: "transcript-1", UsageIdentity: "lifetime:transcript-1", Model: "model-a",
			InputTokens: int64Ptr(10), OutputTokens: int64Ptr(5), TotalTokens: int64Ptr(15),
			CostSubcents: int64Ptr(1250), CollectedAt: "2026-09-13T10:00:00Z",
		}},
	}
	host := &usageRecordingHost{
		fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")}},
		usage:    usage,
	}
	p := newPlugin()
	p.lookPath = noLookPath
	p.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"model-a","input":20,"output":10,"total":30,"cost":0.5}]}`), nil)
	p.SetHost(host)

	response, err := p.sessionUsage(context.Background(), "ws-1", "task-1", "session-1", true)
	require.NoError(t, err)
	require.True(t, response.Saved)
	require.True(t, response.Stale)
	require.NotEmpty(t, response.Error)
	require.InDelta(t, 0.125, response.Cost, 1e-9)
	require.Equal(t, int64(15), response.Total)
}

func TestPersistSessionUsagePreservesTokscaleUnknownFields(t *testing.T) {
	usage := &usageRecordingReader{}
	host := &usageRecordingHost{
		fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}},
		usage:    usage,
	}
	p := newPlugin()
	p.SetHost(host)

	var report sessionModelsReport
	require.NoError(t, json.Unmarshal([]byte(`{"entries":[{
		"sessionId":"transcript-1","model":"model-a","input":0,"output":5,"cost":0
	}]}`), &report))
	response := responseForEntries("session-1", "transcript-1", report.Entries, 1, 10, time.Now())
	require.NoError(t, p.persistSessionUsage(context.Background(), "ws-1", "task-1", "session-1", &response))

	usage.mu.Lock()
	defer usage.mu.Unlock()
	require.Len(t, usage.items, 1)
	item := usage.items[0]
	require.NotNil(t, item.InputTokens)
	require.Equal(t, int64(0), *item.InputTokens, "an emitted zero remains known")
	require.NotNil(t, item.OutputTokens)
	require.Nil(t, item.CacheReadTokens, "an omitted category remains unknown")
	require.Nil(t, item.CacheWriteTokens)
	require.Nil(t, item.ReasoningTokens)
	require.Nil(t, item.TotalTokens, "a total is unknown when tokscale omits source categories")
	require.Nil(t, item.Turns, "an omitted message count remains unknown")
	require.NotNil(t, item.CostSubcents)
	require.Equal(t, int64(0), *item.CostSubcents, "an emitted zero cost remains known")
	require.Equal(t, "estimated", item.CostBasis)
}

func TestHistoricalImportResumesAndPersistsUndatedUsage(t *testing.T) {
	started := "2026-09-12T10:00:00Z"
	usage := &usageRecordingReader{}
	host := &usageRecordingHost{
		fakeHost: &fakeHost{
			config:   map[string]any{configKeyCollect: true},
			sessions: []pluginsdk.Session{{ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", StartedAt: started, UpdatedAt: started, State: "COMPLETED"}},
		},
		usage: usage,
	}
	p := newPlugin()
	p.lookPath = noLookPath
	p.run = modelsRunner([]byte(`{"entries":[{"sessionId":"transcript-1","model":"model-a","input":10,"output":5,"cost":0.125}]}`), nil)
	p.SetHost(host)
	defer stopPluginWorkers(p)

	response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionKeyImportStart,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1"},
	})
	require.NoError(t, err)
	require.Equal(t, 202, response.Status)
	require.Eventually(t, func() bool {
		state, found, stateErr := host.GetState(context.Background(), importStateScope, "ws-1", importStateKey)
		return stateErr == nil && found && state["status"] == importStatusCompleted
	}, time.Second, 10*time.Millisecond)

	state, found, err := host.GetState(context.Background(), importStateScope, "ws-1", importStateKey)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, float64(1), state["processed"])
	require.Equal(t, false, state["undated"])
	usage.mu.Lock()
	require.Len(t, usage.items, 1)
	require.Equal(t, "2026-09-12", usage.items[0].SourceDate)
	require.Equal(t, "date:transcript-1:2026-09-12", usage.items[0].UsageIdentity)
	usage.mu.Unlock()
}

func TestHistoricalImportIsDisabledWithCollectionOff(t *testing.T) {
	p := newPlugin()
	host := &fakeHost{config: map[string]any{configKeyCollect: false}}
	p.SetHost(host)
	response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionKeyImportStart,
		Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "ws-1"},
	})
	require.NoError(t, err)
	require.Equal(t, 409, response.Status)
	var state historicalImportState
	require.NoError(t, json.Unmarshal(response.Body, &state))
	require.Equal(t, importStatusDisabled, state.Status)
}

func stopPluginWorkers(p *plugin) {
	p.collectorMu.Lock()
	if p.collectorCancel != nil {
		p.collectorCancel()
	}
	p.collectorMu.Unlock()
	p.importMu.Lock()
	if p.importCancel != nil {
		p.importCancel()
	}
	p.importMu.Unlock()
}

func TestReportCoordinatorRunsOneReportForConcurrentCallers(t *testing.T) {
	coordinator := newReportCoordinator()
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var startOnce sync.Once
	calls := 0
	runner := func(context.Context, string, ...string) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		startOnce.Do(func() { close(started) })
		<-release
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"model-a"}]}`), nil
	}
	cmd := resolvedCommand{Argv: []string{"tokscale"}}
	first := make(chan error, 1)
	go func() {
		_, err := coordinator.run(context.Background(), cmd, runner)
		first <- err
	}()
	<-started
	second := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		ctx := &doneSignalContext{Context: context.Background(), signal: joined}
		_, err := coordinator.run(ctx, cmd, runner)
		second <- err
	}()
	<-joined
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	mu.Lock()
	require.Equal(t, 1, calls)
	mu.Unlock()
}

func TestRunDatedReportPassesInclusiveSourceDateBounds(t *testing.T) {
	var got []string
	p := newPlugin()
	p.run = func(_ context.Context, command string, args ...string) ([]byte, error) {
		got = append([]string{command}, args...)
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"model-a"}]}`), nil
	}

	entries, err := p.runDatedReport(context.Background(), resolvedCommand{Argv: []string{"tokscale"}}, "2026-09-12", "2026-09-12")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, []string{
		"tokscale", "models", "--json", "--group-by", "session,model",
		"--since", "2026-09-12", "--until", "2026-09-12",
	}, got)
}

func TestPersistSessionUsageIsIdempotentAcrossMultipleModels(t *testing.T) {
	usage := &usageRecordingReader{}
	host := &usageRecordingHost{
		fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}},
		usage:    usage,
	}
	p := newPlugin()
	p.SetHost(host)
	response := &sessionCostResponse{
		ACPSessionID: "transcript-1",
		Found:        true,
		CostKnown:    true,
		turnsKnown:   true,
		Turns:        2,
		Models: []sessionModel{
			{Model: "model-a", Provider: "provider-a", Input: 10, Output: 2, Cost: 0.01},
			{Model: "model-b", Provider: "provider-b", Input: 20, Output: 3, Cost: 0.02},
		},
	}
	require.NoError(t, p.persistSessionUsage(context.Background(), "ws-1", "task-1", "session-1", response))
	require.NoError(t, p.persistSessionUsage(context.Background(), "ws-1", "task-1", "session-1", response))

	usage.mu.Lock()
	defer usage.mu.Unlock()
	require.Len(t, usage.items, 2)
	require.Equal(t, 1, usage.upserts, "the second identical multi-model observation is unchanged")
}

func TestCollectionSettingsDefaultAndMinimum(t *testing.T) {
	p := newPlugin()
	p.SetHost(&fakeHost{config: map[string]any{}})
	require.False(t, p.collectionEnabled(context.Background()))
	require.Equal(t, 5*time.Minute, p.collectionInterval(context.Background()))

	p.SetHost(&fakeHost{config: map[string]any{configKeyInterval: float64(0)}})
	require.Equal(t, time.Minute, p.collectionInterval(context.Background()))
	p.SetHost(&fakeHost{config: map[string]any{configKeyInterval: float64(2000)}})
	require.Equal(t, 24*time.Hour, p.collectionInterval(context.Background()))
}

func TestCollectorSkipsIdleWorkspace(t *testing.T) {
	called := 0
	p := newPlugin()
	p.lookPath = noLookPath
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		called++
		return nil, nil
	}
	p.SetHost(&fakeHost{config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{{
		ID: "session-unknown", TaskID: "task-1", ACPSessionID: "transcript-unknown", State: "ARCHIVED",
	}}})
	defer stopPluginWorkers(p)
	require.NoError(t, p.collectOnce(context.Background()))
	require.Equal(t, 0, called)
}

func TestCollectorDoesNotImportOldTerminalHistoryOnFirstScan(t *testing.T) {
	fixedNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	called := 0
	p := newPlugin()
	p.now = func() time.Time { return fixedNow }
	p.lookPath = noLookPath
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		called++
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"model-a"}]}`), nil
	}
	host := &fakeHost{
		config: map[string]any{configKeyCollect: true},
		sessions: []pluginsdk.Session{{
			ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", State: "COMPLETED",
			StartedAt: fixedNow.Add(-24 * time.Hour).Format(time.RFC3339Nano),
			UpdatedAt: fixedNow.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		}},
	}
	p.SetHost(host)

	require.NoError(t, p.collectOnce(context.Background()))
	require.Equal(t, 0, called, "first enablement leaves older terminal history to explicit import")
}

func TestCollectorStopsRetryingTerminalSessionAfterSuccessfulCollection(t *testing.T) {
	fixedNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	called := 0
	p := newPlugin()
	p.now = func() time.Time { return fixedNow }
	p.lookPath = noLookPath
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		called++
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"model-a","input":1,"output":1,"total":2,"cost":0.01}]}`), nil
	}
	host := &collectorHost{usageRecordingHost: &usageRecordingHost{
		fakeHost: &fakeHost{
			config:   map[string]any{configKeyCollect: true},
			sessions: []pluginsdk.Session{{ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", State: "COMPLETED", StartedAt: fixedNow.Format(time.RFC3339Nano), UpdatedAt: fixedNow.Format(time.RFC3339Nano)}},
		},
		usage: &usageRecordingReader{},
	}}
	p.SetHost(host)
	require.NoError(t, host.SetState(context.Background(), collectorStateScope, collectorStateID, collectorStateKey, map[string]any{
		"last_scan_at": fixedNow.Add(-time.Hour).Format(time.RFC3339Nano), "initialized": true,
	}))
	defer stopPluginWorkers(p)

	require.NoError(t, p.collectOnce(context.Background()))
	require.NoError(t, p.collectOnce(context.Background()))
	require.NoError(t, p.collectOnce(context.Background()))
	require.NoError(t, p.collectOnce(context.Background()))
	require.Equal(t, 7, called, "the final date gets bounded stabilization passes before the terminal lifetime retry is released")
}

func TestCollectorRetriesTerminalSessionForDelayedTranscript(t *testing.T) {
	fixedNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	called := 0
	p := newPlugin()
	p.now = func() time.Time { return fixedNow }
	p.lookPath = noLookPath
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		called++
		return []byte(`{"entries":[]}`), nil
	}
	host := &fakeHost{
		config:   map[string]any{configKeyCollect: true},
		sessions: []pluginsdk.Session{{ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", State: "COMPLETED", StartedAt: fixedNow.Format(time.RFC3339Nano), UpdatedAt: fixedNow.Format(time.RFC3339Nano)}},
	}
	p.SetHost(host)
	require.NoError(t, host.SetState(context.Background(), collectorStateScope, collectorStateID, collectorStateKey, map[string]any{
		"last_scan_at": fixedNow.Add(-time.Hour).Format(time.RFC3339Nano), "initialized": true,
	}))
	defer stopPluginWorkers(p)

	for i := 0; i < maxFinalCollectionAttempts; i++ {
		require.NoError(t, p.collectOnce(context.Background()))
	}
	require.NoError(t, p.collectOnce(context.Background()))
	require.Equal(t, maxFinalCollectionAttempts*2, called, "each final retry includes lifetime and dated reports")
}

func TestCollectorCorrectsFoundDatedSnapshotAfterTranscriptFlush(t *testing.T) {
	fixedNow := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	called := 0
	p := newPlugin()
	p.now = func() time.Time { return fixedNow }
	p.lookPath = noLookPath
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		called++
		input := 1
		if called > 2 {
			input = 2
		}
		return []byte(fmt.Sprintf(`{"entries":[{"sessionId":"transcript-1","model":"model-a","input":%d,"output":1,"total":%d,"cost":0.01}]}`, input, input+1)), nil
	}
	usage := &usageRecordingReader{}
	host := &collectorHost{usageRecordingHost: &usageRecordingHost{
		fakeHost: &fakeHost{
			config: map[string]any{configKeyCollect: true},
			sessions: []pluginsdk.Session{{
				ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", State: "COMPLETED",
				StartedAt: fixedNow.Format(time.RFC3339Nano), UpdatedAt: fixedNow.Format(time.RFC3339Nano),
			}},
		},
		usage: usage,
	}}
	p.SetHost(host)
	require.NoError(t, host.SetState(context.Background(), collectorStateScope, collectorStateID, collectorStateKey, map[string]any{
		"last_scan_at": fixedNow.Add(-time.Hour).Format(time.RFC3339Nano), "initialized": true,
	}))

	for i := 0; i < 2; i++ {
		require.NoError(t, p.collectOnce(context.Background()))
	}
	require.GreaterOrEqual(t, called, 4)

	usage.mu.Lock()
	defer usage.mu.Unlock()
	var dated *pluginsdk.SessionUsageMeasurement
	for i := range usage.items {
		if usage.items[i].SourceDate == "2026-09-13" {
			copy := usage.items[i]
			dated = &copy
			break
		}
	}
	require.NotNil(t, dated)
	require.NotNil(t, dated.InputTokens)
	require.Equal(t, int64(2), *dated.InputTokens, "a Found response is retried so delayed transcript writes can correct the bucket")
}

func TestNextCollectionDatePrioritizesActiveSnapshots(t *testing.T) {
	sessions := []pluginsdk.Session{
		{ID: "terminal", State: "COMPLETED"},
		{ID: "active", State: "RUNNING"},
	}
	pending := map[string]collectorPending{
		"terminal": {Dates: []string{"2026-09-01"}},
		"active":   {Dates: []string{"2026-09-13"}},
	}

	require.Equal(t, "2026-09-13", nextCollectionDate(pending, sessions))
}

func int64Ptr(value int64) *int64 { return &value }
