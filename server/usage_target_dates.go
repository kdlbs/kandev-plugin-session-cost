package main

import (
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
)

// Session milestones include their endpoint day. Source coverage is half-open.
// Both endpoints must be known; neither collection time nor a deleted task ID
// supplies a missing bound.
// importTargetDateRange bounds the days an import requests. It extends a known
// start with no end to today so an open eligible session still receives daily
// history. The range only selects source days; saved amounts still come from
// tokscale rows. Trusted current-target lookups keep using
// trustedTargetDateRange without this extension.
func importTargetDateRange(target pluginsdk.UsageCollectionTargetV1, today string) []string {
	if dates := trustedTargetDateRange(target); len(dates) > 0 {
		return dates
	}
	if !eligibleUsageSource(target) || target.SessionStartedAt == nil || target.SessionEndedAt != nil || target.CoverageEnd != nil || today == "" {
		return nil
	}
	start := parseSessionTime(*target.SessionStartedAt)
	if start.IsZero() {
		return nil
	}
	first, last := collectionDate(start), today
	if first > last {
		return nil
	}
	if first == last {
		return []string{first}
	}
	return []string{first, last}
}

func trustedTargetDateRange(target pluginsdk.UsageCollectionTargetV1) []string {
	if !eligibleUsageSource(target) {
		return nil
	}
	pairs := []struct {
		start, end *string
		exclusive  bool
	}{
		{target.SessionStartedAt, target.SessionEndedAt, false},
		{target.CoverageStart, target.CoverageEnd, true},
	}
	for _, pair := range pairs {
		if pair.start == nil || pair.end == nil {
			continue
		}
		start, end := parseSessionTime(*pair.start), parseSessionTime(*pair.end)
		if start.IsZero() || end.IsZero() || end.Before(start) || (pair.exclusive && !end.After(start)) {
			continue
		}
		if pair.exclusive {
			end = end.Add(-time.Nanosecond)
		}
		first, last := collectionDate(start), collectionDate(end)
		if first == last {
			return []string{first}
		}
		return []string{first, last}
	}
	return nil
}

// One pending source day is enough to resume even a long retained interval.
// Per-target ranges and the keyset page remain fixed in the import checkpoint.
func nextHistoricalImportDate(ranges map[string][]string, after *string) (string, bool) {
	next := ""
	found := false
	for _, dates := range ranges {
		if len(dates) == 0 {
			continue
		}
		candidate := dates[0]
		last := dates[len(dates)-1]
		if after != nil {
			if last <= *after {
				continue
			}
			if candidate <= *after {
				day, err := time.ParseInLocation("2006-01-02", *after, sourceLocation())
				if err != nil {
					continue
				}
				candidate = day.AddDate(0, 0, 1).Format("2006-01-02")
			}
		}
		if !found || candidate < next {
			next = candidate
			found = true
		}
	}
	return next, found
}
