package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kdlbs/kandev/pluginsdk"
)

// v4.15.1 GroupBy::Session keys on the exported (session_id, model) only.
// Model is already normalized by the producer; do not normalize it again.
const tokscaleSessionModelDecomposition = "tokscale-session-model-v1"
const tokscaleMembershipStateID = "tokscale-membership-v1"
const maxTokscaleMembershipGroups = 1024

var errTokscaleMembership = errors.New("tokscale group membership is ambiguous; previously seen members require retirement proof")

type tokscaleMembershipCheckpoint struct {
	Version     int
	Members     []string
	Quarantined bool
}

func tokscaleSegment(producerSession, model string) *pluginsdk.UsageSegmentIdentityV1 {
	return &pluginsdk.UsageSegmentIdentityV1{DecompositionID: tokscaleSessionModelDecomposition, SegmentID: stableUsageID(tokscaleSessionModelDecomposition, producerSession, model)}
}

func tokscaleSourceRows(response *sessionCostResponse) []sessionModelEntry {
	return response.sourceEntries
}

func ordinaryTokscaleGroup(row sessionModelEntry) bool {
	return row.SourceRecordID == "" && row.SupersedesRecordID == ""
}

func tokscaleMembers(response *sessionCostResponse) ([]string, bool, error) {
	rows := tokscaleSourceRows(response)
	if rows == nil && len(response.Models) > 0 {
		// A rendered models-only DTO cannot reconstruct exact producer keys.
		return nil, true, errUsageSourceUnavailable
	}
	seen := make(map[string]bool)
	ordinary, explicit := len(rows) == 0, false
	for _, row := range rows {
		if !ordinaryTokscaleGroup(row) {
			explicit = true
			continue
		}
		ordinary = true
		if row.Model == "" || row.SessionID == "" || response.ACPSessionID == "" {
			return nil, true, errTokscaleMembership
		}
		member := tokscaleSegment(row.SessionID, row.Model).SegmentID
		if seen[member] {
			return nil, true, fmt.Errorf("%w: ambiguous duplicate producer session/model group", errTokscaleMembership)
		}
		if len(seen) == maxTokscaleMembershipGroups {
			return nil, true, errTokscaleMembership
		}
		seen[member] = true
	}
	if ordinary && explicit {
		return nil, true, errTokscaleMembership
	}
	if !ordinary {
		return nil, false, nil
	}
	members := make([]string, 0, len(seen))
	for member := range seen {
		members = append(members, member)
	}
	sort.Strings(members)
	return members, true, nil
}

func tokscaleMembershipKey(transcript string, coverage usageCoverage) string {
	return stableUsageID(tokscaleSessionModelDecomposition, transcript, coverage.Date, coverage.Timezone)
}

// Caller holds usageMu through this reservation and all resulting reports.
// Reserve before any write: lost/partial receipts cannot erase pending members.
// The fixed-size set contains hashes only, never amounts or transcript content.
func (p *plugin) guardTokscaleMembershipLocked(ctx context.Context, response *sessionCostResponse, coverage usageCoverage, reserve bool) error {
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	members, ordinary, err := tokscaleMembers(response)
	if err != nil || !ordinary {
		return err
	}
	if response.ACPSessionID == "" {
		return errTokscaleMembership
	}
	key := tokscaleMembershipKey(response.ACPSessionID, coverage)
	var state tokscaleMembershipCheckpoint
	found, err := p.readUsageState(ctx, tokscaleMembershipStateID, key, &state)
	if err != nil {
		return err
	}
	if found && (state.Version != 1 || len(state.Members) > maxTokscaleMembershipGroups) {
		return errTokscaleMembership
	}
	present := make(map[string]bool, len(members))
	for _, member := range members {
		present[member] = true
	}
	for _, old := range state.Members {
		if !present[old] {
			// Do not add rejected new names: restoring the previous set must
			// recover, including when an alias was changed and then restored.
			if !state.Quarantined {
				state.Quarantined = true
				if err := p.writeUsageState(ctx, tokscaleMembershipStateID, key, state); err != nil {
					return err
				}
			}
			return errTokscaleMembership
		}
	}
	if reserve && len(members) > 0 {
		return p.writeUsageState(ctx, tokscaleMembershipStateID, key, tokscaleMembershipCheckpoint{Version: 1, Members: members})
	}
	return nil
}

func (p *plugin) checkTokscaleSnapshot(ctx context.Context, response *sessionCostResponse) error {
	return p.checkTokscaleSnapshotCoverage(ctx, response, usageCoverage{})
}

func (p *plugin) checkTokscaleSnapshotCoverage(ctx context.Context, response *sessionCostResponse, coverage usageCoverage) error {
	if p.adapter != nil {
		switch p.adapter.(type) {
		case tokscaleUsageAdapter, *tokscaleUsageAdapter:
		default:
			return nil
		}
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	return p.guardTokscaleMembershipLocked(ctx, response, coverage, false)
}

func singleSourceProvider(raw string) string {
	if strings.Contains(raw, ",") {
		return ""
	}
	return raw
}

// Only the recognized pinned invocation supplies the verified total rule.
// Partial fields and an overflowing exact sum remain unknown, not zero/max.
func markPinnedTokscaleTotals(rows []sessionModelEntry, cmd resolvedCommand) {
	if len(cmd.Argv) != 3 || cmd.Argv[0] != "npx" || cmd.Argv[1] != "-y" || cmd.Argv[2] != pinnedTokscale {
		return
	}
	for i := range rows {
		row := &rows[i]
		if !ordinaryTokscaleGroup(*row) || row.totalPresent || !row.inputPresent || !row.outputPresent || !row.cacheReadPresent || !row.cacheWritePresent || !row.reasoningPresent {
			continue
		}
		total, valid := int64(0), true
		for _, amount := range []int64{row.Input, row.Output, row.CacheRead, row.CacheWrite, row.Reasoning} {
			if amount < 0 || total > math.MaxInt64-amount {
				valid = false
				break
			}
			total += amount
		}
		if valid {
			row.totalPresent, row.totalValue = true, ptr(total)
		}
	}
}
