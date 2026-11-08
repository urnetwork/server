// Synthetic local/archive actors exercise custody ordering and failure recovery.
// They do not qualify a real object store, provider or physical containment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
	"golang.org/x/sys/unix"
)

type winnerHandoffSpoolFixture struct {
	Directory string
	ClaimPath string
	Commit    winnerHandoffSpoolCommit
	Originals map[string][]byte
	Protected map[string][]byte
	Failure   string
}

// Every version is a distinct immutable actor entry; mutable latest retrieval
// is deliberately unavailable to the lifecycle under test.
func (self *winnerHandoffSpoolFixture) retain(_ context.Context, content []byte, digest string) (*controller.CompetitionWinnerReviewEvidence, error) {
	if self.Failure == "upload" {
		return nil, errors.New("synthetic protected upload failure")
	}
	version := fmt.Sprintf("synthetic-version-%d", len(self.Protected)+1)
	self.Protected[version] = bytes.Clone(content)
	return &controller.CompetitionWinnerReviewEvidence{Key: "synthetic/" + digest, VersionId: version, Sha256: digest, RetainUntil: self.Commit.MinimumUntil.Add(time.Hour)}, nil
}

func (self *winnerHandoffSpoolFixture) read(_ context.Context, original winnerHandoffCustodyOriginal) ([]byte, error) {
	if self.Failure == "readback" {
		return nil, errors.New("synthetic exact-version disconnection")
	}
	content, found := self.Protected[original.Retention.VersionId]
	if !found || original.Retention.Key != "synthetic/"+winnerMultipassSha256(content) || original.Retention.Sha256 != winnerMultipassSha256(content) || original.Bytes != int64(len(content)) {
		return nil, errors.New("synthetic exact protected version is unavailable")
	}
	return bytes.Clone(content), nil
}

// Only fixture bytes are accepted; this is intentionally not a mock claiming to
// have observed a real Docker child, mapped UID, network fence or model response.
func (self *winnerHandoffSpoolFixture) verify(commit winnerHandoffSpoolCommit, originals map[string][]byte) error {
	if commit.Binding != self.Commit.Binding || len(originals) != len(self.Originals) {
		return errors.New("synthetic portable original census changed")
	}
	for name, expected := range self.Originals {
		if !bytes.Equal(originals[name], expected) {
			return errors.New("synthetic portable original bytes changed")
		}
	}
	return nil
}

func newWinnerHandoffSpoolFixture(t *testing.T) *winnerHandoffSpoolFixture {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"attempts", "journal", "generation-claims"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	attempt := filepath.Join(root, "attempts", "invocation.synthetic")
	if err := os.Mkdir(attempt, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(attempt, "member-state"), 0700); err != nil {
		t.Fatal(err)
	}
	digest := winnerMultipassSha256([]byte("synthetic pinned release"))
	fixture := &winnerHandoffSpoolFixture{Directory: filepath.Join(root, "journal"), ClaimPath: filepath.Join(root, "generation-claims", "synthetic-one-shot.json"), Originals: map[string][]byte{}, Protected: map[string][]byte{},
		Commit: winnerHandoffSpoolCommit{Schema: 1, Kind: "winner-review-protected-spool-commit/v1", Lifecycle: "protected-exact-version-spool/v1", AttemptRoot: filepath.Join(root, "attempts"), Attempt: attempt,
			Binding:      winnerHandoffSpoolBinding{RunId: "synthetic-review-owner", InvocationPosition: 1, PlanSha256: digest, BundleSha256: digest, ReleaseSha256: digest, NativeDriverSha256: digest, CapacitySha256: digest},
			MinimumUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Limits: defaultWinnerHandoffSpoolLimits(), Extra: []winnerHandoffCustodyOriginal{}}}
	if err := writePrivateReviewFile(fixture.ClaimPath, []byte("synthetic durable one-shot claim")); err != nil {
		t.Fatal(err)
	}
	originals := map[string][]byte{"input.json": []byte("synthetic original input"), "transport.response": []byte("synthetic complete visible original"), "broker.stderr": {}, "evidence-manifest.json": []byte("synthetic manifest")}
	receipt := &winnerMultipassContainmentReceipt{AttemptDirectory: attempt, ManifestSha256: winnerMultipassSha256(originals["evidence-manifest.json"]), Artifacts: []currentQgQualificationBinding{}}
	invocation := winnerHandoffInvocationCustody{Phase: "capture", Operation: "generate", CallId: "qwen-scan-0001", RequestSha256: winnerMultipassSha256(originals["input.json"]), ManifestSha256: receipt.ManifestSha256, Originals: []winnerHandoffCustodyOriginal{}}
	for name, content := range originals {
		if err := os.WriteFile(filepath.Join(attempt, name), content, 0600); err != nil {
			t.Fatal(err)
		}
		if name == "transport.response" {
			if err := os.WriteFile(filepath.Join(attempt, "member-state", name), content, 0600); err != nil {
				t.Fatal(err)
			}
		}
		receipt.Artifacts = append(receipt.Artifacts, currentQgQualificationBinding{Path: name, Sha256: winnerMultipassSha256(content), Bytes: int64(len(content))})
		original, err := retainWinnerHandoffOriginal(t.Context(), name, content, int(fixture.Commit.Limits.MaximumLeafBytes), fixture.Commit.MinimumUntil, fixture.retain)
		if err != nil {
			t.Fatal(err)
		}
		invocation.Originals = append(invocation.Originals, original)
		fixture.Originals[name] = content
	}
	for _, name := range []string{"stdin.pipe", "stdout.pipe", "stderr.pipe"} {
		if err := unix.Mkfifo(filepath.Join(attempt, name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"wrapper-receipt.json", "physical-authentication.json", "invocation-capture.json"} {
		content := []byte("synthetic original " + name)
		original, err := retainWinnerHandoffDocument(t.Context(), name, content, int(fixture.Commit.Limits.MaximumCaptureBytes), fixture.Commit.MinimumUntil, fixture.retain)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Originals[name] = content
		switch name {
		case "wrapper-receipt.json":
			invocation.WrapperReceiptSha256 = original.Retention.Sha256
			invocation.Originals = append(invocation.Originals, original)
		case "physical-authentication.json":
			fixture.Commit.Physical = original
		default:
			fixture.Commit.Extra = append(fixture.Commit.Extra, original)
		}
	}
	entries, err := snapshotWinnerHandoffSpool(fixture.Commit.AttemptRoot, receipt, fixture.Commit.Limits)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Commit.Entries, fixture.Commit.Invocation = entries, invocation
	return fixture
}

func TestWinnerHandoffSpoolRetainsEveryOriginalBeforeEvictionAndExactReplay(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	spool, err := commitWinnerHandoffSpool(t.Context(), fixture.Directory, fixture.Commit, fixture.retain, fixture.read, fixture.verify)
	if err != nil {
		t.Fatal(err)
	}
	if err := evictWinnerHandoffSpool(t.Context(), spool, fixture.read, fixture.verify, unix.Unlinkat); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(fixture.Commit.Attempt); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("protected payload attempt was not retired", err)
	}
	loaded, err := loadWinnerHandoffSpool(t.Context(), spool.referencePath, fixture.Commit.Binding, fixture.Commit.AttemptRoot, fixture.read, fixture.verify)
	if err != nil || loaded == nil {
		t.Fatal("exact protected versions did not replay without local originals", err)
	}
	if err := evictWinnerHandoffSpool(t.Context(), loaded, fixture.read, fixture.verify, unix.Unlinkat); err != nil {
		t.Fatal("already completed cleanup did not remain idempotent", err)
	}
}

func TestWinnerHandoffSpoolArchiveFailuresKeepLocalOriginalsAndFenceRecovery(t *testing.T) {
	for _, failure := range []string{"upload", "readback"} {
		fixture := newWinnerHandoffSpoolFixture(t)
		fixture.Failure = failure
		if _, err := commitWinnerHandoffSpool(t.Context(), fixture.Directory, fixture.Commit, fixture.retain, fixture.read, fixture.verify); err == nil {
			t.Fatal("archive failure authorized cleanup", failure)
		}
		if _, err := os.Stat(filepath.Join(fixture.Commit.Attempt, "input.json")); err != nil {
			t.Fatal("original removed after failed protected custody", failure, err)
		}
		fixture.Failure = ""
		path := filepath.Join(fixture.Directory, "invocation-000001.retention.json")
		if _, err := loadWinnerHandoffSpool(t.Context(), path, fixture.Commit.Binding, fixture.Commit.AttemptRoot, fixture.read, fixture.verify); err == nil {
			t.Fatal("missing final custody journal was treated as completed", failure)
		}
	}
}

func TestWinnerHandoffSpoolUnknownEntryRefusesBeforeAnyDeletion(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	spool, err := commitWinnerHandoffSpool(t.Context(), fixture.Directory, fixture.Commit, fixture.retain, fixture.read, fixture.verify)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.Commit.Attempt, "unexpected-user-data"), []byte("not owned by this invocation"), 0600); err != nil {
		t.Fatal(err)
	}
	removed := 0
	err = evictWinnerHandoffSpool(t.Context(), spool, fixture.read, fixture.verify, func(fd int, path string, flags int) error { removed++; return unix.Unlinkat(fd, path, flags) })
	if err == nil || removed != 0 {
		t.Fatal("unknown local entry did not stop the whole cleanup preflight", err, removed)
	}
}

func TestWinnerHandoffSpoolPartialCleanupRestoresFromExactVersionsWithoutNewDraw(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	spool, err := commitWinnerHandoffSpool(t.Context(), fixture.Directory, fixture.Commit, fixture.retain, fixture.read, fixture.verify)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	err = evictWinnerHandoffSpool(t.Context(), spool, fixture.read, fixture.verify, func(fd int, path string, flags int) error {
		if removed == 2 {
			return errors.New("synthetic crash boundary after two successful unlinks")
		}
		removed++
		return unix.Unlinkat(fd, path, flags)
	})
	if err == nil || removed != 2 {
		t.Fatal("explicit cleanup crash barrier was not exercised", err, removed)
	}
	loaded, err := loadWinnerHandoffSpool(t.Context(), spool.referencePath, fixture.Commit.Binding, fixture.Commit.AttemptRoot, fixture.read, fixture.verify)
	if err != nil {
		t.Fatal("protected originals could not restore custody after partial unlink", err)
	}
	if err := evictWinnerHandoffSpool(t.Context(), loaded, fixture.read, fixture.verify, unix.Unlinkat); err != nil {
		t.Fatal("remaining owned cleanup failed", err)
	}
	claim, err := os.ReadFile(fixture.ClaimPath)
	if err != nil || string(claim) != "synthetic durable one-shot claim" {
		t.Fatal("partial cleanup removed the independent one-shot fence", err)
	}
}

func TestWinnerHandoffSpoolPayloadEvictionNeverReleasesOneShotClaim(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	spool, err := commitWinnerHandoffSpool(t.Context(), fixture.Directory, fixture.Commit, fixture.retain, fixture.read, fixture.verify)
	if err != nil {
		t.Fatal(err)
	}
	if err := evictWinnerHandoffSpool(t.Context(), spool, fixture.read, fixture.verify, unix.Unlinkat); err != nil {
		t.Fatal(err)
	}
	// This is the same exclusive-create custody boundary as the wrapper's
	// noclobber claim. Provider work must remain after that successful boundary.
	providerRequests := 0
	claim, err := os.OpenFile(fixture.ClaimPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err == nil {
		claim.Close()
		providerRequests++
	}
	if !errors.Is(err, os.ErrExist) || providerRequests != 0 {
		t.Fatal("retiring payload copies allowed a second owner/call draw", err)
	}
}

func TestWinnerHandoffSpoolChangedVersionAndChangedIdentityNeverEvict(t *testing.T) {
	for _, mutation := range []string{"version", "inode", "hardlink", "symlink"} {
		fixture := newWinnerHandoffSpoolFixture(t)
		spool, err := commitWinnerHandoffSpool(t.Context(), fixture.Directory, fixture.Commit, fixture.retain, fixture.read, fixture.verify)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixture.Commit.Attempt, "input.json")
		switch mutation {
		case "version":
			delete(fixture.Protected, spool.reference.Commit.Retention.VersionId)
		case "inode":
			if err := os.Rename(path, filepath.Join(fixture.Directory, "saved-input.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, fixture.Originals["input.json"], 0600); err != nil {
				t.Fatal(err)
			}
		case "hardlink":
			if err := os.Link(path, filepath.Join(fixture.Directory, "extra-input-link")); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err := os.Rename(path, filepath.Join(fixture.Directory, "saved-input.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(fixture.Directory, "saved-input.json"), path); err != nil {
				t.Fatal(err)
			}
		}
		removed := 0
		err = evictWinnerHandoffSpool(t.Context(), spool, fixture.read, fixture.verify, func(fd int, path string, flags int) error { removed++; return unix.Unlinkat(fd, path, flags) })
		if err == nil || removed != 0 {
			t.Fatal("changed protected/local identity allowed payload cleanup", mutation, err, removed)
		}
	}
}

func TestWinnerHandoffSpoolCompactAndCaptureBoundsCannotBeRaised(t *testing.T) {
	fixture := newWinnerHandoffSpoolFixture(t)
	for _, mutate := range []func(*winnerHandoffSpoolCommit){
		func(value *winnerHandoffSpoolCommit) { value.Limits.MaximumCommitBytes++ },
		func(value *winnerHandoffSpoolCommit) { value.Limits.MaximumCaptureBytes++ },
		func(value *winnerHandoffSpoolCommit) { value.Attempt = value.AttemptRoot },
		func(value *winnerHandoffSpoolCommit) {
			value.Attempt = filepath.Join(filepath.Dir(value.AttemptRoot), "generation-claims")
		},
	} {
		encoded, _ := json.Marshal(fixture.Commit)
		var changed winnerHandoffSpoolCommit
		if err := json.Unmarshal(encoded, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if err := validateWinnerHandoffSpoolCommit(changed, fixture.Commit.Binding, fixture.Commit.AttemptRoot); err == nil {
			t.Fatal("spool limits or cleanup scope silently widened")
		}
	}
}
