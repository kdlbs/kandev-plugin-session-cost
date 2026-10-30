package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

func TestReviewCollectorRolloverStabilizesObservedDayAcrossRestart(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%v", terminal), func(t *testing.T) {
			t.Setenv("TZ", "UTC")
			now := time.Date(2026, 12, 31, 23, 58, 0, 0, time.UTC)
			usage := &usageRecordingReader{}
			host := &collectorHost{&usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{{ID: "session-1", TaskID: "task-1", ACPSessionID: "transcript-1", State: "RUNNING", UpdatedAt: now.Format(time.RFC3339)}}}, usage: usage}}
			var dates []string
			current := 5
			makePlugin := func() *plugin {
				p := newPlugin()
				p.UnimplementedPlugin.SetHost(host)
				p.lookPath = noLookPath
				p.now = func() time.Time { return now }
				p.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
					date := ""
					for i, arg := range args {
						if arg == "--since" {
							date = args[i+1]
						}
					}
					input := 10
					if date != "" {
						dates = append(dates, date)
					}
					if now.Year() == 2027 {
						input = 20 + current
						if date == "2026-12-31" {
							input = 20
						}
						if date == "2027-01-01" {
							input = current
						}
					}
					return []byte(fmt.Sprintf(`{"entries":[{"sessionId":"transcript-1","model":"m","input":%d,"total":%d}]}`, input, input)), nil
				}
				t.Cleanup(p.Close)
				return p
			}
			p := makePlugin()
			require.NoError(t, p.collectOnce(context.Background()))
			require.Equal(t, []string{"2026-12-31"}, dates)
			now = time.Date(2027, 1, 1, 0, 3, 0, 0, time.UTC)
			if terminal {
				host.sessions[0].State = "COMPLETED"
			}
			host.sessions[0].UpdatedAt = now.Format(time.RFC3339)
			require.NoError(t, p.collectOnce(context.Background()))
			p.Close()
			p = makePlugin()
			if terminal {
				host.sessions = nil
			}
			for tick := 0; tick < 6; tick++ {
				now = now.Add(5 * time.Minute)
				if !terminal {
					current++
					host.sessions[0].UpdatedAt = now.Format(time.RFC3339)
				}
				require.NoError(t, p.collectOnce(context.Background()))
			}
			usage.mu.Lock()
			defer usage.mu.Unlock()
			var previous, today *pluginsdk.SessionUsageMeasurement
			for i := range usage.items {
				item := &usage.items[i]
				if item.SourceDate == "2026-12-31" {
					previous = item
				}
				if item.SourceDate == "2027-01-01" {
					today = item
				}
			}
			require.NotNil(t, previous)
			require.Equal(t, int64(20), *previous.InputTokens, "late pre-midnight usage must correct the dated bucket, not only lifetime")
			require.Equal(t, int64(2), previous.Revision)
			require.NotNil(t, today)
			if !terminal {
				require.Greater(t, *today.InputTokens, int64(5), "current-day amounts must progress while the previous day finalizes")
			}
			oldCalls := 0
			for _, date := range dates {
				require.Contains(t, []string{"2026-12-31", "2027-01-01"}, date)
				if date == "2026-12-31" {
					oldCalls++
				}
			}
			require.Equal(t, 1+maxFinalCollectionAttempts, oldCalls, "observed rollover work must finish after bounded stabilization")
		})
	}
}
