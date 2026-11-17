// Cleanup bounds are local operational ownership, never a new signed job clock.
package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/competitionagent"
)

// The default preserves the agent's full healthy manager/client join plus its
// separate scheduling/diagnostic margin; evaluation limits remain unchanged.
func TestCompetitionAgentControlJoinDefaultCoversFullManagedOwner(t *testing.T) {
	settings := defaultCompetitionDistributedCoordinatorSettings()
	agent := competitionagent.DefaultManagementSshSettings()
	if settings.ControlJoinTimeout != agent.JoinTimeout+agent.Agent.CancelGrace || settings.ControlJoinTimeout <= agent.JoinTimeout ||
		settings.Limits.MaximumJobDuration != competitionagent.DefaultLimits().MaximumJobDuration {
		t.Fatal("transport cleanup truncated native ownership or changed a signed job budget")
	}
}

// Existing pinned profiles omit the additive field and retain its default;
// no role/config rewrite is necessary merely to select the corrected owner.
func TestCompetitionAgentControlJoinDefaultSurvivesLegacySettings(t *testing.T) {
	settings := defaultCompetitionDistributedCoordinatorSettings()
	expected := settings.ControlJoinTimeout
	if err := json.Unmarshal([]byte(`{"poll_interval":1000000}`), &settings); err != nil || settings.ControlJoinTimeout != expected {
		t.Fatal("legacy coordinator settings erased the finite native join default")
	}
}

// The same production validator is wired before request signing/pin/process
// issue, so direct Exchange construction cannot bypass decoded-profile checks.
func TestCompetitionAgentControlJoinRejectsTruncatedConfiguredAllowance(t *testing.T) {
	minimum := competitionagent.DefaultManagementSshSettings().JoinTimeout
	for _, allowance := range []time.Duration{0, -time.Second, minimum - time.Nanosecond, minimum, minimum + time.Second} {
		settings := defaultCompetitionDistributedCoordinatorSettings()
		settings.ControlJoinTimeout = allowance
		value, err := settings.controlJoinAllowance()
		if allowance < minimum {
			if err == nil || value != 0 {
				t.Fatal("truncated configured join allowance was accepted")
			}
			transport := competitionCommandAgentTransport{profile: &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: settings}}}
			if _, got := transport.Exchange(t.Context(), competitionagent.Request{Operation: "availability"}, "", ""); got == nil || got.Error() != err.Error() {
				t.Fatal("real exchange bypassed the shared control-join validator")
			}
		} else if err != nil || value != allowance {
			t.Fatal("complete configured native join allowance was truncated")
		}
	}
}
