//go:build linux

// Native management uses only fixed pinned tools, root metadata, procfs and
// cgroup v2. Candidate engine admission is deliberately not a dependency of
// signed status/cancel/reconciliation or the forced transport wrapper.
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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const managementSshProperties = "Id,LoadState,ActiveState,SubState,Transient,FragmentPath,DropInPaths,Slice,ControlGroup,User,Group,Type,Restart,RemainAfterExit,KillMode,ExecStart,MainPID,InvocationID,ExecMainStartTimestampMonotonic,OnFailure,BindsTo,PartOf,MemoryMax,MemoryHigh,MemorySwapMax,TasksMax,CPUQuotaPerSecUSec,CPUQuotaPeriodUSec,AllowedCPUs,CPUAffinity,RuntimeMaxUSec,TimeoutStopUSec,NoNewPrivileges,CapabilityBoundingSet,AmbientCapabilities"

// This preserves the existing trusted-root Linux 6.8 control class, not a
// claimed minimum. A narrower actual host refuses; native qualification must
// not widen it automatically. Candidate containers keep their separate class.
const managementSshCapabilityLast = 40
const managementSshCapabilityMask = uint64(0x1ffffffffff)
const managementSshCapabilities = "CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER CAP_FSETID CAP_KILL CAP_SETGID CAP_SETUID CAP_SETPCAP CAP_LINUX_IMMUTABLE CAP_NET_BIND_SERVICE CAP_NET_BROADCAST CAP_NET_ADMIN CAP_NET_RAW CAP_IPC_LOCK CAP_IPC_OWNER CAP_SYS_MODULE CAP_SYS_RAWIO CAP_SYS_CHROOT CAP_SYS_PTRACE CAP_SYS_PACCT CAP_SYS_ADMIN CAP_SYS_BOOT CAP_SYS_NICE CAP_SYS_RESOURCE CAP_SYS_TIME CAP_SYS_TTY_CONFIG CAP_MKNOD CAP_LEASE CAP_AUDIT_WRITE CAP_AUDIT_CONTROL CAP_SETFCAP CAP_MAC_OVERRIDE CAP_MAC_ADMIN CAP_SYSLOG CAP_WAKE_ALARM CAP_BLOCK_SUSPEND CAP_AUDIT_READ CAP_PERFMON CAP_BPF CAP_CHECKPOINT_RESTORE"

// Missing differs from inactive. An exited RemainAfterExit service still
// requires an explicit stop while the pinned --wait client remains owned.
type managementSshUnit struct {
	exists       bool
	completed    bool
	inactive     bool
	invocationId string
	pid          int
	projection   []byte
}

// Production constructors never install a filesystem/process/proof waiver.
func newManagementSshHost(settings ManagementSshSettings) *managementSshHost {
	self := &managementSshHost{settings: settings, protected: true, newId: newManagementSshId, joinContext: context.WithTimeout}
	self.load = func(path string) (*Config, error) { return loadManagementSshConfig(path, settings) }
	self.admit = func(ctx context.Context, config *Config) (*ManagementResourcePolicy, Pin, error) {
		return admitManagementSsh(ctx, config, settings)
	}
	self.start = func(ctx context.Context, config *Config, plan managementSshPlan, marker string, files []*os.File) (managementSshProcess, error) {
		return startManagementSsh(ctx, config, plan, marker, files, settings)
	}
	self.observe = func(ctx context.Context, config *Config, policy *ManagementResourcePolicy, manager Pin, plan managementSshPlan) (managementSshUnit, error) {
		return observeManagementSsh(ctx, config, policy, manager, plan, settings)
	}
	self.stop = func(ctx context.Context, manager Pin, unit string) error {
		_, err := managementSshManager(ctx, manager, "stop", unit, settings)
		return err
	}
	self.empty = func(ctx context.Context, unit string) (bool, error) {
		return managementSshKernelEmpty(ctx, unit, settings)
	}
	return self
}

// The recovery loader reads no signing key, worker account, fixture content,
// live daemon, core policy or candidate/native engine readiness state.
func loadManagementSshConfig(path string, settings ManagementSshSettings) (*Config, error) {
	if settings.validate() != nil || !managementSshAuthorityPath(path) {
		return nil, errors.New("managed SSH requires fixed bounded protected configuration")
	}
	config, err := LoadRecoveryConfig(path)
	if err != nil {
		return nil, err
	}
	if !managementSshAuthorityPath(config.Agent.Path) || !managementSshAuthorityPath(config.BaseDirectory) || !managementSshAuthorityPath(config.StateDirectory) {
		return nil, errors.New("managed SSH paths cannot expand shell, sudoers or manager authority")
	}
	if config.SystemdRun.Path != "/usr/bin/systemd-run" || validatePin(config.SystemdRun, true) != nil {
		return nil, errors.New("managed SSH has no exact protected systemd-run executable pin")
	}
	if err := managementSshSelfExecutable(config.Agent); err != nil {
		return nil, err
	}
	if err := verifyManagementSshAmbientCapabilities(settings); err != nil {
		return nil, err
	}
	return config, nil
}

// Both the current executable descriptor and declared path must be the same
// pinned generation. Public OS loader/package custody remains trusted root.
func managementSshSelfExecutable(pin Pin) error {
	maximum := DefaultRuntimeResourceBoundarySettings().ExecutableBytes
	declared, err := openContainmentManagerExecutable(pin, maximum, true)
	if err != nil {
		return err
	}
	defer declared.Close()
	current, err := os.Open("/proc/self/exe") // Fixed kernel magic link, never a caller-selected pathname.
	if err != nil {
		return err
	}
	defer current.Close()
	before, beforeErr := declared.Stat()
	after, afterErr := current.Stat()
	if beforeErr != nil || afterErr != nil || !os.SameFile(before, after) {
		return errors.New("managed SSH is not executing its exact configured agent generation")
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(current, maximum+1))
	if err != nil || count > maximum || hex.EncodeToString(digest.Sum(nil)) != pin.Sha256 {
		return errors.New("managed SSH current executable differs from its protected pin")
	}
	return nil
}

// All ordinary aggregate caps/descendant CPUs and independent recovery are
// freshly proved. No Docker, mount, build, private network or scoring mutation
// is performed by these narrow pre-framing checks.
func admitManagementSsh(parent context.Context, config *Config, settings ManagementSshSettings) (*ManagementResourcePolicy, Pin, error) {
	if config == nil || settings.validate() != nil {
		return nil, Pin{}, errors.New("managed SSH omitted finite management authority")
	}
	ctx, cancel := context.WithTimeout(parent, settings.Agent.ControlTimeout)
	defer cancel()
	policy, err := loadManagementResourcePolicy(config.ManagementPolicy, DefaultManagementBoundarySettings(), settings.Agent.ConfigBytes, true)
	if err != nil {
		return nil, Pin{}, err
	}
	manager, err := recoverySystemctlPin(*config, settings.Agent, true)
	if err != nil {
		return nil, Pin{}, err
	}
	if err := verifyManagementOrdinaryBoundary(ctx, policy, manager, settings.Agent); err != nil {
		return nil, Pin{}, err
	}
	if err := verifyRecoveryService(ctx, *config, settings.Agent, true, false, verifyManagementRecoveryBoundary, runRecoveryManagerCommand); err != nil {
		return nil, Pin{}, err
	}
	return policy, manager, ctx.Err()
}

// RemainAfterExit prevents successful services disappearing before the owner
// can obtain its exact generation. There is no OnFailure recovery hook here:
// loss of a control connection must not reap a healthy durable evaluation.
func managementSshArguments(config Config, plan managementSshPlan, marker string, settings ManagementSshSettings) ([]string, error) {
	if settings.validate() != nil || !digestPattern.MatchString(plan.Id) || plan.UnitName != managementSshUnitName(plan.Id) ||
		!managementSshAuthorityPath(config.Agent.Path) || !managementSshAuthorityPath(config.configPath) || !managementSshAuthorityPath(config.BaseDirectory) ||
		marker != filepath.Join(config.StateDirectory, "managed-controls", plan.Id+".admit") || !managementSshAuthorityPath(marker) ||
		plan.Agent != config.Agent || plan.ConfigSha256 != config.configSha256 || plan.MemoryBytes != settings.MemoryBytes || plan.TasksMaximum != settings.TasksMaximum ||
		plan.RuntimeMaximum != settings.MaximumRuntime || !dockerProxyCpuSetPattern.MatchString(plan.ManagementCpuSet) {
		return nil, errors.New("managed SSH launch differs from its fixed root receipt")
	}
	cpus := strings.ReplaceAll(plan.ManagementCpuSet, ",", " ")
	args := []string{"--quiet", "--no-ask-password", "--pipe", "--wait", "--remain-after-exit", "--service-type=exec", "--unit=" + plan.UnitName,
		"--property=Slice=" + managementPrivateSliceName, "--property=User=root", "--property=Group=root",
		"--property=Restart=no", "--property=KillMode=control-group", "--property=SendSIGKILL=yes", "--property=OOMPolicy=kill", "--property=NoNewPrivileges=yes",
		"--property=CapabilityBoundingSet=" + managementSshCapabilities, "--property=AmbientCapabilities=",
		"--property=LimitCORE=0", "--property=UMask=0077", "--property=WorkingDirectory=/", "--property=MemoryMax=" + strconv.FormatInt(plan.MemoryBytes, 10),
		"--property=MemoryHigh=infinity", "--property=MemorySwapMax=0", "--property=TasksMax=" + strconv.FormatInt(plan.TasksMaximum, 10),
		"--property=AllowedCPUs=" + cpus, "--property=CPUAffinity=" + cpus, "--property=CPUQuota=" + strconv.Itoa(settings.Agent.ManagementCpuQuotaPercent) + "%",
		"--property=CPUQuotaPeriodSec=" + strconv.FormatInt(settings.Agent.ContainmentCpuPeriodMicros, 10) + "us",
		"--property=RuntimeMaxSec=" + managementSshDuration(plan.RuntimeMaximum), "--property=TimeoutStopSec=" + managementSshDuration(settings.Agent.CancelGrace),
		"--property=ConditionPathExists=" + marker, "--property=RequiresMountsFor=" + config.BaseDirectory}
	for _, environment := range controlEnvironment() {
		args = append(args, "--setenv="+environment)
	}
	return append(args, "--", config.Agent.Path, "--config", config.configPath, "--serve"), nil
}

// Rendered forced-command/sudoers authority needs literal paths, not merely
// whitespace-free paths which could still contain shell or sudoers patterns.
func managementSshAuthorityPath(value string) bool {
	if !recoveryAuthorityPath(value) {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("/._-", character) {
			continue
		}
		return false
	}
	return true
}

// The descriptor-held real launcher passes physical stdio directly to PID1;
// exec.Cmd therefore creates no outside-unit stdin/stdout copy goroutines.
func startManagementSsh(ctx context.Context, config *Config, plan managementSshPlan, marker string, files []*os.File, settings ManagementSshSettings) (managementSshProcess, error) {
	if config == nil || config.SystemdRun.Path != "/usr/bin/systemd-run" || len(files) != 3 {
		return nil, errors.New("managed SSH has no exact launcher or physical streams")
	}
	arguments, err := managementSshArguments(*config, plan, marker, settings)
	if err != nil {
		return nil, err
	}
	file, err := openContainmentManagerExecutable(config.SystemdRun, DefaultRuntimeResourceBoundarySettings().ExecutableBytes, true)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	command := exec.CommandContext(ctx, "/proc/self/fd/3", arguments...)
	command.Args[0], command.Env = config.SystemdRun.Path, controlEnvironment()
	command.ExtraFiles = []*os.File{file}
	command.Stdin, command.Stdout, command.Stderr = files[0], files[1], files[2]
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error { return command.Process.Kill() }
	command.WaitDelay = settings.Agent.CancelGrace
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &managementSshNativeProcess{command: command}, nil
}

// The fixed launcher is the only monitor process. Its held process identity
// cannot signal a reused numeric process group or a PID1-parented evaluator.
type managementSshNativeProcess struct{ command *exec.Cmd }

// There are no Go stream-copy workers; Wait physically joins the one launcher.
func (self *managementSshNativeProcess) Wait() error { return self.command.Wait() }

// Go's held process identity refuses an exited/reused process, including the
// ordering where Wait returned but its completion has not reached the owner.
func (self *managementSshNativeProcess) Kill() error {
	return self.command.Process.Kill()
}

// There are exactly two verbs and one fixed property list. No request can
// choose a program, manager endpoint, property, evaluator or durable unit.
func managementSshManager(parent context.Context, pin Pin, operation, unit string, settings ManagementSshSettings) ([]byte, error) {
	if pin.Path != "/usr/bin/systemctl" || !validManagementSshUnit(unit) || settings.validate() != nil {
		return nil, errors.New("managed SSH manager has no closed unit/tool authority")
	}
	var arguments []string
	switch operation {
	case "show":
		arguments = []string{"show", "--no-pager", "--all", "--property=" + managementSshProperties, "--", unit}
	case "stop":
		arguments = []string{"stop", "--", unit}
	default:
		return nil, errors.New("managed SSH manager refused another operation")
	}
	file, err := openContainmentManagerExecutable(pin, DefaultRuntimeResourceBoundarySettings().ExecutableBytes, true)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(parent, settings.Agent.ControlTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "/proc/self/fd/3", arguments...)
	command.Args[0], command.Env = pin.Path, controlEnvironment()
	command.ExtraFiles = []*os.File{file}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error { return command.Process.Kill() }
	command.WaitDelay = settings.Agent.CancelGrace
	output := &boundedOutput{maximum: settings.Agent.DiagnosticBytes}
	command.Stdout, command.Stderr = output, &discardWriter{}
	err = command.Run()
	if ctx.Err() != nil || output.exceeded {
		return nil, errors.New("managed SSH manager did not return a bounded acknowledgement")
	}
	if operation == "stop" && err != nil {
		return nil, errors.New("managed SSH control stop was not acknowledged")
	}
	if operation == "show" && err != nil && !strings.Contains(output.String(), "LoadState=not-found\n") {
		return nil, errors.New("managed SSH control properties are unavailable")
	}
	return append([]byte(nil), output.Bytes()...), nil
}

// Unit spelling is derived from a local random nonce, never caller text.
func validManagementSshUnit(unit string) bool {
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "urnetwork-subeval-control-"), ".service")
	return digestPattern.MatchString(id) && unit == managementSshUnitName(id)
}

// Manager readback is fully typed/closed before its generation is trusted.
func observeManagementSsh(ctx context.Context, config *Config, policy *ManagementResourcePolicy, manager Pin, plan managementSshPlan, settings ManagementSshSettings) (managementSshUnit, error) {
	content, err := managementSshManager(ctx, manager, "show", plan.UnitName, settings)
	if err != nil {
		return managementSshUnit{}, err
	}
	return validateManagementSshUnit(content, *config, policy, plan, settings)
}

// Even terminal properties must identify the same closed service. Missing is
// represented explicitly and can become a join only after a known stop ack.
func validateManagementSshUnit(content []byte, config Config, policy *ManagementResourcePolicy, plan managementSshPlan, settings ManagementSshSettings) (managementSshUnit, error) {
	unit := managementSshUnit{}
	if int64(len(content)) > settings.Agent.DiagnosticBytes || policy == nil {
		return unit, errors.New("managed SSH unit properties exceed their authority")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if _, found := values[key]; !ok || found {
			return unit, errors.New("managed SSH unit properties are ambiguous")
		}
		values[key] = value
	}
	for _, key := range strings.Split(managementSshProperties, ",") {
		if _, found := values[key]; !found {
			return unit, errors.New("managed SSH unit properties are incomplete")
		}
	}
	if len(values) != len(strings.Split(managementSshProperties, ",")) || values["Id"] != plan.UnitName {
		return unit, errors.New("managed SSH unit properties selected another owner")
	}
	if values["LoadState"] == "not-found" {
		if values["ActiveState"] != "inactive" || values["MainPID"] != "0" || values["ControlGroup"] != "" || values["FragmentPath"] != "" || values["DropInPaths"] != "" {
			return unit, errors.New("missing managed SSH unit still has uncertain process custody")
		}
		return unit, nil
	}
	if values["LoadState"] != "loaded" || values["Transient"] != "yes" || values["FragmentPath"] != "/run/systemd/transient/"+plan.UnitName || values["DropInPaths"] != "" ||
		values["Slice"] != managementPrivateSliceName || values["User"] != "root" || values["Group"] != "root" || values["Type"] != "exec" ||
		values["Restart"] != "no" || values["RemainAfterExit"] != "yes" || values["KillMode"] != "control-group" || values["MemoryHigh"] != "infinity" || values["NoNewPrivileges"] != "yes" {
		return unit, errors.New("managed SSH unit changed its fixed placement or lifecycle")
	}
	for _, key := range []string{"OnFailure", "BindsTo", "PartOf"} {
		if values[key] != "" {
			return unit, errors.New("ephemeral control acquired a durable evaluation failure/lifetime hook")
		}
	}
	if values["AmbientCapabilities"] != "" || validateManagementSshCapabilityNames(values["CapabilityBoundingSet"]) != nil {
		return unit, errors.New("managed SSH unit changed its explicit trusted-root capability profile")
	}
	for key, want := range map[string]int64{"MemoryMax": plan.MemoryBytes, "MemorySwapMax": 0, "TasksMax": plan.TasksMaximum} {
		actual, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || strconv.FormatInt(actual, 10) != values[key] || actual != want {
			return unit, errors.New("managed SSH unit changed its explicit child resource cap")
		}
	}
	for key, want := range map[string]int64{"CPUQuotaPerSecUSec": int64(settings.Agent.ManagementCpuQuotaPercent) * 10000, "CPUQuotaPeriodUSec": settings.Agent.ContainmentCpuPeriodMicros,
		"RuntimeMaxUSec": plan.RuntimeMaximum.Microseconds(), "TimeoutStopUSec": settings.Agent.CancelGrace.Microseconds()} {
		actual, err := containmentManagerMicroseconds(values[key])
		if err != nil || actual != want {
			return unit, errors.New("managed SSH unit changed its CPU or full-lifetime budget")
		}
	}
	for _, key := range []string{"AllowedCPUs", "CPUAffinity"} {
		if validateRuntimeBoundaryManagementCpuSet(strings.ReplaceAll(values[key], " ", ","), policy.ManagementCpuSet) != nil {
			return unit, errors.New("managed SSH unit escaped management processors")
		}
	}
	pid, err := strconv.Atoi(values["MainPID"])
	started, startErr := strconv.ParseUint(values["ExecMainStartTimestampMonotonic"], 10, 64)
	invocation := values["InvocationID"]
	if err != nil || strconv.Itoa(pid) != values["MainPID"] || pid < 0 || startErr != nil || started == 0 || strconv.FormatUint(started, 10) != values["ExecMainStartTimestampMonotonic"] ||
		len(invocation) != 32 || !digestPattern.MatchString(invocation+invocation) || invocation == strings.Repeat("0", 32) {
		return unit, errors.New("managed SSH unit omitted its definite invocation generation")
	}
	group := managementPrivateCgroupPath + "/" + plan.UnitName
	unit.exists, unit.invocationId, unit.pid, unit.projection = true, invocation, pid, append([]byte(nil), content...)
	switch values["ActiveState"] {
	case "inactive", "failed":
		if pid != 0 || values["ControlGroup"] != "" && values["ControlGroup"] != group {
			return managementSshUnit{}, errors.New("inactive managed SSH unit retains another process or group")
		}
		unit.inactive, unit.completed = true, true
	case "active":
		if values["ControlGroup"] != group && !(values["SubState"] == "exited" && values["ControlGroup"] == "") ||
			values["SubState"] != "running" && values["SubState"] != "exited" || values["SubState"] == "running" && pid <= 1 || values["SubState"] == "exited" && pid != 0 {
			return managementSshUnit{}, errors.New("managed SSH unit has inconsistent running/exited ownership")
		}
		unit.completed = values["SubState"] == "exited"
	default:
		return managementSshUnit{}, errors.New("managed SSH unit is not a definite active or terminal invocation")
	}
	if err := validateManagementSshExec(values["ExecStart"], config, pid); err != nil {
		return managementSshUnit{}, err
	}
	return unit, nil
}

// Authority paths contain no quoting/whitespace, so systemctl's strv rendering
// has one unambiguous argv. No prefix/suffix command or extra role is accepted.
func validateManagementSshExec(value string, config Config, currentPid int) error {
	if !strings.HasPrefix(value, "{ ") || !strings.HasSuffix(value, " }") {
		return errors.New("managed SSH has no exact executable metadata")
	}
	fields := map[string]string{}
	for _, field := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(value, "{ "), " }"), " ; ") {
		key, value, ok := strings.Cut(field, "=")
		if _, found := fields[key]; !ok || found {
			return errors.New("managed SSH executable metadata is ambiguous")
		}
		fields[key] = value
	}
	if len(fields) != 8 || fields["path"] != config.Agent.Path || fields["argv[]"] != strings.Join([]string{config.Agent.Path, "--config", config.configPath, "--serve"}, " ") || fields["ignore_errors"] != "no" {
		return errors.New("managed SSH executable differs from the closed child role")
	}
	pid, err := strconv.Atoi(fields["pid"])
	if err != nil || pid <= 1 || strconv.Itoa(pid) != fields["pid"] || currentPid > 0 && pid != currentPid {
		return errors.New("managed SSH executable has another process identity")
	}
	for _, name := range []string{"start_time", "stop_time"} {
		if !strings.HasPrefix(fields[name], "[") || !strings.HasSuffix(fields[name], "]") {
			return errors.New("managed SSH executable timestamps are malformed")
		}
	}
	if fields["code"] != "(null)" && fields["code"] != "exited" && fields["code"] != "killed" && fields["code"] != "dumped" || fields["status"] == "" || strings.ContainsAny(fields["status"], " ;{}\t\r\n") {
		return errors.New("managed SSH executable exit metadata is malformed")
	}
	return nil
}

// Empty is checked only after a known-generation stop acknowledgement. Missing
// from the outset never becomes a successful join in the wrapper state machine.
func managementSshKernelEmpty(ctx context.Context, unit string, settings ManagementSshSettings) (bool, error) {
	if !validManagementSshUnit(unit) {
		return false, errors.New("invalid managed SSH kernel scope")
	}
	root, err := openDirectoryNoFollow("/sys/fs/cgroup")
	if err != nil {
		return false, err
	}
	defer root.Close()
	group, err := openRelativeNoFollow(root, strings.TrimPrefix(managementPrivateCgroupPath+"/"+unit, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer group.Close()
	for _, property := range []string{"pids.current", "cgroup.procs"} {
		value, err := readManagementKernelProperty(ctx, group, property, settings.Agent.ConfigBytes, true)
		if err != nil {
			return false, err
		}
		if property == "pids.current" && value != "0" || property == "cgroup.procs" && value != "" {
			return false, nil
		}
	}
	events, err := readManagementSshFile(group, "cgroup.events", settings.Agent.ConfigBytes, 0x63677270)
	if err != nil {
		return false, err
	}
	seen := false
	for _, line := range strings.Split(strings.TrimSuffix(string(events), "\n"), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return false, errors.New("managed SSH cgroup events are malformed")
		}
		if parts[0] == "populated" {
			if seen || parts[1] != "0" {
				return false, nil
			}
			seen = true
		}
	}
	return seen, nil
}

// Bounded native reads use held raw no-follow descriptors, including relative
// leaf aliases. Tests may exercise the validator with private descriptor facts.
func readManagementSshFile(parent *os.File, name string, maximum, magic int64) ([]byte, error) {
	file, err := openRelativeNoFollow(parent, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	var filesystem syscall.Statfs_t
	if err != nil || !info.Mode().IsRegular() || maximum <= 0 || syscall.Fstatfs(int(file.Fd()), &filesystem) != nil || magic != 0 && filesystem.Type != magic {
		return nil, errors.New("managed SSH native evidence changed its filesystem or type")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum {
		return nil, errors.New("managed SSH native evidence exceeded its bound")
	}
	return content, nil
}

// The child derives its nonce only from its own native cgroup membership. The
// marker is rechecked after PID1/kernel proof, closing condition-check→exec
// races before any frame bytes are read. An already committed signed start
// remains with its independent durable services when this control is stopped.
func verifyManagementSshCurrent(parent context.Context, config *Config, policy *ManagementResourcePolicy, manager Pin, settings ManagementSshSettings) error {
	ctx, cancel := context.WithTimeout(parent, settings.Agent.ControlTimeout)
	defer cancel()
	procRoot, err := openDirectoryNoFollow("/proc")
	if err != nil {
		return err
	}
	defer procRoot.Close()
	proc, err := openRelativeNoFollow(procRoot, strconv.Itoa(os.Getpid()), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer proc.Close()
	membership, err := readManagementSshFile(proc, "cgroup", settings.Agent.ConfigBytes, 0x9fa0)
	if err != nil {
		return err
	}
	group := strings.TrimSuffix(string(membership), "\n")
	prefix := "0::" + managementPrivateCgroupPath + "/"
	unit := strings.TrimPrefix(group, prefix)
	if !strings.HasPrefix(group, prefix) || !validManagementSshUnit(unit) {
		return errors.New("direct serve is not the exact managed private control service")
	}
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "urnetwork-subeval-control-"), ".service")
	registry, err := openManagementSshRegistry(*config, settings, true)
	if err != nil {
		return err
	}
	defer registry.Close()
	plan, content, marker, err := readManagementSshAdmission(registry, *config, *policy, unit, settings)
	if err != nil {
		return err
	}
	before, err := observeManagementSsh(ctx, config, policy, manager, plan, settings)
	if err != nil || !before.exists || before.completed || before.pid != os.Getpid() {
		return errors.New("managed control is not PID1's exact current main process")
	}
	cgroupRoot, err := openDirectoryNoFollow("/sys/fs/cgroup")
	if err != nil {
		return err
	}
	defer cgroupRoot.Close()
	cgroup, err := openRelativeNoFollow(cgroupRoot, strings.TrimPrefix(managementPrivateCgroupPath+"/"+unit, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer cgroup.Close()
	if err := verifyManagementSshDescriptors(ctx, proc, cgroup, plan, settings); err != nil {
		return err
	}
	after, err := observeManagementSsh(ctx, config, policy, manager, plan, settings)
	if err != nil || !bytes.Equal(before.projection, after.projection) {
		return errors.New("managed control generation changed around held kernel proof")
	}
	again, err := registry.read(id + ".admit")
	if err != nil || !bytes.Equal(marker, again) {
		return errors.New("managed control admission closed before framing")
	}
	admitted, err := json.Marshal(struct {
		Schema       int    `json:"schema"`
		Id           string `json:"id"`
		PlanSha256   string `json:"plan_sha256"`
		InvocationId string `json:"invocation_id"`
		Pid          int    `json:"pid"`
		ProofSha256  string `json:"proof_sha256"`
	}{Schema: Schema, Id: id, PlanSha256: digestDockerBytes(content), InvocationId: after.invocationId, Pid: os.Getpid(), ProofSha256: digestDockerBytes(after.projection)})
	if err != nil {
		return err
	}
	return registry.write(id+".admitted", admitted)
}

// Static admission is independently testable without connecting to a native
// manager. It is a necessary precondition, never a substitute for current proof.
func readManagementSshAdmission(registry *managementSshRegistry, config Config, policy ManagementResourcePolicy, unit string, settings ManagementSshSettings) (managementSshPlan, []byte, []byte, error) {
	plan := managementSshPlan{}
	if registry == nil || !validManagementSshUnit(unit) {
		return plan, nil, nil, errors.New("invalid managed control admission identity")
	}
	id := strings.TrimSuffix(strings.TrimPrefix(unit, "urnetwork-subeval-control-"), ".service")
	content, err := registry.read(id + ".json")
	if err != nil {
		return plan, nil, nil, err
	}
	if validateDocumentKeys(content) != nil || decodeStrict(content, &plan) != nil || plan.Id != id || plan.validate(config, policy, settings) != nil ||
		!settings.Agent.Now().Before(plan.ExpiresAt) || settings.Agent.Now().Before(plan.CreatedAt.Add(-settings.Agent.MaximumClockSkew)) {
		return plan, nil, nil, errors.New("managed control receipt is absent, changed or expired")
	}
	marker, err := registry.read(id + ".admit")
	if err != nil || string(marker) != digestDockerBytes(content)+"\n" {
		return plan, nil, nil, errors.New("managed control admission has been withdrawn")
	}
	return plan, content, marker, nil
}

// Both snapshots are from held proc/cgroup generations. All actual resource
// controls and effective affinity must agree with the manager's typed values.
func verifyManagementSshDescriptors(ctx context.Context, proc, group *os.File, plan managementSshPlan, settings ManagementSshSettings) error {
	return verifyManagementSshDescriptorProfile(ctx, proc, group, plan, settings, 0x9fa0, 0x63677270, nil)
}

// Only private tests pass ordinary-file magic zero or a snapshot barrier.
// Native entrypoints above fix procfs/cgroup-v2 and never expose those choices.
func verifyManagementSshDescriptorProfile(ctx context.Context, proc, group *os.File, plan managementSshPlan, settings ManagementSshSettings, procMagic, groupMagic int64, afterSnapshot func()) error {
	var birth uint64
	for snapshot := range 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		stat, err := readManagementSshFile(proc, "stat", settings.Agent.ConfigBytes, procMagic)
		if err != nil {
			return err
		}
		current, err := runtimeBoundaryProcessStart(stat)
		if err != nil || current == 0 || birth != 0 && current != birth {
			return errors.New("managed control process generation changed")
		}
		birth = current
		membership, err := readManagementSshFile(proc, "cgroup", settings.Agent.ConfigBytes, procMagic)
		if err != nil || string(membership) != "0::"+managementPrivateCgroupPath+"/"+plan.UnitName+"\n" {
			return errors.New("managed control process escaped its exact private group")
		}
		status, err := readManagementSshFile(proc, "status", settings.Agent.ConfigBytes, procMagic)
		if err != nil {
			return err
		}
		if err := validateManagementSshCapabilityStatus(status, true); err != nil {
			return err
		}
		found := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Cpus_allowed_list:") {
				parts := strings.Fields(line)
				if found || len(parts) != 2 || validateRuntimeBoundaryManagementCpuSet(parts[1], plan.ManagementCpuSet) != nil {
					return errors.New("managed control thread affinity differs from management processors")
				}
				found = true
			}
		}
		if !found {
			return errors.New("managed control affinity is absent")
		}
		for key, want := range map[string]string{"memory.max": strconv.FormatInt(plan.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0",
			"pids.max": strconv.FormatInt(plan.TasksMaximum, 10), "cgroup.type": "domain", "memory.oom.group": "1",
			"cpu.max": strconv.FormatInt(int64(settings.Agent.ManagementCpuQuotaPercent)*settings.Agent.ContainmentCpuPeriodMicros/100, 10) + " " + strconv.FormatInt(settings.Agent.ContainmentCpuPeriodMicros, 10)} {
			content, err := readManagementSshFile(group, key, settings.Agent.ConfigBytes, groupMagic)
			if err != nil || string(content) != want+"\n" {
				return errors.New("managed control kernel resource limit differs from its explicit role")
			}
		}
		for _, key := range []string{"cpuset.cpus", "cpuset.cpus.effective"} {
			value, err := readManagementSshFile(group, key, settings.Agent.ConfigBytes, groupMagic)
			if err != nil || validateRuntimeBoundaryManagementCpuSet(strings.TrimSuffix(string(value), "\n"), plan.ManagementCpuSet) != nil {
				return errors.New("managed control kernel processors escaped management placement")
			}
		}
		if snapshot == 0 && afterSnapshot != nil {
			afterSnapshot()
		}
	}
	return nil
}

// Manager spelling is normalized only for the fixed known Linux capability
// names; an unknown, repeated, narrower or wider set is not another profile.
func validateManagementSshCapabilityNames(value string) error {
	expected := map[string]bool{}
	for _, name := range strings.Fields(managementSshCapabilities) {
		expected[name] = true
	}
	seen := map[string]bool{}
	for _, name := range strings.Fields(value) {
		name = strings.ToUpper(name)
		if !expected[name] || seen[name] {
			return errors.New("managed SSH capability name is unknown or duplicated")
		}
		seen[name] = true
	}
	if len(seen) != len(expected) {
		return errors.New("managed SSH trusted-root capability set is incomplete")
	}
	return nil
}

// Actual permitted/effective/bounding sets must match the declared current-root
// class. Empty ambient/inheritable state prevents another implicit grant.
func validateManagementSshCapabilityStatus(content []byte, requireNnp bool) error {
	expected := map[string]uint64{"CapEff:": managementSshCapabilityMask, "CapPrm:": managementSshCapabilityMask, "CapBnd:": managementSshCapabilityMask, "CapAmb:": 0, "CapInh:": 0}
	seen := map[string]bool{}
	nnp := false
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if want, found := expected[fields[0]]; found {
			if seen[fields[0]] || len(fields) != 2 || len(fields[1]) != 16 {
				return errors.New("managed SSH process capability state is ambiguous")
			}
			actual, err := strconv.ParseUint(fields[1], 16, 64)
			if err != nil || actual != want {
				return errors.New("managed SSH process does not preserve its existing trusted-root capability class")
			}
			seen[fields[0]] = true
		} else if fields[0] == "NoNewPrivs:" {
			if nnp || len(fields) != 2 || fields[1] != "1" && (requireNnp || fields[1] != "0") {
				return errors.New("managed SSH process no-new-privileges state differs")
			}
			nnp = true
		}
	}
	if len(seen) != len(expected) || !nnp {
		return errors.New("managed SSH process omitted its explicit privilege state")
	}
	return nil
}

// This fixed read refuses a narrower existing environment before any attempt
// to launch a broader service. No host capability or kernel setting is changed.
func verifyManagementSshAmbientCapabilities(settings ManagementSshSettings) error {
	root, err := openDirectoryNoFollow("/proc")
	if err != nil {
		return err
	}
	defer root.Close()
	last, err := readManagementSshFile(root, "sys/kernel/cap_last_cap", settings.Agent.ConfigBytes, 0x9fa0)
	if err != nil || string(last) != strconv.Itoa(managementSshCapabilityLast)+"\n" {
		return errors.New("managed SSH kernel capability class changed")
	}
	proc, err := openRelativeNoFollow(root, strconv.Itoa(os.Getpid()), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer proc.Close()
	status, err := readManagementSshFile(proc, "status", settings.Agent.ConfigBytes, 0x9fa0)
	if err != nil {
		return err
	}
	return validateManagementSshCapabilityStatus(status, false)
}
