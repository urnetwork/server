package monitor

import (
	"os"
	"strings"
	"testing"
)

// The response-only reducer cannot certify reader-error coverage or fleet recovery.
func TestProviderCountScoreReadCoverageGapDocumented(t *testing.T) {
	catalog, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	section := strings.SplitN(string(catalog), "### 2.9a ", 2)
	if len(section) != 2 {
		t.Fatal("provider-count catalog section is absent")
	}
	body := strings.SplitN(section[1], "\n### ", 2)[0]
	for _, want := range []string{
		"urnetwork_client_score_read_errors_total",
		"preinitialized phase children: `counts` and `samples`",
		"counts failed loader pipelines, not failed keys, unique requests or API failures",
		"optional other-rank backfill",
		"primary read error leaves the completed-response denominator",
		"caller cancellation/deadline or client lifecycle",
		"not a Redis-service outage",
		"initial connection/PING and payload decoding",
		"Backend-read coverage gap",
		"not consumed by the current registered `provider-count` probe",
		"exact process/start identity",
		"underlying source timestamps",
		"positive successful-read control",
		"absent, partial, reset or rejected samples remain unknown",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("score-read catalog qualification missing %q", want)
		}
	}
}
