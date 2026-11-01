package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

// These explicit source declarations belong to a replacement adapter. They do
// not assert that Tokscale's dimension-only grouped rows prove the same IDs.
type declaredSegmentAdapter struct {
	rows []pluginsdk.UsageObservationV1
}

func (a declaredSegmentAdapter) observations(target pluginsdk.UsageCollectionTargetV1, _ *sessionCostResponse, coverage usageCoverage) ([]pluginsdk.UsageObservationV1, error) {
	rows := append([]pluginsdk.UsageObservationV1(nil), a.rows...)
	for i := range rows {
		rows[i].SubjectBinding = target.SubjectBinding
		rows[i].ItemID = stableUsageID("declared-fixture-item", rows[i].Segment.SegmentID, coverage.Date)
		if coverage.Date != "" {
			rows[i].Coverage = &pluginsdk.UsageCoverageV1{SourceDate: optionalString(coverage.Date), SourceTimezone: optionalString(coverage.Timezone)}
		}
	}
	return rows, nil
}

func declaredSegmentRows() []pluginsdk.UsageObservationV1 {
	var rows []pluginsdk.UsageObservationV1
	for m := 0; m < 2; m++ {
		for p := 0; p < 2; p++ {
			rows = append(rows, pluginsdk.UsageObservationV1{
				SourceRecordID: "shared-source-container", SourceVersion: "trusted-fixture-source-v1",
				Segment: &pluginsdk.UsageSegmentIdentityV1{DecompositionID: "source-partition-v1", SegmentID: fmt.Sprintf("opaque-member-%d-%d", m, p)},
				ModelID: optionalString(fmt.Sprintf("model-%d", m)), ModelProvider: optionalString(fmt.Sprintf("provider-%d", p)),
				Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(10)},
			})
		}
	}
	return rows
}

func TestReviewSegmentsAdapterPreservesDeclarationsCorrectionsAndPartialAbsence(t *testing.T) {
	var batches []pluginsdk.UsageObservationBatchV1
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		if batch.SchemaVersion != pluginsdk.UsageObservationSchemaVersionV2 {
			return pluginsdk.UsageObservationBatchResultV1{Status: pluginsdk.UsageReportStatusUnsupportedVersion}, nil
		}
		return acceptedBatch(batch), nil
	}})
	rows := declaredSegmentRows()
	p.adapter = declaredSegmentAdapter{rows}
	response := parsedUsage(t, `{"entries":[]}`)
	target := eligibleTestTarget()
	require.NoError(t, p.persistTargetUsage(context.Background(), target, response, usageCoverage{}))
	require.Len(t, batches, 1)
	require.Len(t, batches[0].Observations, 4, "two models by two providers remain four declared members even with a shared SourceRecordID")
	bySegment := make(map[string]pluginsdk.UsageObservationV1)
	for _, row := range batches[0].Observations {
		require.NotNil(t, row.Segment)
		bySegment[row.Segment.SegmentID] = row
		digest, err := row.ComputePayloadDigestV1()
		require.NoError(t, err)
		require.Equal(t, digest, row.PayloadDigest)
	}
	require.Len(t, bySegment, 4)
	// A downward correction with different dimensions retains explicit identity.
	corrected := rows[0]
	corrected.ModelID, corrected.ModelProvider = optionalString("renamed-model"), optionalString("corrected-provider")
	corrected.Tokens = &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(3)}
	p.adapter = declaredSegmentAdapter{[]pluginsdk.UsageObservationV1{corrected}}
	require.NoError(t, p.persistTargetUsage(context.Background(), target, response, usageCoverage{}))
	require.Len(t, batches, 2)
	write := batches[1].Observations[0]
	require.Equal(t, bySegment[corrected.Segment.SegmentID].ItemID, write.ItemID)
	require.Equal(t, int64(2), write.Revision)
	require.Equal(t, int64(3), *write.Tokens.InputTokens)
	require.Equal(t, corrected.Segment, write.Segment)
	require.Empty(t, batches[1].Tombstones)
	require.Nil(t, batches[1].CompleteSetRevision, "partial absence proves no retirement")
	p.Close()
	restarted := migrationPlugin(t, host)
	restarted.adapter = p.adapter
	require.NoError(t, restarted.persistTargetUsage(context.Background(), target, response, usageCoverage{}))
	require.Len(t, batches, 2, "accepted correction replay is suppressed after restart")
	// Views and bindings do not redefine the source's segment identities.
	require.NoError(t, restarted.persistTargetUsage(context.Background(), target, response, usageCoverage{Date: "2026-10-29", Timezone: "UTC"}))
	other := target
	other.SubjectBinding = "another-retained-subject"
	require.NoError(t, restarted.persistTargetUsage(context.Background(), other, response, usageCoverage{}))
	require.Equal(t, write.Segment, batches[2].Observations[0].Segment)
	require.Equal(t, write.Segment, batches[3].Observations[0].Segment)
	require.NotEqual(t, write.ItemID, batches[2].Observations[0].ItemID)
	require.Equal(t, write.ItemID, batches[3].Observations[0].ItemID)
	assertNoAccountingAmountsInState(t, host.fakeHost)
}

func TestReviewSegmentsUnsupportedSchemaNeverDowngradesUncertainReceipt(t *testing.T) {
	var batches []pluginsdk.UsageObservationBatchV1
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		return pluginsdk.UsageObservationBatchResultV1{Status: pluginsdk.UsageReportStatusUnsupportedVersion}, nil
	}})
	adapter := declaredSegmentAdapter{declaredSegmentRows()[:1]}
	p.adapter = adapter
	response := parsedUsage(t, `{"entries":[]}`)
	require.ErrorIs(t, p.persistTargetUsage(context.Background(), eligibleTestTarget(), response, usageCoverage{}), errUsageHistoryUnsupported)
	p.Close()
	restarted := migrationPlugin(t, host)
	restarted.adapter = adapter
	require.ErrorIs(t, restarted.persistTargetUsage(context.Background(), eligibleTestTarget(), response, usageCoverage{}), errUsageHistoryUnsupported)
	require.Len(t, batches, 2)
	require.Equal(t, pluginsdk.UsageObservationSchemaVersionV2, batches[0].SchemaVersion)
	require.Equal(t, batches[0], batches[1], "same declaration, revision and digest retry; no unsegmented downgrade")
	item := batches[0].Observations[0]
	var stored usageObservationCheckpoint
	found, err := restarted.readUsageState(context.Background(), "usage-observation-v1", stableUsageID(item.SubjectBinding, item.ItemID), &stored)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, stored.Accepted)
}

func TestReviewSegmentsOrdinaryTokscaleCannotInventProviderPartitions(t *testing.T) {
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"a","provider":"p1","input":1},{"sessionId":"transcript-1","model":"a","provider":"p2","input":2}]}`)
	calls := 0
	p, _ := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		calls++
		return acceptedBatch(batch), nil
	}})
	require.ErrorIs(t, p.persistTargetUsage(context.Background(), eligibleTestTarget(), response, usageCoverage{}), errTokscaleMembership)
	require.Zero(t, calls)
	// The explicit declaration fixture above separately preserves all four
	// proven members; ordinary pinned session grouping cannot produce these rows.
}

func TestReviewSegmentsRejectIncompleteDeclarationBeforeSubmission(t *testing.T) {
	for _, declaration := range []*pluginsdk.UsageSegmentIdentityV1{{SegmentID: "member"}, {DecompositionID: "partition"}} {
		calls := 0
		p, _ := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
			calls++
			return acceptedBatch(batch), nil
		}})
		row := declaredSegmentRows()[0]
		row.Segment = declaration
		p.adapter = declaredSegmentAdapter{[]pluginsdk.UsageObservationV1{row}}
		err := p.persistTargetUsage(context.Background(), eligibleTestTarget(), parsedUsage(t, `{"entries":[]}`), usageCoverage{})
		require.ErrorContains(t, err, "segment identifiers")
		require.Zero(t, calls)
	}
}

func TestReviewSegmentsSDKBrokerCarriesDeclarationAndExactDigest(t *testing.T) {
	request := make(chan pluginsdk.UsageObservationBatchV1, 1)
	p, host := historyPlugin(t, exactHistoryFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		request <- batch
		return acceptedBatch(batch), nil
	}})
	p.UnimplementedPlugin.SetHost(sdkBrokerHost(t, host))
	p.adapter = declaredSegmentAdapter{declaredSegmentRows()}
	require.NoError(t, p.persistTargetUsage(context.Background(), eligibleTestTarget(), parsedUsage(t, `{"entries":[]}`), usageCoverage{}))
	batch := <-request
	require.Equal(t, pluginsdk.UsageObservationSchemaVersionV2, batch.SchemaVersion)
	require.Len(t, batch.Observations, 4)
	for _, row := range batch.Observations {
		require.Equal(t, "source-partition-v1", row.Segment.DecompositionID)
		require.Contains(t, row.Segment.SegmentID, "opaque-member-")
		digest, err := row.ComputePayloadDigestV1()
		require.NoError(t, err)
		require.Equal(t, row.PayloadDigest, digest)
		without := row
		without.Segment = nil
		other, err := without.ComputePayloadDigestV1()
		require.NoError(t, err)
		require.NotEqual(t, digest, other, "segment declaration participates in the immutable payload digest")
	}
}
