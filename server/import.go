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

	importQueueScope = "plugin"
	importQueueID    = "historical-import"
	importQueueKey   = "import-queue"

	importSchemaVersion = 3

	importPhaseScanning  = "scanning"
	importPhaseDaily     = "daily"
	importPhaseCompleted = "completed"

	importStatusNotStarted  = "not_started"
	importStatusPreparing   = "preparing"
	importStatusQueued      = "queued"
	importStatusRunning     = "running"
	importStatusCompleted   = "completed"
	importStatusFailed      = "failed"
	importStatusCancelled   = "cancelled"
	importStatusDisabled    = "disabled"
	importStatusUnsupported = "unsupported"

	importTargetUncounted = ""
	importTargetImported  = "imported"
	importTargetMissing   = "missing"
	importTargetSkipped   = "skipped"

	importStateWriteAttempts = 2
	minImportReportPause     = 5 * time.Second
	maxImportReportPause     = 60 * time.Second
	maxImportDayAttempts     = 3
	minImportETADays         = 3
)

var historicalImportFailureMessage = "historical import failed; check collection compatibility and source availability"

var errUsageCollectionSettingUnavailable = errors.New("token statistics collection setting is temporarily unavailable")

// historicalImportInterval is an inclusive source-date range. Merged intervals
// bound the daily step and keep the checkpoint free of usage amounts.
type historicalImportInterval struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// historicalImportState is the schema version 3 checkpoint. It holds run
// position and counts only, never usage amounts or transcript content.
type historicalImportState struct {
	RunID              string                     `json:"run_id,omitempty"`
	SchemaVersion      int                        `json:"schema_version"`
	WorkspaceID        string                     `json:"workspace_id"`
	Status             string                     `json:"status"`
	Phase              string                     `json:"phase,omitempty"`
	SnapshotID         string                     `json:"snapshot_id,omitempty"`
	SnapshotUpperBound int64                      `json:"snapshot_upper_bound,string,omitempty"`
	ScanCursor         string                     `json:"scan_cursor,omitempty"`
	Intervals          []historicalImportInterval `json:"intervals,omitempty"`
	NextDay            string                     `json:"next_day,omitempty"`
	DaysTotal          int                        `json:"days_total,omitempty"`
	DaysDone           int                        `json:"days_done,omitempty"`
	DayAttempts        int                        `json:"day_attempts,omitempty"`
	DaySecondsTotal    int64                      `json:"day_seconds_total,string,omitempty"`
	SessionsFound      int                        `json:"sessions_found,omitempty"`
	SessionsImported   int                        `json:"sessions_imported,omitempty"`
	SessionsMissing    int                        `json:"sessions_missing,omitempty"`
	SessionsSkipped    int                        `json:"sessions_skipped,omitempty"`
	StartedAt          string                     `json:"started_at,omitempty"`
	FinishedAt         string                     `json:"finished_at,omitempty"`
	LastSuccessfulAt   string                     `json:"last_successful_at,omitempty"`
	LastError          string                     `json:"last_error,omitempty"`
	Undated            bool                       `json:"undated,omitempty"`
}

// activeImportRun is the single process-wide import worker. Tokscale reads the
// same host transcripts whichever workspace asks, so only one run may exist.
type activeImportRun struct {
	workspaceID string
	runID       string
	cancel      context.CancelFunc
}

// importTarget is one in-memory index entry. It holds identifiers and date
// bounds only; the immutable snapshot makes a restart rebuild deterministic.
type importTarget struct {
	target   pluginsdk.UsageCollectionTargetV1
	first    string
	last     string
	hasRange bool
	counted  string
	removed  bool
	rebuilt  bool
}

type importIndex struct {
	entries map[string]*importTarget
	order   []string
}

func newImportIndex() *importIndex {
	return &importIndex{entries: make(map[string]*importTarget)}
}

func (i *importIndex) reset() {
	i.entries = make(map[string]*importTarget)
	i.order = nil
}

// add indexes one eligible target. It reports false for an ineligible or
// unidentifiable target, which counts as skipped. A rebuilt entry keeps its
// persisted classification rather than changing the aggregate counts.
func (i *importIndex) add(target pluginsdk.UsageCollectionTargetV1, rebuilt bool, today string) bool {
	if !eligibleUsageSource(target) || target.SessionID == nil ||
		target.ExternalSessionID == nil || *target.ExternalSessionID == "" || target.SubjectBinding == "" {
		return false
	}
	entry := &importTarget{target: target, rebuilt: rebuilt}
	if dates := importTargetDateRange(target, today); len(dates) > 0 {
		entry.first, entry.last = dates[0], dates[len(dates)-1]
		entry.hasRange = true
	}
	key := target.SubjectBinding
	if _, exists := i.entries[key]; !exists {
		i.order = append(i.order, key)
	}
	i.entries[key] = entry
	return true
}

func (i *importIndex) keys() []string { return i.order }

func (i *importIndex) intervals() []historicalImportInterval {
	intervals := make([]historicalImportInterval, 0, len(i.order))
	for _, key := range i.order {
		entry := i.entries[key]
		if entry.removed || !entry.hasRange {
			continue
		}
		intervals = append(intervals, historicalImportInterval{Start: entry.first, End: entry.last})
	}
	return mergeImportIntervals(intervals)
}

type importWriteOutcome int

const (
	importWriteSucceeded importWriteOutcome = iota
	importWriteSkipped
	importWriteFailed
)

// importStep reports whether the run loop must stop and, when it does, which
// terminal status the deferred settle writes.
type importStep struct {
	done   bool
	status string
	err    error
}

func importContinue() importStep { return importStep{} }

func importFinish(status string, err error) importStep {
	return importStep{done: true, status: status, err: err}
}

func importFinished() importStep { return importStep{done: true} }

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
		// A restart may have left a persisted queue entry behind a delayed
		// startup pass; asking for status also pumps the queue.
		p.resumeHistoricalImports()
		if !found {
			state = historicalImportState{WorkspaceID: req.Context.WorkspaceID, SchemaVersion: importSchemaVersion, Status: importStatusNotStarted}
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
	p.importMu.Lock()
	defer p.importMu.Unlock()

	state, found, err := p.authorizedHistoricalImportState(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	enabled, known := p.collectionSetting(ctx)
	if !known {
		state.LastError = errUsageCollectionSettingUnavailable.Error()
		return importJSONResponse(503, state)
	}
	if !enabled {
		return p.disableHistoricalImportLocked(ctx, workspaceID, state, found)
	}
	if p.importActive != nil && p.importActive.workspaceID == workspaceID {
		return importJSONResponse(202, state)
	}
	if _, err := usageHistory(p.Host()); err != nil {
		state.Status = importStatusUnsupported
		state.LastError = err.Error()
		return importJSONResponse(409, state)
	}
	queue, err := p.readImportQueue(ctx)
	if err != nil {
		return nil, err
	}
	if containsImportWorkspace(queue, workspaceID) {
		p.startNextImportLocked()
		return importJSONResponse(202, state)
	}
	startNow := p.importActive == nil && (len(queue) == 0 || queue[0] == workspaceID)
	resume := importResumableState(state, found)
	state.RunID = rand.Text()
	state.SchemaVersion = importSchemaVersion
	state.Status = importStatusRunning
	state.LastError = ""
	state.FinishedAt = ""
	state.DayAttempts = 0
	if !resume {
		state = historicalImportState{
			WorkspaceID: workspaceID, SchemaVersion: importSchemaVersion, RunID: state.RunID,
			Status: importStatusRunning, Phase: importPhaseScanning, StartedAt: p.now().UTC().Format(time.RFC3339Nano),
		}
	} else if state.StartedAt == "" {
		state.StartedAt = p.now().UTC().Format(time.RFC3339Nano)
	}
	if startNow && state.SnapshotID == "" {
		state = p.prepareHistoricalImportStart(ctx, workspaceID, state)
	}
	if !startNow {
		state.Status = importStatusQueued
	}
	if err := p.appendImportQueueLocked(ctx, workspaceID); err != nil {
		return nil, err
	}
	if err := p.writeHistoricalImportState(ctx, workspaceID, state, true); err != nil {
		_ = p.removeImportQueueEntryLocked(ctx, workspaceID)
		return nil, err
	}
	if startNow {
		p.startNextImportLocked()
	}
	return importJSONResponse(202, state)
}

// prepareHistoricalImportStart obtains the immutable snapshot identity before
// the first report, so a restart resumes the same target membership.
func (p *plugin) prepareHistoricalImportStart(ctx context.Context, workspaceID string, state historicalImportState) historicalImportState {
	reader, err := usageHistory(p.Host())
	if err != nil {
		state.Status = importStatusUnsupported
		state.LastError = err.Error()
		return state
	}
	page, err := p.readUsageTargetPage(ctx, reader, pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspaceID, Limit: 1}, 0)
	if errors.Is(err, errUsageTargetsPreparing) {
		state.Status = importStatusPreparing
		return state
	}
	if err != nil {
		state.LastError = err.Error()
		return state
	}
	state.SnapshotID, state.SnapshotUpperBound = page.SnapshotID, page.SnapshotUpperBound
	return state
}

// disableHistoricalImportLocked stops an active run without replacing a
// finished or cancelled checkpoint. A refused start on a finished import must
// leave its runs resumable state intact.
func (p *plugin) disableHistoricalImportLocked(ctx context.Context, workspaceID string, state historicalImportState, found bool) (*pluginsdk.PluginActionResponse, error) {
	if !found || state.SchemaVersion != importSchemaVersion {
		state = historicalImportState{WorkspaceID: workspaceID, SchemaVersion: importSchemaVersion}
	}
	state.LastError = errUsageCollectionDisabled.Error()
	if !importActiveStatus(state.Status) {
		if !found || state.SchemaVersion != importSchemaVersion || state.Status == "" {
			state.Status = importStatusDisabled
		}
		return importJSONResponse(409, state)
	}
	state.Status = importStatusDisabled
	state.FinishedAt = p.now().UTC().Format(time.RFC3339Nano)
	if err := p.writeHistoricalImportState(ctx, workspaceID, state, true); err != nil {
		return nil, err
	}
	_ = p.removeImportQueueEntryLocked(ctx, workspaceID)
	return importJSONResponse(409, state)
}

func importActiveStatus(status string) bool {
	return status == importStatusPreparing || status == importStatusQueued || status == importStatusRunning
}

func importResumableState(state historicalImportState, found bool) bool {
	if !found || state.SchemaVersion != importSchemaVersion {
		return false
	}
	if state.Phase != importPhaseScanning && state.Phase != importPhaseDaily {
		return false
	}
	return state.Status == importStatusFailed || state.Status == importStatusDisabled
}

func (p *plugin) cancelHistoricalImport(ctx context.Context, workspaceID string) (*pluginsdk.PluginActionResponse, error) {
	state, found, err := p.authorizedHistoricalImportState(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return importJSONResponse(200, historicalImportState{WorkspaceID: workspaceID, SchemaVersion: importSchemaVersion, Status: importStatusNotStarted})
	}
	if state.Status == importStatusPreparing || state.Status == importStatusQueued || state.Status == importStatusRunning {
		state.Status = importStatusCancelled
		state.FinishedAt = p.now().UTC().Format(time.RFC3339Nano)
		state.LastError = ""
		if err := p.saveHistoricalImportState(ctx, workspaceID, state); err != nil {
			return nil, err
		}
	}
	p.importMu.Lock()
	_ = p.removeImportQueueEntryLocked(ctx, workspaceID)
	if p.importActive != nil && p.importActive.workspaceID == workspaceID {
		p.importActive.cancel()
	}
	p.importMu.Unlock()
	return importJSONResponse(200, state)
}

// runHistoricalImport resumes the checkpoint's own run. Tests use it directly;
// production starts through the queue with an explicit run identity.
func (p *plugin) runHistoricalImport(ctx context.Context, workspaceID string) {
	state, _, _ := p.loadHistoricalImportState(ctx, workspaceID)
	p.runHistoricalImportRun(ctx, workspaceID, state.RunID)
}

func (p *plugin) runHistoricalImportRun(ctx context.Context, workspaceID, runID string) {
	outcome := importStep{}
	defer func() { p.settleHistoricalImport(workspaceID, runID, outcome) }()
	outcome.status, outcome.err = p.advanceHistoricalImport(ctx, workspaceID, runID)
}

// settle is the single exit path for every worker. It writes a terminal status
// only when the run did not write one, and never during plugin shutdown.
func (p *plugin) settleHistoricalImport(workspaceID, runID string, outcome importStep) {
	p.importMu.Lock()
	defer p.importMu.Unlock()
	if active := p.importActive; active != nil && active.workspaceID == workspaceID && active.runID == runID {
		p.importActive = nil
	}
	if p.importShuttingDown {
		return
	}
	p.writeHistoricalImportResult(workspaceID, runID, outcome)
	_ = p.removeImportQueueEntryLocked(context.Background(), workspaceID)
	p.startNextImportLocked()
}

func (p *plugin) writeHistoricalImportResult(workspaceID, runID string, outcome importStep) {
	for attempt := 0; attempt < importStateWriteAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := p.saveHistoricalImportResult(ctx, workspaceID, runID, outcome)
		cancel()
		if err == nil {
			return
		}
		log.Printf("session-cost historical import result write failed: %v", err)
	}
}

func (p *plugin) saveHistoricalImportResult(ctx context.Context, workspaceID, runID string, outcome importStep) error {
	p.importStateMu.Lock()
	defer p.importStateMu.Unlock()
	if p.importRevoked[stableUsageID(workspaceID, runID)] {
		return nil
	}
	state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
	if err != nil || !found {
		return err
	}
	if runID != "" && state.RunID != runID {
		// A newer run owns this workspace.
		return nil
	}
	if importStatusTerminal(state.Status) {
		return nil
	}
	status := outcome.status
	if status == "" {
		status = importStatusFailed
	}
	state.Status = status
	state.FinishedAt = p.now().UTC().Format(time.RFC3339Nano)
	if status == importStatusCompleted {
		state.Phase = importPhaseCompleted
	}
	if outcome.err != nil {
		state.LastError = outcome.err.Error()
	} else {
		state.LastError = ""
	}
	return p.writeHistoricalImportStateValue(ctx, workspaceID, state)
}

func (p *plugin) advanceHistoricalImport(ctx context.Context, workspaceID, runID string) (string, error) {
	host := p.Host()
	if host == nil {
		return importStatusFailed, errors.New("host unavailable")
	}
	reader, err := usageHistory(host)
	if err != nil {
		return importStatusUnsupported, err
	}
	index := newImportIndex()
	indexBuilt := false
	for {
		if ctx.Err() != nil {
			return importStatusCancelled, nil
		}
		enabled, ok := p.awaitImportCollection(ctx)
		if !ok {
			return importStatusCancelled, nil
		}
		if !enabled {
			return importStatusDisabled, errUsageCollectionDisabled
		}
		state, found, err := p.loadHistoricalImportState(ctx, workspaceID)
		if err != nil {
			return importStatusFailed, err
		}
		if !found {
			return "", nil
		}
		if runID != "" && state.RunID != runID {
			return "", nil
		}
		if importStatusTerminal(state.Status) {
			return "", nil
		}
		if state.SchemaVersion != importSchemaVersion {
			return importStatusFailed, errors.New("historical import checkpoint schema is not supported")
		}
		if state.Phase == "" {
			state.Phase = importPhaseScanning
		}
		var step importStep
		switch {
		case state.SnapshotID == "":
			step = p.prepareImportSnapshot(ctx, workspaceID, &state, reader)
		case state.Phase == importPhaseScanning:
			step = p.scanImportSnapshot(ctx, reader, workspaceID, &state, index)
			indexBuilt = true
		case state.Phase == importPhaseDaily && !indexBuilt:
			// The snapshot is immutable, so a restart rebuilds the same index
			// before continuing from NextDay.
			step = p.rebuildImportIndex(ctx, reader, &state, index)
			indexBuilt = !step.done
		case state.Phase == importPhaseDaily:
			step = p.advanceImportDay(ctx, workspaceID, &state, index)
		case state.Phase == importPhaseCompleted:
			return "", nil
		default:
			return importStatusFailed, fmt.Errorf("unknown historical import phase %q", state.Phase)
		}
		if step.done {
			return step.status, step.err
		}
	}
}

func (p *plugin) prepareImportSnapshot(ctx context.Context, workspaceID string, state *historicalImportState, reader pluginsdk.UsageHistoryReader) importStep {
	page, err := p.readUsageTargetPage(ctx, reader, pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspaceID, Limit: 1}, 0)
	if errors.Is(err, errUsageTargetsPreparing) {
		state.Status = importStatusPreparing
		if saveErr := p.saveHistoricalImportState(ctx, workspaceID, *state); saveErr != nil {
			return importFinish(importStatusFailed, saveErr)
		}
		if !p.waitTargetPreparation(ctx) {
			return importFinish(importStatusCancelled, nil)
		}
		return importContinue()
	}
	if err != nil {
		return importFinish(importFailureStatus(err), historyError(err))
	}
	state.SnapshotID, state.SnapshotUpperBound = page.SnapshotID, page.SnapshotUpperBound
	state.Phase = importPhaseScanning
	state.Status = importStatusRunning
	if err := p.saveHistoricalImportState(ctx, workspaceID, *state); err != nil {
		return importFinish(importStatusFailed, err)
	}
	return importContinue()
}

// scanImportSnapshot walks every immutable target page and rebuilds the index
// from scratch. Only then does the all-time step run its single lifetime report.
func (p *plugin) scanImportSnapshot(ctx context.Context, reader pluginsdk.UsageHistoryReader, workspaceID string, state *historicalImportState, index *importIndex) importStep {
	index.reset()
	state.Phase = importPhaseScanning
	state.ScanCursor = ""
	// A restart rebuilds the whole index, so recount from zero.
	state.SessionsFound, state.SessionsImported, state.SessionsMissing, state.SessionsSkipped = 0, 0, 0, 0
	state.Undated = false
	for {
		query := pluginsdk.UsageCollectionTargetQueryV1{
			WorkspaceID: workspaceID, Limit: importPageSize, SnapshotID: state.SnapshotID, Cursor: state.ScanCursor,
		}
		page, err := p.readUsageTargetPage(ctx, reader, query, state.SnapshotUpperBound)
		if err != nil {
			return importFinish(importFailureStatus(err), historyError(err))
		}
		for _, target := range page.Targets {
			state.SessionsFound++
			if !index.add(target, false, collectionDate(p.now())) {
				state.SessionsSkipped++
			}
		}
		if !page.HasMore {
			break
		}
		state.ScanCursor = page.NextCursor
		if err := p.saveHistoricalImportState(ctx, workspaceID, *state); err != nil {
			return importFinish(importStatusFailed, err)
		}
	}
	state.ScanCursor = ""
	state.Status = importStatusRunning
	state.LastError = ""
	if err := p.saveHistoricalImportState(ctx, workspaceID, *state); err != nil {
		return importFinish(importStatusFailed, err)
	}
	if len(index.order) == 0 {
		// No eligible session means no source report has anything to match.
		state.Intervals = nil
		state.DaysTotal = 0
		state.NextDay = ""
		state.Phase = importPhaseDaily
		p.completeHistoricalImportState(state)
		if err := p.saveHistoricalImportState(ctx, workspaceID, *state); err != nil {
			return importFinish(importStatusFailed, err)
		}
		return importFinished()
	}
	return p.runImportLifetimeReport(ctx, workspaceID, state, index)
}

// rebuildImportIndex replays the immutable snapshot after a restart. It never
// changes persisted counts; the earlier classification is already final.
func (p *plugin) rebuildImportIndex(ctx context.Context, reader pluginsdk.UsageHistoryReader, state *historicalImportState, index *importIndex) importStep {
	index.reset()
	cursor := ""
	for {
		query := pluginsdk.UsageCollectionTargetQueryV1{
			WorkspaceID: state.WorkspaceID, Limit: importPageSize, SnapshotID: state.SnapshotID, Cursor: cursor,
		}
		page, err := p.readUsageTargetPage(ctx, reader, query, state.SnapshotUpperBound)
		if err != nil {
			return importFinish(importFailureStatus(err), historyError(err))
		}
		for _, target := range page.Targets {
			index.add(target, true, collectionDate(p.now()))
		}
		if !page.HasMore {
			return importContinue()
		}
		cursor = page.NextCursor
	}
}

func (p *plugin) runImportLifetimeReport(ctx context.Context, workspaceID string, state *historicalImportState, index *importIndex) importStep {
	if err := p.validateUsageSnapshot(ctx, workspaceID, state.SnapshotID, state.SnapshotUpperBound); err != nil {
		return p.importSnapshotStep(ctx, workspaceID, state, err)
	}
	warn, high := p.configuredThresholds(ctx)
	cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)
	started := p.now()
	runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	entries, err := p.runReport(runCtx, cmd)
	cancel()
	if ctx.Err() != nil {
		return importFinish(importStatusCancelled, nil)
	}
	enabled, collectionOK := p.awaitImportCollection(ctx)
	if !collectionOK {
		return importFinish(importStatusCancelled, nil)
	}
	if !enabled {
		return importFinish(importStatusDisabled, errUsageCollectionDisabled)
	}
	if err != nil {
		return importFinish(importStatusFailed, err)
	}
	for _, key := range index.keys() {
		entry := index.entries[key]
		if entry.removed {
			continue
		}
		response := responseForEntries(*entry.target.SessionID, *entry.target.ExternalSessionID, entries, warn, high, p.now())
		if !response.Found {
			if coverageErr := p.checkTokscaleSnapshotCoverage(ctx, &response, usageCoverage{}); coverageErr != nil {
				if !usageSourceIneligible(coverageErr) {
					return importFinish(importFailureStatus(coverageErr), coverageErr)
				}
				p.skipImportTarget(state, entry)
				continue
			}
			state.SessionsMissing++
			entry.counted = importTargetMissing
			continue
		}
		outcome, writeErr := p.writeImportTarget(ctx, workspaceID, state, entry, &response, usageCoverage{})
		if outcome == importWriteFailed {
			return importFinish(importFailureStatus(writeErr), writeErr)
		}
		if outcome == importWriteSkipped {
			continue
		}
		state.SessionsImported++
		state.Undated = state.Undated || !entry.hasRange
		entry.counted = importTargetImported
	}
	state.Intervals = index.intervals()
	state.DaysTotal = importIntervalDays(state.Intervals)
	state.DaysDone = 0
	state.DayAttempts = 0
	state.DaySecondsTotal = 0
	state.NextDay = ""
	if len(state.Intervals) > 0 {
		state.NextDay = state.Intervals[0].Start
	}
	state.Phase = importPhaseDaily
	state.Status = importStatusRunning
	state.LastError = ""
	state.LastSuccessfulAt = p.now().UTC().Format(time.RFC3339Nano)
	if state.DaysTotal == 0 {
		p.completeHistoricalImportState(state)
	}
	if saveErr := p.saveHistoricalImportState(ctx, workspaceID, *state); saveErr != nil {
		return importFinish(importStatusFailed, saveErr)
	}
	if state.Status == importStatusCompleted {
		return importFinished()
	}
	if !p.waitImportBudget(ctx, p.now().Sub(started)) {
		return importFinish(importStatusCancelled, nil)
	}
	return importContinue()
}

func (p *plugin) advanceImportDay(ctx context.Context, workspaceID string, state *historicalImportState, index *importIndex) importStep {
	day := state.NextDay
	if day == "" || !importDayCovered(state.Intervals, day) {
		p.completeHistoricalImportState(state)
		if err := p.saveHistoricalImportState(ctx, workspaceID, *state); err != nil {
			return importFinish(importStatusFailed, err)
		}
		return importFinished()
	}
	if err := p.validateUsageSnapshot(ctx, workspaceID, state.SnapshotID, state.SnapshotUpperBound); err != nil {
		return p.importSnapshotStep(ctx, workspaceID, state, err)
	}
	warn, high := p.configuredThresholds(ctx)
	cmd := resolveCommand(p.configuredCommand(ctx), p.lookPath)
	started := p.now()
	runCtx, cancel := context.WithTimeout(ctx, reportTimeout)
	entries, err := p.runDatedReport(runCtx, cmd, day, day)
	cancel()
	elapsed := p.now().Sub(started)
	if ctx.Err() != nil {
		return importFinish(importStatusCancelled, nil)
	}
	enabled, collectionOK := p.awaitImportCollection(ctx)
	if !collectionOK {
		return importFinish(importStatusCancelled, nil)
	}
	if !enabled {
		return importFinish(importStatusDisabled, errUsageCollectionDisabled)
	}
	if err != nil {
		return p.retryImportDay(ctx, workspaceID, state, elapsed, err)
	}
	coverage := usageCoverage{Date: day, Timezone: sourceTimezone()}
	for _, key := range index.keys() {
		entry := index.entries[key]
		if entry.removed || !entry.hasRange || day < entry.first || day > entry.last {
			continue
		}
		response := responseForEntries(*entry.target.SessionID, *entry.target.ExternalSessionID, entries, warn, high, p.now())
		if !response.Found {
			if coverageErr := p.checkTokscaleSnapshotCoverage(ctx, &response, coverage); coverageErr != nil {
				if !usageSourceIneligible(coverageErr) {
					return p.retryImportDay(ctx, workspaceID, state, elapsed, coverageErr)
				}
				p.skipImportTarget(state, entry)
			}
			continue
		}
		outcome, writeErr := p.writeImportTarget(ctx, workspaceID, state, entry, &response, coverage)
		if outcome == importWriteFailed {
			return p.retryImportDay(ctx, workspaceID, state, elapsed, writeErr)
		}
	}
	state.DaysDone++
	state.DayAttempts = 0
	state.LastError = ""
	state.LastSuccessfulAt = p.now().UTC().Format(time.RFC3339Nano)
	state.DaySecondsTotal += int64((elapsed + importReportPause(elapsed)).Seconds())
	next := nextImportDay(state.Intervals, day)
	state.NextDay = next
	if next == "" {
		p.completeHistoricalImportState(state)
		if err := p.saveHistoricalImportState(ctx, workspaceID, *state); err != nil {
			return importFinish(importStatusFailed, err)
		}
		return importFinished()
	}
	if err := p.saveHistoricalImportState(ctx, workspaceID, *state); err != nil {
		return importFinish(importStatusFailed, err)
	}
	if !p.waitImportBudget(ctx, elapsed) {
		return importFinish(importStatusCancelled, nil)
	}
	return importContinue()
}

// retryImportDay keeps the position and stops after three consecutive failures
// for the same day. A successful day resets the counter.
func (p *plugin) retryImportDay(ctx context.Context, workspaceID string, state *historicalImportState, elapsed time.Duration, err error) importStep {
	status := importFailureStatus(err)
	if status == importStatusDisabled || status == importStatusUnsupported {
		return importFinish(status, err)
	}
	state.DayAttempts++
	if state.DayAttempts >= maxImportDayAttempts {
		if saveErr := p.saveHistoricalImportState(ctx, workspaceID, *state); saveErr != nil {
			return importFinish(importStatusFailed, saveErr)
		}
		return importFinish(importStatusFailed, err)
	}
	state.LastError = err.Error()
	if saveErr := p.saveHistoricalImportState(ctx, workspaceID, *state); saveErr != nil {
		return importFinish(importStatusFailed, saveErr)
	}
	if !p.waitImportBudget(ctx, elapsed) {
		return importFinish(importStatusCancelled, nil)
	}
	return importContinue()
}

// writeImportTarget authorizes the exact binding again before every write. A
// binding that became ineligible is reclassified instead of failing the run.
func (p *plugin) writeImportTarget(ctx context.Context, workspaceID string, state *historicalImportState, entry *importTarget, response *sessionCostResponse, coverage usageCoverage) (importWriteOutcome, error) {
	current, err := p.currentSourceTarget(ctx, workspaceID, entry.target.TaskID,
		*entry.target.SessionID, *entry.target.ExternalSessionID, entry.target.SubjectBinding)
	if err != nil {
		if usageSourceIneligible(err) {
			p.skipImportTarget(state, entry)
			return importWriteSkipped, nil
		}
		return importWriteFailed, err
	}
	entry.target = current
	if err := p.persistTargetUsage(ctx, current, response, coverage); err != nil {
		if usageSourceIneligible(err) {
			p.skipImportTarget(state, entry)
			return importWriteSkipped, nil
		}
		return importWriteFailed, err
	}
	return importWriteSucceeded, nil
}

func (p *plugin) skipImportTarget(state *historicalImportState, entry *importTarget) {
	switch entry.counted {
	case importTargetImported:
		state.SessionsImported--
	case importTargetMissing:
		state.SessionsMissing--
	}
	entry.counted = importTargetSkipped
	entry.removed = true
	if entry.rebuilt {
		// A rebuilt index cannot tell which persisted bucket this session
		// occupied, so its earlier classification stays final.
		return
	}
	state.SessionsSkipped++
}

// importSnapshotStep discards a checkpoint only when its captured scope is invalidated.
func (p *plugin) importSnapshotStep(ctx context.Context, workspaceID string, state *historicalImportState, err error) importStep {
	if !invalidatesHistoricalImportScope(err) {
		return importFinish(importFailureStatus(err), err)
	}
	p.discardHistoricalImportState(ctx, workspaceID, *state)
	return importFinished()
}

func (p *plugin) completeHistoricalImportState(state *historicalImportState) {
	now := p.now().UTC().Format(time.RFC3339Nano)
	state.Phase = importPhaseCompleted
	state.Status = importStatusCompleted
	state.NextDay = ""
	state.LastSuccessfulAt = now
	state.FinishedAt = now
	state.LastError = ""
}

func importFailureStatus(err error) string {
	if errors.Is(err, errUsageCollectionDisabled) {
		return importStatusDisabled
	}
	if errors.Is(historyError(err), errUsageHistoryUnsupported) {
		return importStatusUnsupported
	}
	return importStatusFailed
}

func importStatusTerminal(status string) bool {
	switch status {
	case importStatusCompleted, importStatusFailed, importStatusCancelled, importStatusDisabled, importStatusUnsupported:
		return true
	default:
		return false
	}
}

func importReportPause(previous time.Duration) time.Duration {
	if previous < minImportReportPause {
		return minImportReportPause
	}
	if previous > maxImportReportPause {
		return maxImportReportPause
	}
	return previous
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

// waitImportBudget waits as long as the previous report took, clamped to the
// import pacing range.
func (p *plugin) waitImportBudget(ctx context.Context, previous time.Duration) bool {
	if p.importWait != nil {
		return p.importWait(ctx)
	}
	timer := time.NewTimer(importReportPause(previous))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func mergeImportIntervals(intervals []historicalImportInterval) []historicalImportInterval {
	sorted := make([]historicalImportInterval, 0, len(intervals))
	for _, interval := range intervals {
		if interval.Start == "" || interval.End == "" || interval.End < interval.Start {
			continue
		}
		sorted = append(sorted, interval)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End < sorted[j].End
	})
	merged := make([]historicalImportInterval, 0, len(sorted))
	for _, interval := range sorted {
		if len(merged) == 0 {
			merged = append(merged, interval)
			continue
		}
		last := &merged[len(merged)-1]
		if interval.Start <= importDayAfter(last.End) {
			if interval.End > last.End {
				last.End = interval.End
			}
			continue
		}
		merged = append(merged, interval)
	}
	return merged
}

func importIntervalDays(intervals []historicalImportInterval) int {
	location := sourceLocation()
	total := 0
	for _, interval := range intervals {
		start, err := time.ParseInLocation("2006-01-02", interval.Start, location)
		if err != nil {
			continue
		}
		end, err := time.ParseInLocation("2006-01-02", interval.End, location)
		if err != nil {
			continue
		}
		for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
			total++
		}
	}
	return total
}

func importDayCovered(intervals []historicalImportInterval, day string) bool {
	for _, interval := range intervals {
		if day >= interval.Start && day <= interval.End {
			return true
		}
	}
	return false
}

// nextImportDay returns the first covered day after day, or "" when the merged
// interval list is exhausted.
func nextImportDay(intervals []historicalImportInterval, day string) string {
	for _, interval := range intervals {
		if day < interval.Start {
			return interval.Start
		}
		if day < interval.End {
			return importDayAfter(day)
		}
	}
	return ""
}

func importDayAfter(day string) string {
	parsed, err := time.ParseInLocation("2006-01-02", day, sourceLocation())
	if err != nil {
		return ""
	}
	return parsed.AddDate(0, 0, 1).Format("2006-01-02")
}

func importETASeconds(state historicalImportState) (int64, bool) {
	if state.DaysDone < minImportETADays || state.DaysTotal <= state.DaysDone || state.DaySecondsTotal <= 0 {
		return 0, false
	}
	average := state.DaySecondsTotal / int64(state.DaysDone)
	return average * int64(state.DaysTotal-state.DaysDone), true
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
		return historicalImportState{WorkspaceID: workspaceID, SchemaVersion: importSchemaVersion, Status: importStatusNotStarted}, true, nil
	}
	state.WorkspaceID = workspaceID
	if state.SchemaVersion != importSchemaVersion {
		// A pre-release or unreadable checkpoint carries no resumable position.
		return historicalImportState{WorkspaceID: workspaceID, SchemaVersion: importSchemaVersion, Status: importStatusNotStarted}, true, nil
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
	return p.writeHistoricalImportStateValue(ctx, workspaceID, state)
}

func (p *plugin) writeHistoricalImportStateValue(ctx context.Context, workspaceID string, state historicalImportState) error {
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

// importQueueEntry is the plugin-scope start-order queue.
type importQueueEntry struct {
	WorkspaceIDs []string `json:"workspace_ids"`
}

func (p *plugin) readImportQueue(ctx context.Context) ([]string, error) {
	host := p.Host()
	if host == nil {
		return nil, errors.New("host unavailable")
	}
	value, found, err := host.GetState(ctx, importQueueScope, importQueueID, importQueueKey)
	if err != nil || !found {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var queue importQueueEntry
	if err := json.Unmarshal(encoded, &queue); err != nil {
		return nil, err
	}
	return queue.WorkspaceIDs, nil
}

func (p *plugin) writeImportQueue(ctx context.Context, workspaceIDs []string) error {
	host := p.Host()
	if host == nil {
		return errors.New("host unavailable")
	}
	encoded, err := json.Marshal(importQueueEntry{WorkspaceIDs: workspaceIDs})
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return err
	}
	return host.SetState(ctx, importQueueScope, importQueueID, importQueueKey, value)
}

func containsImportWorkspace(queue []string, workspaceID string) bool {
	for _, candidate := range queue {
		if candidate == workspaceID {
			return true
		}
	}
	return false
}

func (p *plugin) appendImportQueueLocked(ctx context.Context, workspaceID string) error {
	queue, err := p.readImportQueue(ctx)
	if err != nil {
		return err
	}
	if containsImportWorkspace(queue, workspaceID) {
		return nil
	}
	return p.writeImportQueue(ctx, append(queue, workspaceID))
}

func (p *plugin) removeImportQueueEntryLocked(ctx context.Context, workspaceID string) error {
	queue, err := p.readImportQueue(ctx)
	if err != nil || !containsImportWorkspace(queue, workspaceID) {
		return err
	}
	kept := make([]string, 0, len(queue))
	for _, candidate := range queue {
		if candidate != workspaceID {
			kept = append(kept, candidate)
		}
	}
	return p.writeImportQueue(ctx, kept)
}

// resumeHistoricalImports starts the queue head after startup cleanup. A queue
// entry can outlive workspace access, so each entry is reauthorized first.
func (p *plugin) resumeHistoricalImports() {
	p.importMu.Lock()
	defer p.importMu.Unlock()
	p.startNextImportLocked()
}

func (p *plugin) startNextImportLocked() {
	if p.importShuttingDown || p.importActive != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.hostRead)
	defer cancel()
	queue, err := p.readImportQueue(ctx)
	if err != nil {
		log.Printf("session-cost import queue: %v", err)
		return
	}
	for len(queue) > 0 {
		workspaceID := queue[0]
		state, found, err := p.authorizedHistoricalImportState(ctx, workspaceID)
		if err != nil {
			log.Printf("session-cost import queue authorization: %v", err)
			return
		}
		if !found || (state.Status != importStatusQueued && state.Status != importStatusRunning && state.Status != importStatusPreparing) {
			queue = queue[1:]
			if err := p.writeImportQueue(ctx, queue); err != nil {
				log.Printf("session-cost import queue: %v", err)
			}
			continue
		}
		runID := state.RunID
		if runID == "" {
			runID = rand.Text()
			state.RunID = runID
			state.Status = importStatusRunning
			if err := p.writeHistoricalImportState(ctx, workspaceID, state, true); err != nil {
				log.Printf("session-cost import resume: %v", err)
				return
			}
		}
		runCtx, cancelRun := context.WithCancel(context.Background())
		p.importActive = &activeImportRun{workspaceID: workspaceID, runID: runID, cancel: cancelRun}
		go p.runHistoricalImportRun(runCtx, workspaceID, runID)
		return
	}
}
