// sn_earnings_model holds the read paths behind the points-first earnings
// surface (`GET /sn/wallet`, `GET /account/epochs`, `GET /sn/head`,
// `POST /sn/head/binding`) and the once-per-epoch earnings notification.
// Everything here reads settled state; chain writes stay in the st pipeline.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// GetStProviderWalletsForNetwork returns the newest wallet of every provider
// client that ever set one inside the network. The network-level wallet is
// read separately with GetStWallet.
func GetStProviderWalletsForNetwork(ctx context.Context, networkId server.Id) []*StProviderWallet {
	wallets := []*StProviderWallet{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT DISTINCT ON (client_id)
                client_id, network_id, coldkey_ss58, coldkey_pubkey, set_time
            FROM st_provider_wallet_history
            WHERE network_id = $1
            ORDER BY client_id, set_time DESC, wallet_version DESC
        `, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				wallet := &StProviderWallet{}
				var pubkey []byte
				server.Raise(result.Scan(
					&wallet.ClientId,
					&wallet.NetworkId,
					&wallet.ColdkeySs58,
					&pubkey,
					&wallet.SetTime,
				))
				copy(wallet.ColdkeyPubkey[:], pubkey)
				wallets = append(wallets, wallet)
			}
		})
	})
	return wallets
}

// GetFinalizedStEpochs returns finalized epochs, newest first.
func GetFinalizedStEpochs(ctx context.Context, deploymentKey StDeploymentKey, limit int) []*StEpoch {
	key := requireStDeploymentKey(deploymentKey)
	epochs := []*StEpoch{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+stEpochSelectColumns+` FROM st_epoch WHERE deployment_key = $1 AND status = 'finalized' ORDER BY epoch DESC LIMIT $2`,
			key,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				epochs = append(epochs, scanStEpoch(result))
			}
		})
	})
	return epochs
}

// GetStPayoutShareBpsForNetwork sums a network's committed leaf shares per
// epoch. A network with several provider coldkeys holds several leaves in an
// epoch; the sum is the network's share of the operator pool.
func GetStPayoutShareBpsForNetwork(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, noId uint64, epochs []uint64) map[uint64]int {
	key := requireStDeploymentKey(deploymentKey)
	shares := map[uint64]int{}
	if len(epochs) == 0 {
		return shares
	}
	epochInts := make([]int64, len(epochs))
	for i, epoch := range epochs {
		epochInts[i] = int64(epoch)
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT epoch, SUM(share_bps)::bigint
            FROM st_payout_leaf
            WHERE deployment_key = $1 AND network_id = $2 AND no_id = $3 AND epoch = ANY($4::bigint[])
            GROUP BY epoch
        `, key, networkId, int64(noId), epochInts)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var epoch int64
				var share int64
				server.Raise(result.Scan(&epoch, &share))
				shares[uint64(epoch)] = int(share)
			}
		})
	})
	return shares
}

// GetStPayoutNetworkShares returns every network's summed share of one
// epoch's operator pool (only networks that hold a leaf are present).
func GetStPayoutNetworkShares(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64) map[server.Id]int {
	key := requireStDeploymentKey(deploymentKey)
	shares := map[server.Id]int{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT network_id, SUM(share_bps)::bigint
            FROM st_payout_leaf
            WHERE deployment_key = $1 AND epoch = $2 AND no_id = $3
            GROUP BY network_id
        `, key, int64(epoch), int64(noId))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var share int64
				server.Raise(result.Scan(&networkId, &share))
				shares[networkId] = int(share)
			}
		})
	})
	return shares
}

// GetAccountNanoPointsInWindow sums one network's account points created in
// [start, end). Points are written by the payout planner
// (PaymentPlanner.applyPayoutPoints), so an epoch's points are the point rows
// whose create time falls inside the epoch's wall-clock window.
func GetAccountNanoPointsInWindow(ctx context.Context, networkId server.Id, start time.Time, end time.Time) NanoPoints {
	var nanoPoints NanoPoints
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT COALESCE(SUM(point_value), 0)::bigint
            FROM account_point
            WHERE network_id = $1 AND create_time >= $2 AND create_time < $3
        `, networkId, start, end)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&nanoPoints))
			}
		})
	})
	return nanoPoints
}

// GetNetworkNanoPointsInWindow sums account points per network created in
// [start, end); networks without points are absent.
func GetNetworkNanoPointsInWindow(ctx context.Context, start time.Time, end time.Time) map[server.Id]NanoPoints {
	points := map[server.Id]NanoPoints{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT network_id, SUM(point_value)::bigint
            FROM account_point
            WHERE create_time >= $1 AND create_time < $2
            GROUP BY network_id
        `, start, end)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var nanoPoints NanoPoints
				server.Raise(result.Scan(&networkId, &nanoPoints))
				if 0 < nanoPoints {
					points[networkId] = nanoPoints
				}
			}
		})
	})
	return points
}

// GetActiveNetworkClientIds lists up to `limit` active clients of a network.
func GetActiveNetworkClientIds(ctx context.Context, networkId server.Id, limit int) []server.Id {
	clientIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT client_id
            FROM network_client
            WHERE network_id = $1 AND active = true
            LIMIT $2
        `, networkId, limit)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return clientIds
}

// GetNetworkIdsForClientIds maps client ids to their networks in one query.
func GetNetworkIdsForClientIds(ctx context.Context, clientIds []server.Id) map[server.Id]server.Id {
	networkIds := map[server.Id]server.Id{}
	if len(clientIds) == 0 {
		return networkIds
	}
	server.Db(ctx, func(conn server.PgConn) {
		for start := 0; start < len(clientIds); start += 4096 {
			end := min(start+4096, len(clientIds))
			result, err := conn.Query(ctx, `
                SELECT client_id, network_id
                FROM network_client
                WHERE client_id = ANY($1::uuid[])
            `, clientIds[start:end])
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var networkId server.Id
					server.Raise(result.Scan(&clientId, &networkId))
					networkIds[clientId] = networkId
				}
			})
		}
	})
	return networkIds
}

// GetActiveStHeadBindingsForCkeys returns the active legacy head bindings
// (`bindHead`, mirrored from chain events) for the given client keys.
func GetActiveStHeadBindingsForCkeys(ctx context.Context, deploymentKey StDeploymentKey, ckeys [][32]byte) map[[32]byte]*StHeadBinding {
	key := requireStDeploymentKey(deploymentKey)
	bindings := map[[32]byte]*StHeadBinding{}
	if len(ckeys) == 0 {
		return bindings
	}
	ckeyBytes := make([][]byte, len(ckeys))
	for i := range ckeys {
		ckeyBytes[i] = ckeys[i][:]
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT ckey, hotkey, uid, active, update_block, update_time
            FROM st_head_binding
            WHERE deployment_key = $1 AND active = true AND ckey = ANY($2::bytea[])
        `, key, ckeyBytes)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				binding := &StHeadBinding{}
				var ckey []byte
				var hotkey []byte
				var uid int64
				var updateBlock int64
				server.Raise(result.Scan(&ckey, &hotkey, &uid, &binding.Active, &updateBlock, &binding.UpdateTime))
				copy(binding.Ckey[:], ckey)
				copy(binding.Hotkey[:], hotkey)
				binding.Uid = uint64(uid)
				binding.UpdateBlock = uint64(updateBlock)
				bindings[binding.Ckey] = binding
			}
		})
	})
	return bindings
}

// StFleetBindingSignature is a device's stored consent for one fleet binding
// generation (WHITEPAPER §11.4): the canonical binding, its digest and the
// client Ed25519 signature, plus the hotkey sr25519 signature once the
// operator adds it. The server never submits it on chain; the operator
// fetches the assembled calldata and sends it from their own key.
type StFleetBindingSignature struct {
	DeploymentKey   StDeploymentKey
	ClientId        server.Id
	NetworkId       server.Id
	Generation      uint64
	Hotkey          [32]byte
	Digest          [32]byte
	BindingJson     string
	ClientSignature []byte
	HotkeySignature []byte
	CreateTime      time.Time
}

// SetStFleetBindingSignature upserts the stored consent for (client,
// generation). A later hotkey signature never clears an earlier one.
func SetStFleetBindingSignature(ctx context.Context, signature *StFleetBindingSignature) {
	key := requireStDeploymentKey(signature.DeploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
            INSERT INTO st_fleet_binding_signature (
                deployment_key, client_id, network_id, generation, hotkey, digest, binding_json,
                client_signature, hotkey_signature, create_time
            ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
            ON CONFLICT (deployment_key, client_id, generation) DO UPDATE
            SET
                network_id = EXCLUDED.network_id,
                hotkey = EXCLUDED.hotkey,
                digest = EXCLUDED.digest,
                binding_json = EXCLUDED.binding_json,
                client_signature = EXCLUDED.client_signature,
                hotkey_signature = COALESCE(EXCLUDED.hotkey_signature, st_fleet_binding_signature.hotkey_signature),
                create_time = EXCLUDED.create_time
        `,
			key,
			signature.ClientId,
			signature.NetworkId,
			int64(signature.Generation),
			signature.Hotkey[:],
			signature.Digest[:],
			signature.BindingJson,
			signature.ClientSignature,
			signature.HotkeySignature,
			signature.CreateTime,
		))
	})
}

// GetStFleetBindingSignature reads the stored consent for (client, generation).
func GetStFleetBindingSignature(ctx context.Context, deploymentKey StDeploymentKey, clientId server.Id, generation uint64) *StFleetBindingSignature {
	key := requireStDeploymentKey(deploymentKey)
	var signature *StFleetBindingSignature
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            SELECT network_id, hotkey, digest, binding_json, client_signature, hotkey_signature, create_time
            FROM st_fleet_binding_signature
            WHERE deployment_key = $1 AND client_id = $2 AND generation = $3
        `, key, clientId, int64(generation))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				signature = &StFleetBindingSignature{DeploymentKey: deploymentKey, ClientId: clientId, Generation: generation}
				var hotkey []byte
				var digest []byte
				server.Raise(result.Scan(
					&signature.NetworkId,
					&hotkey,
					&digest,
					&signature.BindingJson,
					&signature.ClientSignature,
					&signature.HotkeySignature,
					&signature.CreateTime,
				))
				copy(signature.Hotkey[:], hotkey)
				copy(signature.Digest[:], digest)
			}
		})
	})
	return signature
}

// ClaimStEpochNotification marks the earnings notification for an epoch as
// taken. Exactly one caller gets true, so several finalize paths and workers
// send the epoch email once.
func ClaimStEpochNotification(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64) (claimed bool) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `
            INSERT INTO st_epoch_notification (deployment_key, epoch, notify_time)
            VALUES ($1, $2, $3)
            ON CONFLICT (deployment_key, epoch) DO NOTHING
        `, key, int64(epoch), server.NowUtc()))
		claimed = tag.RowsAffected() == 1
	})
	return claimed
}
