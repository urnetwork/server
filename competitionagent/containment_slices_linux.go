// Fixed slices are prepared before daemon mutation and retained frozen across
// unknown outcomes. A late daemon start cannot escape an already-frozen parent.
package competitionagent

import (
	"context"
	"errors"
	"strings"
	"time"
)

// The kernel's frozen acknowledgment is distinct from an empty process census.
type runSliceCgroups interface {
	cleanupCgroups
	Freeze(context.Context, string) error
	Frozen(context.Context, string) (bool, error)
	SetLimits(context.Context, string, runSliceLimits) error
	VerifyLimits(context.Context, string, runSliceLimits) error
}

// Private seams keep preparation/freezing tests away from systemd and cgroups.
// Only immutable authenticated cleanup plans may supply its finite unit list.
type hostRunSlices struct {
	config   Config
	settings AgentSettings
	cgroups  runSliceCgroups
	command  func(context.Context, string, []string) error
	retired  func(context.Context, string) (bool, error)
}

func newHostRunSlices(config Config, settings AgentSettings) *hostRunSlices {
	self := &hostRunSlices{config: config, settings: settings,
		cgroups: &hostCleanupCgroups{rootPath: "/sys/fs/cgroup", maximumBytes: settings.DiagnosticBytes, requireCgroupFs: true,
			limitCommandTimeout: settings.CleanupCommandTimeout, managerBoundary: config.DockerRole.ResourceBoundary, managerMaximumBytes: settings.DiagnosticBytes}}
	self.command = self.systemctl
	self.retired = self.systemdRetired
	return self
}

// Starting only the exact empty slices commits kernel containment before any
// proxy owner or evaluator may issue a mutating Docker operation. Existing
// populated/frozen parents are refused, never silently reused or thawed.
func (self *hostRunSlices) Prepare(parent context.Context, plan cleanupPlan) error {
	if err := self.validate(plan); err != nil {
		return err
	}
	limits, err := prepareRunSliceLimits(plan, self.config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupTimeout)
	defer cancel()
	if err := self.command(ctx, "start", plan.CgroupNames); err != nil {
		return err
	}
	// No limit is written until every parent has passed the complete census.
	if err := self.verifyPreparedEmpty(ctx, plan); err != nil {
		return err
	}
	for index, name := range plan.CgroupNames {
		if err := self.cgroups.SetLimits(ctx, name, limits[index]); err != nil {
			return err
		}
	}
	for index, name := range plan.CgroupNames {
		if err := self.cgroups.VerifyLimits(ctx, name, limits[index]); err != nil {
			return err
		}
	}
	if err := self.verifyPreparedEmpty(ctx, plan); err != nil {
		return err
	}
	return ctx.Err()
}

// The second census refuses population or freeze changes during limit writes.
// Cleanup deliberately does not depend on healthy admission-time limits.
func (self *hostRunSlices) verifyPreparedEmpty(ctx context.Context, plan cleanupPlan) error {
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

// The caller has already committed its current-run reap proof and closed the
// daemon's mutation authority. Missing parents are acceptable only at this
// retirement stage; preparation/freezing still require positive existence.
func (self *hostRunSlices) Release(parent context.Context, plan cleanupPlan) error {
	if err := self.validate(plan); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupTimeout)
	defer cancel()
	if self.retired == nil {
		return errors.New("slice retirement has no native unit acknowledgment owner")
	}
	stopErr := self.command(ctx, "stop", plan.CgroupNames)
	for _, name := range plan.CgroupNames {
		if err := self.joinCgroupReleased(ctx, name); err != nil {
			return errors.Join(stopErr, err)
		}
	}
	return ctx.Err()
}

// An interrupted retirement may already have removed some exact parents. No
// prefix scanning, thaw or replacement parent is permitted on its retry.
func (self *hostRunSlices) joinCgroupReleased(ctx context.Context, name string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		retired, err := self.retired(ctx, name)
		if err != nil {
			return err
		}
		_, frozenErr := self.cgroups.Frozen(ctx, name)
		if frozenErr != nil && !errors.Is(frozenErr, errCleanupCgroupAbsent) {
			return frozenErr
		}
		if retired && errors.Is(frozenErr, errCleanupCgroupAbsent) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.CleanupPollInterval):
		}
	}
}

// A synchronous stop's error is not positive retirement. Independently check
// both manager inactivity/no queued job and kernel parent absence before a
// later run can reuse the same exact content-derived build parent name.
func (self *hostRunSlices) systemdRetired(parent context.Context, name string) (bool, error) {
	if _, err := cleanupSlicePath(name); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupCommandTimeout)
	defer cancel()
	output, runErr := self.managerCommand(ctx, []string{"show", "--no-pager", "--property=LoadState,ActiveState,Job", "--", name})
	if ctx.Err() != nil {
		return false, errors.New("slice retirement manager observation is unavailable")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			if _, exists := values[key]; exists {
				return false, errors.New("slice retirement manager observation is ambiguous")
			}
			values[key] = value
		}
	}
	if values["Job"] != "" && values["Job"] != "0" {
		return false, nil
	}
	if values["LoadState"] == "not-found" {
		return true, nil
	}
	if runErr != nil || values["LoadState"] == "" || values["ActiveState"] == "" {
		return false, errors.New("slice retirement manager observation was not acknowledged")
	}
	return values["ActiveState"] == "inactive" || values["ActiveState"] == "failed", nil
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
	if operation != "start" && operation != "stop" || len(names) == 0 || len(names) > self.settings.CleanupObjectCount {
		return errors.New("invalid fixed containment unit operation")
	}
	for _, name := range names {
		if _, err := cleanupSlicePath(name); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.CleanupCommandTimeout)
	defer cancel()
	args := append([]string{operation, "--"}, names...)
	if _, err := self.managerCommand(ctx, args); err != nil {
		return errors.New("exact containment slice preparation was not acknowledged")
	}
	return ctx.Err()
}
