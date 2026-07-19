package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	webhookKeySessionCost = "session-cost"
	reportTimeout         = 120 * time.Second

	configKeyCommand       = "command"
	configKeyWarnThreshold = "warn_threshold"
	configKeyHighThreshold = "high_threshold"

	defaultWarnThreshold = 1.0  // USD — amount turns amber at or above this
	defaultHighThreshold = 10.0 // USD — amount turns red at or above this
)

// plugin implements pluginsdk.Plugin (via UnimplementedPlugin). Its single
// webhook is relayed by kandev from
// GET /api/plugins/kandev-session-cost/webhooks/session-cost over gRPC; the
// chat-toolbar UI is the only intended caller (host.api.fetch).
type plugin struct {
	pluginsdk.UnimplementedPlugin

	// Seams injected for tests; production values set in newPlugin.
	run      runner
	lookPath func(string) (string, error)
	now      func() time.Time
}

func newPlugin() *plugin {
	return &plugin{
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		lookPath: exec.LookPath,
		now:      time.Now,
	}
}

// sessionCostResponse is the session-cost webhook payload the chat-toolbar UI
// renders in its hover popover.
type sessionCostResponse struct {
	GeneratedAt string        `json:"generated_at"`
	Tokscale    InstallStatus `json:"tokscale"`
	// KandevSessionID is the active composer session id the UI asked about.
	KandevSessionID string `json:"kandev_session_id"`
	// ACPSessionID is the agent transcript id it resolved to (server-side),
	// empty if the agent hasn't reported one yet.
	ACPSessionID string `json:"acp_session_id"`
	// Found is true when tokscale had usage recorded for that transcript.
	Found     bool    `json:"found"`
	Cost      float64 `json:"cost"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CacheRead int64   `json:"cache_read"`
	// Turns is the number of messages tokscale parsed for this session's
	// transcript — the session's turn count, computed server-side.
	Turns int64 `json:"turns"`
	// CostPerTurn is the average spend per turn (Cost / Turns), 0 when there
	// are no turns yet. Precomputed here so the UI stays a dumb renderer.
	CostPerTurn float64        `json:"cost_per_turn"`
	Models      []sessionModel `json:"models"`
	// WarnThreshold / HighThreshold are the operator-configured USD cutoffs the
	// UI uses to colour the amount (green < warn <= amber < high <= red).
	WarnThreshold float64 `json:"warn_threshold"`
	HighThreshold float64 `json:"high_threshold"`
}

type sessionModel struct {
	Model string  `json:"model"`
	Cost  float64 `json:"cost"`
}

func (p *plugin) HandleWebhook(ctx context.Context, req *pluginsdk.WebhookRequest) (*pluginsdk.WebhookResponse, error) {
	if req.WebhookKey != webhookKeySessionCost {
		return jsonResponse(404, []byte(`{"error":"unknown webhook"}`)), nil
	}
	query, err := url.ParseQuery(req.Query)
	if err != nil {
		query = url.Values{}
	}
	taskID := query.Get("task_id")
	activeSessionID := query.Get("active")

	body, err := p.sessionCost(ctx, taskID, activeSessionID)
	if err != nil {
		log.Printf("session-cost failed: %v", err)
		msg, _ := json.Marshal(map[string]string{"error": err.Error()})
		return jsonResponse(500, msg), nil
	}
	return jsonResponse(200, body), nil
}

// sessionCost is the whole flow: resolve the active session's ACP transcript
// id via the Host data API (server-side matching — the UI only ever sends
// kandev ids), run tokscale grouped by session, and pick out the row for that
// transcript.
func (p *plugin) sessionCost(ctx context.Context, taskID, activeSessionID string) ([]byte, error) {
	warn, high := p.configuredThresholds(ctx)
	cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)

	resp := sessionCostResponse{
		GeneratedAt:     p.now().UTC().Format(time.RFC3339),
		KandevSessionID: activeSessionID,
		Models:          []sessionModel{},
		WarnThreshold:   warn,
		HighThreshold:   high,
	}

	// Server-side session -> ACP transcript id mapping via the Host data API.
	resp.ACPSessionID = p.resolveACPSessionID(ctx, taskID, activeSessionID)

	runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()

	entries, err := runSessionModels(runCtx, cmd, p.run)
	if err != nil {
		// Degrade to a status-only payload so the UI can render setup guidance
		// from the same shape it always reads.
		log.Printf("tokscale run failed (degrading): %v", err)
		resp.Tokscale = probeInstall(runCtx, cmd, p.run)
		return json.Marshal(resp)
	}
	resp.Tokscale = InstallStatus{Command: commandDisplay(cmd), Source: cmd.Source, Installed: true}

	if resp.ACPSessionID == "" {
		return json.Marshal(resp) // agent hasn't reported a transcript id yet
	}
	for _, e := range entries {
		if !sessionMatches(e.SessionID, resp.ACPSessionID) {
			continue
		}
		resp.Found = true
		resp.Cost += e.Cost
		resp.Input += e.Input
		resp.Output += e.Output
		resp.CacheRead += e.CacheRead
		resp.Turns += e.MessageCount
		resp.Models = append(resp.Models, sessionModel{Model: e.Model, Cost: e.Cost})
	}
	if resp.Turns > 0 {
		resp.CostPerTurn = resp.Cost / float64(resp.Turns)
	}
	return json.Marshal(resp)
}

// resolveACPSessionID looks up the active kandev session's ACP transcript id
// through the Host data API (capability api_read: ["sessions"]). Best-effort:
// returns "" when the Host is unavailable, the capability is denied, or the
// agent has not reported a transcript id yet.
func (p *plugin) resolveACPSessionID(ctx context.Context, taskID, activeSessionID string) string {
	host := p.Host()
	if host == nil || activeSessionID == "" {
		return ""
	}
	filter := pluginsdk.SessionFilter{}
	if taskID != "" {
		filter.TaskIDs = []string{taskID}
	}
	sessions, _, err := host.Sessions().List(ctx, filter, pluginsdk.Page{Limit: 200})
	if err != nil {
		log.Printf("resolving acp session id: %v", err)
		return ""
	}
	for _, s := range sessions {
		if s.ID == activeSessionID {
			return s.ACPSessionID
		}
	}
	return ""
}

func (p *plugin) configuredCommand(ctx context.Context) string {
	command, _ := p.config(ctx)[configKeyCommand].(string)
	return strings.TrimSpace(command)
}

// configuredThresholds reads the amber/red USD cutoffs from operator config,
// falling back to sane defaults. A configured value only wins when positive.
func (p *plugin) configuredThresholds(ctx context.Context) (warn, high float64) {
	cfg := p.config(ctx)
	warn = positiveFloatOr(cfg[configKeyWarnThreshold], defaultWarnThreshold)
	high = positiveFloatOr(cfg[configKeyHighThreshold], defaultHighThreshold)
	if high < warn {
		high = warn
	}
	return warn, high
}

func (p *plugin) config(ctx context.Context) map[string]any {
	host := p.Host()
	if host == nil {
		return map[string]any{}
	}
	cfg, err := host.GetConfig(ctx)
	if err != nil {
		log.Printf("reading plugin config: %v", err)
		return map[string]any{}
	}
	return cfg
}

// positiveFloatOr coerces a JSON config value (numbers arrive as float64) to a
// positive float, or returns the fallback.
func positiveFloatOr(v any, fallback float64) float64 {
	if f, ok := v.(float64); ok && f > 0 {
		return f
	}
	return fallback
}

// sessionMatches reports whether a tokscale session key belongs to the given
// ACP transcript id. Different agent CLIs key their transcripts differently:
//   - Claude uses the bare transcript UUID, so kandev's acp.session_id matches
//     tokscale's sessionId exactly.
//   - Codex keys sessions by the rollout filename "rollout-<timestamp>-<uuid>",
//     so the ACP UUID is the trailing segment.
//
// Matching exact-or-suffix covers both without over-matching (the UUID is
// globally unique and hyphen-delimited).
func sessionMatches(tokscaleSessionID, acpSessionID string) bool {
	if acpSessionID == "" {
		return false
	}
	return tokscaleSessionID == acpSessionID ||
		strings.HasSuffix(tokscaleSessionID, "-"+acpSessionID)
}

func jsonResponse(status int32, body []byte) *pluginsdk.WebhookResponse {
	return &pluginsdk.WebhookResponse{
		Status:  status,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
	}
}

// sessionModelEntry mirrors one element of `tokscale models --json --group-by
// session,model` output (tokscale 4.5.x). Only the fields used are declared.
type sessionModelEntry struct {
	SessionID    string  `json:"sessionId"`
	Model        string  `json:"model"`
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cacheRead"`
	MessageCount int64   `json:"messageCount"`
	Cost         float64 `json:"cost"`
}

type sessionModelsReport struct {
	Entries []sessionModelEntry `json:"entries"`
}

// runSessionModels runs tokscale grouped by session and model and returns the
// per-(session,model) entries.
func runSessionModels(ctx context.Context, cmd resolvedCommand, run runner) ([]sessionModelEntry, error) {
	args := append(append([]string{}, cmd.Argv[1:]...), "models", "--json", "--group-by", "session,model")
	out, err := run(ctx, cmd.Argv[0], args...)
	if err != nil {
		return nil, fmt.Errorf("running %s: %w", cmd.Argv[0], err)
	}
	var report sessionModelsReport
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("parsing tokscale output: %w", err)
	}
	return report.Entries, nil
}
