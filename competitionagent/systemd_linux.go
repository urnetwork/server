// Detached systemd services own evaluator trees independently of SSH sessions.
// A lost launch acknowledgement never authorizes starting another service.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The launcher contains only agent-owned paths and the signed remaining budget.
type Launch struct {
	UnitName         string
	RunId            string
	ConfigPath       string
	BaseDirectory    string
	Agent            Pin
	DeadlineAt       time.Time
	Role             string
	OwnerUnitName    string
	ManagementCpuSet string
}

// Active includes starting and stopping so neither can free the physical slot.
type UnitStatus struct {
	Exists   bool
	Active   bool
	Failed   bool
	ExitCode int
}

// Tests replace only service management; the durable admission path is real.
type Runner interface {
	Start(context.Context, Launch) error
	Status(context.Context, string) (UnitStatus, error)
	Stop(context.Context, string) error
}

// The concrete runner is safe for concurrent control calls. It never uses a
// shell, a caller-selected executable, or inherited production environment.
type SystemdRunner struct {
	settings AgentSettings
}

// Host paths are system-owned commands, not values from the wire protocol.
func NewSystemdRunner(settings AgentSettings) *SystemdRunner {
	return &SystemdRunner{settings: settings}
}

// The immutable unit name is retained after exit; no --collect or restart can
// turn a reconnect into another execution. RuntimeMaxSec is never a fresh clock.
func (self *SystemdRunner) Start(ctx context.Context, launch Launch) error {
	if launch.ManagementCpuSet == "" || launch.Role == "" && launch.OwnerUnitName != dockerOwnerUnitName(launch.RunId) {
		return errors.New("protected systemd launch requires bounded independent management custody")
	}
	if err := validatePin(launch.Agent, true); err != nil {
		return err
	}
	args, err := systemdLaunchArguments(launch, self.settings)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "/usr/bin/systemd-run", args...)
	command.Env = controlEnvironment()
	command.Stdout, command.Stderr = &discardWriter{}, &discardWriter{}
	if err := command.Run(); err != nil {
		return errors.New("systemd launch outcome is uncertain; reconcile the original unit")
	}
	return nil
}

// ExecStopPost runs after unexpected exit and startup failure as well as normal
// stop. KillMode alone cannot reap Docker/BuildKit's separate daemon cgroups.
// See systemd.service(5), ExecStopPost and Command lines. The post-stop command
// uses only the same pinned agent/config and immutable persisted run identity.
func systemdLaunchArguments(launch Launch, settings AgentSettings) ([]string, error) {
	executionFlag, cleanupFlag := "--execute=", "--cleanup="
	expectedUnit := unitName(launch.RunId)
	if launch.Role == "docker-owner" {
		executionFlag, cleanupFlag, expectedUnit = "--docker-owner=", "--freeze=", dockerOwnerUnitName(launch.RunId)
	} else if launch.Role != "" {
		return nil, errors.New("systemd role is outside the fixed evaluator/owner pair")
	}
	if !validUnitName(launch.UnitName) || !digestPattern.MatchString(launch.RunId) || launch.UnitName != expectedUnit ||
		settings.Now == nil || settings.CancelGrace <= 0 || settings.CleanupTimeout <= 0 {
		return nil, errors.New("invalid fixed systemd launch identity")
	}
	for _, path := range []string{launch.BaseDirectory, launch.ConfigPath, launch.Agent.Path} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "$%\n\r\x00") {
			return nil, errors.New("systemd authority paths must be canonical and cannot contain substitutions")
		}
	}
	remaining := launch.DeadlineAt.Sub(settings.Now().UTC())
	if remaining <= 0 {
		return nil, errors.New("subevaluation deadline already elapsed")
	}
	stopTimeout := settings.CancelGrace + settings.CleanupTimeout
	if stopTimeout <= settings.CancelGrace {
		return nil, errors.New("systemd cleanup stop allowance overflowed")
	}
	cleanup := strings.Join([]string{systemdCommandWord(launch.Agent.Path), systemdCommandWord("--config"),
		systemdCommandWord(launch.ConfigPath), systemdCommandWord(cleanupFlag + launch.RunId)}, " ")
	args := []string{"--quiet", "--no-block", "--unit=" + launch.UnitName, "--service-type=exec",
		"--property=RequiresMountsFor=" + systemdCommandWord(launch.BaseDirectory),
		"--property=Restart=no", "--property=KillMode=control-group", "--property=SendSIGKILL=yes",
		"--property=ExecStopPost=" + cleanup,
		"--property=RuntimeMaxSec=" + strconv.FormatFloat(remaining.Seconds(), 'f', 6, 64) + "s",
		"--property=TimeoutStopSec=" + strconv.FormatFloat(stopTimeout.Seconds(), 'f', 6, 64) + "s",
		"--property=StandardInput=null", "--property=StandardOutput=null", "--property=StandardError=null",
		"--property=UMask=0077"}
	if launch.OwnerUnitName != "" {
		if launch.Role != "" || launch.OwnerUnitName != dockerOwnerUnitName(launch.RunId) {
			return nil, errors.New("evaluator must bind only its own independent Docker service")
		}
		args = append(args, "--property=BindsTo="+launch.OwnerUnitName, "--property=After="+launch.OwnerUnitName)
	}
	if launch.ManagementCpuSet != "" {
		if !dockerProxyCpuSetPattern.MatchString(launch.ManagementCpuSet) || settings.ManagementMemoryBytes <= 0 || settings.ManagementCpuQuotaPercent <= 0 {
			return nil, errors.New("invalid bounded management resource identity")
		}
		cpus := strings.ReplaceAll(launch.ManagementCpuSet, ",", " ")
		args = append(args, "--property=AllowedCPUs="+cpus, "--property=CPUAffinity="+cpus,
			"--property=MemoryMax="+strconv.FormatInt(settings.ManagementMemoryBytes, 10), "--property=MemorySwapMax=0",
			"--property=CPUQuota="+strconv.Itoa(settings.ManagementCpuQuotaPercent)+"%", "--property=OOMPolicy=kill")
	}
	args = append(args, "--", launch.Agent.Path, "--config", launch.ConfigPath, executionFlag+launch.RunId)
	return args, nil
}

// Double quoting prevents property parsing from splitting a protected path;
// doubled percent/dollar tokens cannot introduce manager substitutions.
func systemdCommandWord(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$").Replace(value) + "\""
}

// Not-found is distinct from inactive: a missing unit after durable launch is
// uncertain, including after reboot, and can never authorize a replacement run.
func (self *SystemdRunner) Status(ctx context.Context, unit string) (UnitStatus, error) {
	if !validUnitName(unit) {
		return UnitStatus{}, errors.New("invalid fixed systemd unit")
	}
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--no-pager", "--property=LoadState,ActiveState,Result,ExecMainStatus,Job", "--", unit)
	command.Env = controlEnvironment()
	output := &boundedOutput{maximum: self.settings.DiagnosticBytes}
	command.Stdout, command.Stderr = output, &discardWriter{}
	err := command.Run()
	if output.exceeded {
		return UnitStatus{}, errors.New("systemd status exceeded its bound")
	}
	values := map[string]string{}
	for _, line := range strings.Split(output.String(), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	if values["LoadState"] == "not-found" {
		return UnitStatus{}, nil
	}
	if err != nil || values["LoadState"] == "" {
		return UnitStatus{}, errors.New("systemd status could not be authenticated")
	}
	exitCode, err := strconv.Atoi(values["ExecMainStatus"])
	if err != nil {
		return UnitStatus{}, errors.New("systemd exit status is invalid")
	}
	active := values["ActiveState"]
	queued := values["Job"] != "" && values["Job"] != "0"
	return UnitStatus{Exists: true, Active: queued || active != "inactive" && active != "failed", Failed: values["Result"] != "success" || active == "failed", ExitCode: exitCode}, nil
}

// A successful synchronous stop joins the unit before cancellation is reported.
func (self *SystemdRunner) Stop(ctx context.Context, unit string) error {
	if !validUnitName(unit) {
		return errors.New("invalid fixed systemd unit")
	}
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", "stop", "--", unit)
	command.Env = controlEnvironment()
	command.Stdout, command.Stderr = &discardWriter{}, &discardWriter{}
	if err := command.Run(); err != nil {
		return errors.New("systemd cancellation outcome is uncertain")
	}
	return nil
}

// The service identity cannot contain options, paths or unit substitutions.
func validUnitName(unit string) bool {
	const suffix = ".service"
	for _, prefix := range []string{"urnetwork-subeval-", "urnetwork-subeval-docker-"} {
		if strings.HasPrefix(unit, prefix) && strings.HasSuffix(unit, suffix) && digestPattern.MatchString(strings.TrimSuffix(strings.TrimPrefix(unit, prefix), suffix)) {
			return true
		}
	}
	return false
}

// No ambient database, vault or session credentials cross an exec boundary.
func controlEnvironment() []string {
	return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
}

// Diagnostics remain bounded even if a trusted host program is noisy.
type boundedOutput struct {
	content  bytes.Buffer
	maximum  int64
	exceeded bool
}

// Retains at most the declared bound while continuing to drain the child pipe.
func (self *boundedOutput) Write(content []byte) (int, error) {
	remaining := self.maximum - int64(self.content.Len())
	if remaining < int64(len(content)) {
		self.exceeded = true
		if remaining > 0 {
			_, _ = self.content.Write(content[:int(remaining)])
		}
		return len(content), nil
	}
	return self.content.Write(content)
}

// No embedded ReaderFrom can bypass the diagnostic write bound.
func (self *boundedOutput) Bytes() []byte { return self.content.Bytes() }

// Status parsing reads only the bounded retained diagnostic snapshot.
func (self *boundedOutput) String() string { return self.content.String() }

// Ignored child output is drained without retaining data or exposing secrets.
type discardWriter struct{}

// A full write preserves the subprocess pipe contract without allocations.
func (self *discardWriter) Write(content []byte) (int, error) { return len(content), nil }

// Converts a run id into its single deterministic service name.
func unitName(runId string) string { return fmt.Sprintf("urnetwork-subeval-%s.service", runId) }
