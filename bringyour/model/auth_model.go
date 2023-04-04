package model


import (

	"bringyour.com/bringyour"
)


type AuthLoginArgs struct {
	userAuth *string
	authJwtType *string
	authJwt *string
}

type AuthLoginResult struct {
	authAllowed []string
	error AuthLoginResultError
	network AuthLoginResultNetwork
}

type AuthLoginResultError struct {
	suggestedUserAuth string
	message string
}

type AuthLoginResultNetwork struct {
	byJwt string
}


// fixme add ClientSession to all model functions
func AuthLogin(login *AuthLoginArgs, session *bringyour.ClientSession) (*AuthLoginResult, error) {
	// fixme

	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session.clientIpv4)
	if !allow {
		return nil, error("")
	}

	if login.userAuth != nil {
		// var networkId Ulid
		// var userId Ulid
		var authType string
		// select authType where userAuth = ?
		if authType == nil {
			// new network
		}
		else {
			// return authAllowed = authTyope
		}
	}
	else if authJwt != nil {
		authJwt := ParseAuthJwt(login.authJwt, login.authJwtType)
		if authJwt != nil {
			var authType string
			// select authType where userAuth = ?
			if authType == nil {
				// new network
			}
			else if authType == authJwt.authType {
				// successful login, return a by jwt
				byJwt = CreateByJwt(network_id, user_id)
			}
			else {
				// return authAllowed = authTyope
			}
		}
	}

	// if userAuth, check if there is an existing user and network
	// if jwt, validate that the jwt are valid, then check if there is an existing user and network

	// todo sql
	
	// SetUserAuthAttemptSuccess(userAuthAttemptId, true)

	return nil, nil
}



type AuthLoginWithPasswordArgs struct {
	userAuth string
	password string
}

type AuthLoginWithPasswordResult struct {
	validationRequired *AuthLoginWithPasswordResultValidation
	network *AuthLoginWithPasswordResultNetwork
	error *AuthLoginWithPasswordResultError
}

type AuthLoginWithPasswordResultValidation struct {
	userAuth string
}

type AuthLoginWithPasswordResultValidationNetwork struct {
	byJwt *string
	name *string
}

type AuthLoginWithPasswordResultValidationError struct {
	message string
}


func AuthLoginWithPassword(
	loginWithPassword *AuthLoginWithPasswordArgs,
	session *bringyour.ClientSession,
) (*AuthLoginWithPasswordResult, error) {
	userAuthAttemptId, allow := UserAuthAttempt(userAuth, session.clientIpv4)
	if !allow {
		return nil, error("")
	}

	passwordHash := computePasswordHash(loginWithPassword.password)

	// select user_id, passwordHash, validated

	// list of network_ids where the user is associated
	// expect list to be length 1

	// fixme
	return nil, nil
}



type AuthValidateArgs struct {
	userAuth string
	validateCode string
}

type AuthValidateResult struct {
	network *AuthValidateResultNetwork
}

type AuthValidateResultNetwork struct {
	byJwt string
}


func AuthValidate(validate *AuthValidateArgs, session *bringyour.ClientSession) (*AuthValidateResult, error) {
	// fixme
	return nil, nil
}




type AuthValidateSendArgs struct {
	userAuth string
}

type AuthValidateSendResult struct {
}


func AuthValidateSend(
	validateSend *model.AuthValidateSendArgs,
	session *bringyour.ClientSession,
) (model.AuthValidateSendResult, error) {
	// lookup userAuth type
	// send either email or sms
	// fixme
	return nil, nil
}





type AuthPasswordResetArgs struct {
	userAuth string
}

type AuthPasswordResetResult struct {
}


func AuthPasswordReset(
	validateSend *model.AuthPasswordResetArgs,
	session *bringyour.ClientSession,
) (model.AuthPasswordResetResult, error) {
	// lookup userAuth type
	// send either email or sms
	// fixme
	return nil, nil
}





type AuthPasswordSetArgs struct {
	resetCode string
	password string
}

type AuthPasswordSetResult struct {
	
}

func AuthPasswordSet(passwordSet *AuthPasswordSetArgs, session *bringyour.ClientSession) (*AuthPasswordSetResult, error) {
	// fixme
	return nil, nil
}

