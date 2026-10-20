// Database controls for the old ARIN slot's write fence and the additive
// canary migration (arindbctl/ARINDB.md, "Connection and rollup facts").
package server

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The published canary pair keeps its adjacency after provider evidence;
// subsequent additive migrations do not change either migration's position.
func TestArinDatabaseCanaryMigrationsKeepPublishedOrder(t *testing.T) {
	rollup := sqlMigrationIndex(t, "ADD COLUMN arin_old_epoch")
	location := sqlMigrationIndex(t, "ADD COLUMN arin_old_quality_write_token")
	evidence := sqlMigrationIndex(t, "CREATE TABLE provider_modern_evidence")
	if rollup != 837 || location != rollup+1 || rollup <= evidence {
		t.Fatalf("canary migrations at versions %d and %d of %d, after the modern provider evidence at %d; want published adjacent versions 838 and 839",
			rollup+1, location+1, len(migrations), evidence+1)
	}
}

var arinCanaryMigrationTablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)`),
	regexp.MustCompile(`(?is)CREATE\s+TRIGGER\s+\w+\s+.*?\s+ON\s+(\w+)`),
}

// Each canary migration locks one table. One transaction over both held
// ACCESS EXCLUSIVE on the connection rows while it waited for the rollup,
// queueing every location write behind update_reliabilities, which takes
// those locks in the other order.
func TestArinDatabaseCanaryMigrationsLockOneTableEach(t *testing.T) {
	lockedTables := []string{}
	canaryMigrations := 0
	for i, migration := range migrations {
		sqlMigration, ok := migration.(*SqlMigration)
		if !ok || !strings.Contains(sqlMigration.sql, "arin_old_") {
			continue
		}
		canaryMigrations += 1
		tables := []string{}
		for _, pattern := range arinCanaryMigrationTablePatterns {
			for _, match := range pattern.FindAllStringSubmatch(sqlMigration.sql, -1) {
				if !slices.Contains(tables, match[1]) {
					tables = append(tables, match[1])
				}
			}
		}
		if len(tables) != 1 {
			t.Errorf("canary migration %d locks %v, want exactly one table", i+1, tables)
			continue
		}
		lockedTables = append(lockedTables, tables[0])
	}
	slices.Sort(lockedTables)
	if canaryMigrations != 2 || !slices.Equal(lockedTables, []string{"network_client_location", "network_client_location_reliability"}) {
		t.Fatalf("%d canary migrations lock %v, want one for the connection rows and one for the rollup", canaryMigrations, lockedTables)
	}
}

// Rows written before the migrations read as unavailable in the old slot, and
// the migrations leave the latest slot's facts and v751 guard untouched.
func TestArinDatabaseCanaryMigrationsFromThePreviousHeadAreAdditive(t *testing.T) {
	rollup := sqlMigrationIndex(t, "ADD COLUMN arin_old_epoch")
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, rollup)
		connectionId, clientId, locationId := NewId(), NewId(), NewId()
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
				(connection_id, client_id, city_location_id, region_location_id, country_location_id,
				 arin_database_build_epoch, arin_quality_verified, arin_quality_write_token)
				VALUES ($1,$2,$3,$3,$3,1700086400,true,$4)`, connectionId, clientId, locationId, NewId()))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
				(client_id, network_id, update_block_number, connected, client_address_hash_count, location_count)
				VALUES ($1,$2,1,true,1,1)`, clientId, NewId()))
		})
		// the rollup migration alone adds the rollup's slots and leaves the
		// connection rows as they were
		ApplyDbMigrationsUpTo(ctx, rollup+1)
		Db(ctx, func(conn PgConn) {
			var rollupColumns, locationColumns int
			Raise(conn.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE table_name='network_client_location_reliability' AND column_name IN ('arin_old_risk','arin_old_non_quality','arin_epoch','arin_old_epoch')),
				count(*) FILTER (WHERE table_name='network_client_location' AND column_name LIKE 'arin_old_%')
				FROM information_schema.columns WHERE table_schema='public'`).Scan(&rollupColumns, &locationColumns))
			if rollupColumns != 4 || locationColumns != 0 {
				t.Fatalf("version %d has %d rollup slot columns and %d connection old-slot columns, want 4 and 0", rollup+1, rollupColumns, locationColumns)
			}
		}, OptReadOnly(), OptNoRetry())
		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
		Db(ctx, func(conn PgConn) {
			var verified, oldRisk, oldNonQuality, oldVerified bool
			var epoch, oldEpoch int64
			var oldToken *Id
			Raise(conn.QueryRow(ctx, `SELECT arin_quality_verified, arin_database_build_epoch,
				arin_old_risk, arin_old_non_quality, arin_old_quality_verified, arin_old_database_build_epoch, arin_old_quality_write_token
				FROM network_client_location WHERE connection_id=$1`, connectionId).Scan(
				&verified, &epoch, &oldRisk, &oldNonQuality, &oldVerified, &oldEpoch, &oldToken))
			if !verified || epoch != 1700086400 || oldRisk || oldNonQuality || oldVerified || oldEpoch != 0 || oldToken != nil {
				t.Fatalf("migration changed the latest slot or invented old facts: verified=%t epoch=%d old=%t/%t/%t/%d token=%v",
					verified, epoch, oldRisk, oldNonQuality, oldVerified, oldEpoch, oldToken)
			}
			var rollupOldRisk, rollupOldNonQuality bool
			var rollupEpoch, rollupOldEpoch int64
			Raise(conn.QueryRow(ctx, `SELECT arin_old_risk, arin_old_non_quality, arin_epoch, arin_old_epoch
				FROM network_client_location_reliability WHERE client_id=$1`, clientId).Scan(
				&rollupOldRisk, &rollupOldNonQuality, &rollupEpoch, &rollupOldEpoch))
			if rollupOldRisk || rollupOldNonQuality || rollupEpoch != 0 || rollupOldEpoch != 0 {
				t.Fatal("migration invented rollup slot facts")
			}
		}, OptReadOnly(), OptNoRetry())
	})
}

// A writer that predates the old slot supplies a fresh latest token but no
// old token. It must not keep an earlier old-slot positive or epoch, while a
// fresh old token keeps exactly what it wrote; a reused or null token cannot
// restore a revoked fact.
func TestArinOldQualityWriteGuardRejectsMissingAndReusedTokens(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		connectionId, clientId, locationId := NewId(), NewId(), NewId()
		firstToken, secondToken, thirdToken := NewId(), NewId(), NewId()
		for _, row := range []struct {
			name         string
			sql          string
			args         []any
			wantVerified bool
			wantEpoch    int64
			wantToken    *Id
		}{
			{name: "insert without an old token", sql: `INSERT INTO network_client_location
				(connection_id,client_id,city_location_id,region_location_id,country_location_id,
				 arin_quality_write_token,arin_old_quality_verified,arin_old_database_build_epoch)
				VALUES($1,$2,$3,$3,$3,$4,true,1700000000)`, args: []any{connectionId, clientId, locationId, NewId()}},
			{name: "fresh old positive", sql: `UPDATE network_client_location SET arin_old_quality_verified=true,
				arin_old_database_build_epoch=1700000000, arin_old_quality_write_token=$2 WHERE connection_id=$1`,
				args: []any{connectionId, firstToken}, wantVerified: true, wantEpoch: 1700000000, wantToken: &firstToken},
			{name: "old-binary conflict with a fresh latest token", sql: `INSERT INTO network_client_location
				(connection_id,client_id,city_location_id,region_location_id,country_location_id,arin_quality_write_token)
				VALUES($1,$2,$3,$3,$3,$4) ON CONFLICT(connection_id) DO UPDATE SET arin_quality_write_token=$4`,
				args: []any{connectionId, clientId, locationId, NewId()}, wantToken: &firstToken},
			{name: "token replay after revocation", sql: `UPDATE network_client_location SET arin_old_quality_verified=true,
				arin_old_database_build_epoch=1700000000, arin_old_quality_write_token=$2 WHERE connection_id=$1`,
				args: []any{connectionId, firstToken}, wantToken: &firstToken},
			{name: "re-attested", sql: `UPDATE network_client_location SET arin_old_quality_verified=true,
				arin_old_database_build_epoch=1700000000, arin_old_quality_write_token=$2 WHERE connection_id=$1`,
				args: []any{connectionId, secondToken}, wantVerified: true, wantEpoch: 1700000000, wantToken: &secondToken},
			{name: "legacy no-op", sql: `UPDATE network_client_location SET city_location_id=city_location_id WHERE connection_id=$1`,
				args: []any{connectionId}, wantToken: &secondToken},
			{name: "explicit null token", sql: `UPDATE network_client_location SET arin_old_quality_verified=true,
				arin_old_database_build_epoch=1700000000, arin_old_quality_write_token=NULL WHERE connection_id=$1`,
				args: []any{connectionId}, wantToken: &secondToken},
			{name: "fresh write without old facts", sql: `UPDATE network_client_location SET arin_old_quality_verified=false,
				arin_old_database_build_epoch=0, arin_old_quality_write_token=$2 WHERE connection_id=$1`,
				args: []any{connectionId, thirdToken}, wantToken: &thirdToken},
		} {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, row.sql, row.args...))
			})
			Db(ctx, func(conn PgConn) {
				var verified bool
				var epoch int64
				var token *Id
				Raise(conn.QueryRow(ctx, `SELECT arin_old_quality_verified, arin_old_database_build_epoch, arin_old_quality_write_token
					FROM network_client_location WHERE connection_id=$1`, connectionId).Scan(&verified, &epoch, &token))
				tokenMatches := token == nil && row.wantToken == nil || token != nil && row.wantToken != nil && *token == *row.wantToken
				if verified != row.wantVerified || epoch != row.wantEpoch || !tokenMatches {
					t.Fatalf("%s: verified=%t epoch=%d want=%t/%d; token matches=%t", row.name, verified, epoch, row.wantVerified, row.wantEpoch, tokenMatches)
				}
			}, OptReadOnly(), OptNoRetry())
		}
	})
}
