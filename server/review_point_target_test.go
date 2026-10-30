package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Deliberately bypass the ordinary supported-Host fixture's page helpers:
// these tests exercise missing acknowledgement and invalid transport replies.
type exactHistoryFixture struct {
	list   func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error)
	report func(context.Context, pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error)
}

func (f exactHistoryFixture) ListTargets(ctx context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
	return f.list(ctx, query)
}

func (f exactHistoryFixture) Report(ctx context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
	if f.report != nil {
		return f.report(ctx, batch)
	}
	return acceptedBatch(batch), nil
}

func pointTargetPage(target pluginsdk.UsageCollectionTargetV1) pluginsdk.UsageCollectionTargetPageV1 {
	return pluginsdk.UsageCollectionTargetPageV1{CurrentTarget: true, Targets: []pluginsdk.UsageCollectionTargetV1{target}}
}

func TestReviewPointLookupNeverEnumeratesYearsOfTargets(t *testing.T) {
	var mu sync.Mutex
	var queries []pluginsdk.UsageCollectionTargetQueryV1
	var reports atomic.Int32
	p, host := historyPlugin(t, exactHistoryFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		mu.Lock()
		queries = append(queries, query)
		mu.Unlock()
		if query.CurrentTarget == nil {
			return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.ResourceExhausted, "a million-target years archive must not be enumerated for current eligibility")
		}
		return pointTargetPage(eligibleTestTarget()), nil
	}, report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		reports.Add(1)
		return acceptedBatch(batch), nil
	}})
	bridge := sdkBrokerHost(t, host)
	p.UnimplementedPlugin.SetHost(bridge)
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":7}]}`)
	for process := 0; process < 3; process++ {
		require.NoError(t, p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response))
		p.Close()
		p = migrationPlugin(t, bridge)
	}
	require.Equal(t, int32(1), reports.Load(), "accepted replay remains idempotent after current eligibility checks")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, queries, 3, "one bounded lookup per request, independent of archive size and restart")
	for i, query := range queries {
		require.Equal(t, "ws", query.WorkspaceID)
		require.Empty(t, query.SnapshotID)
		require.Empty(t, query.Cursor)
		require.NotNil(t, query.CurrentTarget)
		require.Equal(t, "task-1", query.CurrentTarget.TaskID)
		require.Equal(t, "session-1", query.CurrentTarget.SessionID)
		require.Equal(t, "transcript-1", query.CurrentTarget.ExternalSessionID)
		if i == 0 {
			require.Empty(t, query.CurrentTarget.SubjectBinding)
		} else {
			require.Equal(t, eligibleTestTarget().SubjectBinding, query.CurrentTarget.SubjectBinding)
		}
	}
}

func TestReviewPointLookupRejectsOldHostUnknownDeniedAndWrongIdentity(t *testing.T) {
	for _, mode := range []string{"old", "empty", "unknown", "mixed", "denied", "wrong-task", "wrong-session", "wrong-transcript", "wrong-binding", "enumeration", "preparing"} {
		for _, broker := range []bool{false, true} {
			name := mode + "/local"
			if broker {
				name = mode + "/broker"
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				var pointCalls atomic.Int32
				p, host := historyPlugin(t, exactHistoryFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
					calls.Add(1)
					if query.CurrentTarget == nil {
						return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.ResourceExhausted, "enumeration fallback is forbidden")
					}
					pointCalls.Add(1)
					target := eligibleTestTarget()
					page := pointTargetPage(target)
					switch mode {
					case "old":
						page.CurrentTarget = false
					case "empty":
						page.Targets = nil
					case "unknown", "mixed":
						page.Targets[0].SourceScopeStatus = mode
						page.Targets[0].ExternalSessionID = nil
					case "denied":
						return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.PermissionDenied, "denied")
					case "wrong-task":
						page.Targets[0].TaskID = "another-task"
					case "wrong-session":
						page.Targets[0].SessionID = optionalString("another-session")
					case "wrong-transcript":
						page.Targets[0].ExternalSessionID = optionalString("another-transcript")
					case "wrong-binding":
						page.Targets[0].SubjectBinding = "new-incarnation"
					case "enumeration":
						page.SnapshotID, page.NextCursor, page.HasMore = "archive", "next", true
					case "preparing":
						page.Targets, page.Preparing = nil, true
					}
					return page, nil
				}})
				if broker {
					p.UnimplementedPlugin.SetHost(sdkBrokerHost(t, host))
				}
				var expected []string
				if mode == "wrong-binding" {
					expected = []string{eligibleTestTarget().SubjectBinding}
				}
				_, err := p.resolveCurrentUsageTarget(context.Background(), "ws", "task-1", "session-1", "transcript-1", expected...)
				require.Error(t, err)
				if mode == "old" {
					require.ErrorIs(t, err, errUsageHistoryUnsupported)
				}
				if mode == "wrong-binding" {
					require.ErrorIs(t, err, errUsageScopeChanged)
				}
				require.Equal(t, int32(1), calls.Load(), "every refusal stops without retrying archive enumeration")
				require.Equal(t, int32(1), pointCalls.Load())
			})
		}
	}
}

func TestReviewPointLookupRequiresCompleteScopedSourceContext(t *testing.T) {
	var calls atomic.Int32
	p, _ := historyPlugin(t, exactHistoryFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		calls.Add(1)
		return pointTargetPage(eligibleTestTarget()), nil
	}})
	for _, identity := range [][4]string{{"", "task-1", "session-1", "transcript-1"}, {"ws", "", "session-1", "transcript-1"}, {"ws", "task-1", "", "transcript-1"}, {"ws", "task-1", "session-1", ""}} {
		_, err := p.resolveUsageTarget(context.Background(), identity[0], identity[1], identity[2], identity[3])
		require.ErrorIs(t, err, errUsageSourceUnavailable)
	}
	require.Zero(t, calls.Load(), "an incomplete identity cannot broaden a point lookup")
}

func TestReviewPointLookupRevokedBindingQuarantinesIDReuseAcrossRestart(t *testing.T) {
	var revoked atomic.Bool
	var mu sync.Mutex
	var queries []pluginsdk.UsageCollectionTargetQueryV1
	var reports atomic.Int32
	p, host := historyPlugin(t, exactHistoryFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		mu.Lock()
		queries = append(queries, query)
		mu.Unlock()
		if query.CurrentTarget == nil {
			return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.ResourceExhausted, "no archive reads")
		}
		target := eligibleTestTarget()
		if revoked.Load() {
			if query.CurrentTarget.SubjectBinding == target.SubjectBinding {
				return pluginsdk.UsageCollectionTargetPageV1{CurrentTarget: true}, nil
			}
			target.SubjectBinding = "new-org-incarnation"
		}
		return pointTargetPage(target), nil
	}, report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		reports.Add(1)
		return acceptedBatch(batch), nil
	}})
	bridge := sdkBrokerHost(t, host)
	p.UnimplementedPlugin.SetHost(bridge)
	response := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":7}]}`)
	require.NoError(t, p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response))
	revoked.Store(true)
	require.ErrorIs(t, p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response), errUsageScopeChanged)
	p.Close()
	restarted := migrationPlugin(t, bridge)
	require.ErrorIs(t, restarted.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", response), errUsageScopeChanged)
	require.Equal(t, int32(1), reports.Load())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, queries, 2, "a revoked binding is not dropped so that the same transcript can be relabeled")
	require.Equal(t, eligibleTestTarget().SubjectBinding, queries[1].CurrentTarget.SubjectBinding)
}

func TestReviewPointLookupCorrectsDeletedPrunedTargetWithoutLiveOrArchiveReads(t *testing.T) {
	var queries []pluginsdk.UsageCollectionTargetQueryV1
	var batches []pluginsdk.UsageObservationBatchV1
	p, host := historyPlugin(t, exactHistoryFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		require.NotNil(t, query.CurrentTarget)
		require.Equal(t, eligibleTestTarget().SubjectBinding, query.CurrentTarget.SubjectBinding)
		queries = append(queries, query)
		return pointTargetPage(eligibleTestTarget()), nil
	}, report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		return acceptedBatch(batch), nil
	}})
	// Tasks() is unimplemented and Sessions() must not be consulted after prune.
	host.sessionsFn = func(context.Context, pluginsdk.SessionFilter, pluginsdk.Page) ([]pluginsdk.Session, error) {
		t.Fatal("deleted-target correction read live sessions")
		return nil, nil
	}
	pending := collectorPending{WorkspaceID: "ws", SubjectBinding: eligibleTestTarget().SubjectBinding, TaskID: "task-1", ACPSessionID: "transcript-1"}
	row := session("session-1", "transcript-1")
	first := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"old","sourceRecordId":"stable","input":7}]}`)
	var err error
	pending, err = p.persistCollectorUsage(context.Background(), pending, row, first, usageCoverage{})
	require.NoError(t, err)
	p.Close()
	restarted := migrationPlugin(t, host)
	corrected := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"corrected","modelProvider":"new-provider","sourceRecordId":"stable","input":3}]}`)
	pending, err = restarted.persistCollectorUsage(context.Background(), pending, row, corrected, usageCoverage{})
	require.NoError(t, err)
	_, err = restarted.persistCollectorUsage(context.Background(), pending, row, corrected, usageCoverage{})
	require.NoError(t, err)
	require.Len(t, queries, 3)
	require.Len(t, batches, 2)
	before, after := batches[0].Observations[0], batches[1].Observations[0]
	require.Equal(t, before.ItemID, after.ItemID)
	require.Equal(t, int64(2), after.Revision)
	require.Equal(t, int64(3), *after.Tokens.InputTokens)
	require.Equal(t, "corrected", *after.ModelID)
	require.Equal(t, "new-provider", *after.ModelProvider)
}

func TestReviewPointLookupRawPreAndPostChecksNeverEnumerate(t *testing.T) {
	var queries []pluginsdk.UsageCollectionTargetQueryV1
	var revoked bool
	p, host := historyPlugin(t, exactHistoryFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
		require.NotNil(t, query.CurrentTarget)
		require.Empty(t, query.SnapshotID)
		require.Empty(t, query.Cursor)
		queries = append(queries, query)
		if revoked {
			return pluginsdk.UsageCollectionTargetPageV1{CurrentTarget: true}, nil
		}
		return pointTargetPage(eligibleTestTarget()), nil
	}})
	host.config[configKeyCollect] = false
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, canonicalReadyPage("current-empty", ""), nil
	}
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		revoked = true
		return []byte(`{"entries":[{"sessionId":"transcript-1","model":"m","input":999}]}`), nil
	}
	response, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.False(t, response.Found)
	require.False(t, response.CostKnown)
	require.Empty(t, response.Models)
	require.Len(t, queries, 2)
	require.Empty(t, queries[0].CurrentTarget.SubjectBinding)
	require.Equal(t, eligibleTestTarget().SubjectBinding, queries[1].CurrentTarget.SubjectBinding)
}
