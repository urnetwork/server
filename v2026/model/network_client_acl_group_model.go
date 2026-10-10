package model

import (
	"context"
	"fmt"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Per-client ACL groups (EMBED1.md).
//
// A top-level client is in the "default" group unless its network's root
// credential puts it in "isolated". An isolated client never appears in the
// network's peer list (GET /network/peers, the peer listener and its key-event
// deltas), receives no peer list itself, and does not count toward the peer
// valve's recently active top-level clients (NetworkPeersEnabled). It still
// counts toward the client limit and the concurrent connection limit: it is
// registered in the counted-but-never-listed zset that hosted proxy clients use
// (NetworkPeerCategoryIsolated).
//
// Only a non-default group is stored (network_client_acl_group); no row means
// "default". A change takes effect promptly: the client's peer registration is
// moved at once, and its resident is retired so its next connection registers
// with the new group.

const (
	NetworkClientAclGroupDefault  = "default"
	NetworkClientAclGroupIsolated = "isolated"
)

const networkClientAclGroupSessionMessage = "Requires the network's root token or an API key."

// parseNetworkClientAclGroup accepts exactly the two group names.
func parseNetworkClientAclGroup(value string) (aclGroup string, ok bool) {
	switch value {
	case NetworkClientAclGroupDefault, NetworkClientAclGroupIsolated:
		return value, true
	default:
		return "", false
	}
}

type SetNetworkClientAclGroupArgs struct {
	ClientId server.Id `json:"client_id"`
	AclGroup string    `json:"acl_group"`
}

type GetNetworkClientAclGroupArgs struct {
	// optional for a client token
	ClientId string
}

type NetworkClientAclGroup struct {
	ClientId server.Id `json:"client_id"`
	AclGroup string    `json:"acl_group"`
}

type NetworkClientAclGroupError struct {
	Message string `json:"message"`
}

// On success the group fields; on a refusal only `error`.
type NetworkClientAclGroupResult struct {
	*NetworkClientAclGroup
	Error *NetworkClientAclGroupError `json:"error,omitempty"`
}

func networkClientAclGroupErrorResult(message string) *NetworkClientAclGroupResult {
	return &NetworkClientAclGroupResult{Error: &NetworkClientAclGroupError{Message: message}}
}

// readNetworkClientAclGroup returns the client's stored group, or "default".
func readNetworkClientAclGroup(ctx context.Context, query server.PgCanQuery, clientId server.Id) string {
	aclGroup := NetworkClientAclGroupDefault
	result, err := query.Query(
		ctx,
		`
			SELECT acl_group
			FROM network_client_acl_group
			WHERE client_id = $1
		`,
		clientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&aclGroup))
		}
	})
	return aclGroup
}

// GetNetworkClientAclGroupForClient reads a client's group without a session,
// for callers inside the server.
func GetNetworkClientAclGroupForClient(ctx context.Context, clientId server.Id) (aclGroup string) {
	server.Db(ctx, func(conn server.PgConn) {
		aclGroup = readNetworkClientAclGroup(ctx, conn, clientId)
	})
	return
}

// SetNetworkClientAclGroup sets a top-level client's ACL group
// (POST /network/client-acl-group). Only a network session — the root JWT or an
// API key — may set it, for a client of its own network.
// Refused unless the caller's network is Embed-enabled (network_embed_model.go).
func SetNetworkClientAclGroup(setAclGroup *SetNetworkClientAclGroupArgs, clientSession *session.ClientSession) (*NetworkClientAclGroupResult, error) {
	if networkEmbedRefused(clientSession) {
		return networkClientAclGroupErrorResult(NetworkEmbedNotEnabledMessage), nil
	}
	if !clientDataCapNetworkSession(clientSession) {
		return networkClientAclGroupErrorResult(networkClientAclGroupSessionMessage), nil
	}
	if setAclGroup.ClientId == (server.Id{}) {
		return networkClientAclGroupErrorResult("client_id is required."), nil
	}
	aclGroup, ok := parseNetworkClientAclGroup(setAclGroup.AclGroup)
	if !ok {
		return networkClientAclGroupErrorResult(`acl_group must be "default" or "isolated".`), nil
	}

	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	clientId := setAclGroup.ClientId

	var setResult *NetworkClientAclGroupResult
	changed := false
	server.Tx(ctx, func(tx server.PgTx) {
		setResult = nil
		changed = false

		_, topLevel, found := clientDataCapTopLevelClient(ctx, tx, networkId, clientId)
		if !found {
			setResult = networkClientAclGroupErrorResult("Client not found in this network.")
			return
		}
		if !topLevel {
			setResult = networkClientAclGroupErrorResult("ACL groups apply to top-level clients.")
			return
		}

		previousAclGroup := readNetworkClientAclGroup(ctx, tx, clientId)
		changed = previousAclGroup != aclGroup

		if aclGroup == NetworkClientAclGroupDefault {
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_client_acl_group WHERE client_id = $1`,
				clientId,
			))
		} else {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client_acl_group (
						client_id,
						network_id,
						acl_group,
						update_time
					)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (client_id) DO UPDATE
					SET
						acl_group = $3,
						update_time = $4
				`,
				clientId,
				networkId,
				aclGroup,
				server.NowUtc(),
			))
		}

		setResult = &NetworkClientAclGroupResult{
			NetworkClientAclGroup: &NetworkClientAclGroup{
				ClientId: clientId,
				AclGroup: aclGroup,
			},
		}
	})

	if changed && setResult != nil && setResult.Error == nil {
		applyNetworkClientAclGroupChange(ctx, networkId, clientId, aclGroup)
	}
	return setResult, nil
}

// GetNetworkClientAclGroup reads a client's ACL group
// (GET /network/client-acl-group). A network session reads any top-level
// client of its network; a client token reads its own (a child client's token
// reads its top-level client's).
// Refused unless the caller's network is Embed-enabled (network_embed_model.go).
func GetNetworkClientAclGroup(getAclGroup *GetNetworkClientAclGroupArgs, clientSession *session.ClientSession) (*NetworkClientAclGroupResult, error) {
	if networkEmbedRefused(clientSession) {
		return networkClientAclGroupErrorResult(NetworkEmbedNotEnabledMessage), nil
	}
	if clientSession == nil || clientSession.ByJwt == nil || clientSession.ByJwt.NetworkId == (server.Id{}) {
		return networkClientAclGroupErrorResult(networkClientAclGroupSessionMessage), nil
	}
	var requestedClientId *server.Id
	if getAclGroup.ClientId != "" {
		clientId, err := server.ParseId(getAclGroup.ClientId)
		if err != nil {
			return networkClientAclGroupErrorResult("Invalid client_id."), nil
		}
		requestedClientId = &clientId
	}

	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId

	var getResult *NetworkClientAclGroupResult
	server.Db(ctx, func(conn server.PgConn) {
		var targetClientId server.Id
		if clientSession.ByJwt.ClientId == nil {
			if requestedClientId == nil {
				getResult = networkClientAclGroupErrorResult("client_id is required.")
				return
			}
			_, topLevel, found := clientDataCapTopLevelClient(ctx, conn, networkId, *requestedClientId)
			if !found {
				getResult = networkClientAclGroupErrorResult("Client not found in this network.")
				return
			}
			if !topLevel {
				getResult = networkClientAclGroupErrorResult("ACL groups apply to top-level clients.")
				return
			}
			targetClientId = *requestedClientId
		} else {
			ownClientId := *clientSession.ByJwt.ClientId
			topLevelClientId, _, found := clientDataCapTopLevelClient(ctx, conn, networkId, ownClientId)
			if !found {
				getResult = networkClientAclGroupErrorResult("Client not found in this network.")
				return
			}
			if requestedClientId != nil && *requestedClientId != ownClientId && *requestedClientId != topLevelClientId {
				getResult = networkClientAclGroupErrorResult("A client token can only read its own ACL group.")
				return
			}
			targetClientId = topLevelClientId
		}
		getResult = &NetworkClientAclGroupResult{
			NetworkClientAclGroup: &NetworkClientAclGroup{
				ClientId: targetClientId,
				AclGroup: readNetworkClientAclGroup(ctx, conn, targetClientId),
			},
		}
	})
	return getResult, nil
}

// applyNetworkClientAclGroupChange moves a client's peer registration to its
// new group after the change commits, then retires the client's resident so
// its next connection registers with the new group (a resident caches its peer
// category for its life, and its poll closes it once its record is gone).
func applyNetworkClientAclGroupChange(ctx context.Context, networkId server.Id, clientId server.Id, aclGroup string) {
	// the valve count excludes isolated clients
	networkPeersEnabledCache.Remove(networkId)

	server.HandleError(func() {
		switch aclGroup {
		case NetworkClientAclGroupIsolated:
			// drop it from the peer list with no disconnect marker, so it is
			// not reported as recently disconnected either
			isolateNetworkPeer(ctx, networkId, clientId)
		default:
			RemoveNetworkIsolatedPeer(ctx, networkId, clientId)
		}

		if resident := GetResidentForClient(ctx, clientId, 0); resident != nil {
			RemoveResidentForClient(ctx, clientId, resident.ResidentId)
		}
	})
}

// Testing_SetNetworkClientAclGroup writes a group directly, for tests that set
// up peers without a session.
func Testing_SetNetworkClientAclGroup(ctx context.Context, networkId server.Id, clientId server.Id, aclGroup string) {
	if _, ok := parseNetworkClientAclGroup(aclGroup); !ok {
		panic(fmt.Errorf("invalid acl group: %s", aclGroup))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if aclGroup == NetworkClientAclGroupDefault {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_acl_group WHERE client_id = $1`, clientId))
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_acl_group (client_id, network_id, acl_group, update_time)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (client_id) DO UPDATE SET acl_group = $3, update_time = $4
			`,
			clientId,
			networkId,
			aclGroup,
			server.NowUtc(),
		))
	})
	networkPeersEnabledCache.Remove(networkId)
}
