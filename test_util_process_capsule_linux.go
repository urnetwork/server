//go:build linux

package server

// Private sealed capsules admit the guardian and worker before env.go init.
// A capsule is an execution capability, never a database or Redis grant.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	testProcessModeKey  = "URNETWORK_OWNED_TEST_PROCESS"
	testProcessIPCBytes = 4096
	testProcessSeals    = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
)

// The exact argument vector and immutable content identities are sealed before
// exec. The worker capsule is derived once by its independently living guardian.
type testProcessCapsule struct {
	Schema              int                            `json:"schema"`
	Role                string                         `json:"role"`
	Token               string                         `json:"token"`
	ParentPID           int                            `json:"parent_pid"`
	Root                string                         `json:"root"`
	Arguments           []string                       `json:"arguments"`
	Environment         []string                       `json:"environment"`
	WorkingDirectory    string                         `json:"working_directory"`
	Parallel            int                            `json:"parallel"`
	ExecutionDeadline   int64                          `json:"execution_deadline"`
	OverallDeadline     int64                          `json:"overall_deadline"`
	ExecutableSHA256    string                         `json:"executable_sha256"`
	ExecutableBytes     int64                          `json:"executable_bytes"`
	ConfigurationSHA256 string                         `json:"configuration_sha256"`
	ConfigurationLimits TestProcessConfigurationLimits `json:"configuration_limits"`
	CgroupDevice        uint64                         `json:"cgroup_device"`
	CgroupInode         uint64                         `json:"cgroup_inode"`
	TerminalObserver    bool                           `json:"terminal_observer"`
}

// A strict top-level test name cannot inject regex alternatives or nested roots.
func validTestProcessRoot(root string) bool {
	return regexp.MustCompile(`^Test[A-Za-z0-9_]+$`).MatchString(root)
}

// Config paths, credentials and execution-control environment are not inherited.
// Extra nonsecret fixture keys are explicit and carry no service authority.
func testProcessEnvironment(values []string, role string) ([]string, error) {
	result := []string{
		testProcessModeKey + "=" + role,
		"WARP_HOME=/proc/self/fd/7",
		"WARP_VAULT_HOME=/proc/self/fd/7/vault",
		"WARP_CONFIG_HOME=/proc/self/fd/7/config",
		"WARP_SITE_HOME=/proc/self/fd/7/site",
	}
	seen := map[string]bool{}
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if !ok || seen[key] || strings.ContainsRune(value, 0) {
			return nil, errors.New("owned test process environment is not canonical")
		}
		switch key {
		case "PATH", "LANG", "LC_ALL", "TZ", "GOMAXPROCS", "WARP_ENV", "WARP_HOST", "WARP_CONFIG_VERSION":
		default:
			if !strings.HasPrefix(key, "URNETWORK_TEST_PROCESS_FIXTURE_") {
				return nil, errors.New("owned test process environment contains an unapproved key")
			}
		}
		seen[key] = true
		result = append(result, value)
	}
	return result, nil
}

// Caps the single atomic pipe write below PIPE_BUF and rejects short writes.
func writeTestProcessStatus(file *os.File, status testProcessStatus) error {
	value, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if len(value) >= testProcessIPCBytes {
		return errors.New("owned test process status exceeds its bound")
	}
	value = append(value, '\n')
	count, err := file.Write(value)
	if err != nil {
		return err
	}
	if count != len(value) {
		return io.ErrShortWrite
	}
	return nil
}

// Inherited ExtraFiles are blocking descriptors. Set nonblocking before
// os.NewFile so the Go poller can actually enforce the original IPC deadline.
func adoptTestProcessPipe(fd int, name string) (*os.File, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFIFO || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("owned test process IPC descriptor is not a private pipe")
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Retains an immutable kernel-sealed record; no named file or rename is used.
func sealTestProcessCapsule(capsule testProcessCapsule) (file *os.File, resultErr error) {
	value, err := json.Marshal(capsule)
	if err != nil || len(value) > testProcessIPCBytes {
		return nil, errors.New("owned test process capsule exceeds its bound")
	}
	fd, err := unix.MemfdCreate("urnetwork-test-execution", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	file = os.NewFile(uintptr(fd), "owned-test-capsule")
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, file.Close())
			file = nil
		}
	}()
	if count, err := file.Write(value); err != nil {
		return file, err
	} else if count != len(value) {
		return file, io.ErrShortWrite
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, testProcessSeals); err != nil {
		return file, err
	}
	return file, nil
}

// Rejects mutable or noncanonical capsules even if their JSON fields look valid.
func readTestProcessCapsule(file *os.File) (testProcessCapsule, error) {
	var capsule testProcessCapsule
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&testProcessSeals != testProcessSeals {
		return capsule, errors.New("owned test process capsule is not kernel sealed")
	}
	value, err := io.ReadAll(io.NewSectionReader(file, 0, testProcessIPCBytes+1))
	if err != nil || len(value) > testProcessIPCBytes {
		return capsule, errors.New("owned test process capsule is unavailable or oversized")
	}
	if err := decodeTestProcessJSON(value, &capsule); err != nil {
		return capsule, err
	}
	if capsule.Schema != 1 || !validTestProcessRoot(capsule.Root) ||
		!validTestProcessDigest(capsule.Token) || capsule.ParentPID <= 0 ||
		capsule.Parallel <= 0 || capsule.ExecutionDeadline <= 0 ||
		capsule.ExecutionDeadline >= capsule.OverallDeadline ||
		!validTestProcessDigest(capsule.ConfigurationSHA256) {
		return capsule, errors.New("owned test process capsule identity differs")
	}
	if capsule.Role != "guardian" && capsule.Role != "worker" {
		return capsule, errors.New("owned test process capsule role differs")
	}
	if !time.Now().Before(time.Unix(0, capsule.ExecutionDeadline)) {
		return capsule, context.DeadlineExceeded
	}
	return capsule, nil
}

// Descriptor identity, not a /proc path spelling, binds the admitted containment.
func validateTestProcessCgroupIdentity(directory *os.File, capsule testProcessCapsule) error {
	var stat unix.Stat_t
	var filesystem unix.Statfs_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if err := unix.Fstatfs(int(directory.Fd()), &filesystem); err != nil {
		return err
	}
	if filesystem.Type != unix.CGROUP2_SUPER_MAGIC || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		stat.Uid != uint32(os.Geteuid()) || uint64(stat.Dev) != capsule.CgroupDevice || stat.Ino != capsule.CgroupInode {
		return errors.New("owned test process cgroup descriptor identity differs")
	}
	return nil
}

// The worker checks real kernel membership before any package init or root code.
// It does not retain the cgroup control descriptor after this admission.
func validateTestProcessMembership(directory *os.File) error {
	value, err := readTestProcessCgroupFile(directory, "cgroup.procs")
	if err != nil {
		return err
	}
	wanted := strconv.Itoa(os.Getpid())
	for _, member := range strings.Fields(string(value)) {
		if member == wanted {
			return nil
		}
	}
	return errors.New("owned test process did not enter its cgroup before initialization")
}

// Validates descriptors before any env.go initializer, then removes private
// launch state from the environment. Descendant execs inherit no control FDs.
func loadTestProcessChild() *testProcessChildState {
	role := os.Getenv(testProcessModeKey)
	if role == "" {
		return nil
	}
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "owned test process admission refused: %v\n", err)
		os.Exit(125)
	}
	capsuleFile := os.NewFile(4, "owned-test-capsule")
	unix.CloseOnExec(4)
	capsule, err := readTestProcessCapsule(capsuleFile)
	if err != nil {
		fail(err)
	}
	if err := capsuleFile.Close(); err != nil {
		fail(err)
	}
	if capsule.Role != role || capsule.ParentPID != os.Getppid() ||
		len(os.Args) != len(capsule.Arguments) {
		fail(errors.New("owned test process launcher identity differs"))
	}
	for index, argument := range os.Args {
		if argument != capsule.Arguments[index] {
			fail(errors.New("owned test process arguments differ"))
		}
	}
	executable := os.NewFile(3, "owned-test-executable")
	unix.CloseOnExec(3)
	if seals, err := unix.FcntlInt(executable.Fd(), unix.F_GET_SEALS, 0); err != nil || seals&testProcessSeals != testProcessSeals {
		fail(errors.New("owned test executable is not kernel sealed"))
	}
	if err := verifyTestProcessExecutable(executable, capsule.ExecutableBytes, capsule.ExecutableSHA256); err != nil {
		fail(err)
	}
	actualExecutable, err := os.Open("/proc/self/exe")
	if err != nil {
		fail(err)
	}
	expectedInfo, expectedErr := executable.Stat()
	actualInfo, actualErr := actualExecutable.Stat()
	closeErr := actualExecutable.Close()
	if expectedErr != nil || actualErr != nil || closeErr != nil || !os.SameFile(expectedInfo, actualInfo) {
		fail(errors.New("owned test process executable descriptor differs"))
	}
	cgroup := os.NewFile(5, "owned-test-cgroup")
	unix.CloseOnExec(5)
	if err := validateTestProcessCgroupIdentity(cgroup, capsule); err != nil {
		fail(err)
	}
	if err := os.Unsetenv(testProcessModeKey); err != nil {
		fail(err)
	}
	if role == "guardian" {
		// No application init or root is ever invoked by the guardian.
		os.Exit(runTestProcessGuardian(capsule, executable, cgroup))
	}
	if err := executable.Close(); err != nil {
		fail(err)
	}
	if err := validateTestProcessMembership(cgroup); err != nil {
		fail(err)
	}
	if err := cgroup.Close(); err != nil {
		fail(err)
	}
	configuration := os.NewFile(7, "owned-test-configuration")
	unix.CloseOnExec(7)
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, capsule.ExecutionDeadline))
	archive, configurationErr := readTestProcessConfigurationArchive(ctx, configuration, capsule.ConfigurationLimits, capsule.ConfigurationSHA256)
	if configurationErr != nil {
		cancel()
		fail(errors.New("owned test process configuration differs before initialization"))
	}
	view, viewErr := materializeTestProcessConfiguration(ctx, archive, capsule.ConfigurationLimits, capsule.ConfigurationSHA256)
	cancel()
	if viewErr != nil {
		fail(viewErr)
	}
	expectedEnvironment, err := testProcessEnvironment(capsule.Environment, role)
	if err != nil {
		fail(err)
	}
	for _, value := range expectedEnvironment {
		key, expected, _ := strings.Cut(value, "=")
		if key != testProcessModeKey && os.Getenv(key) != expected {
			fail(errors.New("owned test process environment differs"))
		}
	}
	status, err := adoptTestProcessPipe(6, "owned-test-status")
	if err != nil {
		fail(err)
	}
	if err := status.SetWriteDeadline(time.Unix(0, capsule.ExecutionDeadline)); err != nil {
		fail(err)
	}
	child := &testProcessChildState{
		root: capsule.Root, deadline: time.Unix(0, capsule.ExecutionDeadline),
		token: capsule.Token, status: status,
	}
	if err := writeTestProcessStatus(status, testProcessStatus{
		Kind: "ready", Token: capsule.Token, Root: capsule.Root, PID: os.Getpid(),
	}); err != nil {
		fail(err)
	}
	// Publish only after complete content/environment/IPC admission. All
	// resolver reads use kernel-sealed files, never the caller's directory.
	ownedTestProcessConfiguration = view
	retainedTestProcessConfiguration = configuration
	return child
}

// Retains the sealed archive for the whole root, closed across descendant exec.
var retainedTestProcessConfiguration *os.File

// Generates an opaque execution token without encoding path or service data.
func newTestProcessToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
