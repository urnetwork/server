// The head of an attempt object at its content address, over real local
// storage, with and without the bounded paged listing.
package startifact

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A store without the paged listing, which lists an address as a prefix.
type attemptObjectStatUnpagedStore struct {
	server.BlobStore
}

// A stored object's length is answered at its own address only: not for
// another hash, another type, or a store that holds nothing.
func TestAttemptObjectStatAnswersOnlyItsAddress(t *testing.T) {
	t.Parallel()
	for _, paged := range []bool{true, false} {
		var store server.BlobStore = server.NewLocalBlobStore(t.TempDir(), "attempt-stat")
		if !paged {
			store = &attemptObjectStatUnpagedStore{BlobStore: store}
		}
		data := []byte("{\"sequence\":1}\n")
		hash := attemptObjectTestHash(data)
		if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", hash, data); err != nil {
			t.Fatal(err)
		}
		size, found, err := StatAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", hash)
		if err != nil || !found || size != uint64(len(data)) {
			t.Fatalf("paged %t: size=%d found=%t err=%v", paged, size, found, err)
		}
		for _, other := range []struct{ kind, hash string }{
			{kind: "records", hash: attemptObjectTestHash([]byte("{\"sequence\":2}\n"))},
			{kind: "proofs", hash: hash},
			{kind: "metadata", hash: hash},
		} {
			if size, found, err := StatAttemptObject(t.Context(), store, attemptObjectTestBounds(), other.kind, other.hash); err != nil || found || size != 0 {
				t.Fatalf("paged %t: %s %s answered size=%d found=%t err=%v", paged, other.kind, other.hash, size, found, err)
			}
		}
	}
}

// An invalid address, an untyped kind, a canceled caller and a stored object
// over its type's bound are errors, not answers.
func TestAttemptObjectStatRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()
	store := server.NewLocalBlobStore(t.TempDir(), "attempt-stat")
	data := []byte("{\"sequence\":1}\n")
	hash := attemptObjectTestHash(data)
	if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", hash, data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StatAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", "0x00"); err == nil {
		t.Fatal("an invalid address was answered")
	}
	if _, _, err := StatAttemptObject(t.Context(), store, attemptObjectTestBounds(), "other", hash); err == nil {
		t.Fatal("an untyped kind was answered")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := StatAttemptObject(ctx, store, attemptObjectTestBounds(), "records", hash); err == nil {
		t.Fatal("a canceled caller was answered")
	}
	small := attemptObjectTestBounds()
	small.RecordBytes = uint64(len(data)) - 1
	if _, found, err := StatAttemptObject(t.Context(), store, small, "records", hash); err == nil || found {
		t.Fatal("an object over its type's bound was answered")
	}
}
