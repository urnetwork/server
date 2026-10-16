// Database controls for the two ARIN slots of a connection and its rollup
// (arindbctl/ARINDB.md, "Connection and rollup facts").
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type arinSlotsForTest struct {
	risk, nonQuality, verified          bool
	epoch                               int64
	oldRisk, oldNonQuality, oldVerified bool
	oldEpoch                            int64
}

func readConnectionArinSlotsForTest(t testing.TB, connectionId server.Id) (slots arinSlotsForTest) {
	t.Helper()
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT arin_risk, arin_non_quality, arin_quality_verified, arin_database_build_epoch,
			arin_old_risk, arin_old_non_quality, arin_old_quality_verified, arin_old_database_build_epoch
			FROM network_client_location WHERE connection_id=$1`, connectionId).Scan(
			&slots.risk, &slots.nonQuality, &slots.verified, &slots.epoch,
			&slots.oldRisk, &slots.oldNonQuality, &slots.oldVerified, &slots.oldEpoch))
	}, server.OptReadOnly(), server.OptNoRetry())
	return
}

// Each write stores both slots as given, and a later write without old facts,
// or one by a writer that predates the old slot, leaves no earlier old fact.
func TestSetConnectionLocationWritesBothArinSlots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		lookup := server.NowUtc().Truncate(time.Microsecond)
		city := egressTestCity(ctx, "Slot City", "Slot Region", "Slot Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: 1700086400, ArinNonQuality: true,
			ArinOldDatabaseBuildEpoch: 1700000000, ArinOldQualityVerified: true, ArinOldRisk: false,
		})
		if slots := readConnectionArinSlotsForTest(t, provider.connectionId); slots != (arinSlotsForTest{
			nonQuality: true, epoch: 1700086400, oldVerified: true, oldEpoch: 1700000000,
		}) {
			t.Fatalf("both slots not stored as written: %+v", slots)
		}
		if err := SetConnectionLocation(ctx, provider.connectionId, city.LocationId, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: 1700086400, ArinQualityVerified: true,
		}); err != nil {
			t.Fatal(err)
		}
		if slots := readConnectionArinSlotsForTest(t, provider.connectionId); slots != (arinSlotsForTest{verified: true, epoch: 1700086400}) {
			t.Fatalf("a write without old facts kept the earlier old slot: %+v", slots)
		}
		if err := SetConnectionLocation(ctx, provider.connectionId, city.LocationId, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: 1700086400, ArinQualityVerified: true,
			ArinOldDatabaseBuildEpoch: 1700000000, ArinOldQualityVerified: true,
		}); err != nil {
			t.Fatal(err)
		}
		// the latest-only upsert of a binary that predates the old slot
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_quality_verified=true,
				arin_quality_write_token=$2 WHERE connection_id=$1`, provider.connectionId, server.NewId()))
		})
		if slots := readConnectionArinSlotsForTest(t, provider.connectionId); slots != (arinSlotsForTest{verified: true, epoch: 1700086400}) {
			t.Fatalf("a writer predating the old slot kept its positive or epoch: %+v", slots)
		}
	})
}

// The rollup folds both slots over live connections and keeps each slot's
// epoch only when every live connection agrees on it; when the old slots
// differ, the old slot carries the epoch every live connection carries in
// either slot.
func TestClientLocationRollupCarriesBothArinSlotsOverLiveConnections(t *testing.T) {
	useSyntheticIpDb(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		lookup := server.NowUtc().Truncate(time.Microsecond)
		city := egressTestCity(ctx, "Rollup City", "Rollup Region", "Rollup Country", "zz")
		paired := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: 1700086400, ArinQualityVerified: true,
			ArinOldDatabaseBuildEpoch: 1700000000, ArinOldNonQuality: true, ArinOldRisk: true,
		})
		// a second live connection of another provider written by a process
		// that is not restarted yet: latest is the old database, no old slot
		lagging := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: 1700000000, ArinQualityVerified: true,
		})
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		read := func(clientId server.Id) (risk, nonQuality, oldRisk, oldNonQuality bool, epoch, oldEpoch int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT arin_risk, arin_non_quality, arin_old_risk, arin_old_non_quality, arin_epoch, arin_old_epoch
					FROM network_client_location_reliability WHERE client_id=$1`, clientId).Scan(
					&risk, &nonQuality, &oldRisk, &oldNonQuality, &epoch, &oldEpoch))
			}, server.OptReadOnly(), server.OptNoRetry())
			return
		}
		if risk, _, oldRisk, oldNonQuality, epoch, oldEpoch := read(paired.clientId); risk || !oldRisk || !oldNonQuality || epoch != 1700086400 || oldEpoch != 1700000000 {
			t.Fatalf("paired rollup: risk=%t old risk=%t old nq=%t epochs=%d/%d", risk, oldRisk, oldNonQuality, epoch, oldEpoch)
		}
		if _, _, _, _, epoch, oldEpoch := read(lagging.clientId); epoch != 1700000000 || oldEpoch != 0 {
			t.Fatalf("lagging rollup epochs=%d/%d, want the latest slot's epoch only", epoch, oldEpoch)
		}
		// a second live connection classified by the previous pair, as while
		// Connect restarts: its latest slot is the first connection's old one
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, paired.clientId, "203.0.113.250:0", handlerId)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: 1700000000, ArinQualityVerified: true,
		}); err != nil {
			t.Fatal(err)
		}
		now = server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		// the latest slots differ, so no latest epoch; both connections carry
		// 1700000000, the first in its old slot and the second in its latest,
		// so the old slot is that epoch with both connections' facts under it
		if _, _, oldRisk, oldNonQuality, epoch, oldEpoch := read(paired.clientId); epoch != 0 || oldEpoch != 1700000000 || !oldRisk || !oldNonQuality {
			t.Fatalf("rollup across a restart: epochs=%d/%d old risk=%t old nq=%t, want 0/1700000000 with the first connection's old facts",
				epoch, oldEpoch, oldRisk, oldNonQuality)
		}
		// a third live connection classified by neither leaves no epoch every
		// connection carries
		thirdId, _, _, _, err := ConnectNetworkClient(ctx, paired.clientId, "203.0.113.251:0", handlerId)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetConnectionLocation(ctx, thirdId, city.LocationId, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: 1700172800, ArinQualityVerified: true,
		}); err != nil {
			t.Fatal(err)
		}
		now = server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		if _, _, _, _, epoch, oldEpoch := read(paired.clientId); epoch != 0 || oldEpoch != 0 {
			t.Fatalf("mixed rollup epochs=%d/%d, want 0/0", epoch, oldEpoch)
		}
	})
}
