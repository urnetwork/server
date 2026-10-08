package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/server"
)

// The Embed plan's per-network top-level client limit (EMBED1.md).
//
// A network without a `network_top_level_client_limit` row uses the default
// `LimitTopLevelClientIdsPerNetwork`. Ops sets a row for a network on an Embed
// plan (`bringyourctl network client-limit`). Only the AuthNetworkClient create
// cap reads it: the peer valve (peer_model.go) keeps the constant, so a large
// embedded network keeps its peer list off and its users stay invisible to
// each other.

// The largest limit ops may set. The create cap counts with `LIMIT limit+1`,
// so the bound also bounds that scan.
const MaxNetworkTopLevelClientLimit = 10_000_000

var ErrNetworkNotFound = errors.New("Network does not exist.")

type NetworkTopLevelClientLimit struct {
	NetworkId server.Id
	Limit     int
	// false when the network uses the default
	Override bool
}

// networkTopLevelClientLimitInTx reads the limit inside the provisioning
// transaction that enforces it.
func networkTopLevelClientLimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id) int {
	limit := LimitTopLevelClientIdsPerNetwork
	result, err := tx.Query(
		ctx,
		`
			SELECT top_level_client_limit
			FROM network_top_level_client_limit
			WHERE network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&limit))
		}
	})
	return limit
}

func GetNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id) *NetworkTopLevelClientLimit {
	limit := &NetworkTopLevelClientLimit{
		NetworkId: networkId,
		Limit:     LimitTopLevelClientIdsPerNetwork,
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT top_level_client_limit
				FROM network_top_level_client_limit
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&limit.Limit))
				limit.Override = true
			}
		})
	})
	return limit
}

// SetNetworkTopLevelClientLimit sets the Embed plan limit for a network.
func SetNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id, limit int) error {
	if limit < 1 || MaxNetworkTopLevelClientLimit < limit {
		return fmt.Errorf("The limit must be between 1 and %d.", MaxNetworkTopLevelClientLimit)
	}
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		exists := false
		result, err := tx.Query(ctx, `SELECT true FROM network WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
		if !exists {
			returnErr = ErrNetworkNotFound
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_top_level_client_limit (
					network_id,
					top_level_client_limit,
					update_time
				)
				VALUES ($1, $2, $3)
				ON CONFLICT (network_id) DO UPDATE
				SET
					top_level_client_limit = $2,
					update_time = $3
			`,
			networkId,
			limit,
			server.NowUtc(),
		))
	})
	return returnErr
}

// ClearNetworkTopLevelClientLimit returns the network to the default limit.
func ClearNetworkTopLevelClientLimit(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_top_level_client_limit WHERE network_id = $1`,
			networkId,
		))
	})
}
