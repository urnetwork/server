package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The webhook acknowledges a TestFlight purchase of a network that is not an
// internal tester and credits nothing: no Pro and no ledger row, so the same
// purchase is credited once the network is on the tester list.
func TestAppleSandboxNotificationOfANonTesterCreditsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "applesandboxnontester", server.NewId())
		installAppleSandboxTesters(t)
		transactionId := "2000000000003401"
		claims := appleSandboxRevenueClaims(networkId, transactionId, "Sandbox", now)

		processed, err := ProcessAppleNotification(ctx, appleSandboxRevenueNotification(claims, now), appleSandboxRevenueProductIds)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, transactionId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		installAppleSandboxTesters(t, networkId)
		processed, err = ProcessAppleNotification(ctx, appleSandboxRevenueNotification(claims, now), appleSandboxRevenueProductIds)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)
		requireSandboxCredit(t, ctx, networkId, transactionId)
	})
}
