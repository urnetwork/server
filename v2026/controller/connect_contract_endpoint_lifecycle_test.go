// Main's active lookup and the creation transaction preserve endpoint lifecycle.
package controller

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"google.golang.org/protobuf/proto"
)

type contractEndpointReadFixture struct{ networkId, sourceId, destinationId, balanceId server.Id }

// Fund actual network contracts and publish the destination's ordinary key.
func newContractEndpointReadFixture(ctx context.Context) contractEndpointReadFixture {
	networkId, sourceId, destinationId := contractPairTestClients(ctx)
	f := contractEndpointReadFixture{networkId: networkId, sourceId: sourceId, destinationId: destinationId, balanceId: server.NewId()}
	model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{model.ProvideModeNetwork: bytes.Repeat([]byte{37}, 32)})
	now := server.NowUtc()
	balance := &model.TransferBalance{NetworkId: networkId, StartTime: now.Add(-time.Minute), EndTime: now.Add(time.Hour), StartBalanceByteCount: 64 * MinContractTransferByteCount, BalanceByteCount: 64 * MinContractTransferByteCount, NetRevenue: model.UsdToNanoCents(1)}
	model.AddTransferBalance(ctx, balance)
	f.balanceId = balance.BalanceId
	return f
}

// The active-pair lookup remains before grant discovery. Verify its main SQL
// boundary only after the actual committed allocation and Redis custody.
func TestContractEndpointLookupPreservesMainBoundary(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		ctx, closeOwner := newRouteRejectionOwnerContext(ctx)
		defer closeOwner()
		f := newContractEndpointReadFixture(ctx)
		observed, statements, closeScope := observeRouteRejectionStatements(t, ctx)
		defer closeScope()
		frames, err := CreateContract(observed, f.sourceId, &protocol.CreateContract{DestinationId: f.destinationId.Bytes(), TransferByteCount: uint64(MinContractTransferByteCount)}, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(frames)
		if err != nil || len(frames) != 1 {
			t.Fatal("network creation failed", err)
		}
		message, err := connect.FromFrame(frames[0])
		if err != nil {
			t.Fatal(err)
		}
		created, ok := message.(*protocol.CreateContractResult)
		if !ok || created.Error != nil || created.Contract == nil {
			t.Fatal("network creation did not return its contract")
		}
		stored := &protocol.StoredContract{}
		if err := proto.Unmarshal(created.Contract.StoredContractBytes, stored); err != nil {
			t.Fatal(err)
		}
		escrow := model.GetTransferEscrow(ctx, server.RequireIdFromBytes(stored.ContractId))
		if escrow == nil || escrow.TransferByteCount != model.ByteCount(stored.TransferByteCount) || model.Testing_NetEscrowByteCount(ctx, f.balanceId) != escrow.TransferByteCount {
			t.Fatal("active endpoint lookup changed exact committed allocation")
		}
		activePairReads := 0
		for _, sql := range statements.take() {
			if strings.Contains(sql, "(SELECT network_id FROM network_client WHERE client_id = $1 AND active)") {
				activePairReads++
			}
		}
		if activePairReads != 1 {
			t.Fatalf("main's active-pair boundary changed: queries=%d; want 1", activePairReads)
		}
	})
}

// Committed retirement after the relationship snapshot must still refuse at
// the actual model write boundary and return every pending reservation.
func TestContractEndpointMappingsRetainFreshLifecycleRefusal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		ctx, closeOwner := newRouteRejectionOwnerContext(ctx)
		defer closeOwner()
		for _, retireSource := range []bool{false, true} {
			func() {
				f := newContractEndpointReadFixture(ctx)
				previous := createTransferEscrow
				defer func() { createTransferEscrow = previous }()
				calls := 0
				createTransferEscrow = func(request context.Context, sourceNetworkId, sourceId, destinationNetworkId, destinationId server.Id, amount model.ByteCount) (*model.TransferEscrow, error) {
					calls++
					retired := destinationId
					if retireSource {
						retired = sourceId
					}
					server.Tx(request, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(request, `SELECT client_id FROM network_client WHERE client_id=$1 FOR UPDATE`, retired))
						server.RaisePgResult(tx.Exec(request, `UPDATE network_client SET active=false,deactivate_time=$2 WHERE client_id=$1`, retired, server.NowUtc()))
					}, server.TxReadCommitted, server.OptNoRetry())
					return previous(request, sourceNetworkId, sourceId, destinationNetworkId, destinationId, amount)
				}
				frames, err := CreateContract(ctx, f.sourceId, &protocol.CreateContract{DestinationId: f.destinationId.Bytes(), TransferByteCount: uint64(MinContractTransferByteCount)}, connect.DefaultContractManagerSettings())
				defer returnConnectControlFrames(frames)
				if err != nil || len(frames) != 1 || calls != 1 {
					t.Fatal("retirement did not reach real write boundary", err, calls)
				}
				message, err := connect.FromFrame(frames[0])
				if err != nil {
					t.Fatal(err)
				}
				created, ok := message.(*protocol.CreateContractResult)
				want := protocol.ContractError_Reliability
				if retireSource {
					want = protocol.ContractError_NoPermission
				}
				if !ok || created.Error == nil || *created.Error != want || created.Contract != nil {
					t.Fatal("cached mapping authorized a retired client")
				}
				var contracts int
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, f.networkId).Scan(&contracts))
				})
				if contracts != 0 || model.Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
					t.Fatal("lifecycle refusal left financial state")
				}
			}()
		}
	})
}

// An exhausted payer can refuse before the creation transaction starts.
// Retirement after the relationship read must retain source/destination error
// precedence and skip grant work before any financial writer starts.
func TestContractEndpointMappingsRetainUnfundedLifecyclePrecedence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		ctx, closeOwner := newRouteRejectionOwnerContext(ctx)
		defer closeOwner()
		for _, retireSource := range []bool{false, true} {
			func() {
				f := newContractEndpointReadFixture(ctx)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, f.balanceId, server.NowUtc().Add(-30*time.Second)))
				}, server.TxReadCommitted, server.OptNoRetry())
				observed, statements, closeScope := observeRouteRejectionStatements(t, ctx)
				defer closeScope()
				previous := getProvideSecretKey
				defer func() { getProvideSecretKey = previous }()
				calls := 0
				getProvideSecretKey = func(request context.Context, destinationId server.Id, mode model.ProvideMode) ([]byte, error) {
					calls++
					key, err := previous(request, destinationId, mode)
					if err != nil {
						return nil, err
					}
					retired := f.destinationId
					if retireSource {
						retired = f.sourceId
					}
					server.Tx(request, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(request, `SELECT client_id FROM network_client WHERE client_id=$1 FOR UPDATE`, retired))
						server.RaisePgResult(tx.Exec(request, `UPDATE network_client SET active=false,deactivate_time=$2 WHERE client_id=$1`, retired, server.NowUtc()))
					}, server.TxReadCommitted, server.OptNoRetry())
					return key, nil
				}
				frames, err := CreateContract(observed, f.sourceId, &protocol.CreateContract{DestinationId: f.destinationId.Bytes(), TransferByteCount: uint64(MinContractTransferByteCount)}, connect.DefaultContractManagerSettings())
				defer returnConnectControlFrames(frames)
				if err != nil || len(frames) != 1 || calls != 1 {
					t.Fatal("unfunded retirement did not reach its real boundary", err, calls)
				}
				var contracts int
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, f.networkId).Scan(&contracts))
				})
				if contracts != 0 || model.Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
					t.Fatal("unfunded retirement left financial state")
				}
				message, err := connect.FromFrame(frames[0])
				if err != nil {
					t.Fatal(err)
				}
				created, ok := message.(*protocol.CreateContractResult)
				want := protocol.ContractError_Reliability
				if retireSource {
					want = protocol.ContractError_NoPermission
				}
				if !ok || created.Error == nil || *created.Error != want || created.Contract != nil {
					t.Fatalf("unfunded snapshot retirement changed lifecycle precedence: source=%t result=%v want=%v", retireSource, created, want)
				}
				for _, sql := range statements.take() {
					if strings.Contains(strings.ToLower(sql), "transfer_balance") {
						t.Fatalf("retired endpoint reached grant work: source=%t statement=%s", retireSource, sql)
					}
				}
			}()
		}
	})
}
