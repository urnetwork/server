package controller

import (
	"time"

	// "bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/model"
)


const InitialTransferBalance = model.ByteCount(30 * 1024 * 1024 * 1024)

// 30 days
const InitialTransferBalanceExpiration = 30 * 24 * time.Hour


type NetworkCreateResult struct {
	Network *model.NetworkCreateResultNetwork `json:"network,omitempty"`
	UserAuth *string `json:"user_auth,omitempty"`
	VerificationRequired *model.NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error *model.NetworkCreateResultError `json:"error,omitempty"`
}


func NetworkCreate(
	networkCreate model.NetworkCreateArgs,
	session *session.ClientSession,
) (*NetworkCreateResult, error) {
	result, err := model.NetworkCreate(networkCreate, session)
	if err != nil {
		return nil, err
	}

	AddInitialTransferBalance(session.Ctx, result.NetworkId)

	// if verification required, send it
	if result.VerificationRequired != nil {
		verifySend := AuthVerifySendArgs{
			UserAuth: result.VerificationRequired.UserAuth,
		}
		AuthVerifySend(verifySend, session)
	} else if result.Network != nil && result.UserAuth != nil {
        SendAccountMessageTemplate(
            *result.UserAuth,
            &NetworkWelcomeTemplate{},
        )
	}

	return &NetworkCreateResult{
		Network: result.Network,
		UserAuth: result.UserAuth,
		VerificationRequired: result.VerificationRequired,
		Error: result.Error,
	}, nil
}

