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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

// BuildKit containers are not job-labelled. Freeze their exact release-derived
// slice alongside the finite full-R9 evaluation slices before any launch.
func (self *Agent) prepareCleanupPlan(ctx context.Context, manifest StartManifest) (*cleanupPlan, error) {
	if err := self.verifyCleanupRuntime(); err != nil {
		return nil, err
	}
	request, err := decodeEvaluatorRequest(manifest.EvaluatorRequest)
	if err != nil {
		return nil, err
	}
	builderBytes, err := self.cleanupIdentityCommand(ctx, self.config.SubmissionBuilderIdentity, nil, nil)
	if err != nil {
		return nil, err
	}
	var builder struct {
		Schema        int    `json:"schema"`
		Kind          string `json:"kind"`
		BuilderSha256 string `json:"builder_sha256"`
	}
	if json.Unmarshal(builderBytes, &builder) != nil || builder.Schema != 1 || builder.Kind != "sim-latency-submission-builder/v2" || builder.BuilderSha256 != self.config.SubmissionBuilderSha256 {
		return nil, errors.New("cleanup builder identity differs from its installed release pin")
	}
	// This is the same fixed jq filter used by evaluator-minimum-work.sh. Hash
	// its exact bytes, including the final newline, rather than approximating
	// jq's Unicode/number/pretty-print encoding in a second implementation.
	policyBytes, err := self.cleanupIdentityCommand(ctx, self.config.JsonFormatter, []string{"-S", ".patch_policy"}, manifest.EvaluatorRequest)
	if err != nil || !sameCleanupJson(policyBytes, request.PatchPolicy) {
		return nil, errors.New("cleanup policy formatter did not retain the authenticated patch policy")
	}
	policyDigest := sha256.Sum256(policyBytes)
	imageDigest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", manifest.EvaluatorImageDigest,
		request.PatchSha256, hex.EncodeToString(policyDigest[:]), builder.BuilderSha256)))
	imageKey := hex.EncodeToString(imageDigest[:])
	attemptDigest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", manifest.Identity.JobId, manifest.Identity.Attempt)))
	attemptToken := hex.EncodeToString(attemptDigest[:])[:32]
	cgroups := []string{"urnetwork-submission-build-" + imageKey[:32] + ".slice"}
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
	if err := validateCleanupPlan(*plan, self.config); err != nil {
		return nil, err
	}
	return plan, nil
}

// Only pinned fixed helpers and fixed arguments run, as the non-root worker,
// with bounded input/output/time and no inherited application credentials.
func (self *Agent) cleanupIdentityCommand(ctx context.Context, pin Pin, args []string, input []byte) ([]byte, error) {
	if err := validatePin(pin, self.protected); err != nil {
		return nil, err
	}
	child, cancel := context.WithTimeout(ctx, self.settings.CleanupCommandTimeout)
	defer cancel()
	output := &boundedOutput{maximum: self.config.Limits.DocumentBytes}
	command := exec.Command(pin.Path, args...)
	command.Env, command.Stdin = controlEnvironment(), bytes.NewReader(input)
	command.Stdout, command.Stderr = output, &discardWriter{}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: self.credential}
	code, err := runProcessGroup(child, command, self.settings.CancelGrace)
	if err != nil || code != 0 || output.exceeded {
		return nil, errors.New("fixed cleanup identity helper failed or exceeded its bound")
	}
	return append([]byte(nil), output.Bytes()...), nil
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
	lock, err := self.lock(cleanupCtx)
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
	// A failed or interrupted retry must never inherit an older success bit.
	state.CleanupVerified, state.StopCleanupVerified = false, false
	if err := self.storeState(runId, state); err != nil {
		return err
	}
	if err := self.cleaner.Cleanup(cleanupCtx, *plan); err != nil {
		return err
	}
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
	return plan, nil
}
