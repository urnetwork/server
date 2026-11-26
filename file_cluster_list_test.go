package server

// FileClusterSettings.ListMaxKeyCount, S3's ceiling on one LIST's keys, read from settings: a List whose limit passes
// it asks for that many keys in its one request and answers one less, more following.

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
)

// A List of limit 10 with six keys under its prefix, on clusters whose ListMaxKeyCount is listMaxKeyCount: the keys
// it answered, whether more follow, and the max-keys its one request asked for.
func fileClusterListUnderMaxKeyCount(t *testing.T, listMaxKeyCount int) (keyCount int, more bool, maxKeys string) {
	t.Helper()
	store := newFakeS3(1, "build-1")
	for i := range 6 {
		store.objects[fmt.Sprintf("meta-bucket/seg/r/%d/_", i)] = &fakeS3Object{body: []byte("synthetic"), etag: "etag-synthetic"}
	}
	settings := fileClusterTestSettings(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()))
	settings.NoChecks = true
	settings.ListMaxKeyCount = listMaxKeyCount
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs("host-a.example:9000"), settings, func(tls bool) (http.RoundTripper, error) {
		return store.transport("any"), nil
	}, func() *SidecarConfig { return &SidecarConfig{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	objects, more, err := clusters.clusterIdClusters["meta-1"].List(t.Context(), "seg/r/", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	requests := store.takeRequests()
	if len(requests) != 1 {
		t.Fatalf("%d requests for one List", len(requests))
	}
	query, err := url.ParseQuery(requests[0].query)
	if err != nil {
		t.Fatal(err)
	}
	return len(objects), more, query.Get("max-keys")
}

// S3's 1,000 by default; a ceiling of 4 asks for 4 keys and answers 3, more following; and one below 2 answers a key
// at a time, so a caller paging through a prefix still moves.
func TestFileClusterListAsksNoMoreThanListMaxKeyCount(t *testing.T) {
	if listMaxKeyCount := DefaultFileClusterSettings().ListMaxKeyCount; listMaxKeyCount != 1000 {
		t.Fatalf("ListMaxKeyCount %d by default, want S3's 1,000", listMaxKeyCount)
	}
	for _, c := range []struct {
		listMaxKeyCount int
		keyCount        int
		maxKeys         string
	}{
		{listMaxKeyCount: 4, keyCount: 3, maxKeys: "4"},
		{listMaxKeyCount: 1, keyCount: 1, maxKeys: "2"},
		{listMaxKeyCount: 0, keyCount: 1, maxKeys: "2"},
	} {
		keyCount, more, maxKeys := fileClusterListUnderMaxKeyCount(t, c.listMaxKeyCount)
		if keyCount != c.keyCount || !more || maxKeys != c.maxKeys {
			t.Errorf("a ceiling of %d: %d keys, more %v, max-keys %s; want %d keys, more, max-keys %s", c.listMaxKeyCount, keyCount, more, maxKeys, c.keyCount, c.maxKeys)
		}
	}
}
