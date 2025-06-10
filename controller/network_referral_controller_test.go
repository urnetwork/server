package controller_test

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNetworkReferral(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networdAId := server.NewId()
		networkBId := server.NewId()
		networkCid := server.NewId()

		model.Testing_CreateNetwork(ctx, networkCid, "c", networkCid)

		referralCodeA := model.CreateNetworkReferralCode(ctx, networdAId)
		referralCodeB := model.CreateNetworkReferralCode(ctx, networkBId)

		networkCSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkCid,
		})

		args := controller.SetNetworkReferralArgs{
			ReferralCode: referralCodeA.ReferralCode,
		}

		/**
		 * Set the referral code for network C to network A
		 */
		_, err := controller.SetNetworkReferral(&args, networkCSession)
		assert.Equal(t, err, nil)

		networkCReferral := model.GetNetworkReferralByNetworkId(ctx, networkCid)
		assert.Equal(t, networkCReferral.ReferralNetworkId, networdAId)
		assert.Equal(t, networkCReferral.NetworkId, networkCid)

		/**
		 * Set the referral code for network C to network B
		 */
		args = controller.SetNetworkReferralArgs{
			ReferralCode: referralCodeB.ReferralCode,
		}

		_, err = controller.SetNetworkReferral(&args, networkCSession)
		assert.Equal(t, err, nil)

		networkCReferral = model.GetNetworkReferralByNetworkId(ctx, networkCid)
		assert.Equal(t, networkCReferral.ReferralNetworkId, networkBId)
		assert.Equal(t, networkCReferral.NetworkId, networkCid)

	})
}
