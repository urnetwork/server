package model

// Each read bounded by unexpiredContractIdFloorSql still returns a current
// contract and skips one whose id was minted before the bound, even when that
// contract's row is otherwise eligible: open or lingering, with a live
// deadline and the earliest creation time. Such a row cannot occur, since ids
// lead with their creation millisecond; the tests write one so that only the
// id bound can exclude it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// An id minted just before the unexpired bound.
func contractIdBeforeUnexpiredBound() server.Id {
	return contractIdMintedAt(server.NowUtc().Add(-unexpiredContractIdAge - time.Minute))
}

// Copies a contract row under another id, creation time and size, keeping its
// pair, payer, companion origin and deadline. A non-nil close time makes the
// copy closed and undecided: disputed with no outcome, so it is not open and
// needs no usage snapshot.
func insertContractIdBoundCopy(
	t testing.TB,
	ctx context.Context,
	contractId server.Id,
	copyId server.Id,
	createTime time.Time,
	transferByteCount ByteCount,
	closeTime *time.Time,
) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, companion_contract_id, payer_network_id, usage_origin_is_source,
				create_time, priority, expiration_time, dispute, close_time
			)
			SELECT
				$2, source_network_id, source_id, destination_network_id, destination_id,
				$4, companion_contract_id, payer_network_id, usage_origin_is_source,
				$3, priority, expiration_time, $5::timestamp IS NOT NULL, $5::timestamp
			FROM transfer_contract
			WHERE contract_id = $1
		`, contractId, copyId, createTime, transferByteCount, closeTime))
		if tag.RowsAffected() != 1 {
			t.Fatalf("contract %s was not copied", contractId)
		}
	})
}

// Inserts one contract between the pair with a live deadline. A non-nil close
// time makes it closed and undecided, as in insertContractIdBoundCopy.
func insertContractIdBoundContract(
	t testing.TB,
	ctx context.Context,
	contractId server.Id,
	clients escrowSelectionTestClients,
	closeTime *time.Time,
) {
	t.Helper()
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, payer_network_id, create_time, expiration_time, dispute, close_time
			)
			VALUES ($1, $2, $3, $4, $5, 1024, $2, $6, $7, $8::timestamp IS NOT NULL, $8::timestamp)
		`, contractId, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId,
			now.Add(-time.Minute), now.Add(time.Hour), closeTime))
	})
}

// The existence read that precedes every companion skips an origin minted
// before the bound, open or lingering, and a companion on only that origin is
// refused as missing. A current origin exists.
func TestCompanionOriginExistsSkipsIdsBeforeUnexpiredBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		closeTime := server.NowUtc().Add(-time.Minute)
		for _, test := range []struct {
			name      string
			closeTime *time.Time
		}{
			{name: "open", closeTime: nil},
			{name: "lingering", closeTime: &closeTime},
		} {
			clients := newEscrowSelectionTestClients(t, ctx)
			addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
			insertContractIdBoundContract(t, ctx, contractIdBeforeUnexpiredBound(), clients, test.closeTime)
			if CompanionOriginExists(ctx, clients.providerId, clients.payerId, time.Hour) {
				t.Fatalf("%s: an origin minted before the bound exists", test.name)
			}
			companion, err := CreateCompanionTransferEscrow(ctx, clients.providerNetworkId, clients.providerId,
				clients.payerNetworkId, clients.payerId, 1024, time.Hour)
			if companion != nil || !errors.Is(err, ErrMissingCompanionOrigin) {
				t.Fatalf("%s: a companion on an origin minted before the bound = %+v, %v", test.name, companion, err)
			}

			current := server.NewId()
			insertContractIdBoundContract(t, ctx, current, clients, test.closeTime)
			if !CompanionOriginExists(ctx, clients.providerId, clients.payerId, time.Hour) {
				t.Fatalf("%s: a current origin does not exist", test.name)
			}
			companion, err = CreateCompanionTransferEscrow(ctx, clients.providerNetworkId, clients.providerId,
				clients.payerNetworkId, clients.payerId, 1024, time.Hour)
			if err != nil || companion.CompanionContractId == nil || *companion.CompanionContractId != current {
				t.Fatalf("%s: a companion on a current origin = %+v, %v", test.name, companion, err)
			}
		}
	})
}

// The escrow companion's plain and chained origin selections take the earliest
// eligible origin. A copy of the anchor minted before the bound is earlier
// still, open or lingering, and is skipped.
func TestCompanionOriginSelectionSkipsIdsBeforeUnexpiredBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			for _, closed := range []bool{false, true} {
				f := newCoveredCompanionFixture(t, ctx, false, chained)
				var closeTime *time.Time
				if closed {
					for _, origin := range []*TransferEscrow{f.anchor, f.grown} {
						server.Raise(CloseContract(ctx, origin.ContractId, f.clients.payerId, 0, false))
						server.Raise(CloseContract(ctx, origin.ContractId, f.clients.providerId, 0, false))
					}
					closed := server.NowUtc().Add(-time.Minute)
					closeTime = &closed
				}
				insertContractIdBoundCopy(t, ctx, f.anchor.ContractId, contractIdBeforeUnexpiredBound(),
					server.NowUtc().Add(-3*time.Minute), f.anchor.TransferByteCount, closeTime)

				companion, err := f.create(ctx, 64)
				if err != nil || companion.CompanionContractId == nil || *companion.CompanionContractId != f.anchor.ContractId {
					t.Fatalf("chained=%t closed=%t: companion = %+v, %v; want the anchor %s", chained, closed, companion, err, f.anchor.ContractId)
				}
			}
		}
	})
}

// The internal prober clamps its companion to the largest eligible origin. A
// larger copy minted before the bound is not eligible, though it is open with
// a live deadline, so it neither anchors nor raises the reservation.
func TestCompanionProberMaximumSkipsIdsBeforeUnexpiredBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			f := newCoveredCompanionFixture(t, ctx, true, chained)
			// created after the anchor and the grown origin, so it is never the
			// earliest
			insertContractIdBoundCopy(t, ctx, f.grown.ContractId, contractIdBeforeUnexpiredBound(),
				server.NowUtc().Add(-30*time.Second), 4096, nil)

			companion, err := f.create(ctx, 2048)
			if err != nil || companion.CompanionContractId == nil || *companion.CompanionContractId != f.anchor.ContractId {
				t.Fatalf("chained=%t: companion = %+v, %v; want the anchor %s", chained, companion, err, f.anchor.ContractId)
			}
			if companion.TransferByteCount != f.grown.TransferByteCount {
				t.Fatalf("chained=%t: companion reserved %d, want the largest eligible origin's %d", chained, companion.TransferByteCount, f.grown.TransferByteCount)
			}
		}
	})
}

// The zero escrow companion selects its origin as the escrow companion does: a
// copy of the plain or chained origin minted before the bound is earlier, and
// is skipped; with only that copy the origin is missing.
func TestZeroEscrowCompanionOriginSkipsIdsBeforeUnexpiredBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)

		// only an origin minted before the bound
		clients := newEscrowSelectionTestClients(t, ctx)
		insertContractIdBoundContract(t, ctx, contractIdBeforeUnexpiredBound(), clients, nil)
		if reply, err := CreateZeroEscrowCompanionContract(ctx, clients.providerNetworkId, clients.providerId,
			clients.payerNetworkId, clients.payerId, Mib, time.Hour); reply != nil || !errors.Is(err, ErrMissingCompanionOrigin) {
			t.Fatalf("a zero escrow companion on an origin minted before the bound = %+v, %v", reply, err)
		}

		clients = newEscrowSelectionTestClients(t, ctx)
		origin, err := CreateZeroEscrowContract(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, Mib)
		requireZeroEscrowTestResult(t, ctx, origin, err, Mib)
		insertContractIdBoundCopy(t, ctx, origin.ContractId, contractIdBeforeUnexpiredBound(),
			server.NowUtc().Add(-3*time.Minute), Mib, nil)
		reply, err := CreateZeroEscrowCompanionContract(ctx, clients.providerNetworkId, clients.providerId,
			clients.payerNetworkId, clients.payerId, Mib, time.Hour)
		requireZeroEscrowTestResult(t, ctx, reply, err, Mib)
		if state := readZeroEscrowTestState(t, ctx, reply.ContractId); state.companionContractId == nil || *state.companionContractId != origin.ContractId {
			t.Fatalf("plain: reply origin %v, want %s", state.companionContractId, origin.ContractId)
		}

		// the reply is the only companion from the provider, so the carrier
		// chains on it, or on its copy
		insertContractIdBoundCopy(t, ctx, reply.ContractId, contractIdBeforeUnexpiredBound(),
			server.NowUtc().Add(-3*time.Minute), Mib, nil)
		carrier, err := CreateZeroEscrowCompanionContract(ctx, clients.payerNetworkId, clients.payerId,
			clients.providerNetworkId, clients.providerId, Mib, time.Hour)
		requireZeroEscrowTestResult(t, ctx, carrier, err, Mib)
		if state := readZeroEscrowTestState(t, ctx, carrier.ContractId); state.companionContractId == nil || *state.companionContractId != reply.ContractId {
			t.Fatalf("chained: carrier origin %v, want %s", state.companionContractId, reply.ContractId)
		}
	})
}

// Copies a contract's escrow rows to its copy, so the copy is funded as the
// contract is.
func insertContractIdBoundEscrowCopy(t testing.TB, ctx context.Context, contractId server.Id, copyId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count, redis_reserved)
			SELECT $2, balance_id, balance_byte_count, redis_reserved
			FROM transfer_escrow
			WHERE contract_id = $1
		`, contractId, copyId))
		if tag.RowsAffected() == 0 {
			t.Fatalf("contract %s has no escrow to copy", contractId)
		}
	})
}

// The reusable escrows of a pair are its open escrowed contracts that no party
// has closed. A funded copy minted before the bound is skipped, and a current
// contract that one party closed is still excluded through the bounded join.
func TestOpenTransferEscrowsSkipIdsBeforeUnexpiredBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		current := createRedisAdmissionTest(ctx, f, 100)
		partial := createRedisAdmissionTest(ctx, f, 100)
		server.Raise(CloseContract(ctx, partial.ContractId, f.sourceId, 0, false))
		copyId := contractIdBeforeUnexpiredBound()
		insertContractIdBoundCopy(t, ctx, current.ContractId, copyId, server.NowUtc().Add(-3*time.Minute), 100, nil)
		insertContractIdBoundEscrowCopy(t, ctx, current.ContractId, copyId)

		escrows := GetOpenTransferEscrowsOrderedByPriorityCreateTime(ctx, f.sourceId, f.destinationId, 100)
		contractIds := []server.Id{}
		for _, escrow := range escrows {
			contractIds = append(contractIds, escrow.ContractId)
		}
		if len(contractIds) != 1 || contractIds[0] != current.ContractId {
			t.Fatalf("open escrows %v, want only the current contract %s", contractIds, current.ContractId)
		}
	})
}
