package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

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

		_, err = model.RedeemBalanceCode(args, ctx)
		assert.Equal(t, err, nil)

		redeemed, err = GetNetworkRedeemedBalanceCodes(
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(redeemed.BalanceCodes), 1)
		assert.Equal(t, redeemed.BalanceCodes[0].BalanceCodeId, balanceCode.BalanceCodeId)

	})
}
