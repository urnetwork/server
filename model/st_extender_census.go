// The SN25 extender pool's census (sn WHITEPAPER.md §8.6.2, E1 and E3–E5):
// one row per extender record of the operator known in an epoch, eligible or
// not, captured once as soon as the epoch's end boundary is finalized, as its
// users are. It is the operator's attestation, published as keyed prefix
// hashes, never as addresses.
package model

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server"
)

// One address of an extender record.
type StExtenderCensusAddress struct {
	Ip netip.Addr
	// the address not deactivated by the five-minute challenge (C3).
	Active bool
}

// One extender record of the directory, as the census reads it.
type StExtenderCensusRecord struct {
	ExtenderId server.Id
	// the network that first stored the record's key, and ClientId the provider
	// client of it that activated it last.
	NetworkId     server.Id
	ClientId      server.Id
	Active        bool
	Revoked       bool
	DirectoryTier int
	Canary        bool
	Addresses     []StExtenderCensusAddress
}

// The records known in an epoch: created before its end, and not revoked
// before its start, with their addresses.
const stExtenderCensusRecordsSql = `
	SELECT e.extender_id, e.network_id, e.client_id, e.active, e.revoke_time IS NOT NULL,
		e.directory_tier, e.canary_channel IS NOT NULL, a.ip, a.active
	FROM network_extender AS e
	LEFT JOIN network_extender_address AS a ON a.extender_id = e.extender_id
	WHERE e.create_time < $2 AND (e.revoke_time IS NULL OR $1 <= e.revoke_time)
	ORDER BY e.extender_id, a.ip_version
`

// Reads the operator's extender records known in the epoch [start, end) as they
// stand now, which the census capture reads as soon as end is finalized.
func GetStExtenderCensusRecords(ctx context.Context, start time.Time, end time.Time) []*StExtenderCensusRecord {
	records := []*StExtenderCensusRecord{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, stExtenderCensusRecordsSql, start.UTC(), end.UTC())
		server.WithPgResult(result, err, func() {
			var current *StExtenderCensusRecord
			for result.Next() {
				record := &StExtenderCensusRecord{}
				var ip *netip.Addr
				var addressActive *bool
				server.Raise(result.Scan(&record.ExtenderId, &record.NetworkId, &record.ClientId, &record.Active, &record.Revoked,
					&record.DirectoryTier, &record.Canary, &ip, &addressActive))
				if current == nil || current.ExtenderId != record.ExtenderId {
					current = record
					records = append(records, current)
				}
				if ip != nil && addressActive != nil {
					current.Addresses = append(current.Addresses, StExtenderCensusAddress{Ip: ip.Unmap(), Active: *addressActive})
				}
			}
		})
	})
	return records
}

// Builds an epoch's census rows from its records, one per record, under the
// shared eligibility rules (extender_eligibility.go: E1, E3–E5). The rows are
// sorted by client id and extender id, and pass the census rules validators
// check (ledger.CheckCensusRows).
func BuildStExtenderCensus(records []*StExtenderCensusRecord, rules *ExtenderEligibilityRules) ([]ledger.CensusRow, error) {
	eligibilities, err := DecideExtenderEligibility(records, rules)
	if err != nil {
		return nil, fmt.Errorf("extender census: %w", err)
	}
	rows := eligibilities.censusRows()
	if err := ledger.CheckCensusRows(rows); err != nil {
		return nil, fmt.Errorf("extender census: %w", err)
	}
	return rows, nil
}

// A census as stored: its rows, concatenated.
func EncodeStExtenderCensus(rows []ledger.CensusRow) []byte {
	return bytes.Join(ledger.EncodeCensusChunks(rows), nil)
}

// Reads a stored census.
func DecodeStExtenderCensus(data []byte) ([]ledger.CensusRow, error) {
	chunks := [][]byte{}
	for start := 0; start < len(data); start += ledger.ChunkRows * ledger.CensusRowBytes {
		chunks = append(chunks, data[start:min(len(data), start+ledger.ChunkRows*ledger.CensusRowBytes)])
	}
	rows, err := ledger.DecodeCensusChunks(chunks)
	if err != nil {
		return nil, err
	}
	if err := ledger.CheckCensusRows(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// An epoch's captured census.
type StLedgerV2ExtenderCensus struct {
	Epoch       uint64
	Census      []byte
	Rows        uint64
	CaptureTime time.Time
}

// Records an epoch's census once: a census already captured is kept, and
// returned, whatever this one holds, because the boundary's census is the first
// one taken after it.
func CaptureStLedgerV2ExtenderCensus(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64, census []byte, rows uint64, captureTime time.Time) *StLedgerV2ExtenderCensus {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_ledger_v2_extender_census (deployment_key, no_id, epoch, census, census_rows, capture_time)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (deployment_key, no_id, epoch) DO NOTHING
		`, key, int64(noId), int64(epoch), census, int64(rows), captureTime.UTC()))
	})
	return GetStLedgerV2ExtenderCensus(ctx, deploymentKey, noId, epoch)
}

// An epoch's captured census, nil before it is captured.
func GetStLedgerV2ExtenderCensus(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) *StLedgerV2ExtenderCensus {
	key := requireStDeploymentKey(deploymentKey)
	var census *StLedgerV2ExtenderCensus
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT census, census_rows, capture_time FROM st_ledger_v2_extender_census
			WHERE deployment_key = $1 AND no_id = $2 AND epoch = $3
		`, key, int64(noId), int64(epoch))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				census = &StLedgerV2ExtenderCensus{Epoch: epoch}
				var rows int64
				server.Raise(result.Scan(&census.Census, &rows, &census.CaptureTime))
				census.Rows = uint64(rows)
			}
		})
	})
	return census
}

// The last amendments file that verified for a deployment, exactly as it was
// published; nil before the first.
func GetStAmendments(ctx context.Context, deploymentKey StDeploymentKey) []byte {
	key := requireStDeploymentKey(deploymentKey)
	var raw []byte
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT raw FROM st_amendments WHERE deployment_key = $1`, key)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&raw))
			}
		})
	})
	return raw
}

// Replaces a deployment's verified amendments file.
func SaveStAmendments(ctx context.Context, deploymentKey StDeploymentKey, raw []byte) {
	key := requireStDeploymentKey(deploymentKey)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO st_amendments (deployment_key, raw, update_time)
			VALUES ($1, $2, $3)
			ON CONFLICT (deployment_key) DO UPDATE SET raw = EXCLUDED.raw, update_time = EXCLUDED.update_time
		`, key, raw, server.NowUtc()))
	})
}
