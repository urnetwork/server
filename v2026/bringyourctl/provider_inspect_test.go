package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
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

// `provider inspect` prints how this host's provider.yml sets modern
// providers apart (FP2-MODERN.md): the gate's switch, off while absent, and
// the effective rank_modern_first, and never a start, which is retired.
func TestProviderInspectModernSettingsPrintTheSwitchAndPriority(t *testing.T) {
	off, on := false, true
	for _, c := range []struct {
		modernSwitch    *bool
		rankModernFirst bool
		want            string
	}{
		{modernSwitch: nil, rankModernFirst: true, want: "quality_speed_requires_modern_provider absent (off); egress_index.rank_modern_first: true"},
		{modernSwitch: &off, rankModernFirst: true, want: "quality_speed_requires_modern_provider: false; egress_index.rank_modern_first: true"},
		{modernSwitch: &on, rankModernFirst: false, want: "quality_speed_requires_modern_provider: true; egress_index.rank_modern_first: false"},
	} {
		settings := model.DefaultEgressIndexSettings()
		settings.QualitySpeedRequiresModernProvider = c.modernSwitch
		settings.RankModernFirst = c.rankModernFirst
		text := providerInspectModernSettings(settings)
		if text != c.want {
			t.Errorf("modern settings %q, want %q", text, c.want)
		}
		if strings.Contains(text, "start") || strings.Contains(text, "2026-11-11") {
			t.Errorf("modern settings %q name a start", text)
		}
	}
	// the default is priority on with the switch absent
	if text := providerInspectModernSettings(model.DefaultEgressIndexSettings()); text != "quality_speed_requires_modern_provider absent (off); egress_index.rank_modern_first: true" {
		t.Errorf("default modern settings %q", text)
	}
}
