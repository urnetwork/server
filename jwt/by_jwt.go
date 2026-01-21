package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
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

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// see https://github.com/golang-jwt/jwt
// see https://golang-jwt.github.io/jwt/usage/create/

var byJwtTlsKeyPaths = sync.OnceValue(func() []string {
	jwt := server.Vault.RequireSimpleResource("jwt.yml")
	return jwt.RequireStringList("tls_key_paths")
})

// one month
const expiryDuration = 30 * 24 * time.Hour

// the first key (most recent version) is used to sign new JWTs
var byPrivateKeys = sync.OnceValue(func() []crypto.PrivateKey {
	keys := []crypto.PrivateKey{}
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
				if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
					glog.Errorf("[jwt]loaded ec key \"%s\"\n", path)
					keys = append(keys, key)
				} else {
					if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
						glog.Errorf("[jwt]loaded pkcs8 key \"%s\"\n", path)
						keys = append(keys, key)
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
	}
	if len(keys) == 0 {
		panic(errors.Join(errs...))
	}
	return keys
})

func byRsaSigningKey() *rsa.PrivateKey {
	for _, key := range byPrivateKeys() {
		switch v := key.(type) {
		case *rsa.PrivateKey:
			return v
		}
	}
	return nil
}

func byEcdsaSigningKey() *ecdsa.PrivateKey {
	for _, key := range byPrivateKeys() {
		switch v := key.(type) {
		case *ecdsa.PrivateKey:
			return v
		}
	}
	return nil
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
	Pro            bool        `json:"pro,omitempty"`
	gojwt.RegisteredClaims
}

func NewByJwt(
	networkId server.Id,
	userId server.Id,
	networkName string,
	guestMode bool,
	pro bool,
	authSessionIds ...server.Id,
) *ByJwt {
	if networkId == (server.Id{}) {
		panic(fmt.Errorf("network_id must be set"))
	}
	if userId == (server.Id{}) {
		panic(fmt.Errorf("user_id must be set"))
	}

	// glog.Infof("Creating ByJwt for network_id=%s, user_id=%s, guest_mode=%v, pro=%v", networkId, userId, guestMode, pro)

	return NewByJwtWithCreateTime(
		networkId,
		userId,
		networkName,
		server.NowUtc(),
		guestMode,
		pro,
		authSessionIds...,
	)
}

func NewByJwtWithCreateTime(
	networkId server.Id,
	userId server.Id,
	networkName string,
	createTime time.Time,
	guestMode bool,
	pro bool,
	authSessionIds ...server.Id,
) *ByJwt {
	if networkId == (server.Id{}) {
		panic(fmt.Errorf("network_id must be set"))
	}
	if userId == (server.Id{}) {
		panic(fmt.Errorf("user_id must be set"))
	}

	return &ByJwt{
		NetworkId:   networkId,
		UserId:      userId,
		NetworkName: networkName,
		GuestMode:   guestMode,
		Pro:         pro,
		// round here so that the string representation in the jwt does not lose information
		CreateTime:     server.CodecTime(createTime),
		AuthSessionIds: authSessionIds,
		RegisteredClaims: gojwt.RegisteredClaims{
			ExpiresAt: gojwt.NewNumericDate(time.Now().Add(expiryDuration)),
		},
	}
}

func ParseByJwt(ctx context.Context, jwtSigned string) (*ByJwt, error) {
	var token *gojwt.Token
	var err error

	// todo - remove this once clients support refresh
	parserOptions := []gojwt.ParserOption{
		gojwt.WithoutClaimsValidation(),
	}

	// attempt all signing keys
	for _, byPrivateKey := range byPrivateKeys() {
		// todo - ParseWithClaims instead of jwt.Parse
		// this will get newly added RegisteredClaims which includes ExipiresAt
		token, err = gojwt.Parse(jwtSigned, func(token *gojwt.Token) (any, error) {
			return byPrivateKey.(interface{ Public() crypto.PublicKey }).Public(), nil
		}, parserOptions...)
		if err == nil {
			break
		}
	}
	if token == nil {
		return nil, errors.New("Could not verify signed token.")
	}

	// if !token.Valid {
	// 	return nil, errors.New("Invalid token.")
	// }

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

	err = fixByJwt(ctx, byJwt)
	if err != nil {
		return nil, err
	}

	return byJwt, nil
}

func ParseByJwtUnverified(ctx context.Context, jwtStr string) (*ByJwt, error) {
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

	err = fixByJwt(ctx, byJwt)
	if err != nil {
		return nil, err
	}

	return byJwt, nil
}

// func (self *ByJwt) Sign() string {
// 	claimsJson, err := json.Marshal(self)
// 	if err != nil {
// 		panic(err)
// 	}

// 	claims := &gojwt.MapClaims{}
// 	err = json.Unmarshal(claimsJson, claims)
// 	if err != nil {
// 		panic(err)
// 	}

// 	token := gojwt.NewWithClaims(gojwt.SigningMethodRS512, claims)

// 	jwtSigned, err := token.SignedString(bySigningKey())
// 	if err != nil {
// 		panic(err)
// 	}

// 	return jwtSigned
// }

func (self *ByJwt) Sign() string {
	return sign(self)
}

func (self *ByJwt) Client(deviceId server.Id, clientId server.Id) *ByJwt {
	return &ByJwt{
		NetworkId:      self.NetworkId,
		UserId:         self.UserId,
		NetworkName:    self.NetworkName,
		CreateTime:     self.CreateTime,
		AuthSessionIds: self.AuthSessionIds,
		GuestMode:      self.GuestMode,
		Pro:            self.Pro,
		DeviceId:       &deviceId,
		ClientId:       &clientId,
		RegisteredClaims: gojwt.RegisteredClaims{
			ExpiresAt: gojwt.NewNumericDate(time.Now().Add(expiryDuration)),
		},
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
		Pro:            self.Pro,
		RegisteredClaims: gojwt.RegisteredClaims{
			ExpiresAt: gojwt.NewNumericDate(time.Now().Add(expiryDuration)),
		},
	}
}

// in some cases, the byJwt might be corrupt
// in some of those cases, we can still recover it
func fixByJwt(ctx context.Context, byJwt *ByJwt) error {
	if byJwt.NetworkId == (server.Id{}) {
		// the NetworkId is missing (FIXME why?)
		// it can be recovered from the client id or the user id

		if byJwt.UserId != (server.Id{}) {
			var cachedValue string
			key := jwtNetworkIdByUserIdKey(byJwt.UserId)
			server.Redis(ctx, func(r server.RedisClient) {
				cachedValue, _ = r.Get(ctx, key).Result()
			})
			if cachedValue != "" {
				networkId, err := server.ParseId(cachedValue)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.Infof("[jwt]fixed network_id with user_id (cached)\n")
				}
			} else {
				networkId, err := getNetworkIdForUser(ctx, byJwt.UserId)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.Infof("[jwt]fixed network_id with user_id\n")
					storeCtx := context.Background()
					ttl := 15 * time.Minute
					go server.HandleError(func() {
						server.Redis(storeCtx, func(r server.RedisClient) {
							// ignore the error
							r.SetNX(storeCtx, key, networkId.String(), ttl).Err()
						})
					})
				}
			}
		} else if byJwt.ClientId != nil {
			var cachedValue string
			key := jwtNetworkIdByClientIdKey(*byJwt.ClientId)
			server.Redis(ctx, func(r server.RedisClient) {
				cachedValue, _ = r.Get(ctx, key).Result()
			})
			if cachedValue != "" {
				networkId, err := server.ParseId(cachedValue)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.Infof("[jwt]fixed network_id with client_id (cached)\n")
				}
			} else {
				networkId, err := getNetworkIdForClient(ctx, *byJwt.ClientId)
				if err == nil {
					byJwt.NetworkId = networkId
					glog.Infof("[jwt]fixed network_id with client_id\n")
					storeCtx := context.Background()
					ttl := 15 * time.Minute
					go server.HandleError(func() {
						server.Redis(storeCtx, func(r server.RedisClient) {
							// ignore the error
							r.SetNX(storeCtx, key, networkId.String(), ttl).Err()
						})
					})
				}
			}
		}

		if byJwt.NetworkId == (server.Id{}) {
			return fmt.Errorf("Missing network_id")
		}
	}

	if byJwt.UserId == (server.Id{}) {
		return fmt.Errorf("Missing user_id")
	}

	return nil
}

func jwtNetworkIdByUserIdKey(userId server.Id) string {
	return fmt.Sprintf("jwt_network_id_u_%s", userId)
}

func getNetworkIdForUser(ctx context.Context, userId server.Id) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
			    network_id
			FROM network
			WHERE admin_user_id = $1
			`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = fmt.Errorf("network_id not found for user_id=%s", userId)
			}
		})
	})
	return
}

func jwtNetworkIdByClientIdKey(clientId server.Id) string {
	return fmt.Sprintf("jwt_network_id_c_%s", clientId)
}

func getNetworkIdForClient(ctx context.Context, clientId server.Id) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
			    network_id
			FROM network_client
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = fmt.Errorf("network_id not found for client_id=%s", clientId)
			}
		})
	})
	return
}

func LoadByJwtFromClientId(ctx context.Context, clientId server.Id) (byJwt *ByJwt, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
			    network.network_id,
			    network.admin_user_id,
			    network.network_name,
			    network_client.device_id

			FROM network_client

			INNER JOIN network ON network.network_id = network_client.network_id

			WHERE
			    network_client.client_id = $1
		    `,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var networkId server.Id
				var userId server.Id
				var networkName string
				var deviceId server.Id
				server.Raise(result.Scan(
					&networkId,
					&userId,
					&networkName,
					&deviceId,
				))

				guestMode := false
				// FIXME
				pro := false

				byJwt = NewByJwt(
					networkId,
					userId,
					networkName,
					guestMode,
					pro,
				).Client(deviceId, clientId)
			} else {
				returnErr = fmt.Errorf("Client not found.")
			}
		})
	})
	return
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

func sign(claims gojwt.Claims) string {
	var signingMethod gojwt.SigningMethod
	var key any

	if ecdsaKey := byEcdsaSigningKey(); ecdsaKey != nil {
		switch bitLen := ecdsaKey.Curve.Params().N.BitLen(); bitLen {
		case 256:
			signingMethod = gojwt.SigningMethodES256
		case 384:
			signingMethod = gojwt.SigningMethodES384
		case 512:
			signingMethod = gojwt.SigningMethodES512
		default:
			panic(fmt.Errorf("Unsupported ECDSA bit len %d", bitLen))
		}
		key = ecdsaKey
	} else if rsaKey := byRsaSigningKey(); rsaKey != nil {
		if bitLen := rsaKey.N.BitLen(); 2048 <= bitLen {
			signingMethod = gojwt.SigningMethodRS512
		} else {
			panic(fmt.Errorf("Unsupported RSA bit len %d", bitLen))
		}
		key = rsaKey
	} else {
		panic(fmt.Errorf("No signing key found"))
	}
	token := gojwt.NewWithClaims(signingMethod, claims)
	jwtSigned, err := token.SignedString(key)
	if err != nil {
		panic(err)
	}
	return jwtSigned
}
