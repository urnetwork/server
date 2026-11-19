//go:build sidecar_localstack

// §22.7's revocation suite's member form on T3 (TEST-M6-01; R-TEST-4, D-TEST-6): sidecartest's suite with the checks
// T2 runs, TEAM's own and the keys and acl checks sidecartest/scenarios composes, each on a T3 Net: gkm's engine on
// the local stack's MinIO and the private Redis, in both ways, an Admin's MemberRevoke and the scim_signer's. The keys
// check runs with evs's gate off, which only a sidecar_testhooks build switches; on a leg without the hooks it reads
// open, as on T2's plain one. The store clock is on, as the keys scenarios' is.

package sidecart3

import (
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
	"github.com/urnetwork/sidecar/sidecartest/scenarios"

	"github.com/urnetwork/server"
)

// T3's settings for the member form: the keys scenarios', with the driver's stall reading off. ACL's schedules build a
// Team on the real stores inside one step, whose actor waits on a pooled connection's reader, a goroutine it did not
// start: the one wait the stall reading cannot tell from a block (sidecartest's barrier_stall.go), which would take the
// baton from a step that is only slow. An actor that is blocked still fails its schedule, at the await's bound.
func revocationMemberSettings() *Settings {
	settings := keysScenarioSettings()
	netSettings := *settings.Net
	barriers := *netSettings.Barriers
	// in real time, only a broken run's bound
	barriers.AwaitTimeout = 60 * time.Second
	barriers.StallTimeout = 0
	netSettings.Barriers = &barriers
	settings.Net = &netSettings
	return settings
}

func TestRevocationSuiteMemberFormOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		report := sidecartest.RunRevocationSuite(t, sidecartest.RevocationFormMember, &sidecartest.RevocationSuiteSettings{
			Net: NetSettings(t, revocationMemberSettings()),
		})
		for _, way := range sidecartest.RevocationFormMember.Ways() {
			results := report.Of(way)
			result := func(checkId string) *sidecartest.RevocationResult {
				index := slices.IndexFunc(results, func(result *sidecartest.RevocationResult) bool { return result.CheckId == checkId })
				if index < 0 {
					t.Fatalf("%s: %s did not run", way, checkId)
				}
				return results[index]
			}
			if team := result(fault.TeamMemberRevocationCheck.Id); team.Err != nil || team.Open != "" {
				t.Errorf("TEAM's check did not judge: %s", team)
			}
			keys := result(scenarios.RevocationMemberKeys.Id)
			switch {
			case !keys.GateOff:
				t.Errorf("the keys check ran with the gate on: %s", keys)
			case sidecartest.HooksLinked && (keys.Err != nil || keys.Open != ""):
				t.Errorf("the keys check did not judge under the hooks: %s", keys)
			case !sidecartest.HooksLinked && keys.Open == "":
				t.Errorf("the keys check did not read open without the hooks: %s", keys)
			}
			if rights := result(scenarios.RevocationMemberRightsDropped.Id); way == sidecartest.RevocationMemberRemove && (rights.Err != nil || rights.Open != "") {
				t.Errorf("ACL's rights check did not judge an Admin's removal: %s", rights)
			}
			if agents := result(scenarios.RevocationMemberTaggedAgents.Id); way == sidecartest.RevocationMemberRemove && (agents.Err != nil || agents.Open != "") {
				t.Errorf("ACL's tagged agents check did not judge an Admin's removal: %s", agents)
			}
		}
	})
}
