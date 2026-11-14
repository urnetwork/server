// An opt-in pre-election bridge builds a real qualification-only instrument
// image from the frozen incumbent. It never creates a production source intent,
// election, review receipt, baseline, readiness acknowledgment or activation.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Root seals this credential-free input separately from every production
// profile. The external maintenance owner must retain admission exclusion and
// cancel this operation if its fence/slot is lost; a marker is not authority.
type g2InstrumentBaseTechnicalSettings struct {
	Schema             int                        `json:"schema"`
	Kind               string                     `json:"kind"`
	ApprovalPath       string                     `json:"approval_path"`
	ApprovalFileSha256 string                     `json:"approval_file_sha256"`
	RepositoriesRoot   string                     `json:"repositories_root"`
	Timeout            time.Duration              `json:"timeout"`
	Sandbox            sourceBuildSandboxSettings `json:"sandbox"`
}

// The total operation owns one fixed three-hour budget. Existing phase cleanup
// remains bounded separately; no replacement process can resume this attempt.
func defaultG2InstrumentBaseTechnicalSettings() g2InstrumentBaseTechnicalSettings {
	return g2InstrumentBaseTechnicalSettings{Schema: 1, Kind: "incumbent-instrument-technical-input", Timeout: 3 * time.Hour, Sandbox: defaultSourceBuildSandboxSettings()}
}

// Retain a COPY-only image and full private logs. The tag is technical ownership,
// never an evaluator alias. An existing different tag/base is not overwritten.
func buildG2InstrumentBaseTechnicalImage(ctx context.Context, directory, owner string, approval model.CompetitionTrustedInstrumentApproval, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (string, error) {
	base := approval.InstrumentBaseImageDigest
	alias := "urnetwork/sim-latency-evaluator-base-id:" + strings.TrimPrefix(base, "sha256:")
	inspect := func(reference string) ([]byte, string, error) {
		out, diagnostic := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
		if err := docker(ctx, []string{"image", "inspect", "--format", "{{json .}}", reference}, out, diagnostic); err != nil || out.exceeded || diagnostic.exceeded {
			return nil, "", errors.New("technical image cannot inspect its exact local image")
		}
		var image struct {
			Id, Os, Architecture string
			Config               struct{ OnBuild []string }
		}
		if json.Unmarshal(out.content, &image) != nil || image.Os != "linux" || image.Architecture != "amd64" || len(image.Config.OnBuild) != 0 {
			return nil, "", errors.New("technical image has changed platform or inherited execution hooks")
		}
		return out.content, image.Id, nil
	}
	baseObservation, actual, err := inspect(base)
	if err != nil || actual != base {
		return "", errors.New("technical image incumbent digest is unavailable or substituted")
	}
	if err := writePrivateReviewFile(filepath.Join(directory, "incumbent-image.inspect.json"), baseObservation); err != nil {
		return "", err
	}
	_, actual, aliasErr := inspect(alias)
	if aliasErr != nil || actual != base {
		return "", errors.New("technical image incumbent alias is missing or differs; root must reconcile it without overwriting a shared alias")
	}
	tag := "urnetwork/sim-latency-instrument-qualification:" + owner
	existing := &sourceSupervisorOutput{limit: settings.OutputBytes}
	if err := docker(ctx, []string{"image", "ls", "--all", "--no-trunc", "--quiet", "--filter", "reference=" + tag}, existing, io.Discard); err != nil || existing.exceeded {
		return "", errors.New("technical image tag absence cannot be verified")
	}
	if strings.TrimSpace(string(existing.content)) != "" {
		return "", errors.New("technical image tag already exists; reconcile rather than rebuild or overwrite")
	}
	out, diagnostic := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
	buildErr := docker(ctx, []string{"build", "--pull=false", "--network=none", "--no-cache", "--progress=plain",
		"--label=com.urnetwork.instrument-qualification.owner=" + owner, "--file=" + filepath.Join(directory, "Dockerfile"), "--tag=" + tag, directory}, out, diagnostic)
	for name, content := range map[string][]byte{"image-build.stdout.log": out.content, "image-build.stderr.log": diagnostic.content} {
		if err := writePrivateReviewFile(filepath.Join(directory, name), content); err != nil {
			return "", errors.Join(buildErr, err)
		}
	}
	if buildErr != nil || out.exceeded || diagnostic.exceeded {
		return "", errors.New("technical COPY-only image assembly failed; original attempt and logs retained")
	}
	observation, image, err := inspect(tag)
	if err != nil || !strings.HasPrefix(image, "sha256:") || !validSha256(strings.TrimPrefix(image, "sha256:")) {
		return "", errors.New("technical image has no actual immutable image id")
	}
	if err := writePrivateReviewFile(filepath.Join(directory, "technical-image.inspect.json"), observation); err != nil {
		return "", err
	}
	var observed struct {
		Config struct {
			User, WorkingDir string
			Labels           map[string]string
		}
	}
	if json.Unmarshal(observation, &observed) != nil || observed.Config.User != "65532:65532" ||
		observed.Config.WorkingDir != g2InstrumentBaseTechnicalNamespace(owner)+"/source/server" ||
		observed.Config.Labels["com.urnetwork.competition.image-kind"] != "evaluator-instrument-qualification" ||
		observed.Config.Labels["com.urnetwork.competition.qualification-only"] != "true" ||
		observed.Config.Labels["com.urnetwork.competition.source-epoch"] != "" || observed.Config.Labels["com.urnetwork.competition.source-config-sha256"] != "" ||
		observed.Config.Labels["com.urnetwork.instrument-qualification.owner"] != owner ||
		observed.Config.Labels["com.urnetwork.competition.instrument-approval-sha256"] != approval.ApprovalSha256 {
		return "", errors.New("technical image lost its explicit non-production owner or approval identity")
	}
	return image, nil
}

// Each fixed CLI is observed before start and cleaned by immutable id only.
// Lost create acknowledgment recovers only the exact image/name/owner/phase;
// an uncertain cleanup fails the entire technical attempt and retains evidence.
func runG2InstrumentBaseTechnicalCli(ctx context.Context, directory, owner, image, phase string, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (output []byte, resultErr error) {
	args, err := g2InstrumentBaseCliArguments(owner, image, phase, settings)
	if err != nil {
		return nil, err
	}
	name, id := "urnetwork-g2-technical-"+owner+"-"+phase, ""
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), settings.CleanupTimeout)
		defer cancel()
		reference := id
		if reference == "" {
			reference = name
		}
		out := &sourceSupervisorOutput{limit: settings.OutputBytes}
		if err := docker(cleanupCtx, []string{"inspect", "--format", "{{json .}}", reference}, out, io.Discard); err != nil || out.exceeded {
			resultErr = errors.Join(resultErr, errors.New("technical CLI cleanup identity is uncertain; reconcile owner, do not retry"))
			return
		}
		var observed struct {
			Id, Image, Name string
			Config          struct{ Labels map[string]string }
		}
		if json.Unmarshal(out.content, &observed) != nil || !validSha256(observed.Id) || (id != "" && observed.Id != id) || observed.Image != image || observed.Name != "/"+name ||
			observed.Config.Labels["com.urnetwork.instrument-qualification.owner"] != owner || observed.Config.Labels["com.urnetwork.instrument-qualification.phase"] != phase {
			resultErr = errors.Join(resultErr, errors.New("technical CLI cleanup refused changed ownership"))
			return
		}
		if err := docker(cleanupCtx, []string{"rm", "--force", "--volumes", observed.Id}, io.Discard, io.Discard); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		remaining := &sourceSupervisorOutput{limit: settings.OutputBytes}
		if err := docker(cleanupCtx, []string{"container", "ls", "--all", "--no-trunc", "--quiet", "--filter", "id=" + observed.Id}, remaining, io.Discard); err != nil || remaining.exceeded || strings.TrimSpace(string(remaining.content)) != "" {
			resultErr = errors.Join(resultErr, errors.New("technical CLI cleanup did not prove exact container absence"))
			return
		}
		resultErr = errors.Join(resultErr, writePrivateReviewFile(filepath.Join(directory, phase+".cleanup.json"), []byte(`{"owned_container_removed":true,"residual_container":false}`)))
	}()
	created := &sourceSupervisorOutput{limit: settings.OutputBytes}
	if err := docker(ctx, args, created, io.Discard); err != nil || created.exceeded {
		return nil, errors.New("technical CLI create failed or lost acknowledgment")
	}
	id = strings.TrimSpace(string(created.content))
	if !validSha256(id) {
		id = ""
		return nil, errors.New("technical CLI create returned an invalid identity")
	}
	before := &sourceSupervisorOutput{limit: settings.OutputBytes}
	if err := docker(ctx, []string{"inspect", "--format", "{{json .}}", id}, before, io.Discard); err != nil || before.exceeded {
		return nil, errors.New("technical CLI actual containment is unavailable")
	}
	if err := writePrivateReviewFile(filepath.Join(directory, phase+".before-start.inspect.json"), before.content); err != nil {
		return nil, err
	}
	if err := validateG2InstrumentBaseCliContainment(before.content, id, owner, image, phase, settings); err != nil {
		return nil, err
	}
	out, diagnostic := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
	startErr := docker(ctx, []string{"start", "--attach", id}, out, diagnostic)
	for name, content := range map[string][]byte{phase + ".stdout.json": out.content, phase + ".stderr.log": diagnostic.content} {
		if err := writePrivateReviewFile(filepath.Join(directory, name), content); err != nil {
			return nil, errors.Join(startErr, err)
		}
	}
	terminal := &sourceSupervisorOutput{limit: settings.OutputBytes}
	terminalErr := docker(ctx, []string{"inspect", "--format", "{{json .State}}", id}, terminal, io.Discard)
	if err := writePrivateReviewFile(filepath.Join(directory, phase+".terminal-state.json"), terminal.content); err != nil {
		return nil, errors.Join(startErr, terminalErr, err)
	}
	var state struct {
		Status    string
		Running   bool
		ExitCode  int
		OomKilled bool `json:"OOMKilled"`
	}
	if startErr != nil || terminalErr != nil || out.exceeded || diagnostic.exceeded || terminal.exceeded || json.Unmarshal(terminal.content, &state) != nil ||
		state.Status != "exited" || state.Running || state.ExitCode != 0 || state.OomKilled {
		return nil, errors.New("technical CLI did not complete with a bounded clean terminal observation")
	}
	return out.content, nil
}

// Execute one isolated technical bridge. The only product is the approval's
// frozen incumbent, not a hypothesized future election. Failed contexts/images
// are retained; only exact owned temporary containers are automatically removed.
func executeG2InstrumentBaseTechnicalQualification(ctx context.Context, config g2InstrumentBaseTechnicalSettings, docker sourceBuildDocker) (directory string, resultErr error) {
	if config.Timeout <= 0 || config.Timeout > defaultG2InstrumentBaseTechnicalSettings().Timeout {
		return "", errors.New("technical instrument budget must be positive and no greater than the original three-hour maximum")
	}
	if config.Schema != 1 || config.Kind != "incumbent-instrument-technical-input" || docker == nil || !validSha256(config.ApprovalFileSha256) {
		return "", errors.New("technical instrument configuration is incomplete")
	}
	info, err := os.Lstat(config.ApprovalPath)
	canonical, pathErr := filepath.EvalSymlinks(config.ApprovalPath)
	if err != nil || pathErr != nil || canonical != config.ApprovalPath || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumSourceConfigBytes {
		return "", errors.New("technical instrument approval is missing, redirected or oversized")
	}
	approvalFile, err := os.Open(config.ApprovalPath)
	if err != nil {
		return "", err
	}
	opened, statErr := approvalFile.Stat()
	if statErr != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		_ = approvalFile.Close()
		return "", errors.New("technical instrument approval changed while opening")
	}
	approvalBytes, readErr := io.ReadAll(io.LimitReader(approvalFile, maximumSourceConfigBytes+1))
	closeErr := approvalFile.Close()
	if err := errors.Join(readErr, closeErr); err != nil || len(approvalBytes) > maximumSourceConfigBytes || int64(len(approvalBytes)) != info.Size() {
		return "", errors.New("technical instrument approval changed or exceeded its bounded read")
	}
	approvalHash := sha256.Sum256(approvalBytes)
	if hex.EncodeToString(approvalHash[:]) != config.ApprovalFileSha256 {
		return "", errors.New("technical instrument approval differs from the exact installed input")
	}
	var approval model.CompetitionTrustedInstrumentApproval
	if err := decodeStrictJSONBytes(approvalBytes, &approval, "technical instrument approval"); err != nil {
		return "", err
	}
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(&approval); err != nil {
		return "", err
	}
	suite, err := newSourceCorrectnessSuite(true, approval.WorkCoveragePolicy)
	if err != nil {
		return "", err
	}
	directory, owner, deadline, err := reserveG2InstrumentBaseAttempt(ctx, config.Sandbox.StateDirectory, approval.ApprovalSha256, approval.InstrumentBaseImageDigest, config.Timeout, time.Now().UTC())
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	defer func() {
		if resultErr != nil {
			content, _ := json.Marshal(map[string]any{"schema": 1, "kind": "incumbent-instrument-technical-failure", "owner": owner, "deadline": deadline,
				"passed": false, "runtime_qualified": false, "retry_permitted": false, "private_error": resultErr.Error()})
			resultErr = errors.Join(resultErr, publishSourceTestRecord(filepath.Join(directory, "failure.json"), content))
		}
	}()
	if err := writePrivateReviewFile(filepath.Join(directory, "operator-approval.json"), approvalBytes); err != nil {
		return directory, err
	}
	effectiveSettings, err := json.Marshal(config)
	if err != nil {
		return directory, err
	}
	if err := writePrivateReviewFile(filepath.Join(directory, "effective-settings.json"), effectiveSettings); err != nil {
		return directory, err
	}
	settingsDigest := sha256.Sum256(effectiveSettings)
	if _, err := runG2InstrumentBaseTechnicalCli(ctx, directory, owner, approval.InstrumentBaseImageDigest, "source-path-vacant", config.Sandbox, docker); err != nil {
		return directory, err
	}
	source := filepath.Join(directory, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		return directory, err
	}
	for _, repository := range sourceRepositoryNames() {
		if err := cloneSourceBuildRepository(ctx, repository, filepath.Join(config.RepositoriesRoot, repository), filepath.Join(source, repository), approval.InstrumentBaseCommits[repository]); err != nil {
			return directory, err
		}
	}
	serverRoot, incumbent := filepath.Join(source, "server"), approval.InstrumentBaseCommits["server"]
	tree, err := trustedInstrumentTree(ctx, serverRoot, incumbent, &approval)
	if err != nil {
		return directory, err
	}
	child, err := trustedInstrumentCommit(ctx, serverRoot, incumbent, tree, approval.ApprovalSha256)
	if err != nil {
		return directory, err
	}
	if _, err := sourceGitText(ctx, serverRoot, "checkout", "--quiet", "--detach", child); err != nil {
		return directory, err
	}
	if err := verifyFrozenSourceGraphForMode(ctx, serverRoot, child, false); err != nil {
		return directory, err
	}
	commits := map[string]string{}
	for repository, commit := range approval.InstrumentBaseCommits {
		commits[repository] = commit
	}
	commits["server"] = child
	if err := sealSourceBuildTree(ctx, source, commits, config.Sandbox); err != nil {
		return directory, err
	}
	var metadata map[string][]byte
	var binary []byte
	for _, phase := range []string{"metadata", "check", "compile"} {
		phaseDirectory := filepath.Join(directory, phase)
		if err := os.Mkdir(phaseDirectory, 0o700); err != nil {
			return directory, err
		}
		input := sourceBuildSandboxInput{Owner: owner, Phase: phase, ImageId: approval.InstrumentBaseImageDigest, Commit: child,
			SourceDirectory: source, MetadataDirectory: filepath.Join(phaseDirectory, "metadata"), OutputDirectory: filepath.Join(phaseDirectory, "output")}
		if err := os.Mkdir(input.OutputDirectory, 0o700); err != nil {
			return directory, err
		}
		if phase == "metadata" {
			if err := os.Mkdir(input.MetadataDirectory, 0o700); err != nil {
				return directory, err
			}
		} else if err := writeSourceBuildMetadata(input.MetadataDirectory, metadata); err != nil {
			return directory, err
		}
		out, diagnostic, phaseErr := executeSourceBuildSandbox(ctx, input, config.Sandbox, docker)
		if err := writePrivateReviewFile(filepath.Join(phaseDirectory, "stderr.log"), diagnostic); err != nil {
			return directory, errors.Join(phaseErr, err)
		}
		name := "stdout.log"
		if phase == "metadata" {
			name = "metadata.tar"
		} else if phase == "compile" {
			name = "sim-latency"
		}
		if err := writePrivateReviewFile(filepath.Join(input.OutputDirectory, name), out); err != nil {
			return directory, errors.Join(phaseErr, err)
		}
		if phaseErr != nil {
			return directory, phaseErr
		}
		if phase == "metadata" {
			metadata, err = decodeSourceBuildMetadata(out, config.Sandbox)
			if err != nil {
				return directory, err
			}
		} else if phase == "compile" {
			binary = out
		}
	}
	binaryDigest, err := verifySourceBuildBinary(binary, child)
	if err != nil {
		return directory, err
	}
	scriptDigests := make([]string, 0, 2)
	for _, relative := range []string{"connect/sim-latency/official-run.sh", "connect/sim-latency/evaluator/container/entrypoint.sh"} {
		content, err := os.ReadFile(filepath.Join(serverRoot, filepath.FromSlash(relative)))
		if err != nil || len(content) <= 0 || len(content) > maximumSourceConfigBytes {
			return directory, errors.New("technical image protected script is unavailable or oversized")
		}
		digest := sha256.Sum256(content)
		scriptDigests = append(scriptDigests, hex.EncodeToString(digest[:]))
	}
	plan, err := planG2InstrumentBaseImage(approval, child, tree, binaryDigest, scriptDigests[0], scriptDigests[1])
	if err != nil {
		return directory, err
	}
	for name, content := range map[string][]byte{"Dockerfile": plan.Dockerfile, "source-lock.json": plan.SourceLock, "image-identity.json": plan.Identity, "sim-latency": binary,
		".dockerignore": []byte("**\n!Dockerfile\n!.dockerignore\n!source/\n!source/**\n!sim-latency\n!source-lock.json\n!image-identity.json\n")} {
		if err := writePrivateReviewFile(filepath.Join(directory, name), content); err != nil {
			return directory, err
		}
	}
	image, err := buildG2InstrumentBaseTechnicalImage(ctx, directory, owner, approval, config.Sandbox, docker)
	if err != nil {
		return directory, err
	}
	if _, err := runG2InstrumentBaseTechnicalCli(ctx, directory, owner, image, "source-graph", config.Sandbox, docker); err != nil {
		return directory, err
	}
	correctness := filepath.Join(directory, "correctness")
	if err := os.Mkdir(correctness, 0o700); err != nil {
		return directory, err
	}
	input := sourceBuildSandboxInput{Owner: owner, Phase: "correctness", MinimumWork: true, WorkCoveragePolicy: approval.WorkCoveragePolicy.Clone(), ImageId: image, Commit: child,
		SourceDirectory: source, MetadataDirectory: filepath.Join(correctness, "metadata"), OutputDirectory: filepath.Join(correctness, "output")}
	if err := os.Mkdir(input.OutputDirectory, 0o700); err != nil {
		return directory, err
	}
	if err := writeSourceBuildMetadata(input.MetadataDirectory, metadata); err != nil {
		return directory, err
	}
	observedDocker, err := sourceCorrectnessContainmentDocker(input, config.Sandbox, docker)
	if err != nil {
		return directory, err
	}
	started := time.Now()
	out, diagnostic, executionErr := executeSourceBuildSandbox(ctx, input, config.Sandbox, observedDocker)
	for name, content := range map[string][]byte{"go-test.jsonl": out, "stderr.log": diagnostic} {
		if err := writePrivateReviewFile(filepath.Join(correctness, name), content); err != nil {
			return directory, errors.Join(executionErr, err)
		}
	}
	if executionErr != nil {
		return directory, executionErr
	}
	count, err := countSourceCorrectnessTestEvents(out, suite.requiredTests)
	if err != nil {
		return directory, err
	}
	logs, err := json.Marshal(struct {
		Stdout []byte `json:"stdout"`
		Stderr []byte `json:"stderr"`
	}{Stdout: out, Stderr: diagnostic})
	if err != nil {
		return directory, err
	}
	if err := writePrivateReviewFile(filepath.Join(correctness, "test-logs.json"), logs); err != nil {
		return directory, err
	}
	logsDigest := sha256.Sum256(logs)
	execution := sourceTestExecution{ImageId: image, SuiteSha256: suite.sha256, ExitCode: 0, TestCount: count,
		DurationMillis: time.Since(started).Milliseconds(), TestLogsSha256: hex.EncodeToString(logsDigest[:])}
	containment, err := retainSourceCorrectnessContainment(input, config.Sandbox, execution)
	if err != nil {
		return directory, err
	}
	identity, err := runG2InstrumentBaseTechnicalCli(ctx, directory, owner, image, "identity", config.Sandbox, docker)
	if err != nil {
		return directory, err
	}
	var actualIdentity map[string]any
	var expectedIdentity map[string]any
	if json.Unmarshal(identity, &actualIdentity) != nil || json.Unmarshal(plan.Identity, &expectedIdentity) != nil || !reflect.DeepEqual(actualIdentity, expectedIdentity) {
		return directory, errors.New("technical compiled image returned substituted or production identity")
	}
	policy, err := runG2InstrumentBaseTechnicalCli(ctx, directory, owner, image, g2InstrumentBasePolicyPhase(approval.WorkCoveragePolicy), config.Sandbox, docker)
	if err != nil {
		return directory, err
	}
	if err := validateG2InstrumentBaseScorePolicy(policy, approval.WorkCoveragePolicy); err != nil {
		return directory, err
	}
	if err := ctx.Err(); err != nil {
		return directory, err
	}
	for repository, commit := range commits {
		root := filepath.Join(source, repository)
		head, err := sourceGitText(ctx, root, "rev-parse", "HEAD")
		if err != nil || head != commit {
			return directory, errors.New("technical source commit changed during qualification")
		}
		status, err := sourceGitText(ctx, root, "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil || status != "" {
			return directory, errors.New("technical source no longer has its clean sealed input")
		}
	}
	report, err := json.MarshalIndent(map[string]any{
		"schema": 1, "kind": "incumbent-instrument-technical-prequalification", "scope": "frozen-incumbent-plus-approved-overlay-only",
		"owner": owner, "deadline": deadline, "image_id": image, "simulator_sha256": binaryDigest, "instrumented_server_commit": child,
		"instrumented_server_tree": tree, "approval_sha256": approval.ApprovalSha256, "patch_sha256": approval.PatchSha256,
		"incumbent_source_lock_sha256": approval.InstrumentBaseSourceLockSha256, "execution": execution, "containment_sha256": containment,
		"effective_settings_sha256": hex.EncodeToString(settingsDigest[:]),
		"source_lock_file_sha256":   actualIdentity["source_lock_file_sha256"], "canonical_child_source_lock_sha256": actualIdentity["canonical_child_source_lock_sha256"],
		"source_digest_scope":   "The file SHA authenticates pretty source-lock.json bytes; the separate model canonical child digest is informational technical provenance, not a selected epoch-two/public source identity.",
		"technical_source_path": plan.SourcePath, "incumbent_namespace_absence_proved": true, "built_in_source_graph_cli_passed": true,
		"cli_identity_passed": true, "cli_policy_passed": true, "technical_checks_passed": true, "production_eligible": false,
		"runtime_qualified": false, "source_election_performed": false, "source_intent_created": false, "accepted_source_build_receipt": false,
		"winner_product_baseline_or_readiness": false, "independent_provider_receipt_claimed": false,
		"retention": "Keep this exact image/context/evidence; only authenticated owner-container cleanup is automatic. No image/source/state deletion or production rollback is performed.",
	}, "", "  ")
	if err != nil {
		return directory, err
	}
	if err := publishSourceTestRecord(filepath.Join(directory, "technical-prequalification.json"), append(report, '\n')); err != nil {
		return directory, err
	}
	return directory, nil
}

// Root must first establish durable intake-preserving admission exclusion after
// an authoritative empty poll and retain it through this process and cleanup.
// A newly winning claim is joined before this opt-in test, never interrupted.
func TestG2InstrumentBaseTechnicalImageQualification(t *testing.T) {
	if os.Getenv("SIM_LATENCY_G2_BRIDGE_REAL") != "1" {
		t.Skip("requires explicit durable natural-empty maintenance and exact incumbent technical input")
	}
	if os.Geteuid() != 0 || os.Getenv("SIM_LATENCY_G2_BRIDGE_AUTHORITY") != "ROOT_CONFIRMED_EMPTY_DURABLE_CLAIM_EXCLUSION" {
		t.Fatal("technical image execution requires the root-owned maintained empty-slot exclusion")
	}
	config := defaultG2InstrumentBaseTechnicalSettings()
	path, expected := os.Getenv("SIM_LATENCY_G2_BRIDGE_SETTINGS"), os.Getenv("SIM_LATENCY_G2_BRIDGE_SETTINGS_SHA256")
	content, err := readSourceHostTrustedFile(path, maximumSourceConfigBytes)
	if err != nil || !validSha256(expected) {
		t.Fatal("technical image input must be root-sealed and digest pinned", err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expected || decodeStrictJSONBytes(content, &config, "technical image input") != nil {
		t.Fatal("technical image input differs from its installed pin")
	}
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	directory, err := executeG2InstrumentBaseTechnicalQualification(ctx, config, runSourceBuildDocker)
	if err != nil {
		t.Fatalf("technical instrument attempt failed; reconcile retained evidence without rerun at %s: %v", directory, err)
	}
	t.Logf("Technical image/evidence retained at %s; not production source selection, runtime qualification, baseline or readiness", directory)
}
