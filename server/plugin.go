package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	webhookKeySessionCost = "session-cost"
	actionKeySessionUsage = "session-usage"
	actionKeyImportStart  = "historical-import-start"
	actionKeyImportStatus = "historical-import-status"
	actionKeyImportCancel = "historical-import-cancel"
	reportTimeout         = 120 * time.Second

	configKeyCommand       = "command"
	configKeyWarnThreshold = "warn_threshold"
	configKeyHighThreshold = "high_threshold"
	configKeyCollect       = "collect_statistics"
	configKeyInterval      = "collection_interval_minutes"

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
	run              runner
	lookPath         func(string) (string, error)
	now              func() time.Time
	reportMu         sync.Mutex
	reports          *reportCoordinator
	collectorMu      sync.Mutex
	collectorStarted bool
	collectorCancel  context.CancelFunc
	importMu         sync.Mutex
	importRunning    bool
	importWorkspace  string
	importCancel     context.CancelFunc
}

func newPlugin() *plugin {
	return &plugin{
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		lookPath: exec.LookPath,
		now:      time.Now,
		reports:  newReportCoordinator(),
	}
}

func (p *plugin) SetHost(host pluginsdk.Host) {
	p.UnimplementedPlugin.SetHost(host)
	p.startCollector()
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
	Found      bool    `json:"found"`
	Cost       float64 `json:"cost"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	Total      int64   `json:"total"`
	// Turns is the number of messages tokscale parsed for this session's
	// transcript — the session's turn count, computed server-side.
	Turns int64 `json:"turns"`
	// CostPerTurn is the average spend per turn (Cost / Turns), 0 when there
	// are no turns yet. Precomputed here so the UI stays a dumb renderer.
	CostPerTurn float64        `json:"cost_per_turn"`
	CostKnown   bool           `json:"cost_known"`
	Models      []sessionModel `json:"models"`
	Saved       bool           `json:"saved"`
	Stale       bool           `json:"stale"`
	Coverage    string         `json:"coverage"`
	LastRefresh string         `json:"last_refresh,omitempty"`
	Error       string         `json:"error,omitempty"`
	// WarnThreshold / HighThreshold are the operator-configured USD cutoffs the
	// UI uses to colour the amount (green < warn <= amber < high <= red).
	WarnThreshold float64 `json:"warn_threshold"`
	HighThreshold float64 `json:"high_threshold"`

	// presence is kept out of the wire response. It carries the source-field
	// presence bits needed when a cumulative tokscale row is saved. A JSON
	// number of zero is a measured zero; an omitted tokscale field must remain
	// nil in the core measurement.
	presence   map[string]usagePresence
	turnsKnown bool
}

type sessionModel struct {
	Model      string  `json:"model"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	Provider   string  `json:"provider,omitempty"`
	Cost       float64 `json:"cost"`
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

// HandleAction serves the authenticated saved/refresh flow used by the
// toolbar. The action context is host-verified, so the plugin receives only
// an authorized task/session/workspace tuple.
func (p *plugin) HandleAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if req == nil {
		return &pluginsdk.PluginActionResponse{Status: 404, Headers: jsonHeaders(), Body: []byte(`{"error":"unknown action"}`)}, nil
	}
	switch req.ActionKey {
	case actionKeyImportStart, actionKeyImportStatus, actionKeyImportCancel:
		return p.handleHistoricalImportAction(ctx, req)
	case actionKeySessionUsage:
		// Continue with the task-scoped saved usage action below.
	default:
		return &pluginsdk.PluginActionResponse{Status: 404, Headers: jsonHeaders(), Body: []byte(`{"error":"unknown action"}`)}, nil
	}
	var body struct {
		Refresh bool `json:"refresh"`
	}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return &pluginsdk.PluginActionResponse{Status: 400, Headers: jsonHeaders(), Body: []byte(`{"error":"invalid request"}`)}, nil
		}
	}
	response, err := p.sessionUsage(ctx, req.Context.WorkspaceID, req.Context.TaskID, req.Context.SessionID, body.Refresh)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &pluginsdk.PluginActionResponse{Status: 200, Headers: jsonHeaders(), Body: encoded}, nil
}

// sessionCost is the whole flow: resolve the active session's ACP transcript
// id via the Host data API (server-side matching — the UI only ever sends
// kandev ids), run tokscale grouped by session, and pick out the row for that
// transcript.
func (p *plugin) sessionCost(ctx context.Context, taskID, activeSessionID string) ([]byte, error) {
	resp, err := p.calculateSessionCost(ctx, taskID, activeSessionID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(resp)
}

func (p *plugin) calculateSessionCost(ctx context.Context, taskID, activeSessionID string) (*sessionCostResponse, error) {
	warn, high := p.configuredThresholds(ctx)
	cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)

	resp := sessionCostResponse{
		GeneratedAt:     p.now().UTC().Format(time.RFC3339),
		KandevSessionID: activeSessionID,
		Models:          []sessionModel{},
		WarnThreshold:   warn,
		HighThreshold:   high,
		Coverage:        "missing",
		CostKnown:       true,
		turnsKnown:      true,
	}

	// Server-side session -> ACP transcript id mapping via the Host data API.
	resp.ACPSessionID = p.resolveACPSessionID(ctx, taskID, activeSessionID)

	runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()

	entries, err := p.runReport(runCtx, cmd)
	if err != nil {
		// Degrade to a status-only payload so the UI can render setup guidance
		// from the same shape it always reads.
		log.Printf("tokscale run failed (degrading): %v", err)
		resp.Tokscale = probeInstall(runCtx, cmd, p.run)
		resp.Error = err.Error()
		return &resp, nil
	}
	resp.Tokscale = InstallStatus{Command: commandDisplay(cmd), Source: cmd.Source, Installed: true}

	if resp.ACPSessionID == "" {
		return &resp, nil // agent hasn't reported a transcript id yet
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
		resp.CacheWrite += e.CacheWrite
		resp.Reasoning += e.Reasoning
		presence := presenceForEntry(e)
		total := e.Input + e.Output + e.CacheRead + e.CacheWrite + e.Reasoning
		if presence.totalValue != nil {
			total = *presence.totalValue
		}
		resp.Total += total
		resp.Turns += e.MessageCount
		resp.turnsKnown = resp.turnsKnown && presence.turns
		resp.CostKnown = resp.CostKnown && presence.cost
		if resp.presence == nil {
			resp.presence = make(map[string]usagePresence)
		}
		resp.presence[usagePresenceKey(e.Model, e.Provider)] = presence
		resp.Models = append(resp.Models, sessionModel{
			Model:      e.Model,
			Input:      e.Input,
			Output:     e.Output,
			CacheRead:  e.CacheRead,
			CacheWrite: e.CacheWrite,
			Reasoning:  e.Reasoning,
			Provider:   e.Provider,
			Cost:       e.Cost,
		})
	}
	if resp.Found {
		resp.Coverage = "complete"
	}
	if resp.CostKnown && resp.Turns > 0 {
		resp.CostPerTurn = resp.Cost / float64(resp.Turns)
	}
	return &resp, nil
}

func jsonHeaders() map[string]string { return map[string]string{"Content-Type": "application/json"} }

func (p *plugin) runReport(ctx context.Context, cmd resolvedCommand) ([]sessionModelEntry, error) {
	p.reportMu.Lock()
	if p.reports == nil {
		p.reports = newReportCoordinator()
	}
	reports := p.reports
	p.reportMu.Unlock()
	return reports.run(ctx, cmd, p.run)
}

func (p *plugin) runDatedReport(ctx context.Context, cmd resolvedCommand, since, until string) ([]sessionModelEntry, error) {
	p.reportMu.Lock()
	if p.reports == nil {
		p.reports = newReportCoordinator()
	}
	reports := p.reports
	p.reportMu.Unlock()
	scope := "date:" + since + ":" + until
	return reports.runScoped(ctx, cmd, scope, p.run, "--since", since, "--until", until)
}

func (p *plugin) sessionUsage(ctx context.Context, workspaceID, taskID, sessionID string, refresh bool) (*sessionCostResponse, error) {
	warn, high := p.configuredThresholds(ctx)
	var saved *sessionCostResponse
	var hasSaved bool
	if workspaceID != "" && taskID != "" && sessionID != "" {
		saved, hasSaved = p.savedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high)
	}
	if !refresh {
		if hasSaved {
			return saved, nil
		}
	}
	response, err := p.calculateSessionCost(ctx, taskID, sessionID)
	if err != nil {
		return nil, err
	}
	if response.Found && p.collectionEnabled(ctx) {
		if err := p.persistSessionUsage(ctx, workspaceID, taskID, sessionID, response); err != nil {
			log.Printf("persisting session usage: %v", err)
			response.Stale = true
			response.Error = "usage was calculated but could not be accepted"
			var rejected *usageWriteRejectedError
			if errors.As(err, &rejected) && rejected.complete && len(rejected.measurements) > 0 {
				canonical := responseFromSaved(rejected.measurements, sessionID, warn, high)
				canonical.ACPSessionID = response.ACPSessionID
				canonical.Stale = true
				canonical.Error = response.Error
				return &canonical, nil
			}
			// A write can lose a revision race after the initial saved read. Do
			// not return the rejected calculation as if it were current. Read
			// the host's canonical value, which may be a newer plugin/native
			// observation, and attach the refresh error to that value.
			if canonical, ok := p.savedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high); ok {
				canonical.Stale = true
				canonical.Error = response.Error
				return canonical, nil
			}
			// If the authoritative read is unavailable, clear the rejected
			// numbers before returning the status so the UI cannot mistake
			// them for accepted saved usage.
			response.Found = false
			response.Cost = 0
			response.Input = 0
			response.Output = 0
			response.CacheRead = 0
			response.CacheWrite = 0
			response.Reasoning = 0
			response.Total = 0
			response.Turns = 0
			response.CostPerTurn = 0
			response.Models = []sessionModel{}
		} else if canonical, ok := p.savedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high); ok {
			return canonical, nil
		}
	}
	if hasSaved && !response.Found {
		saved.Stale = true
		saved.Error = response.Error
		if saved.Error == "" && !response.Tokscale.Installed {
			saved.Error = "refresh unavailable"
		}
		return saved, nil
	}
	return response, nil
}

func (p *plugin) savedSessionUsage(ctx context.Context, workspaceID, taskID, sessionID string, warn, high float64) (*sessionCostResponse, bool) {
	host := p.Host()
	if host == nil || workspaceID == "" || sessionID == "" {
		return nil, false
	}
	var items []pluginsdk.SessionUsageMeasurement
	page := pluginsdk.Page{Limit: 200}
	for {
		pageItems, info, err := host.Usage().ListCanonical(ctx, pluginsdk.SessionUsageFilter{
			WorkspaceID: workspaceID, TaskIDs: []string{taskID}, SessionIDs: []string{sessionID},
		}, page)
		if err != nil {
			return nil, false
		}
		items = append(items, pageItems...)
		if info == nil || !info.HasMore || info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
	}
	if len(items) == 0 {
		return nil, false
	}
	response := responseFromSaved(items, sessionID, warn, high)
	if response.ACPSessionID == "" {
		response.ACPSessionID = p.resolveACPSessionID(ctx, taskID, sessionID)
	}
	return &response, true
}

func responseFromSaved(items []pluginsdk.SessionUsageMeasurement, sessionID string, warn, high float64) sessionCostResponse {
	response := sessionCostResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339), KandevSessionID: sessionID,
		Models: []sessionModel{}, WarnThreshold: warn, HighThreshold: high, Saved: true,
		Coverage: "complete", CostKnown: true, Tokscale: InstallStatus{Installed: true},
	}
	for _, item := range items {
		if response.ACPSessionID == "" {
			response.ACPSessionID = item.TranscriptID
		}
		response.Found = true
		costAvailable := item.CostSubcents != nil &&
			(item.CostCoverage == "" || item.CostCoverage == "complete") &&
			item.CostBasis != "unknown" && item.CostBasis != "mixed" &&
			item.Currency != "mixed"
		response.CostKnown = response.CostKnown && costAvailable
		response.Input += valueOrZero(item.InputTokens)
		response.Output += valueOrZero(item.OutputTokens)
		response.CacheRead += valueOrZero(item.CacheReadTokens)
		response.CacheWrite += valueOrZero(item.CacheWriteTokens)
		response.Reasoning += valueOrZero(item.ReasoningTokens)
		response.Total += valueOrZero(item.TotalTokens)
		response.Turns += valueOrZero(item.Turns)
		if costAvailable {
			response.Cost += float64(*item.CostSubcents) / 10000
		}
		if item.Coverage != "complete" {
			response.Coverage = "partial"
		}
		response.Stale = response.Stale || item.Stale
		response.Models = append(response.Models, sessionModel{
			Model: item.Model, Input: valueOrZero(item.InputTokens), Output: valueOrZero(item.OutputTokens),
			CacheRead: valueOrZero(item.CacheReadTokens), CacheWrite: valueOrZero(item.CacheWriteTokens),
			Reasoning: valueOrZero(item.ReasoningTokens), Provider: item.Provider, Cost: costFromSubcents(item.CostSubcents),
		})
		if item.CollectedAt != "" && item.CollectedAt > response.LastRefresh {
			response.LastRefresh = item.CollectedAt
		}
	}
	if response.CostKnown && response.Turns > 0 {
		response.CostPerTurn = response.Cost / float64(response.Turns)
	}
	return response
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func costFromSubcents(value *int64) float64 {
	if value == nil {
		return 0
	}
	return float64(*value) / 10000
}

type usageCoverage struct {
	Date     string
	Timezone string
}

func (p *plugin) persistSessionUsage(ctx context.Context, workspaceID, taskID, sessionID string, response *sessionCostResponse) error {
	return p.persistSessionUsageWithCoverage(ctx, workspaceID, taskID, sessionID, response, usageCoverage{})
}

func (p *plugin) persistSessionUsageWithCoverage(ctx context.Context, workspaceID, taskID, sessionID string, response *sessionCostResponse, coverage usageCoverage) error {
	host := p.Host()
	if host == nil || workspaceID == "" || taskID == "" || sessionID == "" || response.ACPSessionID == "" {
		return nil
	}
	now := p.now().UTC()
	existing, err := p.savedUsageMeasurements(ctx, workspaceID, taskID, sessionID)
	if err != nil {
		return err
	}
	existingDigests := make(map[string]string, len(existing))
	for _, item := range existing {
		existingDigests[usageMeasurementKey(item.Model, item.Provider, item.UsageIdentity, item.CoverageKey)] = item.PayloadDigest
	}
	usageIdentity, coverageKey, sourceDate, sourceTimezone := usageIdentityForCoverage(response.ACPSessionID, coverage)
	unchanged := true
	for i, model := range response.Models {
		key := usageMeasurementKey(model.Model, model.Provider, usageIdentity, coverageKey)
		presence := responsePresenceForModel(response, model)
		turns := (*int64)(nil)
		if i == 0 && response.turnsKnown {
			turns = ptr(response.Turns)
		}
		total := model.Input + model.Output + model.CacheRead + model.CacheWrite + model.Reasoning
		if presence.totalValue != nil {
			total = *presence.totalValue
		}
		if existingDigests[key] != usagePayloadDigest(response.ACPSessionID, model, presence, turns, ptrIf(presence.total, total)) {
			unchanged = false
			break
		}
	}
	if unchanged {
		return nil
	}
	revision := now.UnixNano()
	items := make([]pluginsdk.SessionUsageMeasurement, 0, len(response.Models))
	for i, model := range response.Models {
		presence := responsePresenceForModel(response, model)
		var turns *int64
		if i == 0 && response.turnsKnown {
			turns = ptr(response.Turns)
		}
		total := model.Input + model.Output + model.CacheRead + model.CacheWrite + model.Reasoning
		if presence.totalValue != nil {
			total = *presence.totalValue
		}
		cost := ptrIf(presence.cost, int64(math.Round(model.Cost*10000)))
		costBasis := "unknown"
		if presence.cost {
			costBasis = "estimated"
		}
		digest := usagePayloadDigest(response.ACPSessionID, model, presence, turns, ptrIf(presence.total, total))
		items = append(items, pluginsdk.SessionUsageMeasurement{
			WorkspaceID: workspaceID, TaskID: taskID, SessionID: sessionID, TranscriptID: response.ACPSessionID,
			SourceRecordID: response.ACPSessionID, UsageIdentity: usageIdentity,
			Model: model.Model, Provider: model.Provider, SourceVersion: "tokscale-4.15.1",
			Revision: revision, PayloadDigest: digest,
			ObservedAt: now.Format(time.RFC3339Nano), CollectedAt: now.Format(time.RFC3339Nano),
			InputTokens: ptrIf(presence.input, model.Input), OutputTokens: ptrIf(presence.output, model.Output),
			CacheReadTokens: ptrIf(presence.cacheRead, model.CacheRead), CacheWriteTokens: ptrIf(presence.cacheWrite, model.CacheWrite),
			ReasoningTokens: ptrIf(presence.reasoning, model.Reasoning), Turns: turns,
			TotalTokens: ptrIf(presence.total, total), CostSubcents: cost, Currency: "USD",
			CostBasis: costBasis, CostCoverage: costCoverageForPresence(presence), Coverage: "complete",
			SourceTimezone: sourceTimezone, SourceDate: sourceDate, CoverageKey: coverageKey,
			AttributionStatus: "attributed",
			Estimated:         presence.cost,
		})
	}
	results, err := host.Usage().UpsertBatch(ctx, workspaceID, items)
	if err != nil {
		return err
	}
	if len(results) != len(items) {
		return fmt.Errorf("usage write returned %d results for %d measurements", len(results), len(items))
	}
	accepted := make([]pluginsdk.SessionUsageMeasurement, 0, len(results))
	allHaveMeasurements := true
	for _, result := range results {
		if result.Measurement != nil {
			accepted = append(accepted, *result.Measurement)
		} else {
			allHaveMeasurements = false
		}
		switch result.Status {
		case "applied", "unchanged":
			continue
		case "stale", "conflict":
			return &usageWriteRejectedError{status: result.Status, measurements: accepted, complete: allHaveMeasurements}
		default:
			if result.Error != "" {
				return errors.New(result.Error)
			}
			return fmt.Errorf("usage write for %s returned status %q", result.Model, result.Status)
		}
	}
	return nil
}

// usageWriteRejectedError carries the rows the Host accepted as the current
// values when a batch loses a revision race. Callers that are presenting a
// refresh can render these rows immediately; background collection can simply
// retry the observation on its next pass.
type usageWriteRejectedError struct {
	status       string
	measurements []pluginsdk.SessionUsageMeasurement
	complete     bool
}

func (e *usageWriteRejectedError) Error() string {
	return fmt.Sprintf("usage write %s was rejected", e.status)
}

func usageMeasurementKey(model, provider, identity, coverageKey string) string {
	return strings.Join([]string{model, provider, identity, coverageKey}, "\x00")
}

func usageIdentityForCoverage(transcriptID string, coverage usageCoverage) (identity, coverageKey, sourceDate, sourceTimezone string) {
	if coverage.Date == "" {
		return "lifetime:" + transcriptID, "", "", ""
	}
	return "date:" + transcriptID + ":" + coverage.Date, coverage.Date, coverage.Date, coverage.Timezone
}

func costCoverageForPresence(presence usagePresence) string {
	if presence.cost {
		return "complete"
	}
	return "missing"
}

func (p *plugin) savedUsageMeasurements(ctx context.Context, workspaceID, taskID, sessionID string) ([]pluginsdk.SessionUsageMeasurement, error) {
	host := p.Host()
	if host == nil {
		return nil, nil
	}
	// The Host source-observation projection chooses either the lifetime rows or
	// the dated buckets for an all-time query. Read both projections so a dated
	// write can compare its digest with an existing bucket even when a lifetime
	// snapshot is also present. The stable key below removes the overlap when a
	// source has only one representation.
	filters := []pluginsdk.SessionUsageFilter{
		{WorkspaceID: workspaceID, TaskIDs: []string{taskID}, SessionIDs: []string{sessionID}},
		{WorkspaceID: workspaceID, TaskIDs: []string{taskID}, SessionIDs: []string{sessionID}, GroupBy: "day"},
	}
	byKey := make(map[string]pluginsdk.SessionUsageMeasurement)
	for _, filter := range filters {
		page := pluginsdk.Page{Limit: 200}
		for {
			items, info, err := host.Usage().List(ctx, filter, page)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				byKey[usageMeasurementKey(item.Model, item.Provider, item.UsageIdentity, item.CoverageKey)] = item
			}
			if info == nil || !info.HasMore || info.NextCursor == "" {
				break
			}
			page.Cursor = info.NextCursor
		}
	}
	all := make([]pluginsdk.SessionUsageMeasurement, 0, len(byKey))
	for _, item := range byKey {
		all = append(all, item)
	}
	return all, nil
}

func ptr(value int64) *int64 { return &value }

func ptrIf(known bool, value int64) *int64 {
	if !known {
		return nil
	}
	return ptr(value)
}

func responsePresenceForModel(response *sessionCostResponse, model sessionModel) usagePresence {
	if response != nil && response.presence != nil {
		if presence, ok := response.presence[usagePresenceKey(model.Model, model.Provider)]; ok {
			return presence
		}
	}
	return usagePresence{
		input: true, output: true, cacheRead: true, cacheWrite: true,
		reasoning: true, turns: true, total: true, cost: true,
	}
}

func usagePayloadDigest(transcriptID string, model sessionModel, presence usagePresence, turns, total *int64) string {
	payload := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%.8f\x00%t%t%t%t%t%t\x00%s\x00%s",
		transcriptID, model.Model, model.Provider, model.Input, model.Output,
		model.CacheRead, model.CacheWrite, model.Reasoning, model.Cost,
		presence.input, presence.output, presence.cacheRead, presence.cacheWrite,
		presence.reasoning, presence.cost, int64String(turns), int64String(total))
	digest := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", digest[:])
}

func int64String(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *value)
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
	filter.SessionIDs = []string{activeSessionID}
	page := pluginsdk.Page{Limit: 200}
	for {
		sessions, info, err := host.Sessions().List(ctx, filter, page)
		if err != nil {
			log.Printf("resolving acp session id: %v", err)
			return ""
		}
		for _, s := range sessions {
			if s.ID == activeSessionID {
				return s.ACPSessionID
			}
		}
		if info == nil || !info.HasMore || info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
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
// session,model` output (tokscale 4.15.x). Only the fields used are declared.
type sessionModelEntry struct {
	SessionID    string  `json:"sessionId"`
	Model        string  `json:"model"`
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cacheRead"`
	CacheWrite   int64   `json:"cacheWrite"`
	Reasoning    int64   `json:"reasoning"`
	MessageCount int64   `json:"messageCount"`
	Provider     string  `json:"provider,omitempty"`
	SourceDate   string  `json:"date,omitempty"`
	Cost         float64 `json:"cost"`

	presenceRecorded    bool
	inputPresent        bool
	outputPresent       bool
	cacheReadPresent    bool
	cacheWritePresent   bool
	reasoningPresent    bool
	messageCountPresent bool
	costPresent         bool
	totalPresent        bool
	totalValue          *int64
}

// UnmarshalJSON records whether tokscale emitted each optional value. The
// default json decoder turns an omitted integer into zero, which would make a
// missing category indistinguishable from a measured zero when the row is
// persisted for native statistics.
func (e *sessionModelEntry) UnmarshalJSON(data []byte) error {
	type plain sessionModelEntry
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*e = sessionModelEntry(decoded)
	e.presenceRecorded = true
	e.inputPresent = hasJSONValue(fields, "input")
	e.outputPresent = hasJSONValue(fields, "output")
	e.cacheReadPresent = hasJSONValue(fields, "cacheRead")
	e.cacheWritePresent = hasJSONValue(fields, "cacheWrite")
	e.reasoningPresent = hasJSONValue(fields, "reasoning")
	e.messageCountPresent = hasJSONValue(fields, "messageCount")
	e.costPresent = hasJSONValue(fields, "cost")
	if raw, ok := firstJSONValue(fields, "total", "totalTokens"); ok {
		var total int64
		if err := json.Unmarshal(raw, &total); err != nil {
			return fmt.Errorf("parsing tokscale total: %w", err)
		}
		e.totalPresent = true
		e.totalValue = &total
	}
	return nil
}

func hasJSONValue(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	return ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func firstJSONValue(fields map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if hasJSONValue(fields, key) {
			return fields[key], true
		}
	}
	return nil, false
}

type usagePresence struct {
	input, output, cacheRead, cacheWrite, reasoning, turns, cost bool
	total                                                        bool
	totalValue                                                   *int64
}

func usagePresenceKey(model, provider string) string {
	return model + "\x00" + provider
}

func presenceForEntry(entry sessionModelEntry) usagePresence {
	if !entry.presenceRecorded {
		return usagePresence{
			input: true, output: true, cacheRead: true, cacheWrite: true,
			reasoning: true, turns: true, total: true, cost: true,
		}
	}
	totalKnown := entry.totalPresent || (entry.inputPresent && entry.outputPresent &&
		entry.cacheReadPresent && entry.cacheWritePresent && entry.reasoningPresent)
	return usagePresence{
		input: entry.inputPresent, output: entry.outputPresent, cacheRead: entry.cacheReadPresent,
		cacheWrite: entry.cacheWritePresent, reasoning: entry.reasoningPresent,
		turns: entry.messageCountPresent, cost: entry.costPresent,
		total: totalKnown, totalValue: entry.totalValue,
	}
}

type sessionModelsReport struct {
	Entries []sessionModelEntry `json:"entries"`
}

// runSessionModels runs tokscale grouped by session and model and returns the
// per-(session,model) entries.
func runSessionModels(ctx context.Context, cmd resolvedCommand, run runner, extraArgs ...string) ([]sessionModelEntry, error) {
	args := append(append([]string{}, cmd.Argv[1:]...), "models", "--json", "--group-by", "session,model")
	args = append(args, extraArgs...)
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
