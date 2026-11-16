// The database lifecycle fixture must retain actual generated workload bytes.
// These private-file controls exercise that exact pairing without a database,
// object-store transport or a synthetic successful ArchiveRound implementation.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Observation preserves the complete retained write/readback path. It records
// only the generated file identity while the ordinary caller owns its lifetime.
type competitionIntakeFirstWorkloadArchiveWitness struct {
	artifactArchive
	workload workloadArtifact
}

func (self *competitionIntakeFirstWorkloadArchiveWitness) ArchiveRound(ctx context.Context, settings *Settings, round *roundRecord, workload workloadArtifact) error {
	self.workload = workload
	return self.artifactArchive.ArchiveRound(ctx, settings, round, workload)
}

// The owned fixture uses a small real encoding, not a path/size/hash stand-in.
func competitionIntakeFirstArchiveFixture(t *testing.T) (*Settings, PostgresStore, *blobArtifactArchive, *competitionIntakeFirstWorkloadArchiveWitness, GenerateRoundArgs) {
	t.Helper()
	settings := automationApiSettings()
	settings.ArtifactRoot = t.TempDir()
	settings.automationTargetRoundId = server.NewId()
	settings.EvaluationPolicy.ProviderCount, settings.EvaluationPolicy.ClientPoolSize = 25, 5
	settings.EvaluationPolicy.ArrivalsPerMinute, settings.EvaluationPolicy.QualityWindowSize = 8, 2
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	settings.RetainUntil = now.Add(400 * 24 * time.Hour)
	archive := newCompetitionIntakeFirstRetainedArchive(settings)
	witness := &competitionIntakeFirstWorkloadArchiveWitness{artifactArchive: archive}
	settings.artifactArchive = witness
	return settings, PostgresStore{now: func() time.Time { return now }}, archive, witness,
		GenerateRoundArgs{OpensAt: now, ClosesAt: now.Add(7 * 24 * time.Hour), RevealAt: now.Add(7 * 24 * time.Hour)}
}

// A metadata-only generator used to reach PutRetained with a nonexistent path.
// The positive path must own real immutable bytes, prove exact version readback
// and retire only the temporary source after the actual archive has returned.
func TestCompetitionIntakeFirstRetainedArchiveMaterializesWorkload(t *testing.T) {
	settings, store, archive, witness, args := competitionIntakeFirstArchiveFixture(t)
	round, err := store.prepareRound(t.Context(), settings, args, false)
	if err != nil || round == nil {
		t.Fatal("intake retained archive could not consume its owned workload bytes", err)
	}
	artifact := witness.workload
	if !artifact.RemoveAfterUse || artifact.Bytes <= 1 || artifact.Sha256 != round.ProvidersSha256 || artifact.Path == round.ProvidersPath {
		t.Fatal("intake retained archive used a metadata stand-in or the committed evaluator path")
	}
	if _, err := os.Lstat(artifact.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary workload did not retire after genuine retained readback", err)
	}
	blobs := archive.store.(*competitionRuntimeBlobTestStore)
	blobs.stateLock.Lock()
	writes, readbacks := blobs.retainedWrites, blobs.versionReads
	var key, version string
	for retainedKey := range blobs.versions {
		key, version, _ = strings.Cut(retainedKey, "\x00")
	}
	versionCount := len(blobs.versions)
	blobs.stateLock.Unlock()
	if writes != 1 || readbacks != 1 || versionCount != 1 || version == "" || key != archive.roundWorkloadKey(settings, round, round.ProvidersSha256) {
		t.Fatal("actual archive did not complete one exact retained write/readback", writes, readbacks, versionCount)
	}
	reader, err := blobs.GetVersion(t.Context(), key, version)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(io.LimitReader(reader, artifact.Bytes+1))
	closeErr := reader.Close()
	digest := sha256.Sum256(content)
	if readErr != nil || closeErr != nil || int64(len(content)) != artifact.Bytes || hex.EncodeToString(digest[:]) != artifact.Sha256 {
		t.Fatal("retained workload bytes no longer authenticate after temporary-file retirement", readErr, closeErr)
	}
}

// The old fake remains valid with a no-op archive, but cannot claim a retained
// upload. A real filesystem error must survive the ordinary safe-error wrapper.
func TestCompetitionIntakeFirstRetainedArchiveRejectsMetadataOnlyWorkload(t *testing.T) {
	settings, store, archive, _, args := competitionIntakeFirstArchiveFixture(t)
	settings.workloadGenerator = fakeWorkloadGenerator{}
	round, err := store.prepareRound(t.Context(), settings, args, false)
	var pathError *os.PathError
	if round != nil || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathError) || pathError.Op != "open" {
		t.Fatal("metadata-only workload bypassed real retained file ownership", err)
	}
	blobs := archive.store.(*competitionRuntimeBlobTestStore)
	blobs.stateLock.Lock()
	writes, readbacks, versions := blobs.retainedWrites, blobs.versionReads, len(blobs.versions)
	blobs.stateLock.Unlock()
	if writes != 0 || readbacks != 0 || versions != 0 {
		t.Fatal("nonexistent source acquired fabricated retention proof", writes, readbacks, versions)
	}
}
