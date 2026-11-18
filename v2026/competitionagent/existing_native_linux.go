//go:build linux

// Existing-run proof reads the original service executable, manager generation
// and actual root-owned cgroup caps. It performs no daemon, slice or file write.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const existingRunUnitProperties = "Id,LoadState,ActiveState,SubState,FragmentPath,DropInPaths,Slice,ControlGroup,User,Group,Type,Restart,KillMode,ExecStart,ExecStopPost,MainPID,InvocationID,ExecMainStartTimestampMonotonic,MemoryMax,MemorySwapMax,CPUQuotaPerSecUSec,CPUQuotaPeriodUSec,AllowedCPUs,CPUAffinity,RuntimeMaxUSec,TimeoutStopUSec,BindsTo,OnFailure,OnFailureJobMode,LimitCORE,UMask,Job"

// Both finite observations must name the same native service invocation around
// held process/executable/cgroup descriptors and the original run's parent caps.
func (self *Agent) proveExistingRun(ctx context.Context, manifest *StartManifest, plan *cleanupPlan, state *runState) (string, error) {
	manager, err := recoverySystemctlPin(self.config, self.settings, true)
	if err != nil {
		return "", err
	}
	policy, err := loadManagementResourcePolicy(self.config.ManagementPolicy, DefaultManagementBoundarySettings(), self.settings.ConfigBytes, true)
	if err != nil {
		return "", err
	}
	if err := verifyManagementOrdinaryForPurpose(ctx, policy, manager, self.settings, managementExistingContainment); err != nil {
		return "", err
	}
	unit := unitName(manifest.Identity.RunId)
	before, values, err := readExistingRunUnit(ctx, manager, unit, self.settings)
	if err != nil {
		return "", err
	}
	if terminalState(state.State) {
		reaped, err := self.loadResourcesReaped(manifest.Identity.RunId, state.CleanupPlanSha256)
		if err != nil || !availableRunState(state) || !reaped || values["ActiveState"] != "inactive" && values["ActiveState"] != "failed" || values["MainPID"] != "0" || values["Job"] != "" && values["Job"] != "0" {
			return "", errors.New("existing terminal state lacks current original-unit stop and authenticated resource cleanup")
		}
	} else {
		if !state.ExecutionStarted || state.State != "running" || state.CancelRequested || state.CleanupVerified || state.StopCleanupVerified {
			return "", errors.New("existing execution has no already-started native owner")
		}
		boundary, err := self.verifyExistingRuntimeBoundary(ctx, *plan)
		if err != nil {
			return "", err
		}
		defer boundary.Close()
		if err := validateExistingRunUnit(values, self.config, manifest.Identity, *plan, self.settings, false); err != nil {
			return "", err
		}
		if err := verifyExistingRunProcess(ctx, values, self.config, *plan, self.settings); err != nil {
			return "", err
		}
		if err := self.verifyExistingParentLimits(ctx, *plan); err != nil {
			return "", err
		}
		ownerBefore, ownerValues, err := readExistingRunUnit(ctx, manager, dockerOwnerUnitName(manifest.Identity.RunId), self.settings)
		if err != nil {
			return "", err
		}
		if err := validateExistingRunUnit(ownerValues, self.config, manifest.Identity, *plan, self.settings, true); err != nil {
			return "", err
		}
		if err := verifyExistingRunProcess(ctx, ownerValues, self.config, *plan, self.settings); err != nil {
			return "", err
		}
		ownerAfter, _, err := readExistingRunUnit(ctx, manager, dockerOwnerUnitName(manifest.Identity.RunId), self.settings)
		if err != nil || !bytes.Equal(ownerBefore, ownerAfter) {
			return "", errors.New("original Docker-owner generation changed during observation")
		}
		if err := boundary.Recheck(ctx); err != nil {
			return "", err
		}
	}
	after, _, err := readExistingRunUnit(ctx, manager, unit, self.settings)
	if err != nil || !bytes.Equal(before, after) {
		return "", errors.New("original evaluator generation changed during observation")
	}
	content, err := json.Marshal(struct {
		Config, Plan string
		Identity     Identity
		Unit         []byte
	}{Config: self.config.configSha256, Plan: state.CleanupPlanSha256, Identity: manifest.Identity, Unit: before})
	if err != nil {
		return "", err
	}
	return digestDockerBytes(content), ctx.Err()
}

// This is the full original boundary's read-only native path, not fresh worker
// admission: consumed memory/disk headroom cannot grant new-start authority or
// deny custody. The original worker ids come only from the authenticated plan.
func (self *Agent) verifyExistingRuntimeBoundary(ctx context.Context, plan cleanupPlan) (*RuntimeResourceBoundary, error) {
	if plan.DockerOwner == nil || plan.DockerOwner.HostFiles == nil || plan.DockerOwner.WorkerUid == 0 || plan.DockerOwner.HostFiles.WorkerGid == 0 {
		return nil, errors.New("existing runtime omitted authenticated original worker ids")
	}
	operator, err := trustedOperatorUid(self.config.TrustedOperatorUser)
	if err != nil || operator != plan.DockerOwner.TrustedOperatorUid {
		return nil, errors.New("original runtime operator identity changed")
	}
	boundary, err := verifyRuntimeResourceBoundaryForPurpose(ctx, self.config.DockerRole.ResourceBoundary,
		RuntimeResourceScope{BaseDirectory: self.config.BaseDirectory, StateDirectory: self.config.StateDirectory, WorkDirectory: self.config.WorkDirectory,
			ScratchDirectory: self.config.DockerRole.ScratchDirectory, TrustedOperatorUid: operator, WorkerUid: plan.DockerOwner.WorkerUid, WorkerGid: plan.DockerOwner.HostFiles.WorkerGid},
		DefaultRuntimeResourceBoundarySettings(), runtimeBoundaryNativeHost{}, managementExistingContainment)
	if err != nil {
		return nil, err
	}
	if boundary.WorkDeviceId != plan.WorkDeviceId || validateCleanupRuntimeBinding(self.config, boundary) != nil ||
		validateRuntimeBoundaryManagementCpuSet(boundary.ManagementCpuSet, plan.ManagementCpuSet) != nil {
		_ = boundary.Close()
		return nil, errors.New("existing actual runtime differs from its original authenticated plan and release")
	}
	return boundary, nil
}

// Missing unit evidence is returned only as an explicit not-found projection;
// callers still need authenticated terminal cleanup before accepting absence.
func readExistingRunUnit(parent context.Context, manager Pin, unit string, settings AgentSettings) ([]byte, map[string]string, error) {
	if !validUnitName(unit) || manager.Path != "/usr/bin/systemctl" {
		return nil, nil, errors.New("invalid original native unit authority")
	}
	file, err := openContainmentManagerExecutable(manager, DefaultRuntimeResourceBoundarySettings().ExecutableBytes, true)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(parent, settings.CleanupCommandTimeout)
	defer cancel()
	arguments := []string{"show", "--no-pager", "--all", "--property=" + existingRunUnitProperties, "--", unit}
	command := exec.CommandContext(ctx, "/proc/self/fd/3", arguments...)
	command.Args[0], command.ExtraFiles, command.Env = manager.Path, []*os.File{file}, controlEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = settings.CleanupCommandTimeout
	output := &boundedOutput{maximum: settings.DiagnosticBytes}
	command.Stdout, command.Stderr = output, &discardWriter{}
	runErr := command.Run()
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
		key, value, present := strings.Cut(line, "=")
		if _, repeated := values[key]; !present || repeated || !strings.Contains(","+existingRunUnitProperties+",", ","+key+",") {
			return nil, nil, errors.New("original unit native projection is malformed")
		}
		values[key] = value
	}
	if output.exceeded || ctx.Err() != nil || values["Id"] != unit || runErr != nil && values["LoadState"] != "not-found" {
		return nil, nil, errors.New("original unit native observation is unavailable")
	}
	return append([]byte(nil), output.Bytes()...), values, nil
}

// Exact original argv/config/binary and source-derived lifecycle/caps are all
// mandatory. The observer's own executable never appears in these comparisons.
func validateExistingRunUnit(values map[string]string, config Config, identity Identity, plan cleanupPlan, settings AgentSettings, owner bool) error {
	unit, execute, cleanup, bindsTo := unitName(identity.RunId), "--execute=", "--cleanup=", dockerOwnerUnitName(identity.RunId)
	if owner {
		unit, execute, cleanup, bindsTo = dockerOwnerUnitName(identity.RunId), "--docker-owner=", "--freeze=", ""
	}
	if len(values) != len(strings.Split(existingRunUnitProperties, ",")) {
		return errors.New("original native service projection is incomplete")
	}
	for key, want := range map[string]string{"Id": unit, "LoadState": "loaded", "ActiveState": "active", "SubState": "running", "FragmentPath": "/run/systemd/transient/" + unit,
		"DropInPaths": "", "Slice": managementPrivateSliceName, "ControlGroup": managementPrivateCgroupPath + "/" + unit, "User": "", "Group": "", "Type": "exec", "Restart": "no", "KillMode": "control-group",
		"BindsTo": bindsTo, "OnFailure": managementRecoveryServiceName, "OnFailureJobMode": "replace", "LimitCORE": "0", "UMask": "0077", "Job": ""} {
		if values[key] != want {
			return errors.New("original native service identity or lifecycle changed")
		}
	}
	pid, err := strconv.Atoi(values["MainPID"])
	started, startErr := strconv.ParseUint(values["ExecMainStartTimestampMonotonic"], 10, 64)
	invocation := values["InvocationID"]
	if err != nil || pid <= 1 || strconv.Itoa(pid) != values["MainPID"] || startErr != nil || started == 0 || strconv.FormatUint(started, 10) != values["ExecMainStartTimestampMonotonic"] ||
		len(invocation) != 32 || !digestPattern.MatchString(invocation+invocation) || invocation == strings.Repeat("0", 32) {
		return errors.New("original service has no definite native invocation")
	}
	if validateManagementSshExecForRole(values["ExecStart"], config, pid, execute+identity.RunId) != nil || validateManagementSshExecForRole(values["ExecStopPost"], config, -1, cleanup+identity.RunId) != nil {
		return errors.New("original unit does not execute its frozen agent/config/cleanup")
	}
	for key, want := range map[string]int64{"MemoryMax": settings.ManagementMemoryBytes, "MemorySwapMax": 0} {
		actual, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || strconv.FormatInt(actual, 10) != values[key] || actual != want {
			return errors.New("original service memory cap changed")
		}
	}
	for key, want := range map[string]int64{"CPUQuotaPerSecUSec": int64(settings.ManagementCpuQuotaPercent) * 10000, "CPUQuotaPeriodUSec": settings.ContainmentCpuPeriodMicros, "TimeoutStopUSec": (settings.CancelGrace + settings.CleanupTimeout).Microseconds()} {
		actual, err := containmentManagerMicroseconds(values[key])
		if err != nil || actual != want {
			return errors.New("original service CPU/cleanup cap changed")
		}
	}
	runtime, err := containmentManagerMicroseconds(values["RuntimeMaxUSec"])
	maximum := identity.DeadlineAt.Sub(identity.StartedAt)
	if owner {
		maximum += DefaultDockerProxySettings().DrainAllowance
	}
	if err != nil || runtime <= 0 || runtime > maximum.Microseconds() {
		return errors.New("original native service widened its execution clock")
	}
	for _, key := range []string{"AllowedCPUs", "CPUAffinity"} {
		if validateRuntimeBoundaryManagementCpuSet(strings.ReplaceAll(values[key], " ", ","), plan.ManagementCpuSet) != nil {
			return errors.New("original service CPU placement changed")
		}
	}
	return nil
}

// Held procfs/cgroup descriptors prove the exact native process still executes
// its original binary with actual memory/swap/CPU caps, not only PID1 metadata.
func verifyExistingRunProcess(ctx context.Context, values map[string]string, config Config, plan cleanupPlan, settings AgentSettings) error {
	procRoot, err := openDirectoryNoFollow("/proc")
	if err != nil {
		return err
	}
	defer procRoot.Close()
	proc, err := openRelativeNoFollow(procRoot, values["MainPID"], os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer proc.Close()
	groupRoot, err := openDirectoryNoFollow("/sys/fs/cgroup")
	if err != nil {
		return err
	}
	defer groupRoot.Close()
	group, err := openRelativeNoFollow(groupRoot, strings.TrimPrefix(values["ControlGroup"], "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer group.Close()
	groupInfo, err := group.Stat()
	if err != nil || !rootOwnedContainmentNode(groupInfo) {
		return errors.New("original service hard caps are delegated outside root custody")
	}
	declared, err := openContainmentManagerExecutable(config.Agent, DefaultRuntimeResourceBoundarySettings().ExecutableBytes, true)
	if err != nil {
		return err
	}
	defer declared.Close()
	// exe is a fixed kernel magic link beneath the held process, not a peer path.
	fd, err := syscall.Openat(int(proc.Fd()), "exe", os.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	executable := os.NewFile(uintptr(fd), "original-existing-executable")
	defer executable.Close()
	declaredInfo, declaredErr := declared.Stat()
	actualInfo, actualErr := executable.Stat()
	if declaredErr != nil || actualErr != nil || !os.SameFile(declaredInfo, actualInfo) {
		return errors.New("running service does not execute its original pinned agent generation")
	}
	digest := sha256.New()
	maximum := DefaultRuntimeResourceBoundarySettings().ExecutableBytes
	count, err := io.Copy(digest, io.LimitReader(executable, maximum+1))
	if err != nil || count > maximum || hex.EncodeToString(digest.Sum(nil)) != config.Agent.Sha256 {
		return errors.New("running original executable digest changed")
	}
	var birth uint64
	for range 2 {
		content, err := readManagementSshFile(proc, "stat", settings.ConfigBytes, 0x9fa0)
		if err != nil {
			return err
		}
		actualBirth, err := runtimeBoundaryProcessStart(content)
		if err != nil || actualBirth == 0 || birth != 0 && birth != actualBirth {
			return errors.New("original native process generation changed")
		}
		birth = actualBirth
		membership, err := readManagementSshFile(proc, "cgroup", settings.ConfigBytes, 0x9fa0)
		if err != nil || string(membership) != "0::"+values["ControlGroup"]+"\n" {
			return errors.New("original process escaped its native management service")
		}
		for key, want := range map[string]string{"memory.max": strconv.FormatInt(settings.ManagementMemoryBytes, 10), "memory.swap.max": "0", "memory.oom.group": "1", "cpu.max": strconv.FormatInt(int64(settings.ManagementCpuQuotaPercent)*settings.ContainmentCpuPeriodMicros/100, 10) + " " + strconv.FormatInt(settings.ContainmentCpuPeriodMicros, 10)} {
			actual, err := readManagementKernelProperty(ctx, group, key, settings.DiagnosticBytes, true)
			if err != nil || actual != want {
				return errors.New("original service lost an actual native hard cap")
			}
		}
		for _, key := range []string{"cpuset.cpus", "cpuset.cpus.effective"} {
			actual, err := readManagementKernelProperty(ctx, group, key, settings.DiagnosticBytes, true)
			if err != nil || validateRuntimeBoundaryManagementCpuSet(actual, plan.ManagementCpuSet) != nil {
				return errors.New("original service lost actual native CPU containment")
			}
		}
	}
	return ctx.Err()
}

// Build retirement is the one authenticated tightening already allowed by the
// original plan. Every other original parent retains its exact hard ceilings.
func (self *Agent) verifyExistingParentLimits(ctx context.Context, plan cleanupPlan) error {
	limits, err := prepareRunSliceLimits(plan, self.config)
	if err != nil || plan.DockerOwner == nil {
		return errors.New("existing run omitted its original complete hard-cap plan")
	}
	slices := newHostRunSlices(self.config, self.settings)
	proxy := &DockerProxy{plan: *plan.DockerOwner, settings: DefaultDockerProxySettings(), buildRetirer: newHostBuildRetirement(self.config.DockerRole.ResourceBoundary, DefaultBuildRetirementSettings())}
	if err := proxy.recoverBuild(); err != nil {
		return err
	}
	if err := proxy.recoverBuildRetirement(); err != nil {
		return err
	}
	for index, name := range plan.CgroupNames {
		if name == plan.DockerOwner.BuildCgroupName && proxy.retirement != nil {
			if proxy.retirement.err != nil || proxy.retirement.receipt == nil || plan.DockerOwner.BuildRetirement == nil {
				return errors.New("existing build retirement is not authenticated")
			}
			if _, err := proxy.buildRetirer.Verify(ctx, *plan.DockerOwner.BuildRetirement); err != nil {
				return err
			}
			continue
		}
		if err := slices.cgroups.VerifyLimits(ctx, name, limits[index]); err != nil {
			return err
		}
	}
	return ctx.Err()
}
