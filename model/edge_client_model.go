package model

// An edge block's platform clients in urnetwork-edge (TLS-M5-11; §6.10.1, §13.6; QD-812, QD-873): api's half of
// server.PlatformClientSource. MintEdgeClient answers a short-lived client jwt for the client whose id
// server.DeriveEdgeClientId derives from (environment, host, block, slot, index), creating the client and its
// device in the urnetwork-edge network on first use, so a block's restart reuses its ids and nothing piles up
// there, and a deploy's old and new process, which hold different slots, hold different clients.
//
// The caller, api's internal edge route, has already held the key to config/<env>/edge.yml
// (server.EdgeClientsConfig.Admits). Nothing here takes a client id from a request.
//
// The jwt is a strict_exp client token: it ends at its exp with no grace, on api and on connect, whose transport
// closes at that moment, so a copied one lasts at most JwtLife. It carries no session: like a prober's it is
// signed outside the session registry, since an edge client is the platform's own and no person signs it in or
// out. A client that was removed stays removed: a row that is not active, or that sits in another network or on
// another device, is refused, never revived.
//
// sockets takes no jwt (QD-1029): beside each, api's route answers the client a service token of audience sockets
// (QD-1054, oauth.MintEdgeClientSocketsToken). A removal of an edge client therefore owes its client rev/ object,
// which sockets reads to refuse the token for the rest of its 15 minutes: the chain's own rev effect owes a
// client's object only for a client bound to a sidecar, which no edge client is, so this file registers the
// edge clients' own effect (applyEdgeClientRevocationEffectInTx).

import (
	"context"
	"errors"
	"fmt"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

var (
	// The urnetwork-edge network has no row, so no client can be minted in it.
	ErrEdgeClientNetworkMissing = errors.New("the urnetwork-edge network does not exist")
	// The derived client exists and is not this network's active client on its derived device.
	ErrEdgeClientRetired = errors.New("the edge client was removed or is not urnetwork-edge's")
)

// What every edge client's device and client rows say they are.
const (
	EdgeClientDescription = "urnetwork-edge platform client"
	EdgeClientDeviceSpec  = "edge"
)

// Tunables of the mint.
type EdgeClientMintSettings struct {
	// How long a minted jwt lasts: 1 h. The transport it opened ends with it, so the block's source refreshes
	// before then (server.PlatformClientSourceSettings.RefreshLifeFraction).
	JwtLife time.Duration
	// What each index adds to its jwt's life, index times this: 2 min, so a block's K transports, which all
	// start on jwts minted together, do not all end in the same moment.
	JwtIndexStagger time.Duration
}

func DefaultEdgeClientMintSettings() *EdgeClientMintSettings {
	return &EdgeClientMintSettings{
		JwtLife:         1 * time.Hour,
		JwtIndexStagger: 2 * time.Minute,
	}
}

// One edge client, as server.EdgeClientsConfig admitted it.
type EdgeClientKey struct {
	Environment string
	Block       string
	Host        string
	// The process's deploy slot (QD-873), which the caller has bounded by the key's SlotCount.
	Slot  int
	Index int
}

// A minted edge client.
type EdgeClientMint struct {
	ClientId server.Id
	// A secret: never logged.
	ByClientJwt string
	ExpireTime  time.Time
}

// The client key's jwt in the urnetwork-edge network networkId, with its client and device created on first
// use. settings nil takes the defaults.
func MintEdgeClient(ctx context.Context, networkId server.Id, key *EdgeClientKey, settings *EdgeClientMintSettings) (mint *EdgeClientMint, returnErr error) {
	if settings == nil {
		settings = DefaultEdgeClientMintSettings()
	}
	if key == nil || networkId == (server.Id{}) || key.Environment == "" || key.Block == "" || key.Host == "" || key.Slot < 0 || key.Index < 0 {
		return nil, errors.New("an edge client needs its network, environment, block, host, slot and index")
	}
	if settings.JwtLife <= 0 || settings.JwtIndexStagger < 0 {
		return nil, errors.New("an edge client's jwt needs a positive life")
	}
	clientId := server.DeriveEdgeClientId(key.Environment, key.Host, key.Block, key.Slot, key.Index)
	deviceId := server.DeriveEdgeDeviceId(key.Environment, key.Host, key.Block, key.Slot, key.Index)
	defer func() {
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			mint = nil
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", r)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		mint = nil
		returnErr = nil

		var networkName string
		var adminUserId *server.Id
		networkFound := false
		result, err := tx.Query(ctx, `SELECT network_name, admin_user_id FROM network WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkName, &adminUserId))
				networkFound = true
			}
		})
		if !networkFound || adminUserId == nil {
			returnErr = ErrEdgeClientNetworkMissing
			return
		}

		// serialized on the network's row, so two first asks for one client make one client
		server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM network WHERE network_id = $1 FOR UPDATE`, networkId))

		var clientNetworkId server.Id
		var clientDeviceId *server.Id
		clientActive := false
		clientFound := false
		result, err = tx.Query(ctx, `SELECT network_id, device_id, active FROM network_client WHERE client_id = $1 FOR UPDATE`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&clientNetworkId, &clientDeviceId, &clientActive))
				clientFound = true
			}
		})
		now := server.NowUtc()
		if clientFound {
			if clientNetworkId != networkId || clientDeviceId == nil || *clientDeviceId != deviceId || !clientActive {
				returnErr = ErrEdgeClientRetired
				return
			}
		} else {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO device (device_id, network_id, device_name, device_spec) VALUES ($1, $2, $3, $4) ON CONFLICT (device_id) DO NOTHING`,
				deviceId,
				networkId,
				EdgeClientDescription,
				EdgeClientDeviceSpec,
			))
			// the device must be this network's: a derived id that met another network's row is refused
			var deviceNetworkId server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT network_id FROM device WHERE device_id = $1`, deviceId).Scan(&deviceNetworkId))
			if deviceNetworkId != networkId {
				returnErr = ErrEdgeClientRetired
				return
			}
			// an edge client is top-level, its own subject (D-ACL-3)
			subjectColumn, subjectValue := NetworkClientAclSubjectInsert("$1")
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time`+subjectColumn+`) VALUES ($1, $2, $3, $4, $5, $5`+subjectValue+`)`,
				clientId,
				networkId,
				deviceId,
				EdgeClientDescription,
				now,
			))
		}

		expireTime := now.Add(settings.JwtLife + time.Duration(key.Index)*settings.JwtIndexStagger)
		credential := session.NewByJwt(networkId, *adminUserId, networkName, false, false).Client(deviceId, clientId)
		// ends at its exp with no grace, wherever it is read
		credential.StrictExp = true
		credential.ExpiresAt = gojwt.NewNumericDate(expireTime)
		signed, err := session.SignInternalProbeInTx(ctx, tx, credential)
		if err != nil {
			returnErr = err
			return
		}
		mint = &EdgeClientMint{
			ClientId:    clientId,
			ByClientJwt: signed,
			// as the jwt carries it, in whole seconds
			ExpireTime: credential.ExpiresAt.Time,
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return mint, nil
}

// The name the edge clients' rev/ effect registers under.
const RemovalEffectEdgeClientRev = "rev-edge-client"

func init() {
	RegisterRemovalEffect(RemovalEffectEdgeClientRev, applyEdgeClientRevocationEffectInTx)
}

// Owes a rev/ object for each client the removal ends in urnetwork-edge, as sidecar.yml names it: each may hold
// a sockets token api's edge client route answered it (QD-1054), whose network_id is that network, so its object
// is the one sockets reads for it. Clients of every other network are the chain's own rev effect's. With a good
// read the file is the only source: naming no urnetwork_edge, it owes nothing, since sockets then takes no edge
// caller's token at all. An absent file owes nothing too: it is no failed read, and names no edge network to
// protect.
//
// A failed first read (server.SidecarConfig.FailedFirstRead; QD-1079) names no urnetwork_edge whatever the file
// says, while sockets, which keeps a good read through a broken push, may still take the edge's tokens, so owing
// nothing would leave a removed edge client's token honored for the rest of its 15 minutes, against I-15. Under
// that mark the effect owes the object for each removed client whose device is an edge device (EdgeClientDeviceSpec,
// which MintEdgeClient sets on every edge client's device), in the client's own network, the one its row and so its
// token name (QD-1114). One query in the removal's transaction reads which clients those are, and it runs only under
// the mark. A user's device that names itself edge owes a harmless extra object then, for a client already removed.
func applyEdgeClientRevocationEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	clients := removal.SubjectsOf(sidecar.RevocationKindClient)
	if len(clients) == 0 {
		return nil
	}
	sidecarConfig := server.CurrentSidecarConfig()
	edgeClients := []*RemovalSubject{}
	if edgeNetworkId, ok := sidecarConfig.UrnetworkEdge(); ok {
		for _, client := range clients {
			if client.NetworkId == edgeNetworkId {
				edgeClients = append(edgeClients, client)
			}
		}
	} else if sidecarConfig.FailedFirstRead() {
		clientIds := make([]server.Id, 0, len(clients))
		for _, client := range clients {
			clientIds = append(clientIds, client.SubjectId)
		}
		result, err := tx.Query(
			ctx,
			`
				SELECT network_client.client_id, network_client.network_id
				FROM network_client
				JOIN device ON device.device_id = network_client.device_id
				WHERE
					network_client.client_id = ANY($1) AND
					device.device_spec = $2
			`,
			clientIds,
			EdgeClientDeviceSpec,
		)
		if err != nil {
			return err
		}
		for result.Next() {
			client := &RemovalSubject{Kind: sidecar.RevocationKindClient}
			if err := result.Scan(&client.SubjectId, &client.NetworkId); err != nil {
				result.Close()
				return err
			}
			edgeClients = append(edgeClients, client)
		}
		result.Close()
		if err := result.Err(); err != nil {
			return err
		}
	}
	if len(edgeClients) == 0 {
		return nil
	}
	if removal.Time.IsZero() {
		return errors.New("a removal with no time")
	}
	notBefore := SidecarRevocationNotBefore(removal.Time, DefaultSidecarRevSettings())
	revocations := make([]*SidecarRevocation, 0, len(edgeClients))
	for _, client := range edgeClients {
		revocations = append(revocations, &SidecarRevocation{
			NetworkId: client.NetworkId,
			Kind:      sidecar.RevocationKindClient,
			SubjectId: client.SubjectId,
			NotBefore: notBefore,
			Reason:    removal.Reason,
		})
	}
	WriteRevocations(ctx, tx, revocations)
	return nil
}
