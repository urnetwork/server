// Synthetic filesystem fixtures exercise the composed inactive recovery path.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Every byte and identity is generated test-only, never copied from production.
type recoveryFixture struct {
	Request           recoveryRequest
	Policy            recoveryPolicy
	Settings          recoverySettings
	Hooks             recoveryHooks
	InitialTreeSha256 string
}

// Test-only writes use private directories and explicit synthetic content.
func writeSyntheticFile(t *testing.T, path string, content []byte) filePin {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return filePin{Path: path, Sha256: digestBytes(content)}
}

// The same serializer is used only for synthetic administrative metadata.
func writeSyntheticJson(t *testing.T, path string, value any) filePin {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeSyntheticFile(t, path, content)
}

// Build the complete six-job shape and explicit owner/proof pins deterministically.
func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	anchor := t.TempDir()
	policy := recoveryPolicy{CampaignRoot: filepath.Join(anchor, "campaign"), SelectionRoot: filepath.Join(anchor, "selection"), Paths: pathPolicy{OwnerUid: uint32(os.Getuid()), AnchorPath: anchor}}
	settings := defaultRecoverySettings()
	config := recoveryConfig{Schema: 1, Kind: "sim-latency-unmeasured-recovery-config", CampaignRoot: policy.CampaignRoot, SelectionRoot: policy.SelectionRoot,
		AuditRoot: filepath.Join(policy.SelectionRoot, "unmeasured-recovery-forensics-000001"), OldDeadline: time.Date(2030, 1, 2, 6, 4, 5, 0, time.UTC), Memory: syntheticMemoryBudget(), RuntimeProcessMarkers: []string{filepath.Join(anchor, "synthetic-runner")}}
	roles := [][]string{{"better", "worse", "no-op"}, {"worse", "no-op", "better"}}
	rounds := []publicRound{}
	for roundIndex := 0; roundIndex < 2; roundIndex++ {
		providers := []byte(fmt.Sprintf("synthetic providers round %d", roundIndex))
		round := publicRound{Position: roundIndex + 1, RoundId: fmt.Sprintf("synthetic-round-%d", roundIndex), ProvidersSha256: digestBytes(providers)}
		for jobIndex := 0; jobIndex < 3; jobIndex++ {
			jobId := fmt.Sprintf("synthetic-job-%d-%d", roundIndex, jobIndex)
			root := filepath.Join(policy.CampaignRoot, fmt.Sprintf("round-%02d", roundIndex+1), fmt.Sprintf("job-%02d-%s", jobIndex+1, roles[roundIndex][jobIndex]))
			inputPins := []filePin{
				writeSyntheticFile(t, filepath.Join(root, "artifacts", "canonical.patch"), []byte("synthetic immutable patch")),
				writeSyntheticFile(t, filepath.Join(root, "artifacts", "precommitted-request.json"), []byte(fmt.Sprintf("opaque synthetic request %d %d", roundIndex, jobIndex))),
				writeSyntheticFile(t, filepath.Join(root, "artifacts", "providers.yml"), providers),
			}
			config.Jobs = append(config.Jobs, jobPins{Root: root, Inputs: inputPins})
			round.Jobs = append(round.Jobs, publicJob{Position: jobIndex + 1, Role: roles[roundIndex][jobIndex], JobId: jobId, RequestSha256: inputPins[1].Sha256})
			if roundIndex == 0 {
				config.FirstJobIds = append(config.FirstJobIds, jobId)
			}
		}
		rounds = append(rounds, round)
	}
	originalPrivate := writeSyntheticFile(t, filepath.Join(policy.CampaignRoot, "private-plan.json"), []byte("opaque synthetic original plan"))
	selectedPrivate := writeSyntheticFile(t, filepath.Join(policy.SelectionRoot, "selected-campaign", "private-plan.json"), []byte("opaque synthetic selected plan"))
	plan := publicPlan{OriginalPrivatePlanSha256: originalPrivate.Sha256, UnstartedSecondRound: rounds[1]}
	plan.Selected.PrivatePlanSha256, plan.Selected.Rounds = selectedPrivate.Sha256, []publicRound{rounds[0]}
	config.PublicPlan = writeSyntheticJson(t, filepath.Join(policy.SelectionRoot, "public-plan.json"), plan)
	policy.PublicPlanSha256 = config.PublicPlan.Sha256
	config.Executor = writeSyntheticFile(t, filepath.Join(anchor, "synthetic-executor"), []byte("synthetic qualified executable bytes"))
	config.ScoringLock = writeSyntheticFile(t, filepath.Join(anchor, "synthetic-scoring.lock"), nil)
	lockInfo, err := os.Lstat(config.ScoringLock.Path)
	if err != nil {
		t.Fatal(err)
	}
	lockStat := lockInfo.Sys().(*syscall.Stat_t)
	config.ScoringLockIdentity = lockIdentity{OwnerUid: uint32(os.Getuid()), Device: uint64(lockStat.Dev), Inode: lockStat.Ino, Mode: 0600}
	policy.ScoringLock = legacyLockPolicy{Path: config.ScoringLock.Path, Identity: config.ScoringLockIdentity, Ancestors: []lockAncestor{{Path: anchor, OwnerUid: uint32(os.Getuid()), Mode: 0700}}}
	config.ProtectedPins = []filePin{originalPrivate, selectedPrivate, config.Executor}
	for _, path := range []string{filepath.Join(policy.CampaignRoot, "configuration.json"), filepath.Join(policy.CampaignRoot, "evaluation-policy-canonical.json"), filepath.Join(policy.CampaignRoot, "preparation.started"),
		filepath.Join(policy.CampaignRoot, "first-committed-selection.json"), filepath.Join(policy.SelectionRoot, "selected-campaign", "configuration.json"), filepath.Join(policy.SelectionRoot, "execution.json")} {
		config.ProtectedPins = append(config.ProtectedPins, writeSyntheticFile(t, path, []byte("opaque synthetic authority "+filepath.Base(path))))
	}
	root := firstJobRoot(policy)
	config.OldGlobalStart = writeSyntheticFile(t, filepath.Join(policy.CampaignRoot, "execution.started.json"), []byte("synthetic historical global start"))
	config.OldFirstStart = writeSyntheticJson(t, filepath.Join(root, "started.json"), jobStart{Schema: 1, JobId: config.FirstJobIds[0], RoundId: rounds[0].RoundId, RequestSha256: config.Jobs[0].Inputs[1].Sha256, Attempt: 1, StartedAt: config.OldDeadline.Add(-3 * time.Hour), Deadline: config.OldDeadline})
	for _, name := range []string{"evaluator.stderr", "evaluator.stdout"} {
		writeSyntheticFile(t, filepath.Join(root, name), nil)
	}
	writeSyntheticFile(t, filepath.Join(root, "finalization.json"), []byte("synthetic first finalization"))
	requestBytes, err := os.ReadFile(config.Jobs[0].Inputs[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	writeSyntheticFile(t, filepath.Join(root, "artifacts", "worker-request.json"), requestBytes)
	writeSyntheticFile(t, filepath.Join(root, "artifacts", "failed-evidence-manifest.json"), []byte("synthetic failed manifest"))
	for _, name := range []string{"local-mounts.json", "evidence-quota.json", "docker-id-map.json", "policy.json", "builder-identity.json", "resource-boundary.json", "failure.json"} {
		writeSyntheticFile(t, filepath.Join(root, "artifacts", "failed-evidence", name), []byte("synthetic setup evidence "+name))
	}
	for _, name := range []string{"artifacts/.evidence-runtime", "artifacts/failed-evidence/runs", "artifacts/failed-evidence/score-output"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, treeSha256, err := failedTreeInventory(root, policy.Paths, settings)
	if err != nil {
		t.Fatal(err)
	}
	config.FailedTreeSha256 = treeSha256
	rawPin := writeSyntheticFile(t, filepath.Join(anchor, "raw-proof", "synthetic-kernel.log"), []byte("synthetic independently inspected zero-measurement evidence"))
	proof := failureProof{Schema: 1, Kind: "sim-latency-unmeasured-setup-failure-proof", CampaignRoot: policy.CampaignRoot, FirstJobRoot: root, PublicPlanSha256: config.PublicPlan.Sha256,
		OldGlobalStartSha256: config.OldGlobalStart.Sha256, OldFirstStartSha256: config.OldFirstStart.Sha256, FailedTreeSha256: treeSha256,
		ZeroMeasurementsProved: true, SetupOomBeforeSimulation: true, NoSimulationProgress: true, NoResultsOrBaseline: true, NoCompletionOrOutcomes: true,
		RunsAndScoreOutputEmpty: true, NoActiveProcesses: true, NoResidualMounts: true, SecondSeedUnstarted: true, RawEvidence: []filePin{rawPin}}
	config.FailureProof = writeSyntheticJson(t, filepath.Join(anchor, "raw-proof", "proof.json"), proof)
	configPin := writeSyntheticJson(t, filepath.Join(policy.SelectionRoot, "recovery-config.json"), config)
	request := recoveryRequest{Config: config, ConfigPin: configPin}
	approval := ownerApproval{Schema: 1, Kind: "sim-latency-unmeasured-recovery-owner-approval", Approved: true, FreshClockException: true, AcknowledgeOldDeadlineException: true,
		ConfigSha256: configPin.Sha256, FailureProofSha256: config.FailureProof.Sha256, PublicPlanSha256: config.PublicPlan.Sha256, OldGlobalStartSha256: config.OldGlobalStart.Sha256,
		OldFirstStartSha256: config.OldFirstStart.Sha256, FailedTreeSha256: treeSha256, OldDeadline: config.OldDeadline, FirstJobIds: config.FirstJobIds, FreshClockLimitSeconds: 10800,
		NoNewEntropy: true, NoInputChanges: true, SecondSeedUnstarted: true, AllFailedArtifactsRetained: true}
	request.ApprovalPin = writeSyntheticJson(t, filepath.Join(policy.SelectionRoot, "owner-approval.json"), approval)
	hooks := recoveryHooks{Now: func() time.Time { return time.Date(2030, 1, 2, 4, 5, 6, 0, time.UTC) }, CheckRuntime: func(recoveryConfig, recoverySettings) error { return nil },
		MemoryLimits: func() ([]cgroupLimit, error) {
			return []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: 8 * 1024 * 1024 * 1024}}, nil
		}, AfterStep: func(string) error { return nil }}
	return recoveryFixture{Request: request, Policy: policy, Settings: settings, Hooks: hooks, InitialTreeSha256: treeSha256}
}

// Default dry-run has no approval requirement and creates no claim or journal.
func TestRecoveryDefaultsToReadOnlyDryRun(t *testing.T) {
	fixture := newRecoveryFixture(t)
	fixture.Request.ApprovalPin = filePin{}
	report, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks)
	if err != nil || report.Mode != "dry-run" || report.FreshClockException {
		t.Fatalf("read-only dry run failed: %v", err)
	}
	_, treeSha256, err := failedTreeInventory(firstJobRoot(fixture.Policy), fixture.Policy.Paths, fixture.Settings)
	if err != nil || treeSha256 != fixture.InitialTreeSha256 {
		t.Fatal("dry run changed failed bytes")
	}
	for _, path := range []string{fixture.Request.Config.AuditRoot, filepath.Join(fixture.Policy.SelectionRoot, "unmeasured-recovery.claim.json")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("dry run wrote runtime state")
		}
	}
}

// Explicit prospective approval permits only archival and three opaque copies.
func TestRecoveryArchivesEveryFailedByteAndRestoresExactInputs(t *testing.T) {
	fixture := newRecoveryFixture(t)
	fixture.Request.Apply = true
	report, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks)
	if err != nil || report.Mode != "applied-administrative-restoration-only" || !report.FreshClockException || !report.FreshClockNotStarted {
		t.Fatalf("authorized synthetic recovery failed: %v", err)
	}
	_, archivedSha256, err := failedTreeInventory(filepath.Join(fixture.Request.Config.AuditRoot, "first-job"), fixture.Policy.Paths, fixture.Settings)
	if err != nil || archivedSha256 != fixture.InitialTreeSha256 {
		t.Fatal("failed artifact tree was lost or changed")
	}
	globalBytes, err := readPinned(filePin{Path: filepath.Join(fixture.Request.Config.AuditRoot, "execution.started.json"), Sha256: fixture.Request.Config.OldGlobalStart.Sha256}, fixture.Policy.Paths, fixture.Settings.MaximumMetadataBytes)
	if err != nil || len(globalBytes) == 0 {
		t.Fatal("old global clock was not retained")
	}
	for _, job := range fixture.Request.Config.Jobs {
		if err := requireDirectoryEntries(job.Root, []string{"artifacts"}, fixture.Policy.Paths); err != nil {
			t.Fatal("job is not in exact initial shape")
		}
		if err := requireDirectoryEntries(filepath.Join(job.Root, "artifacts"), []string{"canonical.patch", "precommitted-request.json", "providers.yml"}, fixture.Policy.Paths); err != nil {
			t.Fatal(err)
		}
		for _, pin := range job.Inputs {
			if _, err := readPinned(pin, fixture.Policy.Paths, fixture.Settings.MaximumFileBytes); err != nil {
				t.Fatal("input bytes changed")
			}
		}
	}
	recordBytes, err := os.ReadFile(filepath.Join(fixture.Policy.SelectionRoot, "unmeasured-recovery.000001.json"))
	if err != nil || !bytes.Contains(recordBytes, []byte(`"fresh_clock_exception":true`)) || !bytes.Contains(recordBytes, []byte(`"original_deadline_not_preserved":true`)) {
		t.Fatal("fresh clock exception was hidden")
	}
	if _, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("spent recovery claim admitted a retry")
	}
}

// Even a failure before the first rename permanently consumes the one-use claim.
func TestPartialApplyRetainsDurableAuditAndCannotRetry(t *testing.T) {
	fixture := newRecoveryFixture(t)
	fixture.Request.Apply = true
	fixture.Hooks.AfterStep = func(step string) error {
		if step == "audit-durable" {
			return errors.New("synthetic failure after durable audit")
		}
		return nil
	}
	if _, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("injected failure missing")
	}
	if _, err := os.Stat(filepath.Join(fixture.Request.Config.AuditRoot, "failure-audit.json")); err != nil {
		t.Fatal("failure audit missing before mutation")
	}
	if _, err := readPinned(fixture.Request.Config.OldGlobalStart, fixture.Policy.Paths, fixture.Settings.MaximumMetadataBytes); err != nil {
		t.Fatal("old start changed before audit barrier")
	}
	fixture.Hooks.AfterStep = func(string) error { return nil }
	if _, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("partial apply retried automatically")
	}
}

// Approval absence, pin mismatch, and missing explicit clock authority all refuse.
func TestApplyRequiresExactProspectiveFreshClockApproval(t *testing.T) {
	fixture := newRecoveryFixture(t)
	fixture.Request.Apply = true
	fixture.Request.ApprovalPin = filePin{}
	if _, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("apply admitted missing owner approval")
	}
	approval := ownerApproval{}
	if err := validateApproval(approval, fixture.Request); err == nil {
		t.Fatal("implicit fresh clock authority admitted")
	}
	if _, err := os.Stat(filepath.Join(fixture.Policy.SelectionRoot, "unmeasured-recovery.claim.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unapproved apply spent claim")
	}
}

// Scope admission precedes the first claim, archive, or byte copy in composition.
func TestRecoveryThreeGibibyteAncestorRefusesBeforeMutation(t *testing.T) {
	fixture := newRecoveryFixture(t)
	fixture.Request.Apply = true
	fixture.Hooks.MemoryLimits = func() ([]cgroupLimit, error) {
		return []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: 8 * 1024 * 1024 * 1024}, {Path: "/synthetic", LimitBytes: 3 * 1024 * 1024 * 1024}}, nil
	}
	if _, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("restrictive parent admitted recovery work")
	}
	if _, err := os.Stat(filepath.Join(fixture.Policy.SelectionRoot, "unmeasured-recovery.claim.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("memory refusal mutated runtime state")
	}
}

// The exact same native lock inode is held before any persistent recovery work.
func TestHeldNativeScoringLockRefusesBeforeClaimOrMove(t *testing.T) {
	fixture := newRecoveryFixture(t)
	fixture.Request.Apply = true
	lock, err := openProtected(fixture.Request.Config.ScoringLock.Path, fixture.Policy.Paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if _, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
		t.Fatal("held native scoring lock admitted recovery")
	}
	if _, err := os.Stat(filepath.Join(fixture.Policy.SelectionRoot, "unmeasured-recovery.claim.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock refusal spent recovery claim")
	}
	if _, err := readPinned(fixture.Request.Config.OldGlobalStart, fixture.Policy.Paths, fixture.Settings.MaximumMetadataBytes); err != nil {
		t.Fatal("lock refusal moved old start")
	}
}

// Every result/progress location, including nested empty-output violations, fails.
func TestFailedTreeRejectsEveryMeasurementAndUnexpectedOutput(t *testing.T) {
	paths := []string{"artifacts/evaluation-progress.json", "artifacts/baseline.json", "artifacts/score.json", "artifacts/evaluation.complete.json", "outcome.json", "manager-terminal.log", "artifacts/failed-evidence/runs/synthetic-run", "artifacts/failed-evidence/score-output/synthetic-score", "artifacts/.evidence-runtime/failure.json"}
	for _, path := range paths {
		fixture := newRecoveryFixture(t)
		writeSyntheticFile(t, filepath.Join(firstJobRoot(fixture.Policy), path), []byte("synthetic inadmissible output"))
		if _, _, err := failedTreeInventory(firstJobRoot(fixture.Policy), fixture.Policy.Paths, fixture.Settings); err == nil {
			t.Errorf("measurement admitted at %s", path)
		}
	}
}

// Unstarted siblings and the second round must remain exact initial inputs.
func TestRecoveryRejectsProgressInAnyOtherJob(t *testing.T) {
	for index := 1; index < 6; index++ {
		fixture := newRecoveryFixture(t)
		writeSyntheticFile(t, filepath.Join(fixture.Request.Config.Jobs[index].Root, "started.json"), []byte("synthetic forbidden start"))
		if _, err := recoverUnmeasured(fixture.Request, fixture.Policy, fixture.Settings, fixture.Hooks); err == nil {
			t.Errorf("progress admitted in sibling %d", index)
		}
	}
}

// Symlinks, writable ancestors, and hardlink aliases cannot redirect authority.
func TestProtectedPathsRejectSymlinksWritableAncestorsAndHardlinks(t *testing.T) {
	fixture := newRecoveryFixture(t)
	target := filepath.Join(fixture.Policy.SelectionRoot, "synthetic-link")
	if err := os.Symlink(fixture.Request.Config.PublicPlan.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := checkProtectedPath(target, fixture.Policy.Paths, false); err == nil {
		t.Fatal("symlink admitted")
	}
	hardlink := filepath.Join(fixture.Policy.SelectionRoot, "synthetic-hardlink")
	if err := os.Link(fixture.Request.Config.PublicPlan.Path, hardlink); err != nil {
		t.Fatal(err)
	}
	if err := checkProtectedPath(hardlink, fixture.Policy.Paths, false); err == nil {
		t.Fatal("hardlink admitted")
	}
	if err := os.Chmod(fixture.Policy.SelectionRoot, 0770); err != nil {
		t.Fatal(err)
	}
	if err := checkProtectedPath(fixture.Request.Config.PublicPlan.Path, fixture.Policy.Paths, false); err == nil {
		t.Fatal("group-writable ancestor admitted")
	}
}

// Untrusted metadata cannot shadow approval fields or leak decoded values.
func TestAdministrativeMetadataRejectsDuplicateUnknownAndTrailingFields(t *testing.T) {
	for _, content := range []string{`{"schema":1,"schema":2}`, `{"unrecognized":"synthetic"}`, `{} {}`} {
		var approval ownerApproval
		if err := decodeMetadata([]byte(content), &approval); err == nil {
			t.Errorf("ambiguous metadata admitted: %s", content)
		}
	}
	if validDigest(strings.Repeat("A", 64)) {
		t.Fatal("noncanonical uppercase digest admitted")
	}
}

// A synthetic 180-mibibyte executable exceeds the incident's known binary size;
// the default admission cap must cover it without reading a binary into memory.
func TestDefaultOpaqueCapCoversLargeQualifiedExecutor(t *testing.T) {
	settings := defaultRecoverySettings()
	if settings.MaximumFileBytes < 180*1024*1024 {
		t.Fatal("opaque streaming cap would reject the qualified executor before recovery preflight")
	}
}

// A small sparse fixture proves the same streaming size boundary deterministically.
func TestOpaqueStreamingHashRejectsOverCapAndAcceptsExactBoundary(t *testing.T) {
	anchor := t.TempDir()
	path := filepath.Join(anchor, "synthetic-sparse-file")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(4097); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	policy := pathPolicy{OwnerUid: uint32(os.Getuid()), AnchorPath: anchor}
	if _, _, err := hashProtected(path, policy, 4096); err == nil {
		t.Fatal("over-cap sparse file admitted")
	}
	digest, size, err := hashProtected(path, policy, 4097)
	if err != nil || size != 4097 || digest != digestBytes(make([]byte, 4097)) {
		t.Fatal("exact-boundary streaming digest mismatch")
	}
}

// Go's case-insensitive struct decoding must not override explicit denial with
// an uppercase alias, even when the key is escaped or is the only spelling.
func TestAdministrativeMetadataRejectsCaseFoldApprovalAliases(t *testing.T) {
	for _, content := range []string{
		`{"approved":false,"APPROVED":true}`,
		`{"APPROVED":true}`,
		`{"fresh_clock_exception":false,"FRESH_CLOCK_EXCEPTION":true}`,
		`{"Fresh_Clock_Exception":true}`,
		`{"approved":false,"\u0041PPROVED":true}`,
		`{"approved":false,"\u0061pproved":true}`,
	} {
		var approval ownerApproval
		if err := decodeMetadata([]byte(content), &approval); err == nil {
			t.Errorf("case alias or escaped duplicate owner field admitted: %s", content)
		}
	}
}

// The canonical-key fence applies recursively, not only to top-level approval.
func TestAdministrativeMetadataRejectsNestedCaseFoldAliases(t *testing.T) {
	for _, content := range []string{
		`{"jobs":[{"root":"synthetic-original","ROOT":"synthetic-substitute","inputs":[]}]}`,
		`{"jobs":[{"ROOT":"synthetic-substitute","inputs":[]}]}`,
		`{"jobs":[{"inputs":[{"path":"synthetic-original","PATH":"synthetic-substitute","sha256":"synthetic-digest"}]}]}`,
	} {
		var config recoveryConfig
		if err := decodeMetadata([]byte(content), &config); err == nil {
			t.Errorf("nested case alias admitted: %s", content)
		}
	}
}

// Canonical positive metadata still decodes the explicit exception normally.
func TestAdministrativeMetadataAcceptsCanonicalOwnerApproval(t *testing.T) {
	content := []byte(`{"schema":1,"approved":true,"fresh_clock_exception":true,"acknowledge_old_deadline_exception":true,"first_job_ids":["synthetic-job"]}`)
	var approval ownerApproval
	if err := decodeMetadata(content, &approval); err != nil {
		t.Fatal(err)
	}
	if approval.Schema != 1 || !approval.Approved || !approval.FreshClockException || !approval.AcknowledgeOldDeadlineException || len(approval.FirstJobIds) != 1 || approval.FirstJobIds[0] != "synthetic-job" {
		t.Fatal("canonical owner metadata no longer decodes exactly")
	}
}
