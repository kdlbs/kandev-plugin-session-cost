package main

import (
	"context"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAuthorizedImportCheckpointDiscardsOnlyConfirmedScopeChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        codes.Code
		wantDiscard bool
	}{
		{name: "changed snapshot", code: codes.FailedPrecondition, wantDiscard: true},
		{name: "deleted snapshot", code: codes.NotFound, wantDiscard: true},
		{name: "denied workspace", code: codes.PermissionDenied, wantDiscard: true},
		{name: "temporary unauthenticated read", code: codes.Unauthenticated},
		{name: "invalid snapshot request", code: codes.InvalidArgument},
		{name: "temporary host failure", code: codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := historyPlugin(t, retainedProtocolFixture{list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(tc.code, "private host detail")
			}})
			state := historicalImportState{
				SchemaVersion: importSchemaVersion, WorkspaceID: "ws", RunID: "run-1",
				SnapshotID: "snapshot-1", SnapshotUpperBound: 7, Status: importStatusRunning,
			}
			require.NoError(t, p.writeHistoricalImportState(context.Background(), "ws", state, true))

			_, found, err := p.authorizedHistoricalImportState(context.Background(), "ws")
			if tc.wantDiscard {
				require.NoError(t, err)
				require.False(t, found)
			} else {
				require.Error(t, err)
				require.False(t, found)
			}
			_, persisted, loadErr := p.loadHistoricalImportState(context.Background(), "ws")
			require.NoError(t, loadErr)
			require.Equal(t, !tc.wantDiscard, persisted, "transient authorization failures must preserve the checkpoint")
		})
	}
}
