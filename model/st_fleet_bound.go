// The coordinator's FleetBound logs mirrored into st_event for settlement v2
// head exclusion (VERSION2-FINAL.md §6.2, §8.2), and the next block the
// FleetBound scan reads. The scan starts at the deploy block and is the only
// writer of FleetBound rows, so rows through its checkpoint are complete.
package model

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/urnetwork/server"
)

// The st_event kind of a mirrored FleetBound log.
const StFleetBoundEventKind = "FleetBound"

// A mirrored FleetBound's data_json: bytes as 0x-hex and integers as decimal
// strings, as the event mirror writes every event.
type StFleetBoundEventData struct {
	ClientId       string `json:"client_id"`
	FleetId        string `json:"fleet_id"`
	Hotkey         string `json:"hotkey"`
	Uid            string `json:"uid"`
	Generation     string `json:"generation"`
	ValidFromEpoch string `json:"valid_from_epoch"`
	ValidToEpoch   string `json:"valid_to_epoch"`
}

// The next block the FleetBound scan reads, or 0 before its first range.
func GetStFleetBoundNextBlock(ctx context.Context, deploymentKey StDeploymentKey) uint64 {
	key := requireStDeploymentKey(deploymentKey)
	var nextBlock int64
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT next_block FROM st_fleet_bound_sync WHERE deployment_key=$1`, key)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&nextBlock))
			}
		})
	})
	return uint64(nextBlock)
}

// Records that every FleetBound log before nextBlock is mirrored.
func SetStFleetBoundNextBlock(ctx context.Context, deploymentKey StDeploymentKey, nextBlock uint64) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_fleet_bound_sync(deployment_key,next_block) VALUES($1,$2) ON CONFLICT (deployment_key) DO UPDATE SET next_block=EXCLUDED.next_block`, key, int64(nextBlock)))
	})
}

// The client ids of every mirrored FleetBound at or before closeBlock whose
// signed validity covers epoch, ascending and unique. A revocation only
// shortens a validity and a cleanup only ends one, so a client bound at either
// of the epoch's boundary blocks is among them. A malformed row fails the
// read: skipping it could pay a head-bound provider twice.
func GetStFleetBoundClientIds(ctx context.Context, deploymentKey StDeploymentKey, epoch uint64, closeBlock uint64) ([][16]byte, error) {
	key := requireStDeploymentKey(deploymentKey)
	clientIds := [][16]byte{}
	var parseErr error
	server.Db(ctx, func(conn server.PgConn) {
		clientIds, parseErr = [][16]byte{}, nil
		rows, err := conn.Query(ctx, `SELECT block_number,log_index,data_json FROM st_event WHERE deployment_key=$1 AND kind=$2 AND block_number<=$3`, key, StFleetBoundEventKind, int64(closeBlock))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var blockNumber int64
				var logIndex int
				var dataJson string
				server.Raise(rows.Scan(&blockNumber, &logIndex, &dataJson))
				clientId, validFrom, validTo, err := parseStFleetBoundEvent(dataJson)
				if err != nil {
					parseErr = fmt.Errorf("FleetBound event %d:%d: %w", blockNumber, logIndex, err)
					return
				}
				if validFrom <= epoch && epoch <= validTo {
					clientIds = append(clientIds, clientId)
				}
			}
		})
	})
	if parseErr != nil {
		return nil, parseErr
	}
	slices.SortFunc(clientIds, func(a [16]byte, b [16]byte) int {
		return strings.Compare(string(a[:]), string(b[:]))
	})
	return slices.Compact(clientIds), nil
}

// The client id and validity of a mirrored FleetBound.
func parseStFleetBoundEvent(dataJson string) ([16]byte, uint64, uint64, error) {
	var data StFleetBoundEventData
	if err := json.Unmarshal([]byte(dataJson), &data); err != nil {
		return [16]byte{}, 0, 0, err
	}
	clientId, err := hex.DecodeString(strings.TrimPrefix(data.ClientId, "0x"))
	if err != nil || len(clientId) != 16 {
		return [16]byte{}, 0, 0, fmt.Errorf("client id %q is not 16 bytes", data.ClientId)
	}
	validFrom, fromErr := strconv.ParseUint(data.ValidFromEpoch, 10, 64)
	validTo, toErr := strconv.ParseUint(data.ValidToEpoch, 10, 64)
	if fromErr != nil || toErr != nil {
		return [16]byte{}, 0, 0, fmt.Errorf("validity %q..%q is not two epochs", data.ValidFromEpoch, data.ValidToEpoch)
	}
	return [16]byte(clientId), validFrom, validTo, nil
}
