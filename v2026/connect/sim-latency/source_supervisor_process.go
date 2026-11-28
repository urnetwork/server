// Trusted worker cancellation preserves its complete management-control join.
// Ordinary automation programs retain their independent, shorter join allowance.
package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026/competitionagent"
)

// Values are immutable after construction and private to one supervisor owner.
type sourceSupervisorProcessSettings struct {
	// Positive post-cancel process/pipe join allowance for non-worker programs.
	ProgramJoinTimeout time.Duration
	// Positive control_join_timeout of the pinned worker role, at least native join.
	WorkerControlJoinTimeout time.Duration
	// Positive time for worker result/report handoff after its control join.
	WorkerHandoffTimeout time.Duration
}

// Worker defaults derive from the transport owner, not a duplicate literal.
func defaultSourceSupervisorProcessSettings() sourceSupervisorProcessSettings {
	management := competitionagent.DefaultManagementSshSettings()
	return sourceSupervisorProcessSettings{ProgramJoinTimeout: 30 * time.Second,
		WorkerControlJoinTimeout: management.JoinTimeout + management.Agent.CancelGrace,
		WorkerHandoffTimeout:     management.Agent.CancelGrace}
}

// Explicit overrides must match the deployment's pinned role; malformed or
// too-short values refuse instead of silently reverting to the default.
func sourceSupervisorProcessSettingsFromOptions(opts docopt.Opts) (sourceSupervisorProcessSettings, error) {
	settings := defaultSourceSupervisorProcessSettings()
	if value := optString(opts, "--worker-control-join-timeout", ""); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return sourceSupervisorProcessSettings{}, errors.New("invalid worker control join duration")
		}
		settings.WorkerControlJoinTimeout = parsed
	}
	if err := settings.validate(); err != nil {
		return sourceSupervisorProcessSettings{}, err
	}
	return settings, nil
}

// No zero/unbounded grace or overflowing worker sum is a valid owner contract.
func (self sourceSupervisorProcessSettings) validate() error {
	if self.ProgramJoinTimeout <= 0 || self.WorkerHandoffTimeout <= 0 ||
		self.WorkerControlJoinTimeout < competitionagent.DefaultManagementSshSettings().JoinTimeout ||
		self.WorkerControlJoinTimeout+self.WorkerHandoffTimeout <= self.WorkerControlJoinTimeout {
		return errors.New("supervisor process grace must preserve the finite worker control join and handoff")
	}
	return nil
}

// This is the actual command constructor used by every trusted launch. Only
// the exact worker protocol selector receives the worker's longer allowance.
func sourceSupervisorProcessCommand(ctx context.Context, path string, arguments []string, settings sourceSupervisorProcessSettings) (*exec.Cmd, error) {
	if err := settings.validate(); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, path, arguments...)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = settings.ProgramJoinTimeout
	if len(arguments) == 1 && arguments[0] == "--supervisor-request" {
		command.WaitDelay = settings.WorkerControlJoinTimeout + settings.WorkerHandoffTimeout
	}
	return command, nil
}
