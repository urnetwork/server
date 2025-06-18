package controller_test

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
)

func TestNetworkReferral(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkAId := server.NewId()
		networkBId := server.NewId()
		networkCId := server.NewId()

		model.Testing_CreateNetwork(ctx, networkAId, "a", networkAId)
		model.Testing_CreateNetwork(ctx, networkBId, "b", networkBId)
		model.Testing_CreateNetwork(ctx, networkCId, "c", networkCId)

		referralCodeA := model.CreateNetworkReferralCode(ctx, networkAId)
		referralCodeB := model.CreateNetworkReferralCode(ctx, networkBId)

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
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)

		networkCReferral := model.GetReferralNetworkByChildNetworkId(ctx, networkCId)
		assert.NotEqual(t, networkCReferral, nil)
		assert.Equal(t, networkCReferral.Id, networkAId)
		assert.Equal(t, networkCReferral.Name, "a")

		/**
		 * Set the referral code for network C to network B
		 */
		args = controller.SetNetworkReferralArgs{
			ReferralCode: referralCodeB.ReferralCode,
		}

		_, err = controller.SetNetworkReferral(&args, networkCSession)
		assert.Equal(t, err, nil)

		networkCReferral = model.GetReferralNetworkByChildNetworkId(ctx, networkCId)
		assert.Equal(t, networkCReferral.Id, networkBId)
		assert.Equal(t, networkCReferral.Name, "b")

		/**
		 * Remove the referral code for network C
		 */
		controller.UnlinkReferralNetwork(networkCSession)
		networkCReferral = model.GetReferralNetworkByChildNetworkId(ctx, networkCId)
		assert.Equal(t, networkCReferral, nil)

	})
}
