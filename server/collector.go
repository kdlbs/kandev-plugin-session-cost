package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	defaultCollectionInterval  = 5 * time.Minute
	maxFinalCollectionAttempts = 3
	collectorStateScope        = "plugin"
	collectorStateID           = "collector"
	collectorStateKey          = "checkpoint"
	maxCollectorDays           = 31
)

type collectorCheckpoint struct {
	LastScanAt  string                      `json:"last_scan_at,omitempty"`
	Initialized bool                        `json:"initialized"`
	Pending     map[string]collectorPending `json:"pending,omitempty"`
}

type collectorPending struct {
	TaskID       string         `json:"task_id"`
	WorkspaceID  string         `json:"workspace_id"`
	ACPSessionID string         `json:"acp_session_id"`
	State        string         `json:"state"`
	UpdatedAt    string         `json:"updated_at,omitempty"`
	Attempts     int            `json:"attempts,omitempty"`
	Dates        []string       `json:"dates,omitempty"`
	DateAttempts map[string]int `json:"date_attempts,omitempty"`
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
	if !p.collectionEnabled(context.Background()) {
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
				pending.DateAttempts = nil
			}
			if pending.Attempts >= maxFinalCollectionAttempts {
				continue
			}
		} else if !isActiveState(session.State) {
			continue
		}
		pending.TaskID = session.TaskID
		pending.ACPSessionID = session.ACPSessionID
		pending.State = session.State
		pending.UpdatedAt = session.UpdatedAt
		if !hasPending || stateChanged {
			// Scheduled collection owns the current active snapshot and the
			// terminal session's final day. Older days are explicit historical
			// import work; queueing them here would make enablement look like an
			// implicit history scan.
			pending.Dates = collectorSessionDates(session, now)
		} else if isActiveState(session.State) {
			// An active session is sampled once for the current source-local day
			// on every tick. Replace an old checkpoint day at a date boundary so
			// a long lived session cannot starve current usage with backfill.
			currentDate := collectionDate(now)
			if len(pending.Dates) != 1 || pending.Dates[0] != currentDate {
				pending.Dates = []string{currentDate}
				pending.DateAttempts = nil
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
	warn, high := p.configuredThresholds(ctx)
	cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)
	runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	entries, err := p.runReport(runCtx, cmd)
	cancel()
	if err != nil {
		return err
	}
	for _, session := range eligible {
		response := responseForEntries(session.ID, session.ACPSessionID, entries, warn, high, p.now())
		if response.Found {
			task, taskErr := host.Tasks().Get(ctx, session.TaskID)
			if taskErr != nil {
				return taskErr
			}
			if task != nil {
				if persistErr := p.persistSessionUsage(ctx, task.WorkspaceID, session.TaskID, session.ID, &response); persistErr != nil {
					return persistErr
				}
			}
		}
	}

	date := nextCollectionDate(checkpoint.Pending, eligible)
	if date != "" {
		runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
		dateEntries, dateErr := p.runDatedReport(runCtx, cmd, date, date)
		cancel()
		if dateErr != nil {
			return dateErr
		}
		for _, session := range eligible {
			pending := checkpoint.Pending[session.ID]
			if !containsDate(pending.Dates, date) {
				continue
			}
			response := responseForEntries(session.ID, session.ACPSessionID, dateEntries, warn, high, p.now())
			if response.Found {
				task, taskErr := host.Tasks().Get(ctx, session.TaskID)
				if taskErr != nil {
					return taskErr
				}
				if task != nil {
					if persistErr := p.persistSessionUsageWithCoverage(ctx, task.WorkspaceID, session.TaskID, session.ID, &response, usageCoverage{Date: date, Timezone: sourceTimezone()}); persistErr != nil {
						return persistErr
					}
				}
				if isActiveState(session.State) {
					// Active snapshots are refreshed again when the current day is
					// re-queued. Remove a successful day now so older pending days
					// cannot starve the current day.
					pending.Dates = removeDate(pending.Dates, date)
					if pending.DateAttempts != nil {
						delete(pending.DateAttempts, date)
					}
				} else {
					if pending.DateAttempts == nil {
						pending.DateAttempts = make(map[string]int)
					}
					// A Found response can still describe the transcript before its
					// final messages have flushed. Keep terminal dates for bounded
					// stabilization passes so a later report can correct the bucket.
					pending.DateAttempts[date]++
					if pending.DateAttempts[date] >= maxFinalCollectionAttempts {
						pending.Dates = removeDate(pending.Dates, date)
						delete(pending.DateAttempts, date)
					}
				}
				pending.Attempts = 0
			} else {
				if pending.DateAttempts == nil {
					pending.DateAttempts = make(map[string]int)
				}
				pending.DateAttempts[date]++
				if pending.DateAttempts[date] >= maxFinalCollectionAttempts {
					pending.Dates = removeDate(pending.Dates, date)
					delete(pending.DateAttempts, date)
				}
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

func mergeCollectionDays(existing []string, session pluginsdk.Session, now time.Time) []string {
	seen := make(map[string]struct{}, len(existing)+1)
	for _, date := range existing {
		seen[date] = struct{}{}
	}
	start := parseSessionTime(session.StartedAt)
	if start.IsZero() {
		start = now
	}
	end := now
	if parsed := parseSessionTime(session.UpdatedAt); !parsed.IsZero() && parsed.After(end) {
		end = parsed
	}
	if session.EndedAt != nil {
		if parsed := parseSessionTime(*session.EndedAt); !parsed.IsZero() {
			end = parsed
		}
	}
	location := sourceLocation()
	day := start.In(location).Truncate(24 * time.Hour)
	// Truncate is not a local midnight for non-UTC zones. Rebuild it from the
	// calendar date so tokscale's local inclusive filters use the same day.
	startLocal := start.In(location)
	day = time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, location)
	endLocal := end.In(location)
	last := time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), 0, 0, 0, 0, location)
	first := day
	minimum := last.AddDate(0, 0, -(maxCollectorDays - 1))
	if first.Before(minimum) {
		first = minimum
	}
	for day = first; !day.After(last); day = day.AddDate(0, 0, 1) {
		seen[day.Format("2006-01-02")] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for date := range seen {
		result = append(result, date)
	}
	sort.Strings(result)
	if len(result) > maxCollectorDays {
		result = result[len(result)-maxCollectorDays:]
	}
	return result
}

func mergeCurrentCollectionDay(existing []string, now time.Time) []string {
	date := collectionDate(now)
	seen := make(map[string]struct{}, len(existing)+1)
	for _, value := range existing {
		seen[value] = struct{}{}
	}
	seen[date] = struct{}{}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > maxCollectorDays {
		result = result[len(result)-maxCollectorDays:]
	}
	return result
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

func nextCollectionDate(pending map[string]collectorPending, sessions []pluginsdk.Session) string {
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
	// A current active snapshot must be collected promptly even when a
	// terminal session has older finalization work waiting in the checkpoint.
	dates := activeDates
	if len(dates) == 0 {
		dates = terminalDates
	}
	if len(dates) == 0 {
		return ""
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

func sourceLocation() *time.Location { return time.Local }

func sourceTimezone() string {
	if name := sourceLocation().String(); name != "" && name != "Local" {
		return name
	}
	if name := strings.TrimPrefix(strings.TrimSpace(os.Getenv("TZ")), ":"); name != "" && name != "Local" {
		return name
	}
	// time.Local commonly reports only "Local" even when the process is using
	// an IANA zone. Recover the zone from the standard Unix localtime link so a
	// saved source date can be interpreted with the same DST rules by the Host.
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if marker := "/zoneinfo/"; strings.Contains(target, marker) {
			name := target[strings.Index(target, marker)+len(marker):]
			if name != "" {
				return name
			}
		}
	}
	return "UTC"
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
		Tokscale: InstallStatus{Installed: true}, Coverage: "missing", CostKnown: true, turnsKnown: true,
	}
	response.presence = make(map[string]usagePresence)
	for _, entry := range entries {
		if !sessionMatches(entry.SessionID, acpID) {
			continue
		}
		response.Found = true
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
		response.Turns += entry.MessageCount
		response.turnsKnown = response.turnsKnown && presence.turns
		response.CostKnown = response.CostKnown && presence.cost
		response.presence[usagePresenceKey(entry.Model, entry.Provider)] = presence
		response.Models = append(response.Models, sessionModel{
			Model: entry.Model, Input: entry.Input, Output: entry.Output, CacheRead: entry.CacheRead,
			CacheWrite: entry.CacheWrite, Reasoning: entry.Reasoning, Provider: entry.Provider, Cost: entry.Cost,
		})
	}
	if response.Found {
		response.Coverage = "complete"
	}
	if response.CostKnown && response.Turns > 0 {
		response.CostPerTurn = response.Cost / float64(response.Turns)
	}
	return response
}
