package server

import "testing"

// Published version 749 must upgrade by one append. Old connection writers
// remain SQL-compatible but cannot accidentally attest subscriber access.
func TestSubscriberQualityMigrationFrom749(t *testing.T) {
	index := sqlMigrationIndex(t, "ADD COLUMN arin_quality_verified")
	if index != 749 {
		t.Fatalf("subscriber migration index=%d, want749 after published version749", index)
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 749)
		oldConnection, oldClient, location := NewId(), NewId(), NewId()
		insertLegacy := func(connection, client Id) {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
					(connection_id,client_id,city_location_id,region_location_id,country_location_id)
					VALUES($1,$2,$3,$3,$3)`, connection, client, location))
			})
		}
		insertLegacy(oldConnection, oldClient)
		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
		newConnection := NewId()
		insertLegacy(newConnection, NewId())
		Db(ctx, func(conn PgConn) {
			var legacyVerified int
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client_location
				WHERE connection_id=ANY($1::uuid[]) AND arin_quality_verified`, []Id{oldConnection, newConnection}).Scan(&legacyVerified))
			if legacyVerified != 0 {
				t.Fatal("legacy row or old writer acquired positive subscriber evidence")
			}
		}, OptReadOnly(), OptNoRetry())
	})
}
