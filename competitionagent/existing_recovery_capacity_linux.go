//go:build linux

// The opt-in companion and singleton observer partition one unchanged recovery
// aggregate by their future hard ceilings, never by observed resident usage.
package competitionagent

import (
	"errors"
	"strconv"
)

// This identifies a protected resource/role contract, not another reserve.
const existingRecoveryCapacityProfile = "urnetwork-existing-recovery-capacity/v1"

// Ordinary recovery retains its historical full cap. Only the explicit new
// companion contract deducts the fixed observer reservation once.
func recoveryServiceLimits(config Config, role string) (int64, int64, error) {
	aggregate := DefaultManagementBoundarySettings()
	if config.ExistingRecoveryProfile == "" {
		return aggregate.CleanupMemoryBytes, aggregate.CleanupTasksMaximum, nil
	}
	if config.ExistingRecoveryProfile != existingRecoveryCapacityProfile || role != "--recover-active" {
		return 0, 0, errors.New("unknown or foreign companion recovery capacity profile")
	}
	observer := defaultExistingControlSettings()
	if observer.MaximumControls != 1 || observer.MemoryBytes <= 0 || observer.MemoryBytes >= aggregate.CleanupMemoryBytes ||
		observer.TasksMaximum <= 0 || observer.TasksMaximum >= aggregate.CleanupTasksMaximum {
		return 0, 0, errors.New("observer reservation cannot fit the unchanged recovery aggregate")
	}
	return aggregate.CleanupMemoryBytes - observer.MemoryBytes, aggregate.CleanupTasksMaximum - observer.TasksMaximum, nil
}

// The native closure receives the profile only after exact config authentication.
// A stale caller setting cannot turn a legacy config into a qualified companion.
func recoverySettingsForConfig(config Config, settings AgentSettings) (AgentSettings, error) {
	memory, tasks, err := recoveryServiceLimits(config, "--recover-active")
	if err != nil || settings.RecoveryMemoryHeadroomBytes <= 0 || settings.RecoveryMemoryHeadroomBytes > memory ||
		settings.RecoveryTasksHeadroom <= 0 || settings.RecoveryTasksHeadroom > tasks {
		return AgentSettings{}, errors.New("recovery margin does not fit its protected service partition")
	}
	settings.existingRecoveryProfile = config.ExistingRecoveryProfile
	return settings, nil
}

// Both actual native generations must retain the exact hard resource contract.
// Current usage remains a separate validity check, never the capacity proof.
func validateRecoveryChildLimits(values map[string]string, policy ManagementResourcePolicy, settings AgentSettings, memory, tasks int64) error {
	if len(values) != 11 || memory <= 0 || tasks <= 0 {
		return errors.New("recovery child native caps are incomplete")
	}
	for key, want := range map[string]string{
		"memory.max": strconv.FormatInt(memory, 10), "memory.high": "max", "memory.swap.max": "0",
		"pids.max": strconv.FormatInt(tasks, 10), "cgroup.type": "domain", "memory.oom.group": "1",
		"cpu.max": strconv.FormatInt(int64(settings.ManagementCpuQuotaPercent)*settings.ContainmentCpuPeriodMicros/100, 10) + " " + strconv.FormatInt(settings.ContainmentCpuPeriodMicros, 10),
	} {
		if values[key] != want {
			return errors.New("recovery child hard ceiling differs from its exact partition")
		}
	}
	for _, key := range []string{"cpuset.cpus", "cpuset.cpus.effective"} {
		if validateRuntimeBoundaryManagementCpuSet(values[key], policy.ManagementCpuSet) != nil {
			return errors.New("recovery child escaped management processors")
		}
	}
	for key, maximum := range map[string]int64{"memory.current": memory, "pids.current": tasks} {
		value, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || value < 0 || value > maximum || strconv.FormatInt(value, 10) != values[key] {
			return errors.New("recovery child usage is unknown or outside its hard ceiling")
		}
	}
	return nil
}
