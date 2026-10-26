// The process-local gates that skip no-op network_client writes.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A new client past the bound clears the map before it is stored; an update of
// a stored client never counts again.
func TestBoundedClientMapClearsWholesaleAtItsBound(t *testing.T) {
	values := newBoundedClientMap[int](2)
	a, b, c := server.NewId(), server.NewId(), server.NewId()

	values.Store(a, 1)
	values.Store(b, 2)
	values.Store(a, 3)
	connect.AssertEqual(t, values.Len(), int64(2))
	if value, ok := values.Load(a); !ok || value != 3 {
		t.Fatal("an update did not replace the stored value")
	}

	values.Store(c, 4)
	connect.AssertEqual(t, values.Len(), int64(1))
	_, okA := values.Load(a)
	_, okB := values.Load(b)
	value, okC := values.Load(c)
	if okA || okB || !okC || value != 4 {
		t.Fatal("the bound did not clear the map and keep only the new client")
	}

	values.Clear()
	connect.AssertEqual(t, values.Len(), int64(0))
	if _, ok := values.Load(c); ok {
		t.Fatal("clear kept an entry")
	}
}

// A skip needs the client's and its parent's recorded auth_time within the
// interval, and the client's parent known. At the interval the refresh's
// predicate (auth_time < connectTime - interval) still matches nothing, but the
// gate asks the database.
func TestClientAuthTimeGateKnownFresh(t *testing.T) {
	gate := newClientAuthTimeGate(16)
	now := server.NowUtc()
	fresh := now.Add(-clientAuthTimeRefreshMinInterval + time.Second)
	atInterval := now.Add(-clientAuthTimeRefreshMinInterval)

	topLevelId := server.NewId()
	connect.AssertEqual(t, gate.KnownFresh(topLevelId, now), false)
	gate.Record(&clientAuthTimeRefresh{clientId: topLevelId, authTime: &fresh})
	connect.AssertEqual(t, gate.KnownFresh(topLevelId, now), true)
	gate.Record(&clientAuthTimeRefresh{clientId: topLevelId, authTime: &atInterval})
	connect.AssertEqual(t, gate.KnownFresh(topLevelId, now), false)

	parentId, childId := server.NewId(), server.NewId()
	gate.Record(&clientAuthTimeRefresh{clientId: childId, authTime: &fresh, parentClientId: &parentId, parentAuthTime: &fresh})
	connect.AssertEqual(t, gate.KnownFresh(childId, now), true)
	gate.Record(&clientAuthTimeRefresh{clientId: childId, authTime: &fresh, parentClientId: &parentId, parentAuthTime: &atInterval})
	connect.AssertEqual(t, gate.KnownFresh(childId, now), false)

	// a parent whose own auth_time the refresh could not read is unknown
	unreadParentId, otherChildId := server.NewId(), server.NewId()
	gate.Record(&clientAuthTimeRefresh{clientId: otherChildId, authTime: &fresh, parentClientId: &unreadParentId})
	connect.AssertEqual(t, gate.KnownFresh(otherChildId, now), false)

	// a refresh that found no auth_time records nothing
	missingId := server.NewId()
	gate.Record(&clientAuthTimeRefresh{clientId: missingId})
	connect.AssertEqual(t, gate.KnownFresh(missingId, now), false)

	gate.Reset()
	connect.AssertEqual(t, gate.KnownFresh(topLevelId, fresh), false)
}

// Fails the commit of any transaction that connects one of the listed
// clients: a deferred constraint trigger raises at COMMIT, after every
// statement of the transaction has returned. Test-only schema, dropped on
// return.
func failConnectCommits(t testing.TB, ctx context.Context, clientIds ...server.Id) func() {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE TABLE test_fail_connect_commit_client (client_id uuid PRIMARY KEY);
			CREATE FUNCTION test_fail_connect_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF EXISTS (SELECT 1 FROM test_fail_connect_commit_client WHERE client_id = NEW.client_id) THEN
					RAISE EXCEPTION 'synthetic commit failure';
				END IF;
				RETURN NEW;
			END
			$$;
			CREATE CONSTRAINT TRIGGER test_fail_connect_commit
				AFTER INSERT ON network_client_connection
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION test_fail_connect_commit();
		`))
		for _, clientId := range clientIds {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_fail_connect_commit_client (client_id) VALUES ($1)`, clientId))
		}
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				DROP TRIGGER test_fail_connect_commit ON network_client_connection;
				DROP FUNCTION test_fail_connect_commit();
				DROP TABLE test_fail_connect_commit_client;
			`))
		})
	}
}

// A refresh whose transaction does not commit left the rows stale, so the
// process must not record it: the next connect refreshes.
func TestConnectNetworkClientRecordsAuthTimesOnlyOnceCommitted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, parentId, childIds := testCreateParentWithChildren(ctx, t, 1)
		staleTime := server.NowUtc().Add(-2 * clientAuthTimeRefreshMinInterval)
		testSetNetworkClientAuthTime(ctx, parentId, staleTime)
		testSetNetworkClientAuthTime(ctx, childIds[0], staleTime)

		observedCtx, statements, closeObservation := observeNoopWriteStatements(t, ctx)
		restoreCommits := failConnectCommits(t, ctx, childIds[0])
		recovered := server.HandleError(func() {
			ConnectNetworkClient(observedCtx, childIds[0], "192.0.2.12:7000", server.NewId())
		})
		restoreCommits()
		closeObservation()
		if recovered == nil {
			t.Fatal("the connect committed despite the failing commit")
		}
		if statements.take("SET auth_time") != 1 {
			t.Fatal("the connect did not reach its auth_time refresh")
		}

		for _, clientId := range []server.Id{parentId, childIds[0]} {
			connect.AssertEqual(t, testGetNetworkClientAuthTime(ctx, clientId).Sub(staleTime).Abs() < time.Millisecond, true)
		}
		if clientAuthTimes.KnownFresh(childIds[0], server.NowUtc()) {
			t.Fatal("a refresh that did not commit was recorded as fresh")
		}

		_, _, _, _, err := ConnectNetworkClient(ctx, childIds[0], "192.0.2.12:7001", server.NewId())
		connect.AssertEqual(t, err, nil)
		for _, clientId := range []server.Id{parentId, childIds[0]} {
			age := server.NowUtc().Sub(testGetNetworkClientAuthTime(ctx, clientId))
			connect.AssertEqual(t, 0 <= age && age < time.Minute, true)
		}
	})
}

// The lifecycle boundary read remembers each contract client's top-level
// identity: the source client, or the client itself.
func TestContractLifecycleBoundaryRemembersTopLevelIdentities(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		Testing_ResetContractTimeStampGate()
		defer Testing_ResetContractTimeStampGate()

		networkId, parentId, childIds := testCreateParentWithChildren(ctx, t, 1)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(lockActiveContractClientsInTx(ctx, tx, networkId, childIds[0], networkId, parentId))
		})

		for clientId, want := range map[server.Id]server.Id{childIds[0]: parentId, parentId: parentId} {
			topLevelId, ok := contractClientTopLevels.Load(clientId)
			if !ok || topLevelId != want {
				t.Fatal("the lifecycle boundary did not remember a client's top-level identity")
			}
		}
	})
}

// A stamp for a payer this process never saw at a lifecycle boundary keys its
// gate by the payer, then learns the top-level identity from the database, so
// a sibling's later stamp is skipped.
func TestContractTimeStampLearnsTheTopLevelIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		Testing_ResetContractTimeStampGate()
		defer Testing_ResetContractTimeStampGate()

		_, parentId, childIds := testCreateParentWithChildren(ctx, t, 2)
		StampTopLevelClientContractTime(ctx, childIds[0])
		if topLevelId, ok := contractClientTopLevels.Load(childIds[0]); !ok || topLevelId != parentId {
			t.Fatal("the stamp did not learn the payer's top-level identity")
		}
		stamped := testGetNetworkClientContractTime(ctx, parentId)
		if marker, ok := contractTimeStampGate.Load(parentId); !ok || stamped == nil || !marker.Equal(*stamped) {
			t.Fatal("the gate did not record the marker the database holds")
		}

		// the sibling maps to the same top-level identity through the boundary
		rememberContractClientTopLevel(childIds[1], &parentId)
		testSetNetworkClientContractTime(ctx, parentId, server.NowUtc().Add(-2*clientAuthTimeRefreshMinInterval))
		StampTopLevelClientContractTime(ctx, childIds[1])
		unmoved := testGetNetworkClientContractTime(ctx, parentId)
		connect.AssertEqual(t, unmoved != nil && unmoved.Before(server.NowUtc().Add(-clientAuthTimeRefreshMinInterval)), true)
	})
}
