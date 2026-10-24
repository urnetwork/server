// The session signer on the weekly-partitioned journal
// (PARTITION-MAINTENANCE-PLAN.md C4): signing, the receipt reference and the
// retire lookup through the partitioned parent.
package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// On the partitioned journal the signer still signs every event of its own
// transaction, each receipt references its event through the trigger, a
// reservation's endpoint chains are complete, and a retire keeps its admit's
// network; the events are in the current week's partition.
func TestProviderWorkSessionPartitionedJournalSignsAndReserves(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		server.Testing_PartitionProviderWorkSessionEvents(t.Context())
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		f.close(t, id)
		receipts := providerWorkFixtureReceipts(t, f.ctx, id)
		if reservation := providerWorkFixtureReservation(t, receipts, id); !reservation.Reservation.Complete {
			t.Fatal("the partitioned journal lost the complete endpoint cuts", reservation.Reservation)
		}
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			states := providerWorkFixtureEndpoint(t, f, receipts, clientId)
			if len(states) != 2 || states[1].ActiveConnections != 1 {
				t.Fatal("the partitioned journal lost the admission chain", states)
			}
		}
		if err := DisconnectNetworkClient(f.ctx, f.sourceConnectionId); err != nil {
			t.Fatal(err)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var events, receipts, outsideWeek int
			var retireNetworkId *server.Id
			server.Raise(conn.QueryRow(
				f.ctx,
				`SELECT
					(SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
					(SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1),
					(SELECT count(*) FROM provider_work_session_event WHERE client_id=$1 AND tableoid::regclass::text NOT LIKE 'provider_work_session_event_w%'),
					(SELECT network_id FROM provider_work_session_event WHERE client_id=$1 AND kind='retire')`,
				f.sourceId,
			).Scan(&events, &receipts, &outsideWeek, &retireNetworkId))
			if events != 3 || receipts != 3 || outsideWeek != 0 {
				t.Fatal("the partitioned journal lost or misplaced an event or receipt", events, receipts, outsideWeek)
			}
			if retireNetworkId == nil || *retireNetworkId != f.sourceNetworkId {
				t.Fatal("the retire lost its admit's network", retireNetworkId)
			}
		})
	})
}
