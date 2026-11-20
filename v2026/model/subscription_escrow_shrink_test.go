// A contract request larger than the payer's available balance shrinks to fit
// the balance instead of failing with "Insufficient balance", down to a floor
// of min(request, MinShrinkContractTransferByteCount).
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestGrantTransferEscrowByteCount(t *testing.T) {
	for _, test := range []struct {
		name        string
		requested   ByteCount
		available   ByteCount
		wantOk      bool
		wantGranted ByteCount
	}{
		// the reported defect: a busy flow asks for up to 128 MiB while the
		// payer still has a few MiB to spend
		{name: "shrink 128 MiB to 3 MiB", requested: 128 * Mib, available: 3 * Mib, wantOk: true, wantGranted: 3 * Mib},
		{name: "shrink to floor", requested: 128 * Mib, available: MinShrinkContractTransferByteCount, wantOk: true, wantGranted: MinShrinkContractTransferByteCount},
		{name: "below floor", requested: 128 * Mib, available: MinShrinkContractTransferByteCount - 1, wantOk: false},
		{name: "no balance", requested: 128 * Mib, available: 0, wantOk: false},
		{name: "exact balance", requested: 128 * Mib, available: 128 * Mib, wantOk: true, wantGranted: 128 * Mib},
		{name: "larger balance", requested: 128 * Mib, available: 512 * Mib, wantOk: true, wantGranted: 128 * Mib},
		// a request below the floor must be covered in full
		{name: "small request short", requested: 3072, available: 2048, wantOk: false},
		{name: "small request covered", requested: 3072, available: 4096, wantOk: true, wantGranted: 3072},
		{name: "zero byte without balance", requested: 0, available: 0, wantOk: true, wantGranted: 0},
		{name: "zero byte with balance", requested: 0, available: 5 * Mib, wantOk: true, wantGranted: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			granted, ok := grantTransferEscrowByteCount(test.requested, test.available)
			if ok != test.wantOk || granted != test.wantGranted {
				t.Fatalf("grant(%d requested, %d available) = %d, %t; want %d, %t",
					test.requested, test.available, granted, ok, test.wantGranted, test.wantOk)
			}
			if test.available < granted {
				t.Fatalf("granted %d exceeds available %d", granted, test.available)
			}
		})
	}
}

// fakeRedisReservations models the Redis reservation script's contract for
// one admission: each reserve atomically takes min(remaining, credit - reserved)
// of a balance and records a token, and release returns the token's bytes.
type fakeRedisReservations struct {
	reserved map[server.Id]ByteCount
	tokens   map[server.Id]ByteCount
	released []server.Id
	// bytes reserved during the last admission, before any release
	admitted ByteCount
}

func newFakeRedisReservations() *fakeRedisReservations {
	return &fakeRedisReservations{reserved: map[server.Id]ByteCount{}}
}

func (self *fakeRedisReservations) admit(t *testing.T, balances []*escrowTransferBalance, requested ByteCount) ([]*TransferEscrowBalance, ByteCount, Priority, error) {
	self.tokens = map[server.Id]ByteCount{}
	self.released = nil
	self.admitted = 0
	return reserveRedisTransferEscrowBalances(
		balances,
		requested,
		func(balance *escrowTransferBalance, remaining ByteCount) (ByteCount, error) {
			if remaining <= 0 {
				t.Fatalf("reserve asked for %d bytes", remaining)
			}
			if _, ok := self.tokens[balance.balanceId]; ok {
				t.Fatalf("balance %s reserved twice in one admission", balance.balanceId)
			}
			amount := min(remaining, max(0, balance.balanceByteCount-self.reserved[balance.balanceId]))
			if amount != 0 {
				self.reserved[balance.balanceId] += amount
				self.tokens[balance.balanceId] = amount
				self.admitted += amount
			}
			return amount, nil
		},
		func(balanceId server.Id) {
			self.reserved[balanceId] -= self.tokens[balanceId]
			delete(self.tokens, balanceId)
			self.released = append(self.released, balanceId)
		},
	)
}

func shrinkTestBalances(paid bool, byteCounts ...ByteCount) []*escrowTransferBalance {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	balances := []*escrowTransferBalance{}
	for i, byteCount := range byteCounts {
		balances = append(balances, &escrowTransferBalance{
			balanceId:        server.NewId(),
			paid:             paid,
			balanceByteCount: byteCount,
			startTime:        now.Add(-time.Hour),
			endTime:          now.Add(time.Duration(i+1) * time.Hour),
		})
	}
	return balances
}

func TestReserveRedisTransferEscrowBalancesShrinkToFit(t *testing.T) {
	for _, test := range []struct {
		name         string
		balances     []ByteCount
		preReserved  []ByteCount
		requested    ByteCount
		wantOk       bool
		wantGranted  ByteCount
		wantSelected []ByteCount
	}{
		{name: "shrink 128 MiB to 3 MiB", balances: []ByteCount{3 * Mib}, requested: 128 * Mib, wantOk: true, wantGranted: 3 * Mib, wantSelected: []ByteCount{3 * Mib}},
		{name: "shrink across balances skips empty", balances: []ByteCount{0, 3 * Mib, 0, 2 * Mib}, requested: 128 * Mib, wantOk: true, wantGranted: 5 * Mib, wantSelected: []ByteCount{3 * Mib, 2 * Mib}},
		{name: "shrink to what other contracts left", balances: []ByteCount{64 * Mib, 8 * Mib}, preReserved: []ByteCount{64 * Mib, 5 * Mib}, requested: 128 * Mib, wantOk: true, wantGranted: 3 * Mib, wantSelected: []ByteCount{3 * Mib}},
		{name: "shrink to floor", balances: []ByteCount{MinShrinkContractTransferByteCount}, requested: 128 * Mib, wantOk: true, wantGranted: MinShrinkContractTransferByteCount, wantSelected: []ByteCount{MinShrinkContractTransferByteCount}},
		{name: "below floor", balances: []ByteCount{MinShrinkContractTransferByteCount / 2, MinShrinkContractTransferByteCount/2 - 1}, requested: 128 * Mib, wantOk: false},
		{name: "small request short", balances: []ByteCount{2048, 2048}, preReserved: []ByteCount{2048, 0}, requested: 3072, wantOk: false},
		{name: "no balance", balances: nil, requested: 128 * Mib, wantOk: false},
		{name: "exact balance", balances: []ByteCount{64 * Mib, 64 * Mib}, requested: 128 * Mib, wantOk: true, wantGranted: 128 * Mib, wantSelected: []ByteCount{64 * Mib, 64 * Mib}},
		{name: "larger balance", balances: []ByteCount{512 * Mib}, requested: 128 * Mib, wantOk: true, wantGranted: 128 * Mib, wantSelected: []ByteCount{128 * Mib}},
		{name: "split across balances", balances: []ByteCount{100 * Mib, 100 * Mib}, requested: 128 * Mib, wantOk: true, wantGranted: 128 * Mib, wantSelected: []ByteCount{100 * Mib, 28 * Mib}},
	} {
		t.Run(test.name, func(t *testing.T) {
			balances := shrinkTestBalances(true, test.balances...)
			fake := newFakeRedisReservations()
			for i, byteCount := range test.preReserved {
				fake.reserved[balances[i].balanceId] = byteCount
			}
			before := map[server.Id]ByteCount{}
			for id, byteCount := range fake.reserved {
				before[id] = byteCount
			}

			selected, granted, priority, err := fake.admit(t, balances, test.requested)
			for _, balance := range balances {
				if balance.balanceByteCount < fake.reserved[balance.balanceId] {
					t.Fatalf("reserved %d of a %d byte balance", fake.reserved[balance.balanceId], balance.balanceByteCount)
				}
			}
			if !test.wantOk {
				if err == nil || selected != nil || granted != 0 {
					t.Fatalf("refusal returned selected=%v granted=%d err=%v", selected, granted, err)
				}
				if err.Error() != fmt.Sprintf("Insufficient balance (%d).", fake.admitted) {
					t.Fatalf("refusal error = %v", err)
				}
				// a refusal holds nothing
				for _, balance := range balances {
					if fake.reserved[balance.balanceId] != before[balance.balanceId] {
						t.Fatalf("refusal kept %d bytes reserved on a balance", fake.reserved[balance.balanceId]-before[balance.balanceId])
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("request of %d refused: %v", test.requested, err)
			}
			if granted != test.wantGranted {
				t.Fatalf("granted = %d, want %d", granted, test.wantGranted)
			}
			if priority != PaidPriority {
				t.Fatalf("priority = %d, want %d", priority, PaidPriority)
			}
			if len(selected) != len(test.wantSelected) {
				t.Fatalf("selected %d balances, want %d", len(selected), len(test.wantSelected))
			}
			sum := ByteCount(0)
			for i, balance := range selected {
				if balance.BalanceByteCount != test.wantSelected[i] {
					t.Errorf("selected %d = %d, want %d", i, balance.BalanceByteCount, test.wantSelected[i])
				}
				if fake.tokens[balance.BalanceId] != balance.BalanceByteCount {
					t.Errorf("escrow row %d differs from its Redis reservation %d", balance.BalanceByteCount, fake.tokens[balance.BalanceId])
				}
				sum += balance.BalanceByteCount
			}
			// the escrow rows, the Redis reservations and the contract size agree
			if sum != granted || len(fake.released) != 0 {
				t.Fatalf("escrow sum %d != granted %d (released %d)", sum, granted, len(fake.released))
			}
		})
	}
}

// A second admission against the same balances sees the first one's
// reservation: shrink-to-fit grants only what is left, and never the same
// bytes twice.
func TestReserveRedisTransferEscrowBalancesNoDoubleReservation(t *testing.T) {
	balances := shrinkTestBalances(false, 3*Mib)
	fake := newFakeRedisReservations()

	_, first, priority, err := fake.admit(t, balances, 2*Mib)
	if err != nil || first != 2*Mib || priority != UnpaidPriority {
		t.Fatalf("first = %d priority %d, %v", first, priority, err)
	}
	_, second, _, err := fake.admit(t, balances, 128*Mib)
	if err != nil || second != 1*Mib {
		t.Fatalf("second = %d, %v; want the remaining %d", second, err, 1*Mib)
	}
	if reserved := fake.reserved[balances[0].balanceId]; reserved != 3*Mib {
		t.Fatalf("reserved %d of a %d balance", reserved, 3*Mib)
	}
	if _, third, _, err := fake.admit(t, balances, 128*Mib); err == nil || third != 0 {
		t.Fatalf("exhausted balance granted %d, %v", third, err)
	}
	if reserved := fake.reserved[balances[0].balanceId]; reserved != 3*Mib {
		t.Fatalf("refusal changed reservations to %d", reserved)
	}
}

// Database path: through the production Redis admission entry points, the
// contract row, escrow rows, and returned escrow record the granted size for
// both origin and companion contracts; below the floor the request is refused
// without writes; an exact or larger balance is unchanged; zero-byte contracts
// keep working.
func TestCreateTransferEscrowShrinkToFit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		addBalance := func(networkId server.Id, byteCount ByteCount) {
			now := server.NowUtc()
			AddTransferBalance(ctx, &TransferBalance{
				NetworkId: networkId,
				StartTime: now.Add(-time.Minute), EndTime: now.Add(24 * time.Hour),
				StartBalanceByteCount: byteCount, BalanceByteCount: byteCount,
			})
		}
		stored := func(contractId server.Id) (contractByteCount ByteCount, escrowByteCount ByteCount) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT transfer_byte_count FROM transfer_contract WHERE contract_id = $1`, contractId).Scan(&contractByteCount))
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(sum(balance_byte_count), 0) FROM transfer_escrow WHERE contract_id = $1`, contractId).Scan(&escrowByteCount))
			})
			return
		}
		contractCount := func(payerNetworkId server.Id) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id = $1`, payerNetworkId).Scan(&count))
			})
			return
		}

		for _, companion := range []bool{false, true} {
			// shrink: 3 MiB available, 128 MiB requested
			clients := newEscrowSelectionTestClients(t, ctx)
			if companion {
				// zero-byte origin anchor, created before any balance exists
				anchor, err := clients.create(ctx, 0, false)
				if err != nil || anchor.TransferByteCount != 0 {
					t.Fatalf("companion=%t: zero-byte anchor = %v, %v", companion, anchor, err)
				}
			}
			addBalance(clients.payerNetworkId, 3*Mib)
			escrow, err := clients.create(ctx, 128*Mib, companion)
			if err != nil {
				t.Fatalf("companion=%t: shrink refused: %v", companion, err)
			}
			if escrow.TransferByteCount != 3*Mib {
				t.Fatalf("companion=%t: granted %d, want %d", companion, escrow.TransferByteCount, 3*Mib)
			}
			if contractByteCount, escrowByteCount := stored(escrow.ContractId); contractByteCount != 3*Mib || escrowByteCount != 3*Mib {
				t.Fatalf("companion=%t: stored contract=%d escrow=%d, want %d", companion, contractByteCount, escrowByteCount, 3*Mib)
			}
			if available := GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId); available != 0 {
				t.Fatalf("companion=%t: %d bytes still available after shrink", companion, available)
			}

			// everything is now held: the next request is refused
			before := contractCount(clients.payerNetworkId)
			if escrow, err := clients.create(ctx, 128*Mib, companion); err == nil || escrow != nil {
				t.Fatalf("companion=%t: exhausted balance returned %v, %v", companion, escrow, err)
			}
			if after := contractCount(clients.payerNetworkId); after != before {
				t.Fatalf("companion=%t: refused request wrote a contract", companion)
			}
		}

		// below the floor: refused without writes
		{
			clients := newEscrowSelectionTestClients(t, ctx)
			addBalance(clients.payerNetworkId, MinShrinkContractTransferByteCount/2)
			if escrow, err := clients.create(ctx, 128*Mib, false); err == nil || escrow != nil {
				t.Fatalf("below floor returned %v, %v", escrow, err)
			}
			if count := contractCount(clients.payerNetworkId); count != 0 {
				t.Fatalf("below floor wrote %d contracts", count)
			}
			if available := GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId); available != MinShrinkContractTransferByteCount/2 {
				t.Fatalf("below floor changed available balance to %d", available)
			}
		}

		// exact and larger balances: the request is granted as is
		for _, balanceByteCount := range []ByteCount{128 * Mib, 512 * Mib} {
			clients := newEscrowSelectionTestClients(t, ctx)
			addBalance(clients.payerNetworkId, balanceByteCount)
			escrow, err := clients.create(ctx, 128*Mib, false)
			if err != nil || escrow.TransferByteCount != 128*Mib {
				t.Fatalf("balance %d: escrow = %v, %v", balanceByteCount, escrow, err)
			}
			if contractByteCount, escrowByteCount := stored(escrow.ContractId); contractByteCount != 128*Mib || escrowByteCount != 128*Mib {
				t.Fatalf("balance %d: stored contract=%d escrow=%d", balanceByteCount, contractByteCount, escrowByteCount)
			}
			if available := GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId); available != balanceByteCount-128*Mib {
				t.Fatalf("balance %d: available %d after escrow", balanceByteCount, available)
			}
		}

		// zero-byte contracts with a balance are unchanged
		{
			clients := newEscrowSelectionTestClients(t, ctx)
			addBalance(clients.payerNetworkId, 5*Mib)
			escrow, err := clients.create(ctx, 0, false)
			if err != nil || escrow.TransferByteCount != 0 {
				t.Fatalf("zero-byte escrow = %v, %v", escrow, err)
			}
			if contractByteCount, escrowByteCount := stored(escrow.ContractId); contractByteCount != 0 || escrowByteCount != 0 {
				t.Fatalf("zero-byte stored contract=%d escrow=%d", contractByteCount, escrowByteCount)
			}
			if available := GetActiveTransferBalanceByteCount(ctx, clients.payerNetworkId); available != 5*Mib {
				t.Fatalf("zero-byte escrow changed available balance to %d", available)
			}
		}
	})
}
