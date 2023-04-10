package jwt

type AppleJwt struct {
	UserAuth string
	UserName *string
}


// fixme use cache-control header in the response to know when to refresh the list

// https://developer.apple.com/documentation/sign_in_with_apple/fetch_apple_s_public_key_for_verifying_token_signature
// save the keys to a file
// https://appleid.apple.com/auth/keys
// we do not do real time lookups against the api. new versions of the api will contain up to date keys

func ParseAppleJwt(jwtSigned string) *AppleJwt {
	// fixme
	// parse, validate key, etc
	return nil
}
