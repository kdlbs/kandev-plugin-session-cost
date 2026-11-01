package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// @covers AC-COSTS-USAGE-IMPORT-003.1 AC-COSTS-USAGE-IMPORT-003.2 AC-COSTS-USAGE-IMPORT-003.3 AC-COSTS-USAGE-IMPORT-003.7
func TestImportStatusPublicView(t *testing.T) {
	state := historicalImportState{
		WorkspaceID: "ws-1", Status: importStatusRunning, Phase: importPhaseDaily,
		DaysTotal: 10, DaysDone: 4, DaySecondsTotal: 120, NextDay: "2026-03-12",
		SessionsFound: 20, SessionsImported: 15, SessionsMissing: 3, SessionsSkipped: 2,
		LastSuccessfulAt: "2026-03-11T10:00:00Z", Undated: true,
	}
	view := publicImportView(state)
	require.Equal(t, "ws-1", view.WorkspaceID)
	require.Equal(t, importStatusRunning, view.Status)
	require.Equal(t, importPhaseDaily, view.Phase)
	require.Equal(t, 20, view.SessionsFound)
	require.Equal(t, 15, view.SessionsImported)
	require.Equal(t, 3, view.SessionsMissing)
	require.Equal(t, 2, view.SessionsSkipped)
	require.Equal(t, 10, view.DaysTotal)
	require.Equal(t, 4, view.DaysDone)
	require.Equal(t, "2026-03-12", view.CurrentDay)
	require.EqualValues(t, 180, view.ETASeconds, "average 30s across six remaining days")
	require.True(t, view.Undated)
	require.Equal(t, "2026-03-11T10:00:00Z", view.LastSuccessfulAt)

	encoded, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "2026-03-12T", "the status view must not expose checkpoint internals")
}

func TestImportStatusHidesEstimateBeforeThreeDays(t *testing.T) {
	for _, done := range []int{0, 1, 2} {
		_, ok := importETASeconds(historicalImportState{DaysTotal: 10, DaysDone: done, DaySecondsTotal: int64(done) * 30})
		require.False(t, ok, "no estimate with %d completed days", done)
	}
	eta, ok := importETASeconds(historicalImportState{DaysTotal: 10, DaysDone: 3, DaySecondsTotal: 90})
	require.True(t, ok)
	require.EqualValues(t, 210, eta)
}

func TestImportStatusCompletedKeepsLabelledCounts(t *testing.T) {
	view := publicImportView(historicalImportState{
		WorkspaceID: "ws-1", Status: importStatusCompleted, Phase: importPhaseCompleted,
		SessionsFound: 312, SessionsImported: 287, SessionsMissing: 21, SessionsSkipped: 4,
		FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	require.Equal(t, importStatusCompleted, view.Status)
	require.Equal(t, 312, view.SessionsFound)
	require.Equal(t, 287, view.SessionsImported)
	require.Equal(t, 21, view.SessionsMissing)
	require.Equal(t, 4, view.SessionsSkipped)
	require.Empty(t, view.CurrentDay)
	require.Zero(t, view.ETASeconds)
}

func TestImportStatusFixedPublicError(t *testing.T) {
	view := publicImportView(historicalImportState{WorkspaceID: "ws-1", Status: importStatusFailed, LastError: "raw disk path /private/secret"})
	require.Equal(t, historicalImportFailureMessage, view.LastError)
	require.NotContains(t, view.LastError, "private")
}

func TestImportStatusDiscreteStatuses(t *testing.T) {
	for _, status := range []string{
		importStatusNotStarted, importStatusPreparing, importStatusQueued, importStatusRunning,
		importStatusCompleted, importStatusFailed, importStatusCancelled, importStatusDisabled, importStatusUnsupported,
	} {
		view := publicImportView(historicalImportState{WorkspaceID: "ws-1", Status: status})
		require.Equal(t, status, view.Status)
	}
}
