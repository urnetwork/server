package jwt

import (
	"errors"
	"crypto/rsa"
	"crypto/x509"
    "encoding/pem"

	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/bringyour/ulid"
)


// see https://github.com/golang-jwt/jwt
// see https://golang-jwt.github.io/jwt/usage/create/



// sign with the same rsa key that is used by bringyour.com


var byPrivateKey = func()(*rsa.PrivateKey) {
	// fixme load from secure storage
	keyPem := `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQCeISXCHpqYgu9E
DI6swmiiVYPEt+LrHlZ0tKC5E4jEAKmyPRBn37JtytJb4bbkGgZCLAoQVXOfZUC5
uIQG4FAsPDxgtSsN2IWLJ+epUatcLljsqCy2lTHVvAm/gRxTn50t0YOxvsKACP+s
SOtBQpaoqDhu+N1KOykszvFeDQIyraNWQ4GYYC9CjDsUn5FwErQaMdEeJHS9Cmwy
1jRdP5E95vx4jE6wzC21LyA89kJdjyDmgypAEaZO+LfWTGaggC1dJBHZPiCnKlbs
pnDvsz8nr5vyzmQJIAD3fbPsI1FChGd1ZyAFOdcEMhqToAqjt8hOLoXrPoQTq8Uh
1s064MAtAgMBAAECggEASuToPUjBb/qT2GcaLDjn1fsqrcFqeHGmASCL/xyBalPm
C8VgP9JzcAzgFSSSuvaYgD7bhWDzoksSnOQHpDoZvtnIvwUPnz8uAPqlfkxwHPjW
pUAB7Xg8Yj7tXwaHpBO1Hj5dYZI4DOw2LCNdSUuAj+Ec2XKFXOMoXVCmgSUoJVfZ
fzYq3SCb8WHip1KO4oIqaxChhtmUMeBJ01Xys7coik8XM9js1zPsCyzWsJ8uOW+p
bEBTfUTOpwvUcZRdvUilsZTsl44W8qnqoxFi8tYPUlOivnFE8sqbRPqM1nSHlPOD
iuPi0g0wC/YnBU+Vm1vechIHjrUpGp5Iy8z0KJyAAQKBgQDSMvxa7mspR+ulQ2Bx
T975QWOysL54kBTE1sFaLJjKPnyE9apRJKZcCfM/zG8du6LCB4EX1+/4jWfIhLfz
x1cq2086IyIw9puomy+0nfKqnNKIifuf65qBg//sH6izvH5b+UhtWTRL0c4sIkf6
vvC9bHFvRDdaO8spR1ev4KJzrQKBgQDAlbBe+61LgT4+Hu1kToLMa5WJYlqtbwiw
9nmedwk/3B61xYVZhYsi9oJTyt8JgojZoGEVl/bTZghDRAwkDieqaAMLEoYs6Y5w
dFVusVkbVcqxFUKjnrZ4dfNXOskD2GgFKfr0u/0UwR5dU1W6jtw0/l6ol79yBss0
DP1Ef5UOgQKBgQCanoajHN4W76CXYIiA0Y/jKgZ8WybA6LteT9rKyiNaIbzW0R8H
sT3uViNouqjB5lRDBeIf9+e9ncbJ6VanK+siy0/sJAvymHTIAd+FrOnkNpdneJhv
eo+c1cxblK40CGOqpCRyyzt8ykgujskD2ZCcxjhq8HMHHRTEuIX4CfV1wQKBgAaE
imWMiv7lLuAXV91vMsoMUhFGPN9lxJuIm/EbAjshDgEE4FB5To4uXZbMZOQDgPIs
lVyPuhDJgToVkXue5wTDZGb5h4T5mpJ/vWxzoBpmuudnWswC0RYel8+585enuU2D
cDTcL+KF7qsl6N7ZeuZoPXfjOt13EWV/kwrAbqEBAoGARcKvLxBqlpdHEfODfJ59
1joSDYLj4cXxtPvlLLJudrSrS1Rsgy8GvRepdQlyLJu6cf8DDyPlYWKdSv5wLlH3
8v1U1VTUV8FweZlH5x40/hj8TZ39bhUi6cxgwBHJCQN4KoqNQRZ600FlMOZEH4SN
Xrdhddzb37sKiZnH4UCwTEU=
-----END PRIVATE KEY-----`
	block, _ := pem.Decode([]byte(keyPem))
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

