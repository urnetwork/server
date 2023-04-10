package controller

import (
	"errors"

	"bringyour.com/bringyour"
	// "bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/model"
)


func AuthLogin(login model.AuthLoginArgs, session *bringyour.ClientSession) (*model.AuthLoginResult, error) {
	// fixme
	/*
	userAuth, userAuthType := normalUserAuthV1(login.userAuth)

	if userAuth == nil {
		// fixme try to infer the login type based on the input
		// if phone, and there there is no +xxx yyyy, infer the country code based on ipinfo
	}
	*/

	result, err := model.AuthLogin(login, session)
	return result, err
}


func AuthLoginWithPassword(
	loginWithPassword model.AuthLoginWithPasswordArgs,
	session *bringyour.ClientSession,
) (*model.AuthLoginWithPasswordResult, error) {
	result, err := model.AuthLoginWithPassword(loginWithPassword, session)
	// if validation required, send it
	if result != nil && result.ValidationRequired != nil {
		validateSend := AuthValidateSendArgs{
			UserAuth: result.ValidationRequired.UserAuth,
		}
		AuthValidateSend(validateSend, session)
	}
	return result, err
}


type AuthValidateSendArgs struct {
	UserAuth string `json:"userAuth"`
}

type AuthValidateSendResult struct {
}

func AuthValidateSend(
	validateSend AuthValidateSendArgs,
	session *bringyour.ClientSession,
) (*AuthValidateSendResult, error) {
	userAuth, userAuthType := model.NormalUserAuthV1(&validateSend.UserAuth)

	validateCreateCode := model.AuthValidateCreateCodeArgs{
		UserAuth: *userAuth,
	}
	validateCreateCodeResult, err := model.AuthValidateCreateCode(validateCreateCode, session)
	if err != nil {
		return nil, err
	}
	if validateCreateCodeResult.ValidateCode != nil {	
		switch userAuthType {
		case model.UserAuthTypeEmail:
			sendAccountEmail(
				*userAuth,
				createValidateBodyHtml(*validateCreateCodeResult.ValidateCode),
			)
		case model.UserAuthTypePhone:
			sendAccountSms(
				*userAuth,
				createValidateBodyText(*validateCreateCodeResult.ValidateCode),
			)		
		}
	}

	return nil, errors.New("Invalid login.")
}

func createValidateBodyHtml(validateCode string) string {
	// fixme
	return ""
}

func createValidateBodyText(validateCode string) string {
	// fixme
	return ""
}


type AuthPasswordResetArgs struct {
	UserAuth string `json:"userAuth"`
}

type AuthPasswordResetResult struct {
}

func AuthPasswordReset(
	reset AuthPasswordResetArgs,
	session *bringyour.ClientSession,
) (*AuthPasswordResetResult, error) {
	userAuth, userAuthType := model.NormalUserAuthV1(&reset.UserAuth)

	resetCreateCode := model.AuthPasswordResetCreateCodeArgs{
		UserAuth: *userAuth,
	}
	resetCreateCodeResult, err := model.AuthPasswordResetCreateCode(resetCreateCode, session)
	if err != nil {
		return nil, err
	}
	if resetCreateCodeResult.ResetCode != nil {	
		switch userAuthType {
		case model.UserAuthTypeEmail:
			sendAccountEmail(
				*userAuth,
				createResetBodyHtml(*resetCreateCodeResult.ResetCode),
			)
		case model.UserAuthTypePhone:
			sendAccountSms(
				*userAuth,
				createResetBodyText(*resetCreateCodeResult.ResetCode),
			)		
		}
	}

	return nil, errors.New("Invalid login.")
}

func createResetBodyHtml(resetCode string) string {
	// fixme
	return ""
}

func createResetBodyText(resetCode string) string {
	// fixme
	return ""
}


type AuthPasswordSetResult struct {
}

func AuthPasswordSet(passwordSet model.AuthPasswordSetArgs, session *bringyour.ClientSession) (*AuthPasswordSetResult, error) {
	passwordSetResult, err := model.AuthPasswordSet(passwordSet, session)
	if err != nil {
		return nil, err
	}
	SendAccountMessage(
		passwordSetResult.NetworkId,
		createPasswordSetNoticeBodyHtml(),
		createPasswordSetNoticeBodyText(),
	)
	safePasswordSetResult := &AuthPasswordSetResult{}
	return safePasswordSetResult, nil
}

func createPasswordSetNoticeBodyHtml() string {
	// fixme
	return ""
}

func createPasswordSetNoticeBodyText() string {
	// fixme
	return ""
}
