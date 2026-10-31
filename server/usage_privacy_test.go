package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUsageHistoryPrivacyTransferPreparingCannotReplayPriorOwner(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	transferred := false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if transferred {
			// The new owner may read this workspace, but has no publication yet.
			return nil, &pluginsdk.PageInfo{ProjectionStatus: "preparing", ProjectionPending: true}, nil
		}
		return canonicalFixture(101), canonicalReadyPage("old-org.publication-1", ""), nil
	}
	before, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Equal(t, int64(101), before.Input)
	transferred = true
	var runs atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) { runs.Add(1); return nil, nil }
	after, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.True(t, after.CanonicalPending)
	require.False(t, after.Found)
	require.False(t, after.Saved)
	require.False(t, after.CostKnown)
	require.Zero(t, after.Input)
	require.Empty(t, after.Models)
	require.Empty(t, after.ProjectionGeneration)
	require.Zero(t, runs.Load())
}

func TestUsageHistoryPrivacyAccountingAmountsNeverEnterHostState(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		return acceptedBatch(batch), nil
	}})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(101), canonicalReadyPage("published.1", ""), nil
	}
	_, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	observation := pluginsdk.UsageObservationV1{SubjectBinding: "binding", ItemID: "item", SourceRecordID: "record", Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(987654321)}}
	require.NoError(t, p.reportUsageObservations(context.Background(), []pluginsdk.UsageObservationV1{observation}))
	assertNoAccountingAmountsInState(t, host.fakeHost)
	// Erasure removes the accounting scope, not this generic plugin store.
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, nil, status.Error(codes.NotFound, "scope erased")
	}
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(host)
	t.Cleanup(restarted.Close)
	response, found := restarted.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.False(t, found)
	require.Nil(t, response)
	assertNoAccountingAmountsInState(t, host.fakeHost)
}

func TestUsageHistoryPrivacyLegacySnapshotsAreScrubbedWithoutReplay(t *testing.T) {
	p, host := historyPlugin(t, retainedProtocolFixture{})
	canonical, err := json.Marshal(canonicalSnapshot{Version: 1, Items: canonicalFixture(101), Generation: "erased-org.publication-1", Status: "ready"})
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "plugin", "canonical-usage-v1", "orphaned-key", map[string]any{"json": string(canonical)}))
	legacy := map[string]any{
		"ContentDigest": "content-digest", "Accepted": false,
		"Observation": pluginsdk.UsageObservationV1{SubjectBinding: "binding", ItemID: "item", SourceRecordID: "record", Revision: 4, PayloadDigest: "payload-digest", Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(987654321)}},
	}
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "plugin", "usage-observation-v1", "orphaned-receipt", map[string]any{"json": string(encoded)}))
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, &pluginsdk.PageInfo{ProjectionStatus: "preparing", ProjectionPending: true}, nil
	}
	p.SetHost(host)
	response, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.False(t, response.Found)
	require.False(t, response.Saved)
	assertNoAccountingAmountsInState(t, host.fakeHost)
	entries, err := host.ListState(context.Background(), "plugin", "usage-observation-v1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal([]byte(entries[0].Value["json"].(string)), &receipt))
	require.Equal(t, float64(4), receipt["Revision"])
	require.Equal(t, "payload-digest", receipt["PayloadDigest"])
	require.Equal(t, "content-digest", receipt["ContentDigest"])
	require.Equal(t, false, receipt["Accepted"])
}

func assertNoAccountingAmountsInState(t *testing.T, host *fakeHost) {
	t.Helper()
	canonical, err := host.ListState(context.Background(), "plugin", "canonical-usage-v1")
	require.NoError(t, err)
	require.Empty(t, canonical, "canonical accounting belongs in the Host's erase-aware retained store")
	receipts, err := host.ListState(context.Background(), "plugin", "usage-observation-v1")
	require.NoError(t, err)
	allowed := map[string]bool{"Version": true, "ContentDigest": true, "Revision": true, "PayloadDigest": true, "Accepted": true, "SubjectBinding": true, "ItemID": true}
	for _, entry := range receipts {
		var receipt map[string]any
		require.NoError(t, json.Unmarshal([]byte(entry.Value["json"].(string)), &receipt))
		for key := range receipt {
			require.True(t, allowed[key], "unexpected recoverable accounting field %q in receipt %q", key, entry.Key)
		}
	}
	memberships, err := host.ListState(context.Background(), "plugin", tokscaleMembershipStateID)
	require.NoError(t, err)
	for _, entry := range memberships {
		var value map[string]any
		require.NoError(t, json.Unmarshal([]byte(entry.Value["json"].(string)), &value))
		for key := range value {
			require.Contains(t, []string{"Version", "Members", "Quarantined"}, key, "membership stores no accounting payload or raw transcript/model")
		}
		var membership tokscaleMembershipCheckpoint
		require.NoError(t, json.Unmarshal([]byte(entry.Value["json"].(string)), &membership))
		require.LessOrEqual(t, len(membership.Members), maxTokscaleMembershipGroups)
		for _, member := range membership.Members {
			require.Regexp(t, `^sha256:[0-9a-f]{64}$`, member)
		}
	}
}

func TestUsageHistoryPrivacyLegacyUncertainReceiptPreservesExactReplay(t *testing.T) {
	var submitted []pluginsdk.UsageObservationV1
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		submitted = append(submitted, batch.Observations...)
		return acceptedBatch(batch), nil
	}})
	observation := pluginsdk.UsageObservationV1{SubjectBinding: "binding", ItemID: "item", SourceRecordID: "record", Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(987654321)}}
	content, err := observation.ComputePayloadDigestV1()
	require.NoError(t, err)
	observation.Revision = 4
	observation.PayloadDigest, err = observation.ComputePayloadDigestV1()
	require.NoError(t, err)
	legacy, err := json.Marshal(map[string]any{"ContentDigest": content, "Accepted": false, "Observation": observation})
	require.NoError(t, err)
	key := stableUsageID(observation.SubjectBinding, observation.ItemID)
	require.NoError(t, host.SetState(context.Background(), "plugin", "usage-observation-v1", key, map[string]any{"json": string(legacy)}))
	require.NoError(t, p.reportUsageObservations(context.Background(), []pluginsdk.UsageObservationV1{observation}))
	require.Equal(t, []pluginsdk.UsageObservationV1{observation}, submitted)
	assertNoAccountingAmountsInState(t, host.fakeHost)
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(host)
	t.Cleanup(restarted.Close)
	require.NoError(t, restarted.reportUsageObservations(context.Background(), []pluginsdk.UsageObservationV1{observation}))
	require.Len(t, submitted, 1, "the accepted receipt must suppress a repeated write after restart")
}
