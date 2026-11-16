package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The cleanup task of a deleted network runs until nothing is left: the
// deletion queues it in its own transaction, without the deleting session's
// credential or address; a run with clients still waiting for their hard
// delete queues the next run for when they are due; the run after that
// completes and queues nothing. A network that exists ends the chain at once.
func TestRemoveDeletedNetworkDataRunsUntilNothingIsLeft(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := model.DefaultDeletedNetworkCleanupSettings()
		functionName := task.NewTaskTargetWithPost(RemoveDeletedNetworkData, RemoveDeletedNetworkDataPost).TargetFunctionName()
		fixture := model.Testing_CreateDeletedNetworkFixture(ctx, "synthetic-deleted-"+server.NewId().String()[:8])
		keptBefore := model.Testing_CountDeletedNetworkFixtureKeptRows(ctx, fixture)

		// the deleting session carries a credential and an address
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", &session.ByJwt{
			NetworkId:   fixture.NetworkId,
			UserId:      fixture.UserId,
			NetworkName: "synthetic",
		})
		defer clientSession.Cancel()
		outcome, _ := RemoveNetworkAndContacts(clientSession, fixture.NetworkId, &fixture.UserId, &model.RemoveNetworkStoreSnapshot{})
		connect.AssertEqual(t, outcome, model.RemoveNetworkRemoved)

		type queuedTask struct {
			count       int
			runAt       time.Time
			withJwt     bool
			withAddress bool
		}
		queued := func() (q queuedTask) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`
						SELECT
							count(*),
							COALESCE(max(run_at), 'epoch'::timestamp),
							COALESCE(bool_or(client_by_jwt_json IS NOT NULL), false),
							COALESCE(bool_or(client_address_hash IS NOT NULL), false)
						FROM pending_task
						WHERE
							function_name = $1 AND
							args_json::jsonb->'deleted_network'->>'network_id' = $2
					`,
					functionName,
					fixture.NetworkId.String(),
				).Scan(&q.count, &q.runAt, &q.withJwt, &q.withAddress))
			})
			return
		}
		q := queued()
		if q.count != 1 || q.withJwt || q.withAddress {
			t.Fatalf("queued by the deletion: %+v, want one run without the session's credential or address", q)
		}

		deletedNetwork := &model.DeletedNetwork{NetworkId: fixture.NetworkId, UserIds: []server.Id{fixture.UserId}}
		now := server.NowUtc()
		result, err := removeDeletedNetworkDataWithSettings(ctx, deletedNetwork, now, settings)
		if err != nil || result.RunAt == nil || result.Cleanup.Complete {
			t.Fatalf("first run = %+v, %v; want the clients to wait", result, err)
		}
		if result.RunAt.Before(now.Add(settings.ClientReapAfterDeactivate)) {
			t.Fatalf("the next run is at %s, before the clients are due", result.RunAt)
		}
		// the post queues the next run for that time
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(RemoveDeletedNetworkDataPost(&RemoveDeletedNetworkDataArgs{DeletedNetwork: deletedNetwork}, result, clientSession, tx))
		})
		if q = queued(); q.count != 1 {
			t.Fatalf("after the first run %d runs are queued, want 1", q.count)
		}

		result, err = removeDeletedNetworkDataWithSettings(ctx, deletedNetwork, result.RunAt.Add(time.Minute), settings)
		if err != nil || result.RunAt != nil || !result.Cleanup.Complete {
			t.Fatalf("the run when the clients are due = %+v, %v; want complete", result, err)
		}
		connect.AssertEqual(t, model.Testing_CountDeletedNetworkFixtureRows(ctx, fixture), map[string]int64{})
		connect.AssertEqual(t, model.Testing_CountDeletedNetworkFixtureKeptRows(ctx, fixture), keptBefore)

		// a network that exists ends the chain without a change
		live := model.Testing_CreateDeletedNetworkFixture(ctx, "synthetic-live-"+server.NewId().String()[:8])
		liveRows := model.Testing_CountDeletedNetworkFixtureRows(ctx, live)
		result, err = removeDeletedNetworkDataWithSettings(ctx, &model.DeletedNetwork{NetworkId: live.NetworkId}, now, settings)
		if err != nil || result.RunAt != nil || !result.Cleanup.NetworkExists {
			t.Fatalf("a run for a network that exists = %+v, %v", result, err)
		}
		connect.AssertEqual(t, model.Testing_CountDeletedNetworkFixtureRows(ctx, live), liveRows)
	})
}

// A refused deletion queues no cleanup.
func TestRefusedNetworkRemovalQueuesNoCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		functionName := task.NewTaskTargetWithPost(RemoveDeletedNetworkData, RemoveDeletedNetworkDataPost).TargetFunctionName()
		fixture := model.Testing_CreateDeletedNetworkFixture(ctx, "synthetic-refused-"+server.NewId().String()[:8])
		now := server.NowUtc()
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          fixture.NetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-time.Hour),
			EndTime:            now.Add(24 * time.Hour),
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      "in_synthetic_refused_cleanup",
		}); err != nil {
			t.Fatal(err)
		}
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		outcome, _ := RemoveNetworkAndContacts(clientSession, fixture.NetworkId, &fixture.UserId, nil)
		connect.AssertEqual(t, outcome, model.RemoveNetworkStripeRenewalActive)
		var n int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name = $1`, functionName).Scan(&n))
		})
		connect.AssertEqual(t, n, 0)
	})
}
