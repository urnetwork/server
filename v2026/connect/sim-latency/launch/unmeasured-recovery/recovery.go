// One-use administrative archival keeps every failed byte and preserves the
// qualified native executor; fresh-clock authority is an explicit exception.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The caller's prospective approval is independent of the recovery config pin.
type recoveryRequest struct {
	Config      recoveryConfig
	ConfigPin   filePin
	ApprovalPin filePin
	Apply       bool
}

// Deterministic tests replace observations and inject partial-apply failures.
// Hooks do not add an evaluator launcher or a claim-removal/rollback operation.
type recoveryHooks struct {
	Now          func() time.Time
	CheckRuntime func(recoveryConfig, recoverySettings) error
	MemoryLimits func() ([]cgroupLimit, error)
	AfterStep    func(string) error
}

// Old attempt data remains historical and is never relabeled as a fresh start.
type jobStart struct {
	Schema        int       `json:"schema"`
	JobId         string    `json:"job_id"`
	RoundId       string    `json:"round_id"`
	RequestSha256 string    `json:"request_sha256"`
	Attempt       int       `json:"attempt"`
	StartedAt     time.Time `json:"started_at"`
	Deadline      time.Time `json:"original_deadline"`
}

// Compare owner scope without printing identities, seeds, or request contents.
func validateApproval(approval ownerApproval, request recoveryRequest) error {
	config := request.Config
	if approval.Schema != 1 || approval.Kind != "sim-latency-unmeasured-recovery-owner-approval" || !approval.Approved || !approval.FreshClockException || !approval.AcknowledgeOldDeadlineException ||
		approval.ConfigSha256 != request.ConfigPin.Sha256 || approval.FailureProofSha256 != config.FailureProof.Sha256 || approval.PublicPlanSha256 != config.PublicPlan.Sha256 ||
		approval.OldGlobalStartSha256 != config.OldGlobalStart.Sha256 || approval.OldFirstStartSha256 != config.OldFirstStart.Sha256 || approval.FailedTreeSha256 != config.FailedTreeSha256 ||
		!approval.OldDeadline.Equal(config.OldDeadline) || !reflect.DeepEqual(approval.FirstJobIds, config.FirstJobIds) || approval.FreshClockLimitSeconds != freshClockLimitSeconds ||
		!approval.NoNewEntropy || !approval.NoInputChanges || !approval.SecondSeedUnstarted || !approval.AllFailedArtifactsRetained {
		return errors.New("prospective owner approval does not authorize this exact fresh-clock exception")
	}
	return nil
}

// Every raw evidence pin is authenticated without decoding its contents.
func validateFailureProof(config recoveryConfig, policy recoveryPolicy, settings recoverySettings) error {
	content, err := readPinned(config.FailureProof, policy.Paths, settings.MaximumMetadataBytes)
	if err != nil {
		return err
	}
	var proof failureProof
	if err := decodeMetadata(content, &proof); err != nil {
		return err
	}
	if proof.Schema != 1 || proof.Kind != "sim-latency-unmeasured-setup-failure-proof" || proof.CampaignRoot != config.CampaignRoot || proof.FirstJobRoot != firstJobRoot(policy) ||
		proof.PublicPlanSha256 != config.PublicPlan.Sha256 || proof.OldGlobalStartSha256 != config.OldGlobalStart.Sha256 || proof.OldFirstStartSha256 != config.OldFirstStart.Sha256 || proof.FailedTreeSha256 != config.FailedTreeSha256 ||
		!proof.ZeroMeasurementsProved || !proof.SetupOomBeforeSimulation || !proof.NoSimulationProgress || !proof.NoResultsOrBaseline || !proof.NoCompletionOrOutcomes || !proof.RunsAndScoreOutputEmpty ||
		!proof.NoActiveProcesses || !proof.NoResidualMounts || !proof.SecondSeedUnstarted || len(proof.RawEvidence) == 0 {
		return errors.New("independent zero-measurement failure proof is incomplete or mismatched")
	}
	seenPaths := map[string]bool{}
	for _, pin := range proof.RawEvidence {
		if seenPaths[pin.Path] || pin.Path == config.FailureProof.Path || strings.HasPrefix(pin.Path, config.CampaignRoot+"/") {
			return errors.New("independent raw proof evidence must be distinct and outside the failed campaign")
		}
		seenPaths[pin.Path] = true
		digest, _, err := hashProtected(pin.Path, policy.Paths, settings.MaximumFileBytes)
		if err != nil || !validDigest(pin.Sha256) || digest != pin.Sha256 {
			return errors.New("raw forensic evidence pin mismatch")
		}
	}
	return nil
}

// All six roots and input hashes are derived from the exact published plan.
func validateConfig(request recoveryRequest, policy recoveryPolicy, settings recoverySettings) (publicPlan, error) {
	config := request.Config
	var plan publicPlan
	if config.Schema != 1 || config.Kind != "sim-latency-unmeasured-recovery-config" || config.CampaignRoot != policy.CampaignRoot || config.SelectionRoot != policy.SelectionRoot ||
		config.AuditRoot != filepath.Join(policy.SelectionRoot, "unmeasured-recovery-forensics-000001") || config.PublicPlan.Path != filepath.Join(policy.SelectionRoot, "public-plan.json") || config.PublicPlan.Sha256 != policy.PublicPlanSha256 ||
		config.OldGlobalStart.Path != filepath.Join(policy.CampaignRoot, "execution.started.json") || config.OldFirstStart.Path != filepath.Join(firstJobRoot(policy), "started.json") ||
		!validDigest(config.FailedTreeSha256) || config.OldDeadline.IsZero() || len(config.Jobs) != 6 || len(config.FirstJobIds) != 3 {
		return plan, errors.New("recovery config changes fixed authority, job census, or failure pins")
	}
	content, err := readPinned(request.ConfigPin, policy.Paths, settings.MaximumMetadataBytes)
	if err != nil {
		return plan, err
	}
	var reread recoveryConfig
	if err := decodeMetadata(content, &reread); err != nil || !reflect.DeepEqual(reread, config) {
		return plan, errors.New("recovery config changed")
	}
	content, err = readPinned(config.PublicPlan, policy.Paths, settings.MaximumMetadataBytes)
	if err != nil {
		return plan, err
	}
	if err := json.Unmarshal(content, &plan); err != nil || len(plan.Selected.Rounds) != 1 || plan.Selected.Rounds[0].Position != 1 || plan.UnstartedSecondRound.Position != 2 {
		return plan, errors.New("public first-committed plan projection mismatch")
	}
	rounds := []publicRound{plan.Selected.Rounds[0], plan.UnstartedSecondRound}
	for roundIndex, round := range rounds {
		if len(round.Jobs) != 3 || !validDigest(round.ProvidersSha256) {
			return plan, errors.New("public job census mismatch")
		}
		for jobIndex, job := range round.Jobs {
			if job.Position != jobIndex+1 || (job.Role != "better" && job.Role != "worse" && job.Role != "no-op") || job.JobId == "" {
				return plan, errors.New("public job order or role mismatch")
			}
			root := filepath.Join(policy.CampaignRoot, fmt.Sprintf("round-%02d", roundIndex+1), fmt.Sprintf("job-%02d-%s", jobIndex+1, job.Role))
			pins := config.Jobs[roundIndex*len(round.Jobs)+jobIndex]
			if pins.Root != root || len(pins.Inputs) != 3 || (roundIndex == 0 && config.FirstJobIds[jobIndex] != job.JobId) {
				return plan, errors.New("same original six jobs and first three identities required")
			}
			expectedNames := []string{"canonical.patch", "precommitted-request.json", "providers.yml"}
			for index, pin := range pins.Inputs {
				if pin.Path != filepath.Join(root, "artifacts", expectedNames[index]) || !validDigest(pin.Sha256) || (index == 1 && pin.Sha256 != job.RequestSha256) || (index == 2 && pin.Sha256 != round.ProvidersSha256) {
					return plan, errors.New("original three input paths or public request/provider hashes changed")
				}
			}
		}
	}
	if config.Jobs[0].Root != firstJobRoot(policy) {
		return plan, errors.New("only the first better job is eligible")
	}
	requiredPins := map[string]string{
		filepath.Join(policy.CampaignRoot, "private-plan.json"):                        plan.OriginalPrivatePlanSha256,
		filepath.Join(policy.SelectionRoot, "selected-campaign", "private-plan.json"):  plan.Selected.PrivatePlanSha256,
		filepath.Join(policy.CampaignRoot, "configuration.json"):                       "",
		filepath.Join(policy.CampaignRoot, "evaluation-policy-canonical.json"):         "",
		filepath.Join(policy.CampaignRoot, "preparation.started"):                      "",
		filepath.Join(policy.CampaignRoot, "first-committed-selection.json"):           "",
		filepath.Join(policy.SelectionRoot, "selected-campaign", "configuration.json"): "",
		filepath.Join(policy.SelectionRoot, "execution.json"):                          "",
		config.Executor.Path: config.Executor.Sha256,
	}
	if !validDigest(config.Executor.Sha256) || !validDigest(config.ScoringLock.Sha256) || len(config.RuntimeProcessMarkers) == 0 {
		return plan, errors.New("executor, scoring lock, and runtime process markers required")
	}
	if _, err := validateLegacyLockPath(config, policy.ScoringLock); err != nil {
		return plan, err
	}
	for _, marker := range config.RuntimeProcessMarkers {
		if !filepath.IsAbs(marker) || filepath.Clean(marker) != marker {
			return plan, errors.New("runtime process marker must be a canonical absolute program path")
		}
	}
	seenPaths := map[string]bool{}
	for _, pin := range config.ProtectedPins {
		if seenPaths[pin.Path] || strings.HasPrefix(pin.Path, firstJobRoot(policy)+"/") || !validDigest(pin.Sha256) {
			return plan, errors.New("duplicate, moved, or invalid protected source pin")
		}
		seenPaths[pin.Path] = true
		if expected, exists := requiredPins[pin.Path]; exists && expected != "" && expected != pin.Sha256 {
			return plan, errors.New("private plan or executor differs from its original pin")
		}
		digest, _, err := hashProtected(pin.Path, policy.Paths, settings.MaximumFileBytes)
		if err != nil || digest != pin.Sha256 {
			return plan, errors.New("protected authority/source pin mismatch")
		}
	}
	for path := range requiredPins {
		if !seenPaths[path] {
			return plan, errors.New("required original authority/source pin omitted")
		}
	}
	for _, root := range []string{policy.CampaignRoot, policy.SelectionRoot} {
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return plan, errors.New("protected root must be a private 0700 directory")
		}
	}
	return plan, nil
}

// Repeated checks are read-only; no existing claim or journal is ever cleared.
func recoveryPreflight(request recoveryRequest, policy recoveryPolicy, settings recoverySettings, hooks recoveryHooks) ([]treeEntry, int64, error) {
	config := request.Config
	plan, err := validateConfig(request, policy, settings)
	if err != nil {
		return nil, 0, err
	}
	limits, err := hooks.MemoryLimits()
	if err != nil {
		return nil, 0, err
	}
	requiredBytes, err := validateMemoryBudget(config.Memory, limits)
	if err != nil {
		return nil, 0, err
	}
	if err := hooks.CheckRuntime(config, settings); err != nil {
		return nil, 0, err
	}
	for _, path := range []string{config.AuditRoot, filepath.Join(policy.SelectionRoot, "unmeasured-recovery.claim.json"), filepath.Join(policy.SelectionRoot, "unmeasured-recovery.000001.json")} {
		if err := checkProtectedPath(path, policy.Paths, true); err != nil {
			return nil, 0, err
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, 0, errors.New("recovery already claimed or partially applied; no automatic retry")
		}
	}
	campaignNames := []string{"configuration.json", "evaluation-policy-canonical.json", "execution.started.json", "first-committed-selection.json", "preparation.started", "private-plan.json", "round-01", "round-02"}
	if err := requireDirectoryEntries(policy.CampaignRoot, campaignNames, policy.Paths); err != nil {
		return nil, 0, err
	}
	for index, job := range config.Jobs {
		if index != 0 {
			if err := requireDirectoryEntries(job.Root, []string{"artifacts"}, policy.Paths); err != nil {
				return nil, 0, err
			}
			if err := requireDirectoryEntries(filepath.Join(job.Root, "artifacts"), []string{"canonical.patch", "precommitted-request.json", "providers.yml"}, policy.Paths); err != nil {
				return nil, 0, err
			}
		}
		for _, pin := range job.Inputs {
			digest, _, err := hashProtected(pin.Path, policy.Paths, settings.MaximumFileBytes)
			if err != nil || digest != pin.Sha256 {
				return nil, 0, errors.New("original input bytes changed")
			}
		}
	}
	for _, roundName := range []string{"round-01", "round-02"} {
		jobNames := []string{}
		for _, job := range config.Jobs {
			if filepath.Base(filepath.Dir(job.Root)) == roundName {
				jobNames = append(jobNames, filepath.Base(job.Root))
			}
		}
		sort.Strings(jobNames)
		if err := requireDirectoryEntries(filepath.Join(policy.CampaignRoot, roundName), jobNames, policy.Paths); err != nil {
			return nil, 0, err
		}
	}
	entries, treeSha256, err := failedTreeInventory(firstJobRoot(policy), policy.Paths, settings)
	if err != nil {
		return nil, 0, err
	}
	if treeSha256 != config.FailedTreeSha256 {
		return nil, 0, errors.New("failed setup tree differs from independently pinned proof")
	}
	content, err := readPinned(config.OldFirstStart, policy.Paths, settings.MaximumMetadataBytes)
	if err != nil {
		return nil, 0, err
	}
	var start jobStart
	if err := decodeMetadata(content, &start); err != nil || start.Schema != 1 || start.Attempt != 1 || start.JobId != config.FirstJobIds[0] || start.RoundId != plan.Selected.Rounds[0].RoundId ||
		start.RequestSha256 != config.Jobs[0].Inputs[1].Sha256 || !start.Deadline.Equal(config.OldDeadline) || start.StartedAt.IsZero() || !start.StartedAt.Add(time.Duration(freshClockLimitSeconds)*time.Second).Equal(start.Deadline) {
		return nil, 0, errors.New("old first start/deadline or identity binding mismatch")
	}
	if _, err := readPinned(config.OldGlobalStart, policy.Paths, settings.MaximumMetadataBytes); err != nil {
		return nil, 0, err
	}
	workerDigest, _, err := hashProtected(filepath.Join(firstJobRoot(policy), "artifacts", "worker-request.json"), policy.Paths, settings.MaximumFileBytes)
	if err != nil || workerDigest != config.Jobs[0].Inputs[1].Sha256 {
		return nil, 0, errors.New("first final request was not the exact original request bytes")
	}
	if err := validateFailureProof(config, policy, settings); err != nil {
		return nil, 0, err
	}
	if request.ApprovalPin.Path != "" || request.ApprovalPin.Sha256 != "" || request.Apply {
		content, err := readPinned(request.ApprovalPin, policy.Paths, settings.MaximumMetadataBytes)
		if err != nil {
			return nil, 0, err
		}
		var approval ownerApproval
		if err := decodeMetadata(content, &approval); err != nil {
			return nil, 0, err
		}
		if err := validateApproval(approval, request); err != nil {
			return nil, 0, err
		}
	}
	return entries, requiredBytes, nil
}

// A permanent claim makes every partial application a manual-review stop.
// The first durable audit precedes every rename; no existing bytes are deleted.
func recoverUnmeasured(request recoveryRequest, policy recoveryPolicy, settings recoverySettings, hooks recoveryHooks) (*recoveryReport, error) {
	if hooks.Now == nil || hooks.MemoryLimits == nil || hooks.CheckRuntime == nil || hooks.AfterStep == nil {
		return nil, errors.New("complete recovery observation hooks required")
	}
	entries, requiredBytes, err := recoveryPreflight(request, policy, settings, hooks)
	if err != nil {
		return nil, err
	}
	config := request.Config
	report := &recoveryReport{Mode: "dry-run", ConfigSha256: request.ConfigPin.Sha256, ApprovalSha256: request.ApprovalPin.Sha256, FailureProofSha256: config.FailureProof.Sha256,
		FailedTreeSha256: config.FailedTreeSha256, AuditRoot: config.AuditRoot, JobCount: len(config.Jobs), RetainedEntryCount: len(entries), OldDeadline: config.OldDeadline,
		FreshClockException: request.ApprovalPin.Path != "", FreshClockNotStarted: true, RequiredManagementBytes: requiredBytes}
	if !request.Apply {
		return report, nil
	}
	lock, err := openLegacyLock(config, policy.ScoringLock)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("native scoring/execution lock is active")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := recheckHeldLegacyLock(lock, config, policy.ScoringLock); err != nil {
		return nil, err
	}
	entries, _, err = recoveryPreflight(request, policy, settings, hooks)
	if err != nil {
		return nil, err
	}
	if err := recheckHeldLegacyLock(lock, config, policy.ScoringLock); err != nil {
		return nil, err
	}
	claimedAt := hooks.Now().UTC()
	claim := map[string]any{"schema": 1, "kind": "sim-latency-unmeasured-recovery-one-use-claim", "sequence": 1, "claimed_at": claimedAt,
		"config_sha256": request.ConfigPin.Sha256, "owner_approval_sha256": request.ApprovalPin.Sha256, "failure_proof_sha256": config.FailureProof.Sha256,
		"audit_root": config.AuditRoot, "fresh_clock_exception": true, "old_deadline": config.OldDeadline, "retry_after_partial_apply": false}
	claimBytes, err := json.Marshal(claim)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(policy.SelectionRoot, "unmeasured-recovery.claim.json"), claimBytes, 0400, policy.Paths); err != nil {
		return nil, err
	}
	if err := hooks.AfterStep("claimed"); err != nil {
		return nil, err
	}
	if err := os.Mkdir(config.AuditRoot, 0700); err != nil {
		return nil, errors.New("audit creation failed; one-use claim remains spent")
	}
	if err := syncDirectory(policy.SelectionRoot); err != nil {
		return nil, err
	}
	audit := map[string]any{"schema": 1, "kind": "sim-latency-unmeasured-failure-audit", "sequence": 1, "recorded_at": claimedAt, "claim": claim,
		"old_global_start": config.OldGlobalStart, "old_first_start": config.OldFirstStart, "public_plan": config.PublicPlan, "failure_proof": config.FailureProof,
		"failed_tree_sha256": config.FailedTreeSha256, "failed_tree": entries, "protected_pins": config.ProtectedPins, "original_input_pins": config.Jobs,
		"all_failed_artifacts_retained": true, "old_deadline_is_historical_not_preserved": true}
	auditBytes, err := json.Marshal(audit)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(config.AuditRoot, "failure-audit.json"), auditBytes, 0400, policy.Paths); err != nil {
		return nil, err
	}
	if err := hooks.AfterStep("audit-durable"); err != nil {
		return nil, err
	}
	if err := renameRetained(config.OldGlobalStart.Path, filepath.Join(config.AuditRoot, "execution.started.json"), policy.Paths); err != nil {
		return nil, err
	}
	if err := hooks.AfterStep("global-start-archived"); err != nil {
		return nil, err
	}
	archivedRoot := filepath.Join(config.AuditRoot, "first-job")
	if err := renameRetained(firstJobRoot(policy), archivedRoot, policy.Paths); err != nil {
		return nil, err
	}
	if err := hooks.AfterStep("first-job-archived"); err != nil {
		return nil, err
	}
	_, archivedSha256, err := failedTreeInventory(archivedRoot, policy.Paths, settings)
	if err != nil || archivedSha256 != config.FailedTreeSha256 {
		return nil, errors.New("archived forensic tree mismatch; retain partial state")
	}
	recovery := map[string]any{"schema": 1, "kind": "sim-latency-unmeasured-recovery", "sequence": 1, "recovered_at": hooks.Now().UTC(), "claim_sha256": digestBytes(claimBytes),
		"audit_sha256": digestBytes(auditBytes), "config_sha256": request.ConfigPin.Sha256, "owner_approval_sha256": request.ApprovalPin.Sha256,
		"failure_proof_sha256": config.FailureProof.Sha256, "old_global_start_sha256": config.OldGlobalStart.Sha256, "old_first_start_sha256": config.OldFirstStart.Sha256,
		"old_deadline": config.OldDeadline, "fresh_clock_exception": true, "original_deadline_not_preserved": true, "fresh_clock_limit_seconds": freshClockLimitSeconds,
		"fresh_clock_starts_on_next_native_start": true, "fresh_clock_not_started": true, "executor_unchanged": config.Executor, "second_seed_unstarted": true, "no_new_entropy": true,
		"scope": "administrative restoration only; no evaluator invocation; not a claim to preserve the original deadline"}
	recoveryBytes, err := json.Marshal(recovery)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(filepath.Join(policy.SelectionRoot, "unmeasured-recovery.000001.json"), recoveryBytes, 0400, policy.Paths); err != nil {
		return nil, err
	}
	if err := os.Mkdir(firstJobRoot(policy), 0700); err != nil {
		return nil, errors.New("fresh job directory unavailable; retain partial state")
	}
	if err := syncDirectory(filepath.Dir(firstJobRoot(policy))); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(firstJobRoot(policy), "artifacts"), 0700); err != nil {
		return nil, errors.New("fresh artifacts directory unavailable; retain partial state")
	}
	for _, pin := range config.Jobs[0].Inputs {
		if err := copyInputExclusive(filepath.Join(archivedRoot, "artifacts", filepath.Base(pin.Path)), pin, policy.Paths, settings.MaximumFileBytes); err != nil {
			return nil, err
		}
		if err := hooks.AfterStep("input-restored-" + filepath.Base(pin.Path)); err != nil {
			return nil, err
		}
	}
	if err := syncDirectory(firstJobRoot(policy)); err != nil {
		return nil, err
	}
	for _, pin := range config.ProtectedPins {
		digest, _, err := hashProtected(pin.Path, policy.Paths, settings.MaximumFileBytes)
		if err != nil || digest != pin.Sha256 {
			return nil, errors.New("protected source/authority changed; retain recovered state for manual review")
		}
	}
	if err := hooks.AfterStep("restored"); err != nil {
		return nil, err
	}
	report.Mode = "applied-administrative-restoration-only"
	return report, nil
}

// /proc is the only runtime interface: no provider, docker, systemd, or database
// operation occurs. Ambiguous observations fail closed, without printing argv.
func checkRuntimeQuiescent(config recoveryConfig, settings recoverySettings) error {
	return checkRuntimeWithFiles(config, settings, "/proc", os.Getpid(), runtimeFiles{ReadFile: os.ReadFile, ReadDir: os.ReadDir, Readlink: os.Readlink})
}

// Read-only dependency injection makes fd-only evidence and disappearance races
// deterministic without starting a real process or relying on scheduling.
type runtimeFiles struct {
	ReadFile func(string) ([]byte, error)
	ReadDir  func(string) ([]os.DirEntry, error)
	Readlink func(string) (string, error)
}

// Vanished processes/descriptors are benign; inaccessible live evidence is not.
func checkRuntimeWithFiles(config recoveryConfig, settings recoverySettings, procRoot string, selfPid int, files runtimeFiles) error {
	processDirs, err := files.ReadDir(procRoot)
	if err != nil {
		return errors.New("cannot inspect active processes")
	}
	markers := append([]string{config.CampaignRoot, config.Executor.Path}, config.RuntimeProcessMarkers...)
	containsMarker := func(content []byte) bool {
		for _, marker := range markers {
			if marker != "" && strings.Contains(string(content), marker) {
				return true
			}
		}
		return false
	}
	processCount := 0
	descriptorCount := 0
	for _, entry := range processDirs {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() || pid == selfPid {
			continue
		}
		processCount++
		if processCount > settings.MaximumProcessCount {
			return errors.New("process census exceeds bound")
		}
		processRoot := filepath.Join(procRoot, entry.Name())
		for _, name := range []string{"cmdline", "mountinfo"} {
			content, err := files.ReadFile(filepath.Join(processRoot, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || int64(len(content)) > settings.MaximumMetadataBytes {
				return errors.New("cannot prove runtime process or mount quiescence")
			}
			if containsMarker(content) {
				return errors.New("active process or residual mount evidence found")
			}
		}
		for _, name := range []string{"cwd", "root"} {
			target, err := files.Readlink(filepath.Join(processRoot, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return errors.New("cannot prove runtime cwd quiescence")
			}
			if containsMarker([]byte(target)) {
				return errors.New("active process retains campaign filesystem")
			}
		}
		descriptors, err := files.ReadDir(filepath.Join(processRoot, "fd"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return errors.New("cannot prove runtime descriptor quiescence")
		}
		for _, descriptor := range descriptors {
			descriptorCount++
			if descriptorCount > settings.MaximumDescriptorCount {
				return errors.New("process descriptor census exceeds bound")
			}
			target, err := files.Readlink(filepath.Join(processRoot, "fd", descriptor.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return errors.New("cannot inspect active process descriptor")
			}
			if containsMarker([]byte(target)) {
				return errors.New("active process descriptor retains campaign filesystem")
			}
		}
	}
	mounts, err := files.ReadFile(filepath.Join(procRoot, "self", "mountinfo"))
	if err != nil || int64(len(mounts)) > settings.MaximumMetadataBytes || containsMarker(mounts) {
		return errors.New("residual campaign mount found or mount census unavailable")
	}
	return nil
}
