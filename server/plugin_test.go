// Package main tests. Exercises plugin.HandleWebhook end to end against a fake
// Host and an injected runner — no go-plugin spawn and no real tokscale needed,
// mirroring the other kandev plugins' test approach.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
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
func (h *fakeHost) DeleteState(context.Context, string, string, string) error { return nil }
func (h *fakeHost) ListState(context.Context, string, string) ([]pluginsdk.StateEntry, error) {
	return nil, nil
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
	p.SetHost(&fakeHost{config: config, sessions: sessions})
	return p
}

func webhookGet(key, query string) *pluginsdk.WebhookRequest {
	return &pluginsdk.WebhookRequest{WebhookKey: key, Method: "GET", Query: query}
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

func callWebhook(t *testing.T, p *plugin, query string) (*pluginsdk.WebhookResponse, sessionCostResponse) {
	t.Helper()
	resp, err := p.HandleWebhook(context.Background(), webhookGet(webhookKeySessionCost, query))
	require.NoError(t, err)
	return resp, decode(t, resp.Body)
}

func callReadyWebhook(t *testing.T, p *plugin, query string) (*pluginsdk.WebhookResponse, sessionCostResponse) {
	t.Helper()
	resp, d := callWebhook(t, p, query)
	if d.ReportState == reportStatePending {
		waitForReport(t, p)
		resp, d = callWebhook(t, p, query)
	}
	return resp, d
}

func TestHandleWebhook_UnknownKey(t *testing.T) {
	p := newTestPlugin(nil, nil, modelsRunner([]byte(sampleSessionJSON), nil))
	resp, err := p.HandleWebhook(context.Background(), webhookGet("nope", ""))
	require.NoError(t, err)
	require.Equal(t, int32(404), resp.Status)
}

func TestHandleWebhook_FoundSumsSessionRows(t *testing.T) {
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))
	resp, d := callReadyWebhook(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, int32(200), resp.Status)
	require.Equal(t, reportStateReady, d.ReportState)
	require.True(t, d.Tokscale.Installed)
	require.Equal(t, "abc-123", d.ACPSessionID)
	require.True(t, d.Found)
	require.InDelta(t, 1.30, d.Cost, 1e-9, "sums both model rows for the matched session")
	require.Equal(t, int64(1050), d.Input)
	require.Equal(t, int64(520), d.Output)
	require.Equal(t, int64(5), d.Turns, "sums messageCount across the session's model rows")
	require.InDelta(t, 1.30/5.0, d.CostPerTurn, 1e-9, "avg cost per turn computed server-side")
	require.Len(t, d.Models, 2)
	require.Equal(t, sessionModel{Model: "claude-opus-4-8", Input: 1000, Output: 500, CacheRead: 200, Cost: 1.25}, d.Models[0])
	require.Equal(t, sessionModel{Model: "claude-haiku-4-5", Input: 50, Output: 20, CacheRead: 0, Cost: 0.05}, d.Models[1])
	// Default thresholds echoed for the UI to colour by.
	require.Equal(t, defaultWarnThreshold, d.WarnThreshold)
	require.Equal(t, defaultHighThreshold, d.HighThreshold)
}

func TestHandleWebhook_SlowReportReturnsPending(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := webhookGet(webhookKeySessionCost, "task_id=task-1&active=kandev-sess")
	response := make(chan *pluginsdk.WebhookResponse, 1)
	go func() {
		resp, _ := p.HandleWebhook(ctx, request)
		response <- resp
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("report runner did not start")
	}
	var first *pluginsdk.WebhookResponse
	select {
	case first = <-response:
	case <-time.After(time.Second):
		t.Fatal("blocked report prevented the webhook from returning promptly")
	}
	require.Equal(t, int32(200), first.Status)
	var pending map[string]any
	require.NoError(t, json.Unmarshal(first.Body, &pending))
	require.Equal(t, "pending", pending["report_state"])

	cancel()
	releaseRunner()
	waitForReport(t, p)
	resp, ready := callReadyWebhook(t, p, request.Query)
	require.Equal(t, int32(200), resp.Status)
	require.Equal(t, reportStateReady, ready.ReportState)
	require.True(t, ready.Found)
	p.Close()
}

func TestHandleWebhook_NoUsageForSession(t *testing.T) {
	sessions := []pluginsdk.Session{session("kandev-sess", "no-match")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadyWebhook(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, "no-match", d.ACPSessionID)
	require.False(t, d.Found)
	require.Zero(t, d.Cost)
}

func TestHandleWebhook_NoTranscriptYet(t *testing.T) {
	// Session exists but the agent hasn't reported an ACP transcript id yet.
	sessions := []pluginsdk.Session{session("kandev-sess", "")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadyWebhook(t, p, "task_id=task-1&active=kandev-sess")
	require.Empty(t, d.ACPSessionID)
	require.False(t, d.Found)
	require.Equal(t, reportStateReady, d.ReportState)
	require.False(t, d.Tokscale.Installed, "no transcript avoids a report and setup probe")
	require.Empty(t, d.GeneratedAt)
	require.Empty(t, d.ReportError)
}

func TestHandleWebhook_CodexRolloutSuffixMatches(t *testing.T) {
	// Codex keys sessions by rollout filename; the ACP UUID is the suffix.
	report := `{"entries":[{"sessionId":"rollout-2026-07-19-abc-123","model":"gpt-x","input":0,"output":0,"cacheRead":0,"cost":2.5,"messageCount":3}]}`
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(report), nil))

	_, d := callReadyWebhook(t, p, "task_id=task-1&active=kandev-sess")
	require.True(t, d.Found)
	require.InDelta(t, 2.5, d.Cost, 1e-9)
	require.Equal(t, int64(3), d.Turns)
	require.InDelta(t, 2.5/3.0, d.CostPerTurn, 1e-9)
}

func TestHandleWebhook_ThresholdsFromConfig(t *testing.T) {
	config := map[string]any{"warn_threshold": 2.0, "high_threshold": 20.0}
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(config, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadyWebhook(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, 2.0, d.WarnThreshold)
	require.Equal(t, 20.0, d.HighThreshold)
}

func TestHandleWebhook_HighClampedBelowWarn(t *testing.T) {
	config := map[string]any{"warn_threshold": 5.0, "high_threshold": 1.0}
	p := newTestPlugin(config, nil, modelsRunner([]byte(sampleSessionJSON), nil))

	_, d := callReadyWebhook(t, p, "active=kandev-sess")
	require.Equal(t, 5.0, d.WarnThreshold)
	require.Equal(t, 5.0, d.HighThreshold, "high never renders below warn")
}

func TestHandleWebhook_DegradesWhenTokscaleMissing(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, &exec.Error{Name: "npx", Err: exec.ErrNotFound}
	}
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, run)

	resp, d := callReadyWebhook(t, p, "task_id=task-1&active=kandev-sess")
	require.Equal(t, int32(200), resp.Status, "a missing CLI is a degraded payload, not a 500")
	require.Equal(t, reportStateFailed, d.ReportState)
	require.Equal(t, reportErrorUnavailable, d.ReportError)
	require.False(t, d.Tokscale.Installed)
	require.False(t, d.Found)
}

func TestHandleWebhook_ConcurrentSessionsShareOneReportAndProjectSeparately(t *testing.T) {
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

	_, first := callWebhook(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStatePending, first.ReportState)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared report did not start")
	}
	_, second := callWebhook(t, p, "task_id=task-1&active=two")
	require.Equal(t, reportStatePending, second.ReportState)
	require.Equal(t, int32(1), calls.Load())

	close(release)
	waitForReport(t, p)
	_, first = callWebhook(t, p, "task_id=task-1&active=one")
	_, second = callWebhook(t, p, "task_id=task-1&active=two")
	require.Equal(t, reportStateReady, first.ReportState)
	require.Equal(t, reportStateReady, second.ReportState)
	require.InDelta(t, 1.30, first.Cost, 1e-9)
	require.InDelta(t, 9.9, second.Cost, 1e-9)
	require.Equal(t, "abc-123", first.ACPSessionID)
	require.Equal(t, "unrelated", second.ACPSessionID)
}

func TestHandleWebhook_RefreshFailureRetainsSnapshotAndExplicitRetryRecovers(t *testing.T) {
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

	_, first := callReadyWebhook(t, p, "task_id=task-1&active=one")
	require.True(t, first.Found)
	require.InDelta(t, 1.30, first.Cost, 1e-9)

	_, pending := callWebhook(t, p, "task_id=task-1&active=one&refresh=1")
	require.Equal(t, reportStatePending, pending.ReportState)
	require.True(t, pending.Stale)
	waitForReport(t, p)
	_, failed := callWebhook(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateFailed, failed.ReportState)
	require.Equal(t, reportErrorFailed, failed.ReportError)
	require.True(t, failed.Stale)
	require.True(t, failed.Found)
	require.InDelta(t, 1.30, failed.Cost, 1e-9, "last successful cost remains available")
	require.Equal(t, int32(2), calls.Load(), "normal lookups respect failure cooldown")

	_, retry := callWebhook(t, p, "task_id=task-1&active=one&refresh=1")
	require.Equal(t, reportStatePending, retry.ReportState)
	waitForReport(t, p)
	_, recovered := callWebhook(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, recovered.ReportState)
	require.False(t, recovered.Stale)
	require.InDelta(t, 2.5, recovered.Cost, 1e-9)
	require.Equal(t, int32(3), calls.Load())
}

func TestHandleWebhook_FreshnessSchedulesOneNewReport(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(sampleSessionJSON), nil
	})
	p.now = func() time.Time { return now }
	defer p.Close()

	_, first := callReadyWebhook(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, first.ReportState)
	now = now.Add(30 * time.Second)
	_, pending := callWebhook(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStatePending, pending.ReportState)
	waitForReport(t, p)
	_, refreshed := callWebhook(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, refreshed.ReportState)
	require.Equal(t, int32(2), calls.Load())
}

func TestHandleWebhook_ColdFailuresAndInvalidOutputAreRetryable(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		p.toolbar.timeout = 20 * time.Millisecond
		defer p.Close()
		_, failed := callReadyWebhook(t, p, "task_id=task-1&active=one")
		require.Equal(t, reportStateFailed, failed.ReportState)
		require.Equal(t, reportErrorTimeout, failed.ReportError)
		require.False(t, failed.Stale)
	})

	t.Run("invalid output", func(t *testing.T) {
		p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(context.Context, string, ...string) ([]byte, error) {
			return []byte(`{"unexpected":[]}`), nil
		})
		defer p.Close()
		_, failed := callReadyWebhook(t, p, "task_id=task-1&active=one")
		require.Equal(t, reportStateFailed, failed.ReportState)
		require.Equal(t, reportErrorFailed, failed.ReportError)
		require.Empty(t, failed.Models)
	})
}

func TestHandleWebhook_NoTranscriptSkipsReportCalculation(t *testing.T) {
	var calls atomic.Int32
	p := newTestPlugin(nil, []pluginsdk.Session{session("one", "")}, func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(sampleSessionJSON), nil
	})
	defer p.Close()

	_, response := callReadyWebhook(t, p, "task_id=task-1&active=one")
	require.Equal(t, reportStateReady, response.ReportState)
	require.Empty(t, response.ACPSessionID)
	require.Empty(t, response.GeneratedAt)
	require.Equal(t, int32(0), calls.Load())
}

func TestHandleWebhook_HostReadIsBoundedAndFailsClearly(t *testing.T) {
	t.Run("configuration", func(t *testing.T) {
		p := newPlugin()
		p.hostRead = 20 * time.Millisecond
		p.SetHost(&fakeHost{configFn: func(ctx context.Context) (map[string]any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		defer p.Close()

		started := time.Now()
		resp, err := p.HandleWebhook(context.Background(), webhookGet(webhookKeySessionCost, "active=one"))
		require.NoError(t, err)
		require.Equal(t, int32(503), resp.Status)
		require.Less(t, time.Since(started), time.Second)
	})

	t.Run("session list", func(t *testing.T) {
		p := newPlugin()
		p.hostRead = 20 * time.Millisecond
		p.SetHost(&fakeHost{sessionsFn: func(ctx context.Context, _ pluginsdk.SessionFilter, _ pluginsdk.Page) ([]pluginsdk.Session, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		defer p.Close()
		resp, err := p.HandleWebhook(context.Background(), webhookGet(webhookKeySessionCost, "active=one"))
		require.NoError(t, err)
		require.Equal(t, int32(503), resp.Status)
	})
}

func TestReportCoordinatorCloseCancelsWorker(t *testing.T) {
	started := make(chan struct{})
	p := newTestPlugin(nil, []pluginsdk.Session{session("one", "abc-123")}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, pending := callWebhook(t, p, "task_id=task-1&active=one")
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

func TestHandleWebhook_InvalidQueryIsBadRequest(t *testing.T) {
	p := newTestPlugin(nil, nil, modelsRunner([]byte(sampleSessionJSON), nil))
	resp, err := p.HandleWebhook(context.Background(), webhookGet(webhookKeySessionCost, "active=%zz"))
	require.NoError(t, err)
	require.Equal(t, int32(400), resp.Status)
}

func TestSessionMatches(t *testing.T) {
	require.True(t, sessionMatches("abc-123", "abc-123"))
	require.True(t, sessionMatches("rollout-x-abc-123", "abc-123"))
	require.False(t, sessionMatches("abc-1234", "abc-123"))
	require.False(t, sessionMatches("abc-123", ""))
}
