package controller

import (

	"bringyour.com/ulid"
	"bringyour.com/model"
)


func AuthLogin(login AuthLoginArgs, session *bringyour.ClientSession) (*model.AuthLoginResult, error) {
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
	loginWithPassword AuthLoginWithPasswordArgs,
	session *bringyour.ClientSession
) (*model.AuthLoginWithPasswordResult, error) {
	result, err := model.AuthLoginWithPassword(loginWithPassword)
	// if validation required, send it
	if result != nil && result.ValidationRequired != nil {
		validateSend := AuthValidateSendArgs{
			UserAuth: result.ValidationRequired.UserAuth
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
	validateSend model.AuthValidateSendArgs,
	session *bringyour.ClientSession
) (*AuthValidateSendResult, error) {
	userAuth, userAuthType := model.NormalUserAuthV1(validateSend.UserAuth)

	validateCreateCode := AuthValidateCreateCodeArgs{
		UserAuth: userAuth
	}
	validateCreateCodeResult, err := model.AuthValidateCreateCode(validateCreateCode, session)
	if validateCreateCodeResult != nil && validateCreateCodeResult.ValidateCode != nil {	
		switch userAuthType {
		case model.UserAuthTypeEmail:
			sendAccountEmail(
				userAuth,
				createValidateBodyHtml(validateCreateCodeResult.ValidateCode),
			)
		case model.UserAuthTypePhone:
			sendAccountSms(
				userAuth,
				createValidateBodyText(validateCreateCodeResult.ValidateCode),
			)		
		}
	}

	return nil, error("Invalid login.")
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
	userAuth string
}

type AuthPasswordResetResult struct {
}

func AuthPasswordReset(
	reset model.AuthPasswordResetArgs,
	session *bringyour.ClientSession
) (*AuthPasswordResetResult, error) {
	userAuth, userAuthType := model.NormalUserAuthV1(reset.UserAuth)

	resetCreateCode := AuthResetCreateCodeArgs{
		UserAuth: userAuth
	}
	resetCreateCodeResult, err := model.AuthResetCreateCode(resetCreateCode, session)
	if resetCreateCodeResult != nil && resetCreateCodeResult.ResetCode != nil {	
		switch userAuthType {
		case model.UserAuthTypeEmail:
			sendAccountEmail(
				userAuth,
				createResetBodyHtml(validateCreateCodeResult.ResetCode),
			)
		case model.UserAuthTypePhone:
			sendAccountSms(
				userAuth,
				createResetBodyText(validateCreateCodeResult.ResetCode),
			)		
		}
	}

	return nil, error("Invalid login.")
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

func AuthPasswordSet(passwordSet model.AuthPasswordSetArgs) (*AuthPasswordSetResult, error) {
	passwordSetResult, err := model.AuthPasswordSet(passwordSet)
	if passwordSetResult != nil {
		SendAccountMessage(
			passwordSetResult.NetworkId,
			createPasswordSetNoticeBodyHtml(),
			createPasswordSetNoticeBodyText(),
		)
		safePasswordSetResult := &AuthPasswordSetResult{}
		return safePasswordSetResult, nil
	}

	return nil, error("Invalid login.")
}

func createPasswordSetNoticeBodyHtml() string {
	// fixme
	return ""
}

func createPasswordSetNoticeBodyText() string {
	// fixme
	return ""
}
