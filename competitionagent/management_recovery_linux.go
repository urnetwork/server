// A pinned independent PID1 service is armed before any ordinary launch.
// Static/native management proof never depends on a live private engine.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Only fixed metadata/resource properties are read; no arbitrary unit selector
// or generic manager command is available to the signed or SSH protocol.
const recoveryServiceProperties = "Id,LoadState,ActiveState,SubState,FragmentPath,DropInPaths,Slice,ControlGroup,User,Group,Type,Restart,KillMode,ExecStart,Job,MainPID,InvocationID,ExecMainStartTimestampMonotonic,OnFailure,BindsTo,PartOf,Requires,After,JoinsNamespaceOf,NetworkNamespacePath,PrivateNetwork,PrivateUsers,PrivateMounts,MemoryMax,MemorySwapMax,TasksMax,CPUQuotaPerSecUSec,CPUQuotaPeriodUSec,AllowedCPUs,CPUAffinity,StartLimitIntervalUSec,TimeoutStartUSec,TimeoutStopUSec"

// A concrete manager call is descriptor-bound to the protected executable and
// retains finite process-group/pipe budgets. It invokes no daemon or worker.
func runRecoveryManagerCommand(parent context.Context, pin Pin, arguments []string, settings AgentSettings) ([]byte, error) {
	if pin.Path != "/usr/bin/systemctl" || settings.CleanupCommandTimeout <= 0 || settings.DiagnosticBytes <= 0 {
		return nil, errors.New("recovery manager requires its fixed finite executable")
	}
	expected := []string{"show", "--no-pager", "--all", "--property=" + recoveryServiceProperties, "--", managementRecoveryServiceName}
	if len(arguments) != len(expected) {
		return nil, errors.New("recovery manager has no generic command surface")
	}
	for index := range expected {
		if arguments[index] != expected[index] {
			return nil, errors.New("recovery manager has no generic command surface")
		}
	}
	file, err := openContainmentManagerExecutable(pin, DefaultRuntimeResourceBoundarySettings().ExecutableBytes, true)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(parent, settings.CleanupCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "/proc/self/fd/3", arguments...)
	command.Args[0] = pin.Path
	command.ExtraFiles = []*os.File{file}
	command.Env = controlEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = settings.CleanupCommandTimeout
	output := &boundedOutput{maximum: settings.DiagnosticBytes}
	command.Stdout, command.Stderr = output, &discardWriter{}
	if err := command.Run(); err != nil || ctx.Err() != nil || output.exceeded {
		return nil, errors.New("recovery manager projection unavailable or oversized")
	}
	return append([]byte(nil), output.Bytes()...), nil
}

// Source metadata is loaded/pinned without admitting or querying the private
// engine. Management recovery must still run if that engine is stopped.
func recoverySystemctlPin(config Config, settings AgentSettings, protected bool) (Pin, error) {
	content, err := readRecoveryAuthority(config.DockerRole.ResourceBoundary, settings.ConfigBytes, protected)
	if err != nil {
		return Pin{}, err
	}
	return containmentManagerExecutablePin(content, settings.ConfigBytes)
}

// Decode only the exact bytes whose content pin is protected, not a second
// pathname read after an earlier hash check could have observed another file.
func readRecoveryAuthority(pin Pin, maximum int64, protected bool) ([]byte, error) {
	if err := validateFilePin(pin, maximum, protected); err != nil {
		return nil, err
	}
	content, err := readBounded(pin.Path, maximum)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != pin.Sha256 {
		return nil, errors.New("recovery decoded authority differs from its protected content pin")
	}
	return content, nil
}

// Recovery's complete owner budget is derived from existing bounded control,
// cleanup and stop owners; none of it renews a submitted evaluation clock.
func recoveryServiceTimeout(settings AgentSettings) (time.Duration, error) {
	if settings.CleanupTimeout <= 0 || settings.ControlTimeout <= 0 || settings.CancelGrace <= 0 {
		return 0, errors.New("recovery service has no finite owner bounds")
	}
	// Two placement proofs, exact freeze, evaluator stop/post-stop, unit
	// status, independent cleanup and the service's own stop each retain
	// their existing owner budgets. This is not a new evaluation clock.
	total := time.Duration(0)
	for _, budget := range []time.Duration{settings.ControlTimeout, settings.ControlTimeout, settings.CleanupTimeout, settings.CleanupTimeout, settings.CancelGrace, settings.ControlTimeout, settings.CleanupTimeout, settings.CancelGrace} {
		next := total + budget
		if next <= total {
			return 0, errors.New("recovery owner budget overflow")
		}
		total = next
	}
	return total, nil
}

// Pinned unit text is closed as well as hashed. Extra programs, command prefixes,
// environment files, namespaces and reverse dependency hooks cannot hide in it.
func validateRecoveryServiceText(content []byte, config Config, policy *ManagementResourcePolicy, settings AgentSettings) error {
	if policy == nil || int64(len(content)) > settings.ConfigBytes {
		return errors.New("recovery service text exceeds its fixed role")
	}
	timeout, err := recoveryServiceTimeout(settings)
	if err != nil {
		return err
	}
	expected := map[string]string{
		"Unit.Description": "Independent subevaluation recovery outside ordinary management ancestry",
		"Unit.Requires":    managementCleanupSliceName, "Unit.After": managementCleanupSliceName, "Unit.StartLimitIntervalSec": "0",
		"Service.Type": "oneshot", "Service.User": "root", "Service.Group": "root", "Service.Slice": managementCleanupSliceName,
		"Service.ExecStart": systemdCommandWord(config.Agent.Path) + " --config " + systemdCommandWord(config.configPath) + " --recover-active",
		"Service.Restart":   "no", "Service.KillMode": "control-group", "Service.OOMPolicy": "kill", "Service.LimitCORE": "0", "Service.UMask": "0077",
		"Service.MemoryMax": strconv.FormatInt(DefaultManagementBoundarySettings().CleanupMemoryBytes, 10), "Service.MemorySwapMax": "0",
		"Service.TasksMax": strconv.FormatInt(DefaultManagementBoundarySettings().CleanupTasksMaximum, 10), "Service.CPUQuota": strconv.Itoa(settings.ManagementCpuQuotaPercent) + "%",
		"Service.CPUQuotaPeriodSec": strconv.FormatInt(settings.ContainmentCpuPeriodMicros, 10) + "us",
		"Service.AllowedCPUs":       policy.ManagementCpuSet, "Service.CPUAffinity": policy.ManagementCpuSet,
		"Service.TimeoutStartSec": strconv.FormatFloat(timeout.Seconds(), 'f', 6, 64) + "s", "Service.TimeoutStopSec": strconv.FormatFloat(settings.CancelGrace.Seconds(), 'f', 6, 64) + "s",
		"Service.StandardInput": "null", "Service.StandardOutput": "null", "Service.StandardError": "null",
		"Service.PrivateNetwork": "no", "Service.PrivateUsers": "no", "Service.PrivateMounts": "no",
		"Service.Environment": "\"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\" \"LANG=C\" \"LC_ALL=C\"",
	}
	seen := map[string]bool{}
	section := ""
	for _, line := range strings.Split(string(content), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[Unit]" || line == "[Service]" {
			section = strings.Trim(line, "[]")
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		name := section + "." + key
		want, known := expected[name]
		if !ok || !known || seen[name] || want != value {
			return errors.New("pinned recovery service expanded or changed its closed contract")
		}
		seen[name] = true
	}
	if len(seen) != len(expected) {
		return errors.New("pinned recovery service omitted a required independent owner property")
	}
	return nil
}

// Manager ExecStart rendering is unquoted strv_join; authority paths are
// deliberately whitespace-free so exact argv boundaries remain unambiguous.
func validateRecoveryExecStart(value string, config Config, current bool) error {
	if !strings.HasPrefix(value, "{ ") || !strings.HasSuffix(value, " }") {
		return errors.New("recovery manager omitted exact executable metadata")
	}
	fields := map[string]string{}
	for _, field := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(value, "{ "), " }"), " ; ") {
		key, value, ok := strings.Cut(field, "=")
		if _, exists := fields[key]; !ok || exists {
			return errors.New("recovery executable metadata is ambiguous")
		}
		fields[key] = value
	}
	if len(fields) != 8 || fields["path"] != config.Agent.Path || fields["argv[]"] != strings.Join([]string{config.Agent.Path, "--config", config.configPath, "--recover-active"}, " ") || fields["ignore_errors"] != "no" {
		return errors.New("recovery manager executable differs from the closed pinned role")
	}
	pid, err := strconv.ParseUint(fields["pid"], 10, 32)
	if err != nil || strconv.FormatUint(pid, 10) != fields["pid"] || current && pid != uint64(os.Getpid()) {
		return errors.New("recovery manager has no exact process identity")
	}
	for _, key := range []string{"start_time", "stop_time"} {
		if !strings.HasPrefix(fields[key], "[") || !strings.HasSuffix(fields[key], "]") {
			return errors.New("recovery manager timestamps are malformed")
		}
	}
	if fields["code"] != "(null)" && fields["code"] != "exited" && fields["code"] != "killed" && fields["code"] != "dumped" {
		return errors.New("recovery manager exit metadata is malformed")
	}
	if fields["status"] == "" || strings.ContainsAny(fields["status"], " ;{}\t\r\n") {
		return errors.New("recovery manager status is malformed")
	}
	return nil
}

// Exact fields/allowed boot dependencies refuse a target which would die with
// the ordinary engine, management aggregate or another run's evaluator.
func validateRecoveryServiceProjection(content []byte, config Config, policy *ManagementResourcePolicy, settings AgentSettings, current bool) error {
	values := map[string]string{}
	if int64(len(content)) > settings.DiagnosticBytes {
		return errors.New("recovery service projection exceeds its bound")
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("recovery service projection malformed")
		}
		if _, exists := values[key]; exists {
			return errors.New("recovery service projection duplicated authority")
		}
		values[key] = value
	}
	for _, key := range strings.Split(recoveryServiceProperties, ",") {
		if _, exists := values[key]; !exists {
			return errors.New("recovery service projection omitted required metadata")
		}
	}
	if len(values) != len(strings.Split(recoveryServiceProperties, ",")) {
		return errors.New("recovery service projection expanded its property surface")
	}
	if policy == nil || values["Id"] != managementRecoveryServiceName || values["FragmentPath"] != config.RecoveryService.Path || values["DropInPaths"] != "" || values["LoadState"] != "loaded" ||
		values["Slice"] != managementCleanupSliceName || values["User"] != "root" || values["Group"] != "root" || values["Type"] != "oneshot" || values["Restart"] != "no" || values["KillMode"] != "control-group" || values["StartLimitIntervalUSec"] != "0" {
		return errors.New("recovery target changed its loaded pinned root ownership")
	}
	if current {
		if values["ActiveState"] != "activating" || values["SubState"] != "start" || values["ControlGroup"] != managementCleanupCgroupPath+"/"+managementRecoveryServiceName {
			return errors.New("recovery is not executing in its independent oneshot service")
		}
	} else if values["ActiveState"] != "inactive" && values["ActiveState"] != "failed" || values["ControlGroup"] != "" && values["ControlGroup"] != managementCleanupCgroupPath+"/"+managementRecoveryServiceName || values["Job"] != "" {
		return errors.New("recovery target is active, pending or has a foreign cgroup")
	}
	mainPid, pidErr := strconv.Atoi(values["MainPID"])
	started, startErr := strconv.ParseUint(values["ExecMainStartTimestampMonotonic"], 10, 64)
	if pidErr != nil || strconv.Itoa(mainPid) != values["MainPID"] || startErr != nil || strconv.FormatUint(started, 10) != values["ExecMainStartTimestampMonotonic"] {
		return errors.New("recovery manager generation is not canonical")
	}
	if current {
		job, jobErr := strconv.ParseUint(values["Job"], 10, 32)
		invocation := values["InvocationID"]
		if mainPid != os.Getpid() || started == 0 || jobErr != nil || job == 0 || strconv.FormatUint(job, 10) != values["Job"] || len(invocation) != 32 || !digestPattern.MatchString(invocation+invocation) || invocation == strings.Repeat("0", 32) {
			return errors.New("recovery is not the observed current PID1 invocation")
		}
	} else if mainPid != 0 {
		return errors.New("inactive recovery target still owns a process")
	}
	for _, key := range []string{"OnFailure", "BindsTo", "PartOf", "JoinsNamespaceOf", "NetworkNamespacePath"} {
		if values[key] != "" {
			return errors.New("recovery depends on an ordinary/recursive owner")
		}
	}
	for _, key := range []string{"PrivateNetwork", "PrivateUsers", "PrivateMounts"} {
		if values[key] != "no" {
			return errors.New("recovery cannot join a failed private namespace")
		}
	}
	for _, key := range []string{"Requires", "After"} {
		found := false
		seen := map[string]bool{}
		for _, unit := range strings.Fields(values[key]) {
			if seen[unit] || unit != managementCleanupSliceName && unit != "sysinit.target" && (key != "After" || unit != "basic.target") {
				return errors.New("recovery target has an unapproved dependency")
			}
			seen[unit] = true
			found = found || unit == managementCleanupSliceName
		}
		if !found {
			return errors.New("recovery target omitted independent slice lifetime ordering")
		}
	}
	for key, want := range map[string]int64{"MemoryMax": DefaultManagementBoundarySettings().CleanupMemoryBytes, "MemorySwapMax": 0, "TasksMax": DefaultManagementBoundarySettings().CleanupTasksMaximum} {
		got, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || strconv.FormatInt(got, 10) != values[key] || got != want {
			return errors.New("recovery target resource ceiling differs from its explicit class")
		}
	}
	for _, key := range []string{"AllowedCPUs", "CPUAffinity"} {
		if validateRuntimeBoundaryManagementCpuSet(strings.ReplaceAll(values[key], " ", ","), policy.ManagementCpuSet) != nil {
			return errors.New("recovery target escaped management processors")
		}
	}
	timeout, err := recoveryServiceTimeout(settings)
	if err != nil {
		return err
	}
	for key, want := range map[string]int64{"CPUQuotaPerSecUSec": int64(settings.ManagementCpuQuotaPercent) * 10000, "CPUQuotaPeriodUSec": settings.ContainmentCpuPeriodMicros, "TimeoutStartUSec": timeout.Microseconds(), "TimeoutStopUSec": settings.CancelGrace.Microseconds()} {
		got, err := containmentManagerMicroseconds(values[key])
		if err != nil || got != want {
			return errors.New("recovery target owner timeout or CPU quota differs")
		}
	}
	return validateRecoveryExecStart(values["ExecStart"], config, current)
}

// Arming verifies the independent target and role natively, without starting it
// or touching an ordinary daemon. Tests replace private observation seams only.
func (self *SystemdRunner) armRecovery(ctx context.Context, launch Launch) error {
	if self.recoveryConfig == nil || self.recoveryBoundary == nil || self.recoveryCommand == nil || !digestPattern.MatchString(launch.ConfigSha256) {
		return errors.New("recovery arming has no exact configured native owner")
	}
	config, err := self.recoveryConfig(launch.ConfigPath)
	if err != nil {
		return err
	}
	if config == nil || config.configSha256 != launch.ConfigSha256 || config.Agent != launch.Agent || config.BaseDirectory != launch.BaseDirectory {
		return errors.New("frozen role configuration changed before recovery arming")
	}
	policy, err := loadManagementResourcePolicy(config.ManagementPolicy, DefaultManagementBoundarySettings(), self.settings.ConfigBytes, self.protected)
	if err != nil || validateRuntimeBoundaryManagementCpuSet(launch.ManagementCpuSet, policy.ManagementCpuSet) != nil {
		return errors.New("launch management processors differ from the armed recovery policy")
	}
	return verifyRecoveryService(ctx, *config, self.settings, self.protected, false, self.recoveryBoundary, self.recoveryCommand)
}

// The current role must prove independent placement before reading custody;
// no worker/signing/engine admission fallback can turn this into a no-op.
func (self *Agent) verifyRecoveryPlacement(ctx context.Context, current bool) error {
	return verifyRecoveryService(ctx, self.config, self.settings, self.protected, current, verifyManagementRecoveryBoundary, runRecoveryManagerCommand)
}

// Both protected documents and manager/kernel proof are sampled afresh. A
// changed config/pin or manager role invalidates the old admission receipt.
func verifyRecoveryService(ctx context.Context, config Config, settings AgentSettings, protected, current bool, boundary func(context.Context, *ManagementResourcePolicy, Pin, AgentSettings, bool, bool) error, command func(context.Context, Pin, []string, AgentSettings) ([]byte, error)) error {
	if settings.ControlTimeout <= 0 || boundary == nil || command == nil {
		return errors.New("recovery proof has no bounded concrete owner")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	configPin := Pin{Path: config.configPath, Sha256: config.configSha256}
	if validateFilePin(configPin, settings.ConfigBytes, protected) != nil {
		return errors.New("recovery role configuration changed before manager proof")
	}
	policy, err := loadManagementResourcePolicy(config.ManagementPolicy, DefaultManagementBoundarySettings(), settings.ConfigBytes, protected)
	if err != nil {
		return err
	}
	content, err := readRecoveryAuthority(config.RecoveryService, settings.ConfigBytes, protected)
	if err != nil || validateRecoveryServiceText(content, config, policy, settings) != nil {
		return errors.New("recovery service is not the closed protected template")
	}
	manager, err := recoverySystemctlPin(config, settings, protected)
	if err != nil {
		return err
	}
	arguments := []string{"show", "--no-pager", "--all", "--property=" + recoveryServiceProperties, "--", managementRecoveryServiceName}
	projection, err := command(ctx, manager, arguments, settings)
	if err != nil {
		return err
	}
	if err := validateRecoveryServiceProjection(projection, config, policy, settings, current); err != nil {
		return err
	}
	if err := boundary(ctx, policy, manager, settings, protected, current); err != nil {
		return err
	}
	after, err := command(ctx, manager, arguments, settings)
	if err != nil || !bytes.Equal(projection, after) {
		return errors.New("recovery service generation changed around kernel proof")
	}
	for _, pin := range []Pin{configPin, config.RecoveryService, config.ManagementPolicy, config.DockerRole.ResourceBoundary} {
		if validateFilePin(pin, settings.ConfigBytes, protected) != nil {
			return errors.New("recovery authority changed during manager observation")
		}
	}
	return ctx.Err()
}
