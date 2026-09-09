// One verified immutable wire owner is shared by an object's replica writes
// and exact readbacks. No mutable envelope or corpus-wide cache is retained.
package startifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urnetwork/server"
)

// Only the verified canonical wire and routing identity survive preparation.
// The caller owns this object until its synchronous replica checks finish.
// Methods do not mutate the owner; store concurrency remains store-owned.
type PreparedEvidence struct {
	identity EvidenceEnvelope
	encoded  []byte
}

// Snapshot the input before the existing identity, hash and signature checks.
// The original envelope and its payload may be changed after this returns
// without changing either the verified bytes or their publication namespace.
func PrepareEvidence(envelope *EvidenceEnvelope) (*PreparedEvidence, error) {
	if envelope == nil {
		return nil, errors.New("evidence envelope is missing")
	}
	snapshot := *envelope
	snapshot.Payload = bytes.Clone(envelope.Payload)
	encoded, err := EvidenceBytes(&snapshot)
	if err != nil {
		return nil, err
	}
	// Do not retain a second payload beside its complete signed carrier.
	snapshot.Payload = nil
	return &PreparedEvidence{identity: snapshot, encoded: encoded}, nil
}

// Derive both immutable routes from the sealed signed identity for this store.
func (self *PreparedEvidence) publication(store server.BlobStore) (*Published, error) {
	if self == nil || len(self.encoded) == 0 {
		return nil, errors.New("prepared evidence is missing")
	}
	if store == nil {
		return nil, errors.New("server/blob store is unavailable")
	}
	contentKey, err := EvidenceContentKey(store, self.identity.ContentHash)
	if err != nil {
		return nil, err
	}
	runId := self.identity.RunID
	if runId == "" {
		runId = EvidenceDeploymentHistoryRunID
	}
	historyKey, err := EvidenceHistoryKey(store, self.identity.DeploymentID, self.identity.Netuid, self.identity.Kind, runId, self.identity.ContentHash)
	if err != nil {
		return nil, err
	}
	return &Published{ContentHash: self.identity.ContentHash, ContentKey: contentKey, HistoryKey: historyKey, Bucket: store.Bucket()}, nil
}

// Both routes still execute the original immutable create and exact winner
// readback. Preparation only removes repeated signature and Json work.
func (self *PreparedEvidence) Publish(ctx context.Context, store server.BlobStore) (*Published, error) {
	if ctx == nil {
		return nil, errors.New("evidence publication context is missing")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	published, err := self.publication(store)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{published.ContentKey, published.HistoryKey} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := putImmutable(ctx, store, key, self.encoded); err != nil {
			return nil, err
		}
	}
	return published, ctx.Err()
}

// Verify receipt identity and independently read both complete stored routes.
// Equality is against the already verified immutable wire, not mutable input
// fields or a digest-only substitute; exact length and Close remain required.
func (self *PreparedEvidence) VerifyPublished(ctx context.Context, store server.BlobStore, published *Published) error {
	if ctx == nil {
		return errors.New("evidence verification context is missing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	want, err := self.publication(store)
	if err != nil {
		return err
	}
	if published == nil || !strings.EqualFold(published.ContentHash, want.ContentHash) || published.Bucket != want.Bucket {
		return errors.New("direct evidence receipt is invalid")
	}
	if published.ContentKey != want.ContentKey || published.HistoryKey != want.HistoryKey {
		return errors.New("direct evidence receipt keys do not match the rendered store")
	}
	for _, key := range []string{published.ContentKey, published.HistoryKey} {
		if err := ctx.Err(); err != nil {
			return err
		}
		reader, err := store.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read direct evidence object %s: %w", key, err)
		}
		equal, readErr := compareArtifactReader(ctx, reader, self.encoded)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return fmt.Errorf("read/close direct evidence object %s: %w", key, errors.Join(readErr, closeErr))
		}
		if !equal {
			return fmt.Errorf("direct evidence object %s differs from its signed envelope", key)
		}
	}
	return ctx.Err()
}
