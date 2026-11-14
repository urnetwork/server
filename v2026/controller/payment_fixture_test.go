package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Sends each account message through SendMessageFunc.
type mockAWSMessageSender struct {
	SendMessageFunc func(userAuth string, template Template, sendOpts ...any) error
}

func (c *mockAWSMessageSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	return c.SendMessageFunc(userAuth, template, sendOpts...)
}

// Declares the provider payout transition with the cutoff and prepares its
// boundary.
func controllerPayoutSchedule(t testing.TB, cutoff time.Time) {
	t.Helper()
	data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.UTC().Format(time.RFC3339Nano), "0x"+strings.Repeat("11", 32))
	t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(data)))
	policyForPreparation, preparationErr := server.LoadProviderPayoutTransition(context.Background())
	if preparationErr != nil {
		t.Fatal(preparationErr)
	}
	if _, err := server.PrepareProviderPayoutBoundary(context.Background(), policyForPreparation.ConfigSha256); err != nil {
		t.Fatal(err)
	}
}

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
