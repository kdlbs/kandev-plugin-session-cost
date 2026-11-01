package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func pinnedProducerPayload(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/tokscale-4.15.1-session-model.json")
	require.NoError(t, err)
	return data
}

func pinnedProducerResponse(t *testing.T, data []byte) *sessionCostResponse {
	t.Helper()
	entries, err := runSessionModels(context.Background(), resolveCommand("", noLookPath), func(context.Context, string, ...string) ([]byte, error) { return data, nil })
	require.NoError(t, err)
	response := responseForEntries("session-1", "transcript-1", entries, 1, 10, time.Now())
	return &response
}

func editPinnedRows(t *testing.T, data []byte, change func([]map[string]any) []map[string]any) []byte {
	t.Helper()
	var report struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(data, &report))
	report.Entries = change(report.Entries)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	return encoded
}

func TestReviewPinnedTokscaleTwoModelSegmentsTotal300AndProviderCorrection(t *testing.T) {
	data := pinnedProducerPayload(t)
	first := pinnedProducerResponse(t, data)
	var batches []pluginsdk.UsageObservationBatchV1
	p, _ := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		return acceptedBatch(batch), nil
	}})
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), first, usageCoverage{}))
	require.Equal(t, pluginsdk.UsageObservationSchemaVersionV2, batches[0].SchemaVersion)
	require.Len(t, batches[0].Observations, 2)
	var total int64
	var old pluginsdk.UsageObservationV1
	for _, row := range batches[0].Observations {
		require.NotNil(t, row.Segment)
		require.NotNil(t, row.Tokens.TotalTokens, "the real pinned report has no total field; its checked source-defined sum is known")
		total += *row.Tokens.TotalTokens
		if *row.ModelID == "gpt-5" {
			old = row
		}
	}
	require.Equal(t, int64(300), total)
	corrected := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any {
		rows[0]["provider"] = "azure"
		rows[0]["input"] = 80
		return rows
	})
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, corrected), usageCoverage{}))
	require.Len(t, batches, 2)
	require.Len(t, batches[1].Observations, 1)
	row := batches[1].Observations[0]
	require.Equal(t, old.ItemID, row.ItemID)
	require.Equal(t, old.Segment, row.Segment)
	require.Equal(t, int64(2), row.Revision)
	require.Equal(t, "azure", *row.ModelProvider)
	require.Equal(t, int64(80), *row.Tokens.TotalTokens)
	digest, err := row.ComputePayloadDigestV1()
	require.NoError(t, err)
	require.Equal(t, row.PayloadDigest, digest)
}

func TestReviewPinnedTokscaleMergedProviderAndFirstClientAreRawMetadata(t *testing.T) {
	data := editPinnedRows(t, pinnedProducerPayload(t), func(rows []map[string]any) []map[string]any {
		rows[0]["provider"] = "anthropic, openai"
		rows[0]["modelProvider"] = "openai" // An extra label cannot override the actual merged producer field.
		rows[0]["client"] = "codex"
		return rows[:1]
	})
	response := pinnedProducerResponse(t, data)
	rows, err := (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Nil(t, rows[0].ModelProvider, "a merged label must not attribute the full spend to a provider")
	require.Nil(t, rows[0].AgentClient, "session grouping retains only the first contributor client")
	require.Equal(t, "anthropic, openai", rows[0].RawSourceLabels["provider"])
	require.Equal(t, "codex", rows[0].RawSourceLabels["client"])
}

func TestReviewPinnedTokscaleMissingMemberQuarantinesAndRecoversAfterRestart(t *testing.T) {
	data := pinnedProducerPayload(t)
	var batches []pluginsdk.UsageObservationBatchV1
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		return acceptedBatch(batch), nil
	}})
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, data), usageCoverage{}))
	rename := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any {
		rows[1]["model"] = "new-alias"
		rows[0]["input"] = 500
		return rows
	})
	err := p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, rename), usageCoverage{})
	require.ErrorContains(t, err, "membership")
	require.Len(t, batches, 1, "no partial changed group may be written before the missing member is detected")
	p.Close()
	restarted := migrationPlugin(t, host)
	require.Error(t, restarted.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, rename), usageCoverage{}))
	require.Len(t, batches, 1)
	// Rejected new names must not poison the baseline and prevent recovery.
	restored := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any { rows[0]["input"] = 90; return rows })
	require.NoError(t, restarted.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, restored), usageCoverage{}))
	require.Len(t, batches, 2)
	require.Len(t, batches[1].Observations, 1)
	require.Equal(t, int64(2), batches[1].Observations[0].Revision)
	assertNoAccountingAmountsInState(t, host.fakeHost)
}

func TestReviewPinnedTokscaleDifferentDayMembershipIsIndependent(t *testing.T) {
	data := pinnedProducerPayload(t)
	firstDay := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any { return rows[:1] })
	secondDay := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any { return rows[1:] })
	var batches []pluginsdk.UsageObservationBatchV1
	p, _ := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		return acceptedBatch(batch), nil
	}})
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, data), usageCoverage{}))
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, firstDay), usageCoverage{Date: "2026-10-28", Timezone: "UTC"}))
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, secondDay), usageCoverage{Date: "2026-10-29", Timezone: "UTC"}))
	require.Len(t, batches, 3)
	for _, batch := range batches[1:] {
		row := batch.Observations[0]
		for _, lifetime := range batches[0].Observations {
			if *lifetime.ModelID == *row.ModelID {
				require.Equal(t, lifetime.Segment, row.Segment)
			}
		}
	}
}

func TestReviewPinnedTokscaleMembershipSurvivesUncertainAndPartialAdmission(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			data := pinnedProducerPayload(t)
			var batches []pluginsdk.UsageObservationBatchV1
			p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
				batches = append(batches, batch)
				if len(batches) == 1 {
					if !partial {
						return pluginsdk.UsageObservationBatchResultV1{}, status.Error(codes.Unavailable, "admitted but receipt lost")
					}
					result := acceptedBatch(batch)
					result.Status = pluginsdk.UsageReportStatusPartial
					result.Results[1].Status = pluginsdk.UsageObservationStatusRejected
					return result, nil
				}
				return acceptedBatch(batch), nil
			}})
			require.Error(t, p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, data), usageCoverage{}))
			var membership tokscaleMembershipCheckpoint
			found, err := p.readUsageState(context.Background(), tokscaleMembershipStateID, tokscaleMembershipKey("transcript-1", usageCoverage{}), &membership)
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, membership.Members, 2, "both members are durable before any report/receipt can be lost")
			p.Close()
			restarted := migrationPlugin(t, host)
			missing := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any { return rows[:1] })
			require.ErrorIs(t, restarted.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, missing), usageCoverage{}), errTokscaleMembership)
			require.Len(t, batches, 1)
			require.NoError(t, restarted.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, data), usageCoverage{}))
			require.Len(t, batches, 2)
			if partial {
				require.Len(t, batches[1].Observations, 1)
				require.Equal(t, batches[0].Observations[1], batches[1].Observations[0], "retry only the rejected member with its exact pending revision/digest")
			} else {
				require.Equal(t, batches[0], batches[1], "uncertain schema2 receipts replay the exact whole declaration")
			}
			assertNoAccountingAmountsInState(t, host.fakeHost)
		})
	}
}

func TestReviewPinnedTokscaleTotalsRequireVerifiedCompleteCheckedFields(t *testing.T) {
	base := editPinnedRows(t, pinnedProducerPayload(t), func(rows []map[string]any) []map[string]any { return rows[:1] })
	for _, mode := range []string{"pinned", "partial", "overflow", "unknown-command", "generic-fields"} {
		t.Run(mode, func(t *testing.T) {
			data := base
			if mode == "partial" {
				data = editPinnedRows(t, data, func(rows []map[string]any) []map[string]any { delete(rows[0], "reasoning"); return rows })
			}
			if mode == "overflow" {
				data = editPinnedRows(t, data, func(rows []map[string]any) []map[string]any {
					rows[0]["input"] = json.Number(fmt.Sprint(int64(math.MaxInt64)))
					rows[0]["output"] = 1
					return rows
				})
			}
			var response *sessionCostResponse
			if mode == "generic-fields" {
				response = parsedUsage(t, string(data)) // No recognized pinned invocation.
			} else {
				cmd := resolveCommand("", noLookPath)
				if mode == "unknown-command" {
					cmd = resolvedCommand{Argv: []string{"custom-usage"}, Source: sourceSettings}
				}
				entries, err := runSessionModels(context.Background(), cmd, func(context.Context, string, ...string) ([]byte, error) { return data, nil })
				require.NoError(t, err)
				value := responseForEntries("session-1", "transcript-1", entries, 1, 10, time.Now())
				response = &value
			}
			rows, err := (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{})
			require.NoError(t, err)
			if mode == "pinned" {
				require.Equal(t, int64(100), *rows[0].Tokens.TotalTokens)
			} else {
				require.Nil(t, rows[0].Tokens.TotalTokens)
			}
		})
	}
}

func TestReviewPinnedTokscaleMembershipBoundAndDuplicateRowsFailBeforeReport(t *testing.T) {
	for _, mode := range []string{"bound", "duplicate-provider"} {
		t.Run(mode, func(t *testing.T) {
			data := editPinnedRows(t, pinnedProducerPayload(t), func(rows []map[string]any) []map[string]any {
				if mode == "duplicate-provider" {
					rows[1]["model"] = rows[0]["model"]
					rows[1]["provider"] = "another-provider"
					return rows
				}
				var result []map[string]any
				for i := 0; i <= maxTokscaleMembershipGroups; i++ {
					row := make(map[string]any)
					for k, v := range rows[0] {
						row[k] = v
					}
					row["model"] = fmt.Sprintf("group-%04d", i)
					result = append(result, row)
				}
				return result
			})
			calls := 0
			p, _ := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
				calls++
				return acceptedBatch(b), nil
			}})
			require.ErrorIs(t, p.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, data), usageCoverage{}), errTokscaleMembership)
			require.Zero(t, calls)
		})
	}
}

func TestReviewPinnedTokscaleActionKeepsAcceptedValuesDuringMembershipQuarantine(t *testing.T) {
	data := pinnedProducerPayload(t)
	var accepted []pluginsdk.SessionUsageMeasurement
	writes := 0
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, b pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		writes++
		for _, item := range b.Observations {
			row := pluginsdk.SessionUsageMeasurement{ID: item.ItemID, TaskID: "task-1", SessionID: "session-1", Model: *item.ModelID, InputTokens: item.Tokens.InputTokens, TotalTokens: item.Tokens.TotalTokens, CostSubcents: item.Cost.Amount, Currency: "USD", CostBasis: "estimated", CostCoverage: "complete", Coverage: "complete"}
			replaced := false
			for i, old := range accepted {
				if old.ID == row.ID {
					accepted[i] = row
					replaced = true
				}
			}
			if !replaced {
				accepted = append(accepted, row)
			}
		}
		return acceptedBatch(b), nil
	}})
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return accepted, canonicalReadyPage("authorized-complete", ""), nil
	}
	p.run = modelsRunner(data, nil)
	first, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, int64(300), first.Input)
	require.True(t, first.Saved)
	bad := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any { rows[0]["input"] = 900; return rows[:1] })
	p.run = modelsRunner(bad, nil)
	blocked, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, int64(300), blocked.Input)
	require.True(t, blocked.Stale)
	require.True(t, blocked.Saved)
	require.Equal(t, "source_membership_ambiguous", blocked.Compatibility)
	require.Equal(t, 1, writes)
	// An entirely empty report also cannot silently drop the saved membership.
	p.run = modelsRunner([]byte(`{"entries":[]}`), nil)
	empty, err := p.sessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Equal(t, "source_membership_ambiguous", empty.Compatibility)
	require.Equal(t, int64(300), empty.Input)
	require.Equal(t, 1, writes)
	assertNoAccountingAmountsInState(t, host.fakeHost)
}

func TestReviewPinnedTokscaleExactProducerSessionIDsSurviveSuffixJoinAndRestart(t *testing.T) {
	data := editPinnedRows(t, pinnedProducerPayload(t), func(rows []map[string]any) []map[string]any {
		rows[0]["sessionId"], rows[0]["model"] = "rollout-2026-10-28-transcript-1", "gpt-5"
		rows[1]["sessionId"], rows[1]["model"] = "rollout-2026-10-29-transcript-1", "gpt-5"
		return rows
	})
	var batches []pluginsdk.UsageObservationBatchV1
	p, host := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		return acceptedBatch(batch), nil
	}})
	response := pinnedProducerResponse(t, data)
	require.Len(t, response.sourceEntries, 2, "both exact producer IDs join the same supported ACP suffix")
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), response, usageCoverage{}))
	require.Len(t, batches[0].Observations, 2)
	one, two := batches[0].Observations[0], batches[0].Observations[1]
	require.NotEqual(t, one.Segment.SegmentID, two.Segment.SegmentID)
	require.NotEqual(t, one.SourceRecordID, two.SourceRecordID)
	require.Equal(t, int64(300), *one.Tokens.TotalTokens+*two.Tokens.TotalTokens)
	expected := map[string]bool{tokscaleSegment("rollout-2026-10-28-transcript-1", "gpt-5").SegmentID: true, tokscaleSegment("rollout-2026-10-29-transcript-1", "gpt-5").SegmentID: true}
	require.True(t, expected[one.Segment.SegmentID])
	require.True(t, expected[two.Segment.SegmentID])
	p.Close()
	restarted := migrationPlugin(t, host)
	missing := editPinnedRows(t, data, func(rows []map[string]any) []map[string]any { return rows[:1] })
	require.ErrorIs(t, restarted.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, missing), usageCoverage{}), errTokscaleMembership)
	require.Len(t, batches, 1)
	require.NoError(t, restarted.persistTargetUsage(context.Background(), testTarget(), pinnedProducerResponse(t, data), usageCoverage{}))
	require.Len(t, batches, 1, "restored members replay idempotently after restart")
}

func TestReviewPinnedTokscaleExplicitRecordIdentitiesAreNotOrdinaryModelGroups(t *testing.T) {
	var batches []pluginsdk.UsageObservationBatchV1
	p, _ := historyPlugin(t, retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		batches = append(batches, batch)
		return acceptedBatch(batch), nil
	}})
	first := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","sourceRecordId":"record-a","model":"same-model","provider":"a","input":10},{"sessionId":"transcript-1","sourceRecordId":"record-b","model":"same-model","provider":"b","input":20}]}`)
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), first, usageCoverage{}))
	require.Len(t, batches[0].Observations, 2)
	var original pluginsdk.UsageObservationV1
	for _, row := range batches[0].Observations {
		require.Nil(t, row.Segment, "explicit record IDs alone do not invent a disjoint partition")
		if row.SourceRecordID == "record-a" {
			original = row
		}
	}
	correction := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","sourceRecordId":"record-a","model":"renamed","provider":"c","input":8}]}`)
	require.NoError(t, p.persistTargetUsage(context.Background(), testTarget(), correction, usageCoverage{}))
	require.Len(t, batches[1].Observations, 1)
	require.Equal(t, original.ItemID, batches[1].Observations[0].ItemID)
	require.Equal(t, int64(2), batches[1].Observations[0].Revision)
	require.Empty(t, batches[1].Tombstones)
}

func TestReviewPinnedTokscaleModelsOnlyCannotInventProducerSessionIdentity(t *testing.T) {
	response := &sessionCostResponse{ACPSessionID: "canonical-only", Models: []sessionModel{{Model: "gpt-5", Input: 100}}}
	_, err := (tokscaleUsageAdapter{}).observations(testTarget(), response, usageCoverage{})
	require.ErrorIs(t, err, errUsageSourceUnavailable)
}
