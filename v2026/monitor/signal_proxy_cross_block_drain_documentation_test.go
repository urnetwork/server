// Proxy rollout documentation tests retain incident discriminators that are
// not yet emitted by a standalone monitor probe.
package monitor

import (
	"os"
	"strings"
	"testing"
)

// Keeps the cross-block signal, controller fix, and recovery proof together.
func TestProxyCrossBlockDrainDocumentationContract(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	start := strings.Index(catalog, "### 14.2 ")
	if start < 0 {
		t.Fatal("SIGNALS.md is missing §14.2")
	}
	section := catalog[start:]
	end := strings.Index(section, "\n### 14.3 ")
	if end < 0 {
		t.Fatal("SIGNALS.md is missing the end of §14.2")
	}
	section = strings.Join(strings.Fields(section[:end]), " ")

	for _, required := range []string{
		"**Cross-block drain from a Docker name-prefix collision (2026-09-03):**",
		"`name=main-proxy-g1-*`",
		"docker ps --filter 'name=^/main-proxy-g1-'",
		"`name=^/` plus `regexp.QuoteMeta(prefix)`",
		"`Found overlapping containers`",
		"`docker update --restart=no` followed by `docker container stop -t 3600`",
		"listeners are not bound to `127.0.0.1`",
		"rebuilding only the Proxy image cannot repair the host controller",
		"already-running workers retain the old executable",
		"restart at least each host's g1 worker",
		"g10 container ID, process ID, `/status`, and DNAT target",
		"all four must remain unchanged",
		"TestContainerNamePrefixFilterSeparatesG1FromG10",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("cross-block drain runbook missing %q", required)
		}
	}
}
