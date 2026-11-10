//go:build linux

// Durable request-owner regressions use only private files and fake daemons.
// Barriers force delayed responses and disconnects without timing assertions.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The production plan validator remains enabled; only kernel root ownership
// and the pinned real CLI are replaced by test-private equivalents.
type dockerProxyTest struct {
	proxy        *DockerProxy
	plan         DockerProxyPlan
	settings     DockerProxySettings
	buildRequest DockerBuildRequest
	create       []byte
}

// Context inputs are the same finite document family the trusted builder uses.
func newDockerProxyTest(t *testing.T) *dockerProxyTest {
	t.Helper()
	scope := newHostCleanupTest(t)
	root, err := os.MkdirTemp("/tmp", "dp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	contextRoot := filepath.Join(root, "jobs", scope.plan.RunId)
	contextDirectory := filepath.Join(contextRoot, "build-input")
	if err := os.MkdirAll(contextDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDirectory, "canonical.patch"), []byte("synthetic sealed input\n"), 0400); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(root, "Dockerfile.submission")
	if err := os.WriteFile(dockerfile, []byte("ARG EVALUATOR_BASE_IMAGE\nFROM ${EVALUATOR_BASE_IMAGE}\n"), 0400); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	stockProfile, coreProfile := testCandidateCoreProfilePins(t, root)
	workerSocket, buildSocket, err := dockerRunSocketPaths(filepath.Join(root, "clients"), scope.plan.RunId)
	if err != nil {
		t.Fatal(err)
	}
	plan := DockerProxyPlan{Schema: Schema, RunId: scope.plan.RunId, JobId: scope.plan.JobId, RoundId: scope.plan.RoundId, Attempt: scope.plan.Attempt,
		DeadlineAt: time.Now().Add(time.Hour), BaseDirectory: root, StateDirectory: filepath.Join(root, "state", scope.plan.RunId, "docker-owner"), ScratchDirectory: filepath.Join(root, "scratch", scope.plan.RunId), SocketPath: workerSocket,
		BuildSocketPath: buildSocket, ContextRoot: contextRoot, UpstreamSocketPath: filepath.Join(root, "engine", "docker.sock"),
		Docker: Pin{Path: filepath.Join(root, "docker"), Sha256: strings.Repeat("d", 64)}, Buildx: Pin{Path: filepath.Join(root, "docker-buildx"), Sha256: strings.Repeat("e", 64)},
		Dockerfile: Pin{Path: dockerfile, Sha256: hex.EncodeToString(digest[:])}, BaseImageDigest: "sha256:" + strings.Repeat("3", 64),
		StockSeccompProfile: stockProfile, CandidateCoreProfile: coreProfile,
		BackingImages: []string{"postgres:synthetic@sha256:" + strings.Repeat("a", 64), "redis:synthetic@sha256:" + strings.Repeat("b", 64)},
		CgroupNames:   append([]string(nil), scope.plan.CgroupNames...), BuildCgroupName: scope.plan.CgroupNames[0], CpuSet: "2,4", ManagementCpuSet: "0", BuildMemory: "5g", WorkerUid: 12345, ResourcePolicySha256: strings.Repeat("f", 64)}
	settings := DefaultDockerProxySettings()
	proxy, err := newDockerProxy(plan, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	profileContent, err := ValidateCandidateCoreProfiles(stockProfile, coreProfile, settings.CoreProfile, false)
	if err != nil {
		t.Fatal(err)
	}
	create, err := json.Marshal(map[string]any{"Image": plan.BaseImageDigest, "User": "65532:65532", "Cmd": []string{"identity"},
		"AttachStdout": true, "AttachStderr": true, "Labels": map[string]string{cleanupJobLabel: plan.JobId, cleanupRoundLabel: plan.RoundId, dockerProxyRunLabel: plan.RunId},
		"HostConfig": map[string]any{"CgroupParent": plan.CgroupNames[len(plan.CgroupNames)-1], "Privileged": false, "ReadonlyRootfs": true,
			"CapDrop": []string{"ALL"}, "CapAdd": nil, "SecurityOpt": []string{"no-new-privileges:true", "seccomp=" + string(profileContent)}, "NetworkMode": "none", "CgroupnsMode": "private",
			"Memory": int64(4 * 1024 * 1024 * 1024), "MemorySwap": int64(4 * 1024 * 1024 * 1024), "PidsLimit": 1024, "CpusetCpus": plan.ManagementCpuSet,
			"Ulimits": []map[string]any{{"Name": "core", "Soft": 1, "Hard": 1}}, "ShmSize": 64 * 1024 * 1024,
			"Binds": nil, "PortBindings": map[string]any{}, "PublishAllPorts": false, "MaskedPaths": nil, "ReadonlyPaths": nil,
			"LogConfig": map[string]any{"Type": "", "Config": map[string]string{}}, "RestartPolicy": map[string]any{"Name": "no", "MaximumRetryCount": 0},
			"AutoRemove": true, "CpuShares": 0, "NanoCpus": 0, "Devices": nil, "DeviceRequests": nil, "VolumeDriver": "", "VolumesFrom": nil}})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"build", "--platform", "linux/amd64", "--provenance=false", "--no-cache", "--pull=false", "--network", "none",
		"--cgroup-parent", plan.BuildCgroupName, "--resource", "cpuset-cpus=" + plan.CpuSet, "--resource", "memory=" + plan.BuildMemory, "--resource", "memory-swap=" + plan.BuildMemory,
		"--file", plan.Dockerfile.Path}
	buildArgs := map[string]string{"EVALUATOR_BASE_IMAGE": "urnetwork/sim-latency-evaluator-base-id:" + strings.TrimPrefix(plan.BaseImageDigest, "sha256:"), "BASE_SHA": strings.Repeat("a", 40), "CANDIDATE_SHA": strings.Repeat("c", 40),
		"PATCH_SHA256": strings.Repeat("1", 64), "POLICY_SHA256": strings.Repeat("2", 64), "BUILDER_SHA256": strings.Repeat("4", 64), "PATCH_VALIDATOR_SHA256": strings.Repeat("5", 64),
		"CANDIDATE_COMMITS_SHA256": strings.Repeat("6", 64), "ORIGINAL_SOURCE_IDENTITY_SHA256": strings.Repeat("7", 64), "CANDIDATE_SOURCE_IDENTITY_SHA256": strings.Repeat("8", 64), "SUBMISSION_IDENTITY": strings.Repeat("b", 64)}
	for _, name := range []string{"EVALUATOR_BASE_IMAGE", "BASE_SHA", "CANDIDATE_SHA", "PATCH_SHA256", "POLICY_SHA256", "BUILDER_SHA256", "PATCH_VALIDATOR_SHA256", "CANDIDATE_COMMITS_SHA256", "ORIGINAL_SOURCE_IDENTITY_SHA256", "CANDIDATE_SOURCE_IDENTITY_SHA256", "SUBMISSION_IDENTITY"} {
		args = append(args, "--build-arg", name+"="+buildArgs[name])
	}
	args = append(args, "--tag", "urnetwork/sim-latency-submission:"+strings.Repeat("b", 32), contextDirectory)
	return &dockerProxyTest{proxy: proxy, plan: plan, settings: settings, create: create,
		buildRequest: DockerBuildRequest{RunId: plan.RunId, Arguments: args, ContextDirectory: contextDirectory}}
}

// Fake transport substitution cannot change the production plan's endpoint.
func (self *dockerProxyTest) daemon(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	self.proxy.dialDaemon = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	return server
}

// A client which disappears after headers must not cancel accepted daemon work.
type dockerProxyBrokenWriter struct {
	header  http.Header
	entered chan struct{}
	once    sync.Once
}

// Header mutation remains local even after delivery has failed.
func (self *dockerProxyBrokenWriter) Header() http.Header { return self.header }

// The exact accepted-response boundary is observable before the terminal body.
func (self *dockerProxyBrokenWriter) WriteHeader(int) { self.once.Do(func() { close(self.entered) }) }

// A downstream disconnect is a delivery error, not upstream cancellation.
func (self *dockerProxyBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Complete input and fsynced intent precede delayed create; quiesce cannot
// certify cleanup at response headers or when the evaluator client disconnects.
func TestDockerProxyDelayedCreateDrainsAfterClientDisconnect(t *testing.T) {
	fixture := newDockerProxyTest(t)
	upstreamEntered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, fixture.create) {
			t.Errorf("upstream observed incomplete body: %v", err)
		}
		entries, err := os.ReadDir(fixture.proxy.operationDirectory())
		committed := false
		for _, entry := range entries {
			committed = committed || strings.HasSuffix(entry.Name(), ".json")
		}
		if err != nil || !committed {
			t.Errorf("upstream preceded durable intent: %v", err)
		}
		close(upstreamEntered)
		w.WriteHeader(http.StatusCreated)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "{\"Id\":\"synthetic-created\"}")
	})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "http://worker/v1.56/containers/create", bytes.NewReader(fixture.create)).WithContext(ctx)
	writer := &dockerProxyBrokenWriter{header: http.Header{}, entered: make(chan struct{})}
	done := make(chan struct{})
	go func() { fixture.proxy.workerHandler(writer, request); close(done) }()
	<-upstreamEntered
	<-writer.entered
	cancel()
	if snapshot := fixture.proxy.Snapshot(); snapshot.Inflight != 1 || snapshot.UnknownOperations != 0 {
		t.Fatalf("headers completed custody: %+v", snapshot)
	}
	quiesceCtx, stop := context.WithCancel(context.Background())
	stop()
	if err := fixture.proxy.Quiesce(quiesceCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("quiesce failed to retain admitted work: %v", err)
	}
	select {
	case <-done:
		t.Fatal("client disconnect canceled upstream custody")
	default:
	}
	close(release)
	<-done
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || fixture.proxy.Snapshot().UnknownOperations != 0 {
		t.Fatal("definite delayed response was replayed or marked unknown")
	}
}

// An incomplete body never reaches the daemon, even if headers admitted it.
type dockerProxyIncompleteBody struct {
	content []byte
	read    bool
}

// The final complete body boundary advances a synthetic clock without sleeps.
type dockerProxyDeadlineBody struct {
	reader   io.Reader
	finished func()
}

func (self *dockerProxyDeadlineBody) Read(destination []byte) (int, error) {
	count, err := self.reader.Read(destination)
	if errors.Is(err, io.EOF) {
		self.finished()
	}
	return count, err
}

func (self *dockerProxyDeadlineBody) Close() error { return nil }

// Admission before deadline cannot authorize a new daemon mutation after a
// delayed upload. Cleanup's drain allowance never renews issue authority.
func TestDockerProxyCompleteUploadCannotIssueAfterOriginalDeadline(t *testing.T) {
	fixture := newDockerProxyTest(t)
	now := fixture.plan.DeadlineAt.Add(-time.Second)
	fixture.proxy.settings.Now = func() time.Time { return now }
	var calls atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusCreated) })
	request := httptest.NewRequest(http.MethodPost, "http://worker/containers/create", nil)
	request.Body = &dockerProxyDeadlineBody{reader: bytes.NewReader(fixture.create), finished: func() { now = fixture.plan.DeadlineAt }}
	request.ContentLength = int64(len(fixture.create))
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, request)
	if response.Code != http.StatusRequestTimeout || calls.Load() != 0 {
		t.Fatal("a pre-deadline upload became a post-deadline daemon issue")
	}
	entries, err := os.ReadDir(fixture.proxy.operationDirectory())
	if err != nil || len(entries) != 0 {
		t.Fatal("expired input manufactured an issued intent", err)
	}
}

// Syncing an intent can itself cross the original clock. The final issue
// boundary must retain definite non-issue, not forward using drain allowance.
func TestDockerProxyCommittedIntentCannotIssueAfterOriginalDeadline(t *testing.T) {
	fixture := newDockerProxyTest(t)
	now := fixture.plan.DeadlineAt.Add(-time.Second)
	fixture.proxy.settings.Now = func() time.Time { return now }
	fixture.proxy.beforeIssue = func() { now = fixture.plan.DeadlineAt }
	var calls atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusCreated) })
	request := httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create))
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, request)
	if response.Code != http.StatusRequestTimeout || calls.Load() != 0 || fixture.proxy.Snapshot().UnknownOperations != 0 {
		t.Fatal("synced intent acquired a fresh post-deadline issue window")
	}
	entries, err := os.ReadDir(fixture.proxy.operationDirectory())
	if err != nil || len(entries) != 1 {
		t.Fatal("committed but unissued intent was not retained", err)
	}
	content, err := readBounded(filepath.Join(fixture.proxy.operationDirectory(), entries[0].Name()), fixture.settings.MaximumDocumentBytes)
	operation := dockerProxyOperation{}
	if err != nil || decodeStrict(content, &operation) != nil || operation.State != "not-issued" {
		t.Fatal("expiry lost its durable definite-not-issued classification", err)
	}
	if err := fixture.proxy.Quiesce(t.Context()); err != nil {
		t.Fatal("definitely unissued intent stranded cleanup custody", err)
	}
}

// The single build's source snapshot and intent sync cannot acquire a new
// execution window after the immutable job deadline passes.
func TestDockerProxyBuildIntentExpiryNeverStartsNativeCli(t *testing.T) {
	fixture := newDockerProxyTest(t)
	now := fixture.plan.DeadlineAt.Add(-time.Second)
	fixture.proxy.settings.Now = func() time.Time { return now }
	fixture.proxy.beforeBuildIssue = func() { now = fixture.plan.DeadlineAt }
	calls := 0
	fixture.proxy.buildRunner = func(context.Context, *dockerProxyBuild) DockerBuildResult {
		calls++
		return DockerBuildResult{Acknowledged: true, ExitCode: 0}
	}
	result, err := fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest)
	if err == nil || calls != 0 || !result.Acknowledged || result.ErrorCode != "build_not_issued" {
		t.Fatal("expired intent launched a fresh native build", result, err)
	}
	if err := fixture.proxy.Quiesce(t.Context()); err != nil {
		t.Fatal("definitely unissued build stranded cleanup", err)
	}
}

// EOF is deliberately not a valid end-of-message.
func (self *dockerProxyIncompleteBody) Read(destination []byte) (int, error) {
	if self.read {
		return 0, io.ErrUnexpectedEOF
	}
	self.read = true
	return copy(destination, self.content), nil
}

// Closing rejected input cannot create side effects.
func (self *dockerProxyIncompleteBody) Close() error { return nil }

// No request byte is upstream-visible until the complete bounded body is synced.
func TestDockerProxyIncompleteUploadNeverIssuesIntent(t *testing.T) {
	fixture := newDockerProxyTest(t)
	var calls atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusCreated) })
	request := httptest.NewRequest(http.MethodPost, "http://worker/containers/create", nil)
	request.Body = &dockerProxyIncompleteBody{content: fixture.create[:len(fixture.create)/2]}
	request.ContentLength = int64(len(fixture.create))
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, request)
	if response.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatal("incomplete upload reached the daemon")
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixture.proxy.operationDirectory())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			t.Fatal("incomplete upload manufactured an issued intent")
		}
	}
}

// A bounded scratch outage after issue must still leave a durable unknown
// receipt outside that filesystem; empty replacement scratch cannot clear it.
func TestDockerProxyScratchLossRetainsIndependentUnknownJournal(t *testing.T) {
	fixture := newDockerProxyTest(t)
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if err := os.RemoveAll(fixture.plan.ScratchDirectory); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "synthetic terminal response")
	})
	request := httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create))
	fixture.proxy.workerHandler(httptest.NewRecorder(), request)
	if fixture.proxy.Snapshot().UnknownOperations != 1 {
		t.Fatal("missing response spool erased an issued mutation")
	}
	entries, err := os.ReadDir(fixture.proxy.operationDirectory())
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("scratch loss consumed the independent journal: %v", err)
	}
	if err := fixture.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Quiesce(context.Background()); !errors.Is(err, ErrDockerProxyUnknown) || recovered.Snapshot().Quiesced {
		t.Fatalf("replacement empty scratch cleared issued authority: %v", err)
	}
}

// An orphan partial body has no issued intent and is never replayed. Its bytes
// remain charged across recovery, independently from the metadata inventory.
func TestDockerProxyRecoversUnissuedBodyWithoutInventingMutation(t *testing.T) {
	fixture := newDockerProxyTest(t)
	body := []byte("synthetic incomplete request")
	path := filepath.Join(fixture.proxy.bodyDirectory(), strings.Repeat("a", 32)+".request")
	if err := exclusiveFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.spoolBytes != int64(len(body)) || recovered.Snapshot().Operations != 1 || recovered.Snapshot().UnknownOperations != 0 {
		t.Fatal("unissued scratch was uncharged or mistaken for issued authority")
	}
	if err := recovered.Quiesce(context.Background()); err != nil || !recovered.Snapshot().Quiesced {
		t.Fatalf("definitely unissued body prevented positive quiescence: %v", err)
	}
}

// Candidate rename/write authority cannot contain the owner scratch, and an
// exhausted candidate filesystem cannot share the journal's directory tree.
func TestDockerProxyRejectsOverlappingStateScratchAndWorkRoots(t *testing.T) {
	fixture := newDockerProxyTest(t)
	for _, scratch := range []string{fixture.plan.StateDirectory, filepath.Join(fixture.plan.StateDirectory, "scratch"), filepath.Dir(fixture.plan.StateDirectory),
		fixture.plan.ContextRoot, filepath.Join(fixture.plan.ContextRoot, "scratch"), filepath.Dir(fixture.plan.ContextRoot)} {
		plan := fixture.plan
		plan.ScratchDirectory = scratch
		if err := validateDockerProxyPlan(plan, fixture.settings, false); err == nil {
			t.Errorf("overlapping root was accepted: %q", scratch)
		}
	}
}

// A canceled real CLI may still be joining its native process group. Releasing
// the journal lock before that join would let recovery overlap its late write.
func TestDockerProxyCloseRetainsJournalUntilOwnedBuildJoins(t *testing.T) {
	fixture := newDockerProxyTest(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fixture.proxy.buildRunner = func(ctx context.Context, _ *dockerProxyBuild) DockerBuildResult {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return DockerBuildResult{RunId: fixture.plan.RunId, Acknowledged: false, ExitCode: -1, ErrorCode: "synthetic_joined_unknown"}
	}
	buildDone, closeDone, closeFinished := make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() { defer close(buildDone); _, _ = fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest) }()
	<-entered
	go func() { defer close(closeFinished); closeDone <- fixture.proxy.Close() }()
	var releaseOnce sync.Once
	t.Cleanup(func() {
		fixture.proxy.cancelOwner()
		releaseOnce.Do(func() { close(release) })
		<-buildDone
		<-closeFinished
	})
	<-canceled
	if replacement, err := newDockerProxy(fixture.plan, fixture.settings, false); err == nil {
		replacement.Close()
		t.Fatal("recovery overlapped a still-owned native build/result writer")
	}
	select {
	case <-closeDone:
		t.Fatal("close returned before the explicit native join barrier")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	<-buildDone
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	<-closeFinished
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Quiesce(t.Context()); !errors.Is(err, ErrDockerProxyUnknown) {
		t.Fatal("joined but unknown build became replayable", err)
	}
}

// An owner hard exit leaves its issued intent held even after a delayed daemon
// creates an object; a replacement owner must never replay the request.
func TestDockerProxyHardExitKeepsUnknownIntentWithoutReplay(t *testing.T) {
	fixture := newDockerProxyTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		w.WriteHeader(http.StatusCreated)
	})
	done := make(chan struct{})
	go func() {
		fixture.proxy.workerHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create)))
		close(done)
	}()
	<-entered
	_ = fixture.proxy.Close()
	<-done
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Quiesce(context.Background()); !errors.Is(err, ErrDockerProxyUnknown) {
		t.Fatalf("lost mutation was certified by restart: %v", err)
	}
	response := httptest.NewRecorder()
	recovered.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("unknown owner admitted replay")
	}
	close(release)
	if calls.Load() != 1 {
		t.Fatal("replacement owner replayed a lost mutation")
	}
}

// Case-insensitive Docker field aliases cannot disagree with exact scope maps.
func TestDockerProxyRejectsAliasedScopeAndCandidateSocketMounts(t *testing.T) {
	fixture := newDockerProxyTest(t)
	var calls atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusCreated) })
	body := append([]byte(nil), fixture.create[:len(fixture.create)-1]...)
	body = append(body, []byte(",\"labels\":{\"synthetic\":\"foreign\"}}")...)
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(body)))
	if response.Code != http.StatusForbidden || calls.Load() != 0 {
		t.Fatal("aliased Docker scope reached the daemon")
	}
	var object map[string]any
	if err := json.Unmarshal(fixture.create, &object); err != nil {
		t.Fatal(err)
	}
	object["HostConfig"].(map[string]any)["Binds"] = []string{filepath.Dir(fixture.plan.StateDirectory) + ":/candidate-host:ro"}
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(body)))
	if response.Code != http.StatusForbidden || calls.Load() != 0 {
		t.Fatal("candidate socket/state mount reached the daemon")
	}
}

// Worker callers cannot accidentally bypass owned BuildKit/session lifetime.
func TestDockerProxyWorkerDeniesBuildkitAndDaemonWideMutations(t *testing.T) {
	fixture := newDockerProxyTest(t)
	for _, path := range []string{"/grpc", "/session", "/build", "/containers/prune", "/images/prune", "/volumes/create", "/plugins/create"} {
		response := httptest.NewRecorder()
		fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/v1.56"+path, http.NoBody))
		if response.Code != http.StatusForbidden {
			t.Fatalf("worker admitted unowned operation %s: %d", path, response.Code)
		}
	}
}

// Build invocation sees only the fully sealed snapshot and fsynced intent.
// Canceling the evaluator wait does not cancel the real CLI/session owner.
func TestDockerProxyOwnedBuildSurvivesWaiterDisconnectAndKeepsSnapshot(t *testing.T) {
	fixture := newDockerProxyTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var executions atomic.Int64
	fixture.proxy.buildRunner = func(ctx context.Context, build *dockerProxyBuild) DockerBuildResult {
		executions.Add(1)
		content, err := os.ReadFile(filepath.Join(build.contextDirectory, "canonical.patch"))
		if err != nil || string(content) != "synthetic sealed input\n" {
			t.Errorf("owned build observed an unsealed context: %v", err)
		}
		record, err := readBounded(filepath.Join(fixture.plan.StateDirectory, "build.json"), fixture.settings.MaximumDocumentBytes)
		if err != nil || !bytes.Contains(record, []byte("\"state\":\"issued\"")) {
			t.Errorf("CLI started before durable issue: %v", err)
		}
		close(entered)
		<-release
		if ctx.Err() != nil {
			t.Errorf("waiter cancellation reached owned CLI: %v", ctx.Err())
		}
		return DockerBuildResult{Acknowledged: true, ExitCode: 0}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := fixture.proxy.RunOwnedBuild(ctx, fixture.buildRequest); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation: %v", err)
	}
	if err := os.Chmod(filepath.Join(fixture.buildRequest.ContextDirectory, "canonical.patch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.buildRequest.ContextDirectory, "canonical.patch"), []byte("synthetic changed input"), 0600); err != nil {
		t.Fatal(err)
	}
	if snapshot := fixture.proxy.Snapshot(); snapshot.Inflight != 1 || !snapshot.BuildStarted || snapshot.BuildAcknowledged {
		t.Fatalf("waiter released live CLI: %+v", snapshot)
	}
	close(release)
	result, err := fixture.proxy.RunOwnedBuild(context.Background(), fixture.buildRequest)
	if err != nil || !result.Acknowledged || result.ExitCode != 0 || executions.Load() != 1 {
		t.Fatalf("owned build was not reattached: %+v %v", result, err)
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Minimum-work tags exactly the frozen image id before passing that alias to
// build-submission --allow-local-base; another tag or a direct digest is not
// interchangeable authority for this fixed workflow.
func TestDockerProxyFrozenBaseAliasMatchesMinimumWorkBuilder(t *testing.T) {
	fixture := newDockerProxyTest(t)
	if _, err := fixture.proxy.validateBuildArguments(fixture.buildRequest); err != nil {
		t.Fatalf("minimum-work derived base alias was rejected: %v", err)
	}
	for _, value := range []string{fixture.plan.BaseImageDigest, "urnetwork/sim-latency-evaluator-base-id:" + strings.Repeat("4", 64), "urnetwork/sim-latency-evaluator-base:latest"} {
		request := fixture.buildRequest
		request.Arguments = append([]string(nil), request.Arguments...)
		for index, argument := range request.Arguments {
			if strings.HasPrefix(argument, "EVALUATOR_BASE_IMAGE=") {
				request.Arguments[index] = "EVALUATOR_BASE_IMAGE=" + value
			}
		}
		if _, err := fixture.proxy.validateBuildArguments(request); err == nil {
			t.Fatalf("non-frozen base alias was accepted: %s", value)
		}
	}
	var calls atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	path := "http://worker/v1.56/images/" + fixture.plan.BaseImageDigest + "/tag?repo=urnetwork/sim-latency-evaluator-base-id&tag=" + strings.TrimPrefix(fixture.plan.BaseImageDigest, "sha256:")
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, path, http.NoBody))
	if response.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("frozen id alias tag was rejected: %d", response.Code)
	}
	response = httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, strings.Replace(path, "evaluator-base-id", "evaluator-base", 1), http.NoBody))
	if response.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatal("foreign base alias reached the daemon")
	}
}

// Only the established build flags/resources/source identity family is legal.
func TestDockerProxyOwnedBuildRejectsPolicyExpansionBeforeIssue(t *testing.T) {
	mutations := []func(*DockerBuildRequest){
		func(request *DockerBuildRequest) { request.Arguments[0] = "run" },
		func(request *DockerBuildRequest) {
			request.Arguments = append(request.Arguments[:len(request.Arguments)-1], "--secret", "id=synthetic", request.ContextDirectory)
		},
		func(request *DockerBuildRequest) {
			for index, value := range request.Arguments {
				if value == "none" {
					request.Arguments[index] = "host"
					return
				}
			}
		},
		func(request *DockerBuildRequest) {
			request.ContextDirectory = filepath.Dir(request.ContextDirectory)
			request.Arguments[len(request.Arguments)-1] = request.ContextDirectory
		},
	}
	for _, mutate := range mutations {
		fixture := newDockerProxyTest(t)
		request := fixture.buildRequest
		request.Arguments = append([]string(nil), request.Arguments...)
		mutate(&request)
		fixture.proxy.buildRunner = func(context.Context, *dockerProxyBuild) DockerBuildResult {
			t.Error("expanded policy reached CLI")
			return DockerBuildResult{}
		}
		if _, err := fixture.proxy.RunOwnedBuild(context.Background(), request); err == nil {
			t.Fatal("owned build accepted expanded policy")
		}
	}
}

// Definite compiler failure is an acknowledgement, not a reason to restart work.
func TestDockerProxyOwnedCompileFailureIsDefiniteAndNotReplayed(t *testing.T) {
	fixture := newDockerProxyTest(t)
	var calls atomic.Int64
	fixture.proxy.buildRunner = func(context.Context, *dockerProxyBuild) DockerBuildResult {
		calls.Add(1)
		fixture.proxy.observeSolve("2", true)
		return DockerBuildResult{Acknowledged: true, ExitCode: 1, GrpcStatus: "2", Stderr: []byte("synthetic ordinary compiler failure")}
	}
	result, err := fixture.proxy.RunOwnedBuild(context.Background(), fixture.buildRequest)
	if err != nil || !result.Acknowledged || result.ExitCode != 1 {
		t.Fatalf("compiler failure treated as unknown: %+v %v", result, err)
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = fixture.proxy.Close()
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	result, err = recovered.RunOwnedBuild(context.Background(), fixture.buildRequest)
	if err != nil || !result.Acknowledged || result.ExitCode != 1 || calls.Load() != 1 {
		t.Fatalf("known compiler failure was repeated: %+v %v", result, err)
	}
}

// Interrupted response bodies are unknown even after successful HTTP headers.
func TestDockerProxyPartialResponseRemainsHeld(t *testing.T) {
	fixture := newDockerProxyTest(t)
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "short")
	})
	fixture.proxy.workerHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create)))
	if err := fixture.proxy.Quiesce(context.Background()); !errors.Is(err, ErrDockerProxyUnknown) {
		t.Fatalf("partial response certified as terminal: %v", err)
	}
}

// This subprocess is deliberately killed after its private durable intent is
// visible. It never loads server configuration or connects to a real daemon.
func TestDockerProxyAbruptOwnerHelper(t *testing.T) {
	path := os.Getenv("SYNTHETIC_DOCKER_PROXY_HELPER")
	if path == "" {
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var plan DockerProxyPlan
	if err := json.Unmarshal(content, &plan); err != nil {
		t.Fatal(err)
	}
	proxy, err := newDockerProxy(plan, DefaultDockerProxySettings(), false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path + ".create")
	if err != nil {
		t.Fatal(err)
	}
	ready := os.NewFile(uintptr(3), "synthetic-ready")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if _, err := ready.Write([]byte{1}); err != nil {
			t.Error(err)
			return
		}
		_ = ready.Close()
		select {}
	}))
	proxy.dialDaemon = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	proxy.workerHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(body)))
	t.Fatal("synthetic hard-exit helper unexpectedly completed")
}

// An actual SIGKILL bypasses all Go cleanup/receipt code. Recovery must retain
// the issued record and block admission, not infer success from an empty census.
func TestDockerProxyAbruptProcessExitKeepsIssuedRecordHeld(t *testing.T) {
	fixture := newDockerProxyTest(t)
	_ = fixture.proxy.Close()
	planPath := filepath.Join(t.TempDir(), "synthetic-plan.json")
	content, err := json.Marshal(fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath+".create", fixture.create, 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), fixture.settings.UploadTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDockerProxyAbruptOwnerHelper$")
	command.Env = append(controlEnvironment(), "SYNTHETIC_DOCKER_PROXY_HELPER="+planPath)
	command.ExtraFiles = []*os.File{writer}
	output := &boundedOutput{maximum: fixture.settings.MaximumDocumentBytes}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	_ = writer.Close()
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		_ = command.Wait()
		t.Fatalf("synthetic owner failed before barrier: %v %s", err, output.String())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("synthetic owner did not exit abruptly")
	}
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if snapshot := recovered.Snapshot(); snapshot.UnknownOperations != 1 || snapshot.Quiesced || !snapshot.AdmissionsClosed {
		t.Fatalf("SIGKILL erased request custody: %+v", snapshot)
	}
	if err := recovered.Quiesce(context.Background()); !errors.Is(err, ErrDockerProxyUnknown) {
		t.Fatalf("SIGKILL was certified as terminal: %v", err)
	}
}

// Bounded rejected bodies and disabled budgets cannot reach upstream mutation.
func TestDockerProxyBodyAndContextBoundsRejectBeforeIssue(t *testing.T) {
	fixture := newDockerProxyTest(t)
	fixture.proxy.settings.MaximumRequestBytes = 8
	var requests atomic.Int64
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusCreated) })
	recorder := httptest.NewRecorder()
	fixture.proxy.workerHandler(recorder, httptest.NewRequest(http.MethodPost, "http://worker/containers/create", bytes.NewReader(fixture.create)))
	if recorder.Code != http.StatusBadRequest || requests.Load() != 0 {
		t.Fatal("oversized mutation was partially issued")
	}
	fixture.proxy.settings.MaximumContextBytes = 1
	called := false
	fixture.proxy.buildRunner = func(context.Context, *dockerProxyBuild) DockerBuildResult { called = true; return DockerBuildResult{} }
	if _, err := fixture.proxy.RunOwnedBuild(context.Background(), fixture.buildRequest); err == nil || called {
		t.Fatal("oversized context reached the owned CLI")
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
}
