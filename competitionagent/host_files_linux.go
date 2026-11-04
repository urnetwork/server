// The trusted worker has no generic sudo capability. Its finite host-file
// broker can mount only this run's bounded evidence tmpfs and hand off/reclaim
// only its exact output directories through non-following descriptors.
package competitionagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Namespace metadata is declared by the protected private-engine boundary.
// A live owned-container probe must independently verify it before handoff.
type HostFilesPlan struct {
	WorkerGid            uint32 `json:"worker_gid"`
	ContainerRootUid     uint32 `json:"container_root_uid"`
	ContainerRootGid     uint32 `json:"container_root_gid"`
	ContainerIdMapLength uint32 `json:"container_id_map_length"`
	EvidenceBytes        int64  `json:"evidence_bytes"`
	WorkDeviceId         uint64 `json:"work_device_id"`
	ResourceBoundary     Pin    `json:"resource_boundary"`
}

// The role accepts no command, owner, mode, mount option, source or executable.
// Only the two output operations have a path; every other target is derived.
type HostFilesRequest struct {
	Schema      int    `json:"schema"`
	RunId       string `json:"run_id"`
	Operation   string `json:"operation"`
	Path        string `json:"path,omitempty"`
	ContainerId string `json:"container_id,omitempty"`
	NetworkId   string `json:"network_id,omitempty"`
}

// Finite errors contain no native error, candidate content or arbitrary path.
// Namespace proof consists only of declared ids and hashes of kernel maps.
type HostFilesResult struct {
	RunId        string               `json:"run_id"`
	Operation    string               `json:"operation"`
	Acknowledged bool                 `json:"acknowledged"`
	ExitCode     int                  `json:"exit_code"`
	ErrorCode    string               `json:"error_code,omitempty"`
	Namespace    *HostNamespaceMap    `json:"namespace,omitempty"`
	Network      *HostNetworkBoundary `json:"network,omitempty"`
}

// No host PID, namespace path, command or raw policy becomes caller authority.
// The link digest retains the bounded kernel observation for exact evidence.
type HostNetworkBoundary struct {
	Schema           int    `json:"schema"`
	NetworkId        string `json:"network_id"`
	BridgeName       string `json:"bridge_name"`
	PrivateNamespace bool   `json:"private_namespace"`
	IsolatedGateway  bool   `json:"isolated_gateway"`
	LinkSha256       string `json:"link_sha256"`
}

type HostNamespaceMap struct {
	ContainerRootUid     uint32 `json:"container_root_uid"`
	ContainerRootGid     uint32 `json:"container_root_gid"`
	ContainerIdMapLength uint32 `json:"container_id_map_length"`
	UidMapSha256         string `json:"uid_map_sha256"`
	GidMapSha256         string `json:"gid_map_sha256"`
	CgroupPath           string `json:"cgroup_path"`
}

// Walks bound work examined and descriptor depth, not merely returned results.
// No peer can enlarge these owner-local values.
type HostFilesSettings struct {
	MaximumTreeEntries int
	MaximumTreeDepth   int
	DirectoryBatch     int
	MaximumProcBytes   int64
	MaximumPathBytes   int
	OperationTimeout   time.Duration
}

func DefaultHostFilesSettings() HostFilesSettings {
	return HostFilesSettings{MaximumTreeEntries: 500000, MaximumTreeDepth: 64, DirectoryBatch: 128,
		MaximumProcBytes: 128 * 1024, MaximumPathBytes: 4096, OperationTimeout: 2 * time.Minute}
}

const hostFilesContainerUid uint32 = 65532
const hostFilesContainerGid uint32 = 65532
const hostFilesTmpfsType = 0x01021994

var errHostFilesUnknown = errors.New("host-file native outcome remains unknown")

var hostFilesOutputPattern = regexp.MustCompile(`^runs/(baseline|candidate)-0[1-9]/output$`)

// Native seams are private, and the production constructor never permits
// caller-selected programs. Mutating operations are invoked only after the
// independent Docker owner's synced intent has taken durable custody.
type hostFileBroker struct {
	plan            DockerProxyPlan
	settings        HostFilesSettings
	stateLock       sync.Mutex
	verified        bool
	proveMap        func(context.Context, string) (HostNamespaceMap, error)
	proveNetwork    func(context.Context, string) (HostNetworkBoundary, error)
	beforePath      func(context.Context, string) error
	mount           func(context.Context, cleanupPlan, uint32, uint32, int64) error
	unmount         func(context.Context, cleanupPlan) error
	chown           func(*os.File, uint32, uint32) error
	beforeOpen      func(*os.File, string)
	operationTokens chan struct{}
}

func newHostFileBroker(plan DockerProxyPlan, settings HostFilesSettings) (*hostFileBroker, error) {
	if err := validateHostFilesPlan(plan, settings); err != nil {
		return nil, err
	}
	copy := *plan.HostFiles
	plan.HostFiles = &copy
	plan.CgroupNames = append([]string(nil), plan.CgroupNames...)
	plan.ReadOnlyBindPaths = append([]string(nil), plan.ReadOnlyBindPaths...)
	broker := &hostFileBroker{plan: plan, settings: settings, operationTokens: make(chan struct{}, 1)}
	broker.chown = func(file *os.File, uid, gid uint32) error { return file.Chown(int(uid), int(gid)) }
	broker.mount = broker.mountEvidence
	broker.unmount = broker.unmountEvidence
	return broker, nil
}

// A mapped namespace cannot overlap the trusted worker or contain host root.
// Arithmetic is checked before any uint32 addition can wrap into authority.
func validateHostFilesPlan(plan DockerProxyPlan, settings HostFilesSettings) error {
	if plan.HostFiles == nil || plan.WorkerUid == 0 || plan.HostFiles.WorkerGid == 0 || plan.HostFiles.WorkDeviceId == 0 || plan.HostFiles.EvidenceBytes <= 0 ||
		settings.MaximumTreeEntries <= 0 || settings.MaximumTreeDepth <= 0 || settings.DirectoryBatch <= 0 || settings.MaximumProcBytes <= 0 ||
		settings.MaximumPathBytes <= 0 || settings.OperationTimeout <= 0 {
		return errors.New("host-file owner requires finite protected run authority")
	}
	rangeLength := uint64(plan.HostFiles.ContainerIdMapLength)
	for _, mapping := range []struct{ root, worker, container uint32 }{
		{root: plan.HostFiles.ContainerRootUid, worker: plan.WorkerUid, container: hostFilesContainerUid},
		{root: plan.HostFiles.ContainerRootGid, worker: plan.HostFiles.WorkerGid, container: hostFilesContainerGid},
	} {
		end := uint64(mapping.root) + rangeLength
		if mapping.root == 0 || rangeLength <= uint64(mapping.container) || end > uint64(^uint32(0)) ||
			uint64(mapping.worker) >= uint64(mapping.root) && uint64(mapping.worker) < end {
			return errors.New("host-file namespace range overlaps privileged or trusted host identity")
		}
	}
	return nil
}

// Paths can select only the finite full-R9 output vocabulary. The evidence
// tmpfs, user namespace and mount source are never selected by request bytes.
func (self *hostFileBroker) validateRequest(request HostFilesRequest) (string, error) {
	if request.Schema != Schema || request.RunId != self.plan.RunId {
		return "", errors.New("host-file request belongs to another run")
	}
	if request.Operation != "verify-network" && request.NetworkId != "" {
		return "", errors.New("network identity cannot alter another finite host operation")
	}
	switch request.Operation {
	case "mount-evidence", "unmount-evidence":
		if request.Path != "" || request.ContainerId != "" {
			return "", errors.New("evidence target cannot be overridden")
		}
		return ".evidence-runtime", nil
	case "verify-id-map":
		if request.Path != "" || !digestPattern.MatchString(request.ContainerId) {
			return "", errors.New("namespace proof requires one full owned container id")
		}
		return "", nil
	case "verify-network":
		if request.Path != "" || request.ContainerId != "" || !digestPattern.MatchString(request.NetworkId) {
			return "", errors.New("network proof requires one full owned network id")
		}
		return "", nil
	case "prepare-output", "reclaim-output":
		if request.ContainerId != "" || !filepath.IsAbs(request.Path) || filepath.Clean(request.Path) != request.Path ||
			len(request.Path) > self.settings.MaximumPathBytes || strings.ContainsAny(request.Path, "\x00\r\n") {
			return "", errors.New("output request has an invalid path")
		}
		evidence := filepath.Join(self.plan.ContextRoot, ".evidence-runtime")
		relative, err := filepath.Rel(evidence, request.Path)
		if err != nil || !hostFilesOutputPattern.MatchString(filepath.ToSlash(relative)) && relative != "baseline-score-output" && relative != "candidate-score-output" {
			return "", errors.New("output path is outside the exact full-R9 stage inventory")
		}
		return filepath.Join(".evidence-runtime", relative), nil
	default:
		return "", errors.New("host-file operation is outside the finite role")
	}
}

// Synchronous native completion is acknowledged even for a definite error;
// no unjoined command, daemon task or background filesystem work is launched.
func (self *hostFileBroker) Execute(parent context.Context, request HostFilesRequest) HostFilesResult {
	result := HostFilesResult{RunId: self.plan.RunId, Operation: request.Operation, Acknowledged: true, ExitCode: 2, ErrorCode: "host_operation_rejected"}
	relative, err := self.validateRequest(request)
	if err != nil {
		return result
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.OperationTimeout)
	defer cancel()
	if self.operationTokens == nil {
		result.ErrorCode = "host_owner_invalid"
		return result
	}
	select {
	case self.operationTokens <- struct{}{}:
		defer func() { <-self.operationTokens }()
	case <-ctx.Done():
		result.ErrorCode = "host_operation_canceled"
		return result
	}
	if err := ctx.Err(); err != nil {
		result.ErrorCode = "host_operation_canceled"
		return result
	}
	if request.Operation == "verify-network" {
		if self.proveNetwork == nil {
			result.ErrorCode = "host_network_unverified"
			return result
		}
		network, err := self.proveNetwork(ctx, request.NetworkId)
		if err != nil || validateHostNetworkProof(network, self.plan.RunId, request.NetworkId) != nil {
			result.ErrorCode = "host_network_unverified"
			return result
		}
		result.Network, result.ExitCode, result.ErrorCode = &network, 0, ""
		return result
	}
	if request.Operation == "verify-id-map" {
		if self.proveMap == nil {
			result.ErrorCode = "host_namespace_unverified"
			return result
		}
		mapping, err := self.proveMap(ctx, request.ContainerId)
		if err != nil || mapping.ContainerRootUid != self.plan.HostFiles.ContainerRootUid || mapping.ContainerRootGid != self.plan.HostFiles.ContainerRootGid ||
			mapping.ContainerIdMapLength != self.plan.HostFiles.ContainerIdMapLength || !digestPattern.MatchString(mapping.UidMapSha256) || !digestPattern.MatchString(mapping.GidMapSha256) {
			result.ErrorCode = "host_namespace_unverified"
			return result
		}
		self.stateLock.Lock()
		self.verified = true
		self.stateLock.Unlock()
		result.ExitCode, result.ErrorCode, result.Namespace = 0, "", &mapping
		return result
	}
	if self.beforePath == nil || self.beforePath(ctx, request.Operation) != nil {
		result.ErrorCode = "host_docker_state_unverified"
		return result
	}
	if request.Operation == "mount-evidence" || request.Operation == "unmount-evidence" {
		plan := self.cleanupPlan()
		if request.Operation == "mount-evidence" {
			err = self.mount(ctx, plan, self.plan.WorkerUid, self.plan.HostFiles.WorkerGid, self.plan.HostFiles.EvidenceBytes)
		} else {
			err = self.unmount(ctx, plan)
		}
	} else {
		self.stateLock.Lock()
		verified := self.verified
		self.stateLock.Unlock()
		if !verified {
			result.ErrorCode = "host_namespace_unverified"
			return result
		}
		directory, openErr := self.openDirectory(relative)
		if openErr != nil {
			err = openErr
		} else {
			defer directory.Close()
			if request.Operation == "prepare-output" {
				names, readErr := directory.Readdirnames(1)
				if len(names) != 0 || readErr != nil && !errors.Is(readErr, io.EOF) {
					err = errors.New("output handoff requires a fresh empty directory")
				} else if err = self.chown(directory, self.plan.HostFiles.ContainerRootUid+hostFilesContainerUid, self.plan.HostFiles.ContainerRootGid+hostFilesContainerGid); err == nil {
					err = directory.Chmod(0700)
				}
			} else {
				examined := 0
				err = self.reclaim(ctx, directory, 0, &examined)
			}
		}
	}
	if err != nil {
		if errors.Is(err, errHostFilesUnknown) {
			result.Acknowledged = false
			result.ErrorCode = "host_operation_outcome_unknown"
			return result
		}
		result.ErrorCode = "host_operation_failed"
		return result
	}
	result.ExitCode, result.ErrorCode = 0, ""
	return result
}

// Each ancestor is captured without following a symlink. Subsequent swaps
// cannot redirect metadata writes through an untrusted output path.
func (self *hostFileBroker) openDirectory(relative string) (*os.File, error) {
	fd, err := syscall.Open(self.plan.ContextRoot, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "owned-host-work")
	info, err := current.Stat()
	if err != nil {
		current.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != self.plan.HostFiles.WorkDeviceId {
		current.Close()
		return nil, errors.New("host-file work root moved off its admitted bounded filesystem")
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			current.Close()
			return nil, errors.New("host-file path has a noncanonical component")
		}
		child, err := syscall.Openat(int(current.Fd()), part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = os.NewFile(uintptr(child), "owned-host-directory")
	}
	return current, nil
}

// No recursive shell tool handles candidate paths. Each entry is counted,
// opened without following links and modified only through the captured fd.
func (self *hostFileBroker) reclaim(ctx context.Context, directory *os.File, depth int, examined *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > self.settings.MaximumTreeDepth || *examined >= self.settings.MaximumTreeEntries {
		return errors.New("output ownership walk exceeds its bound")
	}
	*examined++
	for {
		entries, readErr := directory.ReadDir(self.settings.DirectoryBatch)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if *examined >= self.settings.MaximumTreeEntries || entry.Type()&os.ModeSymlink != 0 || entry.Type() != 0 && !entry.IsDir() {
				return errors.New("output has excess or nonregular entries")
			}
			if self.beforeOpen != nil {
				self.beforeOpen(directory, entry.Name())
			}
			flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
			if entry.IsDir() {
				flags |= syscall.O_DIRECTORY
			}
			fd, err := syscall.Openat(int(directory.Fd()), entry.Name(), flags, 0)
			if err != nil {
				return err
			}
			file := os.NewFile(uintptr(fd), "owned-host-output")
			info, err := file.Stat()
			if err != nil {
				file.Close()
				return err
			}
			if info.IsDir() {
				err = self.reclaim(ctx, file, depth+1, examined)
			} else {
				*examined++
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 {
					err = errors.New("output file is not one unshared regular inode")
				} else if err = self.chown(file, self.plan.WorkerUid, self.plan.HostFiles.WorkerGid); err == nil {
					mode := os.FileMode(0600)
					if info.Mode().Perm()&0111 != 0 {
						mode = 0700
					}
					err = file.Chmod(mode)
				}
			}
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := self.chown(directory, self.plan.WorkerUid, self.plan.HostFiles.WorkerGid); err != nil {
		return err
	}
	return directory.Chmod(0700)
}

func (self *hostFileBroker) cleanupPlan() cleanupPlan {
	return cleanupPlan{Schema: Schema, RunId: self.plan.RunId, JobId: self.plan.JobId, RoundId: self.plan.RoundId, Attempt: self.plan.Attempt,
		CgroupNames: append([]string(nil), self.plan.CgroupNames...), WorkDeviceId: self.plan.HostFiles.WorkDeviceId,
		EvidenceDirectory: filepath.Join(self.plan.ContextRoot, ".evidence-runtime"), EvidenceSource: "urnetwork-evidence-" + strings.ReplaceAll(self.plan.JobId, "-", "") + "-" + strconv.Itoa(self.plan.Attempt)}
}

// Mount options come from the frozen resource helper and actual worker only.
// No process inherits mount capability and no shell interprets a target path.
func (self *hostFileBroker) mountEvidence(ctx context.Context, plan cleanupPlan, uid, gid uint32, bytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mounts := newHostCleanupMounts(Config{WorkDirectory: filepath.Dir(self.plan.ContextRoot)}, DefaultAgentSettings())
	existing, err := mounts.exactMount(ctx, plan)
	if err != nil {
		return errors.Join(errHostFilesUnknown, err)
	}
	if existing != nil {
		return self.verifyEvidenceMount(ctx, plan, uid, gid, bytes)
	}
	directory, err := self.openDirectory(".evidence-runtime")
	if err != nil {
		return err
	}
	names, readErr := directory.Readdirnames(1)
	if len(names) != 0 || readErr != nil && !errors.Is(readErr, io.EOF) {
		directory.Close()
		return errors.New("evidence tmpfs requires a fresh empty exact directory")
	}
	defer directory.Close()
	target := fmt.Sprintf("/proc/self/fd/%d", directory.Fd())
	options := fmt.Sprintf("size=%d,mode=0700,uid=%d,gid=%d", bytes, uid, gid)
	mountErr := syscall.Mount(plan.EvidenceSource, target, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, options)
	current, err := mounts.exactMount(ctx, plan)
	if err != nil {
		return errors.Join(errHostFilesUnknown, err)
	}
	if current == nil {
		if mountErr != nil {
			return mountErr
		}
		return errHostFilesUnknown
	}
	if err := self.verifyEvidenceMount(ctx, plan, uid, gid, bytes); err != nil {
		return errors.Join(errHostFilesUnknown, err)
	}
	return nil
}

// A matching source name alone cannot prove the frozen tmpfs capacity, flags,
// actual worker ownership or that the inspected mount is still on this fd.
func (self *hostFileBroker) verifyEvidenceMount(ctx context.Context, plan cleanupPlan, uid, gid uint32, bytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := self.openDirectory(".evidence-runtime")
	if err != nil {
		return err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	var filesystem syscall.Statfs_t
	if !ok || stat.Uid != uid || stat.Gid != gid || info.Mode().Perm() != 0700 || syscall.Fstatfs(int(directory.Fd()), &filesystem) != nil ||
		filesystem.Type != hostFilesTmpfsType || filesystem.Bsize <= 0 || uint64(bytes)/uint64(filesystem.Bsize) != filesystem.Blocks ||
		uint64(bytes)%uint64(filesystem.Bsize) != 0 || uint64(filesystem.Flags)&uint64(syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC) != uint64(syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC) {
		return errors.New("evidence tmpfs differs from its bounded frozen ownership contract")
	}
	mounts := newHostCleanupMounts(Config{WorkDirectory: filepath.Dir(self.plan.ContextRoot)}, DefaultAgentSettings())
	entry, err := mounts.exactMount(ctx, plan)
	if err != nil || entry == nil {
		return errors.New("evidence tmpfs has no exact mount identity")
	}
	id, err := mounts.mountId(directory)
	if err != nil || id != entry.id {
		return errors.New("evidence tmpfs changed across its descriptor capture")
	}
	return nil
}

// The same exact source/descriptor/nonlazy verifier is used by post-stop.
func (self *hostFileBroker) unmountEvidence(ctx context.Context, plan cleanupPlan) error {
	config := Config{WorkDirectory: filepath.Dir(self.plan.ContextRoot)}
	if err := newHostCleanupMounts(config, DefaultAgentSettings()).Unmount(ctx, plan); err != nil {
		return errors.Join(errHostFilesUnknown, err)
	}
	return nil
}
