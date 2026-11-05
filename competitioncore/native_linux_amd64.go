// The native entry point is intentionally non-activatable: live in-process
// manager proof, pre-Go restoration and Apport script/interpreter custody are
// implementation gaps, not user-configurable qualification flags.
package competitioncore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

var ErrManagerProofUnimplemented = errors.New("in-process native systemd manager resource proof is not implemented")
var ErrOrdinaryHandoffUnimplemented = errors.New("exact ordinary restoration and Apport script/interpreter FD custody are not implemented")

// There are no credentials, main/server configuration or network dependencies.
type nativeHost struct{ settings Settings }

// Root-only policy bytes may declare pins but cannot enable missing backends.
func (self nativeHost) Policy(ctx context.Context) (Policy, error) {
	if ctx.Err() != nil || os.Geteuid() != 0 {
		return Policy{}, errors.New("core helper has no live root invocation")
	}
	content, err := readProtected(ConfigurationPath, self.settings.DocumentBytes)
	if err != nil {
		return Policy{}, err
	}
	policy := Policy{}
	if strictDocument(content, &policy, self.settings) != nil || validatePolicy(policy, self.settings) != nil {
		return Policy{}, errors.New("core helper policy is incomplete or unprotected")
	}
	return policy, nil
}

// Kernel metadata is bounded and read only; no sysctl writer exists here.
func (self nativeHost) kernelValue(ctx context.Context, path string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	root, err := openDirectory("/proc", false)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := requireFilesystem(root, 0x9fa0); err != nil {
		return "", err
	}
	content, err := readRelative(root, path, self.settings.ProcBytes, false)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(content), "\n"), ctx.Err()
}

// The original positive value must remain exactly ten; zero is never safe.
func (self nativeHost) PipeLimit(ctx context.Context) (uint64, error) {
	value, err := self.kernelValue(ctx, "sys/kernel/core_pipe_limit")
	if err != nil {
		return 0, err
	}
	return parseUnsigned(value)
}

func (self nativeHost) Pattern(ctx context.Context) (string, error) {
	return self.kernelValue(ctx, "sys/kernel/core_pattern")
}

func (self nativeHost) BootId(ctx context.Context) (string, error) {
	value, err := self.kernelValue(ctx, "sys/kernel/random/boot_id")
	if err != nil || !validBootId(value) {
		return "", errors.New("core registry has no exact current kernel boot identity")
	}
	return value, nil
}

func (self nativeHost) Snapshot(ctx context.Context) (Snapshot, error) {
	store, err := OpenRegistryStore(self.settings)
	if err != nil {
		return Snapshot{}, err
	}
	defer store.Close()
	return store.Snapshot(ctx)
}

func (self nativeHost) Recheck(ctx context.Context, before Snapshot) error {
	after, err := self.Snapshot(ctx)
	if err != nil || after.Registry.Generation != before.Registry.Generation || after.digest != before.digest {
		return errors.New("core registry generation changed during classification")
	}
	return nil
}

func (self nativeHost) Process(ctx context.Context, invocation Invocation) (process, error) {
	return openNativeProcess(ctx, invocation, self.settings)
}

// Native ordering is fixed: authoritative manager proof, held-root kernel
// verification, then only the helper's own PID may enter the handler slice.
func (self nativeHost) Place(ctx context.Context, policy Policy) (placement, error) {
	if err := nativeManagerProof(ctx, policy, self.settings); err != nil {
		return nil, err
	}
	return joinNativeHandler(ctx, policy, self.settings)
}

// Do not replace this with systemctl, a cached receipt, a Boolean waiver or an
// environment setting. Candidate/unknown branches must execute nothing else.
func nativeManagerProof(context.Context, Policy, Settings) error { return ErrManagerProofUnimplemented }

// Apport is a script. Executing /proc/self/fd/N with CLOEXEC can break its
// shebang, and reopening the path/interpreter loses descriptor identity. Neither
// a nil fake exec nor a metadata pin is evidence this native gap is solved.
func (nativeHost) Handoff(context.Context, Policy, Invocation, io.ReadCloser) error {
	return ErrOrdinaryHandoffUnimplemented
}

// The held cgroup root prevents a path swap from redirecting own-PID movement.
// Ordinary resource restoration is deliberately not approximated here.
type nativePlacement struct {
	root      *directory
	hierarchy *directory
	policy    Policy
	settings  Settings
	pid       int
	directory fileIdentity
}

// This concrete kernel half is reachable only after the absent native manager
// verifier is implemented. It is not called by tests or during development.
func joinNativeHandler(ctx context.Context, policy Policy, settings Settings) (*nativePlacement, error) {
	root, err := openDirectory("/sys/fs/cgroup", false)
	if err != nil {
		return nil, err
	}
	self := &nativePlacement{hierarchy: root, policy: policy, settings: settings, pid: os.Getpid()}
	failed := true
	defer func() {
		if failed {
			if self.root != nil {
				self.root.Close()
			}
			root.Close()
		}
	}()
	if err := requireFilesystem(root, 0x63677270); err != nil {
		return nil, err
	}
	current := ""
	for _, component := range strings.Split(strings.TrimPrefix(HandlerCgroup, "/"), "/") {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return nil, errors.New("core handler cgroup lacks exact protected components")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return nil, errors.New("core handler cgroup is not root-owned")
		}
	}
	self.root, err = root.OpenDirectory(strings.TrimPrefix(HandlerCgroup, "/"))
	if err != nil {
		return nil, err
	}
	if err := requireFilesystem(self.root, 0x63677270); err != nil {
		return nil, err
	}
	info, err := self.root.Stat(".")
	if err != nil {
		return nil, err
	}
	self.directory, err = identity(info)
	if err != nil || self.verifyKernel(ctx) != nil {
		return nil, errors.New("core handler has no positive bounded kernel resource proof")
	}
	file, err := self.root.OpenFile("cgroup.procs", os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	value := strconv.Itoa(os.Getpid()) + "\n"
	written, writeErr := file.WriteString(value)
	closeErr := file.Close()
	if written != len(value) || writeErr != nil || closeErr != nil || self.Recheck(ctx) != nil {
		return nil, errors.New("core helper own-PID-only move was not positively acknowledged")
	}
	failed = false
	return self, nil
}

// Cgroup kernel limits are independent of the required manager properties.
func (self *nativePlacement) verifyKernel(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := self.hierarchy.Lstat(strings.TrimPrefix(HandlerCgroup, "/"))
	if err != nil {
		return err
	}
	current, err := identity(info)
	if err != nil || current != self.directory {
		return errors.New("core handler parent identity changed")
	}
	for name, expected := range map[string]string{"memory.max": strconv.FormatUint(self.settings.HandlerMemoryBytes, 10), "memory.swap.max": "0",
		"pids.max": strconv.FormatUint(self.settings.HandlerTasksMaximum, 10), "cpu.max": fmtQuota(self.settings),
		"cpuset.cpus": self.policy.ManagementCpuSet, "cpuset.cpus.effective": self.policy.ManagementCpuSet} {
		content, err := readRelative(self.root, name, self.settings.ProcBytes, true)
		actual := strings.TrimSuffix(string(content), "\n")
		if err != nil || (name == "cpuset.cpus" || name == "cpuset.cpus.effective") && !sameCpuSet(actual, expected, self.settings.MaximumCpuIndex) ||
			name != "cpuset.cpus" && name != "cpuset.cpus.effective" && actual != expected {
			return errors.New("core handler kernel resource limit differs from its exact class")
		}
	}
	return ctx.Err()
}

// Membership is read after movement through the helper's own proc identity.
func (self *nativePlacement) Recheck(ctx context.Context) error {
	if os.Getpid() != self.pid || nativeManagerProof(ctx, self.policy, self.settings) != nil || self.verifyKernel(ctx) != nil {
		return errors.New("core helper manager/kernel placement proof changed")
	}
	host := nativeHost{settings: self.settings}
	value, err := host.kernelValue(ctx, strconv.Itoa(self.pid)+"/cgroup")
	if err != nil || value != "0::"+HandlerCgroup {
		return errors.New("core helper did not join its exact management parent")
	}
	return nil
}

// Go's pre-main runtime may already alter resources (notably nofile). A later
// proc snapshot cannot prove exact inherited restoration for ordinary Apport.
func (*nativePlacement) Restore(context.Context) error { return ErrOrdinaryHandoffUnimplemented }

func (self *nativePlacement) Close() error {
	return errors.Join(self.root.Close(), self.hierarchy.Close())
}

func fmtQuota(settings Settings) string {
	return strconv.FormatUint(settings.HandlerCpuQuota, 10) + " " + strconv.FormatUint(settings.HandlerCpuPeriod, 10)
}

// CPU sets are bounded exact unions; ranges cannot allocate arbitrarily.
func cpuSet(value string, maximum int) (map[uint64]bool, error) {
	if value == "" || maximum <= 0 || strings.ContainsAny(value, " \t\r\n") {
		return nil, errors.New("invalid finite core management CPU set")
	}
	result := map[uint64]bool{}
	for _, component := range strings.Split(value, ",") {
		first, last, ranged := strings.Cut(component, "-")
		start, err := parseUnsigned(first)
		if err != nil {
			return nil, errors.New("invalid first core management processor")
		}
		end := start
		if ranged {
			end, err = parseUnsigned(last)
		}
		if err != nil || start > end || end > uint64(maximum) {
			return nil, errors.New("core management CPU range is invalid or exceeds its bound")
		}
		for cpu := start; cpu <= end; cpu++ {
			if result[cpu] {
				return nil, errors.New("duplicate core management processor")
			}
			result[cpu] = true
		}
	}
	return result, nil
}

func validCpuSet(value string, maximum int) bool { _, err := cpuSet(value, maximum); return err == nil }

func sameCpuSet(first, second string, maximum int) bool {
	a, err := cpuSet(first, maximum)
	b, otherErr := cpuSet(second, maximum)
	if err != nil || otherErr != nil || len(a) != len(b) {
		return false
	}
	for cpu := range a {
		if !b[cpu] {
			return false
		}
	}
	return true
}
