package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errUsageCollectionDisabled = errors.New("token statistics collection is disabled")

var errUsageHistoryUnsupported = errors.New("upgrade Kandev to a host that supports retained usage collection")

var errUsageSourceUnavailable = errors.New("the Host has not authorized this aggregate source context")

var errUsageTargetsPreparing = errors.New("retained usage targets are preparing")

const maxTargetPreparationSteps = 4

func eligibleUsageSource(target pluginsdk.UsageCollectionTargetV1) bool {
	return target.SourceScopeStatus == "eligible"
}

type usageAdapter interface {
	observations(pluginsdk.UsageCollectionTargetV1, *sessionCostResponse, usageCoverage) ([]pluginsdk.UsageObservationV1, error)
}

type tokscaleUsageAdapter struct{}

func stableUsageID(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
}

// Tokscale's grouped row is a source record, not evidence of a provider account
// or usage actor. Explicit source record IDs permit model corrections; grouped
// model rows without those IDs cannot prove that an omitted row was retired.
func (tokscaleUsageAdapter) observations(target pluginsdk.UsageCollectionTargetV1, response *sessionCostResponse, coverage usageCoverage) ([]pluginsdk.UsageObservationV1, error) {
	if !eligibleUsageSource(target) {
		return nil, errUsageSourceUnavailable
	}
	if _, _, err := tokscaleMembers(response); err != nil {
		return nil, err
	}
	var result []pluginsdk.UsageObservationV1
	rows := tokscaleSourceRows(response)
	seen := make(map[string]bool)
	for _, row := range rows {
		presence := presenceForEntry(row)
		provider := row.ModelProvider
		if provider == "" {
			provider = row.Provider
		}
		if ordinaryTokscaleGroup(row) && row.Provider != "" {
			provider = row.Provider
		}
		rawProvider := provider
		provider = singleSourceProvider(provider)
		recordID := row.SourceRecordID
		if recordID == "" {
			recordID = stableUsageID("tokscale-session-model", row.SessionID, row.Model)
		}
		itemID := stableUsageID("tokscale-v1", target.SubjectBinding, recordID, coverage.Date, coverage.Timezone)
		if seen[itemID] {
			return nil, errors.New("ambiguous duplicate tokscale source record; provider groups require distinct source record IDs")
		}
		seen[itemID] = true
		client := row.AgentClient
		if client == "" {
			client = row.Source
		}
		if ordinaryTokscaleGroup(row) {
			client = ""
		}
		item := pluginsdk.UsageObservationV1{
			SubjectBinding: target.SubjectBinding, ItemID: itemID, SourceRecordID: recordID, SourceVersion: "session-cost-tokscale-adapter-v1",
			ModelID: optionalString(row.Model), ModelProvider: optionalString(provider), AgentClient: optionalString(client),
			ServingPlatform: optionalString(row.ServingPlatform),
			Tokens: &pluginsdk.UsageTokenAmountsV1{
				InputTokens: ptrIf(presence.input, row.Input), OutputTokens: ptrIf(presence.output, row.Output),
				CacheReadTokens: ptrIf(presence.cacheRead, row.CacheRead), CacheWriteTokens: ptrIf(presence.cacheWrite, row.CacheWrite),
				ReasoningTokens: ptrIf(presence.reasoning, row.Reasoning), TotalTokens: presence.totalValue,
			},
			Quality: &pluginsdk.UsageObservationQualityV1{TokenCoverageComplete: presence.totalValue != nil && presence.input && presence.output && presence.cacheRead && presence.cacheWrite && presence.reasoning, CostCoverageComplete: presence.cost, Stale: response.Stale},
			Cost:    &pluginsdk.UsageCostV1{Basis: pluginsdk.UsageCostBasisUnknown},
		}
		if ordinaryTokscaleGroup(row) {
			item.Segment = tokscaleSegment(row.SessionID, row.Model)
		}
		if row.SupersedesRecordID != "" {
			previous := stableUsageID("tokscale-v1", target.SubjectBinding, row.SupersedesRecordID, coverage.Date, coverage.Timezone)
			item.SupersedesItemID = &previous
		}
		item.RawSourceLabels = make(map[string]string)
		for key, value := range map[string]string{"source": row.Source, "provider": rawProvider, "client": row.Client} {
			if value != "" {
				item.RawSourceLabels[key] = value
			}
		}
		if presence.cost {
			amount := row.costValue
			if amount == nil {
				var err error
				amount, err = parseUSDAmount(fmt.Sprintf("%.8f", row.Cost))
				if err != nil {
					return nil, err
				}
			}
			item.Cost = &pluginsdk.UsageCostV1{Amount: amount, Currency: optionalString("USD"), Basis: pluginsdk.UsageCostBasisEstimated}
		}
		if coverage.Date != "" {
			location, err := time.LoadLocation(coverage.Timezone)
			if err != nil {
				return nil, fmt.Errorf("source timezone: %w", err)
			}
			day, err := time.ParseInLocation("2006-01-02", coverage.Date, location)
			if err != nil {
				return nil, err
			}
			start, end := day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339)
			item.Coverage = &pluginsdk.UsageCoverageV1{SourceDate: optionalString(coverage.Date), SourceTimezone: optionalString(coverage.Timezone), CoverageStart: &start, CoverageEnd: &end}
		}
		if err := validateUsageAmounts(item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func parseUSDAmount(value string) (*int64, error) {
	amount, ok := new(big.Rat).SetString(value)
	if !ok || amount.Sign() < 0 {
		return nil, errors.New("invalid USD amount")
	}
	amount.Mul(amount, big.NewRat(10000, 1))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(amount.Num(), amount.Denom(), remainder)
	if remainder.Mul(remainder, big.NewInt(2)).Cmp(amount.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return nil, errors.New("USD amount exceeds int64")
	}
	return ptr(quotient.Int64()), nil
}

func validateUsageAmounts(item pluginsdk.UsageObservationV1) error {
	if item.SubjectBinding == "" || item.ItemID == "" || item.SourceRecordID == "" {
		return errors.New("usage observation requires retained binding and source identity")
	}
	if item.Segment != nil && (item.Segment.DecompositionID == "" || item.Segment.SegmentID == "") {
		return errors.New("source segment requires stable decomposition and segment identifiers")
	}
	if item.Tokens != nil {
		for _, value := range []*int64{item.Tokens.InputTokens, item.Tokens.OutputTokens, item.Tokens.CacheReadTokens, item.Tokens.CacheWriteTokens, item.Tokens.ReasoningTokens, item.Tokens.TotalTokens} {
			if value != nil && *value < 0 {
				return errors.New("negative usage token count")
			}
		}
	}
	return nil
}

func usageHistory(host pluginsdk.Host) (pluginsdk.UsageHistoryReader, error) {
	reader, ok := pluginsdk.HostUsageHistory(host)
	if !ok {
		return nil, errUsageHistoryUnsupported
	}
	return reader, nil
}

func historyError(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errUsageHistoryUnsupported
	}
	return err
}

func (p *plugin) readUsageState(ctx context.Context, id, key string, value any) (bool, error) {
	data, found, err := p.Host().GetState(ctx, "plugin", id, key)
	if err != nil || !found {
		return found, err
	}
	encoded, ok := data["json"].(string)
	if !ok {
		return false, errors.New("invalid retained usage checkpoint")
	}
	return true, json.Unmarshal([]byte(encoded), value)
}

func (p *plugin) writeUsageState(ctx context.Context, id, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// JSON stays a string so the Host's Struct transport cannot round int64s.
	return p.Host().SetState(ctx, "plugin", id, key, map[string]any{"json": string(encoded)})
}

func targetCacheKey(workspaceID, taskID, sessionID string) string {
	return stableUsageID("target-v1", workspaceID, taskID, sessionID)
}

type retainedUsageTarget struct {
	pluginsdk.UsageCollectionTargetV1
	SnapshotID         string
	SnapshotUpperBound int64
	Quarantined        bool
}

func (p *plugin) rememberUsageTarget(ctx context.Context, workspaceID string, target pluginsdk.UsageCollectionTargetV1, proof ...pluginsdk.UsageCollectionTargetPageV1) error {
	if target.SubjectBinding == "" {
		return errors.New("Host returned an empty retained usage binding")
	}
	if target.SessionID == nil {
		return nil
	}
	cached := retainedUsageTarget{UsageCollectionTargetV1: target}
	if len(proof) > 0 {
		cached.SnapshotID, cached.SnapshotUpperBound = proof[0].SnapshotID, proof[0].SnapshotUpperBound
	}
	return p.writeUsageState(ctx, "usage-target-v1", targetCacheKey(workspaceID, target.TaskID, *target.SessionID), cached)
}

func (p *plugin) resolveUsageTarget(ctx context.Context, workspaceID, taskID, sessionID, externalID string, expected ...string) (pluginsdk.UsageCollectionTargetV1, error) {
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()
	if workspaceID == "" || taskID == "" || sessionID == "" || externalID == "" {
		return pluginsdk.UsageCollectionTargetV1{}, errUsageSourceUnavailable
	}
	reader, err := usageHistory(p.Host())
	if err != nil {
		return pluginsdk.UsageCollectionTargetV1{}, err
	}
	var cached retainedUsageTarget
	found, err := p.readUsageState(ctx, "usage-target-v1", targetCacheKey(workspaceID, taskID, sessionID), &cached)
	if err != nil {
		return pluginsdk.UsageCollectionTargetV1{}, err
	}
	previous := ""
	if found && cached.TaskID == taskID && cached.SessionID != nil && *cached.SessionID == sessionID && cached.ExternalSessionID != nil && *cached.ExternalSessionID == externalID {
		if cached.Quarantined {
			return pluginsdk.UsageCollectionTargetV1{}, errUsageScopeChanged
		}
		previous = cached.SubjectBinding
	}
	if len(expected) > 0 && expected[0] != "" {
		if previous != "" && previous != expected[0] {
			return pluginsdk.UsageCollectionTargetV1{}, errUsageScopeChanged
		}
		previous = expected[0]
	}
	quarantine := func() error {
		// Keep the old binding and exact transcript fence. Never retry the same
		// cumulative source with an unconstrained query after revocation or ID reuse.
		value := retainedUsageTarget{UsageCollectionTargetV1: pluginsdk.UsageCollectionTargetV1{
			SubjectBinding: previous, TaskID: taskID, SessionID: optionalString(sessionID), ExternalSessionID: optionalString(externalID),
		}, Quarantined: true}
		if err := p.writeUsageState(ctx, "usage-target-v1", targetCacheKey(workspaceID, taskID, sessionID), value); err != nil {
			return err
		}
		return errUsageScopeChanged
	}
	query := pluginsdk.UsageCollectionTargetQueryV1{WorkspaceID: workspaceID,
		CurrentTarget: &pluginsdk.UsageCollectionCurrentTargetQueryV1{TaskID: taskID, SessionID: sessionID, ExternalSessionID: externalID, SubjectBinding: previous},
	}
	page, err := reader.ListTargets(ctx, query)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return pluginsdk.UsageCollectionTargetV1{}, ctxErr
	}
	if err != nil {
		if previous != "" && usageScopeDenied(err) {
			return pluginsdk.UsageCollectionTargetV1{}, quarantine()
		}
		return pluginsdk.UsageCollectionTargetV1{}, historyError(err)
	}
	if err := validateCurrentTargetPage(page); err != nil {
		return pluginsdk.UsageCollectionTargetV1{}, err
	}
	if len(page.Targets) == 0 {
		if previous != "" {
			return pluginsdk.UsageCollectionTargetV1{}, quarantine()
		}
		return pluginsdk.UsageCollectionTargetV1{}, errUsageSourceUnavailable
	}
	target := page.Targets[0]
	if target.TaskID != taskID || target.SessionID == nil || *target.SessionID != sessionID || target.SubjectBinding == "" || (previous != "" && target.SubjectBinding != previous) {
		if previous != "" {
			return pluginsdk.UsageCollectionTargetV1{}, quarantine()
		}
		return pluginsdk.UsageCollectionTargetV1{}, status.Error(codes.FailedPrecondition, "Host current target does not match the exact subject constraints")
	}
	if !eligibleUsageSource(target) || target.ExternalSessionID == nil || *target.ExternalSessionID != externalID {
		return pluginsdk.UsageCollectionTargetV1{}, errUsageSourceUnavailable
	}
	if err := p.rememberUsageTarget(ctx, workspaceID, target); err != nil {
		return pluginsdk.UsageCollectionTargetV1{}, err
	}
	return target, nil
}

func (p *plugin) resolveCurrentUsageTarget(ctx context.Context, workspaceID, taskID, sessionID, externalID string, expected ...string) (pluginsdk.UsageCollectionTargetV1, error) {
	return p.resolveUsageTarget(ctx, workspaceID, taskID, sessionID, externalID, expected...)
}

// The SDK enforces this shape over gRPC; also enforce it for in-process Hosts.
// An older Host must not silently substitute full archive enumeration.
func validateCurrentTargetPage(page pluginsdk.UsageCollectionTargetPageV1) error {
	if !page.CurrentTarget {
		return errUsageHistoryUnsupported
	}
	if len(page.Targets) > 1 || page.SnapshotID != "" || page.SnapshotUpperBound != 0 || page.NextCursor != "" || page.HasMore || page.Preparing {
		return status.Error(codes.FailedPrecondition, "Host returned enumeration state for current target lookup")
	}
	return nil
}

// Preparing commits one bounded Host metadata page, not frozen membership.
// Repeat only the identical initial query, within one short Host budget and
// a finite per-call work cap. Callers may resume preparation on a later pass.
func (p *plugin) readUsageTargetPage(ctx context.Context, reader pluginsdk.UsageHistoryReader, query pluginsdk.UsageCollectionTargetQueryV1, upper int64) (pluginsdk.UsageCollectionTargetPageV1, error) {
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()
	for step := 0; step < maxTargetPreparationSteps; step++ {
		if err := ctx.Err(); err != nil {
			return pluginsdk.UsageCollectionTargetPageV1{}, err
		}
		page, err := reader.ListTargets(ctx, query)
		if err != nil {
			return pluginsdk.UsageCollectionTargetPageV1{}, historyError(err)
		}
		if err := validateTargetPage(query, upper, page); err != nil {
			return pluginsdk.UsageCollectionTargetPageV1{}, err
		}
		if !page.Preparing {
			return page, nil
		}
	}
	return pluginsdk.UsageCollectionTargetPageV1{}, errUsageTargetsPreparing
}

func validateTargetPage(query pluginsdk.UsageCollectionTargetQueryV1, upper int64, page pluginsdk.UsageCollectionTargetPageV1) error {
	if page.Preparing {
		if query.SnapshotID != "" || query.Cursor != "" || page.SnapshotID != "" || page.SnapshotUpperBound != 0 || len(page.Targets) != 0 || page.NextCursor != "" || page.HasMore {
			return errors.New("Host returned membership during usage target preparation")
		}
		return nil
	}
	if page.SnapshotID == "" || (query.SnapshotID != "" && (query.SnapshotID != page.SnapshotID || upper != page.SnapshotUpperBound)) {
		return errors.New("Host changed the usage target snapshot")
	}
	if page.HasMore && (page.NextCursor == "" || page.NextCursor == query.Cursor || len(page.Targets) == 0) {
		return errors.New("Host returned a non-advancing usage target cursor")
	}
	if len(page.Targets) > int(query.Limit) {
		return errors.New("Host exceeded usage target page limit")
	}
	for _, target := range page.Targets {
		if target.SubjectBinding == "" {
			return errors.New("Host returned an empty usage binding")
		}
	}
	return nil
}

type usageObservationCheckpoint struct {
	Version        int
	ContentDigest  string
	SubjectBinding string
	ItemID         string
	Revision       int64
	PayloadDigest  string
	Accepted       bool
}

func (p *plugin) persistTargetUsage(ctx context.Context, target pluginsdk.UsageCollectionTargetV1, response *sessionCostResponse, coverage usageCoverage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.collectionEnabled(ctx) {
		return errUsageCollectionDisabled
	}
	if !eligibleUsageSource(target) {
		return errUsageSourceUnavailable
	}
	adapter := p.adapter
	if adapter == nil {
		adapter = tokscaleUsageAdapter{}
	}
	observations, err := adapter.observations(target, response, coverage)
	if err != nil {
		return err
	}
	for _, item := range observations {
		if item.SubjectBinding != target.SubjectBinding {
			return errors.New("adapter changed the retained usage target")
		}
	}
	if err := p.ensureUsageStatePrivacy(ctx); err != nil {
		return err
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	switch adapter.(type) {
	case tokscaleUsageAdapter, *tokscaleUsageAdapter:
		if err := p.guardTokscaleMembershipLocked(ctx, response, coverage, true); err != nil {
			return err
		}
	}
	return p.reportUsageObservationsLocked(ctx, observations)
}

func (p *plugin) reportUsageObservations(ctx context.Context, observations []pluginsdk.UsageObservationV1) error {
	if err := p.ensureUsageStatePrivacy(ctx); err != nil {
		return err
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	return p.reportUsageObservationsLocked(ctx, observations)
}

func (p *plugin) reportUsageObservationsLocked(ctx context.Context, observations []pluginsdk.UsageObservationV1) error {
	reader, err := usageHistory(p.Host())
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, item := range observations {
		if err := validateUsageAmounts(item); err != nil {
			return err
		}
		key := item.ItemID
		if seen[key] {
			return errors.New("duplicate usage observation item")
		}
		seen[key] = true
	}
	// Stable ordering makes an interrupted multi-batch report replay predictably.
	sort.Slice(observations, func(i, j int) bool { return observations[i].ItemID < observations[j].ItemID })
	for start := 0; start < len(observations); start += pluginsdk.MaxUsageObservationBatchSizeV1 {
		end := start + pluginsdk.MaxUsageObservationBatchSizeV1
		if end > len(observations) {
			end = len(observations)
		}
		var pending []pluginsdk.UsageObservationV1
		states := make(map[string]usageObservationCheckpoint)
		for _, draft := range observations[start:end] {
			draft.Revision = 0
			draft.PayloadDigest = ""
			content, err := draft.ComputePayloadDigestV1()
			if err != nil {
				return err
			}
			key := stableUsageID(draft.SubjectBinding, draft.ItemID)
			var state usageObservationCheckpoint
			found, err := p.readUsageState(ctx, "usage-observation-v1", key, &state)
			if err != nil {
				return err
			}
			if found && state.ContentDigest == content {
				if state.Accepted {
					continue
				}
				draft.Revision = state.Revision
				draft.PayloadDigest = state.PayloadDigest
			} else {
				revision := int64(1)
				if found {
					if state.Revision == math.MaxInt64 {
						return errors.New("usage revision exhausted")
					}
					revision = state.Revision + 1
				}
				draft.Revision = revision
				draft.PayloadDigest, err = draft.ComputePayloadDigestV1()
				if err != nil {
					return err
				}
				state = usageObservationCheckpoint{Version: 2, ContentDigest: content, SubjectBinding: draft.SubjectBinding, ItemID: draft.ItemID, Revision: draft.Revision, PayloadDigest: draft.PayloadDigest}
				if err := p.writeUsageState(ctx, "usage-observation-v1", key, state); err != nil {
					return err
				}
			}
			states[draft.ItemID] = state
			pending = append(pending, draft)
		}
		if len(pending) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !p.collectionEnabled(ctx) {
			return errUsageCollectionDisabled
		}
		schemaVersion := pluginsdk.UsageObservationSchemaVersionV1
		for _, item := range pending {
			if item.Segment != nil {
				schemaVersion = pluginsdk.UsageObservationSchemaVersionV2
				break
			}
		}
		result, err := reader.Report(ctx, pluginsdk.UsageObservationBatchV1{SchemaVersion: schemaVersion, Observations: pending})
		if err != nil {
			return historyError(err)
		}
		if result.Status == pluginsdk.UsageReportStatusUnsupportedVersion {
			return errUsageHistoryUnsupported
		}
		if result.Status != pluginsdk.UsageReportStatusAccepted && result.Status != pluginsdk.UsageReportStatusPartial {
			return fmt.Errorf("invalid usage report status %q", result.Status)
		}
		if len(result.Results) != len(pending) {
			return errors.New("usage report result count does not match observations")
		}
		reported := make(map[string]bool)
		var rejected string
		for _, receipt := range result.Results {
			state, ok := states[receipt.ItemID]
			if !ok || reported[receipt.ItemID] || receipt.Operation != "observation" {
				return errors.New("usage report returned an unexpected item result")
			}
			reported[receipt.ItemID] = true
			switch receipt.Status {
			case pluginsdk.UsageObservationStatusAccepted, pluginsdk.UsageObservationStatusUnchanged:
				if receipt.CurrentRevision != state.Revision || receipt.CurrentDigest != state.PayloadDigest {
					return errors.New("usage acceptance receipt does not match submitted revision")
				}
				state.Accepted = true
			case pluginsdk.UsageObservationStatusUnsupportedVersion:
				return errUsageHistoryUnsupported
			default:
				rejected = receipt.Status
				if receipt.CurrentObservation != nil && receipt.CurrentObservation.SubjectBinding == state.SubjectBinding && receipt.CurrentObservation.ItemID == receipt.ItemID && receipt.CurrentRevision >= state.Revision {
					current := *receipt.CurrentObservation
					digest, err := current.ComputePayloadDigestV1()
					if err == nil && digest == current.PayloadDigest && digest == receipt.CurrentDigest && current.Revision == receipt.CurrentRevision {
						state.Revision = current.Revision
						state.PayloadDigest = current.PayloadDigest
						current.Revision = 0
						current.PayloadDigest = ""
						state.ContentDigest, _ = current.ComputePayloadDigestV1()
						state.Accepted = true
					}
				}
			}
			key := stableUsageID(state.SubjectBinding, state.ItemID)
			if err := p.writeUsageState(ctx, "usage-observation-v1", key, state); err != nil {
				return err
			}
		}
		if rejected != "" {
			return &usageWriteRejectedError{status: rejected, results: result.Results}
		}
	}
	return nil
}
