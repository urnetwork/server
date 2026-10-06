// Completed URL turns have durable claim identities, independent of URL evidence.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

const (
	ProviderUrlProbeCompletedWindow        = 4 * time.Hour
	providerUrlProbeCompletionSkew         = 2 * time.Minute
	providerUrlProbeCompletionReportMaxAge = 24 * time.Hour
	providerUrlProbeRunRetention           = 7 * 24 * time.Hour
)

// One completed turn, including setup failures; retries retain every field.
// AllowPacing is false when platform readiness forbids a provider verdict.
// Its first durable completion can still release its exact live-claim lease
// into bounded local retry; this grants no measured evidence or quota credit.
type ProviderUrlProbeCompletion struct {
	ClientId     server.Id
	ClaimOrdinal int64
	CompletedAt  time.Time
	ProbeFailure string
	AllowPacing  bool
}

// First acceptance fixes the window boundary. A replay cannot move it.
type ProviderUrlProbeCompletionReceipt struct {
	CompletedAt time.Time
	ReceivedAt  time.Time
	Replay      bool
}

// Records only a server-issued claim. The cycle lock serializes acceptance,
// expiry, and later claims. URL ratio, quota, and security evidence are untouched.
func CompleteProviderUrlProbeRun(ctx context.Context, completion ProviderUrlProbeCompletion, receivedAt time.Time) (receipt *ProviderUrlProbeCompletionReceipt, returnErr error) {
	if completion.ClaimOrdinal <= 0 || completion.CompletedAt.IsZero() || len(completion.ProbeFailure) > 64 {
		return nil, fmt.Errorf("invalid URL completion receipt")
	}
	receivedAt = receivedAt.UTC().Truncate(time.Microsecond)
	recordedFailure := completion.ProbeFailure
	server.Tx(ctx, func(tx server.PgTx) {
		receipt, returnErr = nil, nil
		var latestOrdinal int64
		err := tx.QueryRow(ctx, `SELECT claim_ordinal FROM provider_egress_probe_cycle
			WHERE client_id=$1 FOR UPDATE`, completion.ClientId).Scan(&latestOrdinal)
		if errors.Is(err, pgx.ErrNoRows) {
			returnErr = fmt.Errorf("unknown URL completion provider")
			return
		}
		server.Raise(err)
		if latestOrdinal < completion.ClaimOrdinal {
			returnErr = fmt.Errorf("unknown URL completion claim")
			return
		}
		var claimedAt time.Time
		var completedAt, storedReceivedAt *time.Time
		err = tx.QueryRow(ctx, `SELECT claimed_at,completed_at,received_at,probe_failure FROM provider_url_probe_run
			WHERE client_id=$1 AND claim_ordinal=$2 FOR UPDATE`,
			completion.ClientId, completion.ClaimOrdinal).Scan(&claimedAt, &completedAt, &storedReceivedAt, &recordedFailure)
		if errors.Is(err, pgx.ErrNoRows) {
			returnErr = fmt.Errorf("unknown or retired URL completion claim")
			return
		}
		server.Raise(err)
		if completedAt != nil {
			receipt = &ProviderUrlProbeCompletionReceipt{CompletedAt: *completedAt, ReceivedAt: *storedReceivedAt, Replay: true}
			return
		}
		recordedFailure = completion.ProbeFailure
		reportedAt := completion.CompletedAt.UTC().Truncate(time.Microsecond)
		if receivedAt.Before(claimedAt) || receivedAt.Sub(claimedAt) > providerUrlProbeCompletionReportMaxAge ||
			reportedAt.Before(claimedAt.Add(-providerUrlProbeCompletionSkew)) ||
			receivedAt.Add(providerUrlProbeCompletionSkew).Before(reportedAt) {
			returnErr = fmt.Errorf("URL completion timestamp is outside its issued claim")
			return
		}
		// Keep the raw source timestamp for audit. Bounded clock skew cannot
		// create a completion before issue or after first server observation.
		effectiveAt := reportedAt
		if effectiveAt.Before(claimedAt) {
			effectiveAt = claimedAt
		}
		if receivedAt.Before(effectiveAt) {
			effectiveAt = receivedAt
		}
		counted := effectiveAt.After(receivedAt.Add(-ProviderUrlProbeCompletedWindow))
		increment := 0
		if counted {
			increment = 1
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_url_probe_run
			SET reported_completed_at=$3,completed_at=$4,received_at=$5,probe_failure=$6,counted=$7
			WHERE client_id=$1 AND claim_ordinal=$2`,
			completion.ClientId, completion.ClaimOrdinal, reportedAt, effectiveAt, receivedAt, completion.ProbeFailure, counted))
		// A finished local turn no longer owns an in-flight lease. Only its
		// exact issued deadline may enter local retry; custom/newer deadlines
		// and newer accepted results keep their owners. Anchor the local retry
		// to first server receipt, and never extend an earlier deadline.
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle AS cycle SET
			completed_run_count=completed_run_count+$4,
			completed_next_expiry_at=CASE WHEN $4>0
				THEN LEAST(COALESCE(completed_next_expiry_at,$5),$5) ELSE completed_next_expiry_at END,
			completed_priority_ready=false,
			next_attempt_at=CASE WHEN $7<>'' AND cycle.claim_ordinal=$2
				AND ($6 OR cycle.next_attempt_at=$9)
				AND ((SELECT `+providerUrlProbeMeasuredWorkDueSql("recent", "$3", "$8")+` FROM (`+providerUrlProbeRunWindowSql("cycle.client_id", "$3")+`) AS recent)
					OR (`+providerHasUrlSecurityExceptionSql("cycle.client_id")+`))
				AND (cycle.latest_result_at IS NULL OR cycle.latest_result_at<$3)
				THEN CASE WHEN $6 THEN `+providerUrlProbePacedAttemptSql("cycle", "$3", "0")+`
					ELSE LEAST(cycle.next_attempt_at, `+providerUrlProbePacedAttemptSql("cycle", "$10", "0")+`) END
				ELSE cycle.next_attempt_at END
			WHERE cycle.client_id=$1`,
			completion.ClientId, completion.ClaimOrdinal, effectiveAt, increment,
			effectiveAt.Add(ProviderUrlProbeCompletedWindow), completion.AllowPacing, completion.ProbeFailure, ProviderUrlProbeRunTarget,
			claimedAt.Add(ProviderEgressProbeAttemptBackoff), receivedAt))
		receipt = &ProviderUrlProbeCompletionReceipt{CompletedAt: effectiveAt, ReceivedAt: receivedAt}
	})
	if returnErr != nil || receipt == nil {
		return
	}
	// This projection follows the receipt commit. Keeping its row lock out of
	// the cycle transaction avoids an opposite-order lock with legacy writers.
	server.Tx(ctx, func(tx server.PgTx) {
		setProviderEgressProbeAttemptInTx(ctx, tx, &ProviderEgressProbeAttempt{
			ClientId: completion.ClientId, AttemptAt: receipt.CompletedAt, ProbeFailure: recordedFailure,
		}, false)
	})
	return
}

// A missing convergence epoch is unknown, not four hours of observed zero.
// Ingest collects receipts throughout writer-first rollout; ordering stays old.
func providerUrlProbeCompletedPriorityReady(now time.Time) bool {
	since := GetProviderEgressRules().UrlCompletedRunPrioritySince
	return !since.IsZero() && !now.Before(since.Add(ProviderUrlProbeCompletedWindow))
}

// A retained issued claim is the idempotency authority. Once retired, an old
// completion is rejected, never recreated with a new timestamp or count.
func RemoveExpiredProviderUrlProbeRuns(ctx context.Context, now time.Time, limit int) (removed int64) {
	return removeExpiredProviderUrlProbeRuns(ctx, now, limit, nil)
}

func removeExpiredProviderUrlProbeRuns(ctx context.Context, now time.Time, limit int, observation *ProviderUrlProbeDueObservation) (removed int64) {
	if limit <= 0 {
		return
	}
	observation.measure(ProviderUrlProbeDueRetentionTransaction, func() {
		server.Tx(ctx, func(tx server.PgTx) {
			observation.measure(ProviderUrlProbeDueRetentionQuery, func() {
				result, err := tx.Exec(ctx, providerUrlProbeRunRetentionSql,
					now.Add(-providerUrlProbeRunRetention).UTC(), limit)
				server.Raise(err)
				removed = result.RowsAffected()
			})
		}, observation.database(true))
	})
	return
}

// Tuple identities stay inside the statement that locked these rows. The
// explicit TID set bounds generic DELETE plans even with large retained history;
// immutable client/claim predicates still identify every deleted receipt.
const providerUrlProbeRunRetentionSql = `WITH retired AS MATERIALIZED (
	SELECT client_id,claim_ordinal,ctid FROM provider_url_probe_run
	WHERE NOT counted AND claimed_at<$1
	ORDER BY claimed_at,client_id,claim_ordinal LIMIT $2 FOR UPDATE SKIP LOCKED
) DELETE FROM provider_url_probe_run AS run USING retired
	WHERE run.client_id=retired.client_id AND run.claim_ordinal=retired.claim_ordinal
	AND run.ctid=ANY(ARRAY(SELECT ctid FROM retired))`
