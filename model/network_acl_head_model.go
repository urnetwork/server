package model

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// §8.17's network_acl_head (§2.8 api-7): for each network with a config repo,
// the newest verified position n of its authorization log, that entry's chain
// digest A_n, and whether its rows hold a document. api's append path raises it
// after each entry it verifies (TEAM-M2-13), and §19.10's restore step from
// verified proj/ entries before the exchange and CreateContract reopen
// (TEAM-M3-02's AdvanceAclHeadsAfterRestore). Nothing lowers it: a position
// at or below the head changes nothing, and a second A_n at the head's own n is
// a fork, refused and counted (§8.9, I-16). The row goes with its network.
//
// The head belongs to migration group MG2, pending until its Gn-out. A binary
// that does not link MG2 refuses to write it and reads no head, so every
// network costs the gate its one map miss (server.SidecarMg2Linked).

// One network's head.
type NetworkAclHead struct {
	NetworkId server.Id
	// n, the projection version
	Version uint64
	// A_n, 32 bytes
	Chain       []byte
	HasDocument bool
}

// Refused: this binary does not link MG2, so the table does not exist for it.
var ErrNetworkAclHeadUnavailable = errors.New("network acl heads are not available")

// Refused: the position is no log position, or the digest no SHA-256.
var ErrNetworkAclHeadInvalid = errors.New("invalid network acl head")

// Refused and counted: a second chain digest at the head's own position.
var ErrNetworkAclHeadFork = errors.New("network acl head fork")

var networkAclHeadForkCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "acl",
	Name:      "head_forks_total",
	Help:      "Network acl head advances refused because a second chain digest arrived at the head's own position (§8.9, I-16)",
})

func init() {
	prometheus.MustRegister(networkAclHeadForkCounter)
}

// Raises networkId's head to (version, chain) with hasDocument, when version is
// above the head's or the network has none. Reports whether this call raised
// it. A version at or below the head changes nothing, and the head's own
// version with another chain is ErrNetworkAclHeadFork.
func AdvanceNetworkAclHead(ctx context.Context, networkId server.Id, version uint64, chain []byte, hasDocument bool) (advanced bool, returnErr error) {
	if !server.SidecarMg2Linked() {
		return false, ErrNetworkAclHeadUnavailable
	}
	server.Tx(ctx, func(tx server.PgTx) {
		advanced, returnErr = AdvanceNetworkAclHeadInTx(ctx, tx, networkId, version, chain, hasDocument)
	})
	return
}

// AdvanceNetworkAclHead in the caller's transaction, for api's append path and
// the restore step, which advance it beside their own writes.
func AdvanceNetworkAclHeadInTx(ctx context.Context, tx server.PgTx, networkId server.Id, version uint64, chain []byte, hasDocument bool) (bool, error) {
	if !server.SidecarMg2Linked() {
		return false, ErrNetworkAclHeadUnavailable
	}
	if version == 0 || 1<<63 <= version || len(chain) != sha256.Size {
		return false, ErrNetworkAclHeadInvalid
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_acl_head (
				network_id,
				version,
				chain,
				has_document
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (network_id) DO UPDATE
			SET
				version = EXCLUDED.version,
				chain = EXCLUDED.chain,
				has_document = EXCLUDED.has_document
			WHERE network_acl_head.version < EXCLUDED.version
		`,
		networkId,
		int64(version),
		chain,
		hasDocument,
	))
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	// the head stands at or above version: only the same position with another
	// digest says anything, and it says fork
	var headVersion int64
	var headChain []byte
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT version, chain
			FROM network_acl_head
			WHERE network_id = $1
		`,
		networkId,
	).Scan(&headVersion, &headChain))
	if uint64(headVersion) == version && string(headChain) != string(chain) {
		networkAclHeadForkCounter.Inc()
		glog.Errorf("[acl][%s]a second chain digest at the head's position %d\n", networkId, version)
		return false, ErrNetworkAclHeadFork
	}
	return false, nil
}

// One network's head, or nil when it has none.
func GetNetworkAclHead(ctx context.Context, networkId server.Id) (head *NetworkAclHead, returnErr error) {
	if !server.SidecarMg2Linked() {
		return nil, ErrNetworkAclHeadUnavailable
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT version, chain, has_document
				FROM network_acl_head
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				head = &NetworkAclHead{NetworkId: networkId}
				var version int64
				server.Raise(result.Scan(&version, &head.Chain, &head.HasDocument))
				head.Version = uint64(version)
			}
		})
	})
	return
}

// Every network's head, for ACL-M4-01's 5 s snapshot: a network the map lacks
// has no config repo, and the gate skips it with one miss (R-ACL-8).
func GetNetworkAclHeads(ctx context.Context) (heads map[server.Id]*NetworkAclHead, returnErr error) {
	if !server.SidecarMg2Linked() {
		return nil, ErrNetworkAclHeadUnavailable
	}
	heads = map[server.Id]*NetworkAclHead{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id, version, chain, has_document
				FROM network_acl_head
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				head := &NetworkAclHead{}
				var version int64
				server.Raise(result.Scan(&head.NetworkId, &version, &head.Chain, &head.HasDocument))
				head.Version = uint64(version)
				heads[head.NetworkId] = head
			}
		})
	})
	return
}

// Removes networkId's head in the network's deletion transaction, the one way
// the row goes.
func removeNetworkAclHeadInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_acl_head
			WHERE network_id = $1
		`,
		networkId,
	))
}
