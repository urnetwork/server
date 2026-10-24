// Fixed offline correctness tests cover the permitted contract-manager patch
// surface and protected simulator/scoring regressions without backing services.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// These selectors are an immutable suite protocol, not candidate input or an
// agent-chosen shell command. They exclude DB integration tests deliberately;
// separate live smoke/containment qualification remains mandatory.
func sourceCorrectnessSuiteScript() string {
	return "go test -json -count=1 -run '^(TestDbClassifiesWrappedConnectionWriteTimeout|TestDbRejectsUnsafePgprotoWriteRetry|TestIsDoneErrorRequiresExplicitShutdownSignal)$' .; " +
		"go test -json -count=1 -run '^TestResidentContract' ./connect; " +
		"go test -json -count=1 -run '^(TestScoreProducesSharedContract|TestBuildScoreBaselineAuthenticatesDriverCsvPrecision|TestSimulationExchangeSettingsUseOnlyH1Listeners|TestSimulationHandlerHeartbeatsStopBeforeNextRefresh|TestSimulationHandlerHeartbeatFailureStopsServices)$' ./connect/sim-latency"
}

// Each mandatory named test must actually run and pass. Prefix selection alone
// or a successful package exit can otherwise silently accept missing tests.
func sourceCorrectnessRequiredTests() map[string][]string {
	return map[string][]string{
		"github.com/urnetwork/server": {
			"TestDbClassifiesWrappedConnectionWriteTimeout", "TestDbRejectsUnsafePgprotoWriteRetry", "TestIsDoneErrorRequiresExplicitShutdownSignal",
		},
		"github.com/urnetwork/server/connect": {
			"TestResidentContractManagerCanceledLookupReturnsInactive",
			"TestResidentContractRefreshRaiseKeepsResident",
			"TestResidentContractCacheRetiresExpiredProviderHistory", "TestResidentContractCacheRetiresDeniedProviderHistory", "TestResidentContractCacheDisabledDoesNotRetainProviderHistory",
			"TestResidentContractReadCloseJoinsColdOwner", "TestResidentContractReadDropsConcurrentUncheckedPackets", "TestResidentContractReadLimitsNegativeAndRechecksAtOneSecond",
			"TestResidentContractReadCanonicalPairIsolation", "TestResidentContractReadOtherPairsRemainParallel", "TestResidentContractReadRefreshPreservesFiveSecondBoundary",
			"TestResidentContractReadCanceledOwnerDoesNotPublish", "TestResidentContractReadFailureRetainsInterval",
			"TestResidentContractLeaseExpiresBeforeOrdinaryCache", "TestResidentContractLeaseExpiredDuringReadCannotPublish", "TestResidentContractLeaseDelayedAdmissionDoesNotReuseOldClock",
			"TestResidentContractLeaseBlockedRefreshDoesNotExtendAuthority", "TestResidentContractLeaseFallbackPreservesDeadline",
			"TestResidentContractFallbackSharedCapDropsWithoutWaiting", "TestResidentContractFallbackRechecksAtExactPairInterval", "TestResidentContractFallbackDeadlineErrorAndPanicReleaseAdmission",
			"TestResidentContractFallbackCloseJoinsSourceOwner", "TestResidentContractFallbackPreservesInheritedPostgresGuard", "TestResidentContractFallbackCanceledAdmissionHasNoSource",
			"TestResidentContractFallbackMetricsHaveFiniteOutcomes",
		},
		"github.com/urnetwork/server/connect/sim-latency": {
			"TestScoreProducesSharedContract", "TestBuildScoreBaselineAuthenticatesDriverCsvPrecision", "TestSimulationExchangeSettingsUseOnlyH1Listeners",
			"TestSimulationHandlerHeartbeatsStopBeforeNextRefresh", "TestSimulationHandlerHeartbeatFailureStopsServices",
		},
	}
}

func sourceCorrectnessSuiteDigest() string {
	content, _ := json.Marshal(struct {
		Protocol string              `json:"protocol"`
		Script   string              `json:"script"`
		Required map[string][]string `json:"required"`
	}{Protocol: "reviewed-source-offline-correctness-v1", Script: sourceCorrectnessSuiteScript(), Required: sourceCorrectnessRequiredTests()})
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Parse observed Go test events, not text matching "PASS" or a model summary.
func countSourceCorrectnessTests(output []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	states, packages := map[string]string{}, map[string]bool{}
	running := map[string]int{}
	passed := 0
	for {
		var event struct{ Action, Package, Test string }
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return 0, errors.New("correctness suite did not return complete Go test events")
		}
		if event.Action == "fail" {
			return 0, errors.New("correctness suite reported a failed test or package")
		}
		if event.Test == "" {
			if event.Action == "pass" {
				if packages[event.Package] {
					return 0, errors.New("correctness suite repeated a package completion")
				}
				if running[event.Package] != 0 {
					return 0, errors.New("correctness suite completed with an unfinished test")
				}
				packages[event.Package] = true
			}
			continue
		}
		key := event.Package + "/" + event.Test
		switch event.Action {
		case "run":
			if states[key] != "" || packages[event.Package] {
				return 0, errors.New("correctness suite repeated a test execution")
			}
			states[key] = "run"
			running[event.Package]++
		case "pass", "skip":
			if states[key] != "run" {
				return 0, errors.New("correctness suite completed a test without its run event")
			}
			states[key] = event.Action
			running[event.Package]--
			if event.Action == "pass" && !strings.Contains(event.Test, "/") {
				passed++
			}
		}
	}
	for pkg, names := range sourceCorrectnessRequiredTests() {
		if !packages[pkg] {
			return 0, errors.New("correctness suite did not complete a required package")
		}
		for _, name := range names {
			if states[pkg+"/"+name] != "pass" {
				return 0, errors.New("correctness suite omitted or skipped a mandatory test")
			}
		}
	}
	return passed, nil
}

// Called only through the prescribed-test tool. No model input selects the
// command, image mount, cache, credentials or authority of this executor.
func executeSourceCorrectnessSuite(ctx context.Context, request reviewedSourceBuildRequest, artifacts sourceBuildArtifacts, owner string, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (*sourceTestExecution, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return nil, err
	}
	if artifacts.Directory != filepath.Join(settings.StateDirectory, owner) || !validSha256(owner) ||
		!strings.HasPrefix(artifacts.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(artifacts.ImageId, "sha256:")) {
		return nil, errors.New("correctness suite artifacts do not bind this private attempt")
	}
	canonical, pathErr := filepath.EvalSymlinks(artifacts.Directory)
	rootInfo, rootErr := os.Lstat(artifacts.Directory)
	if pathErr != nil || rootErr != nil || canonical != artifacts.Directory || !rootInfo.IsDir() || rootInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("correctness suite context is not a canonical private directory")
	}
	metadataPath := filepath.Join(artifacts.Directory, "metadata", "output", "metadata.tar")
	info, err := os.Lstat(metadataPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > settings.MetadataBytes {
		return nil, errors.New("correctness suite module seed is unavailable or redirected")
	}
	archive, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}
	metadata, err := decodeSourceBuildMetadata(archive, settings)
	if err != nil {
		return nil, err
	}
	phaseDirectory := filepath.Join(artifacts.Directory, "correctness")
	if err := os.Mkdir(phaseDirectory, 0o700); err != nil {
		return nil, err
	}
	input := sourceBuildSandboxInput{Owner: owner, Phase: "correctness", ImageId: artifacts.ImageId, Commit: request.Proposal.Commits["server"],
		SourceDirectory: filepath.Join(artifacts.Directory, "source"), MetadataDirectory: filepath.Join(phaseDirectory, "metadata"), OutputDirectory: filepath.Join(phaseDirectory, "output")}
	if err := os.Mkdir(input.OutputDirectory, 0o700); err != nil {
		return nil, err
	}
	if err := writeSourceBuildMetadata(input.MetadataDirectory, metadata); err != nil {
		return nil, err
	}
	started := time.Now()
	output, diagnostics, executionErr := executeSourceBuildSandbox(ctx, input, settings, docker)
	for name, data := range map[string][]byte{"go-test.jsonl": output, "stderr.log": diagnostics} {
		if err := writePrivateReviewFile(filepath.Join(phaseDirectory, name), data); err != nil {
			return nil, errors.Join(executionErr, err)
		}
	}
	if executionErr != nil {
		return nil, executionErr
	}
	count, err := countSourceCorrectnessTests(output)
	if err != nil {
		return nil, err
	}
	content, err := json.Marshal(struct {
		Stdout []byte `json:"stdout"`
		Stderr []byte `json:"stderr"`
	}{Stdout: output, Stderr: diagnostics})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if err := writePrivateReviewFile(filepath.Join(phaseDirectory, "test-logs.json"), content); err != nil {
		return nil, err
	}
	return &sourceTestExecution{ImageId: artifacts.ImageId, SuiteSha256: sourceCorrectnessSuiteDigest(), ExitCode: 0,
		TestCount: count, DurationMillis: time.Since(started).Milliseconds(), TestLogsSha256: hex.EncodeToString(digest[:])}, nil
}
