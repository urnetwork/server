// Early generic failures use the real evaluator authentication, retained-blob
// verification and durable completion path; fixtures cannot invent its proof flag.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// The only substituted boundary is the peer: actual framed file transfer and
// signatures feed the production evaluator and byte-backed retained archive.
type competitionOperatorPatchRetryFailureTransport struct {
	t          testing.TB
	host       CompetitionExecutorHostPolicy
	directory  string
	manifest   *competitionagent.StartManifest
	response   competitionagent.Response
	operations []string
}

// No host process runs. Synthetic inputs cross the same bounded file protocol,
// and the response is authenticated against the fixture's frozen public key.
func (self *competitionOperatorPatchRetryFailureTransport) Exchange(ctx context.Context, request competitionagent.Request, inputDirectory, outputDirectory string) (competitionagent.Response, error) {
	self.t.Helper()
	if err := ctx.Err(); err != nil {
		return competitionagent.Response{}, err
	}
	self.operations = append(self.operations, request.Operation)
	limits := competitionagent.DefaultLimits()
	transfer := func(source, destination string, files []competitionagent.File, maximum int64) {
		var stream bytes.Buffer
		if err := competitionagent.WriteFiles(&stream, source, files, maximum, limits); err != nil {
			self.t.Fatal("synthetic framed transfer write failed", err)
		}
		if err := competitionagent.ReadFiles(&stream, destination, files, maximum, limits); err != nil || stream.Len() != 0 {
			self.t.Fatal("synthetic framed transfer authentication failed", err)
		}
	}
	switch request.Operation {
	case "start":
		if self.manifest != nil || request.Start == nil || inputDirectory == "" || outputDirectory != "" {
			self.t.Fatal("synthetic peer received unexpected repeated/malformed start")
		}
		manifest := *request.Start
		self.manifest = &manifest
		transfer(inputDirectory, self.directory, manifest.Inputs, limits.InputBytes)
		var workerRequest evaluatorRequest
		if err := decodeFailureEvidenceJson(manifest.EvaluatorRequest, &workerRequest); err != nil {
			self.t.Fatal(err)
		}
		workerRequest.ArtifactDirectory = self.directory
		workerRequest.PatchPath, workerRequest.ProvidersPath = filepath.Join(self.directory, "canonical.patch"), filepath.Join(self.directory, "providers.yml")
		workerRequest.RoundBaselinePath = filepath.Join(self.directory, "round-baseline.json")
		workerRequest.ConfigLocalDirectory, workerRequest.VaultLocalDirectory = filepath.Join(self.directory, "config", "local"), filepath.Join(self.directory, "vault", "local")
		requestContent, err := json.Marshal(workerRequest)
		if err != nil {
			self.t.Fatal(err)
		}
		workerRequest.RoundSeedHex = ""
		if err := writeExclusiveFile(filepath.Join(self.directory, "worker-request.json"), requestContent, 0400); err != nil {
			self.t.Fatal(err)
		}
		if err := writeExclusiveFile(filepath.Join(self.directory, "worker.stderr.log"), []byte("synthetic generic evaluator process exit\n"), 0400); err != nil {
			self.t.Fatal(err)
		}
		manifestDigest, err := competitionagent.ManifestDigest(manifest)
		if err != nil {
			self.t.Fatal(err)
		}
		requestDigest := sha256.Sum256(requestContent)
		clear(requestContent)
		identity := manifest.Identity
		self.response = competitionagent.Response{Schema: competitionagent.Schema, HostId: self.host.HostId, RunId: request.RunId, FencingToken: request.FencingToken,
			Identity: &identity, ManifestSha256: manifestDigest, RequestSha256: hex.EncodeToString(requestDigest[:]), State: "failed", ExitCode: 1, ErrorCode: "evaluator_process_failed"}
		for _, path := range []string{"canonical.patch", "providers.yml", "round-baseline.json", "worker-request.json", "worker.stderr.log"} {
			digest, size, err := hashRegularFile(filepath.Join(self.directory, path))
			if err != nil {
				self.t.Fatal(err)
			}
			self.response.Files = append(self.response.Files, competitionagent.File{Path: path, Sha256: digest, Bytes: size})
		}
	case "artifacts":
		if self.manifest == nil || request.Start != nil || inputDirectory != "" || outputDirectory == "" || request.RunId != self.response.RunId || request.FencingToken != self.response.FencingToken {
			self.t.Fatal("synthetic peer received artifacts request outside the exact original run")
		}
		transfer(self.directory, outputDirectory, self.response.Files, limits.ArtifactBytes)
	default:
		self.t.Fatal("synthetic terminal peer unexpectedly redispatched, canceled or waited", request.Operation)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(self.host.SlotId)}, ed25519.SeedSize))
	if err := competitionagent.SignResponse(&self.response, key); err != nil {
		self.t.Fatal(err)
	}
	if err := validateCompetitionDistributedRecoveryEnvelope(self.response, self.host); err != nil {
		self.t.Fatal("synthetic response did not authenticate against frozen host", err)
	}
	return self.response, nil
}

// Verification comes only from the production evaluator's successful exact
// request/patch/receipt retention. The test never sets distributedArchiveVerified.
func competitionOperatorPatchRetryRetainedFailureForTest(t testing.TB, settings *Settings, job *queuedJob) (EvaluationOutcome, *competitionagent.Response) {
	t.Helper()
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok {
		t.Fatal("synthetic original has no byte-backed production archive")
	}
	materializeProviders := func() {
		// This fixture separates retained versions from current-object references.
		// Real MinIO current-object behavior is not inferred from that test seam.
		blobs, ok := archive.store.(*competitionRuntimeBlobTestStore)
		if !ok {
			t.Fatal("synthetic provider materialization requires the exact versioned fixture store")
		}
		providerPath := filepath.Join(settings.ArtifactRoot, "rounds", job.RoundId.String(), "providers.yml")
		if job.Round.ProvidersPath != providerPath {
			t.Fatal("synthetic workload has a different frozen local input path")
		}
		key := archive.roundWorkloadKey(settings, &job.Round, job.Round.ProvidersSha256)
		if job.EvaluationCycle == 0 {
			if _, err := os.Lstat(providerPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("root-cause control requires the original provider file to be absent", err)
			}
			current, err := blobs.Get(t.Context(), key)
			if current != nil {
				current.Close()
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("root-cause control requires the fixture's current workload reference to be absent", err)
			}
		}
		version, expectedBytes := "", int64(0)
		func() {
			blobs.stateLock.Lock()
			defer blobs.stateLock.Unlock()
			for retainedKey, content := range blobs.versions {
				candidate, found := strings.CutPrefix(retainedKey, key+"\x00")
				if found && candidate != "" && (version == "" || candidate < version) {
					version, expectedBytes = candidate, int64(len(content))
				}
			}
		}()
		if version == "" || expectedBytes <= 0 || maxProvidersFileSize < expectedBytes {
			t.Fatal("synthetic workload has no bounded already-retained version")
		}
		reader, err := blobs.GetVersion(t.Context(), key, version)
		if err != nil {
			t.Fatal("synthetic provider version read failed", err)
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, expectedBytes+1))
		closeErr := reader.Close()
		defer clear(content)
		digest := sha256.Sum256(content)
		if readErr != nil || closeErr != nil || int64(len(content)) != expectedBytes || hex.EncodeToString(digest[:]) != job.Round.ProvidersSha256 {
			t.Fatal("already-retained synthetic provider version differs from frozen hash or size", readErr, closeErr)
		}
		if _, err := os.Lstat(providerPath); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(providerPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeExclusiveFile(providerPath, content, 0400); err != nil {
				t.Fatal("synthetic provider materialization failed", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		materialized, err := readRoundWorkload(t.Context(), settings, &job.Round)
		defer clear(materialized)
		if err != nil || !bytes.Equal(materialized, content) {
			t.Fatal("canonical synthetic provider input did not authenticate exact retained bytes", err)
		}
	}
	materializeProviders()
	var host CompetitionExecutorHostPolicy
	for _, candidate := range settings.DistributedEvaluationPolicy.Hosts {
		if candidate.HostId == job.ExecutorHostId && candidate.SlotId == job.SlotId {
			host = candidate
		}
	}
	if host.HostId == "" {
		t.Fatal("synthetic original claim has no frozen host")
	}
	copySettings := *settings
	copySettings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{
		Settings: defaultCompetitionDistributedCoordinatorSettings(), Hosts: []competitionExecutorConnection{{HostId: host.HostId}}}}
	transport := &competitionOperatorPatchRetryFailureTransport{t: t, host: host, directory: t.TempDir()}
	evaluator := &competitionAgentEvaluator{host: host, transport: transport}
	outcome := evaluator.Evaluate(t.Context(), &copySettings, job)
	if outcome.Error == nil || outcome.Error.Kind != "infrastructure" || outcome.Error.Code != "executor_process_failed" || outcome.Error.Retriable ||
		outcome.Score != nil || !outcome.distributedArchiveVerified || len(outcome.ArtifactManifest) == 0 || !reflect.DeepEqual(transport.operations, []string{"start", "artifacts"}) {
		outcome.closeReceiveCustody()
		t.Fatal("synthetic original did not traverse authenticated retained handback", outcome.Error, transport.operations)
	}
	t.Cleanup(outcome.closeReceiveCustody)
	var manifest artifactManifest
	if err := decodeFailureEvidenceJson(outcome.ArtifactManifest, &manifest); err != nil || manifest.Retention == nil || !manifest.Retention.AuthenticatedAfterUpload ||
		!manifest.Retention.ComplianceObjectLock || manifest.Retention.ObjectCount != 3 || manifest.Executor == nil ||
		manifest.Executor.RunId != transport.response.RunId || manifest.RequestSha256 != transport.response.RequestSha256 {
		t.Fatal("synthetic handback omitted complete exact retained failure proof", err)
	}
	expected := map[string]bool{"canonical.patch": true, "worker.stderr.log": true, "executor-receipt.json": true}
	for _, object := range manifest.Retention.Objects {
		if !expected[object.Path] || object.VersionId == "" || object.Mode != "COMPLIANCE" {
			t.Fatal("synthetic retained failure contains wrong or incomplete object", object.Path)
		}
		delete(expected, object.Path)
		reader, err := archive.store.GetVersion(t.Context(), object.Key, object.VersionId)
		if err != nil {
			t.Fatal(err)
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, object.Bytes+1))
		closeErr := reader.Close()
		digest := sha256.Sum256(content)
		if readErr != nil || closeErr != nil || int64(len(content)) != object.Bytes || hex.EncodeToString(digest[:]) != object.Sha256 {
			t.Fatal("retained original object failed independent exact-version readback", object.Path, readErr, closeErr)
		}
	}
	if len(expected) != 0 {
		t.Fatal("retained original omitted an independently authenticated object")
	}
	response := transport.response
	return outcome, &response
}
