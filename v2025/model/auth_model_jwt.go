package model

import (
	"fmt"

	"github.com/urnetwork/server/v2025/jwt"
)

type AuthJwt struct {
	AuthType AuthType
	UserAuth string
	UserName string
}

func ParseAuthJwt(authJwt string, authJwtType AuthType) (*AuthJwt, error) {
	switch authJwtType {
	case AuthTypeApple:
		appleJwt, err := jwt.ParseAppleJwt(authJwt)
		if err != nil {
			return nil, err
		}
		return &AuthJwt{
			AuthType: AuthTypeApple,
			UserAuth: appleJwt.UserAuth,
			UserName: appleJwt.UserName,
		}, nil
	case AuthTypeGoogle:
		googleJwt, err := jwt.ParseGoogleJwt(authJwt)
		if err != nil {
			return nil, err
		}
		return &AuthJwt{
			AuthType: AuthTypeGoogle,
			UserAuth: googleJwt.UserAuth,
			UserName: googleJwt.UserName,
		}, nil
	}
	return nil, fmt.Errorf("Unknown auth type: %s", authJwtType)
}

func ParseAuthJwtUnverified(authJwt string, authJwtType AuthType) (*AuthJwt, error) {
	switch authJwtType {
	case AuthTypeApple:
		appleJwt, err := jwt.ParseAppleJwtUnverified(authJwt)
		if err != nil {
			return nil, err
		}
		return &AuthJwt{
			AuthType: AuthTypeApple,
			UserAuth: appleJwt.UserAuth,
			UserName: appleJwt.UserName,
		}, nil
	case AuthTypeGoogle:
		googleJwt, err := jwt.ParseGoogleJwtUnverified(authJwt)
		if err != nil {
			return nil, err
		}
		return &AuthJwt{
			AuthType: AuthTypeGoogle,
			UserAuth: googleJwt.UserAuth,
			UserName: googleJwt.UserName,
		}, nil
	}
	return nil, fmt.Errorf("Unknown auth type: %s", authJwtType)
}
