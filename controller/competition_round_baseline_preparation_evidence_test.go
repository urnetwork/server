// Synthetic native nine-run chains exercise the real evidence reader and
// retained archive without Docker, providers, requests or live measurements.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/urnetwork/server"
)

// All sample bytes are deliberately synthetic; no native scoring is invoked.
type roundBaselinePreparationEvidenceFixture struct {
	settings    *Settings
	preparation *roundBaselinePreparation
	directory   string
	artifacts   []evaluationArtifact
	receipt     roundBaselinePreparationCompletion
}

// Writes a fixture leaf as the same sealed single-link regular file required
// by production. Rewrites are deliberate test mutations, not producer retries.
func roundBaselinePreparationTestFile(t *testing.T, root, relative string, content []byte) evaluationArtifact {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, content, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return evaluationArtifact{Path: relative, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}
}

// JSON helpers marshal owned data, never copy private provider/runtime files.
func roundBaselinePreparationTestJson(t *testing.T, root, relative string, value any) evaluationArtifact {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return roundBaselinePreparationTestFile(t, root, relative, content)
}

// Rebinds only the declared synthetic evidence after an intentional mutation.
func (self *roundBaselinePreparationEvidenceFixture) publish(t *testing.T) {
	t.Helper()
	manifest := competitionQualificationEvidence{Schema: 1, Kind: "sim-latency-evidence-manifest", JobId: self.preparation.JobId.String(), RoundId: self.preparation.RoundId.String(), Artifacts: self.artifacts}
	artifact := roundBaselinePreparationTestJson(t, self.directory, "evidence-manifest.json", manifest)
	self.receipt.EvidenceManifestSha256 = artifact.Sha256
	roundBaselinePreparationTestJson(t, self.directory, "round-baseline-preparation.complete.json", self.receipt)
}

// Native marker hashes bind run bytes, which bind CSV/accounting bytes; the
// manifest also binds every finalized sample segment and resource sidecar.
func newRoundBaselinePreparationEvidenceFixture(t *testing.T) *roundBaselinePreparationEvidenceFixture {
	t.Helper()
	settings, preparation, _, _ := newRoundBaselinePreparationTest(t)
	directory := failureEvidenceTestTempDir(t)
	prepared := preparedRoundBaselineTest(t, settings, preparation)
	roundBaselinePreparationTestFile(t, directory, "baseline.json", prepared.BaselineJson)
	no, yes := false, true
	fixture := &roundBaselinePreparationEvidenceFixture{settings: settings, preparation: preparation, directory: directory,
		receipt: roundBaselinePreparationCompletion{Schema: 1, Kind: "sim-latency-round-baseline-preparation-complete", Owner: preparation.Owner, ContextSha256: preparation.ContextSha256, JobId: preparation.JobId.String(), RoundId: preparation.RoundId.String(),
			SourceEpoch: evaluationSourceEpoch(&preparation.SourceJob.Round), BaseSha: settings.BaseSha, BaseImageId: settings.EvaluatorImageDigest, ProvidersSha256: preparation.SourceJob.Round.ProvidersSha256, ScorerVersion: ScorerVersion, Replicates: 9, BaselineSha256: prepared.BaselineSha256, SubmissionStarted: &no, CandidateExecuted: &no, CleanupComplete: &yes}}
	for index := 1; index <= 9; index++ {
		id := fmt.Sprintf("baseline-%02d-%s", index, preparation.JobId.String()[:8])
		fixture.receipt.ControlEvaluationIds = append(fixture.receipt.ControlEvaluationIds, id)
		root := fmt.Sprintf("evidence/runs/baseline-%02d/output/%s/", index, id)
		csv := roundBaselinePreparationTestFile(t, directory, root+"results.csv", []byte("synthetic,request\n1,200\n"))
		accountingSource := roundBaselinePreparationTestJson(t, directory, root+"accounting.source.json", map[string]any{"schema": 1, "kind": "sim-latency-provider-accounting-source", "evaluation_id": id, "complete": true, "provider_egress_bytes": 100, "measure_start_ms": 1, "measure_end_ms": 2})
		run := roundBaselinePreparationTestJson(t, directory, root+"run.json", map[string]any{"schema": 2, "kind": "sim-latency-run", "evaluation_id": id, "completion_state": "complete", "official": true, "build_modified": false, "build_revision": settings.BaseSha, "config_sha256": preparation.SourceJob.Round.ProvidersSha256,
			"source_epoch": evaluationSourceEpoch(&preparation.SourceJob.Round), "score_schema": ScoreSchema, "scorer_version": ScorerVersion, "request_timeout_ms": settings.EvaluationPolicy.RequestTimeoutMs, "flags": map[string]string{"official": "true"}, "results_csv_sha256": csv.Sha256, "results_csv_bytes": csv.Bytes, "accounting_source_sha256": accountingSource.Sha256, "accounting_source_bytes": accountingSource.Bytes, "stats_instance_id": "synthetic-instance", "stats_root": "/artifacts/" + id + "/site/stats/local/synthetic-instance"})
		marker := roundBaselinePreparationTestJson(t, directory, root+"run.complete.json", map[string]any{"schema": 1, "kind": "sim-latency-complete", "evaluation_id": id, "score_schema": ScoreSchema, "scorer_version": ScorerVersion, "run_manifest_sha256": run.Sha256, "run_manifest_bytes": run.Bytes})
		resource := roundBaselinePreparationTestJson(t, directory, root+"resources.json", map[string]any{"schema": 1, "kind": "sim-latency-resource-report", "evaluation_id": id, "complete": true, "exit_code": 0, "oom_killed": false, "hard_killed": false, "limit_escape": false, "measurement_missing": false})
		accounting := roundBaselinePreparationTestJson(t, directory, root+"accounting.json", map[string]any{"schema": 1, "kind": "sim-latency-accounting", "evaluation_id": id, "complete": true, "provider_egress_bytes": 100})
		stderr := roundBaselinePreparationTestFile(t, directory, root+"stderr.log", []byte("synthetic completed runner\n"))
		segment := roundBaselinePreparationTestFile(t, directory, root+"site/stats/local/synthetic-instance/findproviders2/1-1.pb.zst", []byte("synthetic finalized sample bytes"))
		fixture.artifacts = append(fixture.artifacts, csv, accountingSource, run, marker, resource, accounting, stderr, segment)
	}
	fixture.publish(t)
	return fixture
}

// All nine actual tuple chains must survive private retained upload/readback;
// this does not assert a scored bridge or a successful candidate submission.
func TestCompetitionRoundBaselinePreparationAuthenticatesNineAndPrivateRetention(t *testing.T) {
	fixture := newRoundBaselinePreparationEvidenceFixture(t)
	baseline, document, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation)
	if err != nil {
		t.Fatal(err)
	}
	store := &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}}
	archive := &blobArtifactArchive{store: store}
	fixture.settings.ArtifactBucket = store.Bucket()
	manifest, err := archive.ArchiveRoundBaselinePreparation(context.Background(), fixture.settings, fixture.preparation, fixture.directory, document)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	prepared := &preparedRoundBaseline{BaselineJson: baseline, BaselineSha256: fixture.receipt.BaselineSha256, ArtifactManifest: manifest, ArtifactManifestSha256: hex.EncodeToString(digest[:])}
	if err := validatePreparedRoundBaseline(prepared, fixture.preparation, fixture.settings); err != nil {
		t.Fatal(err)
	}
	if store.retainedWrites != len(document.Artifacts)+1 || store.versionReads != store.retainedWrites {
		t.Fatal("retained raw objects and manifest did not use exact immutable-version readback", store.retainedWrites, store.versionReads)
	}
	if bytes.Contains(manifest, []byte("worker-request")) || bytes.Contains(manifest, []byte("attempt-01")) || bytes.Contains(manifest, []byte("canonical.patch")) {
		t.Fatal("baseline retention borrowed submission authority or hidden input")
	}
}

// A filename/aggregate summary cannot stand in for a real native finalized run.
func TestCompetitionRoundBaselinePreparationRejectsTamperedNativeTuple(t *testing.T) {
	for _, name := range []string{"csv", "marker", "accounting", "missing_segment", "partial_segment", "candidate_authority", "duplicate_manifest", "unsafe_path", "oversize"} {
		fixture := newRoundBaselinePreparationEvidenceFixture(t)
		root := "evidence/runs/baseline-01/output/" + fixture.receipt.ControlEvaluationIds[0] + "/"
		switch name {
		case "csv":
			roundBaselinePreparationTestFile(t, fixture.directory, root+"results.csv", []byte("changed after native finalization"))
		case "marker":
			artifact := roundBaselinePreparationTestJson(t, fixture.directory, root+"run.complete.json", map[string]any{"schema": 1, "kind": "sim-latency-complete", "evaluation_id": fixture.receipt.ControlEvaluationIds[0], "run_manifest_sha256": strings.Repeat("0", 64), "run_manifest_bytes": 1})
			for i := range fixture.artifacts {
				if fixture.artifacts[i].Path == artifact.Path {
					fixture.artifacts[i] = artifact
				}
			}
			fixture.publish(t)
		case "accounting":
			artifact := roundBaselinePreparationTestJson(t, fixture.directory, root+"accounting.json", map[string]any{"complete": true, "evaluation_id": fixture.receipt.ControlEvaluationIds[0], "provider_egress_bytes": 101})
			for i := range fixture.artifacts {
				if fixture.artifacts[i].Path == artifact.Path {
					fixture.artifacts[i] = artifact
				}
			}
			fixture.publish(t)
		case "missing_segment":
			if err := os.Remove(filepath.Join(fixture.directory, root+"site/stats/local/synthetic-instance/findproviders2/1-1.pb.zst")); err != nil {
				t.Fatal(err)
			}
		case "partial_segment":
			roundBaselinePreparationTestFile(t, fixture.directory, root+"site/stats/local/synthetic-instance/findproviders2/2-2.pb.zst.partial", []byte("unfinished"))
		case "candidate_authority":
			roundBaselinePreparationTestFile(t, fixture.directory, "worker-result.json", []byte(`{"schema":1}`))
		case "duplicate_manifest":
			fixture.artifacts = append(fixture.artifacts, fixture.artifacts[0])
			fixture.publish(t)
		case "unsafe_path":
			fixture.artifacts = append(fixture.artifacts, evaluationArtifact{Path: "evidence/../../worker-request.json", Sha256: strings.Repeat("a", 64), Bytes: 1})
			fixture.publish(t)
		case "oversize":
			fixture.settings.roundBaselinePreparation.Settings.MaximumRawFileBytes = 1
		}
		if _, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation); err == nil {
			t.Errorf("%s native tamper accepted", name)
		}
	}
}

// No non-regular leaf or mutable parent can escape a preparation's bounded
// read, even when its manifest advertises an innocent digest and byte count.
func TestCompetitionRoundBaselinePreparationRejectsSymlinkFifoAndHardlink(t *testing.T) {
	for _, kind := range []string{"leaf_symlink", "parent_symlink", "fifo", "hardlink"} {
		fixture := newRoundBaselinePreparationEvidenceFixture(t)
		artifact := fixture.artifacts[0]
		path := filepath.Join(fixture.directory, artifact.Path)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(failureEvidenceTestTempDir(t), "outside")
		if err := os.WriteFile(outside, content, 0o400); err != nil {
			t.Fatal(err)
		}
		if kind == "parent_symlink" {
			parent := filepath.Dir(path)
			moved := parent + ".moved"
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, parent); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "leaf_symlink":
				err = os.Symlink(outside, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0o400)
			case "hardlink":
				err = os.Link(outside, path)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation); err == nil {
			t.Errorf("%s escaped no-follow single-link evidence reader", kind)
		}
	}
}

// Failure retention keeps the observed raw corpus in a hold document; it
// cannot be submitted as the prepared baseline or award a candidate outcome.
func TestCompetitionRoundBaselinePreparationFailureRetainsRawWithoutAttemptAuthority(t *testing.T) {
	fixture := newRoundBaselinePreparationEvidenceFixture(t)
	store := &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}}
	archive := &blobArtifactArchive{store: store}
	fixture.settings.ArtifactBucket = store.Bucket()
	manifest, err := retainFailedRoundBaselinePreparation(context.Background(), fixture.settings, fixture.preparation, fixture.directory, archive)
	if err != nil {
		t.Fatal(err)
	}
	var document roundBaselinePreparationArchiveDocument
	if err := json.Unmarshal(manifest, &document); err != nil {
		t.Fatal(err)
	}
	if document.Outcome != "infrastructure_hold" || document.Retention == nil || !strings.HasSuffix(document.Retention.ManifestKey, "/baseline-preparation-hold-manifest.json") || len(document.ControlEvaluationIds) != 0 || document.BaselineSha256 != "" {
		t.Fatal("failed preparation gained baseline/candidate publication authority")
	}
	if store.retainedWrites != len(document.Artifacts)+1 || store.versionReads != store.retainedWrites {
		t.Fatal("held native evidence did not use exact immutable-version readback", store.retainedWrites, store.versionReads)
	}
	seen := map[string]bool{}
	for _, artifact := range document.Artifacts {
		seen[artifact.Path] = true
	}
	for _, artifact := range fixture.artifacts {
		if !seen[artifact.Path] {
			t.Fatalf("native raw failure evidence lost: %s", artifact.Path)
		}
	}
	digest := sha256.Sum256(manifest)
	prepared := &preparedRoundBaseline{BaselineJson: mustReadRoundBaselinePreparationTest(t, filepath.Join(fixture.directory, "baseline.json")), BaselineSha256: fixture.receipt.BaselineSha256, ArtifactManifest: manifest, ArtifactManifestSha256: hex.EncodeToString(digest[:])}
	if err := validatePreparedRoundBaseline(prepared, fixture.preparation, fixture.settings); err == nil {
		t.Fatal("infrastructure-hold evidence published a successful baseline")
	}
}

// Reads fixture bytes only, not application/runtime configuration.
func mustReadRoundBaselinePreparationTest(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Log spam cannot fill the host filesystem; the writer drains the process
// and its truthful observed/retained counters distinguish a truncated prefix.
func TestCompetitionRoundBaselinePreparationBoundsDiagnosticLog(t *testing.T) {
	var output bytes.Buffer
	writer := &roundBaselinePreparationLogWriter{Writer: &output, Limit: 4}
	for _, content := range []string{"abc", "defgh", strings.Repeat("x", 1024)} {
		if count, err := writer.Write([]byte(content)); err != nil || count != len(content) {
			t.Fatal("bounded diagnostic did not drain", count, err)
		}
	}
	if output.String() != "abcd" || writer.Retained != 4 || writer.Observed != 1032 {
		t.Fatal("diagnostic prefix or explicit truncation counts differ", output.String(), writer.Retained, writer.Observed)
	}
}

// Private preparation directories cannot be reused or redirected through a
// submitted/sibling symlink, and the escape target remains untouched.
func TestCompetitionRoundBaselinePreparationDirectoryRefusesAliasesAndReuse(t *testing.T) {
	_, preparation, _, _ := newRoundBaselinePreparationTest(t)
	root := failureEvidenceTestTempDir(t)
	first, err := createRoundBaselinePreparationDirectory(root, preparation.RoundId.String(), preparation.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if first != filepath.Join(root, "baseline-preparations", preparation.RoundId.String(), preparation.Owner) {
		t.Fatal("owned namespace changed", first)
	}
	if _, err := createRoundBaselinePreparationDirectory(root, preparation.RoundId.String(), preparation.Owner); err == nil {
		t.Fatal("existing owner was reused")
	}
	outside := failureEvidenceTestTempDir(t)
	aliasedRoot := failureEvidenceTestTempDir(t)
	if err := os.Symlink(outside, filepath.Join(aliasedRoot, "baseline-preparations")); err != nil {
		t.Fatal(err)
	}
	if _, err := createRoundBaselinePreparationDirectory(aliasedRoot, preparation.RoundId.String(), preparation.Owner); err == nil {
		t.Fatal("parent alias redirected baseline archive")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("alias target was mutated before refusal", entries, err)
	}
}

// The development backend reports LOCAL, not production COMPLIANCE. Reproduce
// the original smoke mismatch as a negative control rather than weakening the
// publication gate; corrupted immutable-version readback also refuses upload.
func TestCompetitionRoundBaselinePreparationRejectsLocalAndCorruptRetention(t *testing.T) {
	for _, mode := range []string{"local", "corrupt_version"} {
		fixture := newRoundBaselinePreparationEvidenceFixture(t)
		baseline, document, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation)
		if err != nil {
			t.Fatal(err)
		}
		var store server.RetainedBlobStore
		if mode == "local" {
			store = server.NewLocalBlobStore(t.TempDir(), "private").(server.RetainedBlobStore)
		} else {
			store = &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}, corruptReadback: true}
		}
		fixture.settings.ArtifactBucket = store.Bucket()
		archive := &blobArtifactArchive{store: store}
		manifest, err := archive.ArchiveRoundBaselinePreparation(context.Background(), fixture.settings, fixture.preparation, fixture.directory, document)
		if mode == "corrupt_version" {
			if err == nil || !strings.Contains(err.Error(), "failed authentication") {
				t.Fatal("corrupted retained version authorized baseline persistence", err)
			}
			continue
		}
		if err != nil {
			t.Fatal("LOCAL fixture did not reach its strict publication negative control", err)
		}
		digest := sha256.Sum256(manifest)
		prepared := &preparedRoundBaseline{BaselineJson: baseline, BaselineSha256: fixture.receipt.BaselineSha256, ArtifactManifest: manifest, ArtifactManifestSha256: hex.EncodeToString(digest[:])}
		if err := validatePreparedRoundBaseline(prepared, fixture.preparation, fixture.settings); err == nil || !strings.Contains(err.Error(), "exact owned retained provenance") {
			t.Fatal("development LOCAL retention qualified as production WORM", err)
		}
	}
}
