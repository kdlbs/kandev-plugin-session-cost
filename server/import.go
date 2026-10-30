package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
)

const (
	importStateScope = "workspace"
	importStateKey   = "historical-import"
	importPageSize   = int32(50)

	importStatusNotStarted  = "not_started"
	importStatusRunning     = "running"
	importStatusPreparing   = "preparing"
	importStatusCompleted   = "completed"
	importStatusFailed      = "failed"
	importStatusCancelled   = "cancelled"
	importStatusDisabled    = "disabled"
	importStatusUnsupported = "unsupported"
	maxMissingSessionIDs    = 100
)

// historicalImportState retains one bounded target page. Snapshot identity,
// keyset position and pending source dates survive restart and live deletion.
type historicalImportState struct {
	RunID               string                              `json:"run_id,omitempty"`
	SchemaVersion       int                                 `json:"schema_version"`
	DiagnosticsReliable bool                                `json:"diagnostics_reliable"`
	LegacyDiagnostics   bool                                `json:"legacy_diagnostics,omitempty"`
	SnapshotID          string                              `json:"snapshot_id,omitempty"`
	SnapshotUpperBound  int64                               `json:"snapshot_upper_bound,string,omitempty"`
	PageTargets         []pluginsdk.UsageCollectionTargetV1 `json:"page_targets,omitempty"`
	PageNextCursor      string                              `json:"page_next_cursor,omitempty"`
	PageHasMore         bool                                `json:"page_has_more,omitempty"`
	PageDates           map[string][]string                 `json:"page_dates,omitempty"`
	PageFound           map[string]bool                     `json:"page_found,omitempty"`
	PageMissingIDs      []string                            `json:"page_missing_ids,omitempty"`
	WorkspaceID         string                              `json:"workspace_id"`
	Status              string                              `json:"status"`
	Cursor              string                              `json:"cursor,omitempty"`
	Processed           int                                 `json:"processed"`
	Missing             int                                 `json:"missing"`
	MissingSessionIDs   []string                            `json:"missing_session_ids,omitempty"`
	LastError           string                              `json:"last_error,omitempty"`
	StartedAt           string                              `json:"started_at,omitempty"`
	FinishedAt          string                              `json:"finished_at,omitempty"`
	LastSuccessfulAt    string                              `json:"last_successful_at,omitempty"`
	Undated             bool                                `json:"undated"`
	Dates               []string                            `json:"pending_dates,omitempty"`
}

func (p *plugin) handleHistoricalImportAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()
	if req.Context.WorkspaceID == "" || req.Context.TaskID != "" || req.Context.SessionID != "" || req.Context.RepositoryID != "" {
		return importJSONResponse(400, map[string]string{"error": "workspace context is required"})
	}
	switch req.ActionKey {
	case actionKeyImportStatus:
		state, found, err := p.authorizedHistoricalImportState(ctx, req.Context.WorkspaceID)
		if err != nil {
			return nil, err
		}
		if !found {
			state = historicalImportState{WorkspaceID: req.Context.WorkspaceID, Status: importStatusNotStarted, Undated: true}
		}
		return importJSONResponse(200, state)
	case actionKeyImportCancel:
		return p.cancelHistoricalImport(ctx, req.Context.WorkspaceID)
	case actionKeyImportStart:
		return p.startHistoricalImport(ctx, req.Context.WorkspaceID)
	default:
		return importJSONResponse(404, map[string]string{"error": "unknown action"})
	}
}

func (p *plugin) startHistoricalImport(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	if !p.collectionEnabled(ctx) {
		state := historicalImportState{WorkspaceID: workspaceID, Status: importStatusDisabled, Undated: true, LastError: "enable token statistics collection before importing history"}
		return importJSONResponse(409, state)
	}
	p.importMu.Lock()
	defer p.importMu.Unlock()
	state, found, err := p.authorizedHistoricalImportState(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if found && p.importRunning && p.importWorkspace == workspaceID {
		return importJSONResponse(202, state)
	}
	if !found || state.Status == importStatusCompleted || state.SchemaVersion != 2 {
		state = historicalImportState{WorkspaceID: workspaceID, SchemaVersion: 2, DiagnosticsReliable: true}
	}
	if _, err := usageHistory(p.Host()); err != nil {
		state.Status = importStatusUnsupported
		state.LastError = err.Error()
		return importJSONResponse(409, state)
	}
	if state.SnapshotID == "" {
		reader, _ := usageHistory(p.Host())
		query := pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspaceID, Limit: 1}
		page, err := p.readUsageTargetPage(ctx, reader, query, 0)
		if err != nil && !errors.Is(err, errUsageTargetsPreparing) {
			return nil, historyError(err)
		}
		state.SnapshotID, state.SnapshotUpperBound = page.SnapshotID, page.SnapshotUpperBound
	}
	state.WorkspaceID = workspaceID
	state.RunID = rand.Text()
	state.Status = importStatusRunning
	if state.SnapshotID == "" {
		state.Status = importStatusPreparing
	}
	state.LastError = ""
	state.FinishedAt = ""
	if state.StartedAt == "" {
		state.StartedAt = p.now().UTC().Format(time.RFC3339Nano)
	}
	if err := p.writeHistoricalImportState(ctx, workspaceID, state, true); err != nil {
		return nil, err
	}
	if p.importCancel != nil {
		p.importCancel()
	}
	importCtx, cancel := context.WithCancel(context.Background())
	p.importRunning = true
	p.importWorkspace = workspaceID
	p.importCancel = cancel
	p.importGeneration++
	generation := p.importGeneration
	go p.runHistoricalImportGeneration(importCtx, workspaceID, generation)
	return importJSONResponse(202, state)
}

func (p *plugin) cancelHistoricalImport(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	state, found, err := p.authorizedHistoricalImportState(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return importJSONResponse(200, historicalImportState{WorkspaceID: workspaceID, Status: importStatusNotStarted, Undated: true})
	}
	if state.Status == importStatusRunning || state.Status == importStatusPreparing {
		state.Status = importStatusCancelled
		state.FinishedAt = p.now().UTC().Format(time.RFC3339Nano)
		state.LastError = ""
		if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
			return nil, err
		}
	}
	p.importMu.Lock()
	if p.importWorkspace == workspaceID && p.importCancel != nil {
		p.importCancel()
	}
	p.importMu.Unlock()
	return importJSONResponse(200, state)
}

func (p *plugin) runHistoricalImport(ctx context.Context, workspaceID string) {
	p.runHistoricalImportGeneration(ctx, workspaceID, 0)
}

func (p *plugin) runHistoricalImportGeneration(ctx context.Context, workspaceID string, generation uint64) {
	defer func() {
		p.importMu.Lock()
		if p.importWorkspace == workspaceID && (generation == 0 || p.importGeneration == generation) {
			p.importRunning = false
			p.importWorkspace = ""
			p.importCancel = nil
		}
		p.importMu.Unlock()
	}()

	initial, _, _ := p.loadHistoricalImportState(ctx, workspaceID)
	runID := initial.RunID
	finish := func(status string, err error) { p.finishHistoricalImportRun(workspaceID, status, err, runID) }

	host := p.Host()
	if host == nil {
		finish(importStatusFailed, errors.New("host unavailable"))
		return
	}
	reader, err := usageHistory(host)
	if err != nil {
		finish(importStatusUnsupported, err)
		return
	}
	for {
		if ctx.Err() != nil {
			finish(importStatusCancelled, nil)
			return
		}
		if !p.collectionEnabled(ctx) {
			finish(importStatusDisabled, errors.New("token statistics collection is disabled"))
			return
		}
		state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
		if err != nil {
			log.Printf("historical import checkpoint: %v", err)
			return
		}
		if !found || state.SchemaVersion != 2 {
			state = historicalImportState{WorkspaceID: workspaceID, SchemaVersion: 2, Status: importStatusRunning, DiagnosticsReliable: true}
		}
		if state.RunID != runID {
			return
		}
		if state.Status == importStatusCancelled {
			return
		}
		if state.SnapshotID != "" {
			if scopeErr := p.validateUsageSnapshot(ctx, workspaceID, state.SnapshotID, state.SnapshotUpperBound); scopeErr != nil {
				if usageScopeDenied(scopeErr) {
					_, _, _ = p.authorizedHistoricalImportState(ctx, workspaceID)
					return
				}
				finish(importFailureStatus(scopeErr), scopeErr)
				return
			}
		}
		if state.PageFound == nil {
			state.PageFound = make(map[string]bool)
		}
		if len(state.PageTargets) == 0 {
			query := pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspaceID, Limit: importPageSize, SnapshotID: state.SnapshotID, Cursor: state.Cursor}
			page, err := p.readUsageTargetPage(ctx, reader, query, state.SnapshotUpperBound)
			if errors.Is(err, errUsageTargetsPreparing) {
				state.Status = importStatusPreparing
				if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
					return
				}
				if !p.waitTargetPreparation(ctx) {
					finish(importStatusCancelled, nil)
					return
				}
				continue
			}
			if err != nil {
				finish(importFailureStatus(err), historyError(err))
				return
			}
			state.Status = importStatusRunning
			state.SnapshotID = page.SnapshotID
			state.SnapshotUpperBound = page.SnapshotUpperBound
			state.PageTargets = page.Targets
			state.PageNextCursor = page.NextCursor
			state.PageHasMore = page.HasMore
			state.PageDates = make(map[string][]string)
			state.PageFound = make(map[string]bool)
			state.PageMissingIDs = nil
			var ids []string
			for _, target := range page.Targets {
				if !eligibleUsageSource(target) {
					finish(importStatusUnsupported, errUsageSourceUnavailable)
					return
				}
				if target.SessionID != nil && target.ExternalSessionID != nil && len(trustedTargetDateRange(target)) == 0 {
					ids = append(ids, *target.SessionID)
				}
			}
			live := make(map[string]pluginsdk.Session)
			if len(ids) > 0 {
				sessions, _, readErr := host.Sessions().List(ctx, pluginsdk.SessionFilter{WorkspaceIDs: []string{workspaceID}, SessionIDs: ids}, pluginsdk.Page{Limit: importPageSize})
				if readErr == nil {
					for _, session := range sessions {
						live[session.ID] = session
					}
				}
			}
			for _, target := range page.Targets {
				if err := p.rememberUsageTarget(ctx, workspaceID, target, page); err != nil {
					finish(importStatusFailed, err)
					return
				}
				if target.ExternalSessionID == nil || *target.ExternalSessionID == "" {
					continue
				}
				dates := trustedTargetDateRange(target)
				if len(dates) == 0 && target.SessionID != nil {
					if session, ok := live[*target.SessionID]; ok {
						dates = historicalSessionDates(session, p.now())
					}
				}
				if len(dates) == 0 {
					dates = []string{""}
				}
				if len(dates) > 1 {
					state.PageDates[target.SubjectBinding] = []string{dates[0], dates[len(dates)-1]}
				} else {
					state.PageDates[target.SubjectBinding] = dates
				}
			}
			state.Dates = nil
			if date, ok := nextHistoricalImportDate(state.PageDates, nil); ok {
				state.Dates = []string{date}
			}
			// Persist the exact page and snapshot before starting a report. Deletion
			// cannot change the target membership or dates of a resumed page.
			if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
				return
			}
		}
		if len(state.Dates) > 0 {
			date := state.Dates[0]
			for i, target := range state.PageTargets {
				if target.SessionID != nil && target.ExternalSessionID != nil && targetIncludesDate(state.PageDates[target.SubjectBinding], date) {
					current, err := p.currentSourceTarget(ctx, workspaceID, target.TaskID, *target.SessionID, *target.ExternalSessionID, target.SubjectBinding)
					if err != nil {
						finish(importFailureStatus(err), err)
						return
					}
					// Only the current Host response supplies source authority.
					// A checkpoint from before this field existed can resume after
					// that response explicitly authorizes the same retained binding.
					state.PageTargets[i] = current
				}
			}
			warn, high := p.configuredThresholds(ctx)
			cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)
			runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
			var entries []sessionModelEntry
			if date == "" {
				entries, err = p.runReport(runCtx, cmd)
			} else {
				entries, err = p.runDatedReport(runCtx, cmd, date, date)
			}
			cancel()
			if ctx.Err() != nil {
				finish(importStatusCancelled, nil)
				return
			}
			if !p.collectionEnabled(ctx) {
				finish(importStatusDisabled, errUsageCollectionDisabled)
				return
			}
			if err != nil {
				finish(importStatusFailed, err)
				return
			}
			if scopeErr := p.validateUsageSnapshot(ctx, workspaceID, state.SnapshotID, state.SnapshotUpperBound); scopeErr != nil {
				return
			}
			for _, target := range state.PageTargets {
				if !targetIncludesDate(state.PageDates[target.SubjectBinding], date) {
					continue
				}
				if !eligibleUsageSource(target) || target.ExternalSessionID == nil || target.SessionID == nil {
					finish(importStatusUnsupported, errUsageSourceUnavailable)
					return
				}
				if err := p.preflightSource(ctx, workspaceID, target.TaskID, *target.SessionID, *target.ExternalSessionID, target.SubjectBinding); err != nil {
					finish(importFailureStatus(err), err)
					return
				}
				sessionID := ""
				if target.SessionID != nil {
					sessionID = *target.SessionID
				}
				response := responseForEntries(sessionID, *target.ExternalSessionID, entries, warn, high, p.now())
				if !response.Found {
					coverage := usageCoverage{}
					if date != "" {
						coverage = usageCoverage{Date: date, Timezone: sourceTimezone()}
					}
					if err := p.checkTokscaleSnapshotCoverage(ctx, &response, coverage); err != nil {
						finish(importFailureStatus(err), err)
						return
					}
					continue
				}
				coverage := usageCoverage{}
				if date != "" {
					coverage = usageCoverage{Date: date, Timezone: sourceTimezone()}
				}
				if err := p.persistTargetUsage(ctx, target, &response, coverage); err != nil {
					finish(importFailureStatus(err), err)
					return
				}
				state.PageFound[target.SubjectBinding] = true
				if date == "" {
					state.Undated = true
				}
			}
			state.Dates = state.Dates[1:]
			if len(state.Dates) == 0 {
				if next, ok := nextHistoricalImportDate(state.PageDates, &date); ok {
					state.Dates = []string{next}
				}
			}
			state.LastSuccessfulAt = p.now().UTC().Format(time.RFC3339Nano)
			if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
				return
			}
			if len(state.Dates) > 0 {
				if !p.waitImportBudget(ctx) {
					finish(importStatusCancelled, nil)
					return
				}
				continue
			}
		}
		for _, target := range state.PageTargets {
			if !state.PageFound[target.SubjectBinding] {
				sample := target.SubjectBinding
				if target.SessionID != nil {
					sample = *target.SessionID
				}
				markMissingTarget(&state, target.SubjectBinding, sample)
			}
		}
		state.Processed += len(state.PageTargets)
		state.Cursor = state.PageNextCursor
		state.PageTargets = nil
		state.PageDates = nil
		state.PageFound = nil
		state.PageMissingIDs = nil
		state.LastSuccessfulAt = p.now().UTC().Format(time.RFC3339Nano)
		state.LastError = ""
		if !state.PageHasMore {
			state.Status = importStatusCompleted
			state.FinishedAt = state.LastSuccessfulAt
		} else {
			state.Status = importStatusRunning
		}
		if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
			return
		}
		if state.Status == importStatusCompleted {
			return
		}
		if !p.waitImportBudget(ctx) {
			finish(importStatusCancelled, nil)
			return
		}
	}
}

func importFailureStatus(err error) string {
	if errors.Is(err, errUsageCollectionDisabled) {
		return importStatusDisabled
	}
	if errors.Is(historyError(err), errUsageHistoryUnsupported) {
		return importStatusUnsupported
	}
	if errors.Is(err, errUsageSourceUnavailable) {
		return importStatusUnsupported
	}
	return importStatusFailed
}

func (p *plugin) waitTargetPreparation(ctx context.Context) bool {
	if p.importWait != nil {
		return p.importWait(ctx)
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func markMissingSession(state *historicalImportState, sessionID string) {
	markMissingTarget(state, sessionID, sessionID)
}

func markMissingTarget(state *historicalImportState, sessionID, sample string) {
	if sessionID == "" {
		return
	}
	for _, existing := range state.PageMissingIDs {
		if existing == sessionID {
			return
		}
	}
	state.PageMissingIDs = append(state.PageMissingIDs, sessionID)
	state.Missing++
	if len(state.MissingSessionIDs) < maxMissingSessionIDs {
		state.MissingSessionIDs = append(state.MissingSessionIDs, sample)
	}
}

func historicalImportDates(sessions []pluginsdk.Session, now time.Time) []string {
	seen := make(map[string]struct{})
	for _, session := range sessions {
		for _, date := range historicalSessionDates(session, now) {
			seen[date] = struct{}{}
		}
	}
	dates := make([]string, 0, len(seen))
	for date := range seen {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	return dates
}

func historicalSessionDates(session pluginsdk.Session, now time.Time) []string {
	start := parseSessionTime(session.StartedAt)
	if start.IsZero() {
		start = parseSessionTime(session.UpdatedAt)
	}
	if start.IsZero() {
		return nil
	}
	end := start
	if session.EndedAt != nil {
		if parsed := parseSessionTime(*session.EndedAt); !parsed.IsZero() {
			end = parsed
		}
	} else if parsed := parseSessionTime(session.UpdatedAt); !parsed.IsZero() && parsed.After(end) {
		end = parsed
	} else if isActiveState(session.State) && now.After(end) {
		end = now
	}
	location := sourceLocation()
	startLocal := start.In(location)
	day := time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, location)
	endLocal := end.In(location)
	last := time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), 0, 0, 0, 0, location)
	dates := make([]string, 0)
	for !day.After(last) {
		dates = append(dates, day.Format("2006-01-02"))
		day = day.AddDate(0, 0, 1)
	}
	return dates
}

func (p *plugin) finishHistoricalImport(workspaceID, status string, importErr error) {
	p.finishHistoricalImportRun(workspaceID, status, importErr, "")
}
func (p *plugin) finishHistoricalImportRun(workspaceID, status string, importErr error, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
	if err != nil || !found || (runID != "" && state.RunID != runID) {
		return
	}
	if state.Status == importStatusCancelled {
		return
	}
	state.Status = status
	state.FinishedAt = p.now().UTC().Format(time.RFC3339Nano)
	if importErr != nil {
		state.LastError = importErr.Error()
	} else {
		state.LastError = ""
	}
	if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
		log.Printf("session-cost historical import result: %v", err)
	}
}

func (p *plugin) loadHistoricalImportState(ctx context.Context, workspaceID string) (historicalImportState, bool, error) {
	host := p.Host()
	if host == nil {
		return historicalImportState{}, false, errors.New("host unavailable")
	}
	value, found, err := host.GetState(ctx, importStateScope, workspaceID, importStateKey)
	if err != nil || !found {
		return historicalImportState{}, found, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return historicalImportState{}, false, err
	}
	var state historicalImportState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return historicalImportState{}, false, fmt.Errorf("decode historical import state: %w", err)
	}
	state.WorkspaceID = workspaceID
	if state.SchemaVersion != 2 {
		state.DiagnosticsReliable = false
		state.LegacyDiagnostics = true
	}
	if state.Status == "" {
		state.Status = importStatusNotStarted
	}
	return state, true, nil
}

func (p *plugin) saveHistoricalImportState(ctx context.Context, workspaceID string, state historicalImportState) error {
	return p.writeHistoricalImportState(ctx, workspaceID, state, false)
}
func (p *plugin) writeHistoricalImportState(ctx context.Context, workspaceID string, state historicalImportState, starting bool) error {
	p.importStateMu.Lock()
	defer p.importStateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.importRevoked[stableUsageID(workspaceID, state.RunID)] {
		return context.Canceled
	}
	if !starting {
		old, found, err := p.loadHistoricalImportState(ctx, workspaceID)
		if err != nil {
			return err
		}
		if found && ((old.RunID != "" && old.RunID != state.RunID) || (old.Status == importStatusCancelled && state.Status != importStatusCancelled)) {
			return context.Canceled
		}
	}
	host := p.Host()
	if host == nil {
		return errors.New("host unavailable")
	}
	state.WorkspaceID = workspaceID
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return err
	}
	return host.SetState(ctx, importStateScope, workspaceID, importStateKey, value)
}

func importJSONResponse(status int, body any) (*pluginsdk.PluginActionResponse, error) {
	if state, ok := body.(historicalImportState); ok {
		body = publicImportView(state)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &pluginsdk.PluginActionResponse{Status: status, Headers: jsonHeaders(), Body: encoded}, nil
}

func (p *plugin) waitImportBudget(ctx context.Context) bool {
	if p.importWait != nil {
		return p.importWait(ctx)
	}
	timer := time.NewTimer(p.collectionInterval(ctx))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func targetIncludesDate(dates []string, date string) bool {
	if len(dates) == 0 {
		return false
	}
	if len(dates) == 1 {
		return dates[0] == date
	}
	return date >= dates[0] && date <= dates[len(dates)-1]
}
