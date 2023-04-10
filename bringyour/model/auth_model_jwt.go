package model


type AuthJwt struct {
	AuthType string
	UserAuth string
	UserName *string
}


func ParseAuthJwt(authJwt string, authJwtType string) *AuthJwt {
	// returns nil if the jwt does not match the given type
	return nil

}
