//go:build linux

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
var dockerPolicySourcePattern = regexp.MustCompile(`^evaluation-sources\.[0-9A-Za-z]{8}/(baseline|candidate)$`)

const dockerProxyRunLabel = "com.urnetwork.competition.run-id"

// Configuration authority comes from the durable agent, never a worker request.
func validateDockerProxyPlan(plan DockerProxyPlan, settings DockerProxySettings, protected bool) error {
	if plan.Schema != Schema || !digestPattern.MatchString(plan.RunId) || !cleanupUuidPattern.MatchString(plan.JobId) || !cleanupUuidPattern.MatchString(plan.RoundId) || plan.Attempt <= 0 ||
		plan.DeadlineAt.IsZero() || plan.WorkerUid == 0 || !digestPattern.MatchString(plan.ResourcePolicySha256) || !dockerProxyCpuSetPattern.MatchString(plan.CpuSet) || !dockerProxyCpuSetPattern.MatchString(plan.ManagementCpuSet) || !dockerProxyMemoryPattern.MatchString(plan.BuildMemory) ||
		!strings.HasPrefix(plan.BaseImageDigest, "sha256:") || !digestPattern.MatchString(strings.TrimPrefix(plan.BaseImageDigest, "sha256:")) {
		return errors.New("invalid fixed Docker owner identity or resource policy")
	}
	if err := validateDockerBuildRetirementPlan(plan, protected); err != nil {
		return err
	}
	if _, err := ValidateCandidateCoreProfiles(plan.StockSeccompProfile, plan.CandidateCoreProfile, settings.CoreProfile, protected); err != nil {
		return err
	}
	if err := validateDockerBackingImages(plan.BackingImages); err != nil {
		return err
	}
	if settings.MaximumOperations <= 0 || settings.MaximumConnections <= 0 || settings.MaximumHeaderBytes <= 0 || settings.MaximumDocumentBytes <= 0 || settings.MaximumRequestBytes <= 0 ||
		settings.MaximumRefusalReasonBytes <= 0 || int64(settings.MaximumRefusalReasonBytes) > settings.MaximumDocumentBytes ||
		settings.MaximumImageLoadBytes <= 0 || settings.MaximumBuildOutputBytes <= 0 || settings.MaximumResponseBytes <= 0 || settings.MaximumSpoolBytes <= 0 || settings.MaximumContextBytes <= 0 || settings.MaximumContextFileBytes <= 0 || settings.MaximumContextEntries <= 0 ||
		settings.MaximumDocumentDepth <= 0 || settings.MaximumContextDepth <= 0 || settings.DirectoryBatch <= 0 || settings.CopyBufferBytes <= 0 || settings.HeaderTimeout <= 0 || settings.UploadTimeout <= 0 || settings.ClientWriteTimeout <= 0 ||
		settings.OperationTimeout <= 0 || settings.BuildTimeout <= 0 || settings.BuildStopGrace <= 0 || settings.QuiesceTimeout <= 0 || settings.DrainAllowance <= 0 || settings.Now == nil ||
		settings.MaximumContainerMemoryBytes <= 0 || settings.MaximumContainerPids <= 0 || settings.MaximumContainerOpenFiles <= 0 ||
		settings.MaximumContainerTmpfsBytes <= 0 || settings.MaximumContainerTmpfsMounts <= 0 || settings.MaximumContainerMounts <= 0 ||
		settings.MaximumCpuIndex <= 0 || settings.DefaultContainerShmBytes <= 0 || settings.DefaultContainerShmBytes > settings.MaximumContainerTmpfsBytes {
		return errors.New("Docker owner requires positive bounded settings")
	}
	evaluation, err := dockerPolicyCpuSet(plan.CpuSet, settings.MaximumCpuIndex)
	if err != nil {
		return err
	}
	management, err := dockerPolicyCpuSet(plan.ManagementCpuSet, settings.MaximumCpuIndex)
	if err != nil {
		return err
	}
	for cpu := range management {
		if evaluation[cpu] {
			return errors.New("Docker evaluation and management CPUs overlap")
		}
	}
	for _, path := range []string{plan.BaseDirectory, plan.UpstreamSocketPath, plan.StateDirectory, plan.ScratchDirectory, plan.SocketPath, plan.BuildSocketPath, plan.ContextRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("Docker role paths must be canonical and private")
		}
	}
	workerSocket, buildSocket, socketErr := dockerRunSocketPaths(filepath.Join(plan.BaseDirectory, "clients"), plan.RunId)
	if socketErr != nil {
		return socketErr
	}
	if !pathWithin(plan.BaseDirectory, plan.StateDirectory) || !pathWithin(plan.BaseDirectory, plan.ScratchDirectory) || !pathWithin(plan.BaseDirectory, plan.ContextRoot) ||
		!pathWithin(plan.BaseDirectory, plan.UpstreamSocketPath) || pathWithin(plan.StateDirectory, plan.UpstreamSocketPath) || pathWithin(plan.ScratchDirectory, plan.UpstreamSocketPath) || pathWithin(plan.ContextRoot, plan.UpstreamSocketPath) ||
		plan.SocketPath != workerSocket || plan.BuildSocketPath != buildSocket {
		return errors.New("Docker owner endpoints differ from the installed private per-run role")
	}
	if len(plan.SocketPath) >= len(syscall.RawSockaddrUnix{}.Path) || len(plan.BuildSocketPath) >= len(syscall.RawSockaddrUnix{}.Path) {
		return errors.New("Docker authority sockets exceed the native Unix address boundary")
	}
	for _, path := range plan.ReadOnlyBindPaths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("Docker readonly fixture path is not canonical")
		}
	}
	if protected {
		if err := validateHostFilesPlan(plan, DefaultHostFilesSettings()); err != nil {
			return err
		}
		for _, path := range []string{filepath.Dir(plan.StateDirectory), filepath.Dir(plan.ScratchDirectory), filepath.Dir(plan.ContextRoot), filepath.Dir(plan.SocketPath), filepath.Dir(plan.UpstreamSocketPath)} {
			if err := protectedRuntimePath(path, plan.BaseDirectory, plan.TrustedOperatorUid); err != nil {
				return err
			}
		}
	}
	if plan.SocketPath == plan.BuildSocketPath || filepath.Base(plan.ContextRoot) != plan.RunId || pathWithin(plan.ContextRoot, plan.StateDirectory) || pathWithin(plan.StateDirectory, plan.ContextRoot) ||
		pathWithin(plan.ContextRoot, plan.ScratchDirectory) || pathWithin(plan.ScratchDirectory, plan.ContextRoot) || pathWithin(plan.StateDirectory, plan.ScratchDirectory) || pathWithin(plan.ScratchDirectory, plan.StateDirectory) {
		return errors.New("Docker role state, source and sockets are not independent")
	}
	if filepath.Base(plan.StateDirectory) != "docker-owner" || filepath.Base(filepath.Dir(plan.StateDirectory)) != plan.RunId || filepath.Base(plan.ScratchDirectory) != plan.RunId {
		return errors.New("Docker journal and scratch paths differ from the fixed run hierarchy")
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
		content, err := readBounded(bodyPath, self.settings.MaximumDocumentBytes)
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
			if err := self.verifyContainerCreate(ctx, object); err != nil {
				return "", err
			}
		} else if err := self.verifyNetworkPolicy(object, true); err != nil {
			return "", err
		}
		return r.URL.Path, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 {
		return "", errors.New("invalid scoped Docker target")
	}
	if parts[0] == "containers" && len(parts) == 3 && parts[2] == "exec" {
		content, err := readBounded(bodyPath, self.settings.MaximumDocumentBytes)
		if err != nil || validateDockerMutationJson(content, self.settings.MaximumDocumentDepth) != nil {
			return "", errors.New("Docker exec configuration is invalid or unbounded")
		}
		var config struct {
			Privileged bool
			User       string
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(content, &config) != nil || json.Unmarshal(content, &object) != nil ||
			dockerPolicyFields(object, "AttachStdin AttachStdout AttachStderr DetachKeys Tty Env Cmd WorkingDir Privileged User ConsoleSize") != nil ||
			config.Privileged || config.User != "" {
			return "", errors.New("Docker exec cannot override container privilege or user identity")
		}
	}
	if parts[0] == "images" {
		// The Docker CLI normalizes the shorthand alias to Docker Hub's
		// explicit domain. Both name the same single offline daemon alias.
		repository := r.URL.Query().Get("repo")
		if parts[1] != self.plan.BaseImageDigest ||
			(repository != "urnetwork/sim-latency-evaluator-base-id" && repository != "docker.io/urnetwork/sim-latency-evaluator-base-id") ||
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

// The real Docker29 client serializes many zero-valued host fields. Unknown
// nonzero options are never treated as defaults or allowed capabilities.
func dockerPolicyFields(object map[string]json.RawMessage, allowedNames string) error {
	allowed := map[string]bool{}
	for _, name := range strings.Fields(allowedNames) {
		allowed[name] = true
	}
	for name, raw := range object {
		if !allowed[name] && !dockerPolicyZero(raw) {
			return errors.New("Docker request contains an unreviewed nonzero option")
		}
	}
	return nil
}

// Numeric zero is checked lexically, not by a float conversion that could
// underflow a nonzero exponent into a false default.
func dockerPolicyZero(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		return false
	}
	var zero func(any) bool
	zero = func(value any) bool {
		switch value := value.(type) {
		case nil:
			return true
		case bool:
			return !value
		case string:
			return value == ""
		case json.Number:
			mantissa := strings.FieldsFunc(value.String(), func(r rune) bool { return r == 'e' || r == 'E' })[0]
			for _, r := range mantissa {
				if r != '-' && r != '.' && r != '0' {
					return false
				}
			}
			return true
		case []any:
			for _, entry := range value {
				if !zero(entry) {
					return false
				}
			}
			return true
		case map[string]any:
			for _, entry := range value {
				if !zero(entry) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return zero(value)
}

// Empty collections are defaults, but a nonempty map with empty values can
// still request a volume, port binding, device rule or runtime annotation.
func dockerPolicyEmptyCollection(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value := value.(type) {
	case nil:
		return true
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		return false
	}
}

// This closed host vocabulary is the intersection used by direct probes and
// frozen full-R9 Compose roles, not a generic Docker capability pass-through.
type dockerPolicyHostConfig struct {
	CgroupParent, CgroupnsMode, NetworkMode, Runtime string
	PidMode, IpcMode, UtsMode, UsernsMode            string
	ReadonlyRootfs, Privileged                       bool
	CapAdd, CapDrop, SecurityOpt, Binds              []string
	Memory, MemorySwap, ShmSize                      int64
	MemorySwappiness                                 *int64
	PidsLimit                                        *int64
	CpusetCpus                                       string
	Ulimits                                          []struct {
		Name       string
		Soft, Hard int64
	}
	Sysctls, Tmpfs             map[string]string
	MaskedPaths, ReadonlyPaths []string
	Mounts                     []json.RawMessage
	RestartPolicy              struct {
		Name              string
		MaximumRetryCount int
	}
}

// Default/private namespaces remain private. Even an explicitly empty mask
// array is rejected because Docker interprets it as disabling default guards.
func (self *DockerProxy) verifyContainerCreate(ctx context.Context, object map[string]json.RawMessage) error {
	if !dockerPolicyEmptyCollection(object["Volumes"]) {
		return errors.New("Docker create requested an anonymous or named volume")
	}
	if err := dockerPolicyFields(object, "Hostname Domainname User AttachStdin AttachStdout AttachStderr ExposedPorts Tty OpenStdin StdinOnce Env Cmd Healthcheck ArgsEscaped Image WorkingDir Entrypoint Labels StopSignal StopTimeout HostConfig NetworkingConfig"); err != nil {
		return err
	}
	var config struct{ Image, User string }
	if json.Unmarshal(mustDockerPolicyObject(object), &config) != nil || config.Image == "" ||
		config.User != "" && config.User != "65532:65532" && config.User != "999:999" {
		return errors.New("Docker container has an invalid image or fixed-role user")
	}
	var hostObject map[string]json.RawMessage
	var host dockerPolicyHostConfig
	if json.Unmarshal(object["HostConfig"], &hostObject) != nil || hostObject == nil ||
		dockerPolicyFields(hostObject, "CgroupParent CgroupnsMode NetworkMode Runtime PidMode IpcMode UTSMode UsernsMode ReadonlyRootfs Privileged CapAdd CapDrop SecurityOpt Binds Memory MemorySwap MemorySwappiness ShmSize PidsLimit CpusetCpus Ulimits Sysctls Tmpfs MaskedPaths ReadonlyPaths Mounts RestartPolicy AutoRemove") != nil ||
		json.Unmarshal(object["HostConfig"], &host) != nil || !self.allowedCgroup(host.CgroupParent) || host.Privileged || !host.ReadonlyRootfs ||
		host.PidMode != "" || host.UtsMode != "" || host.UsernsMode != "" || host.CgroupnsMode != "" && host.CgroupnsMode != "private" ||
		host.IpcMode != "" && host.IpcMode != "private" && host.IpcMode != "none" || host.Runtime != "" && host.Runtime != "runc" ||
		len(host.CapAdd) != 0 || len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" ||
		host.MaskedPaths != nil || host.ReadonlyPaths != nil || host.RestartPolicy.Name != "" && host.RestartPolicy.Name != "no" || host.RestartPolicy.MaximumRetryCount != 0 {
		return errors.New("Docker container changed its closed namespace or privilege policy")
	}
	if err := self.verifyCandidateCoreSecurity(host.SecurityOpt); err != nil {
		return err
	}
	for _, name := range []string{"Devices", "DeviceRequests", "DeviceCgroupRules", "VolumesFrom", "GroupAdd", "Links", "Annotations", "StorageOpt",
		"PortBindings", "Dns", "DnsOptions", "DnsSearch", "ExtraHosts"} {
		if !dockerPolicyEmptyCollection(hostObject[name]) {
			return errors.New("Docker create requested a nonempty host capability collection")
		}
	}
	if err := self.requireBuildRetired(ctx, host.CgroupParent); err != nil {
		return err
	}
	if host.Memory <= 0 || host.Memory > self.settings.MaximumContainerMemoryBytes || host.MemorySwap != host.Memory ||
		host.PidsLimit == nil || *host.PidsLimit <= 0 || *host.PidsLimit > self.settings.MaximumContainerPids {
		return errors.New("Docker container omitted a finite memory, swap or PID ceiling")
	}
	// Docker CLI serializes its unset swappiness as -1, not null. This sentinel
	// never grants swap: the equal memory/swap ceiling above remains mandatory.
	if host.MemorySwappiness != nil && *host.MemorySwappiness != -1 && *host.MemorySwappiness != 0 {
		return errors.New("Docker container changed its inherited swappiness default")
	}
	requested, err := dockerPolicyCpuSet(host.CpusetCpus, self.settings.MaximumCpuIndex)
	if err != nil {
		return err
	}
	evaluation, err := dockerPolicyCpuSet(self.plan.CpuSet, self.settings.MaximumCpuIndex)
	if err != nil {
		return err
	}
	management, err := dockerPolicyCpuSet(self.plan.ManagementCpuSet, self.settings.MaximumCpuIndex)
	if err != nil {
		return err
	}
	evaluationOnly, managementOnly := true, true
	for cpu := range requested {
		evaluationOnly = evaluationOnly && evaluation[cpu]
		managementOnly = managementOnly && management[cpu]
	}
	if !evaluationOnly && !managementOnly {
		return errors.New("Docker container CPU set escapes both frozen role sets")
	}
	coreFound := false
	limits := map[string]bool{}
	for _, limit := range host.Ulimits {
		if limits[limit.Name] {
			return errors.New("Docker container repeated a resource limit")
		}
		limits[limit.Name] = true
		switch limit.Name {
		case "core":
			if limit.Soft != 1 || limit.Hard != 1 {
				return errors.New("Docker candidate core limits are not both one")
			}
			coreFound = true
		case "nofile":
			if limit.Soft <= 0 || limit.Hard < limit.Soft || limit.Hard > self.settings.MaximumContainerOpenFiles {
				return errors.New("Docker container file limit exceeds the frozen ceiling")
			}
		default:
			return errors.New("Docker container requested an unreviewed resource limit")
		}
	}
	if !coreFound {
		return errors.New("Docker candidate omitted its core-suppression limits")
	}
	for name, value := range host.Sysctls {
		if name != "net.ipv6.conf.all.disable_ipv6" && name != "net.ipv6.conf.default.disable_ipv6" || value != "1" {
			return errors.New("Docker container requested an unreviewed kernel setting")
		}
	}
	if len(host.Binds)+len(host.Mounts)+len(host.Tmpfs) > self.settings.MaximumContainerMounts {
		return errors.New("Docker container mount count exceeds its finite bound")
	}
	targets := map[string]bool{}
	addTarget := func(target string) error {
		if !filepath.IsAbs(target) || filepath.Clean(target) != target || target == "/" || strings.ContainsAny(target, "\x00\r\n") || targets[target] {
			return errors.New("Docker container mount target is invalid or duplicated")
		}
		targets[target] = true
		return nil
	}
	for _, bind := range host.Binds {
		fields := strings.Split(bind, ":")
		if len(fields) != 3 || addTarget(fields[1]) != nil {
			return errors.New("Docker legacy bind must have an explicit finite mode")
		}
		readOnly, modeFound := false, false
		for _, option := range strings.Split(fields[2], ",") {
			switch option {
			case "ro", "rw":
				if modeFound {
					return errors.New("Docker bind repeated its access mode")
				}
				readOnly, modeFound = option == "ro", true
			case "rprivate":
			default:
				return errors.New("Docker bind propagation or option is unreviewed")
			}
		}
		if !modeFound || self.verifyDockerBind(fields[0], fields[1], readOnly) != nil {
			return errors.New("Docker legacy bind escapes the finite source role")
		}
	}
	tmpfsBytes := host.ShmSize
	if tmpfsBytes == 0 {
		tmpfsBytes = self.settings.DefaultContainerShmBytes
	}
	if tmpfsBytes <= 0 || tmpfsBytes > self.settings.MaximumContainerTmpfsBytes {
		return errors.New("Docker shared memory is unbounded")
	}
	tmpfsCount := len(host.Tmpfs)
	for target, options := range host.Tmpfs {
		if err := addTarget(target); err != nil {
			return err
		}
		size, err := self.verifyDockerTmpfs(target, options)
		if err != nil || size > self.settings.MaximumContainerTmpfsBytes-tmpfsBytes {
			return errors.New("Docker tmpfs policy exceeds its finite ceiling")
		}
		tmpfsBytes += size
	}
	for _, raw := range host.Mounts {
		var mount struct {
			Type, Source, Target, Consistency string
			ReadOnly                          bool
			BindOptions                       *struct {
				Propagation                                                                  string
				NonRecursive, CreateMountpoint, ReadOnlyNonRecursive, ReadOnlyForceRecursive bool
			}
			TmpfsOptions *struct {
				SizeBytes int64
				Mode      uint32
				Options   [][]string
			}
		}
		var mountObject map[string]json.RawMessage
		if json.Unmarshal(raw, &mountObject) != nil || json.Unmarshal(raw, &mount) != nil ||
			dockerPolicyFields(mountObject, "Type Source Target ReadOnly Consistency BindOptions TmpfsOptions") != nil || addTarget(mount.Target) != nil ||
			mount.Consistency != "" && mount.Consistency != "consistent" && mount.Consistency != "default" {
			return errors.New("Docker mount changed the closed source vocabulary")
		}
		switch mount.Type {
		case "bind":
			if !dockerPolicyEmptyCollection(mountObject["VolumeOptions"]) || !dockerPolicyEmptyCollection(mountObject["ImageOptions"]) ||
				mount.TmpfsOptions != nil || mount.BindOptions != nil && (mount.BindOptions.Propagation != "" && mount.BindOptions.Propagation != "rprivate" ||
				mount.BindOptions.CreateMountpoint || mount.BindOptions.ReadOnlyNonRecursive) {
				return errors.New("Docker bind changed propagation or creation authority")
			}
			if mount.BindOptions != nil {
				var options map[string]json.RawMessage
				if json.Unmarshal(mountObject["BindOptions"], &options) != nil || dockerPolicyFields(options, "Propagation NonRecursive CreateMountpoint ReadOnlyNonRecursive ReadOnlyForceRecursive") != nil {
					return errors.New("Docker bind contains an unreviewed option")
				}
			}
			if err := self.verifyDockerBind(mount.Source, mount.Target, mount.ReadOnly); err != nil {
				return err
			}
		case "tmpfs":
			if mount.Source != "" || mount.ReadOnly || mount.BindOptions != nil || mount.TmpfsOptions == nil {
				return errors.New("Docker tmpfs contains another mount capability")
			}
			var options map[string]json.RawMessage
			if json.Unmarshal(mountObject["TmpfsOptions"], &options) != nil || dockerPolicyFields(options, "SizeBytes Mode Options") != nil {
				return errors.New("Docker tmpfs contains an unreviewed option")
			}
			pieces := []string{"size=" + strconv.FormatInt(mount.TmpfsOptions.SizeBytes, 10), "mode=" + strconv.FormatUint(uint64(mount.TmpfsOptions.Mode), 8)}
			for _, option := range mount.TmpfsOptions.Options {
				if len(option) < 1 || len(option) > 2 {
					return errors.New("Docker tmpfs option is malformed")
				}
				pieces = append(pieces, strings.Join(option, "="))
			}
			size, err := self.verifyDockerTmpfs(mount.Target, strings.Join(pieces, ","))
			if err != nil || size > self.settings.MaximumContainerTmpfsBytes-tmpfsBytes {
				return errors.New("Docker structured tmpfs exceeds its finite ceiling")
			}
			tmpfsCount++
			tmpfsBytes += size
		default:
			return errors.New("Docker named, external or image volumes are not admitted")
		}
	}
	if tmpfsCount > self.settings.MaximumContainerTmpfsMounts || tmpfsBytes > host.Memory {
		return errors.New("Docker tmpfs count or total exceeds its memory budget")
	}
	if err := self.verifyCandidateSourceBinding(ctx, object, host); err != nil {
		return err
	}
	return self.verifyContainerNetworking(ctx, host.NetworkMode, object["NetworkingConfig"])
}

// This conversion is only for a bounded already-decoded create object.
func mustDockerPolicyObject(object map[string]json.RawMessage) []byte {
	content, _ := json.Marshal(object)
	return content
}

// CPU expansion is bounded before iteration; mixed evaluation/management or
// duplicate/range aliases cannot acquire another processor through formatting.
func dockerPolicyCpuSet(value string, maximum int) (map[int]bool, error) {
	if value == "" || maximum <= 0 || !dockerProxyCpuSetPattern.MatchString(value) {
		return nil, errors.New("Docker CPU set is absent or malformed")
	}
	cpus := map[int]bool{}
	for _, part := range strings.Split(value, ",") {
		first, last, ranged := strings.Cut(part, "-")
		start, err := strconv.Atoi(first)
		if err != nil || start < 0 || start > maximum || strconv.Itoa(start) != first {
			return nil, errors.New("Docker CPU index exceeds its finite bound")
		}
		end := start
		if ranged {
			end, err = strconv.Atoi(last)
			if err != nil || end < start || end > maximum || strconv.Itoa(end) != last {
				return nil, errors.New("Docker CPU range exceeds its finite bound")
			}
		}
		for cpu := start; cpu <= end; cpu++ {
			if cpus[cpu] {
				return nil, errors.New("Docker CPU set contains duplicates")
			}
			cpus[cpu] = true
		}
	}
	return cpus, nil
}

// Authority is rejected in both directions, including an otherwise allowed
// fixture path that accidentally points below an owner socket or journal root.
func (self *DockerProxy) verifyDockerBind(source, target string, readOnly bool) error {
	if !filepath.IsAbs(source) || filepath.Clean(source) != source || source == "/" || strings.ContainsAny(source, "\x00\r\n") {
		return errors.New("Docker bind source is not canonical")
	}
	for _, authority := range []string{self.plan.StateDirectory, self.plan.ScratchDirectory, filepath.Dir(self.plan.UpstreamSocketPath),
		filepath.Dir(filepath.Dir(self.plan.SocketPath)), filepath.Dir(filepath.Dir(self.plan.StateDirectory)), filepath.Dir(self.plan.ScratchDirectory)} {
		if pathWithin(source, authority) || pathWithin(authority, source) {
			return errors.New("Docker bind exposes a parent or child of owner authority")
		}
	}
	for current := source; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || current == source && !info.IsDir() && !info.Mode().IsRegular() || current != source && !info.IsDir() {
			return errors.New("Docker bind source contains a symlink or special file")
		}
		if current == "/" {
			break
		}
	}
	evidence := filepath.Join(self.plan.ContextRoot, ".evidence-runtime")
	relative, err := filepath.Rel(evidence, source)
	if err != nil {
		return err
	}
	relative = filepath.ToSlash(relative)
	if !readOnly {
		if relative == "tracked-build/check-cache" || relative == "tracked-build/compile-cache" {
			if target != "/opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v" {
				return errors.New("tracked compiler metadata changed its only writable cache leaf")
			}
			return nil
		}
		if relative == "tracked-build/output" {
			if target != "/candidate-output" {
				return errors.New("tracked compiler output changed its exact destination")
			}
			return nil
		}
		if !hostFilesOutputPattern.MatchString(relative) && relative != "baseline-score-output" && relative != "candidate-score-output" ||
			target != "/artifacts" && target != "/score-output" {
			return errors.New("Docker writable bind is not an exact full-R9 output")
		}
		return nil
	}
	if relative == "input" && target == "/input" || relative == "scorer-input" && target == "/artifacts" || dockerPolicySourcePattern.MatchString(relative) && target == "/workspace" {
		return nil
	}
	for _, allowed := range self.plan.ReadOnlyBindPaths {
		if source == allowed && (target == "/runtime/config/local" || target == "/runtime/vault/local" || target == "/docker-entrypoint-initdb.d/10-competition-app.sh") {
			return nil
		}
	}
	return errors.New("Docker readonly bind is outside the exact frozen source inventory")
}

// The frozen roles use only these private memory filesystems. A size, mapped
// uid/gid, non-suid/non-device mode and finite total are required on every one.
func (self *DockerProxy) verifyDockerTmpfs(target, value string) (int64, error) {
	switch target {
	case "/tmp", "/work", "/data", "/var/lib/postgresql", "/var/run/postgresql":
	default:
		return 0, errors.New("Docker tmpfs target is not a frozen role path")
	}
	options := map[string]string{}
	for _, piece := range strings.Split(value, ",") {
		name, value, hasValue := strings.Cut(piece, "=")
		if _, exists := options[name]; exists {
			return 0, errors.New("Docker tmpfs option is duplicated")
		}
		switch name {
		case "rw", "nosuid", "nodev", "noexec":
			if hasValue {
				return 0, errors.New("Docker tmpfs flag has an unexpected value")
			}
			options[name] = "true"
		case "size", "uid", "gid", "mode":
			if !hasValue || value == "" {
				return 0, errors.New("Docker tmpfs bound is absent")
			}
			options[name] = value
		default:
			return 0, errors.New("Docker tmpfs requested an unreviewed host option")
		}
	}
	mode, err := strconv.ParseUint(options["mode"], 8, 12)
	if err != nil || mode != 0700 && mode != 0750 && mode != 01770 || options["rw"] != "true" || options["nosuid"] != "true" || options["nodev"] != "true" ||
		options["uid"] != "65532" && options["uid"] != "999" || options["gid"] != options["uid"] {
		return 0, errors.New("Docker tmpfs changed its mapped ownership or safety flags")
	}
	size := options["size"]
	scale := int64(1)
	if len(size) > 0 {
		switch size[len(size)-1] {
		case 'k', 'K':
			scale, size = 1024, size[:len(size)-1]
		case 'm', 'M':
			scale, size = 1024*1024, size[:len(size)-1]
		case 'g', 'G':
			scale, size = 1024*1024*1024, size[:len(size)-1]
		}
	}
	count, err := strconv.ParseInt(size, 10, 64)
	if err != nil || count <= 0 || count > self.settings.MaximumContainerTmpfsBytes/scale {
		return 0, errors.New("Docker tmpfs size exceeds its finite bound")
	}
	return count * scale, nil
}

// The driver and isolated-gateway options prohibit host reachability even
// before the independent private-network-namespace proof is considered.
func (self *DockerProxy) verifyNetworkPolicy(object map[string]json.RawMessage, create bool) error {
	if create && dockerPolicyFields(object, "Name CheckDuplicate Driver Scope EnableIPv4 EnableIPv6 IPAM Internal Attachable Ingress ConfigOnly ConfigFrom Options Labels") != nil {
		return errors.New("Docker network create contains an unreviewed option")
	}
	var network struct {
		Name, Driver, Scope                       string
		EnableIpv4                                *bool `json:"EnableIPv4"`
		EnableIpv6                                bool  `json:"EnableIPv6"`
		Internal, Attachable, Ingress, ConfigOnly bool
		ConfigFrom                                struct{ Network string }
		Options                                   map[string]string
		Ipam                                      struct {
			Driver  string
			Options map[string]string
			Config  []json.RawMessage
		} `json:"IPAM"`
	}
	if json.Unmarshal(mustDockerPolicyObject(object), &network) != nil || network.Name == "" || strings.ContainsAny(network.Name, "/\\\x00\r\n \t") ||
		network.Name == "host" || network.Name == "none" || network.Name == "bridge" || network.Driver != "bridge" ||
		network.Scope != "local" && (!create || network.Scope != "") || !network.Internal || network.Attachable || network.Ingress || network.ConfigOnly ||
		network.ConfigFrom.Network != "" || network.EnableIpv6 || network.EnableIpv4 != nil && !*network.EnableIpv4 ||
		len(network.Options) != 3 || network.Options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" ||
		network.Options["com.docker.network.bridge.gateway_mode_ipv6"] != "isolated" || network.Options["com.docker.network.bridge.name"] != "unev-"+self.plan.RunId[:10] {
		return errors.New("Docker network changed its isolated local bridge policy")
	}
	if create && (network.Ipam.Driver != "" && network.Ipam.Driver != "default" || len(network.Ipam.Options) != 0 || len(network.Ipam.Config) != 0) {
		return errors.New("Docker network requested an external or static IPAM policy")
	}
	return nil
}

// Only none or an independently inspected exact owned internal network is
// valid. Endpoint options may name aliases, never host routes or another net.
func (self *DockerProxy) verifyContainerNetworking(ctx context.Context, mode string, raw json.RawMessage) error {
	var config struct{ EndpointsConfig map[string]json.RawMessage }
	if len(raw) != 0 && json.Unmarshal(raw, &config) != nil {
		return errors.New("Docker endpoint configuration is malformed")
	}
	var object map[string]json.RawMessage
	if len(raw) != 0 && json.Unmarshal(raw, &object) != nil || dockerPolicyFields(object, "EndpointsConfig") != nil {
		return errors.New("Docker endpoint configuration contains an unreviewed option")
	}
	if mode == "none" {
		if len(config.EndpointsConfig) != 0 {
			return errors.New("network-none Docker container requested endpoints")
		}
		return nil
	}
	if mode == "" || mode == "default" || mode == "bridge" || mode == "host" || strings.Contains(mode, ":") {
		return errors.New("Docker container requested an implicit or shared network namespace")
	}
	id, err := self.inspectOwnedDocker(ctx, mode, true)
	if err != nil {
		return err
	}
	if len(config.EndpointsConfig) > 1 {
		return errors.New("Docker container requested multiple networks")
	}
	for name, raw := range config.EndpointsConfig {
		var endpoint map[string]json.RawMessage
		var identity struct {
			NetworkId string `json:"NetworkID"`
		}
		if name != mode && name != id || json.Unmarshal(raw, &endpoint) != nil ||
			dockerPolicyFields(endpoint, "Aliases DNSNames NetworkID") != nil || !dockerPolicyEmptyCollection(endpoint["DriverOpts"]) ||
			!dockerPolicyEmptyCollection(endpoint["Links"]) || json.Unmarshal(raw, &identity) != nil || identity.NetworkId != "" && identity.NetworkId != id {
			return errors.New("Docker container endpoint escapes its owned internal network")
		}
	}
	return nil
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
	} else if err := self.verifyNetworkPolicy(object, false); err != nil {
		return "", err
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
