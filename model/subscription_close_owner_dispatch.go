// Source-client owners have indexed durable custody independent of paid keys.
// The additive schema must pass the deployment gate before this dispatcher runs.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Paid and free registration share one resolver. Source custody also marks
// historical non-NULL inferred payer hints as repaired; no financial fact is
// fabricated by copying a source client UUID into payer_network_id.
var legacyCloseOwnerRegistrationSql = `SELECT picked.contract_id,owner.*
 FROM (SELECT contract_id FROM legacy_settlement_intent
 WHERE shard>=$1::smallint AND shard<($1::smallint+1) AND source_client_id IS NULL
 ORDER BY shard,contract_id LIMIT 256 FOR UPDATE SKIP LOCKED) AS picked
 CROSS JOIN LATERAL (` + strings.ReplaceAll(server.ContractCloseOwnerReadSql, "$1", "picked.contract_id") + ` OFFSET 0) AS owner`

// Registration traverses a fixed contract-key round independently of discovery.
// Invalid retained custody stays unclassified and is counted, without holding
// every valid owner behind the same bounded head on subsequent task turns.
type LegacySettlementOwnerCursor struct {
	After *server.Id `json:"after,omitempty"`
	End   server.Id  `json:"end"`
}

var legacyCloseOwnerRegistrationPageSql = strings.Replace(legacyCloseOwnerRegistrationSql,
	"ORDER BY shard,contract_id LIMIT 256", "AND contract_id<=$2 ORDER BY shard,contract_id LIMIT 256", 1)

func registerLegacyCloseOwners(ctx context.Context, shard int) int {
	_, registered, _ := registerLegacyCloseOwnerPage(ctx, shard, nil)
	return registered
}

func registerLegacyCloseOwnerPage(ctx context.Context, shard int, after *LegacySettlementOwnerCursor) (*LegacySettlementOwnerCursor, int, int) {
	type registration struct {
		contractId     server.Id
		sourceId       server.Id
		payerNetworkId *server.Id
	}
	var registrations []registration
	var unresolvedIds []server.Id
	var next *LegacySettlementOwnerCursor
	unresolved := 0
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'`))
		if after != nil {
			cursor := *after
			next = &cursor
		} else {
			rows, err := tx.Query(ctx, `SELECT contract_id FROM legacy_settlement_intent
 WHERE shard>=$1::smallint AND shard<($1::smallint+1) AND source_client_id IS NULL
 ORDER BY shard DESC,contract_id DESC LIMIT 1`, shard)
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					next = &LegacySettlementOwnerCursor{}
					server.Raise(rows.Scan(&next.End))
				}
			})
		}
		if next == nil {
			return
		}
		visited := 0
		query := legacyCloseOwnerRegistrationPageSql
		args := []any{shard, next.End}
		if next.After != nil {
			query = strings.Replace(query, "AND contract_id<=$2", "AND contract_id<=$2 AND contract_id>$3", 1)
			args = append(args, *next.After)
		}
		rows, err := tx.Query(ctx, query, args...)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var value registration
				var escrowNetworkIds []server.Id
				var hasEscrow bool
				server.Raise(rows.Scan(&value.contractId, &value.sourceId, &value.payerNetworkId, &escrowNetworkIds, &hasEscrow))
				visited++
				id := value.contractId
				next.After = &id
				owner, err := selectContractCloseOwner(value.sourceId, value.payerNetworkId, escrowNetworkIds, hasEscrow)
				if err != nil {
					unresolved++
					unresolvedIds = append(unresolvedIds, value.contractId)
					continue
				}
				value.payerNetworkId = nil
				if owner.Kind == ContractCloseOwnerPayerNetwork {
					value.payerNetworkId = &owner.Id
				}
				registrations = append(registrations, value)
			}
		})
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			// An unresolved legacy hint must not remain the earliest row of
			// a healthy payer. Keep its source marker NULL for future repair;
			// no source task can claim it and no financial field is changed.
			for _, id := range unresolvedIds {
				batch.Queue(`UPDATE legacy_settlement_intent SET payer_network_id=NULL WHERE contract_id=$1`, id)
			}
			for _, value := range registrations {
				batch.Queue(`UPDATE legacy_settlement_intent SET payer_network_id=$2,source_client_id=$3 WHERE contract_id=$1`, value.contractId, value.payerNetworkId, value.sourceId)
			}
		})
		if visited < legacySettlementPayerRegistrationLimit {
			next = nil
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	// None of these acknowledgements escape before the transaction commits.
	return next, len(registrations), unresolved
}

// All legacy payer query builders retain their bounded prefix/order shape.
// Replacing only the key/predicate gives the source partial index the same plan.
func legacySourceOwnerSql(query string) string {
	query = strings.ReplaceAll(query, "payer_network_id", "source_client_id")
	return strings.ReplaceAll(query, "source_client_id IS NOT NULL", "source_client_id IS NOT NULL AND payer_network_id IS NULL")
}

var legacySourceOwnerIntentSql = strings.Replace(legacySourceOwnerSql(legacySettlementPayerIntentSql),
	"WHERE shard=$1 AND source_client_id>=", "WHERE shard=$1 AND payer_network_id IS NULL AND source_client_id IS NOT NULL AND source_client_id>=", 1)

func beginLegacySourceOwnerRound(ctx context.Context, shard int) (cursor *LegacySettlementPayerCursor) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, legacySourceOwnerSql(legacySettlementPayerBeginSql), shard)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				cursor = &LegacySettlementPayerCursor{}
				server.Raise(rows.Scan(&cursor.End, &cursor.PassEndTime))
			}
		})
	}, server.OptNoRetry())
	return
}

func nextLegacySourceOwner(ctx context.Context, shard int, cursor *LegacySettlementPayerCursor) (source *server.Id, next *LegacySettlementPosition) {
	server.Db(ctx, func(conn server.PgConn) {
		query := legacySourceOwnerSql(legacySettlementPayerNextSql)
		args := []any{shard, cursor.End}
		if cursor.After != nil {
			query += ` AND source_client_id>$3`
			args = append(args, *cursor.After)
		}
		query += ` ORDER BY source_client_id LIMIT 1`
		rows, err := conn.Query(ctx, query, args...)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				source = &server.Id{}
				server.Raise(rows.Scan(source))
			}
		})
		if source == nil {
			return
		}
		query = legacySourceOwnerIntentSql
		rows, err = conn.Query(ctx, query, shard, *source, cursor.PassEndTime)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				next = &LegacySettlementPosition{}
				server.Raise(rows.Scan(&next.NextAttemptTime, &next.ContractId))
			}
		})
	}, server.OptNoRetry())
	return
}

// Both dispatch paths resolve their head through the shared selector. A stale
// hint never publishes financial work under the wrong owner. Release discovery's
// connection before exact registration takes its own intent/contract locks.
func legacyDispatchOwnerMatches(ctx context.Context, contractId server.Id, expected ContractCloseOwner) (matches bool) {
	repair := false
	server.Db(ctx, func(conn server.PgConn) {
		actual, _, err := readContractCloseOwnerInConn(ctx, conn, contractId)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errContractCloseOwnerUnresolved) {
			return
		}
		server.Raise(err)
		matches = actual == expected
		repair = !matches
	}, server.OptNoRetry())
	if repair {
		// Populated source markers also occur on paid intents, outside the
		// missing-marker registration index. Repair only this observed head.
		server.Tx(ctx, func(tx server.PgTx) {
			var payerHint, sourceHint *server.Id
			err := tx.QueryRow(ctx, `SELECT payer_network_id,source_client_id FROM legacy_settlement_intent
				WHERE contract_id=$1 FOR UPDATE SKIP LOCKED`, contractId).Scan(&payerHint, &sourceHint)
			if err == pgx.ErrNoRows {
				return
			}
			server.Raise(err)
			var lockedId server.Id
			err = tx.QueryRow(ctx, `SELECT contract_id FROM transfer_contract
				WHERE contract_id=$1 AND outcome IS NULL FOR UPDATE SKIP LOCKED`, contractId).Scan(&lockedId)
			if err == pgx.ErrNoRows {
				return
			}
			server.Raise(err)
			actual, sourceId, err := readContractCloseOwnerInConn(ctx, tx, contractId)
			if errors.Is(err, errContractCloseOwnerUnresolved) {
				return
			}
			server.Raise(err)
			var payer *server.Id
			if actual.Kind == ContractCloseOwnerPayerNetwork {
				payer = &actual.Id
			}
			payerMatches := payer == nil && payerHint == nil || payer != nil && payerHint != nil && *payer == *payerHint
			if payerMatches && sourceHint != nil && *sourceHint == sourceId {
				return
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
				SET payer_network_id=$2,source_client_id=$3 WHERE contract_id=$1`, contractId, payer, sourceId))
		}, server.TxReadCommitted, server.OptNoRetry())
	}
	return
}

type legacySourceOwnerDispatch struct {
	cursor     *LegacySettlementPayerCursor
	ids        []server.Id
	probes     int
	yielded    bool
	payerDone  bool
	sourceDone bool
	begin      func(context.Context, int) *LegacySettlementPayerCursor
	next       func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition)
	matches    func(context.Context, server.Id, ContractCloseOwner) bool
}

// Invocation-local seams keep cancellation tests on the same discovery path.
func (self *legacySourceOwnerDispatch) ownerMatches(ctx context.Context, contractId server.Id, expected ContractCloseOwner) bool {
	if self.matches != nil {
		return self.matches(ctx, contractId, expected)
	}
	return legacyDispatchOwnerMatches(ctx, contractId, expected)
}

// Each identity family gets half the existing three-second discovery budget;
// optional registration still retains the full two-second allowance.
func (self *legacySourceOwnerDispatch) discover(ctx context.Context, shard int) error {
	if self.sourceDone {
		return nil
	}
	begin, next := self.begin, self.next
	if begin == nil {
		begin = beginLegacySourceOwnerRound
	}
	if next == nil {
		next = nextLegacySourceOwner
	}
	if self.cursor == nil || self.cursor.End == (server.Id{}) {
		// An interrupted initial seek remains explicitly unstarted, rather
		// than looking like EOF while the peer family continues its round.
		self.cursor = &LegacySettlementPayerCursor{}
		self.cursor = begin(ctx, shard)
	}
	for probe := 0; probe < legacySettlementPayerProbeLimit && self.cursor != nil; probe++ {
		source, ready := next(ctx, shard, self.cursor)
		if source == nil {
			self.cursor = nil
			break
		}
		self.probes++
		if ready != nil && self.ownerMatches(ctx, ready.ContractId, ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: *source}) {
			self.ids = append(self.ids, *source)
		}
		self.cursor.After = source
	}
	return nil
}

// Deployment owns schema readiness. Runtime catalog probes cannot defer close
// work or masquerade as a financial refusal.
func DispatchLegacySettlementCloseOwners(ctx context.Context, shard int, after *LegacySettlementCursor,
	payerAfter, sourceAfter *LegacySettlementPayerCursor, registrationAfter ...*LegacySettlementOwnerCursor) (result LegacySettlementDispatchResult, returnErr error) {
	result.Private = true
	if shard < 0 || shard >= LegacySettlementShardCount {
		return result, fmt.Errorf("invalid legacy settlement dispatch shard")
	}
	bounded, cancel := context.WithTimeoutCause(ctx, legacySettlementDispatchBudget, errLegacySettlementDispatchBudget)
	defer cancel()
	// While either family is in a round, a nil peer cursor means that family
	// reached EOF. Restarting it independently would alternate forever when
	// payer and source populations need different numbers of probes.
	source := &legacySourceOwnerDispatch{
		payerDone:  payerAfter == nil && sourceAfter != nil,
		sourceDone: sourceAfter == nil && payerAfter != nil,
	}
	if sourceAfter != nil {
		cursor := *sourceAfter
		source.cursor = &cursor
	}
	var registrationCursor *LegacySettlementOwnerCursor
	if len(registrationAfter) > 0 {
		registrationCursor = registrationAfter[0]
	}
	var registrationUnresolved int
	result, returnErr = dispatchLegacySettlementPayersPage(ctx, bounded, shard, after, payerAfter,
		nextLegacySettlementPayer, func(ctx context.Context, shard int, after *LegacySettlementCursor) (*LegacySettlementCursor, int) {
			var registered int
			registrationCursor, registered, registrationUnresolved = registerLegacyCloseOwnerPage(ctx, shard, registrationCursor)
			return after, registered
		}, source)
	result.RegistrationCursor, result.RegistrationUnresolved = registrationCursor, registrationUnresolved
	result.SourceClientIds, result.SourceCursor, result.SourceProbes = source.ids, source.cursor, source.probes
	if returnErr == nil {
		result.More = result.More || source.cursor != nil || source.yielded || registrationCursor != nil
	}
	return
}
