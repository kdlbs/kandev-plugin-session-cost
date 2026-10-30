// Package main tests. Exercises the authenticated usage action and report
// helpers against fake Host and CLI implementations, plus the static legacy
// webhook response. No go-plugin spawn or real tokscale process is needed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

// fakeHost serves GetConfig and Sessions() — the only Host surfaces this
// plugin uses. Everything else comes from UnimplementedHostData.
type fakeHost struct {
	pluginsdk.UnimplementedHostData
	config     map[string]any
	configFn   func(context.Context) (map[string]any, error)
	sessionsFn func(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, error)
	sessions   []pluginsdk.Session
	stateMu    sync.Mutex
	state      map[string]map[string]any
}

func (h *fakeHost) GetState(_ context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	value, found := h.state[scope+"\x00"+scopeID+"\x00"+key]
	if !found {
		return nil, false, nil
	}
	copy := make(map[string]any, len(value))
	for name, item := range value {
		copy[name] = item
	}
	return copy, true, nil
}

func (h *fakeHost) SetState(_ context.Context, scope, scopeID, key string, value map[string]any) error {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.state == nil {
		h.state = make(map[string]map[string]any)
	}
	copy := make(map[string]any, len(value))
	for name, item := range value {
		copy[name] = item
	}
	h.state[scope+"\x00"+scopeID+"\x00"+key] = copy
	return nil
}
func (h *fakeHost) DeleteState(_ context.Context, scope, scopeID, key string) error {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	delete(h.state, scope+"\x00"+scopeID+"\x00"+key)
	return nil
}
func (h *fakeHost) ListState(_ context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	prefix := scope + "\x00" + scopeID + "\x00"
	var entries []pluginsdk.StateEntry
	for key, value := range h.state {
		if strings.HasPrefix(key, prefix) {
			entries = append(entries, pluginsdk.StateEntry{Key: strings.TrimPrefix(key, prefix), Value: value})
		}
	}
	return entries, nil
}
func (h *fakeHost) GetConfig(ctx context.Context) (map[string]any, error) {
	if h.configFn != nil {
		return h.configFn(ctx)
	}
	if h.config == nil {
		return map[string]any{}, nil
	}
	return h.config, nil
}
func (h *fakeHost) RevealSecret(context.Context, string) (string, error) { return "", nil }
func (h *fakeHost) GetSecret(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (h *fakeHost) SetSecret(context.Context, string, string) error         { return nil }
func (h *fakeHost) DeleteSecret(context.Context, string) error              { return nil }
func (h *fakeHost) EmitEvent(context.Context, string, map[string]any) error { return nil }

func (h *fakeHost) Sessions() pluginsdk.SessionReader {
	return fakeSessionReader{sessions: h.sessions, listFn: h.sessionsFn}
}

type fakeSessionReader struct {
	listFn   func(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, error)
	sessions []pluginsdk.Session
}

func (r fakeSessionReader) List(ctx context.Context, filter pluginsdk.SessionFilter, page pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
	if r.listFn != nil {
		items, err := r.listFn(ctx, filter, page)
		return items, nil, err
	}
	return r.sessions, nil, nil
}
func (r fakeSessionReader) CodeStats(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.SessionCodeStats, *pluginsdk.PageInfo, error) {
	return nil, nil, nil
}

func noLookPath(string) (string, error) { return "", errors.New("not found") }

// newTestPlugin wires a plugin with a scripted runner, no PATH lookup, and a
// fake Host serving the given config + sessions.
func newTestPlugin(config map[string]any, sessions []pluginsdk.Session, run runner) *plugin {
	p := newPlugin()
	p.lookPath = noLookPath
	p.run = run
	// Successful manual-source fixtures explicitly model a current Host that
	// authorizes the transcript context; an old/unknown Host is not a bypass.
	p.SetHost(&collectorHost{&usageRecordingHost{fakeHost: &fakeHost{config: config, sessions: sessions}, usage: &usageRecordingReader{}}})
	return p
}

func webhookGet(key, query string) *pluginsdk.WebhookRequest {
	return &pluginsdk.WebhookRequest{WebhookKey: key, Method: "GET", Query: query}
}

type webhookHostSpy struct {
	*collectorHost
	calls atomic.Int32
}

func (h *webhookHostSpy) GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	h.calls.Add(1)
	return h.collectorHost.GetState(ctx, scope, scopeID, key)
}
func (h *webhookHostSpy) SetState(ctx context.Context, scope, scopeID, key string, value map[string]any) error {
	h.calls.Add(1)
	return h.collectorHost.SetState(ctx, scope, scopeID, key, value)
}
func (h *webhookHostSpy) DeleteState(ctx context.Context, scope, scopeID, key string) error {
	h.calls.Add(1)
	return h.collectorHost.DeleteState(ctx, scope, scopeID, key)
}
func (h *webhookHostSpy) ListState(ctx context.Context, scope, scopeID string) ([]pluginsdk.StateEntry, error) {
	h.calls.Add(1)
	return h.collectorHost.ListState(ctx, scope, scopeID)
}
func (h *webhookHostSpy) GetConfig(ctx context.Context) (map[string]any, error) {
	h.calls.Add(1)
	return h.collectorHost.GetConfig(ctx)
}
func (h *webhookHostSpy) RevealSecret(ctx context.Context, ref string) (string, error) {
	h.calls.Add(1)
	return h.collectorHost.RevealSecret(ctx, ref)
}
func (h *webhookHostSpy) GetSecret(ctx context.Context, key string) (string, bool, error) {
	h.calls.Add(1)
	return h.collectorHost.GetSecret(ctx, key)
}
func (h *webhookHostSpy) SetSecret(ctx context.Context, key, value string) error {
	h.calls.Add(1)
	return h.collectorHost.SetSecret(ctx, key, value)
}
func (h *webhookHostSpy) DeleteSecret(ctx context.Context, key string) error {
	h.calls.Add(1)
	return h.collectorHost.DeleteSecret(ctx, key)
}
func (h *webhookHostSpy) EmitEvent(ctx context.Context, name string, payload map[string]any) error {
	h.calls.Add(1)
	return h.collectorHost.EmitEvent(ctx, name, payload)
}
func (h *webhookHostSpy) Tasks() pluginsdk.TaskReader {
	h.calls.Add(1)
	return h.collectorHost.Tasks()
}
func (h *webhookHostSpy) Sessions() pluginsdk.SessionReader {
	h.calls.Add(1)
	return h.collectorHost.Sessions()
}
func (h *webhookHostSpy) ExecutorProfiles() pluginsdk.ExecutorProfileReader {
	h.calls.Add(1)
	return h.collectorHost.ExecutorProfiles()
}
func (h *webhookHostSpy) Usage() pluginsdk.UsageReader {
	h.calls.Add(1)
	return h.collectorHost.Usage()
}
func (h *webhookHostSpy) UsageHistory() pluginsdk.UsageHistoryReader {
	h.calls.Add(1)
	return h.collectorHost.usageRecordingHost.UsageHistory()
}
func (h *webhookHostSpy) Workspaces() pluginsdk.WorkspaceReader {
	h.calls.Add(1)
	return h.collectorHost.Workspaces()
}
func (h *webhookHostSpy) Workflows() pluginsdk.WorkflowReader {
	h.calls.Add(1)
	return h.collectorHost.Workflows()
}
func (h *webhookHostSpy) AgentProfiles() pluginsdk.AgentProfileReader {
	h.calls.Add(1)
	return h.collectorHost.AgentProfiles()
}
func (h *webhookHostSpy) Repositories() pluginsdk.RepositoryReader {
	h.calls.Add(1)
	return h.collectorHost.Repositories()
}
func (h *webhookHostSpy) Messages() pluginsdk.MessageReader {
	h.calls.Add(1)
	return h.collectorHost.Messages()
}
func (h *webhookHostSpy) InvokeUtilityAgent(ctx context.Context, prompt string, options ...pluginsdk.UtilityAgentOptions) (string, error) {
	h.calls.Add(1)
	return h.collectorHost.InvokeUtilityAgent(ctx, prompt, options...)
}

// modelsRunner answers --version probes and `models` runs distinctly.
func modelsRunner(modelsOut []byte, modelsErr error) runner {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "--version" {
			return []byte("tokscale 4.15.1\n"), nil
		}
		return modelsOut, modelsErr
	}
}

const sampleSessionJSON = `{"entries":[
  {"sessionId":"abc-123","model":"claude-opus-4-8","input":1000,"output":500,"cacheRead":200,"messageCount":4,"cost":1.25},
  {"sessionId":"abc-123","model":"claude-haiku-4-5","input":50,"output":20,"cacheRead":0,"messageCount":1,"cost":0.05},
  {"sessionId":"unrelated","model":"x","cost":9.9}
]}`

func session(id, acp string) pluginsdk.Session {
	return pluginsdk.Session{ID: id, TaskID: "task-1", ACPSessionID: acp}
}

func decode(t *testing.T, body []byte) sessionCostResponse {
	t.Helper()
	var resp sessionCostResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

func waitForReport(t *testing.T, p *plugin) {
	t.Helper()
	p.toolbar.mu.Lock()
	done := p.toolbar.activeDone
	p.toolbar.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("report worker did not finish")
	}
}

func callSessionUsageAction(t *testing.T, p *plugin, query string) (*pluginsdk.PluginActionResponse, sessionCostResponse) {
	t.Helper()
	values, err := url.ParseQuery(query)
	require.NoError(t, err)
	body, err := json.Marshal(struct {
		Refresh bool `json:"refresh"`
	}{Refresh: values.Get("refresh") == "1"})
	require.NoError(t, err)
	resp, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
		ActionKey: actionKeySessionUsage,
		Context: pluginsdk.VerifiedActionContext{
			WorkspaceID: "ws-1",
			TaskID:      values.Get("task_id"),
			SessionID:   values.Get("active"),
		},
		Body: body,
	})
	require.NoError(t, err)
	return resp, decode(t, resp.Body)
}

func callReadySessionUsageAction(t *testing.T, p *plugin, query string) (*pluginsdk.PluginActionResponse, sessionCostResponse) {
	t.Helper()
	resp, d := callSessionUsageAction(t, p, query)
	if d.ReportState == reportStatePending {
		waitForReport(t, p)
		resp, d = callSessionUsageAction(t, p, query)
	}
	return resp, d
}

func TestLegacySessionCostWebhookIsStaticAndDoesNotCallHostOrCLI(t *testing.T) {
	base := &collectorHost{&usageRecordingHost{fakeHost: &fakeHost{}, usage: &usageRecordingReader{}}}
	host := &webhookHostSpy{collectorHost: base}
	p := newPlugin()
	p.UnimplementedPlugin.SetHost(host)
	var cliCalls atomic.Int32
	p.lookPath = func(string) (string, error) {
		cliCalls.Add(1)
		return "tokscale", nil
	}
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		cliCalls.Add(1)
		return []byte(`{"entries":[{"sessionId":"foreign-session","model":"private-model","input":999,"cost":999}]}`), nil
	}
	t.Cleanup(p.Close)

	request := webhookGet(webhookKeySessionCost, "task_id=foreign-task&active=foreign-session&refresh=1&cost=999&malformed=%zz")
	request.Headers = map[string]string{
		"Authorization": "Bearer caller-controlled-value",
		"X-Workspace":   "foreign-workspace",
	}
	response, err := p.HandleWebhook(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, int32(200), response.Status)
	require.JSONEq(t, `{"compatibility":"upgrade_required","error":"Use the authenticated session-usage action to view or refresh usage."}`, string(response.Body))
	for _, privateValue := range []string{"foreign-task", "foreign-session", "foreign-workspace", "caller-controlled-value", "private-model", `"cost"`, `"input"`, `"models"`} {
		require.NotContains(t, string(response.Body), privateValue)
	}
	require.Zero(t, host.calls.Load(), "legacy webhook must not call any Host API")
	require.Zero(t, cliCalls.Load(), "legacy webhook must not locate or run a CLI")

	for _, unknown := range []*pluginsdk.WebhookRequest{webhookGet("unknown", "task_id=foreign-task&active=foreign-session"), nil} {
		response, err = p.HandleWebhook(context.Background(), unknown)
		require.NoError(t, err)
		require.Equal(t, int32(404), response.Status)
		require.NotContains(t, string(response.Body), "foreign-")
	}
	require.Zero(t, host.calls.Load(), "unknown and nil requests must not call any Host API")
	require.Zero(t, cliCalls.Load(), "unknown and nil requests must not locate or run a CLI")
}

func TestSessionUsageAction_FoundSumsSessionRows(t *testing.T) {
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))
	resp, d := callReadySessionUsageAction(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, 200, resp.Status)
	require.Equal(t, reportStateReady, d.ReportState)
	require.True(t, d.Tokscale.Installed)
	require.Equal(t, "abc-123", d.ACPSessionID)
	require.True(t, d.Found)
	require.InDelta(t, 1.30, d.Cost, 1e-9, "sums both model rows for the matched session")
	require.Equal(t, int64(1050), d.Input)
	require.Equal(t, int64(520), d.Output)
	require.Zero(t, d.Turns, "tokscale message counts do not establish Kandev turns")
	require.Nil(t, d.TokenAmounts["turns"])
	require.Zero(t, d.CostPerTurn, "cost per Kandev turn requires canonical turn evidence")
	require.Len(t, d.Models, 2)
	known := true
	require.Equal(t, sessionModel{CostKnown: &known, Currency: "USD", CostBasis: "estimated", ExactCost: optionalString("12500"), TokenAmounts: displayTokens(&pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(1000), OutputTokens: ptr(500), CacheReadTokens: ptr(200)}, nil), Model: "claude-opus-4-8", Input: 1000, Output: 500, CacheRead: 200, Cost: 1.25}, d.Models[0])
	require.Equal(t, sessionModel{CostKnown: &known, Currency: "USD", CostBasis: "estimated", ExactCost: optionalString("500"), TokenAmounts: displayTokens(&pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(50), OutputTokens: ptr(20), CacheReadTokens: ptr(0)}, nil), Model: "claude-haiku-4-5", Input: 50, Output: 20, CacheRead: 0, Cost: 0.05}, d.Models[1])
	// Default thresholds echoed for the UI to colour by.
	require.Equal(t, defaultWarnThreshold, d.WarnThreshold)
	require.Equal(t, defaultHighThreshold, d.HighThreshold)
}

func TestSessionUsageAction_SlowReportReturnsPending(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRunner := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRunner()

	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		close(started)
		select {
		case <-release:
			return []byte(sampleSessionJSON), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p := newTestPlugin(nil, []pluginsdk.Session{session("kandev-sess", "abc-123")}, run)
	query := "task_id=task-1&active=kandev-sess"
	first, pending := callSessionUsageAction(t, p, query)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("report runner did not start")
	}
	require.Equal(t, 200, first.Status)
	require.Equal(t, reportStatePending, pending.ReportState)

	releaseRunner()
	waitForReport(t, p)
	resp, ready := callReadySessionUsageAction(t, p, query)
	require.Equal(t, 200, resp.Status)
	require.Equal(t, reportStateReady, ready.ReportState)
	require.True(t, ready.Found)
	p.Close()
}

func TestSessionUsageAction_NoUsageForSession(t *testing.T) {
	sessions := []pluginsdk.Session{session("kandev-sess", "no-match")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadySessionUsageAction(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, "no-match", d.ACPSessionID)
	require.False(t, d.Found)
	require.Zero(t, d.Cost)
}

func TestSessionUsageAction_NoTranscriptYet(t *testing.T) {
	// Session exists but the agent hasn't reported an ACP transcript id yet.
	sessions := []pluginsdk.Session{session("kandev-sess", "")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadySessionUsageAction(t, p, "task_id=task-1&active=kandev-sess")
	require.Empty(t, d.ACPSessionID)
	require.False(t, d.Found)
	require.Equal(t, reportStateReady, d.ReportState)
	require.False(t, d.Tokscale.Installed, "no transcript avoids a report and setup probe")
	require.Empty(t, d.GeneratedAt)
	require.Empty(t, d.ReportError)
}

func TestSessionUsageAction_CodexRolloutSuffixMatches(t *testing.T) {
	// Codex keys sessions by rollout filename; the ACP UUID is the suffix.
	report := `{"entries":[{"sessionId":"rollout-2026-07-19-abc-123","model":"gpt-x","input":0,"output":0,"cacheRead":0,"cost":2.5,"messageCount":3}]}`
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(report), nil))

	_, d := callReadySessionUsageAction(t, p, "task_id=task-1&active=kandev-sess")
	require.True(t, d.Found)
	require.InDelta(t, 2.5, d.Cost, 1e-9)
	require.Zero(t, d.Turns, "rollout messages do not establish Kandev turns")
	require.Nil(t, d.TokenAmounts["turns"])
	require.Zero(t, d.CostPerTurn)
}

func TestSessionUsageAction_ThresholdsFromConfig(t *testing.T) {
	config := map[string]any{"warn_threshold": 2.0, "high_threshold": 20.0}
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(config, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadySessionUsageAction(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, 2.0, d.WarnThreshold)
	require.Equal(t, 20.0, d.HighThreshold)
}

func TestSessionUsageAction_HighClampedBelowWarn(t *testing.T) {
	config := map[string]any{"warn_threshold": 5.0, "high_threshold": 1.0}
	p := newTestPlugin(config, []pluginsdk.Session{session("kandev-sess", "abc-123")}, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadySessionUsageAction(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, 5.0, d.WarnThreshold)
	require.Equal(t, 5.0, d.HighThreshold, "high never renders below warn")
}

func TestSessionUsageAction_DegradesWhenTokscaleMissing(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, &exec.Error{Name: "npx", Err: exec.ErrNotFound}
	}
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, run)

	resp, d := callReadySessionUsageAction(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, 200, resp.Status, "a missing CLI is a degraded payload, not an action failure")
	require.Equal(t, reportStateFailed, d.ReportState)
	require.Equal(t, reportErrorUnavailable, d.ReportError)
	require.False(t, d.Tokscale.Installed)
	require.False(t, d.Found)
}

func TestSessionUsageAction_ConcurrentSessionsShareOneReportAndProjectSeparately(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return []byte(sampleSessionJSON), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p := newTestPlugin(nil, []pluginsdk.Session{
		session("one", "abc-123"),
		session("two", "unrelated"),
	}, run)
	defer p.Close()

	_, first := callSessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStatePending, first.ReportState)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared report did not start")
	}
	_, second := callSessionUsageAction(t, p, "task_id=task-1&active=two")
	require.Equal(t, reportStatePending, second.ReportState)
	require.Equal(t, int32(1), calls.Load())

	close(release)
	waitForReport(t, p)
	_, first = callSessionUsageAction(t, p, "task_id=task-1&active=one")
	_, second = callSessionUsageAction(t, p, "task_id=task-1&active=two")
	require.Equal(t, reportStateReady, first.ReportState)
	require.Equal(t, reportStateReady, second.ReportState)
	require.InDelta(t, 1.30, first.Cost, 1e-9)
	require.InDelta(t, 9.9, second.Cost, 1e-9)
	require.Equal(t, "abc-123", first.ACPSessionID)
	require.Equal(t, "unrelated", second.ACPSessionID)
}

func TestSessionUsageAction_RefreshFailureRetainsSnapshotAndExplicitRetryRecovers(t *testing.T) {
	var calls atomic.Int32
	newReport := `{"entries":[{"sessionId":"abc-123","model":"gpt-x","input":2,"output":1,"cacheRead":0,"messageCount":1,"cost":2.5}]}`
	run := func(context.Context, string, ...string) ([]byte, error) {
		switch calls.Add(1) {
		case 1:
			return []byte(sampleSessionJSON), nil
		case 2:
			return nil, errors.New("temporary CLI failure")
		default:
			return []byte(newReport), nil
		}
	}
	p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, run)
	p.toolbar.cooldown = time.Hour
	defer p.Close()

	_, first := callReadySessionUsageAction(t, p, "task_id=task-1&active=one")
	require.True(t, first.Found)
	require.InDelta(t, 1.30, first.Cost, 1e-9)

	_, pending := callSessionUsageAction(t, p, "task_id=task-1&active=one&refresh=1")
	require.Equal(t, reportStatePending, pending.ReportState)
	require.True(t, pending.Stale)
	waitForReport(t, p)
	_, failed := callSessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateFailed, failed.ReportState)
	require.Equal(t, reportErrorFailed, failed.ReportError)
	require.True(t, failed.Stale)
	require.True(t, failed.Found)
	require.InDelta(t, 1.30, failed.Cost, 1e-9, "last successful cost remains available")
	require.Equal(t, int32(2), calls.Load(), "normal lookups respect failure cooldown")

	_, retry := callSessionUsageAction(t, p, "task_id=task-1&active=one&refresh=1")
	require.Equal(t, reportStatePending, retry.ReportState)
	waitForReport(t, p)
	_, recovered := callSessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, recovered.ReportState)
	require.False(t, recovered.Stale)
	require.InDelta(t, 2.5, recovered.Cost, 1e-9)
	require.Equal(t, int32(3), calls.Load())
}

func TestSessionUsageAction_FreshnessSchedulesOneNewReport(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(sampleSessionJSON), nil
	})
	p.now = func() time.Time { return now }
	defer p.Close()

	_, first := callReadySessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, first.ReportState)
	now = now.Add(30 * time.Second)
	_, pending := callSessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStatePending, pending.ReportState)
	waitForReport(t, p)
	_, refreshed := callSessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, refreshed.ReportState)
	require.Equal(t, int32(2), calls.Load())
}

func TestSessionUsageAction_ColdFailuresAndInvalidOutputAreRetryable(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		p.toolbar.timeout = 20 * time.Millisecond
		defer p.Close()
		_, failed := callReadySessionUsageAction(t, p, "task_id=task-1&active=one")
		require.Equal(t, reportStateFailed, failed.ReportState)
		require.Equal(t, reportErrorTimeout, failed.ReportError)
		require.False(t, failed.Stale)
	})

	t.Run("invalid output", func(t *testing.T) {
		p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(context.Context, string, ...string) ([]byte, error) {
			return []byte(`{"unexpected":[]}`), nil
		})
		defer p.Close()
		_, failed := callReadySessionUsageAction(t, p, "task_id=task-1&active=one")
		require.Equal(t, reportStateFailed, failed.ReportState)
		require.Equal(t, reportErrorFailed, failed.ReportError)
		require.Empty(t, failed.Models)
	})
}

func TestSessionUsageAction_NoTranscriptSkipsReportCalculation(t *testing.T) {
	var calls atomic.Int32
	p := newTestPlugin(nil, []pluginsdk.Session{session("one", "")}, func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(sampleSessionJSON), nil
	})
	defer p.Close()

	_, response := callReadySessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, response.ReportState)
	require.Empty(t, response.ACPSessionID)
	require.Empty(t, response.GeneratedAt)
	require.Equal(t, int32(0), calls.Load())
}

func TestSessionUsageAction_HostReadIsBoundedAndFailsClearly(t *testing.T) {
	t.Run("configuration", func(t *testing.T) {
		p := newPlugin()
		p.hostRead = 20 * time.Millisecond
		p.UnimplementedPlugin.SetHost(&fakeHost{configFn: func(ctx context.Context) (map[string]any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		defer p.Close()

		started := time.Now()
		_, err := p.responsiveSessionUsage(context.Background(), "", "task-1", "one", false)
		require.ErrorContains(t, err, "host unavailable")
		require.Less(t, time.Since(started), time.Second)
	})

	t.Run("session list", func(t *testing.T) {
		p := newPlugin()
		p.hostRead = 20 * time.Millisecond
		p.UnimplementedPlugin.SetHost(&fakeHost{sessionsFn: func(ctx context.Context, _ pluginsdk.SessionFilter, _ pluginsdk.Page) ([]pluginsdk.Session, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		defer p.Close()
		started := time.Now()
		_, err := p.responsiveSessionUsage(context.Background(), "", "task-1", "one", false)
		require.ErrorContains(t, err, "host unavailable")
		require.Less(t, time.Since(started), time.Second)
	})
}

func TestReportCoordinatorCloseCancelsWorker(t *testing.T) {
	started := make(chan struct{})
	p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, pending := callSessionUsageAction(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStatePending, pending.ReportState)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("report worker did not start")
	}
	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("plugin close did not cancel and drain the report worker")
	}
}

func TestSessionMatches(t *testing.T) {
	require.True(t, sessionMatches("abc-123", "abc-123"))
	require.True(t, sessionMatches("rollout-x-abc-123", "abc-123"))
	require.False(t, sessionMatches("abc-1234", "abc-123"))
	require.False(t, sessionMatches("abc-123", ""))
}
