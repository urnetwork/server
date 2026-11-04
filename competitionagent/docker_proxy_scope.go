// The proxy role is a closed subset of the fixed evaluator's local Docker API.
// Creation requires all run labels and precommitted cgroups; follow-up actions
// are rewritten to inspected immutable ids before an upstream mutation.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

var dockerProxyVersionPattern = regexp.MustCompile("^/v[0-9]+\\.[0-9]+(/.*)$")
var dockerProxyCpuSetPattern = regexp.MustCompile("^[0-9]+([,-][0-9]+)*$")
var dockerProxyMemoryPattern = regexp.MustCompile("^[1-9][0-9]*[kKmMgG]?$")
var dockerProxyCompileExitPattern = regexp.MustCompile("(?s)^(failed to solve: )?process .+ did not complete successfully: exit code:? [1-9][0-9]*$")

const dockerProxyRunLabel = "com.urnetwork.competition.run-id"

// Configuration authority comes from the durable agent, never a worker request.
func validateDockerProxyPlan(plan DockerProxyPlan, settings DockerProxySettings, protected bool) error {
	if plan.Schema != Schema || !digestPattern.MatchString(plan.RunId) || !cleanupUuidPattern.MatchString(plan.JobId) || !cleanupUuidPattern.MatchString(plan.RoundId) || plan.Attempt <= 0 ||
		plan.DeadlineAt.IsZero() || plan.UpstreamSocketPath != "/run/docker.sock" || !dockerProxyCpuSetPattern.MatchString(plan.CpuSet) || !dockerProxyMemoryPattern.MatchString(plan.BuildMemory) ||
		!strings.HasPrefix(plan.BaseImageDigest, "sha256:") || !digestPattern.MatchString(strings.TrimPrefix(plan.BaseImageDigest, "sha256:")) {
		return errors.New("invalid fixed Docker owner identity or resource policy")
	}
	if settings.MaximumOperations <= 0 || settings.MaximumConnections <= 0 || settings.MaximumHeaderBytes <= 0 || settings.MaximumDocumentBytes <= 0 || settings.MaximumRequestBytes <= 0 ||
		settings.MaximumImageLoadBytes <= 0 || settings.MaximumBuildOutputBytes <= 0 || settings.MaximumResponseBytes <= 0 || settings.MaximumSpoolBytes <= 0 || settings.MaximumContextBytes <= 0 || settings.MaximumContextFileBytes <= 0 || settings.MaximumContextEntries <= 0 ||
		settings.MaximumDocumentDepth <= 0 || settings.MaximumContextDepth <= 0 || settings.DirectoryBatch <= 0 || settings.CopyBufferBytes <= 0 || settings.HeaderTimeout <= 0 || settings.UploadTimeout <= 0 || settings.ClientWriteTimeout <= 0 ||
		settings.OperationTimeout <= 0 || settings.BuildTimeout <= 0 || settings.BuildStopGrace <= 0 || settings.QuiesceTimeout <= 0 || settings.DrainAllowance <= 0 || settings.Now == nil {
		return errors.New("Docker owner requires positive bounded settings")
	}
	for _, path := range []string{plan.StateDirectory, plan.ScratchDirectory, plan.SocketPath, plan.BuildSocketPath, plan.ContextRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("Docker role paths must be canonical and private")
		}
	}
	if plan.SocketPath == plan.BuildSocketPath || filepath.Base(plan.ContextRoot) != plan.RunId || pathWithin(plan.ContextRoot, plan.StateDirectory) || pathWithin(plan.StateDirectory, plan.ContextRoot) ||
		pathWithin(plan.ContextRoot, plan.ScratchDirectory) || pathWithin(plan.ScratchDirectory, plan.ContextRoot) || pathWithin(plan.StateDirectory, plan.ScratchDirectory) || pathWithin(plan.ScratchDirectory, plan.StateDirectory) {
		return errors.New("Docker role state, source and sockets are not independent")
	}
	cleanup := cleanupPlan{Schema: Schema, RunId: plan.RunId, JobId: plan.JobId, RoundId: plan.RoundId, Attempt: plan.Attempt, CgroupNames: plan.CgroupNames,
		EvidenceDirectory: filepath.Join(plan.ContextRoot, ".evidence-runtime"), EvidenceSource: "urnetwork-evidence-" + strings.ReplaceAll(plan.JobId, "-", "") + "-" + strconv.Itoa(plan.Attempt)}
	if err := validateCleanupPlan(cleanup, Config{WorkDirectory: filepath.Dir(plan.ContextRoot)}); err != nil {
		return err
	}
	if !cleanupBuildSlicePattern.MatchString(plan.BuildCgroupName) {
		return errors.New("Docker build owner has no exact precommitted build cgroup")
	}
	present := false
	for _, name := range plan.CgroupNames {
		present = present || name == plan.BuildCgroupName
	}
	if !present {
		return errors.New("Docker build cgroup differs from durable cleanup authority")
	}
	for _, pin := range []Pin{plan.Docker, plan.Buildx, plan.Dockerfile} {
		if protected {
			if pin == plan.Dockerfile {
				if err := protectedRootPath(pin.Path, false, false); err != nil {
					return err
				}
				content, err := readBounded(pin.Path, settings.MaximumContextFileBytes)
				if err != nil {
					return err
				}
				if digestDockerBytes(content) != pin.Sha256 {
					return errors.New("owned Dockerfile pin mismatch")
				}
			} else if err := validatePin(pin, true); err != nil {
				return err
			}
		} else if !filepath.IsAbs(pin.Path) || !digestPattern.MatchString(pin.Sha256) {
			return errors.New("invalid Docker test executable pin")
		}
	}
	return nil
}

// Docker API versions do not create another operation vocabulary.
func dockerProxyApiPath(path string) string {
	match := dockerProxyVersionPattern.FindStringSubmatch(path)
	if len(match) == 2 {
		return match[1]
	}
	return path
}

// No daemon-wide pruning, volume allocation, arbitrary builds, plugin loading,
// registry pull/push, host exec or container update API is admitted by this role.
func allowedDockerMutation(method, path string) bool {
	if method == http.MethodPost && (path == "/containers/create" || path == "/networks/create") {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 2 && method == http.MethodDelete && (parts[0] == "containers" || parts[0] == "networks") {
		return true
	}
	if len(parts) == 3 && method == http.MethodPost && parts[0] == "containers" {
		switch parts[2] {
		case "start", "stop", "kill", "restart", "pause", "unpause", "wait", "exec":
			return true
		}
	}
	if len(parts) == 3 && method == http.MethodPost && parts[0] == "networks" && (parts[2] == "connect" || parts[2] == "disconnect") {
		return true
	}
	return len(parts) == 3 && method == http.MethodPost && parts[0] == "images" && parts[2] == "tag"
}

// Every mutable target is either created under this run's authority or resolved
// by an independent daemon inspect to the exact same labels and allowed group.
func (self *DockerProxy) verifyMutationScope(ctx context.Context, r *http.Request, bodyPath string) (string, error) {
	path := dockerProxyApiPath(r.URL.Path)
	if path == "/containers/create" || path == "/networks/create" {
		content, err := readBounded(bodyPath, self.settings.MaximumRequestBytes)
		if err != nil {
			return "", err
		}
		if err := validateDockerMutationJson(content, self.settings.MaximumDocumentDepth); err != nil {
			return "", err
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(content, &object) != nil {
			return "", errors.New("Docker create body is malformed")
		}
		var labels map[string]string
		if json.Unmarshal(object["Labels"], &labels) != nil || !self.labelsMatch(labels) {
			return "", errors.New("Docker create body omitted exact run labels")
		}
		if path == "/containers/create" {
			var host struct {
				CgroupParent string
				Privileged   bool
				Binds        []string
				Mounts       []struct{ Type, Source string }
			}
			if json.Unmarshal(object["HostConfig"], &host) != nil || host.Privileged || !self.allowedCgroup(host.CgroupParent) {
				return "", errors.New("Docker create escaped its precommitted cgroup")
			}
			sources := []string{}
			for _, bind := range host.Binds {
				source, _, _ := strings.Cut(bind, ":")
				sources = append(sources, source)
			}
			for _, mount := range host.Mounts {
				if mount.Type == "bind" {
					sources = append(sources, mount.Source)
				}
			}
			for _, source := range sources {
				if !filepath.IsAbs(source) {
					return "", errors.New("Docker bind source is not absolute")
				}
				resolved, err := filepath.EvalSymlinks(source)
				if err != nil {
					return "", errors.New("Docker bind source could not be confined")
				}
				for _, authority := range []string{self.plan.UpstreamSocketPath, self.plan.SocketPath, self.plan.BuildSocketPath, self.plan.StateDirectory, self.plan.ScratchDirectory} {
					if pathWithin(resolved, authority) {
						return "", errors.New("Docker candidate mount exposes request-owner authority")
					}
				}
			}
		}
		return r.URL.Path, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 {
		return "", errors.New("invalid scoped Docker target")
	}
	if parts[0] == "images" {
		if parts[1] != self.plan.BaseImageDigest || r.URL.Query().Get("repo") != "urnetwork/sim-latency-evaluator-base-id" ||
			r.URL.Query().Get("tag") != strings.TrimPrefix(self.plan.BaseImageDigest, "sha256:") {
			return "", errors.New("Docker image tag is outside the frozen source")
		}
		return r.URL.Path, nil
	}
	network := parts[0] == "networks"
	id, err := self.inspectOwnedDocker(ctx, parts[1], network)
	if err != nil {
		return "", err
	}
	original := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	offset := 0
	if len(original) > 0 && strings.HasPrefix(original[0], "v") && dockerProxyApiPath(r.URL.Path) != r.URL.Path {
		offset = 1
	}
	original[offset+1] = id
	if network && len(parts) == 3 {
		content, err := readBounded(bodyPath, self.settings.MaximumRequestBytes)
		if err != nil {
			return "", err
		}
		var object struct{ Container string }
		if json.Unmarshal(content, &object) != nil {
			return "", errors.New("Docker network attachment body is malformed")
		}
		if _, err := self.inspectOwnedDocker(ctx, object.Container, false); err != nil {
			return "", err
		}
	}
	return "/" + strings.Join(original, "/"), nil
}

// The daemon address remains fixed for ownership reads too.
func (self *DockerProxy) inspectOwnedDocker(ctx context.Context, target string, network bool) (string, error) {
	if target == "" || strings.ContainsAny(target, "/\\\x00\r\n") {
		return "", errors.New("invalid Docker target identity")
	}
	path := "/containers/" + target + "/json"
	if network {
		path = "/networks/" + target
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return "", err
	}
	response, err := self.transport.RoundTrip(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaximumDocumentBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(content)) > self.settings.MaximumDocumentBytes {
		return "", errors.New("Docker target ownership cannot be established")
	}
	var object map[string]json.RawMessage
	var id string
	if json.Unmarshal(content, &object) != nil || json.Unmarshal(object["Id"], &id) != nil || !digestPattern.MatchString(id) {
		return "", errors.New("Docker target inspect identity is malformed")
	}
	labelObject := object
	if !network && json.Unmarshal(object["Config"], &labelObject) != nil {
		return "", errors.New("Docker target omitted container scope")
	}
	var labels map[string]string
	if json.Unmarshal(labelObject["Labels"], &labels) != nil || !self.labelsMatch(labels) {
		return "", errors.New("Docker target belongs to another run")
	}
	if !network {
		var host struct{ CgroupParent string }
		if json.Unmarshal(object["HostConfig"], &host) != nil || !self.allowedCgroup(host.CgroupParent) {
			return "", errors.New("Docker target is outside committed cgroups")
		}
	}
	return id, nil
}

// All three labels are required; a shared competition or job alone is not proof.
func (self *DockerProxy) labelsMatch(labels map[string]string) bool {
	return labels[cleanupJobLabel] == self.plan.JobId && labels[cleanupRoundLabel] == self.plan.RoundId && labels[dockerProxyRunLabel] == self.plan.RunId
}

// No slice prefix, path or descendant wildcard can acquire cleanup authority.
func (self *DockerProxy) allowedCgroup(name string) bool {
	for _, allowed := range self.plan.CgroupNames {
		if name == allowed {
			return true
		}
	}
	return false
}

// A listener's peer identity, not an HTTP header, establishes the socket role.
type dockerProxyPeerKey struct{}

// HTTP/2 is needed for the root-owned build endpoint; the worker remains HTTP/1.
func (self *DockerProxy) httpServer(handler http.Handler, build bool) *http.Server {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	if build {
		protocols.SetUnencryptedHTTP2(true)
	}
	server := &http.Server{Handler: handler, Protocols: protocols, ReadHeaderTimeout: self.settings.HeaderTimeout,
		ReadTimeout: self.settings.UploadTimeout, MaxHeaderBytes: self.settings.MaximumHeaderBytes,
		BaseContext: func(net.Listener) context.Context { return self.ownerCtx },
		HTTP2:       &http.HTTP2Config{MaxConcurrentStreams: self.settings.MaximumConnections},
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			uid := ^uint32(0)
			connection = dockerProxyOriginalConn(connection)
			if unix, ok := connection.(*net.UnixConn); ok {
				if raw, err := unix.SyscallConn(); err == nil {
					_ = raw.Control(func(fd uintptr) {
						if credentials, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); err == nil {
							uid = credentials.Uid
						}
					})
				}
			}
			return context.WithValue(ctx, dockerProxyPeerKey{}, uid)
		}}
	if build {
		server.ReadTimeout = 0
	}
	return server
}

// Private test servers bypass only operating-system peer credentials.
func (self *DockerProxy) authorized(r *http.Request, rootOnly bool) bool {
	if !self.protected {
		return true
	}
	uid, present := r.Context().Value(dockerProxyPeerKey{}).(uint32)
	return present && (uid == 0 || !rootOnly && uid == self.plan.WorkerUid)
}

// The source root must already be a real private directory before snapshotting.
func validateDockerContextRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("Docker context root is not private")
	}
	return nil
}

// Docker's case-insensitive struct decoding must not disagree with our exact
// ownership maps. Reject duplicate/case-alias fields before either decoder runs.
func validateDockerMutationJson(content []byte, maximumDepth int) error {
	if !utf8.Valid(content) || maximumDepth <= 0 {
		return errors.New("Docker mutation JSON is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	var visit func(int) error
	visit = func(depth int) error {
		if depth > maximumDepth {
			return errors.New("Docker mutation JSON exceeds its depth bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok {
					return errors.New("Docker mutation object has an invalid key")
				}
				for _, r := range key {
					if r > 127 {
						return errors.New("Docker mutation helper keys must be ASCII")
					}
				}
				folded := strings.ToLower(key)
				if seen[folded] {
					return errors.New("Docker mutation contains duplicate or case-aliased fields")
				}
				seen[folded] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("Docker mutation has an invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("Docker mutation JSON has trailing data")
	}
	return nil
}
