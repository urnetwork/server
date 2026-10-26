// Version-aware capacity uses the actual MinIO XML iterator against loopback
// fixtures; no real storage, object writes, deletes or credentials are used.
package server

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Namespace/config identity stays synthetic even though the real SDK signs
// requests and parses version/delete-marker pagination from this local server.
func newBlobVersionMinioFixture(t *testing.T, handler http.HandlerFunc) *minioBlobStore {
	t.Helper()
	endpoint := httptest.NewServer(handler)
	t.Cleanup(endpoint.Close)
	client, err := minio.New(strings.TrimPrefix(endpoint.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("synthetic-access", "synthetic-secret", ""), Secure: false, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return &minioBlobStore{client: client, authority: strings.TrimPrefix(endpoint.URL, "http://"), bucket: "synthetic-competition", prefix: "synthetic-artifacts"}
}

// Current and noncurrent bodies share a key but are separate charges across
// actual SDK pages. Delete markers count as entries and carry exactly zero.
func TestBlobVersionUsageIncludesOlderVersionsAndDeleteMarkers(t *testing.T) {
	calls := 0
	store := newBlobVersionMinioFixture(t, func(response http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != http.MethodGet || !request.URL.Query().Has("versions") || request.URL.Query().Get("prefix") != "synthetic-artifacts/competition/" {
			t.Error("version proof used a current-only or mutating API")
		}
		response.Header().Set("Content-Type", "application/xml")
		if request.URL.Query().Get("version-id-marker") == "" {
			io.WriteString(response, `<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>synthetic-competition</Name><Prefix>synthetic-artifacts/competition/</Prefix><IsTruncated>true</IsTruncated><NextKeyMarker>synthetic-artifacts/competition/v2/object</NextKeyMarker><NextVersionIdMarker>synthetic-new</NextVersionIdMarker><Version><Key>synthetic-artifacts/competition/v2/object</Key><VersionId>synthetic-new</VersionId><IsLatest>true</IsLatest><LastModified>2020-01-01T00:00:00Z</LastModified><Size>100</Size><ETag>synthetic</ETag><StorageClass>STANDARD</StorageClass></Version></ListVersionsResult>`)
		} else {
			io.WriteString(response, `<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>synthetic-competition</Name><Prefix>synthetic-artifacts/competition/</Prefix><IsTruncated>false</IsTruncated><Version><Key>synthetic-artifacts/competition/v2/object</Key><VersionId>synthetic-old</VersionId><IsLatest>false</IsLatest><LastModified>2019-01-01T00:00:00Z</LastModified><Size>900</Size><ETag>synthetic</ETag><StorageClass>STANDARD</StorageClass></Version><DeleteMarker><Key>synthetic-artifacts/competition/v2/deleted</Key><VersionId>synthetic-marker</VersionId><IsLatest>true</IsLatest><LastModified>2020-01-02T00:00:00Z</LastModified></DeleteMarker></ListVersionsResult>`)
		}
	})
	usage, err := MeasureBlobVersionUsageAtPrefix(t.Context(), store, "synthetic-artifacts/competition/", 1000)
	if err != nil || usage == nil || usage.UsedBytes != 1000 || usage.ObjectCount != 3 || usage.FreeBytes != 0 || !usage.IncludesObjectVersions || calls != 2 {
		t.Fatalf("version/pagination charge failed: %#v calls=%d err=%v", usage, calls, err)
	}
}

// The existing List call keeps its current-only query; the new optional method
// is the only owner permitted to ask for historical object versions.
func TestBlobVersionUsageDoesNotChangeCurrentList(t *testing.T) {
	store := newBlobVersionMinioFixture(t, func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Has("versions") {
			t.Error("legacy current-only List silently changed")
		}
		io.WriteString(response, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>synthetic-competition</Name><IsTruncated>false</IsTruncated><Contents><Key>synthetic-artifacts/current</Key><Size>7</Size><LastModified>2020-01-01T00:00:00Z</LastModified><ETag>synthetic</ETag></Contents></ListBucketResult>`)
	})
	objects, err := store.List(t.Context(), "synthetic-artifacts/")
	if err != nil || len(objects) != 1 || objects[0].Size != 7 {
		t.Fatalf("legacy current List changed: %#v %v", objects, err)
	}
}

// Visitor refusal and already-canceled input stop the owned iterator rather
// than draining more pages or returning a partial successful capacity proof.
func TestBlobVersionUsageStopsOnVisitorRefusalAndCancellation(t *testing.T) {
	store := newBlobVersionMinioFixture(t, func(response http.ResponseWriter, request *http.Request) {
		io.WriteString(response, `<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated><Version><Key>synthetic-artifacts/object</Key><VersionId>synthetic-version</VersionId><Size>1</Size><LastModified>2020-01-01T00:00:00Z</LastModified><ETag>synthetic</ETag></Version></ListVersionsResult>`)
	})
	want := errors.New("synthetic visitor refusal")
	if err := store.ListVersions(t.Context(), "synthetic-artifacts/", func(BlobObjectVersion) error { return want }); !errors.Is(err, want) {
		t.Fatalf("visitor error lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := store.ListVersions(ctx, "synthetic-artifacts/", func(BlobObjectVersion) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled version discovery ran or qualified")
	}
}

// Pure version fixtures force arithmetic/marker/capability errors independent
// of SDK transport, wall clocks, provider services or timing assumptions.
type blobVersionUsageFixture struct {
	BlobStore
	versions []BlobObjectVersion
}

// A fixed namespace cannot charge an escaping or malformed backend entry.
func (self *blobVersionUsageFixture) Prefix() string { return "synthetic" }

// Synthetic identity avoids coupling tests to live endpoints or bucket names.
func (self *blobVersionUsageFixture) Authority() string { return "storage.example" }

// All fixture versions occupy one dedicated synthetic bucket.
func (self *blobVersionUsageFixture) Bucket() string { return "synthetic-bucket" }

// Every record passes through the real accumulator callback.
func (self *blobVersionUsageFixture) ListVersions(_ context.Context, _ string, visit func(BlobObjectVersion) error) error {
	for _, version := range self.versions {
		if err := visit(version); err != nil {
			return err
		}
	}
	return nil
}

// A marker cannot hide a positive byte charge, negative size, scope escape or
// overflow; unsupported stores never fall back to current-only enumeration.
func TestBlobVersionUsageRejectsInvalidMetadataAndFallback(t *testing.T) {
	for index, versions := range [][]BlobObjectVersion{{{Key: "synthetic/competition/a", Size: -1}}, {{Key: "synthetic/competition/a", Size: 1, DeleteMarker: true}}, {{Key: "outside/a", Size: 1}}, {{Key: "synthetic/competition/a", Size: math.MaxInt64}, {Key: "synthetic/competition/a", Size: 1}}} {
		store := &blobVersionUsageFixture{versions: versions}
		if _, err := MeasureBlobVersionUsageAtPrefix(t.Context(), store, "synthetic/competition/", 1000); err == nil {
			t.Errorf("invalid version metadata %d qualified", index)
		}
	}
	currentOnly := struct{ BlobStore }{BlobStore: &blobVersionUsageFixture{}}
	if _, err := MeasureBlobVersionUsageAtPrefix(t.Context(), currentOnly, "synthetic/competition/", 1000); err == nil {
		t.Fatal("unsupported current-only store qualified version-aware capacity")
	}
}

// Local development stores expose the additive physical-body capability but
// retain their existing non-versioned List semantics and safety bounds.
func TestBlobVersionUsageSupportsLocalDevelopmentBodies(t *testing.T) {
	store := NewLocalBlobStore(t.TempDir(), "synthetic")
	if err := store.Put(t.Context(), "synthetic/competition/object", localBlobUsageSource(t, "synthetic-body"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	usage, err := MeasureBlobVersionUsageAtPrefix(t.Context(), store, "synthetic/competition/", 1000)
	if err != nil || usage.ObjectCount != 1 || usage.UsedBytes != int64(len("synthetic-body")) {
		t.Fatalf("local capability failed: %#v %v", usage, err)
	}
}
