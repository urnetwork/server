// Test support for a package whose tests need a local blob store with its own
// root, prefix or allocation.
package server

import (
	"sync/atomic"
	"testing"
)

// The store a test stands in for LoadBlobStore's selection. Only
// UseLocalBlobStoreForTest installs one, and it refuses to run outside a test
// binary, so a production binary always reads nil.
var blobStoreForTest atomic.Pointer[BlobStore]

// Makes LoadBlobStore return a local store at root, under prefix, holding at
// most maxBytes, for the rest of a test, and puts the configured selection
// back afterwards. It stands in for an explicit local minio.yml, whose durable
// volume custody needs a declaration of the host's real devices that a test
// cannot make. The selection belongs to the process, so the test cannot be
// parallel: the marker set through t.Setenv makes the testing package refuse
// t.Parallel, before this call or after it.
func UseLocalBlobStoreForTest(t testing.TB, root string, prefix string, maxBytes int64) {
	t.Helper()
	if !testing.Testing() {
		panic("UseLocalBlobStoreForTest runs only in a test binary")
	}
	t.Setenv("URNETWORK_TEST_LOCAL_BLOB_STORE", "1")
	store := NewLocalBlobStoreWithMaxBytes(root, prefix, maxBytes)
	previous := blobStoreForTest.Swap(&store)
	t.Cleanup(func() {
		blobStoreForTest.Store(previous)
	})
}
