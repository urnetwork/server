// Check exact computed-class acknowledgment coverage and the non-alert metadata boundary.
package monitor

import "testing"

// Missing current sites and stale old aliases remain failures; the exact renamed producer succeeds.
func TestAlertClassAcknowledgmentsRejectMissingAndStale(t *testing.T) {
	computed := map[string][]string{"synthetic.go legacyVisibilityAlert": {"providerFailure.monitorVisibilityClass()"}}
	cases := []struct {
		name         string
		acknowledged map[string][]string
		missing      int
		stale        int
	}{
		{name: "missing", acknowledged: map[string][]string{}, missing: 1},
		{name: "stale alias", acknowledged: map[string][]string{"synthetic.go visibilityAlert": {"provider-api"}}, missing: 1, stale: 1},
		{name: "exact computed alias", acknowledged: map[string][]string{"synthetic.go legacyVisibilityAlert": {"provider-api", "provider-authentication", "provider-data-invalid"}}},
	}
	for _, c := range cases {
		missing, stale := 0, 0
		for _, issue := range alertClassAcknowledgmentIssues(computed, c.acknowledged) {
			if issue.stale {
				stale++
			} else {
				missing++
			}
		}
		if missing != c.missing || stale != c.stale {
			t.Errorf("%s: missing=%d stale=%d; want %d/%d", c.name, missing, stale, c.missing, c.stale)
		}
	}
}

// Excluding one observation carrier must not hide actual alert/finding classes in the same function.
func TestAlertClassHarvesterSeparatesPreparedObservationMetadata(t *testing.T) {
	harvest := harvestAlertClasses(t, `package monitor
func prepared() {
 _ = mimirBalancePreparedDiagnostic{class: mimirBalancePrepareClass(classifyObservationError(err))}
 _ = finding{class: "synthetic-incident"}
 _ = Alert{Class: chooseAlertClass()}
}`)
	values := harvest.siteComputedValues["synthetic-0.go prepared"]
	if len(values) != 1 || values[0] != "chooseAlertClass()" {
		t.Fatalf("computed alert values = %v; want only the actual Alert expression", values)
	}
	if len(harvest.classPositions["synthetic-incident"]) != 1 || len(harvest.markedSites) != 1 {
		t.Fatal("non-alert metadata exception hid an actual finding or Alert")
	}
	if len(alertClassAcknowledgmentIssues(harvest.siteComputedValues, map[string][]string{})) != 1 {
		t.Fatal("actual computed Alert no longer requires an acknowledgment")
	}
}

// Parameter forwarding still attributes an unresolved expression to its real caller, not the wrapper.
func TestAlertClassHarvesterTracksComputedHelperAliases(t *testing.T) {
	harvest := harvestAlertClasses(t, `package monitor
func providerFailureAlert(class string) Alert { return Alert{Class: class} }
func legacyVisibilityAlert() Alert { return providerFailureAlert(providerFailure.monitorVisibilityClass()) }
func visibilityAlert() Alert { return legacyVisibilityAlert() }
`)
	site := "synthetic-0.go legacyVisibilityAlert"
	values := harvest.siteComputedValues[site]
	if len(values) != 1 || values[0] != "providerFailure.monitorVisibilityClass()" {
		t.Fatalf("computed helper values = %v; want the actual legacy caller", values)
	}
	if _, ok := harvest.siteComputedValues["synthetic-0.go visibilityAlert"]; ok {
		t.Fatal("delegating wrapper acquired a computed-class acknowledgment")
	}
	acknowledged := map[string][]string{site: {"provider-api", "provider-authentication", "provider-data-invalid"}}
	if issues := alertClassAcknowledgmentIssues(harvest.siteComputedValues, acknowledged); len(issues) != 0 {
		t.Fatalf("correct caller acknowledgment rejected: %v", issues)
	}
}
