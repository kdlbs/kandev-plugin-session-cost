package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/kdlbs/kandev/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	usagePrivacyStateID = "usage-state-privacy-v2"
	usagePrivacyKey     = "completed"
	usageCleanupBatch   = 200
)

var (
	errUsageStateCleanupPending     = errors.New("legacy usage state cleanup is pending")
	errUsageStateCleanupUnsupported = errors.New("upgrade Kandev to support bounded usage state cleanup")
)

type usagePrivacyMigration struct {
	Version        int
	Complete       bool
	Namespace      int      `json:",omitempty"`
	Cursor         string   `json:",omitempty"`
	PageLoaded     bool     `json:",omitempty"`
	PendingKeys    []string `json:",omitempty"`
	PageNextCursor string   `json:",omitempty"`
}

// Generic Host state has no captured accounting scope or erasure contract.
// Retain receipt identities only; accounting payloads belong to the Host ledger.
func (p *plugin) ensureUsageStatePrivacy(ctx context.Context) error {
	ctx, cancel := p.canonicalReadContext(ctx)
	defer cancel()
	if !p.usageStateMu.TryLock() {
		return errUsageStateCleanupPending
	}
	defer p.usageStateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.usageStateReady {
		return nil
	}
	host := p.Host()
	if host == nil {
		return errors.New("host unavailable")
	}
	var migration usagePrivacyMigration
	found, err := p.readUsageState(ctx, usagePrivacyStateID, usagePrivacyKey, &migration)
	if err != nil {
		return err
	}
	if found && migration.Version == 2 && migration.Complete {
		p.usageStateReady = true
		return nil
	}
	reader, ok := pluginsdk.HostStateKeys(host)
	if !ok {
		return errUsageStateCleanupUnsupported
	}
	if !found || migration.Version != 2 {
		migration = usagePrivacyMigration{Version: 2}
	}
	namespaces := []string{"canonical-usage-v1", "usage-observation-v1"}
	if migration.Namespace < 0 || migration.Namespace > len(namespaces) || len(migration.PendingKeys) > usageCleanupBatch {
		return errors.New("invalid usage cleanup progress")
	}
	processed := 0
	for migration.Namespace < len(namespaces) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if processed == usageCleanupBatch {
			return errUsageStateCleanupPending
		}
		namespace := namespaces[migration.Namespace]
		if !migration.PageLoaded {
			page, err := reader.ListKeys(ctx, pluginsdk.StateKeyQuery{Scope: "plugin", ScopeID: namespace, Cursor: migration.Cursor, Limit: usageCleanupBatch})
			if status.Code(err) == codes.Unimplemented {
				return errUsageStateCleanupUnsupported
			}
			if err != nil {
				return err
			}
			if len(page.Keys) > usageCleanupBatch || (page.NextCursor != "" && (len(page.Keys) == 0 || page.NextCursor == migration.Cursor)) {
				return errors.New("invalid usage cleanup key page")
			}
			migration.PageLoaded = true
			migration.PendingKeys = page.Keys
			migration.PageNextCursor = page.NextCursor
			// Capture the bounded page before changing any key. Its continuation
			// remains uncommitted until every key on this page is acknowledged.
			if err := p.saveUsagePrivacyProgress(ctx, migration); err != nil {
				return err
			}
		}
		for len(migration.PendingKeys) > 0 {
			if processed == usageCleanupBatch {
				return errUsageStateCleanupPending
			}
			if err := p.scrubUsageStateKey(ctx, namespace, migration.PendingKeys[0]); err != nil {
				return err
			}
			migration.PendingKeys = migration.PendingKeys[1:]
			// A failed checkpoint write repeats this one idempotent operation.
			// A deadline cannot lose the rest of an already-enumerated page.
			if err := p.saveUsagePrivacyProgress(ctx, migration); err != nil {
				return err
			}
			processed++
		}
		migration.Cursor = migration.PageNextCursor
		migration.PageNextCursor = ""
		migration.PendingKeys = nil
		migration.PageLoaded = false
		if migration.Cursor == "" {
			migration.Namespace++
		}
		if err := p.saveUsagePrivacyProgress(ctx, migration); err != nil {
			return err
		}
	}
	// No accounting copies are written while cleanup is pending. This process
	// therefore cannot insert legacy values behind the keyset continuation.
	if err := p.saveUsagePrivacyProgress(ctx, usagePrivacyMigration{Version: 2, Complete: true}); err != nil {
		return err
	}
	p.usageStateReady = true
	return nil
}

func (p *plugin) saveUsagePrivacyProgress(ctx context.Context, migration usagePrivacyMigration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.writeUsageState(ctx, usagePrivacyStateID, usagePrivacyKey, migration)
}

func (p *plugin) scrubUsageStateKey(ctx context.Context, namespace, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	host := p.Host()
	if namespace == "canonical-usage-v1" {
		return host.DeleteState(ctx, "plugin", namespace, key)
	}
	value, found, err := host.GetState(ctx, "plugin", namespace, key)
	if err != nil || !found {
		return err
	}
	encoded, ok := value["json"].(string)
	var receipt usageObservationCheckpoint
	if !ok || json.Unmarshal([]byte(encoded), &receipt) != nil {
		return host.DeleteState(ctx, "plugin", namespace, key)
	}
	// Rewrite even a v2 record: unknown or legacy fields must not preserve
	// recoverable accounting payloads in an otherwise metadata-only receipt.
	receipt.Version = 2
	return p.writeUsageState(ctx, namespace, key, receipt)
}

func retryUsageStateCleanup(err error) bool {
	if errors.Is(err, errUsageStateCleanupUnsupported) {
		return false
	}
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument, codes.Unimplemented:
		return false
	default:
		return true
	}
}

// Startup does one bounded batch. Larger archives continue in one cancellable
// worker, each batch retaining the same short Host budget and finite key cap.
func (p *plugin) startUsageStateCleanup() {
	p.usageCleanupMu.Lock()
	defer p.usageCleanupMu.Unlock()
	if p.usageCleanupCancel != nil || p.usageCleanupClosed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.usageCleanupCancel = cancel
	go func() {
		defer cancel()
		for p.waitUsageStateCleanup(ctx) {
			err := p.ensureUsageStatePrivacy(ctx)
			if err == nil {
				p.usageCleanupMu.Lock()
				if !p.usageCleanupClosed && ctx.Err() == nil {
					p.startCollector()
				}
				p.usageCleanupMu.Unlock()
				return
			}
			if ctx.Err() != nil || !retryUsageStateCleanup(err) {
				return
			}
			if !errors.Is(err, errUsageStateCleanupPending) {
				log.Printf("session-cost state cleanup remains pending: %v", err)
			}
		}
	}()
}

func (p *plugin) waitUsageStateCleanup(ctx context.Context) bool {
	if p.usageCleanupWait != nil {
		return p.usageCleanupWait(ctx)
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

// Read pre-migration receipts long enough to preserve revision and digest
// identity. Marshaling uses only the metadata fields of the new checkpoint.
func (v *usageObservationCheckpoint) UnmarshalJSON(data []byte) error {
	type metadata usageObservationCheckpoint
	var stored struct {
		*metadata
		Observation *pluginsdk.UsageObservationV1
	}
	stored.metadata = (*metadata)(v)
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	if stored.Observation != nil {
		v.Version = 0
		v.SubjectBinding = stored.Observation.SubjectBinding
		v.ItemID = stored.Observation.ItemID
		v.Revision = stored.Observation.Revision
		v.PayloadDigest = stored.Observation.PayloadDigest
	}
	return nil
}
