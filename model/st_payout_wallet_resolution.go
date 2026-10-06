// The earning wallet each published release epoch settled for every expected
// provider, and the consent that selected it. Rows are append-only and are
// written before the epoch's artifact record, so a recorded artifact always
// has its resolutions.
package model

import (
	"bytes"
	"context"
	"slices"
	"sort"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// One provider's selected consent: its own (mode provider), its network's
// (mode network) or its network's hotkey delegation (mode hotkey), the
// selected generation and the head the roster pinned. In mode hotkey those
// describe the delegation chain, and the Hotkey fields name the delegation's
// hotkey and the global consent selected at the epoch, with the consent head
// the delegation pins. They are zero in the other modes.
type StPayoutWalletResolution struct {
	ClientId                    server.Id
	NetworkId                   server.Id
	Mode                        string
	Coldkey                     [32]byte
	ConsentHash                 [32]byte
	ConsentGeneration           uint64
	HeadHash                    [32]byte
	HeadGeneration              uint64
	Hotkey                      [32]byte
	HotkeyConsentHash           [32]byte
	HotkeyConsentGeneration     uint64
	HotkeyConsentHeadHash       [32]byte
	HotkeyConsentHeadGeneration uint64
}

// The hotkey columns are SQL NULL outside mode hotkey and read back as zero.
const stPayoutWalletResolutionColumns = `network_id,mode,coldkey,consent_hash,consent_generation,head_hash,head_generation,coalesce(hotkey,''::bytea),coalesce(hotkey_consent_hash,''::bytea),coalesce(hotkey_consent_generation,0),coalesce(hotkey_consent_head_hash,''::bytea),coalesce(hotkey_consent_head_generation,0)`

// Scans stPayoutWalletResolutionColumns, after any leading columns the caller
// scans itself.
func scanStPayoutWalletResolution(scan func(...any) error, resolution *StPayoutWalletResolution, leading ...any) {
	var coldkey, consentHash, headHash, hotkey, hotkeyConsentHash, hotkeyConsentHeadHash []byte
	server.Raise(scan(append(leading, &resolution.NetworkId, &resolution.Mode, &coldkey, &consentHash, &resolution.ConsentGeneration, &headHash, &resolution.HeadGeneration, &hotkey, &hotkeyConsentHash, &resolution.HotkeyConsentGeneration, &hotkeyConsentHeadHash, &resolution.HotkeyConsentHeadGeneration)...))
	copy(resolution.Coldkey[:], coldkey)
	copy(resolution.ConsentHash[:], consentHash)
	copy(resolution.HeadHash[:], headHash)
	copy(resolution.Hotkey[:], hotkey)
	copy(resolution.HotkeyConsentHash[:], hotkeyConsentHash)
	copy(resolution.HotkeyConsentHeadHash[:], hotkeyConsentHeadHash)
}

// Retains the resolutions of one release epoch. A retry after a lost
// acknowledgement finds the same rows; a row that differs from the
// recomputed resolution is an integrity error, never overwritten.
func AddStPayoutWalletResolutions(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, noId uint64, resolutions []*StPayoutWalletResolution) error {
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
	sort.Slice(ordered, func(i, j int) bool {
		return bytes.Compare(ordered[i].ClientId[:], ordered[j].ClientId[:]) < 0
	})
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
	var mismatch bool
	server.Tx(ctx, func(tx server.PgTx) {
		mismatch = false
		for _, resolution := range ordered {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_payout_wallet_resolution(deployment_key,epoch,no_id,client_id,network_id,mode,coldkey,consent_hash,consent_generation,head_hash,head_generation,hotkey,hotkey_consent_hash,hotkey_consent_generation,hotkey_consent_head_hash,hotkey_consent_head_generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT (deployment_key,epoch,no_id,client_id) DO NOTHING`, key, int64(epoch), int64(noId), resolution.ClientId, resolution.NetworkId, resolution.Mode, resolution.Coldkey[:], resolution.ConsentHash[:], int64(resolution.ConsentGeneration), resolution.HeadHash[:], int64(resolution.HeadGeneration), nullableHash(resolution.Hotkey), nullableHash(resolution.HotkeyConsentHash), nullableGeneration(resolution.HotkeyConsentGeneration), nullableHash(resolution.HotkeyConsentHeadHash), nullableGeneration(resolution.HotkeyConsentHeadGeneration)))
			retained := StPayoutWalletResolution{ClientId: resolution.ClientId}
			scanStPayoutWalletResolution(tx.QueryRow(ctx, `SELECT `+stPayoutWalletResolutionColumns+` FROM st_payout_wallet_resolution WHERE deployment_key=$1 AND epoch=$2 AND no_id=$3 AND client_id=$4`, key, int64(epoch), int64(noId), resolution.ClientId).Scan, &retained)
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
		rows, err := conn.Query(ctx, `SELECT client_id,`+stPayoutWalletResolutionColumns+` FROM st_payout_wallet_resolution WHERE deployment_key=$1 AND epoch=$2 AND no_id=$3 ORDER BY client_id`, key, int64(epoch), int64(noId))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				resolution := &StPayoutWalletResolution{}
				scanStPayoutWalletResolution(rows.Scan, resolution, &resolution.ClientId)
				resolutions = append(resolutions, resolution)
			}
		})
	})
	return resolutions
}
