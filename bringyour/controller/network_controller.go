package controller


func NetworkCreate(
	create model.NetworkCreate,
	session *bringyour.ClientSession
) (*NetworkCreateResult, error) {
	result, err := model.NetworkCreate(create, session)
	// if validation required, send it
	if result != nil && result.ValidationRequired != nil {
		validateSend := AuthValidateSendArgs{
			UserAuth: result.ValidationRequired.UserAuth
		}
		AuthValidateSend(validateSend, session)
	}
	return result, err
}

