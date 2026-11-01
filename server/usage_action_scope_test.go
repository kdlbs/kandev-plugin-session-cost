package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type usageActionLegacyHost struct {
	*fakeHost
	usage *usageRecordingReader
}

func (h *usageActionLegacyHost) Usage() pluginsdk.UsageReader { return h.usage }

func TestSessionUsageActionReauthorizesCurrentBindingBeforeCanonicalRead(t *testing.T) {
	for _, tc := range []struct {
		name          string
		workspace     string
		changed       bool
		wantStatus    int
		wantReadCount int32
	}{
		{name: "authorized current binding", workspace: "ws", wantStatus: 200, wantReadCount: 1},
		{name: "foreign workspace has no binding", workspace: "other-ws", wantStatus: 404},
		{name: "cached binding was replaced", workspace: "ws", changed: true, wantStatus: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queried pluginsdk.UsageCollectionTargetQueryV1
			var canonicalReads atomic.Int32
			reader := retainedProtocolFixture{list: func(_ context.Context, query pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				queried = query
				if query.WorkspaceID != "ws" {
					return pluginsdk.UsageCollectionTargetPageV1{CurrentTarget: true}, nil
				}
				target := testTarget()
				if tc.changed {
					target.SubjectBinding = "replacement-binding"
				}
				return fixtureCurrentTargetPage(query, pluginsdk.UsageCollectionTargetPageV1{Targets: []pluginsdk.UsageCollectionTargetV1{target}}), nil
			}}
			host := &retainedFixtureHost{
				usageRecordingHost: &usageRecordingHost{
					fakeHost: &fakeHost{config: map[string]any{configKeyCollect: false}, sessions: []pluginsdk.Session{session("session-1", "transcript-1")}},
					usage:    &usageRecordingReader{},
				},
				reader: reader,
			}
			host.usage.canonicalFn = func(_ context.Context, filter pluginsdk.SessionUsageFilter, _ pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				canonicalReads.Add(1)
				require.Equal(t, "ws", filter.WorkspaceID)
				require.Equal(t, []string{"task-1"}, filter.TaskIDs)
				require.Equal(t, []string{"session-1"}, filter.SessionIDs)
				return canonicalFixture(15), canonicalReadyPage("verified-action.1", ""), nil
			}
			p := newPlugin()
			p.lookPath = noLookPath
			p.UnimplementedPlugin.SetHost(host)
			t.Cleanup(p.Close)
			if tc.changed {
				require.NoError(t, p.writeUsageState(context.Background(), "usage-target-v1", targetCacheKey("ws", "task-1", "session-1"), retainedUsageTarget{UsageCollectionTargetV1: testTarget()}))
			}

			response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
				ActionKey: actionKeySessionUsage,
				Context:   pluginsdk.VerifiedActionContext{WorkspaceID: tc.workspace, TaskID: "task-1", SessionID: "session-1"},
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, response.Status)
			require.Equal(t, tc.wantReadCount, canonicalReads.Load(), "canonical data must not be read before current binding authorization")
			if tc.wantReadCount != 0 {
				result := decode(t, response.Body)
				require.True(t, result.Found)
				require.Equal(t, int64(15), result.Input)
				require.Empty(t, result.Compatibility)
			}
			require.Equal(t, tc.workspace, queried.WorkspaceID)
			require.NotNil(t, queried.CurrentTarget)
			require.Equal(t, "task-1", queried.CurrentTarget.TaskID)
			require.Equal(t, "session-1", queried.CurrentTarget.SessionID)
			require.Empty(t, queried.SnapshotID)
			require.Empty(t, queried.Cursor)
			if tc.changed {
				require.Equal(t, "retained-core-binding", queried.CurrentTarget.SubjectBinding)
			}
		})
	}
}

func TestSessionUsageActionUnsupportedHostReturnsUpgradeRequiredWithoutReadingUsage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		missingAPI    bool
		list          func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error)
		wantStatus    int
		wantUpgrade   bool
		privateDetail string
	}{
		{
			name:        "Host has no retained usage interface",
			missingAPI:  true,
			wantStatus:  200,
			wantUpgrade: true,
		},
		{
			name: "current target query is unimplemented",
			list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.Unimplemented, "private unsupported detail")
			},
			wantStatus:    200,
			wantUpgrade:   true,
			privateDetail: "private unsupported detail",
		},
		{
			name: "older Host does not acknowledge current target query",
			list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				return pluginsdk.UsageCollectionTargetPageV1{Targets: []pluginsdk.UsageCollectionTargetV1{testTarget()}}, nil
			},
			wantStatus:  200,
			wantUpgrade: true,
		},
		{
			name: "permission denial remains inaccessible",
			list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				return pluginsdk.UsageCollectionTargetPageV1{}, status.Error(codes.PermissionDenied, "private authorization detail")
			},
			wantStatus:    403,
			privateDetail: "private authorization detail",
		},
		{
			name: "malformed acknowledged page remains inaccessible",
			list: func(context.Context, pluginsdk.UsageCollectionTargetQueryV1) (pluginsdk.UsageCollectionTargetPageV1, error) {
				return pluginsdk.UsageCollectionTargetPageV1{CurrentTarget: true, SnapshotID: "private-snapshot"}, nil
			},
			wantStatus:    403,
			privateDetail: "private-snapshot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var canonicalReads, commandLookups, commandRuns atomic.Int32
			var p *plugin
			var usage *usageRecordingReader
			if tc.missingAPI {
				legacyHost := &usageActionLegacyHost{
					fakeHost: &fakeHost{config: map[string]any{configKeyCollect: true}},
					usage:    &usageRecordingReader{},
				}
				usage = legacyHost.usage
				p = newPlugin()
				p.UnimplementedPlugin.SetHost(legacyHost)
				t.Cleanup(p.Close)
			} else {
				var fixture *retainedFixtureHost
				p, fixture = historyPlugin(t, exactHistoryFixture{list: tc.list})
				usage = fixture.usage
			}
			usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				canonicalReads.Add(1)
				return canonicalFixture(987654), canonicalReadyPage("private-generation", ""), nil
			}
			p.lookPath = func(string) (string, error) {
				commandLookups.Add(1)
				return "tokscale", nil
			}
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				commandRuns.Add(1)
				return []byte(`{"entries":[{"sessionId":"transcript-1","model":"private-model","input":999999,"cost":999}]}`), nil
			}

			response, err := p.HandleAction(context.Background(), &pluginsdk.PluginActionRequest{
				ActionKey: actionKeySessionUsage,
				Context:   pluginsdk.VerifiedActionContext{WorkspaceID: "ws", TaskID: "task-1", SessionID: "session-1"},
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, response.Status)
			require.Zero(t, canonicalReads.Load(), "unsupported and inaccessible scopes must not read canonical usage")
			require.Zero(t, commandLookups.Load(), "the action guard must run before looking up a source command")
			require.Zero(t, commandRuns.Load(), "the action guard must run before executing a source command")
			if tc.wantUpgrade {
				result := decode(t, response.Body)
				require.Equal(t, "upgrade_required", result.Compatibility)
				require.Equal(t, reportStateFailed, result.ReportState)
				require.Equal(t, reportErrorUnavailable, result.ReportError)
				require.False(t, result.Found)
				require.False(t, result.Saved)
				require.False(t, result.CostKnown)
				require.Zero(t, result.Cost)
				require.Zero(t, result.Input)
				require.Zero(t, result.Output)
				require.Zero(t, result.Total)
				require.Zero(t, result.Turns)
				require.Zero(t, result.CostPerTurn)
				require.Empty(t, result.Models)
				require.NotContains(t, string(response.Body), "private-")
				require.NotContains(t, string(response.Body), "999")
			} else {
				require.Contains(t, string(response.Body), "saved usage is no longer accessible")
				require.NotContains(t, string(response.Body), tc.privateDetail)
			}
		})
	}
}
