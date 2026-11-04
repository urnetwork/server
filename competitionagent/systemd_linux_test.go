// Systemd argument construction is pure: no test contacts the host manager.
package competitionagent

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Runtime ownership and post-stop ownership have separate bounded clocks. The
// absolute deadline shrinks on reconnect rather than renewing three hours.
func TestSystemdLaunchPinsPostStopCleanupAndOriginalDeadline(t *testing.T) {
	now := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	settings := DefaultAgentSettings()
	settings.Now = func() time.Time { return now }
	runId := strings.Repeat("a", 64)
	launch := Launch{UnitName: unitName(runId), RunId: runId, ConfigPath: "/synthetic-release/agent config.json", BaseDirectory: "/synthetic-runtime",
		Agent: Pin{Path: "/synthetic-release/agent"}, DeadlineAt: now.Add(6 * time.Minute)}
	args, err := systemdLaunchArguments(launch, settings)
	if err != nil || !slices.Contains(args, "--property=ExecStopPost=\"/synthetic-release/agent\" \"--config\" \"/synthetic-release/agent config.json\" \"--cleanup="+runId+"\"") ||
		!slices.Contains(args, "--property=RuntimeMaxSec=360.000000s") || !slices.Contains(args, "--property=TimeoutStopSec=130.000000s") ||
		!slices.Contains(args, "--property=KillMode=control-group") || !slices.Contains(args, "--property=Restart=no") {
		t.Fatal("systemd does not retain fixed post-stop cleanup and original budget", args, err)
	}
	now = now.Add(3 * time.Minute)
	args, err = systemdLaunchArguments(launch, settings)
	if err != nil || !slices.Contains(args, "--property=RuntimeMaxSec=180.000000s") {
		t.Fatal("reconnect reset its original absolute systemd runtime", args, err)
	}
}

// Manager expansion syntax must not turn a root-configured path into another
// binary/config. Unit and cleanup identities must be the same exact stable run.
func TestSystemdLaunchRejectsSubstitutionsAndMismatchedUnit(t *testing.T) {
	settings := DefaultAgentSettings()
	runId := strings.Repeat("a", 64)
	base := Launch{UnitName: unitName(runId), RunId: runId, ConfigPath: "/synthetic-release/agent.json", BaseDirectory: "/synthetic-runtime",
		Agent: Pin{Path: "/synthetic-release/agent"}, DeadlineAt: time.Now().Add(time.Minute)}
	for _, value := range []string{"/synthetic/%n.json", "/synthetic/$CONFIG.json", "/synthetic/line\nbreak", "relative", "/"} {
		launch := base
		launch.ConfigPath = value
		if _, err := systemdLaunchArguments(launch, settings); err == nil {
			t.Fatal("systemd accepted path substitution or expanded authority", value)
		}
	}
	base.UnitName = unitName(strings.Repeat("b", 64))
	if _, err := systemdLaunchArguments(base, settings); err == nil {
		t.Fatal("post-stop command targeted a different unit identity")
	}
}

// The evaluator follows its independently managed owner, never the reverse.
// Owner stop freezes directly instead of entering a mutual cleanup/unit join.
func TestSystemdDockerOwnerAndEvaluatorHaveIndependentBoundedRoles(t *testing.T) {
	now := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	settings := DefaultAgentSettings()
	settings.Now = func() time.Time { return now }
	runId := strings.Repeat("a", 64)
	launch := Launch{UnitName: dockerOwnerUnitName(runId), Role: "docker-owner", RunId: runId, ConfigPath: "/synthetic-release/agent.json", BaseDirectory: "/synthetic-runtime",
		Agent: Pin{Path: "/synthetic-release/agent"}, DeadlineAt: now.Add(8 * time.Minute), ManagementCpuSet: "6,8"}
	args, err := systemdLaunchArguments(launch, settings)
	if err != nil || !slices.Contains(args, "--docker-owner="+runId) ||
		!slices.Contains(args, "--property=ExecStopPost=\"/synthetic-release/agent\" \"--config\" \"/synthetic-release/agent.json\" \"--freeze="+runId+"\"") ||
		!slices.Contains(args, "--property=RuntimeMaxSec=480.000000s") || !slices.Contains(args, "--property=AllowedCPUs=6 8") ||
		!slices.Contains(args, "--property=CPUAffinity=6 8") || !slices.Contains(args, "--property=MemoryMax=8589934592") ||
		!slices.Contains(args, "--property=MemorySwapMax=0") || !slices.Contains(args, "--property=CPUQuota=200%") || !slices.Contains(args, "--property=OOMPolicy=kill") ||
		!slices.Contains(args, "--property=LimitCORE=0") {
		t.Fatal("independent Docker owner lost its bounded emergency role", args, err)
	}
	for _, arg := range args {
		if strings.Contains(arg, "BindsTo=") || strings.Contains(arg, "--cleanup=") {
			t.Fatal("Docker owner depends on a unit which joins its post-stop callback")
		}
	}
	launch.Role, launch.UnitName, launch.OwnerUnitName = "", unitName(runId), dockerOwnerUnitName(runId)
	launch.DeadlineAt = now.Add(6 * time.Minute)
	args, err = systemdLaunchArguments(launch, settings)
	if err != nil || !slices.Contains(args, "--execute="+runId) || !slices.Contains(args, "--property=BindsTo="+dockerOwnerUnitName(runId)) ||
		!slices.Contains(args, "--property=After="+dockerOwnerUnitName(runId)) || !slices.Contains(args, "--property=RuntimeMaxSec=360.000000s") {
		t.Fatal("evaluator escaped its exact owner or original job clock", args, err)
	}
	launch.Role, launch.UnitName = "docker-owner", dockerOwnerUnitName(runId)
	if _, err := systemdLaunchArguments(launch, settings); err == nil {
		t.Fatal("Docker owner accepted a reverse dependency")
	}
}
