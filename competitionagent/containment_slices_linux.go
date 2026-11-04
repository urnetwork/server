// Fixed slices are prepared before daemon mutation and retained frozen across
// unknown outcomes. A late daemon start cannot escape an already-frozen parent.
package competitionagent

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// The kernel's frozen acknowledgment is distinct from an empty process census.
type runSliceCgroups interface {
	cleanupCgroups
	Freeze(context.Context, string) error
	Frozen(context.Context, string) (bool, error)
}

// Private seams keep preparation/freezing tests away from systemd and cgroups.
// Only immutable authenticated cleanup plans may supply its finite unit list.
type hostRunSlices struct {
	config   Config
	settings AgentSettings
	cgroups  runSliceCgroups
	command  func(context.Context, string, []string) error
}

func newHostRunSlices(config Config, settings AgentSettings) *hostRunSlices {
	self := &hostRunSlices{config: config, settings: settings,
		cgroups: &hostCleanupCgroups{rootPath: "/sys/fs/cgroup", maximumBytes: settings.DiagnosticBytes, requireCgroupFs: true}}
	self.command = self.systemctl
	return self
}

// Starting only the exact empty slices commits kernel containment before any
// proxy owner or evaluator may issue a mutating Docker operation. Existing
// populated/frozen parents are refused, never silently reused or thawed.
func (self *hostRunSlices) Prepare(parent context.Context, plan cleanupPlan) error {
	if err := self.validate(plan); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupTimeout)
	defer cancel()
	if err := self.command(ctx, "start", plan.CgroupNames); err != nil {
		return err
	}
	for _, name := range plan.CgroupNames {
		frozen, err := self.cgroups.Frozen(ctx, name)
		if err != nil || frozen {
			return errors.New("prepared containment parent is missing or unexpectedly frozen")
		}
		empty, err := self.cgroups.Empty(ctx, name)
		if err != nil || !empty {
			return errors.New("prepared containment parent is not positively empty")
		}
	}
	return ctx.Err()
}

// Every exact parent receives freeze and kill before waiting on any child.
// A failed first target cannot prevent attempts to stop the other owned CPU
// trees. Any unknown acknowledgment still returns failure and retains custody.
func (self *hostRunSlices) FreezeAndReap(parent context.Context, plan cleanupPlan) error {
	if err := self.validate(plan); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupTimeout)
	defer cancel()
	var failures []error
	for _, name := range plan.CgroupNames {
		if err := self.cgroups.Freeze(ctx, name); err != nil {
			failures = append(failures, err)
		}
	}
	for _, name := range plan.CgroupNames {
		if err := self.cgroups.Kill(ctx, name); err != nil {
			failures = append(failures, err)
		}
	}
	for _, name := range plan.CgroupNames {
		if err := self.joinFrozenEmpty(ctx, name); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(append(failures, ctx.Err())...)
}

// Quiescence is deliberately not inferred here from emptiness. The caller
// must retain frozen parents until the durable Docker owner has independently
// acknowledged no outstanding mutations and the exact resource reap is done.
// This helper only verifies the kernel-side frozen-and-empty half of that proof.
func (self *hostRunSlices) VerifyRetained(parent context.Context, plan cleanupPlan) error {
	if err := self.validate(plan); err != nil {
		return err
	}
	for _, name := range plan.CgroupNames {
		frozen, err := self.cgroups.Frozen(parent, name)
		if err != nil || !frozen {
			return errors.New("containment parent lost its frozen acknowledgment")
		}
		empty, err := self.cgroups.Empty(parent, name)
		if err != nil || !empty {
			return errors.New("frozen containment parent still has owned processes")
		}
	}
	return parent.Err()
}

func (self *hostRunSlices) joinFrozenEmpty(ctx context.Context, name string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frozen, freezeErr := self.cgroups.Frozen(ctx, name)
		empty, emptyErr := self.cgroups.Empty(ctx, name)
		if freezeErr != nil || emptyErr != nil {
			return errors.Join(freezeErr, emptyErr)
		}
		if frozen && empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.CleanupPollInterval):
		}
	}
}

func (self *hostRunSlices) validate(plan cleanupPlan) error {
	if self.command == nil || self.cgroups == nil || self.settings.CleanupTimeout <= 0 || self.settings.CleanupCommandTimeout <= 0 ||
		self.settings.CleanupPollInterval <= 0 || len(plan.CgroupNames) > self.settings.CleanupObjectCount {
		return errors.New("invalid finite slice containment owner or bounds")
	}
	return validateCleanupPlan(plan, self.config)
}

// There is no caller-selected program, endpoint, unit property or shell. The
// fixed unit names were validated as the complete authenticated run inventory.
func (self *hostRunSlices) systemctl(parent context.Context, operation string, names []string) error {
	if operation != "start" || len(names) == 0 || len(names) > self.settings.CleanupObjectCount {
		return errors.New("invalid fixed containment unit operation")
	}
	if err := protectedRootPath("/usr/bin/systemctl", false, false); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := cleanupSlicePath(name); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupCommandTimeout)
	defer cancel()
	args := append([]string{"start", "--"}, names...)
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", args...)
	command.Env = controlEnvironment()
	command.Stdout, command.Stderr = &discardWriter{}, &discardWriter{}
	if err := command.Run(); err != nil {
		return errors.New("exact containment slice preparation was not acknowledged")
	}
	return ctx.Err()
}
