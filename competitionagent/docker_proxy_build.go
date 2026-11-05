//go:build linux

// A real Docker/buildx process owns BuildKit's filesync/session lifetime outside
// the evaluator tree. Only its fixed build command and fully sealed context may
// start it; disconnecting the evaluator only stops waiting for its receipt.
package competitionagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Immutable input fields precede issued; mutable completion is guarded by the
// proxy state lock. The done edge always follows durable terminal publication.
type dockerProxyBuild struct {
	request                                 DockerBuildRequest
	requestSha256                           string
	contextSha256                           string
	contextBytes                            int64
	arguments                               []string
	contextDirectory                        string
	issued, finished                        bool
	solveObserved, solveAck, transportFault bool
	grpcStatus                              string
	result                                  DockerBuildResult
	done                                    chan struct{}
}

// Private persisted provenance excludes log bodies and caller/session lifetime.
type dockerProxyBuildRecord struct {
	Schema        int                `json:"schema"`
	RunId         string             `json:"run_id"`
	Request       DockerBuildRequest `json:"request"`
	RequestSha256 string             `json:"request_sha256"`
	ContextSha256 string             `json:"context_sha256"`
	ContextBytes  int64              `json:"context_bytes"`
	State         string             `json:"state"`
	Result        DockerBuildResult  `json:"result"`
}

// Clients can reattach to the same fixed build; no issued build is repeated.
func (self *DockerProxy) RunOwnedBuild(ctx context.Context, request DockerBuildRequest) (DockerBuildResult, error) {
	arguments, err := self.validateBuildArguments(request)
	if err != nil {
		return DockerBuildResult{}, err
	}
	content, err := json.Marshal(request)
	if err != nil || int64(len(content)) > self.settings.MaximumDocumentBytes {
		return DockerBuildResult{}, errors.New("owned build request exceeds its bound")
	}
	requestSha := digestDockerBytes(content)
	self.stateLock.Lock()
	if self.buildAdmissionsClosed {
		self.stateLock.Unlock()
		return DockerBuildResult{}, errors.New("build admission is permanently closed for retirement")
	}
	existing := self.build
	if existing != nil {
		if existing.requestSha256 != requestSha {
			self.stateLock.Unlock()
			return DockerBuildResult{}, errors.New("owned build conflicts with the committed command")
		}
		done := existing.done
		self.stateLock.Unlock()
		select {
		case <-done:
			return self.buildResult(existing)
		case <-ctx.Done():
			return DockerBuildResult{}, ctx.Err()
		}
	}
	self.stateLock.Unlock()
	release, err := self.admit(false)
	if err != nil {
		return DockerBuildResult{}, err
	}
	build := &dockerProxyBuild{request: request, requestSha256: requestSha, arguments: arguments, done: make(chan struct{})}
	self.stateLock.Lock()
	if self.buildAdmissionsClosed {
		self.stateLock.Unlock()
		release()
		return DockerBuildResult{}, errors.New("retirement closed build admission before its reservation")
	}
	if self.build != nil {
		self.stateLock.Unlock()
		release()
		return self.RunOwnedBuild(ctx, request)
	}
	self.build = build
	self.stateLock.Unlock()
	failBeforeIssue := func(cause error) (DockerBuildResult, error) {
		result := DockerBuildResult{RunId: self.plan.RunId, Acknowledged: true, ExitCode: -1, ErrorCode: "build_not_issued"}
		if err := self.storeBuildRecord(build, "not-issued", result); err != nil {
			result.Acknowledged = false
			result.ErrorCode = "build_receipt_persistence_failed"
		}
		self.stateLock.Lock()
		build.finished = true
		build.result = result
		if !result.Acknowledged {
			self.unknown++
			self.accepting = false
		}
		close(build.done)
		self.notifyWithLock()
		self.stateLock.Unlock()
		release()
		return build.result, cause
	}
	if err := os.Mkdir(self.buildDirectory(), 0700); err != nil {
		return failBeforeIssue(errors.New("owned build state already exists or is unavailable"))
	}
	snapshot := filepath.Join(self.buildDirectory(), "context")
	digest, size, err := self.snapshotContext(ctx, request.ContextDirectory, snapshot)
	build.contextBytes = size
	if err != nil {
		return failBeforeIssue(err)
	}
	build.contextDirectory, build.contextSha256, build.contextBytes = snapshot, digest, size
	dockerfile, err := readBounded(self.plan.Dockerfile.Path, self.settings.MaximumContextFileBytes)
	if err != nil || digestDockerBytes(dockerfile) != self.plan.Dockerfile.Sha256 {
		return failBeforeIssue(errors.New("owned Dockerfile failed its fixed content pin"))
	}
	if err := exclusiveFile(filepath.Join(self.buildDirectory(), "Dockerfile"), dockerfile, 0400); err != nil {
		return failBeforeIssue(err)
	}
	for index := range build.arguments {
		if build.arguments[index] == self.plan.Dockerfile.Path {
			build.arguments[index] = filepath.Join(self.buildDirectory(), "Dockerfile")
		}
		if build.arguments[index] == request.ContextDirectory {
			build.arguments[index] = snapshot
		}
	}
	if !self.settings.Now().Before(self.plan.DeadlineAt) {
		return failBeforeIssue(errors.New("original deadline elapsed before owned build issue"))
	}
	if err := self.storeBuild(build, "issued"); err != nil {
		return failBeforeIssue(err)
	}
	if self.beforeBuildIssue != nil {
		self.beforeBuildIssue()
	}
	if !self.settings.Now().Before(self.plan.DeadlineAt) {
		return failBeforeIssue(errors.New("original deadline elapsed during owned build intent commit"))
	}
	self.stateLock.Lock()
	build.issued = true
	self.notifyWithLock()
	self.stateLock.Unlock()
	go func() {
		defer release()
		ownerCtx, cancel := context.WithTimeout(self.ownerCtx, self.settings.BuildTimeout)
		result := self.buildRunner(ownerCtx, build)
		cancel()
		result.RunId = self.plan.RunId
		if err := self.storeBuildResult(build, result); err != nil {
			result.Acknowledged = false
			result.ErrorCode = "build_receipt_persistence_failed"
		}
		self.stateLock.Lock()
		build.result, build.finished = result, true
		if !result.Acknowledged {
			self.unknown++
			self.accepting = false
		}
		close(build.done)
		self.notifyWithLock()
		self.stateLock.Unlock()
	}()
	select {
	case <-build.done:
		return self.buildResult(build)
	case <-ctx.Done():
		return DockerBuildResult{}, ctx.Err()
	}
}

// The worker-facing command is finite JSON; build context is sealed locally by
// the independent owner, not streamed from a client that can disappear.
func (self *DockerProxy) buildRequestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "fixed build command requires POST", http.StatusMethodNotAllowed)
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, self.settings.MaximumDocumentBytes+1))
	if err != nil || int64(len(content)) > self.settings.MaximumDocumentBytes {
		http.Error(w, "build request exceeds its bound", http.StatusBadRequest)
		return
	}
	request := DockerBuildRequest{}
	if decodeStrict(content, &request) != nil {
		http.Error(w, "build request rejected", http.StatusBadRequest)
		return
	}
	result, err := self.RunOwnedBuild(r.Context(), request)
	if err != nil && !result.Acknowledged {
		http.Error(w, "owned build outcome remains held", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// The fixed argv vocabulary preserves the existing BuildKit resource policy.
func (self *DockerProxy) validateBuildArguments(request DockerBuildRequest) ([]string, error) {
	args := request.Arguments
	if request.RunId != self.plan.RunId || len(args) < 2 || args[0] != "build" || len(args) > self.settings.MaximumContextEntries ||
		!filepath.IsAbs(request.ContextDirectory) || filepath.Clean(request.ContextDirectory) != request.ContextDirectory ||
		request.ContextDirectory == self.plan.ContextRoot || !pathWithin(self.plan.ContextRoot, request.ContextDirectory) || args[len(args)-1] != request.ContextDirectory {
		return nil, errors.New("owned Docker build is outside its fixed private context")
	}
	flags := map[string]string{}
	resources := map[string]string{}
	buildArgs := map[string]string{}
	for index := 1; index < len(args)-1; index++ {
		arg := args[index]
		if arg == "--no-cache" {
			if _, present := flags[arg]; present {
				return nil, errors.New("duplicate owned build flag")
			}
			flags[arg] = "true"
			continue
		}
		key, value, equal := strings.Cut(arg, "=")
		if !equal {
			key = arg
			if index+1 >= len(args)-1 {
				return nil, errors.New("incomplete owned build option")
			}
			index++
			value = args[index]
		}
		switch key {
		case "--resource":
			name, value, ok := strings.Cut(value, "=")
			if !ok || resources[name] != "" {
				return nil, errors.New("ambiguous owned build resources")
			}
			resources[name] = value
		case "--build-arg":
			name, value, ok := strings.Cut(value, "=")
			if !ok || buildArgs[name] != "" {
				return nil, errors.New("ambiguous owned build argument")
			}
			buildArgs[name] = value
		case "--platform", "--provenance", "--pull", "--network", "--cgroup-parent", "--file", "--tag":
			if _, present := flags[key]; present {
				return nil, errors.New("duplicate owned build option")
			}
			flags[key] = value
		default:
			return nil, errors.New("owned build contains an unapproved option")
		}
	}
	if len(flags) != 8 || flags["--no-cache"] != "true" || flags["--platform"] != "linux/amd64" || flags["--provenance"] != "false" || flags["--pull"] != "false" ||
		flags["--network"] != "none" || flags["--cgroup-parent"] != self.plan.BuildCgroupName || flags["--file"] != self.plan.Dockerfile.Path ||
		len(resources) != 3 || resources["cpuset-cpus"] != self.plan.CpuSet || resources["memory"] != self.plan.BuildMemory || resources["memory-swap"] != self.plan.BuildMemory {
		return nil, errors.New("owned build changed frozen network, executable or resource policy")
	}
	allowed := map[string]bool{"EVALUATOR_BASE_IMAGE": true, "BASE_SHA": true, "CANDIDATE_SHA": true, "PATCH_SHA256": true, "POLICY_SHA256": true,
		"BUILDER_SHA256": true, "PATCH_VALIDATOR_SHA256": true, "CANDIDATE_COMMITS_SHA256": true, "ORIGINAL_SOURCE_IDENTITY_SHA256": true, "SUBMISSION_IDENTITY": true}
	baseAlias := "urnetwork/sim-latency-evaluator-base-id:" + strings.TrimPrefix(self.plan.BaseImageDigest, "sha256:")
	if len(buildArgs) != len(allowed) || buildArgs["EVALUATOR_BASE_IMAGE"] != baseAlias {
		return nil, errors.New("owned build omitted frozen source arguments")
	}
	for name, value := range buildArgs {
		if !allowed[name] {
			return nil, errors.New("owned build introduced a new argument")
		}
		if name == "EVALUATOR_BASE_IMAGE" {
			continue
		}
		length := 64
		if name == "BASE_SHA" || name == "CANDIDATE_SHA" {
			length = 40
		}
		if len(value) != length || !isDockerProxyHex(value) {
			return nil, errors.New("owned build argument has invalid content identity")
		}
	}
	key := buildArgs["SUBMISSION_IDENTITY"][:32]
	if self.plan.BuildCgroupName != "urnetwork-submission-build-"+key+".slice" || flags["--tag"] != "urnetwork/sim-latency-submission:"+key {
		return nil, errors.New("owned build tag or cgroup differs from its source identity")
	}
	return append([]string(nil), args...), nil
}

// A bounded private tree snapshot preserves relative symlinks but never follows
// them or imports special files. Every file and directory is synced before issue.
func (self *DockerProxy) snapshotContext(ctx context.Context, source, destination string) (string, int64, error) {
	if err := validateDockerContextRoot(self.plan.ContextRoot); err != nil {
		return "", 0, err
	}
	contextRoot, err := os.OpenRoot(self.plan.ContextRoot)
	if err != nil {
		return "", 0, err
	}
	defer contextRoot.Close()
	relative, err := filepath.Rel(self.plan.ContextRoot, source)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		return "", 0, errors.New("owned build source escaped its private root")
	}
	current := ""
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := contextRoot.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", 0, errors.New("owned build source has an unsafe ancestor")
		}
	}
	root, err := contextRoot.OpenRoot(relative)
	if err != nil {
		return "", 0, err
	}
	defer root.Close()
	if err := os.Mkdir(destination, 0700); err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	total := int64(0)
	examined := 0
	var copyDirectory func(string, int) error
	copyDirectory = func(relative string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > self.settings.MaximumContextDepth {
			return errors.New("owned build context depth exceeds its bound")
		}
		directory, err := root.Open(relative)
		if err != nil {
			return err
		}
		names := []string{}
		for {
			entries, readErr := directory.ReadDir(self.settings.DirectoryBatch)
			for _, entry := range entries {
				examined++
				if examined > self.settings.MaximumContextEntries {
					directory.Close()
					return errors.New("owned build context entries exceed their bound")
				}
				names = append(names, entry.Name())
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				directory.Close()
				return readErr
			}
		}
		if err := directory.Close(); err != nil {
			return err
		}
		sort.Strings(names)
		for _, name := range names {
			child := filepath.Join(relative, name)
			info, err := root.Lstat(child)
			if err != nil {
				return err
			}
			target := filepath.Join(destination, child)
			if info.IsDir() {
				if err := os.Mkdir(target, 0700); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(hash, "directory\x00%s\x00", filepath.ToSlash(child))
				if err := copyDirectory(child, depth+1); err != nil {
					return err
				}
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				link, err := root.Readlink(child)
				resolved := filepath.Clean(filepath.Join(filepath.Dir(child), link))
				if err != nil || filepath.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
					return errors.New("owned build symlink escaped its sealed context")
				}
				if err := os.Symlink(link, target); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(hash, "symlink\x00%s\x00%s\x00", filepath.ToSlash(child), link)
				continue
			}
			if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > self.settings.MaximumContextFileBytes || self.settings.MaximumContextBytes-total < info.Size() {
				return errors.New("owned build context file exceeds its bound")
			}
			input, err := root.OpenFile(child, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			mode := os.FileMode(0400)
			if info.Mode().Perm()&0111 != 0 {
				mode = 0500
			}
			output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				input.Close()
				return err
			}
			fileHash := sha256.New()
			counter := &dockerContextWriter{proxy: self, writer: io.MultiWriter(output, fileHash), remaining: self.settings.MaximumContextBytes - total}
			count, copyErr := io.CopyBuffer(counter, &contextReader{ctx: ctx, reader: io.LimitReader(input, self.settings.MaximumContextFileBytes+1)}, make([]byte, self.settings.CopyBufferBytes))
			after, statErr := input.Stat()
			closeInputErr, syncErr, closeOutputErr := input.Close(), output.Sync(), output.Close()
			if copyErr != nil || statErr != nil || closeInputErr != nil || syncErr != nil || closeOutputErr != nil || count != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				return errors.New("owned build context changed or failed durable copy")
			}
			total += count
			_, _ = fmt.Fprintf(hash, "file\x00%s\x00%d\x00%d\x00%s\x00", filepath.ToSlash(child), mode, count, hex.EncodeToString(fileHash.Sum(nil)))
		}
		return syncDirectory(filepath.Join(destination, relative))
	}
	if err := copyDirectory(".", 0); err != nil {
		return "", total, err
	}
	if err := syncDirectory(self.buildDirectory()); err != nil {
		return "", total, err
	}
	return hex.EncodeToString(hash.Sum(nil)), total, nil
}

// Every context byte is charged before it reaches disk.
type dockerContextWriter struct {
	proxy     *DockerProxy
	writer    io.Writer
	remaining int64
}

// A hostile file cannot grow beyond either the context or aggregate budget.
func (self *dockerContextWriter) Write(content []byte) (int, error) {
	if int64(len(content)) > self.remaining {
		return 0, errors.New("owned context grew beyond its byte bound")
	}
	if err := self.proxy.reserveSpool(int64(len(content))); err != nil {
		return 0, err
	}
	count, err := self.writer.Write(content)
	self.remaining -= int64(count)
	return count, err
}

// Real BuildKit remains enabled. Its pinned buildx plugin and live sessions run
// under the independent owner with no ambient Docker context or credentials.
func (self *DockerProxy) runBuildCli(ctx context.Context, build *dockerProxyBuild) DockerBuildResult {
	result := DockerBuildResult{RunId: self.plan.RunId, ExitCode: -1, ErrorCode: "build_outcome_unknown"}
	for _, pin := range []Pin{self.plan.Docker, self.plan.Buildx} {
		if err := validatePin(pin, self.protected); err != nil {
			return result
		}
	}
	configDirectory := filepath.Join(self.buildDirectory(), "docker-config")
	if err := os.Mkdir(configDirectory, 0700); err != nil {
		return result
	}
	config, err := json.Marshal(struct {
		Extra []string `json:"cliPluginsExtraDirs"`
	}{Extra: []string{filepath.Dir(self.plan.Buildx.Path)}})
	if err != nil || exclusiveFile(filepath.Join(configDirectory, "config.json"), config, 0600) != nil {
		return result
	}
	arguments := append([]string{"--host", "unix://" + self.plan.BuildSocketPath, "--config", configDirectory}, build.arguments...)
	command := exec.Command(self.plan.Docker.Path, arguments...)
	command.Dir = self.buildDirectory()
	command.Env = append(controlEnvironment(), "HOME="+self.buildDirectory(), "DOCKER_BUILDKIT=1", "BUILDX_BUILDER=default")
	stdout := &boundedOutput{maximum: self.settings.MaximumBuildOutputBytes}
	stderr := &boundedOutput{maximum: self.settings.MaximumBuildOutputBytes}
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	exitCode, runErr := runProcessGroup(ctx, command, self.settings.BuildStopGrace)
	self.closeBuildSessions()
	self.stateLock.Lock()
	observed, ack, fault, status := build.solveObserved, build.solveAck, build.transportFault, build.grpcStatus
	self.stateLock.Unlock()
	result.ExitCode, result.GrpcStatus = exitCode, status
	result.Stdout, result.Stderr = append([]byte(nil), stdout.Bytes()...), append([]byte(nil), stderr.Bytes()...)
	normalExit := runErr == nil
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0 {
		normalExit = true
	}
	result.Acknowledged = ctx.Err() == nil && normalExit && !fault && (observed && ack || !observed && !fault)
	if result.Acknowledged {
		result.ErrorCode = ""
	}
	if stdout.exceeded || stderr.exceeded {
		result.ExitCode = 1
		result.ErrorCode = "build_output_limit"
	}
	return result
}

// Complete Solve trailers, not log parsing or HTTP headers, establish known
// compiler failure. A later successful subcall cannot erase an unknown one.
func (self *DockerProxy) observeSolve(status string, ack bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.build == nil {
		return
	}
	if self.build.solveObserved {
		self.build.solveAck = self.build.solveAck && ack
	} else {
		self.build.solveObserved = true
		self.build.solveAck = ack
	}
	self.build.grpcStatus = status
}

// Cached result delivery never mutates or reruns an acknowledged build.
func (self *DockerProxy) buildResult(build *dockerProxyBuild) (DockerBuildResult, error) {
	self.stateLock.Lock()
	result := build.result
	result.Stdout = append([]byte(nil), result.Stdout...)
	result.Stderr = append([]byte(nil), result.Stderr...)
	self.stateLock.Unlock()
	if !result.Acknowledged {
		return result, ErrDockerProxyUnknown
	}
	return result, nil
}

// Issued input provenance is durable before the first real CLI process starts.
func (self *DockerProxy) storeBuild(build *dockerProxyBuild, state string) error {
	self.stateLock.Lock()
	result := build.result
	self.stateLock.Unlock()
	return self.storeBuildRecord(build, state, result)
}

// Result persistence does not race concurrent status reads or Solve callbacks.
func (self *DockerProxy) storeBuildRecord(build *dockerProxyBuild, state string, result DockerBuildResult) error {
	record := dockerProxyBuildRecord{Schema: Schema, RunId: self.plan.RunId, Request: build.request, RequestSha256: build.requestSha256,
		ContextSha256: build.contextSha256, ContextBytes: build.contextBytes, State: state, Result: result}
	record.Result.Stdout, record.Result.Stderr = nil, nil
	content, err := json.Marshal(record)
	if err != nil || int64(len(content)) > self.settings.MaximumDocumentBytes {
		return errors.New("owned build receipt exceeds its bound")
	}
	return atomicFile(self.plan.StateDirectory, "build.json", content)
}

// Retained output is independently synced before terminal receipt publication.
func (self *DockerProxy) storeBuildResult(build *dockerProxyBuild, result DockerBuildResult) error {
	if err := exclusiveFile(filepath.Join(self.buildDirectory(), "stdout"), result.Stdout, 0400); err != nil {
		return err
	}
	if err := exclusiveFile(filepath.Join(self.buildDirectory(), "stderr"), result.Stderr, 0400); err != nil {
		return err
	}
	if err := self.syncSpoolDirectory(self.buildDirectory()); err != nil {
		return err
	}
	state := "unknown"
	if result.Acknowledged {
		state = "acknowledged"
	}
	return self.storeBuildRecord(build, state, result)
}

// A recovered launch is held, never replayed, even if no container is visible.
func (self *DockerProxy) recoverBuild() error {
	content, err := readBounded(filepath.Join(self.plan.StateDirectory, "build.json"), self.settings.MaximumDocumentBytes)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(self.buildDirectory()); err == nil {
			self.unknown++
		}
		return nil
	}
	if err != nil {
		return err
	}
	record := dockerProxyBuildRecord{}
	if decodeStrict(content, &record) != nil || record.Schema != Schema || record.RunId != self.plan.RunId || !digestPattern.MatchString(record.RequestSha256) {
		return errors.New("owned build journal is corrupt")
	}
	build := &dockerProxyBuild{request: record.Request, requestSha256: record.RequestSha256, contextSha256: record.ContextSha256,
		contextBytes: record.ContextBytes, issued: record.State != "not-issued", finished: true, result: record.Result, done: make(chan struct{})}
	self.operations++
	if record.State != "acknowledged" && record.State != "not-issued" {
		self.unknown++
		build.result.Acknowledged = false
		build.result.ErrorCode = "build_outcome_unknown"
	} else if record.State == "acknowledged" {
		var err error
		build.result.Stdout, err = readBounded(filepath.Join(self.buildDirectory(), "stdout"), self.settings.MaximumBuildOutputBytes)
		if err != nil {
			return err
		}
		build.result.Stderr, err = readBounded(filepath.Join(self.buildDirectory(), "stderr"), self.settings.MaximumBuildOutputBytes)
		if err != nil {
			return err
		}
	}
	if err := self.reserveSpool(record.ContextBytes); err != nil {
		return err
	}
	close(build.done)
	self.build = build
	return nil
}

// The directory belongs only to this fixed single-build run.
func (self *DockerProxy) buildDirectory() string {
	return filepath.Join(self.plan.ScratchDirectory, "build")
}

// Shared content hashing has no mutable process-global state.
func digestDockerBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// The root-pinned shim uses this typed local client; no caller-selected endpoint
// or arbitrary command reaches the owner protocol.
func RequestDockerBuild(ctx context.Context, socket string, request DockerBuildRequest, settings DockerProxySettings) (DockerBuildResult, error) {
	content, err := json.Marshal(request)
	if err != nil || int64(len(content)) > settings.MaximumDocumentBytes {
		return DockerBuildResult{}, errors.New("owned build command exceeds its bound")
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://owner/_urnetwork/build", strings.NewReader(string(content)))
	if err != nil {
		return DockerBuildResult{}, err
	}
	response, err := transport.RoundTrip(httpRequest)
	if err != nil {
		return DockerBuildResult{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, settings.MaximumBuildOutputBytes*4+settings.MaximumDocumentBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(body)) > settings.MaximumBuildOutputBytes*4+settings.MaximumDocumentBytes {
		return DockerBuildResult{}, errors.New("owned build response remains unknown")
	}
	result := DockerBuildResult{}
	if decodeStrict(body, &result) != nil || result.RunId != request.RunId || !result.Acknowledged {
		return result, ErrDockerProxyUnknown
	}
	return result, nil
}
