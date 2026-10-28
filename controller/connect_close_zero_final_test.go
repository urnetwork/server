package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A client's legacy CloseContract without a report id settles an unused
// contract on the sender's zero final, as an identified report does (owner,
// 2026-10-28). A used sender final still waits for the receiver.
func TestCloseContractLegacyZeroFinalSettlesUnusedContract(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "zero-final", server.NewId())
		sourceId := server.NewId()
		destinationId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "s", "s")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "d", "d")
		balanceCode, err := model.CreateBalanceCode(ctx, 1024*1024*1024, 365*24*60*60*1000000000, 0, "zero-final", "", "")
		connect.AssertEqual(t, err, nil)
		_, err = model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: balanceCode.Secret, NetworkId: networkId}, ctx)
		connect.AssertEqual(t, err, nil)

		unusedContractId, _, err := model.CreateContract(ctx, networkId, sourceId, networkId, destinationId, 4*1024)
		connect.AssertEqual(t, err, nil)
		usedContractId, _, err := model.CreateContract(ctx, networkId, sourceId, networkId, destinationId, 4*1024)
		connect.AssertEqual(t, err, nil)

		// legacy frames: no report id
		connect.AssertEqual(t, CloseContract(ctx, sourceId, &protocol.CloseContract{ContractId: unusedContractId.Bytes()}), nil)
		if contractClose, closed := model.GetContractClose(ctx, unusedContractId); !closed || contractClose.Outcome != string(model.ContractOutcomeSettled) {
			t.Fatal("a client's legacy zero final left its unused contract open")
		}

		connect.AssertEqual(t, CloseContract(ctx, sourceId, &protocol.CloseContract{ContractId: usedContractId.Bytes(), AckedByteCount: 1024}), nil)
		if _, closed := model.GetContractClose(ctx, usedContractId); closed {
			t.Fatal("a used sender final settled without its receiver")
		}
	})
}
