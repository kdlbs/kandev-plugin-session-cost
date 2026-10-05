package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Close cancels all process-owned work before the signal handler exits.
func (p *plugin) Close() {
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
	saved, hasSaved := p.savedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high)
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
	if hasSaved && !refresh && !active && !failure && (snapshot == nil || !snapshot.generatedAt.After(savedAt)) {
		saved.ReportState = reportStateReady
		return saved, nil
	}
	acpID := p.resolveACPSessionID(ctx, taskID, sessionID)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("host unavailable")
	}
	response := responseForEntries(sessionID, acpID, nil, warn, high, p.now())
	response.ReportState = reportStateReady
	if acpID == "" {
		response.GeneratedAt = ""
		response.Tokscale.Installed = false
		return &response, nil
	}
	command, _ := cfg[configKeyCommand].(string)
	cmd := resolveCommand(strings.TrimSpace(command), p.lookPath)
	view := p.toolbar.Request(cmd, refresh)
	if view.snapshot != nil {
		response = responseForEntries(sessionID, acpID, view.snapshot.entries, warn, high, view.snapshot.generatedAt)
		response.Tokscale = view.snapshot.tokscale
	}
	if !response.Found && hasSaved {
		response = *saved
	}
	if view.state == reportStateReady && response.Found && !response.Saved && cfg[configKeyCollect] == true {
		if err := p.persistSessionUsage(ctx, workspaceID, taskID, sessionID, &response); err != nil {
			// A rejected revision must never be displayed as the accepted current value.
			response = responseForEntries(sessionID, acpID, nil, warn, high, p.now())
			response.Error = "usage was calculated but could not be accepted"
			response.Stale = true
		}
		if canonical, ok := p.savedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high); ok {
			canonical.Error = response.Error
			canonical.Stale = response.Stale
			response = *canonical
		}
	}
	response.ReportState = view.state
	response.ReportError = view.errCode
	response.Stale = response.Stale || view.stale
	response.Tokscale.Command = commandDisplay(cmd)
	response.Tokscale.Source = cmd.Source
	response.Tokscale.Installed = view.errCode != reportErrorUnavailable
	return &response, nil
}
