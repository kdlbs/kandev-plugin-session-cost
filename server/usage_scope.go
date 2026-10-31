package main

import (
	"context"
	"errors"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errUsageScopeChanged = errors.New("usage scope changed; cumulative transcript attribution is ambiguous")

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
		return historyError(err)
	}
	return validateTargetPage(query, upper, page)
}

func (p *plugin) authorizedHistoricalImportState(ctx context.Context, workspace string) (historicalImportState, bool, error) {
	state, found, err := p.loadHistoricalImportState(ctx, workspace)
	if err != nil || !found {
		return state, found, err
	}
	if preparingImportHasNoMembership(state) {
		// Preparation contains no former target IDs, dates, counts or samples.
		// Authorize an initial query in the caller's current workspace scope.
		reader, readErr := usageHistory(p.Host())
		if readErr != nil {
			return historicalImportState{}, false, readErr
		}
		query := pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspace, Limit: 1}
		page, readErr := reader.ListTargets(ctx, query)
		err = readErr
		if err == nil {
			err = validateTargetPage(query, 0, page)
		}
	} else {
		err = p.validateUsageSnapshot(ctx, workspace, state.SnapshotID, state.SnapshotUpperBound)
	}
	if err == nil {
		return state, true, nil
	}
	if !errors.Is(err, errUsageScopeChanged) && !usageScopeDenied(err) {
		return historicalImportState{}, false, err
	}
	// Delete only the checkpoint we validated; a newer authorized run wins.
	p.importStateMu.Lock()
	defer p.importStateMu.Unlock()
	current, exists, readErr := p.loadHistoricalImportState(ctx, workspace)
	if readErr != nil {
		return historicalImportState{}, false, readErr
	}
	if exists && current.RunID == state.RunID && current.SnapshotID == state.SnapshotID {
		if p.importRevoked == nil {
			p.importRevoked = make(map[string]bool)
		}
		p.importRevoked[stableUsageID(workspace, state.RunID)] = true
		if err := p.Host().DeleteState(ctx, importStateScope, workspace, importStateKey); err != nil {
			return historicalImportState{}, false, err
		}
	}
	return historicalImportState{}, false, nil
}

func preparingImportHasNoMembership(state historicalImportState) bool {
	return state.Status == importStatusPreparing && state.SchemaVersion == 2 && state.SnapshotID == "" && state.SnapshotUpperBound == 0 && state.Cursor == "" && len(state.PageTargets) == 0 && len(state.PageDates) == 0 && len(state.Dates) == 0 && len(state.PageFound) == 0 && len(state.PageMissingIDs) == 0 && state.Processed == 0 && state.Missing == 0 && len(state.MissingSessionIDs) == 0 && state.LastSuccessfulAt == ""
}

type historicalImportView struct {
	WorkspaceID         string `json:"workspace_id"`
	Status              string `json:"status"`
	Processed           int    `json:"processed"`
	Missing             int    `json:"missing"`
	DiagnosticsReliable bool   `json:"diagnostics_reliable"`
	LegacyDiagnostics   bool   `json:"legacy_diagnostics,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	LastSuccessfulAt    string `json:"last_successful_at,omitempty"`
	Undated             bool   `json:"undated"`
}

func publicImportView(state historicalImportState) historicalImportView {
	view := historicalImportView{WorkspaceID: state.WorkspaceID, Status: state.Status, Processed: state.Processed, Missing: state.Missing, DiagnosticsReliable: state.DiagnosticsReliable, LegacyDiagnostics: state.LegacyDiagnostics, LastSuccessfulAt: state.LastSuccessfulAt, Undated: state.Undated}
	if state.LastError != "" {
		view.LastError = "historical import failed; check collection compatibility and source availability"
	}
	return view
}
