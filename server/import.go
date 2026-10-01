package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	importStateScope = "workspace"
	importStateKey   = "historical-import"
	importPageSize   = int32(50)

	importStatusNotStarted = "not_started"
	importStatusRunning    = "running"
	importStatusCompleted  = "completed"
	importStatusFailed     = "failed"
	importStatusCancelled  = "cancelled"
	importStatusDisabled   = "disabled"
	maxMissingSessionIDs   = 100
)

// historicalImportState is deliberately small. The session cursor is the
// resumable boundary; accepted measurements are idempotent at the Host usage
// service, so a process restart can safely replay the current page.
type historicalImportState struct {
	WorkspaceID       string   `json:"workspace_id"`
	Status            string   `json:"status"`
	Cursor            string   `json:"cursor,omitempty"`
	Processed         int      `json:"processed"`
	Missing           int      `json:"missing"`
	MissingSessionIDs []string `json:"missing_session_ids,omitempty"`
	LastError         string   `json:"last_error,omitempty"`
	StartedAt         string   `json:"started_at,omitempty"`
	FinishedAt        string   `json:"finished_at,omitempty"`
	LastSuccessfulAt  string   `json:"last_successful_at,omitempty"`
	Undated           bool     `json:"undated"`
	Dates             []string `json:"pending_dates,omitempty"`
}

func (p *plugin) handleHistoricalImportAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if req.Context.WorkspaceID == "" || req.Context.TaskID != "" || req.Context.SessionID != "" || req.Context.RepositoryID != "" {
		return importJSONResponse(400, map[string]string{"error": "workspace context is required"})
	}
	switch req.ActionKey {
	case actionKeyImportStatus:
		state, found, err := p.loadHistoricalImportState(ctx, req.Context.WorkspaceID)
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
	state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !found || state.Status == importStatusCompleted {
		state = historicalImportState{WorkspaceID: workspaceID}
	}
	state.WorkspaceID = workspaceID
	state.Status = importStatusRunning
	state.LastError = ""
	state.FinishedAt = ""
	if state.StartedAt == "" {
		state.StartedAt = p.now().UTC().Format(time.RFC3339Nano)
	}
	if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
		return nil, err
	}

	p.importMu.Lock()
	if p.importRunning && p.importWorkspace == workspaceID {
		p.importMu.Unlock()
		return importJSONResponse(202, state)
	}
	if p.importCancel != nil {
		p.importCancel()
	}
	importCtx, cancel := context.WithCancel(context.Background())
	p.importRunning = true
	p.importWorkspace = workspaceID
	p.importCancel = cancel
	p.importMu.Unlock()
	go p.runHistoricalImport(importCtx, workspaceID)
	return importJSONResponse(202, state)
}

func (p *plugin) cancelHistoricalImport(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return importJSONResponse(200, historicalImportState{WorkspaceID: workspaceID, Status: importStatusNotStarted, Undated: true})
	}
	if state.Status == importStatusRunning {
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
	defer func() {
		p.importMu.Lock()
		if p.importWorkspace == workspaceID {
			p.importRunning = false
			p.importWorkspace = ""
			p.importCancel = nil
		}
		p.importMu.Unlock()
	}()

	host := p.Host()
	if host == nil {
		p.finishHistoricalImport(workspaceID, importStatusFailed, errors.New("host unavailable"))
		return
	}
	for {
		if ctx.Err() != nil {
			p.finishHistoricalImport(workspaceID, importStatusCancelled, nil)
			return
		}
		if !p.collectionEnabled(ctx) {
			p.finishHistoricalImport(workspaceID, importStatusDisabled, errors.New("token statistics collection is disabled"))
			return
		}
		state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
		if err != nil {
			log.Printf("session-cost historical import state: %v", err)
			return
		}
		if !found {
			state = historicalImportState{WorkspaceID: workspaceID, Status: importStatusRunning}
		}
		if state.Status == importStatusCancelled {
			return
		}
		page := pluginsdk.Page{Limit: importPageSize, Cursor: state.Cursor}
		sessions, info, err := host.Sessions().List(ctx, pluginsdk.SessionFilter{WorkspaceIDs: []string{workspaceID}}, page)
		if err != nil {
			p.finishHistoricalImport(workspaceID, importStatusFailed, err)
			return
		}
		if len(sessions) == 0 {
			state.Status = importStatusCompleted
			state.FinishedAt = p.now().UTC().Format(time.RFC3339Nano)
			state.LastError = ""
			_ = p.saveHistoricalImportState(ctx, workspaceID, state)
			return
		}

		eligible := make([]pluginsdk.Session, 0, len(sessions))
		for _, session := range sessions {
			if session.ACPSessionID != "" {
				eligible = append(eligible, session)
			} else {
				markMissingSession(&state, session.ID)
			}
		}
		if len(eligible) > 0 {
			if len(state.Dates) == 0 {
				state.Dates = historicalImportDates(eligible, p.now())
				if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
					log.Printf("session-cost historical import checkpoint: %v", err)
					return
				}
			}
			if len(state.Dates) > 0 {
				date := state.Dates[0]
				warn, high := p.configuredThresholds(ctx)
				cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)
				runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
				entries, reportErr := p.runDatedReport(runCtx, cmd, date, date)
				cancel()
				if reportErr != nil {
					p.finishHistoricalImport(workspaceID, importStatusFailed, reportErr)
					return
				}
				missingSessions := make(map[string]bool)
				for _, session := range eligible {
					if !containsDate(historicalSessionDates(session, p.now()), date) {
						continue
					}
					response := responseForEntries(session.ID, session.ACPSessionID, entries, warn, high, p.now())
					if !response.Found {
						missingSessions[session.ID] = true
						continue
					}
					if err := p.persistSessionUsageWithCoverage(ctx, workspaceID, session.TaskID, session.ID, &response, usageCoverage{Date: date, Timezone: sourceTimezone()}); err != nil {
						p.finishHistoricalImport(workspaceID, importStatusFailed, err)
						return
					}
					state.Undated = false
				}
				for sessionID := range missingSessions {
					markMissingSession(&state, sessionID)
				}
				state.Dates = state.Dates[1:]
				state.LastSuccessfulAt = p.now().UTC().Format(time.RFC3339Nano)
				state.LastError = ""
				if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
					log.Printf("session-cost historical import checkpoint: %v", err)
					return
				}
				if len(state.Dates) > 0 {
					continue
				}
			}
		}

		state.Processed += len(sessions)
		state.LastSuccessfulAt = p.now().UTC().Format(time.RFC3339Nano)
		state.LastError = ""
		state.Status = importStatusRunning
		if info == nil || !info.HasMore || info.NextCursor == "" {
			state.Cursor = ""
			state.Status = importStatusCompleted
			state.FinishedAt = p.now().UTC().Format(time.RFC3339Nano)
		} else {
			state.Cursor = info.NextCursor
		}
		state.Dates = nil
		if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
			log.Printf("session-cost historical import checkpoint: %v", err)
			return
		}
		if state.Status == importStatusCompleted {
			return
		}
		// Let active/final collection and manual refresh take the next report
		// slot before the next bounded import page.
		select {
		case <-ctx.Done():
			p.finishHistoricalImport(workspaceID, importStatusCancelled, nil)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func markMissingSession(state *historicalImportState, sessionID string) {
	if sessionID == "" {
		return
	}
	for _, existing := range state.MissingSessionIDs {
		if existing == sessionID {
			return
		}
	}
	state.Missing++
	if len(state.MissingSessionIDs) < maxMissingSessionIDs {
		state.MissingSessionIDs = append(state.MissingSessionIDs, sessionID)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
	if err != nil || !found {
		return
	}
	if status == importStatusCancelled && state.Status == importStatusCancelled {
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
	if state.Status == "" {
		state.Status = importStatusNotStarted
	}
	return state, true, nil
}

func (p *plugin) saveHistoricalImportState(ctx context.Context, workspaceID string, state historicalImportState) error {
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
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &pluginsdk.PluginActionResponse{Status: status, Headers: jsonHeaders(), Body: encoded}, nil
}
