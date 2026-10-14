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
		// a restarted and a not yet restarted process
		{rows: []clientLocationReliabilityRow{{arinEpoch: 20, arinOldEpoch: 10}, {arinEpoch: 10, arinOldEpoch: 5}}},
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
