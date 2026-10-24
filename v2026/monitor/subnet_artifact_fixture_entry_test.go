package monitor

import (
	"context"
	"net/http"
	"testing"
)

// Lifecycle controls must pass the public settings contract and enter their
// injected request source before their cancellation assertions can be causal.
func TestSubnetArtifactsFixturePassesSettingsAndEntersOwnedSource(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		settings, source, artifact, raw := subnetArtifactFixture(t)
		if scoped {
			settings.ExcludedHosts = []string{"pg-1"}
		}
		if err := settings.Validate(); err != nil {
			if source.calls != 0 {
				t.Fatal("settings validation contacted a source")
			}
			t.Fatalf("scoped=%v: fixture settings rejected before source entry: %v", scoped, err)
		}
		parent, cancel := context.WithCancel(context.Background())
		entered := 0
		source.do = func(request *http.Request) (*http.Response, error) {
			if parent.Err() != nil || request.Context().Err() != nil {
				t.Fatal("fixture entered its source without a live owner")
			}
			if _, ok := request.Context().Deadline(); !ok {
				t.Fatal("fixture entered its source without the read-owner deadline")
			}
			entered++
			return subnetArtifactTestResponse(request, raw, artifact.ContentHash), nil
		}
		alerts, err := NewSubnetArtifactsSignal().Run(parent, settings)
		parentErr := parent.Err()
		cancel()
		if err != nil || parentErr != nil || entered != 2 || source.calls != 2 || len(alerts) != 0 {
			t.Fatalf("scoped=%v: valid fixture did not complete both owned source entries", scoped)
		}
	}
}
