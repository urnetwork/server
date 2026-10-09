// A durable payer task reuses the ordinary bounded financial page and its
// exact accounting owners. Cross-shard selection never scans another payer.
package model

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

type legacySettlementPayerScopeKey struct{}

const legacySettlementShardValuesSql = `(VALUES (0::smallint),(1),(2),(3),(4),(5),(6),(7),
 (8),(9),(10),(11),(12),(13),(14),(15)) AS payer_shard(shard)`

// Each existing index seek returns at most limit rows before the outer merge.
// Use only exact equality for the fixed payer prefix. Redundant inclusive
// ranges can underestimate generic-plan selectivity and select a bitmap that
// reads the whole payer before sorting and limiting it. Keep the indexed order.
func legacySettlementPayerSelectionSql(query string, limit int) string {
	if limit < 1 || limit > legacyFinancialCohortLimit ||
		strings.Count(query, "FROM legacy_settlement_intent") != 1 ||
		strings.Count(query, "WHERE shard=$1") != 1 ||
		strings.Count(query, "ORDER BY next_attempt_time,contract_id") != 1 ||
		strings.Count(query, "LIMIT ") != 1 ||
		!strings.HasSuffix(strings.TrimSpace(query), "LIMIT "+fmt.Sprint(limit)) {
		// This adapter shares the ordinary cursor/head builder. A changed
		// shape must fail before acquisition instead of losing payer scope.
		panic("unsupported legacy payer selection shape")
	}
	query = strings.Replace(query, "WHERE shard=$1", `WHERE shard=payer_shard.shard
 AND payer_network_id IS NOT NULL AND payer_network_id=$1::uuid`, 1)
	query = strings.Replace(query, "ORDER BY next_attempt_time,contract_id", "ORDER BY payer_network_id,next_attempt_time,contract_id", 1)
	return `SELECT payer_page.next_attempt_time,payer_page.contract_id,payer_page.pass_end_time
 FROM ` + legacySettlementShardValuesSql + ` CROSS JOIN LATERAL (` + query +
		`) AS payer_page(next_attempt_time,contract_id,pass_end_time)
 ORDER BY next_attempt_time,contract_id LIMIT ` + fmt.Sprint(limit)
}

// Immutable payer scope changes discovery only. Financial transactions,
// per-contract fallback, head revisits and confirmed-prefix handling are shared
// with the existing owner. One joined post batch publishes reclaimed credit
// before this bounded task turn returns; no task-row lock covers those posts.
func FlushLegacyPayerSettlements(ctx context.Context, payerNetworkId server.Id,
	after *LegacySettlementCursor, limit int) (result LegacySettlementFlushResult, returnErr error) {
	turn, err := runLegacyPayerSettlementPages(ctx, payerNetworkId, after, limit, false)
	return turn.LegacySettlementFlushResult, err
}

// A task keeps healthy pages inside one fixed budget. Each page still owns at
// most 256 visits, each cohort at most eight contracts, and each page joins its
// bounded projection posts before another page can begin.
func runLegacyPayerSettlementPages(ctx context.Context, payerNetworkId server.Id,
	after *LegacySettlementCursor, limit int, drain bool) (result LegacyPayerSettlementResult, returnErr error) {
	if payerNetworkId == (server.Id{}) || limit < 1 || limit > LegacySettlementPageLimit {
		return result, fmt.Errorf("invalid legacy payer settlement scope")
	}
	bounded, cancel := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
	defer cancel()
	trace, _ := ctx.Value(legacyTargetTraceKey{}).(*legacyTargetTrace)
	if trace == nil {
		origin := "payer_page"
		if drain {
			origin = "payer_turn"
		}
		trace = legacyTargetTraceRuntimeState.begin(-1, after, origin, nil)
	}
	if trace != nil {
		ctx = context.WithValue(ctx, legacyTargetTraceKey{}, trace)
		bounded = context.WithValue(bounded, legacyTargetTraceKey{}, trace)
	}
	observer := &legacySettlementTimingObserver{now: time.Now}
	bounded = context.WithValue(bounded, legacySettlementTimingKey{}, observer)
	result, returnErr = flushLegacyPayerSettlementPages(ctx, bounded, payerNetworkId, after, limit, drain,
		func(parent, page context.Context, shard int, cursor *LegacySettlementCursor, pageLimit int) (LegacySettlementFlushResult, error) {
			return flushLegacySettlementsPage(parent, page, shard, cursor, pageLimit,
				flushLegacySettlementWithGrantWait, flushLegacySettlementCohort)
		})
	result.Timings = observer.snapshot()
	result.Trace = trace.finish(result.LegacySettlementFlushResult, returnErr)
	return
}

// The injected page operation lets controls expire the owned budget after a
// real committed page. It never supplies a fresh timeout between pages. Only a
// confirmed prefix survives the owner's cancellation; foreign failures and
// parent cancellation retain their error and the interrupted cursor is not used.
func flushLegacyPayerSettlementPages(ctx, bounded context.Context, payerNetworkId server.Id,
	after *LegacySettlementCursor, limit int, drain bool,
	page func(context.Context, context.Context, int, *LegacySettlementCursor, int) (LegacySettlementFlushResult, error)) (result LegacyPayerSettlementResult, returnErr error) {
	bounded = context.WithValue(bounded, legacySettlementPayerScopeKey{}, payerNetworkId)
	shard := int(payerNetworkId[15]) % LegacySettlementShardCount
	result.Cursor = after
	if after != nil {
		result.PassEndTime = after.PassEndTime
	}
	ownedBudgetExpired := func(err error) bool {
		return ctx.Err() == nil && bounded.Err() != nil && context.Cause(bounded) == errLegacySettlementPageBudget &&
			result.Visited > 0 && result.Failed == 0 && isSettlementPageCancellation(err)
	}
	for {
		if err := bounded.Err(); err != nil {
			if ownedBudgetExpired(err) {
				result.More = true
				return result, nil
			}
			return result, err
		}
		part, err := page(ctx, bounded, shard, result.Cursor, limit)
		result.Pages++
		// An empty interrupted selection owns no new cursor. A successful
		// EOF deliberately resets it so predecessors can be rediscovered.
		if part.Visited > 0 || err == nil {
			result.Cursor = part.Cursor
		}
		if !part.PassEndTime.IsZero() {
			result.PassEndTime = part.PassEndTime
		}
		result.Visited += part.Visited
		result.Completed += part.Completed
		result.BusyOrGone += part.BusyOrGone
		result.Failed += part.Failed
		result.More = part.More
		result.FinancialCohortAttempts += part.FinancialCohortAttempts
		result.FinancialCohortSelected += part.FinancialCohortSelected
		result.FinancialCohortCompleted += part.FinancialCohortCompleted
		result.FinancialCohortFallbacks += part.FinancialCohortFallbacks
		result.HeadVisited += part.HeadVisited
		result.HeadCompleted += part.HeadCompleted
		result.HeadBusyOrGone += part.HeadBusyOrGone
		result.HeadFailed += part.HeadFailed
		result.BusyIntentUnavailable += part.BusyIntentUnavailable
		result.BusyContractUnavailable += part.BusyContractUnavailable
		result.BusyGrantSetMismatch += part.BusyGrantSetMismatch
		result.HeadBusyIntentUnavailable += part.HeadBusyIntentUnavailable
		result.HeadBusyContractUnavailable += part.HeadBusyContractUnavailable
		result.HeadBusyGrantSetMismatch += part.HeadBusyGrantSetMismatch
		result.BusyAdmissionDeferred += part.BusyAdmissionDeferred
		result.HeadBusyAdmissionDeferred += part.HeadBusyAdmissionDeferred
		result.HeadGrantWaitAttempted += part.HeadGrantWaitAttempted
		result.HeadGrantWaitCompleted += part.HeadGrantWaitCompleted
		result.HeadGrantWaitTimedOut += part.HeadGrantWaitTimedOut
		if err != nil {
			if ownedBudgetExpired(err) {
				result.More = true
				return result, nil
			}
			return result, err
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !drain || !part.More || part.BusyOrGone > 0 || part.Failed > 0 || part.Completed == 0 {
			return result, nil
		}
	}
}

const nextLegacySettlementPayerAttemptSql = `SELECT min(payer_head.next_attempt_time)
 FROM ` + legacySettlementShardValuesSql + ` CROSS JOIN LATERAL (
 SELECT payer_network_id,next_attempt_time FROM legacy_settlement_intent
 WHERE shard=payer_shard.shard AND payer_network_id IS NOT NULL
 AND payer_network_id>=$1::uuid
 ORDER BY payer_network_id,next_attempt_time,contract_id LIMIT 1
 ) AS payer_head WHERE payer_head.payer_network_id=$1::uuid`

// One ordered boundary per original shard bounds this read to sixteen metadata
// candidates. If the first payer at or after the key is a successor, this payer
// has no work in that shard. The outer equality excludes that boundary row.
// The task's finishing transaction holds no financial lock; future work keeps
// its existing accounting/operational retry deadline.
func NextLegacySettlementPayerAttemptInTx(ctx context.Context, tx server.PgTx, payerNetworkId server.Id) (next *time.Time, returnErr error) {
	if payerNetworkId == (server.Id{}) {
		return nil, fmt.Errorf("invalid legacy payer settlement scope")
	}
	returnErr = tx.QueryRow(ctx, nextLegacySettlementPayerAttemptSql, payerNetworkId).Scan(&next)
	return
}
