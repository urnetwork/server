package jwt


import (
	"net/http"
	"encoding/json"

	"github.com/go-jose/go-jose/v3"
)



// validate public key
// use the cache control to determine when to refresh the table

// read the url
// json unmarshal into JSONWebKeySet

type JwkValidator struct {
	jwkUrl string
	keySet *jose.JSONWebKeySet
}


	// type https://pkg.go.dev/crypto/rsa#PrivateKey.Public
func (self JwkValidator) Keys() []interface{} {
	// RsaPublicKeys iterate Keys in JSONWebKeySet 
	// key.Key
	var keys []interface{}
	for _, key := range self.keySet.Keys {
		keys = append(keys, key.Key)
	}
	return keys
}

func NewJwkValidator(jwkUrl string) *JwkValidator {
	// fixme store cache control and check it on use
	keySet := parseKeySet(jwkUrl)
	return &JwkValidator{
		jwkUrl: jwkUrl,
		keySet: keySet,
	}
}

func parseKeySet(jwkUrl string) *jose.JSONWebKeySet {
	res, err := http.Get(jwkUrl)
	if err != nil {
		panic(err)
	}
	var keySet jose.JSONWebKeySet
	err = json.NewDecoder(res.Body).Decode(&keySet)
	if err != nil {
		panic(err)
	}
	return &keySet
}

