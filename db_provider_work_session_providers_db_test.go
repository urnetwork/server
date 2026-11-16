// The journal for provider clients only against PostgreSQL with the migrated
// schema (db_provider_work_session_providers.go), through the real connection
// trigger: a client that does not provide appends nothing and gets no head, a
// provider's connections append as before, a retire finds its admit after its
// client stops providing, and a client that starts providing while connected
// retires as a provider did before.
package server

import (
	"context"
	"testing"
	"time"
)

// provide_key modes, as model.ProvideMode stores them
const (
	testProvideModeNetwork          = 1
	testProvideModeFriendsAndFamily = 2
	testProvideModePublic           = 3
	testProvideModeStream           = 4
)

// A client with a network and a provide key in each of modes, the rows
// SetProvide writes.
func testProviderWorkSessionClientProviding(t testing.TB, ctx context.Context, modes ...int) (clientId Id, networkId Id) {
	clientId, networkId = NewId(), NewId()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`, clientId, networkId))
		for _, mode := range modes {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO provide_key (client_id, provide_mode, secret_key) VALUES ($1, $2, $3)`, clientId, mode, make([]byte, 32)))
		}
	})
	return
}

// Deletes the client's provide keys in modes, as a provide without them,
// a pause or a deactivation does.
func testProviderWorkSessionRemoveProvideKeys(t testing.TB, ctx context.Context, clientId Id, modes ...int) {
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key WHERE client_id = $1 AND provide_mode = ANY($2::int[])`, clientId, modes))
	})
}

// Closes a connection by deleting its row through the real row trigger.
func testProviderWorkSessionDeleteConnection(t testing.TB, ctx context.Context, connectionId Id) {
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_connection WHERE connection_id = $1`, connectionId))
	}, TxReadCommitted)
}

// The client's head sequence, or 0 when it has no head.
func testProviderWorkSessionHeadSequence(t testing.TB, ctx context.Context, clientId Id) int64 {
	return testContractPartitionScalar[int64](t, ctx, `SELECT COALESCE((SELECT sequence FROM provider_work_session_head WHERE client_id = $1), 0)`, clientId)
}

// What the append returns for one event, called as the trigger calls it.
func testProviderWorkSessionAppend(t testing.TB, ctx context.Context, clientId Id, connectionId Id, kind string) (sequence *int64) {
	Tx(ctx, func(tx PgTx) {
		Raise(tx.QueryRow(ctx, `SELECT provider_work_session_append($1, $2, $3, NULL)`, clientId, connectionId, kind).Scan(&sequence))
	}, TxReadCommitted)
	return
}

// Requires events to be the expected kinds from sequence 1, each with the
// network or without it.
func testProviderWorkSessionRequireEvents(t testing.TB, events []testProviderWorkSessionEvent, networkId Id, kinds []string, networks []bool) {
	t.Helper()
	testProviderWorkSessionRequireContiguous(t, events)
	if len(events) != len(kinds) {
		t.Fatalf("events %+v, want kinds %v", events, kinds)
	}
	for i, event := range events {
		if event.kind != kinds[i] || (event.networkId != nil) != networks[i] || (event.networkId != nil && *event.networkId != networkId) {
			t.Fatalf("event %d is %+v, want %s with network %t (all %+v)", i, event, kinds[i], networks[i], events)
		}
	}
}

// A client with no provide key, with Stream only (every client's return
// traffic) or with Network and Stream (a hosted window client) does not
// provide: its connections open and close through the real trigger, by update
// and by delete, and the journal gets no event and no head. The append, called
// as the trigger calls it, returns NULL for each kind. Before the change every
// client's connections appended an admit and a retire.
func TestProviderWorkSessionNonProviderAppendsNothing(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, modes := range [][]int{
			{},
			{testProvideModeStream},
			{testProvideModeNetwork, testProvideModeStream},
		} {
			clientId, _ := testProviderWorkSessionClientProviding(t, ctx, modes...)
			testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
			testProviderWorkSessionDeleteConnection(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
			requireNothing := func(step string) {
				t.Helper()
				if events := testProviderWorkSessionEvents(t, ctx, clientId); len(events) != 0 {
					t.Fatalf("modes %v, %s: a client that does not provide appended %+v", modes, step, events)
				}
				if sequence := testProviderWorkSessionHeadSequence(t, ctx, clientId); sequence != 0 {
					t.Fatalf("modes %v, %s: a client that does not provide has a head at %d", modes, step, sequence)
				}
			}
			requireNothing("connections opened and closed")
			if connected := testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM network_client_connection WHERE client_id = $1 AND connected`, clientId); connected != 0 {
				t.Fatalf("modes %v: %d connections stayed open", modes, connected)
			}

			open := testProviderWorkSessionConnect(t, ctx, clientId)
			for _, kind := range []string{"baseline", "admit", "retire"} {
				if sequence := testProviderWorkSessionAppend(t, ctx, clientId, open, kind); sequence != nil {
					t.Fatalf("modes %v: the %s append returned %d", modes, kind, *sequence)
				}
			}
			testProviderWorkSessionDisconnect(t, ctx, open)
			requireNothing("appends called directly")
		}
	})
}

// A client with a friends and family or public key provides: its connections
// open and close through the real trigger, by update and by delete, and append
// an admit and a retire each, both with the client's network, from sequence 1.
// The head follows the last event. This is what the append wrote before.
func TestProviderWorkSessionProvidersAppendAsBefore(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, modes := range [][]int{
			{testProvideModeFriendsAndFamily, testProvideModeNetwork, testProvideModeStream},
			{testProvideModePublic, testProvideModeFriendsAndFamily, testProvideModeNetwork, testProvideModeStream},
			{testProvideModePublic},
		} {
			clientId, networkId := testProviderWorkSessionClientProviding(t, ctx, modes...)
			testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
			testProviderWorkSessionDeleteConnection(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
			testProviderWorkSessionRequireEvents(t, testProviderWorkSessionEvents(t, ctx, clientId), networkId,
				[]string{"admit", "retire", "admit", "retire"}, []bool{true, true, true, true})
			if sequence := testProviderWorkSessionHeadSequence(t, ctx, clientId); sequence != 4 {
				t.Fatalf("modes %v: head %d", modes, sequence)
			}
		}
	})
}

// A connection admitted while its client provided retires with the admit's
// network after the client stops providing, whether its friends and family
// and public keys went alone (a provide without them, or a pause) or with
// every key (a deactivation), and whether the row is updated or deleted. A
// connection whose admit the append wrote before the change, for a client that
// does not provide, retires with that admit's network too. Its later
// connections append nothing.
func TestProviderWorkSessionRetireFindsItsAdmitAfterProvidingStops(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		allModes := []int{testProvideModePublic, testProvideModeFriendsAndFamily, testProvideModeNetwork, testProvideModeStream}

		paused, pausedNetworkId := testProviderWorkSessionClientProviding(t, ctx, allModes...)
		pausedConnection := testProviderWorkSessionConnect(t, ctx, paused)
		testProviderWorkSessionRemoveProvideKeys(t, ctx, paused, testProvideModePublic, testProvideModeFriendsAndFamily)
		testProviderWorkSessionDisconnect(t, ctx, pausedConnection)
		testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, paused))
		testProviderWorkSessionRequireEvents(t, testProviderWorkSessionEvents(t, ctx, paused), pausedNetworkId,
			[]string{"admit", "retire"}, []bool{true, true})

		deactivated, deactivatedNetworkId := testProviderWorkSessionClientProviding(t, ctx, allModes...)
		deactivatedConnection := testProviderWorkSessionConnect(t, ctx, deactivated)
		testProviderWorkSessionRemoveProvideKeys(t, ctx, deactivated, allModes...)
		testProviderWorkSessionDeleteConnection(t, ctx, deactivatedConnection)
		testProviderWorkSessionRequireEvents(t, testProviderWorkSessionEvents(t, ctx, deactivated), deactivatedNetworkId,
			[]string{"admit", "retire"}, []bool{true, true})

		// a window client's connection admitted by the append before the change
		windowClient, windowNetworkId := testProviderWorkSessionClientProviding(t, ctx, testProvideModeNetwork, testProvideModeStream)
		windowConnection := testProviderWorkSessionOpenWithAdmit(t, ctx, windowClient, windowNetworkId, 1, NowUtc().Add(-time.Hour))
		testProviderWorkSessionDisconnect(t, ctx, windowConnection)
		testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, windowClient))
		testProviderWorkSessionRequireEvents(t, testProviderWorkSessionEvents(t, ctx, windowClient), windowNetworkId,
			[]string{"admit", "retire"}, []bool{true, true})
		if sequence := testProviderWorkSessionHeadSequence(t, ctx, windowClient); sequence != 2 {
			t.Fatalf("window client head %d", sequence)
		}
	})
}

// A client that starts providing while connected has no admit for that
// connection; it retires as a provider's connection without an admit did
// before, without a network id, and its next connection appends an admit and
// a retire with its network.
func TestProviderWorkSessionClientThatStartsProvidingWhileConnected(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId, networkId := testProviderWorkSessionClientProviding(t, ctx, testProvideModeNetwork, testProvideModeStream)
		connectionId := testProviderWorkSessionConnect(t, ctx, clientId)
		if events := testProviderWorkSessionEvents(t, ctx, clientId); len(events) != 0 {
			t.Fatalf("a client that does not provide appended %+v", events)
		}
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO provide_key (client_id, provide_mode, secret_key) VALUES ($1, $2, $3)`, clientId, testProvideModePublic, make([]byte, 32)))
		})
		testProviderWorkSessionDisconnect(t, ctx, connectionId)
		testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
		testProviderWorkSessionRequireEvents(t, testProviderWorkSessionEvents(t, ctx, clientId), networkId,
			[]string{"retire", "admit", "retire"}, []bool{false, true, true})
	})
}

// The unbounded fallback stays a provider's: a provider's connection id from
// no clock still finds its admit there, and a client that does not provide
// skips it, so its retire is not appended.
func TestProviderWorkSessionFallbackLookupOnlyForProviders(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		unordered := Id{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x70, 0x00, 0x80}
		admittedAt := NowUtc().Add(-time.Hour)

		provider, providerNetworkId := testProviderWorkSessionClientProviding(t, ctx, testProvideModePublic)
		testProviderWorkSessionInsertEvent(t, ctx, provider, &providerNetworkId, &unordered, 1, "admit", admittedAt)
		if sequence := testProviderWorkSessionAppend(t, ctx, provider, unordered, "retire"); sequence == nil || *sequence != 2 {
			t.Fatalf("the provider's retire returned %v", sequence)
		}
		testProviderWorkSessionRequireEvents(t, testProviderWorkSessionEvents(t, ctx, provider), providerNetworkId,
			[]string{"admit", "retire"}, []bool{true, true})

		other, otherNetworkId := testProviderWorkSessionClientProviding(t, ctx, testProvideModeNetwork, testProvideModeStream)
		testProviderWorkSessionInsertEvent(t, ctx, other, &otherNetworkId, &unordered, 1, "admit", admittedAt)
		if sequence := testProviderWorkSessionAppend(t, ctx, other, unordered, "retire"); sequence != nil {
			t.Fatalf("the retire of a client that does not provide returned %d", *sequence)
		}
		testProviderWorkSessionRequireEvents(t, testProviderWorkSessionEvents(t, ctx, other), otherNetworkId,
			[]string{"admit"}, []bool{true})
	})
}
