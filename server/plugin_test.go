// Package main tests. Exercises plugin.HandleWebhook end to end against a fake
// Host and an injected runner — no go-plugin spawn and no real tokscale needed,
// mirroring the other kandev plugins' test approach.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

// fakeHost serves GetConfig and Sessions() — the only Host surfaces this
// plugin uses. Everything else comes from UnimplementedHostData.
type fakeHost struct {
	pluginsdk.UnimplementedHostData
	config   map[string]any
	sessions []pluginsdk.Session
}

func (h *fakeHost) GetState(context.Context, string, string, string) (map[string]any, bool, error) {
	return nil, false, nil
}
func (h *fakeHost) SetState(context.Context, string, string, string, map[string]any) error {
	return nil
}
func (h *fakeHost) DeleteState(context.Context, string, string, string) error { return nil }
func (h *fakeHost) ListState(context.Context, string, string) ([]pluginsdk.StateEntry, error) {
	return nil, nil
}
func (h *fakeHost) GetConfig(context.Context) (map[string]any, error) {
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
	return fakeSessionReader{sessions: h.sessions}
}

type fakeSessionReader struct {
	sessions []pluginsdk.Session
}

func (r fakeSessionReader) List(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, *pluginsdk.PageInfo, error) {
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
			return []byte("tokscale 4.5.3\n"), nil
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

func TestHandleWebhook_UnknownKey(t *testing.T) {
	p := newTestPlugin(nil, nil, modelsRunner([]byte(sampleSessionJSON), nil))
	resp, err := p.HandleWebhook(context.Background(), webhookGet("nope", ""))
	require.NoError(t, err)
	require.Equal(t, int32(404), resp.Status)
}

func TestHandleWebhook_FoundSumsSessionRows(t *testing.T) {
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	resp, err := p.HandleWebhook(context.Background(),
		webhookGet(webhookKeySessionCost, "task_id=task-1&active=kandev-sess"))
	require.NoError(t, err)
	require.Equal(t, int32(200), resp.Status)

	d := decode(t, resp.Body)
	require.True(t, d.Tokscale.Installed)
	require.Equal(t, "abc-123", d.ACPSessionID)
	require.True(t, d.Found)
	require.InDelta(t, 1.30, d.Cost, 1e-9, "sums both model rows for the matched session")
	require.Equal(t, int64(1050), d.Input)
	require.Equal(t, int64(520), d.Output)
	require.Equal(t, int64(5), d.Turns, "sums messageCount across the session's model rows")
	require.InDelta(t, 1.30/5.0, d.CostPerTurn, 1e-9, "avg cost per turn computed server-side")
	require.Len(t, d.Models, 2)
	// Default thresholds echoed for the UI to colour by.
	require.Equal(t, defaultWarnThreshold, d.WarnThreshold)
	require.Equal(t, defaultHighThreshold, d.HighThreshold)
}

func TestHandleWebhook_NoUsageForSession(t *testing.T) {
	sessions := []pluginsdk.Session{session("kandev-sess", "no-match")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	resp, err := p.HandleWebhook(context.Background(),
		webhookGet(webhookKeySessionCost, "task_id=task-1&active=kandev-sess"))
	require.NoError(t, err)

	d := decode(t, resp.Body)
	require.Equal(t, "no-match", d.ACPSessionID)
	require.False(t, d.Found)
	require.Zero(t, d.Cost)
}

func TestHandleWebhook_NoTranscriptYet(t *testing.T) {
	// Session exists but the agent hasn't reported an ACP transcript id yet.
	sessions := []pluginsdk.Session{session("kandev-sess", "")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	resp, err := p.HandleWebhook(context.Background(),
		webhookGet(webhookKeySessionCost, "task_id=task-1&active=kandev-sess"))
	require.NoError(t, err)

	d := decode(t, resp.Body)
	require.Empty(t, d.ACPSessionID)
	require.False(t, d.Found)
	require.True(t, d.Tokscale.Installed)
}

func TestHandleWebhook_CodexRolloutSuffixMatches(t *testing.T) {
	// Codex keys sessions by rollout filename; the ACP UUID is the suffix.
	report := `{"entries":[{"sessionId":"rollout-2026-07-19-abc-123","model":"gpt-x","cost":2.5,"messageCount":3}]}`
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, modelsRunner([]byte(report), nil))

	resp, err := p.HandleWebhook(context.Background(),
		webhookGet(webhookKeySessionCost, "task_id=task-1&active=kandev-sess"))
	require.NoError(t, err)

	d := decode(t, resp.Body)
	require.True(t, d.Found)
	require.InDelta(t, 2.5, d.Cost, 1e-9)
	require.Equal(t, int64(3), d.Turns)
	require.InDelta(t, 2.5/3.0, d.CostPerTurn, 1e-9)
}

func TestHandleWebhook_ThresholdsFromConfig(t *testing.T) {
	config := map[string]any{"warn_threshold": 2.0, "high_threshold": 20.0}
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(config, sessions, modelsRunner([]byte(sampleSessionJSON), nil))

	resp, err := p.HandleWebhook(context.Background(),
		webhookGet(webhookKeySessionCost, "task_id=task-1&active=kandev-sess"))
	require.NoError(t, err)

	d := decode(t, resp.Body)
	require.Equal(t, 2.0, d.WarnThreshold)
	require.Equal(t, 20.0, d.HighThreshold)
}

func TestHandleWebhook_HighClampedBelowWarn(t *testing.T) {
	config := map[string]any{"warn_threshold": 5.0, "high_threshold": 1.0}
	p := newTestPlugin(config, nil, modelsRunner([]byte(sampleSessionJSON), nil))

	resp, err := p.HandleWebhook(context.Background(),
		webhookGet(webhookKeySessionCost, "active=kandev-sess"))
	require.NoError(t, err)

	d := decode(t, resp.Body)
	require.Equal(t, 5.0, d.WarnThreshold)
	require.Equal(t, 5.0, d.HighThreshold, "high never renders below warn")
}

func TestHandleWebhook_DegradesWhenTokscaleMissing(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exec: \"npx\": executable file not found in $PATH")
	}
	sessions := []pluginsdk.Session{session("kandev-sess", "abc-123")}
	p := newTestPlugin(nil, sessions, run)

	resp, err := p.HandleWebhook(context.Background(),
		webhookGet(webhookKeySessionCost, "task_id=task-1&active=kandev-sess"))
	require.NoError(t, err)
	require.Equal(t, int32(200), resp.Status, "a missing CLI is a degraded payload, not a 500")

	d := decode(t, resp.Body)
	require.False(t, d.Tokscale.Installed)
	require.Contains(t, d.Tokscale.Error, "not found")
	require.False(t, d.Found)
}

func TestSessionMatches(t *testing.T) {
	require.True(t, sessionMatches("abc-123", "abc-123"))
	require.True(t, sessionMatches("rollout-x-abc-123", "abc-123"))
	require.False(t, sessionMatches("abc-1234", "abc-123"))
	require.False(t, sessionMatches("abc-123", ""))
}
