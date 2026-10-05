// Public publication must preserve its cancellation owner before selecting any durable key.
package startifact

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// The embedded store implements real immutable publication; this counts only route selection.
type canceledArtifactStore struct {
	*immutableRaceBlobStore
	prefixes atomic.Int32
}

// A canceled canonical build must never reach a destination prefix.
func (self *canceledArtifactStore) Prefix() string {
	self.prefixes.Add(1)
	return self.immutableRaceBlobStore.Prefix()
}

// Prove the public owner cancels before serialization and store publication, not merely inside storage.
func TestArtifactPublicationCancelsBeforeDestinationSelection(t *testing.T) {
	store := &canceledArtifactStore{immutableRaceBlobStore: newImmutableRaceBlobStore("")}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	published, err := Publish(ctx, store, testArtifact(t))
	if published != nil || !errors.Is(err, context.Canceled) || store.prefixes.Load() != 0 {
		t.Fatal("canceled publication reached a durable destination", published, err, store.prefixes.Load())
	}
	if _, err := Publish(t.Context(), store, testArtifact(t)); err != nil {
		t.Fatal("healthy publication did not recover", err)
	}
}
