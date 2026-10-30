package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type migrationArchiveHost struct {
	*retainedFixtureHost
	mu           sync.Mutex
	queries      []pluginsdk.StateKeyQuery
	wholeLists   int
	pointReads   map[string]int
	deletes      map[string]int
	keyError     error
	beforeGet    func(string, string) error
	beforeDelete func(string, string) error
	beforeSet    func(string, string, map[string]any) error
}

func newMigrationArchiveHost() *migrationArchiveHost {
	return &migrationArchiveHost{
		retainedFixtureHost: &retainedFixtureHost{
			usageRecordingHost: &usageRecordingHost{fakeHost: &fakeHost{}, usage: &usageRecordingReader{}},
			reader: retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
				return acceptedBatch(batch), nil
			}},
		},
		pointReads: make(map[string]int), deletes: make(map[string]int),
	}
}

func (h *migrationArchiveHost) StateKeys() pluginsdk.StateKeyReader {
	return stateKeyReaderFunc(func(ctx context.Context, query pluginsdk.StateKeyQuery) (pluginsdk.StateKeyPage, error) {
		h.mu.Lock()
		h.queries = append(h.queries, query)
		err := h.keyError
		h.mu.Unlock()
		if err != nil {
			return pluginsdk.StateKeyPage{}, err
		}
		return h.fakeHost.listStateKeys(ctx, query)
	})
}

func (h *migrationArchiveHost) ListState(context.Context, string, string) ([]pluginsdk.StateEntry, error) {
	h.mu.Lock()
	h.wholeLists++
	h.mu.Unlock()
	return nil, status.Error(codes.ResourceExhausted, "unbounded archive reads are forbidden")
}

func (h *migrationArchiveHost) GetState(ctx context.Context, scope, id, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	h.pointReads[id+"/"+key]++
	h.mu.Unlock()
	if h.beforeGet != nil {
		if err := h.beforeGet(id, key); err != nil {
			return nil, false, err
		}
	}
	return h.fakeHost.GetState(ctx, scope, id, key)
}

func (h *migrationArchiveHost) DeleteState(ctx context.Context, scope, id, key string) error {
	h.mu.Lock()
	h.deletes[id+"/"+key]++
	h.mu.Unlock()
	if h.beforeDelete != nil {
		if err := h.beforeDelete(id, key); err != nil {
			return err
		}
	}
	return h.fakeHost.DeleteState(ctx, scope, id, key)
}

func (h *migrationArchiveHost) SetState(ctx context.Context, scope, id, key string, value map[string]any) error {
	if h.beforeSet != nil {
		if err := h.beforeSet(id, key, value); err != nil {
			return err
		}
	}
	return h.fakeHost.SetState(ctx, scope, id, key, value)
}

type reviewMigrationProgress struct {
	Version        int
	Complete       bool
	Namespace      int
	Cursor         string
	PageLoaded     bool
	PendingKeys    []string
	PageNextCursor string
}

func migrationProgress(t *testing.T, host pluginsdk.Host) reviewMigrationProgress {
	t.Helper()
	entry, found, err := host.GetState(context.Background(), "plugin", "usage-state-privacy-v2", "completed")
	require.NoError(t, err)
	require.True(t, found)
	var progress reviewMigrationProgress
	require.NoError(t, json.Unmarshal([]byte(entry["json"].(string)), &progress))
	return progress
}

func migrationPlugin(t *testing.T, host pluginsdk.Host) *plugin {
	t.Helper()
	p := newPlugin()
	p.UnimplementedPlugin.SetHost(host)
	t.Cleanup(p.Close)
	return p
}

func legacyReceiptJSON(t *testing.T, padding string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"ContentDigest": "content-exact", "Accepted": false, "untrustedPayload": padding,
		"Observation": pluginsdk.UsageObservationV1{SubjectBinding: "binding-exact", ItemID: "item-exact", Revision: 4, PayloadDigest: "payload-exact", Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(987654321)}},
	})
	require.NoError(t, err)
	return string(encoded)
}

func TestReviewSDKBrokerLegacyCleanupPagesLargeArchiveAndResumesEachKey(t *testing.T) {
	host := newMigrationArchiveHost()
	ctx := context.Background()
	for i := 0; i < 453; i++ {
		key := fmt.Sprintf("c%03d", i-1)
		if i == 0 {
			key = "" // The continuation contract includes empty keys.
		}
		canonical := "legacy accounting"
		if i == 210 {
			canonical = strings.Repeat("x", 3*1024*1024)
		}
		require.NoError(t, host.fakeHost.SetState(ctx, "plugin", "canonical-usage-v1", key, map[string]any{"json": canonical}))
		padding := ""
		if i == 210 || i == 211 {
			padding = strings.Repeat("y", 3*1024*1024)
		}
		require.NoError(t, host.fakeHost.SetState(ctx, "plugin", "usage-observation-v1", fmt.Sprintf("r%03d", i), map[string]any{"json": legacyReceiptJSON(t, padding)}))
	}
	// Other namespaces and scopes are never part of this cleanup.
	require.NoError(t, host.fakeHost.SetState(ctx, "workspace", "canonical-usage-v1", "private", map[string]any{"json": "untouched"}))
	require.NoError(t, host.fakeHost.SetState(ctx, "plugin", "other", "private", map[string]any{"json": "untouched"}))
	var failDelete atomic.Bool
	failDelete.Store(true)
	host.beforeDelete = func(id, key string) error {
		if id == "canonical-usage-v1" && key == "c099" && failDelete.Swap(false) {
			return status.Error(codes.Unavailable, "interrupted delete")
		}
		return nil
	}
	bridge := sdkBrokerHost(t, host)
	p := migrationPlugin(t, bridge)
	err := p.ensureUsageStatePrivacy(ctx)
	require.Equal(t, codes.Unavailable, status.Code(err), "cleanup must use bounded key enumeration before any archive value read")
	progress := migrationProgress(t, bridge)
	require.False(t, progress.Complete)
	require.Zero(t, progress.Namespace)
	require.Empty(t, progress.Cursor, "the page continuation is uncommitted while any page key remains")
	require.True(t, progress.PageLoaded)
	require.Len(t, progress.PendingKeys, 100)
	require.Equal(t, "c099", progress.PendingKeys[0])
	require.NotEmpty(t, progress.PageNextCursor)
	p.Close()
	// A key may disappear between enumeration and point cleanup.
	require.NoError(t, host.fakeHost.DeleteState(ctx, "plugin", "canonical-usage-v1", "c100"))
	var failRead atomic.Bool
	failRead.Store(true)
	host.beforeGet = func(id, key string) error {
		if id == "usage-observation-v1" && key == "r210" && failRead.Swap(false) {
			return status.Error(codes.Unavailable, "interrupted receipt read")
		}
		return nil
	}
	var sawReadFailure bool
	for process := 0; process < 10; process++ {
		p = migrationPlugin(t, bridge)
		err = p.ensureUsageStatePrivacy(ctx)
		p.Close()
		progress = migrationProgress(t, bridge)
		if status.Code(err) == codes.Unavailable {
			sawReadFailure = true
			require.Equal(t, 1, progress.Namespace)
			require.Equal(t, "r210", progress.PendingKeys[0])
			// Losing an enumerated receipt must not block or skip its successor.
			require.NoError(t, host.fakeHost.DeleteState(ctx, "plugin", "usage-observation-v1", "r211"))
		} else if !progress.Complete {
			require.Error(t, err)
		}
		if progress.Complete {
			require.NoError(t, err)
			break
		}
	}
	require.True(t, progress.Complete, "finite per-call work must finish a finite archive across restarts")
	require.True(t, sawReadFailure)
	require.Empty(t, progress.PendingKeys)
	assertNoAccountingAmountsInState(t, host.fakeHost)
	value, found, err := host.fakeHost.GetState(ctx, "plugin", "usage-observation-v1", "r210")
	require.NoError(t, err)
	require.True(t, found)
	var receipt usageObservationCheckpoint
	require.NoError(t, json.Unmarshal([]byte(value["json"].(string)), &receipt))
	require.Equal(t, usageObservationCheckpoint{Version: 2, ContentDigest: "content-exact", SubjectBinding: "binding-exact", ItemID: "item-exact", Revision: 4, PayloadDigest: "payload-exact", Accepted: false}, receipt)
	for _, namespace := range []struct{ scope, id string }{{"workspace", "canonical-usage-v1"}, {"plugin", "other"}} {
		value, found, err := host.fakeHost.GetState(ctx, namespace.scope, namespace.id, "private")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "untouched", value["json"])
	}
	host.mu.Lock()
	require.Zero(t, host.wholeLists)
	require.Len(t, host.queries, 6, "each namespace has three key pages; a partial page is durable, not re-enumerated")
	for i, query := range host.queries {
		require.Equal(t, "plugin", query.Scope)
		require.Equal(t, 200, query.Limit)
		if i%3 == 0 {
			require.Empty(t, query.Cursor)
		} else {
			require.Contains(t, query.Cursor, "keys.fixture+/=")
		}
	}
	require.Equal(t, 1, host.deletes["canonical-usage-v1/c000"], "acknowledged keys are not replayed after restart")
	require.Equal(t, 2, host.deletes["canonical-usage-v1/c099"], "the failed key is retried")
	require.Zero(t, host.pointReads["canonical-usage-v1/c000"], "legacy snapshots are deleted without loading their large values")
	host.mu.Unlock()
	// Completed archives use one point marker read, even if the pager is later unavailable.
	host.keyError = status.Error(codes.PermissionDenied, "state keys denied")
	p = migrationPlugin(t, bridge)
	require.NoError(t, p.ensureUsageStatePrivacy(ctx))
	host.mu.Lock()
	require.Len(t, host.queries, 6)
	host.mu.Unlock()
}

func TestReviewSDKBrokerLegacyCleanupRetriesUnacknowledgedSanitization(t *testing.T) {
	host := newMigrationArchiveHost()
	ctx := context.Background()
	require.NoError(t, host.fakeHost.SetState(ctx, "plugin", "usage-observation-v1", "receipt", map[string]any{"json": legacyReceiptJSON(t, "secret amounts")}))
	var failProgress atomic.Bool
	failProgress.Store(true)
	host.beforeSet = func(id, _ string, value map[string]any) error {
		if id == "usage-state-privacy-v2" {
			var progress reviewMigrationProgress
			if json.Unmarshal([]byte(value["json"].(string)), &progress) == nil && progress.Namespace == 1 && progress.PageLoaded && len(progress.PendingKeys) == 0 && failProgress.Swap(false) {
				return status.Error(codes.Unavailable, "checkpoint write interrupted after sanitization")
			}
		}
		return nil
	}
	bridge := sdkBrokerHost(t, host)
	p := migrationPlugin(t, bridge)
	require.Equal(t, codes.Unavailable, status.Code(p.ensureUsageStatePrivacy(ctx)))
	progress := migrationProgress(t, bridge)
	require.Equal(t, []string{"receipt"}, progress.PendingKeys)
	require.False(t, progress.Complete)
	assertNoAccountingAmountsInState(t, host.fakeHost)
	p.Close()
	p = migrationPlugin(t, bridge)
	require.NoError(t, p.ensureUsageStatePrivacy(ctx))
	require.True(t, migrationProgress(t, bridge).Complete)
	host.mu.Lock()
	require.Equal(t, 2, host.pointReads["usage-observation-v1/receipt"], "an unacknowledged metadata-only rewrite is safe to repeat")
	require.Len(t, host.queries, 2, "a restart consumes the durable page rather than enumerating it again")
	require.Zero(t, host.wholeLists)
	host.mu.Unlock()
}

// Embedding just Host deliberately hides the optional state-key capability.
type oldStateHost struct{ pluginsdk.Host }

func TestReviewSDKBrokerLegacyCleanupCapabilityAndUnsupportedDoNotFallback(t *testing.T) {
	for _, mode := range []string{"old-local", "old-remote", "denied"} {
		t.Run(mode, func(t *testing.T) {
			host := newMigrationArchiveHost()
			require.NoError(t, host.fakeHost.SetState(context.Background(), "plugin", "canonical-usage-v1", "orphan", map[string]any{"json": "recoverable legacy accounting"}))
			var connected pluginsdk.Host = host
			if mode == "denied" {
				host.keyError = status.Error(codes.PermissionDenied, "state capability denied")
			} else {
				connected = oldStateHost{host}
			}
			if mode != "old-local" {
				connected = sdkBrokerHost(t, connected)
			}
			p := migrationPlugin(t, connected)
			err := p.ensureUsageStatePrivacy(context.Background())
			require.Error(t, err)
			if mode == "denied" {
				require.Equal(t, codes.PermissionDenied, status.Code(err))
			} else {
				require.Contains(t, err.Error(), "upgrade")
			}
			_, found, err := host.fakeHost.GetState(context.Background(), "plugin", "usage-state-privacy-v2", "completed")
			require.NoError(t, err)
			require.False(t, found, "an unsupported or denied cleanup cannot certify a legacy archive")
			_, found, err = host.fakeHost.GetState(context.Background(), "plugin", "canonical-usage-v1", "orphan")
			require.NoError(t, err)
			require.True(t, found, "cleanup must stay pending rather than abandoning legacy accounting payloads")
			host.mu.Lock()
			require.Zero(t, host.wholeLists)
			host.mu.Unlock()
		})
	}
}

func TestReviewSDKBrokerLegacyCleanupCancelsWithoutSkippingPendingKey(t *testing.T) {
	host := newMigrationArchiveHost()
	require.NoError(t, host.fakeHost.SetState(context.Background(), "plugin", "usage-observation-v1", "receipt", map[string]any{"json": legacyReceiptJSON(t, "private")}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host.beforeGet = func(id, _ string) error {
		if id == "usage-observation-v1" {
			cancel()
			return status.Error(codes.Canceled, "cleanup canceled during point read")
		}
		return nil
	}
	bridge := sdkBrokerHost(t, host)
	p := migrationPlugin(t, bridge)
	require.Error(t, p.ensureUsageStatePrivacy(ctx))
	progress := migrationProgress(t, bridge)
	require.False(t, progress.Complete)
	require.True(t, progress.PageLoaded)
	require.Equal(t, []string{"receipt"}, progress.PendingKeys)
	p.Close()
	host.beforeGet = nil
	p = migrationPlugin(t, bridge)
	require.NoError(t, p.ensureUsageStatePrivacy(context.Background()))
	require.True(t, migrationProgress(t, bridge).Complete)
	assertNoAccountingAmountsInState(t, host.fakeHost)
	host.mu.Lock()
	require.Len(t, host.queries, 2, "cancellation preserves the loaded page for the next process")
	require.Zero(t, host.wholeLists)
	host.mu.Unlock()
}

func TestReviewSDKBrokerLegacyCleanupStripsMalformedAndHiddenPayloads(t *testing.T) {
	host := newMigrationArchiveHost()
	for key, value := range map[string]map[string]any{
		"broken":  {"json": "malformed accounting payload"},
		"missing": {"private": "accounting payload"},
		"v2":      {"json": `{"Version":2,"ContentDigest":"unchanged","Revision":7,"PayloadDigest":"exact","Accepted":true,"cost":12345,"private":{"input":987654321}}`},
	} {
		require.NoError(t, host.fakeHost.SetState(context.Background(), "plugin", "usage-observation-v1", key, value))
	}
	p := migrationPlugin(t, sdkBrokerHost(t, host))
	require.NoError(t, p.ensureUsageStatePrivacy(context.Background()))
	assertNoAccountingAmountsInState(t, host.fakeHost)
	entries, err := host.fakeHost.ListState(context.Background(), "plugin", "usage-observation-v1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "v2", entries[0].Key)
	var receipt usageObservationCheckpoint
	require.NoError(t, json.Unmarshal([]byte(entries[0].Value["json"].(string)), &receipt))
	require.Equal(t, usageObservationCheckpoint{Version: 2, ContentDigest: "unchanged", Revision: 7, PayloadDigest: "exact", Accepted: true}, receipt)
}

func TestReviewSDKBrokerLegacyCleanupPreservesUncertainReceiptReplay(t *testing.T) {
	host := newMigrationArchiveHost()
	host.config = map[string]any{configKeyCollect: true}
	observation := pluginsdk.UsageObservationV1{SubjectBinding: "binding", ItemID: "item", SourceRecordID: "record", Tokens: &pluginsdk.UsageTokenAmountsV1{InputTokens: ptr(987654321)}}
	content, err := observation.ComputePayloadDigestV1()
	require.NoError(t, err)
	observation.Revision = 4
	observation.PayloadDigest, err = observation.ComputePayloadDigestV1()
	require.NoError(t, err)
	legacy, err := json.Marshal(map[string]any{"ContentDigest": content, "Accepted": false, "Observation": observation})
	require.NoError(t, err)
	key := stableUsageID(observation.SubjectBinding, observation.ItemID)
	require.NoError(t, host.fakeHost.SetState(context.Background(), "plugin", "usage-observation-v1", key, map[string]any{"json": string(legacy)}))
	var mu sync.Mutex
	var submitted []pluginsdk.UsageObservationV1
	host.reader = retainedProtocolFixture{report: func(_ context.Context, batch pluginsdk.UsageObservationBatchV1) (pluginsdk.UsageObservationBatchResultV1, error) {
		mu.Lock()
		submitted = append(submitted, batch.Observations...)
		mu.Unlock()
		return acceptedBatch(batch), nil
	}}
	bridge := sdkBrokerHost(t, host)
	p := migrationPlugin(t, bridge)
	require.NoError(t, p.ensureUsageStatePrivacy(context.Background()))
	assertNoAccountingAmountsInState(t, host.fakeHost)
	require.NoError(t, p.reportUsageObservations(context.Background(), []pluginsdk.UsageObservationV1{observation}))
	p.Close()
	p = migrationPlugin(t, bridge)
	require.NoError(t, p.reportUsageObservations(context.Background(), []pluginsdk.UsageObservationV1{observation}))
	mu.Lock()
	require.Equal(t, []pluginsdk.UsageObservationV1{observation}, submitted, "the uncertain revision and digest replay exactly once, then accepted state survives restart")
	mu.Unlock()
	assertNoAccountingAmountsInState(t, host.fakeHost)
}

func TestReviewSDKBrokerLegacyCleanupContinuesAfterBoundedStartupAndStopsOnClose(t *testing.T) {
	for _, closeBeforeContinue := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%t", closeBeforeContinue), func(t *testing.T) {
			host := newMigrationArchiveHost()
			host.config = map[string]any{configKeyCollect: true}
			host.usage.canonicalFn = func(context.Context, pluginsdk.SessionUsageFilter, pluginsdk.Page) ([]pluginsdk.SessionUsageMeasurement, *pluginsdk.PageInfo, error) {
				return canonicalFixture(2), canonicalReadyPage("published.1", ""), nil
			}
			for i := 0; i < 201; i++ {
				require.NoError(t, host.fakeHost.SetState(context.Background(), "plugin", "canonical-usage-v1", fmt.Sprintf("c%03d", i), map[string]any{"json": "accounting copy"}))
			}
			p := newPlugin()
			t.Cleanup(p.Close)
			continueBatch := make(chan struct{})
			workerStopped := make(chan struct{})
			p.usageCleanupWait = func(ctx context.Context) bool {
				select {
				case <-continueBatch:
					return ctx.Err() == nil
				case <-ctx.Done():
					close(workerStopped)
					return false
				}
			}
			bridge := sdkBrokerHost(t, host)
			p.SetHost(bridge)
			progress := migrationProgress(t, bridge)
			require.False(t, progress.Complete)
			require.NotEmpty(t, progress.Cursor)
			p.collectorMu.Lock()
			started := p.collectorStarted
			p.collectorMu.Unlock()
			require.False(t, started)
			saved, err := p.responsiveSessionUsage(context.Background(), "ws", "task-1", "session-1", false)
			require.NoError(t, err)
			require.True(t, saved.Saved)
			require.Equal(t, int64(2), saved.Input, "canonical reads stay usable while bounded cleanup waits to continue")
			if closeBeforeContinue {
				p.Close()
				select {
				case <-workerStopped:
				case <-time.After(time.Second):
					t.Fatal("Close did not cancel the pending migration worker")
				}
				host.mu.Lock()
				require.Len(t, host.queries, 1, "a closed plugin must not resume cleanup or start collection")
				host.mu.Unlock()
				return
			}
			continueBatch <- struct{}{}
			require.Eventually(t, func() bool {
				p.collectorMu.Lock()
				defer p.collectorMu.Unlock()
				return p.collectorStarted
			}, time.Second, time.Millisecond)
			require.True(t, migrationProgress(t, bridge).Complete)
			assertNoAccountingAmountsInState(t, host.fakeHost)
			host.mu.Lock()
			require.Zero(t, host.wholeLists)
			require.Len(t, host.queries, 3)
			host.mu.Unlock()
		})
	}
}
