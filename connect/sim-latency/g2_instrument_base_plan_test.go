// The pre-election bridge is a technical test artifact, not a source generation,
// product selection, reviewed-build receipt or production evaluator identity.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// These values describe compilation provenance only. No successor epoch,
// elected product, review, opening time or readiness authority is represented.
type g2InstrumentBaseImagePlan struct {
	Dockerfile []byte
	Identity   []byte
	SourceLock []byte
	Commits    map[string]string
	SourcePath string
}

// Operation ownership is independent of an election, revision or output path.
func g2InstrumentBaseTechnicalOwner(approvalDigest, baseImage string) string {
	digest := sha256.Sum256([]byte("g2-incumbent-instrument-technical-v1\x00" + approvalDigest + "\x00" + baseImage))
	return hex.EncodeToString(digest[:])
}

// The incumbent must prove this entire namespace absent before assembly.
func g2InstrumentBaseTechnicalNamespace(owner string) string {
	return "/opt/urnetwork/instrument-qualification/" + owner
}

// The caller has independently verified the clean child and compiled binary.
// The immutable approval supplies the whole incumbent graph; only its server
// child can change. The resulting identity intentionally fails live admission.
func planG2InstrumentBaseImage(approval model.CompetitionTrustedInstrumentApproval, child, tree, binarySha256, runnerSha256, entrypointSha256 string) (*g2InstrumentBaseImagePlan, error) {
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(&approval); err != nil {
		return nil, err
	}
	if !sourceGitShaPattern.MatchString(child) || child == approval.InstrumentBaseCommits["server"] || !sourceGitShaPattern.MatchString(tree) ||
		!validSha256(binarySha256) || !validSha256(runnerSha256) || !validSha256(entrypointSha256) {
		return nil, errors.New("technical instrument image requires its exact approved child and compiled/script digests")
	}
	commits := maps.Clone(approval.InstrumentBaseCommits)
	commits["server"] = child
	lock, err := json.MarshalIndent(evaluatorSourceLock{Schema: 1, Repositories: commits, WorkCoveragePolicy: approval.WorkCoveragePolicy.Clone()}, "", "  ")
	if err != nil {
		return nil, err
	}
	lock = append(lock, '\n')
	lockDigest := sha256.Sum256(lock)
	canonicalLock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		return nil, err
	}
	sourcePath := g2InstrumentBaseTechnicalNamespace(g2InstrumentBaseTechnicalOwner(approval.ApprovalSha256, approval.InstrumentBaseImageDigest)) + "/source"
	scorerVersion, err := model.CompetitionScorerVersionForWorkCoverage(approval.G2VolumePolicy, approval.WorkCoveragePolicy)
	if err != nil {
		return nil, err
	}
	identityFields := map[string]any{
		"schema": 1, "image_kind": "evaluator-instrument-qualification", "scope": "incumbent-plus-approved-instrument-technical-only",
		"production_eligible": false, "runtime_qualified": false, "instrument_approval_sha256": approval.ApprovalSha256,
		"instrument_patch_sha256": approval.PatchSha256, "incumbent_source_lock_sha256": approval.InstrumentBaseSourceLockSha256,
		"incumbent_image_digest": approval.InstrumentBaseImageDigest, "instrumented_server_commit": child, "instrumented_server_tree": tree,
		"source_lock_sha256": hex.EncodeToString(lockDigest[:]), "simulator_sha256": binarySha256,
		"source_lock_file_sha256": hex.EncodeToString(lockDigest[:]), "canonical_child_source_lock_sha256": canonicalLock, "technical_source_path": sourcePath,
		"g2_volume_policy": model.CompetitionG2VolumePolicyMinimumWork, "scorer_version": scorerVersion,
	}
	if approval.WorkCoveragePolicy != nil {
		identityFields["work_coverage_policy"] = approval.WorkCoveragePolicy.Clone()
	}
	identity, err := json.MarshalIndent(identityFields, "", "  ")
	if err != nil {
		return nil, err
	}
	dockerfile := fmt.Sprintf(`FROM urnetwork/sim-latency-evaluator-base-id:%s
COPY source/ %s/
COPY --chmod=0555 sim-latency /opt/urnetwork/bin/sim-latency
COPY --chmod=0555 source/server/connect/sim-latency/official-run.sh /usr/local/libexec/competition-official-run
COPY --chmod=0555 source/server/connect/sim-latency/evaluator/container/entrypoint.sh /usr/local/libexec/competition-container
COPY --chmod=0444 source-lock.json /opt/urnetwork/source-lock.json
COPY --chmod=0444 image-identity.json /opt/urnetwork/image-identity.json
LABEL org.opencontainers.image.title="URnetwork G2 instrument technical qualification only" \
      org.opencontainers.image.revision="%s" \
      com.urnetwork.competition.image-kind="evaluator-instrument-qualification" \
      com.urnetwork.competition.qualification-only="true" \
      com.urnetwork.competition.base-sha="%s" \
      com.urnetwork.competition.source-epoch="" \
      com.urnetwork.competition.source-config-sha256="" \
      com.urnetwork.competition.source-lock-sha256="%s" \
      com.urnetwork.competition.instrument-approval-sha256="%s" \
      com.urnetwork.competition.official-run-sha256="%s" \
      com.urnetwork.competition.entrypoint-sha256="%s" \
      com.urnetwork.competition.shell-overlay="authenticated-source-scripts-v1" \
      com.urnetwork.competition.shell-overlay-base="%s"
USER 65532:65532
WORKDIR %s/server
`, strings.TrimPrefix(approval.InstrumentBaseImageDigest, "sha256:"), sourcePath, child, child, hex.EncodeToString(lockDigest[:]), approval.ApprovalSha256,
		runnerSha256, entrypointSha256, approval.InstrumentBaseImageDigest, sourcePath)
	return &g2InstrumentBaseImagePlan{Dockerfile: []byte(dockerfile), Identity: append(identity, '\n'), SourceLock: lock, Commits: commits, SourcePath: sourcePath}, nil
}

// Exclusive reservation is the single attempt. A completed or interrupted
// directory refuses another execution; restart cannot reset the original clock.
func reserveG2InstrumentBaseAttempt(ctx context.Context, stateDirectory, approvalDigest, baseImage string, timeout time.Duration, now time.Time) (string, string, time.Time, error) {
	canonical, err := filepath.EvalSymlinks(stateDirectory)
	info, statErr := os.Lstat(stateDirectory)
	if err != nil || statErr != nil || canonical != stateDirectory || !filepath.IsAbs(stateDirectory) || !info.IsDir() || info.Mode().Perm()&0o077 != 0 ||
		!strings.HasPrefix(filepath.Base(stateDirectory), "competition-g2-instrument-base-") || !validSha256(approvalDigest) ||
		!strings.HasPrefix(baseImage, "sha256:") || !validSha256(strings.TrimPrefix(baseImage, "sha256:")) || timeout <= 0 || now.IsZero() {
		return "", "", time.Time{}, errors.New("technical instrument reservation requires its private isolated state and exact approved base")
	}
	if err := ctx.Err(); err != nil {
		return "", "", time.Time{}, err
	}
	owner := g2InstrumentBaseTechnicalOwner(approvalDigest, baseImage)
	directory := filepath.Join(stateDirectory, owner)
	deadline := now.Add(timeout)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if !now.Before(deadline) {
		return "", "", time.Time{}, errors.New("technical instrument original deadline has expired")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", "", time.Time{}, fmt.Errorf("technical instrument attempt already exists or cannot be reserved; reconcile without rerun: %w", err)
	}
	parent, err := os.Open(stateDirectory)
	if err != nil {
		return "", "", time.Time{}, err
	}
	syncErr, closeErr := parent.Sync(), parent.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return "", "", time.Time{}, err
	}
	content, err := json.Marshal(struct {
		Schema         int       `json:"schema"`
		Kind           string    `json:"kind"`
		Owner          string    `json:"owner"`
		ApprovalSha256 string    `json:"approval_sha256"`
		BaseImage      string    `json:"base_image"`
		StartedAt      time.Time `json:"started_at"`
		Deadline       time.Time `json:"deadline"`
	}{Schema: 1, Kind: "incumbent-instrument-technical-attempt", Owner: owner, ApprovalSha256: approvalDigest, BaseImage: baseImage, StartedAt: now.UTC(), Deadline: deadline.UTC()})
	if err != nil {
		return "", "", time.Time{}, err
	}
	if err := publishSourceTestRecord(filepath.Join(directory, "reservation.json"), content); err != nil {
		return "", "", time.Time{}, err
	}
	return directory, owner, deadline, nil
}

// The CLI has no source/runtime mounts. Only fixed read-only technical probes are
// admitted, never a model- or configuration-selected command or argument.
func g2InstrumentBaseCliArguments(owner, image, phase string, settings sourceBuildSandboxSettings) ([]string, error) {
	if !validSha256(owner) || !strings.HasPrefix(image, "sha256:") || !validSha256(strings.TrimPrefix(image, "sha256:")) ||
		settings.CpuSet == "" || settings.CgroupParent == "" || settings.MemoryBytes <= 0 || settings.Pids <= 0 || settings.NoFile <= 0 {
		return nil, errors.New("technical instrument CLI probe needs exact identity and positive containment bounds")
	}
	entrypoint, command := "/opt/urnetwork/bin/sim-latency", []string{"score-policy", "--g2-volume-policy=minimum-work/1"}
	if phase == "identity" {
		entrypoint, command = "/usr/local/libexec/competition-container", []string{"identity"}
	} else if phase == "source-path-vacant" {
		entrypoint, command = "/bin/bash", []string{"-ceu", "test ! -e " + g2InstrumentBaseTechnicalNamespace(owner)}
	} else if phase == "source-graph" {
		// A fixed source-lock reader checks the built-in image snapshot, not the
		// fresh read-only host mount used separately by correctness containers.
		entrypoint, command = "/bin/bash", []string{"-ceu", "root=" + g2InstrumentBaseTechnicalNamespace(owner) + "/source; " +
			"expected_lock=$(jq -er '.source_lock_file_sha256' /opt/urnetwork/image-identity.json); " +
			"actual_lock=$(sha256sum /opt/urnetwork/source-lock.json | cut -d' ' -f1); test \"$actual_lock\" = \"$expected_lock\"; " +
			"for repository in " + strings.Join(sourceRepositoryNames(), " ") + "; do " +
			"expected=$(jq -er --arg repository \"$repository\" '.repositories[$repository]' /opt/urnetwork/source-lock.json); " +
			"actual=$(git -c safe.directory=\"$root/$repository\" -c core.hooksPath=/dev/null -c core.fsmonitor=false --no-optional-locks -C \"$root/$repository\" rev-parse HEAD); " +
			"test \"$actual\" = \"$expected\"; " +
			"status=$(git -c safe.directory=\"$root/$repository\" -c core.hooksPath=/dev/null -c core.fsmonitor=false --no-optional-locks -C \"$root/$repository\" status --porcelain=v1 --untracked-files=all --ignored=matching); " +
			"test -z \"$status\"; done"}
	} else if phase == "coverage-policy" {
		command = append(command, "--work-coverage-policy="+model.CompetitionWorkCoverageKind)
	} else if phase != "policy" {
		return nil, errors.New("technical instrument CLI probe command is not permitted")
	}
	args := []string{"create", "--init", "--pull=never", "--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--cgroupns=private",
		"--name=urnetwork-g2-technical-" + owner + "-" + phase, "--cgroup-parent=" + settings.CgroupParent, "--cpuset-cpus=" + settings.CpuSet,
		"--memory=" + strconv.FormatInt(settings.MemoryBytes, 10), "--memory-swap=" + strconv.FormatInt(settings.MemoryBytes, 10), "--pids-limit=" + strconv.Itoa(settings.Pids),
		"--ulimit=nofile=" + strconv.Itoa(settings.NoFile) + ":" + strconv.Itoa(settings.NoFile), "--ulimit=core=0:0",
		"--label=com.urnetwork.instrument-qualification.owner=" + owner, "--label=com.urnetwork.instrument-qualification.phase=" + phase,
		"--entrypoint=" + entrypoint, image}
	return append(args, command...), nil
}

// Only an explicit validated approval chooses the new fixed CLI operation.
// The historical nil-policy phase/argv stays byte-for-byte unchanged.
func g2InstrumentBasePolicyPhase(coverage *model.CompetitionWorkCoveragePolicy) string {
	if coverage != nil {
		return "coverage-policy"
	}
	return "policy"
}

// Technical capability output must match the full approved relative policy;
// neither a version string nor kind-only claim proves page/byte floor support.
func validateG2InstrumentBaseScorePolicy(content []byte, coverage *model.CompetitionWorkCoveragePolicy) error {
	expectedVersion, err := model.CompetitionScorerVersionForWorkCoverage(model.CompetitionG2VolumePolicyMinimumWork, coverage)
	if err != nil {
		return err
	}
	var actual struct {
		ScoreSchema        int                                  `json:"score_schema"`
		G2VolumePolicy     model.CompetitionG2VolumePolicy      `json:"g2_volume_policy"`
		ScorerVersion      string                               `json:"scorer_version"`
		WorkCoveragePolicy *model.CompetitionWorkCoveragePolicy `json:"work_coverage_policy"`
	}
	if json.Unmarshal(content, &actual) != nil || actual.ScoreSchema != 1 || actual.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork ||
		actual.ScorerVersion != expectedVersion || !reflect.DeepEqual(actual.WorkCoveragePolicy, coverage) {
		return errors.New("technical compiled CLI does not implement the approved scoring policy")
	}
	return nil
}

// The exact name is checked both before execution and before owned cleanup.
type g2InstrumentBaseCliInspect struct {
	sourceCorrectnessContainerInspect
	Name string
}

// Authenticate the actual daemon state before starting any technical CLI.
// Inherited anonymous volumes, extra mounts and widened namespaces all refuse.
func validateG2InstrumentBaseCliContainment(content []byte, id, owner, image, phase string, settings sourceBuildSandboxSettings) error {
	args, err := g2InstrumentBaseCliArguments(owner, image, phase, settings)
	if err != nil || !validSha256(id) {
		return errors.New("technical CLI containment has invalid expected identity")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := validateUniqueJSONValue(decoder); err != nil {
		return err
	}
	var observed g2InstrumentBaseCliInspect
	if json.Unmarshal(content, &observed) != nil {
		return errors.New("technical CLI containment observation is malformed")
	}
	entrypoint, command := "/opt/urnetwork/bin/sim-latency", args[len(args)-2:]
	if phase == "identity" {
		entrypoint, command = "/usr/local/libexec/competition-container", args[len(args)-1:]
	} else if phase == "source-path-vacant" || phase == "source-graph" {
		entrypoint = "/bin/bash"
	}
	host := observed.HostConfig
	if observed.Id != id || observed.Name != "/urnetwork-g2-technical-"+owner+"-"+phase || observed.Image != image || observed.State.Status != "created" || observed.State.Running || observed.Config.User != "65532:65532" ||
		!slices.Equal(observed.Config.Entrypoint, []string{entrypoint}) || !slices.Equal(observed.Config.Cmd, command) ||
		observed.Config.Labels["com.urnetwork.instrument-qualification.owner"] != owner || observed.Config.Labels["com.urnetwork.instrument-qualification.phase"] != phase ||
		len(observed.Mounts) != 0 || host.Privileged || !host.ReadonlyRootfs || host.Init == nil || !*host.Init || host.NetworkMode != "none" || host.CgroupnsMode != "private" ||
		host.CgroupParent != settings.CgroupParent || host.CpusetCpus != settings.CpuSet || host.Memory != settings.MemoryBytes || host.MemorySwap != settings.MemoryBytes ||
		host.PidsLimit == nil || *host.PidsLimit != int64(settings.Pids) || len(host.CapDrop) != 1 || !strings.EqualFold(host.CapDrop[0], "ALL") || len(host.CapAdd) != 0 ||
		len(host.SecurityOpt) != 1 || (host.SecurityOpt[0] != "no-new-privileges:true" && host.SecurityOpt[0] != "no-new-privileges") ||
		len(host.Binds) != 0 || len(host.VolumesFrom) != 0 || len(host.GroupAdd) != 0 || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 || len(host.DeviceCgroupRules) != 0 ||
		host.PidMode != "" || host.IpcMode != "private" || host.UsernsMode != "" || len(host.Mounts) != 0 || len(host.Tmpfs) != 0 {
		return errors.New("technical CLI actual container lost its fixed command or containment")
	}
	limits := map[string]bool{}
	for _, limit := range host.Ulimits {
		if limits[limit.Name] || limit.Soft != limit.Hard || (limit.Name != "core" && limit.Name != "nofile") ||
			(limit.Name == "core" && limit.Soft != 0) || (limit.Name == "nofile" && limit.Soft != int64(settings.NoFile)) {
			return errors.New("technical CLI actual container changed its limits")
		}
		limits[limit.Name] = true
	}
	if len(limits) != 2 {
		return errors.New("technical CLI actual container omitted required limits")
	}
	return nil
}

// A small synthetic approval suffices for image-format tests; no real identity
// or recorded production approval is copied into a test fixture.
func g2InstrumentBasePlanFixture(t *testing.T) (model.CompetitionTrustedInstrumentApproval, *g2InstrumentBaseImagePlan) {
	t.Helper()
	request := sourceBuildImageMinimumWorkFixture(t, false)
	approval := request.TrustedInstrument.Approval
	plan, err := planG2InstrumentBaseImage(approval, strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	return approval, plan
}

// Technical metadata must not acquire a selected-product or successor tuple.
func TestG2InstrumentBasePlanIsCopyOnlyAndNotAnElection(t *testing.T) {
	approval, plan := g2InstrumentBasePlanFixture(t)
	var identity map[string]any
	if err := json.Unmarshal(plan.Identity, &identity); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"source_epoch", "source_generation", "round_id", "review_id", "selected_product_kind", "target_epoch", "source_config_sha256"} {
		if _, present := identity[key]; present {
			t.Fatal("technical qualification fabricated production selection metadata", key)
		}
	}
	if identity["image_kind"] != "evaluator-instrument-qualification" || identity["production_eligible"] != false || identity["runtime_qualified"] != false ||
		identity["instrument_approval_sha256"] != approval.ApprovalSha256 || strings.Contains(string(plan.Dockerfile), "\nRUN ") || strings.Contains(string(plan.Dockerfile), "vault/") {
		t.Fatal("technical image plan widened execution or claimed production qualification")
	}
	if strings.Contains(string(plan.Dockerfile), "COPY source/ /workspace/") || !strings.Contains(string(plan.Dockerfile), "COPY source/ "+plan.SourcePath+"/\n") ||
		!strings.Contains(string(plan.Dockerfile), "WORKDIR "+plan.SourcePath+"/server\n") || identity["technical_source_path"] != plan.SourcePath {
		t.Fatal("technical source merged into the inherited workspace or lost its bound fresh path")
	}
	fileHash := sha256.Sum256(plan.SourceLock)
	canonical, err := model.CompetitionSourceLockDigest(plan.Commits)
	if err != nil || identity["source_lock_file_sha256"] != hex.EncodeToString(fileHash[:]) || identity["source_lock_sha256"] != identity["source_lock_file_sha256"] ||
		identity["canonical_child_source_lock_sha256"] != canonical || identity["source_lock_file_sha256"] == canonical {
		t.Fatal("technical source confused raw image-file and canonical model identities", err)
	}
	for repository, commit := range approval.InstrumentBaseCommits {
		if repository != "server" && plan.Commits[repository] != commit {
			t.Fatal("technical image replaced an incumbent dependency", repository)
		}
	}
}

// Unknown provenance and the unchanged incumbent cannot masquerade as a child.
func TestG2InstrumentBasePlanRejectsMissingOrChangedAuthority(t *testing.T) {
	approval, _ := g2InstrumentBasePlanFixture(t)
	for _, child := range []string{"", approval.InstrumentBaseCommits["server"], strings.Repeat("c", 39)} {
		if _, err := planG2InstrumentBaseImage(approval, child, strings.Repeat("d", 40), strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("1", 64)); err == nil {
			t.Fatal("technical image accepted an unbound instrument child", child)
		}
	}
	approval.ApprovalSha256 = strings.Repeat("0", 64)
	if _, err := planG2InstrumentBaseImage(approval, strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("1", 64)); err == nil {
		t.Fatal("technical image accepted substituted approval authority")
	}
}

// Exercise the actual live evaluator's identity predicate. Even caller-supplied
// matching digest/epoch-shaped metadata cannot relabel the technical image kind.
func TestG2InstrumentBasePlanFailsActualProductionImageAdmission(t *testing.T) {
	_, plan := g2InstrumentBasePlanFixture(t)
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator-minimum-work.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(content), `jq -e --arg base_sha "$base_sha" --arg empty_patch "$EMPTY_PATCH_SHA256" --argjson source_epoch "$source_epoch"`)
	if start < 0 {
		t.Fatal("actual production image predicate is missing")
	}
	_, rest, found := strings.Cut(string(content[start:]), "\n    '")
	predicate, _, closed := strings.Cut(rest, "' \\\n")
	if !found || !closed {
		t.Fatal("actual production image predicate is incomplete")
	}
	var identity map[string]any
	if err := json.Unmarshal(plan.Identity, &identity); err != nil {
		t.Fatal(err)
	}
	emptyPatch := sha256.Sum256(nil)
	for _, c := range []struct {
		kind, tuple, accepted bool
	}{
		{},
		{tuple: true},
		{kind: true},
		{kind: true, tuple: true, accepted: true},
	} {
		inputIdentity := maps.Clone(identity)
		if c.tuple {
			inputIdentity["base_sha"], inputIdentity["build_sha"], inputIdentity["source_epoch"], inputIdentity["patch_sha256"] = strings.Repeat("c", 40), strings.Repeat("c", 40), 2, hex.EncodeToString(emptyPatch[:])
		}
		if c.kind {
			inputIdentity["image_kind"] = "evaluator-base"
		}
		input, err := json.Marshal(inputIdentity)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "jq", "-e", "--arg", "base_sha", strings.Repeat("c", 40), "--arg", "empty_patch", hex.EncodeToString(emptyPatch[:]), "--argjson", "source_epoch", "2", predicate)
		command.Stdin = bytes.NewReader(input)
		if err := command.Run(); (err == nil) != c.accepted {
			t.Fatal("actual production predicate accepted technical identity or lost its positive control", c, err)
		}
	}
}

// No restart can turn the same approval/base into another attempt or deadline.
func TestG2InstrumentBasePlanReservationIsOneAttemptWithOriginalDeadline(t *testing.T) {
	state := filepath.Join(t.TempDir(), "competition-g2-instrument-base-synthetic")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(time.Hour))
	defer cancel()
	// A past test clock reserves without a live parent; its expired parent below
	// separately proves cancellation admission without a timing-sensitive wait.
	directory, owner, deadline, err := reserveG2InstrumentBaseAttempt(context.Background(), state, strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), time.Hour, now)
	if err != nil || !validSha256(owner) || deadline != now.Add(time.Hour) {
		t.Fatal("technical reservation lost its initial operation budget", err)
	}
	before, err := os.ReadFile(filepath.Join(directory, "reservation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := reserveG2InstrumentBaseAttempt(context.Background(), state, strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), 2*time.Hour, now.Add(time.Hour)); err == nil {
		t.Fatal("technical reservation silently retried under a later deadline")
	}
	after, err := os.ReadFile(filepath.Join(directory, "reservation.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("technical reservation was replaced", err)
	}
	if _, _, _, err := reserveG2InstrumentBaseAttempt(ctx, state, strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64), time.Hour, now); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired parent admitted technical work", err)
	}
	parentDeadline := time.Now().Add(24 * time.Hour)
	parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
	defer cancelParent()
	if _, _, deadline, err := reserveG2InstrumentBaseAttempt(parent, state, strings.Repeat("e", 64), "sha256:"+strings.Repeat("f", 64), 3*time.Hour, parentDeadline.Add(-2*time.Hour)); err != nil || !deadline.Equal(parentDeadline) {
		t.Fatal("technical work extended its earlier parent deadline", deadline, err)
	}
}

// CLI plans are finite read-only operations; no arbitrary command can enter.
func TestG2InstrumentBasePlanCliHasOnlyFixedBoundedCommands(t *testing.T) {
	settings := sourceSandboxTestSettings()
	owner, image := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	for _, phase := range []string{"identity", "policy", "source-path-vacant", "source-graph"} {
		args, err := g2InstrumentBaseCliArguments(owner, image, phase, settings)
		if err != nil || !slices.Contains(args, "--network=none") || !slices.Contains(args, "--read-only") {
			t.Fatal("fixed technical CLI containment rejected", phase, err)
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--mount=") {
				t.Fatal("technical CLI acquired a host mount")
			}
		}
	}
	if args, err := g2InstrumentBaseCliArguments(owner, image, "arbitrary-command", settings); err == nil || !reflect.DeepEqual(args, []string(nil)) {
		t.Fatal("technical CLI accepted caller-selected execution", err)
	}
}

// The daemon's actual result, not our requested argv, controls CLI admission.
// A positive fixture prevents a reject-everything validator from passing.
func TestG2InstrumentBasePlanCliRejectsChangedActualContainment(t *testing.T) {
	settings := sourceSandboxTestSettings()
	owner, image, id := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64)
	for _, phase := range []string{"identity", "policy", "source-path-vacant", "source-graph"} {
		entrypoint, command := "/opt/urnetwork/bin/sim-latency", []string{"score-policy", "--g2-volume-policy=minimum-work/1"}
		if phase == "identity" {
			entrypoint, command = "/usr/local/libexec/competition-container", []string{"identity"}
		} else if phase == "source-path-vacant" || phase == "source-graph" {
			args, err := g2InstrumentBaseCliArguments(owner, image, phase, settings)
			if err != nil {
				t.Fatal(err)
			}
			entrypoint, command = "/bin/bash", args[len(args)-2:]
		}
		content, err := json.Marshal(map[string]any{
			"Id": id, "Name": "/urnetwork-g2-technical-" + owner + "-" + phase, "Image": image, "State": map[string]any{"Status": "created", "Running": false}, "Mounts": []any{},
			"Config": map[string]any{"User": "65532:65532", "Entrypoint": []string{entrypoint}, "Cmd": command,
				"Labels": map[string]string{"com.urnetwork.instrument-qualification.owner": owner, "com.urnetwork.instrument-qualification.phase": phase}},
			"HostConfig": map[string]any{"Init": true, "ReadonlyRootfs": true, "NetworkMode": "none", "CgroupnsMode": "private", "IpcMode": "private",
				"CgroupParent": settings.CgroupParent, "CpusetCpus": settings.CpuSet, "Memory": settings.MemoryBytes, "MemorySwap": settings.MemoryBytes, "PidsLimit": settings.Pids,
				"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"},
				"Ulimits": []map[string]any{{"Name": "core", "Soft": 0, "Hard": 0}, {"Name": "nofile", "Soft": settings.NoFile, "Hard": settings.NoFile}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateG2InstrumentBaseCliContainment(content, id, owner, image, phase, settings); err != nil {
			t.Fatal("complete technical CLI observation was refused", phase, err)
		}
		var wrongName g2InstrumentBaseCliInspect
		if err := json.Unmarshal(content, &wrongName); err != nil {
			t.Fatal(err)
		}
		wrongName.Name = "/synthetic-unowned-name"
		changedName, err := json.Marshal(wrongName)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateG2InstrumentBaseCliContainment(changedName, id, owner, image, phase, settings); err == nil {
			t.Fatal("technical CLI started a name its cleanup must refuse", phase)
		}
		for _, mutate := range []func(*sourceCorrectnessContainerInspect){
			func(o *sourceCorrectnessContainerInspect) { o.Image = "sha256:" + strings.Repeat("d", 64) },
			func(o *sourceCorrectnessContainerInspect) { o.Config.User = "0:0" },
			func(o *sourceCorrectnessContainerInspect) { o.Config.Cmd = []string{"arbitrary-command"} },
			func(o *sourceCorrectnessContainerInspect) {
				o.Config.Labels["com.urnetwork.instrument-qualification.owner"] = strings.Repeat("e", 64)
			},
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Privileged = true },
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.ReadonlyRootfs = false },
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.NetworkMode = "host" },
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.MemorySwap++ },
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.PidMode = "host" },
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.CapAdd = []string{"SYS_ADMIN"} },
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Ulimits = nil },
			func(o *sourceCorrectnessContainerInspect) { o.HostConfig.Tmpfs = map[string]string{"/synthetic": "rw"} },
			func(o *sourceCorrectnessContainerInspect) {
				if err := json.Unmarshal([]byte(`[{"Type":"volume","Destination":"/synthetic","RW":true}]`), &o.Mounts); err != nil {
					t.Fatal(err)
				}
			},
		} {
			var changed g2InstrumentBaseCliInspect
			if err := json.Unmarshal(content, &changed); err != nil {
				t.Fatal(err)
			}
			mutate(&changed.sourceCorrectnessContainerInspect)
			actual, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateG2InstrumentBaseCliContainment(actual, id, owner, image, phase, settings); err == nil {
				t.Fatal("technical CLI admitted changed actual daemon containment", phase)
			}
		}
	}
}

// The composed runner must reject a name that owned cleanup cannot remove.
// Lost create acknowledgment recovers only its own exact immutable identity.
func TestG2InstrumentBasePlanCliRefusesWrongNameBeforeStartOrCleanup(t *testing.T) {
	settings := sourceSandboxTestSettings()
	owner, image, id := strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64)
	for _, c := range []struct {
		wrongName, lostCreate bool
	}{
		{},
		{wrongName: true},
		{lostCreate: true},
	} {
		name := "/urnetwork-g2-technical-" + owner + "-policy"
		if c.wrongName {
			name = "/synthetic-unowned-name"
		}
		observation, err := json.Marshal(map[string]any{
			"Id": id, "Name": name, "Image": image, "State": map[string]any{"Status": "created", "Running": false}, "Mounts": []any{},
			"Config": map[string]any{"User": "65532:65532", "Entrypoint": []string{"/opt/urnetwork/bin/sim-latency"}, "Cmd": []string{"score-policy", "--g2-volume-policy=minimum-work/1"},
				"Labels": map[string]string{"com.urnetwork.instrument-qualification.owner": owner, "com.urnetwork.instrument-qualification.phase": "policy"}},
			"HostConfig": map[string]any{"Init": true, "ReadonlyRootfs": true, "NetworkMode": "none", "CgroupnsMode": "private", "IpcMode": "private",
				"CgroupParent": settings.CgroupParent, "CpusetCpus": settings.CpuSet, "Memory": settings.MemoryBytes, "MemorySwap": settings.MemoryBytes, "PidsLimit": settings.Pids,
				"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"},
				"Ulimits": []map[string]any{{"Name": "core", "Soft": 0, "Hard": 0}, {"Name": "nofile", "Soft": settings.NoFile, "Hard": settings.NoFile}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		started, removed := false, false
		docker := func(_ context.Context, args []string, out, _ io.Writer) error {
			switch args[0] {
			case "create":
				if c.lostCreate {
					return errors.New("synthetic lost create acknowledgment")
				}
				_, err := io.WriteString(out, id+"\n")
				return err
			case "inspect":
				if args[2] == "{{json .State}}" {
					_, err := io.WriteString(out, `{"Status":"exited","Running":false,"ExitCode":0,"OOMKilled":false}`)
					return err
				}
				_, err := out.Write(observation)
				return err
			case "start":
				started = true
				_, err := io.WriteString(out, `{"score_schema":1,"g2_volume_policy":"minimum-work/1","scorer_version":"sim-latency-score/2"}`)
				return err
			case "rm":
				if !reflect.DeepEqual(args, []string{"rm", "--force", "--volumes", id}) {
					t.Fatal("technical cleanup used a mutable/unowned target", args)
				}
				removed = true
				return nil
			case "container":
				if !removed {
					return errors.New("synthetic container remains")
				}
				return nil
			}
			return errors.New("unexpected synthetic technical Docker operation")
		}
		output, err := runG2InstrumentBaseTechnicalCli(context.Background(), t.TempDir(), owner, image, "policy", settings, docker)
		if (err != nil) != (c.wrongName || c.lostCreate) || started != (!c.wrongName && !c.lostCreate) || removed != !c.wrongName || (!c.wrongName && !c.lostCreate && len(output) == 0) {
			t.Fatal("technical CLI started an uncleanable name, lost owned cleanup or retried creation", c, started, removed, err)
		}
	}
}

// Budget refusal precedes any approval read, state reservation or Docker call.
func TestG2InstrumentBasePlanRejectsLargerTechnicalBudgetBeforeSideEffects(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second, defaultG2InstrumentBaseTechnicalSettings().Timeout + time.Nanosecond} {
		config := defaultG2InstrumentBaseTechnicalSettings()
		config.Timeout = timeout
		called := false
		docker := func(context.Context, []string, io.Writer, io.Writer) error { called = true; return nil }
		if directory, err := executeG2InstrumentBaseTechnicalQualification(context.Background(), config, docker); err == nil || !strings.Contains(err.Error(), "budget") || directory != "" || called {
			t.Fatal("technical budget expanded or reached operational side effects", timeout, directory, err, called)
		}
	}
}
