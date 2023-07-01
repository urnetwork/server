package jwt

import (
	"errors"
	"crypto/rsa"
	"crypto/x509"
    "encoding/pem"

	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
)


// see https://github.com/golang-jwt/jwt
// see https://golang-jwt.github.io/jwt/usage/create/



// sign with the same rsa key that is used by bringyour.com


var byPrivateKey = func()(*rsa.PrivateKey) {
	keyPem := bringyour.Vault.RequireBytes("tls/bringyour.com/bringyour.com.key")
	block, _ := pem.Decode(keyPem)
    parseResult, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
    return parseResult.(*rsa.PrivateKey)
}()



type ByJwt struct {
	NetworkId ulid.ULID
	UserId ulid.ULID
	NetworkName string
}

func (self ByJwt) Sign() string {
	token := gojwt.NewWithClaims(gojwt.SigningMethodRS512, gojwt.MapClaims{
		"networkId": self.NetworkId.String(),
		"userId": self.UserId.String(),
		"networkName": self.NetworkName,
	})

	jwtSigned, err := token.SignedString(byPrivateKey)
	if err != nil {
		panic(err)
	}
	return jwtSigned
}


func NewByJwt(networkId ulid.ULID, userId ulid.ULID, networkName string) *ByJwt {
	return &ByJwt{
		NetworkId: networkId,
		UserId: userId,
		NetworkName: networkName,
	}
}

func ParseByJwt(jwtSigned string) (*ByJwt, error) {
	token, err := gojwt.Parse(jwtSigned, func(token *gojwt.Token) (interface{}, error) {
		return byPrivateKey.Public(), nil
	})
	if err == nil {
		claims := token.Claims.(gojwt.MapClaims)
		
		var networkIdString string		
		var networkId ulid.ULID
		var userIdString string
		var userId ulid.ULID
		var networkName string
		var ok bool
		var err error

		networkIdString, ok = claims["networkId"].(string)
		if !ok {
			return nil, errors.New("Malformed jwt.")
		}
		networkId, err = ulid.Parse(networkIdString)
		if err != nil {
			return nil, err
		}
		userIdString, ok = claims["userId"].(string)
		if !ok {
			return nil, errors.New("Malformed jwt.")
		}
		userId, err = ulid.Parse(userIdString)
		if err != nil {
			return nil, err
		}
		networkName, ok = claims["networkName"].(string)
		if !ok {
			return nil, errors.New("Malformed jwt.")
		}

		jwt := &ByJwt{
			NetworkId: networkId,
			UserId: userId,
			NetworkName: networkName,
		}
		return jwt, nil
	}

	return nil, errors.New("Could not verify signed token.")
}

