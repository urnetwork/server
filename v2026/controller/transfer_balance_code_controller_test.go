package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

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

		redeemed, err := GetNetworkRedeemedBalanceCodes(
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(redeemed.BalanceCodes), 0)

		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := model.CreateBalanceCode(
			ctx,
			1024,
			subscriptionYearDuration,
			100,
			"",
			"",
			"",
		)
		assert.Equal(t, err, nil)

		args := &model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: clientSession.ByJwt.NetworkId,
		}

		redeemResult, err := model.RedeemBalanceCode(args, ctx)
		assert.Equal(t, err, nil)
		// A failed redeem is reported via result.Error with a nil Go error (the
		// user-facing channel, mirrored by networkCreateRedeemBalanceCodeInTx).
		// Without this check a silent "Unknown balance code." passes here and
		// resurfaces as a confusing 0-vs-1 mismatch on the fetch below.
		assert.Equal(t, redeemResult.Error, nil)

		redeemed, err = GetNetworkRedeemedBalanceCodes(
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(redeemed.BalanceCodes), 1)
		assert.Equal(t, redeemed.BalanceCodes[0].BalanceCodeId, balanceCode.BalanceCodeId)

	})
}
