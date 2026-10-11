// The contract degradation valve at the CreateContract boundary. Contract
// creation deducts first: while the payer has anything left, a contract
// escrows, shrink-to-fit included, whether or not the valve is open. Only a
// payer whose balance escrow admission finds exhausted is affected by the
// valve: open, the contract is signed without escrow and settles with no
// payout; closed, it is refused with InsufficientBalance. A data capped
// client and an acceptance-test drain are refused either way. The tests open
// and close the valve through degraded.yml and the published Redis state,
// the path contract creation reads.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A user and a public provider in two synthetic networks. The provider
// publishes the public mode and the user the stream mode, so the user can
// open a contract to the provider and the provider can answer it with a
// companion. Neither network has a balance unless a test adds one.
type zeroEscrowFixture struct {
	userNetworkId     server.Id
	userAdminUserId   server.Id
	userNetworkName   string
	userId            server.Id
	providerNetworkId server.Id
	providerId        server.Id
	providerSecret    []byte
	userSecret        []byte
}

// Creates the fixture's networks, clients and provide keys.
func newZeroEscrowFixture(ctx context.Context) *zeroEscrowFixture {
	f := &zeroEscrowFixture{
		userNetworkId:     server.NewId(),
		userAdminUserId:   server.NewId(),
		userId:            server.NewId(),
		providerNetworkId: server.NewId(),
		providerId:        server.NewId(),
		providerSecret:    []byte("synthetic-zero-escrow-provider-key"),
		userSecret:        []byte("synthetic-zero-escrow-user-key"),
	}
	f.userNetworkName = "zero-escrow-user-" + f.userNetworkId.String()
	model.Testing_CreateNetwork(ctx, f.userNetworkId, f.userNetworkName, f.userAdminUserId)
	model.Testing_CreateNetwork(ctx, f.providerNetworkId, "zero-escrow-provider-"+f.providerNetworkId.String(), server.NewId())
	model.Testing_CreateDevice(ctx, f.userNetworkId, server.NewId(), f.userId, "synthetic zero escrow user", "synthetic")
	model.Testing_CreateDevice(ctx, f.providerNetworkId, server.NewId(), f.providerId, "synthetic zero escrow provider", "synthetic")
	model.SetProvide(ctx, f.providerId, map[model.ProvideMode][]byte{model.ProvideModePublic: f.providerSecret})
	model.SetProvide(ctx, f.userId, map[model.ProvideMode][]byte{model.ProvideModeStream: f.userSecret})
	return f
}

// The size the controller signs for a request on one hop with no plan cap.
func zeroEscrowSignedSize(requested model.ByteCount) model.ByteCount {
	return min(max(MinContractTransferByteCount, requested), MaxContractTransferByteCount)
}

// Requests a contract through CreateContract. Returns the signed contract,
// verified against the destination's provide key, or the protocol refusal.
func zeroEscrowCreateContract(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	createContract *protocol.CreateContract,
	destinationSecret []byte,
) (*protocol.StoredContract, *protocol.ContractError) {
	t.Helper()
	frames, err := CreateContract(ctx, clientId, createContract, connect.DefaultContractManagerSettings())
	defer returnConnectControlFrames(frames)
	if err != nil {
		t.Fatalf("create contract failed outside the protocol: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("create contract replied with %d frames", len(frames))
	}
	message, err := connect.FromFrame(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	result, ok := message.(*protocol.CreateContractResult)
	if !ok {
		t.Fatalf("create contract replied with %T", message)
	}
	if result.Error != nil {
		return nil, result.Error
	}
	if result.Contract == nil {
		t.Fatal("create contract replied with neither a contract nor an error")
	}
	if !connect.VerifyStoredContract(connect.DefaultContractManagerSettings(), destinationSecret,
		result.Contract.StoredContractBytes, result.Contract.StoredContractHmac) {
		t.Fatal("the contract is not signed with the destination's provide key")
	}
	stored := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, stored); err != nil {
		t.Fatal(err)
	}
	return stored, nil
}

// One contract's row and its financial custody as settlement reads it.
type zeroEscrowContractState struct {
	payerNetworkId      *server.Id
	companionContractId *server.Id
	usageOriginIsSource *bool
	priority            model.Priority
	transferByteCount   model.ByteCount
	outcome             *string
	escrowCount         int
	sweepCount          int
	debitCount          int
	providerTotalsTask  bool
}

// Reads the contract row with its escrow, sweep and debit rows and the
// provider totals task that a payout would schedule.
func readZeroEscrowContractState(t testing.TB, ctx context.Context, contractId server.Id) zeroEscrowContractState {
	t.Helper()
	var state zeroEscrowContractState
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					payer_network_id,
					companion_contract_id,
					usage_origin_is_source,
					priority,
					transfer_byte_count,
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
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(
					&state.payerNetworkId,
					&state.companionContractId,
					&state.usageOriginIsSource,
					&state.priority,
					&state.transferByteCount,
					&state.outcome,
					&state.escrowCount,
					&state.sweepCount,
					&state.debitCount,
					&state.providerTotalsTask,
				))
			}
		})
	})
	if !found {
		t.Fatalf("contract %s was not created", contractId)
	}
	return state
}

// Every transfer balance of a network with its remaining byte count.
func zeroEscrowNetworkBalances(ctx context.Context, networkId server.Id) map[server.Id]model.ByteCount {
	balances := map[server.Id]model.ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT balance_id, balance_byte_count FROM transfer_balance WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				var balanceByteCount model.ByteCount
				server.Raise(result.Scan(&balanceId, &balanceByteCount))
				balances[balanceId] = balanceByteCount
			}
		})
	})
	return balances
}

// Both parties close with the same acked byte count, as ordinary final closes.
func zeroEscrowCloseContract(t testing.TB, ctx context.Context, contractId server.Id, sourceId server.Id, destinationId server.Id, ackedByteCount model.ByteCount) {
	t.Helper()
	for _, clientId := range []server.Id{sourceId, destinationId} {
		err := CloseContract(ctx, clientId, &protocol.CloseContract{
			ContractId:     contractId.Bytes(),
			AckedByteCount: uint64(ackedByteCount),
		})
		if err != nil {
			t.Fatalf("close by %s: %v", clientId, err)
		}
	}
}

// The contract reached the settled outcome with no escrow, sweep, debit or
// provider totals: nothing was charged and no provider was paid or credited.
func requireZeroEscrowSettledWithoutPayout(t testing.TB, ctx context.Context, contractId server.Id) {
	t.Helper()
	state := readZeroEscrowContractState(t, ctx, contractId)
	if state.outcome == nil || *state.outcome != string(model.ContractOutcomeSettled) {
		t.Fatalf("zero escrow contract did not settle: outcome=%v", state.outcome)
	}
	if state.escrowCount != 0 || state.sweepCount != 0 || state.debitCount != 0 || state.providerTotalsTask {
		t.Fatalf("zero escrow settlement took financial custody: escrow=%d sweep=%d debit=%d provider_totals=%t",
			state.escrowCount, state.sweepCount, state.debitCount, state.providerTotalsTask)
	}
}

// The contract is escrowed against the payer network, so its balance is
// charged as usual.
func requireZeroEscrowEscrowed(t testing.TB, ctx context.Context, contractId server.Id, payerNetworkId server.Id) {
	t.Helper()
	state := readZeroEscrowContractState(t, ctx, contractId)
	if state.payerNetworkId == nil || *state.payerNetworkId != payerNetworkId || state.escrowCount == 0 {
		t.Fatalf("contract %s has payer %v and %d escrow rows, want escrow against %s", contractId, state.payerNetworkId, state.escrowCount, payerNetworkId)
	}
}

// The contract was created free: no escrow and no payer.
func requireZeroEscrowFree(t testing.TB, ctx context.Context, contractId server.Id) {
	t.Helper()
	state := readZeroEscrowContractState(t, ctx, contractId)
	if state.payerNetworkId != nil || state.escrowCount != 0 {
		t.Fatalf("contract %s has payer %v and %d escrow rows, want a free contract", contractId, state.payerNetworkId, state.escrowCount)
	}
}

// Adds an unpaid grant of byteCount to a network, valid for the next hour.
// It starts a minute ago: admission rechecks the start against the database
// clock, which may trail this process's clock.
func addZeroEscrowGrant(ctx context.Context, networkId server.Id, byteCount model.ByteCount) {
	now := server.NowUtc()
	server.Raise(model.AddBasicTransferBalance(ctx, networkId, byteCount, now.Add(-time.Minute), now.Add(time.Hour)))
}

// Case 1: with the valve open a funded payer is still charged. The contract
// escrows the request against the payer and its available balance decreases by
// that much. The previous valve gave this payer a free contract.
func TestValveOpenEscrowsFundedPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, true)()
		addZeroEscrowGrant(ctx, f.userNetworkId, 64*model.Mib)
		before := model.GetActiveTransferBalanceByteCount(ctx, f.userNetworkId)

		requested := 4 * model.Mib
		stored, refusal := zeroEscrowCreateContract(t, ctx, f.userId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(requested),
		}, f.providerSecret)
		if refusal != nil {
			t.Fatalf("a funded payer was refused while the valve is open: %s", refusal)
		}
		if stored.TransferByteCount != uint64(zeroEscrowSignedSize(requested)) {
			t.Fatalf("signed %d bytes, want the requested %d", stored.TransferByteCount, zeroEscrowSignedSize(requested))
		}
		requireZeroEscrowEscrowed(t, ctx, server.RequireIdFromBytes(stored.ContractId), f.userNetworkId)
		if after := model.GetActiveTransferBalanceByteCount(ctx, f.userNetworkId); after != before-zeroEscrowSignedSize(requested) {
			t.Fatalf("available balance went from %d to %d, want it to drop by %d", before, after, zeroEscrowSignedSize(requested))
		}
		if open := model.GetOpenTransferByteCount(ctx, f.userNetworkId); open != zeroEscrowSignedSize(requested) {
			t.Fatalf("%d open bytes against the payer, want %d", open, zeroEscrowSignedSize(requested))
		}
	})
}

// Case 2: with the valve open a partially funded payer is charged what it has
// left. A request larger than the balance shrinks to the balance and escrows
// it, with no fallback while any balance remains; only the next request, once
// nothing is left, is free.
func TestValveOpenShrinksBeforeFallingBack(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, true)()
		available := 2 * model.Mib
		addZeroEscrowGrant(ctx, f.userNetworkId, available)
		request := &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(8 * model.Mib),
		}

		shrunk, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal != nil {
			t.Fatalf("a partially funded payer was refused while the valve is open: %s", refusal)
		}
		if shrunk.TransferByteCount != uint64(available) {
			t.Fatalf("signed %d bytes, want the %d left in the balance", shrunk.TransferByteCount, available)
		}
		requireZeroEscrowEscrowed(t, ctx, server.RequireIdFromBytes(shrunk.ContractId), f.userNetworkId)
		if left := model.GetActiveTransferBalanceByteCount(ctx, f.userNetworkId); left != 0 {
			t.Fatalf("%d bytes still available after the shrunk contract, want 0", left)
		}

		free, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal != nil {
			t.Fatalf("an exhausted payer was refused while the valve is open: %s", refusal)
		}
		if free.TransferByteCount != uint64(zeroEscrowSignedSize(8*model.Mib)) {
			t.Fatalf("the free contract signed %d bytes, want the request %d", free.TransferByteCount, zeroEscrowSignedSize(8*model.Mib))
		}
		requireZeroEscrowFree(t, ctx, server.RequireIdFromBytes(free.ContractId))
		if open := model.GetOpenTransferByteCount(ctx, f.userNetworkId); open != available {
			t.Fatalf("%d open bytes against the payer, want only the shrunk contract's %d", open, available)
		}
	})
}

// Case 3: with the valve open a payer with no balance at all is signed a free
// contract of the requested size, with no escrow and no payer, and it settles
// with no payout after the valve closes again.
func TestValveOpenGivesExhaustedPayerFreeContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		closeValve := model.Testing_SetZeroContractCost(ctx, true)
		defer closeValve()

		requested := 4 * model.Mib
		stored, refusal := zeroEscrowCreateContract(t, ctx, f.userId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(requested),
		}, f.providerSecret)
		if refusal != nil {
			t.Fatalf("a payer with no balance was refused while the valve is open: %s", refusal)
		}
		contractId := server.RequireIdFromBytes(stored.ContractId)
		if stored.TransferByteCount != uint64(zeroEscrowSignedSize(requested)) {
			t.Fatalf("signed %d bytes, want the requested %d", stored.TransferByteCount, zeroEscrowSignedSize(requested))
		}
		// no grant: the unpaid priority escrow gives a payer with no grant
		if stored.Priority == nil || *stored.Priority != model.UnpaidPriority {
			t.Fatalf("signed priority %v, want unpaid %d", stored.Priority, model.UnpaidPriority)
		}
		requireZeroEscrowFree(t, ctx, contractId)
		state := readZeroEscrowContractState(t, ctx, contractId)
		if state.companionContractId != nil || state.usageOriginIsSource == nil || !*state.usageOriginIsSource {
			t.Fatal("a public contract lost its source usage origin")
		}
		if state.transferByteCount != zeroEscrowSignedSize(requested) || state.priority != model.UnpaidPriority {
			t.Fatalf("stored %d bytes at priority %d, want the signed contract", state.transferByteCount, state.priority)
		}
		if balances := zeroEscrowNetworkBalances(ctx, f.userNetworkId); len(balances) != 0 {
			t.Fatalf("free creation wrote payer balances %v", balances)
		}

		// Settlement reads the missing escrow, not the valve.
		closeValve()
		zeroEscrowCloseContract(t, ctx, contractId, f.userId, f.providerId, requested/2)
		requireZeroEscrowSettledWithoutPayout(t, ctx, contractId)
		if balances := zeroEscrowNetworkBalances(ctx, f.userNetworkId); len(balances) != 0 {
			t.Fatalf("free settlement wrote payer balances %v", balances)
		}
	})
}

// Case 4: with the valve closed a payer with no balance is refused with
// InsufficientBalance and no contract is created.
func TestValveClosedRefusesExhaustedPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, false)()

		_, refusal := zeroEscrowCreateContract(t, ctx, f.userId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(model.Mib),
		}, f.providerSecret)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("a payer with no balance got %v while the valve is closed, want InsufficientBalance", refusal)
		}
		var contractCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE source_id = $1`, f.userId).Scan(&contractCount))
		})
		if contractCount != 0 {
			t.Fatalf("the refused payer has %d contracts", contractCount)
		}
	})
}

// The valve decides only for an exhausted payer. Closed, a balance held by an
// open contract refuses the next one; opening the valve makes that next one
// free without touching the held escrow; closing it again restores the refusal.
func TestValveFallsBackOnlyWhileOpen(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, false)()

		size := zeroEscrowSignedSize(2 * model.Mib)
		addZeroEscrowGrant(ctx, f.userNetworkId, size)
		request := &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(size),
		}

		held, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal != nil {
			t.Fatalf("a funded payer was refused while the valve is closed: %s", refusal)
		}
		heldId := server.RequireIdFromBytes(held.ContractId)
		requireZeroEscrowEscrowed(t, ctx, heldId, f.userNetworkId)
		balances := zeroEscrowNetworkBalances(ctx, f.userNetworkId)

		_, refusal = zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("valve closed: a payer whose balance an open contract holds got %v, want InsufficientBalance", refusal)
		}

		defer model.Testing_SetZeroContractCost(ctx, true)()
		admitted, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal != nil {
			t.Fatalf("valve open: a payer whose balance an open contract holds was refused: %s", refusal)
		}
		requireZeroEscrowFree(t, ctx, server.RequireIdFromBytes(admitted.ContractId))
		if open := model.GetOpenTransferByteCount(ctx, f.userNetworkId); open != size {
			t.Fatalf("valve open: %d open bytes, want only the held %d", open, size)
		}
		after := zeroEscrowNetworkBalances(ctx, f.userNetworkId)
		for balanceId, balanceByteCount := range balances {
			if after[balanceId] != balanceByteCount || len(after) != len(balances) {
				t.Fatalf("valve open: payer balances changed from %v to %v", balances, after)
			}
		}
		if heldAfter := readZeroEscrowContractState(t, ctx, heldId); heldAfter.escrowCount == 0 || heldAfter.outcome != nil {
			t.Fatal("valve open: the held contract's escrow changed")
		}

		defer model.Testing_SetZeroContractCost(ctx, false)()
		_, refusal = zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("valve closed again: got %v, want InsufficientBalance", refusal)
		}
	})
}

// Creates the user's escrowed origin to the provider, the contract a provider
// companion answers.
func createZeroEscrowOrigin(t testing.TB, ctx context.Context, f *zeroEscrowFixture, size model.ByteCount) server.Id {
	t.Helper()
	origin, refusal := zeroEscrowCreateContract(t, ctx, f.userId, &protocol.CreateContract{
		DestinationId:     f.providerId.Bytes(),
		TransferByteCount: uint64(size),
	}, f.providerSecret)
	if refusal != nil {
		t.Fatalf("the origin was refused: %s", refusal)
	}
	originId := server.RequireIdFromBytes(origin.ContractId)
	requireZeroEscrowEscrowed(t, ctx, originId, f.userNetworkId)
	return originId
}

// The provider's companion reply to the user, paid by the user.
func createZeroEscrowCompanion(t testing.TB, ctx context.Context, f *zeroEscrowFixture, size model.ByteCount) (*protocol.StoredContract, *protocol.ContractError) {
	t.Helper()
	streamVersion := uint32(connect.DefaultStreamVersion)
	return zeroEscrowCreateContract(t, ctx, f.providerId, &protocol.CreateContract{
		DestinationId:     f.userId.Bytes(),
		TransferByteCount: uint64(size),
		Companion:         true,
		StreamVersion:     &streamVersion,
	}, f.userSecret)
}

// Companion case 1: with the valve open a companion paid by a funded user is
// escrowed against the user, linked to its origin, and the user's available
// balance decreases by it.
func TestValveOpenEscrowsFundedCompanionPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, true)()
		addZeroEscrowGrant(ctx, f.userNetworkId, 64*model.Mib)
		originId := createZeroEscrowOrigin(t, ctx, f, zeroEscrowSignedSize(2*model.Mib))
		before := model.GetActiveTransferBalanceByteCount(ctx, f.userNetworkId)

		requested := 3 * model.Mib
		companion, refusal := createZeroEscrowCompanion(t, ctx, f, requested)
		if refusal != nil {
			t.Fatalf("a companion paid by a funded user was refused while the valve is open: %s", refusal)
		}
		companionId := server.RequireIdFromBytes(companion.ContractId)
		requireZeroEscrowEscrowed(t, ctx, companionId, f.userNetworkId)
		state := readZeroEscrowContractState(t, ctx, companionId)
		if state.companionContractId == nil || *state.companionContractId != originId {
			t.Fatalf("companion origin %v, want %s", state.companionContractId, originId)
		}
		if after := model.GetActiveTransferBalanceByteCount(ctx, f.userNetworkId); after != before-model.ByteCount(companion.TransferByteCount) {
			t.Fatalf("available balance went from %d to %d, want it to drop by the companion's %d", before, after, companion.TransferByteCount)
		}
	})
}

// Companion case 3: the user's whole balance is held by the escrowed origin, so
// escrow admission finds the companion's payer exhausted. With the valve open
// the companion is free: signed for the request, linked to its origin with the
// destination's usage origin, no escrow, and it settles with no payout.
func TestValveOpenGivesExhaustedCompanionPayerFreeContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, true)()
		originSize := zeroEscrowSignedSize(2 * model.Mib)
		addZeroEscrowGrant(ctx, f.userNetworkId, originSize)
		// a funded origin escrows even with the valve open
		originId := createZeroEscrowOrigin(t, ctx, f, originSize)
		balances := zeroEscrowNetworkBalances(ctx, f.userNetworkId)

		requested := 3 * model.Mib
		companion, refusal := createZeroEscrowCompanion(t, ctx, f, requested)
		if refusal != nil {
			t.Fatalf("a companion whose payer is exhausted was refused while the valve is open: %s", refusal)
		}
		companionId := server.RequireIdFromBytes(companion.ContractId)
		if companion.TransferByteCount != uint64(zeroEscrowSignedSize(requested)) {
			t.Fatalf("signed %d bytes, want the requested %d", companion.TransferByteCount, zeroEscrowSignedSize(requested))
		}
		// the payer's only grant is unpaid
		if companion.Priority == nil || *companion.Priority != model.UnpaidPriority {
			t.Fatalf("signed priority %v, want the payer's unpaid %d", companion.Priority, model.UnpaidPriority)
		}
		requireZeroEscrowFree(t, ctx, companionId)
		state := readZeroEscrowContractState(t, ctx, companionId)
		if state.companionContractId == nil || *state.companionContractId != originId {
			t.Fatalf("companion origin %v, want %s", state.companionContractId, originId)
		}
		if state.usageOriginIsSource == nil || *state.usageOriginIsSource {
			t.Fatal("a companion lost its destination usage origin")
		}
		if open := model.GetOpenTransferByteCount(ctx, f.userNetworkId); open != originSize {
			t.Fatalf("%d open bytes, want only the origin's %d", open, originSize)
		}
		after := zeroEscrowNetworkBalances(ctx, f.userNetworkId)
		for balanceId, balanceByteCount := range balances {
			if after[balanceId] != balanceByteCount || len(after) != len(balances) {
				t.Fatalf("payer balances changed from %v to %v", balances, after)
			}
		}

		zeroEscrowCloseContract(t, ctx, companionId, f.providerId, f.userId, requested/2)
		requireZeroEscrowSettledWithoutPayout(t, ctx, companionId)
		if originState := readZeroEscrowContractState(t, ctx, originId); originState.outcome != nil || originState.escrowCount == 0 {
			t.Fatal("closing the companion changed its escrowed origin")
		}
	})
}

// Companion case 4: with the valve closed, a companion whose payer's balance
// its origin holds is refused with InsufficientBalance.
func TestValveClosedRefusesExhaustedCompanionPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, false)()
		originSize := zeroEscrowSignedSize(2 * model.Mib)
		addZeroEscrowGrant(ctx, f.userNetworkId, originSize)
		createZeroEscrowOrigin(t, ctx, f, originSize)

		_, refusal := createZeroEscrowCompanion(t, ctx, f, 3*model.Mib)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("an exhausted companion payer got %v while the valve is closed, want InsufficientBalance", refusal)
		}
		var companionCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE source_id = $1`, f.providerId).Scan(&companionCount))
		})
		if companionCount != 0 {
			t.Fatalf("the refused companion created %d contracts", companionCount)
		}
	})
}

// A per-client data cap is the user's own limit, not balance: a capped payer is
// refused while the valve is open, and clearing the cap admits it.
func TestValveOpenStillRefusesDataCappedClient(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		model.Testing_ResetClientDataCapState()
		defer model.Testing_ResetClientDataCapState()
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, true)()

		model.Testing_EnableNetworkEmbed(ctx, f.userNetworkId)
		networkSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(f.userNetworkId, f.userAdminUserId, f.userNetworkName, false, false))
		defer networkSession.Cancel()
		setMonthlyCap := func(limit *model.ByteCount) {
			args := &model.SetClientDataCapArgs{ClientId: f.userId}
			args.SetMonthlyByteLimit(limit)
			result, err := model.SetClientDataCap(args, networkSession)
			if err != nil || result == nil || result.Error != nil {
				t.Fatalf("set data cap: %v %+v", err, result)
			}
		}
		// a zero monthly cap pauses the client
		pause := model.ByteCount(0)
		setMonthlyCap(&pause)

		request := &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(model.Mib),
		}
		_, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("a capped client got %v while the valve is open, want InsufficientBalance", refusal)
		}
		var contractCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE source_id = $1`, f.userId).Scan(&contractCount))
		})
		if contractCount != 0 {
			t.Fatalf("the refused capped client has %d contracts", contractCount)
		}

		setMonthlyCap(nil)
		if _, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret); refusal != nil {
			t.Fatalf("clearing the cap did not admit the client: %s", refusal)
		}
	})
}

// The acceptance-test balance drain is an explicit test switch: a drained payer
// is refused while the valve is open, and restoring the drain admits it.
func TestValveOpenStillRefusesTestBalanceDrain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, true)()
		model.Testing_SetTestBalanceDrainAllowlist([]server.Id{f.userNetworkId})
		defer model.Testing_SetTestBalanceDrainAllowlist(nil)
		if _, err := model.DrainTestBalance(ctx, f.userNetworkId, 5*time.Minute); err != nil {
			t.Fatal(err)
		}

		request := &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(model.Mib),
		}
		_, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("a drained payer got %v while the valve is open, want InsufficientBalance", refusal)
		}

		if restored := model.RestoreTestBalance(ctx, f.userNetworkId); restored != 1 {
			t.Fatalf("restored %d drains, want 1", restored)
		}
		if _, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret); refusal != nil {
			t.Fatalf("restoring the drain did not admit the payer: %s", refusal)
		}
	})
}

// A probe shard follows the same rule. With the valve open a funded shard
// escrows against its private grant; once the grant is exhausted the shard's
// next contract is free; with the valve closed it is refused.
func TestValveDeductsProberShardBeforeFreeData(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		owner, err := model.BeginProberShard(ctx, model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardCount: 1}, 8*model.Mib, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		peerNetworkId, peerId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, peerNetworkId, "zero-escrow-shard-peer-"+peerNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(ctx, peerNetworkId, server.NewId(), peerId, "synthetic shard peer", "synthetic")
		create := func() (server.Id, error) {
			contractId, _, _, _, _, err := newContract(ctx, owner.ClientId, peerId, nil, false, true, model.Mib,
				model.ProvideModePublic, false, 0, connect.DefaultContractManagerSettings())
			return contractId, err
		}

		closeValve := model.Testing_SetZeroContractCost(ctx, true)
		defer closeValve()
		funded, err := create()
		if err != nil {
			t.Fatalf("a funded shard was refused while the valve is open: %v", err)
		}
		requireZeroEscrowEscrowed(t, ctx, funded, owner.NetworkId)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count = 0 WHERE balance_id = $1`, owner.BalanceId))
		})
		free, err := create()
		if err != nil {
			t.Fatalf("an exhausted shard was refused while the valve is open: %v", err)
		}
		requireZeroEscrowFree(t, ctx, free)

		closeValve()
		defer model.Testing_SetZeroContractCost(ctx, false)()
		if _, err := create(); !errors.Is(err, model.ErrContractBalanceExhausted) {
			t.Fatalf("an exhausted shard got %v while the valve is closed, want the exhausted balance refusal", err)
		}
	})
}
