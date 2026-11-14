// A legacy closure requires an operator-installed ingress fence and exact PID1
// start denials. Native observations never accept a Boolean supplied in a permit.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// These are closed protocol/template bytes, not runtime policy tunables.
const legacyPrelaunchIngressFence = "#!/bin/sh\nexit 1\n"
const legacyPrelaunchUnitProperties = "Id,LoadState,ActiveState,SubState,FragmentPath,DropInPaths,Job,MainPID,ControlPID,ControlGroup,RefuseManualStart,NeedDaemonReload"

// Each closure owner bounds its own process census. Unknown or excessive work
// refuses closure instead of truncating the observation to a safe-looking set.
type legacyPrelaunchNativeSettings struct {
	MaximumProcesses int
}

// A native process census includes kernel tasks, not only matching executables.
func defaultLegacyPrelaunchNativeSettings() legacyPrelaunchNativeSettings {
	return legacyPrelaunchNativeSettings{MaximumProcesses: 16 * 1024}
}

// The caller installs these inert bytes only under explicit host authority.
func RenderLegacyPrelaunchIngressFence() []byte {
	return []byte(legacyPrelaunchIngressFence)
}

// A loaded condition denies dependency starts too; RefuseManualStart alone
// would not close that surface. The protected authorization remains in place.
func RenderLegacyPrelaunchOwnerDenial(authorizationPath string) ([]byte, error) {
	if !recoveryAuthorityPath(authorizationPath) {
		return nil, errors.New("legacy owner denial requires the exact authorization path")
	}
	return []byte("[Unit]\nRefuseManualStart=yes\nConditionPathExists=!" + authorizationPath + "\n"), nil
}

// File-content authority excludes links and writable metadata even for root.
func legacyPrelaunchSealedFile(path string, protected bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || stat.Nlink != 1 || protected && stat.Uid != 0 {
		return errors.New("legacy closure metadata is not a sealed single-link file")
	}
	return nil
}

// Original execution authority and new recovery-only authority stay separate.
type legacyPrelaunchNativeHost struct {
	agent             *Agent
	authorization     LegacyPrelaunchAuthorization
	authorizationPath string
	digest            string
	manager           Pin
	settings          legacyPrelaunchNativeSettings
	controls          []managementSshPlan
	controlUid        uint32
}

// No daemon, manager mutation or execution occurs at construction.
func newLegacyPrelaunchNativeHost(agent *Agent, authorization LegacyPrelaunchAuthorization, path, digest string) (*legacyPrelaunchNativeHost, error) {
	if agent == nil || !agent.recoveryOnly || authorization.Sshd.Path != "/usr/sbin/sshd" || filepath.Base(authorization.RecoveryService.Path) != managementRecoveryServiceName {
		return nil, errors.New("legacy closure omitted its fixed native role")
	}
	if _, err := validateLegacyPrelaunchPrior(path, authorization, agent.settings, agent.protected); err != nil {
		return nil, err
	}
	manager, err := recoverySystemctlPin(agent.config, agent.settings, agent.protected)
	if err != nil {
		return nil, err
	}
	control, err := user.Lookup(authorization.ControlUser)
	if err != nil {
		return nil, errors.New("legacy closure fixed SSH control user is unavailable")
	}
	uid, err := strconv.ParseUint(control.Uid, 10, 32)
	if err != nil || uid == 0 || strconv.FormatUint(uid, 10) != control.Uid {
		return nil, errors.New("legacy closure fixed SSH control user has ambiguous identity")
	}
	return &legacyPrelaunchNativeHost{agent: agent, authorization: authorization, authorizationPath: path, digest: digest, manager: manager,
		settings: defaultLegacyPrelaunchNativeSettings(), controlUid: uint32(uid)}, nil
}

// Both roles retain the same fixed independent PID1 service and native caps.
// Only this separately pinned template can select the new authorization role.
func (self *legacyPrelaunchNativeHost) Placement(ctx context.Context) error {
	config := self.agent.config
	config.Agent, config.RecoveryService = self.authorization.RecoveryAgent, self.authorization.RecoveryService
	config.configPath, config.configSha256 = self.authorizationPath, self.digest
	if validatePin(config.Agent, self.agent.protected) != nil {
		return errors.New("legacy recovery executable changed")
	}
	executable, err := os.Executable()
	if err != nil || executable != config.Agent.Path {
		return errors.New("legacy recovery is not its exact new pinned executable")
	}
	return verifyRecoveryServiceForRole(ctx, config, self.agent.settings, self.agent.protected, true,
		verifyManagementRecoveryBoundary, runRecoveryManagerCommand, "--close-legacy-prelaunch")
}

// All commands have closed native callers. Output remains private proof bytes.
func (self *legacyPrelaunchNativeHost) command(ctx context.Context, pin Pin, arguments []string) ([]byte, error) {
	settings := DefaultRuntimeResourceBoundarySettings()
	settings.DocumentBytes, settings.NativeBytes, settings.CommandTimeout = self.agent.settings.ConfigBytes, self.agent.settings.DiagnosticBytes, self.agent.settings.CleanupCommandTimeout
	return runtimeBoundaryRunCommand(ctx, pin, arguments, nil, settings)
}

// The effective fixed SSH user must route every operation to the inert wrapper.
// Key bytes and effective configuration never leave the native proof owner.
func (self *legacyPrelaunchNativeHost) ingress(ctx context.Context) error {
	authorization := self.authorization
	if _, err := validateLegacyPrelaunchPrior(self.authorizationPath, authorization, self.agent.settings, self.agent.protected); err != nil {
		return err
	}
	if err := validateLegacyPrelaunchIngressPins(authorization, self.agent.settings, self.agent.protected); err != nil {
		return err
	}
	if authorization.OriginalIngressWrapper.Path != authorization.IngressWrapper.Path || authorization.OriginalIngressWrapperArchive.Path == authorization.IngressWrapper.Path || authorization.OriginalIngressWrapperArchive.Sha256 != authorization.OriginalIngressWrapper.Sha256 {
		return errors.New("legacy ingress fence did not replace the exact old forced wrapper in place")
	}
	archived, err := readRecoveryAuthority(authorization.OriginalIngressWrapperArchive, self.agent.settings.ConfigBytes, self.agent.protected)
	expectedOriginal := "#!/bin/bash\n# Fixed signed protocol only; never execute SSH_ORIGINAL_COMMAND.\nset -Eeuo pipefail\n[[ $# == 0 ]] || exit 2\numask 077\nexec " + self.agent.config.Agent.Path + " --config " + self.agent.config.configPath + " --serve-managed\n"
	if err != nil || !bytes.Equal(archived, []byte(expectedOriginal)) {
		return errors.New("legacy historical forced wrapper does not match its closed original route")
	}
	wrapper, err := readRecoveryAuthority(authorization.IngressWrapper, self.agent.settings.ConfigBytes, self.agent.protected)
	if err != nil || !bytes.Equal(wrapper, []byte(legacyPrelaunchIngressFence)) {
		return errors.New("legacy ingress wrapper is not the closed denial template")
	}
	content, err := self.command(ctx, authorization.Sshd, []string{"-T", "-C", "user=" + authorization.ControlUser})
	if err != nil {
		return errors.New("legacy effective SSH ingress is unavailable")
	}
	if err := validateLegacyPrelaunchSshdProjection(content, authorization, self.agent.settings.DiagnosticBytes); err != nil {
		return err
	}
	return self.boot(ctx)
}

// sshd -T legitimately repeats unrelated list keys such as hostkey. Only the
// three exact routing-authority keys must be present once and match their pins.
func validateLegacyPrelaunchSshdProjection(content []byte, authorization LegacyPrelaunchAuthorization, maximum int64) error {
	if maximum <= 0 || int64(len(content)) > maximum {
		return errors.New("legacy effective SSH ingress exceeded its owner bound")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, found := strings.Cut(line, " ")
		if !found || key == "" {
			return errors.New("legacy effective SSH ingress is ambiguous")
		}
		if key == "forcecommand" || key == "authorizedkeysfile" || key == "authorizedkeyscommand" {
			if _, duplicate := values[key]; duplicate {
				return errors.New("legacy effective SSH ingress repeats routing authority")
			}
			values[key] = value
		}
	}
	if values["forcecommand"] != "/usr/bin/sudo -n -- "+authorization.IngressWrapper.Path || values["authorizedkeysfile"] != authorization.AuthorizedKeys.Path || values["authorizedkeyscommand"] != "none" {
		return errors.New("legacy effective SSH ingress differs from its exact fixed route")
	}
	return nil
}

// Executable pins and document pins retain their different existing bounds.
// A genuine SSH ELF must not inherit the much smaller configuration limit.
func validateLegacyPrelaunchIngressPins(authorization LegacyPrelaunchAuthorization, settings AgentSettings, protected bool) error {
	if validatePin(authorization.Sshd, protected) != nil {
		return errors.New("legacy ingress SSH executable changed")
	}
	for _, pin := range []Pin{authorization.OriginalConfig, authorization.OriginalRecoveryServiceArchive, authorization.IngressWrapper,
		authorization.OriginalIngressWrapperArchive, authorization.AuthorizedKeys, authorization.OwnerStartDenial} {
		if validateFilePin(pin, settings.ConfigBytes, protected) != nil {
			return errors.New("legacy ingress document or start-denial authority changed")
		}
	}
	for _, role := range authorization.PriorControlRoles {
		if validatePin(role.Agent, protected) != nil || validateFilePin(role.Config, settings.ConfigBytes, protected) != nil {
			return errors.New("legacy prior control-role authority changed")
		}
	}
	return nil
}

// The boot UUID comes only from native procfs, never from a pathname in a permit.
func (self *legacyPrelaunchNativeHost) boot(ctx context.Context) error {
	root, err := openDirectoryNoFollow("/proc")
	if err != nil {
		return err
	}
	defer root.Close()
	var filesystem syscall.Statfs_t
	if syscall.Fstatfs(int(root.Fd()), &filesystem) != nil || filesystem.Type != 0x9fa0 {
		return errors.New("legacy closure has no native procfs boot identity")
	}
	content, err := readBounded("/proc/sys/kernel/random/boot_id", self.agent.settings.ConfigBytes)
	if err != nil || strings.TrimSuffix(string(content), "\n") != self.authorization.BootId {
		return errors.New("legacy closure cannot cross a boot generation")
	}
	return ctx.Err()
}

// Every root-sealed registry plan predates its own possible PID1 launch.
// With ingress fenced and old issuers gone, withdrawing each admission closes
// even a delayed managed-control service whose PID1 acknowledgement was lost.
func (self *legacyPrelaunchNativeHost) CloseControls(ctx context.Context) error {
	if err := self.ingress(ctx); err != nil {
		return err
	}
	registry, err := openManagementSshRegistry(self.agent.config, DefaultManagementSshSettings(), self.agent.protected)
	if err != nil {
		return err
	}
	defer registry.Close()
	lock, err := registry.lock(ctx)
	if err != nil {
		return err
	}
	reader, err := openRelativeNoFollow(registry.directory, ".", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		lock.Close()
		return err
	}
	maximum := registry.settings.MaximumControls * 4
	entries, readErr := reader.ReadDir(maximum + 1)
	_ = reader.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(entries) > maximum {
		lock.Close()
		return errors.New("legacy managed-control census exceeded its bound")
	}
	plans := []managementSshPlan{}
	known := map[string]bool{}
	for _, entry := range entries {
		if entry.Name() == "registry.lock" {
			continue
		}
		id, suffix, found := strings.Cut(entry.Name(), ".")
		if !found || !digestPattern.MatchString(id) || suffix != "json" && suffix != "admit" && suffix != "admitted" && suffix != "closed" || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			lock.Close()
			return errors.New("legacy managed-control registry has unrelated custody")
		}
		if suffix != "json" {
			continue
		}
		content, err := registry.read(entry.Name())
		plan := managementSshPlan{}
		if err != nil || validateDocumentKeys(content) != nil || decodeStrict(content, &plan) != nil || plan.Schema != Schema || plan.Id != id || plan.UnitName != managementSshUnitName(id) ||
			!digestPattern.MatchString(plan.ConfigSha256) || !digestPattern.MatchString(plan.Agent.Sha256) || !recoveryAuthorityPath(plan.Agent.Path) || plan.CreatedAt.IsZero() || !plan.ExpiresAt.After(plan.CreatedAt) {
			lock.Close()
			return errors.New("legacy managed-control plan failed root-custody authentication")
		}
		knownAgent := plan.Agent == self.agent.config.Agent
		for _, role := range self.authorization.PriorControlRoles {
			knownAgent = knownAgent || plan.Agent == role.Agent
		}
		if !knownAgent {
			lock.Close()
			return errors.New("legacy managed-control generation has an unrecognized executable")
		}
		if err := registry.withdraw(id); err != nil {
			lock.Close()
			return err
		}
		plans, known[plan.UnitName] = append(plans, plan), true
	}
	lock.Close()
	slices.SortFunc(plans, func(left, right managementSshPlan) int { return strings.Compare(left.UnitName, right.UnitName) })
	self.controls = plans
	units, err := self.command(ctx, self.manager, []string{"list-units", "--all", "--full", "--plain", "--no-legend", "--no-pager", "urnetwork-subeval-control-*.service"})
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(units)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || !known[fields[0]] {
			return errors.New("legacy PID1 contains an unrecorded control generation")
		}
	}
	if len(plans) != 0 {
		arguments := []string{"stop", "--"}
		for _, plan := range plans {
			arguments = append(arguments, plan.UnitName)
		}
		// Lost stop acknowledgement is not proof. Verify observes each exact
		// manager generation and native cgroup independently below.
		_, _ = self.command(ctx, self.manager, arguments)
	}
	_, _ = self.command(ctx, self.manager, []string{"stop", "--", dockerOwnerUnitName(self.authorization.RunId), unitName(self.authorization.RunId)})
	_, err = self.Verify(ctx)
	return err
}

// No old native executable or in-flight systemd-run issuer may survive the
// ingress fence. Every proc entry is bounded and sampled against PID reuse.
func (self *legacyPrelaunchNativeHost) noLaunchers(ctx context.Context) error {
	if self.settings.MaximumProcesses <= 0 {
		return errors.New("legacy process census has no owner bound")
	}
	listenerProjection, err := self.command(ctx, self.manager, []string{"show", "--all", "--no-pager", "--property=Id,LoadState,ActiveState,MainPID", "--", "ssh.service"})
	if err != nil {
		return errors.New("legacy SSH listener generation is unavailable")
	}
	listener := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(listenerProjection), "\n"), "\n") {
		key, value, found := strings.Cut(line, "=")
		if _, duplicate := listener[key]; !found || duplicate {
			return errors.New("legacy SSH listener generation is ambiguous")
		}
		listener[key] = value
	}
	listenerPid, err := strconv.Atoi(listener["MainPID"])
	if err != nil || listenerPid <= 0 || strconv.Itoa(listenerPid) != listener["MainPID"] || len(listener) != 4 || listener["Id"] != "ssh.service" || listener["LoadState"] != "loaded" || listener["ActiveState"] != "active" {
		return errors.New("legacy SSH listener lacks exact native manager custody")
	}
	targets := map[string]bool{self.agent.config.Agent.Path: true, self.agent.config.configPath: true, self.authorization.IngressWrapper.Path: true}
	for _, role := range self.authorization.PriorControlRoles {
		targets[role.Agent.Path], targets[role.Config.Path] = true, true
	}
	for _, plan := range self.controls {
		targets[plan.Agent.Path] = true
	}
	root, err := openDirectoryNoFollow("/proc")
	if err != nil {
		return err
	}
	defer root.Close()
	var filesystem syscall.Statfs_t
	if syscall.Fstatfs(int(root.Fd()), &filesystem) != nil || filesystem.Type != 0x9fa0 {
		return errors.New("legacy process census is not native procfs")
	}
	entries, err := root.ReadDir(self.settings.MaximumProcesses + 1)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > self.settings.MaximumProcesses {
		return errors.New("legacy process census exceeded its bound")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || strconv.Itoa(pid) != entry.Name() || pid == os.Getpid() {
			continue
		}
		directory, err := openRelativeNoFollow(root, entry.Name(), os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return errors.New("legacy process generation cannot be observed")
		}
		read := func(name string) ([]byte, error) {
			file, err := openRelativeNoFollow(directory, name, os.O_RDONLY, 0)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			content, err := io.ReadAll(io.LimitReader(file, self.agent.settings.ConfigBytes+1))
			if err != nil || int64(len(content)) > self.agent.settings.ConfigBytes {
				return nil, errors.New("legacy process observation exceeded its bound")
			}
			return content, nil
		}
		before, beforeErr := read("stat")
		arguments, argumentErr := read("cmdline")
		status, statusErr := read("status")
		executable, executableErr := os.Readlink("/proc/" + entry.Name() + "/exe")
		after, afterErr := read("stat")
		_ = directory.Close()
		if errors.Is(beforeErr, os.ErrNotExist) || errors.Is(argumentErr, os.ErrNotExist) || errors.Is(statusErr, os.ErrNotExist) || errors.Is(afterErr, os.ErrNotExist) || errors.Is(beforeErr, syscall.ESRCH) || errors.Is(argumentErr, syscall.ESRCH) || errors.Is(statusErr, syscall.ESRCH) || errors.Is(afterErr, syscall.ESRCH) {
			continue
		}
		first, firstErr := runtimeBoundaryProcessStart(before)
		last, lastErr := runtimeBoundaryProcessStart(after)
		if beforeErr != nil || argumentErr != nil || statusErr != nil || afterErr != nil || firstErr != nil || lastErr != nil || first != last {
			return errors.New("legacy process generation changed or is unreadable")
		}
		seenUid := false
		for _, line := range strings.Split(string(status), "\n") {
			if !strings.HasPrefix(line, "Uid:") {
				continue
			}
			fields := strings.Fields(line)
			if seenUid || len(fields) != 5 {
				return errors.New("legacy process user identity is ambiguous")
			}
			seenUid = true
			for _, value := range fields[1:] {
				uid, err := strconv.ParseUint(value, 10, 32)
				if err != nil || uid == uint64(self.controlUid) {
					return errors.New("legacy SSH control-user connection or launcher remains present")
				}
			}
		}
		if !seenUid {
			return errors.New("legacy process omitted its native user identity")
		}
		if err := legacyPrelaunchSshGeneration(arguments, executable, executableErr, pid, listenerPid, self.authorization); err != nil {
			return err
		}
		if err := validateLegacyPrelaunchLauncher(arguments, executable, targets); err != nil {
			return err
		}
	}
	return nil
}

// A retired manager record never licenses forgetting its native executable or
// config argv. Unknown agent generations also refuse closure by fixed basename.
func validateLegacyPrelaunchLauncher(arguments []byte, executable string, targets map[string]bool) error {
	if targets[executable] || filepath.Base(executable) == "competitionsubeval" {
		return errors.New("legacy native launcher executable remains present")
	}
	for _, argument := range bytes.Split(arguments, []byte{0}) {
		if targets[string(argument)] {
			return errors.New("legacy launch-capable process is still present")
		}
	}
	return nil
}

// Authenticated sessions for other users cannot change into the control user.
// Unknown/preauth children may still cache old options, so they refuse closure.
func legacyPrelaunchSshGeneration(arguments []byte, executable string, executableErr error, pid, listenerPid int, authorization LegacyPrelaunchAuthorization) error {
	command := strings.TrimRight(string(arguments), "\x00")
	sshDescription := strings.HasPrefix(command, "sshd: ")
	sshExecutable := executable == authorization.Sshd.Path
	if !sshDescription && !sshExecutable {
		return nil
	}
	if executableErr != nil || !sshExecutable {
		return errors.New("legacy SSH process executable is unknown")
	}
	if pid == listenerPid {
		if !strings.Contains(command, "[listener]") {
			return errors.New("legacy SSH listener identity changed")
		}
		return nil
	}
	if !sshDescription || strings.ContainsAny(command, "\x00\r\n") || strings.Contains(command, "[listener]") || strings.Contains(command, "[preauth]") || strings.Contains(command, "[net]") || strings.Contains(command, "[accepted]") {
		return errors.New("legacy SSH connection has uncertain old launch options")
	}
	fields := strings.Fields(strings.TrimPrefix(command, "sshd: "))
	if len(fields) == 0 {
		return errors.New("legacy SSH connection omitted its authenticated user")
	}
	name, _, _ := strings.Cut(fields[0], "@")
	if !namePattern.MatchString(name) || name == "unknown" || name == authorization.ControlUser {
		return errors.New("legacy SSH control or preauth generation remains present")
	}
	return nil
}

// Manager acknowledgements and native cgroup emptiness are independent. The
// evaluator mask is recorded as a NEW denial, never original unit existence.
func (self *legacyPrelaunchNativeHost) unit(ctx context.Context, name, role string) ([]byte, error) {
	content, err := self.command(ctx, self.manager, []string{"show", "--all", "--no-pager", "--property=" + legacyPrelaunchUnitProperties, "--", name})
	if err != nil {
		return nil, err
	}
	if err := validateLegacyPrelaunchUnit(content, name, role, self.authorization); err != nil {
		return nil, err
	}
	empty, err := legacyPrelaunchKernelEmpty(ctx, name, self.agent.settings)
	if err != nil || !empty {
		return nil, errors.New("legacy unit native cgroup is not positively empty")
	}
	return content, nil
}

// A denial tombstone and the historical owner are intentionally different
// classes. Neither missing metadata nor a newly masked unit is a stop receipt.
func validateLegacyPrelaunchUnit(content []byte, name, role string, authorization LegacyPrelaunchAuthorization) error {
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			return errors.New("legacy unit proof is malformed")
		}
		if _, duplicate := values[key]; duplicate {
			return errors.New("legacy unit proof is duplicated")
		}
		values[key] = value
	}
	if len(values) != len(strings.Split(legacyPrelaunchUnitProperties, ",")) || values["Id"] != name || values["Job"] != "" && values["Job"] != "0" || values["MainPID"] != "0" || values["ControlPID"] != "0" ||
		values["ActiveState"] != "inactive" && values["ActiveState"] != "failed" || values["NeedDaemonReload"] != "no" {
		return errors.New("legacy unit still has active, pending or uncertain custody")
	}
	if values["ActiveState"] == "inactive" && values["SubState"] != "dead" || values["ActiveState"] == "failed" && values["SubState"] != "failed" {
		return errors.New("legacy unit manager state is internally inconsistent")
	}
	group := managementPrivateCgroupPath + "/" + name
	if values["ControlGroup"] != "" && values["ControlGroup"] != group {
		return errors.New("legacy unit has a foreign native cgroup")
	}
	if role == "owner" {
		if name != dockerOwnerUnitName(authorization.RunId) || values["LoadState"] != "loaded" || values["FragmentPath"] != "/run/systemd/transient/"+name || values["DropInPaths"] != authorization.OwnerStartDenial.Path || values["RefuseManualStart"] != "yes" {
			return errors.New("legacy owner does not retain its exact loaded start denial")
		}
	} else if role == "evaluator" {
		if name != unitName(authorization.RunId) || values["LoadState"] != "masked" || values["FragmentPath"] != "/etc/systemd/system/"+name || values["DropInPaths"] != "" {
			return errors.New("legacy evaluator has no positive PID1 launch-denial tombstone")
		}
	} else if role != "control" || !validManagementSshUnit(name) || values["LoadState"] != "not-found" && values["LoadState"] != "loaded" ||
		values["DropInPaths"] != "" || values["LoadState"] == "loaded" && values["FragmentPath"] != "/run/systemd/transient/"+name || values["LoadState"] == "not-found" && values["FragmentPath"] != "" {
		return errors.New("legacy control generation is not positively retired")
	}
	return nil
}

// Missing groups are inspected only after the independently authenticated
// PID1 start denial/retirement proof, not used to infer it.
func legacyPrelaunchKernelEmpty(ctx context.Context, name string, settings AgentSettings) (bool, error) {
	if !validUnitName(name) && !validManagementSshUnit(name) {
		return false, errors.New("legacy kernel scope is outside its fixed unit classes")
	}
	root, err := openDirectoryNoFollow("/sys/fs/cgroup")
	if err != nil {
		return false, err
	}
	defer root.Close()
	var filesystem syscall.Statfs_t
	if syscall.Fstatfs(int(root.Fd()), &filesystem) != nil || filesystem.Type != 0x63677270 {
		return false, errors.New("legacy unit proof is not native cgroup v2")
	}
	group, err := openRelativeNoFollow(root, strings.TrimPrefix(managementPrivateCgroupPath+"/"+name, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer group.Close()
	for _, property := range []string{"pids.current", "cgroup.procs"} {
		value, err := readManagementKernelProperty(ctx, group, property, settings.DiagnosticBytes, true)
		if err != nil {
			return false, err
		}
		if property == "pids.current" && value != "0" || property == "cgroup.procs" && value != "" {
			return false, nil
		}
	}
	events, err := readManagementSshFile(group, "cgroup.events", settings.DiagnosticBytes, 0x63677270)
	if err != nil {
		return false, err
	}
	seen := false
	for _, line := range strings.Split(strings.TrimSuffix(string(events), "\n"), "\n") {
		key, value, found := strings.Cut(line, " ")
		if !found {
			return false, errors.New("legacy cgroup events are ambiguous")
		}
		if key == "populated" {
			if seen || value != "0" {
				return false, nil
			}
			seen = true
		}
	}
	return seen, ctx.Err()
}

// This is sampled both before and after the unchanged full cleanup chain.
func (self *legacyPrelaunchNativeHost) Verify(ctx context.Context) ([]byte, error) {
	if err := self.ingress(ctx); err != nil {
		return nil, err
	}
	if err := self.noLaunchers(ctx); err != nil {
		return nil, err
	}
	fenceAuthorizationPath := self.authorizationPath
	if self.authorization.PriorAuthorization != nil {
		fenceAuthorizationPath = self.authorization.PriorAuthorization.Path
	}
	expected, err := RenderLegacyPrelaunchOwnerDenial(fenceAuthorizationPath)
	content, readErr := readRecoveryAuthority(self.authorization.OwnerStartDenial, self.agent.settings.ConfigBytes, self.agent.protected)
	owner := dockerOwnerUnitName(self.authorization.RunId)
	if err != nil || readErr != nil || !bytes.Equal(expected, content) || self.authorization.OwnerStartDenial.Path != "/etc/systemd/system/"+owner+".d/99-legacy-prelaunch-closure.conf" {
		return nil, errors.New("legacy owner start denial differs from its exact closed template")
	}
	mask := "/etc/systemd/system/" + unitName(self.authorization.RunId)
	if protectedRootPath(filepath.Dir(mask), true, false) != nil {
		return nil, errors.New("legacy evaluator denial parent is not root protected")
	}
	info, err := os.Lstat(mask)
	target, targetErr := os.Readlink(mask)
	if err != nil || targetErr != nil || info.Mode()&os.ModeSymlink == 0 || target != "/dev/null" {
		return nil, errors.New("legacy evaluator denial is not the exact root-owned mask")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Nlink != 1 {
		return nil, errors.New("legacy evaluator denial has ambiguous ownership")
	}
	proofs := map[string]string{"authorization_sha256": self.digest, "boot_id": self.authorization.BootId}
	for _, role := range []string{"owner", "evaluator"} {
		name := owner
		if role == "evaluator" {
			name = unitName(self.authorization.RunId)
		}
		proof, err := self.unit(ctx, name, role)
		if err != nil {
			return nil, err
		}
		proofs[role] = digestDockerBytes(proof)
	}
	registry, err := openManagementSshRegistry(self.agent.config, DefaultManagementSshSettings(), self.agent.protected)
	if err != nil {
		return nil, err
	}
	defer registry.Close()
	for _, plan := range self.controls {
		if _, err := registry.read(plan.Id + ".admit"); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("legacy control admission is present or unreadable")
		}
		proof, err := self.unit(ctx, plan.UnitName, "control")
		if err != nil {
			return nil, err
		}
		proofs[plan.UnitName] = digestDockerBytes(proof)
	}
	reader, err := openRelativeNoFollow(registry.directory, ".", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	maximum := registry.settings.MaximumControls * 4
	entries, readErr := reader.ReadDir(maximum + 1)
	_ = reader.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(entries) > maximum {
		return nil, errors.New("legacy control recheck exceeded its bound")
	}
	known := map[string]bool{}
	for _, plan := range self.controls {
		known[plan.Id] = true
	}
	for _, entry := range entries {
		if entry.Name() == "registry.lock" {
			continue
		}
		id, suffix, found := strings.Cut(entry.Name(), ".")
		if !found || !known[id] || suffix != "json" && suffix != "admitted" && suffix != "closed" || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, errors.New("legacy control generation or admission changed after withdrawal")
		}
	}
	if err := self.noLaunchers(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(proofs)
}
