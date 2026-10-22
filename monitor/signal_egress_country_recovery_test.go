// Country-query absence cannot prove recovery: its having clause returns only
// failing observed cohorts, without the coverage needed to compare later loads.
package monitor

import (
	"context"
	"strings"
	"testing"
)

// Both public rendering and ticket lifecycle consume the same synthetic source.
// No deployment settings, database, metrics server or network is opened.
func egressCountryRecoveryTestTick(t *testing.T, fixture *egressSitePoolFixture) ([]finding, Alerts) {
	t.Helper()
	signal := testEgressSitePoolSignal(testEgressSitePoolContext())
	settings := testEgressSitePoolSettings(fixture.source(t))
	findings, err := signal.(*signalAdapter).probe.check(context.Background(), mustEgressSitePoolProbeEnv(t, settings))
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return findings, alerts
}

// Losing the query's only observed country must keep its mature warning open.
// The same empty result also describes a country below the run minimum, or a
// retired/probationary last loaded site; the source cannot distinguish them.
func TestEgressSitePoolCountryAbsentCohortDoesNotResolveTicket(t *testing.T) {
	emitter := &ticketEscalationEmitter{}
	manager := newTicketManager("synthetic", emitter)
	bad := healthyEgressSitePoolFixture()
	bad.countries = [][]string{{"zz", "1", "1", "1", "69", "0"}}
	for range 2 {
		findings, alerts := egressCountryRecoveryTestTick(t, bad)
		alert := requireAlertClass(t, alerts, "egress-country-unreachable")
		if alert.Severity != SeverityWarn || alert.Frame != "zz" || !strings.Contains(alert.Observed, "sites=1 loads=1 failed=1 runs=69") {
			t.Fatalf("sparse country warning changed: %+v", alert)
		}
		manager.ingest(context.Background(), findings)
	}
	if manager.openCount() != 1 {
		t.Fatal("initial country warning did not mature")
	}
	for range manager.resolveTicks + 1 {
		findings, alerts := egressCountryRecoveryTestTick(t, healthyEgressSitePoolFixture())
		if len(alerts) != 0 {
			t.Fatalf("absent country invented a new detection: %+v", alerts)
		}
		manager.ingest(context.Background(), findings)
	}
	if manager.openCount() != 1 {
		t.Fatal("a disappeared country cohort resolved its warning without comparable measured recovery")
	}
	for _, event := range emitter.events {
		if event.kind == ticketResolve {
			t.Fatal("country-query absence emitted a recovery event")
		}
	}
}

// Withholding country recovery must preserve independent measured pool recovery
// and positive warnings from other returned countries, including capped lists.
func TestEgressSitePoolCountryUnknownPreservesSiblingRecoveryAndWarnings(t *testing.T) {
	manager := newTicketManager("synthetic", nil)
	bad := healthyEgressSitePoolFixture()
	bad.countries = [][]string{{"zz", "1", "1", "1", "69", "0"}}
	bad.destinations = bad.destinations[1:]
	for range 2 {
		findings, alerts := egressCountryRecoveryTestTick(t, bad)
		requireAlertClass(t, alerts, "egress-site-pool-thin")
		requireAlertClass(t, alerts, "egress-country-unreachable")
		manager.ingest(context.Background(), findings)
	}
	if manager.openCount() != 2 {
		t.Fatalf("country and independent pool warnings did not mature: %d", manager.openCount())
	}
	for range manager.resolveTicks + 1 {
		findings, _ := egressCountryRecoveryTestTick(t, healthyEgressSitePoolFixture())
		manager.ingest(context.Background(), findings)
	}
	for _, ticket := range manager.tickets {
		if ticket.open && ticket.class != "egress-country-unreachable" {
			t.Fatalf("independently measured sibling failed to recover: %s", ticket.class)
		}
	}
	if manager.openCount() != 1 {
		t.Fatal("unknown country recovery changed an independent sibling's recovery")
	}
	other := healthyEgressSitePoolFixture()
	other.countries = [][]string{{"zy", "2", "4", "4", "30", "0"}}
	for range manager.resolveTicks + 1 {
		findings, alerts := egressCountryRecoveryTestTick(t, other)
		if alert := requireAlertClass(t, alerts, "egress-country-unreachable"); alert.Frame != "zy" {
			t.Fatalf("another country's positive warning disappeared: %+v", alert)
		}
		manager.ingest(context.Background(), findings)
	}
	if manager.openCount() != 2 {
		t.Fatal("a returned country row resolved the absent country's warning")
	}
}
