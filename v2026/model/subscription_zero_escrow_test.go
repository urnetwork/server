// Zero escrow contract creation, the degradation valve's creation path: no
// escrow admission, the payer's normal priority, the companion origin rules,
// the probe shard fence, and settlement with no payout. All tests use
// synthetic networks and clients.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// One contract's row and its financial custody as settlement reads it.
type zeroEscrowTestState struct {
	payerNetworkId      *server.Id
	companionContractId *server.Id
	usageOriginIsSource *bool
	priority            Priority
	outcome             *string
	escrowCount         int
	sweepCount          int
	debitCount          int
	providerTotalsTask  bool
}

// Reads the contract row with its escrow, sweep and debit rows and the
// provider totals task that a payout schedules.
func readZeroEscrowTestState(t testing.TB, ctx context.Context, contractId server.Id) zeroEscrowTestState {
	t.Helper()
	var state zeroEscrowTestState
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT
					payer_network_id,
					companion_contract_id,
					usage_origin_is_source,
					priority,
					outcome::text,
					(SELECT count(*) FROM transfer_escrow WHERE contract_id = $1),
					(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id = $1),
					(SELECT count(*) FROM transfer_debit_journal WHERE contract_id = $1),
					EXISTS (SELECT 1 FROM pending_task WHERE run_once_key = $2)
				FROM transfer_contract
				WHERE contract_id = $1
			`,
			contractId,
			task.RunOnce("legacy_provider_totals", contractId).String(),
		).Scan(
			&state.payerNetworkId,
			&state.companionContractId,
			&state.usageOriginIsSource,
			&state.priority,
			&state.outcome,
			&state.escrowCount,
			&state.sweepCount,
			&state.debitCount,
			&state.providerTotalsTask,
		))
	})
	return state
}

// The only balance of a network, with its remaining byte count.
func zeroEscrowTestOnlyBalance(t testing.TB, ctx context.Context, networkId server.Id) (balanceId server.Id, balanceByteCount ByteCount) {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT balance_id, balance_byte_count FROM transfer_balance WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				count += 1
				server.Raise(result.Scan(&balanceId, &balanceByteCount))
			}
		})
	})
	if count != 1 {
		t.Fatalf("network %s has %d balances, want 1", networkId, count)
	}
	return
}

// A zero escrow result signs the request, holds no balance and writes no
// payer or escrow row.
func requireZeroEscrowTestResult(t testing.TB, ctx context.Context, escrow *TransferEscrow, err error, requested ByteCount) {
	t.Helper()
	if err != nil {
		t.Fatalf("zero escrow creation failed: %v", err)
	}
	if escrow.TransferByteCount != requested || len(escrow.Balances) != 0 || escrow.ExpirationTime.IsZero() {
		t.Fatalf("zero escrow result %d bytes with %d balances, want the request with none", escrow.TransferByteCount, len(escrow.Balances))
	}
	state := readZeroEscrowTestState(t, ctx, escrow.ContractId)
	if state.payerNetworkId != nil || state.escrowCount != 0 {
		t.Fatalf("zero escrow contract has payer %v and %d escrow rows", state.payerNetworkId, state.escrowCount)
	}
}

// Escrow admission reserves in Redis inside its PostgreSQL transaction, and the
// legacy admission path queues per payer first. With the payer's Redis
// reservation key poisoned, escrow admission is refused, and with the payer's
// legacy admission turn held, legacy admission would wait. A zero escrow public
// contract and its companion still complete under both, and the poisoned key
// is byte-identical afterwards, so neither admission ran.
func TestZeroEscrowContractRunsNoEscrowAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 60*time.Second)
		defer cancel()
		clients := newEscrowSelectionTestClients(t, ctx)
		now := server.NowUtc()
		server.Raise(AddBasicTransferBalance(ctx, clients.payerNetworkId, 64*Mib, now, now.Add(time.Hour)))
		balanceId, balanceByteCount := zeroEscrowTestOnlyBalance(t, ctx, clients.payerNetworkId)

		// the reservation token key must be a hash; a string fails every
		// reservation script with "invalid reservation key type"
		reservationKeys := redisContractReservationKeys(balanceId)
		poison := "synthetic-zero-escrow-poison"
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, reservationKeys[1], poison, time.Hour).Err())
		})
		defer server.Redis(context.WithoutCancel(ctx), func(r server.RedisClient) {
			server.Raise(r.Del(context.WithoutCancel(ctx), reservationKeys[1]).Err())
		})
		if _, err := CreateTransferEscrow(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, Mib); err == nil {
			t.Fatal("escrow admission did not reach the poisoned Redis reservation key")
		}

		release, err := transferEscrowAdmissionQueue.acquire(ctx, clients.payerNetworkId)
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		origin, err := CreateZeroEscrowContract(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, Mib)
		requireZeroEscrowTestResult(t, ctx, origin, err, Mib)
		companion, err := CreateZeroEscrowCompanionContract(ctx, clients.providerNetworkId, clients.providerId, clients.payerNetworkId, clients.payerId, Mib, time.Hour)
		requireZeroEscrowTestResult(t, ctx, companion, err, Mib)
		if companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatalf("companion origin %v, want %s", companion.CompanionContractId, origin.ContractId)
		}

		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.Get(ctx, reservationKeys[1]).Result()
			server.Raise(err)
			if value != poison {
				t.Fatalf("a reservation script ran against the payer's key: %q", value)
			}
			if _, err := r.Get(ctx, reservationKeys[0]).Result(); !errors.Is(err, redis.Nil) {
				t.Fatalf("the payer has a Redis reservation total: %v", err)
			}
		})
		if _, after := zeroEscrowTestOnlyBalance(t, ctx, clients.payerNetworkId); after != balanceByteCount {
			t.Fatalf("payer balance changed from %d to %d", balanceByteCount, after)
		}
	})
}

// The priority is the one escrow admission gives the would-be payer: the
// blend of the grants it would fund the request from, earliest expiry first,
// counting whole balances because nothing is reserved, and unpaid with no
// grant. It is never the trusted network priority.
func TestZeroEscrowContractPriorityFollowsPayerGrants(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		type grant struct {
			paid      bool
			byteCount ByteCount
			lifetime  time.Duration
		}
		for _, test := range []struct {
			name    string
			grants  []grant
			request ByteCount
			want    Priority
		}{
			{name: "no grant", request: Mib, want: UnpaidPriority},
			{name: "unpaid", grants: []grant{{byteCount: 64 * Mib, lifetime: time.Hour}}, request: Mib, want: UnpaidPriority},
			{name: "paid", grants: []grant{{paid: true, byteCount: 64 * Mib, lifetime: time.Hour}}, request: Mib, want: PaidPriority},
			// the earlier unpaid grant covers half, so escrow would draw both
			{name: "blend", grants: []grant{{byteCount: Mib, lifetime: time.Hour}, {paid: true, byteCount: 64 * Mib, lifetime: 2 * time.Hour}}, request: 2 * Mib, want: (UnpaidPriority + PaidPriority) / 2},
			// the earlier paid grant covers the request alone
			{name: "earliest covers", grants: []grant{{paid: true, byteCount: 64 * Mib, lifetime: time.Hour}, {byteCount: 64 * Mib, lifetime: 2 * time.Hour}}, request: Mib, want: PaidPriority},
		} {
			clients := newEscrowSelectionTestClients(t, ctx)
			now := server.NowUtc()
			for _, g := range test.grants {
				balance := &TransferBalance{
					NetworkId:             clients.payerNetworkId,
					StartTime:             now,
					EndTime:               now.Add(g.lifetime),
					StartBalanceByteCount: g.byteCount,
					BalanceByteCount:      g.byteCount,
				}
				if g.paid {
					balance.NetRevenue = UsdToNanoCents(1)
				}
				AddTransferBalance(ctx, balance)
			}
			escrow, err := CreateZeroEscrowContract(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, test.request)
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			state := readZeroEscrowTestState(t, ctx, escrow.ContractId)
			if escrow.Priority != test.want || state.priority != test.want || escrow.Priority == TrustedPriority {
				t.Errorf("%s: priority %d (stored %d), want %d", test.name, escrow.Priority, state.priority, test.want)
			}
		}
	})
}

// Even against a paid balance, a closed zero escrow contract settles with no
// escrow, sweep, debit or provider totals, while an escrowed control from the
// same payer and provider settles with a payout, so the checks can see one.
// Like a network contract, it keeps its payment-independent usage snapshot.
func TestZeroEscrowContractSettlesWithoutPayout(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		now := server.NowUtc()
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             clients.payerNetworkId,
			StartTime:             now,
			EndTime:               now.Add(time.Hour),
			StartBalanceByteCount: 64 * Mib,
			BalanceByteCount:      64 * Mib,
			NetRevenue:            UsdToNanoCents(1),
		})
		zero, err := CreateZeroEscrowContract(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, 4*Mib)
		requireZeroEscrowTestResult(t, ctx, zero, err, 4*Mib)
		escrowed, err := CreateTransferEscrow(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, 4*Mib)
		if err != nil {
			t.Fatal(err)
		}
		for _, contractId := range []server.Id{zero.ContractId, escrowed.ContractId} {
			server.Raise(CloseContract(ctx, contractId, clients.payerId, 2*Mib, false))
			server.Raise(CloseContract(ctx, contractId, clients.providerId, 2*Mib, false))
		}

		zeroState := readZeroEscrowTestState(t, ctx, zero.ContractId)
		if zeroState.outcome == nil || *zeroState.outcome != ContractOutcomeSettled {
			t.Fatalf("zero escrow contract outcome %v, want settled", zeroState.outcome)
		}
		if zeroState.escrowCount != 0 || zeroState.sweepCount != 0 || zeroState.debitCount != 0 || zeroState.providerTotalsTask {
			t.Fatalf("zero escrow settlement took financial custody: %+v", zeroState)
		}
		var usageRecorded bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage IS NOT NULL FROM transfer_contract WHERE contract_id = $1`, zero.ContractId).Scan(&usageRecorded))
		})
		if !usageRecorded {
			t.Fatal("zero escrow settlement dropped the usage snapshot a network contract keeps")
		}
		escrowedState := readZeroEscrowTestState(t, ctx, escrowed.ContractId)
		if escrowedState.outcome == nil || *escrowedState.outcome != ContractOutcomeSettled ||
			escrowedState.escrowCount == 0 || escrowedState.sweepCount == 0 || !escrowedState.providerTotalsTask {
			t.Fatalf("the escrowed control did not pay its provider: %+v", escrowedState)
		}
	})
}

// A companion needs its reverse origin: without one it is the retryable
// missing origin and writes nothing. It links the earliest plain origin, and an
// asymmetric reply carrier, whose only reverse contract is a companion, chains
// to that companion. Companions take the destination's usage origin.
func TestZeroEscrowCompanionContractRequiresAndChainsOrigin(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)

		_, err := CreateZeroEscrowCompanionContract(ctx, clients.providerNetworkId, clients.providerId, clients.payerNetworkId, clients.payerId, Mib, time.Hour)
		if !errors.Is(err, ErrMissingCompanionOrigin) {
			t.Fatalf("a companion with no origin got %v, want the missing origin", err)
		}
		var contractCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE source_id = $1 OR destination_id = $1`, clients.payerId).Scan(&contractCount))
		})
		if contractCount != 0 {
			t.Fatalf("the missing origin wrote %d contracts", contractCount)
		}

		origin, err := CreateZeroEscrowContract(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, Mib)
		requireZeroEscrowTestResult(t, ctx, origin, err, Mib)
		reply, err := CreateZeroEscrowCompanionContract(ctx, clients.providerNetworkId, clients.providerId, clients.payerNetworkId, clients.payerId, 2*Mib, time.Hour)
		requireZeroEscrowTestResult(t, ctx, reply, err, 2*Mib)
		carrier, err := CreateZeroEscrowCompanionContract(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, Mib, time.Hour)
		requireZeroEscrowTestResult(t, ctx, carrier, err, Mib)

		for _, test := range []struct {
			name       string
			contract   *TransferEscrow
			wantOrigin *server.Id
		}{
			{name: "origin", contract: origin, wantOrigin: nil},
			{name: "reply", contract: reply, wantOrigin: &origin.ContractId},
			{name: "carrier", contract: carrier, wantOrigin: &reply.ContractId},
		} {
			state := readZeroEscrowTestState(t, ctx, test.contract.ContractId)
			if (state.companionContractId == nil) != (test.wantOrigin == nil) ||
				(test.wantOrigin != nil && *state.companionContractId != *test.wantOrigin) {
				t.Errorf("%s: origin %v, want %v", test.name, state.companionContractId, test.wantOrigin)
			}
			if state.usageOriginIsSource == nil || *state.usageOriginIsSource != (test.wantOrigin == nil) {
				t.Errorf("%s: usage origin is source %v, want %t", test.name, state.usageOriginIsSource, test.wantOrigin == nil)
			}
		}
	})
}

// A probe shard is a hot payer, so its contracts are zero escrow too, behind
// the same fence escrow admission applies: the shard is admitted only while
// active and only as the would-be payer, including its reply carrier, which
// inherits the payer of its shard anchor. Its private grant is untouched.
// Cleanup does not wait for zero escrow contracts, which hold no grant, and an
// orphaned one still closes at its deadline with no payout.
func TestZeroEscrowProberShardKeepsItsFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 60*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		_, grantByteCount := zeroEscrowTestOnlyBalance(t, ctx, owner.NetworkId)

		origin, err := CreateZeroEscrowContract(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, Mib)
		requireZeroEscrowTestResult(t, ctx, origin, err, Mib)
		reply, err := CreateZeroEscrowCompanionContract(ctx, peer.providerNetworkId, peer.providerId, owner.NetworkId, owner.ClientId, Mib, time.Hour)
		requireZeroEscrowTestResult(t, ctx, reply, err, Mib)
		// without the inherited shard payer, the fence refuses the carrier
		carrier, err := CreateZeroEscrowCompanionContract(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, Mib, time.Hour)
		requireZeroEscrowTestResult(t, ctx, carrier, err, Mib)
		if carrier.CompanionContractId == nil || *carrier.CompanionContractId != reply.ContractId {
			t.Fatalf("the shard reply carrier chained to %v, want %s", carrier.CompanionContractId, reply.ContractId)
		}
		if _, after := zeroEscrowTestOnlyBalance(t, ctx, owner.NetworkId); after != grantByteCount {
			t.Fatalf("the shard grant changed from %d to %d", grantByteCount, after)
		}

		// a peer is never the payer of a contract with a shard endpoint
		_, err = CreateZeroEscrowContract(ctx, peer.providerNetworkId, peer.providerId, owner.NetworkId, owner.ClientId, Mib)
		if err == nil || !strings.Contains(err.Error(), "outside its probe shard payer") {
			t.Fatalf("a peer paying for a shard contract got %v, want the shard fence", err)
		}

		server.Raise(DrainProberShard(ctx, owner.Key))
		_, err = CreateZeroEscrowContract(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, Mib)
		if err == nil || !strings.Contains(err.Error(), "outside its probe shard payer") {
			t.Fatalf("a drained shard got %v, want the shard fence", err)
		}

		deleted, err := ReapProberShard(ctx, owner.Key)
		if err != nil || !deleted {
			t.Fatalf("shard cleanup with only zero escrow contracts open: deleted=%t err=%v", deleted, err)
		}
		reconciliation, err := ReconcileContractAtDeadline(ctx, origin.ContractId, server.NowUtc())
		if err != nil || reconciliation.Outcome != ContractOutcomeSettled || reconciliation.Charged != 0 {
			t.Fatalf("the orphaned zero escrow contract did not close at its deadline: %+v %v", reconciliation, err)
		}
		state := readZeroEscrowTestState(t, ctx, origin.ContractId)
		if state.outcome == nil || state.escrowCount != 0 || state.sweepCount != 0 || state.debitCount != 0 || state.providerTotalsTask {
			t.Fatalf("the orphaned zero escrow contract closed with financial custody: %+v", state)
		}
	})
}
