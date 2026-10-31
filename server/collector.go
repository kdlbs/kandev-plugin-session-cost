package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
)

const (
	defaultCollectionInterval  = 5 * time.Minute
	maxFinalCollectionAttempts = 3
	maxObservedRolloverDays    = 3
	collectorStateScope        = "plugin"
	collectorStateID           = "collector"
	collectorStateKey          = "checkpoint"
)

type collectorCheckpoint struct {
	LastDate    string                      `json:"last_date,omitempty"`
	LastScanAt  string                      `json:"last_scan_at,omitempty"`
	Initialized bool                        `json:"initialized"`
	Pending     map[string]collectorPending `json:"pending,omitempty"`
}

type collectorPending struct {
	ActiveDate     string         `json:"active_date,omitempty"`
	RolloverDates  []string       `json:"rollover_dates,omitempty"`
	SubjectBinding string         `json:"subject_binding,omitempty"`
	TaskID         string         `json:"task_id"`
	WorkspaceID    string         `json:"workspace_id"`
	ACPSessionID   string         `json:"acp_session_id"`
	State          string         `json:"state"`
	UpdatedAt      string         `json:"updated_at,omitempty"`
	Attempts       int            `json:"attempts,omitempty"`
	Dates          []string       `json:"dates,omitempty"`
	DateAttempts   map[string]int `json:"date_attempts,omitempty"`
}

func (p *plugin) collectionEnabled(ctx context.Context) bool {
	value, ok := p.config(ctx)[configKeyCollect].(bool)
	return ok && value
}

func (p *plugin) collectionInterval(ctx context.Context) time.Duration {
	value := p.config(ctx)[configKeyInterval]
	minutes := 5.0
	switch typed := value.(type) {
	case float64:
		minutes = typed
	case int:
		minutes = float64(typed)
	case int64:
		minutes = float64(typed)
	}
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 24*60 {
		minutes = 24 * 60
	}
	return time.Duration(minutes * float64(time.Minute))
}

func (p *plugin) startCollector() {
	ctx, cancelConfig := context.WithTimeout(context.Background(), p.hostRead)
	defer cancelConfig()
	if !p.collectionEnabled(ctx) {
		return
	}
	p.collectorMu.Lock()
	defer p.collectorMu.Unlock()
	if p.collectorStarted {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.collectorStarted = true
	p.collectorCancel = cancel
	go p.collectorLoop(ctx, p.collectionInterval(context.Background()))
}

func (p *plugin) collectorLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.collectOnce(ctx); err != nil && ctx.Err() == nil {
				log.Printf("session-cost collection: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *plugin) collectOnce(ctx context.Context) error {
	if !p.collectionEnabled(ctx) {
		return nil
	}
	host := p.Host()
	if host == nil {
		return nil
	}
	checkpoint, err := p.loadCollectorCheckpoint(ctx, host)
	if err != nil {
		return err
	}
	previousScan := checkpoint.LastScanAt
	wasInitialized := checkpoint.Initialized
	now := p.now().UTC()
	sessions, err := listCollectorSessions(ctx, host.Sessions(), previousScan, wasInitialized, checkpoint.Pending)
	if err != nil {
		return err
	}
	if checkpoint.Pending == nil {
		checkpoint.Pending = make(map[string]collectorPending)
	}
	liveIDs := make(map[string]bool)
	for _, session := range sessions {
		liveIDs[session.ID] = true
	}
	for id, pending := range checkpoint.Pending {
		if liveIDs[id] || pending.SubjectBinding == "" {
			continue
		}
		state := pending.State
		if isActiveState(state) {
			state = "COMPLETED"
		}
		sessions = append(sessions, pluginsdk.Session{ID: id, TaskID: pending.TaskID, ACPSessionID: pending.ACPSessionID, State: state, UpdatedAt: pending.UpdatedAt})
	}
	eligible := make([]pluginsdk.Session, 0, len(sessions))
	for _, session := range sessions {
		if session.ACPSessionID == "" {
			continue
		}
		pending, hasPending := checkpoint.Pending[session.ID]
		stateChanged := hasPending && (pending.State != session.State || pending.UpdatedAt != session.UpdatedAt)
		if isTerminalState(session.State) {
			if !hasPending && (!wasInitialized || previousScan == "" || session.UpdatedAt == "" || session.UpdatedAt <= previousScan) {
				continue
			}
			if stateChanged {
				pending.Attempts = 0
			}
			if pending.Attempts >= maxFinalCollectionAttempts && len(pending.Dates) == 0 {
				continue
			}
		} else if !isActiveState(session.State) {
			continue
		}
		pending.TaskID = session.TaskID
		pending.ACPSessionID = session.ACPSessionID
		pending.State = session.State
		pending.UpdatedAt = session.UpdatedAt
		if isActiveState(session.State) {
			currentDate := collectionDate(now)
			if pending.ActiveDate != "" && pending.ActiveDate != currentDate {
				addObservedRollover(&pending, pending.ActiveDate)
			}
			pending.ActiveDate = currentDate
			pending.Dates = append([]string{currentDate}, pending.RolloverDates...)
		} else if !hasPending || stateChanged {
			if pending.ActiveDate != "" {
				addObservedRollover(&pending, pending.ActiveDate)
				pending.ActiveDate = ""
			}
			pending.Dates = collectorSessionDates(session, now)
			for _, date := range pending.RolloverDates {
				if !containsDate(pending.Dates, date) {
					pending.Dates = append(pending.Dates, date)
				}
			}
		}
		if pending.SubjectBinding == "" {
			if _, ok := pluginsdk.HostUsageHistory(host); ok {
				task, taskErr := host.Tasks().Get(ctx, session.TaskID)
				if taskErr == nil && task != nil {
					target, targetErr := p.resolveUsageTarget(ctx, task.WorkspaceID, session.TaskID, session.ID, session.ACPSessionID)
					if targetErr != nil {
						return targetErr
					}
					pending.SubjectBinding = target.SubjectBinding
					pending.WorkspaceID = task.WorkspaceID
				}
			}
		}
		checkpoint.Pending[session.ID] = pending
		eligible = append(eligible, session)
	}
	checkpoint.LastScanAt = now.Format(time.RFC3339Nano)
	checkpoint.Initialized = true
	if len(eligible) == 0 {
		return p.saveCollectorCheckpoint(ctx, host, checkpoint)
	}
	// Save the scan boundary and pending terminal work before starting an
	// external command. A timeout or process restart must not turn this scan
	// into an invisible gap in final collection.
	if err := p.saveCollectorCheckpoint(ctx, host, checkpoint); err != nil {
		return err
	}
	for _, session := range eligible {
		pending := checkpoint.Pending[session.ID]
		if err := p.preflightSource(ctx, pending.WorkspaceID, session.TaskID, session.ID, session.ACPSessionID); err != nil {
			return err
		}
	}
	warn, high := p.configuredThresholds(ctx)
	cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)
	runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	entries, err := p.runReport(runCtx, cmd)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !p.collectionEnabled(ctx) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, session := range eligible {
		response := responseForEntries(session.ID, session.ACPSessionID, entries, warn, high, p.now())
		if !response.Found {
			if err := p.checkTokscaleSnapshotCoverage(ctx, &response, usageCoverage{}); err != nil {
				return err
			}
		}
		if response.Found {
			pending, err := p.persistCollectorUsage(ctx, checkpoint.Pending[session.ID], session, &response, usageCoverage{})
			if err != nil {
				return err
			}
			checkpoint.Pending[session.ID] = pending
		}
	}

	date := nextCollectionDate(checkpoint.Pending, eligible, checkpoint.LastDate)
	if date != "" {
		for _, session := range eligible {
			pending := checkpoint.Pending[session.ID]
			if containsDate(pending.Dates, date) {
				if err := p.preflightSource(ctx, pending.WorkspaceID, session.TaskID, session.ID, session.ACPSessionID); err != nil {
					return err
				}
			}
		}
		runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
		dateEntries, dateErr := p.runDatedReport(runCtx, cmd, date, date)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !p.collectionEnabled(ctx) {
			return nil
		}
		if dateErr != nil {
			return dateErr
		}
		checkpoint.LastDate = date
		for _, session := range eligible {
			pending := checkpoint.Pending[session.ID]
			if !containsDate(pending.Dates, date) {
				continue
			}
			response := responseForEntries(session.ID, session.ACPSessionID, dateEntries, warn, high, p.now())
			if !response.Found {
				if err := p.checkTokscaleSnapshotCoverage(ctx, &response, usageCoverage{Date: date, Timezone: sourceTimezone()}); err != nil {
					return err
				}
			}
			if response.Found {
				updated, persistErr := p.persistCollectorUsage(ctx, pending, session, &response, usageCoverage{Date: date, Timezone: sourceTimezone()})
				if persistErr != nil {
					return persistErr
				}
				pending = updated
				if isActiveState(session.State) && date == pending.ActiveDate {
					pending.Dates = removeDate(pending.Dates, date)
					delete(pending.DateAttempts, date)
				} else {
					finishObservedDate(&pending, date)
				}
				pending.Attempts = 0
			} else {
				finishObservedDate(&pending, date)
			}
			checkpoint.Pending[session.ID] = pending
		}
	}
	for _, session := range eligible {
		pending, ok := checkpoint.Pending[session.ID]
		if !ok {
			continue
		}
		if isTerminalState(session.State) {
			pending.Attempts++
			if pending.Attempts >= maxFinalCollectionAttempts && len(pending.Dates) == 0 {
				delete(checkpoint.Pending, session.ID)
				continue
			}
		}
		checkpoint.Pending[session.ID] = pending
	}
	return p.saveCollectorCheckpoint(ctx, host, checkpoint)
}

func (p *plugin) loadCollectorCheckpoint(ctx context.Context, host pluginsdk.Host) (collectorCheckpoint, error) {
	value, found, err := host.GetState(ctx, collectorStateScope, collectorStateID, collectorStateKey)
	if err != nil {
		return collectorCheckpoint{}, err
	}
	checkpoint := collectorCheckpoint{Pending: make(map[string]collectorPending)}
	if found {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return collectorCheckpoint{}, marshalErr
		}
		if unmarshalErr := json.Unmarshal(encoded, &checkpoint); unmarshalErr != nil {
			return collectorCheckpoint{}, unmarshalErr
		}
		if checkpoint.Pending == nil {
			checkpoint.Pending = make(map[string]collectorPending)
		}
	}
	if checkpoint.LastScanAt == "" {
		checkpoint.LastScanAt = p.now().UTC().Format(time.RFC3339Nano)
	}
	return checkpoint, nil
}

func (p *plugin) saveCollectorCheckpoint(ctx context.Context, host pluginsdk.Host, checkpoint collectorCheckpoint) error {
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return err
	}
	return host.SetState(ctx, collectorStateScope, collectorStateID, collectorStateKey, value)
}

func listCollectorSessions(ctx context.Context, reader pluginsdk.SessionReader, since string, initialized bool, pending map[string]collectorPending) ([]pluginsdk.Session, error) {
	byID := make(map[string]pluginsdk.Session)
	activeFilter := pluginsdk.SessionFilter{States: []string{"CREATED", "STARTING", "RUNNING", "IDLE", "WAITING_FOR_INPUT"}}
	if initialized && since != "" {
		activeSince := since
		activeFilter.UpdatedSince = &activeSince
	}
	active, err := listAllSessionsWithFilter(ctx, reader, activeFilter)
	if err != nil {
		return nil, err
	}
	for _, session := range active {
		byID[session.ID] = session
	}
	if initialized && since != "" {
		terminalSince := since
		terminal, err := listAllSessionsWithFilter(ctx, reader, pluginsdk.SessionFilter{States: []string{"COMPLETED", "FAILED", "CANCELLED"}, UpdatedSince: &terminalSince})
		if err != nil {
			return nil, err
		}
		for _, session := range terminal {
			byID[session.ID] = session
		}
	}
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		pendingSessions, err := listAllSessionsWithFilter(ctx, reader, pluginsdk.SessionFilter{
			SessionIDs: ids,
			States:     []string{"CREATED", "STARTING", "RUNNING", "IDLE", "WAITING_FOR_INPUT", "COMPLETED", "FAILED", "CANCELLED"},
		})
		if err != nil {
			return nil, err
		}
		for _, session := range pendingSessions {
			byID[session.ID] = session
		}
	}
	result := make([]pluginsdk.Session, 0, len(byID))
	for _, session := range byID {
		result = append(result, session)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// collectorSessionDates is intentionally limited to scheduled collection's
// live coverage. The explicit historical import owns older source-local days.
func collectorSessionDates(session pluginsdk.Session, now time.Time) []string {
	if isActiveState(session.State) {
		return []string{collectionDate(now)}
	}
	end := now
	if session.EndedAt != nil {
		if parsed := parseSessionTime(*session.EndedAt); !parsed.IsZero() {
			end = parsed
		}
	} else if parsed := parseSessionTime(session.UpdatedAt); !parsed.IsZero() {
		end = parsed
	}
	return []string{collectionDate(end)}
}

func collectionDate(value time.Time) string {
	location := sourceLocation()
	local := value.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).Format("2006-01-02")
}

func nextCollectionDate(pending map[string]collectorPending, sessions []pluginsdk.Session, lastDate ...string) string {
	activeDates := make([]string, 0)
	terminalDates := make([]string, 0)
	for _, session := range sessions {
		item, ok := pending[session.ID]
		if !ok {
			continue
		}
		if isActiveState(session.State) {
			activeDates = append(activeDates, item.Dates...)
		} else {
			terminalDates = append(terminalDates, item.Dates...)
		}
	}
	// Start with active work, then rotate through every queued date so
	// repeatedly queued active days cannot starve terminal stabilization.
	dates := activeDates
	if len(dates) == 0 {
		dates = terminalDates
	}
	if len(dates) == 0 {
		return ""
	}
	if len(lastDate) > 0 && lastDate[0] != "" {
		dates = append(activeDates, terminalDates...)
		sort.Strings(dates)
		for _, date := range dates {
			if date > lastDate[0] {
				return date
			}
		}
	}
	sort.Strings(dates)
	return dates[0]
}

func containsDate(dates []string, target string) bool {
	for _, date := range dates {
		if date == target {
			return true
		}
	}
	return false
}

func removeDate(dates []string, target string) []string {
	result := dates[:0]
	for _, date := range dates {
		if date != target {
			result = append(result, date)
		}
	}
	return result
}

func parseSessionTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func sourceLocation() *time.Location {
	if name := strings.TrimPrefix(strings.TrimSpace(os.Getenv("TZ")), ":"); name != "" {
		if location, err := time.LoadLocation(name); err == nil {
			return location
		}
	}
	return time.Local
}

func sourceTimezone() string {
	return resolveSourceTimezone(sourceLocation(), os.Readlink, platformSourceTimezone)
}

func resolveSourceTimezone(location *time.Location, readlink func(string) (string, error), fallback func() string) string {
	if name := location.String(); name != "" && name != "Local" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}

	// time.Local commonly reports only "Local" even when the process is using
	// an IANA zone. Recover the zone from the standard Unix localtime link so a
	// saved source date can be interpreted with the same DST rules by the Host.
	if target, err := readlink("/etc/localtime"); err == nil {
		if marker := "/zoneinfo/"; strings.Contains(target, marker) {
			name := target[strings.Index(target, marker)+len(marker):]
			if name != "" {
				return name
			}
		}
	}
	return fallback()
}

func listAllSessions(ctx context.Context, reader pluginsdk.SessionReader) ([]pluginsdk.Session, error) {
	return listAllSessionsWithFilter(ctx, reader, pluginsdk.SessionFilter{})
}

func listAllSessionsWithFilter(ctx context.Context, reader pluginsdk.SessionReader, filter pluginsdk.SessionFilter) ([]pluginsdk.Session, error) {
	var sessions []pluginsdk.Session
	page := pluginsdk.Page{Limit: 200}
	for {
		items, info, err := reader.List(ctx, filter, page)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, items...)
		if info == nil || !info.HasMore || info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
	}
	return sessions, nil
}

func isActiveState(state string) bool {
	switch strings.ToUpper(state) {
	case "CREATED", "STARTING", "RUNNING", "IDLE", "WAITING_FOR_INPUT":
		return true
	default:
		return false
	}
}

func isTerminalState(state string) bool {
	switch strings.ToUpper(state) {
	case "COMPLETED", "FAILED", "CANCELLED":
		return true
	default:
		return false
	}
}

func responseForEntries(sessionID, acpID string, entries []sessionModelEntry, warn, high float64, now time.Time) sessionCostResponse {
	response := sessionCostResponse{
		GeneratedAt: now.UTC().Format(time.RFC3339), KandevSessionID: sessionID,
		ACPSessionID: acpID, Models: []sessionModel{}, WarnThreshold: warn, HighThreshold: high,
		Tokscale: InstallStatus{Installed: true}, Coverage: "missing", CostKnown: true,
	}
	response.presence = make(map[string]usagePresence)
	for _, entry := range entries {
		if !sessionMatches(entry.SessionID, acpID) {
			continue
		}
		response.Found = true
		response.sourceEntries = append(response.sourceEntries, entry)
		response.Cost += entry.Cost
		response.Input += entry.Input
		response.Output += entry.Output
		response.CacheRead += entry.CacheRead
		response.CacheWrite += entry.CacheWrite
		response.Reasoning += entry.Reasoning
		presence := presenceForEntry(entry)
		total := entry.Input + entry.Output + entry.CacheRead + entry.CacheWrite + entry.Reasoning
		if presence.totalValue != nil {
			total = *presence.totalValue
		}
		response.Total += total
		response.CostKnown = response.CostKnown && presence.cost
		response.presence[usagePresenceKey(entry.Model, entry.Provider)] = presence
		response.Models = append(response.Models, sessionModel{
			Model: entry.Model, Input: entry.Input, Output: entry.Output, CacheRead: entry.CacheRead,
			CacheWrite: entry.CacheWrite, Reasoning: entry.Reasoning, Provider: entry.Provider, Cost: entry.Cost,
		})
	}
	sourceDisplay(&response)
	if response.Found {
		response.Coverage = "complete"
	}
	if response.CostKnown && response.Turns > 0 {
		response.CostPerTurn = response.Cost / float64(response.Turns)
	}
	return response
}

func (p *plugin) persistCollectorUsage(ctx context.Context, pending collectorPending, session pluginsdk.Session, response *sessionCostResponse, coverage usageCoverage) (collectorPending, error) {
	workspace := pending.WorkspaceID
	if workspace == "" {
		task, err := p.Host().Tasks().Get(ctx, session.TaskID)
		if err != nil {
			return pending, err
		}
		if task == nil {
			return pending, errors.New("usage task unavailable before binding")
		}
		workspace = task.WorkspaceID
	}
	target, err := p.resolveUsageTarget(ctx, workspace, session.TaskID, session.ID, session.ACPSessionID, pending.SubjectBinding)
	if err != nil {
		return pending, err
	}
	pending.SubjectBinding, pending.WorkspaceID = target.SubjectBinding, workspace
	return pending, p.persistTargetUsage(ctx, target, response, coverage)
}

// Keep only days actually observed active. This bounded queue is not history
// discovery; current sampling and final work share the existing date rotation.
func addObservedRollover(pending *collectorPending, date string) {
	if date == "" || containsDate(pending.RolloverDates, date) {
		return
	}
	pending.RolloverDates = append(pending.RolloverDates, date)
	sort.Strings(pending.RolloverDates)
	if len(pending.RolloverDates) > maxObservedRolloverDays {
		old := pending.RolloverDates[0]
		pending.RolloverDates = pending.RolloverDates[1:]
		delete(pending.DateAttempts, old)
	}
}

func finishObservedDate(pending *collectorPending, date string) {
	if pending.DateAttempts == nil {
		pending.DateAttempts = make(map[string]int)
	}
	pending.DateAttempts[date]++
	if pending.DateAttempts[date] >= maxFinalCollectionAttempts {
		pending.Dates = removeDate(pending.Dates, date)
		pending.RolloverDates = removeDate(pending.RolloverDates, date)
		delete(pending.DateAttempts, date)
	}
}
