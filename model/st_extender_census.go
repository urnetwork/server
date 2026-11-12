// The SN25 extender pool's census (sn WHITEPAPER.md §8.6.2, E1 and E3–E5):
// one row per extender record of the operator known in an epoch, eligible or
// not, captured once as soon as the epoch's end boundary is finalized, as its
// users are. It is the operator's attestation, published as keyed prefix
// hashes, never as addresses.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/urfoundation/sn/ledger"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// StExtenderCensusAddress is one address of an extender record.
type StExtenderCensusAddress struct {
	Ip netip.Addr
	// Active is the address not deactivated by the five-minute challenge (C3).
	Active bool
}

// StExtenderCensusRecord is one extender record of the directory, as the
// census reads it.
type StExtenderCensusRecord struct {
	ExtenderId server.Id
	// NetworkId is the network that first stored the record's key, and
	// ClientId the provider client of it that activated it last.
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

// GetStExtenderCensusRecords reads the operator's extender records known in
// the epoch [start, end) as they stand now, which the census capture reads as
// soon as end is finalized.
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

// StExtenderCensusRules are the operator's facts the census rows need beside
// the records: the unit key, the operator's own networks, and each provider
// client's payout coldkey and head binding.
type StExtenderCensusRules struct {
	// Unit is the keyed /29 or /48 prefix hash of an address, §8.4's head
	// unit under the same key and granularity
	// (VerifyEgressIpHashWithSettings).
	Unit func(netip.Addr) [32]byte
	// Durable are the operator's gated, durable networks
	// (durable_network_ids), and Declared the other networks it declares it
	// controls (E5).
	Durable  map[server.Id]bool
	Declared map[server.Id]bool
	// Coldkeys are the provider clients' payout coldkeys under the wallet
	// selection provider rows use, absent without one (E4).
	Coldkeys map[server.Id][32]byte
	// HeadBound are the provider clients with an active fleet binding at
	// either of the epoch's boundaries (E4).
	HeadBound map[server.Id]bool
}

// The address a record's unit is keyed from: its active IPv4 address, else
// its active IPv6 address, as a provider's head score pins its IPv4 hash
// first.
func stExtenderCensusUnitAddress(record *StExtenderCensusRecord) (netip.Addr, uint8, bool) {
	for _, family := range []uint8{ledger.UnitIPv4, ledger.UnitIPv6} {
		for _, address := range record.Addresses {
			if address.Active && address.Ip.IsValid() && (address.Ip.Is4() == (family == ledger.UnitIPv4)) {
				return address.Ip, family, true
			}
		}
	}
	return netip.Addr{}, ledger.UnitNone, false
}

// The keys E3 deduplicates by: the provider client, each active IPv4 address
// and each active IPv6 address's /64.
func stExtenderCensusDedupKeys(record *StExtenderCensusRecord) []string {
	keys := []string{"client:" + record.ClientId.String()}
	for _, address := range record.Addresses {
		switch {
		case !address.Active || !address.Ip.IsValid():
		case address.Ip.Is4():
			keys = append(keys, "ipv4:"+address.Ip.String())
		default:
			keys = append(keys, "ipv6:"+netip.PrefixFrom(address.Ip, 64).Masked().String())
		}
	}
	return keys
}

// BuildStExtenderCensus builds an epoch's census rows from its records:
//   - E1, active: the record is active and unrevoked and one of its addresses
//     is active; only then has it a unit, the keyed hash of its active IPv4
//     address's /29, else of its active IPv6 address's /48;
//   - E3, duplicate: of the active records that share a provider client, an
//     IPv4 address or an IPv6 /64, directly or through others, only the
//     lowest by (client id, extender id) counts;
//   - E4: the provider client's coldkey, and its head binding;
//   - E5: gated (the gated tier or a durable network), canary, declared.
//
// The rows are sorted by client id and extender id, and pass the census rules
// validators check (ledger.CheckCensusRows).
func BuildStExtenderCensus(records []*StExtenderCensusRecord, rules *StExtenderCensusRules) ([]ledger.CensusRow, error) {
	if rules == nil || rules.Unit == nil {
		return nil, errors.New("extender census: no unit key")
	}
	records = slices.Clone(records)
	slices.SortFunc(records, func(a, b *StExtenderCensusRecord) int {
		if c := a.ClientId.Cmp(b.ClientId); c != 0 {
			return c
		}
		return a.ExtenderId.Cmp(b.ExtenderId)
	})
	rows := make([]ledger.CensusRow, len(records))
	active := make([]bool, len(records))
	listed := make(map[server.Id]bool, len(records))
	for i, record := range records {
		if listed[record.ExtenderId] {
			return nil, fmt.Errorf("extender census: extender %s is listed twice", record.ExtenderId)
		}
		listed[record.ExtenderId] = true
		row := ledger.CensusRow{ExtenderID: [16]byte(record.ExtenderId), ClientID: [16]byte(record.ClientId)}
		if address, family, ok := stExtenderCensusUnitAddress(record); ok && record.Active && !record.Revoked {
			active[i] = true
			row.Flags |= ledger.CensusActive
			row.Unit, row.Family = rules.Unit(address), family
			if row.Unit == ([32]byte{}) {
				return nil, fmt.Errorf("extender census: extender %s has a zero unit", record.ExtenderId)
			}
		}
		if record.Revoked {
			row.Flags |= ledger.CensusRevoked
		}
		if record.DirectoryTier != connect.ExtenderDirectoryTierOpen || rules.Durable[record.NetworkId] {
			row.Flags |= ledger.CensusGated
		}
		if record.Canary {
			row.Flags |= ledger.CensusCanary
		}
		if rules.Declared[record.NetworkId] {
			row.Flags |= ledger.CensusDeclared
		}
		if rules.HeadBound[record.ClientId] {
			row.Flags |= ledger.CensusHeadBound
		}
		row.Coldkey = rules.Coldkeys[record.ClientId]
		rows[i] = row
	}
	// E3 over the active records: a union of every key's records, each
	// component counted once, for its lowest (client id, extender id), which
	// in this order is its lowest index.
	parent := make([]int, len(records))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	first := map[string]int{}
	for i, record := range records {
		if !active[i] {
			continue
		}
		for _, key := range stExtenderCensusDedupKeys(record) {
			j, ok := first[key]
			if !ok {
				first[key] = i
				continue
			}
			a, b := find(i), find(j)
			if a != b {
				parent[max(a, b)] = min(a, b)
			}
		}
	}
	for i := range records {
		if active[i] && find(i) != i {
			rows[i].Flags |= ledger.CensusDuplicate
		}
	}
	if err := ledger.CheckCensusRows(rows); err != nil {
		return nil, fmt.Errorf("extender census: %w", err)
	}
	return rows, nil
}

// EncodeStExtenderCensus is a census as stored: its rows, concatenated.
func EncodeStExtenderCensus(rows []ledger.CensusRow) []byte {
	return bytes.Join(ledger.EncodeCensusChunks(rows), nil)
}

// DecodeStExtenderCensus reads a stored census.
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

// StLedgerV2ExtenderCensus is an epoch's captured census.
type StLedgerV2ExtenderCensus struct {
	Epoch       uint64
	Census      []byte
	Rows        uint64
	CaptureTime time.Time
}

// CaptureStLedgerV2ExtenderCensus records an epoch's census once: a census
// already captured is kept, and returned, whatever this one holds, because
// the boundary's census is the first one taken after it.
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

// GetStLedgerV2ExtenderCensus is an epoch's captured census, nil before it is
// captured.
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

// GetStAmendments is the last amendments file that verified for a deployment,
// exactly as it was published; nil before the first.
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

// SaveStAmendments replaces a deployment's verified amendments file.
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
