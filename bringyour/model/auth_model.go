package model

// a create network session id to hold the auth of a network create in progress


// user_id, network_id, auth_type, email, phone, auth_jwt, name
// unique email
// unique phone

// network_id, network_name
// unique network_name


// fixme convert jwt to network_id


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


func AuthLogin(login *AuthLoginArgs) (*AuthLoginResult, error) {
	// fixme
	return nil, nil
}



type AuthLoginWithPasswordArgs struct {
	userAuth string
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


func AuthLoginWithPassword(loginWithPassword *AuthLoginWithPasswordArgs) (*AuthLoginWithPasswordResult, error) {
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


func AuthValidate(validate *AuthValidateArgs) (*AuthValidateResult, error) {
	// fixme
	return nil, nil
}




type AuthValidateSendArgs struct {
	userAuth string
}

type AuthValidateSendResult struct {
}



type AuthPasswordResetArgs struct {
	userAuth string
}

type AuthPasswordResetResult struct {
}



type AuthPasswordSetArgs struct {
	resetCode string
	password string
}

type AuthPasswordSetResult struct {
	
}

func AuthPasswordSet(passwordSet *AuthPasswordSetArgs) (*AuthPasswordSetResult, error) {
	// fixme
	return nil, nil
}

