package main

import (
	"encoding/json"
	"math/big"
	"strconv"

	"github.com/kdlbs/kandev/pluginsdk"
)

const maxBrowserInteger = int64(1<<53 - 1)

type tokenDisplay map[string]*string

func displayTokens(tokens *pluginsdk.UsageTokenAmountsV1, turns *int64) tokenDisplay {
	if tokens == nil {
		tokens = &pluginsdk.UsageTokenAmountsV1{}
	}
	values := map[string]*int64{"input": tokens.InputTokens, "output": tokens.OutputTokens, "cache_read": tokens.CacheReadTokens, "cache_write": tokens.CacheWriteTokens, "reasoning": tokens.ReasoningTokens, "total": tokens.TotalTokens, "turns": turns}
	result := make(tokenDisplay)
	for key, value := range values {
		if value == nil {
			result[key] = nil
		} else {
			text := strconv.FormatInt(*value, 10)
			result[key] = &text
		}
	}
	return result
}

func sumDisplayTokens(rows []tokenDisplay) tokenDisplay {
	result := make(tokenDisplay)
	for _, key := range []string{"input", "output", "cache_read", "cache_write", "reasoning", "total", "turns"} {
		total := new(big.Int)
		known := len(rows) > 0
		for _, row := range rows {
			value := row[key]
			if value == nil {
				known = false
				break
			}
			amount, ok := new(big.Int).SetString(*value, 10)
			if !ok {
				known = false
				break
			}
			total.Add(total, amount)
		}
		if known {
			text := total.String()
			result[key] = &text
		} else {
			result[key] = nil
		}
	}
	return result
}

type savedMetricDisplay struct {
	tokens    tokenDisplay
	cost      float64
	costKnown bool
	currency  string
	basis     string
	exactCost *string
}

// Only explicit Host applicability can exclude a canonical metric. Older
// replies default to applicable, so a nil amount still makes its sum unknown.
// Reuse this rule for the session and each exact model/provider display group.
func savedMetricAmounts(items []pluginsdk.SessionUsageMeasurement) savedMetricDisplay {
	result := savedMetricDisplay{costKnown: true}
	var tokenRows []tokenDisplay
	costRows := 0
	costTotal, exactTotal := new(big.Int), new(big.Int)
	exactKnown := true
	for _, item := range items {
		if !item.TokenMetricAbsent {
			tokenRows = append(tokenRows, displayTokens(&pluginsdk.UsageTokenAmountsV1{
				InputTokens: item.InputTokens, OutputTokens: item.OutputTokens,
				CacheReadTokens: item.CacheReadTokens, CacheWriteTokens: item.CacheWriteTokens,
				ReasoningTokens: item.ReasoningTokens, TotalTokens: item.TotalTokens,
			}, item.Turns))
		}
		if item.CostMetricAbsent {
			continue
		}
		if costRows == 0 {
			result.currency, result.basis = item.Currency, item.CostBasis
		} else {
			if result.currency != item.Currency {
				result.currency = "mixed"
			}
			if result.basis != item.CostBasis {
				result.basis = "mixed"
			}
		}
		costRows++
		available := item.CostSubcents != nil && *item.CostSubcents >= 0 && *item.CostSubcents <= maxBrowserInteger &&
			(item.CostCoverage == "" || item.CostCoverage == "complete") &&
			item.CostBasis != "unknown" && item.CostBasis != "mixed" && (item.Currency == "" || item.Currency == "USD")
		result.costKnown = result.costKnown && available
		if available {
			costTotal.Add(costTotal, big.NewInt(*item.CostSubcents))
		}
		if item.CostSubcents == nil || *item.CostSubcents < 0 {
			exactKnown = false
		} else {
			exactTotal.Add(exactTotal, big.NewInt(*item.CostSubcents))
		}
	}
	result.tokens = sumDisplayTokens(tokenRows)
	result.costKnown = result.costKnown && costRows > 0
	if !costTotal.IsInt64() || costTotal.Cmp(big.NewInt(maxBrowserInteger)) > 0 {
		result.costKnown = false
	} else {
		result.cost = float64(costTotal.Int64()) / 10000
	}
	if exactKnown && costRows > 0 && result.currency != "mixed" {
		text := exactTotal.String()
		result.exactCost = &text
	}
	return result
}

func displayInt64(value *string) int64 {
	if value == nil {
		return 0
	}
	amount, err := strconv.ParseInt(*value, 10, 64)
	if err != nil {
		return 0
	}
	return amount
}

func (r *sessionCostResponse) applyTokenDisplay(amounts tokenDisplay) {
	r.TokenAmounts = amounts
	fields := map[string]*int64{"input": &r.Input, "output": &r.Output, "cache_read": &r.CacheRead, "cache_write": &r.CacheWrite, "reasoning": &r.Reasoning, "total": &r.Total, "turns": &r.Turns}
	for key, field := range fields {
		*field = 0
		if value := amounts[key]; value != nil {
			if parsed, err := strconv.ParseInt(*value, 10, 64); err == nil {
				*field = parsed
			}
		}
	}
}

func sourceDisplay(response *sessionCostResponse) {
	if response.sourceEntries == nil {
		return
	}
	var rows []tokenDisplay
	costTotal := new(big.Int)
	for i, entry := range response.sourceEntries {
		response.Models[i].Provider = singleSourceProvider(response.Models[i].Provider)
		presence := presenceForEntry(entry)
		tokens := &pluginsdk.UsageTokenAmountsV1{InputTokens: ptrIf(presence.input, entry.Input), OutputTokens: ptrIf(presence.output, entry.Output), CacheReadTokens: ptrIf(presence.cacheRead, entry.CacheRead), CacheWriteTokens: ptrIf(presence.cacheWrite, entry.CacheWrite), ReasoningTokens: ptrIf(presence.reasoning, entry.Reasoning), TotalTokens: presence.totalValue}
		amounts := displayTokens(tokens, nil)
		rows = append(rows, amounts)
		response.Models[i].TokenAmounts = amounts
		known := presence.cost
		amount := entry.costValue
		if presence.cost && amount == nil {
			amount, _ = parseUSDAmount(strconv.FormatFloat(entry.Cost, 'f', 8, 64))
		}
		if amount != nil {
			text := strconv.FormatInt(*amount, 10)
			response.Models[i].ExactCost = &text
			costTotal.Add(costTotal, big.NewInt(*amount))
			if *amount > maxBrowserInteger {
				known = false
				response.CostKnown = false
			}
		}
		response.Models[i].CostKnown = &known
		response.Models[i].CostBasis = "estimated"
		response.Models[i].Currency = "USD"
	}
	response.applyTokenDisplay(sumDisplayTokens(rows))
	response.CostBasis = "estimated"
	response.Currency = "USD"
	if !costTotal.IsInt64() || costTotal.Cmp(big.NewInt(maxBrowserInteger)) > 0 {
		response.CostKnown = false
	}
	if !response.CostKnown {
		response.Cost = 0
		response.CostBasis = "unknown"
	}
}

func marshalUsageDisplay(value any, amounts tokenDisplay) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || amounts == nil {
		return encoded, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for key, value := range amounts {
		if value == nil {
			fields[key] = json.RawMessage("null")
			continue
		}
		amount, err := strconv.ParseInt(*value, 10, 64)
		if err != nil || amount > maxBrowserInteger {
			fields[key] = json.RawMessage("null")
		}
	}
	return json.Marshal(fields)
}

func (r sessionCostResponse) MarshalJSON() ([]byte, error) {
	type plain sessionCostResponse
	return marshalUsageDisplay(plain(r), r.TokenAmounts)
}
func (m sessionModel) MarshalJSON() ([]byte, error) {
	type plain sessionModel
	return marshalUsageDisplay(plain(m), m.TokenAmounts)
}
