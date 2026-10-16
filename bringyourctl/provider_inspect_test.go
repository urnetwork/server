package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// `provider inspect` prints the minimums the rules read: any accepted URL run
// within the evidence age is evidence, and Quality and Speed read the ratio
// from the native URL-run minimums (model/egress_native_min_runs.go). The
// legacy min_scored_loads setting decides nothing, whatever provider.yml
// holds (26 on main).
func TestProviderInspectIndexLinesPrintTheNativeMinimums(t *testing.T) {
	settings := model.DefaultEgressIndexSettings()
	settings.MinScoredLoads = 26
	lines := providerInspectIndexLines(&model.ProviderEgressInspection{
		CurrentIndex: model.EgressIndex{Evidence: true, Quality: true},
		Settings:     settings,
	})
	if strings.Contains(lines, "26") {
		t.Fatalf("the inspection prints the legacy min_scored_loads: %q", lines)
	}
	for _, want := range []string{
		fmt.Sprintf("evidence is any accepted URL run within %s", min(settings.EvidenceMaxAge, model.ProviderEgressHealthMaxAge)),
		fmt.Sprintf("quality %d or more runs spanning %s or more", model.ProviderQualityMinMeasuredUrlRuns, model.ProviderQualityMinMeasuredUrlSpan),
		fmt.Sprintf("speed %d or more runs", model.ProviderSpeedMinMeasuredUrlRuns),
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("the inspection does not print %q: %q", want, lines)
		}
	}
}

// It says whether the provider's runs meet each minimum, which tells a
// provider warming up from one with no evidence under the unprobed reason.
func TestProviderInspectIndexLinesPrintWhetherTheMinimumsAreMet(t *testing.T) {
	inspection := &model.ProviderEgressInspection{
		CurrentIndex:    model.EgressIndex{Evidence: true, Quality: true},
		Settings:        model.DefaultEgressIndexSettings(),
		SpeedUrlRunsMet: true,
	}
	if lines := providerInspectIndexLines(inspection); !strings.Contains(lines, "or more, met false; speed") || !strings.HasSuffix(lines, "or more runs, met true\n") {
		t.Fatalf("Quality short and Speed met: %q", lines)
	}
}
