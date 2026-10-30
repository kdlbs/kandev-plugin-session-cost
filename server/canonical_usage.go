package main

import (
	"context"
	"errors"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxCanonicalSnapshots   = 64
	maxCanonicalRestarts    = 2
	maxCanonicalPages       = 100
	canonicalReadTimeout    = 3 * time.Second
	reportErrorInvalidQuery = "invalid_query"
	reportErrorAccessDenied = "access_denied"
)

type canonicalSnapshot struct {
	Version    int
	Items      []pluginsdk.SessionUsageMeasurement
	Generation string
	Status     string
}

type canonicalProjection struct {
	Status     string
	Generation string
	Pending    bool
}

func projectionMetadata(info *pluginsdk.PageInfo) canonicalProjection {
	if info == nil {
		return canonicalProjection{}
	}
	return canonicalProjection{Status: info.ProjectionStatus, Generation: info.ProjectionGeneration, Pending: info.ProjectionPending}
}

func (v canonicalProjection) complete() bool {
	return !v.Pending && (v.Status == "ready" || (v.Status == "" && v.Generation == ""))
}

func (v canonicalProjection) readable() bool {
	return v.complete() || (v.Status == "ready" && v.Generation != "")
}

func (v canonicalProjection) preparing() canonicalProjection {
	v.Pending = true
	if v.Status == "ready" || v.Status == "" {
		v.Status = "preparing"
	}
	return v
}

func (p *plugin) canonicalReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := p.hostRead
	if budget <= 0 {
		budget = canonicalReadTimeout
	}
	return context.WithTimeout(ctx, budget)
}

// Generation tokens are opaque. A page sequence is usable only when every
// page describes the same completed selection; collection timestamps cannot
// establish projection freshness.
func (p *plugin) readCanonicalSelection(ctx context.Context, filter pluginsdk.SessionUsageFilter) ([]pluginsdk.SessionUsageMeasurement, canonicalProjection, error) {
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()
	for attempt := 0; ; attempt++ {
		items, projection, err := p.readCanonicalPublication(ctx, filter)
		if ctx.Err() != nil {
			return nil, projection.preparing(), ctx.Err()
		}
		if status.Code(err) != codes.FailedPrecondition {
			return items, projection, err
		}
		if attempt == maxCanonicalRestarts {
			return nil, projection.preparing(), err
		}
		// A publication changed. Discard every partial row and restart with
		// the identical filter and an empty cursor, within the same deadline.
	}
}

func (p *plugin) readCanonicalPublication(ctx context.Context, filter pluginsdk.SessionUsageFilter) ([]pluginsdk.SessionUsageMeasurement, canonicalProjection, error) {
	page := pluginsdk.Page{Limit: 200}
	seen := map[string]bool{"": true}
	var rows []pluginsdk.SessionUsageMeasurement
	var projection canonicalProjection
	first := true
	for pageIndex := 0; pageIndex < maxCanonicalPages; pageIndex++ {
		if err := ctx.Err(); err != nil {
			return nil, projection, err
		}
		items, info, err := p.Host().Usage().ListCanonical(ctx, filter, page)
		if err != nil {
			return nil, projection, err
		}
		current := projectionMetadata(info)
		if first {
			projection = current
			first = false
		} else if current != projection {
			return nil, current.preparing(), nil
		}
		if !current.readable() {
			return nil, current, nil
		}
		rows = append(rows, items...)
		if info == nil || !info.HasMore {
			return rows, projection, nil
		}
		if info.NextCursor == "" || seen[info.NextCursor] {
			return nil, projection, errors.New("canonical usage cursor did not advance")
		}
		seen[info.NextCursor] = true
		page.Cursor = info.NextCursor
	}
	return nil, projection.preparing(), errors.New("canonical usage page limit exceeded")
}

func (p *plugin) beginCanonicalRead() uint64 {
	p.canonicalMu.Lock()
	defer p.canonicalMu.Unlock()
	p.canonicalSerial++
	return p.canonicalSerial
}

// Keep request order only. Accounting amounts stay in the Host's erase-aware
// publication; neither plugin memory nor generic Host state owns a saved copy.
func (p *plugin) currentCanonicalRead(key string, serial uint64) bool {
	p.canonicalMu.Lock()
	defer p.canonicalMu.Unlock()
	if p.canonicalReads[key] > serial {
		return false
	}
	if p.canonicalReads == nil {
		p.canonicalReads = make(map[string]uint64)
	}
	p.canonicalReads[key] = serial
	if len(p.canonicalReads) > maxCanonicalSnapshots {
		var oldest string
		oldestSerial := ^uint64(0)
		for candidate, value := range p.canonicalReads {
			if candidate != key && value < oldestSerial {
				oldest, oldestSerial = candidate, value
			}
		}
		delete(p.canonicalReads, oldest)
	}
	return true
}

func (p *plugin) savedSessionUsage(ctx context.Context, workspaceID, taskID, sessionID string, warn, high float64) (*sessionCostResponse, bool) {
	response, found, _ := p.loadSavedSessionUsage(ctx, workspaceID, taskID, sessionID, warn, high)
	return response, found
}

func (p *plugin) loadSavedSessionUsage(ctx context.Context, workspaceID, taskID, sessionID string, warn, high float64) (*sessionCostResponse, bool, error) {
	if p.Host() == nil || workspaceID == "" || sessionID == "" {
		return nil, false, nil
	}
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()
	// Saved reads use no generic-state payload. Legacy state migration must
	// not block the independently authorized canonical publication.
	serial := p.beginCanonicalRead()
	key := stableUsageID(workspaceID, taskID, sessionID)
	items, projection, err := p.readCanonicalSelection(ctx, pluginsdk.SessionUsageFilter{WorkspaceID: workspaceID, TaskIDs: []string{taskID}, SessionIDs: []string{sessionID}})
	current := p.currentCanonicalRead(key, serial)
	switch status.Code(err) {
	case codes.PermissionDenied, codes.NotFound, codes.Unauthenticated:
		return nil, false, err
	case codes.InvalidArgument:
		response := p.canonicalReadFailure(sessionID, warn, high)
		response.ReportError = reportErrorInvalidQuery
		response.Error = "Kandev rejected the saved usage query"
		return response, true, nil
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled || errors.Is(ctx.Err(), context.Canceled) {
		return nil, false, context.Canceled
	}
	if err == nil && current && projection.readable() {
		if len(items) == 0 && projection.Status == "" && projection.Generation == "" {
			return nil, false, nil
		}
		response := p.canonicalSnapshotResponse(ctx, canonicalSnapshot{Items: items, Generation: projection.Generation, Status: projection.Status}, taskID, sessionID, warn, high)
		if !projection.complete() {
			response.CanonicalPending = true
			response.ProjectionPending = true
			response.Stale = true
			response.ReportState = reportStatePending
		}
		return response, true, nil
	}
	response := p.canonicalSnapshotResponse(ctx, canonicalSnapshot{}, taskID, sessionID, warn, high)
	response.Saved = false
	if status.Code(err) == codes.Unimplemented {
		response.Compatibility = "upgrade_required"
		response.ReportState = reportStateFailed
		response.ReportError = reportErrorUnavailable
		response.Stale = true
		response.Error = errUsageHistoryUnsupported.Error()
		return response, true, nil
	}
	response.CanonicalPending = true
	response.ProjectionPending = true
	response.Stale = true
	response.ReportState = reportStatePending
	response.ProjectionStatus = projection.Status
	response.PendingProjectionGeneration = projection.Generation
	if err != nil {
		response.Error = "saved usage is temporarily unavailable"
	}
	return response, true, nil
}

func (p *plugin) canonicalReadFailure(sessionID string, warn, high float64) *sessionCostResponse {
	response := responseForEntries(sessionID, "", nil, warn, high, p.now())
	response.CostKnown = false
	response.ReportState = reportStateFailed
	response.ReportError = reportErrorUnavailable
	return &response
}

// A database read that is preparing, unsupported, or invalid cannot be
// repaired by starting a source process, including on an explicit refresh.
func savedUsageReadOnly(response *sessionCostResponse) bool {
	return response != nil && (response.CanonicalPending || response.Compatibility == "upgrade_required" || response.ReportError == reportErrorInvalidQuery)
}

func canonicalAccessStatus(err error) int {
	switch status.Code(err) {
	case codes.PermissionDenied:
		return 403
	case codes.Unauthenticated:
		return 401
	case codes.NotFound:
		return 404
	default:
		return 0
	}
}

func (p *plugin) canonicalSnapshotResponse(ctx context.Context, snapshot canonicalSnapshot, taskID, sessionID string, warn, high float64) *sessionCostResponse {
	response := responseFromSaved(snapshot.Items, sessionID, warn, high)
	response.ProjectionStatus = snapshot.Status
	response.ProjectionGeneration = snapshot.Generation
	response.ReportState = reportStateReady
	if len(snapshot.Items) == 0 {
		response.CostKnown = false
	}
	if response.ACPSessionID == "" {
		response.ACPSessionID = p.resolveACPSessionID(ctx, taskID, sessionID)
	}
	return &response
}
