// Synthetic retained objects exercise the actual publisher and every-runtime
// reader without contacting live storage or asserting genuine host readiness.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Deliberately separate current references from retained versions so a test
// cannot accidentally pass by reading the mutable current envelope object.
type competitionRuntimeBlobTestStore struct {
	server.RetainedBlobStore
	stateLock       sync.Mutex
	objects         map[string][]byte
	versions        map[string][]byte
	retainedWrites  int
	versionReads    int
	protectionErr   error
	corruptReadback bool
}

// All object identities are synthetic and have no network transport.
func (self *competitionRuntimeBlobTestStore) Authority() string { return "minio.synthetic.example" }

// The test bucket cannot address the deployment's archive.
func (self *competitionRuntimeBlobTestStore) Bucket() string { return "synthetic-competition" }

// Publication keys remain under one visibly synthetic namespace.
func (self *competitionRuntimeBlobTestStore) Prefix() string { return "synthetic-artifacts" }

// Protection is an explicit test seam, never enabled by a production setting.
func (self *competitionRuntimeBlobTestStore) CheckProtection(context.Context) (*server.BlobProtection, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.protectionErr != nil {
		return nil, self.protectionErr
	}
	return &server.BlobProtection{Authority: self.Authority(), Bucket: self.Bucket(), ObjectLock: "COMPLIANCE", Versioning: true, ReplicationTargets: []string{"synthetic-backup"}}, nil
}

// API dependency checks use the same independently failing protection seam.
func (self *competitionRuntimeBlobTestStore) CheckRetention(ctx context.Context) error {
	_, err := self.CheckProtection(ctx)
	return err
}

// Exact bytes are copied into a distinct immutable synthetic version.
func (self *competitionRuntimeBlobTestStore) PutRetained(_ context.Context, key, localPath, _ string, retainUntil time.Time) (*server.BlobRetention, error) {
	content, err := os.ReadFile(localPath)
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retainedWrites++
	version := fmt.Sprintf("synthetic-version-%d", self.retainedWrites)
	self.versions[key+"\x00"+version] = bytes.Clone(content)
	return &server.BlobRetention{Key: key, Size: int64(len(content)), VersionId: version, Mode: "COMPLIANCE", RetainUntil: retainUntil}, nil
}

// Conditional creation cannot overwrite even a malformed existing reference.
func (self *competitionRuntimeBlobTestStore) PutIfAbsent(_ context.Context, key, localPath, _ string) (bool, error) {
	content, err := os.ReadFile(localPath)
	if err != nil {
		return false, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.objects[key]; ok {
		return false, nil
	}
	self.objects[key] = bytes.Clone(content)
	return true, nil
}

// Only the reference is available through the mutable current-object API.
func (self *competitionRuntimeBlobTestStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	content, ok := self.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(content))), nil
}

// Empty or unknown version ids refuse instead of selecting current bytes.
func (self *competitionRuntimeBlobTestStore) GetVersion(_ context.Context, key, version string) (io.ReadCloser, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.versionReads++
	content, ok := self.versions[key+"\x00"+version]
	if !ok || version == "" {
		return nil, os.ErrNotExist
	}
	if self.corruptReadback {
		content = []byte("synthetic-corruption")
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(content))), nil
}

// Uses a genuine source-shaped fixture with all ten stage pins and a frozen
// policy, but leaves publication absent until the tested adapter performs it.
func newCompetitionRuntimeBlobFixture(t testing.TB) (*Settings, *competitionAutomationStagingPreflightFixture, *blobArtifactArchive, *competitionRuntimeBlobTestStore) {
	t.Helper()
	settings, stageStore, _ := newAutomationStagingApiFixture(t)
	settings.AutomationPolicy.RequiredApiCapability = model.CompetitionRuntimeBlobCapability
	settings.RetainUntil = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	intent := stageStore.intent
	intent.Manifest = []byte("schema: 2\nstaging: true\nsynthetic: runtime-envelope\n")
	manifestDigest := sha256.Sum256(intent.Manifest)
	intent.ManifestSha256, intent.ManifestCommit = hex.EncodeToString(manifestDigest[:]), strings.Repeat("a", 40)
	var err error
	intent.IntentSha256, err = competitionPreparationIntentDigest(*intent)
	if err != nil {
		t.Fatal(err)
	}
	blobs := &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}}
	archive := &blobArtifactArchive{store: blobs}
	settings.artifactArchive = archive
	frozen, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		t.Fatal(err)
	}
	stageStore.stagingRound.PolicyJson, err = policySnapshot(frozen)
	if err != nil {
		t.Fatal(err)
	}
	stageStore.enqueueJob.Round = *stageStore.stagingRound
	stageStore.getJob = stageStore.enqueueJob
	return settings, &competitionAutomationStagingPreflightFixture{automationStagingApiStore: stageStore}, archive, blobs
}

// No caller-created envelope counts as installation; publication reads back
// the retained version before exposing a reference, and replay performs no write.
func TestCompetitionRuntimeBlobPublicationIsRetainedAndIdempotent(t *testing.T) {
	settings, store, archive, blobs := newCompetitionRuntimeBlobFixture(t)
	ctx := context.Background()
	if LoadCompetitionRuntimeManifestContext(ctx, settings, store.intent) == nil {
		t.Fatal("unpublished envelope was synthesized from source intent")
	}
	receipt, err := archive.publishRuntimeManifest(ctx, settings, store.intent)
	if err != nil || receipt == nil || receipt.VersionId == "" || blobs.retainedWrites != 1 {
		t.Fatalf("retained publication failed: %+v %v", receipt, err)
	}
	if err := LoadCompetitionRuntimeManifestContext(ctx, settings, store.intent); err != nil {
		t.Fatal(err)
	}
	replayed, err := archive.publishRuntimeManifest(ctx, settings, store.intent)
	if err != nil || *replayed != *receipt || blobs.retainedWrites != 1 {
		t.Fatalf("replay changed retained identity: %+v %v writes=%d", replayed, err, blobs.retainedWrites)
	}
	if blobs.versionReads < 3 {
		t.Fatal("runtime reused a role receipt instead of reading its retained bytes")
	}
}

// A good write return is insufficient when exact-version readback is corrupt.
func TestCompetitionRuntimeBlobPublicationRequiresAuthenticatedReadback(t *testing.T) {
	settings, store, archive, blobs := newCompetitionRuntimeBlobFixture(t)
	blobs.corruptReadback = true
	if _, err := archive.publishRuntimeManifest(context.Background(), settings, store.intent); err == nil || len(blobs.objects) != 0 {
		t.Fatal("corrupt retained bytes acquired a publication reference")
	}
}

// A late joining API and an already-serving API both fail closed after storage
// loses protection; no previous successful result or shared role row is a cache.
func TestCompetitionRuntimeBlobEveryReplicaChecksExactEnvelope(t *testing.T) {
	settings, store, archive, blobs := newCompetitionRuntimeBlobFixture(t)
	if _, err := archive.publishRuntimeManifest(context.Background(), settings, store.intent); err != nil {
		t.Fatal(err)
	}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	if ready, evalError := service.Ready(context.Background()); evalError != nil || !ready.Ready {
		t.Fatalf("published runtime was not ready: %+v %+v", ready, evalError)
	}
	blobs.stateLock.Lock()
	blobs.protectionErr = errors.New("synthetic protection outage")
	blobs.stateLock.Unlock()
	for _, replica := range []*Service{service, newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)} {
		if ready, evalError := replica.Ready(context.Background()); evalError == nil || ready.Ready {
			t.Fatal("a previous runtime acknowledgment masked missing protection")
		}
	}
}

// Every public path that loads the new runtime contract refuses absent bytes
// before admission, fee screening, polling or discovery can claim readiness.
func TestCompetitionRuntimeBlobApiPathsCannotBypassMissingInstallation(t *testing.T) {
	settings, store, _, _ := newCompetitionRuntimeBlobFixture(t)
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	principal := &Principal{Id: "synthetic-submitter", Role: "submitter"}
	args := ScoreArgs{RoundId: store.stagingRound.RoundId, Patch: testPatch("synthetic-runtime")}
	if _, status, err := service.SubmitV2(context.Background(), args, principal); status != http.StatusServiceUnavailable || err == nil || store.enqueueCalls != 0 {
		t.Fatal("admission bypassed missing runtime bytes")
	}
	if _, status, err := service.PreflightV2(context.Background(), args, principal); status != http.StatusServiceUnavailable || err == nil || store.screened != 0 {
		t.Fatal("preflight bypassed missing runtime bytes")
	}
	if _, status, err := service.GetScoreV2(context.Background(), store.getJob.JobId, principal); status != http.StatusServiceUnavailable || err == nil {
		t.Fatal("polling bypassed missing runtime bytes")
	}
	if _, err := service.Info(context.Background()); err == nil {
		t.Fatal("discovery bypassed missing runtime bytes")
	}
	if _, err := service.Leaderboards(context.Background(), true); err == nil {
		t.Fatal("leaderboards bypassed missing runtime bytes")
	}
}

// Capability is frozen and advertised verbatim; accepting two known contracts
// does not permit a new round to downgrade its installation requirement.
func TestCompetitionRuntimeBlobCapabilitySurvivesApiAndAdapterProjection(t *testing.T) {
	settings, store, archive, _ := newCompetitionRuntimeBlobFixture(t)
	if _, err := archive.publishRuntimeManifest(context.Background(), settings, store.intent); err != nil {
		t.Fatal(err)
	}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	info, evalError := service.Info(context.Background())
	if evalError != nil || info.RequiredApiCapability != model.CompetitionRuntimeBlobCapability || info.StagingRound.RequiredApiCapability != model.CompetitionRuntimeBlobCapability {
		t.Fatalf("discovery downgraded capability: %+v %+v", info, evalError)
	}
	if err := validateApexAdmissionContract(settings.CompetitionId, *info.StagingRound, settings.CompetitionId); err != nil {
		t.Fatal(err)
	}
	principal := &Principal{Id: "synthetic-submitter", Role: "submitter"}
	args := ScoreArgs{RoundId: store.stagingRound.RoundId, Patch: testPatch("synthetic-runtime")}
	preflight, status, evalError := service.PreflightV2(context.Background(), args, principal)
	if evalError != nil || status != http.StatusOK || preflight.RequiredApiCapability != model.CompetitionRuntimeBlobCapability {
		t.Fatalf("preflight downgraded capability: %+v %d %+v", preflight, status, evalError)
	}
	accepted, status, evalError := service.SubmitV2(context.Background(), args, principal)
	if evalError != nil || status != http.StatusAccepted || accepted.RequiredApiCapability != model.CompetitionRuntimeBlobCapability {
		t.Fatalf("admission downgraded capability: %+v %d %+v", accepted, status, evalError)
	}
	settings.AutomationPolicy.RequiredApiCapability = model.CompetitionAutomationCapability
	if _, err := settingsForAutomationRound(context.Background(), settings, store, store.stagingRound); err == nil {
		t.Fatal("older runtime configuration ignored frozen blob capability")
	}
}

// Reference tampering cannot select another immutable version or weaken the
// retention date, and a retained-body mutation always invalidates the runtime.
func TestCompetitionRuntimeBlobRejectsReferenceAndBodyTampering(t *testing.T) {
	settings, store, archive, blobs := newCompetitionRuntimeBlobFixture(t)
	receipt, err := archive.publishRuntimeManifest(context.Background(), settings, store.intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*competitionRuntimeManifestReceipt){
		func(value *competitionRuntimeManifestReceipt) { value.VersionId = "synthetic-other-version" },
		func(value *competitionRuntimeManifestReceipt) { value.Mode = "LOCAL" },
		func(value *competitionRuntimeManifestReceipt) {
			value.RetainUntil = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		},
		func(value *competitionRuntimeManifestReceipt) { value.Key += ".other" },
		func(value *competitionRuntimeManifestReceipt) { value.Bytes++ },
	} {
		altered := *receipt
		change(&altered)
		content, err := json.Marshal(altered)
		if err != nil {
			t.Fatal(err)
		}
		blobs.objects[receipt.Key+".receipt"] = content
		if LoadCompetitionRuntimeManifestContext(context.Background(), settings, store.intent) == nil {
			t.Fatal("altered runtime reference was accepted")
		}
	}
	content, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	blobs.objects[receipt.Key+".receipt"] = content
	blobs.corruptReadback = true
	if LoadCompetitionRuntimeManifestContext(context.Background(), settings, store.intent) == nil {
		t.Fatal("corrupt retained body was accepted")
	}
}

// Blob selection is never an implicit fallback for an uninstalled historical
// filesystem policy, and changing a complete intent changes the object key.
func TestCompetitionRuntimeBlobTransportAndIntentAreExplicit(t *testing.T) {
	settings, store, archive, _ := newCompetitionRuntimeBlobFixture(t)
	if _, err := archive.publishRuntimeManifest(context.Background(), settings, store.intent); err != nil {
		t.Fatal(err)
	}
	changed := *store.intent
	changed.TargetRoundId = server.RequireParseId("00000000-0000-0000-0000-000000000991")
	var err error
	changed.IntentSha256, err = competitionPreparationIntentDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if LoadCompetitionRuntimeManifestContext(context.Background(), settings, &changed) == nil {
		t.Fatal("another target round reused installed content")
	}
	t.Setenv("SIM_LATENCY_RUNTIME_MANIFEST_DIRECTORY", "")
	settings.AutomationPolicy.RequiredApiCapability = model.CompetitionAutomationCapability
	if LoadCompetitionRuntimeManifestContext(context.Background(), settings, store.intent) == nil {
		t.Fatal("missing filesystem installation silently fell back to blob mode")
	}
}
