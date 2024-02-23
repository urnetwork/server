package model

import (
	"errors"
	"context"
	// "bytes"
	"time"
	"fmt"

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/ulid"
	// "bringyour.com/bringyour/jwt"
)


const LimitClientIdsPer24Hours = 1024
const LimitClientIdsPerNetwork = 128


// aligns with `protocol.ProvideMode`
type ProvideMode = int
const (
	ProvideModeDefault ProvideMode = -1
	ProvideModeNone ProvideMode = 0
	ProvideModeNetwork ProvideMode = 1
	ProvideModeFriendsAndFamily ProvideMode = 2
	ProvideModePublic ProvideMode = 3
	ProvideModeStream ProvideMode = 4
)


// client_ids are globally unique addressess tantamount to IPv6
// they are never revoked once allocated, to preserve security and audit records
// because they are a finite resource, the number created is rate limited per network
// the total number active per network is also limited


func FindClientNetwork(
	ctx context.Context,
	clientId bringyour.Id,
) (networkId bringyour.Id, returnErr error) {
	bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&networkId))
			} else {
				returnErr = fmt.Errorf("Client does not exist.")
			}
		})
	}))

	return
}


type AuthNetworkClientArgs struct {
	// if omitted, a new client_id is created
	ClientId *bringyour.Id `json:"client_id,omitempty"`
	Description string `json:"description"`
	DeviceSpec string `json:"device_spec"`
}

type AuthNetworkClientResult struct {
	ByClientJwt *string `json:"by_client_jwt,omitempty"`
	Error *AuthNetworkClientError `json:"error,omitempty"`
}

type AuthNetworkClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool `json:"client_limit_exceeded"` 
	Message string `json:"message"`
}

func AuthNetworkClient(
	authClient *AuthNetworkClientArgs,
	session *session.ClientSession,
) (authClientResult *AuthNetworkClientResult, authClientError error) {
	if authClient.ClientId == nil {
		// important: use serializable tx for rate limits
		bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
			result, err := tx.Query(
				session.Ctx,
				`
					SELECT COUNT(client_id) FROM network_client
					WHERE network_id = $1 AND $2 <= create_time
				`,
				session.ByJwt.NetworkId,
				time.Now(),
			)
			var last24HourCount int
			bringyour.WithPgResult(result, err, func() {
				if result.Next() {
					bringyour.Raise(result.Scan(&last24HourCount))
				}
			})

			if LimitClientIdsPer24Hours <= last24HourCount {
				authClientResult = &AuthNetworkClientResult{
					Error: &AuthNetworkClientError{
						ClientLimitExceeded: true,
						Message: "Too many new clients in the last 24 hours.",
					},
				}
				return
			}

			result, err = tx.Query(
				session.Ctx,
				`
					SELECT COUNT(client_id) FROM network_client
					WHERE network_id = $1 AND active = true
				`,
				session.ByJwt.NetworkId,
			)
			var activeCount int
			bringyour.WithPgResult(result, err, func() {
				result.Next()
				bringyour.Raise(result.Scan(&last24HourCount))
			})

			if LimitClientIdsPerNetwork <= activeCount {
				authClientResult = &AuthNetworkClientResult{
					Error: &AuthNetworkClientError{
						ClientLimitExceeded: true,
						Message: "Too many active clients.",
					},
				}
				return
			}

			createTime := time.Now()

			clientId := bringyour.NewId()
			deviceId := bringyour.NewId()

			bringyour.RaisePgResult(tx.Exec(
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

			bringyour.RaisePgResult(tx.Exec(
				session.Ctx,
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
				session.ByJwt.NetworkId,
				deviceId,
				authClient.Description,
				createTime,
			))

			byJwtWithClientId := session.ByJwt.Client(deviceId, clientId).Sign()
			authClientResult = &AuthNetworkClientResult{
				ByClientJwt: &byJwtWithClientId,
			}
		}))
	} else {
		// important: must check `network_id = session network_id`
		bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
			tag := bringyour.RaisePgResult(tx.Exec(
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
				time.Now(),
			))
			if tag.RowsAffected() != 1 {
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
			var deviceId *bringyour.Id
			bringyour.WithPgResult(result, err, func() {
				if result.Next() {
					bringyour.Raise(result.Scan(deviceId))
				}
			})

			if deviceId == nil {
				authClientResult = &AuthNetworkClientResult{
					Error: &AuthNetworkClientError{
						Message: "Client needs to be migrated (support@bringyour.com).",
					},
				}
				return
			}

			tag = bringyour.RaisePgResult(tx.Exec(
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
			if tag.RowsAffected() != 1 {
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
		}))
	}

	return
}


type RemoveNetworkClientArgs struct {
	ClientId bringyour.Id `json:"client_id"`
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
	bringyour.Raise(bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		tag, err := tx.Exec(
			session.Ctx,
			`
				UPDATE network_client SET active = false
				WHERE client_id = $1 AND network_id = $2
			`,
			removeClient.ClientId,
			session.ByJwt.NetworkId,
		)
		bringyour.Raise(err)
		if tag.RowsAffected() != 1 {
			removeClientResult = &RemoveNetworkClientResult{
				Error: &RemoveNetworkClientError{
					Message: "Client does not exist.",
				},
			}
			return
		}

		removeClientResult = &RemoveNetworkClientResult{}
	}))

	return removeClientResult, removeClientErr
}


type NetworkClientsResult struct {
	Clients []*NetworkClientInfo `json:"clients"`
}

type NetworkClientInfo struct {
	// ClientId bringyour.Id `json:"client_id"`
	// NetworkId bringyour.Id `json:"network_id"`
	// Description string `json:"description"`
	// DeviceSpec string `json:"device_spec"`

	// CreateTime time.Time `json:"create_time"`
	// AuthTime time.Time `json:"auth_time"`

	// InstanceId bringyour.Id `json:"client_id"`
	// ResidentId bringyour.Id `json:"resident_id"`
	// ResidentHost string `json:"resident_host"`
	// ResidentService string `json:"resident_service"`
	// ResidentBlock string `json:"resident_block"`
	// ResidentInternalPorts []int `json:"resident_internal_ports"`

	NetworkClient

	Resident *NetworkClientResident `json:"resident,omitempty"`

	ProvideMode *ProvideMode `json:"provide_mode"`
	Connections []*NetworkClientConnection `json:"connections"`
}

type NetworkClientConnection struct {
	ClientId bringyour.Id `json:"client_id"`
	ConnectionId bringyour.Id `json:"connection_id"`
	ConnectTime time.Time `json:"connect_time"`
	DisconnectTime *time.Time `json:"disconnect_time,omitempty"`
	ConnectionHost string `json:"connection_host"`
	ConnectionService string `json:"connection_service"`
	ConnectionBlock string `json:"connection_block"`
}

func GetNetworkClients(session *session.ClientSession) (*NetworkClientsResult, error) {
	var clientsResult *NetworkClientsResult
	var clientsErr error

	bringyour.Raise(bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
				SELECT
					network_client.client_id,
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
					client_provide.provide_mode
				FROM network_client
				LEFT JOIN network_client_resident ON
					network_client_resident.client_id = network_client.client_id
				LEFT JOIN client_provide ON
					client_provide.client_id = network_client.client_id
				LEFT JOIN device ON
					device.device_id = network_client.device_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
			`,
			session.ByJwt.NetworkId,
		)
		clientInfos := map[bringyour.Id]*NetworkClientInfo{}
		bringyour.WithPgResult(result, err, func() {

			var residentId_ *bringyour.Id
			var residentHost_ *string
			var residentService_ *string
			var residentBlock_ *string

			for result.Next() {
				clientInfo := &NetworkClientInfo{}
				var deviceName_ *string
				var deviceSpec_ *string
				bringyour.Raise(result.Scan(
					&clientInfo.ClientId,
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
						ClientId: clientInfo.ClientId,
						ResidentId: *residentId_,
						ResidentHost: *residentHost_,
						ResidentService: *residentService_,
						ResidentBlock: *residentBlock_,
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
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId bringyour.Id
				var port int
				bringyour.Raise(result.Scan(&clientId, &port))
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
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				clientConnection := &NetworkClientConnection{}
				bringyour.Raise(result.Scan(
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
	}))

	return clientsResult, clientsErr
}



type NetworkClient struct {
	ClientId bringyour.Id `json:"client_id"`
	DeviceId bringyour.Id `json:"device_id"`
	NetworkId bringyour.Id `json:"network_id"`
	Description string `json:"description"`
	DeviceName string `json:"device_name"`
	DeviceSpec string `json:"device_spec"`

	CreateTime time.Time `json:"create_time"`
	AuthTime time.Time `json:"auth_time"`
}


func GetNetworkClient(ctx context.Context, clientId bringyour.Id) *NetworkClient {
	var networkClient *NetworkClient

	bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				networkClient = &NetworkClient{
					ClientId: clientId,
				}
				var deviceName_ *string
				var deviceSpec_ *string
				bringyour.Raise(result.Scan(
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
	}))

	return networkClient
}


func GetProvideMode(ctx context.Context, clientId bringyour.Id) (provideMode ProvideMode, returnErr error) {
	bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT provide_mode FROM client_provide
				WHERE client_id = $1
			`,
			clientId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&provideMode))
			} else {
				returnErr = fmt.Errorf("Client provide mode not set.")
			}
		})
	}))
	return
}


func GetProvideSecretKey(
	ctx context.Context,
	clientId bringyour.Id,
	provideMode ProvideMode,
) (secretKey []byte, returnErr error) {
	bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&secretKey))
			} else {
				returnErr = fmt.Errorf("Provide secret key not set.")
			}
		})
	}))
	return
}


func SetProvide(
	ctx context.Context,
	clientId bringyour.Id,
	secretKeys map[ProvideMode][]byte,
) {
	var maxProvideMode ProvideMode
	for provideMode, _ := range secretKeys {
		if maxProvideMode < provideMode {
			maxProvideMode = provideMode
		}
	}
	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM client_provide
				WHERE client_id = $1
			`,
			clientId,
		))

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO client_provide (
					client_id,
					provide_mode
				) VALUES ($1, $2)
				ON CONFLICT (client_id) DO UPDATE
				SET
					provide_mode = $2
			`,
			clientId,
			maxProvideMode,
		))

		bringyour.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM provide_key
			WHERE client_id = $1
			`,
			clientId,
		))

		bringyour.Raise(bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
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
		}))
	}))
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
	clientId bringyour.Id,
	clientAddress string,
) bringyour.Id {
	var connectionId bringyour.Id

	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		connectionId = bringyour.NewId()
		connectTime := time.Now()

		_, err := tx.Exec(
			ctx,
			`
				INSERT INTO network_client_connection (
					client_id,
					connection_id,
					connect_time,
					connection_host,
					connection_service,
					connection_block,
					client_address
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			clientId,
			connectionId,
			connectTime,
			bringyour.RequireHost(),
			bringyour.RequireService(),
			bringyour.RequireBlock(),
			clientAddress,
		)
		bringyour.Raise(err)
	}))

	return connectionId
}


func DisconnectNetworkClient(ctx context.Context, connectionId bringyour.Id) error {
	var disconnectErr error

	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		disconnectTime := time.Now()
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
		bringyour.Raise(err)
		if tag.RowsAffected() != 1 {
			disconnectErr = errors.New("Connection does not exist.")
			return
		}
	}))

	return disconnectErr
}


func IsNetworkClientConnected(ctx context.Context, connectionId bringyour.Id) bool {
	connected := false

	bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT connected FROM network_client_connection
				WHERE connection_id = $1
			`,
			connectionId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&connected))
			}
		})
	}))

	return connected
}


// the resident is a transport client that runs on the platform on behalf of a client
// there is at most one resident per client, which is self-nominated by any endpoint
// the nomination happens when the endpoint cannot communicate with the current resident

type NetworkClientResident struct {
	ClientId bringyour.Id `json:"client_id"`
	InstanceId bringyour.Id `json:"instance_id"`
	ResidentId bringyour.Id `json:"resident_id"`
	ResidentHost string `json:"resident_host"`
	ResidentService string `json:"resident_service"`
	ResidentBlock string `json:"resident_block"`
	ResidentInternalPorts []int `json:"resident_internal_ports"`
}


func dbGetResidentInTx(
	ctx context.Context,
	tx bringyour.PgTx,
	clientId bringyour.Id,
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
				client_id = $1 AND
				resident_id IS NOT NULL
		`,
		clientId,
	)
	bringyour.WithPgResult(result, err, func() {
		if result.Next() {
			resident = &NetworkClientResident{
				ClientId: clientId,
			}
			bringyour.Raise(result.Scan(
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
	bringyour.WithPgResult(result, err, func() {
		ports := []int{}
		for result.Next() {
			var port int
			bringyour.Raise(result.Scan(&port))
			ports = append(ports, port)
		}
		resident.ResidentInternalPorts = ports
	})

	return resident
}


func dbGetResidentWithInstanceInTx(
	ctx context.Context,
	tx bringyour.PgTx,
	clientId bringyour.Id,
	instanceId bringyour.Id,
) *NetworkClientResident {
	resident := dbGetResidentInTx(ctx, tx, clientId)
	if resident != nil && resident.InstanceId == instanceId {
		return resident
	}
	return nil
}


func GetResident(ctx context.Context, clientId bringyour.Id) *NetworkClientResident {
	var resident *NetworkClientResident

	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		resident = dbGetResidentInTx(ctx, tx, clientId)
	}))

	return resident
}


func GetResidentWithInstance(ctx context.Context, clientId bringyour.Id, instanceId bringyour.Id) *NetworkClientResident {
	var resident *NetworkClientResident

	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		resident = dbGetResidentWithInstanceInTx(ctx, tx, clientId, instanceId)
	}))

	return resident
}


func GetResidentId(ctx context.Context, clientId bringyour.Id) (residentId bringyour.Id, returnErr error) {
	bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					resident_id
				FROM network_client_resident
				WHERE
					client_id = $1 AND
					resident_id IS NOT NULL
			`,
			clientId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&residentId))
			} else {
				returnErr = errors.New("No resident for client.")
			}
		})
	}))
	return
}


func GetResidentIdWithInstance(ctx context.Context, clientId bringyour.Id, instanceId bringyour.Id) (residentId bringyour.Id, returnErr error) {
	bringyour.Raise(bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					resident_id
				FROM network_client_resident
				WHERE
					client_id = $1 AND
					instance_id = $2 AND
					resident_id IS NOT NULL
			`,
			clientId,
			instanceId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				bringyour.Raise(result.Scan(&residentId))
			} else {
				returnErr = errors.New("No resident for client instance.")
			}
		})
	}))
	return
}


// replace an existing resident with the given, or if there was already a replacement, return it
func NominateResident(
	ctx context.Context,
	residentIdToReplace *bringyour.Id,
	nomination *NetworkClientResident,
) (resident *NetworkClientResident) {
	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					instance_id,
					resident_id
				FROM network_client_resident
				WHERE
					client_id = $1
				FOR UPDATE
			`,
			nomination.ClientId,
		)

		hasResident := false
		var residentId bringyour.Id
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				var instanceId bringyour.Id
				var residentId_ *bringyour.Id
				bringyour.Raise(result.Scan(&instanceId, &residentId_))
				if instanceId == nomination.InstanceId && residentId_ != nil {
					hasResident = true
					residentId = *residentId_
				}
			}
		})

		// fmt.Printf("hasResident=%t test=%t\n", hasResident, hasResident && (residentIdToReplace == nil || residentId != *residentIdToReplace))

		if hasResident {
			if residentIdToReplace == nil || residentId != *residentIdToReplace {
				// already replaced
				resident = dbGetResidentWithInstanceInTx(
					ctx,
					tx,
					nomination.ClientId,
					nomination.InstanceId,
				)
				return
			}
		}

		bringyour.RaisePgResult(tx.Exec(
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
			`,
			nomination.ClientId,
			nomination.InstanceId,
			nomination.ResidentId,
			nomination.ResidentHost,
			nomination.ResidentService,
			nomination.ResidentBlock,
		))

		bringyour.RaisePgResult(tx.Exec(
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

		bringyour.BatchInTx(ctx, tx, func(batch bringyour.PgBatch) {
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

		resident = nomination
	}, bringyour.TxReadCommitted))
	return
}


// if any of the ports overlap
func GetResidentsForHostPorts(ctx context.Context, host string, ports []int) []*NetworkClientResident {
	residents := []*NetworkClientResident{}

	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		bringyour.CreateTempTableInTx(
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
		clientIds := []bringyour.Id{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId bringyour.Id
				bringyour.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})

		for _, clientId := range clientIds {
			resident := dbGetResidentInTx(ctx, tx, clientId)
			residents = append(residents, resident)
		}
	}))
	
	return residents
}


func RemoveResident(
	ctx context.Context,
	clientId bringyour.Id,
	residentId bringyour.Id,
) {
	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
		bringyour.Raise(err)

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
		bringyour.Raise(err)
	}))
}


type DeviceSetNameArgs struct {
	DeviceId bringyour.Id `json:"client_id"`
	DeviceName string `json:"device_name"`
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
	bringyour.Tx(clientSession.Ctx, func(tx bringyour.PgTx) {
		tag := bringyour.RaisePgResult(tx.Exec(
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
	ClientId bringyour.Id `json:"client_id"`
	ProvideMode ProvideMode `json:"provide_mode"`
}

type DeviceSetProvideResult struct {
	ProvideMode ProvideMode `json:"provide_mode"`
	Error *DeviceSetProvideError `json:"error,omitempty"`
}

type DeviceSetProvideError struct {
	Message string `json:"message"`
}

func DeviceSetProvide(setProvide *DeviceSetProvideArgs, clientSession *session.ClientSession) (*DeviceSetProvideResult, error) {
	// FIXME
	return nil, nil
}


func Testing_CreateDevice(
	ctx context.Context,
	networkId bringyour.Id,
	deviceId bringyour.Id,
	clientId bringyour.Id,
	deviceName string,
	deviceSpec string,
) {
	bringyour.Raise(bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		createTime := time.Now()

		bringyour.RaisePgResult(tx.Exec(
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

		bringyour.RaisePgResult(tx.Exec(
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
	}))
}

