//go:build !windows

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSourceTimezoneDoesNotGuessUTCForCopiedLocaltime(t *testing.T) {
	local := time.FixedZone("Local", 2*60*60)
	zone := resolveSourceTimezone(local, func(string) (string, error) {
		return "", errors.New("localtime is a regular file")
	}, platformSourceTimezone)
	require.Empty(t, zone)

	p := newPlugin()
	err := p.persistSessionUsageWithCoverage(context.Background(), "ws", "task", "session", &sessionCostResponse{}, usageCoverage{Date: "2026-10-01", Timezone: zone})
	require.ErrorContains(t, err, "cannot resolve source timezone")
}

func TestSourceTimezoneResolvesNamedAndLinkedZones(t *testing.T) {
	readlink := func(string) (string, error) { return "/usr/share/zoneinfo/Europe/Lisbon", nil }
	require.Equal(t, "UTC", resolveSourceTimezone(time.UTC, readlink, platformSourceTimezone))
	require.Equal(t, "Europe/Lisbon", resolveSourceTimezone(time.FixedZone("Local", 3600), readlink, platformSourceTimezone))
}
