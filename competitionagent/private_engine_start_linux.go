//go:build linux

// Docker must mount images in the same private mount namespace as containerd.
// Joining the network alone, or binding the same shim directory twice, is not
// sufficient: systemd BindPaths creates independent slave mount namespaces.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Startup may precede the first fingerprint, but this role cannot run a patch,
// serve SSH, or recover a job. All protected documents and executable pins are
// still mandatory; candidate authority continues to require qualification.
func LoadPrivateEngineStartupConfig(path string) (*Config, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("private engine startup requires the root role")
	}
	return loadConfigForPurpose(path, true)
}

// This root-only startup role accepts no command or namespace from its caller.
// The live pinned containerd invocation owns the descriptor;
// exec preserves systemd's main PID and the daemon's original resource limits.
// It neither admits an evaluation nor claims host qualification.
func StartPrivateEngine(ctx context.Context, config *Config) error {
	if os.Geteuid() != 0 || config == nil {
		return errors.New("private engine startup requires root metadata")
	}
	executable, err := os.Executable()
	if err != nil || executable != config.Agent.Path {
		return errors.New("private engine startup is not the installed agent")
	}
	if err := config.validateForPurpose(true, true); err != nil {
		return err
	}
	operator, err := trustedOperatorUid(config.TrustedOperatorUser)
	if err != nil {
		return err
	}
	worker, err := user.Lookup(config.WorkerUser)
	if err != nil {
		return err
	}
	workerUid, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		return err
	}
	workerGid, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		return err
	}
	scope := RuntimeResourceScope{BaseDirectory: config.BaseDirectory, StateDirectory: config.StateDirectory, WorkDirectory: config.WorkDirectory, ScratchDirectory: config.DockerRole.ScratchDirectory, TrustedOperatorUid: operator, WorkerUid: uint32(workerUid), WorkerGid: uint32(workerGid)}
	settings := DefaultRuntimeResourceBoundarySettings()
	host := runtimeBoundaryNativeHost{}
	if _, err := readDockerEngineProfile(config.DockerRole.ResourceBoundary, scope, host, settings); err != nil {
		return err
	}
	content, err := host.Document(config.DockerRole.ResourceBoundary, settings.DocumentBytes)
	if err != nil {
		return err
	}
	var boundary RuntimeResourceBoundary
	if err := decodeStrict(content, &boundary); err != nil {
		return err
	}
	var candidateStat, deviceStat syscall.Stat_t
	if err := syscall.Lstat(boundary.CandidateRoot, &candidateStat); err != nil {
		return err
	}
	if err := syscall.Lstat(boundary.BlockDevice, &deviceStat); err != nil {
		return err
	}
	if err := validatePrivateEngineStartupDevice(candidateStat, deviceStat); err != nil {
		return err
	}
	boundary.WorkDeviceId = deviceStat.Rdev
	for _, pin := range []Pin{boundary.Dockerd, boundary.Nsenter} {
		if err := validatePin(pin, true); err != nil {
			return err
		}
	}
	unit, err := runtimeBoundaryReadUnit(ctx, boundary, boundary.ContainerdUnit, settings)
	if err != nil {
		return err
	}
	unit, err = runtimeBoundaryParticipant(unit, boundary, settings)
	if err != nil {
		return err
	}
	current, currentIdentity, err := runtimeBoundaryOpenNamespace("/proc/self/ns/net")
	if err != nil {
		return err
	}
	_ = current.Close()
	hostNetwork, hostIdentity, err := runtimeBoundaryOpenNamespace("/proc/1/ns/net")
	if err != nil {
		return err
	}
	_ = hostNetwork.Close()
	if currentIdentity != unit.Namespace || currentIdentity == hostIdentity {
		return errors.New("engine startup is outside containerd's private network")
	}
	proc := "/proc/" + strconv.Itoa(unit.Pid)
	target, err := os.Readlink(filepath.Join(proc, "exe"))
	arguments, argumentsErr := readBounded(filepath.Join(proc, "cmdline"), settings.DocumentBytes)
	if err != nil || target != boundary.Containerd.Path || argumentsErr != nil || !bytes.Equal(arguments, []byte(boundary.Containerd.Path+"\x00--config\x00"+boundary.ContainerdConfig.Path+"\x00")) {
		return errors.New("containerd startup peer changed its executable or configuration")
	}
	if err := validatePin(boundary.Containerd, true); err != nil {
		return err
	}
	if err := verifyRuntimeBoundaryShimSocketMount(proc, boundary); err != nil {
		return err
	}
	file, identity, err := runtimeBoundaryOpenNamespace(filepath.Join(proc, "ns", "mnt"))
	if err != nil {
		return err
	}
	defer file.Close()
	after, err := runtimeBoundaryParticipant(unit, boundary, settings)
	if err != nil || after.ProcessStart != unit.ProcessStart || identity != after.MountNamespace {
		return errors.New("containerd changed while acquiring its image mount namespace")
	}
	// Keep the held namespace across nsenter's exec; no numeric PID is used by
	// the join, so PID reuse cannot redirect startup to a different namespace.
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	launchArguments := []string{boundary.Nsenter.Path, "--mount=/proc/self/fd/" + strconv.FormatUint(uint64(file.Fd()), 10), "--", boundary.Dockerd.Path, "--config-file", boundary.EngineConfig.Path}
	return syscall.Exec(boundary.Nsenter.Path, launchArguments, os.Environ())
}

// A missing mount must not place engine writes on the operator data/root
// filesystem. Full quota/core/pressure admission remains a separate gate.
func validatePrivateEngineStartupDevice(candidate, device syscall.Stat_t) error {
	if candidate.Mode&syscall.S_IFMT != syscall.S_IFDIR || candidate.Uid != 0 || candidate.Mode&0022 != 0 ||
		device.Mode&syscall.S_IFMT != syscall.S_IFBLK || device.Uid != 0 || device.Mode&0077 != 0 || device.Rdev == 0 || candidate.Dev != device.Rdev {
		return errors.New("private engine startup has no protected candidate block mount")
	}
	return nil
}
