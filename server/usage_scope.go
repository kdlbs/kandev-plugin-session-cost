package main

import (
	"context"
	"errors"
	"log"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errUsageScopeChanged = errors.New("usage scope changed; cumulative transcript attribution is ambiguous")

func invalidatesHistoricalImportScope(err error) bool {
	if errors.Is(err, errUsageScopeChanged) {
		return true
	}
	switch status.Code(err) {
	case codes.NotFound, codes.PermissionDenied:
		return true
	default:
		return false
	}
}

// Reauthorize task-scoped action reads against the Host's current binding in
// the verified workspace. Generic Host Usage RPCs are installation-scoped and
// do not carry the action caller, so a verified tuple alone is not enough to
// proxy a canonical read.
func (p *plugin) authorizeSessionUsageActionScope(ctx context.Context, workspaceID, taskID, sessionID string) error {
	if workspaceID == "" || taskID == "" || sessionID == "" {
		return status.Error(codes.PermissionDenied, "verified session usage scope is incomplete")
	}
	reader, err := usageHistory(p.Host())
	if err != nil {
		return err
	}
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()

	var cached retainedUsageTarget
	found, err := p.readUsageState(ctx, "usage-target-v1", targetCacheKey(workspaceID, taskID, sessionID), &cached)
	if err != nil {
		return err
	}
	selector := pluginsdk.UsageCollectionCurrentTargetQueryV1{TaskID: taskID, SessionID: sessionID}
	if found {
		if cached.Quarantined || cached.TaskID != taskID || cached.SessionID == nil || *cached.SessionID != sessionID || cached.SubjectBinding == "" {
			return status.Error(codes.PermissionDenied, "saved usage binding is no longer accessible")
		}
		selector.SubjectBinding = cached.SubjectBinding
	}
	query := pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspaceID, CurrentTarget: &selector}
	page, err := reader.ListTargets(ctx, query)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		if usageScopeDenied(err) {
			return status.Error(codes.PermissionDenied, "current usage binding is no longer accessible")
		}
		return historyError(err)
	}
	if err := validateCurrentTargetPage(page); err != nil {
		return err
	}
	if len(page.Targets) != 1 {
		return status.Error(codes.NotFound, "current session usage binding not found")
	}
	target := page.Targets[0]
	if target.TaskID != taskID || target.SessionID == nil || *target.SessionID != sessionID || target.SubjectBinding == "" ||
		(selector.SubjectBinding != "" && target.SubjectBinding != selector.SubjectBinding) {
		return status.Error(codes.PermissionDenied, "current session usage binding does not match the verified action scope")
	}
	return nil
}

func usageScopeDenied(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument:
		return true
	}
	return false
}

// An existing snapshot is source/workspace/captured-scope bound in the Host.
// Revalidating it does not refresh discovery or require a live task/session.
func (p *plugin) validateUsageSnapshot(ctx context.Context, workspace, snapshot string, upper int64) error {
	if snapshot == "" {
		return errUsageScopeChanged
	}
	reader, err := usageHistory(p.Host())
	if err != nil {
		return err
	}
	query := pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspace, SnapshotID: snapshot, Limit: 1}
	page, err := reader.ListTargets(ctx, query)
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return errUsageScopeChanged
		}
		return historyError(err)
	}
	return validateTargetPage(query, upper, page)
}

func (p *plugin) authorizedHistoricalImportState(ctx context.Context, workspace string) (historicalImportState, bool, error) {
	state, found, err := p.loadHistoricalImportState(ctx, workspace)
	if err != nil || !found {
		return state, found, err
	}
	if state.SnapshotID == "" {
		// No captured membership exists yet. The verified action context already
		// proves workspace access; discovery belongs to the worker so a status
		// poll cannot create or replace the snapshot the worker will persist.
		return state, true, nil
	}
	err = p.validateUsageSnapshot(ctx, workspace, state.SnapshotID, state.SnapshotUpperBound)
	if err == nil {
		return state, true, nil
	}
	if !invalidatesHistoricalImportScope(err) {
		return historicalImportState{}, false, err
	}
	p.discardHistoricalImportState(ctx, workspace, state)
	return historicalImportState{}, false, nil
}

// discardHistoricalImportState deletes only the checkpoint that failed
// authorization; a newer authorized run wins.
func (p *plugin) discardHistoricalImportState(ctx context.Context, workspace string, state historicalImportState) {
	p.importStateMu.Lock()
	defer p.importStateMu.Unlock()
	current, exists, err := p.loadHistoricalImportState(ctx, workspace)
	if err != nil {
		return
	}
	if exists && (current.RunID != state.RunID || current.SnapshotID != state.SnapshotID) {
		return
	}
	if p.importRevoked == nil {
		p.importRevoked = make(map[string]bool)
	}
	p.importRevoked[stableUsageID(workspace, state.RunID)] = true
	if err := p.Host().DeleteState(ctx, importStateScope, workspace, importStateKey); err != nil {
		log.Printf("session-cost historical import discard: %v", err)
	}
}

type historicalImportView struct {
	WorkspaceID      string `json:"workspace_id"`
	Status           string `json:"status"`
	Phase            string `json:"phase"`
	SessionsFound    int    `json:"sessions_found"`
	SessionsImported int    `json:"sessions_imported"`
	SessionsMissing  int    `json:"sessions_missing"`
	SessionsSkipped  int    `json:"sessions_skipped"`
	DaysTotal        int    `json:"days_total"`
	DaysDone         int    `json:"days_done"`
	CurrentDay       string `json:"current_day"`
	ETASeconds       int64  `json:"eta_seconds,omitempty"`
	LastSuccessfulAt string `json:"last_successful_at"`
	Undated          bool   `json:"undated"`
	LastError        string `json:"last_error"`
}

func publicImportView(state historicalImportState) historicalImportView {
	view := historicalImportView{
		WorkspaceID:      state.WorkspaceID,
		Status:           state.Status,
		Phase:            state.Phase,
		SessionsFound:    state.SessionsFound,
		SessionsImported: state.SessionsImported,
		SessionsMissing:  state.SessionsMissing,
		SessionsSkipped:  state.SessionsSkipped,
		DaysTotal:        state.DaysTotal,
		DaysDone:         state.DaysDone,
		LastSuccessfulAt: state.LastSuccessfulAt,
		Undated:          state.Undated,
	}
	if state.Phase == importPhaseDaily {
		view.CurrentDay = state.NextDay
	}
	if state.LastError != "" {
		view.LastError = historicalImportFailureMessage
	}
	if eta, ok := importETASeconds(state); ok {
		view.ETASeconds = eta
	}
	return view
}
