//go:build linux

// Original-unit contract controls use synthetic PID1 projections only. The
// actual observer executable cannot stand in for the binary which launched it.
package competitionagent

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The projection mirrors only the immutable, already-issued native class.
func existingRunTestProjection(config Config, identity Identity, plan cleanupPlan, settings AgentSettings) map[string]string {
	values := map[string]string{}
	for _, key := range strings.Split(existingRunUnitProperties, ",") {
		values[key] = ""
	}
	unit := unitName(identity.RunId)
	for key, value := range map[string]string{"Id": unit, "LoadState": "loaded", "ActiveState": "active", "SubState": "running", "FragmentPath": "/run/systemd/transient/" + unit,
		"Slice": managementPrivateSliceName, "ControlGroup": managementPrivateCgroupPath + "/" + unit, "Type": "exec", "Restart": "no", "KillMode": "control-group", "MainPID": "17",
		"InvocationID": strings.Repeat("e", 32), "ExecMainStartTimestampMonotonic": "100", "MemoryMax": strconv.FormatInt(settings.ManagementMemoryBytes, 10), "MemorySwapMax": "0",
		"CPUQuotaPerSecUSec": fmt.Sprintf("%dus", settings.ManagementCpuQuotaPercent*10000), "CPUQuotaPeriodUSec": fmt.Sprintf("%dus", settings.ContainmentCpuPeriodMicros),
		"AllowedCPUs": strings.ReplaceAll(plan.ManagementCpuSet, ",", " "), "CPUAffinity": strings.ReplaceAll(plan.ManagementCpuSet, ",", " "),
		"RuntimeMaxUSec": fmt.Sprintf("%dus", identity.DeadlineAt.Sub(identity.StartedAt).Microseconds()), "TimeoutStopUSec": fmt.Sprintf("%dus", (settings.CancelGrace + settings.CleanupTimeout).Microseconds()),
		"BindsTo": dockerOwnerUnitName(identity.RunId), "OnFailure": managementRecoveryServiceName, "OnFailureJobMode": "replace", "LimitCORE": "0", "UMask": "0077",
		"ExecStart":    fmt.Sprintf("{ path=%s ; argv[]=%s --config %s --execute=%s ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=17 ; code=(null) ; status=0 }", config.Agent.Path, config.Agent.Path, config.configPath, identity.RunId),
		"ExecStopPost": fmt.Sprintf("{ path=%s ; argv[]=%s --config %s --cleanup=%s ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0 }", config.Agent.Path, config.Agent.Path, config.configPath, identity.RunId)} {
		values[key] = value
	}
	return values
}

// The exact already-launched original role is accepted; changed generation,
// executable/config, source-derived hard caps or original budget fail closed.
func TestExistingNativeUnitRequiresOriginalBinaryAndExactHardCaps(t *testing.T) {
	fixture, observer, _, _, _ := newExistingControlTest(t)
	_, _, plan, err := fixture.agent.authenticatedOwnedRun(fixture.manifest.Identity.RunId)
	if err != nil {
		t.Fatal(err)
	}
	valid := existingRunTestProjection(*fixture.config, fixture.manifest.Identity, *plan, fixture.settings)
	if err := validateExistingRunUnit(valid, *fixture.config, fixture.manifest.Identity, *plan, fixture.settings, false); err != nil {
		t.Fatal("exact original unit was rejected", err)
	}
	for _, mutate := range []func(map[string]string){
		func(values map[string]string) {
			values["ExecStart"] = strings.ReplaceAll(values["ExecStart"], fixture.config.Agent.Path, observer.existingControl.Agent.Path)
		},
		func(values map[string]string) {
			values["ExecStart"] = strings.ReplaceAll(values["ExecStart"], fixture.config.configPath, observer.existingControl.configPath)
		},
		func(values map[string]string) { values["MemoryMax"] = "max" },
		func(values map[string]string) { values["MemorySwapMax"] = "1" },
		func(values map[string]string) { values["AllowedCPUs"] = "0-4095" },
		func(values map[string]string) { values["CPUQuotaPerSecUSec"] = "3s" },
		func(values map[string]string) {
			values["RuntimeMaxUSec"] = fmt.Sprintf("%dus", (fixture.manifest.Identity.DeadlineAt.Sub(fixture.manifest.Identity.StartedAt) + time.Second).Microseconds())
		},
		func(values map[string]string) { values["InvocationID"] = "" },
		func(values map[string]string) { values["MainPID"] = "0" },
		func(values map[string]string) { values["BindsTo"] = dockerOwnerUnitName(strings.Repeat("f", 64)) },
	} {
		values := maps.Clone(valid)
		mutate(values)
		if validateExistingRunUnit(values, *fixture.config, fixture.manifest.Identity, *plan, fixture.settings, false) == nil {
			t.Fatal("changed native original identity/containment acquired attachment proof")
		}
	}
}
