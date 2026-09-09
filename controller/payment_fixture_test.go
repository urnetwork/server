package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// testingCreatePaymentNetworkRow inserts only the network domain row required
// by paid-credit writers, without adding a network_user to subsidy counts.
func testingCreatePaymentNetworkRow(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id)
			 VALUES ($1, $2, $3)`,
			networkId,
			"synthetic-payment-"+networkId.String(),
			server.NewId(),
		))
	})
}

// testingCreatePaymentClient adds the domain-only network plus one client used
// by synthetic contract-payment tests, without adding a network_user.
func testingCreatePaymentClient(ctx context.Context, networkId, clientId server.Id) {
	testingCreatePaymentNetworkRow(ctx, networkId)
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic-payment-client", "synthetic")
}

// testingRedeemPaymentBalanceCode requires a successful synthetic redemption.
func testingRedeemPaymentBalanceCode(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	secret string,
) {
	t.Helper()
	result, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
		Secret:    secret,
		NetworkId: networkId,
	}, ctx)
	if err != nil {
		t.Fatalf("redeem synthetic balance code: %v", err)
	}
	if result == nil {
		t.Fatal("redeem synthetic balance code returned no result")
	}
	if result.Error != nil {
		t.Fatalf("redeem synthetic balance code: %s", result.Error.Message)
	}
	if result.TransferBalance == nil {
		t.Fatal("redeem synthetic balance code returned no transfer balance")
	}
}
