package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestClientCloseKeyEvents(t *testing.T) {
	clientId := server.NewId()

	removedChannel := "__keyspace@0__:" + clientRemovedKey(clientId)
	if parsed, ok := ParseClientRemovedKeyEvent(removedChannel); !ok || parsed != clientId {
		t.Fatalf("removed channel parsed to %s %t", parsed, ok)
	}
	closeChannel := "__keyspace@0__:" + clientCloseKey(clientId)
	if parsed, ok := ParseClientCloseKeyEvent(closeChannel); !ok || parsed != clientId {
		t.Fatalf("close channel parsed to %s %t", parsed, ok)
	}

	// each parser accepts only its own suffix
	if _, ok := ParseClientRemovedKeyEvent(closeChannel); ok {
		t.Fatal("removed parser accepted a close channel")
	}
	if _, ok := ParseClientCloseKeyEvent(removedChannel); ok {
		t.Fatal("close parser accepted a removed channel")
	}
	if _, ok := ParseClientCloseKeyEvent("__keyspace@0__:" + clientStreamHopsKey(clientId)); ok {
		t.Fatal("close parser accepted a stream hops channel")
	}
	if _, ok := ParseClientRemovedKeyEvent("__keyspace@0__:{not-an-id}c_removed"); ok {
		t.Fatal("removed parser accepted a malformed id")
	}

	if ClientRemovedKeyEventPattern(3) != "__keyspace@3__:{*}c_removed" {
		t.Fatalf("removed pattern %s", ClientRemovedKeyEventPattern(3))
	}
	if ClientCloseKeyEventPattern(3) != "__keyspace@3__:{*}c_close" {
		t.Fatalf("close pattern %s", ClientCloseKeyEventPattern(3))
	}
}

func TestClientCloseRequestsAreTakenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		contractIds := []server.Id{server.NewId(), server.NewId()}
		peerId := server.NewId()

		AddClientCloseRequests(ctx, clientId, contractIds, []server.Id{peerId})
		// a repeated request is one request
		AddClientCloseRequests(ctx, clientId, contractIds[:1], nil)

		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.PTTL(ctx, clientCloseKey(clientId)).Result()
			server.Raise(err)
		})
		if ttl <= 0 || ClientCloseTtl < ttl {
			t.Fatalf("close requests ttl %s, want (0, %s]", ttl, ClientCloseTtl)
		}

		requests := TakeClientCloseRequests(ctx, clientId, ClientCloseTakeLimit)
		if !sameIds(requests.ContractIds, contractIds) || !sameIds(requests.PeerIds, []server.Id{peerId}) {
			t.Fatalf("took %d contracts and %d peers, want 2 and 1", len(requests.ContractIds), len(requests.PeerIds))
		}
		if again := TakeClientCloseRequests(ctx, clientId, ClientCloseTakeLimit); !again.Empty() {
			t.Fatalf("a second take found %d contracts and %d peers", len(again.ContractIds), len(again.PeerIds))
		}

		// the limit bounds one take
		AddClientCloseRequests(ctx, clientId, contractIds, nil)
		if first := TakeClientCloseRequests(ctx, clientId, 1); len(first.ContractIds) != 1 {
			t.Fatalf("a take of 1 took %d", len(first.ContractIds))
		}
		if rest := TakeClientCloseRequests(ctx, clientId, ClientCloseTakeLimit); len(rest.ContractIds) != 1 {
			t.Fatalf("the rest was %d", len(rest.ContractIds))
		}
	})
}

// The sender's final asks the destination to send its final, unless the
// destination has already sent one (CLIENTCLOSE1.md section 3).
func TestSourceFinalAsksDestinationToClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const byteCount = ByteCount(11)

		newContract := func() server.Id {
			escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 40)
			server.RunPosts(ctx, posts...)
			return escrow.ContractId
		}
		report := func(contractId server.Id, clientId server.Id, checkpoint bool) {
			if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: contractId, ClientId: clientId, AckedByteCount: byteCount, Checkpoint: checkpoint}); err != nil {
				t.Fatal(err)
			}
		}
		takeDestination := func() *ClientCloseRequests {
			return TakeClientCloseRequests(ctx, f.destinationId, ClientCloseTakeLimit)
		}

		// the receiver checkpointed at its sequence exit; the sender's final asks
		// it to finish
		checkpointed := newContract()
		report(checkpointed, f.destinationId, true)
		if requests := takeDestination(); !requests.Empty() {
			t.Fatal("a receiver checkpoint asked the receiver to close")
		}
		report(checkpointed, f.sourceId, false)
		if requests := takeDestination(); !slices.Equal(requests.ContractIds, []server.Id{checkpointed}) || len(requests.PeerIds) != 0 {
			t.Fatalf("after the sender's final the receiver was asked to close %d contracts and %d peers, want the contract", len(requests.ContractIds), len(requests.PeerIds))
		}
		if requests := TakeClientCloseRequests(ctx, f.sourceId, ClientCloseTakeLimit); !requests.Empty() {
			t.Fatal("the sender was asked to close its own final")
		}

		// the receiver's zero final after the request settles the contract on
		// reports, with the checkpoint's count
		if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: checkpointed, ClientId: f.destinationId}); err != nil {
			t.Fatal(err)
		}
		var used ByteCount
		var settled bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT used_transfer_byte_count,
				(SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1)
				FROM contract_close WHERE contract_id=$1 AND party=$2 AND NOT checkpoint`, checkpointed, ContractPartyDestination).Scan(&used, &settled))
		})
		if used != byteCount || !settled {
			t.Fatalf("receiver zero final: bytes=%d settled=%t, want %d and settled", used, settled, byteCount)
		}

		// a receiver that has no report yet is asked too
		unreported := newContract()
		report(unreported, f.sourceId, false)
		if requests := takeDestination(); !slices.Equal(requests.ContractIds, []server.Id{unreported}) {
			t.Fatalf("an unreported receiver was asked to close %d contracts, want 1", len(requests.ContractIds))
		}

		// a receiver that already sent its final is not asked
		final := newContract()
		report(final, f.destinationId, false)
		report(final, f.sourceId, false)
		if requests := takeDestination(); !requests.Empty() {
			t.Fatal("a receiver that already sent its final was asked to close")
		}

		// a sender checkpoint asks nothing
		senderCheckpoint := newContract()
		report(senderCheckpoint, f.sourceId, true)
		if requests := takeDestination(); !requests.Empty() {
			t.Fatal("a sender checkpoint asked the receiver to close")
		}

		// a repeated sender final asks nothing more
		repeated := newContract()
		repeatedReport := ContractCloseReport{ReportId: server.NewId(), ContractId: repeated, ClientId: f.sourceId, AckedByteCount: byteCount}
		if _, err := CloseContractWithReport(ctx, repeatedReport); err != nil {
			t.Fatal(err)
		}
		takeDestination()
		if _, err := CloseContractWithReport(ctx, repeatedReport); err != nil {
			t.Fatal(err)
		}
		if requests := takeDestination(); !requests.Empty() {
			t.Fatal("a retried sender final asked the receiver again")
		}

		// the legacy id-less report path asks the same
		legacy := newContract()
		if err := CloseContract(ctx, legacy, f.sourceId, byteCount, false); err != nil {
			t.Fatal(err)
		}
		if requests := takeDestination(); !slices.Equal(requests.ContractIds, []server.Id{legacy}) {
			t.Fatalf("a legacy sender final asked the receiver to close %d contracts, want 1", len(requests.ContractIds))
		}
	})
}

// An api removal sets the client's removed flag and asks each counterparty to
// close its open contracts and sequences with the client (CLIENTCLOSE1.md
// section 4).
func TestRemoveNetworkClientAsksCounterpartiesToClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)

		open, posts := createNetEscrowOrderingTestContract(ctx, f, 40)
		server.RunPosts(ctx, posts...)
		settled, posts := createNetEscrowOrderingTestContract(ctx, f, 40)
		server.RunPosts(ctx, posts...)
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			if _, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: settled.ContractId, ClientId: clientId, AckedByteCount: 1}); err != nil {
				t.Fatal(err)
			}
		}
		TakeClientCloseRequests(ctx, f.destinationId, ClientCloseTakeLimit)

		if IsClientRemoved(ctx, f.sourceId) {
			t.Fatal("an active client is flagged removed")
		}
		ownerSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: f.sourceNetworkId,
			UserId:    server.NewId(),
		})
		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: f.sourceId}, ownerSession)
		if err != nil || result == nil || result.Error != nil {
			t.Fatalf("remove client: %v", err)
		}
		if !IsClientRemoved(ctx, f.sourceId) {
			t.Fatal("the removed client is not flagged removed")
		}
		if IsClientRemoved(ctx, f.destinationId) {
			t.Fatal("the counterparty is flagged removed")
		}

		// the open contract's read runs off the request path
		var requests *ClientCloseRequests
		deadline := time.Now().Add(10 * time.Second)
		for {
			requests = TakeClientCloseRequests(ctx, f.destinationId, ClientCloseTakeLimit)
			if !requests.Empty() || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		if !slices.Equal(requests.ContractIds, []server.Id{open.ContractId}) || !slices.Equal(requests.PeerIds, []server.Id{f.sourceId}) {
			t.Fatalf("the counterparty was asked to close %d contracts and %d peers, want the open contract and the removed client", len(requests.ContractIds), len(requests.PeerIds))
		}
	})
}

// A process that may have missed events checks the flag directly.
func TestClientsRemovedFlag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		removed := server.NewId()
		kept := server.NewId()
		SetClientsRemoved(ctx, []server.Id{removed})
		if !IsClientRemoved(ctx, removed) || IsClientRemoved(ctx, kept) {
			t.Fatal("removed flag mismatch")
		}
		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.PTTL(ctx, clientRemovedKey(removed)).Result()
			server.Raise(err)
		})
		if ttl <= 0 || ClientRemovedTtl < ttl {
			t.Fatalf("removed flag ttl %s, want (0, %s]", ttl, ClientRemovedTtl)
		}
	})
}

func sameIds(a []server.Id, b []server.Id) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.SortFunc(a, func(x server.Id, y server.Id) int { return x.Cmp(y) })
	slices.SortFunc(b, func(x server.Id, y server.Id) int { return x.Cmp(y) })
	return slices.Equal(a, b)
}
