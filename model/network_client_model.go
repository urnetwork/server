package model

import (
	"context"

	"encoding/json"
	// "crypto/sha256"
	"errors"
	// "net"
	"net/netip"
	// "regexp"
	"slices"
	"strconv"
	// "strings"
	// "sync"

	// "bytes"
	"fmt"
	"time"

	// "github.com/twmb/murmur3"
	"golang.org/x/exp/maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	// "github.com/urnetwork/server/ulid"
	// "github.com/urnetwork/server/jwt"
	"github.com/urnetwork/connect"
)

const NetworkClientHandlerHeartbeatTimeout = 60 * time.Second

// const LimitClientIdsPer24Hours = 1024
// const LimitClientIdsPerNetwork = 128
// 2025-08-29 increase this for now to allow larger providers to come online
const LimitClientIdsPerNetwork = 100000

// top-level clients (no `source_client_id`) are the network peers
// (see peer_model.go). Only top-level clients get peer subscriptions,
// so the active count per network is limited.
const LimitTopLevelClientIdsPerNetwork = 100

const MaxClientRoleCount = 32
const MaxClientRoleLength = 128
const MaxClientPrincipalLength = 256

// aligns with `protocol.ProvideMode`
type ProvideMode = int

const (
	ProvideModeDefault          ProvideMode = -1
	ProvideModeNone             ProvideMode = 0
	ProvideModeNetwork          ProvideMode = 1
	ProvideModeFriendsAndFamily ProvideMode = 2
	ProvideModePublic           ProvideMode = 3
	ProvideModeStream           ProvideMode = 4
)

// client_ids are globally unique addressess tantamount to IPv6
// they are never revoked once allocated, to preserve security and audit records
// because they are a finite resource, the number created is rate limited per network
// the total number active per network is also limited

func FindClientNetwork(
	ctx context.Context,
	clientId server.Id,
) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id
				FROM network_client
				WHERE
					client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = fmt.Errorf("Client does not exist.")
			}
		})
	})

	return
}

// FindActiveClientNetwork is `FindClientNetwork` restricted to active
// clients. The jwt refresh path uses this so a removed (inactive) or deleted
// client stops refreshing: the app sees the error and logs out instead of
// running against a dead client until the client row is reaped.
func FindActiveClientNetwork(
	ctx context.Context,
	clientId server.Id,
) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id
				FROM network_client
				WHERE
					client_id = $1 AND
					active = true
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = fmt.Errorf("Client does not exist.")
			}
		})
	})

	return
}

// FIXME source client id. if source, tag the client as ancillary and just copy the device id from the source
// FIXME get network clients to include the source network id
type AuthNetworkClientArgs struct {
	// if omitted, a new client_id is created
	ClientId       *server.Id `json:"client_id,omitempty"`
	SourceClientId *server.Id `json:"source_client_id,omitempty"`
	Description    string     `json:"description"`
	DeviceSpec     string     `json:"device_spec"`

	// identity roles and principal, assigned at creation and immutable after.
	// Only a network-level non-guest session may set these; when omitted, the
	// session's own roles and principal (e.g. from an auth code) are inherited.
	// The values have no meaning to the network.
	Roles     []string `json:"roles,omitempty"`
	Principal string   `json:"principal,omitempty"`

	ProxyConfig *ProxyConfig `json:"proxy_config,omitempty"`
}

type ProxyConfig struct {
	LockCallerIp bool     `json:"lock_caller_ip"`
	LockIpList   []string `json:"lock_ip_list"`

	HttpsRequireAuth bool `json:"https_require_auth"`
	EnableWg         bool `json:"enable_wg"`

	InitialDeviceState *ExtendedProxyDeviceState `json:"initial_device_state,omitempty"`
}

type ExtendedProxyDeviceState struct {
	ProxyDeviceState
	CountryCode string `json:"country_code,omitempty"`
}

type AuthNetworkClientResult struct {
	ByClientJwt       *string                 `json:"by_client_jwt,omitempty"`
	ClientId          *server.Id              `json:"client_id,omitempty"`
	Error             *AuthNetworkClientError `json:"error,omitempty"`
	ProxyConfigResult *ProxyConfigResult      `json:"proxy_config_result,omitempty"`
}

type AuthNetworkClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool `json:"client_limit_exceeded"`
	// the network is at its plan's limit for concurrent connected top-level
	// clients (pro.yml concurrent_clients). Unlike ClientLimitExceeded, this is
	// a plan limit rather than a hard cap: the client should surface an upgrade
	// prompt. Corresponds to HTTP 402 semantics for the caller.
	UpgradeRequired bool   `json:"upgrade_required,omitempty"`
	Message         string `json:"message"`
}

type ProxyConfigResult struct {
	KeepaliveSeconds int `json:"keepalive_seconds"`
	ProxyClient
}

type ProxyAuthResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// validateClientIdentityArgs resolves the roles and principal a new client or
// auth code is created with. Explicit values require a network-level non-guest
// session; when omitted, the session's own roles and principal are inherited.
func validateClientIdentityArgs(
	roles []string,
	principal string,
	session *session.ClientSession,
) (resolvedRoles []string, resolvedPrincipal string, message string) {
	if 0 < len(roles) || principal != "" {
		if session.ByJwt.GuestMode || session.ByJwt.ClientId != nil {
			message = "Roles and principal can only be assigned by a network session."
			return
		}
	} else {
		// inherit the session identity (e.g. a session logged in with an auth code)
		roles = session.ByJwt.Roles
		principal = session.ByJwt.Principal
	}

	if MaxClientRoleCount < len(roles) {
		message = fmt.Sprintf("Too many roles (limit %d).", MaxClientRoleCount)
		return
	}
	if MaxClientPrincipalLength < len(principal) {
		message = fmt.Sprintf("Principal too long (limit %d).", MaxClientPrincipalLength)
		return
	}
	rolesSet := map[string]bool{}
	for _, role := range roles {
		if role == "" || MaxClientRoleLength < len(role) {
			message = fmt.Sprintf("Invalid role (limit %d).", MaxClientRoleLength)
			return
		}
		rolesSet[role] = true
	}
	resolvedRoles = maps.Keys(rolesSet)
	slices.Sort(resolvedRoles)
	resolvedPrincipal = principal
	return
}

func AuthNetworkClient(
	authClient *AuthNetworkClientArgs,
	session *session.ClientSession,
) (authClientResult *AuthNetworkClientResult, authClientError error) {
	if authClient.ClientId == nil {
		roles, principal, message := validateClientIdentityArgs(authClient.Roles, authClient.Principal, session)
		if message != "" {
			authClientResult = &AuthNetworkClientResult{
				Error: &AuthNetworkClientError{
					Message: message,
				},
			}
			return
		}

		// Client-creation gate for the plan's concurrent connected-client limit:
		// Do not provision a new top-level client while the network is already at its
		// connected limit. Only top-level clients count and public providers are
		// exempt; Pro is read live rather than from the jwt's stale claim; and while
		// the rollout is dark this does no redis/db work at all. See
		// NetworkConcurrentClientsExceeded. Checked before the tx so the lookup does
		// not hold it open. Connection activation applies the same limit; see
		// CanConnectNetworkPeer.
		if authClient.SourceClientId == nil &&
			NetworkConcurrentClientsExceeded(session.Ctx, session.ByJwt.NetworkId) {
			authClientResult = &AuthNetworkClientResult{
				Error: &AuthNetworkClientError{
					ClientLimitExceeded: true,
					UpgradeRequired:     true,
					Message:             "Your plan's concurrent client limit is reached. Upgrade to UR Pro to connect more clients.",
				},
			}
			return
		}

		var clientId server.Id

		server.Tx(session.Ctx, func(tx server.PgTx) {
			createTime := server.NowUtc()

			clientId = server.NewId()
			var deviceId server.Id

			if authClient.SourceClientId == nil {
				// only top-level clients get peer subscriptions,
				// so the active count is hard limited (see peer_model.go).
				// the scan is bounded at the limit since only the threshold matters
				result, err := tx.Query(
					session.Ctx,
					`
						SELECT COUNT(*) AS top_level_client_count
						FROM (
							SELECT 1
							FROM network_client
							WHERE
								network_id = $1 AND
								active = true AND
								source_client_id IS NULL
							LIMIT $2
						) t
					`,
					session.ByJwt.NetworkId,
					LimitTopLevelClientIdsPerNetwork+1,
				)
				topLevelClientCount := 0
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&topLevelClientCount))
					}
				})
				if LimitTopLevelClientIdsPerNetwork <= topLevelClientCount {
					authClientResult = &AuthNetworkClientResult{
						Error: &AuthNetworkClientError{
							ClientLimitExceeded: true,
							Message:             "Client limit exceeded.",
						},
					}
					return
				}

				deviceId = server.NewId()

				server.RaisePgResult(tx.Exec(
					session.Ctx,
					`
						INSERT INTO device (
							device_id,
							network_id,
							device_name,
							device_spec,
							create_time
						)
						VALUES ($1, $2, $3, $4, $5)
					`,
					deviceId,
					session.ByJwt.NetworkId,
					authClient.Description,
					authClient.DeviceSpec,
					createTime,
				))
			} else {
				// copy the device id from the source
				// important: validate the source client id is in the same network
				result, err := tx.Query(
					session.Ctx,
					`
						SELECT
							device_id
						FROM network_client
						WHERE
							client_id = $1 AND
							network_id = $2
					`,
					*authClient.SourceClientId,
					session.ByJwt.NetworkId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&deviceId))
					} else {
						authClientResult = &AuthNetworkClientResult{
							Error: &AuthNetworkClientError{
								Message: "Client does not exist.",
							},
						}
						return
					}
				})
			}

			server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					INSERT INTO network_client (
						client_id,
						network_id,
						device_id,
						description,
						create_time,
						auth_time,
						source_client_id,
						principal
					)
					VALUES ($1, $2, $3, $4, $5, $5, $6, $7)
				`,
				clientId,
				session.ByJwt.NetworkId,
				deviceId,
				authClient.Description,
				createTime,
				authClient.SourceClientId,
				principal,
			))

			if 0 < len(roles) {
				server.BatchInTx(session.Ctx, tx, func(batch server.PgBatch) {
					for _, role := range roles {
						batch.Queue(
							`
							INSERT INTO network_client_role (
								client_id,
								role
							) VALUES ($1, $2)
							`,
							clientId,
							role,
						)
					}
				})
			}

			byJwtWithClientId := session.ByJwt.Client(deviceId, clientId)
			byJwtWithClientId.Roles = roles
			byJwtWithClientId.Principal = principal
			byClientJwtSigned := byJwtWithClientId.Sign()
			authClientResult = &AuthNetworkClientResult{
				ByClientJwt: &byClientJwtSigned,
				ClientId:    &clientId,
			}
		})

		if authClientResult != nil && authClientResult.Error == nil {
			setClientIdentityCache(session.Ctx, clientId, &ClientIdentity{
				Roles:     roles,
				Principal: principal,
			})
		}

		if authClientResult != nil && authClientResult.Error == nil && authClient.ProxyConfig != nil {
			var lockSubnets []netip.Prefix
			if authClient.ProxyConfig.LockCallerIp {
				addr, _, err := session.ParseClientIpPort()
				if err != nil {
					authClientError = fmt.Errorf("Could not lock caller ip")
					return
				}
				prefix, _ := addr.Prefix(addr.BitLen())
				lockSubnets = append(lockSubnets, prefix)
			}
			for _, lockIp := range authClient.ProxyConfig.LockIpList {
				addr, err := netip.ParseAddr(lockIp)
				if err == nil {
					prefix, _ := addr.Prefix(addr.BitLen())
					lockSubnets = append(lockSubnets, prefix)
				} else {
					prefix, err := netip.ParsePrefix(lockIp)
					if err != nil {
						authClientError = fmt.Errorf("Could not parse lock ip %s", lockIp)
						return
					}
					lockSubnets = append(lockSubnets, prefix)
				}
			}

			proxyDeviceState := authClient.ProxyConfig.InitialDeviceState.ProxyDeviceState
			if proxyDeviceState.Location == nil {
				// try the country code
				proxyDeviceState.Location = GetConnectLocationForCountryCode(
					session.Ctx,
					authClient.ProxyConfig.InitialDeviceState.CountryCode,
				)
			}

			if proxyDeviceState.Location == nil {
				authClientResult.Error = &AuthNetworkClientError{
					Message: "Invalid location",
				}
			} else {
				if proxyDeviceState.Location.CountryCode != "" {
					proxyDeviceState.DnsResolverSettings = connect.RegionalDnsResolverSettings(proxyDeviceState.Location.CountryCode)
				}

				proxyDeviceConfig := &ProxyDeviceConfig{
					ProxyDeviceConnection: ProxyDeviceConnection{
						ClientId: clientId,
					},
					LockSubnets:        lockSubnets,
					InitialDeviceState: &proxyDeviceState,
				}
				err := CreateProxyDeviceConfig(session.Ctx, proxyDeviceConfig)
				if err == nil {

					// SOCKS and WireGuard are Pro-only (pro.yml features): a
					// free-tier client is issued neither a SOCKS url nor a WireGuard
					// config.
					//
					// NetworkFeatureAllowed resolves Pro LIVE (never from the jwt's
					// claim, which is stale for a user who just upgraded -- handing a
					// fresh subscriber a config with no SOCKS/WireGuard until they
					// re-auth is exactly the broken upgrade we are avoiding), and it
					// short-circuits while enforce_features is dark, so today this
					// costs no lookup and every tier still gets them.
					networkId := session.ByJwt.NetworkId
					opts := CreateProxyClientOptions{
						HttpsRequireAuth: authClient.ProxyConfig.HttpsRequireAuth,
						EnableSocks:      NetworkFeatureAllowed(session.Ctx, networkId, FeatureSocksProxy),
						EnableWg: authClient.ProxyConfig.EnableWg &&
							NetworkFeatureAllowed(session.Ctx, networkId, FeatureWireguardProxy),
					}
					proxyClient, err := CreateProxyClient(
						session.Ctx,
						proxyDeviceConfig.ProxyId,
						proxyDeviceConfig.ClientId,
						proxyDeviceConfig.InstanceId,
						opts,
					)

					if err == nil {
						authClientResult.ProxyConfigResult = &ProxyConfigResult{
							ProxyClient: *proxyClient,
						}
					} else {
						authClientResult.Error = &AuthNetworkClientError{
							Message: "Could not create proxy client",
						}
					}
				} else {
					authClientResult.Error = &AuthNetworkClientError{
						Message: "Could not create proxy device",
					}
				}
			}
		}
	} else {
		// note `ProxyConfig` is ignored in this case

		// roles and principal are assigned at creation and immutable after
		if 0 < len(authClient.Roles) || authClient.Principal != "" {
			authClientResult = &AuthNetworkClientResult{
				Error: &AuthNetworkClientError{
					Message: "Roles and principal are assigned at creation and cannot be changed.",
				},
			}
			return
		}

		// important: must check `network_id = session network_id`
		server.Tx(session.Ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					UPDATE network_client
					SET
						description = $3,
						auth_time = $4
					WHERE
						client_id = $1 AND
						network_id = $2 AND
						active = true
				`,
				authClient.ClientId,
				session.ByJwt.NetworkId,
				authClient.Description,
				server.NowUtc(),
			))
			if tag.RowsAffected() == 0 {
				authClientResult = &AuthNetworkClientResult{
					Error: &AuthNetworkClientError{
						Message: "Client does not exist.",
					},
				}
				return
			}

			result, err := tx.Query(
				session.Ctx,
				`
					SELECT device_id FROM network_client
					WHERE client_id = $1
				`,
				authClient.ClientId,
			)
			var deviceId *server.Id
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var deviceIdValue server.Id
					server.Raise(result.Scan(&deviceIdValue))
					deviceId = &deviceIdValue
				}
			})

			if deviceId == nil {
				authClientResult = &AuthNetworkClientResult{
					Error: &AuthNetworkClientError{
						Message: "Client needs to be migrated (support@ur.io).",
					},
				}
				return
			}

			tag = server.RaisePgResult(tx.Exec(
				session.Ctx,
				`
					UPDATE device
					SET
						device_spec = $2
					WHERE
						device_id = $1
				`,
				deviceId,
				authClient.DeviceSpec,
			))
			if tag.RowsAffected() == 0 {
				authClientResult = &AuthNetworkClientResult{
					Error: &AuthNetworkClientError{
						Message: "Device does not exist.",
					},
				}
				return
			}

			// the client jwt carries the client's stored identity
			var principal string
			roles := []string{}
			result, err = tx.Query(
				session.Ctx,
				`
					SELECT principal FROM network_client
					WHERE client_id = $1
				`,
				authClient.ClientId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&principal))
				}
			})
			result, err = tx.Query(
				session.Ctx,
				`
					SELECT role FROM network_client_role
					WHERE client_id = $1
					ORDER BY role
				`,
				authClient.ClientId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var role string
					server.Raise(result.Scan(&role))
					roles = append(roles, role)
				}
			})

			byJwtWithClientId := session.ByJwt.Client(*deviceId, *authClient.ClientId)
			byJwtWithClientId.Roles = roles
			byJwtWithClientId.Principal = principal
			byClientJwtSigned := byJwtWithClientId.Sign()
			authClientResult = &AuthNetworkClientResult{
				ByClientJwt: &byClientJwtSigned,
				ClientId:    authClient.ClientId,
			}
		})
	}

	return
}

type RemoveNetworkClientArgs struct {
	ClientId server.Id `json:"client_id"`
}

type RemoveNetworkClientResult struct {
	Error *RemoveNetworkClientError `json:"error,omitempty"`
}

type RemoveNetworkClientError struct {
	Message string `json:"message"`
}

func RemoveNetworkClient(
	removeClient *RemoveNetworkClientArgs,
	session *session.ClientSession,
) (*RemoveNetworkClientResult, error) {
	var removeClientResult *RemoveNetworkClientResult
	var removeClientErr error

	// important: must check `network_id = session network_id`
	server.Tx(session.Ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			session.Ctx,
			`
				UPDATE network_client
				SET
					active = false,
					deactivate_time = $3
				WHERE
					client_id = $1 AND
					network_id = $2
			`,
			removeClient.ClientId,
			session.ByJwt.NetworkId,
			server.NowUtc(),
		)
		server.Raise(err)
		if tag.RowsAffected() != 1 {
			removeClientResult = &RemoveNetworkClientResult{
				Error: &RemoveNetworkClientError{
					Message: "Client does not exist.",
				},
			}
			return
		}

		removeClientResult = &RemoveNetworkClientResult{}
	})

	return removeClientResult, removeClientErr
}

type NetworkClientsResult struct {
	Clients []*NetworkClientInfo `json:"clients"`
}

type NetworkClientInfo struct {
	// ClientId server.Id `json:"client_id"`
	// NetworkId server.Id `json:"network_id"`
	// Description string `json:"description"`
	// DeviceSpec string `json:"device_spec"`

	// CreateTime time.Time `json:"create_time"`
	// AuthTime time.Time `json:"auth_time"`

	// InstanceId server.Id `json:"client_id"`
	// ResidentId server.Id `json:"resident_id"`
	// ResidentHost string `json:"resident_host"`
	// ResidentService string `json:"resident_service"`
	// ResidentBlock string `json:"resident_block"`
	// ResidentInternalPorts []int `json:"resident_internal_ports"`

	NetworkClient

	Resident *NetworkClientResident `json:"resident,omitempty"`

	Connections []*NetworkClientConnection `json:"connections,omitempty"`
}

type NetworkClientConnection struct {
	ClientId          server.Id  `json:"client_id"`
	ConnectionId      server.Id  `json:"connection_id"`
	ConnectTime       time.Time  `json:"connect_time"`
	DisconnectTime    *time.Time `json:"disconnect_time,omitempty"`
	ConnectionHost    string     `json:"connection_host"`
	ConnectionService string     `json:"connection_service"`
	ConnectionBlock   string     `json:"connection_block"`
}

func GetNetworkClients(session *session.ClientSession) (*NetworkClientsResult, error) {
	var clientsResult *NetworkClientsResult
	var clientsErr error

	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					network_client.client_id,
					network_client.source_client_id,
					network_client.description,
					network_client.device_id,
					device.device_name,
					device.device_spec,
					network_client.create_time,
					network_client.auth_time,
					network_client.principal,
					provide_key.provide_mode,
					proxy_client.proxy_client_json
				FROM network_client
				LEFT JOIN provide_key ON
					provide_key.client_id = network_client.client_id AND
					provide_key.provide_mode = $2
				LEFT JOIN device ON
					device.device_id = network_client.device_id
				LEFT JOIN proxy_client ON
					proxy_client.client_id = network_client.client_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
			`,
			session.ByJwt.NetworkId,
			ProvideModePublic,
		)
		clientInfos := map[server.Id]*NetworkClientInfo{}
		server.WithPgResult(result, err, func() {

			for result.Next() {
				clientInfo := &NetworkClientInfo{}
				var deviceName_ *string
				var deviceSpec_ *string
				var proxyClientJson *string
				server.Raise(result.Scan(
					&clientInfo.ClientId,
					&clientInfo.SourceClientId,
					&clientInfo.Description,
					&clientInfo.DeviceId,
					&deviceName_,
					&deviceSpec_,
					&clientInfo.CreateTime,
					&clientInfo.AuthTime,
					&clientInfo.Principal,
					&clientInfo.ProvideMode,
					&proxyClientJson,
				))
				if deviceName_ != nil {
					clientInfo.DeviceName = *deviceName_
				}
				if deviceSpec_ != nil {
					clientInfo.DeviceSpec = *deviceSpec_
				}
				if proxyClientJson != nil {
					var proxyClient ProxyClient
					err := json.Unmarshal([]byte(*proxyClientJson), &proxyClient)
					if err == nil {
						clientInfo.ProxyClient = &proxyClient
					}
				}
				clientInfos[clientInfo.ClientId] = clientInfo
			}
		})

		result, err = conn.Query(
			session.Ctx,
			`
				SELECT
					network_client_role.client_id,
					network_client_role.role
				FROM network_client
				INNER JOIN network_client_role ON
					network_client_role.client_id = network_client.client_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
				ORDER BY network_client_role.role
			`,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var role string
				server.Raise(result.Scan(&clientId, &role))
				if clientInfo, ok := clientInfos[clientId]; ok {
					clientInfo.Roles = append(clientInfo.Roles, role)
				}
			}
		})

		result, err = conn.Query(
			session.Ctx,
			`
				SELECT
					network_client.client_id,
					network_client_connection.connection_id,
					network_client_connection.connect_time,
					network_client_connection.disconnect_time,
					network_client_connection.connection_host,
					network_client_connection.connection_service,
					network_client_connection.connection_block
				FROM network_client
				INNER JOIN network_client_connection ON
					network_client.client_id = network_client_connection.client_id AND
					network_client_connection.connected
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
			`,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				clientConnection := &NetworkClientConnection{}
				server.Raise(result.Scan(
					&clientConnection.ClientId,
					&clientConnection.ConnectionId,
					&clientConnection.ConnectTime,
					&clientConnection.DisconnectTime,
					&clientConnection.ConnectionHost,
					&clientConnection.ConnectionService,
					&clientConnection.ConnectionBlock,
				))
				if clientInfo, ok := clientInfos[clientConnection.ClientId]; ok {
					clientInfo.Connections = append(clientInfo.Connections, clientConnection)
				}
				// else read appears to be inconsistent
			}
		})

		clientsResult = &NetworkClientsResult{
			Clients: maps.Values(clientInfos),
		}
	})

	if clientsResult != nil && 0 < len(clientsResult.Clients) {
		keys := []string{}
		for _, clientInfo := range clientsResult.Clients {
			keys = append(keys, pendingClientConnectionKey(clientInfo.ClientId))
		}
		server.Redis(session.Ctx, func(r server.RedisClient) {
			unixMilliStrs, err := r.MGet(session.Ctx, keys...).Result()
			if err != nil {
				clientsErr = err
				return
			}
			for i, clientInfo := range clientsResult.Clients {
				clientId := clientInfo.ClientId
				if unixMilliStrs[i] != nil {
					unixMilliStr := unixMilliStrs[i].(string)
					unixMilli, err := strconv.ParseInt(unixMilliStr, 10, 64)
					if err == nil {
						connectTime := time.UnixMilli(unixMilli)
						pendingClientConnection := &NetworkClientConnection{
							ClientId:     clientId,
							ConnectionId: clientId,
							ConnectTime:  connectTime,
						}
						clientInfo.Connections = append(clientInfo.Connections, pendingClientConnection)
					}
				}
			}
		})
	}

	return clientsResult, clientsErr
}

func pendingClientConnectionKey(clientId server.Id) string {
	return fmt.Sprintf("{pending_client_connection}_%s", clientId)
}

func SetPendingNetworkClientConnection(ctx context.Context, clientId server.Id, expire time.Duration) {
	server.Redis(ctx, func(r server.RedisClient) {
		unixMilliStr := strconv.FormatInt(server.NowUtc().UnixMilli(), 10)
		r.Set(
			ctx,
			pendingClientConnectionKey(clientId),
			unixMilliStr,
			expire,
		)
	})
}

type NetworkClient struct {
	ClientId       server.Id  `json:"client_id"`
	SourceClientId *server.Id `json:"source_client_id,omitempty"`
	DeviceId       server.Id  `json:"device_id"`
	NetworkId      server.Id  `json:"network_id"`
	Description    string     `json:"description"`
	DeviceName     string     `json:"device_name"`
	DeviceSpec     string     `json:"device_spec"`

	CreateTime time.Time `json:"create_time"`
	AuthTime   time.Time `json:"auth_time"`

	// identity roles and principal assigned at creation
	Roles     []string `json:"roles,omitempty"`
	Principal string   `json:"principal,omitempty"`

	ProvideMode *ProvideMode `json:"provide_mode,omitempty"`
	ProxyClient *ProxyClient `json:"proxy_client,omitempty"`
}

func GetNetworkClient(ctx context.Context, clientId server.Id) *NetworkClient {
	var networkClient *NetworkClient

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client.network_id,
					network_client.description,
					device.device_name,
					device.device_spec,
					network_client.create_time,
					network_client.auth_time,
					network_client.principal,
					provide_key.provide_mode,
					proxy_client.proxy_client_json
				FROM network_client
				LEFT JOIN provide_key ON
					provide_key.client_id = network_client.client_id AND
					provide_key.provide_mode = $2
				LEFT JOIN device ON
					device.device_id = network_client.device_id
				LEFT JOIN proxy_client ON
					proxy_client.client_id = network_client.client_id
				WHERE
					network_client.client_id = $1 AND
					network_client.active = true
			`,
			clientId,
			ProvideModePublic,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				networkClient = &NetworkClient{
					ClientId: clientId,
				}
				var deviceName_ *string
				var deviceSpec_ *string
				var proxyClientJson *string
				server.Raise(result.Scan(
					&networkClient.NetworkId,
					&networkClient.Description,
					&deviceName_,
					&deviceSpec_,
					&networkClient.CreateTime,
					&networkClient.AuthTime,
					&networkClient.Principal,
					&networkClient.ProvideMode,
					&proxyClientJson,
				))
				if deviceName_ != nil {
					networkClient.DeviceName = *deviceName_
				}
				if deviceSpec_ != nil {
					networkClient.DeviceSpec = *deviceSpec_
				}
				if proxyClientJson != nil {
					var proxyClient ProxyClient
					err := json.Unmarshal([]byte(*proxyClientJson), &proxyClient)
					if err == nil {
						networkClient.ProxyClient = &proxyClient
					}
				}
			}
		})

		if networkClient == nil {
			return
		}

		result, err = conn.Query(
			ctx,
			`
				SELECT role FROM network_client_role
				WHERE client_id = $1
				ORDER BY role
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var role string
				server.Raise(result.Scan(&role))
				networkClient.Roles = append(networkClient.Roles, role)
			}
		})
	})

	return networkClient
}

func GetNetworkClientNetwork(ctx context.Context, clientId server.Id) (networkId *server.Id) {
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
				var networkId_ server.Id
				server.Raise(result.Scan(&networkId_))
				networkId = &networkId_
			}
		})
	})
	return
}

func GetProvideRelationship(ctx context.Context, clientIdA server.Id, clientIdB server.Id) ProvideMode {
	if clientIdA == clientIdB {
		return ProvideModeNetwork
	}

	sameNetwork := false

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				a.network_id,
				b.network_id
			FROM network_client a
			INNER JOIN network_client b ON b.client_id = $2
			WHERE a.client_id = $1
			`,
			clientIdA,
			clientIdB,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var networkIdA server.Id
				var networkIdB server.Id
				server.Raise(result.Scan(&networkIdA, &networkIdB))
				if networkIdA == networkIdB {
					sameNetwork = true
				}
			}
		})
	})

	if sameNetwork {
		return ProvideModeNetwork
	}

	// TODO network and friends-and-family not implemented yet
	// FIXME these exist in the association model now, can be added

	return ProvideModePublic
}

// the roles and identity principal assigned to a client at creation.
// The values have no meaning to the network.
type ClientIdentity struct {
	Roles     []string `json:"roles,omitempty"`
	Principal string   `json:"principal,omitempty"`
}

// note this shares the {pm_<clientId>} hash tag with the provide mode keys
func clientIdentityKey(clientId server.Id) string {
	return fmt.Sprintf("{pm_%s}rp", clientId)
}

func setClientIdentityCache(ctx context.Context, clientId server.Id, identity *ClientIdentity) {
	identityJson, err := json.Marshal(identity)
	if err != nil {
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		// no ttl; the identity is immutable post-create
		r.Set(ctx, clientIdentityKey(clientId), identityJson, server.NoTtl)
	})
}

// GetClientIdentity returns the roles and principal assigned to the client at
// creation. Redis first with a db fallback that fills the cache (the identity
// is immutable post-create). The empty identity is cached too, since most
// clients have no roles or principal.
func GetClientIdentity(ctx context.Context, clientId server.Id) (identity *ClientIdentity) {
	server.Redis(ctx, func(r server.RedisClient) {
		identityJson, _ := r.Get(ctx, clientIdentityKey(clientId)).Result()
		if identityJson == "" {
			// not in redis; fall back to the db below
			return
		}
		var identity_ ClientIdentity
		if err := json.Unmarshal([]byte(identityJson), &identity_); err == nil {
			identity = &identity_
		}
	})
	if identity != nil {
		return
	}

	identity = &ClientIdentity{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT principal FROM network_client
				WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&identity.Principal))
			}
		})

		result, err = conn.Query(
			ctx,
			`
				SELECT role FROM network_client_role
				WHERE client_id = $1
				ORDER BY role
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var role string
				server.Raise(result.Scan(&role))
				identity.Roles = append(identity.Roles, role)
			}
		})
	})
	setClientIdentityCache(ctx, clientId, identity)
	return
}

func provideModesKey(clientId server.Id) string {
	return fmt.Sprintf("{pm_%s}pms", clientId)
}

func provideModeSecretKeyKey(clientId server.Id, provideMode ProvideMode) string {
	return fmt.Sprintf("{pm_%s}sk_%d", clientId, provideMode)
}

// MigrateProvideMode backfills the redis provide-key state from postgres for
// clients whose provide_key rows predate the redis layer. The db is the source
// of truth, so existing redis keys are overwritten.
//
// All of a client's keys share the {pm_<clientId>} hash tag and so can be
// written in a single pipeline; keys for different clients live in different
// slots and must not share a pipeline.
func MigrateProvideMode(ctx context.Context, blockSize int) {
	for b := 0; true; b += 1 {
		clientSecretKeys := map[server.Id]map[ProvideMode][]byte{}

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					provide_key.client_id,
					provide_key.provide_mode,
					provide_key.secret_key
				FROM provide_key
				INNER JOIN (
					SELECT
						DISTINCT client_id
					FROM provide_key
					ORDER BY client_id
					LIMIT $1
					OFFSET $2
				) t ON t.client_id = provide_key.client_id
				`,
				blockSize,
				b*blockSize,
			)
			server.WithPgResult(result, err, func() {
				i := 0
				for result.Next() {
					if (i+1)%1000 == 0 {
						glog.Infof("[migrate][provide-mode][b%d][%d/]\n", b, i+1)
					}

					var clientId server.Id
					var provideMode ProvideMode
					var secretKey []byte
					server.Raise(result.Scan(&clientId, &provideMode, &secretKey))
					secretKeys, ok := clientSecretKeys[clientId]
					if !ok {
						secretKeys = map[ProvideMode][]byte{}
						clientSecretKeys[clientId] = secretKeys
					}
					secretKeys[provideMode] = secretKey
					i += 1
				}
			})
		})

		clientIds := maps.Keys(clientSecretKeys)

		if len(clientIds) == 0 {
			break
		}

		out := make(chan server.Id)

		for _, clientId := range clientIds {
			go server.HandleError(func() {
				defer func() {
					select {
					case <-ctx.Done():
					case out <- clientId:
					}
				}()

				server.Redis(ctx, func(r server.RedisClient) {

					secretKeys := clientSecretKeys[clientId]

					// all keys share the {pm_<clientId>} hash tag
					pipe := r.TxPipeline()

					provideModesList := maps.Keys(secretKeys)
					provideModesListJson, _ := json.Marshal(provideModesList)
					// no ttl
					pipe.Set(ctx, provideModesKey(clientId), provideModesListJson, server.NoTtl)

					for provideMode, secretKey := range secretKeys {
						// no ttl
						pipe.Set(ctx, provideModeSecretKeyKey(clientId, provideMode), secretKey, server.NoTtl)
					}

					_, err := pipe.Exec(ctx)
					server.Raise(err)

				})

			})

		}

		for i := range len(clientIds) {
			select {
			case <-ctx.Done():
			case <-out:
				if (i+1)%10 == 0 {
					glog.Infof("[migrate][provide-mode][b%d][%d/%d]\n", b, i+1, len(clientIds))
				}
			}
		}

		glog.Infof("[migrate][provide-mode][b%d]done (%d clients)\n", b, len(clientIds))
	}
}

func GetProvideModes(ctx context.Context, clientId server.Id) (provideModes map[ProvideMode]bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		provideModesListJson, _ := r.Get(ctx, provideModesKey(clientId)).Result()
		if provideModesListJson == "" {
			// not in redis; fall back to the db below
			return
		}
		var provideModesList []ProvideMode
		err := json.Unmarshal([]byte(provideModesListJson), &provideModesList)
		if err != nil {
			returnErr = err
			return
		}
		provideModes = map[ProvideMode]bool{}
		for _, provideMode := range provideModesList {
			provideModes[provideMode] = true
		}
	})

	// TODO this can be removed once provide_key older than the redis set have been removed
	if provideModes == nil && returnErr == nil {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT provide_mode FROM provide_key
					WHERE client_id = $1
				`,
				clientId,
			)
			server.WithPgResult(result, err, func() {
				provideModes = map[ProvideMode]bool{}
				for result.Next() {
					var provideMode ProvideMode
					server.Raise(result.Scan(&provideMode))
					provideModes[provideMode] = true
				}
			})
		})
	}

	return
}

func GetProvideSecretKey(
	ctx context.Context,
	clientId server.Id,
	provideMode ProvideMode,
) (secretKey []byte, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		secretKeyStr, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, provideMode)).Result()
		if secretKeyStr != "" {
			secretKey = []byte(secretKeyStr)
		}
		// otherwise leave secretKey nil and fall back to the db below
	})

	// TODO this can be removed once provide_key older than the redis set have been removed
	if secretKey == nil && returnErr == nil {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT
						secret_key
					FROM provide_key
					WHERE
						client_id = $1 AND
						provide_mode = $2
				`,
				clientId,
				provideMode,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&secretKey))
				} else {
					returnErr = fmt.Errorf("Provide secret key not set.")
				}
			})
		})
	}

	return
}

// GetClientTlsCertificateAndSignature returns the published TLS cert chain
// (concatenated PEM, leaf first) and the client's Ed25519 signature over it.
// Either may be nil: not-published yields both nil; a client pre-dating
// client-key signing yields a cert and nil signature.
func GetClientTlsCertificateAndSignature(
	ctx context.Context,
	clientId server.Id,
) (tlsCertificatePem []byte, clientKeySignedTlsCertificate []byte, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					tls_certificate_pem,
					client_key_signed_tls_certificate
				FROM client_tls_certificate
				WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&tlsCertificatePem, &clientKeySignedTlsCertificate))
			}
		})
	})
	return
}

// SetClientTlsCertificateWithSignature stores the PEM cert chain and the
// client's signature over it (by its long-lived identity key), published via
// `EncryptedKey`. An empty/nil chain clears both. A non-empty chain with a nil
// signature is allowed (older clients that don't sign yet).
func SetClientTlsCertificateWithSignature(
	ctx context.Context,
	clientId server.Id,
	tlsCertificatePem []byte,
	clientKeySignedTlsCertificate []byte,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		if 0 < len(tlsCertificatePem) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO client_tls_certificate (
					client_id,
					tls_certificate_pem,
					client_key_signed_tls_certificate,
					set_time
				) VALUES ($1, $2, $3, $4)
				ON CONFLICT (client_id) DO UPDATE
				SET tls_certificate_pem = EXCLUDED.tls_certificate_pem,
				    client_key_signed_tls_certificate = EXCLUDED.client_key_signed_tls_certificate,
				    set_time = EXCLUDED.set_time
				`,
				clientId,
				tlsCertificatePem,
				clientKeySignedTlsCertificate,
				server.NowUtc(),
			))
		} else {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM client_tls_certificate
				WHERE client_id = $1
				`,
				clientId,
			))
		}
	})
}

func SetProvide(
	ctx context.Context,
	clientId server.Id,
	secretKeys map[ProvideMode][]byte,
) {
	var removedProvideModes []ProvideMode

	server.Tx(ctx, func(tx server.PgTx) {
		// reset in case the tx is retried on a transient error
		removedProvideModes = nil

		changeTime := server.NowUtc()

		result, err := tx.Query(
			ctx,
			`
			DELETE FROM provide_key
			WHERE client_id = $1
			RETURNING provide_key.provide_mode
			`,
			clientId,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var provideMode ProvideMode
				server.Raise(result.Scan(&provideMode))
				removedProvideModes = append(removedProvideModes, provideMode)
			}
		})

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for provideMode, secretKey := range secretKeys {
				batch.Queue(
					`
					INSERT INTO provide_key (
						client_id,
						provide_mode,
						secret_key
					)
					VALUES ($1, $2, $3)
					`,
					clientId,
					provideMode,
					secretKey,
				)
			}
		})

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provide_key_change (
				client_id,
				change_time
			) VALUES ($1, $2)
			`,
			clientId,
			changeTime,
		))

	})

	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()

		provideModesList := maps.Keys(secretKeys)
		provideModesListJson, _ := json.Marshal(provideModesList)
		// no ttl
		pipe.Set(ctx, provideModesKey(clientId), provideModesListJson, server.NoTtl)

		for provideMode, secretKey := range secretKeys {
			// no ttl
			pipe.Set(ctx, provideModeSecretKeyKey(clientId, provideMode), secretKey, server.NoTtl)
		}
		for _, provideMode := range removedProvideModes {
			if _, ok := secretKeys[provideMode]; !ok {
				pipe.Del(ctx, provideModeSecretKeyKey(clientId, provideMode))
			}
		}

		_, err := pipe.Exec(ctx)
		server.Raise(err)
	})

	// update the peer registry so connected network peers see the change
	provideModes := map[ProvideMode]bool{}
	for provideMode := range secretKeys {
		provideModes[provideMode] = true
	}
	UpdateNetworkPeerProvideModes(ctx, clientId, provideModes)
}

func GetProvideKeyChanges(
	ctx context.Context,
	clientId server.Id,
	minTime time.Time,
) (
	changedCount int,
	provideModes map[ProvideMode]bool,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
			SELECT
				COUNT(*) AS changed_count
			FROM provide_key_change
			WHERE
				client_id = $1 AND
				$2 <= change_time
			`,
			clientId,
			minTime,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&changedCount))
			}
		})
	})

	provideModes, _ = GetProvideModes(ctx, clientId)

	return
}

func RemoveOldProvideKeyChanges(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM provide_key_change
			WHERE change_time < $1
			`,
			minTime.UTC(),
		))
	})
}

func IsIpConnectedToNetwork(
	ctx context.Context,
	clientIp string,
) bool {
	addressHash, err := server.ClientIpHash(clientIp)
	if err != nil {
		return false
	}

	connected := false

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT true FROM network_client_connection
				WHERE client_address_hash = $1 AND connected
				LIMIT 1
			`,
			addressHash[:],
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&connected))
			}
		})
	})

	return connected

}

// a client_id can have multiple connections to the platform
// each connection forms a transmit for the resident transport
// there is one resident transport
// if connect to the resident transport fails,
// attempt claim local resident and start resident locally
// if attempt claim fails, connect to the next (repeat until a successful connection)

// returns a connection_id
func ConnectNetworkClient(
	ctx context.Context,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
) (
	connectionId server.Id,
	clientIp string,
	clientPort int,
	clientIpHash [32]byte,
	err error,
) {
	clientIp, clientPort, err = server.SplitClientAddress(clientAddress)
	if err != nil {
		return
	}

	clientIpHash, err = server.ClientIpHash(clientIp)
	if err != nil {
		return
	}

	var expectedLatencyMillis int
	if ipInfo, err := server.GetIpInfoFromString(clientIp); err == nil {
		hostLatitude, hostLongitude := server.HostLatituteLongitude()
		distanceMillis := server.DistanceMillis(
			hostLatitude,
			hostLongitude,
			ipInfo.Latitude,
			ipInfo.Longitude,
		)
		expectedLatencyMillis = int(2.5*distanceMillis + 0.5)
	}

	server.Tx(ctx, func(tx server.PgTx) {
		connectionId = server.NewId()
		connectTime := server.NowUtc()

		host, _ := server.Host()
		service, _ := server.Service()
		block, _ := server.Block()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_connection (
					client_id,
					connection_id,
					connect_time,
					connection_host,
					connection_service,
					connection_block,
					client_address_hash,
					client_address_port,
					handler_id,
					expected_latency_ms
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`,
			clientId,
			connectionId,
			connectTime,
			host,
			service,
			block,
			clientIpHash[:],
			clientPort,
			handlerId,
			expectedLatencyMillis,
		))

		// refresh auth_time as a durable last-seen marker. connection rows are
		// retained only briefly by `RemoveDisconnectedNetworkClients`, so the
		// disconnected-client reap keys off auth_time to mean "not seen for the
		// client window" rather than "created long ago".
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_client
				SET auth_time = $2
				WHERE client_id = $1
			`,
			clientId,
			connectTime,
		))
	})

	return
}

func DisconnectNetworkClient(ctx context.Context, connectionId server.Id) error {
	var disconnectErr error

	server.Tx(ctx, func(tx server.PgTx) {
		disconnectTime := server.NowUtc()
		tag, err := tx.Exec(
			ctx,
			`
				UPDATE network_client_connection
				SET
					connected = false,
					disconnect_time = $2
				WHERE
					connection_id = $1
			`,
			connectionId,
			disconnectTime,
		)
		server.Raise(err)
		if tag.RowsAffected() != 1 {
			disconnectErr = errors.New("Connection does not exist.")
			return
		}
	})

	return disconnectErr
}

// `minConnectionTime` bounds how long disconnected connection rows are
// retained. `minClientTime` bounds when disconnected clients are reaped —
// a client is reaped only if it has not authed or connected since
// `minClientTime` (see the auth_time refresh in `ConnectNetworkClient`).
// Keep the client window much larger than the connection window: provisioned
// an inactive client is reaped this long after it was deactivated (user
// removal or the idle top-level marker), giving a notice window before the
// hard delete and its cascades
const NetworkClientReapAfterDeactivate = 30 * 24 * time.Hour

// a top-level client (source_client_id IS NULL) that has not authed or
// connected for this long is abandoned: it is marked inactive, which makes
// the jwt refresh fail so the app logs the user out (see
// FindActiveClientNetwork), and the reap hard deletes it
// `NetworkClientReapAfterDeactivate` later. A returning user logs in again
// with a fresh client id.
const TopLevelClientIdleExpiration = 90 * 24 * time.Hour

// child clients (e.g. proxy devices, see `proxy_device_config`) cannot
// recover from a reaped client_id.
func RemoveDisconnectedNetworkClients(ctx context.Context, minConnectionTime time.Time, minClientTime time.Time, minTopLevelAuthTime time.Time) {
	// remove old disconnected connections in bounded batches, cascading
	// network_client_location/latency/speed for the same connection ids in one
	// statement. The per-connection tables are keyed by connection_id, so the
	// cascade is a set of pk probes instead of the full-table anti-joins this
	// used to do (see SweepOrphanNetworkClientData for the safety net).
	// keep batches bounded so no single tx holds locks for long
	removeConnectionBatchCount := 50000
	for {
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				WITH candidate AS (
					SELECT connection_id
					FROM network_client_connection
					WHERE
						connected = false AND
						disconnect_time < $1
					LIMIT $2
				), deleted_location AS (
					DELETE FROM network_client_location
					USING candidate
					WHERE network_client_location.connection_id = candidate.connection_id
				), deleted_latency AS (
					DELETE FROM network_client_latency
					USING candidate
					WHERE network_client_latency.connection_id = candidate.connection_id
				), deleted_speed AS (
					DELETE FROM network_client_speed
					USING candidate
					WHERE network_client_speed.connection_id = candidate.connection_id
				)
				DELETE FROM network_client_connection
				USING candidate
				WHERE network_client_connection.connection_id = candidate.connection_id
				`,
				minConnectionTime.UTC(),
				removeConnectionBatchCount,
			))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		if batchCount < int64(removeConnectionBatchCount) {
			break
		}
	}

	// mark abandoned top-level clients inactive in bounded batches: no auth or
	// connect since `minTopLevelAuthTime` (auth_time refreshes on both) and no
	// live connection. Marking makes the jwt refresh fail so the app logs the
	// user out; the reap below hard deletes the row
	// `NetworkClientReapAfterDeactivate` after deactivate_time.
	markTopLevelBatchCount := 10000
	for {
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_client
				SET active = false, deactivate_time = $2
				WHERE client_id IN (
					SELECT network_client.client_id
					FROM network_client
					LEFT JOIN network_client_connection ON
						network_client_connection.client_id = network_client.client_id AND
						network_client_connection.connected = true
					WHERE
						network_client.active = true AND
						network_client.source_client_id IS NULL AND
						network_client.auth_time < $1 AND
						network_client_connection.client_id IS NULL
					LIMIT $3
				)
				`,
				minTopLevelAuthTime.UTC(),
				server.NowUtc(),
				markTopLevelBatchCount,
			))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		if 0 < batchCount {
			glog.Infof("[ncm]marked %d abandoned top level clients inactive\n", batchCount)
		}
		if batchCount < int64(markTopLevelBatchCount) {
			break
		}
	}

	// Capture the deleted client_ids so we can sweep their identity keys from
	// redis below, and target the dependent-table cascades (provide_key,
	// proxy_device_config, client_tls_certificate, device) at exactly the
	// reaped ids instead of full-table anti-joins. device_ids are captured for
	// the device cascade, which re-checks that no other client still
	// references the device.
	var reapedClientIds []server.Id
	var reapedDeviceIds []server.Id
	collectReaped := func(rows server.PgResult, err error) {
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				var deviceId *server.Id
				server.Raise(rows.Scan(&clientId, &deviceId))
				reapedClientIds = append(reapedClientIds, clientId)
				if deviceId != nil {
					reapedDeviceIds = append(reapedDeviceIds, *deviceId)
				}
			}
		})
	}

	// inactive clients reap `NetworkClientReapAfterDeactivate` after their
	// deactivate_time; rows deactivated before that column existed (NULL)
	// fall back to create_time, which was the previous behavior
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		rows, err := tx.Query(
			ctx,
			`
			DELETE FROM network_client
			WHERE COALESCE(network_client.deactivate_time, network_client.create_time) < $1 AND active = false
			RETURNING client_id, device_id
			`,
			minClientTime.UTC(),
		)
		collectReaped(rows, err)

	}, server.TxReadCommitted)

	// remove network clients with a parent, not seen since `minClientTime`,
	// and without a connection. auth_time (not create_time) is the reap key:
	// it is refreshed on every auth and every connect, so a client in regular
	// use is never reaped no matter how old it is.
	// important: to delete clients without a source id (top level clients),
	//            the app will need to create a new client id for these clients when it notices the existing jwt fails
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		rows, err := tx.Query(
			ctx,
			`
			DELETE FROM network_client
			USING (
				SELECT network_client.client_id
				FROM network_client
			    	LEFT JOIN network_client_connection ON network_client_connection.client_id = network_client.client_id
				WHERE
					network_client.auth_time < $1
					AND network_client.source_client_id IS NOT NULL
					AND network_client_connection.client_id IS NULL
			) t
			WHERE network_client.client_id = t.client_id
			RETURNING network_client.client_id, network_client.device_id
			`,
			minClientTime.UTC(),
		)
		collectReaped(rows, err)
	})

	// Sweep per-client redis state for each reaped client_id. Outside the DB tx
	// since redis isn't transactional with Postgres; a failure just leaves keys
	// until the next sweep or overwrite.
	for _, clientId := range reapedClientIds {
		RemoveClientPublicKey(ctx, clientId)
		// clear the reaped client's verify egress index entries so a
		// reassigned ip is never miscredited (sn/VALIDATOR.md §8.2)
		RemoveVerifyEgressForClient(ctx, clientId)
	}

	// (cascade) the dependent tables are all keyed by the reaped ids, so the
	// cascades below are targeted deletes on those ids (chunked to bound
	// statement size) instead of full-table anti-joins over provide_key,
	// proxy_device_config, client_tls_certificate, and device. Orphans created
	// by any other path are caught by the low-cadence
	// SweepOrphanNetworkClientData safety net.
	removedProxyIds := removeProxyDeviceConfigsForClientIds(ctx, reapedClientIds)
	removeProxyClientData(ctx, removedProxyIds)
	removeProvideKeysForClientIds(ctx, reapedClientIds)

	for chunk := range slices.Chunk(reapedClientIds, removeCascadeChunkCount) {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// (cascade) remove TLS certificates of the reaped clients
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM client_tls_certificate
				WHERE client_id = ANY($1::uuid[])
				`,
				idStrings(chunk),
			))
		}, server.TxReadCommitted)
	}

	for chunk := range slices.Chunk(reapedDeviceIds, removeCascadeChunkCount) {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// (cascade) remove the reaped clients' devices that no other
			// client still references
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM device
				WHERE
					device_id = ANY($1::uuid[]) AND
					NOT EXISTS (
						SELECT 1 FROM network_client
						WHERE network_client.device_id = device.device_id
					)
				`,
				idStrings(chunk),
			))
		}, server.TxReadCommitted)
	}
}

// removeCascadeChunkCount bounds the id-array size of a single targeted
// cascade delete statement.
const removeCascadeChunkCount = 10000

// removeProxyDeviceConfigsForClientIds deletes the proxy device configs of the
// given clients and their redis mirrors, returning the removed proxy ids.
func removeProxyDeviceConfigsForClientIds(ctx context.Context, clientIds []server.Id) []server.Id {
	removedProxyIds := []server.Id{}
	for chunk := range slices.Chunk(clientIds, removeCascadeChunkCount) {
		var chunkProxyIds []server.Id
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// reset in case the tx is retried on a transient error
			chunkProxyIds = nil

			result, err := tx.Query(
				ctx,
				`
				DELETE FROM proxy_device_config
				WHERE client_id = ANY($1::uuid[])
				RETURNING proxy_id
				`,
				idStrings(chunk),
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var proxyId server.Id
					server.Raise(result.Scan(&proxyId))
					chunkProxyIds = append(chunkProxyIds, proxyId)
				}
			})
		}, server.TxReadCommitted)
		removedProxyIds = append(removedProxyIds, chunkProxyIds...)
	}

	server.Redis(ctx, func(r server.RedisClient) {
		for _, proxyId := range removedProxyIds {
			err := r.Del(ctx, proxyDeviceConfigKey(proxyId)).Err()
			server.Raise(err)
		}
	})

	return removedProxyIds
}

// removeProxyClientData deletes proxy_client and proxy_client_change rows for
// the removed proxy ids.
// proxy_client rows are otherwise never deleted, and each wg proxy instance
// restores all rows for its (host, block) as wg device peers at startup, which
// is bounded by the device max peer count - so stale rows must be reaped.
// change rows are reaped so the startup full sync (GetProxyClientsSince from 0)
// stays bounded.
func removeProxyClientData(ctx context.Context, proxyIds []server.Id) {
	for chunk := range slices.Chunk(proxyIds, removeCascadeChunkCount) {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM proxy_client
				WHERE proxy_id = ANY($1::uuid[])
				`,
				idStrings(chunk),
			))
		}, server.TxReadCommitted)

		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM proxy_client_change
				WHERE proxy_id = ANY($1::uuid[])
				`,
				idStrings(chunk),
			))
		}, server.TxReadCommitted)
	}
}

// removeProvideKeysForClientIds deletes the provide keys of the given clients
// and their redis mirrors (provide modes and per-mode secret keys).
func removeProvideKeysForClientIds(ctx context.Context, clientIds []server.Id) {
	for chunk := range slices.Chunk(clientIds, removeCascadeChunkCount) {
		clientProvideModes := map[server.Id][]ProvideMode{}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// reset in case the tx is retried on a transient error
			clientProvideModes = map[server.Id][]ProvideMode{}

			result, err := tx.Query(
				ctx,
				`
				DELETE FROM provide_key
				WHERE client_id = ANY($1::uuid[])
				RETURNING client_id, provide_mode
				`,
				idStrings(chunk),
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var provideMode ProvideMode
					server.Raise(result.Scan(&clientId, &provideMode))
					clientProvideModes[clientId] = append(clientProvideModes[clientId], provideMode)
				}
			})
		}, server.TxReadCommitted)

		server.Redis(ctx, func(r server.RedisClient) {
			for clientId, provideModes := range clientProvideModes {
				pipe := r.TxPipeline()
				pipe.Del(ctx, provideModesKey(clientId))
				for _, provideMode := range provideModes {
					pipe.Del(ctx, provideModeSecretKeyKey(clientId, provideMode))
				}
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			}
		})
	}
}

// SweepOrphanNetworkClientData removes rows in the network-client dependent
// tables whose parent row no longer exists. RemoveDisconnectedNetworkClients
// cascades dependents together with the parent deletes, so this is a
// low-cadence safety net for orphans left by other deletion paths or older
// releases, not the primary cleanup mechanism. Each sweep deletes in bounded
// batches (streaming NOT EXISTS scans with LIMIT, no sort) until a batch comes
// up short.
func SweepOrphanNetworkClientData(ctx context.Context, limit int) (removedCount int64) {
	// per-connection tables whose connection is gone
	removedCount += sweepOrphanBatches(
		ctx,
		`
		DELETE FROM network_client_location
		USING (
			SELECT connection_id
			FROM network_client_location
			WHERE NOT EXISTS (
				SELECT 1 FROM network_client_connection
				WHERE network_client_connection.connection_id = network_client_location.connection_id
			)
			LIMIT $1
		) t
		WHERE network_client_location.connection_id = t.connection_id
		`,
		limit,
	)

	removedCount += sweepOrphanBatches(
		ctx,
		`
		DELETE FROM network_client_latency
		USING (
			SELECT connection_id
			FROM network_client_latency
			WHERE NOT EXISTS (
				SELECT 1 FROM network_client_connection
				WHERE network_client_connection.connection_id = network_client_latency.connection_id
			)
			LIMIT $1
		) t
		WHERE network_client_latency.connection_id = t.connection_id
		`,
		limit,
	)

	removedCount += sweepOrphanBatches(
		ctx,
		`
		DELETE FROM network_client_speed
		USING (
			SELECT connection_id
			FROM network_client_speed
			WHERE NOT EXISTS (
				SELECT 1 FROM network_client_connection
				WHERE network_client_connection.connection_id = network_client_speed.connection_id
			)
			LIMIT $1
		) t
		WHERE network_client_speed.connection_id = t.connection_id
		`,
		limit,
	)

	// proxy device configs whose client is gone. RETURNING feeds the redis
	// mirror cleanup and the proxy_client/proxy_client_change cascade.
	for {
		var orphanProxyIds []server.Id
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// reset in case the tx is retried on a transient error
			orphanProxyIds = nil

			result, err := tx.Query(
				ctx,
				`
				DELETE FROM proxy_device_config
				USING (
					SELECT proxy_id
					FROM proxy_device_config
					WHERE NOT EXISTS (
						SELECT 1 FROM network_client
						WHERE network_client.client_id = proxy_device_config.client_id
					)
					LIMIT $1
				) t
				WHERE proxy_device_config.proxy_id = t.proxy_id
				RETURNING proxy_device_config.proxy_id
				`,
				limit,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var proxyId server.Id
					server.Raise(result.Scan(&proxyId))
					orphanProxyIds = append(orphanProxyIds, proxyId)
				}
			})
		}, server.TxReadCommitted)

		server.Redis(ctx, func(r server.RedisClient) {
			for _, proxyId := range orphanProxyIds {
				err := r.Del(ctx, proxyDeviceConfigKey(proxyId)).Err()
				server.Raise(err)
			}
		})
		removeProxyClientData(ctx, orphanProxyIds)

		removedCount += int64(len(orphanProxyIds))
		if len(orphanProxyIds) < limit {
			break
		}
	}

	// proxy clients whose config is gone (covers configs deleted outside the
	// reap, e.g. RemoveProxyDeviceConfig)
	removedCount += sweepOrphanBatches(
		ctx,
		`
		DELETE FROM proxy_client
		USING (
			SELECT proxy_id
			FROM proxy_client
			WHERE NOT EXISTS (
				SELECT 1 FROM proxy_device_config
				WHERE proxy_device_config.proxy_id = proxy_client.proxy_id
			)
			LIMIT $1
		) t
		WHERE proxy_client.proxy_id = t.proxy_id
		`,
		limit,
	)

	// change rows whose proxy client is gone
	removedCount += sweepOrphanBatches(
		ctx,
		`
		DELETE FROM proxy_client_change
		USING (
			SELECT proxy_host, block, change_id
			FROM proxy_client_change
			WHERE NOT EXISTS (
				SELECT 1 FROM proxy_client
				WHERE proxy_client.proxy_id = proxy_client_change.proxy_id
			)
			LIMIT $1
		) t
		WHERE
			proxy_client_change.proxy_host = t.proxy_host AND
			proxy_client_change.block = t.block AND
			proxy_client_change.change_id = t.change_id
		`,
		limit,
	)

	// provide keys whose client is gone. RETURNING feeds the redis mirror
	// cleanup.
	for {
		clientProvideModes := map[server.Id][]ProvideMode{}
		batchCount := 0
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// reset in case the tx is retried on a transient error
			clientProvideModes = map[server.Id][]ProvideMode{}
			batchCount = 0

			result, err := tx.Query(
				ctx,
				`
				DELETE FROM provide_key
				USING (
					SELECT client_id, provide_mode
					FROM provide_key
					WHERE NOT EXISTS (
						SELECT 1 FROM network_client
						WHERE network_client.client_id = provide_key.client_id
					)
					LIMIT $1
				) t
				WHERE
					provide_key.client_id = t.client_id AND
					provide_key.provide_mode = t.provide_mode
				RETURNING provide_key.client_id, provide_key.provide_mode
				`,
				limit,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var provideMode ProvideMode
					server.Raise(result.Scan(&clientId, &provideMode))
					clientProvideModes[clientId] = append(clientProvideModes[clientId], provideMode)
					batchCount += 1
				}
			})
		}, server.TxReadCommitted)

		server.Redis(ctx, func(r server.RedisClient) {
			for clientId, provideModes := range clientProvideModes {
				pipe := r.TxPipeline()
				pipe.Del(ctx, provideModesKey(clientId))
				for _, provideMode := range provideModes {
					pipe.Del(ctx, provideModeSecretKeyKey(clientId, provideMode))
				}
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			}
		})

		removedCount += int64(batchCount)
		if batchCount < limit {
			break
		}
	}

	// TLS certificates whose client is gone
	removedCount += sweepOrphanBatches(
		ctx,
		`
		DELETE FROM client_tls_certificate
		USING (
			SELECT client_id
			FROM client_tls_certificate
			WHERE NOT EXISTS (
				SELECT 1 FROM network_client
				WHERE network_client.client_id = client_tls_certificate.client_id
			)
			LIMIT $1
		) t
		WHERE client_tls_certificate.client_id = t.client_id
		`,
		limit,
	)

	// devices no client references
	removedCount += sweepOrphanBatches(
		ctx,
		`
		DELETE FROM device
		USING (
			SELECT device_id
			FROM device
			WHERE NOT EXISTS (
				SELECT 1 FROM network_client
				WHERE network_client.device_id = device.device_id
			)
			LIMIT $1
		) t
		WHERE device.device_id = t.device_id
		`,
		limit,
	)

	return
}

func CreateNetworkClientHandler(ctx context.Context) (handlerId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		handlerId = server.NewId()
		host, _ := server.Host()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_handler (
					handler_id,
					heartbeat_time,
					handler_host
				)
				VALUES ($1, $2, $3)
			`,
			handlerId,
			server.NowUtc(),
			host,
		))
	})
	return
}

func HeartbeatNetworkClientHandler(ctx context.Context, handlerId server.Id) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_client_handler
				SET
					heartbeat_time = $2
				WHERE
					handler_id = $1
			`,
			handlerId,
			server.NowUtc(),
		))
		if tag.RowsAffected() != 1 {
			returnErr = errors.New("Handler does not exist.")
			return
		}
	})
	return
}

func CloseExpiredNetworkClientHandlers(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		handlerIds := []server.Id{}

		result, err := tx.Query(
			ctx,
			`
				SELECT
					handler_id
				FROM network_client_handler
				WHERE
					heartbeat_time < $1
			`,
			minTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var handlerId server.Id
				server.Raise(result.Scan(&handlerId))
				handlerIds = append(handlerIds, handlerId)
			}
		})

		server.CreateTempTableInTx(ctx, tx, "temp_handler_ids(handler_id uuid)", handlerIds...)

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_client_connection
				SET
					connected = false,
					disconnect_time = $1
				FROM temp_handler_ids
				WHERE
					temp_handler_ids.handler_id = network_client_connection.handler_id

			`,
			server.NowUtc(),
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_client_handler
				USING temp_handler_ids
				WHERE
					temp_handler_ids.handler_id = network_client_handler.handler_id
			`,
		))
	})
}

type NetworkClientConnectionStatus struct {
	Connected    bool
	ClientExists bool
}

func (self *NetworkClientConnectionStatus) Err() error {
	if !self.Connected {
		return fmt.Errorf("force disconnected")
	}
	if !self.ClientExists {
		return fmt.Errorf("client does not exist")
	}
	return nil
}

func GetNetworkClientConnectionStatus(ctx context.Context, connectionId server.Id) *NetworkClientConnectionStatus {
	status := &NetworkClientConnectionStatus{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client_connection.connected,
					network_client.client_id IS NOT NULL AS client_exists
				FROM network_client_connection
				LEFT JOIN network_client ON
					network_client.client_id = network_client_connection.client_id
				WHERE network_client_connection.connection_id = $1
			`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&status.Connected,
					&status.ClientExists,
				))
			}
		})
	})

	return status
}

type DeviceSetNameArgs struct {
	DeviceId   server.Id `json:"device_id"`
	DeviceName string    `json:"device_name"`
}

type DeviceSetNameResult struct {
	Error *DeviceSetNameError `json:"error,omitempty"`
}

type DeviceSetNameError struct {
	Message string `json:"message"`
}

func DeviceSetName(
	setName *DeviceSetNameArgs,
	clientSession *session.ClientSession,
) (setNameResult *DeviceSetNameResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
				UPDATE device SET
					device_name = $2
				WHERE
					device_id = $1 AND
					network_id = $3
			`,
			setName.DeviceId,
			setName.DeviceName,
			clientSession.ByJwt.NetworkId,
		))
		if tag.RowsAffected() != 1 {
			setNameResult = &DeviceSetNameResult{
				Error: &DeviceSetNameError{
					Message: "Device does not exist.",
				},
			}
			return
		}
		setNameResult = &DeviceSetNameResult{}
	})
	return
}

type DeviceSetProvideArgs struct {
	ClientId    server.Id   `json:"client_id"`
	ProvideMode ProvideMode `json:"provide_mode"`
}

type DeviceSetProvideResult struct {
	ProvideMode ProvideMode            `json:"provide_mode"`
	Error       *DeviceSetProvideError `json:"error,omitempty"`
}

type DeviceSetProvideError struct {
	Message string `json:"message"`
}

func DeviceSetProvide(setProvide *DeviceSetProvideArgs, clientSession *session.ClientSession) (*DeviceSetProvideResult, error) {
	// Remote provide-mode setting is not supported: the provide secret keys are
	// device-held (see SetProvide's secretKeys arg) and a remote API call does
	// not have them. Return a spec-conformant error rather than a 500 until the
	// security model is changed to support server-side provide keys.
	return &DeviceSetProvideResult{
		Error: &DeviceSetProvideError{
			Message: "Remote provide-mode setting is not supported.",
		},
	}, nil
}

func Testing_CreateDevice(
	ctx context.Context,
	networkId server.Id,
	deviceId server.Id,
	clientId server.Id,
	deviceName string,
	deviceSpec string,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		createTime := server.NowUtc()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO device (
					device_id,
					network_id,
					device_name,
					device_spec,
					create_time
				)
				VALUES ($1, $2, $3, $4, $5)
			`,
			deviceId,
			networkId,
			deviceName,
			deviceSpec,
			createTime,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client (
					client_id,
					network_id,
					device_id,
					description,
					create_time,
					auth_time
				)
				VALUES ($1, $2, $3, $4, $5, $5)
			`,
			clientId,
			networkId,
			deviceId,
			deviceName,
			createTime,
		))
	})
}

// Testing_DeleteProvideMirror removes the redis-mirrored provide state for a
// client, simulating an incomplete migration or lost redis state. The db
// `provide_key` rows are left in place so tests can exercise the db fallback
// in `GetProvideModes`/`GetProvideSecretKey`.
func Testing_DeleteProvideMirror(ctx context.Context, clientId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		// all of a client's keys share the {pm_<clientId>} hash tag (same slot)
		keys := []string{provideModesKey(clientId)}
		for _, provideMode := range []ProvideMode{
			ProvideModeNetwork,
			ProvideModeFriendsAndFamily,
			ProvideModePublic,
			ProvideModeStream,
		} {
			keys = append(keys, provideModeSecretKeyKey(clientId, provideMode))
		}
		r.Del(ctx, keys...)
	})
}

func ClientError(ctx context.Context, networkId server.Id, clientId server.Id, connectionId server.Id, op string, err error) {
	ttl := 5 * time.Minute
	warnThreshold := int64(30)

	// scrub the error message
	errorMessage := server.ScrubIpPort(err.Error())

	networkKey := fmt.Sprintf("{client_error}network_%s", networkId)
	clientKey := fmt.Sprintf("{client_error}client_%s", clientId)
	networkErrorMessageKey := fmt.Sprintf("{client_error}network_%s_message_%s", networkId, errorMessage)
	clientErrorMessageKey := fmt.Sprintf("{client_error}client_%s_message_%s", clientId, errorMessage)

	server.Redis(ctx, func(r server.RedisClient) {

		var networkCountCmd *redis.IntCmd
		var clientCountCmd *redis.IntCmd
		var networkErrorMessageCountCmd *redis.IntCmd
		var clientErrorMessageCountCmd *redis.IntCmd
		r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			networkCountCmd = pipe.Incr(ctx, networkKey)
			pipe.Expire(ctx, networkKey, ttl)

			clientCountCmd = pipe.Incr(ctx, clientKey)
			pipe.Expire(ctx, clientKey, ttl)

			networkErrorMessageCountCmd = pipe.Incr(ctx, networkErrorMessageKey)
			pipe.Expire(ctx, networkErrorMessageKey, ttl)

			clientErrorMessageCountCmd = pipe.Incr(ctx, clientErrorMessageKey)
			pipe.Expire(ctx, clientErrorMessageKey, ttl)

			return nil
		})

		networkCount, err := networkCountCmd.Result()
		if err == nil {
			if networkCount%warnThreshold == 0 {
				glog.V(1).Infof("[ncm][%s]network has a significant amount of connection errors (%d)\n", networkId, networkCount)
			}
		}

		clientCount, err := clientCountCmd.Result()
		if err == nil {
			if clientCount%warnThreshold == 0 {
				glog.V(1).Infof("[ncm][%s]client has a significant amount of connection errors (%d)\n", clientId, clientCount)
			}
		}

		networkErrorMessageCount, err := networkErrorMessageCountCmd.Result()
		if err == nil {
			if networkErrorMessageCount%warnThreshold == 0 {
				glog.V(1).Infof("[ncm][%s]network has a significant count of connection error message (%d): %s\n", networkId, networkErrorMessageCount, errorMessage)
			}
		}

		clientErrorMessageCount, err := clientErrorMessageCountCmd.Result()
		if err == nil {
			if clientErrorMessageCount%warnThreshold == 0 {
				glog.V(1).Infof("[ncm][%s]client has a significant count of connection error message (%d): %s\n", clientId, clientErrorMessageCount, errorMessage)
			}
		}

	})
}
