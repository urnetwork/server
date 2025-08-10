package controller

import (
	// "time"

	// "github.com/urnetwork/server"
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

	model.CreateNetworkReferralCode(session.Ctx, result.Network.NetworkId)

	AddRefreshTransferBalance(session.Ctx, result.Network.NetworkId)

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

type NetworkRemoveResult struct{}

func NetworkRemove(session *session.ClientSession) (*NetworkRemoveResult, error) {

	model.RemoveNetwork(
		session.Ctx,
		session.ByJwt.NetworkId,
		session.ByJwt.UserId,
	)

	return &NetworkRemoveResult{}, nil
}
