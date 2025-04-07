package controller

import (
	// "context"
	// "fmt"
	// "errors"
	// "time"
	"sync"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
)

var SsoRedirectUrl = sync.OnceValue(func() string {
	c := server.Config.RequireSimpleResource("sso.yml").Parse()
	return c["web_connect"].(map[string]any)["redirect_url"].(string)
})

func AuthLogin(
	login model.AuthLoginArgs,
	session *session.ClientSession,
) (*model.AuthLoginResult, error) {
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
	// if verification required, send it
	if result != nil && result.VerificationRequired != nil {

		useNumeric := false

		if loginWithPassword.VerifyOtpNumeric {
			useNumeric = true
		}

		verifySend := AuthVerifySendArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			UseNumeric: useNumeric,
		}
		AuthVerifySend(verifySend, session)
	}
	return result, err
}

type AuthVerifySendArgs struct {
	UserAuth   string `json:"user_auth"`
	UseNumeric bool   `json:"use_numeric,omitempty"`
}

type AuthVerifySendResult struct {
	UserAuth string `json:"user_auth"`
}

func AuthVerifySend(
	verifySend AuthVerifySendArgs,
	session *session.ClientSession,
) (*AuthVerifySendResult, error) {
	userAuth, _ := model.NormalUserAuthV1(&verifySend.UserAuth)

	verifyCodeType := model.VerifyCodeDefault
	if verifySend.UseNumeric {
		verifyCodeType = model.VerifyCodeNumeric
	}

	verifyCreateCode := model.AuthVerifyCreateCodeArgs{
		UserAuth: *userAuth,
		CodeType: verifyCodeType,
	}
	verifyCreateCodeResult, err := model.AuthVerifyCreateCode(verifyCreateCode, session)
	if err != nil {
		return nil, err
	}

	if verifyCreateCodeResult.VerifyCode != nil {
		awsMessageSender := GetAWSMessageSender()
		awsMessageSender.SendAccountMessageTemplate(
			*userAuth,
			&AuthVerifyTemplate{
				VerifyCode: *verifyCreateCodeResult.VerifyCode,
			},
		)
	}

	result := &AuthVerifySendResult{
		UserAuth: *userAuth,
	}
	return result, nil
}

func Testing_SendAuthVerifyCode(userAuth string) {
	normalUserAuth, _ := model.NormalUserAuthV1(&userAuth)

	verifyCode := model.Testing_CreateVerifyCode()

	awsMessageSender := GetAWSMessageSender()
	awsMessageSender.SendAccountMessageTemplate(
		*normalUserAuth,
		&AuthVerifyTemplate{
			VerifyCode: verifyCode,
		},
	)
}

type AuthPasswordResetArgs struct {
	UserAuth string `json:"user_auth"`
}

type AuthPasswordResetResult struct {
	UserAuth string `json:"user_auth"`
}

func AuthPasswordReset(
	reset AuthPasswordResetArgs,
	session *session.ClientSession,
) (*AuthPasswordResetResult, error) {
	userAuth, _ := model.NormalUserAuthV1(&reset.UserAuth)

	resetCreateCode := model.AuthPasswordResetCreateCodeArgs{
		UserAuth: *userAuth,
	}
	resetCreateCodeResult, err := model.AuthPasswordResetCreateCode(resetCreateCode, session)
	if err != nil {
		return nil, err
	}
	if resetCreateCodeResult.ResetCode != nil {
		awsMessageSender := GetAWSMessageSender()
		awsMessageSender.SendAccountMessageTemplate(
			*userAuth,
			&AuthPasswordResetTemplate{
				ResetCode: *resetCreateCodeResult.ResetCode,
			},
		)
	}

	result := &AuthPasswordResetResult{
		UserAuth: *userAuth,
	}
	return result, nil
}

type AuthPasswordSetResult struct {
}

func AuthPasswordSet(passwordSet model.AuthPasswordSetArgs, session *session.ClientSession) (*AuthPasswordSetResult, error) {
	passwordSetResult, err := model.AuthPasswordSet(passwordSet, session)
	if err != nil {
		return nil, err
	}
	userAuth, err := model.GetUserAuth(session.Ctx, passwordSetResult.NetworkId)
	if err != nil {
		return nil, err
	}
	normalUserAuth, _ := model.NormalUserAuthV1(&userAuth)
	awsMessageSender := GetAWSMessageSender()
	awsMessageSender.SendAccountMessageTemplate(
		*normalUserAuth,
		&AuthPasswordSetTemplate{},
	)

	safePasswordSetResult := &AuthPasswordSetResult{}
	return safePasswordSetResult, nil
}

func AuthVerify(
	verify model.AuthVerifyArgs,
	session *session.ClientSession,
) (*model.AuthVerifyResult, error) {
	result, err := model.AuthVerify(verify, session)
	if result.Network != nil {
		awsMessageSender := GetAWSMessageSender()
		awsMessageSender.SendAccountMessageTemplate(
			verify.UserAuth,
			&NetworkWelcomeTemplate{},
		)
	}
	return result, err
}
