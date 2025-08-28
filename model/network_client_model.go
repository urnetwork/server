package model

import (
	"context"
	// "crypto/sha256"
	"errors"
	// "net"
	// "net/netip"
	// "regexp"
	"strconv"
	// "strings"
	// "sync"

	// "bytes"
	"fmt"
	"time"

	// "github.com/twmb/murmur3"
	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	// "github.com/urnetwork/server/ulid"
	// "github.com/urnetwork/server/jwt"
)

const NetworkClientHandlerHeartbeatTimeout = 5 * time.Second

// const LimitClientIdsPer24Hours = 1024
const LimitClientIdsPerNetwork = 128

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

// FIXME source client id. if source, tag the client as ancillary and just copy the device id from the source
// FIXME get network clients to include the source network id
type AuthNetworkClientArgs struct {
	// if omitted, a new client_id is created
	ClientId       *server.Id `json:"client_id,omitempty"`
	SourceClientId *server.Id `json:"source_client_id,omitempty"`
	Description    string     `json:"description"`
	DeviceSpec     string     `json:"device_spec"`
}

type AuthNetworkClientResult struct {
	ByClientJwt *string                 `json:"by_client_jwt,omitempty"`
	Error       *AuthNetworkClientError `json:"error,omitempty"`
}

type AuthNetworkClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool   `json:"client_limit_exceeded"`
	Message             string `json:"message"`
}

func AuthNetworkClient(
	authClient *AuthNetworkClientArgs,
	session *session.ClientSession,
) (authClientResult *AuthNetworkClientResult, authClientError error) {
	if authClient.ClientId == nil {
		// important: use serializable tx for rate limits
		server.Tx(session.Ctx, func(tx server.PgTx) {
			createTime := server.NowUtc()

			clientId := server.NewId()
			var deviceId server.Id

			if authClient.SourceClientId == nil {
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
						source_client_id
					)
					VALUES ($1, $2, $3, $4, $5, $5, $6)
				`,
				clientId,
				session.ByJwt.NetworkId,
				deviceId,
				authClient.Description,
				createTime,
				authClient.SourceClientId,
			))

			byJwtWithClientId := session.ByJwt.Client(deviceId, clientId).Sign()
			authClientResult = &AuthNetworkClientResult{
				ByClientJwt: &byJwtWithClientId,
			}
		})
	} else {
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
					server.Raise(result.Scan(deviceId))
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
						device_spec = $2,
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

			byJwtWithClientId := session.ByJwt.Client(*deviceId, *authClient.ClientId).Sign()
			authClientResult = &AuthNetworkClientResult{
				ByClientJwt: &byJwtWithClientId,
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
	removeClient RemoveNetworkClientArgs,
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
					active = false
				WHERE
					client_id = $1 AND
					network_id = $2
			`,
			removeClient.ClientId,
			session.ByJwt.NetworkId,
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

	ProvideMode *ProvideMode               `json:"provide_mode"`
	Connections []*NetworkClientConnection `json:"connections"`
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
					network_client_resident.resident_id,
					network_client_resident.resident_host,
					network_client_resident.resident_service,
					network_client_resident.resident_block,
					provide_key.provide_mode
				FROM network_client
				LEFT JOIN network_client_resident ON
					network_client_resident.client_id = network_client.client_id
				LEFT JOIN provide_key ON
					provide_key.client_id = network_client.client_id AND
					provide_key.provide_mode = $2
				LEFT JOIN device ON
					device.device_id = network_client.device_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
			`,
			session.ByJwt.NetworkId,
			ProvideModePublic,
		)
		clientInfos := map[server.Id]*NetworkClientInfo{}
		server.WithPgResult(result, err, func() {

			var residentId_ *server.Id
			var residentHost_ *string
			var residentService_ *string
			var residentBlock_ *string

			for result.Next() {
				clientInfo := &NetworkClientInfo{}
				var deviceName_ *string
				var deviceSpec_ *string
				server.Raise(result.Scan(
					&clientInfo.ClientId,
					&clientInfo.SourceClientId,
					&clientInfo.Description,
					&clientInfo.DeviceId,
					&deviceName_,
					&deviceSpec_,
					&clientInfo.CreateTime,
					&clientInfo.AuthTime,
					&residentId_,
					&residentHost_,
					&residentService_,
					&residentBlock_,
					// &clientInfo.ResidentId,
					// &clientInfo.ResidentHost,
					// &clientInfo.ResidentService,
					// &clientInfo.ResidentBlock,
					&clientInfo.ProvideMode,
				))
				if deviceName_ != nil {
					clientInfo.DeviceName = *deviceName_
				}
				if deviceSpec_ != nil {
					clientInfo.DeviceSpec = *deviceSpec_
				}
				if residentId_ != nil {
					clientInfo.Resident = &NetworkClientResident{
						ClientId:        clientInfo.ClientId,
						ResidentId:      *residentId_,
						ResidentHost:    *residentHost_,
						ResidentService: *residentService_,
						ResidentBlock:   *residentBlock_,
					}
				}
				clientInfos[clientInfo.ClientId] = clientInfo
			}
		})

		// join in internal ports
		result, err = conn.Query(
			session.Ctx,
			`
				SELECT
					network_client_resident_port.client_id,
					network_client_resident_port.resident_internal_port

				FROM network_client

				INNER JOIN network_client_resident ON
					network_client.client_id = network_client_resident.client_id

				INNER JOIN network_client_resident_port ON
					network_client_resident_port.client_id = network_client_resident.client_id AND
					network_client_resident_port.resident_id = network_client_resident.resident_id
				
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
			`,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var port int
				server.Raise(result.Scan(&clientId, &port))
				if clientInfo, ok := clientInfos[clientId]; ok {
					if resident := clientInfo.Resident; resident != nil {
						resident.ResidentInternalPorts = append(resident.ResidentInternalPorts, port)
					}
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
	return fmt.Sprintf("pending_client_connection_%s", clientId)
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
					network_client.auth_time
				FROM network_client
				LEFT JOIN device ON device.device_id = network_client.device_id
				WHERE
					client_id = $1 AND
					active = true
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				networkClient = &NetworkClient{
					ClientId: clientId,
				}
				var deviceName_ *string
				var deviceSpec_ *string
				server.Raise(result.Scan(
					&networkClient.NetworkId,
					&networkClient.Description,
					&deviceName_,
					&deviceSpec_,
					&networkClient.CreateTime,
					&networkClient.AuthTime,
				))
				if deviceName_ != nil {
					networkClient.DeviceName = *deviceName_
				}
				if deviceSpec_ != nil {
					networkClient.DeviceSpec = *deviceSpec_
				}
			}
		})
	})

	return networkClient
}

func GetProvideModes(ctx context.Context, clientId server.Id) (provideModes map[ProvideMode]bool, returnErr error) {
	provideModes = map[ProvideMode]bool{}
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
			for result.Next() {
				var provideMode ProvideMode
				server.Raise(result.Scan(&provideMode))
				provideModes[provideMode] = true
			}
		})
	})
	return
}

func GetProvideSecretKey(
	ctx context.Context,
	clientId server.Id,
	provideMode ProvideMode,
) (secretKey []byte, returnErr error) {
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
	return
}

func SetProvide(
	ctx context.Context,
	clientId server.Id,
	secretKeys map[ProvideMode][]byte,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		changeTime := server.NowUtc()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM provide_key
			WHERE client_id = $1
			`,
			clientId,
		))

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

		result, err = tx.Query(
			ctx,
			`
			SELECT
				provide_mode
			FROM provide_key
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
	return
}

func RemoveOldProvideKeyChanges(ctx context.Context, minTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
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
					handler_id
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
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

func RemoveDisconnectedNetworkClients(ctx context.Context, minTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_client_connection
				WHERE
					connected = false AND
					disconnect_time < $1
			`,
			minTime.UTC(),
		))
	}, server.TxReadCommitted)

	server.Tx(ctx, func(tx server.PgTx) {
		// (cascade) clean up network_client_location
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM network_client_location
			USING (
			    SELECT
			        network_client_location.connection_id
			    FROM network_client_location
			    LEFT JOIN network_client_connection ON
			        network_client_connection.connection_id = network_client_location.connection_id
			    WHERE network_client_connection.connection_id IS NULL
			) t
			WHERE network_client_location.connection_id = t.connection_id
			`,
		))

	}, server.TxReadCommitted)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM network_client
			WHERE network_client.create_time < $1 AND active = false
			`,
			minTime.UTC(),
		))

	}, server.TxReadCommitted)

	// FIXME perf only do this when the client can recover correctly
	// remove network clients without a connection
	// important: the app will need to create a new client id for these clients when it notices the existing jwt fails
	// server.RaisePgResult(tx.Exec(
	// 	ctx,
	// 	`
	// 	DELETE FROM network_client
	// 	USING (
	// 		SELECT network_client.client_id
	// 		FROM network_client
	// 	    	LEFT JOIN network_client_connection ON network_client_connection.client_id = network_client.client_id
	// 		WHERE network_client.create_time < $1 AND network_client_connection.client_id IS NULL
	// 	) t
	// 	WHERE network_client.client_id = t.client_id
	// 	`,
	// 	minTime.UTC(),
	// ))

	server.Tx(ctx, func(tx server.PgTx) {

		// (cascade) remove provide keys without a network client
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM provide_key
			USING (
				SELECT provide_key.client_id
			    FROM provide_key
			    	LEFT JOIN network_client ON network_client.client_id = provide_key.client_id
			    WHERE network_client.client_id IS NULL
			) t
			WHERE provide_key.client_id = t.client_id
			`,
		))

	}, server.TxReadCommitted)

	server.Tx(ctx, func(tx server.PgTx) {

		// (cascade) remove devices without a network client
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM device
			USING (
				SELECT device.device_id
		       	FROM device
					LEFT JOIN network_client ON network_client.device_id = device.device_id
		      	WHERE network_client.device_id IS NULL
		    ) t
			WHERE device.device_id = t.device_id
			`,
		))
	}, server.TxReadCommitted)

	server.Tx(ctx, func(tx server.PgTx) {

		// (cascade) remove residents without a network client
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM network_client_resident
			USING (
				SELECT network_client_resident.client_id
		       	FROM network_client_resident
					LEFT JOIN network_client ON network_client.client_id = network_client_resident.client_id
		      	WHERE network_client.client_id IS NULL
		    ) t
			WHERE network_client_resident.client_id = t.client_id
			`,
		))

	}, server.TxReadCommitted)
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
	server.Tx(ctx, func(tx server.PgTx) {
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

// the resident is a transport client that runs on the platform on behalf of a client
// there is at most one resident per client, which is self-nominated by any endpoint
// the nomination happens when the endpoint cannot communicate with the current resident

type NetworkClientResident struct {
	ClientId              server.Id `json:"client_id"`
	InstanceId            server.Id `json:"instance_id"`
	ResidentId            server.Id `json:"resident_id"`
	ResidentHost          string    `json:"resident_host"`
	ResidentService       string    `json:"resident_service"`
	ResidentBlock         string    `json:"resident_block"`
	ResidentInternalPorts []int     `json:"resident_internal_ports"`
}

func dbGetResidentInTx(
	ctx context.Context,
	tx server.PgTx,
	clientId server.Id,
) *NetworkClientResident {
	var resident *NetworkClientResident

	result, err := tx.Query(
		ctx,
		`
			SELECT
				instance_id,
				resident_id,
				resident_host,
				resident_service,
				resident_block
			FROM network_client_resident
			WHERE
				client_id = $1
		`,
		clientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			resident = &NetworkClientResident{
				ClientId: clientId,
			}
			server.Raise(result.Scan(
				&resident.InstanceId,
				&resident.ResidentId,
				&resident.ResidentHost,
				&resident.ResidentService,
				&resident.ResidentBlock,
			))
		}
	})
	if resident == nil {
		return nil
	}

	// join in internal ports
	result, err = tx.Query(
		ctx,
		`
			SELECT
				resident_internal_port
			FROM network_client_resident_port
			WHERE
				client_id = $1 AND
				resident_id = $2
		`,
		clientId,
		resident.ResidentId,
	)
	server.WithPgResult(result, err, func() {
		ports := []int{}
		for result.Next() {
			var port int
			server.Raise(result.Scan(&port))
			ports = append(ports, port)
		}
		resident.ResidentInternalPorts = ports
	})

	return resident
}

func dbGetResidentWithInstanceInTx(
	ctx context.Context,
	tx server.PgTx,
	clientId server.Id,
	instanceId server.Id,
) *NetworkClientResident {
	resident := dbGetResidentInTx(ctx, tx, clientId)
	if resident != nil && resident.InstanceId == instanceId {
		return resident
	}
	return nil
}

func GetResident(ctx context.Context, clientId server.Id) *NetworkClientResident {
	var resident *NetworkClientResident

	server.Tx(ctx, func(tx server.PgTx) {
		resident = dbGetResidentInTx(ctx, tx, clientId)
	})

	return resident
}

func GetResidentWithInstance(ctx context.Context, clientId server.Id, instanceId server.Id) *NetworkClientResident {
	var resident *NetworkClientResident

	server.Tx(ctx, func(tx server.PgTx) {
		resident = dbGetResidentWithInstanceInTx(ctx, tx, clientId, instanceId)
	})

	return resident
}

func GetResidentId(ctx context.Context, clientId server.Id) (residentId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					resident_id
				FROM network_client_resident
				WHERE
					client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&residentId))
			} else {
				returnErr = errors.New("No resident for client.")
			}
		})
	})
	return
}

func GetResidentIdWithInstance(ctx context.Context, clientId server.Id, instanceId server.Id) (residentId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					resident_id
				FROM network_client_resident
				WHERE
					client_id = $1 AND
					instance_id = $2
			`,
			clientId,
			instanceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&residentId))
			} else {
				returnErr = errors.New("No resident for client instance.")
			}
		})
	})
	return
}

// replace an existing resident with the given, or if there was already a replacement, return it
func NominateResident(
	ctx context.Context,
	residentIdToReplace *server.Id,
	nomination *NetworkClientResident,
) (nominated bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		/*
			result, err := tx.Query(
				ctx,
				`
					SELECT
						resident_id
					FROM network_client_resident
					WHERE
						client_id = $1 AND
						instance_id = $2
					FOR UPDATE
				`,
				nomination.ClientId,
				nomination.InstanceId,
			)

			hasResident := false
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var residentId server.Id
					server.Raise(result.Scan(&residentId))
					if residentIdToReplace != nil && *residentIdToReplace == residentId {
						hasResident = true
					}
				} else if residentIdToReplace == nil {
					hasResident = true
				}
			})

			// fmt.Printf("hasResident=%t test=%t\n", hasResident, hasResident && (residentIdToReplace == nil || residentId != *residentIdToReplace))

			if !hasResident {
				// already replaced
				nominated = false
				return
			}
		*/

		var tag server.PgTag
		if residentIdToReplace == nil {
			tag = server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client_resident (
						client_id,
						instance_id,
						resident_id,
						resident_host,
						resident_service,
						resident_block
					)
					VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (client_id) DO UPDATE
					SET
						instance_id = $2,
						resident_id = $3,
						resident_host = $4,
						resident_service = $5,
						resident_block = $6
					WHERE
						network_client_resident.instance_id != $2
				`,
				nomination.ClientId,
				nomination.InstanceId,
				nomination.ResidentId,
				nomination.ResidentHost,
				nomination.ResidentService,
				nomination.ResidentBlock,
			))
		} else {
			tag = server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client_resident (
						client_id,
						instance_id,
						resident_id,
						resident_host,
						resident_service,
						resident_block
					)
					VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (client_id) DO UPDATE
					SET
						instance_id = $2,
						resident_id = $3,
						resident_host = $4,
						resident_service = $5,
						resident_block = $6
					WHERE
						network_client_resident.instance_id != $2 OR
							network_client_resident.resident_id = $7
				`,
				nomination.ClientId,
				nomination.InstanceId,
				nomination.ResidentId,
				nomination.ResidentHost,
				nomination.ResidentService,
				nomination.ResidentBlock,
				residentIdToReplace,
			))
		}
		if tag.RowsAffected() == 0 {
			nominated = false
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_client_resident_port
				WHERE
					client_id = $1 AND
					resident_id = $2
			`,
			nomination.ClientId,
			nomination.ResidentId,
		))

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, port := range nomination.ResidentInternalPorts {
				batch.Queue(
					`
						INSERT INTO network_client_resident_port (
							client_id,
							resident_id,
							resident_internal_port
						)
						VALUES ($1, $2, $3)
					`,
					nomination.ClientId,
					nomination.ResidentId,
					port,
				)
			}
		})

		nominated = true
	})
	return
}

// if any of the ports overlap
func GetResidentsForHostPorts(ctx context.Context, host string, ports []int) []*NetworkClientResident {
	residents := []*NetworkClientResident{}

	server.Tx(ctx, func(tx server.PgTx) {
		server.CreateTempTableInTx(
			ctx,
			tx,
			"resident_ports(resident_internal_port int)",
			ports...,
		)

		result, err := tx.Query(
			ctx,
			`
				SELECT
					DISTINCT network_client_resident.client_id
				FROM network_client_resident

				INNER JOIN network_client_resident_port ON
					network_client_resident_port.client_id = network_client_resident.client_id AND
					network_client_resident_port.resident_id = network_client_resident.resident_id

				INNER JOIN resident_ports ON
					resident_ports.resident_internal_port = network_client_resident_port.resident_internal_port

				WHERE
					resident_host = $1
				`,
			host,
		)
		clientIds := []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})

		for _, clientId := range clientIds {
			resident := dbGetResidentInTx(ctx, tx, clientId)
			residents = append(residents, resident)
		}
	})

	return residents
}

func RemoveResident(
	ctx context.Context,
	clientId server.Id,
	residentId server.Id,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			DELETE FROM network_client_resident
			WHERE
				client_id = $1 AND
				resident_id = $2
			`,
			clientId,
			residentId,
		)
		server.Raise(err)

		_, err = tx.Exec(
			ctx,
			`
			DELETE FROM network_client_resident_port
			WHERE
				client_id = $1 AND
				resident_id = $2
			`,
			clientId,
			residentId,
		)
		server.Raise(err)
	})
}

type DeviceSetNameArgs struct {
	DeviceId   server.Id `json:"client_id"`
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
					device_id = $1
			`,
			setName.DeviceId,
			setName.DeviceName,
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
	// FIXME we don't support remote setting of local settings at the moment
	return nil, fmt.Errorf("Not implemented.")
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

func ClientError(ctx context.Context, networkId server.Id, clientId server.Id, connectionId server.Id, op string, err error) {
	ttl := 5 * time.Minute
	warnThreshold := int64(30)

	// scrub the error message
	errorMessage := server.ScrubIpPort(err.Error())

	networkKey := fmt.Sprintf("client_error_network_%s", networkId)
	clientKey := fmt.Sprintf("client_error_client_%s", clientId)
	networkErrorMessageKey := fmt.Sprintf("client_error_network_%s_message_%s", networkId, errorMessage)
	clientErrorMessageKey := fmt.Sprintf("client_error_client_%s_message_%s", clientId, errorMessage)

	server.Redis(ctx, func(r server.RedisClient) {

		networkCount, err := r.Incr(ctx, networkKey).Result()
		if err == nil {
			r.Expire(ctx, networkKey, ttl)
			if networkCount%warnThreshold == 0 {
				glog.Infof("[ncm][%s]network has a significant amount of connection errors (%d)\n", networkId, networkCount)
			}
		}

		clientCount, err := r.Incr(ctx, clientKey).Result()
		if err == nil {
			r.Expire(ctx, clientKey, ttl)
			if clientCount%warnThreshold == 0 {
				glog.Infof("[ncm][%s]client has a significant amount of connection errors (%d)\n", clientId, clientCount)
			}
		}

		networkErrorMessageCount, err := r.Incr(ctx, networkErrorMessageKey).Result()
		if err == nil {
			r.Expire(ctx, networkErrorMessageKey, ttl)
			if networkErrorMessageCount%warnThreshold == 0 {
				glog.Infof("[ncm][%s]network has a significant count of connection error message (%d): %s\n", networkId, networkErrorMessageCount, errorMessage)
			}
		}

		clientErrorMessageCount, err := r.Incr(ctx, clientErrorMessageKey).Result()
		if err == nil {
			r.Expire(ctx, clientErrorMessageKey, ttl)
			if clientErrorMessageCount%warnThreshold == 0 {
				glog.Infof("[ncm][%s]client has a significant count of connection error message (%d): %s\n", clientId, clientErrorMessageCount, errorMessage)
			}
		}

	})
}
