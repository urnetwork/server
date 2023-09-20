package model

import (
	"errors"
	"context"
	"bytes"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/jwt"
)


const LimitClientIdsPer24Hours = 1024
const LimitClientIdsPerNetwork = 128


type ProvideMode string
const (
	ProvideModeNetwork ProvideMode = "network"
	ProvideModeFriendsAndFamily ProvideMode = "ff"
	ProvideModePublic ProvideMode = "public"
	ProvideModeStream ProvideMode = "stream"
)


// client_ids are globally unique addressess tantamount to IPv6
// they are never revoked once allocated, to preserve security and audit records
// because they are a finite resource, the number created is rate limited per network
// the total number active per network is also limited


type NetworkAuthClientArgs struct {
	// if omitted, a new client_id is created
	ClientId *string `json:"clientId",omitempty`
	Description string `json:"description"`
	DeviceSpec string `json:"deviceSpec"`
}

type NetworkAuthClientResult struct {
	ByJwt *string `json:"byJwt,omitempty"`
	Error *NetworkClientCreateError `json:"error,omitempty"`
}

type NetworkAuthClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool `json:"clientLimitExceeded"` 
	Message string `json:"message"`
}

func NetworkAuthClient(
	authClient NetworkAuthClientArgs,
	session *session.ClientSession,
) (*NetworkAuthClientResult, error) {
	if session == nil {
		return nil, errors.New("Auth required")
	}

	var authClientResult *NetworkAuthClientResult
	var authClientError error

	if authClient.ClientId == nil {
		// important: use serializable tx for rate limits
		bringyour.Tx(session.Ctx, func(conn bringyour.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT COUNT(client_id) FROM network_client
					WHERE network_id = $1 AND $1 <= create_time
				`,
				session.NetworkId,
				time.Now(),
			)
			var last24HourCount int
			bringyour.WithDbResult(result, err, func() {
				result.Next()
				bringyour.Raise(
					result.Scan(&last24HourCount),
				)
			})

			if LimitClientIdsPer24Hours <= last24HourCount {
				authClientResult = &NetworkAuthClientResult{
					Error: &NetworkAuthClientError{
						ClientLimitExceeded: true,
						Message: "Too many new clients in the last 24 hours.",
					}
				}
				return
			}

			result, err = conn.Query(
				ctx,
				`
					SELECT COUNT(client_id) FROM network_client
					WHERE network_id = $1 AND active = true
				`,
				session.NetworkId,
			)
			var activeCount int
			bringyour.WithDbResult(result, err, func() {
				result.Next()
				bringyour.Raise(
					result.Scan(&last24HourCount),
				)
			})

			if LimitClientIdsPerNetwork <= activeCount {
				authClientResult = &NetworkAuthClientResult{
					Error: &NetworkAuthClientError{
						ClientLimitExceeded: true,
						Message: "Too many active clients.",
					}
				}
				return
			}

			clientId := ulid.New()

			_, err = conn.Exec(
				ctx,
				`
					INSERT INTO network_client (
						client_id,
						network_id,
						description,
						device_spec,
						create_time,
						auth_time,
					)
					VALUES ($1, $2, $3, $4, $5, $5)
				`,
				clientId,
				session.NetworkId,
				authClient.Description,
				authClient.DeviceSpec,
				time.Now(),
			)
			bringyour.Raise(err)

			authClientResult := &NetworkAuthClientResult{
				ByJwt: session.ByJwt.WithClientId(clientId).Sign(),
			}
		}, bringyour.TxSerializable)
	} else {
		// important: must check `network_id = session network_id`
		bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
			tag, err := conn.Exec(
				ctx,
				`
					UPDATE network_client
					SET
						description = $1,
						device_spec = $2,
						auth_time = $3
					WHERE
						client_id = $4 AND
						network_id = $5 AND
						active = true
				`,
				authClient.Description,
				authClient.DeviceSpec,
				time.Now(),
				session.ClientId,
				session.NetworkId,
			)
			bringyour.Raise(err)
			if tag.RowsAffected() != 1 {
				authClientResult = &NetworkAuthClientResult{
					Error: &NetworkAuthClientError{
						Message: "Client does not exist.",
					}
				}
				return
			}

			authClientResult := &NetworkAuthClientResult{
				ByJwt: session.ByJwt.WithClientId(clientId).Sign(),
			}
		})

		authClientResult := &NetworkAuthClientResult{
			ByJwt: session.ByJwt.WithClientId(authClient.ClientId).Sign(),
		}
	}

	return authClientResult, authClientError
}


type NetworkRemoveClientArgs struct {
	ClientId string `json:"clientId"`
}

type NetworkRemoveClientResult struct {
	Error *NetworkRemoveClientError `json:"error,omitempty"`
}

type NetworkRemoveClientError struct {
	Message string `json:"message"`
}

func NetworkRemoveClient(
	removeClient NetworkRemoveClientArgs,
	session *session.ClientSession,
) (*NetworkRemoveClientResult, error) {
	if session == nil {
		return nil, errors.New("Auth required")
	}

	var removeClientResult *NetworkRemoveClientResult
	var removeClientErr error

	// important: must check `network_id = session network_id`
	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		tag, err := conn.Exec(
			ctx,
			`
				UPDATE network_client SET active = false
				WHERE client_id = $1 AND network_id = $2
			`,
			session.ClientId,
			session.NetworkId,
		)
		bringyour.Raise(err)
		if tag.RowsAffected() != 1 {
			removeClientResult = &NetworkRemoveClientResult{
				Error: &NetworkRemoveClientError{
					Message: "Client does not exist.",
				}
			}
			return
		}

		removeClientResult := &NetworkRemoveClientResult{}
	})

	return removeClientResult, removeClientErr
}


type NetworkClientsResult struct {
	Clients map[string]*NetworkClientInfo `json:"clients"`
}

type NetworkClientInfo struct {
	NetworkClient
	NetworkClientResident
	ProvideMode *ProvideMode `json:"provideMode"`
	Connections []*NetworkClientConnection `json:"connections"`
}

func GetNetworkClients(session *session.ClientSession) (*NetworkClientsResult, error) {
	if session == nil {
		return nil, errors.New("Auth required")
	}

	var clientsResult *NetworkClientsResult
	var clientsErr error

	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client.client_id,
					network_client.description,
					network_client.device_spec,
					network_client.create_time,
					network_client.auth_time,
					network_client_resident.resident_id,
					network_client_resident.resident_host,
					network_client_resident.resident_service,
					network_client_resident.resident_block,
					network_client_resident.resident_internal_port,
					provide_config.provide_mode
				FROM network_client
				LEFT JOIN network_client_resident ON
					network_client.client_id = network_client_resident.client_id
				LEFT JOIN provide_config ON
					network_client.client_id = provide_config.client_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true
			`,
			session.NetworkId,
		)
		clientInfos := map[Id]*NetworkClientInfo{}
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				clientInfo := &NetworkClientInfo{}
				bringyour.Raise(
					result.Scan(
						&clientInfo.ClientId,
						&clientInfo.Description,
						&clientInfo.DeviceSpec,
						&clientInfo.CreateTime,
						&clientInfo.AuthTime,
						&clientInfo.ResidentId,
						&clientInfo.ResidentHost,
						&clientInfo.ResidentService,
						&clientInfo.ResidentBlock,
						&clientInfo.ResidentInternalPort,
						&clientInfo.ProvideMode,
					),
				)
				clientInfos[clientInfo.ClientId] = clientInfo
			}
		})

		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client.connection_id,
					network_client.connect_time,
					network_client.disconnect_time,
					network_client.connection_host,
					network_client.connection_service,
					network_client.connection_block,
				FROM network_client
				LEFT JOIN network_client_connection ON
					network_client.client_id = network_client_connection.client_id AND 
					network_client_connection.connected
				WHERE
					network_client.network_id = %s AND
					network_client.active = true
			`,
			session.NetworkId,
		)
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				clientConnection := &NetworkClientConnection{}
				bringyour.Raise(
					result.Scan(
						&clientConnection.ConnectionId,
						&clientConnection.ConnectTime,
						&clientConnection.DisconnectTime,
						&clientConnection.ConnectionHost,
						&clientConnection.ConnectionService,
						&clientConnection.ConnectionBlock,
					),
				)
				if clientInfo, ok := clientInfos[clientConnection.ClientId]; ok {
					if clientInfo.Connections == nil {
						clientInfo.Connections = []*Connections{}
					}
					clientInfo.Connections = append(clientInfo.Connections, clientConnection)
				} else {
					LOG("Read appears to be inconsistent.")
				}
			}
		})

		clients := map[string]*NetworkClientInfo{}
		clientsResult = &NetworkClientsResult{
			Clients: clients,
		}
	})

	return clientsResult, clientsErr
}



type NetworkClient struct {
	ClientId Id `json:"clientId"`
	NetworkId Id `json:"networkId"`
	Description string `json:"description"`
	DeviceSpec string `json:"deviceSpec"`

	CreateTime time.Time `json:"createTime"`
	AuthTime time.Time `json:"authTime"`
}


func GetNetworkClient(clientId Id) *NetworkClient {
	var networkClient *NetworkClient

	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id,
					description,
					device_spec,
					create_time,
					auth_time
				FROM network_client
				WHERE
					client_id = $1 AND
					networkd_id = $2 AND
					active = true
			`,
			clientId,
		)
		bringyour.WithDbResult(result, err, func() {
			if result.Next() {
				networkClient = &NetworkClient{
					ClientId: clientId,
				}
				bringyour.Raise(
					result.Scan(
						&networkClient.NetworkId,
						&networkClient.Description,
						&networkClient.DeviceSpec,
						&networkClient.CreateTime,
						&networkClient.DeviceSpec,
					)
				)
			}
		})
	})

	return networkClient
}




func GetProvideMode(clientId Id) *ProvideMode {
	var provideMode *ProvideMode

	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT provide_mode FROM provide_config
				WHERE client_id = $1
			`,
			clientId,
		)
		bringyour.WithDbResult(result, err, func() {
			if result.Next() {
				var provideModeValue string
				bringyour.Raise(result.Scan(&provideModeValue))
				provideMode = &ProvideMode(provideModeValue)
			}
		})
	})

	return provideMode
}



// a client_id can have multiple connections to the platform
// each connection forms a transmit for the resident transport
// there is one resident transport
// if connect to the resident transport fails,
// attempt claim local resident and start resident locally
// if attempt claim fails, connect to the next (repeat until a successful connection)



// returns a connection_id
func ConnectNetworkClient(clientId Id) Id {
	var connectionId Id

	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		connectionId = bringyour.NewId()
		connectTime := time.Now()

		_, err := conn.Exec(
			ctx,
			`
				INSERT INTO network_client_connection (
					client_id,
					connection_id,
					connect_time,
					connection_host,
					connection_service,
					connection_block
				)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			clientId,
			connectionId,
			connectTime,
			bringyour.RequireHost(),
			bringyour.RequireService(),
			bringyour.RequireBlock(),
		)
		bringyour.Raise(err)
	})

	return connectionId, nil
}


func DisconnectNetworkClient(connectionId Id) error {
	var disconnectErr error

	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		disconnectTime := time.Now()
		tag, err := conn.Exec(
			ctx,
			`
				UPDATE network_client_connection
				SET
					connected = false,
					disconnect_time = $1
				WHERE
					connection_id = $2
			`,
			connectionId,
			disconnectTime,
		)
		bringyour.Raise(err)
		if tag.RowsAffected() != 1 {
			disconnectErr = errors.New("Connection does not exist.")
			return
		}
	})

	return disconnectErr
}


// the resident is a transport client that runs on the platform on behalf of a client
// there is at most one resident per client, which is self-nominated by any endpoint
// the nomination happens when the endpoint cannot communicate with the current resident

type NetworkClientResident struct {
	ClientId Id `json:"clientId"`
	InstanceId Id `json:"clientId"`
	ResidentId Id `json:"residentId"`
	ResidentHost string `json:"residentHost"`
	ResidentService string `json:"residentService"`
	ResidentBlock string `json:"residentBlock"`
	ResidentInternalPorts []int `json:"residentInternalPorts"`
}


func dbGetResident(ctx context.Context, conn bringyour.PgConn, clientId Id) *NetworkClientResident {
	var resident *NetworkClientResident

	result, err := conn.Query(
		ctx,
		`
			SELECT
				instance_id,
				resident_id,
				resident_host,
				resident_service,
				resident_block,
			FROM network_client_resident
			WHERE client_id = $1
		`,
		clientId,
	)
	bringyour.WithDbResult(result, err, func() {
		if result.Next() {
			resident = &NetworkClientResident{
				ClientId: clientId,
			}
			bringyour.Raise(
				result.Scan(
					&resident.InstanceId,
					&resident.ResidentId,
					&resident.ResidentHost,
					&resident.ResidentService,
					&resident.ResidentBlock,
					&resident.ResidentInternalPort,
				)
			)
		}
	})
	if resident == nil {
		return
	}

	// join in internal ports
	result, err = conn.Query(
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
	bringyour.WithDbResult(result, err, func() {
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


func GetResident(clientId Id) *NetworkClientResident {
	var resident *NetworkClientResident

	// important: use serializable tx
	bringyour.Tx(session.Ctx, func(conn bringyour.PgConn) {
		resident = dbGetResident(session.Ctx, conn, clientId)
	}, bringyour.TxSerializable)

	return resident
}


// replace an existing resident with the given, or if there was already a replacement, return it
func NominateResident(residentIdToReplace *Id, nomination *NetworkClientResident) *NetworkClientResident {
	var resident *NetworkClientResident

	// important: use serializable tx
	bringyour.Tx(session.Ctx, func(conn bringyour.PgConn) {
		resident = dbGetResident(session.Ctx, conn, nomination.ClientId)

		if resident != nil && resident.ResidentId != residentIdToReplace {
			// already replaced
			return
		}

		nomination.ResidentId := bringyour.NewId()
		_, err := conn.Exec(
			ctx,
			`
				INSERT INTO network_client_resident (
					instance_id,
					resident_id,
					resident_host,
					resident_service,
					resident_block
				)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT UPDATE
				SET
					instance_id = $1
					resident_id = $2
					resident_host = $3
					resident_service = $4
					resident_block = $5
			`,
			nomination.InstanceId,
			nomination.ResidentId,
			nomination.ResidentHost,
			nomination.ResidentService,
			nomination.ResidentBlock,
		)
		bringyour.Raise(err)

		for _, port := range nomination.ResidentInternalPorts {
			_, err = conn.Exec(
				ctx,
				`
					INSERT INTO network_client_resident_port (
						client_id,
						resident_id,
						resident_internal_port,
					)
					VALUES ($1, $2, $3)
				`,
				nomination.clientId,
				nomination.ResidentId,
				port,
			)
			bringyour.Raise(err)
		}

		resident = nomination
	}, bringyour.TxSerializable)

	return resident
}


// if any of the ports overlap
func GetResidentsForHostPort(ctx context.Context, host string, port int) []*NetworkClientResident {residents := []*NetworkClientResident{}
	residents := []*NetworkClientResident{}

	bringyour.Tx(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_client_resident.client_id
				FROM network_client_resident

				INNER JOIN network_client_resident_port ON
					network_client_resident_port.client_id = network_client_resident.client_id AND
					network_client_resident_port.resident_id = network_client_resident.resident_id AND
					network_client_resident_port.resident_internal_port = $2

				WHERE
					resident_host = $1
				`,
			host,
			port,
		)
		clientIds := []Id{}
		bringyour.WithDbResult(result, err, func() {
			for result.Next() {
				var clientId Id
				bringyour.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})

		for _, clientId := range clientIds {
			resident := dbGetResident(ctx, conn, clientId)
			residents = append(residents, resident)
		}
	}, bringyour.TxSerializable)
	
	return residents
}


func RemoveResident(clientId Id, residentId Id) {
	bringyour.Tx(ctx, func(conn bringyour.PgConn) {
		_, err = conn.Exec(
			ctx,
			`
			UPDATE network_client_resident
			SET
				instance_id = NULL,
				resident_id = NULL,
				resident_host = NULL,
				resident_service = NULL,
				resident_block = NULL
			WHERE
				client_id = $1 AND
				resident_id = $2
			`,
			clientId,
			residentId,
		)

		_, err = conn.Exec(
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
		
	}, bringyour.TxSerializable)
}



