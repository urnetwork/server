// Bucket lifecycle identity includes the remote transport authority, not prefix.
package server

import "testing"

func TestBlobStoresShareLifecycleRequiresExactRemoteBucket(t *testing.T) {
	newStore := func(authority, bucket, prefix string, tls bool) BlobStore {
		t.Helper()
		store, err := NewBlobStore(&BlobStoreConfig{Authority: authority, Bucket: bucket, Prefix: prefix, Tls: tls, AccessKey: "synthetic-access", SecretKey: "synthetic-secret"})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	original := newStore("storage.example:9000", "shared-bucket", "stats", false)
	for _, testCase := range []struct {
		name  string
		other BlobStore
		share bool
	}{
		{name: "another prefix and client", other: newStore("storage.example:9000", "shared-bucket", "logs", false), share: true},
		{name: "another bucket", other: newStore("storage.example:9000", "feedback-bucket", "logs", false)},
		{name: "another authority", other: newStore("other-storage.example:9000", "shared-bucket", "logs", false)},
		{name: "another scheme", other: newStore("storage.example:9000", "shared-bucket", "logs", true)},
		{name: "absent store"},
		{name: "local reaper", other: NewLocalBlobStore(t.TempDir(), "stats")},
	} {
		if got := BlobStoresShareLifecycle(original, testCase.other); got != testCase.share {
			t.Fatalf("%s lifecycle identity = %v, want %v", testCase.name, got, testCase.share)
		}
	}
	local := NewLocalBlobStore(t.TempDir(), "stats")
	if BlobStoresShareLifecycle(local, local) {
		t.Fatal("local reaper was treated as a remote bucket configuration")
	}
}
