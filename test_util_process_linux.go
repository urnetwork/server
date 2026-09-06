//go:build linux

package server

// A fresh descriptor-anchored cgroup capability is one-shot and never adopted
// from an existing pathname. Run is externally managed; Close refuses reuse
// until the guardian and its complete child tree are joined.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Methods serialize using cancellable admission. A running or uncertain tree
// remains owned; no second generation can consume this capability.
type TestProcessCgroup struct {
	admission chan struct{}
	parent    *os.File
	directory *os.File
	name      string
	identity  unix.Stat_t
	used      bool
	closed    bool
	job       *testProcessJob
}

// The waiter owns the only direct Wait. Its completion is retained even when
// an original deadline expires, so a later Close can observe, but never rerun it.
type testProcessJob struct {
	done      chan struct{}
	owner     *os.File
	ownerOnce sync.Once
	result    TestProcessResult
	err       error
}

// Distinguishes terminal, independently verified tree state from ordinary IPC.
type testProcessGuardianStatus struct {
	Kind                 string `json:"kind"`
	Token                string `json:"token"`
	Root                 string `json:"root"`
	PID                  int    `json:"pid"`
	ExitCode             int    `json:"exit_code"`
	Ready                bool   `json:"ready"`
	Claimed              bool   `json:"claimed"`
	Reaped               int    `json:"reaped"`
	Empty                bool   `json:"empty"`
	NoChildren           bool   `json:"no_children"`
	LeaderJoined         bool   `json:"leader_joined"`
	ConfigurationChecked bool   `json:"configuration_checked"`
	Reason               string `json:"reason"`
	CgroupDevice         uint64 `json:"cgroup_device"`
	CgroupInode          uint64 `json:"cgroup_inode"`
}

// Creates only a fresh child under the caller's explicit delegated capability.
// It never targets the delegation itself, an existing scope, or a guessed path.
func CreateTestProcessCgroup(delegation *os.File) (*TestProcessCgroup, error) {
	parent, err := openTestProcessAt(delegation, ".", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	var parentStat unix.Stat_t
	var filesystem unix.Statfs_t
	if err := unix.Fstat(int(parent.Fd()), &parentStat); err != nil {
		parent.Close()
		return nil, err
	}
	if err := unix.Fstatfs(int(parent.Fd()), &filesystem); err != nil {
		parent.Close()
		return nil, err
	}
	if filesystem.Type != unix.CGROUP2_SUPER_MAGIC || parentStat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		parentStat.Uid != uint32(os.Geteuid()) || parentStat.Mode&0o022 != 0 {
		parent.Close()
		return nil, errors.New("owned test process requires an explicit private cgroup-v2 delegation")
	}
	token, err := newTestProcessToken()
	if err != nil {
		parent.Close()
		return nil, err
	}
	name := "urnetwork-test-" + token
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
		parent.Close()
		return nil, err
	}
	var created unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &created, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		parent.Close()
		return nil, err
	}
	directory, err := openTestProcessAt(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		// No descriptor was acquired: preserve the new entry, never remove by
		// name after a failed acquisition which could have raced a replacement.
		parent.Close()
		return nil, err
	}
	var identity unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &identity); err != nil {
		directory.Close()
		parent.Close()
		return nil, err
	}
	if identity.Dev != created.Dev || identity.Ino != created.Ino || identity.Uid != uint32(os.Geteuid()) ||
		identity.Mode&unix.S_IFMT != unix.S_IFDIR {
		directory.Close()
		parent.Close()
		return nil, errors.New("fresh owned test cgroup changed during descriptor acquisition")
	}
	// Only this newly created, anchored directory is made private. Existing
	// ancestry and caller-owned delegations are never chmodded or repaired.
	if err := directory.Chmod(0o700); err != nil {
		directory.Close()
		parent.Close()
		return nil, err
	}
	if err := unix.Fstat(int(directory.Fd()), &identity); err != nil {
		directory.Close()
		parent.Close()
		return nil, err
	}
	owner := &TestProcessCgroup{
		admission: make(chan struct{}, 1), parent: parent, directory: directory,
		name: name, identity: identity,
	}
	owner.admission <- struct{}{}
	if identity.Uid != uint32(os.Geteuid()) || identity.Mode&unix.S_IFMT != unix.S_IFDIR ||
		identity.Mode&0o077 != 0 {
		return owner, errors.New("fresh owned test cgroup is not private")
	}
	if err := owner.checkIdentity(); err != nil {
		return owner, err
	}
	if empty, err := testProcessCgroupEmpty(directory); err != nil || !empty {
		return owner, errors.Join(err, errors.New("fresh owned test cgroup is not empty"))
	}
	if value, err := readTestProcessCgroupFile(directory, "cgroup.type"); err != nil || string(value) != "domain\n" {
		return owner, errors.Join(err, errors.New("owned test cgroup is not a domain"))
	}
	return owner, nil
}

// Resolves the created entry back to the retained inode before any mutation.
func (self *TestProcessCgroup) checkIdentity() error {
	var current unix.Stat_t
	if err := unix.Fstatat(int(self.parent.Fd()), self.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if current.Dev != self.identity.Dev || current.Ino != self.identity.Ino ||
		current.Mode&unix.S_IFMT != unix.S_IFDIR || current.Uid != self.identity.Uid {
		return errors.New("owned test cgroup entry was replaced")
	}
	var retained unix.Stat_t
	if err := unix.Fstat(int(self.directory.Fd()), &retained); err != nil {
		return err
	}
	if retained.Dev != current.Dev || retained.Ino != current.Ino {
		return errors.New("owned test cgroup descriptor differs")
	}
	return nil
}

// Cgroup pseudo-files have size zero, so their explicit read bound is separate.
func readTestProcessCgroupFile(directory *os.File, name string) (value []byte, resultErr error) {
	file, err := openTestProcessAt(directory, name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	value, err = io.ReadAll(io.LimitReader(file, testProcessIPCBytes+1))
	if err != nil || len(value) > testProcessIPCBytes {
		return nil, errors.Join(err, errors.New("owned test cgroup metadata exceeds its bound"))
	}
	return value, nil
}

// The kernel's populated field covers nested cgroups, not only the leader PID.
func testProcessCgroupEmpty(directory *os.File) (bool, error) {
	value, err := readTestProcessCgroupFile(directory, "cgroup.events")
	if err != nil {
		return false, err
	}
	found := false
	empty := false
	for _, line := range strings.Split(strings.TrimSuffix(string(value), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return false, errors.New("owned test cgroup events are malformed")
		}
		if fields[0] == "populated" {
			if found || (fields[1] != "0" && fields[1] != "1") {
				return false, errors.New("owned test cgroup population is malformed")
			}
			found = true
			empty = fields[1] == "0"
		}
	}
	if !found {
		return false, errors.New("owned test cgroup population is unavailable")
	}
	return empty, nil
}

// The complete tree is killed through its immutable capability, never a PID,
// process group guess, parent cgroup or age-derived resource name.
func killTestProcessCgroup(directory *os.File) (resultErr error) {
	file, err := openTestProcessAt(directory, "cgroup.kill", unix.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if count, err := file.Write([]byte("1")); err != nil {
		return err
	} else if count != 1 {
		return io.ErrShortWrite
	}
	return nil
}

// Copies bounded exact executable bytes into a kernel-sealed executable memfd.
// Even dependency init therefore cannot run from a changed original inode.
func sealTestProcessExecutable(original *os.File, size int64, expected string) (sealed *os.File, resultErr error) {
	if err := verifyTestProcessExecutable(original, size, expected); err != nil {
		return nil, err
	}
	fd, err := unix.MemfdCreate("urnetwork-test-executable", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	if err != nil {
		return nil, err
	}
	sealed = os.NewFile(uintptr(fd), "owned-test-executable")
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, sealed.Close())
			sealed = nil
		}
	}()
	digest := sha256.New()
	count, err := io.Copy(io.MultiWriter(sealed, digest), io.NewSectionReader(original, 0, size+1))
	if err != nil || count != size || hex.EncodeToString(digest.Sum(nil)) != expected {
		return sealed, errors.Join(err, errors.New("owned test executable changed during sealing"))
	}
	if err := sealed.Chmod(0o500); err != nil {
		return sealed, err
	}
	if _, err := unix.FcntlInt(sealed.Fd(), unix.F_ADD_SEALS, testProcessSeals); err != nil {
		return sealed, err
	}
	return sealed, nil
}

// Starts only the independent guardian. It starts the worker atomically in the
// cgroup after becoming a subreaper and arming the sole-owner liveness channel.
func (self *TestProcessCgroup) Run(ctx context.Context, spec TestProcessSpec) (TestProcessResult, error) {
	select {
	case <-ctx.Done():
		return TestProcessResult{}, ctx.Err()
	case <-self.admission:
	}
	defer func() { self.admission <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return TestProcessResult{}, err
	}
	if self.closed || self.used {
		return TestProcessResult{}, ErrTestProcessGenerationUsed
	}
	deadline, ok := ctx.Deadline()
	if !ok || spec.JoinReserve <= 0 || spec.JoinReserve >= time.Until(deadline) ||
		!validTestProcessRoot(spec.Root) || spec.Parallel <= 0 ||
		!strings.HasPrefix(spec.WorkingDirectory, "/") || strings.ContainsRune(spec.WorkingDirectory, 0) {
		return TestProcessResult{}, errors.New("owned test process requires a root, absolute directory and original deadline with join reserve")
	}
	if err := self.checkIdentity(); err != nil {
		return TestProcessResult{}, err
	}
	if empty, err := testProcessCgroupEmpty(self.directory); err != nil || !empty {
		return TestProcessResult{}, errors.Join(err, ErrTestProcessUnjoined)
	}
	actualConfiguration, err := snapshotTestProcessConfiguration(ctx, spec.Configuration.Directory, spec.Configuration.Limits)
	if err != nil || actualConfiguration != spec.Configuration.SHA256 || !validTestProcessDigest(spec.Configuration.SHA256) {
		return TestProcessResult{}, errors.Join(err, errors.New("owned test process configuration identity differs"))
	}
	if _, err := testProcessEnvironment(spec.Environment, "worker"); err != nil {
		return TestProcessResult{}, err
	}
	if spec.TerminalStatus != nil {
		var observerStat unix.Stat_t
		if err := unix.Fstat(int(spec.TerminalStatus.Fd()), &observerStat); err != nil {
			return TestProcessResult{}, err
		}
		if observerStat.Mode&unix.S_IFMT != unix.S_IFIFO || observerStat.Uid != uint32(os.Geteuid()) {
			return TestProcessResult{}, errors.New("owned test guardian observer must be a private caller-owned pipe")
		}
	}
	executable, err := sealTestProcessExecutable(spec.Executable, spec.ExecutableBytes, spec.ExecutableSHA256)
	if err != nil {
		return TestProcessResult{}, err
	}
	defer executable.Close()
	configuration, err := sealTestProcessConfiguration(ctx, spec.Configuration)
	if err != nil {
		return TestProcessResult{}, err
	}
	defer configuration.Close()
	token, err := newTestProcessToken()
	if err != nil {
		return TestProcessResult{}, err
	}
	capsule := testProcessCapsule{
		Schema: 1, Role: "guardian", Token: token, ParentPID: os.Getpid(), Root: spec.Root,
		Arguments:   []string{"/proc/self/fd/3", "--owned-test-guardian"},
		Environment: append([]string(nil), spec.Environment...), WorkingDirectory: spec.WorkingDirectory,
		Parallel: spec.Parallel, ExecutionDeadline: deadline.Add(-spec.JoinReserve).UnixNano(),
		OverallDeadline: deadline.UnixNano(), ExecutableSHA256: spec.ExecutableSHA256,
		ExecutableBytes: spec.ExecutableBytes, ConfigurationSHA256: spec.Configuration.SHA256,
		ConfigurationLimits: spec.Configuration.Limits,
		CgroupDevice:        uint64(self.identity.Dev), CgroupInode: self.identity.Ino,
		TerminalObserver: spec.TerminalStatus != nil,
	}
	capsuleFile, err := sealTestProcessCapsule(capsule)
	if err != nil {
		return TestProcessResult{}, err
	}
	defer capsuleFile.Close()
	ownerRead, ownerWrite, err := os.Pipe()
	if err != nil {
		return TestProcessResult{}, err
	}
	defer ownerRead.Close()
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		ownerWrite.Close()
		return TestProcessResult{}, err
	}
	defer statusWrite.Close()
	command := exec.Command("/proc/self/fd/3", capsule.Arguments[1:]...)
	command.Args = capsule.Arguments
	// The guardian gets no inherited environment or service credentials.
	command.Env = []string{testProcessModeKey + "=guardian"}
	command.Dir = spec.WorkingDirectory
	command.Stdin, command.Stdout, command.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	command.ExtraFiles = []*os.File{executable, capsuleFile, self.directory, ownerRead, configuration, statusWrite}
	if spec.TerminalStatus != nil {
		command.ExtraFiles = append(command.ExtraFiles, spec.TerminalStatus)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ctx.Err(); err != nil {
		ownerWrite.Close()
		statusRead.Close()
		return TestProcessResult{}, err
	}
	// Consumption precedes the first process effect; even launch failure never
	// lets this same capability silently become a second service generation.
	self.used = true
	if err := command.Start(); err != nil {
		ownerWrite.Close()
		statusRead.Close()
		return TestProcessResult{}, err
	}
	ownerRead.Close()
	statusWrite.Close()
	job := &testProcessJob{done: make(chan struct{}), owner: ownerWrite}
	self.job = job
	// This sole waiter is lifecycle-owned by the capability until done closes.
	go self.waitGuardian(command, statusRead, capsule, job)
	select {
	case <-job.done:
		return job.result, errors.Join(ctx.Err(), job.err)
	case <-ctx.Done():
		job.ownerOnce.Do(func() { job.owner.Close() })
	}
	// Cancellation requests whole-tree kill, then uses only the original
	// remaining join budget. No test execution deadline is reset.
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return TestProcessResult{Started: true}, errors.Join(ctx.Err(), ErrTestProcessUnjoined)
	}
	select {
	case <-job.done:
		return job.result, errors.Join(ctx.Err(), job.err)
	case <-time.After(remaining):
		return TestProcessResult{Started: true}, errors.Join(ctx.Err(), ErrTestProcessUnjoined)
	}
}

// Reads a bounded terminal record after guardian exit; no stdout text can
// impersonate it. The goroutine remains reachable through its owning job.
func (self *TestProcessCgroup) waitGuardian(command *exec.Cmd, status *os.File, capsule testProcessCapsule, job *testProcessJob) {
	defer close(job.done)
	defer status.Close()
	defer job.ownerOnce.Do(func() { job.owner.Close() })
	waitErr := command.Wait()
	value, readErr := io.ReadAll(io.LimitReader(status, 3*testProcessIPCBytes+1))
	terminal := testProcessGuardianStatus{}
	records := 0
	decoder := bufio.NewScanner(strings.NewReader(string(value)))
	decoder.Buffer(make([]byte, testProcessIPCBytes), testProcessIPCBytes)
	var protocolErr error
	for decoder.Scan() {
		records++
		var observed testProcessGuardianStatus
		if records > 2 || decodeTestProcessJSON(decoder.Bytes(), &observed) != nil ||
			observed.Token != capsule.Token || observed.Root != capsule.Root ||
			observed.CgroupDevice != capsule.CgroupDevice || observed.CgroupInode != capsule.CgroupInode ||
			(records == 1 && observed.Kind != "armed") || (records == 2 && observed.Kind != "terminal") {
			protocolErr = errors.New("owned test guardian status differs")
			break
		}
		terminal = observed
	}
	if records != 2 || decoder.Err() != nil || len(value) > 3*testProcessIPCBytes {
		protocolErr = errors.New("owned test guardian terminal status is unavailable")
	}
	empty, emptyErr := testProcessCgroupEmpty(self.directory)
	joined := protocolErr == nil && readErr == nil && terminal.Empty && terminal.NoChildren &&
		(terminal.PID == 0 || (terminal.LeaderJoined && terminal.Reaped > 0)) && empty && emptyErr == nil
	job.result = TestProcessResult{
		Started: terminal.PID > 0, Joined: joined, PID: terminal.PID, ExitCode: terminal.ExitCode,
		GenerationClaimed: terminal.Claimed, ReapedProcesses: terminal.Reaped,
	}
	if !joined {
		// Failure of the guardian never becomes permission to clean or reuse.
		killErr := killTestProcessCgroup(self.directory)
		job.err = errors.Join(ErrTestProcessUnjoined, waitErr, readErr, protocolErr, emptyErr, killErr)
		return
	}
	if waitErr != nil || terminal.Reason != "complete" || !terminal.Ready || !terminal.Claimed || !terminal.ConfigurationChecked || terminal.ExitCode != 0 {
		job.err = errors.Join(waitErr, fmt.Errorf("owned test root failed: %s", terminal.Reason))
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, capsule.OverallDeadline))
	defer cancel()
	// Guardian admission and final checks bind the kernel-sealed archive.
	// Runtime reads never return to the caller's mutable source directory.
	job.err = ctx.Err()
}

// Refuses to remove a populated, replaced or unjoined capability. An uncertain
// failed execution is preserved for explicit investigation, never age reaped.
func (self *TestProcessCgroup) Close(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-self.admission:
	}
	defer func() { self.admission <- struct{}{} }()
	if self.closed {
		return nil
	}
	if self.job != nil {
		self.job.ownerOnce.Do(func() { self.job.owner.Close() })
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), ErrTestProcessUnjoined)
		case <-self.job.done:
		}
		if !self.job.result.Joined {
			return ErrTestProcessUnjoined
		}
	}
	if err := self.checkIdentity(); err != nil {
		return err
	}
	if empty, err := testProcessCgroupEmpty(self.directory); err != nil || !empty {
		return errors.Join(err, ErrTestProcessUnjoined)
	}
	if err := unix.Unlinkat(int(self.parent.Fd()), self.name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	self.closed = true
	return errors.Join(self.directory.Close(), self.parent.Close())
}
