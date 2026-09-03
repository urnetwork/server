package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestBalanceCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()
		guestMode := false
		isPro := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode, isPro),
		)

		checkResult0, err := CheckBalanceCode(
			&CheckBalanceCodeArgs{
				Secret: "foobar",
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, checkResult0.Error, nil)

		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := CreateBalanceCode(
			ctx,
			1024,
			subscriptionYearDuration,
			100,
			"test-purchase-1",
			"rest-purchase-1-receipt",
			"test@bringyour.com",
		)
		connect.AssertEqual(t, err, nil)

		balanceCodeId2, err := GetBalanceCodeIdForPurchaseEventId(ctx, balanceCode.PurchaseEventId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, balanceCode.BalanceCodeId, balanceCodeId2)

		_, err = GetBalanceCodeIdForPurchaseEventId(ctx, "test-purchase-nothing")
		connect.AssertNotEqual(t, err, nil)

		balanceCode2, err := GetBalanceCode(ctx, balanceCode.BalanceCodeId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *balanceCode, *balanceCode2)

		checkResult1, err := CheckBalanceCode(
			&CheckBalanceCodeArgs{
				Secret: balanceCode.Secret,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checkResult1.Error, nil)
		connect.AssertEqual(t, checkResult1.Balance.BalanceByteCount, ByteCount(1024))

		redeemResult0, err := RedeemBalanceCode(
			&RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: clientSessionA.ByJwt.NetworkId,
			},
			ctx,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, redeemResult0.Error, nil)
		connect.AssertEqual(t, redeemResult0.TransferBalance.BalanceByteCount, ByteCount(1024))

		redeemedBalanceCode, err := GetBalanceCode(ctx, balanceCode.BalanceCodeId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, redeemedBalanceCode.RedeemTime.IsZero(), false)
		connect.AssertNotEqual(t, redeemedBalanceCode.RedeemNetworkId, nil)
		connect.AssertEqual(t, *redeemedBalanceCode.RedeemNetworkId, networkIdA)
	})
}

func TestFetchNetworkRedeemedBalanceCodes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()
		guestMode := false
		isPro := false

		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode, isPro),
		)

		redeemed, err := FetchNetworkRedeemedBalanceCodes(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(redeemed), 0)

		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := CreateBalanceCode(
			ctx,
			1024,
			subscriptionYearDuration,
			100,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)

		args := &RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: clientSession.ByJwt.NetworkId,
		}

		redeemResult, err := RedeemBalanceCode(args, ctx)
		connect.AssertEqual(t, err, nil)
		// redeem reports failures via result.Error with a nil Go error; checking
		// only err would let a silent "Unknown balance code." pass and resurface
		// as a 0-vs-1 mismatch on the fetch below.
		connect.AssertEqual(t, redeemResult.Error, nil)

		redeemed, err = FetchNetworkRedeemedBalanceCodes(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(redeemed), 1)
		connect.AssertEqual(t, redeemed[0].BalanceCodeId, balanceCode.BalanceCodeId)
		connect.AssertEqual(t, redeemed[0].Secret, balanceCode.Secret)

	})
}

// TestRedeemBalanceCodeConcurrent pins the S4 race: a double-click, or a webhook
// racing a manual redeem, runs RedeemBalanceCodeInTx twice at once. Both SELECT the
// code as unredeemed under READ COMMITTED; the guarded UPDATE
// (redeem_balance_id IS NULL, rows-affected checked) is what makes exactly one of
// them credit -- the other must report an error result and insert NO balance.
func TestRedeemBalanceCodeConcurrent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()

		balanceCode, err := CreateBalanceCode(
			ctx,
			1024,
			365*24*time.Hour,
			100,
			"concurrent-redeem-purchase-1",
			"concurrent-redeem-receipt-1",
			"test@bringyour.com",
		)
		connect.AssertEqual(t, err, nil)

		start := make(chan struct{})
		type redeemOutcome struct {
			result *RedeemBalanceCodeResult
			err    error
		}
		outcomes := make(chan redeemOutcome, 2)
		for i := 0; i < 2; i += 1 {
			go func() {
				<-start
				result, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{
					Secret:    balanceCode.Secret,
					NetworkId: networkId,
				}, ctx)
				outcomes <- redeemOutcome{result: result, err: err}
			}()
		}
		close(start)

		credits := 0
		for i := 0; i < 2; i += 1 {
			outcome := <-outcomes
			connect.AssertEqual(t, outcome.err, nil)
			if outcome.result.Error == nil {
				credits += 1
			}
		}
		// exactly one redeem credits
		connect.AssertEqual(t, credits, 1)

		// and exactly one balance row exists
		balances := GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].BalanceByteCount, ByteCount(1024))
	})
}
