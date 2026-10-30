package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

// These are canonical Host replies, not source observations. Applicability is
// explicit Host evidence; nil amounts never supply that evidence.
func splitCanonicalMetricFixture() []pluginsdk.SessionUsageMeasurement {
	return []pluginsdk.SessionUsageMeasurement{
		{
			ID: "native-tokens", WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
			Model: "exact/model", Provider: "provider", InputTokens: ptr(100), OutputTokens: ptr(0),
			CacheReadTokens: ptr(0), CacheWriteTokens: ptr(0), ReasoningTokens: ptr(0), TotalTokens: ptr(100), Turns: ptr(2),
			CostBasis: "unknown", CostCoverage: "missing", CostMetricAbsent: true, Coverage: "complete",
		},
		{
			ID: "external-cost", WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1", TranscriptID: "transcript-1",
			Model: "exact/model", Provider: "provider", CostSubcents: ptr(20), Currency: "USD",
			CostBasis: "estimated", CostCoverage: "complete", TokenMetricAbsent: true, Coverage: "complete",
		},
	}
}

type canonicalMetricCase struct {
	name        string
	rows        []pluginsdk.SessionUsageMeasurement
	input       *string
	total       *string
	turns       *string
	costKnown   bool
	cost        float64
	costPerTurn float64
	currency    string
	basis       string
	exactCost   *string
	largeInput  bool
}

func canonicalMetricCases() []canonicalMetricCase {
	split := func(name string) canonicalMetricCase {
		return canonicalMetricCase{name: name, rows: splitCanonicalMetricFixture(), input: optionalString("100"), total: optionalString("100"), turns: optionalString("2"), costKnown: true, cost: 0.002, costPerTurn: 0.001, currency: "USD", basis: "estimated", exactCost: optionalString("20")}
	}
	cases := []canonicalMetricCase{split("split token and cost metrics")}
	unknownCost := split("applicable unknown cost taints only cost")
	unknownCost.rows[1].CostSubcents = nil
	unknownCost.costKnown, unknownCost.cost, unknownCost.costPerTurn, unknownCost.exactCost = false, 0, 0, nil
	cases = append(cases, unknownCost)
	additionalUnknown := split("one unknown applicable cost prevents a partial sum from becoming current")
	unknown := additionalUnknown.rows[1]
	unknown.ID, unknown.CostSubcents = "unknown-cost", nil
	additionalUnknown.rows = append(additionalUnknown.rows, unknown)
	additionalUnknown.costKnown, additionalUnknown.costPerTurn, additionalUnknown.exactCost = false, 0, nil
	cases = append(cases, additionalUnknown)
	unknownToken := split("applicable unknown category taints only that category")
	unknownToken.rows[0].InputTokens = nil
	unknownToken.input = nil
	cases = append(cases, unknownToken)
	additionalUnknownToken := split("one unknown applicable token category taints its sum only")
	unknown = additionalUnknownToken.rows[0]
	unknown.ID, unknown.InputTokens, unknown.TotalTokens, unknown.Turns = "unknown-input", nil, ptr(10), ptr(1)
	additionalUnknownToken.rows = append(additionalUnknownToken.rows, unknown)
	additionalUnknownToken.input, additionalUnknownToken.total, additionalUnknownToken.turns = nil, optionalString("110"), optionalString("3")
	additionalUnknownToken.costPerTurn = 0.002 / 3
	cases = append(cases, additionalUnknownToken)
	unknownTurns := split("unknown Kandev turns prohibit cost per turn")
	unknownTurns.rows[0].Turns = nil
	unknownTurns.turns, unknownTurns.costPerTurn = nil, 0
	cases = append(cases, unknownTurns)
	legacyUnknown := split("older defaults keep both metrics applicable")
	legacyUnknown.rows[0].CostMetricAbsent = false
	legacyUnknown.rows[1].TokenMetricAbsent = false
	legacyUnknown.input, legacyUnknown.total, legacyUnknown.turns = nil, nil, nil
	legacyUnknown.costKnown, legacyUnknown.costPerTurn, legacyUnknown.exactCost = false, 0, nil
	legacyUnknown.currency, legacyUnknown.basis = "mixed", "mixed"
	cases = append(cases, legacyUnknown)
	zero := split("measured zeros remain known")
	zero.rows[0].InputTokens, zero.rows[0].TotalTokens, zero.rows[0].Turns = ptr(0), ptr(0), ptr(0)
	zero.rows[1].CostSubcents = ptr(0)
	zero.input, zero.total, zero.turns = optionalString("0"), optionalString("0"), optionalString("0")
	zero.cost, zero.costPerTurn, zero.exactCost = 0, 0, optionalString("0")
	cases = append(cases, zero)
	partial := split("partial cost is not a complete spend")
	partial.rows[1].CostCoverage, partial.rows[1].Coverage = "partial", "partial"
	partial.costKnown, partial.cost, partial.costPerTurn = false, 0, 0
	cases = append(cases, partial)
	unknownBasis := split("unknown cost basis remains unavailable")
	unknownBasis.rows[1].CostBasis = "unknown"
	unknownBasis.costKnown, unknownBasis.cost, unknownBasis.costPerTurn, unknownBasis.basis = false, 0, 0, "unknown"
	cases = append(cases, unknownBasis)
	mixedBasis := split("mixed row cost basis remains unavailable")
	mixedBasis.rows[1].CostBasis = "mixed"
	mixedBasis.costKnown, mixedBasis.cost, mixedBasis.costPerTurn, mixedBasis.basis = false, 0, 0, "mixed"
	cases = append(cases, mixedBasis)
	foreign := split("foreign currency is not formatted as USD")
	foreign.rows[1].Currency = "EUR"
	foreign.costKnown, foreign.cost, foreign.costPerTurn, foreign.currency = false, 0, 0, "EUR"
	cases = append(cases, foreign)
	mixedCurrency := split("mixed currencies have no invented common amount")
	extra := mixedCurrency.rows[1]
	extra.ID, extra.Currency, extra.CostSubcents = "foreign-cost", "EUR", ptr(30)
	mixedCurrency.rows = append(mixedCurrency.rows, extra)
	mixedCurrency.costKnown, mixedCurrency.costPerTurn, mixedCurrency.currency, mixedCurrency.exactCost = false, 0, "mixed", nil
	cases = append(cases, mixedCurrency)
	onlyCost := split("cost only does not invent zero tokens or turns")
	onlyCost.rows = onlyCost.rows[1:]
	onlyCost.input, onlyCost.total, onlyCost.turns, onlyCost.costPerTurn = nil, nil, nil, 0
	cases = append(cases, onlyCost)
	onlyTokens := split("tokens only do not invent zero cost")
	onlyTokens.rows = onlyTokens.rows[:1]
	onlyTokens.costKnown, onlyTokens.cost, onlyTokens.costPerTurn, onlyTokens.exactCost = false, 0, 0, nil
	onlyTokens.currency, onlyTokens.basis = "", ""
	cases = append(cases, onlyTokens)
	allAbsent := split("no applicable metric stays unavailable")
	allAbsent.rows = []pluginsdk.SessionUsageMeasurement{{Model: "exact/model", Provider: "provider", TokenMetricAbsent: true, CostMetricAbsent: true, Coverage: "complete"}}
	allAbsent.input, allAbsent.total, allAbsent.turns, allAbsent.exactCost = nil, nil, nil, nil
	allAbsent.costKnown, allAbsent.cost, allAbsent.costPerTurn, allAbsent.currency, allAbsent.basis = false, 0, 0, "", ""
	cases = append(cases, allAbsent)
	overflow := split("exact large cost never crosses the browser number boundary")
	overflow.rows[1].CostSubcents = ptr(math.MaxInt64)
	overflow.costKnown, overflow.cost, overflow.costPerTurn = false, 0, 0
	overflow.exactCost = optionalString(strconv.FormatInt(math.MaxInt64, 10))
	cases = append(cases, overflow)
	largeTokens := split("large token sums preserve exact strings and null browser amounts")
	largeTokens.rows[0].InputTokens, largeTokens.rows[0].TotalTokens = ptr(math.MaxInt64), ptr(math.MaxInt64)
	second := largeTokens.rows[0]
	second.ID = "another-native-token-fact"
	largeTokens.rows = append(largeTokens.rows, second)
	largeTokens.input, largeTokens.total, largeTokens.turns = optionalString("18446744073709551614"), optionalString("18446744073709551614"), optionalString("4")
	largeTokens.largeInput = true
	largeTokens.costPerTurn = 0.0005
	cases = append(cases, largeTokens)
	ignoredPayload := split("only Host applicability excludes a metric")
	ignoredPayload.rows[0].CostSubcents, ignoredPayload.rows[0].Currency, ignoredPayload.rows[0].CostBasis = ptr(999), "EUR", "reported"
	ignoredPayload.rows[1].InputTokens, ignoredPayload.rows[1].TotalTokens, ignoredPayload.rows[1].Turns = ptr(999), ptr(999), ptr(999)
	cases = append(cases, ignoredPayload)
	return cases
}

func assertCanonicalMetricCase(t *testing.T, response *sessionCostResponse, test canonicalMetricCase) {
	t.Helper()
	require.NotNil(t, response)
	require.True(t, response.Found)
	require.True(t, response.Saved)
	require.Equal(t, test.input, response.TokenAmounts["input"])
	require.Equal(t, test.total, response.TokenAmounts["total"])
	require.Equal(t, test.turns, response.TokenAmounts["turns"])
	require.Equal(t, test.costKnown, response.CostKnown)
	if test.costKnown {
		require.InDelta(t, test.cost, response.Cost, 1e-12)
	}
	require.InDelta(t, test.costPerTurn, response.CostPerTurn, 1e-12)
	require.Equal(t, test.currency, response.Currency)
	require.Equal(t, test.basis, response.CostBasis)
	require.Len(t, response.Models, 1, "the same exact model/provider is one display group even when metrics have separate facts")
	model := response.Models[0]
	require.Equal(t, "exact/model", model.Model)
	require.Equal(t, "provider", model.Provider)
	require.Equal(t, test.input, model.TokenAmounts["input"])
	require.Equal(t, test.total, model.TokenAmounts["total"])
	require.Equal(t, test.exactCost, model.ExactCost)
	require.NotNil(t, model.CostKnown)
	require.Equal(t, test.costKnown, *model.CostKnown)
	require.Equal(t, test.currency, model.Currency)
	require.Equal(t, test.basis, model.CostBasis)
	if test.costKnown {
		require.InDelta(t, test.cost, model.Cost, 1e-12)
	}
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &wire))
	var models []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire["models"], &models))
	if test.input == nil || test.largeInput {
		require.JSONEq(t, "null", string(wire["input"]), "unknown or inapplicable input must not become numeric zero")
		require.JSONEq(t, "null", string(models[0]["input"]))
	} else {
		require.JSONEq(t, *test.input, string(wire["input"]))
		require.JSONEq(t, *test.input, string(models[0]["input"]))
	}
	if test.turns == nil {
		require.JSONEq(t, "null", string(wire["turns"]), "cost facts supply no Kandev turn evidence")
	}
}

func TestReviewCanonicalMetricApplicabilityFakeHost(t *testing.T) {
	for _, test := range canonicalMetricCases() {
		t.Run(test.name, func(t *testing.T) {
			p, host := historyPlugin(t, retainedProtocolFixture{})
			host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				return test.rows, canonicalReadyPage("canonical-metric-generation", ""), nil
			}
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("canonical metric applicability started a source process")
				return nil, nil
			}
			response, found, err := p.loadSavedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
			require.NoError(t, err)
			require.True(t, found)
			assertCanonicalMetricCase(t, response, test)
			require.Equal(t, "canonical-metric-generation", response.ProjectionGeneration)
		})
	}
}

func TestReviewCanonicalMetricApplicabilityRPC(t *testing.T) {
	for _, test := range canonicalMetricCases() {
		t.Run(test.name, func(t *testing.T) {
			cursors := []string{"", "usage-v1.metric+/=cost", "usage-v1.metric+/=another-fact"}
			p, requests := canonicalRPCFixture(t, func(_ context.Context, request canonicalRPCRequest, _ int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				index := -1
				for i, cursor := range cursors {
					if request.Page.Cursor == cursor {
						index = i
					}
				}
				if index < 0 || index >= len(test.rows) {
					return nil, nil, errors.New("unexpected opaque metric cursor")
				}
				next := ""
				if index+1 < len(test.rows) {
					next = cursors[index+1]
				}
				return test.rows[index : index+1], canonicalReadyPage("canonical-metric-generation", next), nil
			})
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("canonical metric applicability started a source process")
				return nil, nil
			}
			response, found, err := p.loadSavedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
			require.NoError(t, err)
			require.True(t, found)
			assertCanonicalMetricCase(t, response, test)
			calls := requests()
			require.Len(t, calls, len(test.rows), "split metrics can cross opaque cursor pages in the same publication")
			for _, call := range calls {
				require.True(t, call.Filter.Canonical)
				require.Equal(t, "ws", call.Filter.WorkspaceID)
				require.Equal(t, []string{"task-1"}, call.Filter.TaskIDs)
				require.Equal(t, []string{"session-1"}, call.Filter.SessionIDs)
			}
		})
	}
}

func TestReviewCanonicalMetricApplicabilityKeepsExactModelProviderGroups(t *testing.T) {
	rows := splitCanonicalMetricFixture()
	second := splitCanonicalMetricFixture()
	for i := range second {
		second[i].Provider = "another-provider"
	}
	second[0].InputTokens, second[0].TotalTokens, second[0].Turns = ptr(200), ptr(200), ptr(3)
	second[1].CostSubcents, second[1].CostBasis = ptr(30), "reported"
	rows = append(rows, second...)
	response := responseFromSaved(rows, "session-1", 1, 10)
	require.True(t, response.CostKnown)
	require.Equal(t, "USD", response.Currency)
	require.Equal(t, "mixed", response.CostBasis, "known estimated and reported values retain their mixed basis")
	require.Equal(t, "300", *response.TokenAmounts["total"])
	require.Equal(t, "5", *response.TokenAmounts["turns"])
	require.InDelta(t, 0.005, response.Cost, 1e-12)
	require.InDelta(t, 0.001, response.CostPerTurn, 1e-12)
	require.Len(t, response.Models, 2)
	require.Equal(t, "provider", response.Models[0].Provider)
	require.Equal(t, "another-provider", response.Models[1].Provider)
	require.Equal(t, "100", *response.Models[0].TokenAmounts["total"])
	require.Equal(t, "200", *response.Models[1].TokenAmounts["total"])
	require.Equal(t, "20", *response.Models[0].ExactCost)
	require.Equal(t, "30", *response.Models[1].ExactCost)
}
