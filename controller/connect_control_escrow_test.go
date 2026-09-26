// The serialized control endpoint is an escrow writer in the api process, even
// when a connect or taskworker process originated the contract request.
package controller

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A real earlier reservation exhausts two unpaid grants only in Redis. The api
// response and durable escrow must use only the remaining paid grant.
func TestConnectControlSkipsReservedEscrowGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		payerNetworkId := server.NewId()
		payerUserId := server.NewId()
		payerDeviceId := server.NewId()
		payerId := server.NewId()
		providerNetworkId := server.NewId()
		providerId := server.NewId()
		model.Testing_CreateNetwork(ctx, payerNetworkId, "escrow api payer", payerUserId)
		model.Testing_CreateNetwork(ctx, providerNetworkId, "escrow api provider", server.NewId())
		model.Testing_CreateDevice(ctx, payerNetworkId, payerDeviceId, payerId, "payer", "payer")
		model.Testing_CreateDevice(ctx, providerNetworkId, server.NewId(), providerId, "provider", "provider")
		model.SetProvide(ctx, providerId, map[model.ProvideMode][]byte{
			model.ProvideModePublic: []byte("test-provide-secret-key-public00"),
		})

		byteCount := MinContractTransferByteCount
		now := server.NowUtc()
		var paidBalanceId server.Id
		for index := range 3 {
			balance := &model.TransferBalance{
				NetworkId:             payerNetworkId,
				StartTime:             now.Add(-time.Minute),
				EndTime:               now.Add(time.Duration(index+1) * 24 * time.Hour),
				StartBalanceByteCount: byteCount,
				BalanceByteCount:      byteCount,
			}
			if index == 2 {
				balance.NetRevenue = model.UsdToNanoCents(1)
			}
			model.AddTransferBalance(ctx, balance)
			if index == 2 {
				paidBalanceId = balance.BalanceId
			}
		}
		reserved, err := model.CreateTransferEscrow(ctx, payerNetworkId, payerId,
			providerNetworkId, providerId, 2*byteCount)
		if err != nil {
			t.Fatal(err)
		}
		if len(reserved.Balances) != 2 || reserved.TransferByteCount != 2*byteCount {
			t.Fatal("fixture did not reserve both unpaid grants")
		}

		byJwt := jwt.NewByJwt(payerNetworkId, payerUserId, "escrow api payer", false, false).
			Client(payerDeviceId, payerId)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)
		defer clientSession.Cancel()
		requestFrame, err := connect.ToFrame(&protocol.CreateContract{
			DestinationId:     providerId.Bytes(),
			TransferByteCount: uint64(byteCount),
		}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		requestBytes, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{requestFrame}})
		connect.MessagePoolReturn(requestFrame.MessageBytes)
		if err != nil {
			t.Fatal(err)
		}
		result, err := ConnectControl(
			&ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(requestBytes)},
			clientSession,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.Error != nil {
			t.Fatalf("control error: %s", result.Error.Message)
		}
		responseBytes, err := base64.StdEncoding.DecodeString(result.Pack)
		if err != nil {
			t.Fatal(err)
		}
		responsePack := &protocol.Pack{}
		if err := proto.Unmarshal(responseBytes, responsePack); err != nil {
			t.Fatal(err)
		}
		if len(responsePack.Frames) != 1 {
			t.Fatalf("control response frames=%d, want 1", len(responsePack.Frames))
		}
		message, err := connect.FromFrame(responsePack.Frames[0])
		if err != nil {
			t.Fatal(err)
		}
		contractResult, ok := message.(*protocol.CreateContractResult)
		if !ok || contractResult.Error != nil || contractResult.Contract == nil {
			t.Fatal("control response did not create a contract")
		}
		if contractResult.Contract.ProvideMode != protocol.ProvideMode_Public {
			t.Fatal("cross-network request did not use public escrow")
		}
		storedContract := &protocol.StoredContract{}
		if err := proto.Unmarshal(contractResult.Contract.StoredContractBytes, storedContract); err != nil {
			t.Fatal(err)
		}
		if storedContract.GetPriority() != uint32(model.PaidPriority) {
			t.Errorf("signed priority=%d, want %d", storedContract.GetPriority(), model.PaidPriority)
		}
		if storedContract.TransferByteCount != uint64(byteCount) {
			t.Errorf("signed byte count=%d, want %d", storedContract.TransferByteCount, byteCount)
		}
		contractId, err := server.IdFromBytes(storedContract.ContractId)
		if err != nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `
				SELECT COUNT(*), COUNT(*) FILTER (WHERE balance_byte_count = 0),
					COALESCE(SUM(balance_byte_count), 0),
					COALESCE(BOOL_AND(balance_id = $2), false), MIN(priority)
				FROM transfer_escrow INNER JOIN transfer_contract USING (contract_id)
				WHERE contract_id = $1
			`, contractId, paidBalanceId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing escrow aggregate")
				}
				var escrowCount, zeroCount int
				var fundedByteCount model.ByteCount
				var paidOnly bool
				var priority model.Priority
				server.Raise(rows.Scan(&escrowCount, &zeroCount, &fundedByteCount, &paidOnly, &priority))
				if escrowCount != 1 || zeroCount != 0 || fundedByteCount != byteCount || !paidOnly || priority != model.PaidPriority {
					t.Errorf("api escrow rows=%d zero=%d bytes=%d paid-only=%t priority=%d; want one paid row of %d bytes with priority %d",
						escrowCount, zeroCount, fundedByteCount, paidOnly, priority, byteCount, model.PaidPriority)
				}
			})
		})
	})
}
