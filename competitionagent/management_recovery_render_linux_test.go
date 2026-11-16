// Inert recovery provisioning uses the exact same owner as its strict loader.
package competitionagent

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// Unquoted paths and integer duration spellings are valid systemd syntax but
// were refused by the closed runtime contract. Never hand-render those fields.
func TestRecoveryServiceRendererMatchesClosedContract(t *testing.T) {
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	settings := DefaultAgentSettings()
	config := Config{Agent: Pin{Path: "/synthetic/bin/agent"}, configPath: "/synthetic/role/agent.json"}
	content, err := RenderRecoveryService(config.Agent.Path, config.configPath, policy, settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRecoveryServiceText(content, config, &policy, settings); err != nil {
		t.Fatal("renderer and admission disagree", err)
	}
	if err := validateRecoveryServiceText(renderRecoveryTestService(t, config, settings, &policy), config, &policy, settings); err != nil {
		t.Fatal("tracked template differs from owner", err)
	}
	for _, mutation := range []string{
		strings.Replace(string(content), systemdCommandWord(config.Agent.Path), config.Agent.Path, 1),
		strings.Replace(string(content), systemdCommandWord(config.configPath), config.configPath, 1),
		strings.Replace(string(content), "TimeoutStopSec="+strconv.FormatFloat(settings.CancelGrace.Seconds(), 'f', 6, 64)+"s", "TimeoutStopSec="+strconv.FormatFloat(settings.CancelGrace.Seconds(), 'f', 0, 64)+"s", 1),
	} {
		if mutation == string(content) {
			t.Fatal("legacy regression mutation did not change a required field")
		}
		if err := validateRecoveryServiceText([]byte(mutation), config, &policy, settings); err == nil {
			t.Fatal("closed contract accepted a legacy hand-rendered spelling")
		}
	}
	second, err := RenderRecoveryService(config.Agent.Path, config.configPath, policy, settings)
	if err != nil || !bytes.Equal(content, second) {
		t.Fatal("rendered unit is not deterministic", err)
	}
}

// The renderer accepts no shell/manager expansions, missing owner budget or
// altered ledger merely because its output is not yet installed.
func TestRecoveryServiceRendererRejectsExpandedOrUnboundedAuthority(t *testing.T) {
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	settings := DefaultAgentSettings()
	for _, path := range []string{"", "relative", "/synthetic/agent --shell", "/synthetic/%n", "/synthetic/$AGENT", "/synthetic/agent\nExecStart=/bin/sh"} {
		if _, err := RenderRecoveryService(path, "/synthetic/agent.json", policy, settings); err == nil {
			t.Fatal("expanded executable rendered", path)
		}
		if _, err := RenderRecoveryService("/synthetic/agent", path, policy, settings); err == nil {
			t.Fatal("expanded configuration rendered", path)
		}
	}
	settings.CancelGrace = 0
	if _, err := RenderRecoveryService("/synthetic/agent", "/synthetic/agent.json", policy, settings); err == nil {
		t.Fatal("unbounded recovery rendered")
	}
	settings = DefaultAgentSettings()
	for _, maximum := range []int64{0, 1} {
		settings.ConfigBytes = maximum
		if _, err := RenderRecoveryService("/synthetic/agent", "/synthetic/agent.json", policy, settings); err == nil {
			t.Fatal("absent or insufficient document bound rendered", maximum)
		}
	}
	settings = DefaultAgentSettings()
	policy.Parent.MemoryBytes++
	if _, err := RenderRecoveryService("/synthetic/agent", "/synthetic/agent.json", policy, settings); err == nil {
		t.Fatal("unapproved ledger rendered")
	}
}
