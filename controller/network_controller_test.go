package controller

import (
	"context"
	"testing"
	"time"

	// "maps"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNetworkCreate(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, nil)

		referralNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, referralNetworkId, "referralNetwork", server.NewId())
		referralCode := model.CreateNetworkReferralCode(ctx, referralNetworkId)
		connect.AssertNotEqual(t, referralCode, nil)

		userAuth := "foo@ur.io"
		password := "bar123456789Foo!"

		// check referral network has no points
		networkPoints := model.FetchAccountPoints(ctx, referralNetworkId)
		connect.AssertEqual(t, len(networkPoints), 0)

		networkCreate := model.NetworkCreateArgs{
			UserName:     "",
			UserAuth:     &userAuth,
			Password:     &password,
			NetworkName:  "foobar",
			Terms:        true,
			ReferralCode: &referralCode.ReferralCode,
		}
		result, err := NetworkCreate(networkCreate, session)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.Network, nil)

		// session.ByJwt.NetworkId = result.Network.NetworkId
		session.ByJwt = &jwt.ByJwt{
			NetworkId: result.Network.NetworkId,
		}

		// ensure referral code has been created for this network
		networkReferralCode := model.GetNetworkReferralCode(session.Ctx, result.Network.NetworkId)
		connect.AssertNotEqual(t, networkReferralCode, nil)

		// check referral network has points applied
		// networkPoints = model.FetchNetworkPoints(ctx, referralNetworkId)
		// connect.AssertEqual(t, len(networkPoints), 1)
		// connect.AssertEqual(t, networkPoints[0].NetworkId, referralNetworkId)
		// connect.AssertEqual(t, networkPoints[0].Event, "referral")
		// connect.AssertNotEqual(t, networkPoints[0].PointValue, 0)
		//
		// network name should not contain profanity
		network := model.GetNetwork(session)
		connect.AssertNotEqual(t, network, nil)
		connect.AssertEqual(t, network.ContainsProfanity, false)

		// check network referral
		networkReferral := model.GetReferralNetworkByChildNetworkId(ctx, result.Network.NetworkId)
		connect.AssertEqual(t, networkReferral.Id, referralNetworkId)

		transferBalances := model.GetActiveTransferBalances(ctx, result.Network.NetworkId)
		connect.AssertEqual(t, 1, len(transferBalances))
		transferBalance := transferBalances[0]
		connect.AssertEqual(t, transferBalance.BalanceByteCount, model.Pro().DataAmount(false))
		connect.AssertEqual(t, !transferBalance.StartTime.After(time.Now()), true)
		connect.AssertEqual(t, time.Now().Before(transferBalance.EndTime), true)
	})
}

func TestNetworkCreateWithProfanity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, nil)

		referralNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, referralNetworkId, "referralNetwork", server.NewId())

		userAuth := "foo@ur.io"
		password := "bar123456789Foo!"
		referralCode := ""

		networkCreate := model.NetworkCreateArgs{
			UserName:     "",
			UserAuth:     &userAuth,
			Password:     &password,
			NetworkName:  "shitty", // must be at least 6 characters
			Terms:        true,
			ReferralCode: &referralCode,
		}
		result, err := NetworkCreate(networkCreate, session)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.Network, nil)

		session.ByJwt = &jwt.ByJwt{
			NetworkId: result.Network.NetworkId,
		}

		// check network contains profanity
		network := model.GetNetwork(session)
		connect.AssertNotEqual(t, network, nil)
		connect.AssertEqual(t, network.ContainsProfanity, true)
	})
}

func TestNetworkNameUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		networkIdB := server.NewId()
		userIdB := server.NewId()
		networkNameB := "bcdefg"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		model.Testing_CreateNetwork(ctx, networkIdB, networkNameB, userIdB)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// should fail because network not greater than 5 characters
		updateArgs := &UpdateNetworkNameArgs{
			NetworkName: "",
		}
		updateNetworkUserResult, err := UpdateNetworkName(updateArgs, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, updateNetworkUserResult.Error, nil)

		networkResult := model.GetNetwork(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, networkResult.NetworkName, networkName)

		// should fail because network name unavailable
		updateArgs = &UpdateNetworkNameArgs{
			NetworkName: networkNameB,
		}
		updateNetworkUserResult, err = UpdateNetworkName(updateArgs, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, updateNetworkUserResult.Error, nil)

		networkResult = model.GetNetwork(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, networkResult.NetworkName, networkName)

		// should update the network name
		updatedNetworkName := "uvwxyz"
		updateArgs = &UpdateNetworkNameArgs{
			NetworkName: updatedNetworkName,
		}
		updateNetworkUserResult, err = UpdateNetworkName(updateArgs, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, updateNetworkUserResult.Error, nil)

		networkResult = model.GetNetwork(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, networkResult.NetworkName, updatedNetworkName)
	})
}

func TestNetworkCreateWithBalanceCodeSuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		session := session.Testing_CreateClientSession(ctx, nil)

		userAuth := "foo@ur.io"
		password := "bar123456789Foo!"

		netTransferByteCount := model.ByteCount(1024 * 1024)
		netRevenue := model.UsdToNanoCents(10.00)
		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := model.CreateBalanceCode(
			ctx,
			netTransferByteCount,
			subscriptionYearDuration,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)

		networkCreate := model.NetworkCreateArgs{
			UserName:    "",
			UserAuth:    &userAuth,
			Password:    &password,
			NetworkName: "foobar",
			Terms:       true,
			BalanceCode: &balanceCode.Secret,
		}

		result, err := NetworkCreate(networkCreate, session)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		// The balance code's data lands on the new network...
		transferBalances := model.GetActiveTransferBalances(ctx, result.Network.NetworkId)
		redeemed := model.ByteCount(0)
		for _, transferBalance := range transferBalances {
			redeemed += transferBalance.BalanceByteCount
			// ...and NO balance it created carries the Pro entitlement.
			connect.AssertEqual(t, transferBalance.Pro, false)
		}
		// the code's data is there (alongside the free signup grant)
		connect.AssertEqual(t, netTransferByteCount <= redeemed, true)

		// ...but redeeming a code does NOT make the network Pro. A data code is
		// DATA ONLY. It is a `paid` balance (it carries revenue), which is exactly
		// why Pro cannot be inferred from `paid` -- see model/pro_model.go. This
		// assertion used to be `true`, back when IsPro meant "has any paid balance"
		// and buying data silently upgraded you for free.
		isPro := model.IsPro(ctx, &result.Network.NetworkId)
		connect.AssertEqual(t, isPro, false)

	})
}
