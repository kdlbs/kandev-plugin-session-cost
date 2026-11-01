package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type reviewedArchiveHost struct {
	*usageRecordingHost
	lists    atomic.Int32
	keyLists atomic.Int32
	keyError error
}

func (h *reviewedArchiveHost) ListState(context.Context, string, string) ([]pluginsdk.StateEntry, error) {
	h.lists.Add(1)
	return nil, status.Error(codes.ResourceExhausted, "whole-archive enumeration is forbidden")
}

func (h *reviewedArchiveHost) StateKeys() pluginsdk.StateKeyReader {
	return stateKeyReaderFunc(func(ctx context.Context, query pluginsdk.StateKeyQuery) (pluginsdk.StateKeyPage, error) {
		h.keyLists.Add(1)
		if h.keyError != nil {
			return pluginsdk.StateKeyPage{}, h.keyError
		}
		return h.fakeHost.listStateKeys(ctx, query)
	})
}

func TestReviewSDKBrokerRestartUsesDurablePrivacyMarkerWithoutArchiveScan(t *testing.T) {
	host := &reviewedArchiveHost{usageRecordingHost: &usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")}}, usage: &usageRecordingReader{}}}
	require.NoError(t, host.SetState(context.Background(), "plugin", "usage-state-privacy-v2", "completed", map[string]any{"json": `{"Version":2,"Complete":true}`}))
	for i := 0; i < 500; i++ {
		require.NoError(t, host.SetState(context.Background(), "plugin", "usage-observation-v1", fmt.Sprintf("receipt-%d", i), map[string]any{"json": `{"Version":2,"Accepted":true,"Revision":1,"PayloadDigest":"opaque"}`}))
	}
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(2), canonicalReadyPage("published.1", ""), nil
	}
	bridge := sdkBrokerHost(t, host)
	for process := 0; process < 2; process++ {
		p := newPlugin()
		p.SetHost(bridge)
		t.Cleanup(p.Close)
		p.collectorMu.Lock()
		started := p.collectorStarted
		p.collectorMu.Unlock()
		require.True(t, started)
		saved, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
		require.NoError(t, err)
		require.Equal(t, int64(2), saved.Input)
		require.True(t, saved.Saved)
		require.NoError(t, p.persistSessionUsage(context.Background(), "ws", "task-1", "session-1", parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":7}]}`)))
		p.Close()
	}
	require.Zero(t, host.lists.Load(), "neither startup nor saved reads may enumerate current-format archive receipts")
	require.Zero(t, host.keyLists.Load(), "the durable completion marker also avoids enumerating archive keys")
}

func TestReviewSDKBrokerPendingLegacyMigrationDoesNotBlockCanonicalRead(t *testing.T) {
	host := &reviewedArchiveHost{usageRecordingHost: &usageRecordingHost{fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}}, usage: &usageRecordingReader{}}, keyError: status.Error(codes.PermissionDenied, "state capability denied")}
	host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(2), canonicalReadyPage("published.1", ""), nil
	}
	p := newPlugin()
	p.SetHost(sdkBrokerHost(t, host))
	t.Cleanup(p.Close)
	saved, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
	require.NoError(t, err)
	require.True(t, saved.Saved)
	require.Equal(t, int64(2), saved.Input)
	require.Zero(t, host.lists.Load(), "an unavailable pager must never trigger a whole-archive fallback")
	require.Equal(t, int32(1), host.keyLists.Load(), "failed startup cleanup must not be repeated by canonical reads")
	p.collectorMu.Lock()
	started := p.collectorStarted
	p.collectorMu.Unlock()
	require.False(t, started, "collection waits for successful cleanup")
	_, found, err := p.Host().GetState(context.Background(), "plugin", "usage-state-privacy-v2", "completed")
	require.NoError(t, err)
	require.False(t, found, "a failed legacy cleanup must never certify the archive as migrated")
}
