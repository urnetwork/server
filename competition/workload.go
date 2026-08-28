package competition

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/urnetwork/server"
)

const (
	workloadSeedDomain   = "urnetwork-sim-latency-generator-v1\x00"
	maxProvidersFileSize = 1 << 30
)

type workloadArtifact struct {
	Path   string
	Sha256 string
	Bytes  int64
}

// WorkloadGenerator is the trusted round-fixture boundary. Production uses
// CommandWorkloadGenerator; the interface also lets database tests avoid
// executing a season binary.
type WorkloadGenerator interface {
	Generate(context.Context, *Settings, server.Id, string) (workloadArtifact, error)
}

type CommandWorkloadGenerator struct{}

func (CommandWorkloadGenerator) Generate(ctx context.Context, settings *Settings, roundId server.Id, seedHex string) (artifact workloadArtifact, err error) {
	if err := verifyPinnedExecutable(settings.SimulatorCommand, settings.EvaluationPolicy.SimulatorSha256); err != nil {
		return artifact, fmt.Errorf("simulator identity: %w", err)
	}
	seed, err := workloadSeed(seedHex)
	if err != nil {
		return artifact, err
	}
	roundDirectory, err := createRoundArtifactDirectory(settings.ArtifactRoot, roundId)
	if err != nil {
		return artifact, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(filepath.Join(roundDirectory, "providers.yml"))
			_ = os.Remove(roundDirectory)
		}
	}()
	providersPath := filepath.Join(roundDirectory, "providers.yml")
	p := settings.EvaluationPolicy
	args := []string{
		"init",
		"--out=" + providersPath,
		"--count=" + strconv.Itoa(p.ProviderCount),
		"--clients=" + strconv.Itoa(p.ClientPoolSize),
		"--rate=" + strconv.Itoa(p.ArrivalsPerMinute),
		"--seed=" + strconv.FormatInt(seed, 10),
		"--quality-window=" + strconv.Itoa(p.QualityWindowSize),
	}
	stdout := &boundedBuffer{limit: maxSelfCheckBytes}
	stderr := &boundedBuffer{limit: maxSelfCheckBytes}
	exitCode, runErr := runContainedCommand(ctx, roundDirectory, settings.SimulatorCommand, args, stdout, stderr)
	if runErr != nil {
		return artifact, fmt.Errorf("workload generator: %w", runErr)
	}
	if exitCode != 0 {
		return artifact, fmt.Errorf("workload generator exited %d", exitCode)
	}
	digest, size, err := hashRegularFile(providersPath)
	if err != nil || size <= 0 || maxProvidersFileSize < size {
		return artifact, errors.New("generated providers file is absent, empty, oversized, or unreadable")
	}
	if err := os.Chmod(providersPath, 0400); err != nil {
		return artifact, err
	}
	keep = true
	return workloadArtifact{Path: providersPath, Sha256: digest, Bytes: size}, nil
}

func generateRoundWorkload(ctx context.Context, settings *Settings, roundId server.Id, seedHex string) (workloadArtifact, error) {
	generator := settings.workloadGenerator
	if generator == nil {
		generator = CommandWorkloadGenerator{}
	}
	artifact, err := generator.Generate(ctx, settings, roundId, seedHex)
	if err != nil {
		return workloadArtifact{}, err
	}
	if !filepath.IsAbs(artifact.Path) || !sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes <= 0 {
		return workloadArtifact{}, errors.New("workload generator returned an invalid artifact identity")
	}
	return artifact, nil
}

func workloadSeed(seedHex string) (int64, error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != 32 {
		return 0, errors.New("round seed is malformed")
	}
	defer clear(seed)
	h := sha256.New()
	h.Write([]byte(workloadSeedDomain))
	h.Write(seed)
	digest := h.Sum(nil)
	value := binary.BigEndian.Uint64(digest[:8]) & math.MaxInt64
	if value == 0 {
		value = 1
	}
	return int64(value), nil
}

func createRoundArtifactDirectory(root string, roundId server.Id) (string, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&0022 != 0 {
		return "", errors.New("artifact root must be an existing non-group/world-writable directory")
	}
	rounds := filepath.Join(root, "rounds")
	if err := os.Mkdir(rounds, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	roundsInfo, err := os.Lstat(rounds)
	if err != nil || !roundsInfo.IsDir() || roundsInfo.Mode()&0022 != 0 {
		return "", errors.New("round artifact root is unsafe")
	}
	directory := filepath.Join(rounds, roundId.String())
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", err
	}
	return directory, nil
}

func removeRoundWorkload(settings *Settings, round *roundRecord) {
	if settings == nil || round == nil || round.ProvidersPath == "" {
		return
	}
	expectedDirectory := filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String())
	if filepath.Dir(round.ProvidersPath) != expectedDirectory || filepath.Base(round.ProvidersPath) != "providers.yml" {
		return
	}
	_ = os.Remove(round.ProvidersPath)
	_ = os.Remove(expectedDirectory)
}

func readRoundWorkload(ctx context.Context, settings *Settings, round *roundRecord) ([]byte, error) {
	if settings == nil || round == nil || !sha256Pattern.MatchString(round.ProvidersSha256) {
		return nil, errors.New("round workload identity is missing")
	}
	expected := filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String(), "providers.yml")
	if round.ProvidersPath != expected {
		return nil, errors.New("round workload path does not match its immutable identity")
	}
	bytes, err := readRegularFile(round.ProvidersPath, maxProvidersFileSize)
	if err != nil {
		if settings.artifactArchive == nil {
			return nil, err
		}
		// MinIO is the durable copy. Falling back to it makes a local artifact
		// disk loss recoverable without weakening the immutable round identity;
		// the archive reader authenticates the committed SHA-256 before return.
		return settings.artifactArchive.ReadRoundWorkload(ctx, settings, round)
	}
	digest := sha256.Sum256(bytes)
	if hex.EncodeToString(digest[:]) != round.ProvidersSha256 {
		clear(bytes)
		return nil, errors.New("round workload hash mismatch")
	}
	return bytes, nil
}
