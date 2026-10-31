package grafana

import (
	"strings"
	"testing"
)

// The reservation panel groups by operation, so the pending reservation
// operations appear as soon as a release emits them and the in-transaction
// ones go flat. Beside them it shows the creation transactions that refused to
// publish a pending reservation, which a rollout must be able to see.
func TestConnectDashboardShowsPendingReservationsAndRefusals(t *testing.T) {
	panel := dashboardPanelById(readTestDashboard(t, "connect.json"), 48)
	if panel == nil || len(panel.Targets) != 2 {
		t.Fatal("the reservation panel must show reservations and publication refusals")
	}
	reservations, refusals := panel.Targets[0].Expr, panel.Targets[1].Expr
	if !strings.Contains(reservations, "sum by (service, operation, result)") || !strings.Contains(reservations, "urnetwork_redis_contract_reservation_total") {
		t.Errorf("reservation query does not group by operation: %s", reservations)
	}
	if !strings.Contains(refusals, "sum by (service, reason)") || !strings.Contains(refusals, "urnetwork_redis_contract_publication_refusals_total") {
		t.Errorf("publication refusal query is missing: %s", refusals)
	}
	for _, query := range []string{reservations, refusals} {
		if !strings.Contains(query, `env="$env"`) || !strings.Contains(query, `instance!=""`) || !strings.Contains(query, "rate(") {
			t.Errorf("query lost its environment scope or counter rate: %s", query)
		}
	}
	for _, operation := range []string{"reserve-pending", "reserve-pending-full", "release-pending", "reserve-owned", "reserve-probe", "exhaustion-proof"} {
		if !strings.Contains(panel.Description, operation) {
			t.Errorf("panel description does not explain %s", operation)
		}
	}
	// A probe-proven refusal is counted, not sent: refused plus skipped is the
	// refusal rate before probing.
	if !strings.Contains(panel.Description, "skipped") {
		t.Error("panel description does not explain skipped reservations")
	}
}
