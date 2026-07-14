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
