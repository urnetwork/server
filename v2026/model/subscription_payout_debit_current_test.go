// Current payout fixtures follow the journal owner across its explicit debit
// boundary. Escrow metadata and full Redis tokens remain pending until then.
package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A partial positive close retains all 64 reserved bytes while its durable
// consumption is 11. The actual debit owner returns only the unused 53 bytes.
func TestPayoutDebitCurrentPartialSettlementKeepsOriginalReservation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := asyncPayoutRecoveryFixture(t, ctx)
		anchor, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 0)
		if err != nil || anchor == nil {
			t.Fatal("public zero anchor failed", err)
		}
		escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 64)
		if err != nil || escrow == nil || escrow.TransferByteCount != 64 {
			t.Fatal("public positive reservation failed", err)
		}
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			server.Raise(CloseContract(ctx, escrow.ContractId, clientId, 11, false))
		}
		want := map[server.Id]contractPayoutTestAmount{f.destinationNetworkId: {byteCount: 11, payout: 11}}
		if got := contractPayoutTestAmounts(t, ctx, escrow.ContractId); !maps.Equal(got, want) {
			t.Fatal("partial settlement lost the exact provider earning", got)
		}
		assertContractPayoutTestAccounts(t, ctx, []server.Id{f.sourceNetworkId, f.destinationNetworkId}, want)
		assertCurrentPayoutDebitTestConsumptionAndDrain(t, ctx, f.balanceId, 1000,
			map[server.Id]currentPayoutDebitTestAmount{escrow.ContractId: {reserved: 64, consumed: 11}})
		beforeReplay := readPayoutDebitTestState(t, ctx, f.balanceId)
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			if err := CloseContract(ctx, escrow.ContractId, clientId, 11, false); !errors.Is(err, errContractAlreadySettled) {
				t.Fatal("projected settlement replay was not terminal", err)
			}
		}
		if after := readPayoutDebitTestState(t, ctx, f.balanceId); after != beforeReplay {
			t.Fatal("public close replay changed already projected consumption")
		}
		if got := contractPayoutTestAmounts(t, ctx, escrow.ContractId); !maps.Equal(got, want) {
			t.Fatal("public close replay changed the provider earning", got)
		}
		assertContractPayoutTestAccounts(t, ctx, []server.Id{f.sourceNetworkId, f.destinationNetworkId}, want)
	})
}

type currentPayoutDebitTestAmount struct {
	reserved, consumed ByteCount
}

// Every expected contract has independently specified reservation and usage.
// Neither a provider payout nor nullable escrow metadata supplies payer debt.
func assertCurrentPayoutDebitTestConsumptionAndDrain(t testing.TB, ctx context.Context, balanceId server.Id, initial ByteCount, amounts map[server.Id]currentPayoutDebitTestAmount) {
	t.Helper()
	contractIds := slices.Collect(maps.Keys(amounts))
	slices.SortFunc(contractIds, server.Id.Cmp)
	reservations := make([]ByteCount, len(contractIds))
	consumptions := make([]ByteCount, len(contractIds))
	var reserved, consumed ByteCount
	for index, contractId := range contractIds {
		amount := amounts[contractId]
		if amount.reserved <= 0 || amount.consumed < 0 || amount.consumed > amount.reserved || amount.reserved > initial-reserved {
			t.Fatal("current payout fixture has invalid expected reservation or consumption")
		}
		reservations[index], consumptions[index] = amount.reserved, amount.consumed
		reserved += amount.reserved
		consumed += amount.consumed
	}
	if len(contractIds) == 0 {
		t.Fatal("current payout fixture has no expected journal owners")
	}
	before := readPayoutDebitTestState(t, ctx, balanceId)
	sweeps := readPayoutDebitTestSweeps(t, ctx, balanceId)
	accounts := map[server.Id]contractPayoutTestAmount{
		before.networkId: contractPayoutTestAccountAmount(t, ctx, before.networkId),
	}
	for _, sweep := range sweeps {
		accounts[sweep.networkId] = contractPayoutTestAccountAmount(t, ctx, sweep.networkId)
	}

	// The exact owner page sorts by contract id. Once an entry is drained,
	// its escrow projection must carry that entry's amount and no journal.
	var originalContracts, settledEscrows string
	reservationExpiries := map[string]float64{}
	var originalRecoveryScores map[string]float64
	observe := func(drained int) payoutDebitTestState {
		t.Helper()
		var drainedReserved, drainedConsumed ByteCount
		for _, contractId := range contractIds[:drained] {
			drainedReserved += amounts[contractId].reserved
			drainedConsumed += amounts[contractId].consumed
		}
		state := readPayoutDebitTestState(t, ctx, balanceId)
		if state.initial != initial || state.credit != initial-drainedConsumed || state.pending != len(contractIds)-drained ||
			state.pendingBytes != consumed-drainedConsumed || state.applied != 0 || state.settled != drainedConsumed ||
			state.settledEscrows != drained || state.invalid != 0 || state.invalidReservations != 0 ||
			state.unmaterializedEscrows != len(contractIds)-drained || state.unmaterialized != consumed-drainedConsumed ||
			state.pendingReserved != reserved-drainedReserved || state.escrows != len(contractIds)+before.anchors ||
			state.anchors != before.anchors || state.anchorRows != before.anchorRows || state.legacy != 0 || state.reserved != reserved-drainedReserved {
			t.Fatalf("current debit owner lost exact credit, metadata or full reservation: drained=%d state=%+v", drained, state)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var valid bool
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(bool_and(COALESCE(
				escrow.balance_byte_count=expected.reserved AND escrow.redis_reserved
				AND contract.outcome IS NOT NULL AND contract.payer_network_id=$6
				AND CASE WHEN expected.position<=$5 THEN
					escrow.settled AND escrow.settle_time IS NOT NULL AND escrow.payout_byte_count=expected.consumed
					AND journal.contract_id IS NULL
				ELSE NOT escrow.settled AND escrow.settle_time IS NULL AND escrow.payout_byte_count IS NULL
					AND journal.contract_id IS NOT NULL AND NOT journal.applied AND journal.debit_byte_count=expected.consumed
				END,false)),false)
				FROM unnest($2::uuid[],$3::bigint[],$4::bigint[]) WITH ORDINALITY AS expected(contract_id,reserved,consumed,position)
				LEFT JOIN transfer_escrow escrow ON escrow.balance_id=$1 AND escrow.contract_id=expected.contract_id
				LEFT JOIN transfer_contract contract ON contract.contract_id=expected.contract_id
				LEFT JOIN transfer_debit_journal journal ON journal.balance_id=$1 AND journal.contract_id=expected.contract_id`,
				balanceId, contractIds, reservations, consumptions, drained, before.networkId).Scan(&valid))
			if !valid {
				t.Fatalf("current debit authority or exact escrow projection differs after %d entries", drained)
			}
			var contracts string
			server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(contract) ORDER BY contract_id)::text
				FROM transfer_contract contract WHERE contract_id=ANY($1)`, contractIds).Scan(&contracts))
			if drained == 0 {
				originalContracts = contracts
			} else if contracts != originalContracts {
				t.Fatal("debit worker changed terminal contract or usage authority")
			}
			if drained == len(contractIds) {
				var escrows string
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(escrow) ORDER BY contract_id)::text
					FROM transfer_escrow escrow WHERE balance_id=$1`, balanceId).Scan(&escrows))
				if settledEscrows == "" {
					settledEscrows = escrows
				} else if settledEscrows != escrows {
					t.Fatal("debit replay changed settled escrow metadata")
				}
			}
		})
		server.Redis(ctx, func(client server.RedisClient) {
			keys := redisContractReservationKeys(balanceId)
			tokens, err := client.HGetAll(ctx, keys[1]).Result()
			server.Raise(err)
			leases, err := client.ZRangeWithScores(ctx, keys[2], 0, -1).Result()
			server.Raise(err)
			leaseExpiries := map[string]float64{}
			for _, lease := range leases {
				leaseExpiries[fmt.Sprint(lease.Member)] = lease.Score
			}
			if len(tokens) != len(contractIds)-drained || len(leaseExpiries) != len(tokens) {
				t.Fatal("debit release lost or retained an unexpected request token or lease")
			}
			for _, contractId := range contractIds[drained:] {
				key := contractId.String()
				if tokens[key] != strconv.FormatInt(int64(amounts[contractId].reserved), 10) || leaseExpiries[key] <= 0 {
					t.Fatal("pending consumption lost its original full reservation and lease")
				}
				if drained == 0 {
					reservationExpiries[key] = leaseExpiries[key]
				} else if reservationExpiries[key] != leaseExpiries[key] {
					t.Fatal("another journal entry's debit changed a pending request lease")
				}
			}
			// Publication acknowledgement is optional. Retained recovery
			// markers keep their identity until this exact debit releases them.
			recovery, err := client.ZRangeWithScores(ctx, keys[4], 0, -1).Result()
			server.Raise(err)
			recoveryScores := map[string]float64{}
			for _, marker := range recovery {
				key := fmt.Sprint(marker.Member)
				if _, exists := tokens[key]; !exists || marker.Score <= 0 {
					t.Fatal("reservation recovery marker has no exact pending token")
				}
				recoveryScores[key] = marker.Score
			}
			if drained == 0 {
				originalRecoveryScores = recoveryScores
			} else {
				expectedScores := map[string]float64{}
				for _, contractId := range contractIds[drained:] {
					if score, exists := originalRecoveryScores[contractId.String()]; exists {
						expectedScores[contractId.String()] = score
					}
				}
				if !maps.Equal(recoveryScores, expectedScores) {
					t.Fatal("debit worker changed another reservation's recovery marker")
				}
			}
		})
		available := ByteCount(0)
		if state.inWindow {
			available = max(0, state.credit-state.reserved)
		}
		if got := GetActiveTransferBalanceByteCount(ctx, before.networkId); got != available {
			t.Fatalf("public credit ignored the full outstanding reservation: got=%d want=%d", got, available)
		}
		if after := readPayoutDebitTestSweeps(t, ctx, balanceId); !slices.Equal(sweeps, after) {
			t.Fatal("payer debit changed an exact provider allocation")
		}
		for networkId, amount := range accounts {
			if after := contractPayoutTestAccountAmount(t, ctx, networkId); after != amount {
				t.Fatal("payer debit changed a payer or provider account")
			}
		}
		return state
	}
	observe(0)
	previous := balanceId
	for index := len(previous) - 1; ; index-- {
		if index < 0 {
			t.Fatal("synthetic balance has no preceding UUID")
		}
		if previous[index] != 0 {
			previous[index]--
			break
		}
		previous[index] = 255
	}
	var after payoutDebitTestState
	for drained := 0; drained < len(contractIds); {
		count := min(transferDebitBatchSize, len(contractIds)-drained)
		result, err := FlushTransferDebits(ctx, transferDebitShard(balanceId), &previous, 1)
		if err != nil || result.Failed != 0 || result.Busy != 0 || result.Balances != 1 || result.Applied != count || result.Released != count || !result.More ||
			result.LastBalanceId == nil || *result.LastBalanceId != balanceId {
			t.Fatalf("public debit page did not complete the exact journal owners: result=%+v err=%v", result, err)
		}
		drained += count
		after = observe(drained)
	}
	applied, released, busy, err := flushTransferDebitBalance(ctx, balanceId)
	if err != nil || applied != 0 || released != 0 || busy {
		t.Fatalf("empty exact debit replay changed the journal: applied=%d released=%d busy=%t err=%v", applied, released, busy, err)
	}
	if replayed := observe(len(contractIds)); replayed != after {
		t.Fatal("empty debit replay changed settled credit or metadata")
	}
}
