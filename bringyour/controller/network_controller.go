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
	// if validation required, send it
	if result != nil && result.ValidationRequired != nil {
		validateSend := AuthValidateSendArgs{
			UserAuth: result.ValidationRequired.UserAuth,
		}
		AuthValidateSend(validateSend, session)
	}
	return result, err
}

