// AddStPayoutWalletResolutions for the settlement v2 ledger build at a
// million providers (VERSION2-FINAL.md §8.2): one read and one insert per
// batch instead of an insert and a read per provider, with v1's rows and
// refusals.
package model

import (
	"bytes"
	"context"
	"slices"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// Resolutions per read and per insert.
const stPayoutWalletResolutionBatch = 4096

// Retains the resolutions of one release epoch as AddStPayoutWalletResolutions
// does. Each batch, in client order, reads the rows already retained for its
// clients and inserts the others. A resolution that differs from a retained
// one, or from an earlier one for the same client, is an integrity error and
// is never written; the resolutions before it stay retained, as v1 leaves
// them. Under the transaction's repeatable read a row another writer commits
// meanwhile fails the insert's conflict check and the transaction reruns, so
// the read sees every row the insert could meet.
func AddStPayoutWalletResolutionsBatched(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64, resolutions []*StPayoutWalletResolution) error {
	key := requireStDeploymentKey(deploymentKey)
	ordered := make([]*StPayoutWalletResolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		if resolution == nil || resolution.ClientId == (server.Id{}) || resolution.NetworkId == (server.Id{}) {
			return protocol.ErrWalletMappingIntegrity
		}
		// every hotkey field is set in mode hotkey and none in another mode
		hotkeyMode := resolution.Mode == protocol.EarningWalletModeHotkey
		hotkeyFields := []bool{resolution.Hotkey != ([32]byte{}), resolution.HotkeyConsentHash != ([32]byte{}), resolution.HotkeyConsentGeneration != 0, resolution.HotkeyConsentHeadHash != ([32]byte{}), resolution.HotkeyConsentHeadGeneration != 0}
		if !hotkeyMode && resolution.Mode != protocol.EarningWalletModeProvider && resolution.Mode != protocol.EarningWalletModeNetwork || slices.Contains(hotkeyFields, !hotkeyMode) {
			return protocol.ErrWalletMappingIntegrity
		}
		ordered = append(ordered, resolution)
	}
	// stable, so of two resolutions for one client the earlier input is kept
	slices.SortStableFunc(ordered, func(a *StPayoutWalletResolution, b *StPayoutWalletResolution) int {
		return bytes.Compare(a.ClientId[:], b.ClientId[:])
	})
	var mismatch bool
	server.Tx(ctx, func(tx server.PgTx) {
		mismatch = false
		for start := 0; start < len(ordered) && !mismatch; start += stPayoutWalletResolutionBatch {
			batch := ordered[start:min(start+stPayoutWalletResolutionBatch, len(ordered))]
			clientIds := make([]server.Id, len(batch))
			for index, resolution := range batch {
				clientIds[index] = resolution.ClientId
			}
			retained := map[server.Id]StPayoutWalletResolution{}
			rows, err := tx.Query(ctx, `SELECT client_id,`+stPayoutWalletResolutionColumns+` FROM st_payout_wallet_resolution WHERE deployment_key=$1 AND epoch=$2 AND no_id=$3 AND client_id=ANY($4)`, key, int64(epoch), int64(noId), clientIds)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					resolution := StPayoutWalletResolution{}
					scanStPayoutWalletResolution(rows.Scan, &resolution, &resolution.ClientId)
					retained[resolution.ClientId] = resolution
				}
			})
			inserted := []*StPayoutWalletResolution{}
			for _, resolution := range batch {
				if prior, ok := retained[resolution.ClientId]; ok {
					if prior != *resolution {
						mismatch = true
						break
					}
					continue
				}
				retained[resolution.ClientId] = *resolution
				inserted = append(inserted, resolution)
			}
			insertStPayoutWalletResolutionsInTx(ctx, tx, key, epoch, noId, inserted)
		}
	})
	if mismatch {
		return protocol.ErrWalletMappingIntegrity
	}
	return nil
}

// One insert of new resolutions. The hotkey columns are SQL NULL outside
// mode hotkey, as v1 writes them.
func insertStPayoutWalletResolutionsInTx(ctx context.Context, tx server.PgTx, key string, epoch uint64, noId uint64, resolutions []*StPayoutWalletResolution) {
	if len(resolutions) == 0 {
		return
	}
	nullableHash := func(value [32]byte) []byte {
		if value == ([32]byte{}) {
			return nil
		}
		return value[:]
	}
	nullableGeneration := func(value uint64) *int64 {
		if value == 0 {
			return nil
		}
		generation := int64(value)
		return &generation
	}
	count := len(resolutions)
	clientIds, networkIds, modes := make([]server.Id, count), make([]server.Id, count), make([]string, count)
	coldkeys, consentHashes, headHashes := make([][]byte, count), make([][]byte, count), make([][]byte, count)
	consentGenerations, headGenerations := make([]int64, count), make([]int64, count)
	hotkeys, hotkeyConsentHashes, hotkeyConsentHeadHashes := make([][]byte, count), make([][]byte, count), make([][]byte, count)
	hotkeyConsentGenerations, hotkeyConsentHeadGenerations := make([]*int64, count), make([]*int64, count)
	for index, resolution := range resolutions {
		clientIds[index], networkIds[index], modes[index] = resolution.ClientId, resolution.NetworkId, resolution.Mode
		coldkeys[index], consentHashes[index], headHashes[index] = resolution.Coldkey[:], resolution.ConsentHash[:], resolution.HeadHash[:]
		consentGenerations[index], headGenerations[index] = int64(resolution.ConsentGeneration), int64(resolution.HeadGeneration)
		hotkeys[index], hotkeyConsentHashes[index], hotkeyConsentHeadHashes[index] = nullableHash(resolution.Hotkey), nullableHash(resolution.HotkeyConsentHash), nullableHash(resolution.HotkeyConsentHeadHash)
		hotkeyConsentGenerations[index], hotkeyConsentHeadGenerations[index] = nullableGeneration(resolution.HotkeyConsentGeneration), nullableGeneration(resolution.HotkeyConsentHeadGeneration)
	}
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_payout_wallet_resolution(deployment_key,epoch,no_id,client_id,network_id,mode,coldkey,consent_hash,consent_generation,head_hash,head_generation,hotkey,hotkey_consent_hash,hotkey_consent_generation,hotkey_consent_head_hash,hotkey_consent_head_generation) SELECT $1::varchar,$2::bigint,$3::bigint,* FROM unnest($4::uuid[],$5::uuid[],$6::varchar[],$7::bytea[],$8::bytea[],$9::bigint[],$10::bytea[],$11::bigint[],$12::bytea[],$13::bytea[],$14::bigint[],$15::bytea[],$16::bigint[]) ON CONFLICT (deployment_key,epoch,no_id,client_id) DO NOTHING`,
		key, int64(epoch), int64(noId), clientIds, networkIds, modes, coldkeys, consentHashes, consentGenerations, headHashes, headGenerations, hotkeys, hotkeyConsentHashes, hotkeyConsentGenerations, hotkeyConsentHeadHashes, hotkeyConsentHeadGenerations))
}
