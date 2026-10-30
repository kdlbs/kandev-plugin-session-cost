package main

import (
	"context"
	"errors"

	"github.com/kdlbs/kandev/pluginsdk"
)

// Every raw source path must obtain current supported transcript context.
// A saved snapshot grants neither current source access nor cross-scope reuse.
// The coordinator's source-context handoff must confirm the Host's supported
// context semantics before these paths count as integrated verification.
func (p *plugin) preflightSource(ctx context.Context, workspace, task, session, transcript string, binding ...string) error {
	_, err := p.currentSourceTarget(ctx, workspace, task, session, transcript, binding...)
	return err
}

func (p *plugin) currentSourceTarget(ctx context.Context, workspace, task, session, transcript string, binding ...string) (pluginsdk.UsageCollectionTargetV1, error) {
	if workspace == "" {
		row, err := p.Host().Tasks().Get(ctx, task)
		if err != nil {
			return pluginsdk.UsageCollectionTargetV1{}, err
		}
		if row == nil {
			return pluginsdk.UsageCollectionTargetV1{}, errors.New("source context is unavailable")
		}
		workspace = row.WorkspaceID
	}
	target, err := p.resolveCurrentUsageTarget(ctx, workspace, task, session, transcript, binding...)
	if err == nil && len(binding) > 0 && binding[0] != "" && target.SubjectBinding != binding[0] {
		return pluginsdk.UsageCollectionTargetV1{}, errUsageScopeChanged
	}
	return target, err
}

func (p *plugin) unavailableSource(saved *sessionCostResponse, session string, warn, high float64, cause ...error) *sessionCostResponse {
	response := p.canonicalReadFailure(session, warn, high)
	if saved != nil {
		copy := *saved
		response = &copy
		response.Stale = true
	}
	response.ReportState = reportStateFailed
	response.ReportError = "source_context"
	response.Compatibility = "source_context_unavailable"
	response.Error = "current retained source context is unavailable; saved usage remains authoritative"
	if len(cause) > 0 && errors.Is(cause[0], errTokscaleMembership) {
		response.ReportError = "source_membership"
		response.Compatibility = "source_membership_ambiguous"
		response.Error = errTokscaleMembership.Error()
	}
	if len(cause) > 0 && errors.Is(historyError(cause[0]), errUsageHistoryUnsupported) {
		response.ReportError = reportErrorUnavailable
		response.Compatibility = "upgrade_required"
		response.Error = errUsageHistoryUnsupported.Error()
	}
	if len(cause) > 0 && errors.Is(cause[0], errUsageTargetsPreparing) {
		response.ReportState = reportStatePending
		response.ReportError = ""
		response.Compatibility = "source_context_preparing"
		response.Error = ""
	}
	return response
}
