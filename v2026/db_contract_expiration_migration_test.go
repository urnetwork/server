// An absolute deadline is prospective, including during a rolling deployment.
package server

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Existing rows and inserts from old binaries stay NULL. The migration neither
// infers a lifetime from creation age nor rewrites the retained contract ledger.
func TestContractExpirationMigrationPreservesLegacyWriters(t *testing.T) {
	index := slices.IndexFunc(migrations, func(migration any) bool {
		sql, ok := migration.(*SqlMigration)
		return ok && strings.Contains(sql.sql, "ADD COLUMN expiration_time")
	})
	if index < 0 {
		t.Fatal("contract expiration migration missing")
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, index)
		insert := func() Id {
			id := NewId()
			Db(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time)
					VALUES($1,$2,$3,$4,$5,100,$6)`, id, NewId(), NewId(), NewId(), NewId(), time.Unix(1_700_000_000, 0).UTC()))
			})
			return id
		}
		existing := insert()
		ApplyDbMigrationsUpTo(ctx, index+1)
		rollingWriter := insert()
		dataType, notNull, defaultExpression, err := loadMigrationColumnShape(ctx, "transfer_contract", "expiration_time")
		if err != nil || dataType != "timestamp without time zone" || notNull || defaultExpression != "" {
			t.Fatalf("deadline shape=%s required=%t default=%q err=%v", dataType, notNull, defaultExpression, err)
		}
		Db(ctx, func(conn PgConn) {
			var untouched bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(expiration_time IS NULL)
				FROM transfer_contract WHERE contract_id=ANY($1)`, []Id{existing, rollingWriter}).Scan(&untouched))
			if !untouched {
				t.Fatal("migration assigned new deadlines to legacy contracts")
			}
		})
	})
}
