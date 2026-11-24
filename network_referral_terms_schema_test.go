package server

import "testing"

// DB-backed: the links that exist when the referral terms migration applies
// are existing links, and a link the running release inserts after it,
// without naming the column, is a new one. The migration's statements run
// again change neither.
func TestNetworkReferralTermsMigrationMarksTheLinksThatExist(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		version := NetworkReferralTermsSchemaMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		// the running release's insert (model.CreateNetworkReferral)
		insert := func(networkId Id) {
			MaintenanceDb(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(
					ctx,
					`INSERT INTO network_referral (network_id, referral_network_id, create_time) VALUES ($1, $2, $3)`,
					networkId,
					NewId(),
					NowUtc(),
				))
			}, OptReadWrite(), OptNoRetry())
		}
		existingLink := func(networkId Id) (existing bool) {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT existing_link FROM network_referral WHERE network_id = $1`, networkId).Scan(&existing))
			})
			return
		}
		before := NewId()
		insert(before)
		ApplyDbMigrationsUpTo(ctx, version)
		after := NewId()
		insert(after)
		if !existingLink(before) {
			t.Fatal("a link that existed before the migration is not an existing link")
		}
		if existingLink(after) {
			t.Fatal("a link inserted after the migration is an existing link")
		}

		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, networkReferralTermsSchemaSql))
		}, OptNoRetry())
		if !existingLink(before) || existingLink(after) {
			t.Fatal("the migration's statements run again changed a link")
		}
	})
}
