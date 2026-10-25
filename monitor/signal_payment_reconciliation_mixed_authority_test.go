// Query-contract controls reject impossible mixed aggregates without payment actions.
package monitor

import (
	"context"
	"testing"
)

// A valid SQL remainder requires nonblank evidence and a non-null run ID.
// Null evidence remains legitimate in an invalid-only aggregate, covered separately.
func TestPaymentReconciliationMixedContradictoryAuthorityIsUnknown(t *testing.T) {
	for _, test := range []struct {
		name string
		row  Row
	}{
		{name: "zero-evidence", row: Row{"0", "2", "2", "1", "120"}},
		{name: "zero-runs", row: Row{"1", "2", "0", "1", "120"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), paymentMixedSettings([]Row{test.row}))
			if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !isVisibilityAlert(alerts[0]) {
				t.Fatal("contradictory mixed SQL authority must remain unknown without a terminal incident")
			}
			requirePaymentMixedPrivacy(t, alerts, "synthetic-private-payment-audit-value")
		})
	}
}
