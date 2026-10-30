package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// plugin implements pluginsdk.Plugin (via UnimplementedPlugin). The legacy
// webhook only directs callers to the authenticated session usage action.
type plugin struct {
	pluginsdk.UnimplementedPlugin

	// Seams injected for tests; production values set in newPlugin.
	run                runner
	lookPath           func(string) (string, error)
	now                func() time.Time
	reportMu           sync.Mutex
	reports            *reportCoordinator
	toolbar            *toolbarReportCoordinator
	hostRead           time.Duration
	canonicalMu        sync.Mutex
	canonicalSerial    uint64
	canonicalReads     map[string]uint64
	usageStateMu       sync.Mutex
	usageStateReady    bool
	usageCleanupMu     sync.Mutex
	usageCleanupCancel context.CancelFunc
	usageCleanupClosed bool
	usageCleanupWait   func(context.Context) bool
	usageMu            sync.Mutex
	adapter            usageAdapter
	importWait         func(context.Context) bool
	collectorMu        sync.Mutex
	collectorStarted   bool
	collectorCancel    context.CancelFunc
	importStateMu      sync.Mutex
	importRevoked      map[string]bool
	importMu           sync.Mutex
	importGeneration   uint64
	importRunning      bool
	importWorkspace    string
	importCancel       context.CancelFunc
}

func newPlugin() *plugin {
	p := &plugin{
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return runCommand(ctx, name, args...)
		},
		lookPath: exec.LookPath,
		now:      time.Now,
		reports:  newReportCoordinator(),
	}
	p.hostRead = 3 * time.Second
	p.toolbar = newToolbarReportCoordinator(p.runReport, func() time.Time { return p.now() })
	return p
}

func (p *plugin) SetHost(host pluginsdk.Host) {
	p.UnimplementedPlugin.SetHost(host)
	ctx, cancel := p.canonicalReadContext(context.Background())
	defer cancel()
	if err := p.ensureUsageStatePrivacy(ctx); err != nil {
		log.Printf("session-cost state cleanup: %v", err)
		if retryUsageStateCleanup(err) {
			p.startUsageStateCleanup()
		}
		return
	}
	p.startCollector()
}

// sessionCostResponse is the authenticated session-usage action payload the
// chat-toolbar UI renders in its hover popover.
type sessionCostResponse struct {
	GeneratedAt string        `json:"generated_at"`
	ReportState string        `json:"report_state,omitempty"`
	ReportError string        `json:"report_error,omitempty"`
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
	// Turns is populated only by canonical Kandev turn evidence. Tokscale
	// message counts do not establish Kandev turns.
	Turns int64 `json:"turns"`
	// CostPerTurn is the average spend per turn (Cost / Turns), 0 when there
	// are no turns yet. Precomputed here so the UI stays a dumb renderer.
	CostPerTurn  float64        `json:"cost_per_turn"`
	CostKnown    bool           `json:"cost_known"`
	TokenAmounts tokenDisplay   `json:"token_amounts,omitempty"`
	Currency     string         `json:"currency,omitempty"`
	CostBasis    string         `json:"cost_basis,omitempty"`
	Models       []sessionModel `json:"models"`
	Saved        bool           `json:"saved"`
	Stale        bool           `json:"stale"`
	Coverage     string         `json:"coverage"`
	LastRefresh  string         `json:"last_refresh,omitempty"`
	Error        string         `json:"error,omitempty"`
	// WarnThreshold / HighThreshold are the operator-configured USD cutoffs the
	// UI uses to colour the amount (green < warn <= amber < high <= red).
	WarnThreshold float64 `json:"warn_threshold"`
	HighThreshold float64 `json:"high_threshold"`

	// presence is kept out of the wire response. It carries the source-field
	// presence bits needed when a cumulative tokscale row is saved. A JSON
	// number of zero is a measured zero; an omitted tokscale field must remain
	// nil in the core measurement.
	presence                    map[string]usagePresence
	sourceEntries               []sessionModelEntry
	Compatibility               string `json:"compatibility,omitempty"`
	CanonicalPending            bool   `json:"canonical_pending,omitempty"`
	ProjectionStatus            string `json:"projection_status,omitempty"`
	ProjectionGeneration        string `json:"projection_generation,omitempty"`
	ProjectionPending           bool   `json:"projection_pending,omitempty"`
	PendingProjectionGeneration string `json:"pending_projection_generation,omitempty"`
}

type sessionModel struct {
	TokenAmounts tokenDisplay `json:"token_amounts,omitempty"`
	CostKnown    *bool        `json:"cost_known,omitempty"`
	Currency     string       `json:"currency,omitempty"`
	CostBasis    string       `json:"cost_basis,omitempty"`
	ExactCost    *string      `json:"cost_subcents_exact,omitempty"`
	Model        string       `json:"model"`
	Input        int64        `json:"input"`
	Output       int64        `json:"output"`
	CacheRead    int64        `json:"cache_read"`
	CacheWrite   int64        `json:"cache_write"`
	Reasoning    int64        `json:"reasoning"`
	Provider     string       `json:"provider,omitempty"`
	Cost         float64      `json:"cost"`
}

func (_ *plugin) HandleWebhook(_ context.Context, req *pluginsdk.WebhookRequest) (*pluginsdk.WebhookResponse, error) {
	if req == nil || req.WebhookKey != webhookKeySessionCost {
		return jsonResponse(404, []byte(`{"error":"unknown webhook"}`)), nil
	}
	return jsonResponse(200, []byte(`{"compatibility":"upgrade_required","error":"Use the authenticated session-usage action to view or refresh usage."}`)), nil
}

// HandleAction serves the authenticated saved/refresh flow used by the
// toolbar. The action context is host-verified, then the current usage binding
// is reauthorized in that workspace before any installation-scoped read.
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
	if err := p.authorizeSessionUsageActionScope(ctx, req.Context.WorkspaceID, req.Context.TaskID, req.Context.SessionID); err != nil {
		if errors.Is(err, errUsageHistoryUnsupported) {
			response := p.unavailableSource(nil, req.Context.SessionID, defaultWarnThreshold, defaultHighThreshold, errUsageHistoryUnsupported)
			encoded, encodeErr := json.Marshal(response)
			if encodeErr != nil {
				return nil, encodeErr
			}
			return &pluginsdk.PluginActionResponse{Status: 200, Headers: jsonHeaders(), Body: encoded}, nil
		}
		switch status.Code(err) {
		case codes.PermissionDenied, codes.FailedPrecondition:
			return &pluginsdk.PluginActionResponse{Status: 403, Headers: jsonHeaders(), Body: []byte(`{"error":"saved usage is no longer accessible"}`)}, nil
		case codes.NotFound:
			return &pluginsdk.PluginActionResponse{Status: 404, Headers: jsonHeaders(), Body: []byte(`{"error":"saved usage is no longer accessible"}`)}, nil
		case codes.Unauthenticated:
			return &pluginsdk.PluginActionResponse{Status: 401, Headers: jsonHeaders(), Body: []byte(`{"error":"saved usage is no longer accessible"}`)}, nil
		}
		return nil, err
	}
	response, err := p.responsiveSessionUsage(ctx, req.Context.WorkspaceID, req.Context.TaskID, req.Context.SessionID, body.Refresh)
	if err != nil {
		if accessStatus := canonicalAccessStatus(err); accessStatus != 0 {
			return &pluginsdk.PluginActionResponse{Status: accessStatus, Headers: jsonHeaders(), Body: []byte(`{"error":"saved usage is no longer accessible"}`)}, nil
		}
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
	return p.calculateSessionCostInScope(ctx, "", taskID, activeSessionID)
}

func (p *plugin) calculateSessionCostInScope(ctx context.Context, workspaceID, taskID, activeSessionID string) (*sessionCostResponse, error) {
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
	}

	// Server-side session -> ACP transcript id mapping via the Host data API.
	resp.ACPSessionID = p.resolveACPSessionID(ctx, taskID, activeSessionID)
	if resp.ACPSessionID != "" {
		if err := p.preflightSource(ctx, workspaceID, taskID, activeSessionID, resp.ACPSessionID); err != nil {
			return p.unavailableSource(nil, activeSessionID, warn, high, err), nil
		}
	} else {
		return &resp, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()

	entries, err := p.runReport(runCtx, cmd)
	if err != nil {
		// Degrade to a status-only payload so the UI can render setup guidance
		// from the same shape it always reads.
		log.Printf("tokscale run failed (degrading): %v", err)
		resp.Tokscale = InstallStatus{Command: commandDisplay(cmd), Source: cmd.Source, Error: err.Error()}
		resp.Error = err.Error()
		return &resp, nil
	}
	resp.Tokscale = InstallStatus{Command: commandDisplay(cmd), Source: cmd.Source, Installed: true}
	if err := p.preflightSource(ctx, workspaceID, taskID, activeSessionID, resp.ACPSessionID); err != nil {
		return p.unavailableSource(nil, activeSessionID, warn, high, err), nil
	}

	if resp.ACPSessionID == "" {
		return &resp, nil // agent hasn't reported a transcript id yet
	}
	for _, e := range entries {
		if !sessionMatches(e.SessionID, resp.ACPSessionID) {
			continue
		}
		resp.Found = true
		resp.sourceEntries = append(resp.sourceEntries, e)
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
	sourceDisplay(&resp)
	if err := p.checkTokscaleSnapshot(ctx, &resp); err != nil {
		return p.unavailableSource(nil, activeSessionID, warn, high, err), nil
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
	saved, hasSaved, readErr := p.loadSavedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high)
	if readErr != nil {
		return nil, readErr
	}
	if hasSaved && savedUsageReadOnly(saved) {
		return saved, nil
	}
	if hasSaved && saved.Found && !p.collectionEnabled(ctx) {
		return saved, nil
	}
	if hasSaved && (saved.Found || saved.CanonicalPending) && !refresh {
		return saved, nil
	}
	response, err := p.calculateSessionCostInScope(ctx, workspaceID, taskID, sessionID)
	if err != nil {
		return nil, err
	}
	if response.Compatibility == "source_context_unavailable" {
		return p.unavailableSource(saved, sessionID, warn, high), nil
	}
	if response.Compatibility == "source_context_preparing" {
		return p.unavailableSource(saved, sessionID, warn, high, errUsageTargetsPreparing), nil
	}
	if response.Compatibility == "upgrade_required" {
		return p.unavailableSource(saved, sessionID, warn, high, errUsageHistoryUnsupported), nil
	}
	if response.Compatibility == "source_membership_ambiguous" {
		return p.unavailableSource(saved, sessionID, warn, high, errTokscaleMembership), nil
	}
	if response.Found && p.collectionEnabled(ctx) {
		writeErr := p.persistSessionUsage(ctx, workspaceID, taskID, sessionID, response)
		return p.canonicalUsageAfterWrite(ctx, workspaceID, taskID, sessionID, response, saved, writeErr), nil
	}
	if hasSaved && !response.Found {
		saved.Stale = true
		saved.Error = response.Error
		return saved, nil
	}
	return response, nil
}

// Admission receipts describe source rows. Only a canonical read may supply
// displayed saved amounts, including when the projection is still pending.
func (p *plugin) canonicalUsageAfterWrite(ctx context.Context, workspaceID, taskID, sessionID string, calculated, _ *sessionCostResponse, writeErr error) *sessionCostResponse {
	canonical, found, readErr := p.loadSavedSessionUsage(ctx, workspaceID, taskID, sessionID, calculated.WarnThreshold, calculated.HighThreshold)
	if readErr != nil {
		unavailable := p.canonicalReadFailure(sessionID, calculated.WarnThreshold, calculated.HighThreshold)
		if canonicalAccessStatus(readErr) != 0 {
			unavailable.ReportError = reportErrorAccessDenied
		}
		unavailable.Error = "saved usage is no longer accessible"
		return unavailable
	}
	if canonical == nil {
		empty := responseForEntries(sessionID, calculated.ACPSessionID, nil, calculated.WarnThreshold, calculated.HighThreshold, p.now())
		empty.CostKnown = false
		canonical = &empty
	}
	if canonical.Compatibility == "upgrade_required" || canonical.ReportError == reportErrorInvalidQuery {
		return canonical
	}
	canonical.Stale = canonical.Stale || writeErr != nil || !found
	if writeErr != nil {
		canonical.ReportState = reportStateFailed
		canonical.ReportError = reportErrorFailed
		canonical.Error = "usage was calculated but could not be accepted"
		if errors.Is(writeErr, errTokscaleMembership) {
			canonical.Compatibility = "source_membership_ambiguous"
			canonical.ReportError = "source_membership"
			canonical.Error = errTokscaleMembership.Error()
		}
		if errors.Is(writeErr, errUsageHistoryUnsupported) {
			canonical.Compatibility = "upgrade_required"
			canonical.ReportError = reportErrorUnavailable
			canonical.Error = errUsageHistoryUnsupported.Error()
		}
	} else if !found {
		canonical.CanonicalPending = true
		canonical.ProjectionPending = true
		canonical.ReportState = reportStatePending
	}
	return canonical
}

func responseFromSaved(items []pluginsdk.SessionUsageMeasurement, sessionID string, warn, high float64) sessionCostResponse {
	response := sessionCostResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339), KandevSessionID: sessionID,
		Models: []sessionModel{}, WarnThreshold: warn, HighThreshold: high, Saved: true,
		Coverage: "complete", Tokscale: InstallStatus{Installed: true},
	}
	type modelKey struct{ model, provider string }
	var modelOrder []modelKey
	modelRows := make(map[modelKey][]pluginsdk.SessionUsageMeasurement)
	for _, item := range items {
		if response.ACPSessionID == "" {
			response.ACPSessionID = item.TranscriptID
		}
		response.Found = true
		if item.Coverage != "complete" {
			response.Coverage = "partial"
		}
		response.Stale = response.Stale || item.Stale
		key := modelKey{item.Model, item.Provider}
		if _, exists := modelRows[key]; !exists {
			modelOrder = append(modelOrder, key)
		}
		modelRows[key] = append(modelRows[key], item)
		if item.CollectedAt != "" && item.CollectedAt > response.LastRefresh {
			response.LastRefresh = item.CollectedAt
		}
	}
	amounts := savedMetricAmounts(items)
	response.applyTokenDisplay(amounts.tokens)
	response.Cost, response.CostKnown = amounts.cost, amounts.costKnown
	response.Currency, response.CostBasis = amounts.currency, amounts.basis
	for _, key := range modelOrder {
		amounts := savedMetricAmounts(modelRows[key])
		known := amounts.costKnown
		response.Models = append(response.Models, sessionModel{
			TokenAmounts: amounts.tokens, CostKnown: &known, Currency: amounts.currency, CostBasis: amounts.basis, ExactCost: amounts.exactCost,
			Model: key.model, Provider: key.provider, Cost: amounts.cost,
			Input: displayInt64(amounts.tokens["input"]), Output: displayInt64(amounts.tokens["output"]),
			CacheRead: displayInt64(amounts.tokens["cache_read"]), CacheWrite: displayInt64(amounts.tokens["cache_write"]),
			Reasoning: displayInt64(amounts.tokens["reasoning"]),
		})
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

type usageCoverage struct {
	Date     string
	Timezone string
}

func (p *plugin) persistSessionUsage(ctx context.Context, workspaceID, taskID, sessionID string, response *sessionCostResponse) error {
	return p.persistSessionUsageWithCoverage(ctx, workspaceID, taskID, sessionID, response, usageCoverage{})
}

func (p *plugin) persistSessionUsageWithCoverage(ctx context.Context, workspaceID, taskID, sessionID string, response *sessionCostResponse, coverage usageCoverage) error {
	if coverage.Date != "" && coverage.Timezone == "" {
		return errors.New("cannot resolve source timezone; set TZ to an IANA timezone before dated collection")
	}
	if !p.collectionEnabled(ctx) {
		return nil
	}
	if p.Host() == nil || workspaceID == "" || taskID == "" || sessionID == "" || response.ACPSessionID == "" {
		return errors.New("usage collection requires workspace, task, session and transcript context")
	}
	target, err := p.resolveUsageTarget(ctx, workspaceID, taskID, sessionID, response.ACPSessionID)
	if err != nil {
		return err
	}
	return p.persistTargetUsage(ctx, target, response, coverage)
}

type usageWriteRejectedError struct {
	status  string
	results []pluginsdk.UsageObservationWriteResultV1
}

func (e *usageWriteRejectedError) Error() string {
	return fmt.Sprintf("usage write %s was rejected", e.status)
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
		reasoning: true, total: true, cost: true,
	}
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
	SessionID          string `json:"sessionId"`
	Model              string `json:"model"`
	Input              int64  `json:"input"`
	Output             int64  `json:"output"`
	CacheRead          int64  `json:"cacheRead"`
	CacheWrite         int64  `json:"cacheWrite"`
	Reasoning          int64  `json:"reasoning"`
	MessageCount       int64  `json:"messageCount"`
	Provider           string `json:"provider,omitempty"`
	Client             string `json:"client,omitempty"`
	SourceDate         string `json:"date,omitempty"`
	Source             string `json:"source,omitempty"`
	ModelProvider      string `json:"modelProvider,omitempty"`
	AgentClient        string `json:"agentClient,omitempty"`
	ServingPlatform    string `json:"servingPlatform,omitempty"`
	SourceRecordID     string `json:"sourceRecordId,omitempty"`
	SupersedesRecordID string `json:"supersedesRecordId,omitempty"`
	costValue          *int64
	Cost               float64 `json:"cost"`

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
	if e.costPresent {
		var err error
		e.costValue, err = parseUSDAmount(string(fields["cost"]))
		if err != nil {
			return fmt.Errorf("parsing tokscale cost: %w", err)
		}
	}
	for _, v := range []int64{e.Input, e.Output, e.CacheRead, e.CacheWrite, e.Reasoning, e.MessageCount} {
		if v < 0 {
			return errors.New("negative tokscale token or message count")
		}
	}
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
	input, output, cacheRead, cacheWrite, reasoning, cost bool
	total                                                 bool
	totalValue                                            *int64
}

func usagePresenceKey(model, provider string) string {
	return model + "\x00" + provider
}

func presenceForEntry(entry sessionModelEntry) usagePresence {
	if !entry.presenceRecorded {
		return usagePresence{
			input: true, output: true, cacheRead: true, cacheWrite: true,
			reasoning: true, total: true, cost: true,
		}
	}
	totalKnown := entry.totalPresent
	return usagePresence{
		input: entry.inputPresent, output: entry.outputPresent, cacheRead: entry.cacheReadPresent,
		cacheWrite: entry.cacheWritePresent, reasoning: entry.reasoningPresent,
		cost:  entry.costPresent,
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
	if report.Entries == nil {
		return nil, fmt.Errorf("parsing tokscale output: missing entries array")
	}
	for _, entry := range report.Entries {
		if entry.SessionID == "" || entry.Model == "" {
			return nil, fmt.Errorf("parsing tokscale entry: missing identity")
		}
	}
	markPinnedTokscaleTotals(report.Entries, cmd)
	return report.Entries, nil
}
