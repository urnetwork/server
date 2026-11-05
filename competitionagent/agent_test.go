// Deterministic protocol-level tests exercise real persistence and framing with
// only the systemd bus replaced. No database, Docker or network is involved.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake exposes exact service-management boundaries rather than sleeps.
type agentTestRunner struct {
	stateLock     sync.Mutex
	launches      []Launch
	units         map[string]UnitStatus
	unknownLaunch bool
	stops         int
	beforeStart   func(Launch)
	beforeStop    func()
}

// Durable state must already exist when the service manager first sees launch.
func (self *agentTestRunner) Start(_ context.Context, launch Launch) error {
	self.stateLock.Lock()
	self.launches = append(self.launches, launch)
	unknown, beforeStart := self.unknownLaunch, self.beforeStart
	if !unknown {
		self.units[launch.UnitName] = UnitStatus{Exists: true, Active: true}
	}
	self.stateLock.Unlock()
	if beforeStart != nil {
		beforeStart(launch)
	}
	if unknown {
		return errors.New("synthetic lost launch acknowledgement")
	}
	return nil
}

// Inspection never starts or replaces a service.
func (self *agentTestRunner) Status(_ context.Context, unit string) (UnitStatus, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.units[unit], nil
}

// A test barrier proves cancellation does not acknowledge an unjoined service.
func (self *agentTestRunner) Stop(_ context.Context, unit string) error {
	self.stateLock.Lock()
	self.stops++
	beforeStop := self.beforeStop
	self.stateLock.Unlock()
	if beforeStop != nil {
		beforeStop()
	}
	self.stateLock.Lock()
	self.units[unit] = UnitStatus{Exists: true, Active: false}
	self.stateLock.Unlock()
	return nil
}

// Reads test service admission without racing its control goroutine.
func (self *agentTestRunner) launchCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.launches)
}

// Synthetic private fixtures never contain production identities or secrets.
type agentTestFixture struct {
	agent          *Agent
	config         *Config
	settings       AgentSettings
	runner         *agentTestRunner
	cleaner        *agentTestCleaner
	containment    *agentTestContainment
	coordinatorKey ed25519.PrivateKey
	agentPublicKey ed25519.PublicKey
	manifest       StartManifest
	inputDirectory string
	now            time.Time
}

// Produces an inert evaluator and an independent cleanup checker in a temp root.
func newAgentTestFixture(t *testing.T) *agentTestFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDirectory, workDirectory := filepath.Join(root, "state"), filepath.Join(root, "work")
	scratchDirectory, clientsDirectory := filepath.Join(root, "scratch"), filepath.Join(root, "clients")
	inputDirectory := filepath.Join(root, "inputs")
	configDirectory, vaultDirectory := filepath.Join(root, "config", "local"), filepath.Join(root, "vault", "local")
	for _, directory := range []string{stateDirectory, workDirectory, scratchDirectory, clientsDirectory, inputDirectory, configDirectory, vaultDirectory} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, coordinatorKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentPublicKey, agentKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(stateDirectory, "agent-seed")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(agentKey.Seed())), 0600); err != nil {
		t.Fatal(err)
	}
	program := "#!/bin/sh\nset -eu\nresult=$4\nprintf '%s\\n' '{\"schema\":1,\"job_id\":\"11111111-1111-4111-8111-111111111111\",\"security\":{\"cleanup_complete\":true}}' > \"$result\"\nprintf '%s\\n' once > process-invocations.txt\nenv > process-environment.txt\n"
	evaluatorPath, selfCheckPath, agentPath := filepath.Join(root, "evaluator"), filepath.Join(root, "self-check"), filepath.Join(root, "agent")
	builderPath, formatterPath, ownershipPath := filepath.Join(root, "builder-identity"), filepath.Join(root, "json-formatter"), filepath.Join(root, "subevaluation-ownership.sh")
	if err := os.WriteFile(evaluatorPath, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selfCheckPath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"cleanup_verified\":true}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	builderSha256 := strings.Repeat("e", 64)
	if err := os.WriteFile(ownershipPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ownershipPin := testAgentPin(t, ownershipPath)
	if err := os.WriteFile(builderPath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schema\":1,\"kind\":\"sim-latency-submission-builder/v2\",\"builder_sha256\":\""+builderSha256+"\",\"subevaluation_ownership_sha256\":\""+ownershipPin.Sha256+"\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formatterPath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"allowed_paths\":[\"server/example.go\"],\"forbidden_paths\":[\"server/connect/sim-latency/**\"],\"max_patch_bytes\":1024}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	resourcePath := filepath.Join(root, "resource-policy")
	if err := os.WriteFile(resourcePath, []byte("#!/bin/sh\nprintf '%s\\n' '"+testRunResourceIdentityJson(t)+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	boundaryPath := filepath.Join(root, "resource-boundary.json")
	if err := os.WriteFile(boundaryPath, []byte(`{"schema":1,"container_root_uid":200000,"container_root_gid":200000,"container_id_map_length":65536}`), 0400); err != nil {
		t.Fatal(err)
	}
	fixture := &agentTestFixture{inputDirectory: inputDirectory, coordinatorKey: coordinatorKey, agentPublicKey: agentPublicKey,
		runner: &agentTestRunner{units: map[string]UnitStatus{}}, now: time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)}
	fixture.config = &Config{Schema: Schema, HostId: "synthetic-executor", BaseDirectory: root, TrustedOperatorUser: "synthetic-operator", ExpectedDataDeviceId: 1, StateDirectory: stateDirectory, WorkDirectory: workDirectory, WorkerUser: "synthetic-worker",
		Evaluator: testAgentPin(t, evaluatorPath), SelfCheck: testAgentPin(t, selfCheckPath), Agent: testAgentPin(t, agentPath),
		Docker: testAgentPin(t, agentPath), DockerSocketPath: filepath.Join(root, "engine", "docker.sock"), SubmissionBuilderIdentity: testAgentPin(t, builderPath),
		DockerRole: DockerRoleConfig{ScratchDirectory: scratchDirectory, Compose: testAgentPin(t, agentPath), Buildx: testAgentPin(t, agentPath), Dockerfile: testAgentPin(t, agentPath),
			PostgresInitialization: testAgentPin(t, agentPath), ResourcePolicy: testAgentPin(t, resourcePath), ResourceBoundary: testAgentPin(t, boundaryPath)},
		SubmissionBuilderSha256: builderSha256, SubevaluationOwnership: ownershipPin, JsonFormatter: testAgentPin(t, formatterPath),
		ConfigLocalDirectory: configDirectory, VaultLocalDirectory: vaultDirectory, ConfigLocalSha256: strings.Repeat("1", 64), VaultLocalSha256: strings.Repeat("2", 64),
		EvaluatorImageDigest: "sha256:" + strings.Repeat("3", 64), SourceLockSha256: strings.Repeat("4", 64), QualificationSha256: strings.Repeat("5", 64),
		CoordinatorPublicKeyBase64: base64.StdEncoding.EncodeToString(coordinatorKey.Public().(ed25519.PublicKey)), SigningKeyPath: keyPath, Limits: DefaultLimits(), configPath: filepath.Join(root, "agent.json")}
	fixture.settings = DefaultAgentSettings()
	fixture.settings.Now = func() time.Time { return fixture.now }
	fixture.agent, err = newAgent(fixture.config, fixture.runner, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	fixture.cleaner = &agentTestCleaner{}
	fixture.agent.cleaner = fixture.cleaner
	fixture.containment = &agentTestContainment{}
	fixture.agent.containment = fixture.containment
	contents := map[string][]byte{"canonical.patch": {}, "providers.yml": []byte("synthetic-workload\n"), "round-baseline.json": []byte("{\"synthetic_baseline\":true}\n")}
	files := []File{}
	for _, name := range []string{"canonical.patch", "providers.yml", "round-baseline.json"} {
		content := contents[name]
		if err := os.WriteFile(filepath.Join(inputDirectory, name), content, 0400); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		files = append(files, File{Path: name, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))})
	}
	identity := Identity{HostId: fixture.config.HostId, JobId: "11111111-1111-4111-8111-111111111111", RoundId: "22222222-2222-4222-8222-222222222222", RoundEpoch: 2, SourceEpoch: 0, Cycle: 0, Attempt: 1, StartedAt: fixture.now, DeadlineAt: fixture.now.Add(3 * time.Hour)}
	identity.RunId = RunId(identity.JobId, identity.Cycle, identity.Attempt)
	policy, err := json.Marshal(map[string]any{"host_qualification_sha256": fixture.config.QualificationSha256, "config_local_sha256": fixture.config.ConfigLocalSha256,
		"vault_local_sha256": fixture.config.VaultLocalSha256, "score_timeout_seconds": 10800, "g2_volume_policy": "minimum-work/1"})
	if err != nil {
		t.Fatal(err)
	}
	request := localEvaluatorRequest{Schema: 1, JobId: identity.JobId, RoundId: identity.RoundId, SourceEpoch: identity.SourceEpoch, Attempt: identity.Attempt,
		CompetitionId: "synthetic-competition", BaseSha: strings.Repeat("a", 40), EvaluatorImageDigest: fixture.config.EvaluatorImageDigest,
		ApiImageDigest: "sha256:" + strings.Repeat("b", 64), WorkerImageDigest: "sha256:" + strings.Repeat("c", 64), ScorerVersion: "sim-latency-score/2", RoundSeedHex: strings.Repeat("d", 64),
		ProvidersPath: "providers.yml", ProvidersSha256: files[1].Sha256, RoundBaselinePath: "round-baseline.json", RoundBaselineSha256: files[2].Sha256,
		PatchPath: "canonical.patch", PatchSha256: files[0].Sha256, ArtifactDirectory: ".", ConfigLocalDirectory: "@config-local", VaultLocalDirectory: "@vault-local",
		PatchPolicy: json.RawMessage(`{"max_patch_bytes":1024,"allowed_paths":["server/example.go"],"forbidden_paths":["server/connect/sim-latency/**"]}`), EvaluationPolicy: policy}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifest = StartManifest{Identity: identity, EvaluatorImageDigest: fixture.config.EvaluatorImageDigest, EvaluatorCommandSha256: fixture.config.Evaluator.Sha256,
		SourceLockSha256: fixture.config.SourceLockSha256, QualificationSha256: fixture.config.QualificationSha256, ConfigLocalSha256: fixture.config.ConfigLocalSha256,
		VaultLocalSha256: fixture.config.VaultLocalSha256, RoundBaselineSha256: files[2].Sha256, EvaluatorRequest: requestBytes, Inputs: files}
	return fixture
}

// Hashes test-owned executable bytes through the same content identity.
func testAgentPin(t *testing.T, path string) Pin {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return Pin{Path: path, Sha256: hex.EncodeToString(digest[:])}
}

// Every ordinary call exercises signed request/response framing and file bodies.
func (self *agentTestFixture) call(t *testing.T, operation string, fence int64, manifest *StartManifest) (Response, error) {
	t.Helper()
	request := Request{Schema: Schema, Operation: operation, HostId: self.config.HostId, RunId: self.manifest.Identity.RunId, FencingToken: fence, Start: manifest}
	if manifest != nil {
		request.RunId = manifest.Identity.RunId
	}
	if operation == "check" || operation == "availability" {
		request.RunId, request.FencingToken = "", 0
	}
	if err := SignRequest(&request, self.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	input := &bytes.Buffer{}
	if err := WriteDocument(input, request, self.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if manifest != nil {
		if err := WriteFiles(input, self.inputDirectory, manifest.Inputs, self.config.Limits.InputBytes, self.config.Limits); err != nil {
			t.Fatal(err)
		}
	}
	output := &bytes.Buffer{}
	if err := self.agent.Serve(context.Background(), input, output); err != nil {
		return Response{}, err
	}
	response := Response{}
	if err := ReadDocument(output, &response, self.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResponse(response, self.agentPublicKey); err != nil {
		t.Fatal(err)
	}
	return response, nil
}

// A different synthetic job or attempt retains all exact named input bytes.
func (self *agentTestFixture) anotherManifest(t *testing.T, jobId string, attempt int) StartManifest {
	t.Helper()
	if jobId == "synthetic-job" {
		jobId = self.manifest.Identity.JobId
	} else {
		digest := sha256.Sum256([]byte(jobId))
		jobId = fmt.Sprintf("%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
	}
	manifest := self.manifest
	manifest.Identity.JobId, manifest.Identity.Attempt = jobId, attempt
	manifest.Identity.RunId = RunId(jobId, manifest.Identity.Cycle, attempt)
	request, err := decodeEvaluatorRequest(manifest.EvaluatorRequest)
	if err != nil {
		t.Fatal(err)
	}
	request.JobId, request.Attempt = jobId, attempt
	manifest.EvaluatorRequest, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// A later competition epoch may carry the same reviewed source generation.
// Round admission and measured source identity must never be conflated.
func TestAgentAdmitsEpochTwoAndLaterCarriedSourceGenerations(t *testing.T) {
	for _, epochs := range []struct{ round, source int }{{round: 2, source: 0}, {round: 2, source: 1}, {round: 6, source: 1}} {
		fixture := newAgentTestFixture(t)
		manifest := fixture.manifest
		manifest.Identity.RoundEpoch, manifest.Identity.SourceEpoch = epochs.round, epochs.source
		request, err := decodeEvaluatorRequest(manifest.EvaluatorRequest)
		if err != nil {
			t.Fatal(err)
		}
		request.SourceEpoch = epochs.source
		manifest.EvaluatorRequest, err = json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.call(t, "start", 1, &manifest); err != nil || fixture.runner.launchCount() != 1 {
			t.Fatalf("round %d carrying source %d was rejected: %v", epochs.round, epochs.source, err)
		}
	}
}

// Distributed execution cannot be retroactively enabled for epoch one even
// when its source generation happens to look like a later generation number.
func TestAgentRejectsEpochOneDespiteLaterSourceGeneration(t *testing.T) {
	fixture := newAgentTestFixture(t)
	manifest := fixture.manifest
	manifest.Identity.RoundEpoch, manifest.Identity.SourceEpoch = 1, 2
	if _, err := fixture.call(t, "start", 1, &manifest); err == nil || fixture.runner.launchCount() != 0 {
		t.Fatal("epoch-one work reached the distributed execution role")
	}
}

// A dropped SSH acknowledgement and a fresh process cannot repeat dispatch.
func TestAgentStartCommitsBeforeSystemdAndReattaches(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.runner.beforeStart = func(launch Launch) {
		state, err := fixture.agent.loadState(launch.RunId)
		if err != nil || !state.LaunchAttempted || state.ExecutionStarted {
			t.Fatalf("launch preceded durable custody: %+v %v", state, err)
		}
		slot, err := fixture.agent.loadSlot()
		if err != nil || slot.RunId != launch.RunId {
			t.Fatalf("launch preceded durable slot: %+v %v", slot, err)
		}
		if !launch.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) {
			t.Fatal("launcher received a new execution clock")
		}
	}
	response, err := fixture.call(t, "start", 1, &fixture.manifest)
	if err != nil || response.State != "accepted" || fixture.runner.launchCount() != 1 {
		t.Fatalf("initial start: %+v %v", response, err)
	}
	fixture.agent, err = newAgent(fixture.config, fixture.runner, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	response, err = fixture.call(t, "start", 7, &fixture.manifest)
	if err != nil || response.FencingToken != 7 || fixture.runner.launchCount() != 1 || response.Identity == nil || !response.Identity.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) {
		t.Fatalf("reattachment repeated dispatch or changed identity: %+v %v", response, err)
	}
}

// Launch uncertainty stays attached to the first input and absolute deadline.
func TestAgentUncertainLaunchNeverRepeatsOrResetsClock(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.runner.unknownLaunch = true
	response, err := fixture.call(t, "start", 1, &fixture.manifest)
	if err != nil || response.State != "unknown" {
		t.Fatalf("lost launch was guessed: %+v %v", response, err)
	}
	fixture.now = fixture.now.Add(time.Hour)
	response, err = fixture.call(t, "start", 2, &fixture.manifest)
	if err != nil || response.State != "unknown" || fixture.runner.launchCount() != 1 {
		t.Fatalf("uncertain start replayed: %+v %v", response, err)
	}
	changed := fixture.manifest
	changed.Identity.StartedAt = changed.Identity.StartedAt.Add(time.Minute)
	changed.Identity.DeadlineAt = changed.Identity.DeadlineAt.Add(time.Minute)
	if _, err := fixture.call(t, "start", 3, &changed); err == nil {
		t.Fatal("same execution accepted a reset clock")
	}
	if fixture.runner.launchCount() != 1 {
		t.Fatal("conflicting reconnect launched another process")
	}
}

// A stale callback cannot take back custody even when the SSH identity is reused.
func TestAgentRejectsStaleFencesBeforeCancellation(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.call(t, "status", 9, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.call(t, "cancel", 8, nil); err == nil {
		t.Fatal("stale cancellation took newer custody")
	}
	if _, err := fixture.call(t, "start", 8, &fixture.manifest); err == nil {
		t.Fatal("stale dispatch took newer custody")
	}
	if fixture.runner.stops != 0 || fixture.runner.launchCount() != 1 {
		t.Fatal("stale request reached service manager")
	}
}

// Failed transfer has no execution and cannot strand the physical slot.
func TestAgentTruncatedUploadDoesNotReserveSlot(t *testing.T) {
	fixture := newAgentTestFixture(t)
	request := Request{Schema: Schema, Operation: "start", HostId: fixture.config.HostId, RunId: fixture.manifest.Identity.RunId, FencingToken: 1, Start: &fixture.manifest}
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	input := &bytes.Buffer{}
	if err := WriteDocument(input, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Serve(context.Background(), input, &bytes.Buffer{}); err == nil {
		t.Fatal("truncated workload was accepted")
	}
	if _, err := fixture.agent.loadSlot(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed upload reserved custody: %v", err)
	}
	if fixture.runner.launchCount() != 0 {
		t.Fatal("incomplete input launched execution")
	}
	if _, err := fixture.call(t, "start", 2, &fixture.manifest); err != nil {
		t.Fatalf("valid retry after incomplete transfer: %v", err)
	}
}

// Actual evaluator execution is one-shot; signed artifacts contain no private
// agent state and inherited service credentials are removed before exec.
func TestAgentExecuteOnceAuthenticatesArtifactsAndStripsEnvironment(t *testing.T) {
	fixture := newAgentTestFixture(t)
	t.Setenv("PGPASSWORD", "synthetic-not-forwarded")
	t.Setenv("SSH_AUTH_SOCK", "/synthetic/credential-agent.sock")
	t.Setenv("VAULT_PATH", "/synthetic/vault/main")
	t.Setenv("URNETWORK_SUBEVALUATION_RUN_ID", "synthetic-malicious-scope")
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Execute(context.Background(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	fixture.joinStoppedRun(t)
	if err := fixture.agent.Execute(context.Background(), fixture.manifest.Identity.RunId); err == nil {
		t.Fatal("persisted execution was run twice")
	}
	response, err := fixture.call(t, "status", 2, nil)
	if err != nil || response.State != "succeeded" || len(response.Files) == 0 {
		t.Fatalf("terminal execution not retained: %+v %v", response, err)
	}
	for _, file := range response.Files {
		if file.Path == "manifest.json" || file.Path == "state.json" || file.Path == "agent-seed" {
			t.Fatalf("private durable authority leaked: %s", file.Path)
		}
	}
	environment, err := os.ReadFile(filepath.Join(fixture.agent.workDirectory(fixture.manifest.Identity.RunId), "process-environment.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PGPASSWORD", "SSH_AUTH_SOCK", "VAULT_PATH", "synthetic-not-forwarded"} {
		if bytes.Contains(environment, []byte(secret)) {
			t.Fatalf("ambient service credential crossed exec boundary: %s", secret)
		}
	}
	for _, scope := range subevaluationScopeEnvironment(fixture.manifest.Identity) {
		if !bytes.Contains(environment, []byte(scope+"\n")) {
			t.Fatal("trusted evaluator did not receive its exact authenticated prospective ownership scope")
		}
	}
	if bytes.Contains(environment, []byte("synthetic-malicious-scope")) {
		t.Fatal("caller-selected ownership environment crossed the authenticated run boundary")
	}
	request := Request{Schema: Schema, Operation: "artifacts", HostId: fixture.config.HostId, RunId: fixture.manifest.Identity.RunId, FencingToken: 2}
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	input, output := &bytes.Buffer{}, &bytes.Buffer{}
	if err := WriteDocument(input, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Serve(context.Background(), input, output); err != nil {
		t.Fatal(err)
	}
	if err := ReadDocument(output, &response, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResponse(response, fixture.agentPublicKey); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.Chmod(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ReadFiles(output, destination, response.Files, fixture.config.Limits.ArtifactBytes, fixture.config.Limits); err != nil {
		t.Fatal(err)
	}
	requestBytes, err := os.ReadFile(filepath.Join(destination, "worker-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var localRequest localEvaluatorRequest
	if err := json.Unmarshal(requestBytes, &localRequest); err != nil {
		t.Fatal(err)
	}
	if localRequest.ArtifactDirectory != fixture.agent.workDirectory(fixture.manifest.Identity.RunId) || localRequest.ConfigLocalDirectory != fixture.config.ConfigLocalDirectory {
		t.Fatal("agent did not replace logical paths with its restricted local role")
	}
	if output.Len() != 0 {
		t.Fatal("artifact body has undeclared trailing bytes")
	}
}

// Cancellation is not acknowledged while the service manager still owns work.
func TestAgentCancelWaitsForStopAndPreventsLateExecution(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	stopEntered, releaseStop := make(chan struct{}), make(chan struct{})
	fixture.runner.beforeStop = func() { close(stopEntered); <-releaseStop }
	type cancelResult struct {
		response Response
		err      error
	}
	done := make(chan cancelResult, 1)
	go func() {
		response, err := fixture.call(t, "cancel", 3, nil)
		done <- cancelResult{response: response, err: err}
	}()
	<-stopEntered
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !state.CancelRequested || state.FencingToken != 3 {
		t.Fatalf("stop preceded durable cancellation: %+v %v", state, err)
	}
	select {
	case result := <-done:
		t.Fatalf("cancel acknowledged before stop joined: %+v", result)
	default:
	}
	close(releaseStop)
	result := <-done
	if result.err != nil || result.response.State != "canceled" {
		t.Fatalf("joined cancellation: %+v %v", result.response, result.err)
	}
	if err := fixture.agent.Execute(context.Background(), fixture.manifest.Identity.RunId); err == nil {
		t.Fatal("late service entry executed a canceled run")
	}
}

// Unknown work remains on its original host until its original budget ends.
func TestAgentUnknownRunRetainsSlotAndExpiresWithoutReplay(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.runner.unknownLaunch = true
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	other := fixture.anotherManifest(t, "synthetic-other-job", 1)
	if _, err := fixture.call(t, "start", 2, &other); err == nil {
		t.Fatal("another execution overlapped unknown custody")
	}
	fixture.now = fixture.manifest.Identity.DeadlineAt.Add(time.Nanosecond)
	response, err := fixture.call(t, "status", 4, nil)
	if err != nil || response.State != "expired" || fixture.runner.launchCount() != 1 {
		t.Fatalf("expiry guessed a new launch: %+v %v", response, err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || state.ExecutionStarted || !state.CleanupVerified {
		t.Fatalf("expired unknown work lacks bounded cleanup proof: %+v %v", state, err)
	}
}

// The per-job/cycle ledger survives known terminal attempts and intervening work.
func TestAgentLaterAttemptCannotResetSubmissionClock(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Execute(context.Background(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	fixture.joinStoppedRun(t)
	later := fixture.anotherManifest(t, "synthetic-job", 2)
	later.Identity.StartedAt = later.Identity.StartedAt.Add(time.Minute)
	later.Identity.DeadlineAt = later.Identity.DeadlineAt.Add(time.Minute)
	fixture.now = fixture.now.Add(time.Minute)
	if _, err := fixture.call(t, "start", 2, &later); err == nil {
		t.Fatal("later attempt received a fresh job clock")
	}
	if fixture.runner.launchCount() != 1 {
		t.Fatal("clock reset reached the service manager")
	}
}

// Changed hidden request bytes cannot execute even if all public inputs match.
func TestAgentReauthenticatesMaterializedHiddenRequest(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.agent.workDirectory(fixture.manifest.Identity.RunId), "worker-request.json")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"synthetic_corruption\":true}"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Execute(context.Background(), fixture.manifest.Identity.RunId); err == nil {
		t.Fatal("changed hidden request was executed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "process-invocations.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt request reached evaluator")
	}
}

// Closed operations and independent signatures are checked before mutation.
func TestAgentRejectsUnsignedAndArbitraryOperations(t *testing.T) {
	fixture := newAgentTestFixture(t)
	for _, operation := range []string{"shell", "start"} {
		request := Request{Schema: Schema, Operation: operation, HostId: fixture.config.HostId, RunId: fixture.manifest.Identity.RunId, FencingToken: 1, Start: &fixture.manifest}
		if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
			t.Fatal(err)
		}
		if operation == "start" {
			request.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		}
		input := &bytes.Buffer{}
		if err := WriteDocument(input, request, fixture.config.Limits.DocumentBytes); err != nil {
			t.Fatal(err)
		}
		if err := fixture.agent.Serve(context.Background(), input, io.Discard); err == nil {
			t.Fatalf("accepted restricted operation %s", operation)
		}
	}
	if fixture.runner.launchCount() != 0 {
		t.Fatal("unauthorized request reached service manager")
	}
}

// io.Copy must not discover a promoted unbounded bytes.Buffer.ReadFrom method.
func TestAgentDiagnosticCopyCannotBypassWriteBound(t *testing.T) {
	output := &boundedOutput{maximum: 8}
	count, err := io.Copy(output, &agentTestReadOnly{reader: strings.NewReader(strings.Repeat("x", 100))})
	if err != nil || count != 100 || len(output.Bytes()) != 8 || !output.exceeded {
		t.Fatalf("diagnostic bound bypassed: count=%d retained=%d exceeded=%t error=%v", count, len(output.Bytes()), output.exceeded, err)
	}
}

// Only Read is exposed so io.Copy tests the destination's optimized interfaces.
type agentTestReadOnly struct{ reader io.Reader }

// The wrapper deliberately does not expose ReaderFrom or WriterTo.
func (self *agentTestReadOnly) Read(content []byte) (int, error) { return self.reader.Read(content) }

// A root-filesystem fallback and arbitrary runtime-root escapes fail closed.
func TestAgentRuntimeMountAndBaseFailClosed(t *testing.T) {
	fixture := newAgentTestFixture(t)
	config := *fixture.config
	config.WorkDirectory = filepath.Join(t.TempDir(), "outside-jobs")
	if err := config.validate(false); err == nil {
		t.Fatal("runtime workspace escaped the explicit base")
	}
	config = *fixture.config
	config.BaseDirectory = "/"
	config.ExpectedDataDeviceId = 1
	if err := config.requireDataMount(); err == nil {
		t.Fatal("root filesystem accepted as the pinned data mount")
	}
}

// Neither symlinked evidence nor unbounded directory counts become artifacts.
func TestAgentArtifactInventoryRejectsLinksAndExcessEntries(t *testing.T) {
	fixture := newAgentTestFixture(t)
	root := t.TempDir()
	if err := os.Symlink(fixture.config.StateDirectory, filepath.Join(root, "synthetic-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agent.collectFiles(root); err == nil {
		t.Fatal("artifact inventory followed a link to private authority")
	}
	root = t.TempDir()
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("synthetic"), 0400); err != nil {
			t.Fatal(err)
		}
	}
	fixture.agent.config.Limits.FileCount = 1
	if _, err := fixture.agent.collectFiles(root); err == nil {
		t.Fatal("artifact inventory examined unbounded entries")
	}
}
