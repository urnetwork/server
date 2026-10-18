// Run the owning activity SELECT against connection-local catalog shadows.
// The complete state denominator must survive selection of active groups.
package monitor

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/urnetwork/server"
)

// Decode the additive wire field without requiring its production Go type, so
// the same native control compiles against the baseline that loses these totals.
func pgSampleStateTotalsNativeRead(t testing.TB, conn server.PgConn) (pgSampleWire, []int) {
	t.Helper()
	var raw []byte
	server.Raise(conn.QueryRow(t.Context(), pgSampleActivitySQL(0)).Scan(&raw))
	var frame pgSampleWire
	var extension struct {
		StateTotals json.RawMessage `json:"state_totals"`
	}
	if json.Unmarshal(raw, &frame) != nil || json.Unmarshal(raw, &extension) != nil || frame.Kind != "activity" {
		t.Fatal("native activity frame is invalid")
	}
	var totals []int
	if len(extension.StateTotals) > 64 || json.Unmarshal(extension.StateTotals, &totals) != nil || len(totals) != 7 {
		t.Fatal("native activity snapshot lost its bounded complete state totals")
	}
	return frame, totals
}

// A much larger idle cohort is omitted by both active-first rankings. State
// totals still include every backend/database, null/future states and no self.
func TestPgQuerySampleStateTotalsNativeWholeSnapshotSurvivesGroupCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgSampleSqlNativeFixture(t, conn)
			server.RaisePgResult(conn.Exec(t.Context(), `INSERT INTO pg_stat_activity
 SELECT pg_backend_pid()+i,i,'SELECT 1 /* synthetic active */','active',NULL,NULL,NULL,'','client backend',oid,
 '2026-01-01'::timestamptz,'2026-01-01'::timestamptz,'2026-01-01'::timestamptz
 FROM generate_series(1,96) i CROSS JOIN pg_database WHERE datname=current_database();
 INSERT INTO pg_stat_activity
 SELECT pg_backend_pid()+1000+i,1000,'SELECT 1 /* synthetic idle */','idle','ClientRead','Client',NULL,'','client backend',oid,
 '2026-01-01'::timestamptz,'2026-01-01'::timestamptz,'2026-01-01'::timestamptz
 FROM generate_series(1,5000) i CROSS JOIN pg_database WHERE datname=current_database();
 INSERT INTO pg_stat_activity
 SELECT pg_backend_pid()+20000+v.id*10+i,10000+v.id,'SELECT 1 /* synthetic state */',v.state,NULL,NULL,NULL,'',v.backend,
 CASE WHEN v.id=5 THEN NULL::oid WHEN v.id>=3 THEN 0::oid ELSE (SELECT oid FROM pg_database WHERE datname=current_database()) END,
 '2026-01-01'::timestamptz,'2026-01-01'::timestamptz,'2026-01-01'::timestamptz
 FROM (VALUES (1,'idle in transaction','client backend',3),
 (2,'idle in transaction (aborted)','client backend',2),
 (3,'fastpath function call','client backend',1),(4,'disabled','client backend',1),
 (5,NULL,'checkpointer',2),(6,'synthetic-future-state','synthetic-worker',1)) v(id,state,backend,n)
 CROSS JOIN LATERAL generate_series(1,v.n) i;
 INSERT INTO pg_stat_activity SELECT pg_backend_pid(),99999,'SELECT self_only','active',NULL,NULL,NULL,'','client backend',oid,
 clock_timestamp(),clock_timestamp(),clock_timestamp() FROM pg_database WHERE datname=current_database();`))
			frame, totals := pgSampleStateTotalsNativeRead(t, conn)
			if frame.Total != 5106 || !reflect.DeepEqual(totals, []int{96, 5000, 3, 2, 1, 1, 3}) {
				t.Fatalf("whole snapshot denominator changed: total=%d states=%v", frame.Total, totals)
			}
			if frame.Groups <= len(frame.Rows) || len(frame.Rows) > 128 || frame.SqlCaptureVersion == nil || *frame.SqlCaptureVersion != 1 {
				t.Fatal("native control did not retain the existing capped/private row contract")
			}
			selected := 0
			for _, row := range frame.Rows {
				var state string
				var count int
				if len(row) != 13 || json.Unmarshal(row[1], &state) != nil || json.Unmarshal(row[8], &count) != nil || state != "active" {
					t.Fatal("large idle cohort unexpectedly entered the active-first selected rows")
				}
				selected += count
			}
			if selected == 0 || selected > 96 || selected >= frame.Total {
				t.Fatal("native control did not reproduce the selected-state coverage gap")
			}
		})
	})
}

// Zero is measured only after the complete source rowset is observed empty.
func TestPgQuerySampleStateTotalsNativeEmptySnapshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgSampleSqlNativeFixture(t, conn)
			frame, totals := pgSampleStateTotalsNativeRead(t, conn)
			if frame.Total != 0 || frame.Groups != 0 || len(frame.Rows) != 0 || !reflect.DeepEqual(totals, []int{0, 0, 0, 0, 0, 0, 0}) {
				t.Fatal("empty complete snapshot became missing state coverage")
			}
		})
	})
}
