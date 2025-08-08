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

	"github.com/golang/glog"

	"github.com/urnetwork/server"
)

// see https://github.com/golang-jwt/jwt
// see https://golang-jwt.github.io/jwt/usage/create/

var byJwtTlsKeyPaths = sync.OnceValue(func() []string {
	jwt := server.Vault.RequireSimpleResource("jwt.yml")
	return jwt.RequireStringList("tls_key_paths")
})

// the first key (most recent version) is used to sign new JWTs
var byPrivateKeys = sync.OnceValue(func() []*rsa.PrivateKey {
	keys := []*rsa.PrivateKey{}
	glog.Infof("[jwt]paths: %s", byJwtTlsKeyPaths())
	errs := []error{}
	for _, jwtTlsKeyPath := range byJwtTlsKeyPaths() {
		// `ResourcePaths` returns the version paths in descending order
		// hence the `paths[0]` will be the most recent version
		paths, err := server.Vault.ResourcePaths(jwtTlsKeyPath)
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, path := range paths {
				bytes, err := os.ReadFile(path)
				if err != nil {
					panic(err)
				}
				block, _ := pem.Decode(bytes)

				keyPathErrs := []error{}
				if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
					glog.Errorf("[jwt]loaded pkcs8 key \"%s\"\n", path)
					keys = append(keys, key.(*rsa.PrivateKey))
				} else {
					keyPathErrs = append(keyPathErrs, err)
					if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
						glog.Errorf("[jwt]loaded pkcs1 key \"%s\"\n", path)
						keys = append(keys, key)
					} else {
						keyPathErrs = append(keyPathErrs, err)
						err = errors.Join(keyPathErrs...)
						glog.Errorf("[jwt]could not load key \"%s\". err = %s\n", path, err)
						errs = append(errs, err)
					}
				}
			}
		}
	}
	if len(keys) == 0 {
		panic(errors.Join(errs...))
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
	NetworkId      server.Id   `json:"network_id,omitempty"`
	NetworkName    string      `json:"network_name,omitempty"`
	UserId         server.Id   `json:"user_id,omitempty"`
	CreateTime     time.Time   `json:"create_time,omitempty"`
	AuthSessionIds []server.Id `json:"auth_session_ids,omitempty"`
	DeviceId       *server.Id  `json:"device_id,omitempty"`
	ClientId       *server.Id  `json:"client_id,omitempty"`
	GuestMode      bool        `json:"guest_mode,omitempty"`
}

func NewByJwt(
	networkId server.Id,
	userId server.Id,
	networkName string,
	guestMode bool,
	authSessionIds ...server.Id,
) *ByJwt {
	return NewByJwtWithCreateTime(
		networkId,
		userId,
		networkName,
		server.NowUtc(),
		guestMode,
		authSessionIds...,
	)
}

func NewByJwtWithCreateTime(
	networkId server.Id,
	userId server.Id,
	networkName string,
	createTime time.Time,
	guestMode bool,
	authSessionIds ...server.Id,
) *ByJwt {
	return &ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: networkName,
		GuestMode:   guestMode,
		// round here so that the string representation in the jwt does not lose information
		CreateTime:     server.CodecTime(createTime),
		AuthSessionIds: authSessionIds,
	}
}

func ParseByJwt(jwtSigned string) (*ByJwt, error) {
	var token *gojwt.Token
	var err error
	// attempt all signing keys
	for _, byPrivateKey := range byPrivateKeys() {
		token, err = gojwt.Parse(jwtSigned, func(token *gojwt.Token) (any, error) {
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

func ParseByJwtUnverified(jwtStr string) (*ByJwt, error) {
	token, _, err := gojwt.NewParser().ParseUnverified(jwtStr, &gojwt.MapClaims{})
	if err != nil {
		return nil, err
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

func (self *ByJwt) Client(deviceId server.Id, clientId server.Id) *ByJwt {
	return &ByJwt{
		NetworkId:      self.NetworkId,
		UserId:         self.UserId,
		NetworkName:    self.NetworkName,
		CreateTime:     self.CreateTime,
		AuthSessionIds: self.AuthSessionIds,
		GuestMode:      self.GuestMode,
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
		GuestMode:      self.GuestMode,
	}
}

func IsByJwtActive(ctx context.Context, byJwt *ByJwt) bool {
	// test the create time and sessions
	// - all sessions created before a certain time may be expired (`auth_session_expiration`)
	// - individual sessions may be expired (`auth_session`)

	// FIXME perf
	if true {
		return true
	}

	var hasInactiveSession bool

	server.Db(ctx, func(conn server.PgConn) {
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
			server.WithPgResult(result, err, func() {
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
			server.WithPgResult(result, err, func() {
				hasInactiveSession = result.Next()
			})
		}
	})

	return !hasInactiveSession
}
