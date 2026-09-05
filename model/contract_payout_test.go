package model

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type contractPayoutTestAmount struct {
	byteCount ByteCount
	payout    NanoCents
}

func TestEvenContractPayoutShare(t *testing.T) {
	for _, test := range []struct {
		name  string
		total int64
		count int
		want  []int64
	}{
		{name: "zero", total: 0, count: 3, want: []int64{0, 0, 0}},
		{name: "smaller than participant count", total: 2, count: 3, want: []int64{1, 1, 0}},
		{name: "exact", total: 6, count: 3, want: []int64{2, 2, 2}},
		{name: "remainder", total: 8, count: 3, want: []int64{3, 3, 2}},
	} {
		got := make([]int64, test.count)
		var sum int64
		for i := range test.count {
			got[i] = evenContractPayoutShare(test.total, i, test.count)
			sum += got[i]
		}
		if !slices.Equal(got, test.want) || sum != test.total {
			t.Fatalf("%s: shares = %v (sum %d), want %v (sum %d)", test.name, got, sum, test.want, test.total)
		}
	}
}

func contractPayoutTestId(value byte) server.Id {
	var id server.Id
	id[len(id)-1] = value
	return id
}

// The allocation test is intentionally database-free: it pins every
// eligibility branch and the exact remainder owner, so failures identify the
// payout rule rather than test-service timing or ledger-post ordering.
func TestAllocateContractParticipantPayouts(t *testing.T) {
	originNetworkId := contractPayoutTestId(1)
	balanceId := contractPayoutTestId(2)
	eligibleNetworkA := contractPayoutTestId(3)
	eligibleNetworkB := contractPayoutTestId(4)
	eligibleNetworkC := contractPayoutTestId(5)

	participant := func(client byte, networkId server.Id) ContractParticipant {
		return ContractParticipant{ClientId: contractPayoutTestId(client), NetworkId: networkId}
	}
	for _, test := range []struct {
		name         string
		total        ByteCount
		grossPayout  NanoCents
		participants []ContractParticipant
		want         map[server.Id]contractPayoutTestAmount
	}{
		{
			name:         "single eligible egress",
			total:        120,
			participants: []ContractParticipant{participant(10, eligibleNetworkA)},
			want:         map[server.Id]contractPayoutTestAmount{eligibleNetworkA: {byteCount: 120, payout: 120}},
		},
		{
			name:  "all hops eligible",
			total: 120,
			participants: []ContractParticipant{
				participant(10, eligibleNetworkA),
				participant(11, eligibleNetworkB),
				participant(12, eligibleNetworkC),
			},
			want: map[server.Id]contractPayoutTestAmount{
				eligibleNetworkA: {byteCount: 40, payout: 40},
				eligibleNetworkB: {byteCount: 40, payout: 40},
				eligibleNetworkC: {byteCount: 40, payout: 40},
			},
		},
		{
			name:  "same-network first hop is suppressed",
			total: 120,
			participants: []ContractParticipant{
				participant(10, originNetworkId),
				participant(11, eligibleNetworkA),
			},
			want: map[server.Id]contractPayoutTestAmount{eligibleNetworkA: {byteCount: 60, payout: 60}},
		},
		{
			name:  "same-network egress is suppressed",
			total: 120,
			participants: []ContractParticipant{
				participant(10, eligibleNetworkA),
				participant(11, originNetworkId),
			},
			want: map[server.Id]contractPayoutTestAmount{eligibleNetworkA: {byteCount: 60, payout: 60}},
		},
		{
			name:  "no eligible participants",
			total: 120,
			participants: []ContractParticipant{
				participant(10, originNetworkId),
				participant(11, originNetworkId),
			},
			want: map[server.Id]contractPayoutTestAmount{},
		},
		{
			name:  "eligible hops aggregate by payout network",
			total: 120,
			participants: []ContractParticipant{
				participant(10, eligibleNetworkA),
				participant(11, eligibleNetworkA),
			},
			want: map[server.Id]contractPayoutTestAmount{eligibleNetworkA: {byteCount: 120, payout: 120}},
		},
		{
			name:  "remainder follows stable participant order",
			total: 121,
			participants: []ContractParticipant{
				participant(10, eligibleNetworkA),
				participant(11, eligibleNetworkB),
				participant(12, eligibleNetworkC),
			},
			want: map[server.Id]contractPayoutTestAmount{
				eligibleNetworkA: {byteCount: 41, payout: 41},
				eligibleNetworkB: {byteCount: 40, payout: 40},
				eligibleNetworkC: {byteCount: 40, payout: 40},
			},
		},
		{
			name:        "money remains evenly split when bytes are fewer than participants",
			total:       2,
			grossPayout: 8,
			participants: []ContractParticipant{
				participant(10, eligibleNetworkA),
				participant(11, eligibleNetworkB),
				participant(12, eligibleNetworkC),
			},
			want: map[server.Id]contractPayoutTestAmount{
				eligibleNetworkA: {byteCount: 1, payout: 3},
				eligibleNetworkB: {byteCount: 1, payout: 3},
				eligibleNetworkC: {byteCount: 0, payout: 2},
			},
		},
	} {
		grossPayout := test.grossPayout
		if grossPayout == 0 {
			grossPayout = NanoCents(test.total)
		}
		sweeps := map[server.Id]sweepPayout{
			balanceId: {payoutByteCount: test.total, payout: grossPayout},
		}
		participantSweeps, accountPayouts := allocateContractParticipantPayouts(
			test.participants,
			originNetworkId,
			sweeps,
		)
		got := map[server.Id]contractPayoutTestAmount{}
		for networkId, payout := range accountPayouts {
			got[networkId] = contractPayoutTestAmount{
				byteCount: payout.payoutByteCount,
				payout:    payout.payout,
			}
		}
		if !maps.Equal(got, test.want) {
			t.Errorf("%s: account payout = %v, want %v", test.name, got, test.want)
		}
		if len(participantSweeps) != len(test.want) {
			t.Errorf("%s: participant sweep count = %d, want %d", test.name, len(participantSweeps), len(test.want))
		}
	}

	// Per-balance splitting would award both one-byte remainders to participant
	// A. Cumulative deltas instead make the contract totals even while preserving
	// each source balance's exact totals.
	secondBalanceId := contractPayoutTestId(6)
	participants := []ContractParticipant{
		participant(10, eligibleNetworkA),
		participant(11, eligibleNetworkB),
		participant(12, eligibleNetworkC),
	}
	participantSweeps, accountPayouts := allocateContractParticipantPayouts(
		participants,
		originNetworkId,
		map[server.Id]sweepPayout{
			balanceId:       {payoutByteCount: 1, payout: 2},
			secondBalanceId: {payoutByteCount: 1, payout: 2},
		},
	)
	wantAccounts := map[server.Id]contractPayoutTestAmount{
		eligibleNetworkA: {byteCount: 1, payout: 2},
		eligibleNetworkB: {byteCount: 1, payout: 1},
		eligibleNetworkC: {byteCount: 0, payout: 1},
	}
	gotAccounts := map[server.Id]contractPayoutTestAmount{}
	for networkId, payout := range accountPayouts {
		gotAccounts[networkId] = contractPayoutTestAmount{
			byteCount: payout.payoutByteCount,
			payout:    payout.payout,
		}
	}
	if !maps.Equal(gotAccounts, wantAccounts) {
		t.Errorf("multiple balances: account payout = %v, want %v", gotAccounts, wantAccounts)
	}
	for balance, want := range map[server.Id]contractPayoutTestAmount{
		balanceId:       {byteCount: 1, payout: 2},
		secondBalanceId: {byteCount: 1, payout: 2},
	} {
		var got contractPayoutTestAmount
		for key, payout := range participantSweeps {
			if key.balanceId == balance {
				got.byteCount += payout.payoutByteCount
				got.payout += payout.payout
			}
		}
		if got != want {
			t.Errorf("multiple balances: sweep total for %s = %v, want %v", balance, got, want)
		}
	}
}

// Every eligibility mask matters because same-origin-network shares stay in
// the denominator but must disappear from both the durable sweep and the
// account payout. The last participant is the egress; preceding participants
// are intermediaries, so enumerating every mask covers either role alone and
// every mixed path.
func TestAllocateContractParticipantPayoutEligibilityMatrix(t *testing.T) {
	originNetworkId := contractPayoutTestId(1)
	balanceId := contractPayoutTestId(2)
	const totalByteCount = ByteCount(37)
	const totalPayout = NanoCents(43)

	for participantCount := 1; participantCount <= 5; participantCount++ {
		for sameNetworkMask := 0; sameNetworkMask < 1<<participantCount; sameNetworkMask++ {
			name := fmt.Sprintf("%d_hops/same_network_mask_%0*x", participantCount, participantCount, sameNetworkMask)
			t.Run(name, func(t *testing.T) {
				participants := make([]ContractParticipant, participantCount)
				want := map[server.Id]contractPayoutTestAmount{}
				for participantIndex := range participantCount {
					clientId := contractPayoutTestId(byte(10 + participantIndex))
					networkId := contractPayoutTestId(byte(30 + participantIndex))
					if sameNetworkMask&(1<<participantIndex) != 0 {
						networkId = originNetworkId
					}
					participants[participantIndex] = ContractParticipant{
						ClientId:  clientId,
						NetworkId: networkId,
					}
					if networkId != originNetworkId {
						want[networkId] = contractPayoutTestAmount{
							byteCount: ByteCount(evenContractPayoutShare(int64(totalByteCount), participantIndex, participantCount)),
							payout:    NanoCents(evenContractPayoutShare(int64(totalPayout), participantIndex, participantCount)),
						}
					}
				}

				participantSweeps, accountPayouts := allocateContractParticipantPayouts(
					participants,
					originNetworkId,
					map[server.Id]sweepPayout{
						balanceId: {payoutByteCount: totalByteCount, payout: totalPayout},
					},
				)
				got := map[server.Id]contractPayoutTestAmount{}
				for networkId, payout := range accountPayouts {
					got[networkId] = contractPayoutTestAmount{
						byteCount: payout.payoutByteCount,
						payout:    payout.payout,
					}
				}
				if !maps.Equal(got, want) {
					t.Fatalf("account payout = %v, want %v", got, want)
				}
				if len(participantSweeps) != len(want) {
					t.Fatalf("participant sweep count = %d, want %d", len(participantSweeps), len(want))
				}
				for participantIndex, participant := range participants {
					key := participantSweepKey{balanceId: balanceId, networkId: participant.NetworkId}
					participantSweep, exists := participantSweeps[key]
					if participant.NetworkId == originNetworkId {
						if exists {
							t.Fatalf("same-network participant %d received sweep %+v", participantIndex, participantSweep)
						}
						continue
					}
					if !exists {
						t.Fatalf("eligible participant %d has no sweep", participantIndex)
					}
					wantAmount := want[participant.NetworkId]
					if participantSweep.destinationId != participant.ClientId ||
						participantSweep.payoutByteCount != wantAmount.byteCount ||
						participantSweep.payout != wantAmount.payout {
						t.Fatalf("participant %d sweep = %+v, want client %s amount %v", participantIndex, participantSweep, participant.ClientId, wantAmount)
					}
				}
			})
		}
	}

	participantSweeps, accountPayouts := allocateContractParticipantPayouts(
		nil,
		originNetworkId,
		map[server.Id]sweepPayout{balanceId: {payoutByteCount: totalByteCount, payout: totalPayout}},
	)
	if len(participantSweeps) != 0 || len(accountPayouts) != 0 {
		t.Fatalf("empty participant set produced sweeps/accounts: %v/%v", participantSweeps, accountPayouts)
	}
}

func contractPayoutTestAmounts(
	t testing.TB,
	ctx context.Context,
	contractId server.Id,
) map[server.Id]contractPayoutTestAmount {
	t.Helper()
	amounts := map[server.Id]contractPayoutTestAmount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id,
					SUM(payout_byte_count)::bigint,
					SUM(payout_net_revenue_nano_cents)::bigint
				FROM transfer_escrow_sweep
				WHERE contract_id = $1
				GROUP BY network_id
			`,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var amount contractPayoutTestAmount
				server.Raise(result.Scan(&networkId, &amount.byteCount, &amount.payout))
				amounts[networkId] = amount
			}
		})
	})
	return amounts
}

func contractPayoutTestAccountAmount(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
) contractPayoutTestAmount {
	t.Helper()
	var amount contractPayoutTestAmount
	server.Redis(ctx, func(r server.RedisClient) {
		byteCount, err := r.Get(ctx, accountBalanceNetPayoutByteCountKey(networkId)).Int64()
		if err != nil && err != server.RedisNil {
			t.Fatalf("read payout byte count for %s: %v", networkId, err)
		}
		payout, err := r.Get(ctx, accountBalanceNetPayout(networkId)).Int64()
		if err != nil && err != server.RedisNil {
			t.Fatalf("read payout revenue for %s: %v", networkId, err)
		}
		amount.byteCount = ByteCount(byteCount)
		amount.payout = NanoCents(payout)
	})
	return amount
}

func assertContractPayoutTestAccounts(
	t testing.TB,
	ctx context.Context,
	networkIds []server.Id,
	want map[server.Id]contractPayoutTestAmount,
) {
	t.Helper()
	seen := map[server.Id]bool{}
	for _, networkId := range networkIds {
		if seen[networkId] {
			continue
		}
		seen[networkId] = true
		got := contractPayoutTestAccountAmount(t, ctx, networkId)
		if got != want[networkId] {
			t.Fatalf("account payout for %s = %v, want %v", networkId, got, want[networkId])
		}
	}
}

func addContractPayoutTestBalance(
	ctx context.Context,
	networkId server.Id,
	byteCount ByteCount,
) *TransferBalance {
	// With net revenue at twice the byte count, the 50% provider share is
	// exactly one nanocent per settled byte. That makes both halves of the
	// payout split independently observable without floating-point ambiguity.
	balance := &TransferBalance{
		NetworkId:             networkId,
		StartTime:             server.NowUtc().Add(-time.Hour),
		EndTime:               server.NowUtc().Add(time.Hour),
		StartBalanceByteCount: byteCount,
		BalanceByteCount:      byteCount,
		NetRevenue:            NanoCents(2 * byteCount),
		PurchaseToken:         server.NewId().String(),
	}
	AddTransferBalance(ctx, balance)
	return balance
}

func addContractPayoutTestClients(
	ctx context.Context,
	clients map[server.Id]server.Id,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		for clientId, networkId := range clients {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
				clientId,
				networkId,
			))
		}
	})
}

func assertContractPayoutTestBalanceConsumed(
	t testing.TB,
	ctx context.Context,
	balanceId server.Id,
	contractId server.Id,
	want ByteCount,
) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					transfer_balance.balance_byte_count,
					transfer_escrow.payout_byte_count
				FROM transfer_balance
				INNER JOIN transfer_escrow ON
					transfer_escrow.balance_id = transfer_balance.balance_id
				WHERE
					transfer_balance.balance_id = $1 AND
					transfer_escrow.contract_id = $2
			`,
			balanceId,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatal("missing settled escrow balance")
			}
			var remaining ByteCount
			var consumed ByteCount
			server.Raise(result.Scan(&remaining, &consumed))
			if remaining != 0 || consumed != want {
				t.Fatalf("settled balance remaining/consumed = %d/%d, want 0/%d", remaining, consumed, want)
			}
		})
	})
}

// This is the deterministic root-cause matrix for contract payouts. A contract
// reserves the aggregate traffic for all service hops, so its payout is divided
// over the intermediary clients plus the egress client. A participant on the
// payer/origin network keeps its share out of the payout instead of reallocating
// that same-network traffic to another participant. The sender's balance is
// nevertheless charged for every settled byte.
func TestContractPayoutParticipantMatrix(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for _, test := range []struct {
			name                   string
			participantSameNetwork []bool // intermediaries, then egress
			sharedEligibleNetwork  bool
			usedByteCount          ByteCount
		}{
			{name: "direct cross-network egress", participantSameNetwork: []bool{false}},
			{name: "direct same-network egress", participantSameNetwork: []bool{true}},
			{name: "intermediary and egress eligible", participantSameNetwork: []bool{false, false}},
			{name: "same-network intermediary", participantSameNetwork: []bool{true, false}},
			{name: "same-network egress", participantSameNetwork: []bool{false, true}},
			{name: "no eligible participants", participantSameNetwork: []bool{true, true}},
			{name: "two intermediaries and egress eligible", participantSameNetwork: []bool{false, false, false}},
			{name: "two intermediaries with mixed eligibility and remainder", participantSameNetwork: []bool{true, false, true}, usedByteCount: 121},
			{name: "eligible hops share a payout network", participantSameNetwork: []bool{false, false}, sharedEligibleNetwork: true},
		} {
			usedByteCount := test.usedByteCount
			if usedByteCount == 0 {
				usedByteCount = 120
			}
			originNetworkId := server.NewId()
			originClientId := server.NewId()
			participantIds := make([]server.Id, len(test.participantSameNetwork))
			participantNetworkIds := make([]server.Id, len(test.participantSameNetwork))
			clients := map[server.Id]server.Id{originClientId: originNetworkId}
			sharedEligibleNetworkId := server.NewId()
			for i, sameNetwork := range test.participantSameNetwork {
				participantIds[i] = server.NewId()
				participantNetworkIds[i] = server.NewId()
				if sameNetwork {
					participantNetworkIds[i] = originNetworkId
				} else if test.sharedEligibleNetwork {
					participantNetworkIds[i] = sharedEligibleNetworkId
				}
				clients[participantIds[i]] = participantNetworkIds[i]
			}
			addContractPayoutTestClients(ctx, clients)
			balance := addContractPayoutTestBalance(ctx, originNetworkId, usedByteCount)

			egressIndex := len(participantIds) - 1
			egressClientId := participantIds[egressIndex]
			escrow, err := CreateTransferEscrow(
				ctx,
				originNetworkId,
				originClientId,
				participantNetworkIds[egressIndex],
				egressClientId,
				usedByteCount,
			)
			if err != nil {
				t.Fatalf("%s: create escrow: %v", test.name, err)
			}
			intermediaryIds := participantIds[:egressIndex]
			if 0 < len(intermediaryIds) {
				streamId := AddToStream(ctx, escrow.ContractId, originClientId, egressClientId, intermediaryIds)
				if err := SetContractStream(ctx, escrow.ContractId, streamId, intermediaryIds); err != nil {
					t.Fatalf("%s: persist contract participants: %v", test.name, err)
				}
			}

			if err := CloseContract(ctx, escrow.ContractId, originClientId, usedByteCount, false); err != nil {
				t.Fatalf("%s: close origin: %v", test.name, err)
			}
			if err := CloseContract(ctx, escrow.ContractId, egressClientId, usedByteCount, false); err != nil {
				t.Fatalf("%s: close egress: %v", test.name, err)
			}

			got := contractPayoutTestAmounts(t, ctx, escrow.ContractId)
			want := map[server.Id]contractPayoutTestAmount{}
			wantProviders := map[server.Id]int64{}
			sortedParticipantIds := slices.Clone(participantIds)
			slices.SortFunc(sortedParticipantIds, func(a, b server.Id) int {
				return a.Cmp(b)
			})
			shareByParticipantId := map[server.Id]ByteCount{}
			for participantIndex, participantId := range sortedParticipantIds {
				shareByParticipantId[participantId] = ByteCount(evenContractPayoutShare(
					int64(usedByteCount),
					participantIndex,
					len(sortedParticipantIds),
				))
			}
			for i, sameNetwork := range test.participantSameNetwork {
				if !sameNetwork {
					share := shareByParticipantId[participantIds[i]]
					wantProviders[participantIds[i]] = share
					amount := want[participantNetworkIds[i]]
					amount.byteCount += share
					amount.payout += NanoCents(share)
					want[participantNetworkIds[i]] = amount
				}
			}
			if !maps.Equal(got, want) {
				t.Fatalf("%s: payout = %v, want %v", test.name, got, want)
			}
			assertContractPayoutTestProviderAllocations(t, ctx, escrow.ContractId, wantProviders)
			assertContractPayoutTestAccounts(
				t,
				ctx,
				append([]server.Id{originNetworkId}, participantNetworkIds...),
				want,
			)
			assertContractPayoutTestBalanceConsumed(t, ctx, balance.BalanceId, escrow.ContractId, usedByteCount)
		}
	})
}

// Companion contracts reverse the payer endpoint. They do not carry the
// intermediary list themselves, so settlement must join the participants from
// the origin contract through the shared stream id.
func TestCompanionContractPayoutParticipantsJoinByStreamId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const usedByteCount = ByteCount(120)

		for _, test := range []struct {
			name                    string
			intermediarySameNetwork bool
			egressSameNetwork       bool
			sharedEligibleNetwork   bool
		}{
			{name: "all participants eligible"},
			{name: "same-network intermediary", intermediarySameNetwork: true},
			{name: "same-network egress", egressSameNetwork: true},
			{name: "no eligible participants", intermediarySameNetwork: true, egressSameNetwork: true},
			{name: "eligible participants share payout network", sharedEligibleNetwork: true},
		} {
			originNetworkId := server.NewId()
			egressNetworkId := server.NewId()
			intermediaryNetworkId := server.NewId()
			if test.egressSameNetwork {
				egressNetworkId = originNetworkId
			}
			if test.intermediarySameNetwork {
				intermediaryNetworkId = originNetworkId
			} else if test.sharedEligibleNetwork {
				intermediaryNetworkId = egressNetworkId
			}
			originClientId := server.NewId()
			egressClientId := server.NewId()
			intermediaryClientId := server.NewId()
			addContractPayoutTestClients(ctx, map[server.Id]server.Id{
				originClientId:       originNetworkId,
				egressClientId:       egressNetworkId,
				intermediaryClientId: intermediaryNetworkId,
			})
			balance := addContractPayoutTestBalance(ctx, originNetworkId, usedByteCount)

			// The zero-byte forward contract is only the companion anchor. Its
			// stream carries the intermediary participant into the paid reverse
			// contract, whose payer is its destination endpoint.
			originEscrow, err := CreateTransferEscrow(
				ctx,
				originNetworkId,
				originClientId,
				egressNetworkId,
				egressClientId,
				0,
			)
			if err != nil {
				t.Fatalf("%s: create origin escrow: %v", test.name, err)
			}
			streamId := AddToStream(
				ctx,
				originEscrow.ContractId,
				originClientId,
				egressClientId,
				[]server.Id{intermediaryClientId},
			)
			// Deliberately leave the origin contract unpersisted to model a live
			// stream created by a prior release before durable participants existed.
			// Joining the companion must recover the intermediaries from that stream,
			// then persist them under its shared stream id.

			companionEscrow, err := CreateCompanionTransferEscrow(
				ctx,
				egressNetworkId,
				egressClientId,
				originNetworkId,
				originClientId,
				usedByteCount,
				time.Hour,
			)
			if err != nil {
				t.Fatalf("%s: create companion escrow: %v", test.name, err)
			}
			companionStreamId, ok := AddCompanionContractToStream(
				ctx,
				companionEscrow.ContractId,
				originEscrow.ContractId,
				egressClientId,
				originClientId,
			)
			if !ok || companionStreamId != streamId {
				t.Fatalf("%s: companion stream = %s/%t, want %s/true", test.name, companionStreamId, ok, streamId)
			}
			if err := SetContractStream(ctx, companionEscrow.ContractId, companionStreamId, nil); err != nil {
				t.Fatalf("%s: persist companion stream: %v", test.name, err)
			}
			// Prove settlement joins the durable participant rows by stream id, not
			// the transient Redis path: remove both contracts' stream markings before
			// either close is submitted.
			if removedStreamId, ok := RemoveFromStream(ctx, originEscrow.ContractId); !ok || removedStreamId != streamId {
				t.Fatalf("%s: remove origin stream = %s/%t, want %s/true", test.name, removedStreamId, ok, streamId)
			}
			if removedStreamId, ok := RemoveFromStream(ctx, companionEscrow.ContractId); !ok || removedStreamId != streamId {
				t.Fatalf("%s: remove companion stream = %s/%t, want %s/true", test.name, removedStreamId, ok, streamId)
			}

			if err := CloseContract(ctx, companionEscrow.ContractId, egressClientId, usedByteCount, false); err != nil {
				t.Fatalf("%s: close companion egress: %v", test.name, err)
			}
			if err := CloseContract(ctx, companionEscrow.ContractId, originClientId, usedByteCount, false); err != nil {
				t.Fatalf("%s: close companion origin: %v", test.name, err)
			}

			want := map[server.Id]contractPayoutTestAmount{}
			wantProviders := map[server.Id]int64{}
			if !test.intermediarySameNetwork {
				wantProviders[intermediaryClientId] = usedByteCount / 2
			}
			if !test.egressSameNetwork {
				wantProviders[egressClientId] = usedByteCount / 2
			}
			for _, participant := range []struct {
				networkId   server.Id
				sameNetwork bool
			}{
				{networkId: intermediaryNetworkId, sameNetwork: test.intermediarySameNetwork},
				{networkId: egressNetworkId, sameNetwork: test.egressSameNetwork},
			} {
				if !participant.sameNetwork {
					amount := want[participant.networkId]
					amount.byteCount += usedByteCount / 2
					amount.payout += NanoCents(usedByteCount / 2)
					want[participant.networkId] = amount
				}
			}
			got := contractPayoutTestAmounts(t, ctx, companionEscrow.ContractId)
			if !maps.Equal(got, want) {
				t.Fatalf("%s: companion payout = %v, want %v", test.name, got, want)
			}
			assertContractPayoutTestProviderAllocations(t, ctx, companionEscrow.ContractId, wantProviders)
			assertContractPayoutTestAccounts(
				t,
				ctx,
				[]server.Id{originNetworkId, intermediaryNetworkId, egressNetworkId},
				want,
			)
			assertContractPayoutTestBalanceConsumed(t, ctx, balance.BalanceId, companionEscrow.ContractId, usedByteCount)
		}
	})
}

// Manual dispute resolution enters settlement through different close-row
// branches, but participant selection and splitting must be identical to the
// ordinary two-party close path for either winner.
func TestContractParticipantPayoutDisputeOutcomes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const usedByteCount = ByteCount(120)

		for _, test := range []struct {
			name       string
			closeParty ContractParty
			outcome    ContractOutcome
		}{
			{
				name:       "resolved to source",
				closeParty: ContractPartySource,
				outcome:    ContractOutcomeDisputeResolvedToSource,
			},
			{
				name:       "resolved to destination",
				closeParty: ContractPartyDestination,
				outcome:    ContractOutcomeDisputeResolvedToDestination,
			},
		} {
			originNetworkId := server.NewId()
			egressNetworkId := server.NewId()
			intermediaryNetworkId := server.NewId()
			originClientId := server.NewId()
			egressClientId := server.NewId()
			intermediaryClientId := server.NewId()
			addContractPayoutTestClients(ctx, map[server.Id]server.Id{
				originClientId:       originNetworkId,
				egressClientId:       egressNetworkId,
				intermediaryClientId: intermediaryNetworkId,
			})
			balance := addContractPayoutTestBalance(ctx, originNetworkId, usedByteCount)
			escrow, err := CreateTransferEscrow(
				ctx,
				originNetworkId,
				originClientId,
				egressNetworkId,
				egressClientId,
				usedByteCount,
			)
			if err != nil {
				t.Fatalf("%s: create escrow: %v", test.name, err)
			}
			streamId := AddToStream(
				ctx,
				escrow.ContractId,
				originClientId,
				egressClientId,
				[]server.Id{intermediaryClientId},
			)
			if err := SetContractStream(ctx, escrow.ContractId, streamId, []server.Id{intermediaryClientId}); err != nil {
				t.Fatalf("%s: persist participants: %v", test.name, err)
			}

			closeClientId := originClientId
			if test.closeParty == ContractPartyDestination {
				closeClientId = egressClientId
			}
			if err := CloseContract(ctx, escrow.ContractId, closeClientId, usedByteCount, false); err != nil {
				t.Fatalf("%s: close winning party: %v", test.name, err)
			}
			SetContractDispute(ctx, escrow.ContractId, true)
			if err := SettleEscrow(ctx, escrow.ContractId, test.outcome); err != nil {
				t.Fatalf("%s: settle dispute: %v", test.name, err)
			}
			RemoveFromStream(ctx, escrow.ContractId)

			want := map[server.Id]contractPayoutTestAmount{
				egressNetworkId: {
					byteCount: usedByteCount / 2,
					payout:    NanoCents(usedByteCount / 2),
				},
				intermediaryNetworkId: {
					byteCount: usedByteCount / 2,
					payout:    NanoCents(usedByteCount / 2),
				},
			}
			got := contractPayoutTestAmounts(t, ctx, escrow.ContractId)
			if !maps.Equal(got, want) {
				t.Fatalf("%s: dispute payout = %v, want %v", test.name, got, want)
			}
			assertContractPayoutTestProviderAllocations(t, ctx, escrow.ContractId, map[server.Id]int64{
				egressClientId: usedByteCount / 2, intermediaryClientId: usedByteCount / 2,
			})
			assertContractPayoutTestAccounts(
				t,
				ctx,
				[]server.Id{originNetworkId, intermediaryNetworkId, egressNetworkId},
				want,
			)
			assertContractPayoutTestBalanceConsumed(t, ctx, balance.BalanceId, escrow.ContractId, usedByteCount)
		}
	})
}

func TestContractParticipantOrphanSweep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractId := server.NewId()
		liveStreamId := server.NewId()
		orphanStreamId := server.NewId()
		liveParticipantId := server.NewId()
		orphanParticipantId := server.NewId()

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO transfer_contract (
						contract_id,
						source_network_id,
						source_id,
						destination_network_id,
						destination_id,
						transfer_byte_count,
						stream_id
					)
					VALUES ($1, $2, $3, $4, $5, 0, $6)
				`,
				contractId,
				server.NewId(),
				server.NewId(),
				server.NewId(),
				server.NewId(),
				liveStreamId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO contract_participant (stream_id, client_id, network_id)
					VALUES
						($1, $2, $3),
						($4, $5, $6)
				`,
				liveStreamId,
				liveParticipantId,
				server.NewId(),
				orphanStreamId,
				orphanParticipantId,
				server.NewId(),
			))
		})

		removed, _, done := SweepOrphanContractData(ctx, SweepOrphanCursor{}, 0, 1)
		if !done || removed != 1 {
			t.Fatalf("participant orphan sweep removed/done = %d/%t, want 1/true", removed, done)
		}

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT stream_id, client_id FROM contract_participant ORDER BY stream_id, client_id`,
			)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("live contract participant was removed")
				}
				var streamId server.Id
				var clientId server.Id
				server.Raise(result.Scan(&streamId, &clientId))
				if streamId != liveStreamId || clientId != liveParticipantId {
					t.Fatalf("remaining participant = %s/%s, want %s/%s", streamId, clientId, liveStreamId, liveParticipantId)
				}
				if result.Next() {
					t.Fatal("orphan contract participant survived")
				}
			})
		})
	})
}
