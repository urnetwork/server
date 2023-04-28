package model


import (
	"bringyour.com/bringyour/jwt"
)


type AuthJwt struct {
	AuthType AuthType
	UserAuth string
	UserName string
}


func ParseAuthJwt(authJwt string, authJwtType AuthType) *AuthJwt {
	switch authJwtType {
	case AuthTypeApple:
		appleJwt, err := jwt.ParseAppleJwt(authJwt)
		if err != nil {
			return nil
		}
		return &AuthJwt{
			AuthType: AuthTypeApple,
			UserAuth: appleJwt.UserAuth,
			UserName: appleJwt.UserName,
		}
	case AuthTypeGoogle:
		googleJwt, err := jwt.ParseGoogleJwt(authJwt)
		if err != nil {
			return nil
		}
		return &AuthJwt{
			AuthType: AuthTypeGoogle,
			UserAuth: googleJwt.UserAuth,
			UserName: googleJwt.UserName,
		}
	}
	return nil
}
