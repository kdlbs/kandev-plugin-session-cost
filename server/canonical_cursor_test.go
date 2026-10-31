package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type canonicalRPCRequest struct {
	Filter pluginsdk.SessionUsageFilter
	Page   pluginsdk.Page
}

type canonicalRPCProbe struct {
	pluginsdk.UnimplementedPlugin
	host chan pluginsdk.Host
}

func (p *canonicalRPCProbe) SetHost(host pluginsdk.Host) {
	p.UnimplementedPlugin.SetHost(host)
	p.host <- host
}

// The SDK's broker harness exercises the real Host client, server and proto
// adapters, without a subprocess, a backend or a database. Assertions run on
// the test goroutine; the RPC handler only records calls and supplies replies.
func canonicalRPCFixture(t *testing.T, reply func(context.Context, canonicalRPCRequest, int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error)) (*plugin, func() []canonicalRPCRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []canonicalRPCRequest
	host := &usageRecordingHost{fakeHost: &fakeHost{
		config: map[string]any{configKeyCollect: true}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")},
	}, usage: &usageRecordingReader{}}
	host.usage.canonicalFn = func(ctx context.Context, filter pluginsdk.SessionUsageFilter, page pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		request := canonicalRPCRequest{Filter: filter, Page: page}
		mu.Lock()
		requests = append(requests, request)
		n := len(requests)
		mu.Unlock()
		return reply(ctx, request, n)
	}
	p := newPlugin()
	p.lookPath = noLookPath
	t.Cleanup(p.Close)
	p.UnimplementedPlugin.SetHost(sdkBrokerHost(t, host))
	return p, func() []canonicalRPCRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]canonicalRPCRequest(nil), requests...)
	}
}

func sdkBrokerHost(t *testing.T, host pluginsdk.Host) pluginsdk.Host {
	t.Helper()
	author := &canonicalRPCProbe{host: make(chan pluginsdk.Host, 1)}
	bridge := &pluginsdk.GRPCPlugin{Impl: author, Host: host, HostDialTimeout: 3 * time.Second}
	client, server := hcplugin.TestPluginGRPCConn(t, false, map[string]hcplugin.Plugin{pluginsdk.PluginMapKey: bridge})
	t.Cleanup(func() {
		// Close owns the controller shutdown. Calling Stop again races with
		// that handler's broker teardown in go-plugin. Await its Serve exit.
		_ = client.Close()
		<-server.DoneCh
	})
	_, err := client.Dispense(pluginsdk.PluginMapKey)
	require.NoError(t, err)
	select {
	case injected := <-author.host:
		return injected
	case <-time.After(5 * time.Second):
		t.Fatal("SDK broker did not inject the fixture Host")
		return nil
	}
}

func canonicalReadyPage(generation, next string) *pluginsdk.PageInfo {
	return &pluginsdk.PageInfo{ProjectionStatus: "ready", ProjectionGeneration: generation, HasMore: next != "", NextCursor: next}
}

func TestUsageHistoryCanonicalCursorRPCPreservesOpaqueCursorAndFilter(t *testing.T) {
	const cursor = "usage-v1.opaque+/=publication.9007199254740993"
	p, requests := canonicalRPCFixture(t, func(_ context.Context, request canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if n == 1 {
			return canonicalFixture(3), canonicalReadyPage("published.opaque-9", cursor), nil
		}
		if request.Page.Cursor != cursor {
			return nil, nil, status.Error(codes.InvalidArgument, "cursor bytes changed")
		}
		return canonicalFixture(4), canonicalReadyPage("published.opaque-9", ""), nil
	})
	filter := pluginsdk.SessionUsageFilter{
		WorkspaceID: "ws", TaskIDs: []string{"task-1"}, SessionIDs: []string{"session-1"},
		Model: "exact-model", Provider: "provider", Timezone: "Europe/Lisbon", GroupBy: "model",
		Start: optionalString("2026-10-01T00:00:00Z"), End: optionalString("2026-11-01T00:00:00Z"),
		SortBy: "model", SortDirection: "asc", IncludeUndated: true,
	}
	items, projection, err := p.readCanonicalSelection(context.Background(), filter)
	require.NoError(t, err)
	require.True(t, projection.complete())
	require.Equal(t, "published.opaque-9", projection.Generation)
	require.Len(t, items, 2)
	require.Equal(t, int64(3), *items[0].InputTokens)
	require.Equal(t, int64(4), *items[1].InputTokens)
	got := requests()
	require.Len(t, got, 2)
	filter.Canonical = true // The SDK method owns this bit on every request.
	require.Equal(t, filter, got[0].Filter)
	require.Equal(t, filter, got[1].Filter)
	require.Equal(t, pluginsdk.Page{Limit: 200}, got[0].Page)
	require.Equal(t, pluginsdk.Page{Limit: 200, Cursor: cursor}, got[1].Page)
}

func TestUsageHistoryCanonicalCursorRPCRestartsChangedPublicationFromFirstPage(t *testing.T) {
	p, requests := canonicalRPCFixture(t, func(_ context.Context, _ canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		switch n {
		case 1:
			return canonicalFixture(999), canonicalReadyPage("old.7", "usage-v1.old-page"), nil
		case 2:
			return nil, nil, status.Error(codes.FailedPrecondition, "usage publication changed; restart the saved query")
		case 3:
			return canonicalFixture(3), canonicalReadyPage("new.8", "usage-v1.new-page"), nil
		default:
			return canonicalFixture(4), canonicalReadyPage("new.8", ""), nil
		}
	})
	response, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.Equal(t, int64(7), response.Input, "the rejected publication's partial 999 must be discarded")
	require.Equal(t, "new.8", response.ProjectionGeneration)
	require.False(t, response.CanonicalPending)
	got := requests()
	require.Len(t, got, 4)
	require.Equal(t, []string{"", "usage-v1.old-page", "", "usage-v1.new-page"}, []string{got[0].Page.Cursor, got[1].Page.Cursor, got[2].Page.Cursor, got[3].Page.Cursor})
	for _, request := range got[1:] {
		require.Equal(t, got[0].Filter, request.Filter)
	}
}

func TestUsageHistoryCanonicalCursorRPCBoundsRestartsWithoutReplayingPriorPublication(t *testing.T) {
	p, requests := canonicalRPCFixture(t, func(_ context.Context, request canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if n == 1 {
			return canonicalFixture(2), canonicalReadyPage("last-complete.4", ""), nil
		}
		if request.Page.Cursor == "" {
			return canonicalFixture(999), canonicalReadyPage("changing.5", "usage-v1.changing"), nil
		}
		return nil, nil, status.Error(codes.FailedPrecondition, "publication changed")
	})
	_, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return nil, nil }
	pending, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Zero(t, pending.Input)
	require.False(t, pending.Found)
	require.False(t, pending.Saved)
	require.False(t, pending.CostKnown)
	require.Empty(t, pending.ProjectionGeneration)
	require.True(t, pending.CanonicalPending)
	require.Equal(t, reportStatePending, pending.ReportState)
	require.Zero(t, calls.Load(), "publication preparation must not invoke tokscale, including explicit refresh")
	require.Len(t, requests(), 7, "one saved read, then three attempts of two pages")
	restarted := newPlugin()
	restarted.UnimplementedPlugin.SetHost(p.Host())
	t.Cleanup(restarted.Close)
	again, found := restarted.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	require.True(t, found)
	require.True(t, again.CanonicalPending)
	require.Zero(t, again.Input, "restart cannot recover a prior publication from generic Host state")
	require.False(t, again.Found)
	require.False(t, again.Saved)
	require.False(t, again.CostKnown)
	require.Empty(t, again.ProjectionGeneration)
	require.Len(t, requests(), 13)
}

func TestUsageHistoryCanonicalCursorRPCUncachedChangesRemainUnavailable(t *testing.T) {
	p, requests := canonicalRPCFixture(t, func(context.Context, canonicalRPCRequest, int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return nil, nil, status.Error(codes.FailedPrecondition, "saved publication unavailable")
	})
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return nil, nil }
	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Len(t, requests(), 3)
	require.Zero(t, calls.Load())
	require.True(t, response.CanonicalPending)
	require.False(t, response.Found)
	require.False(t, response.CostKnown)
	require.Empty(t, response.ProjectionGeneration)
}

func TestUsageHistoryCanonicalCursorRPCPreparingRefreshDoesNotStartTokscale(t *testing.T) {
	p, requests := canonicalRPCFixture(t, func(context.Context, canonicalRPCRequest, int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		return canonicalFixture(999), &pluginsdk.PageInfo{ProjectionStatus: "preparing", ProjectionPending: true}, nil
	})
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return nil, nil }
	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Len(t, requests(), 1)
	p.toolbar.mu.Lock()
	active := p.toolbar.active
	p.toolbar.mu.Unlock()
	require.False(t, active, "preparing reads must not schedule asynchronous source work")
	require.Zero(t, calls.Load())
	require.True(t, response.CanonicalPending)
	require.False(t, response.Found)
}

func TestUsageHistoryCanonicalCursorRPCRejectsInvalidQueryWithoutCacheReplay(t *testing.T) {
	p, requests := canonicalRPCFixture(t, func(_ context.Context, _ canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if n == 1 {
			return canonicalFixture(2), canonicalReadyPage("last.1", ""), nil
		}
		if n == 2 {
			return canonicalFixture(999), canonicalReadyPage("query.2", "usage-v1.rejected-query"), nil
		}
		return nil, nil, status.Error(codes.InvalidArgument, "saved usage cursor does not match the query")
	})
	previous, _ := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
	var calls atomic.Int32
	p.run = func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return nil, nil }
	response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
	require.NoError(t, err)
	require.Len(t, requests(), 3, "an invalid continuation must not restart or expose the partial page")
	require.Equal(t, "invalid_query", response.ReportError)
	require.Equal(t, reportStateFailed, response.ReportState)
	require.False(t, response.Found)
	require.False(t, response.Saved)
	require.False(t, response.CanonicalPending)
	require.Empty(t, response.ProjectionGeneration)
	require.Zero(t, calls.Load())
	calculated := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"cost":9}]}`)
	readback := p.canonicalUsageAfterWrite(context.Background(), "ws", "task-1", "session-1", calculated, previous, &usageWriteRejectedError{status: "conflict"})
	require.False(t, readback.Found)
	require.False(t, readback.Saved)
	require.False(t, readback.CostKnown)
	require.Zero(t, readback.Input)
	require.Equal(t, "invalid_query", readback.ReportError)
}

func TestUsageHistoryCanonicalCursorRPCUnsupportedHostHasExplicitUpgradeState(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			p, requests := canonicalRPCFixture(t, func(_ context.Context, _ canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				if cached && n == 1 {
					return canonicalFixture(2), canonicalReadyPage("last.1", ""), nil
				}
				return nil, nil, status.Error(codes.Unimplemented, "older Host has no canonical usage RPC")
			})
			if cached {
				_, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
				require.True(t, found)
			}
			var calls atomic.Int32
			p.run = func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return nil, nil }
			response, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
			require.NoError(t, err)
			require.Equal(t, "upgrade_required", response.Compatibility)
			require.Equal(t, reportStateFailed, response.ReportState)
			require.False(t, response.CanonicalPending)
			require.False(t, response.Found)
			require.False(t, response.Saved)
			require.Zero(t, response.Input)
			require.False(t, response.CostKnown)
			require.Empty(t, response.ProjectionGeneration)
			expectedCalls := 1
			if cached {
				expectedCalls++
			}
			require.Len(t, requests(), expectedCalls)
			require.Zero(t, calls.Load())
		})
	}
}

func TestUsageHistoryCanonicalCursorRPCDenialAfterRestartCannotReplaySavedOrCalculatedAmounts(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.NotFound, codes.Unauthenticated} {
		t.Run(code.String(), func(t *testing.T) {
			p, requests := canonicalRPCFixture(t, func(_ context.Context, _ canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				if n == 1 {
					return canonicalFixture(2), canonicalReadyPage("authorized.1", ""), nil
				}
				if n == 2 {
					return canonicalFixture(999), canonicalReadyPage("changing.2", "usage-v1.before-denial"), nil
				}
				if n == 3 {
					return nil, nil, status.Error(codes.FailedPrecondition, "publication changed before access was revoked")
				}
				return nil, nil, status.Error(code, "scope no longer authorized")
			})
			previous, found := p.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
			require.True(t, found)
			restarted := newPlugin()
			restarted.UnimplementedPlugin.SetHost(p.Host())
			t.Cleanup(restarted.Close)
			response, found := restarted.savedSessionUsage(context.Background(), "ws", "task-1", "session-1", 1, 10)
			require.False(t, found)
			require.Nil(t, response)
			got := requests()
			require.Len(t, got, 4)
			require.Empty(t, got[3].Page.Cursor, "the denied retry must start from the first page")
			var calls atomic.Int32
			restarted.run = func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return nil, nil }
			response, err := restarted.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
			require.Equal(t, code, status.Code(err))
			require.Nil(t, response)
			action, err := restarted.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{ActionKey: actionKeySessionUsage, Context: pluginsdk.VerifiedActionContext{WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1"}})
			require.NoError(t, err)
			require.Equal(t, map[codes.Code]int{codes.PermissionDenied: 403, codes.NotFound: 404, codes.Unauthenticated: 401}[code], action.Status)
			require.JSONEq(t, `{"error":"saved usage is no longer accessible"}`, string(action.Body))
			calculated := parsedUsage(t, `{"entries":[{"sessionId":"transcript-1","model":"m","input":999,"cost":9}]}`)
			readback := restarted.canonicalUsageAfterWrite(context.Background(), "ws", "task-1", "session-1", calculated, previous, nil)
			require.False(t, readback.Found)
			require.False(t, readback.Saved)
			require.False(t, readback.CostKnown)
			require.Zero(t, readback.Input)
			require.Empty(t, readback.ProjectionGeneration)
			require.Equal(t, reportStateFailed, readback.ReportState)
			require.Equal(t, "access_denied", readback.ReportError)
			require.Zero(t, calls.Load())
		})
	}
}

func TestUsageHistoryCanonicalCursorRPCCancellationStopsContinuation(t *testing.T) {
	started := make(chan struct{})
	p, requests := canonicalRPCFixture(t, func(ctx context.Context, _ canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if n == 1 {
			return canonicalFixture(999), canonicalReadyPage("changing.1", "usage-v1.wait"), nil
		}
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := p.readCanonicalSelection(ctx, pluginsdk.SessionUsageFilter{WorkspaceID: "ws"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("continuation was not requested")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled read did not finish")
	}
	require.Len(t, requests(), 2)
}

func TestUsageHistoryCanonicalCursorRPCSharesDeadlineAcrossRestarts(t *testing.T) {
	p, requests := canonicalRPCFixture(t, func(ctx context.Context, _ canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if n == 1 {
			return nil, nil, status.Error(codes.FailedPrecondition, "publication changed")
		}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	p.hostRead = 100 * time.Millisecond
	_, _, err := p.readCanonicalSelection(context.Background(), pluginsdk.SessionUsageFilter{WorkspaceID: "ws"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, requests(), 2, "a deadline must end the whole read, without another restart")
}

func TestUsageHistoryCanonicalCursorRPCBoundsDistinctPages(t *testing.T) {
	p, requests := canonicalRPCFixture(t, func(_ context.Context, _ canonicalRPCRequest, n int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		if n > 100 {
			return nil, nil, status.Error(codes.ResourceExhausted, "fixture detected a missing page bound")
		}
		return canonicalFixture(999), canonicalReadyPage("unbounded-host.1", fmt.Sprintf("usage-v1.page-%d", n)), nil
	})
	rows, _, err := p.readCanonicalSelection(context.Background(), pluginsdk.SessionUsageFilter{WorkspaceID: "ws"})
	require.ErrorContains(t, err, "page limit")
	require.Nil(t, rows)
	require.Len(t, requests(), 100)
}

func TestUsageHistoryCanonicalCursorRPCPublishedPendingPagesSurviveRestart(t *testing.T) {
	const cursor = "usage-v1.published-pending"
	p, requests := canonicalRPCFixture(t, func(_ context.Context, request canonicalRPCRequest, _ int) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
		info := canonicalReadyPage("authorized-publication.7", "")
		info.ProjectionPending = true
		if request.Page.Cursor == "" {
			info.HasMore, info.NextCursor = true, cursor
			return canonicalFixture(2), info, nil
		}
		if request.Page.Cursor != cursor {
			return nil, nil, status.Error(codes.InvalidArgument, "cursor changed")
		}
		return canonicalFixture(3), info, nil
	})
	for _, instance := range []*plugin{p, newPlugin()} {
		instance.UnimplementedPlugin.SetHost(p.Host())
		t.Cleanup(instance.Close)
		response, err := instance.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", true)
		require.NoError(t, err)
		require.True(t, response.Saved)
		require.True(t, response.Found)
		require.Equal(t, int64(5), response.Input)
		require.InDelta(t, 0.05, response.Cost, 1e-12)
		require.Equal(t, "authorized-publication.7", response.ProjectionGeneration)
		require.True(t, response.CanonicalPending)
		require.Equal(t, reportStatePending, response.ReportState)
		instance.toolbar.mu.Lock()
		active := instance.toolbar.active
		instance.toolbar.mu.Unlock()
		require.False(t, active)
	}
	require.Len(t, requests(), 4, "each process must read both authorized Host pages without a durable amount snapshot")
}
