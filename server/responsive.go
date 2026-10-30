package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Close cancels all process-owned work before the signal handler exits.
func (p *plugin) Close() {
	p.usageCleanupMu.Lock()
	p.usageCleanupClosed = true
	if p.usageCleanupCancel != nil {
		p.usageCleanupCancel()
	}
	p.usageCleanupMu.Unlock()
	p.collectorMu.Lock()
	if p.collectorCancel != nil {
		p.collectorCancel()
	}
	p.collectorMu.Unlock()
	p.importMu.Lock()
	if p.importCancel != nil {
		p.importCancel()
	}
	p.importMu.Unlock()
	if p.toolbar != nil {
		p.toolbar.Close()
	}
	p.reportMu.Lock()
	reports := p.reports
	p.reportMu.Unlock()
	if reports != nil {
		reports.Close()
	}
}

// Host calls have one short budget. Reports retain the upstream process-owned
// pending/ready/failed contract and share the collector's serialized runner.
func (p *plugin) responsiveSessionUsage(ctx context.Context, workspaceID, taskID, sessionID string, refresh bool) (*sessionCostResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, p.hostRead)
	defer cancel()
	host := p.Host()
	if host == nil {
		return nil, fmt.Errorf("host unavailable")
	}
	cfg, err := host.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("host unavailable")
	}
	warn := positiveFloatOr(cfg[configKeyWarnThreshold], defaultWarnThreshold)
	high := positiveFloatOr(cfg[configKeyHighThreshold], defaultHighThreshold)
	if high < warn {
		high = warn
	}
	saved, hasSaved, err := p.loadSavedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high)
	if err != nil {
		return nil, err
	}
	if hasSaved && savedUsageReadOnly(saved) {
		return saved, nil
	}
	// Until current source homogeneity is authoritative, disabled collection
	// keeps an existing canonical selection even on a forced refresh.
	if hasSaved && saved.Found && cfg[configKeyCollect] != true {
		return saved, nil
	}
	p.toolbar.mu.Lock()
	active := p.toolbar.active
	failure := p.toolbar.failure != nil
	snapshot := p.toolbar.snapshot
	p.toolbar.mu.Unlock()
	savedAt, _ := time.Parse(time.RFC3339Nano, func() string {
		if saved != nil {
			return saved.LastRefresh
		}
		return ""
	}())
	if hasSaved && (saved.Found || saved.CanonicalPending) && !refresh && !active && (saved.CanonicalPending || (!failure && (snapshot == nil || !snapshot.generatedAt.After(savedAt)))) {
		if saved.CanonicalPending {
			saved.ReportState = reportStatePending
		} else {
			saved.ReportState = reportStateReady
		}
		return saved, nil
	}
	acpID := p.resolveACPSessionID(ctx, taskID, sessionID)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("host unavailable")
	}
	response := responseForEntries(sessionID, acpID, nil, warn, high, p.now())
	response.ReportState = reportStateReady
	if acpID == "" {
		if hasSaved && (saved.Found || saved.CanonicalPending) {
			saved.Stale = true
			if !saved.CanonicalPending {
				saved.ReportState = reportStateFailed
				saved.ReportError = reportErrorUnavailable
			}
			return saved, nil
		}
		response.GeneratedAt = ""
		response.Tokscale.Installed = false
		return &response, nil
	}
	if err := p.preflightSource(ctx, workspaceID, taskID, sessionID, acpID); err != nil {
		return p.unavailableSource(saved, sessionID, warn, high, err), nil
	}
	command, _ := cfg[configKeyCommand].(string)
	cmd := resolveCommand(strings.TrimSpace(command), p.lookPath)
	view := p.toolbar.Request(cmd, refresh)
	if view.snapshot != nil {
		response = responseForEntries(sessionID, acpID, view.snapshot.entries, warn, high, view.snapshot.generatedAt)
		response.Tokscale = view.snapshot.tokscale
		if err := p.checkTokscaleSnapshot(ctx, &response); err != nil {
			return p.unavailableSource(saved, sessionID, warn, high, err), nil
		}
	}
	if !response.Found && hasSaved {
		response = *saved
	}
	if cfg[configKeyCollect] == true {
		if view.state == reportStateReady && response.Found && !response.Saved {
			writeErr := p.persistSessionUsage(ctx, workspaceID, taskID, sessionID, &response)
			response = *p.canonicalUsageAfterWrite(ctx, workspaceID, taskID, sessionID, &response, saved, writeErr)
		} else if hasSaved {
			response = *saved
		} else {
			response = responseForEntries(sessionID, acpID, nil, warn, high, p.now())
			response.CostKnown = false
		}
	}
	// Source readiness does not establish successful Host admission/readback.
	// Keep a failed saved result and its error code through the action response.
	if response.ReportState != reportStateFailed {
		response.ReportState = view.state
		response.ReportError = view.errCode
	}
	if response.CanonicalPending {
		response.ReportState = reportStatePending
	}
	response.Stale = response.Stale || view.stale
	response.Tokscale.Command = commandDisplay(cmd)
	response.Tokscale.Source = cmd.Source
	response.Tokscale.Installed = view.errCode != reportErrorUnavailable
	return &response, nil
}
