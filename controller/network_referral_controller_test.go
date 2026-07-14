package controller_test

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNetworkReferral(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkAId := server.NewId()
		networkBId := server.NewId()
		networkCId := server.NewId()

		model.Testing_CreateNetwork(ctx, networkAId, "a", networkAId)
		model.Testing_CreateNetwork(ctx, networkBId, "b", networkBId)
		model.Testing_CreateNetwork(ctx, networkCId, "c", networkCId)

		referralCodeA := model.CreateNetworkReferralCode(ctx, networkAId)
		referralCodeB := model.CreateNetworkReferralCode(ctx, networkBId)
		referralCodeC := model.CreateNetworkReferralCode(ctx, networkCId)

		networkCSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkCId,
		})

		args := controller.SetNetworkReferralArgs{
			ReferralCode: referralCodeA.ReferralCode,
		}

		/**
		 * Set the referral code for network C to network A
		 */
		result, err := controller.SetNetworkReferral(&args, networkCSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		networkCReferral := model.GetReferralNetworkByChildNetworkId(ctx, networkCId)
		connect.AssertNotEqual(t, networkCReferral, nil)
		connect.AssertEqual(t, networkCReferral.Id, networkAId)
		connect.AssertEqual(t, networkCReferral.Name, "a")

		/**
		 * Set the referral code for network C to network B
		 */
		args = controller.SetNetworkReferralArgs{
			ReferralCode: referralCodeB.ReferralCode,
		}

		_, err = controller.SetNetworkReferral(&args, networkCSession)
		connect.AssertEqual(t, err, nil)

		networkCReferral = model.GetReferralNetworkByChildNetworkId(ctx, networkCId)
		connect.AssertEqual(t, networkCReferral.Id, networkBId)
		connect.AssertEqual(t, networkCReferral.Name, "b")

		/**
		 * Remove the referral code for network C
		 */
		controller.UnlinkReferralNetwork(networkCSession)
		networkCReferral = model.GetReferralNetworkByChildNetworkId(ctx, networkCId)
		connect.AssertEqual(t, networkCReferral, nil)

		/**
		 * User should not be able to set their referral code to their own network
		 */
		args = controller.SetNetworkReferralArgs{
			ReferralCode: referralCodeC.ReferralCode,
		}
		result, err = controller.SetNetworkReferral(&args, networkCSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

	})
}
