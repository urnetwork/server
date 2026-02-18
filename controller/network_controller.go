package controller

import (
	// "time"
	"fmt"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func NetworkCreate(
	networkCreate model.NetworkCreateArgs,
	session *session.ClientSession,
) (*model.NetworkCreateResult, error) {
	result, err := model.NetworkCreate(networkCreate, session)
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return result, nil
	}

	/**
	 * we only add transfer balance if the user is not pro (no balance code redeemed)
	 *
	 * redeeming a balance code successfully automatically adds paid transfer balance for the network
	 */
	if !result.IsPro {
		// add regular balance
		AddRefreshTransferBalance(session.Ctx, result.Network.NetworkId)
	}

	if networkCreate.ReferralCode != nil {
		model.CreateNetworkReferral(
			session.Ctx,
			result.Network.NetworkId,
			*networkCreate.ReferralCode,
		)

		// note: should we check if the network subscribes before applying points?
		// if networkReferral != nil {
		// 	model.ApplyNetworkPoints(
		// 		session.Ctx,
		// 		*networkReferral.ReferralNetworkId,
		// 		"referral",
		// 	)
		// }

	}

	verifyUseNumeric := false

	if networkCreate.VerifyUseNumeric {
		verifyUseNumeric = true
	}

	// if verification required, send it
	if result.VerificationRequired != nil {
		verifySend := AuthVerifySendArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			UseNumeric: verifyUseNumeric,
		}
		AuthVerifySend(verifySend, session)
	} else {

		if result.UserAuth != nil {
			awsMessageSender := GetAWSMessageSender()
			awsMessageSender.SendAccountMessageTemplate(
				*result.UserAuth,
				&NetworkWelcomeTemplate{},
			)
		}

		byJwt, err := jwt.ParseByJwt(session.Ctx, *(result.Network.ByJwt))
		if err == nil {
			AccountPreferencesSet(
				&model.AccountPreferencesSetArgs{
					ProductUpdates: true,
				},
				session.WithByJwt(byJwt),
			)
		}

	}

	return result, nil
}

type UpdateNetworkNameArgs struct {
	NetworkName string `json:"network_name"`
}

type UpdateNetworkNameError struct {
	Message string `json:"message"`
}

type UpdateNetworkNameResult struct {
	Error *UpdateNetworkNameError `json:"error,omitempty"`
}

func UpdateNetworkName(
	args *UpdateNetworkNameArgs,
	clientSession *session.ClientSession,
) (*UpdateNetworkNameResult, error) {

	// get the current network name
	network := model.GetNetwork(clientSession)

	if network.NetworkName != args.NetworkName {
		// update the network name
		result, err := model.NetworkUpdate(
			model.NetworkUpdateArgs{NetworkName: args.NetworkName},
			clientSession,
		)
		if err != nil {
			return nil, err
		}

		if result.Error != nil {
			return &UpdateNetworkNameResult{
				Error: &UpdateNetworkNameError{
					Message: result.Error.Message,
				},
			}, nil
		}
	}

	return &UpdateNetworkNameResult{}, nil
}

/**
 * Upgrades a guest to a new account
 */
func UpgradeFromGuest(
	upgradeGuest model.UpgradeGuestArgs,
	session *session.ClientSession,
) (*model.UpgradeGuestResult, error) {

	result, err := model.UpgradeGuest(
		upgradeGuest,
		session,
	)

	// if verification required, send it
	if result.VerificationRequired != nil {
		verifySend := AuthVerifySendArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			UseNumeric: true,
		}
		AuthVerifySend(verifySend, session)
	} else {

		if result.UserAuth != nil {
			awsMessageSender := GetAWSMessageSender()
			awsMessageSender.SendAccountMessageTemplate(
				*result.UserAuth,
				&NetworkWelcomeTemplate{},
			)
		}

	}

	return result, err

}

/**
 * Upgrades a guest to an existing account
 */
func UpgradeFromGuestExisting(
	upgradeGuest model.UpgradeGuestExistingArgs,
	session *session.ClientSession,
) (*model.UpgradeGuestExistingResult, error) {

	result, err := model.UpgradeFromGuestExisting(
		upgradeGuest,
		session,
	)

	// if verification required, send it
	if result.VerificationRequired != nil {
		verifySend := AuthVerifySendArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			UseNumeric: true,
		}
		AuthVerifySend(verifySend, session)
	}

	return result, err

}

type NetworkRemoveResultError struct {
	Message string `json:"message"`
}

type NetworkRemoveResult struct {
	Error *NetworkRemoveResultError `json:"error,omitempty"`
}

func NetworkRemove(session *session.ClientSession) (*NetworkRemoveResult, error) {
	success, userAuths := model.RemoveNetwork(
		session.Ctx,
		session.ByJwt.NetworkId,
		&session.ByJwt.UserId,
	)
	if success {
		server.Tx(session.Ctx, func(tx server.PgTx) {
			for userAuth, _ := range userAuths {
				ScheduleRemoveProductUpdates(session, userAuth, tx)
			}
		})

		// ensure we wrap up any Stripe subscriptions for the network
		err := UnsubscribeStripe(session)
		if err != nil {
			glog.Errorf("Failed to unsubscribe Stripe: %v", err)
			return &NetworkRemoveResult{
				Error: &NetworkRemoveResultError{
					Message: "Failed to unsubscribe Stripe",
				},
			}, nil
		}

		return &NetworkRemoveResult{}, nil
	}

	return nil, fmt.Errorf("Could not remove network")
}

type GetNetworkReliabilityResult struct {
	ReliabilityWindow *model.ReliabilityWindow    `json:"reliability_window,omitempty"`
	Error             *GetNetworkReliabilityError `json:"error,omitempty"`
}

type GetNetworkReliabilityError struct {
	Message string `json:"message"`
}

func GetNetworkReliability(
	session *session.ClientSession,
) (*GetNetworkReliabilityResult, error) {

	window, err := model.GetNetworkReliabilityWindow(session)
	if err != nil {
		return nil, err
	}

	return &GetNetworkReliabilityResult{
		ReliabilityWindow: window,
	}, nil
}
