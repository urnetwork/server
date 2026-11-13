// Synthetic storage catches default-bucket substitution and hidden v2 usage;
// no MinIO, database, archive write, metrics or model request is made.
package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

// The nil embed trips accidental writes or unrelated operations immediately.
type launchStorageFixture struct {
	server.ProtectedBlobStore
	bucket        string
	protection    *server.BlobProtection
	protectionErr error
	objects       []server.BlobObject
	versions      []server.BlobObjectVersion
	prefixes      []string
}

// All identities are synthetic and deliberately distinguish the two buckets.
func (self *launchStorageFixture) Authority() string { return "storage.synthetic.example" }

// Configured competition and unrelated general blobs never share a bucket.
func (self *launchStorageFixture) Bucket() string { return self.bucket }

// One private namespace contains both old and current archive versions.
func (self *launchStorageFixture) Prefix() string { return "synthetic-artifacts" }

// A storage refusal is not made healthy by some other bucket's success.
func (self *launchStorageFixture) CheckProtection(context.Context) (*server.BlobProtection, error) {
	return self.protection, self.protectionErr
}

// Only the requested prefix is returned, reproducing real S3 listing semantics.
func (self *launchStorageFixture) List(_ context.Context, prefix string) ([]server.BlobObject, error) {
	self.prefixes = append(self.prefixes, prefix)
	objects := []server.BlobObject{}
	for _, object := range self.objects {
		if strings.HasPrefix(object.Key, prefix) {
			objects = append(objects, object)
		}
	}
	return objects, nil
}

// Tests distinguish retained history from the old current-object collection.
func (self *launchStorageFixture) ListVersions(ctx context.Context, prefix string, visit func(server.BlobObjectVersion) error) error {
	self.prefixes = append(self.prefixes, prefix)
	if self.versions != nil {
		for _, object := range self.versions {
			if strings.HasPrefix(object.Key, prefix) {
				if err := visit(object); err != nil {
					return err
				}
			}
		}
		return ctx.Err()
	}
	for _, object := range self.objects {
		if strings.HasPrefix(object.Key, prefix) {
			if err := visit(server.BlobObjectVersion{Key: object.Key, Size: object.Size, VersionId: "synthetic-current"}); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// Produces a full valid proof that subsequent tests change one invariant at a time.
func newLaunchStorageFixture(bucket string) *launchStorageFixture {
	store := &launchStorageFixture{bucket: bucket}
	store.protection = &server.BlobProtection{Authority: store.Authority(), Bucket: bucket, ObjectLock: "COMPLIANCE", Versioning: true, ReplicationTargets: []string{"synthetic-backup"}}
	return store
}

// Neither a different healthy bucket nor a correct but unprotected archive is
// sufficient to pass competition storage launch gates.
func TestLaunchStorageRejectsMisleadingHealthyDefaultBucket(t *testing.T) {
	info := &controller.InfoResult{CompetitionId: "synthetic-competition"}
	healthy := newLaunchStorageFixture("synthetic-default-blob")
	if _, err := checkCompetitionMinioStore(t.Context(), info, 1000, healthy, "synthetic-competition"); err == nil {
		t.Fatal("healthy unrelated default blob bucket qualified competition storage")
	}
	broken := newLaunchStorageFixture("synthetic-competition")
	broken.protectionErr = errors.New("synthetic competition protection failure")
	if _, err := checkCompetitionMinioStore(t.Context(), info, 1000, broken, broken.Bucket()); err == nil {
		t.Fatal("broken competition bucket qualified using unrelated health")
	}
}

// Current v2 runtime, host and review evidence consume the same allocation as
// retained v1 attempts. Non-competition sibling data is outside that scope.
func TestLaunchStorageChargesAllCompetitionArchiveVersions(t *testing.T) {
	store := newLaunchStorageFixture("synthetic-competition")
	store.objects = []server.BlobObject{{Key: "synthetic-artifacts/competition/v1/synthetic-competition/run", Size: 100}, {Key: "synthetic-artifacts/competition/v2/runtime/synthetic", Size: 200}, {Key: "synthetic-artifacts/competition/v2/host-readiness/synthetic", Size: 300}, {Key: "synthetic-artifacts/competition/v2/review-evidence/synthetic", Size: 400}, {Key: "synthetic-artifacts/other/synthetic", Size: 100000}}
	evidence, err := checkCompetitionMinioStore(t.Context(), &controller.InfoResult{CompetitionId: "synthetic-competition"}, 10000, store, store.Bucket())
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := evidence["usage"].(*server.BlobUsage)
	if !ok || usage.UsedBytes != 1000 || usage.ObjectCount != 4 || usage.UsedPercent != 10 || !usage.IncludesObjectVersions || len(store.prefixes) != 1 || store.prefixes[0] != "synthetic-artifacts/competition/" {
		t.Fatalf("v2 usage omitted or sibling overcharged: %#v %v", usage, store.prefixes)
	}
}

// A v1-only listing would incorrectly pass this exact allocation boundary.
func TestLaunchStorageV2UsageCanStopLaunch(t *testing.T) {
	store := newLaunchStorageFixture("synthetic-competition")
	store.objects = []server.BlobObject{{Key: "synthetic-artifacts/competition/v1/synthetic/run", Size: 50}, {Key: "synthetic-artifacts/competition/v2/runtime/synthetic", Size: 900}}
	evidence, err := checkCompetitionMinioStore(t.Context(), &controller.InfoResult{CompetitionId: "synthetic-competition"}, 1000, store, store.Bucket())
	usage, ok := evidence["usage"].(*server.BlobUsage)
	if err == nil || !ok || usage.UsedBytes != 950 || usage.UsedPercent != 95 {
		t.Fatalf("v2 allocation overrun qualified: %#v %v", usage, err)
	}
}

// A nil, relabeled or partial configuration response is never a WORM proof.
func TestLaunchStorageRejectsIncompleteProtectionProof(t *testing.T) {
	for index, mutate := range []func(*launchStorageFixture){func(s *launchStorageFixture) { s.protection = nil }, func(s *launchStorageFixture) { s.protection.Versioning = false }, func(s *launchStorageFixture) { s.protection.ObjectLock = "GOVERNANCE" }, func(s *launchStorageFixture) { s.protection.Bucket = "synthetic-other" }, func(s *launchStorageFixture) { s.protection.Authority = "other.synthetic.example" }, func(s *launchStorageFixture) { s.protection.ReplicationTargets = nil }, func(s *launchStorageFixture) { s.protection.ReplicationTargets = []string{" "} }} {
		store := newLaunchStorageFixture("synthetic-competition")
		mutate(store)
		if _, err := checkCompetitionMinioStore(t.Context(), &controller.InfoResult{CompetitionId: "synthetic-competition"}, 1000, store, store.Bucket()); err == nil || len(store.prefixes) != 0 {
			t.Errorf("incomplete proof %d qualified or reached capacity listing", index)
		}
	}
}

// Retained older bodies of the same key, not just new v2 key layouts, can
// exhaust the allocation. A zero-byte delete marker is retained in the census.
func TestLaunchStorageNoncurrentVersionCanStopLaunch(t *testing.T) {
	store := newLaunchStorageFixture("synthetic-competition")
	key := "synthetic-artifacts/competition/v2/runtime/synthetic"
	store.objects = []server.BlobObject{{Key: key, Size: 50}}
	store.versions = []server.BlobObjectVersion{{Key: key, Size: 50, VersionId: "synthetic-current"}, {Key: key, Size: 900, VersionId: "synthetic-older"}, {Key: key, VersionId: "synthetic-delete-marker", DeleteMarker: true}}
	evidence, err := checkCompetitionMinioStore(t.Context(), &controller.InfoResult{CompetitionId: "synthetic-competition"}, 1000, store, store.Bucket())
	usage, ok := evidence["usage"].(*server.BlobUsage)
	if err == nil || !ok || usage.UsedBytes != 950 || usage.ObjectCount != 3 || !usage.IncludesObjectVersions {
		t.Fatalf("retained older body omitted from capacity: %#v %v", usage, err)
	}
}

// Hiding the optional capability reproduces an old current-only store without
// changing its existing List behavior or manufacturing a compatibility pass.
type launchCurrentOnlyStorage struct{ server.ProtectedBlobStore }

// A protected store that cannot enumerate versions cannot prove allocation.
func TestLaunchStorageRejectsCurrentOnlyCapacityCapability(t *testing.T) {
	store := newLaunchStorageFixture("synthetic-competition")
	current := &launchCurrentOnlyStorage{ProtectedBlobStore: store}
	if _, err := checkCompetitionMinioStore(t.Context(), &controller.InfoResult{CompetitionId: "synthetic-competition"}, 1000, current, store.Bucket()); err == nil || len(store.prefixes) != 0 {
		t.Fatal("current-only fallback qualified WORM capacity")
	}
}

// The actual docopt contract must agree with the owning constructor, otherwise
// an implicit old CLI value would silently defeat the user's decimal10TB grant.
func TestLaunchStorageDefaultParsesDecimalTenTerabytes(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	opts, err := parser.ParseArgs(simulationUsage(), []string{"launch-preflight", "--epoch=0", "--evaluator-image=sha256:" + strings.Repeat("a", 64), "--operator-token-file=/synthetic/operator.token", "--grafana-url=https://grafana.example", "--grafana-token-file=/synthetic/grafana.token", "--metrics-url=https://metrics.example/prometheus", "--metrics-token-file=/synthetic/metrics.token"}, "synthetic-version")
	if err != nil {
		t.Fatal(err)
	}
	text, err := opts.String("--artifact-capacity-bytes")
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value != 10_000_000_000_000 || defaultLaunchPreflightConfig().ArtifactCapacityBytes != value {
		t.Fatalf("CLI silently kept an old/10TiB allocation: %q %v", text, err)
	}
	if err := validateLaunchArtifactAllocation(value); err != nil {
		t.Fatal(err)
	}
	if err := validateLaunchArtifactAllocation(value + 1); err == nil {
		t.Fatal("an arbitrary larger denominator exceeded the user's allocation")
	}
}
