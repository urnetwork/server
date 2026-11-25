// A reply carrier, the companion that answers a companion, is paid by the
// network whose forward contract the chain answers. A refusal that an
// exhaustion proof decides runs its recovery page and the rerun after it only
// as its payer's first in a hold.
package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// Asserts the returned and the durable funding of one contract: its anchor, its
// payer network, and one escrow row of want bytes on the balance.
func requireReplyCarrierTestFunding(t testing.TB, ctx context.Context, name string, escrow *TransferEscrow, err error,
	origin *server.Id, payerNetworkId server.Id, balanceId server.Id, want ByteCount) {
	t.Helper()
	if err != nil || escrow == nil {
		t.Fatalf("%s: refused: %v", name, err)
	}
	if (escrow.CompanionContractId == nil) != (origin == nil) || origin != nil && *escrow.CompanionContractId != *origin {
		t.Fatalf("%s: anchored on %v, want %v", name, escrow.CompanionContractId, origin)
	}
	if escrow.TransferByteCount != want || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != balanceId || escrow.Balances[0].BalanceByteCount != want {
		t.Fatalf("%s: escrow %d from %+v, want %d from %s", name, escrow.TransferByteCount, escrow.Balances, want, balanceId)
	}
	server.Db(ctx, func(conn server.PgConn) {
		var payer *server.Id
		var escrowRows int
		var escrowBalanceId *server.Id
		var escrowByteCount *ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT c.payer_network_id,
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=c.contract_id),
			(SELECT balance_id FROM transfer_escrow WHERE contract_id=c.contract_id LIMIT 1),
			(SELECT balance_byte_count FROM transfer_escrow WHERE contract_id=c.contract_id LIMIT 1)
			FROM transfer_contract c WHERE c.contract_id=$1`, escrow.ContractId).Scan(&payer, &escrowRows, &escrowBalanceId, &escrowByteCount))
		if payer == nil || *payer != payerNetworkId || escrowRows != 1 || *escrowBalanceId != balanceId || *escrowByteCount != want {
			t.Fatalf("%s: durable payer %v with %d escrow rows on %v for %v, want payer %s with %d on %s",
				name, payer, escrowRows, escrowBalanceId, escrowByteCount, payerNetworkId, want, balanceId)
		}
	})
}

// The provider fleet's only grant is held whole, as open reply carriers held
// four fleets' daily grants on 2026-11-25, and the client has balance. The
// client's forward contract and the provider's return companion are billed to
// the client, as before. The client's reply carrier chains to the return
// companion and is billed to the client too, on the client's grant. Billed to
// its destination, the provider's network, it was refused.
func TestCompanionReplyCarrierBillsTheForwardPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		// the payer is the client network, the provider the provider fleet
		clients := newEscrowSelectionTestClients(t, ctx)
		clientBalance := addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
		providerGrantIds := redisSelectionTestGrants(t, ctx, escrowSelectionTestClients{payerNetworkId: clients.providerNetworkId}, 1, 4096, false)
		redisGrantProbeToken(t, ctx, providerGrantIds[0], 4096, 4096)
		providerHeld := Testing_NetEscrowByteCount(ctx, providerGrantIds[0])

		forward, err := CreateTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
			clients.providerNetworkId, clients.providerId, 1024)
		requireReplyCarrierTestFunding(t, ctx, "forward", forward, err, nil, clients.payerNetworkId, clientBalance.BalanceId, 1024)
		back, err := CreateCompanionTransferEscrow(ctx, clients.providerNetworkId, clients.providerId,
			clients.payerNetworkId, clients.payerId, 1024, time.Hour)
		requireReplyCarrierTestFunding(t, ctx, "return companion", back, err, &forward.ContractId, clients.payerNetworkId, clientBalance.BalanceId, 1024)

		clientBefore := GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId)
		reply, err := CreateCompanionTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
			clients.providerNetworkId, clients.providerId, 512, time.Hour)
		requireReplyCarrierTestFunding(t, ctx, "reply carrier", reply, err, &back.ContractId, clients.payerNetworkId, clientBalance.BalanceId, 512)
		if spent := clientBefore - GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId); spent != 512 {
			t.Fatalf("the reply carrier reserved %d of the client's balance, want 512", spent)
		}
		if held := Testing_NetEscrowByteCount(ctx, providerGrantIds[0]); held != providerHeld {
			t.Fatalf("the reply carrier changed the provider's held grant from %d to %d", providerHeld, held)
		}
	})
}

// Only the carrier's own network is inherited. A companion with a plain origin
// stays billed to its destination even when that destination is exhausted and
// its source has balance. An anchor whose payer is not the carrier's source,
// or whose networks are not the pair's reversed, leaves the destination paying,
// so a requester can never move a contract onto another network.
func TestCompanionReplyCarrierInheritsOnlyItsOwnNetwork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)

		// an exhausted destination: its plain-origin companion is refused, and
		// the funded source pays nothing
		exhausted := newEscrowSelectionTestClients(t, ctx)
		exhaustedIds := redisSelectionTestGrants(t, ctx, exhausted, 1, 4096, false)
		addContractPayoutTestBalance(ctx, exhausted.providerNetworkId, 8192)
		forward, err := CreateTransferEscrow(ctx, exhausted.payerNetworkId, exhausted.payerId,
			exhausted.providerNetworkId, exhausted.providerId, 1024)
		requireReplyCarrierTestFunding(t, ctx, "exhausted forward", forward, err, nil, exhausted.payerNetworkId, exhaustedIds[0], 1024)
		redisGrantProbeToken(t, ctx, exhaustedIds[0], 4096, 3072)
		providerBefore := GetActiveTransferBalanceByteCount(ctx, exhausted.providerNetworkId)
		if back, err := CreateCompanionTransferEscrow(ctx, exhausted.providerNetworkId, exhausted.providerId,
			exhausted.payerNetworkId, exhausted.payerId, 1024, time.Hour); back != nil || !errors.Is(err, ErrContractBalanceExhausted) {
			t.Fatalf("a plain-origin companion of an exhausted destination = %+v, %v; want its destination's refusal", back, err)
		}
		if GetActiveTransferBalanceByteCount(ctx, exhausted.providerNetworkId) != providerBefore {
			t.Fatal("a plain-origin companion charged its source")
		}

		for _, change := range []string{"payer", "source-network", "destination-network"} {
			clients := newEscrowSelectionTestClients(t, ctx)
			clientBalance := addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
			providerBalance := addContractPayoutTestBalance(ctx, clients.providerNetworkId, 8192)
			forward, err := CreateTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
				clients.providerNetworkId, clients.providerId, 1024)
			requireReplyCarrierTestFunding(t, ctx, change+" forward", forward, err, nil, clients.payerNetworkId, clientBalance.BalanceId, 1024)
			back, err := CreateCompanionTransferEscrow(ctx, clients.providerNetworkId, clients.providerId,
				clients.payerNetworkId, clients.payerId, 1024, time.Hour)
			requireReplyCarrierTestFunding(t, ctx, change+" return companion", back, err, &forward.ContractId, clients.payerNetworkId, clientBalance.BalanceId, 1024)
			server.Tx(ctx, func(tx server.PgTx) {
				switch change {
				case "payer":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, back.ContractId, clients.providerNetworkId))
				case "source-network":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET source_network_id=$2 WHERE contract_id=$1`, back.ContractId, server.NewId()))
				case "destination-network":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET destination_network_id=$2 WHERE contract_id=$1`, back.ContractId, server.NewId()))
				}
			})
			clientBefore := GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId)
			reply, err := CreateCompanionTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
				clients.providerNetworkId, clients.providerId, 512, time.Hour)
			requireReplyCarrierTestFunding(t, ctx, change+" reply carrier", reply, err, &back.ContractId, clients.providerNetworkId, providerBalance.BalanceId, 512)
			if GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId) != clientBefore {
				t.Fatalf("%s: a carrier whose anchor is not its own network's charged its source", change)
			}
		}
	})
}

// Every counter a refusal's compensation and recovery page can move.
func replyCarrierTestRecoveryCounts() map[string]float64 {
	counts := redisExhaustionCounts()
	counts["recovery skipped"] = testutil.ToFloat64(redisContractReservationResults.WithLabelValues("recovery", "skipped"))
	return counts
}

func replyCarrierTestRecoveryCountsSince(before map[string]float64) map[string]float64 {
	delta := map[string]float64{}
	for name, value := range replyCarrierTestRecoveryCounts() {
		if difference := value - before[name]; difference != 0 {
			delta[name] = difference
		}
	}
	return delta
}

// The client's return companions are refused for the client's exhaustion, as
// four fleets' requests were on 2026-11-25. A live writer's aged token holds
// the client's grant, so every recovery page finds it and holds a PostgreSQL
// transaction for its custody check, which fails while the writer holds its
// fence: the incident's recovery pending.
//
// The first proven refusal in the payer's hold runs its page. Every later one
// in the hold runs neither the page nor the rerun, even once the writer has
// died and the page would release its token. The first proven refusal of the
// next hold runs the page again, which releases the token, and its rerun funds
// that request.
func TestCompanionRefusalOfAKnownExhaustedPayerRunsNoRecoveryPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		withRedisExhaustedPayersForTest(t)
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 1, 1000, false)
		forward, err := CreateTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
			clients.providerNetworkId, clients.providerId, 100)
		requireReplyCarrierTestFunding(t, ctx, "forward", forward, err, nil, clients.payerNetworkId, ids[0], 100)

		// a writer holds the rest of the grant, aged past the pending window,
		// and keeps its fence until it dies below
		writer := server.NewId()
		if reserved, err := redisContractReservation(ctx, "reserve-pending", ids[0], writer, 1000, 900, redisContractReservationLease); err != nil || reserved != 900 {
			t.Fatal("writer setup", reserved, err)
		}
		redisRecoveryAgePendingMarker(t, ctx, ids[0], writer)
		fenced := make(chan struct{})
		die := make(chan struct{})
		dead := make(chan struct{})
		go func() {
			defer close(dead)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, redisContractAdmissionLock(writer)))
				close(fenced)
				<-die
			})
		}()
		select {
		case <-fenced:
		case <-dead:
			t.Fatal("the writer did not take its fence")
		}
		killWriter := func() {
			select {
			case <-die:
			default:
				close(die)
			}
			<-dead
		}
		defer killWriter()

		// a census in this process just refused the client
		exhaustedRedisPayers.note(clients.payerNetworkId, time.Now())

		type outcome struct {
			escrow *TransferEscrow
			owner  *redisContractAdmission
			counts map[string]float64
			err    error
		}
		request := func() outcome {
			admitted := withRedisContractAdmission(ctx)
			before := replyCarrierTestRecoveryCounts()
			escrow, err := CreateCompanionTransferEscrow(admitted, clients.providerNetworkId, clients.providerId,
				clients.payerNetworkId, clients.payerId, 200, time.Hour)
			drainRedisContractCompensations(t, ctx)
			return outcome{escrow: escrow, owner: redisAdmissionFromContext(admitted), counts: replyCarrierTestRecoveryCountsSince(before), err: err}
		}
		requireRefused := func(name string, got outcome, page bool) {
			t.Helper()
			if got.escrow != nil || !errors.Is(got.err, ErrContractBalanceExhausted) {
				t.Fatalf("%s: %+v, %v; want the exhausted payer's refusal", name, got.escrow, got.err)
			}
			if got.counts["exhaustion-proof proven"] != 1 {
				t.Fatalf("%s: the proof did not decide the refusal: %v", name, got.counts)
			}
			if got.owner.recoveryRerun() {
				t.Fatalf("%s: the refusal reran", name)
			}
			for _, counter := range []string{"release-pending accepted", "compensation completed", "compensation deferred", "compensation pending"} {
				if got.counts[counter] != 0 {
					t.Fatalf("%s: the proven refusal still did %s: %v", name, counter, got.counts)
				}
			}
			if page {
				if got.counts["recovery pending"] != 1 || got.counts["recovery skipped"] != 0 {
					t.Fatalf("%s: the payer's first page in the hold did not check the busy writer: %v", name, got.counts)
				}
			} else if got.counts["recovery pending"] != 0 || got.counts["recovery completed"] != 0 || got.counts["recovery skipped"] != 1 {
				t.Fatalf("%s: a later proven refusal in the hold ran its recovery page: %v", name, got.counts)
			}
		}
		held := func() ByteCount {
			return Testing_NetEscrowByteCount(ctx, ids[0])
		}
		if held() != 1000 {
			t.Fatal("the grant is not held whole", held())
		}

		first := request()
		requireRefused("first", first, true)
		second := request()
		requireRefused("second", second, false)
		if second.err.Error() != first.err.Error() {
			t.Fatalf("the refusals differ: %q and %q", first.err, second.err)
		}

		// the writer dies: a page would now release its token and rerun
		killWriter()
		third := request()
		requireRefused("after the writer died", third, false)
		if held() != 1000 {
			t.Fatal("a skipped page released the dead writer's token", held())
		}

		// the next hold: the payer's page is due again
		replyCarrierTestNextPageHold(t, clients.payerNetworkId)
		next := request()
		requireReplyCarrierTestFunding(t, ctx, "next hold", next.escrow, next.err, &forward.ContractId, clients.payerNetworkId, ids[0], 200)
		if !next.owner.recoveryRerun() || next.counts["recovery completed"] == 0 || next.counts["recovery skipped"] != 0 {
			t.Fatalf("the next hold's page did not release the dead token and rerun: %v", next.counts)
		}
		if got := held(); got != 300 {
			t.Fatalf("the grant holds %d after recovery, want the forward 100 and the companion 200", got)
		}
	})
}

// Moves the payer's last page a hold back, as if the next hold had begun.
func replyCarrierTestNextPageHold(t testing.TB, payerNetworkId server.Id) {
	t.Helper()
	exhaustedRedisPayers.stateLock.Lock()
	defer exhaustedRedisPayers.stateLock.Unlock()
	hold, ok := exhaustedRedisPayers.payerIdHolds[payerNetworkId]
	if !ok || hold.pageTime.IsZero() {
		t.Fatal("the payer's page time was not recorded")
	}
	hold.pageTime = hold.pageTime.Add(-exhaustedRedisPayers.hold)
	exhaustedRedisPayers.payerIdHolds[payerNetworkId] = hold
}

// Proven refusals of one payer that race for its page take it once: exactly
// one of them runs a page in the hold, and another payer's page is its own.
func TestRedisExhaustedPayersGiveConcurrentRefusalsOnePage(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	payers := newRedisExhaustedPayers(5*time.Second, 4)
	payer, other := server.NewId(), server.NewId()
	payers.note(payer, now)
	payers.note(other, now)
	const refusals = 64
	var wait sync.WaitGroup
	var pages atomic.Int64
	start := make(chan struct{})
	for range refusals {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if payers.takeRecoveryPage(payer, now) {
				pages.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := pages.Load(); got != 1 {
		t.Fatalf("%d concurrent proven refusals ran %d pages, want 1", refusals, got)
	}
	if !payers.takeRecoveryPage(other, now) {
		t.Fatal("another payer's page was taken")
	}
}

// One proven refusal a hold runs its page. A proof's listing keeps the page's
// time, a payer the list does not hold runs its page, and a hold after the
// payer's last page the next proven refusal runs one again.
func TestRedisExhaustedPayersBoundProvenRecoveryPages(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	payers := newRedisExhaustedPayers(5*time.Second, 4)
	payer := server.NewId()
	if !payers.takeRecoveryPage(payer, start) {
		t.Fatal("an unlisted payer skipped its page")
	}
	if _, ok := payers.payerIdHolds[payer]; ok {
		t.Fatal("an unlisted payer's page listed it")
	}
	payers.note(payer, start)
	if !payers.takeRecoveryPage(payer, start) {
		t.Fatal("the first proven refusal in the hold skipped its page")
	}
	if payers.takeRecoveryPage(payer, start.Add(time.Second)) {
		t.Fatal("a second proven refusal in the hold ran a page")
	}
	// proofs and an undecided proof list the payer again within the hold
	payers.note(payer, start.Add(2*time.Second))
	if payers.takeRecoveryPage(payer, start.Add(3*time.Second)) {
		t.Fatal("a proof's listing reset the payer's page")
	}
	payers.noteUnproven(payer, start.Add(4*time.Second))
	if payers.takeRecoveryPage(payer, start.Add(4500*time.Millisecond)) {
		t.Fatal("an undecided proof reset the payer's page")
	}
	if !payers.takeRecoveryPage(payer, start.Add(5*time.Second)) {
		t.Fatal("a hold after the payer's last page the next proven refusal skipped its page")
	}
	if payers.takeRecoveryPage(payer, start.Add(9*time.Second)) {
		t.Fatal("the next hold ran a second page")
	}
	// an expired listing is dropped with its page time
	if payers.recent(payer, start.Add(10*time.Second)) {
		t.Fatal("an expired payer was recent")
	}
	if !payers.takeRecoveryPage(payer, start.Add(10*time.Second)) {
		t.Fatal("a payer whose listing expired skipped its page")
	}
	payers.note(payer, start.Add(10*time.Second))
	if !payers.takeRecoveryPage(payer, start.Add(10*time.Second)) {
		t.Fatal("a payer listed again skipped its first page")
	}
}
