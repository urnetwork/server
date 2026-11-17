package startifact

import (
	"context"
	"errors"
	"os"
	"testing"
)

// Registrations publish one signed carrier through two immutable routes. Both
// actual writes must use the same private source, including collided retries;
// recreating and syncing it per route serializes redundant disk flushes.
func TestPreparedEvidencePublishSharesOneOwnedStage(t *testing.T) {
	prepared, err := PrepareEvidence(preparedEvidenceEnvelopeTest(t, "publication-stage"))
	if err != nil {
		t.Fatal(err)
	}
	store := newStagedEvidenceReplicaTest(t, t.TempDir(), "operator-1", prepared.encoded)
	for attempt := 0; attempt < 2; attempt++ {
		published, err := prepared.Publish(t.Context(), store)
		if err != nil || published == nil {
			t.Fatal("actual publication", err)
		}
		if store.puts != 2*(attempt+1) || store.gets != store.puts || store.closes != store.gets {
			t.Fatal("publication omitted an immutable write or exact winner read/close")
		}
		first := 2 * attempt
		if store.paths[first] != store.paths[first+1] || !os.SameFile(store.files[first], store.files[first+1]) {
			t.Fatal("one signed carrier was staged separately for its two immutable routes")
		}
		if attempt > 0 && store.paths[first] == store.paths[0] {
			t.Fatal("a later invocation reused an earlier stage")
		}
		for _, path := range store.paths {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed publication retained its private stage", err)
			}
		}
	}
}

// A failure on the second route still owns cleanup of the shared source. It
// cannot turn the first committed route into a complete publication result.
func TestPreparedEvidencePublishSecondRouteFailureCleansStage(t *testing.T) {
	prepared, err := PrepareEvidence(preparedEvidenceEnvelopeTest(t, "publication-stage"))
	if err != nil {
		t.Fatal(err)
	}
	store := newStagedEvidenceReplicaTest(t, t.TempDir(), "operator-1", prepared.encoded)
	sentinel := errors.New("second immutable route refused")
	store.beforePut = func(string, string) error {
		if len(store.paths) == 2 {
			return sentinel
		}
		return nil
	}
	published, err := prepared.Publish(t.Context(), store)
	if published != nil || !errors.Is(err, sentinel) {
		t.Fatal("incomplete publication lost its actual failure", err)
	}
	if len(store.paths) != 2 || store.paths[0] != store.paths[1] || !os.SameFile(store.files[0], store.files[1]) || store.gets != 1 || store.closes != 1 {
		t.Fatal("second-route failure lost the shared stage or first winner read/close")
	}
	if _, err := os.Stat(store.paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed publication retained its private stage", err)
	}
}

func TestPreparedEvidencePublishCancellationCleansSharedStage(t *testing.T) {
	for _, cancelAfter := range []int{1, 2} {
		prepared, err := PrepareEvidence(preparedEvidenceEnvelopeTest(t, "publication-stage"))
		if err != nil {
			t.Fatal(err)
		}
		store := newStagedEvidenceReplicaTest(t, t.TempDir(), "operator-1", prepared.encoded)
		ctx, cancel := context.WithCancel(t.Context())
		store.afterClose = func() {
			if store.closes == cancelAfter {
				cancel()
			}
		}
		published, err := prepared.Publish(ctx, store)
		cancel()
		if published != nil || !errors.Is(err, context.Canceled) || store.puts != cancelAfter || store.gets != cancelAfter || store.closes != cancelAfter {
			t.Fatalf("cancellation after route %d lost its failure or reader ownership: %v", cancelAfter, err)
		}
		for _, path := range store.paths {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("canceled publication retained its private stage", err)
			}
		}
	}
}
