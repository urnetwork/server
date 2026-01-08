package model

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

// ordered in precedence
var proxySigningSecrets = sync.OnceValue(func() [][]byte {
	proxy := server.Vault.RequireSimpleResource("proxy.yml")
	secretStrs := proxy.RequireStringList("secrets")
	var secrets [][]byte
	for _, secretStr := range secretStrs {
		secrets = append(secrets, []byte(secretStr))
	}
	return secrets
})

var proxySigningSecret = sync.OnceValue(func() []byte {
	return proxySigningSecrets()[0]
})

// the signed proxy id is intended to use in the proxy hostname,
// to make it hard to guess a proxy id (160 bits of entropy),
// and to allow the server to fast reject a request
// the returned length is <63 alphanumeric characters
func SignProxyId(proxyId server.Id) string {
	proxyIdBytes := proxyId.Bytes()

	secret := proxySigningSecret()

	h := hmac.New(sha1.New, secret)
	h.Write(proxyIdBytes)
	signature := h.Sum(nil)

	var b []byte
	b = base32.HexEncoding.AppendEncode(b, proxyIdBytes)
	b = base32.HexEncoding.AppendEncode(b, signature)

	return string(b)
}

func ParseProxyId(signedProxyId string) (proxyId server.Id, returnErr error) {
	b := make([]byte, 64)
	var n int
	n, returnErr = base32.HexEncoding.Decode(b, []byte(signedProxyId))

	if returnErr != nil {
		return
	}

	if n < 16 {
		returnErr = fmt.Errorf("Invalid input length")
		return
	}

	proxyId, returnErr = server.IdFromBytes(b[0:16])
	if returnErr != nil {
		return
	}
	signature := b[16:n]

	// validate the signature with all known secrets
	ok := func() bool {
		proxyIdBytes := proxyId.Bytes()
		for _, secret := range proxySigningSecrets() {
			h := hmac.New(sha1.New, secret)
			h.Write(proxyIdBytes)
			checkSignature := h.Sum(nil)
			if slices.Equal(signature, checkSignature) {
				return true
			}
		}
		return false
	}()
	if !ok {
		returnErr = fmt.Errorf("Invalid signature")
		return
	}

	return
}

type ProxyDeviceMode int

const (
	ProxyDeviceModeDevice ProxyDeviceMode = 1
)

type ProxyDeviceConnection struct {
	ProxyId    server.Id `json:"proxy_id"`
	ClientId   server.Id `json:"client_id"`
	InstanceId server.Id `json:"instance_id"`
}

type ProxyDeviceConfig struct {
	ProxyDeviceConnection
	ProxyDeviceMode ProxyDeviceMode `json:"proxy_device_mode"`
	LockSubnets     []netip.Prefix  `json:"lock_subnets"`
	HttpRequireAuth bool            `json:"http_require_auth"`

	InitialDeviceState *ProxyDeviceState `json:"initial_device_state"`
}

type ProxyDeviceState struct {
	Location *sdk.ConnectLocation `json:"location"`

	PerformanceProfile *sdk.PerformanceProfile `json:"performance_profile"`
}

func GetProxyDeviceConnection(ctx context.Context, proxyId server.Id) (proxyDeviceConnection *ProxyDeviceConnection) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				instance_id
			FROM proxy_device_config
			WHERE proxy_id = $1
			`,
			proxyId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var clientId server.Id
				var instanceId server.Id
				server.Raise(result.Scan(&clientId, &instanceId))
				proxyDeviceConnection = &ProxyDeviceConnection{
					ProxyId:    proxyId,
					ClientId:   clientId,
					InstanceId: instanceId,
				}
			}
		})
	})
	return
}

func GetProxyDeviceConnectionForClient(
	ctx context.Context,
	clientId server.Id,
	instanceId server.Id,
) (proxyDeviceConnection *ProxyDeviceConnection) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				proxy_id
			FROM proxy_device_config
			WHERE
				client_id = $1 AND
				instance_id = $2
			`,
			clientId,
			instanceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var proxyId server.Id
				server.Raise(result.Scan(&clientId, &instanceId))
				proxyDeviceConnection = &ProxyDeviceConnection{
					ProxyId:    proxyId,
					ClientId:   clientId,
					InstanceId: instanceId,
				}
			}
		})
	})
	return
}

func CreateProxyDeviceConfig(ctx context.Context, proxyDeviceConfig *ProxyDeviceConfig) error {
	proxyDeviceConfigJson, err := json.Marshal(proxyDeviceConfig)
	if err != nil {
		return err
	}

	server.Tx(ctx, func(tx server.PgTx) {
		proxyId := server.NewId()
		instanceId := server.NewId()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO proxy_device_config (
				proxy_id,
				client_id,
				instance_id,
				config_json
			)
			VALUES ($1, $2, $3, $4)
			`,
			proxyId,
			proxyDeviceConfig.ClientId,
			instanceId,
			proxyDeviceConfigJson,
		))
	})
	return nil
}

func RemoveProxyDeviceConfig(ctx context.Context, proxyId server.Id) {

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM proxy_device_config
			WHERE proxy_id = $1
			`,
			proxyId,
		))
	})

}

func GetProxyDeviceConfig(ctx context.Context, proxyId server.Id) *ProxyDeviceConfig {
	var proxyDeviceConfigJson string

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				config_json
			FROM proxy_device_config
			WHERE
				proxy_id = $1
			`,
			proxyId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&proxyDeviceConfigJson))
			}
		})
	})

	if proxyDeviceConfigJson == "" {
		return nil
	}

	var proxyDeviceConfig ProxyDeviceConfig
	err := json.Unmarshal([]byte(proxyDeviceConfigJson), &proxyDeviceConfig)
	if err != nil {
		return nil
	}
	return &proxyDeviceConfig
}

func GetProxyDeviceConfigByClientId(ctx context.Context, clientId server.Id, instanceId server.Id) *ProxyDeviceConfig {

	var proxyDeviceConfigJson string

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				config_json
			FROM proxy_device_config
			WHERE
				client_id = $1 AND
				instance_id = $2
			`,
			clientId,
			instanceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&proxyDeviceConfigJson))
			}
		})
	})

	if proxyDeviceConfigJson == "" {
		return nil
	}

	var proxyDeviceConfig ProxyDeviceConfig
	err := json.Unmarshal([]byte(proxyDeviceConfigJson), &proxyDeviceConfig)
	if err != nil {
		return nil
	}
	return &proxyDeviceConfig
}

type connectCountry struct {
	LocationId  server.Id
	Country     string
	CountryCode string
}

// county code is lower
var countryCodeConnectCountries = sync.OnceValue(func() map[string]*connectCountry {
	ctx := context.Background()

	m := map[string]*connectCountry{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				location_id,
				location_name,
				country_code
			FROM location
			WHERE location_type = $1
			`,
			LocationTypeCountry,
		)
		server.WithPgResult(result, err, func() {
			var c connectCountry
			server.Raise(result.Scan(
				&c.LocationId,
				&c.Country,
				&c.CountryCode,
			))
		})
	})

	return m
})

func GetConnectLocationForCountryCode(ctx context.Context, countryCode string) *sdk.ConnectLocation {
	normalCountryCode := strings.ToLower(countryCode)
	c, ok := countryCodeConnectCountries()[normalCountryCode]
	if !ok {
		return nil
	}

	return &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{
			LocationId: toSdkId(c.LocationId),
		},
		Name:         c.Country,
		LocationType: sdk.LocationTypeCountry,

		Country:     c.Country,
		CountryCode: c.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  nil,
		CountryLocationId: toSdkId(c.LocationId),
	}
}

func toSdkId(id server.Id) *sdk.Id {
	sdkId, err := sdk.IdFromBytes(id.Bytes())
	if err != nil {
		panic(err)
	}
	return sdkId
}
