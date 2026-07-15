package model

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"maps"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/proxy"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

func base32Encoder() *base32.Encoding {
	return base32.HexEncoding.WithPadding(base32.NoPadding)
}

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

type ServerProxyConfig struct {
	// host -> block -> service -> port
	Hosts   map[string]map[string]map[string]int `yaml:"hosts"`
	Secrets []string                             `yaml:"secrets"`
	Wg      ServerProxyConfigWg                  `yaml:"wg"`
}

type ServerProxyConfigWg struct {
	PublicKey  string `yaml:"public_key"`
	PrivateKey string `yaml:"private_key"`
}

var LoadServerProxyConfig = sync.OnceValue(func() ServerProxyConfig {
	proxyConfigBytes := server.Vault.RequireBytes("proxy.yml")
	var proxyConfig ServerProxyConfig
	err := yaml.Unmarshal(proxyConfigBytes, &proxyConfig)
	if err != nil {
		panic(err)
	}
	return proxyConfig
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

	e := base32Encoder()
	var b []byte
	b = append(b, proxyIdBytes...)
	b = append(b, signature...)
	return e.EncodeToString(b)
}

func ParseSignedProxyId(signedProxyId string) (proxyId server.Id, returnErr error) {
	e := base32Encoder()
	b, err := e.DecodeString(strings.ToUpper(signedProxyId))

	if err != nil {
		returnErr = err
		return
	}

	if len(b) < 16 {
		returnErr = fmt.Errorf("Invalid input length")
		return
	}

	proxyId, returnErr = server.IdFromBytes(b[0:16])
	if returnErr != nil {
		return
	}
	signature := b[16:]

	// validate the signature with all known secrets
	ok := func() bool {
		for _, secret := range proxySigningSecrets() {
			h := hmac.New(sha1.New, secret)
			h.Write(b[0:16])
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

func EncodeProxyId(proxyId server.Id) string {
	proxyIdBytes := proxyId.Bytes()

	e := base32Encoder()
	var b []byte
	b = append(b, proxyIdBytes...)
	return e.EncodeToString(b)
}

func ParseEncodedProxyId(encodedProxyId string) (proxyId server.Id, returnErr error) {
	e := base32Encoder()
	b, err := e.DecodeString(strings.ToUpper(encodedProxyId))

	if err != nil {
		returnErr = err
		return
	}

	if len(b) < 16 {
		returnErr = fmt.Errorf("Invalid input length")
		return
	}

	proxyId, returnErr = server.IdFromBytes(b[0:16])
	return
}

func RequireEncodedProxyId(encodedProxyId string) server.Id {
	proxyId, err := ParseEncodedProxyId(encodedProxyId)
	if err != nil {
		panic(err)
	}
	return proxyId
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

	DnsResolverSettings *connect.DnsResolverSettings `json:"dns_resolver_settings"`
}

// the config mirror carries a ttl (pg proxy_device_config is the row of
// record and GetProxyDeviceConfig falls back to it); without one these keys
// are unevictable under volatile-ttl and accumulate per removed/idle proxy
const proxyDeviceConfigMirrorTtl = 72 * time.Hour

func proxyDeviceConfigKey(proxyId server.Id) string {
	return fmt.Sprintf("{pd_%s}c", proxyId)
}

// MigrateProxyDeviceConfig backfills the redis proxy-device-config state from
// postgres for proxies whose proxy_device_config rows predate the redis layer.
// The db is the source of truth, so existing redis keys are overwritten.
//
// Each proxy's key has its own {pd_<proxyId>} hash tag (one key per proxy), so
// the writes are issued individually rather than pipelined.
func MigrateProxyDeviceConfig(ctx context.Context, blockSize int) {
	for b := 0; true; b += 1 {
		proxyDeviceConfigJsons := map[server.Id]string{}

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					proxy_id,
					config_json
				FROM proxy_device_config
				ORDER BY proxy_id
				LIMIT $1
				OFFSET $2
				`,
				blockSize,
				b*blockSize,
			)
			server.WithPgResult(result, err, func() {
				i := 0
				for result.Next() {
					if (i+1)%1000 == 0 {
						glog.Infof("[migrate][proxy-device-config][b%d][%d/]\n", b, i+1)
					}

					var proxyId server.Id
					var configJson string
					server.Raise(result.Scan(&proxyId, &configJson))
					proxyDeviceConfigJsons[proxyId] = configJson
					i += 1
				}
			})
		})

		proxyIds := slices.Collect(maps.Keys(proxyDeviceConfigJsons))

		if len(proxyIds) == 0 {
			break
		}

		out := make(chan server.Id)
		for _, proxyId := range proxyIds {
			go server.HandleError(func() {
				defer func() {
					select {
					case <-ctx.Done():
					case out <- proxyId:
					}
				}()

				server.Redis(ctx, func(r server.RedisClient) {

					err := r.Set(ctx, proxyDeviceConfigKey(proxyId), proxyDeviceConfigJsons[proxyId], proxyDeviceConfigMirrorTtl).Err()
					server.Raise(err)

				})

			})
		}

		for i := range len(proxyIds) {
			select {
			case <-ctx.Done():
			case <-out:
				if (i+1)%10 == 0 {
					glog.Infof("[migrate][proxy-device-config][b%d][%d/%d]\n", b, i+1, len(proxyIds))
				}
			}
		}

		glog.Infof("[migrate][proxy-device-config][b%d]done (%d proxies)\n", b, len(proxyIds))
	}
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

func CreateProxyDeviceConfig(ctx context.Context, proxyDeviceConfig *ProxyDeviceConfig) (returnErr error) {

	var proxyDeviceConfigJson []byte

	server.Tx(ctx, func(tx server.PgTx) {
		proxyDeviceConfig.ProxyId = server.NewId()
		proxyDeviceConfig.InstanceId = server.NewId()

		var err error
		proxyDeviceConfigJson, err = json.Marshal(proxyDeviceConfig)
		if err != nil {
			returnErr = err
			return
		}

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
			proxyDeviceConfig.ProxyId,
			proxyDeviceConfig.ClientId,
			proxyDeviceConfig.InstanceId,
			proxyDeviceConfigJson,
		))
	})

	if returnErr != nil {
		return returnErr
	}

	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, proxyDeviceConfigKey(proxyDeviceConfig.ProxyId), proxyDeviceConfigJson, proxyDeviceConfigMirrorTtl).Err())
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

		// (cascade) remove the proxy's wg peer rows and their change rows with
		// the config, so a removed config never leaves a stale peer for the
		// instance startup restore. Orphans from any other path are caught by
		// the daily SweepOrphanNetworkClientData safety net.
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM proxy_client
			WHERE proxy_id = $1
			`,
			proxyId,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM proxy_client_change
			WHERE proxy_id = $1
			`,
			proxyId,
		))
	})

	server.Redis(ctx, func(r server.RedisClient) {
		// a silently failed Del would leave a stale mirror serving a removed
		// config for up to the mirror ttl
		server.Raise(r.Del(ctx, proxyDeviceConfigKey(proxyId)).Err())
	})
}

func GetProxyDeviceConfig(ctx context.Context, proxyId server.Id) *ProxyDeviceConfig {
	var proxyDeviceConfigJson string

	server.Redis(ctx, func(r server.RedisClient) {
		proxyDeviceConfigJson, _ = r.Get(ctx, proxyDeviceConfigKey(proxyId)).Result()
	})

	// the redis mirror carries proxyDeviceConfigMirrorTtl, so this pg fallback
	// is load-bearing for idle proxies whose mirror expired (not just legacy
	// rows from before the redis set)
	if proxyDeviceConfigJson == "" {
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
	}

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

func GetProxyDeviceConfigForClient(ctx context.Context, clientId server.Id, instanceId server.Id) *ProxyDeviceConfig {
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
			for result.Next() {
				var c connectCountry
				server.Raise(result.Scan(
					&c.LocationId,
					&c.Country,
					&c.CountryCode,
				))
				m[c.CountryCode] = &c
			}
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
			LocationId: server.ToSdkId(c.LocationId),
		},
		Name:         c.Country,
		LocationType: sdk.LocationTypeCountry,

		Country:     c.Country,
		CountryCode: c.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  nil,
		CountryLocationId: server.ToSdkId(c.LocationId),
	}
}

type ProxyClient struct {
	ChangeId       int64     `json:"change_id,omitempty"`
	CreateTime     time.Time `json:"create_time"`
	ProxyId        server.Id `json:"proxy_id"`
	ClientId       server.Id `json:"client_id"`
	InstanceId     server.Id `json:"instance_id"`
	SocksProxyUrl  string    `json:"socks_proxy_url"`
	HttpProxyUrl   string    `json:"http_proxy_url"`
	HttpsProxyUrl  string    `json:"https_proxy_url"`
	ApiBaseUrl     string    `json:"api_base_url"`
	AuthToken      string    `json:"auth_token"`
	ProxyHost      string    `json:"proxy_host"`
	Block          string    `json:"block"`
	HttpProxyPort  int       `json:"http_proxy_port"`
	HttpsProxyPort int       `json:"https_proxy_port"`
	SocksProxyPort int       `json:"socks_proxy_port"`
	ApiPort        int       `json:"api_port"`

	WgConfig *WgConfig `json:"wg_config"`
}

type WgConfig struct {
	WgProxyPort      int        `json:"wg_proxy_port"`
	ClientPrivateKey string     `json:"client_private_key"`
	ClientPublicKey  string     `json:"client_public_key"`
	ProxyPublicKey   string     `json:"proxy_public_key"`
	ClientIpv4       netip.Addr `json:"client_ipv4"`
	Config           string     `json:"config"`
}

type CreateProxyClientOptions struct {
	HttpsRequireAuth bool
	// EnableSocks / EnableWg gate the Pro-only proxy features (pro.yml features).
	// When false the client is issued no SOCKS url / no WireGuard config, so it
	// never receives credentials for a feature its plan does not include. Callers
	// derive these from model.Pro().FeatureAllowed, which folds in the
	// enforce_features rollout switch.
	EnableSocks bool
	EnableWg    bool
}

func CreateProxyClient(
	ctx context.Context,
	proxyId server.Id,
	clientId server.Id,
	instanceId server.Id,
	opts CreateProxyClientOptions,
) (
	proxyClient *ProxyClient,
	returnErr error,
) {
	proxyConfig := LoadServerProxyConfig()
	signedProxyId := SignProxyId(proxyId)

	server.Tx(ctx, func(tx server.PgTx) {
		hosts := slices.Collect(maps.Keys(proxyConfig.Hosts))
		if len(hosts) == 0 {
			returnErr = fmt.Errorf("No proxy hosts available")
			return
		}
		proxyHost := hosts[mathrand.Intn(len(hosts))]

		blockServicePorts := proxyConfig.Hosts[proxyHost]

		blocks := slices.Collect(maps.Keys(blockServicePorts))
		block := blocks[mathrand.Intn(len(blocks))]

		servicePorts := blockServicePorts[block]

		socksProxyPort := servicePorts["socks"]
		httpProxyPort := servicePorts["http"]
		httpsProxyPort := servicePorts["https"]
		apiPort := servicePorts["api"]
		wgPort := servicePorts["wg"]

		// SOCKS is a Pro-only feature. A client whose plan does not include it is
		// issued no SOCKS url, so it never gets SOCKS credentials.
		socksProxyUrl := ""
		if opts.EnableSocks {
			socksProxyUrl = fmt.Sprintf("socks5h://%s:%d", proxyHost, socksProxyPort)
		} else {
			socksProxyPort = 0
		}

		httpProxyUrl := fmt.Sprintf(
			"http://%s:%d",
			proxyHost,
			httpProxyPort,
		)

		var httpsProxyUrl string
		if opts.HttpsRequireAuth {
			// use the encoded proxy id for the url, since the signed proxy id will be passed in auth
			httpsProxyUrl = fmt.Sprintf(
				"https://%s:%d",
				proxyHost,
				httpsProxyPort,
			)
		} else {
			httpsProxyUrl = fmt.Sprintf(
				"https://%s.%s:%d",
				strings.ToLower(signedProxyId),
				proxyHost,
				httpsProxyPort,
			)
		}

		apiBaseUrl := fmt.Sprintf(
			"https://api.%s:%d",
			proxyHost,
			apiPort,
		)

		proxyClient = &ProxyClient{
			CreateTime:     server.NowUtc(),
			ProxyId:        proxyId,
			ClientId:       clientId,
			InstanceId:     instanceId,
			SocksProxyUrl:  socksProxyUrl,
			HttpProxyUrl:   httpProxyUrl,
			HttpsProxyUrl:  httpsProxyUrl,
			ApiBaseUrl:     apiBaseUrl,
			AuthToken:      signedProxyId,
			ProxyHost:      proxyHost,
			Block:          block,
			HttpProxyPort:  httpProxyPort,
			HttpsProxyPort: httpsProxyPort,
			SocksProxyPort: socksProxyPort,
			ApiPort:        apiPort,
		}

		if opts.EnableWg {

			var clientIpv4 int64

			result, err := tx.Query(
				ctx,
				`
				SELECT
					proxy_client_ipv4.client_ipv4
				FROM proxy_client_ipv4
				LEFT JOIN proxy_client ON
					proxy_client.proxy_host = $1 AND
					proxy_client.block = $2 AND
					proxy_client.client_ipv4 = proxy_client_ipv4.client_ipv4
				WHERE
					$3 <= proxy_client_ipv4.sequence_id AND
					proxy_client.client_ipv4 IS NULL
				ORDER BY proxy_client_ipv4.sequence_id
				LIMIT 1
				`,
				proxyHost,
				block,
				mathrand.Intn((31*ProxyClientIpv4Count)/32),
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&clientIpv4))
				} else {
					panic(&server.PgRetry{})
				}
			})

			clientPrivateKey, clientPublicKey, err := proxy.WgGenKeyPairStrings()
			if err != nil {
				returnErr = err
				return
			}

			proxyPublicKey := proxyConfig.Wg.PublicKey

			clientAddr := IntToIpv4(clientIpv4)

			// PersistentKeepalive keeps the client sending even when idle, so it
			// detects a dead session (e.g. proxy instance restart) and
			// re-handshakes on its own within the rekey/reject window (~2-3min).
			// It also keeps the client's NAT mapping open. Without it an idle
			// client never notices a server restart and the tunnel appears
			// permanently dead until new client traffic.
			config := fmt.Sprintf(
				`[Interface]
PrivateKey = %s
Address = %s/32
DNS = 1.1.1.1

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25`,
				clientPrivateKey,
				clientAddr,
				proxyPublicKey,
				fmt.Sprintf("%s:%d", proxyHost, wgPort),
			)

			proxyClient.WgConfig = &WgConfig{
				WgProxyPort:      wgPort,
				ClientPrivateKey: clientPrivateKey,
				ClientPublicKey:  clientPublicKey,
				ProxyPublicKey:   proxyPublicKey,
				ClientIpv4:       clientAddr,
				Config:           config,
			}
		}

		var clientIpv4 *int64
		var clientPublicKey *string
		if proxyClient.WgConfig != nil {
			b := Ipv4ToInt(proxyClient.WgConfig.ClientIpv4)
			clientIpv4 = &b
			clientPublicKey = &proxyClient.WgConfig.ClientPublicKey
		}

		proxyClientJson, err := json.Marshal(proxyClient)
		if err != nil {
			returnErr = err
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO proxy_client (
				proxy_id,
				client_id,
				instance_id,
				proxy_host,
				block,
				client_ipv4,
				client_public_key,
				proxy_client_json
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`,
			proxyId,
			clientId,
			instanceId,
			proxyClient.ProxyHost,
			proxyClient.Block,
			clientIpv4,
			clientPublicKey,
			proxyClientJson,
		))

		result, err := tx.Query(
			ctx,
			`
			INSERT INTO proxy_client_change (
				proxy_host,
            	block,
				proxy_id
			)
			VALUES ($1, $2, $3)
			RETURNING change_id
			`,
			proxyClient.ProxyHost,
			proxyClient.Block,
			proxyId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&proxyClient.ChangeId))
			}
		})

	})

	if returnErr == nil {
		server.Redis(ctx, func(r server.RedisClient) {
			r.SPublish(
				ctx,
				ProxyClientChannel(proxyClient.ProxyHost, proxyClient.Block),
				proxyId.String(),
			)
		})

		// verify egress feeder (proxy-allocated egress, sn/VALIDATOR.md §8):
		// register the allocated egress ipv4 in the bijection-gated egress
		// index. `RefreshVerifyProxyEgress` re-feeds it periodically while
		// the allocation exists, so it ages out after release (§8.2).
		if proxyClient.WgConfig != nil {
			FeedVerifyEgress(ctx, clientId, proxyClient.WgConfig.ClientIpv4, DefaultVerifySettings())
			// TODO(verify §8.2): clear egress on proxy release; currently ages
			// out via EgressTtl. There is no clean single release site to hook:
			// proxy_client rows are freed only by the bulk cascade DELETE in
			// RemoveDisconnectedNetworkClients (network_client_model.go), which
			// drops rows without loading their client_id/client_ipv4. The common
			// case (client fully reaped) is already covered — that path calls
			// RemoveVerifyEgressForClient(clientId), which clears every egress ip
			// for the client, including this one. The residual gap (a proxy_client
			// freed while its network_client persists) is bounded by the read-time
			// reverse-bijection re-check in ResolveVerifyEgress (LOW).
		}
	}

	return
}

func GetProxyClientsSince(
	ctx context.Context,
	proxyHost string,
	block string,
	changeId int64,
) (
	proxyClients map[server.Id]*ProxyClient,
	maxChangeId int64,
	returnErr error,
) {
	proxyClients = map[server.Id]*ProxyClient{}
	maxChangeId = changeId
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				proxy_client_change.change_id,
				proxy_client_change.proxy_id,
				proxy_client.proxy_client_json
			FROM proxy_client_change
			INNER JOIN proxy_client ON proxy_client.proxy_id = proxy_client_change.proxy_id
			WHERE
				proxy_client_change.proxy_host = $1 AND
				proxy_client_change.block = $2 AND
				$3 <= proxy_client_change.change_id
			`,
			proxyHost,
			block,
			changeId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var changeId int64
				var proxyId server.Id
				var proxyClientJson string
				server.Raise(result.Scan(&changeId, &proxyId, &proxyClientJson))
				maxChangeId = max(maxChangeId, changeId)

				var proxyClient ProxyClient
				err := json.Unmarshal([]byte(proxyClientJson), &proxyClient)
				if err == nil {
					proxyClients[proxyId] = &proxyClient
				} else {
					returnErr = errors.Join(returnErr, err)
				}
			}
		})
	})
	return
}

func GetProxyIdsSince(ctx context.Context, proxyHost string, block string, changeId int64) (proxyIds []server.Id, maxChangeId int64) {
	maxChangeId = changeId
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				change_id,
				proxy_id
			FROM proxy_client_change
			WHERE
				proxy_host = $1 AND
				block = $2 AND
				$3 <= change_id
			`,
			proxyHost,
			block,
			changeId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var changeId int64
				var proxyId server.Id
				server.Raise(result.Scan(&changeId, &proxyId))
				proxyIds = append(proxyIds, proxyId)
				maxChangeId = max(maxChangeId, changeId)
			}
		})
	})
	return
}

func GetProxyClient(ctx context.Context, proxyId server.Id) (proxyClient *ProxyClient, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				proxy_client.proxy_client_json
			FROM proxy_client
			WHERE proxy_client.proxy_id = $1
			`,
			proxyId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var proxyClientJson string
				server.Raise(result.Scan(&proxyClientJson))

				var proxyClient_ ProxyClient
				returnErr = json.Unmarshal([]byte(proxyClientJson), &proxyClient_)
				if returnErr == nil {
					proxyClient = &proxyClient_
				}
			}
		})
	})
	return
}

func GetProxyClients(ctx context.Context, proxyIds ...server.Id) (proxyClients map[server.Id]*ProxyClient, returnErr error) {
	proxyClients = map[server.Id]*ProxyClient{}

	server.Tx(ctx, func(tx server.PgTx) {
		server.CreateTempTableInTx(ctx, tx, "temp_proxy_id(proxy_id uuid)", proxyIds...)

		result, err := tx.Query(
			ctx,
			`
			SELECT
				proxy_client.proxy_id,
				proxy_client.proxy_client_json
			FROM proxy_client
			INNER JOIN temp_proxy_id ON temp_proxy_id.proxy_id = proxy_client.proxy_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var proxyId server.Id
				var proxyClientJson string
				server.Raise(result.Scan(&proxyId, &proxyClientJson))

				var proxyClient ProxyClient
				err := json.Unmarshal([]byte(proxyClientJson), &proxyClient)
				if err == nil {
					proxyClients[proxyId] = &proxyClient
				} else {
					returnErr = errors.Join(returnErr, err)
				}
			}
		})
	})
	return
}

func Ipv4ToInt(addr netip.Addr) int64 {
	return int64(binary.BigEndian.Uint32(addr.AsSlice()))
}

func IntToIpv4(ipv4 int64) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(ipv4))
	return netip.AddrFrom4(b)
}

// 10m per (host, block)
const ProxyClientIpv4Count = 10_000_000

// reset the entire table proxy_client_ipv4 with a randomized list of client ips that avoid popular subnets
// this generates `ProxyClientIpv4Count` ipv4s in a random order
func ResetProxyClientIpv4(ctx context.Context) {
	subnets := func(subnetStrs ...string) []netip.Prefix {
		var prefixes []netip.Prefix
		for _, subnetStr := range subnetStrs {
			prefix := netip.MustParsePrefix(subnetStr)
			prefixes = append(prefixes, prefix)
		}
		return prefixes
	}

	containsAny := func(prefixes []netip.Prefix, addr netip.Addr) bool {
		for _, prefix := range prefixes {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}

	ipv4Subnets := subnets(
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	)

	commonIpv4Subnets := subnets(
		"192.168.1.0/24",
		"192.168.0.0/24",
		"10.0.0.0/24",
		"192.168.2.0/24",
		"192.168.100.0/24",
		"192.168.86.0/24",
		"192.168.4.0/22",
		"192.168.50.0/24",
		"192.168.68.0/24",
		"192.168.85.0/24",
		"192.168.178.0/24",
		"192.168.179.0/24",
		"192.168.88.0/24",
		"192.168.8.0/24",
		"192.168.31.0/24",
		"10.8.0.0/24",
		"10.6.0.0/24",
		"100.64.0.0/10",
		"172.17.0.0/16",
		"10.252.0.0/24",
		"172.16.0.0/16",
		"192.168.10.0/24",
		"192.168.11.0/24",
		"192.168.15.0/24",
		"192.168.123.0/24",
		"192.168.254.0/24",
		"10.1.1.0/24",
		"10.1.10.0/24",
		"10.90.90.0/24",
		"192.168.168.0/24",
		"192.168.99.0/24",
		"192.168.115.0/24",
		"10.74.0.0/24",
		"172.28.0.0/24",
		"192.168.201.0/24",
	)

	var addrs []netip.Addr
	for _, prefix := range ipv4Subnets {
		for addr := range connect.AddrsInPrefix(prefix) {
			if !containsAny(commonIpv4Subnets, addr) {
				addrs = append(addrs, addr)
			}
		}
	}

	mathrand.Shuffle(len(addrs), func(i int, j int) {
		addrs[i], addrs[j] = addrs[j], addrs[i]
	})

	if len(addrs) < ProxyClientIpv4Count {
		panic(fmt.Errorf("must have at least %d ipv4 addresses (found %d)", ProxyClientIpv4Count, len(addrs)))
	}

	addrs = addrs[:ProxyClientIpv4Count]

	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			batch.Queue(
				"DELETE FROM proxy_client_ipv4",
			)

			for i, addr := range addrs {
				if (i+1)%10000 == 0 {
					glog.Infof("[reset][%d/%d]%.2f%% queued\n", i+1, len(addrs), (100.0*float64(i+1))/float64(len(addrs)))
				}
				batch.Queue(
					`
					INSERT INTO proxy_client_ipv4 (
						sequence_id,
						client_ipv4
					)
					VALUES ($1, $2)
					`,
					i,
					Ipv4ToInt(addr),
				)
			}
		})
	})

	glog.Infof("[reset][%d/%d]done\n", len(addrs), len(addrs))
}

func ProxyClientChannel(proxyHost string, block string) string {
	return fmt.Sprintf("proxy_client_%s_%s", proxyHost, block)
}
