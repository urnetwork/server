//go:build linux

// Stop cleanup owns only identities derived before launch from the signed
// request and pinned builder release. Candidate-written files never choose a
// Docker object, cgroup or mount on which the privileged cleanup may operate.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

// BuildKit containers are not job-labelled. Freeze their exact release-derived
// slice alongside the finite full-R9 evaluation slices before any launch.
func (self *Agent) prepareCleanupPlan(ctx context.Context, manifest StartManifest) (*cleanupPlan, error) {
	boundary, err := self.runtimeAdmission(ctx)
	if err != nil {
		return nil, err
	}
	defer boundary.Close()
	request, err := decodeEvaluatorRequest(manifest.EvaluatorRequest)
	if err != nil {
		return nil, err
	}
	builderBytes, err := self.cleanupIdentityCommand(ctx, self.config.SubmissionBuilderIdentity, nil, nil, manifest.Identity)
	if err != nil {
		return nil, err
	}
	var builder struct {
		Schema                       int      `json:"schema"`
		Kind                         string   `json:"kind"`
		BuilderSha256                string   `json:"builder_sha256"`
		SubevaluationOwnershipSha256 string   `json:"subevaluation_ownership_sha256"`
		BackingImages                []string `json:"backing_images"`
	}
	if json.Unmarshal(builderBytes, &builder) != nil || builder.Schema != 1 || builder.Kind != "sim-latency-submission-builder/tracked-v1" || builder.BuilderSha256 != self.config.SubmissionBuilderSha256 ||
		builder.SubevaluationOwnershipSha256 != self.config.SubevaluationOwnership.Sha256 {
		return nil, errors.New("cleanup builder identity differs from its installed release pin")
	}
	if err := validateDockerBackingImages(builder.BackingImages); err != nil {
		return nil, err
	}
	// This is the same fixed jq filter used by evaluator-subevaluation.sh. Hash
	// its exact bytes, including the final newline, rather than approximating
	// jq's Unicode/number/pretty-print encoding in a second implementation.
	policyBytes, err := self.cleanupIdentityCommand(ctx, self.config.JsonFormatter, []string{"-S", ".patch_policy"}, manifest.EvaluatorRequest, manifest.Identity)
	if err != nil || !sameCleanupJson(policyBytes, request.PatchPolicy) {
		return nil, errors.New("cleanup policy formatter did not retain the authenticated patch policy")
	}
	policyDigest := sha256.Sum256(policyBytes)
	imageDigest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", manifest.EvaluatorImageDigest,
		request.PatchSha256, hex.EncodeToString(policyDigest[:]), builder.BuilderSha256)))
	imageKey := hex.EncodeToString(imageDigest[:])
	attemptDigest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", manifest.Identity.JobId, manifest.Identity.Attempt)))
	attemptToken := hex.EncodeToString(attemptDigest[:])[:32]
	cgroups := []string{"urnetwork-submission-build-" + imageKey[:32] + ".slice", "urnetwork-evaluation-" + attemptToken + "-preflight.slice"}
	for _, role := range []string{"baseline", "candidate"} {
		for ordinal := 1; ordinal <= 9; ordinal++ {
			cgroups = append(cgroups, fmt.Sprintf("urnetwork-evaluation-%s-%s-%02d.slice", attemptToken, role, ordinal))
		}
		cgroups = append(cgroups, fmt.Sprintf("urnetwork-evaluation-%s-%s-score.slice", attemptToken, role))
	}
	for ordinal := 1; ordinal <= 9; ordinal++ {
		cgroups = append(cgroups, fmt.Sprintf("urnetwork-evaluation-%s-live-%02d.slice", strings.ReplaceAll(manifest.Identity.JobId, "-", ""), ordinal))
	}
	plan := &cleanupPlan{Schema: Schema, RunId: manifest.Identity.RunId, JobId: manifest.Identity.JobId, RoundId: manifest.Identity.RoundId,
		Attempt: manifest.Identity.Attempt, CgroupNames: cgroups, EvidenceDirectory: filepath.Join(self.workDirectory(manifest.Identity.RunId), ".evidence-runtime"),
		EvidenceSource: fmt.Sprintf("urnetwork-evidence-%s-%d", strings.ReplaceAll(manifest.Identity.JobId, "-", ""), manifest.Identity.Attempt)}
	if boundary != nil {
		plan.WorkDeviceId = boundary.WorkDeviceId
	} else {
		workInfo, err := os.Stat(self.config.WorkDirectory)
		if err != nil {
			return nil, err
		}
		workStat, ok := workInfo.Sys().(*syscall.Stat_t)
		if !ok || workStat.Dev == 0 {
			return nil, errors.New("candidate work filesystem has no concrete native device identity")
		}
		plan.WorkDeviceId = uint64(workStat.Dev)
	}
	resourceBytes, err := self.cleanupIdentityCommand(ctx, self.config.DockerRole.ResourcePolicy, nil, nil, manifest.Identity)
	if err != nil {
		return nil, err
	}
	resource, err := decodeRunResourceIdentity(resourceBytes)
	if err != nil {
		return nil, err
	}
	if boundary != nil {
		if err := validateRuntimeBoundaryManagementIdentity(*boundary, resource); err != nil {
			return nil, err
		}
	}
	plan.ManagementCpuSet = resource.ManagementCpuSet
	plan.SliceResources = &runSliceResources{ResourcePolicySha256: self.config.DockerRole.ResourcePolicy.Sha256, Identity: resource,
		BuildTasksMaximum: self.settings.BuildTasksMaximum, CpuPeriodMicros: self.settings.ContainmentCpuPeriodMicros}
	if _, err := prepareRunSliceLimits(*plan, self.config); err != nil {
		return nil, err
	}
	plan.DockerOwner, err = self.makeDockerOwnerPlan(manifest, *plan, resource, builder.BackingImages)
	if err != nil {
		return nil, err
	}
	if err := validateCleanupPlan(*plan, self.config); err != nil {
		return nil, err
	}
	if boundary != nil {
		if err := boundary.Recheck(ctx); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// Only pinned fixed helpers and fixed arguments run, as the non-root worker,
// with bounded input/output/time and no inherited application credentials.
func (self *Agent) cleanupIdentityCommand(ctx context.Context, pin Pin, args []string, input []byte, identity Identity) ([]byte, error) {
	if err := validatePin(pin, self.protected); err != nil {
		return nil, err
	}
	child, cancel := context.WithTimeout(ctx, self.settings.CleanupCommandTimeout)
	defer cancel()
	output := &boundedOutput{maximum: self.config.Limits.DocumentBytes}
	command := exec.Command(pin.Path, args...)
	command.Env, command.Stdin = append(controlEnvironment(), subevaluationScopeEnvironment(identity)...), bytes.NewReader(input)
	command.Stdout, command.Stderr = output, &discardWriter{}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: self.credential}
	code, err := runProcessGroup(child, command, self.settings.CancelGrace)
	if err != nil || code != 0 || output.exceeded {
		return nil, errors.New("fixed cleanup identity helper failed or exceeded its bound")
	}
	return append([]byte(nil), output.Bytes()...), nil
}

// Only the authenticated manifest supplies this prospective helper scope. No
// ambient caller environment can select another job, round, attempt or run.
func subevaluationScopeEnvironment(identity Identity) []string {
	return []string{"URNETWORK_SUBEVALUATION_RUN_ID=" + identity.RunId,
		"URNETWORK_SUBEVALUATION_JOB_ID=" + identity.JobId, "URNETWORK_SUBEVALUATION_ROUND_ID=" + identity.RoundId,
		fmt.Sprintf("URNETWORK_SUBEVALUATION_ATTEMPT=%d", identity.Attempt)}
}

// Exact semantic JSON comparison retains integer precision and rejects extra
// documents. The formatter is pinned; its output is never independent authority.
func sameCleanupJson(left, right []byte) bool {
	decode := func(content []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, errors.New("cleanup JSON has trailing data")
		}
		return value, nil
	}
	l, lErr := decode(left)
	r, rErr := decode(right)
	return lErr == nil && rErr == nil && reflect.DeepEqual(l, r)
}

// Systemd invokes this only after its evaluator process group has stopped,
// including SIGKILL, OOM, startup failure and the original absolute deadline.
// The SSH protocol cannot request cleanup independently of stop ownership.
func (self *Agent) Cleanup(ctx context.Context, runId string) error {
	if !digestPattern.MatchString(runId) {
		return errors.New("invalid fixed cleanup identity")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, self.settings.CleanupTimeout)
	defer cancel()
	// Physical reap never waits for admission's publication lock. Immutable
	// manifest/plan bytes exist before launch, and unfinished cleanup retains
	// the slot. A per-run lock only prevents duplicate cleanup subprocesses.
	lock, err := self.lockFile(cleanupCtx, filepath.Join(self.stateDirectory(runId), "cleanup.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := self.loadState(runId)
	if err != nil {
		return err
	}
	manifest, err := self.loadManifest(runId, state.ManifestSha256)
	if err != nil || !state.LaunchAttempted {
		return errors.New("cleanup has no authenticated launched run")
	}
	slot, err := self.loadSlot()
	if err != nil || slot.RunId != runId {
		return errors.New("cleanup cannot act after physical slot custody changed")
	}
	plan, err := self.loadCleanupPlan(runId, state, manifest)
	if err != nil {
		return err
	}
	if state.StopCleanupVerified && state.CleanupVerified {
		return nil
	}
	reaped, err := self.loadResourcesReaped(runId, state.CleanupPlanSha256)
	if err != nil {
		return err
	}
	if err := self.containment.Quiesce(cleanupCtx, *plan, reaped); err != nil {
		return err
	}
	if !reaped {
		if err := self.cleaner.Cleanup(cleanupCtx, *plan); err != nil {
			return err
		}
		if err := self.containment.VerifyRetained(cleanupCtx, *plan); err != nil {
			return err
		}
		if err := self.storeResourcesReaped(runId, state.CleanupPlanSha256); err != nil {
			return err
		}
	}
	if err := self.containment.Release(cleanupCtx, *plan); err != nil {
		return err
	}
	publication, err := self.lock(cleanupCtx)
	if err != nil {
		return err
	}
	defer publication.Close()
	current, err := self.loadState(runId)
	if err != nil {
		return err
	}
	slot, err = self.loadSlot()
	if err != nil || slot.RunId != runId || current.ManifestSha256 != state.ManifestSha256 || current.CleanupPlanSha256 != state.CleanupPlanSha256 || !current.LaunchAttempted {
		return errors.New("cleanup custody changed before durable publication")
	}
	state = current
	state.CleanupVerified, state.StopCleanupVerified = true, true
	return self.storeState(runId, state)
}

// The plan's hash is committed in root-only run state before launch. Its
// redundant identity fields must agree with the signed persisted manifest.
func (self *Agent) loadCleanupPlan(runId string, state *runState, manifest *StartManifest) (*cleanupPlan, error) {
	content, err := readBounded(filepath.Join(self.stateDirectory(runId), "cleanup-plan.json"), self.config.Limits.DocumentBytes)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	plan := &cleanupPlan{}
	if hex.EncodeToString(digest[:]) != state.CleanupPlanSha256 || decodeStrict(content, plan) != nil || plan.Schema != Schema ||
		plan.RunId != runId || plan.JobId != manifest.Identity.JobId || plan.RoundId != manifest.Identity.RoundId || plan.Attempt != manifest.Identity.Attempt ||
		plan.EvidenceDirectory != filepath.Join(self.workDirectory(runId), ".evidence-runtime") ||
		plan.EvidenceSource != fmt.Sprintf("urnetwork-evidence-%s-%d", strings.ReplaceAll(manifest.Identity.JobId, "-", ""), manifest.Identity.Attempt) {
		return nil, errors.New("persisted cleanup scope failed authentication")
	}
	if err := validateCleanupPlan(*plan, self.config); err != nil {
		return nil, err
	}
	if err := self.verifyDockerOwnerPlan(*manifest, *plan); err != nil {
		return nil, err
	}
	return plan, nil
}
