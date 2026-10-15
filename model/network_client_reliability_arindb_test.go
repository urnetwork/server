// The location rollup carries both ARIN slots of a client's live connections
// (arindbctl/ARINDB.md, "Connection and rollup facts").
package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// The rollup values of one client folded from rows.
func foldArinRollupRowsForTest(rows ...clientLocationReliabilityRow) []any {
	reliability := newClientLocationReliability(server.NewId(), true)
	for _, row := range rows {
		reliability.add(row)
	}
	return reliability.Values()
}

// The old slot folds exactly like the latest: any risky or non-quality live
// connection marks the client, and neither slot leaks into the other.
func TestClientLocationReliabilityFoldsTheOldSlotLikeTheLatest(t *testing.T) {
	values := foldArinRollupRowsForTest(
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 10},
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 10, arinOldNonQuality: true},
	)
	if values[18] != false || values[19] != false || values[20] != false || values[21] != true {
		t.Fatalf("slots crossed: latest risk=%v nq=%v old risk=%v nq=%v", values[18], values[19], values[20], values[21])
	}
	values = foldArinRollupRowsForTest(
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 10, arinNonQuality: true},
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 10, arinOldRisk: true},
	)
	if values[18] != false || values[19] != true || values[20] != true || values[21] != false {
		t.Fatalf("slots crossed: latest risk=%v nq=%v old risk=%v nq=%v", values[18], values[19], values[20], values[21])
	}
}

// A slot's epoch is the common epoch of every live connection, and 0 when the
// connections were classified by different databases, so a mixed client is
// never read under either.
func TestClientLocationReliabilityEpochsAreCommonOrZero(t *testing.T) {
	for _, c := range []struct {
		rows                 []clientLocationReliabilityRow
		wantEpoch, wantOldEp int64
	}{
		{rows: []clientLocationReliabilityRow{{arinEpoch: 20, arinOldEpoch: 10}}, wantEpoch: 20, wantOldEp: 10},
		{rows: []clientLocationReliabilityRow{{arinEpoch: 20, arinOldEpoch: 10}, {arinEpoch: 20, arinOldEpoch: 10}}, wantEpoch: 20, wantOldEp: 10},
		// a restarted and a not yet restarted process: both carry 10
		{rows: []clientLocationReliabilityRow{{arinEpoch: 20, arinOldEpoch: 10}, {arinEpoch: 10, arinOldEpoch: 5}}, wantOldEp: 10},
		// one connection without old facts
		{rows: []clientLocationReliabilityRow{{arinEpoch: 20, arinOldEpoch: 10}, {arinEpoch: 20}}, wantEpoch: 20},
		// a mixed first row cannot be restored by later agreeing rows
		{rows: []clientLocationReliabilityRow{{arinEpoch: 10}, {arinEpoch: 20}, {arinEpoch: 20}}},
	} {
		values := foldArinRollupRowsForTest(c.rows...)
		if values[22] != c.wantEpoch || values[23] != c.wantOldEp {
			t.Errorf("rows %+v: epochs %v/%v, want %d/%d", c.rows, values[22], values[23], c.wantEpoch, c.wantOldEp)
		}
	}
}

// While Connect restarts onto a new release, a provider whose connections span
// a restarted and a waiting host carries the release the fleet is leaving in
// every row: in the old slot of one and the latest slot of the other. The
// rollup keeps it available under that epoch with the facts of whichever slot
// carries it in each row, so a rolling restart does not make multi-host
// providers unavailable, while the new latest stays unavailable until every
// row carries it. The latest columns keep every row's latest slot, as before.
func TestClientLocationReliabilityKeepsTheEpochEveryRowCarries(t *testing.T) {
	values := foldArinRollupRowsForTest(
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 10, arinOldNonQuality: true},
		clientLocationReliabilityRow{arinEpoch: 10, arinOldEpoch: 5, arinRisk: true},
	)
	if values[22] != int64(0) || values[23] != int64(10) || values[20] != true || values[21] != true {
		t.Fatalf("spanning a restart: epochs %v/%v, old facts risk=%v non-quality=%v, want 0/10 with both rows' facts under 10",
			values[22], values[23], values[20], values[21])
	}
	if values[18] != true || values[19] != false {
		t.Fatalf("the latest columns changed: risk=%v non-quality=%v", values[18], values[19])
	}
	// an epoch one row does not carry is not available
	values = foldArinRollupRowsForTest(
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 10},
		clientLocationReliabilityRow{arinEpoch: 10, arinOldEpoch: 5},
		clientLocationReliabilityRow{arinEpoch: 20},
	)
	if values[22] != int64(0) || values[23] != int64(0) {
		t.Fatalf("an epoch one row lacks: epochs %v/%v, want 0/0", values[22], values[23])
	}
	// the newest epoch every row carries, other than the common latest
	values = foldArinRollupRowsForTest(
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 10},
		clientLocationReliabilityRow{arinEpoch: 20, arinOldEpoch: 5},
	)
	if values[22] != int64(20) || values[23] != int64(0) {
		t.Fatalf("common latest, differing old: epochs %v/%v, want 20/0", values[22], values[23])
	}
}
