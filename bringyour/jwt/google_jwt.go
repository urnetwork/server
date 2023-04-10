package jwt

type GoogleJwt struct {
	UserAuth string
	UserName *string
}


// see https://developers.google.com/identity/sign-in/web/backend-auth
// https://www.googleapis.com/oauth2/v3/certs


func ParseGoogleJwt(jwtSigned string) *GoogleJwt {
	// fixme
	// parse, validate key, etc
	return nil
}
