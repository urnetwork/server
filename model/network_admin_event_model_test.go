//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §7.11's admin event log, which needs MG1.

func testAdminEvent(networkId server.Id, action string) *AdminEvent {
	userId := server.NewId()
	return &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorUser,
		ActorId:    &userId,
		Action:     action,
		TargetKind: AdminEventTargetNetwork,
		TargetId:   &networkId,
		Result:     AdminEventResultOk,
	}
}

// Writers of one network that all took their snapshot before any of them
// appended, some under repeatable read and some under read committed, still
// leave one chain: seqs 1 to n, each chained to the last.
func TestAdminEventChainAcrossConcurrentWriters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		// another network's chain is its own
		server.Tx(ctx, func(tx server.PgTx) {
			AddAdminEventInTx(ctx, tx, testAdminEvent(otherNetworkId, AdminEventActionNetworkRename))
		})

		writerCount := 6
		arrived := sync.WaitGroup{}
		arrived.Add(writerCount)
		release := make(chan struct{})
		done := sync.WaitGroup{}
		done.Add(writerCount)
		failures := make(chan any, writerCount)
		for i := range writerCount {
			go func() {
				defer done.Done()
				defer func() {
					if r := recover(); r != nil {
						failures <- r
					}
				}()
				options := []any{}
				if i%2 == 1 {
					options = append(options, server.TxReadCommitted)
				}
				attempt := 0
				server.Tx(ctx, func(tx server.PgTx) {
					attempt += 1
					// the snapshot, before the barrier
					var count int
					server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM network_admin_event WHERE network_id = $1`, networkId).Scan(&count))
					if attempt == 1 {
						arrived.Done()
						<-release
					}
					AddAdminEventInTx(ctx, tx, testAdminEvent(networkId, AdminEventActionNetworkRename))
				}, options...)
			}()
		}
		arrived.Wait()
		close(release)
		done.Wait()
		close(failures)
		for failure := range failures {
			t.Fatalf("a writer failed: %v", failure)
		}

		events := GetNetworkAdminEvents(ctx, networkId, 0, 0)
		if len(events) != writerCount {
			t.Fatalf("%d events, want %d", len(events), writerCount)
		}
		for i, event := range events {
			if event.Seq != int64(i+1) {
				t.Fatalf("event %d has seq %d", i, event.Seq)
			}
		}
		if err := VerifyNetworkAdminEventChain(ctx, networkId); err != nil {
			t.Fatal(err)
		}
		if err := VerifyNetworkAdminEventChain(ctx, otherNetworkId); err != nil {
			t.Fatal(err)
		}
	})
}

// A changed field, a removed event or a swapped predecessor fails verification.
func TestAdminEventChainRefusesATamperedLog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		for range 3 {
			server.Tx(ctx, func(tx server.PgTx) {
				AddAdminEventInTx(ctx, tx, testAdminEvent(networkId, AdminEventActionAuthKeyCreate))
			})
		}
		events := GetNetworkAdminEvents(ctx, networkId, 0, 0)
		if err := verifyAdminEventChain(events); err != nil {
			t.Fatal(err)
		}

		changed := *events[1]
		changed.Action = AdminEventActionAuthKeyRevoke
		if err := verifyAdminEventChain([]*NetworkAdminEvent{events[0], &changed, events[2]}); !errors.Is(err, errAdminEventChainBroken) {
			t.Fatalf("a changed action verified: %v", err)
		}
		if err := verifyAdminEventChain([]*NetworkAdminEvent{events[0], events[2]}); !errors.Is(err, errAdminEventChainBroken) {
			t.Fatalf("a removed event verified: %v", err)
		}
		reanchored := *events[0]
		reanchored.PrevSha256 = events[2].ChainSha256
		if err := verifyAdminEventChain([]*NetworkAdminEvent{&reanchored}); !errors.Is(err, errAdminEventChainBroken) {
			t.Fatalf("a first event off the genesis value verified: %v", err)
		}
		// a run from the middle anchors on its first event
		if err := verifyAdminEventChain(events[1:]); err != nil {
			t.Fatal(err)
		}
	})
}

// The sweep removes an event 400 days past its time and keeps one a moment
// younger; the rest of the chain still verifies from its oldest row.
func TestRemoveOldNetworkAdminEventsAt400Days(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		retention := DefaultNetworkAdminEventSettings().Retention
		if retention != 400*24*time.Hour {
			t.Fatalf("retention = %s", retention)
		}
		now := server.NowUtc()
		cutoff := now.Add(-retention)
		for _, eventTime := range []time.Time{
			cutoff.Add(-time.Microsecond),
			cutoff,
			cutoff.Add(time.Microsecond),
			now,
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				addAdminEventInTxAt(ctx, tx, testAdminEvent(networkId, AdminEventActionNetworkRename), eventTime)
			})
		}

		removedCount := RemoveOldNetworkAdminEvents(ctx, cutoff)
		if removedCount != 1 {
			t.Fatalf("removed %d events, want 1", removedCount)
		}
		events := GetNetworkAdminEvents(ctx, networkId, 0, 0)
		if len(events) != 3 || events[0].Seq != 2 || !events[0].EventTime.Equal(cutoff) {
			t.Fatalf("kept %+v", events)
		}
		if err := VerifyNetworkAdminEventChain(ctx, networkId); err != nil {
			t.Fatal(err)
		}
	})
}

// An event never takes a time below its predecessor's, so seq order and time
// order agree for the sweep.
func TestAdminEventTimeNeverFalls(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		var first, second *NetworkAdminEvent
		server.Tx(ctx, func(tx server.PgTx) {
			first = addAdminEventInTxAt(ctx, tx, testAdminEvent(networkId, AdminEventActionNetworkRename), now)
		})
		server.Tx(ctx, func(tx server.PgTx) {
			second = addAdminEventInTxAt(ctx, tx, testAdminEvent(networkId, AdminEventActionNetworkRename), now.Add(-time.Hour))
		})
		if !second.EventTime.Equal(first.EventTime) || second.Seq != 2 {
			t.Fatalf("first %+v, second %+v", first, second)
		}
	})
}

// A session's event names its request, and its actor: an API key over the token,
// a client over its user.
func TestSessionAdminEventActorAndRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		apiKeyId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})
		if kind, id := AdminEventActorOf(userSession); kind != AdminEventActorUser || *id != userId {
			t.Fatalf("user session actor %s %v", kind, id)
		}
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, ClientId: &clientId})
		if kind, id := AdminEventActorOf(clientSession); kind != AdminEventActorClient || *id != clientId {
			t.Fatalf("client session actor %s %v", kind, id)
		}
		apiKeySession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})
		apiKeySession.ApiKeyAuthenticated = true
		apiKeySession.ApiKeyId = &apiKeyId
		if kind, id := AdminEventActorOf(apiKeySession); kind != AdminEventActorApiKey || *id != apiKeyId {
			t.Fatalf("api key session actor %s %v", kind, id)
		}

		// a local session made from no request has no request id
		if event := NewSessionAdminEvent(userSession, AdminEventActionNetworkRename, AdminEventTargetNetwork, &networkId); event.RequestId != nil {
			t.Fatalf("a local session's event names request %s", event.RequestId)
		}
	})
}
