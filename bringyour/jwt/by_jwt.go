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


// the first key (most recent version) is used to sign new JWTs
var byPrivateKeys := func() []*rsa.PrivateKey {
	keys := []*rsa.PrivateKey{}
	// `ResourcePaths` returns the version paths in descending order
	// hence the `paths[0]` will be the most recent version
	paths, err := bringyour.Vault.ResourcePaths("tls/bringyour.com/bringyour.com.key")
	if err != nil {
		panic
	}
	for _, path := range paths {
		bytes, err := os.ReadFile(path)
	    if err != nil {
	        panic(err)
	    }
		block, _ := pem.Decode(bytes)
	    parseResult, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
	    keys = append(keys, parseResult.(*rsa.PrivateKey))
	}
	return keys
}


func bySigningKey() *rsa.PrivateKey {
	return byPrivateKeys[0]
}



type ByJwt struct {
	NetworkId ulid.ULID
	UserId ulid.ULID
	NetworkName string
	ClientId *Id
}

func (self ByJwt) Sign() string {
	claims := gojwt.MapClaims{
		"networkId": self.NetworkId.String(),
		"userId": self.UserId.String(),
		"networkName": self.NetworkName,
	}
	if self.ClientId != nil {
		claims["clientId"] = self.ClientId.String()
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodRS512, claims)

	jwtSigned, err := token.SignedString(bySigningKey())
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
	var token *gojwt.Token
	var err error
	// attempt all signing keys
	for _, byPrivateKey := range byPrivateKeys {
		token, err = gojwt.Parse(jwtSigned, func(token *gojwt.Token) (interface{}, error) {
			return byPrivateKey.Public(), nil
		})
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, errors.New("Could not verify signed token.")
	}

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

