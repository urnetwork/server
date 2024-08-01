package controller

import (
	// "time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
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

	if success := AddInitialTransferBalance(session.Ctx, result.Network.NetworkId); !success {
		bringyour.Logger().Printf("Could not add initial transfer balance to networkId=%s\n", result.Network.NetworkId)
	}

	// if verification required, send it
	if result.VerificationRequired != nil {
		verifySend := AuthVerifySendArgs{
			UserAuth: result.VerificationRequired.UserAuth,
		}
		AuthVerifySend(verifySend, session)
	} else {
		awsMessageSender := GetAWSMessageSender()
		awsMessageSender.SendAccountMessageTemplate(
			*result.UserAuth,
			&NetworkWelcomeTemplate{},
		)
	}

	return result, nil
}
