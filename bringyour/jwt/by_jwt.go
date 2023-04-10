package jwt

import (
	"github.com/golang-jwt/jwt/v5"

	"bringyour.com/bringyour/ulid"
)


// see https://github.com/golang-jwt/jwt
// see https://golang-jwt.github.io/jwt/usage/create/



// sign with the same rsa key that is used by bringyour.com




type ByJwt struct {
	NetworkId ulid.ULID
	UserId ulid.ULID
	NetworkName string
}

func (self ByJwt) Sign() string {
	// fixme
	return ""
}


func NewByJwt(networkId ulid.ULID, userId ulid.ULID, networkName string) *ByJwt {
	return &ByJwt{
		NetworkId: networkId,
		UserId: userId,
		NetworkName: networkName,
	}
}

func ParseByJwt(jwtSigned string) *ByJwt {
	// fixme
	// parse, validate key, etc
	return nil
}

