package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestBalanceCode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
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
		assert.Equal(t, err, nil)
		assert.NotEqual(t, checkResult0.Error, nil)

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
		assert.Equal(t, err, nil)

		balanceCodeId2, err := GetBalanceCodeIdForPurchaseEventId(ctx, balanceCode.PurchaseEventId)
		assert.Equal(t, err, nil)
		assert.Equal(t, balanceCode.BalanceCodeId, balanceCodeId2)

		_, err = GetBalanceCodeIdForPurchaseEventId(ctx, "test-purchase-nothing")
		assert.NotEqual(t, err, nil)

		balanceCode2, err := GetBalanceCode(ctx, balanceCode.BalanceCodeId)
		assert.Equal(t, err, nil)
		assert.Equal(t, *balanceCode, *balanceCode2)

		checkResult1, err := CheckBalanceCode(
			&CheckBalanceCodeArgs{
				Secret: balanceCode.Secret,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, checkResult1.Error, nil)
		assert.Equal(t, checkResult1.Balance.BalanceByteCount, ByteCount(1024))

		redeemResult0, err := RedeemBalanceCode(
			&RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: clientSessionA.ByJwt.NetworkId,
			},
			ctx,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, redeemResult0.Error, nil)
		assert.Equal(t, redeemResult0.TransferBalance.BalanceByteCount, ByteCount(1024))
	})
}

func TestFetchNetworkRedeemedBalanceCodes(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
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
		assert.Equal(t, err, nil)
		assert.Equal(t, len(redeemed), 0)

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
		assert.Equal(t, err, nil)

		args := &RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: clientSession.ByJwt.NetworkId,
		}

		_, err = RedeemBalanceCode(args, ctx)
		assert.Equal(t, err, nil)

		redeemed, err = FetchNetworkRedeemedBalanceCodes(clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(redeemed), 1)
		assert.Equal(t, redeemed[0].BalanceCodeId, balanceCode.BalanceCodeId)
		assert.Equal(t, redeemed[0].Secret, balanceCode.Secret)

	})
}
