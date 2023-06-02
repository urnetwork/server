package controller

import (

	// "bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/model"
)


func NetworkCreate(
	networkCreate model.NetworkCreateArgs,
	session *session.ClientSession,
) (*model.NetworkCreateResult, error) {
	result, err := model.NetworkCreate(networkCreate, session)
	// if verification required, send it
	if result != nil && result.VerificationRequired != nil {
		verifySend := AuthVerifySendArgs{
			UserAuth: result.VerificationRequired.UserAuth,
		}
		AuthVerifySend(verifySend, session)
	}
	return result, err
}

