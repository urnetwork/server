package jwt

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/bringyour"
)

// see https://github.com/golang-jwt/jwt
// see https://golang-jwt.github.io/jwt/usage/create/

// the first key (most recent version) is used to sign new JWTs
var byPrivateKeys = sync.OnceValue(func() []*rsa.PrivateKey {
	keys := []*rsa.PrivateKey{}
	// `ResourcePaths` returns the version paths in descending order
	// hence the `paths[0]` will be the most recent version
	paths, err := bringyour.Vault.ResourcePaths("tls/bringyour.com/bringyour.com.key")
	if err != nil {
		panic(err)
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
})

func bySigningKey() *rsa.PrivateKey {
	return byPrivateKeys()[0]
}

// the bringyour authorization model is:
// Network
//
//	User
//	  Client
//
// Trust verification happens at the user level.
// A client is always tied to a user.
type ByJwt struct {
	NetworkId      bringyour.Id   `json:"network_id,omitempty"`
	NetworkName    string         `json:"network_name,omitempty"`
	UserId         bringyour.Id   `json:"user_id,omitempty"`
	CreateTime     time.Time      `json:"create_time,omitempty"`
	AuthSessionIds []bringyour.Id `json:"auth_session_ids,omitempty"`
	DeviceId       *bringyour.Id  `json:"device_id,omitempty"`
	ClientId       *bringyour.Id  `json:"client_id,omitempty"`
}

func NewByJwt(
	networkId bringyour.Id,
	userId bringyour.Id,
	networkName string,
	authSessionIds ...bringyour.Id,
) *ByJwt {
	return NewByJwtWithCreateTime(
		networkId,
		userId,
		networkName,
		bringyour.NowUtc(),
		authSessionIds...,
	)
}

func NewByJwtWithCreateTime(
	networkId bringyour.Id,
	userId bringyour.Id,
	networkName string,
	createTime time.Time,
	authSessionIds ...bringyour.Id,
) *ByJwt {
	return &ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: networkName,
		// round here so that the string representation in the jwt does not lose information
		CreateTime:     bringyour.CodecTime(createTime),
		AuthSessionIds: authSessionIds,
	}
}

func ParseByJwt(jwtSigned string) (*ByJwt, error) {
	var token *gojwt.Token
	var err error
	// attempt all signing keys
	for _, byPrivateKey := range byPrivateKeys() {
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

	claimsJson, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}

	byJwt := &ByJwt{}
	err = json.Unmarshal(claimsJson, byJwt)
	if err != nil {
		return nil, err
	}

	return byJwt, nil
}

func (self *ByJwt) Sign() string {
	claimsJson, err := json.Marshal(self)
	if err != nil {
		panic(err)
	}

	claims := &gojwt.MapClaims{}
	err = json.Unmarshal(claimsJson, claims)
	if err != nil {
		panic(err)
	}

	token := gojwt.NewWithClaims(gojwt.SigningMethodRS512, claims)

	jwtSigned, err := token.SignedString(bySigningKey())
	if err != nil {
		panic(err)
	}

	return jwtSigned
}

func (self *ByJwt) Client(deviceId bringyour.Id, clientId bringyour.Id) *ByJwt {
	return &ByJwt{
		NetworkId:      self.NetworkId,
		UserId:         self.UserId,
		NetworkName:    self.NetworkName,
		CreateTime:     self.CreateTime,
		AuthSessionIds: self.AuthSessionIds,
		DeviceId:       &deviceId,
		ClientId:       &clientId,
	}
}

func (self *ByJwt) User() *ByJwt {
	return &ByJwt{
		NetworkId:      self.NetworkId,
		UserId:         self.UserId,
		NetworkName:    self.NetworkName,
		CreateTime:     self.CreateTime,
		AuthSessionIds: self.AuthSessionIds,
	}
}

func IsByJwtActive(ctx context.Context, byJwt *ByJwt) bool {
	// test the create time and sessions
	// - all sessions created before a certain time may be expired (`auth_session_expiration`)
	// - individual sessions may be expired (`auth_session`)

	var hasInactiveSession bool

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		if len(byJwt.AuthSessionIds) == 0 {
			result, err := conn.Query(
				ctx,
				`
					SELECT false AS active
					FROM auth_session_expiration
					WHERE
						network_id = $1 AND
						$2 <= expire_time
				`,
				byJwt.NetworkId,
				byJwt.CreateTime,
			)
			bringyour.WithPgResult(result, err, func() {
				hasInactiveSession = result.Next()
			})
		} else {
			authSessionIdPlaceholders := []string{}
			for i := 0; i < len(byJwt.AuthSessionIds); i += 1 {
				// start at $3
				authSessionIdPlaceholders = append(authSessionIdPlaceholders, fmt.Sprintf("$%d", 3+i))
			}
			args := []any{
				byJwt.NetworkId,
				byJwt.CreateTime,
			}
			for _, authSessionId := range byJwt.AuthSessionIds {
				args = append(args, authSessionId)
			}
			result, err := conn.Query(
				ctx,
				`
					SELECT false AS active
					FROM auth_session_expiration
					WHERE
						network_id = $1 AND
						$2 <= expire_time

					UNION ALL

					SELECT active
					FROM auth_session
					WHERE
						auth_session_id IN (`+strings.Join(authSessionIdPlaceholders, ",")+`) AND
						active = false
					LIMIT 1
				`,
				args...,
			)
			bringyour.WithPgResult(result, err, func() {
				hasInactiveSession = result.Next()
			})
		}
	})

	return !hasInactiveSession
}
