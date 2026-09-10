package controller

// These tests lock the control-plane workload generator to the frozen
// simulator encoding and its API-local filesystem boundary.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sampling must round the multiplication before addition so every supported
// architecture produces the amd64-frozen workload bytes.
func TestSimLatencyRangeSampleUsesCanonicalRounding(t *testing.T) {
	value := (simLatencyRange{Min: 10, Max: 40}).sample(newSimLatencyWorkloadRng(0))
	if bits := math.Float64bits(value); bits != 0x40432d8d9f62d9b8 {
		t.Fatalf("canonical sampled float bits = %016x", bits)
	}
}

// Captures the ephemeral workload while delegating all other archive methods
// to the ordinary deterministic test implementation.
type workloadInspectingArtifactArchive struct {
	fakeArtifactArchive
	path  string
	bytes []byte
}

// Records the source artifact while it must still exist.
func (self *workloadInspectingArtifactArchive) ArchiveRound(
	_ context.Context,
	_ *Settings,
	_ *roundRecord,
	workload workloadArtifact,
) error {
	workloadBytes, err := os.ReadFile(workload.Path)
	if err != nil {
		return err
	}
	self.path = workload.Path
	self.bytes = workloadBytes
	return nil
}

// The shared Go encoder must remain byte-identical to the frozen evaluator
// binary used to produce the competition baseline.
func TestGenerateSimLatencyWorkloadMatchesFrozenSimulator(t *testing.T) {
	workloadBytes, err := GenerateSimLatencyWorkload(48, 25, 5, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(workloadBytes)
	if actual := hex.EncodeToString(digest[:]); actual != "feaaf0d1f267774bfd6f8fa2f145a3b52c8c454ce9ebd76df007aac0a6ea2b41" {
		t.Fatalf("frozen workload SHA-256 = %s", actual)
	}
	if len(workloadBytes) != 22910 {
		t.Fatalf("frozen workload bytes = %d, want 22910", len(workloadBytes))
	}
}

// Round preparation executes in the API container. It must use temporary API
// storage while persisting the canonical evaluator path in the round record.
func TestPrepareRoundDoesNotRequireEvaluatorHostFilesystem(t *testing.T) {
	settings := validSettings()
	settings.ArtifactRoot = "/evaluator-host-only/competition"
	settings.SimulatorCommand = "/evaluator-host-only/sim-latency"
	settings.workloadGenerator = nil
	archive := &workloadInspectingArtifactArchive{}
	settings.artifactArchive = archive
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	store := PostgresStore{now: func() time.Time { return now }}

	round, err := store.prepareRound(context.Background(), settings, GenerateRoundArgs{
		OpensAt: now.Add(time.Hour), ClosesAt: now.Add(2 * time.Hour),
		RevealAt: now.Add(2 * time.Hour),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	expectedPath := filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String(), "providers.yml")
	if round.ProvidersPath != expectedPath {
		t.Fatalf("committed providers path = %q, want %q", round.ProvidersPath, expectedPath)
	}
	if archive.path == "" || strings.HasPrefix(archive.path, settings.ArtifactRoot+string(filepath.Separator)) {
		t.Fatalf("API-local workload path = %q", archive.path)
	}
	if _, statErr := os.Stat(archive.path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary workload survived archive: %v", statErr)
	}
	digest := sha256.Sum256(archive.bytes)
	if round.ProvidersSha256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("committed workload SHA-256 = %s", round.ProvidersSha256)
	}
}
