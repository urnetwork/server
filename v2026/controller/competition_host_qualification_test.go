// Private host-readiness fixtures exercise proof binding without running an
// evaluator, accessing a real host, or manufacturing deployment readiness.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The probe must not rewrite the official workload, inferential replication,
// source identity, local mounts, wire budget, or staging margin in its owner.
func TestCompetitionHostQualificationProfileIsSeparateFromOfficialPolicy(t *testing.T) {
	settings := validSettings()
	settings.EvaluationPolicy.Replicates = 9
	settings.EvaluationPolicy.TakeoverMargin = 0
	before := settings.EvaluationPolicy
	probe := competitionHostQualificationSettings(settings)
	if !reflect.DeepEqual(settings.EvaluationPolicy, before) || probe == settings {
		t.Fatal("host-readiness profile mutated its official owner")
	}
	if probe.EvaluationPolicy.Replicates != 1 || probe.EvaluationPolicy.ProviderCount != 32 || probe.EvaluationPolicy.DurationMs != 30000 || probe.EvaluationPolicy.PrewarmMs != 5000 || probe.EvaluationPolicy.ImpairmentEnabled {
		t.Fatalf("unexpected bounded probe profile: %#v", probe.EvaluationPolicy)
	}
	if probe.EvaluationPolicy.TakeoverMargin != before.TakeoverMargin || probe.EvaluationPolicy.ScoreTimeoutSeconds != before.ScoreTimeoutSeconds ||
		probe.EvaluationPolicy.HostQualificationSha256 != before.HostQualificationSha256 || probe.EvaluationPolicy.ConfigLocalSha256 != before.ConfigLocalSha256 ||
		probe.EvaluationPolicy.VaultLocalSha256 != before.VaultLocalSha256 || probe.EvaluatorImageDigest != settings.EvaluatorImageDigest ||
		probe.ConfigLocalDirectory != settings.ConfigLocalDirectory || probe.VaultLocalDirectory != settings.VaultLocalDirectory || probe.BaseSha != settings.BaseSha {
		t.Fatal("probe changed frozen identity, wire budget, or direct local leaves")
	}
	settings.EvaluationPolicy.TakeoverMargin = .12
	if competitionHostQualificationSettings(settings).EvaluationPolicy.TakeoverMargin != .12 {
		t.Fatal("production margin was weakened by the probe")
	}
}

// Empty patches do not pass the real build path; a bounded trusted comment
// does, without deriving any executable addition from candidate input.
func TestCompetitionHostQualificationUsesOnlyTrustedCommentPatch(t *testing.T) {
	policy := validSettings().PatchPolicy
	source := []byte("package synthetic\n\nfunc Example() int { return 1 }\n")
	patch, err := competitionQualificationPatch("connect/example.go", source, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patch.Bytes, []byte("+"+competitionHostQualificationComment+"\n")) || !bytes.Contains(patch.Bytes, []byte("@@ -1,3 +1,5 @@")) {
		t.Fatalf("unexpected comment-only patch: %s", patch.Bytes)
	}
	for _, line := range strings.Split(string(patch.Bytes), "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") && line != "+" && line != "+"+competitionHostQualificationComment {
			t.Fatalf("qualification added executable content: %q", line)
		}
	}
	for _, sample := range []struct {
		path string
		data []byte
	}{
		{path: "connect/outside-policy.go", data: source},
		{path: "../example.go", data: source},
		{path: "connect/example.go", data: nil},
		{path: "connect/example.go", data: []byte("package synthetic")},
		{path: "connect/example.go", data: []byte("package synthetic\x00\n")},
		{path: "connect/example.go", data: []byte("package synthetic\r\n")},
	} {
		if _, err := competitionQualificationPatch(sample.path, sample.data, policy); err == nil {
			t.Fatalf("unsafe source accepted for %q", sample.path)
		}
	}
}

// Durable API evidence, not the request or root configuration, supplies the
// observed API identity for this exact unactivated transition.
func TestCompetitionHostQualificationRequiresExactApiReceipt(t *testing.T) {
	_, fixture, _, _ := newCompetitionRuntimeBlobFixture(t)
	intent := fixture.intent
	receipt := competitionPreparationTestReceipt(intent, "api", "synthetic-api", testApiImageDigest())
	if err := competitionQualificationApiReceipt(intent, testApiImageDigest(), []model.CompetitionSourceReadiness{receipt}); err != nil {
		t.Fatal(err)
	}
	// Fixed placeholder digests can equal the fixture's current value. Change
	// one hex digit instead so every negative case proves a real mismatch.
	differentDigest := func(digest string) string {
		if strings.HasPrefix(digest, "0") {
			return "1" + digest[1:]
		}
		return "0" + digest[1:]
	}
	for _, sample := range []struct {
		name   string
		mutate func(*model.CompetitionSourceReadiness)
	}{
		{name: "ready", mutate: func(value *model.CompetitionSourceReadiness) { value.Ready = false }},
		{name: "actor", mutate: func(value *model.CompetitionSourceReadiness) { value.ActorId = "" }},
		{name: "mode", mutate: func(value *model.CompetitionSourceReadiness) { value.Staging = !value.Staging }},
		{name: "generation", mutate: func(value *model.CompetitionSourceReadiness) { value.Generation++ }},
		{name: "round", mutate: func(value *model.CompetitionSourceReadiness) { value.TargetRoundId = server.NewId() }},
		{name: "manifest commit", mutate: func(value *model.CompetitionSourceReadiness) {
			value.ManifestCommit = differentDigest(value.ManifestCommit)
		}},
		{name: "manifest digest", mutate: func(value *model.CompetitionSourceReadiness) {
			value.ManifestSha256 = differentDigest(value.ManifestSha256)
		}},
		{name: "source lock", mutate: func(value *model.CompetitionSourceReadiness) {
			value.SourceLockSha256 = differentDigest(value.SourceLockSha256)
		}},
		{name: "qualification", mutate: func(value *model.CompetitionSourceReadiness) {
			value.QualificationSha256 = differentDigest(value.QualificationSha256)
		}},
		{name: "API image", mutate: func(value *model.CompetitionSourceReadiness) { value.RuntimeImageDigest = testWorkerImageDigest() }},
		{name: "season", mutate: func(value *model.CompetitionSourceReadiness) { value.SeasonId = "synthetic-other-season" }},
	} {
		changed := receipt
		sample.mutate(&changed)
		if reflect.DeepEqual(changed, receipt) {
			t.Fatalf("%s mutation left the synthetic receipt unchanged", sample.name)
		}
		if err := competitionQualificationApiReceipt(intent, testApiImageDigest(), []model.CompetitionSourceReadiness{changed}); err == nil {
			t.Fatalf("mismatched %s API receipt accepted", sample.name)
		}
	}
	for _, receipts := range [][]model.CompetitionSourceReadiness{nil, {receipt, receipt}, {competitionPreparationTestReceipt(intent, "worker", "synthetic-worker", testWorkerImageDigest())}} {
		if err := competitionQualificationApiReceipt(intent, testApiImageDigest(), receipts); err == nil {
			t.Fatal("missing or ambiguous API receipt accepted")
		}
	}
}

// One synthetic evidence tree exposes every authenticated layer independently.
type competitionQualificationProofFixture struct {
	directory  string
	settings   *Settings
	request    evaluatorRequest
	result     evaluatorResult
	completion competitionQualificationCompletion
	evidence   competitionQualificationEvidence
}

// Retains exact bytes and returns the same hash/size shape as the real host.
func (self *competitionQualificationProofFixture) write(t testing.TB, path string, content []byte) evaluationArtifact {
	t.Helper()
	full := filepath.Join(self.directory, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return evaluationArtifact{Path: path, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}
}

// Deliberately re-signs a synthetic outer layer so inner semantic corruption
// cannot be rejected merely by a stale parent hash in these tests.
func (self *competitionQualificationProofFixture) publish(t testing.TB) {
	t.Helper()
	marshal := func(value any) []byte {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	self.write(t, "evidence-manifest.json", marshal(self.evidence))
	self.completion.Artifacts = map[string]string{}
	for kind, path := range map[string]string{"accounting": "accounting.json", "baseline": "baseline.json", "resources": "resources.json", "score": "score.json", "evidence_manifest": "evidence-manifest.json"} {
		digest, _, err := hashRegularFile(filepath.Join(self.directory, path))
		if err != nil {
			t.Fatal(err)
		}
		self.completion.Artifacts[kind] = digest
	}
	self.write(t, "evaluation.complete.json", marshal(self.completion))
	self.result.Artifacts = nil
	for _, path := range []string{"accounting.json", "baseline.json", "resources.json", "score.json", "evaluation.complete.json", "evidence-manifest.json"} {
		digest, size, err := hashRegularFile(filepath.Join(self.directory, path))
		if err != nil {
			t.Fatal(err)
		}
		self.result.Artifacts = append(self.result.Artifacts, evaluationArtifact{Path: path, Sha256: digest, Bytes: size})
	}
	self.write(t, "worker-result.json", marshal(self.result))
}

// This fixture contains visibly synthetic data only; it is not a host proof
// and is never passed to the root-only promotion path or a live archive.
func newCompetitionQualificationProofFixture(t testing.TB) *competitionQualificationProofFixture {
	t.Helper()
	settings := competitionHostQualificationSettings(validSettings())
	settings.EvaluationPolicy.TakeoverMargin = 0
	roundId, jobId := server.NewId(), server.NewId()
	fixture := &competitionQualificationProofFixture{directory: t.TempDir(), settings: settings,
		request: evaluatorRequest{Schema: 1, Staging: true, JobId: jobId.String(), RoundId: roundId.String(), Attempt: 1,
			EvaluatorImageDigest: settings.EvaluatorImageDigest, PatchSha256: strings.Repeat("1", 64), ProvidersSha256: strings.Repeat("2", 64), EvaluationPolicy: settings.EvaluationPolicy}}
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	baseline := testRoundBaseline(t, settings, &roundRecord{RoundResult: RoundResult{RoundId: roundId, ProvidersSha256: fixture.request.ProvidersSha256}, PolicyJson: policy}, []float64{100})
	baselineArtifact := fixture.write(t, "baseline.json", baseline)
	raw, normalized := 100.0, 100.0
	fixture.result = evaluatorResult{Schema: 1, JobId: jobId.String(), Score: &ScoreResult{
		ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized, Placeable: true,
		Significance: &ScoreSignificance{Method: "one-sided-welch-t", Alpha: .05, ReplicateCount: 1, BaselineMeanRawScore: 100, CandidateMeanRawScore: 100},
		Gates: map[string]Gate{
			"G1_success": {Passed: true, Details: map[string]any{}}, "G2_volume": {Passed: true, Details: map[string]any{}},
			"G3_path_integrity": {Passed: true, Details: map[string]any{}}, "G4_matchmaking": {Passed: true, Details: map[string]any{}},
			"G5_stability": {Passed: true, Details: map[string]any{}}, "G6_resources": {Passed: true, Details: map[string]any{}},
		},
		Diagnostics: map[string]any{"baseline_sha256": baselineArtifact.Sha256, "baseline_takeover_eligible": false}},
		Security: evaluationSecurity{TemplateDatabaseReset: true, RedisReset: true, CgroupContained: true, ResourceLimits: true, ManagementCpuReserved: true,
			ManagementMemoryReserved: true, DefaultDenyNetwork: true, OfflineBuild: true, OfflineBuildResourceLimits: true, NoProductionSecrets: true, StructuralPatchCheck: true,
			AccountingComplete: true, ResourceReportComplete: true, CleanupComplete: true, ImmutableReports: true, CgroupId: "synthetic-cgroup", TemplateDatabaseId: "synthetic-template", RedisGenerationId: "synthetic-redis"}}
	scoreBytes, err := json.Marshal(fixture.result.Score)
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(t, "score.json", scoreBytes)
	fixture.write(t, "accounting.json", []byte("{\"synthetic\":true}\n"))
	fixture.write(t, "resources.json", []byte("{\"synthetic\":true}\n"))
	fixture.evidence = competitionQualificationEvidence{Schema: 1, Kind: "sim-latency-evidence-manifest", JobId: jobId.String(), RoundId: roundId.String(), Artifacts: []evaluationArtifact{
		fixture.write(t, "evidence/local-mounts.json", []byte("{\"synthetic\":true}\n")),
		fixture.write(t, "evidence/docker-id-map.json", []byte("{\"synthetic\":true}\n")),
	}}
	fixture.completion = competitionQualificationCompletion{Schema: 1, Kind: "sim-latency-worker-evaluation-complete", JobId: jobId.String(), RoundId: roundId.String(), Attempt: 1,
		BaseImageId: settings.EvaluatorImageDigest, CandidateImageId: "sha256:" + strings.Repeat("c", 64), PatchSha256: fixture.request.PatchSha256, ProvidersSha256: fixture.request.ProvidersSha256, CleanupComplete: true}
	fixture.publish(t)
	return fixture
}

// The one-replicate host control is accepted only in this host-specific
// authenticator; no persistence or official baseline record is created.
func TestCompetitionHostQualificationAuthenticatesExactProbeEvidence(t *testing.T) {
	fixture := newCompetitionQualificationProofFixture(t)
	if err := authenticateCompetitionQualification(fixture.directory, fixture.request, fixture.settings); err != nil {
		t.Fatal(err)
	}
}

// The real scorer uses six mixed-case gate identifiers and can include
// mixed-case diagnostic labels. Only those data maps receive the exception.
func TestCompetitionHostQualificationAuthenticatesMixedCaseScorerData(t *testing.T) {
	fixture := newCompetitionQualificationProofFixture(t)
	fixture.result.Score.Gates["G4_matchmaking"].Details["SyntheticSamples"] = []any{map[string]any{"PoolP05": 4.0}}
	fixture.result.Score.Diagnostics["SyntheticProbe"] = map[string]any{"RequestCount": 32.0}
	scoreBytes, err := json.Marshal(fixture.result.Score)
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(t, "score.json", scoreBytes)
	fixture.publish(t)
	if err := authenticateCompetitionQualification(fixture.directory, fixture.request, fixture.settings); err != nil {
		t.Fatalf("real-shaped scorer maps were rejected: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(fixture.directory, "worker-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var control evaluatorResult
	if err := decodeFailureEvidenceJson(content, &control); err == nil {
		t.Fatal("qualification exception weakened the separate failure/control decoder")
	}
}

// Allowing gate names never permits duplicate data keys, protocol aliases,
// unknown typed fields, appended documents or unbounded diagnostic nesting.
func TestCompetitionHostQualificationRejectsAmbiguousResultFields(t *testing.T) {
	fixture := newCompetitionQualificationProofFixture(t)
	content, err := os.ReadFile(filepath.Join(fixture.directory, "worker-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		name   string
		before string
		after  string
	}{
		{name: "root alias", before: `"schema":`, after: `"Schema":`},
		{name: "score alias", before: `"raw_score":`, after: `"Raw_Score":`},
		{name: "significance alias", before: `"significance":`, after: `"Significance":`},
		{name: "significance field alias", before: `"method":`, after: `"Method":`},
		{name: "gate field alias", before: `"passed":true`, after: `"Passed":true`},
		{name: "security alias", before: `"offline_build":`, after: `"Offline_Build":`},
		{name: "artifact alias", before: `"artifacts":`, after: `"Artifacts":`},
		{name: "duplicate gate name", before: `"G1_success":`, after: `"G1_success":{"passed":true,"details":{}},"G1_success":`},
		{name: "duplicate typed key", before: `"passed":true`, after: `"passed":false,"passed":true`},
		{name: "duplicate diagnostic key", before: `"details":{}`, after: `"details":{"Synthetic":1,"Synthetic":2}`},
		{name: "unknown gate field", before: `"passed":true`, after: `"unknown":true,"passed":true`},
		{name: "excessive diagnostic nesting", before: `"details":{}`, after: `"details":` + strings.Repeat(`{"Synthetic":`, 65) + `null` + strings.Repeat(`}`, 65)},
	} {
		changed := strings.Replace(string(content), sample.before, sample.after, 1)
		if changed == string(content) {
			t.Fatalf("%s mutation did not change the result", sample.name)
		}
		fixture.write(t, "worker-result.json", []byte(changed))
		if err := authenticateCompetitionQualification(fixture.directory, fixture.request, fixture.settings); err == nil {
			t.Fatalf("ambiguous %s result was accepted", sample.name)
		}
	}
	for _, malformed := range [][]byte{append(bytes.Clone(content), []byte(`{}`)...), append(bytes.Clone(content), 0xff)} {
		fixture.write(t, "worker-result.json", malformed)
		if err := authenticateCompetitionQualification(fixture.directory, fixture.request, fixture.settings); err == nil {
			t.Fatal("appended or invalid utf-8 result was accepted")
		}
	}
}

// Fresh outer hashes must not hide wrong-round completions, false cleanup,
// source substitution, score disagreement or missing container evidence.
func TestCompetitionHostQualificationRejectsAdjacentProofSubstitution(t *testing.T) {
	for _, sample := range []struct {
		name   string
		mutate func(*competitionQualificationProofFixture)
	}{
		{name: "round", mutate: func(f *competitionQualificationProofFixture) { f.completion.RoundId = server.NewId().String() }},
		{name: "cleanup", mutate: func(f *competitionQualificationProofFixture) { f.completion.CleanupComplete = false }},
		{name: "image", mutate: func(f *competitionQualificationProofFixture) { f.completion.BaseImageId = testWorkerImageDigest() }},
		{name: "patch", mutate: func(f *competitionQualificationProofFixture) { f.completion.PatchSha256 = strings.Repeat("9", 64) }},
		{name: "workload", mutate: func(f *competitionQualificationProofFixture) { f.completion.ProvidersSha256 = strings.Repeat("9", 64) }},
		{name: "security", mutate: func(f *competitionQualificationProofFixture) { f.result.Security.OfflineBuild = false }},
		{name: "score mismatch", mutate: func(f *competitionQualificationProofFixture) { *f.result.Score.RawScore = 99 }},
		{name: "score gate", mutate: func(f *competitionQualificationProofFixture) {
			f.result.Score.Gates["G1_success"] = Gate{Passed: false, Details: map[string]any{}}
		}},
		{name: "missing gate", mutate: func(f *competitionQualificationProofFixture) { delete(f.result.Score.Gates, "G6_resources") }},
		{name: "substituted gate", mutate: func(f *competitionQualificationProofFixture) {
			f.result.Score.Gates["synthetic-success"] = f.result.Score.Gates["G6_resources"]
			delete(f.result.Score.Gates, "G6_resources")
		}},
		{name: "score margin", mutate: func(f *competitionQualificationProofFixture) { f.result.Score.Significance.TakeoverMarginPercent = 1 }},
		{name: "baseline", mutate: func(f *competitionQualificationProofFixture) {
			f.write(t, "baseline.json", []byte("{\"synthetic_invalid\":true}\n"))
		}},
		{name: "evidence round", mutate: func(f *competitionQualificationProofFixture) { f.evidence.RoundId = server.NewId().String() }},
		{name: "evidence missing", mutate: func(f *competitionQualificationProofFixture) { f.evidence.Artifacts = f.evidence.Artifacts[:1] }},
		{name: "evidence duplicate", mutate: func(f *competitionQualificationProofFixture) {
			f.evidence.Artifacts = append(f.evidence.Artifacts, f.evidence.Artifacts[0])
		}},
		{name: "evidence corrupt", mutate: func(f *competitionQualificationProofFixture) {
			f.write(t, "evidence/local-mounts.json", []byte("changed\n"))
		}},
	} {
		fixture := newCompetitionQualificationProofFixture(t)
		sample.mutate(fixture)
		fixture.publish(t)
		if err := authenticateCompetitionQualification(fixture.directory, fixture.request, fixture.settings); err == nil {
			t.Fatalf("accepted %s substitution", sample.name)
		}
	}
}

// Hashes alone cannot authorize a path through an attacker-controlled parent.
func TestCompetitionHostQualificationRejectsEvidenceParentSymlinks(t *testing.T) {
	directory, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "evidence"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := competitionQualificationArtifactPath(directory, "evidence/report.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "redirect")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"redirect/report.json", "../report.json", "/report.json", "evidence/../report.json"} {
		if err := competitionQualificationArtifactPath(directory, path); err == nil {
			t.Fatalf("unsafe evidence path accepted: %q", path)
		}
	}
	if err := competitionQualificationArtifactPath("relative", "report.json"); err == nil {
		t.Fatal("relative root could escape the parent-walk bound")
	}
}

// Current-boot markers also bind the exact profile, round, intent and retained
// inventory; a success flag alone is intentionally insufficient for caching.
func TestCompetitionHostQualificationCacheBindsBootAndExactScope(t *testing.T) {
	settings, fixture, _, _ := newCompetitionRuntimeBlobFixture(t)
	intent := fixture.intent
	profile, boot := strings.Repeat("7", 64), "synthetic-current-boot"
	marker := competitionHostQualificationMarker{Schema: 1, Kind: competitionHostQualificationProfile, Passed: true, RoundId: intent.TargetRoundId.String(), BootId: boot,
		ProfileSha256: profile, IntentSha256: intent.IntentSha256, Receipt: retainedArtifact{Path: "host-readiness-retention.json", Key: "synthetic-retained-inventory", Sha256: strings.Repeat("8", 64), Bytes: 1,
			VersionId: "synthetic-version", Mode: "COMPLIANCE", RetainUntil: settings.RetainUntil}}
	if !competitionQualificationMarkerMatches(marker, intent, profile, boot, settings.RetainUntil) {
		t.Fatal("exact synthetic marker rejected")
	}
	for _, mutate := range []func(*competitionHostQualificationMarker){
		func(value *competitionHostQualificationMarker) { value.Passed = false },
		func(value *competitionHostQualificationMarker) { value.Kind = "synthetic-other-kind" },
		func(value *competitionHostQualificationMarker) { value.BootId = "synthetic-old-boot" },
		func(value *competitionHostQualificationMarker) { value.ProfileSha256 = strings.Repeat("9", 64) },
		func(value *competitionHostQualificationMarker) { value.IntentSha256 = strings.Repeat("9", 64) },
		func(value *competitionHostQualificationMarker) { value.RoundId = server.NewId().String() },
		func(value *competitionHostQualificationMarker) { value.Receipt.VersionId = "" },
		func(value *competitionHostQualificationMarker) { value.Receipt.Mode = "LOCAL" },
		func(value *competitionHostQualificationMarker) {
			value.Receipt.RetainUntil = settings.RetainUntil.Add(-time.Hour)
		},
	} {
		changed := marker
		mutate(&changed)
		if competitionQualificationMarkerMatches(changed, intent, profile, boot, settings.RetainUntil) {
			t.Fatal("stale or unretained marker accepted")
		}
	}
}

// The probe archive reads every immutable upload back and remains separate
// from official attempt/baseline namespaces. Cache reads also use exact versions.
func TestCompetitionHostQualificationRetainsAndAuthenticatesDistinctInventory(t *testing.T) {
	settings, fixture, archive, blobs := newCompetitionRuntimeBlobFixture(t)
	settings.artifactArchive = archive
	proof := newCompetitionQualificationProofFixture(t)
	receipt, err := retainCompetitionQualification(context.Background(), settings, fixture.intent, proof.directory, strings.Repeat("7", 64))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt.Key, "/competition/v2/host-readiness/") || !competitionQualificationReceiptValid(receipt, settings.RetainUntil) {
		t.Fatalf("inventory is not an independently retained host probe: %#v", receipt)
	}
	if err := readCompetitionQualificationReceipt(context.Background(), settings, *receipt); err != nil {
		t.Fatal(err)
	}
	blobs.corruptReadback = true
	if err := readCompetitionQualificationReceipt(context.Background(), settings, *receipt); err == nil {
		t.Fatal("corrupt cached inventory accepted")
	}
}

// The existing global slot serializes the root probe with ordinary scorers;
// acquiring it must neither invent a public job nor permit a stale release.
func TestCompetitionHostQualificationSlotBlocksOrdinaryClaim(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		owner := "synthetic-host-qualifier"
		if err := reserveCompetitionQualificationSlot(ctx, owner, server.NowUtc().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = releaseCompetitionQualificationSlot(ctx, owner) })
		if err := reserveCompetitionQualificationSlot(ctx, "synthetic-second-qualifier", server.NowUtc().Add(time.Minute)); !errors.Is(err, ErrEvaluationRunning) {
			t.Fatalf("overlapping qualification = %v", err)
		}
		store := PostgresStore{dequeueCompetitionSignal: func(context.Context, *Settings) (*server.Id, error) { return nil, nil }}
		if job, err := store.Claim(ctx, validSettings(), "synthetic-worker", testWorkerImageDigest()); err != nil || job != nil {
			t.Fatalf("ordinary scorer overlapped host probe: %#v, %v", job, err)
		}
		if err := releaseCompetitionQualificationSlot(ctx, "synthetic-stale-owner"); err != nil {
			t.Fatal(err)
		}
		if err := checkCompetitionQualificationSlot(ctx, owner); err != nil {
			t.Fatalf("stale release cleared current slot: %v", err)
		}
		var jobId *server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT job_id FROM competition_worker_slot WHERE slot_id=1`).Scan(&jobId))
		})
		if jobId != nil {
			t.Fatal("host probe created a public scoring job")
		}
		if err := releaseCompetitionQualificationSlot(ctx, owner); err != nil {
			t.Fatal(err)
		}
		if err := checkCompetitionQualificationSlot(ctx, owner); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("released slot still authorizes a probe: %v", err)
		}
	})
}

// Even a stale/missing singleton owner cannot hide an actual running job.
func TestCompetitionHostQualificationRefusesRunningJobWithoutSlot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newCompetitionRankingFixture(t, false)
		fixture.addJob(t)
		if err := reserveCompetitionQualificationSlot(context.Background(), "synthetic-host-qualifier", server.NowUtc().Add(time.Minute)); !errors.Is(err, ErrEvaluationRunning) {
			t.Fatalf("live job was ignored: %v", err)
		}
	})
}
