package controller

import (
	"fmt"
	// "errors"

	// "bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/model"
)


func AuthLogin(login model.AuthLoginArgs, session *session.ClientSession) (*model.AuthLoginResult, error) {
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
	session *session.ClientSession,
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
	UserAuth string `json:"userAuth"`
}

func AuthValidateSend(
	validateSend AuthValidateSendArgs,
	session *session.ClientSession,
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
				"Verify your email",
				createValidateBodyHtml(*validateCreateCodeResult.ValidateCode),
				createValidateBodyText(*validateCreateCodeResult.ValidateCode),
			)
		case model.UserAuthTypePhone:
			sendAccountSms(
				*userAuth,
				createValidateBodyText(*validateCreateCodeResult.ValidateCode),
			)		
		}
	}

	result := &AuthValidateSendResult{
		UserAuth: *userAuth,
	}
	return result, nil
}

func createValidateBodyHtml(validateCode string) string {
	// fixme
	return fmt.Sprintf("%s", validateCode)
}

func createValidateBodyText(validateCode string) string {
	// fixme
	return fmt.Sprintf("%s", validateCode)
}


type AuthPasswordResetArgs struct {
	UserAuth string `json:"userAuth"`
}

type AuthPasswordResetResult struct {
	UserAuth string `json:"userAuth"`
}

func AuthPasswordReset(
	reset AuthPasswordResetArgs,
	session *session.ClientSession,
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
				"Reset your password",
				createResetBodyHtml(*resetCreateCodeResult.ResetCode),
				createResetBodyText(*resetCreateCodeResult.ResetCode),
			)
		case model.UserAuthTypePhone:
			sendAccountSms(
				*userAuth,
				createResetBodyText(*resetCreateCodeResult.ResetCode),
			)		
		}
	}

	result := &AuthPasswordResetResult{
		UserAuth: *userAuth,
	}
	return result, nil
}

func createResetBodyHtml(resetCode string) string {
	// fixme
	return fmt.Sprintf("<a href=\"https://bringyour.com?resetCode=%s\">Reset password</a>", resetCode)
}

func createResetBodyText(resetCode string) string {
	// fixme
	return fmt.Sprintf("https://bringyour.com?resetCode=%s", resetCode)
}


type AuthPasswordSetResult struct {
}

func AuthPasswordSet(passwordSet model.AuthPasswordSetArgs, session *session.ClientSession) (*AuthPasswordSetResult, error) {
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
