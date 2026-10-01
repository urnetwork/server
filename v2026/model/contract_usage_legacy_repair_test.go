// The operator repair script is qualified against real synthetic database
// rows. Changed originals or an expanded census roll back the whole repair.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Capture the same exact row/report representation used by the offline repair.
func stLegacyUsageRepairTestManifest(t testing.TB, ctx context.Context, start time.Time) []byte {
	t.Helper()
	var rows json.RawMessage
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object('contract',to_jsonb(c),
			'closes',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.party) FROM contract_close r WHERE r.contract_id=c.contract_id)) ORDER BY c.contract_id)
			FROM transfer_contract c WHERE $1<=close_time AND close_time<$2 AND provider_usage IS NULL AND outcome='settled'`, start, start.Add(time.Hour)).Scan(&rows))
	})
	wire, err := json.Marshal(map[string]any{"schema": "urnetwork-st-legacy-usage-repair-v1", "epoch": 17,
		"epoch_start": start, "epoch_end": start.Add(time.Hour), "legacy_start": start, "legacy_end": start.Add(time.Minute),
		"contract_count": 2, "retained_report_minimum": 100, "final_acceptance": false, "contracts": rows})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// Apply the identical SQL script under its required serializable transaction;
// no model-reader fallback or test-only data patch stands in for the repair.
func stApplyLegacyUsageRepairTest(ctx context.Context, wire []byte, digest string) ([]byte, error) {
	script, err := os.ReadFile("../local/st-legacy-usage-repair.sql")
	if err != nil {
		return nil, err
	}
	var receipt []byte
	var resultErr error
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			resultErr = err
			return
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SELECT set_config('urnetwork.st_usage_repair_manifest',$1,true),set_config('urnetwork.st_usage_repair_sha256',$2,true)`, string(wire), digest); err == nil {
			_, err = tx.Exec(ctx, string(script))
		}
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT current_setting('urnetwork.st_usage_repair_receipt')`).Scan(&receipt)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		resultErr = err
	})
	return receipt, resultErr
}

// Both zero and positive retained reports remain uncredited. A second apply
// cannot rewrite the durable exclusion or pretend the original rows were null.
func TestStContractUsageLegacyRepairTransactionRetainsExactDebt(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		start := time.Unix(1_700_000_000, 0).UTC()
		for _, debt := range []int{0, 100} {
			id := addStContractUsageSnapshotTestRow(t, ctx, start, nil)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',$2,false),($1,'destination',$2,false)`, id, debt))
			})
		}
		wire := stLegacyUsageRepairTestManifest(t, ctx, start)
		server.ApplyDbMigrations(ctx)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(wire))
		receipt, err := stApplyLegacyUsageRepairTest(ctx, wire, digest)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Contracts int    `json:"contracts"`
			Debt      int    `json:"retained_report_minimum"`
			Credited  int    `json:"credited_bytes"`
			Final     bool   `json:"final_acceptance"`
			Manifest  string `json:"repair_manifest_sha256"`
		}
		if err := json.Unmarshal(receipt, &result); err != nil || result.Contracts != 2 || result.Debt != 100 || result.Credited != 0 || result.Final || result.Manifest != digest {
			t.Fatalf("incorrect repair receipt: %s, %v", receipt, err)
		}
		if usages, err := GetStEpochProviderUsageAtEpoch(ctx, 17, start, start.Add(time.Hour)); err != nil || len(usages) != 0 {
			t.Fatalf("legacy debt received credit: %+v, %v", usages, err)
		}
		if _, err := stApplyLegacyUsageRepairTest(ctx, wire, digest); err == nil {
			t.Fatal("repair replay overwrote non-null snapshots")
		}
	})
}

// Hash, original close bytes, and the complete missing-row census are checked
// before mutation. Each forced failure leaves every candidate unchanged.
func TestStContractUsageLegacyRepairTransactionRejectsChangedCohort(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		start := time.Unix(1_700_000_000, 0).UTC()
		var ids []server.Id
		for _, debt := range []int{0, 100} {
			id := addStContractUsageSnapshotTestRow(t, ctx, start, nil)
			ids = append(ids, id)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',$2,false),($1,'destination',$2,false)`, id, debt))
			})
		}
		wire := stLegacyUsageRepairTestManifest(t, ctx, start)
		addStContractUsageSnapshotTestOutcome(t, ctx, start.Add(30*time.Minute), nil, "canceled")
		server.ApplyDbMigrations(ctx)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(wire))
		assertRefused := func(hash string) {
			t.Helper()
			if _, err := stApplyLegacyUsageRepairTest(ctx, wire, hash); err == nil {
				t.Fatal("changed repair cohort accepted")
			}
			var changed int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND provider_usage IS NOT NULL`, ids).Scan(&changed))
			})
			if changed != 0 {
				t.Fatalf("partial repair escaped rollback: %d", changed)
			}
		}
		assertRefused("sha256:" + fmt.Sprintf("%064x", 1))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=99 WHERE contract_id=$1 AND party='source'`, ids[1]))
		})
		assertRefused(digest)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=100 WHERE contract_id=$1 AND party='source'`, ids[1]))
		})
		if _, err := stApplyLegacyUsageRepairTest(ctx, wire, digest); err != nil {
			t.Fatalf("unchanged complete cohort did not recover: %v", err)
		}
	})
}

// A capture made before all older writers stopped cannot borrow a later
// migration's admission: the entire expanded retained census must be reviewed.
func TestStContractUsageLegacyRepairTransactionRejectsExpandedNullCensus(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		start := time.Unix(1_700_000_000, 0).UTC()
		for _, debt := range []int{0, 100} {
			id := addStContractUsageSnapshotTestRow(t, ctx, start, nil)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',$2,false),($1,'destination',$2,false)`, id, debt))
			})
		}
		wire := stLegacyUsageRepairTestManifest(t, ctx, start)
		addStContractUsageSnapshotTestRow(t, ctx, start.Add(30*time.Minute), nil)
		server.ApplyDbMigrations(ctx)
		if _, err := stApplyLegacyUsageRepairTest(ctx, wire, fmt.Sprintf("sha256:%x", sha256.Sum256(wire))); err == nil {
			t.Fatal("expanded missing-proof census borrowed the original repair")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var missing int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE provider_usage IS NULL`).Scan(&missing))
			if missing != 3 {
				t.Fatalf("expanded census refusal changed historical usage: %d missing rows", missing)
			}
		})
	})
}
