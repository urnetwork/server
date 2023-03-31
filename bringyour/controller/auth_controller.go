package controller

import (
)


func AuthLoginWithPassword(loginWithPassword *AuthLoginWithPasswordArgs) (*AuthLoginWithPasswordResult, error) {
	result, err := model.AuthLoginWithPassword(loginWithPassword)
	// fixme is validation needed, send validaton link
	return result, err
}


func AuthValidateSend(validateSend *model.AuthValidateSendArgs) (model.AuthValidateSendResult, error) {
	// lookup userAuth type
	// send either email or sms
	// fixme
	return nil, nil
}


func AuthPasswordReset(validateSend *model.AuthPasswordResetArgs) (model.AuthPasswordResetResult, error) {
	// lookup userAuth type
	// send either email or sms
	// fixme
	return nil, nil
}


func AuthPasswordSet(passwordSet *AuthPasswordSetArgs) (*AuthPasswordSetResult, error) {
	result, err := model.AuthPasswordSet(passwordSet)
	// fixme send email/sms notice that password was changed
	return result, err
}

