// The contract degradation valve at the CreateContract boundary. An open
// contract holds its escrow against the payer's balance, so with escrow a payer
// whose balance is empty or held by unclosed contracts is refused with
// InsufficientBalance. While the valve makes contracts zero cost, the same
// requests are signed without escrow, leave every balance untouched and settle
// with no payout, while a data capped client and an acceptance-test drain are
// still refused. The tests open and close the valve through degraded.yml and
// the published Redis state, the path contract creation reads.
package controller

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
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

// A payer with no transfer balance at all is signed a contract of the requested
// size while the valve is open, with no escrow and no payer, and it settles with
// no payout after the valve closes again. With escrow this request is refused
// with InsufficientBalance.
func TestZeroEscrowPublicContractNeedsNoBalance(t *testing.T) {
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

		state := readZeroEscrowContractState(t, ctx, contractId)
		if state.payerNetworkId != nil || state.escrowCount != 0 {
			t.Fatalf("zero escrow contract has a payer %v or %d escrow rows", state.payerNetworkId, state.escrowCount)
		}
		if state.companionContractId != nil || state.usageOriginIsSource == nil || !*state.usageOriginIsSource {
			t.Fatal("a public contract lost its source usage origin")
		}
		if state.transferByteCount != zeroEscrowSignedSize(requested) || state.priority != model.UnpaidPriority {
			t.Fatalf("stored %d bytes at priority %d, want the signed contract", state.transferByteCount, state.priority)
		}
		if balances := zeroEscrowNetworkBalances(ctx, f.userNetworkId); len(balances) != 0 {
			t.Fatalf("zero escrow creation wrote payer balances %v", balances)
		}
		if open := model.GetOpenTransferByteCount(ctx, f.userNetworkId); open != 0 {
			t.Fatalf("zero escrow contract counts %d open bytes against the payer", open)
		}

		// Settlement reads the missing escrow, not the valve.
		closeValve()
		zeroEscrowCloseContract(t, ctx, contractId, f.userId, f.providerId, requested/2)
		requireZeroEscrowSettledWithoutPayout(t, ctx, contractId)
		if balances := zeroEscrowNetworkBalances(ctx, f.userNetworkId); len(balances) != 0 {
			t.Fatalf("zero escrow settlement wrote payer balances %v", balances)
		}
	})
}

// The control: while the valve is closed a contract escrows the whole balance
// and a second one is refused while the first holds it. Opening the valve
// admits the same payer without touching the held escrow; closing it again
// restores escrow, and the refusal, for new contracts.
func TestZeroEscrowValveClosedKeepsNormalEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, false)()

		size := zeroEscrowSignedSize(2 * model.Mib)
		now := server.NowUtc()
		server.Raise(model.AddBasicTransferBalance(ctx, f.userNetworkId, size, now, now.Add(time.Hour)))
		request := &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(size),
		}

		held, refusal := zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal != nil {
			t.Fatalf("a funded payer was refused while the valve is closed: %s", refusal)
		}
		heldId := server.RequireIdFromBytes(held.ContractId)
		heldState := readZeroEscrowContractState(t, ctx, heldId)
		if heldState.payerNetworkId == nil || *heldState.payerNetworkId != f.userNetworkId || heldState.escrowCount != 1 {
			t.Fatalf("valve closed: contract payer %v with %d escrow rows, want the user network with one", heldState.payerNetworkId, heldState.escrowCount)
		}
		if open := model.GetOpenTransferByteCount(ctx, f.userNetworkId); open != size {
			t.Fatalf("valve closed: %d open bytes, want the escrowed %d", open, size)
		}
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
		admittedState := readZeroEscrowContractState(t, ctx, server.RequireIdFromBytes(admitted.ContractId))
		if admittedState.payerNetworkId != nil || admittedState.escrowCount != 0 {
			t.Fatal("valve open: the contract was escrowed")
		}
		if open := model.GetOpenTransferByteCount(ctx, f.userNetworkId); open != size {
			t.Fatalf("valve open: %d open bytes, want only the held %d", open, size)
		}
		after := zeroEscrowNetworkBalances(ctx, f.userNetworkId)
		if len(after) != len(balances) {
			t.Fatalf("valve open: payer balances changed from %v to %v", balances, after)
		}
		for balanceId, balanceByteCount := range balances {
			if after[balanceId] != balanceByteCount {
				t.Fatalf("valve open: payer balances changed from %v to %v", balances, after)
			}
		}
		if heldAfter := readZeroEscrowContractState(t, ctx, heldId); heldAfter.escrowCount != 1 || heldAfter.outcome != nil {
			t.Fatal("valve open: the held contract's escrow changed")
		}

		defer model.Testing_SetZeroContractCost(ctx, false)()
		_, refusal = zeroEscrowCreateContract(t, ctx, f.userId, request, f.providerSecret)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("valve closed again: got %v, want InsufficientBalance", refusal)
		}
	})
}

// A companion is paid by its destination. Here the user's whole balance is held
// by the escrowed origin, so with escrow the provider's companion reply is
// refused with InsufficientBalance. While the valve is open the companion is signed
// for the requested size, keeps its origin link and the destination's usage
// origin, takes no escrow and settles with no payout.
func TestZeroEscrowCompanionContractNeedsNoBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		f := newZeroEscrowFixture(ctx)
		defer model.Testing_SetZeroContractCost(ctx, false)()

		originSize := zeroEscrowSignedSize(2 * model.Mib)
		now := server.NowUtc()
		server.Raise(model.AddBasicTransferBalance(ctx, f.userNetworkId, originSize, now, now.Add(time.Hour)))
		origin, refusal := zeroEscrowCreateContract(t, ctx, f.userId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(originSize),
		}, f.providerSecret)
		if refusal != nil {
			t.Fatalf("the escrowed origin was refused: %s", refusal)
		}
		originId := server.RequireIdFromBytes(origin.ContractId)
		balances := zeroEscrowNetworkBalances(ctx, f.userNetworkId)

		defer model.Testing_SetZeroContractCost(ctx, true)()
		requested := 3 * model.Mib
		streamVersion := uint32(connect.DefaultStreamVersion)
		companion, refusal := zeroEscrowCreateContract(t, ctx, f.providerId, &protocol.CreateContract{
			DestinationId:     f.userId.Bytes(),
			TransferByteCount: uint64(requested),
			Companion:         true,
			StreamVersion:     &streamVersion,
		}, f.userSecret)
		if refusal != nil {
			t.Fatalf("a companion whose payer's balance an open contract holds was refused while the valve is open: %s", refusal)
		}
		companionId := server.RequireIdFromBytes(companion.ContractId)
		if companion.TransferByteCount != uint64(zeroEscrowSignedSize(requested)) {
			t.Fatalf("signed %d bytes, want the requested %d", companion.TransferByteCount, zeroEscrowSignedSize(requested))
		}
		// the payer's only grant is unpaid
		if companion.Priority == nil || *companion.Priority != model.UnpaidPriority {
			t.Fatalf("signed priority %v, want the payer's unpaid %d", companion.Priority, model.UnpaidPriority)
		}

		state := readZeroEscrowContractState(t, ctx, companionId)
		if state.companionContractId == nil || *state.companionContractId != originId {
			t.Fatalf("companion origin %v, want %s", state.companionContractId, originId)
		}
		if state.usageOriginIsSource == nil || *state.usageOriginIsSource {
			t.Fatal("a companion lost its destination usage origin")
		}
		if state.payerNetworkId != nil || state.escrowCount != 0 {
			t.Fatalf("zero escrow companion has a payer %v or %d escrow rows", state.payerNetworkId, state.escrowCount)
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
		if originState := readZeroEscrowContractState(t, ctx, originId); originState.outcome != nil || originState.escrowCount != 1 {
			t.Fatal("closing the companion changed its escrowed origin")
		}
	})
}

// A per-client data cap is the user's own limit, not balance: a capped payer is
// refused while the valve is open, and clearing the cap admits it.
func TestZeroEscrowStillRefusesDataCappedClient(t *testing.T) {
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
func TestZeroEscrowStillRefusesTestBalanceDrain(t *testing.T) {
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
