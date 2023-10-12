package model

import (
	"errors"
	"context"
	// "bytes"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/ulid"
	// "bringyour.com/bringyour/jwt"
)


const LimitClientIdsPer24Hours = 1024
const LimitClientIdsPerNetwork = 128


type ProvideMode = string
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


type AuthNetworkClientArgs struct {
	// if omitted, a new client_id is created
	ClientId *bringyour.Id `json:"clientId",omitempty`
	Description string `json:"description"`
	DeviceSpec string `json:"deviceSpec"`
}

type AuthNetworkClientResult struct {
	ByJwt *string `json:"byJwt,omitempty"`
	Error *AuthNetworkClientError `json:"error,omitempty"`
}

type AuthNetworkClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool `json:"clientLimitExceeded"` 
	Message string `json:"message"`
}

func AuthNetworkClient(
	authClient AuthNetworkClientArgs,
	session *session.ClientSession,
) (*AuthNetworkClientResult, error) {
	if session == nil {
		return nil, errors.New("Auth required")
	}

	var authClientResult *AuthNetworkClientResult
	var authClientError error

	if authClient.ClientId == nil {
		// important: use serializable tx for rate limits
		bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
			result, err := tx.Query(
				session.Ctx,
				`
					SELECT COUNT(client_id) FROM network_client
					WHERE network_id = $1 AND $1 <= create_time
				`,
				session.ByJwt.NetworkId,
				time.Now(),
			)
			var last24HourCount int
			bringyour.WithPgResult(result, err, func() {
				result.Next()
				bringyour.Raise(result.Scan(&last24HourCount))
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

			clientId := bringyour.NewId()

			_, err = tx.Exec(
				session.Ctx,
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
				session.ByJwt.NetworkId,
				authClient.Description,
				authClient.DeviceSpec,
				time.Now(),
			)
			bringyour.Raise(err)

			byJwtWithClientId := session.ByJwt.WithClientId(&clientId).Sign()
			authClientResult = &AuthNetworkClientResult{
				ByJwt: &byJwtWithClientId,
			}
		}, bringyour.TxSerializable)
	} else {
		// important: must check `network_id = session network_id`
		bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
			tag, err := tx.Exec(
				session.Ctx,
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
				authClient.ClientId,
				session.ByJwt.NetworkId,
			)
			bringyour.Raise(err)
			if tag.RowsAffected() != 1 {
				authClientResult = &AuthNetworkClientResult{
					Error: &AuthNetworkClientError{
						Message: "Client does not exist.",
					},
				}
				return
			}

			byJwtWithClientId := session.ByJwt.WithClientId(authClient.ClientId).Sign()
			authClientResult = &AuthNetworkClientResult{
				ByJwt: &byJwtWithClientId,
			}
		})
	}

	return authClientResult, authClientError
}


type RemoveNetworkClientArgs struct {
	ClientId bringyour.Id `json:"clientId"`
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
	if session == nil {
		return nil, errors.New("Auth required")
	}

	var removeClientResult *RemoveNetworkClientResult
	var removeClientErr error

	// important: must check `network_id = session network_id`
	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
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

type NetworkClientConnection struct {
	ClientId bringyour.Id
	ConnectionId bringyour.Id
	ConnectTime time.Time
	DisconnectTime time.Time
	ConnectionHost string
	ConnectionService string
	ConnectionBlock string
}

func GetNetworkClients(session *session.ClientSession) (*NetworkClientsResult, error) {
	if session == nil {
		return nil, errors.New("Auth required")
	}

	var clientsResult *NetworkClientsResult
	var clientsErr error

	bringyour.Db(session.Ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			session.Ctx,
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
			session.ByJwt.NetworkId,
		)
		clientInfos := map[bringyour.Id]*NetworkClientInfo{}
		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				clientInfo := &NetworkClientInfo{}
				bringyour.Raise(result.Scan(
					&clientInfo.NetworkClient.ClientId,
					&clientInfo.Description,
					&clientInfo.DeviceSpec,
					&clientInfo.CreateTime,
					&clientInfo.AuthTime,
					&clientInfo.ResidentId,
					&clientInfo.ResidentHost,
					&clientInfo.ResidentService,
					&clientInfo.ResidentBlock,
					&clientInfo.ProvideMode,
				))
				clientInfos[clientInfo.NetworkClient.ClientId] = clientInfo
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
					network_client_resident_port.resident_id = etwork_client_resident.resident_id
				
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
					clientInfo.ResidentInternalPorts = append(clientInfo.ResidentInternalPorts, port)
				}
			}
		})

		result, err = conn.Query(
			session.Ctx,
			`
				SELECT
					network_client.client_id,
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

		clients := map[string]*NetworkClientInfo{}
		clientsResult = &NetworkClientsResult{
			Clients: clients,
		}
	})

	return clientsResult, clientsErr
}



type NetworkClient struct {
	ClientId bringyour.Id `json:"clientId"`
	NetworkId bringyour.Id `json:"networkId"`
	Description string `json:"description"`
	DeviceSpec string `json:"deviceSpec"`

	CreateTime time.Time `json:"createTime"`
	AuthTime time.Time `json:"authTime"`
}


func GetNetworkClient(ctx context.Context, clientId bringyour.Id) *NetworkClient {
	var networkClient *NetworkClient

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				networkClient = &NetworkClient{
					ClientId: clientId,
				}
				bringyour.Raise(result.Scan(
					&networkClient.NetworkId,
					&networkClient.Description,
					&networkClient.DeviceSpec,
					&networkClient.CreateTime,
					&networkClient.DeviceSpec,
				))
			}
		})
	})

	return networkClient
}




func GetProvideMode(ctx context.Context, clientId bringyour.Id) *ProvideMode {
	var provideMode *ProvideMode

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT provide_mode FROM provide_config
				WHERE client_id = $1
			`,
			clientId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {
				var provideModeValue string
				bringyour.Raise(result.Scan(&provideModeValue))
				provideMode_ := ProvideMode(provideModeValue)
				provideMode = &provideMode_
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
func ConnectNetworkClient(
	ctx context.Context,
	clientId bringyour.Id,
	clientAddress string,
) bringyour.Id {
	var connectionId bringyour.Id

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
	})

	return connectionId
}


func DisconnectNetworkClient(ctx context.Context, connectionId bringyour.Id) error {
	var disconnectErr error

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		disconnectTime := time.Now()
		tag, err := tx.Exec(
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


func IsNetworkClientConnected(ctx context.Context, connectionId bringyour.Id) bool {
	connected := false

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
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
	})

	return connected
}


// the resident is a transport client that runs on the platform on behalf of a client
// there is at most one resident per client, which is self-nominated by any endpoint
// the nomination happens when the endpoint cannot communicate with the current resident

type NetworkClientResident struct {
	ClientId bringyour.Id `json:"clientId"`
	InstanceId bringyour.Id `json:"clientId"`
	ResidentId bringyour.Id `json:"residentId"`
	ResidentHost string `json:"residentHost"`
	ResidentService string `json:"residentService"`
	ResidentBlock string `json:"residentBlock"`
	ResidentInternalPorts []int `json:"residentInternalPorts"`
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
				resident_block,
			FROM network_client_resident
			WHERE client_id = $1
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


func GetResident(ctx context.Context, clientId bringyour.Id) *NetworkClientResident {
	var resident *NetworkClientResident

	// important: use serializable tx
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		resident = dbGetResidentInTx(ctx, tx, clientId)
	}, bringyour.TxSerializable)

	return resident
}


func GetResidentWithInstance(ctx context.Context, clientId bringyour.Id, instanceId bringyour.Id) *NetworkClientResident {
	var resident *NetworkClientResident

	// important: use serializable tx
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		resident_ := dbGetResidentInTx(ctx, tx, clientId)
		if resident.InstanceId == instanceId {
			resident = resident_
		}
	}, bringyour.TxSerializable)

	return resident
}


// replace an existing resident with the given, or if there was already a replacement, return it
func NominateResident(
	ctx context.Context,
	residentIdToReplace *bringyour.Id,
	nomination *NetworkClientResident,
) *NetworkClientResident {
	var resident *NetworkClientResident

	// important: use serializable tx
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		resident = dbGetResidentInTx(ctx, tx, nomination.ClientId)

		if resident != nil && (residentIdToReplace == nil || resident.ResidentId != *residentIdToReplace) {
			// already replaced
			return
		}

		nomination.ResidentId = bringyour.NewId()
		_, err := tx.Exec(
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
					instance_id = $1,
					resident_id = $2,
					resident_host = $3,
					resident_service = $4,
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
			_, err = tx.Exec(
				ctx,
				`
					INSERT INTO network_client_resident_port (
						client_id,
						resident_id,
						resident_internal_port,
					)
					VALUES ($1, $2, $3)
				`,
				nomination.ClientId,
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
func GetResidentsForHostPorts(ctx context.Context, host string, ports []int) []*NetworkClientResident {
	residents := []*NetworkClientResident{}

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
					network_client_resident_port.resident_id = network_client_resident.resident_id AND

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
	}, bringyour.TxSerializable)
	
	return residents
}


func RemoveResident(
	ctx context.Context,
	clientId bringyour.Id,
	residentId bringyour.Id,
) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		_, err := tx.Exec(
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
	}, bringyour.TxSerializable)
}

