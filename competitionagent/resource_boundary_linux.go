// Candidate writes occupy one independently bounded loop filesystem. Protected
// configuration is not proof: admission also checks kernel mount/block facts,
// subordinate-id allocation and the live scoped network guard without writes.
package competitionagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// These values come only from the protected agent role, never the wire.
type RuntimeResourceScope struct {
	BaseDirectory      string
	StateDirectory     string
	WorkDirectory      string
	ScratchDirectory   string
	TrustedOperatorUid uint32
	WorkerUid          uint32
	WorkerGid          uint32
}

// Every potentially large read, inventory or command has an owner-local bound.
// Nonpositive values refuse admission, rather than selecting another default.
type RuntimeResourceBoundarySettings struct {
	DocumentBytes            int64
	NativeBytes              int64
	ExecutableBytes          int64
	MountCount               int
	DocumentDepth            int
	CommandTimeout           time.Duration
	ContainerLogMaximumSize  string
	ContainerLogMaximumFiles int
}

// Native inventories exceed a small configuration but remain strictly bounded.
func DefaultRuntimeResourceBoundarySettings() RuntimeResourceBoundarySettings {
	return RuntimeResourceBoundarySettings{DocumentBytes: 128 * 1024, NativeBytes: 4 * 1024 * 1024,
		ExecutableBytes: 512 * 1024 * 1024, MountCount: 16384, DocumentDepth: 64, CommandTimeout: 10 * time.Second,
		ContainerLogMaximumSize: "8m", ContainerLogMaximumFiles: 2}
}

// The protected manifest pins a separate engine, all of its writable roots and
// its host-id allocation. Container identity still requires a native uid_map
// and gid_map probe before handing off output; this document cannot prove that.
type RuntimeResourceBoundary struct {
	Schema                  int      `json:"schema"`
	BaseDirectory           string   `json:"base_directory"`
	CandidateRoot           string   `json:"candidate_root"`
	BackingFile             string   `json:"backing_file"`
	BlockDevice             string   `json:"block_device"`
	MaximumBytes            uint64   `json:"maximum_bytes"`
	MinimumFreeBytes        uint64   `json:"minimum_free_bytes"`
	MinimumBackingFreeBytes uint64   `json:"minimum_backing_free_bytes"`
	PrivateSocketPath       string   `json:"private_socket_path"`
	ContainerdSocketPath    string   `json:"containerd_socket_path"`
	DockerDataRoot          string   `json:"docker_data_root"`
	ContainerdRoot          string   `json:"containerd_root"`
	EngineExecRoot          string   `json:"engine_exec_root"`
	ScratchRoot             string   `json:"scratch_root"`
	WorkRoot                string   `json:"work_root"`
	UserNamespace           string   `json:"user_namespace"`
	ContainerRootUid        uint32   `json:"container_root_uid"`
	ContainerRootGid        uint32   `json:"container_root_gid"`
	ContainerIdMapLength    uint32   `json:"container_id_map_length"`
	RuntimeName             string   `json:"runtime_name"`
	RuntimeArguments        []string `json:"runtime_arguments"`
	Dockerd                 Pin      `json:"dockerd"`
	Containerd              Pin      `json:"containerd"`
	Runc                    Pin      `json:"runc"`
	ContainerdShim          Pin      `json:"containerd_shim"`
	SocketModeHelper        Pin      `json:"socket_mode_helper"`
	Nftables                Pin      `json:"nftables"`
	EngineConfig            Pin      `json:"engine_config"`
	ContainerdConfig        Pin      `json:"containerd_config"`
	NetworkPolicy           Pin      `json:"network_policy"`
	WorkDeviceId            uint64   `json:"-"`
}

// Test seams replace native facts, not the closed schema or its policy checks.
type runtimeBoundaryHost interface {
	Document(Pin, int64) ([]byte, error)
	Executable(Pin) error
	Directory(string, RuntimeResourceScope) error
	Kernel(context.Context, RuntimeResourceBoundary, RuntimeResourceScope, RuntimeResourceBoundarySettings) (runtimeBoundaryFacts, error)
	Daemon(context.Context, RuntimeResourceBoundary, RuntimeResourceBoundarySettings) ([]byte, error)
	Network(context.Context, RuntimeResourceBoundary, RuntimeResourceBoundarySettings) ([]byte, error)
}

// Device identities and loop backing provenance are captured from descriptors.
type runtimeBoundaryFacts struct {
	MountInfo, MountInfoAfter                    []byte
	SubUids, SubGids                             []byte
	BlockDeviceId, BlockBytes                    uint64
	LoopBackingDevice, LoopBackingInode          uint64
	LoopOffset, LoopSizeLimit                    uint64
	LoopNumber, LoopFlags                        uint32
	BackingDevice, BackingInode                  uint64
	BackingBytes, BackingAllocatedBytes          uint64
	BackingFilesystemType                        int64
	FilesystemType                               int64
	FilesystemBytes, FreeBytes, BackingFreeBytes uint64
	PathDeviceIds                                map[string]uint64
}

var runtimeBoundaryLoopPattern = regexp.MustCompile(`^/dev/loop(0|[1-9][0-9]*)$`)
var runtimeBoundaryUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

const runtimeBoundaryExt4 = 0xef53
const runtimeBoundaryXfs = 0x58465342
const runtimeBoundaryNftTable = "urnetwork_subeval"
const runtimeBoundaryBridgePattern = "unev-*"

// A successful result is a fresh admission proof, not a renewable lease. Call
// again before each launch; no fallback to the user's daemon or host storage.
func VerifyRuntimeResourceBoundary(ctx context.Context, pin Pin, scope RuntimeResourceScope, settings RuntimeResourceBoundarySettings) (*RuntimeResourceBoundary, error) {
	return verifyRuntimeResourceBoundary(ctx, pin, scope, settings, runtimeBoundaryNativeHost{})
}

// The concrete public path always installs native protected-file/kernel checks.
func verifyRuntimeResourceBoundary(ctx context.Context, pin Pin, scope RuntimeResourceScope, settings RuntimeResourceBoundarySettings, host runtimeBoundaryHost) (*RuntimeResourceBoundary, error) {
	if settings.DocumentBytes <= 0 || settings.NativeBytes <= 0 || settings.ExecutableBytes <= 0 || settings.MountCount <= 0 || settings.DocumentDepth <= 0 || settings.CommandTimeout <= 0 ||
		!dockerProxyMemoryPattern.MatchString(settings.ContainerLogMaximumSize) || settings.ContainerLogMaximumFiles <= 0 {
		return nil, errors.New("resource boundary requires positive finite settings")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := host.Document(pin, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	boundary := &RuntimeResourceBoundary{}
	if int64(len(content)) > settings.DocumentBytes || validateDocumentKeys(content) != nil || decodeStrict(content, boundary) != nil {
		return nil, errors.New("invalid protected resource boundary")
	}
	if err := boundary.validate(scope); err != nil {
		return nil, err
	}
	for _, executable := range []Pin{boundary.Dockerd, boundary.Containerd, boundary.ContainerdShim, boundary.Runc, boundary.SocketModeHelper, boundary.Nftables} {
		if err := host.Executable(executable); err != nil {
			return nil, err
		}
	}
	for _, directory := range []string{scope.BaseDirectory, scope.StateDirectory, boundary.CandidateRoot, boundary.DockerDataRoot, boundary.ContainerdRoot,
		boundary.EngineExecRoot, boundary.WorkRoot, boundary.ScratchRoot, filepath.Dir(boundary.PrivateSocketPath), filepath.Dir(boundary.ContainerdSocketPath)} {
		if err := host.Directory(directory, scope); err != nil {
			return nil, err
		}
	}
	for _, directory := range runtimeBoundaryTemporaryDirectories(*boundary) {
		if err := host.Directory(directory, scope); err != nil {
			return nil, err
		}
	}
	if err := host.Directory(filepath.Join(boundary.CandidateRoot, "daemon-logs"), scope); err != nil {
		return nil, err
	}
	engine, err := host.Document(boundary.EngineConfig, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	containerd, err := host.Document(boundary.ContainerdConfig, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	policy, err := host.Document(boundary.NetworkPolicy, settings.DocumentBytes)
	if err != nil {
		return nil, err
	}
	if err := validateRuntimeBoundaryDocuments(*boundary, engine, containerd, policy, settings); err != nil {
		return nil, err
	}
	facts, err := host.Kernel(ctx, *boundary, scope, settings)
	if err != nil {
		return nil, err
	}
	if err := validateRuntimeBoundaryFacts(*boundary, scope, settings, facts); err != nil {
		return nil, err
	}
	boundary.WorkDeviceId = facts.BlockDeviceId
	daemon, err := host.Daemon(ctx, *boundary, settings)
	if err != nil {
		return nil, err
	}
	if err := validateRuntimeBoundaryDaemon(daemon, *boundary, settings); err != nil {
		return nil, err
	}
	network, err := host.Network(ctx, *boundary, settings)
	if err != nil {
		return nil, err
	}
	if err := validateRuntimeBoundaryNetwork(network, settings); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return boundary, nil
}

// All mutable engine paths are distinct children of the one quota filesystem.
// Socket/control state and durable agent journals cannot consume that budget.
func (self *RuntimeResourceBoundary) validate(scope RuntimeResourceScope) error {
	if self.Schema != Schema || scope.WorkerUid == 0 || scope.WorkerGid == 0 || self.BaseDirectory != scope.BaseDirectory || self.WorkRoot != scope.WorkDirectory || self.ScratchRoot != scope.ScratchDirectory ||
		self.MaximumBytes == 0 || self.MinimumFreeBytes == 0 || self.MinimumFreeBytes >= self.MaximumBytes || self.MinimumBackingFreeBytes == 0 ||
		!runtimeBoundaryLoopPattern.MatchString(self.BlockDevice) || !runtimeBoundaryUserPattern.MatchString(self.UserNamespace) ||
		self.ContainerRootUid == 0 || self.ContainerRootGid == 0 || self.ContainerIdMapLength <= 65532 ||
		uint64(self.ContainerRootUid)+uint64(self.ContainerIdMapLength) > uint64(math.MaxUint32)+1 ||
		uint64(self.ContainerRootGid)+uint64(self.ContainerIdMapLength) > uint64(math.MaxUint32)+1 {
		return errors.New("invalid resource boundary identity, hard quota or mapped root range")
	}
	if self.RuntimeName != "runc" || self.RuntimeArguments == nil || len(self.RuntimeArguments) != 0 {
		return errors.New("private engine changed the explicitly qualified stock runc profile")
	}
	if _, err := strconv.ParseUint(strings.TrimPrefix(self.BlockDevice, "/dev/loop"), 10, 32); err != nil {
		return errors.New("private engine loop-device number exceeds the kernel identity range")
	}
	for _, path := range []string{scope.BaseDirectory, scope.StateDirectory, self.CandidateRoot, self.BackingFile, self.BlockDevice, self.PrivateSocketPath,
		self.ContainerdSocketPath, self.DockerDataRoot, self.ContainerdRoot, self.EngineExecRoot, self.ScratchRoot, self.WorkRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("resource boundary contains a noncanonical authority path")
		}
	}
	if self.CandidateRoot == self.BaseDirectory || !pathWithin(self.BaseDirectory, self.CandidateRoot) ||
		pathWithin(self.CandidateRoot, scope.StateDirectory) || pathWithin(scope.StateDirectory, self.CandidateRoot) ||
		pathWithin(self.CandidateRoot, self.BackingFile) || pathWithin(self.CandidateRoot, self.PrivateSocketPath) || pathWithin(self.CandidateRoot, self.ContainerdSocketPath) ||
		!pathWithin(self.BaseDirectory, self.PrivateSocketPath) || !pathWithin(self.BaseDirectory, self.ContainerdSocketPath) ||
		pathWithin(scope.StateDirectory, self.PrivateSocketPath) || pathWithin(scope.StateDirectory, self.ContainerdSocketPath) ||
		self.PrivateSocketPath == self.ContainerdSocketPath || self.PrivateSocketPath == "/run/docker.sock" || self.PrivateSocketPath == "/var/run/docker.sock" {
		return errors.New("candidate filesystem overlaps authority or the user's engine")
	}
	roots := []string{self.DockerDataRoot, self.ContainerdRoot, self.EngineExecRoot, self.ScratchRoot, self.WorkRoot}
	for index, root := range roots {
		if root == self.CandidateRoot || !pathWithin(self.CandidateRoot, root) {
			return errors.New("candidate writable root escapes the hard quota")
		}
		for _, other := range roots[:index] {
			if pathWithin(root, other) || pathWithin(other, root) {
				return errors.New("candidate writable roots overlap")
			}
		}
	}
	inRange := func(id, start uint32) bool {
		return uint64(id) >= uint64(start) && uint64(id) < uint64(start)+uint64(self.ContainerIdMapLength)
	}
	if inRange(scope.WorkerUid, self.ContainerRootUid) || inRange(scope.WorkerGid, self.ContainerRootGid) ||
		inRange(scope.TrustedOperatorUid, self.ContainerRootUid) {
		return errors.New("container subordinate ids overlap worker or operator authority")
	}
	for _, pin := range []Pin{self.EngineConfig, self.ContainerdConfig, self.NetworkPolicy, self.Dockerd, self.Containerd, self.ContainerdShim, self.Runc, self.SocketModeHelper, self.Nftables} {
		if !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || !digestPattern.MatchString(pin.Sha256) || pathWithin(self.CandidateRoot, pin.Path) {
			return errors.New("resource boundary pin is invalid or candidate-writable")
		}
	}
	if filepath.Base(self.Runc.Path) != "runc" || filepath.Base(self.ContainerdShim.Path) != "containerd-shim-runc-v2" ||
		filepath.Dir(self.Runc.Path) != filepath.Dir(self.ContainerdShim.Path) || self.SocketModeHelper.Path != "/usr/bin/chmod" {
		return errors.New("private engine helper path differs from pinned stock runtime tools")
	}
	return nil
}

// Exact trusted engine configuration prevents unbounded roots, alternate
// sockets, unremapped root or an implicit fallback runtime from being hidden.
func runtimeBoundaryEngineDocument(boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) map[string]any {
	return map[string]any{
		"data-root": boundary.DockerDataRoot, "exec-root": filepath.Join(boundary.EngineExecRoot, "docker"),
		"storage-driver": "overlay2", "features": map[string]any{"containerd-snapshotter": false},
		"pidfile": filepath.Join(filepath.Dir(boundary.PrivateSocketPath), "dockerd.pid"),
		"hosts":   []any{"unix://" + boundary.PrivateSocketPath}, "group": "root",
		"containerd": boundary.ContainerdSocketPath, "containerd-namespace": "urnetwork-evaluation",
		"containerd-plugins-namespace": "urnetwork-evaluation-plugins",
		"bridge":                       "none", "iptables": false, "ip6tables": false, "ip-forward": false, "ip-masq": false, "userland-proxy": false, "ipv6": false,
		"userns-remap": boundary.UserNamespace + ":" + boundary.UserNamespace,
		"exec-opts":    []any{"native.cgroupdriver=systemd"}, "default-runtime": boundary.RuntimeName, "live-restore": false,
		"log-driver": "json-file", "log-opts": map[string]any{"max-size": settings.ContainerLogMaximumSize, "max-file": strconv.Itoa(settings.ContainerLogMaximumFiles)},
	}
}

// Containerd's root, state and socket are explicit; only its fixed daemon is
// permitted to load the private engine's trusted runtime integrations.
func runtimeBoundaryContainerdDocument(boundary RuntimeResourceBoundary) []byte {
	return []byte(fmt.Sprintf("version = 3\nroot = %q\nstate = %q\ndisabled_plugins = [\"io.containerd.cri.v1.images\", \"io.containerd.cri.v1.runtime\"]\n[grpc]\n  address = %q\n  uid = 0\n  gid = 0\n[debug]\n  level = \"error\"\n[metrics]\n  address = \"\"\n", boundary.ContainerdRoot, filepath.Join(boundary.EngineExecRoot, "containerd"), boundary.ContainerdSocketPath))
}

// Candidate-controlled temporary data may not spill into the host's /tmp or
// an engine process's default root home outside the bounded filesystem.
func runtimeBoundaryTemporaryDirectories(boundary RuntimeResourceBoundary) []string {
	return []string{filepath.Join(boundary.ScratchRoot, "engine-tmp"), filepath.Join(boundary.ScratchRoot, "docker-tmp"), filepath.Join(boundary.ScratchRoot, "engine-home")}
}

// Both daemons resolve only protected pinned helpers and use bounded temporary
// roots; systemd supplies incidental invocation/notification metadata separately.
func runtimeBoundaryEnvironment(boundary RuntimeResourceBoundary) map[string]string {
	directories := runtimeBoundaryTemporaryDirectories(boundary)
	return map[string]string{"PATH": filepath.Dir(boundary.Runc.Path), "TMPDIR": directories[0], "DOCKER_TMPDIR": directories[1], "HOME": directories[2]}
}

// Protected unit configuration is checked against the actual daemon process,
// including duplicate keys and environment switches that override its class.
func validateRuntimeBoundaryEnvironment(content []byte, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) error {
	if int64(len(content)) > settings.DocumentBytes || len(content) == 0 || content[len(content)-1] != 0 {
		return errors.New("private engine environment is incomplete or unbounded")
	}
	expected := runtimeBoundaryEnvironment(boundary)
	seen := map[string]bool{}
	for _, raw := range bytes.Split(content[:len(content)-1], []byte{0}) {
		name, value, present := strings.Cut(string(raw), "=")
		if !present || name == "" || seen[name] {
			return errors.New("private engine environment is ambiguous")
		}
		seen[name] = true
		if required, owns := expected[name]; owns && value != required {
			return errors.New("private engine helper or temporary root differs")
		}
		if strings.HasPrefix(name, "DOCKER_") && name != "DOCKER_TMPDIR" || strings.HasPrefix(name, "CONTAINERD_") ||
			name == "LD_PRELOAD" || name == "LD_LIBRARY_PATH" || name == "HTTP_PROXY" || name == "HTTPS_PROXY" || name == "ALL_PROXY" ||
			name == "http_proxy" || name == "https_proxy" || name == "all_proxy" {
			return errors.New("private engine inherited a runtime or endpoint override")
		}
	}
	for name := range expected {
		if !seen[name] {
			return errors.New("private engine environment omitted a bounded root")
		}
	}
	return nil
}

// The policy changes only the dedicated bridge prefix, never user Docker
// rules. No earlier accept, conntrack exception or daemon-generated NAT can
// authorize host ingress or routed forwarding through these base chains.
func runtimeBoundaryNetworkPolicy() []byte {
	return []byte("table inet urnetwork_subeval {\n  chain input {\n    type filter hook input priority -300; policy accept;\n    iifname \"unev-*\" drop\n  }\n  chain forward {\n    type filter hook forward priority -300; policy accept;\n    iifname \"unev-*\" drop\n    oifname \"unev-*\" drop\n  }\n  chain output {\n    type filter hook output priority -300; policy accept;\n    oifname \"unev-*\" drop\n  }\n}\n")
}

// Content pins are necessary but insufficient: the pinned content must itself
// express this closed storage, namespace, runtime and scoped network policy.
func validateRuntimeBoundaryDocuments(boundary RuntimeResourceBoundary, engine, containerd, policy []byte, settings RuntimeResourceBoundarySettings) error {
	if int64(len(engine)) > settings.DocumentBytes || int64(len(containerd)) > settings.DocumentBytes || int64(len(policy)) > settings.DocumentBytes ||
		validateDockerMutationJson(engine, settings.DocumentDepth) != nil {
		return errors.New("private engine configuration is ambiguous")
	}
	var actual any
	if json.Unmarshal(engine, &actual) != nil || !reflect.DeepEqual(actual, runtimeBoundaryEngineDocument(boundary, settings)) {
		return errors.New("private engine configuration changed the closed boundary")
	}
	if !bytes.Equal(containerd, runtimeBoundaryContainerdDocument(boundary)) || !bytes.Equal(policy, runtimeBoundaryNetworkPolicy()) {
		return errors.New("private containerd or network configuration changed the closed boundary")
	}
	return nil
}

// Actual engine state must match the qualified classic overlay2/userns/runtime
// class. Kernel/minor-version permission does not imply storage equivalence.
func validateRuntimeBoundaryDaemon(content []byte, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) error {
	if int64(len(content)) > settings.NativeBytes || validateDockerMutationJson(content, settings.DocumentDepth) != nil {
		return errors.New("private daemon identity is invalid or unbounded")
	}
	var info struct {
		Driver             string
		DriverStatus       [][2]string
		DockerRootDir      string
		CgroupDriver       string
		CgroupVersion      string
		DefaultRuntime     string
		LiveRestoreEnabled bool
		SecurityOptions    []string
		Runtimes           map[string]struct {
			Path    string         `json:"path"`
			Args    []string       `json:"runtimeArgs"`
			Type    string         `json:"runtimeType"`
			Options map[string]any `json:"options"`
		}
		Containerd *struct {
			Address    string
			Namespaces struct{ Containers, Plugins string }
		}
	}
	if json.Unmarshal(content, &info) != nil || info.Driver != "overlay2" ||
		info.DockerRootDir != filepath.Join(boundary.DockerDataRoot, fmt.Sprintf("%d.%d", boundary.ContainerRootUid, boundary.ContainerRootGid)) ||
		info.CgroupDriver != "systemd" || info.CgroupVersion != "2" || info.DefaultRuntime != boundary.RuntimeName || info.LiveRestoreEnabled ||
		info.Containerd == nil || info.Containerd.Address != boundary.ContainerdSocketPath ||
		info.Containerd.Namespaces.Containers != "urnetwork-evaluation" || info.Containerd.Namespaces.Plugins != "urnetwork-evaluation-plugins" {
		return errors.New("private daemon storage, remap, cgroup or runtime class differs")
	}
	userns := false
	for _, option := range info.SecurityOptions {
		userns = userns || option == "name=userns"
	}
	for _, status := range info.DriverStatus {
		if status[0] == "driver-type" && strings.Contains(status[1], "snapshotter") {
			return errors.New("private daemon unexpectedly uses the containerd image store")
		}
	}
	runc, runcPresent := info.Runtimes["runc"]
	if !userns || !runcPresent || runc.Path != "runc" || len(runc.Args) != 0 || runc.Type != "" || len(runc.Options) != 0 {
		return errors.New("private daemon actual user namespace or stock runtime differs")
	}
	for name, runtime := range info.Runtimes {
		if name != "runc" && name != "io.containerd.runc.v2" || runtime.Path != "runc" || len(runtime.Args) != 0 || runtime.Type != "" || len(runtime.Options) != 0 {
			return errors.New("private daemon registered an unqualified custom runtime")
		}
	}
	return nil
}

// Mount identity includes source/root/options and every descendant, not only a
// statfs capacity that could describe a bind mount onto an unrelated volume.
type runtimeBoundaryMount struct {
	Id, DeviceId               uint64
	Root, Target, Source, Type string
	Options                    map[string]bool
	Shared                     bool
}

// This parser bounds both examined bytes and entries, including foreign mounts.
func parseRuntimeBoundaryMounts(content []byte, settings RuntimeResourceBoundarySettings) ([]runtimeBoundaryMount, error) {
	if int64(len(content)) > settings.NativeBytes {
		return nil, errors.New("resource mount inventory exceeds its byte bound")
	}
	reader := bufio.NewScanner(bytes.NewReader(content))
	reader.Buffer(make([]byte, len(content)+1), len(content)+1)
	mounts := []runtimeBoundaryMount{}
	for reader.Scan() {
		if len(mounts) >= settings.MountCount {
			return nil, errors.New("resource mount inventory exceeds its entry bound")
		}
		fields := strings.Fields(reader.Text())
		if len(fields) < 10 {
			return nil, errors.New("malformed resource mount inventory")
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || len(fields) != separator+4 {
			return nil, errors.New("malformed resource mount filesystem")
		}
		id, idErr := strconv.ParseUint(fields[0], 10, 64)
		majorText, minorText, found := strings.Cut(fields[2], ":")
		major, majorErr := strconv.ParseUint(majorText, 10, 32)
		minor, minorErr := strconv.ParseUint(minorText, 10, 32)
		root, rootErr := decodeMountField(fields[3])
		target, targetErr := decodeMountField(fields[4])
		source, sourceErr := decodeMountField(fields[separator+2])
		if idErr != nil || id == 0 || !found || majorErr != nil || minorErr != nil || rootErr != nil || targetErr != nil || sourceErr != nil ||
			!filepath.IsAbs(target) || filepath.Clean(target) != target || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, errors.New("invalid resource mount identity")
		}
		options := map[string]bool{}
		for _, option := range strings.Split(fields[5]+","+fields[separator+3], ",") {
			options[option] = true
		}
		shared := false
		for _, optional := range fields[6:separator] {
			shared = shared || strings.HasPrefix(optional, "shared:") || strings.HasPrefix(optional, "master:")
		}
		device := (major&0xfff)<<8 | minor&0xff | (minor&0xffffff00)<<12 | (major&0xfffff000)<<32
		mounts = append(mounts, runtimeBoundaryMount{Id: id, DeviceId: device, Root: root, Target: target, Source: source, Type: fields[separator+1], Options: options, Shared: shared})
	}
	if reader.Err() != nil {
		return nil, errors.New("resource mount inventory is malformed")
	}
	return mounts, nil
}

// A full allocated backing file reserves host storage before candidate work.
// Capacity and free-space arithmetic must not overflow into an admission proof.
func validateRuntimeBoundaryFacts(boundary RuntimeResourceBoundary, scope RuntimeResourceScope, settings RuntimeResourceBoundarySettings, facts runtimeBoundaryFacts) error {
	if !bytes.Equal(facts.MountInfo, facts.MountInfoAfter) {
		return errors.New("resource mounts changed during admission")
	}
	mounts, err := parseRuntimeBoundaryMounts(facts.MountInfo, settings)
	if err != nil {
		return err
	}
	var owned *runtimeBoundaryMount
	var root *runtimeBoundaryMount
	for index := range mounts {
		mount := &mounts[index]
		if mount.Target == "/" {
			if root != nil {
				return errors.New("host root mount identity is ambiguous")
			}
			root = mount
		}
		if mount.Target == boundary.CandidateRoot {
			if owned != nil {
				return errors.New("candidate filesystem has stacked mount authority")
			}
			owned = mount
		} else if pathWithin(boundary.CandidateRoot, mount.Target) || mount.DeviceId == facts.BlockDeviceId {
			return errors.New("candidate filesystem has nested or aliased mount authority")
		}
	}
	if owned == nil || root == nil || owned.DeviceId == root.DeviceId || owned.DeviceId != facts.BlockDeviceId || owned.Type != "ext4" ||
		owned.Source != boundary.BlockDevice || owned.Root != "/" || owned.Shared || !owned.Options["rw"] || !owned.Options["nosuid"] || !owned.Options["nodev"] || owned.Options["ro"] {
		return errors.New("candidate mount is not the exact private non-root loop ext4 filesystem")
	}
	number, _ := strconv.ParseUint(strings.TrimPrefix(boundary.BlockDevice, "/dev/loop"), 10, 32)
	if facts.BlockDeviceId == 0 || facts.LoopNumber != uint32(number) || facts.LoopFlags&1 != 0 || facts.LoopOffset != 0 ||
		facts.BlockBytes == 0 || facts.BlockBytes > boundary.MaximumBytes || facts.BackingBytes != facts.BlockBytes || facts.BackingAllocatedBytes < facts.BackingBytes ||
		facts.LoopBackingDevice != facts.BackingDevice || facts.LoopBackingInode != facts.BackingInode || facts.BackingInode == 0 || facts.BackingDevice == facts.BlockDeviceId ||
		facts.LoopSizeLimit != 0 && facts.LoopSizeLimit != facts.BlockBytes || facts.FilesystemType != runtimeBoundaryExt4 ||
		facts.BackingFilesystemType != runtimeBoundaryExt4 && facts.BackingFilesystemType != runtimeBoundaryXfs ||
		facts.FilesystemBytes == 0 || facts.FilesystemBytes > facts.BlockBytes || facts.FreeBytes > facts.FilesystemBytes ||
		facts.FreeBytes < boundary.MinimumFreeBytes || facts.BackingFreeBytes < boundary.MinimumBackingFreeBytes {
		return errors.New("candidate hard capacity, backing allocation or minimum headroom is unproved")
	}
	for _, path := range []string{boundary.CandidateRoot, boundary.DockerDataRoot, boundary.ContainerdRoot, boundary.EngineExecRoot, boundary.WorkRoot, boundary.ScratchRoot} {
		if facts.PathDeviceIds[path] != facts.BlockDeviceId {
			return errors.New("candidate writable root is outside the bounded filesystem")
		}
	}
	for _, path := range []string{scope.StateDirectory, filepath.Dir(boundary.PrivateSocketPath), filepath.Dir(boundary.ContainerdSocketPath)} {
		if facts.PathDeviceIds[path] == 0 || facts.PathDeviceIds[path] == facts.BlockDeviceId {
			return errors.New("durable authority shares the candidate quota filesystem")
		}
	}
	if err := validateRuntimeBoundarySubIds(facts.SubUids, boundary.UserNamespace, boundary.ContainerRootUid, boundary.ContainerIdMapLength, settings); err != nil {
		return err
	}
	return validateRuntimeBoundarySubIds(facts.SubGids, boundary.UserNamespace, boundary.ContainerRootGid, boundary.ContainerIdMapLength, settings)
}

// Docker uses the delegated ranges, not a number asserted by the manifest.
// Multiple target ranges or another user's overlapping range fail closed.
func validateRuntimeBoundarySubIds(content []byte, account string, start, count uint32, settings RuntimeResourceBoundarySettings) error {
	if int64(len(content)) > settings.NativeBytes {
		return errors.New("subordinate id inventory exceeds its byte bound")
	}
	lines := bytes.Split(content, []byte{'\n'})
	if len(lines) > settings.MountCount {
		return errors.New("subordinate id inventory exceeds its entry bound")
	}
	found := false
	for _, raw := range lines {
		line := strings.TrimSpace(string(raw))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 3 {
			return errors.New("malformed subordinate id delegation")
		}
		rangeStart, startErr := strconv.ParseUint(fields[1], 10, 32)
		rangeCount, countErr := strconv.ParseUint(fields[2], 10, 32)
		if fields[0] == "" || startErr != nil || countErr != nil || rangeStart == 0 || rangeCount == 0 || rangeStart+rangeCount > uint64(math.MaxUint32)+1 {
			return errors.New("invalid subordinate id delegation")
		}
		if fields[0] == account {
			if found || rangeStart != uint64(start) || rangeCount != uint64(count) {
				return errors.New("private engine subordinate id delegation changed")
			}
			found = true
		} else if rangeStart < uint64(start)+uint64(count) && uint64(start) < rangeStart+rangeCount {
			return errors.New("private engine subordinate id delegation overlaps another account")
		}
	}
	if !found {
		return errors.New("private engine subordinate id delegation is absent")
	}
	return nil
}

// Native rules must contain exactly three early filter chains and four drops.
// Ignoring handles does not ignore extra rules, accept verdicts or table flags.
func validateRuntimeBoundaryNetwork(content []byte, settings RuntimeResourceBoundarySettings) error {
	if int64(len(content)) > settings.NativeBytes || validateDockerMutationJson(content, settings.DocumentDepth) != nil {
		return errors.New("private engine network inventory is invalid or unbounded")
	}
	var document struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if decodeStrict(content, &document) != nil || len(document.Nftables) > settings.MountCount {
		return errors.New("private engine network inventory changed schema")
	}
	tableFound, metaFound := false, false
	chains := map[string]bool{}
	rules := map[string]int{}
	for _, object := range document.Nftables {
		if len(object) != 1 {
			return errors.New("private engine network object is ambiguous")
		}
		for kind, raw := range object {
			var value map[string]any
			if json.Unmarshal(raw, &value) != nil {
				return errors.New("private engine network object is malformed")
			}
			switch kind {
			case "metainfo":
				if metaFound {
					return errors.New("private engine network metadata is duplicated")
				}
				metaFound = true
			case "table":
				delete(value, "handle")
				if tableFound || !reflect.DeepEqual(value, map[string]any{"family": "inet", "name": runtimeBoundaryNftTable}) {
					return errors.New("private engine network table differs")
				}
				tableFound = true
			case "chain":
				delete(value, "handle")
				name, _ := value["name"].(string)
				if name != "input" && name != "forward" && name != "output" || chains[name] ||
					!reflect.DeepEqual(value, map[string]any{"family": "inet", "table": runtimeBoundaryNftTable, "name": name,
						"type": "filter", "hook": name, "prio": float64(-300), "policy": "accept"}) {
					return errors.New("private engine network base chain differs")
				}
				chains[name] = true
			case "rule":
				delete(value, "handle")
				chain, _ := value["chain"].(string)
				key := "iifname"
				if chain == "output" || chain == "forward" && rules[chain] == 1 {
					key = "oifname"
				}
				if chain != "input" && chain != "forward" && chain != "output" || rules[chain] >= 1 && chain != "forward" || rules[chain] >= 2 {
					return errors.New("private engine network has extra rules")
				}
				expected := map[string]any{"family": "inet", "table": runtimeBoundaryNftTable, "chain": chain,
					"expr": []any{map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{"meta": map[string]any{"key": key}}, "right": runtimeBoundaryBridgePattern}},
						map[string]any{"drop": nil}}}
				if !reflect.DeepEqual(value, expected) {
					return errors.New("private engine network drop rule differs")
				}
				rules[chain]++
			default:
				return errors.New("private engine network contains an unexpected object")
			}
		}
	}
	if !tableFound || len(chains) != 3 || rules["input"] != 1 || rules["forward"] != 2 || rules["output"] != 1 {
		return errors.New("private engine network guard is incomplete")
	}
	return nil
}

// No mutable native fact is supplied by a peer or retained across admission.
type runtimeBoundaryNativeHost struct{}

// Protected configuration is opened without following its final path element.
func (runtimeBoundaryNativeHost) Document(pin Pin, maximum int64) ([]byte, error) {
	if err := validateFilePin(pin, maximum, true); err != nil {
		return nil, err
	}
	content, err := readBounded(pin.Path, maximum)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != pin.Sha256 {
		return nil, errors.New("resource boundary document changed while opening")
	}
	return content, nil
}

// Executables are fixed root-owned bytes, not caller-selected command names.
func (runtimeBoundaryNativeHost) Executable(pin Pin) error { return validatePin(pin, true) }

// Only trusted operator ancestors above BaseDirectory may be non-root-owned.
func (runtimeBoundaryNativeHost) Directory(path string, scope RuntimeResourceScope) error {
	return protectedRuntimePath(path, scope.BaseDirectory, scope.TrustedOperatorUid)
}

// Kernel UAPI layout for LOOP_GET_STATUS64; ioctl values are Linux constants.
type runtimeBoundaryLoopInfo struct {
	Device, Inode, Rdevice, Offset, SizeLimit  uint64
	Number, EncryptType, EncryptKeySize, Flags uint32
	FileName, CryptName                        [64]byte
	EncryptKey                                 [32]byte
	Init                                       [2]uint64
}

// All opened paths are checked through descriptors; the second mount snapshot
// catches a concurrent mount swap before a proof can be returned.
func (runtimeBoundaryNativeHost) Kernel(ctx context.Context, boundary RuntimeResourceBoundary, scope RuntimeResourceScope, settings RuntimeResourceBoundarySettings) (runtimeBoundaryFacts, error) {
	facts := runtimeBoundaryFacts{PathDeviceIds: map[string]uint64{}}
	var err error
	if err = ctx.Err(); err != nil {
		return facts, err
	}
	facts.MountInfo, err = readBounded("/proc/self/mountinfo", settings.NativeBytes)
	if err != nil {
		return facts, err
	}
	for _, path := range []string{boundary.CandidateRoot, boundary.DockerDataRoot, boundary.ContainerdRoot, boundary.EngineExecRoot, boundary.WorkRoot,
		boundary.ScratchRoot, scope.StateDirectory, filepath.Dir(boundary.PrivateSocketPath), filepath.Dir(boundary.ContainerdSocketPath)} {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return facts, err
		}
		var stat syscall.Stat_t
		err = syscall.Fstat(int(file.Fd()), &stat)
		facts.PathDeviceIds[path] = stat.Dev
		if err == nil && path == boundary.CandidateRoot {
			var filesystem syscall.Statfs_t
			err = syscall.Fstatfs(int(file.Fd()), &filesystem)
			if err == nil {
				facts.FilesystemType = filesystem.Type
				facts.FilesystemBytes, err = runtimeBoundaryBytes(filesystem.Blocks, filesystem.Bsize)
			}
			if err == nil {
				facts.FreeBytes, err = runtimeBoundaryBytes(filesystem.Bavail, filesystem.Bsize)
			}
		}
		_ = file.Close()
		if err != nil {
			return facts, err
		}
	}
	for _, socket := range []string{boundary.PrivateSocketPath, boundary.ContainerdSocketPath} {
		info, err := os.Lstat(socket)
		if err != nil {
			return facts, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || stat.Uid != 0 || stat.Gid != 0 || info.Mode().Perm()&0077 != 0 {
			return facts, errors.New("private engine control socket is not root-only")
		}
	}
	if err := protectedRuntimeBoundaryFile(boundary.BackingFile, scope); err != nil {
		return facts, err
	}
	backing, err := openRegularNoFollow(boundary.BackingFile)
	if err != nil {
		return facts, err
	}
	defer backing.Close()
	var backingStat syscall.Stat_t
	if err := syscall.Fstat(int(backing.Fd()), &backingStat); err != nil || backingStat.Size <= 0 || backingStat.Blocks < 0 {
		return facts, errors.New("private engine backing file has invalid kernel allocation")
	}
	facts.BackingDevice, facts.BackingInode, facts.BackingBytes = backingStat.Dev, backingStat.Ino, uint64(backingStat.Size)
	facts.BackingAllocatedBytes, err = runtimeBoundaryBytes(uint64(backingStat.Blocks), 512)
	if err != nil {
		return facts, err
	}
	var backingFilesystem syscall.Statfs_t
	if err := syscall.Fstatfs(int(backing.Fd()), &backingFilesystem); err != nil {
		return facts, err
	}
	facts.BackingFilesystemType = backingFilesystem.Type
	facts.BackingFreeBytes, err = runtimeBoundaryBytes(backingFilesystem.Bavail, backingFilesystem.Bsize)
	if err != nil {
		return facts, err
	}
	if err := protectedRootPath(filepath.Dir(boundary.BlockDevice), true, false); err != nil {
		return facts, err
	}
	device, err := os.OpenFile(boundary.BlockDevice, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return facts, err
	}
	defer device.Close()
	var deviceStat syscall.Stat_t
	if err := syscall.Fstat(int(device.Fd()), &deviceStat); err != nil || deviceStat.Mode&syscall.S_IFMT != syscall.S_IFBLK ||
		deviceStat.Uid != 0 || deviceStat.Mode&0022 != 0 {
		return facts, errors.New("private engine block device is not protected")
	}
	facts.BlockDeviceId = deviceStat.Rdev
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, device.Fd(), 0x80081272, uintptr(unsafe.Pointer(&facts.BlockBytes))); errno != 0 {
		return facts, errors.New("cannot inspect private engine hard block capacity")
	}
	var loop runtimeBoundaryLoopInfo
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, device.Fd(), 0x4c05, uintptr(unsafe.Pointer(&loop))); errno != 0 {
		return facts, errors.New("private engine block device is not an attached loop")
	}
	facts.LoopBackingDevice, facts.LoopBackingInode, facts.LoopOffset, facts.LoopSizeLimit = loop.Device, loop.Inode, loop.Offset, loop.SizeLimit
	facts.LoopNumber, facts.LoopFlags = loop.Number, loop.Flags
	for path, target := range map[string]*[]byte{"/etc/subuid": &facts.SubUids, "/etc/subgid": &facts.SubGids} {
		if err := protectedRootPath(path, false, false); err != nil {
			return facts, err
		}
		*target, err = readBounded(path, settings.NativeBytes)
		if err != nil {
			return facts, err
		}
	}
	facts.MountInfoAfter, err = readBounded("/proc/self/mountinfo", settings.NativeBytes)
	return facts, err
}

// Multiplication from kernel unsigned counts may not wrap into a small quota.
func runtimeBoundaryBytes(count uint64, size int64) (uint64, error) {
	if size <= 0 || count > math.MaxUint64/uint64(size) {
		return 0, errors.New("resource capacity arithmetic is invalid or overflows")
	}
	return count * uint64(size), nil
}

// Backing files beneath the logical base retain operator-aware ancestors;
// arbitrary external backing paths require root ownership all the way up.
func protectedRuntimeBoundaryFile(path string, scope RuntimeResourceScope) error {
	if pathWithin(scope.BaseDirectory, path) {
		if err := protectedRuntimePath(filepath.Dir(path), scope.BaseDirectory, scope.TrustedOperatorUid); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0077 != 0 {
			return errors.New("private engine backing file is not protected root storage")
		}
		return nil
	}
	return protectedRootPath(path, false, true)
}

// The live socket peer must be the pinned daemon running only its protected
// configuration. A different root process on the same path is not equivalent.
func verifyRuntimeBoundaryPeer(connection net.Conn, executable Pin, configPath, configFlag string, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) error {
	unix, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("private engine peer is not a Unix connection")
	}
	raw, err := unix.SyscallConn()
	if err != nil {
		return err
	}
	var credential *syscall.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credential, credentialErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || credentialErr != nil || credential == nil || credential.Uid != 0 || credential.Pid <= 0 {
		return errors.New("private engine peer is not an owned root daemon")
	}
	proc := "/proc/" + strconv.FormatInt(int64(credential.Pid), 10)
	target, err := os.Readlink(filepath.Join(proc, "exe"))
	if err != nil || target != executable.Path {
		return errors.New("private engine live executable differs")
	}
	file, err := os.Open(filepath.Join(proc, "exe"))
	if err != nil {
		return err
	}
	digest := sha256.New()
	count, copyErr := io.Copy(digest, io.LimitReader(file, settings.ExecutableBytes+1))
	_ = file.Close()
	if copyErr != nil || count > settings.ExecutableBytes || hex.EncodeToString(digest.Sum(nil)) != executable.Sha256 {
		return errors.New("private engine live executable content differs")
	}
	arguments, err := readBounded(filepath.Join(proc, "cmdline"), settings.DocumentBytes)
	expected := []byte(executable.Path + "\x00" + configFlag + "\x00" + configPath + "\x00")
	if executable == (Pin{}) || configPath == "" || err != nil || !bytes.Equal(arguments, expected) {
		return errors.New("private engine live arguments changed its protected configuration")
	}
	program := "dockerd"
	if executable == boundary.Containerd {
		program = "containerd"
	} else if executable != boundary.Dockerd {
		return errors.New("private engine live peer has no daemon role")
	}
	logs := []runtimeBoundaryLog{}
	for index, stream := range []string{"stdout", "stderr"} {
		path := filepath.Join(proc, "fd", strconv.Itoa(index+1))
		target, err := os.Readlink(path)
		if err != nil || target != filepath.Join(boundary.CandidateRoot, "daemon-logs", program+"."+stream+".log") {
			return errors.New("private engine live diagnostic target is not bounded")
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		var stat syscall.Stat_t
		err = syscall.Fstat(int(file.Fd()), &stat)
		_ = file.Close()
		if err != nil {
			return err
		}
		logs = append(logs, runtimeBoundaryLog{Target: target, Stream: stream, DeviceId: stat.Dev, Uid: stat.Uid, Mode: stat.Mode})
	}
	if err := validateRuntimeBoundaryLogs(logs, program, boundary); err != nil {
		return err
	}
	environment, err := readBounded(filepath.Join(proc, "environ"), settings.DocumentBytes)
	if err != nil {
		return err
	}
	defer clear(environment)
	return validateRuntimeBoundaryEnvironment(environment, boundary, settings)
}

// Live daemon diagnostics are candidate-amplifiable and must not target the
// host journal, /var/log, a pipe with an unknown owner or another filesystem.
type runtimeBoundaryLog struct {
	Target, Stream string
	DeviceId       uint64
	Uid, Mode      uint32
}

// Descriptor facts, not merely unit-file paths, establish quota containment.
func validateRuntimeBoundaryLogs(logs []runtimeBoundaryLog, program string, boundary RuntimeResourceBoundary) error {
	if boundary.WorkDeviceId == 0 || len(logs) != 2 || program != "dockerd" && program != "containerd" {
		return errors.New("private engine live log ownership is incomplete")
	}
	for index, log := range logs {
		stream := "stdout"
		if index == 1 {
			stream = "stderr"
		}
		if log.Stream != stream || log.Target != filepath.Join(boundary.CandidateRoot, "daemon-logs", program+"."+stream+".log") ||
			log.DeviceId != boundary.WorkDeviceId || log.Uid != 0 || log.Mode&syscall.S_IFMT != syscall.S_IFREG || log.Mode&0077 != 0 {
			return errors.New("private engine live diagnostics escape the bounded filesystem")
		}
	}
	return nil
}

// Only the manifest's private Unix endpoint is queried. No environment proxy,
// redirect, alternate context, network listener or Docker CLI is involved.
func (runtimeBoundaryNativeHost) Daemon(parent context.Context, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, settings.CommandTimeout)
	defer cancel()
	dial := func(ctx context.Context, socket string, executable Pin, configPath, configFlag string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		if err := verifyRuntimeBoundaryPeer(connection, executable, configPath, configFlag, boundary, settings); err != nil {
			_ = connection.Close()
			return nil, err
		}
		return connection, nil
	}
	containerd, err := dial(ctx, boundary.ContainerdSocketPath, boundary.Containerd, boundary.ContainerdConfig.Path, "--config")
	if err != nil {
		return nil, err
	}
	_ = containerd.Close()
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: settings.CommandTimeout, MaxResponseHeaderBytes: settings.DocumentBytes, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, boundary.PrivateSocketPath, boundary.Dockerd, boundary.EngineConfig.Path, "--config-file")
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://private-engine/v1.56/info", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("private daemon identity query failed")
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, settings.NativeBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(content)) > settings.NativeBytes {
		return nil, errors.New("private daemon identity response is incomplete or unbounded")
	}
	return content, nil
}

// The command is read-only, bounded and selected only by protected policy.
func (runtimeBoundaryNativeHost) Network(parent context.Context, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) ([]byte, error) {
	if err := validatePin(boundary.Nftables, true); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, settings.CommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, boundary.Nftables.Path, "-j", "list", "table", "inet", runtimeBoundaryNftTable)
	command.Env = controlEnvironment()
	output := &boundedOutput{maximum: settings.NativeBytes}
	command.Stdout, command.Stderr = output, io.Discard
	if err := command.Run(); err != nil || output.exceeded {
		return nil, errors.New("cannot prove the private engine's live network guard")
	}
	return append([]byte(nil), output.Bytes()...), nil
}
