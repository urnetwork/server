// Database controls for the old ARIN slot's write fence and the additive
// canary migration (arindbctl/ARINDB.md, "Connection and rollup facts").
package server

import "testing"

// The canary schema is appended after version 836, the FP2 modern provider
// evidence.
func TestArinDatabaseCanaryMigrationIsAppendedAfter836(t *testing.T) {
	if index := sqlMigrationIndex(t, "ADD COLUMN arin_old_quality_write_token"); index != 836 {
		t.Fatalf("canary migration index=%d, want 836 after version 836", index)
	}
}

// Rows written before the migration read as unavailable in the old slot, and
// the migration leaves the latest slot's facts and v751 guard untouched.
func TestArinDatabaseCanaryMigrationFrom836IsAdditive(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 836)
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
