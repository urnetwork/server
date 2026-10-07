package controller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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

// Covers the app path (/account/set-referral). The controller refuses the
// network's own code with strings.EqualFold, but the code lookup upper-cases
// it, and a dotless "ı" (U+0131) upper-cases to "I" without case-folding to it.
// That spelling passes the controller check and resolves to the caller's own
// network, so only the model guard can refuse it.
func TestSetNetworkReferralRefusesOwnCodeWithDotlessI(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "self", networkId)
		model.CreateNetworkReferralCode(ctx, networkId)

		// give the network a code with an "I"
		idString := server.NewId().String()
		code := "I" + strings.ToUpper(idString[len(idString)-5:])
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE network_referral_code
					SET referral_code = $2
					WHERE network_id = $1
				`,
				networkId,
				code,
			))
		})

		networkSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
		})

		args := controller.SetNetworkReferralArgs{
			ReferralCode: strings.ReplaceAll(strings.ToLower(code), "i", "\u0131"),
		}
		result, err := controller.SetNetworkReferral(&args, networkSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

		connect.AssertEqual(t, model.GetReferralNetworkByChildNetworkId(ctx, networkId), nil)
	})
}
