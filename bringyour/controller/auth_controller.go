package controller

import (
    "fmt"
    // "errors"

    "bringyour.com/bringyour"
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
    // if verification required, send it
    if result != nil && result.VerificationRequired != nil {
        verifySend := AuthVerifySendArgs{
            UserAuth: result.VerificationRequired.UserAuth,
        }
        AuthVerifySend(verifySend, session)
    }
    return result, err
}


type AuthVerifySendArgs struct {
    UserAuth string `json:"userAuth"`
}

type AuthVerifySendResult struct {
    UserAuth string `json:"userAuth"`
}

func AuthVerifySend(
    verifySend AuthVerifySendArgs,
    session *session.ClientSession,
) (*AuthVerifySendResult, error) {
    userAuth, userAuthType := model.NormalUserAuthV1(&verifySend.UserAuth)

    verifyCreateCode := model.AuthVerifyCreateCodeArgs{
        UserAuth: *userAuth,
    }
    verifyCreateCodeResult, err := model.AuthVerifyCreateCode(verifyCreateCode, session)
    if err != nil {
        return nil, err
    }
    if verifyCreateCodeResult.VerifyCode != nil {   
        switch userAuthType {
        case model.UserAuthTypeEmail:
            err := sendAccountEmail(
                *userAuth,
                "Verify your email",
                createVerifyBodyHtml(*verifyCreateCodeResult.VerifyCode),
                createVerifyBodyText(*verifyCreateCodeResult.VerifyCode),
            )
            if err != nil {
                bringyour.Logger().Printf("Error sending email: %s\n", err)
            }
        case model.UserAuthTypePhone:
            err := sendAccountSms(
                *userAuth,
                createVerifyBodyText(*verifyCreateCodeResult.VerifyCode),
            )
            if err != nil {
                bringyour.Logger().Printf("Error sending sms: %s\n", err)
            }
        }
    }

    result := &AuthVerifySendResult{
        UserAuth: *userAuth,
    }
    return result, nil
}

func createVerifyBodyHtml(verifyCode string) string {
    // fixme
    return fmt.Sprintf(`Verify your BringYour account using this code:
<br><br><strong>%s</strong>
<br><br><b>Why you received this email.</b>
<br><a href="https://bringyour.com">BringYour</a> requires verification that a user of the service is in control of the identity they assign to their network. The network cannot be used until the identity is verified.
<br><br>If you did not make this change or you believe and unauthorized person has accessed your account, please contact <a href="mailto:security@bringyour.com">security@bringyour.com</a>.
<br><br>Copyright 2023 BringYour, Inc., 2261 Market Street #5245, San Francisco, CA 94114, United States`, verifyCode)
}

func createVerifyBodyText(verifyCode string) string {
    // fixme
    return fmt.Sprintf("Verify your BringYour account using this code: %s", verifyCode)
}


func TestAuthVerifyCode(userAuth string) {
    normalUserAuth, userAuthType := model.NormalUserAuthV1(&userAuth)

    verifyCode := model.TestCreateVerifyCode()

    switch userAuthType {
    case model.UserAuthTypeEmail:
        err := sendAccountEmail(
            *normalUserAuth,
            "Verify your email",
            createVerifyBodyHtml(verifyCode),
            createVerifyBodyText(verifyCode),
        )
        if err != nil {
            bringyour.Logger().Printf("Error sending email: %s\n", err)
        }
    case model.UserAuthTypePhone:
        err := sendAccountSms(
            *normalUserAuth,
            createVerifyBodyText(verifyCode),
        )
        if err != nil {
            bringyour.Logger().Printf("Error sending sms: %s\n", err)
        }
    }
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
            err := sendAccountEmail(
                *userAuth,
                "Reset your password",
                createResetBodyHtml(*resetCreateCodeResult.ResetCode),
                createResetBodyText(*resetCreateCodeResult.ResetCode),
            )
            if err != nil {
                bringyour.Logger().Printf("Error sending email: %s\n", err)
            }
        case model.UserAuthTypePhone:
            err := sendAccountSms(
                *userAuth,
                createResetBodyText(*resetCreateCodeResult.ResetCode),
            )
            if err != nil {
                bringyour.Logger().Printf("Error sending sms: %s\n", err)
            }
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
