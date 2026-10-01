// Database-level controls cover already-contaminated rows and writers that do
// not participate in the positive-fact write protocol.
package server

import "testing"

// A true schema-750 fact survives a legacy conflict update. The successor must
// revoke that existing fact without replaying or changing the old migration.
func TestSubscriberQualityWriteGuardMigrationFrom750(t *testing.T) {
	if index := sqlMigrationIndex(t, "ADD COLUMN arin_quality_write_token"); index != 750 {
		t.Fatalf("write guard migration index=%d, want750 after published version750", index)
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 750)
		connectionId, clientId, locationId, changedLocationId := NewId(), NewId(), NewId(), NewId()
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
				(connection_id, client_id, city_location_id, region_location_id, country_location_id, arin_quality_verified)
				VALUES ($1,$2,$3,$3,$3,true)`, connectionId, clientId, locationId))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
				(connection_id, client_id, city_location_id, region_location_id, country_location_id, arin_risk, arin_non_quality)
				VALUES ($1,$2,$3,$3,$3,false,false)
				ON CONFLICT (connection_id) DO UPDATE SET city_location_id=$3, arin_risk=false, arin_non_quality=false`,
				connectionId, clientId, changedLocationId))
		})
		assertFact := func(want bool) {
			Db(ctx, func(conn PgConn) {
				var verified, eligible bool
				var gotLocationId Id
				Raise(conn.QueryRow(ctx, `SELECT city_location_id, arin_quality_verified,
					arin_quality_verified AND NOT arin_risk AND NOT arin_non_quality
					FROM network_client_location WHERE connection_id=$1`, connectionId).Scan(&gotLocationId, &verified, &eligible))
				if verified != want || eligible != want || gotLocationId != changedLocationId {
					t.Fatalf("verified=%t strict_guard_eligible=%t location=%s, want verified/eligible=%t location=%s",
						verified, eligible, gotLocationId, want, changedLocationId)
				}
			}, OptReadOnly(), OptNoRetry())
		}
		assertFact(true)
		t.Log("schema750 causal control: legacy conflict update retained verified=true and strict_guard_eligible=true")
		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
		assertFact(false)
		// Restart verifies the existing catalog; it must not revoke a fresh fact.
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location
				SET arin_quality_verified=true, arin_quality_write_token=$2 WHERE connection_id=$1`, connectionId, NewId()))
		})
		assertFact(true)
		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
		assertFact(true)
	})
}

// The token is a write boundary, including no-op updates. A legacy write keeps
// the old token so reusing it after invalidation cannot restore a positive fact.
func TestSubscriberQualityWriteGuardRejectsMissingAndReusedTokens(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		connectionId, clientId, locationId := NewId(), NewId(), NewId()
		firstToken, secondToken, thirdToken, fourthToken := NewId(), NewId(), NewId(), NewId()
		for _, row := range []struct {
			name         string
			sql          string
			args         []any
			wantVerified bool
			wantToken    *Id
		}{
			{name: "schema750 insert", sql: `INSERT INTO network_client_location
				(connection_id,client_id,city_location_id,region_location_id,country_location_id,arin_quality_verified)
				VALUES($1,$2,$3,$3,$3,true)`, args: []any{connectionId, clientId, locationId}},
			{name: "fresh positive", sql: `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=$2 WHERE connection_id=$1`, args: []any{connectionId, firstToken}, wantVerified: true, wantToken: &firstToken},
			{name: "same facts fresh token", sql: `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=$2 WHERE connection_id=$1`, args: []any{connectionId, secondToken}, wantVerified: true, wantToken: &secondToken},
			{name: "schema750 conflict", sql: `INSERT INTO network_client_location
				(connection_id,client_id,city_location_id,region_location_id,country_location_id,arin_quality_verified)
				VALUES($1,$2,$3,$3,$3,true) ON CONFLICT(connection_id) DO UPDATE SET arin_quality_verified=true`,
				args: []any{connectionId, clientId, locationId}, wantToken: &secondToken},
			{name: "legacy no-op", sql: `UPDATE network_client_location SET city_location_id=city_location_id WHERE connection_id=$1`,
				args: []any{connectionId}, wantToken: &secondToken},
			{name: "token replay after invalidation", sql: `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=$2 WHERE connection_id=$1`, args: []any{connectionId, secondToken}, wantToken: &secondToken},
			{name: "schema750 positive update", sql: `UPDATE network_client_location SET arin_quality_verified=true WHERE connection_id=$1`,
				args: []any{connectionId}, wantToken: &secondToken},
			{name: "re-attested", sql: `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=$2 WHERE connection_id=$1`, args: []any{connectionId, thirdToken}, wantVerified: true, wantToken: &thirdToken},
			{name: "token collision changed facts", sql: `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=$2, city_location_id=$3 WHERE connection_id=$1`, args: []any{connectionId, thirdToken, NewId()}, wantToken: &thirdToken},
			{name: "explicit null token", sql: `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=NULL WHERE connection_id=$1`, args: []any{connectionId}, wantToken: &thirdToken},
			{name: "token replay after null", sql: `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=$2 WHERE connection_id=$1`, args: []any{connectionId, thirdToken}, wantToken: &thirdToken},
			{name: "fresh negative", sql: `UPDATE network_client_location SET arin_quality_verified=false,
				arin_quality_write_token=$2 WHERE connection_id=$1`, args: []any{connectionId, fourthToken}, wantToken: &fourthToken},
		} {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, row.sql, row.args...))
			})
			Db(ctx, func(conn PgConn) {
				var verified bool
				var token *Id
				Raise(conn.QueryRow(ctx, `SELECT arin_quality_verified, arin_quality_write_token
					FROM network_client_location WHERE connection_id=$1`, connectionId).Scan(&verified, &token))
				tokenMatches := token == nil && row.wantToken == nil || token != nil && row.wantToken != nil && *token == *row.wantToken
				if verified != row.wantVerified || !tokenMatches {
					t.Fatalf("%s: verified=%t want=%t; token matches=%t", row.name, verified, row.wantVerified, tokenMatches)
				}
			}, OptReadOnly(), OptNoRetry())
		}
	})
}
