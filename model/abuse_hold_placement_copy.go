package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/sidecar"
)

var ErrHoldPlacementSnapshotUnavailable = errors.New("hold placement snapshot unavailable")

// A prepared placement has parts but no part zero. Its manifest makes the
// original snapshot recoverable across a crash between the mirror's PUT and
// publishing part zero. The requesting worker retries the same terms.
const holdPlacementManifest = "placement"
const holdPlacementRequest = "requested"

func privateHoldPreparation(ctx context.Context, tx server.PgTx, networkId, caseId server.Id, states ...string) (saved *LegalHold, err error) {
	found, err := tx.Query(ctx, `SELECT `+legalHoldColumns+` FROM abuse_legal_hold
		WHERE network_id=$1 AND case_id=$2 AND part=1 AND copy_state=ANY($3::text[])`, networkId, caseId, states)
	if err != nil {
		return nil, err
	}
	defer found.Close()
	if found.Next() {
		saved = scanLegalHold(found)
	}
	return saved, found.Err()
}

// Save the staff-authorized terms in the hold table. Only its two opaque IDs
// cross the task queue. The reservation is invisible to readers of part zero
// and expires even when a worker never accepts the placement.
func QueueLegalHoldPlacement(ctx context.Context, args *LegalHoldArgs, at time.Time, queue func(server.PgTx)) (err error) {
	if !server.SidecarMg5Linked() {
		return ErrAbuseUnlinked
	}
	if !server.GetAbuseConfig().HoldsEnabled() {
		return ErrLegalHoldsDisabled
	}
	at = legalHoldTime(at)
	if err = args.validate(at); err != nil {
		return err
	}
	expiry, err := LegalHoldDeadline(args.Kind, at, args.reportTime())
	if err != nil {
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		err = nil
		terms := getLegalHoldInTx(ctx, tx, args.NetworkId, args.CaseId, false)
		if terms == nil {
			terms, err = privateHoldPreparation(ctx, tx, args.NetworkId, args.CaseId, holdPlacementRequest, holdPlacementManifest)
			if err != nil {
				return
			}
		}
		if terms != nil {
			if !terms.Live(at) {
				err = ErrLegalHoldExpired
				return
			}
			if !args.sameAs(terms) {
				err = ErrLegalHoldConflict
				return
			}
		} else {
			if getAbuseCaseInTx(ctx, tx, args.CaseId, true) == nil {
				err = ErrAbuseCaseNotFound
				return
			}
			if !networkExistsInTx(ctx, tx, args.NetworkId) {
				err = ErrNetworkNotFound
				return
			}
			tag, insertErr := tx.Exec(ctx, `INSERT INTO abuse_legal_hold
				(network_id,case_id,part,kind,scope,place_time,report_time,expire_time,copy_state)
				VALUES($1,$2,1,$3,$4::jsonb,$5,$6,$7,$8)
				ON CONFLICT(network_id,case_id,part) DO UPDATE SET kind=abuse_legal_hold.kind
				WHERE abuse_legal_hold.copy_state IN ($8,$9) AND abuse_legal_hold.kind=EXCLUDED.kind
					AND abuse_legal_hold.scope=EXCLUDED.scope
					AND abuse_legal_hold.report_time IS NOT DISTINCT FROM EXCLUDED.report_time`,
				args.NetworkId, args.CaseId, string(args.Kind), legalHoldScopeJson(args.Scope), at, args.reportTime(), expiry,
				holdPlacementRequest, holdPlacementManifest)
			server.Raise(insertErr)
			if tag.RowsAffected() == 0 {
				err = ErrLegalHoldConflict
				return
			}
		}
		queue(tx)
	})
	return
}

func LegalHoldPlacementTerms(ctx context.Context, networkId, caseId server.Id) (args *LegalHoldArgs, err error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		args, err = nil, nil
		terms := getLegalHoldInTx(ctx, tx, networkId, caseId, false)
		if terms == nil {
			terms, err = privateHoldPreparation(ctx, tx, networkId, caseId, holdPlacementRequest, holdPlacementManifest)
		}
		if err != nil {
			return
		}
		if terms == nil {
			err = ErrLegalHoldNotFound
			return
		}
		args = &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: terms.Kind, Scope: terms.Scope, ReportTime: terms.ReportTime}
	})
	return
}

func DiscardLegalHoldPlacementRequest(ctx context.Context, args *LegalHoldArgs) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM abuse_legal_hold
			WHERE network_id=$1 AND case_id=$2 AND part=1 AND copy_state=$3
				AND kind=$4 AND scope=$5::jsonb AND report_time IS NOT DISTINCT FROM $6`,
			args.NetworkId, args.CaseId, holdPlacementRequest, string(args.Kind), legalHoldScopeJson(args.Scope), args.reportTime()))
	})
}

func prepareLegalHoldSnapshot(ctx context.Context, placing *LegalHold, mirrorExists bool, clock func() time.Time) (snapshot *LegalHold, err error) {
	source := currentAbuseHoldSoftStateSource()
	var before map[server.Id][sha256.Size]byte
	server.Tx(ctx, func(tx server.PgTx) {
		snapshot, err = privateHoldPreparation(ctx, tx, placing.NetworkId, placing.CaseId, holdPlacementManifest)
		if err == nil && snapshot == nil && !mirrorExists {
			before, err = readPlacementSoftState(ctx, tx, placing, source, time.Time{}, nil)
		}
	})
	if err != nil || snapshot != nil || mirrorExists {
		return
	}
	// Raising a failed stage rolls back every staged part in the outer transaction.
	// The database helper may retry transient errors before returning the final one.
	defer func() {
		if raised := recover(); raised != nil {
			cause, ok := raised.(error)
			if !ok {
				panic(raised)
			}
			snapshot, err = nil, cause
		}
	}()
	save := func(stage server.PgTx) error {
		snapshot = nil
		// A partition's DETACH cannot remove a source between the
		// placement snapshot and the last contract cursor.
		if _, err := stage.Exec(ctx, "LOCK TABLE transfer_contract, contract_close, contract_close_report IN ACCESS SHARE MODE"); err != nil {
			return err
		}
		snapshot, err = privateHoldPreparation(ctx, stage, placing.NetworkId, placing.CaseId, holdPlacementManifest)
		if err != nil || snapshot != nil {
			return err
		}
		placing.PlaceTime = legalHoldTime(clock())
		placing.ExpireTime, err = LegalHoldDeadline(placing.Kind, placing.PlaceTime, placing.ReportTime)
		if err != nil {
			return err
		}
		// Reserve the manifest before collecting anything. Concurrent
		// requests either see the winner or retry a serialization conflict.
		tag, err := stage.Exec(ctx, `INSERT INTO abuse_legal_hold
				(network_id, case_id, part, kind, scope, place_time, report_time, expire_time, copy_state)
				VALUES ($1,$2,1,$3,$4::jsonb,$5,$6,$7,$8)
				ON CONFLICT(network_id,case_id,part) DO UPDATE SET copy_state=EXCLUDED.copy_state,
					place_time=EXCLUDED.place_time, expire_time=EXCLUDED.expire_time
				WHERE abuse_legal_hold.copy_state=$9 AND abuse_legal_hold.kind=EXCLUDED.kind
					AND abuse_legal_hold.scope=EXCLUDED.scope AND abuse_legal_hold.report_time IS NOT DISTINCT FROM EXCLUDED.report_time`,
			placing.NetworkId, placing.CaseId, string(placing.Kind), legalHoldScopeJson(placing.Scope),
			placing.PlaceTime, placing.ReportTime, placing.ExpireTime, holdPlacementManifest, holdPlacementRequest)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLegalHoldConflict
		}
		settings := DefaultAbuseHoldCopySettings()
		part := 1
		wroteSoft := false
		after, err := readPlacementSoftState(ctx, stage, placing, source, placing.PlaceTime, func(repoId server.Id, keys []*AbuseHoldSoftStateKey) error {
			if len(keys) == 0 {
				return nil
			}
			wroteSoft = true
			return writePlacementSoftStateParts(ctx, stage, placing, &part, repoId, keys, settings)
		})
		if err != nil {
			return err
		}
		if !maps.Equal(before, after) {
			return ErrHoldPlacementSnapshotUnavailable
		}
		if !wroteSoft {
			if err := writePlacementPart(ctx, stage, placing, &part, HoldCopyPartHeader{Kind: HoldCopyPartKindSoftState, Complete: true}, nil, settings); err != nil {
				return err
			}
		}
		if err := copyPlacementContracts(ctx, stage, placing, &part, settings); err != nil {
			return err
		}
		header := &HoldCopyPartHeader{Version: holdCopyPartVersion, CaseId: placing.CaseId,
			Kind: holdPlacementManifest, Complete: true, PlacementLastPart: part}
		manifest, err := encodeHoldCopyPart(header, nil, settings)
		if err != nil {
			return err
		}
		_, err = stage.Exec(ctx, `UPDATE abuse_legal_hold SET copied_rows=$3 WHERE network_id=$1 AND case_id=$2 AND part=1`,
			placing.NetworkId, placing.CaseId, manifest)
		snapshot = placing
		return err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(save(tx))
	})
	return
}

// RemoveExpiredHoldPreparations removes abandoned placement parts at the
// original deadline. An acknowledged hold belongs to the ordinary end pass,
// which also transfers overlapping copies and sends the legal record.
func RemoveExpiredHoldPreparations(ctx context.Context, now time.Time, limit int) int {
	var removed int
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(tx.QueryRow(ctx, `WITH expired AS MATERIALIZED (
			SELECT m.network_id,m.case_id FROM abuse_legal_hold m
			WHERE m.part=1 AND m.copy_state=ANY($1::text[]) AND m.expire_time <= $2
			AND NOT EXISTS (SELECT 1 FROM abuse_legal_hold h WHERE h.network_id=m.network_id AND h.case_id=m.case_id AND h.part=0)
			ORDER BY m.expire_time,m.network_id,m.case_id LIMIT $3 FOR UPDATE OF m SKIP LOCKED
		), deleted AS (
			DELETE FROM abuse_legal_hold p USING expired
			WHERE p.network_id=expired.network_id AND p.case_id=expired.case_id AND p.part>0 RETURNING p.part
		) SELECT count(*) FROM deleted WHERE part=1`, []string{holdPlacementManifest, holdPlacementRequest}, now, limit).Scan(&removed))
	})
	return removed
}

// A reaper that already holds a bounded set of contracts checks their
// historical owners before it removes the contracts and their dependents.
func contractsOutsidePendingHolds(ctx context.Context, tx server.PgTx, ids []server.Id) []server.Id {
	if !server.SidecarMg5Linked() || len(ids) == 0 {
		return ids
	}
	var allowed []server.Id
	server.Raise(tx.QueryRow(ctx, `SELECT ARRAY(SELECT candidate.contract_id FROM transfer_contract candidate
		WHERE candidate.contract_id=ANY($1::uuid[]) AND NOT `+server.ContractPlacementCopyPendingSQL()+`
		ORDER BY candidate.contract_id)`, ids).Scan(&allowed))
	return allowed
}

// Mutable contract and close rows must be read under one placement snapshot.
// A worker running later may see new byte counts, different close times, or
// no contract at all. The cursor bounds memory and the transaction retains
// its MVCC snapshot and its relation locks until all parts are durable.
type holdContractTable struct {
	name    string
	columns []string
	from    string
	at      string
}

var holdContractTables = []holdContractTable{
	{"transfer_contract",
		[]string{"contract_id", "source_id", "destination_id", "source_network_id", "destination_network_id", "payer_network_id", "transfer_byte_count", "create_time", "close_time", "open", "dispute", "outcome", "companion_contract_id", "priority", "reap_time", "stream_id", "expiration_time", "usage_origin_is_source", "usage_unverified", "provider_usage"},
		"transfer_contract c", "c.create_time"},
	{"contract_close",
		[]string{"contract_id", "party", "used_transfer_byte_count", "checkpoint", "close_time", "legacy_checkpoint_byte_count", "identified_checkpoint_byte_count", "settlement_completion", "settlement_completion_checkpoint_byte_count"},
		"contract_close r JOIN transfer_contract c USING (contract_id)", "r.close_time"},
	{"contract_close_report",
		[]string{"contract_id", "party", "report_id", "used_transfer_byte_count", "checkpoint", "create_time"},
		"contract_close_report r JOIN transfer_contract c USING (contract_id)", "r.create_time"},
}

func writePlacementPart(ctx context.Context, tx server.PgTx, hold *LegalHold, number *int, header HoldCopyPartHeader, lines [][]byte, settings *AbuseHoldCopySettings) error {
	header.Version = holdCopyPartVersion
	header.CaseId = hold.CaseId
	header.RowCount = len(lines)
	body, err := encodeHoldCopyPart(&header, lines, settings)
	if err != nil {
		return err
	}
	*number = *number + 1
	_, err = tx.Exec(ctx, `INSERT INTO abuse_legal_hold (network_id, case_id, part, copied_rows) VALUES ($1, $2, $3, $4)`,
		hold.NetworkId, hold.CaseId, *number, body)
	return err
}

func copyPlacementContracts(ctx context.Context, tx server.PgTx, hold *LegalHold, number *int, settings *AbuseHoldCopySettings) error {
	for _, table := range holdContractTables {
		prefix := "r."
		if table.name == "transfer_contract" {
			prefix = "c."
		}
		selected := make([]string, 0, len(table.columns))
		for _, column := range table.columns {
			selected = append(selected, prefix+pgx.Identifier{column}.Sanitize()+"::text")
		}
		// DECLARE accepts bind parameters in its SELECT. Both directions count;
		// a contract internal to the network is still read just once.
		query := `DECLARE hold_contract_snapshot NO SCROLL CURSOR FOR SELECT json_build_array(` + strings.Join(selected, ",") +
			`)::text FROM ` + table.from + ` WHERE (c.source_network_id = $1 OR c.destination_network_id = $1)
			AND c.create_time <= $2 AND ` + table.at + ` <= $2`
		if _, err := tx.Exec(ctx, query, hold.NetworkId, hold.PlaceTime); err != nil {
			return err
		}
		var lines [][]byte
		rawSize := 0
		flush := func(complete bool) error {
			err := writePlacementPart(ctx, tx, hold, number, HoldCopyPartHeader{
				Kind: HoldCopyPartKindRows, Table: table.name, Columns: table.columns, Complete: complete,
			}, lines, settings)
			lines, rawSize = nil, 0
			return err
		}
		for {
			rows, err := tx.Query(ctx, fmt.Sprintf("FETCH FORWARD %d FROM hold_contract_snapshot", settings.ReadBatchRowCount))
			if err != nil {
				return err
			}
			var batch [][]byte
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					rows.Close()
					return err
				}
				batch = append(batch, []byte(line))
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, line := range batch {
				if len(lines) != 0 && rawSize+len(line)+1 > settings.PartRawMaxByteCount {
					if err := flush(false); err != nil {
						return err
					}
				}
				lines = append(lines, line)
				rawSize += len(line) + 1
			}
			if len(batch) < settings.ReadBatchRowCount {
				if err := flush(true); err != nil {
					return err
				}
				break
			}
		}
		if _, err := tx.Exec(ctx, "CLOSE hold_contract_snapshot"); err != nil {
			return err
		}
	}
	return nil
}

// A first pass fingerprints the scoped families before the SQL snapshot;
// the second checks every digest and writes their fields in its transaction.
// Recovering a durable manifest never reads present values.
func readPlacementSoftState(ctx context.Context, tx server.PgTx, hold *LegalHold, source AbuseHoldSoftStateSource, at time.Time, visit func(server.Id, []*AbuseHoldSoftStateKey) error) (map[server.Id][sha256.Size]byte, error) {
	if source == nil {
		return nil, ErrHoldPlacementSnapshotUnavailable
	}
	repos, err := source.NetworkRepoIds(ctx, tx, hold.NetworkId)
	if err != nil {
		return nil, err
	}
	digests := make(map[server.Id][sha256.Size]byte)
	for _, repoId := range repos {
		if !hold.Scope.Covers(sidecar.Id(repoId)) {
			continue
		}
		keys, err := source.RepoSoftState(ctx, repoId, at)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(keys)
		if err != nil {
			return nil, err
		}
		digests[repoId] = sha256.Sum256(body)
		if visit != nil {
			if err := visit(repoId, keys); err != nil {
				return nil, err
			}
		}
	}
	return digests, nil
}

// A large hash can span parts. Each line has a key and a disjoint set of
// fields, so combining the lines restores the exact hash, including a hash
// that has more fields than fit in a single compressed part.
func writePlacementSoftStateParts(ctx context.Context, tx server.PgTx, hold *LegalHold, number *int, repoId server.Id, keys []*AbuseHoldSoftStateKey, settings *AbuseHoldCopySettings) error {
	var lines [][]byte
	rawBytes := 0
	flush := func(complete bool) error {
		err := writePlacementPart(ctx, tx, hold, number, HoldCopyPartHeader{
			Kind: HoldCopyPartKindSoftState, RepoId: &repoId, Complete: complete,
		}, lines, settings)
		lines, rawBytes = nil, 0
		return err
	}
	for _, key := range keys {
		if key == nil || key.Key == "" {
			return ErrHoldPlacementSnapshotUnavailable
		}
		// Preserve compact, one-line records for ordinary hashes.
		line, err := json.Marshal(key)
		if err != nil {
			return err
		}
		if len(line)+1 <= settings.PartRawMaxByteCount {
			if rawBytes > 0 && rawBytes+len(line)+1 > settings.PartRawMaxByteCount {
				if err := flush(false); err != nil {
					return err
				}
			}
			lines = append(lines, line)
			rawBytes += len(line) + 1
			continue
		}
		for _, field := range slices.Sorted(maps.Keys(key.Fields)) {
			line, err := json.Marshal(&AbuseHoldSoftStateKey{Key: key.Key, Fields: map[string]string{field: key.Fields[field]}})
			if err != nil {
				return err
			}
			if len(line)+1 > settings.PartRawMaxByteCount {
				return ErrHoldCopyPartInvalid
			}
			if rawBytes > 0 && rawBytes+len(line)+1 > settings.PartRawMaxByteCount {
				if err := flush(false); err != nil {
					return err
				}
			}
			lines = append(lines, line)
			rawBytes += len(line) + 1
		}
	}
	return flush(true)
}
