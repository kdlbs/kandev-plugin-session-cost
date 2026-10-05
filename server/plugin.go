package main

import (
	"bytes"
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
	hostReadTimeout       = 3 * time.Second

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
	reports  *reportCoordinator
	hostRead time.Duration
}

func newPlugin() *plugin {
	p := &plugin{
		run:      runCommand,
		lookPath: exec.LookPath,
		now:      time.Now,
		hostRead: hostReadTimeout,
	}
	p.reports = newReportCoordinator(func() runner { return p.run }, func() time.Time { return p.now() })
	return p
}

func (p *plugin) Close() {
	if p.reports != nil {
		p.reports.Close()
	}
}

// sessionCostResponse is the session-cost webhook payload the chat-toolbar UI
// renders in its hover popover.
type sessionCostResponse struct {
	GeneratedAt string        `json:"generated_at,omitempty"`
	Tokscale    InstallStatus `json:"tokscale"`
	ReportState string        `json:"report_state"`
	ReportError string        `json:"report_error,omitempty"`
	Stale       bool          `json:"stale"`
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
	Model     string  `json:"model"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CacheRead int64   `json:"cache_read"`
	Cost      float64 `json:"cost"`
}

func (p *plugin) HandleWebhook(ctx context.Context, req *pluginsdk.WebhookRequest) (*pluginsdk.WebhookResponse, error) {
	if req.WebhookKey != webhookKeySessionCost {
		return jsonResponse(404, []byte(`{"error":"unknown webhook"}`)), nil
	}
	query, err := url.ParseQuery(req.Query)
	if err != nil {
		return jsonResponse(400, []byte(`{"error":"invalid query"}`)), nil
	}
	body, status, err := p.sessionCost(ctx, query.Get("task_id"), query.Get("active"), query.Get("refresh") == "1")
	if err != nil {
		log.Printf("session-cost response encoding failed: %v", err)
		return jsonResponse(500, []byte(`{"error":"internal_error"}`)), nil
	}
	return jsonResponse(status, body), nil
}

// sessionCost bounds Host reads, then projects the latest process-wide report
// snapshot for the active session. Report work runs independently of the HTTP
// request lifetime.
func (p *plugin) sessionCost(ctx context.Context, taskID, activeSessionID string, refresh bool) ([]byte, int32, error) {
	host := p.Host()
	if host == nil {
		return []byte(`{"error":"host_unavailable"}`), 503, nil
	}
	hostCtx, cancel := context.WithTimeout(ctx, p.hostRead)
	defer cancel()
	cfg, err := host.GetConfig(hostCtx)
	if err != nil {
		log.Printf("session-cost config read failed")
		return []byte(`{"error":"host_unavailable"}`), 503, nil
	}
	warn, high := configuredThresholds(cfg)
	resp := sessionCostResponse{
		KandevSessionID: activeSessionID,
		Models:          []sessionModel{},
		ReportState:     reportStateReady,
		WarnThreshold:   warn,
		HighThreshold:   high,
	}

	if activeSessionID != "" {
		filter := pluginsdk.SessionFilter{}
		if taskID != "" {
			filter.TaskIDs = []string{taskID}
		}
		sessions, _, listErr := host.Sessions().List(hostCtx, filter, pluginsdk.Page{Limit: 200})
		if listErr != nil {
			log.Printf("session-cost session read failed")
			return []byte(`{"error":"host_unavailable"}`), 503, nil
		}
		for _, session := range sessions {
			if session.ID == activeSessionID {
				resp.ACPSessionID = session.ACPSessionID
				break
			}
		}
	}

	if resp.ACPSessionID == "" {
		body, err := json.Marshal(resp)
		return body, 200, err
	}

	command, _ := cfg[configKeyCommand].(string)
	cmd := resolveCommand(strings.TrimSpace(command), p.lookPath)
	view := p.reports.Request(cmd, refresh)
	resp.ReportState = view.state
	resp.ReportError = view.errCode
	resp.Stale = view.stale
	resp.Tokscale = InstallStatus{
		Command:   commandDisplay(cmd),
		Source:    cmd.Source,
		Installed: view.errCode != reportErrorUnavailable,
	}
	if view.snapshot != nil {
		resp.GeneratedAt = view.snapshot.generatedAt.UTC().Format(time.RFC3339)
		resp.Tokscale = view.snapshot.tokscale
		usage := view.snapshot.usageFor(resp.ACPSessionID)
		resp.Found = usage.Found
		resp.Cost = usage.Cost
		resp.Input = usage.Input
		resp.Output = usage.Output
		resp.CacheRead = usage.CacheRead
		resp.Turns = usage.Turns
		resp.Models = usage.Models
	}
	if resp.Turns > 0 {
		resp.CostPerTurn = resp.Cost / float64(resp.Turns)
	}
	body, err := json.Marshal(resp)
	return body, 200, err
}

// configuredThresholds reads the amber/red USD cutoffs from one Host config
// response, falling back to sane defaults. A configured value only wins when
// positive.
func configuredThresholds(cfg map[string]any) (warn, high float64) {
	warn = positiveFloatOr(cfg[configKeyWarnThreshold], defaultWarnThreshold)
	high = positiveFloatOr(cfg[configKeyHighThreshold], defaultHighThreshold)
	if high < warn {
		high = warn
	}
	return warn, high
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
// session,model` output (tokscale 4.15.x). Only the fields used are declared.
type sessionModelEntry struct {
	SessionID    string  `json:"sessionId"`
	Model        string  `json:"model"`
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cacheRead"`
	MessageCount int64   `json:"messageCount"`
	Cost         float64 `json:"cost"`
}

// runSessionModels runs tokscale grouped by session and model and returns the
// per-(session,model) entries.
func runSessionModels(ctx context.Context, cmd resolvedCommand, run runner) ([]sessionModelEntry, error) {
	args := append(append([]string{}, cmd.Argv[1:]...), "models", "--json", "--group-by", "session,model")
	out, err := run(ctx, cmd.Argv[0], args...)
	if err != nil {
		return nil, fmt.Errorf("running %s: %w", cmd.Argv[0], err)
	}
	var report struct {
		Entries json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("parsing tokscale output: %w", err)
	}
	if len(report.Entries) == 0 || bytes.Equal(bytes.TrimSpace(report.Entries), []byte("null")) {
		return nil, fmt.Errorf("parsing tokscale output: missing entries")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(report.Entries, &rows); err != nil {
		return nil, fmt.Errorf("parsing tokscale entries: %w", err)
	}
	if rows == nil {
		return nil, fmt.Errorf("parsing tokscale entries: expected an array")
	}
	entries := make([]sessionModelEntry, 0, len(rows))
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("parsing tokscale entry: expected an object")
		}
		for _, required := range []string{"sessionId", "model", "input", "output", "cacheRead", "messageCount", "cost"} {
			value, ok := fields[required]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("parsing tokscale entry: missing %s", required)
			}
		}
		var entry sessionModelEntry
		if err := json.Unmarshal(row, &entry); err != nil {
			return nil, fmt.Errorf("parsing tokscale entry: %w", err)
		}
		if entry.SessionID == "" || entry.Model == "" {
			return nil, fmt.Errorf("parsing tokscale entry: empty sessionId or model")
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
