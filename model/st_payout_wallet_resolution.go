// The earning wallet each published release epoch settled for every expected
// provider, and the consent that selected it. Rows are append-only and are
// written before the epoch's artifact record, so a recorded artifact always
// has its resolutions.
package model

import (
	"bytes"
	"context"
	"sort"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// One provider's selected consent: its own (mode provider) or its network's
// (mode network), the selected generation and the head the roster pinned.
type StPayoutWalletResolution struct {
	ClientId          server.Id
	NetworkId         server.Id
	Mode              string
	Coldkey           [32]byte
	ConsentHash       [32]byte
	ConsentGeneration uint64
	HeadHash          [32]byte
	HeadGeneration    uint64
}

// Retains the resolutions of one release epoch. A retry after a lost
// acknowledgement finds the same rows; a row that differs from the
// recomputed resolution is an integrity error, never overwritten.
func AddStPayoutWalletResolutions(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64, resolutions []*StPayoutWalletResolution) error {
	key := requireStDeploymentKey(deploymentKey)
	ordered := make([]*StPayoutWalletResolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		if resolution == nil || resolution.ClientId == (server.Id{}) || resolution.NetworkId == (server.Id{}) || resolution.Mode != protocol.EarningWalletModeProvider && resolution.Mode != protocol.EarningWalletModeNetwork {
			return protocol.ErrWalletMappingIntegrity
		}
		ordered = append(ordered, resolution)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return bytes.Compare(ordered[i].ClientId[:], ordered[j].ClientId[:]) < 0
	})
	var mismatch bool
	server.Tx(ctx, func(tx server.PgTx) {
		mismatch = false
		for _, resolution := range ordered {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_payout_wallet_resolution(deployment_key,epoch,no_id,client_id,network_id,mode,coldkey,consent_hash,consent_generation,head_hash,head_generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (deployment_key,epoch,no_id,client_id) DO NOTHING`, key, int64(epoch), int64(noId), resolution.ClientId, resolution.NetworkId, resolution.Mode, resolution.Coldkey[:], resolution.ConsentHash[:], int64(resolution.ConsentGeneration), resolution.HeadHash[:], int64(resolution.HeadGeneration)))
			var retained StPayoutWalletResolution
			var coldkey, consentHash, headHash []byte
			server.Raise(tx.QueryRow(ctx, `SELECT network_id,mode,coldkey,consent_hash,consent_generation,head_hash,head_generation FROM st_payout_wallet_resolution WHERE deployment_key=$1 AND epoch=$2 AND no_id=$3 AND client_id=$4`, key, int64(epoch), int64(noId), resolution.ClientId).Scan(&retained.NetworkId, &retained.Mode, &coldkey, &consentHash, &retained.ConsentGeneration, &headHash, &retained.HeadGeneration))
			retained.ClientId = resolution.ClientId
			copy(retained.Coldkey[:], coldkey)
			copy(retained.ConsentHash[:], consentHash)
			copy(retained.HeadHash[:], headHash)
			if retained != *resolution {
				mismatch = true
				return
			}
		}
	})
	if mismatch {
		return protocol.ErrWalletMappingIntegrity
	}
	return nil
}

// The retained resolutions of one release epoch, ordered by client.
func GetStPayoutWalletResolutions(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64) []*StPayoutWalletResolution {
	key := requireStDeploymentKey(deploymentKey)
	resolutions := []*StPayoutWalletResolution{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT client_id,network_id,mode,coldkey,consent_hash,consent_generation,head_hash,head_generation FROM st_payout_wallet_resolution WHERE deployment_key=$1 AND epoch=$2 AND no_id=$3 ORDER BY client_id`, key, int64(epoch), int64(noId))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				resolution := &StPayoutWalletResolution{}
				var coldkey, consentHash, headHash []byte
				server.Raise(rows.Scan(&resolution.ClientId, &resolution.NetworkId, &resolution.Mode, &coldkey, &consentHash, &resolution.ConsentGeneration, &headHash, &resolution.HeadGeneration))
				copy(resolution.Coldkey[:], coldkey)
				copy(resolution.ConsentHash[:], consentHash)
				copy(resolution.HeadHash[:], headHash)
				resolutions = append(resolutions, resolution)
			}
		})
	})
	return resolutions
}
