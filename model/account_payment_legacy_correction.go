// Corrected amounts for legacy keyed provider payments
// (account_payment_legacy_attempt.go). Two defects of the July to September
// planner inflated them:
//   - a plan's subsidy took the revenue term 0.2 x net revenue whenever it
//     exceeded the configured floor, and the net revenue input summed the full
//     revenue of every balance a plan touched, so anomalous balances set the
//     subsidy of the September plans far above the floor
//   - the reliability subsidy per payout was paid in full by every plan, however
//     short its window, instead of pro rata over its period
//
// A payment's submission basis is immutable (account_payment_submission_basis_guard)
// and its payout may change only by an exact bonus, so a corrected amount is a
// fresh replacement payment, never an update of the original:
//  1. one transaction cancels the exact observed legacy attempt, a status-only
//     change that keeps its legacy key, and inserts the replacement in the same
//     plan, network and wallet with the corrected amounts. Both payments journal
//     the pair. The replacement starts with attribution_review_required, which
//     the earning admission refuses, so it cannot be paid before its sweeps are
//     its own.
//  2. the replacement takes earning authority from the original's sweeps:
//     either one anchor sweep for a zero-revenue history (the first, in contract
//     order, that satisfies the earning admission), or every sweep, in bounded
//     transactions, when any sweep carries revenue
//  3. the review hold is then released: with an anchor once the replacement
//     owns it, journaling on both payments the sweeps the original keeps
//     pending reattribution; with a full move once the original owns none. A
//     replacement that cannot cover the transfer fee keeps its hold.
//
// The canceled original keeps its key and has no retained request: it is never
// advanced (it is canceled), never re-planned or restored (both require a null
// key) and no longer a legacy attempt, so the legacy reconciliation never
// releases its key. Its remaining sweeps are therefore never selected by a plan
// (only a null owner or a canceled owner without a key is unpaid), and contract
// retention keeps their contracts while the keyed owner is not completed. Only
// the replacement can be submitted, once, through the ordinary fresh attempt
// with its own key. The replacement carries the original's payout byte count,
// so transfer statistics count those bytes exactly once.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/urnetwork/server"
)

var ErrLegacyProviderPaymentReplacementChanged = errors.New("legacy provider payment replacement changed; nothing written")

// Journaled on the original and on its replacement in the replacing
// transaction. The two link events take the payment id as their event id, so
// provider-facing listings find them with a primary key probe.
const (
	legacyProviderPaymentReplacedEventType    = "legacy_payment_replaced"
	legacyProviderPaymentReplacementEventType = "legacy_payment_replacement"
	legacyProviderPaymentHeldEventType        = "legacy_payment_replacement_held"
	// journaled on both payments of an anchored pair
	legacyProviderPaymentPendingReattributionEventType = "legacy_payment_sweeps_pending_reattribution"
)

// The earning admission's per-sweep conditions (inspectProviderUsdcPaymentAttribution)
// for sweep `s` and contract `c`, given the owner's network expression and the
// position of the earning cutoff parameter.
func legacyProviderPaymentAnchorSweepSql(networkExpression string, cutoffParam int) string {
	return fmt.Sprintf(`
		s.network_id = %s AND s.payout_net_revenue_nano_cents >= 0 AND
		c.close_time IS NOT NULL AND ($%d::timestamp IS NULL OR c.close_time < $%d::timestamp) AND
		c.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')`,
		networkExpression, cutoffParam, cutoffParam)
}

// The payment of the expression is a legacy original that a corrected
// replacement superseded; provider-facing listings show only the replacement.
func legacyProviderPaymentReplacedSql(paymentIdExpression string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM audit_account_payment replaced
		WHERE replaced.event_id = %s AND replaced.payment_id = %s AND replaced.event_type = '%s')`,
		paymentIdExpression, paymentIdExpression, legacyProviderPaymentReplacedEventType)
}

// The legacy original that the payment of the expression replaces, or null.
func legacyProviderPaymentReplacesSql(paymentIdExpression string) string {
	return fmt.Sprintf(`(SELECT (replacement.event_details::jsonb->>'original_payment_id')::uuid FROM audit_account_payment replacement
		WHERE replacement.event_id = %s AND replacement.payment_id = %s AND replacement.event_type = '%s')`,
		paymentIdExpression, paymentIdExpression, legacyProviderPaymentReplacementEventType)
}

var ErrLegacyProviderPaymentAnchorUnavailable = errors.New("legacy provider payment has no sweep the earning admission accepts; replacement held")

// The configured subsidy floor and the period over which the reliability
// subsidy per payout is paid. Corrections never increase an amount.
type LegacyProviderPaymentCorrectionPolicy struct {
	MinPayoutUsd                   float64
	UsdPerActiveUser               float64
	SubscriptionNetRevenueFraction float64
	ReliabilityPeriod              time.Duration
	// a replacement at or below this cannot cover the transfer fee and is held
	TransferFee NanoCents
}

// From config/<env>/subsidy.yml: the floor min_payout_usd per `days`, and the
// reliability subsidy per payout over the same `days`, as the current planner pays it.
func DefaultLegacyProviderPaymentCorrectionPolicy() *LegacyProviderPaymentCorrectionPolicy {
	config := EnvSubsidyConfig()
	return &LegacyProviderPaymentCorrectionPolicy{
		MinPayoutUsd:                   config.MinPayoutUsd,
		UsdPerActiveUser:               config.UsdPerActiveUser,
		SubscriptionNetRevenueFraction: config.SubscriptionNetRevenueFraction,
		ReliabilityPeriod:              config.Duration(),
		TransferFee:                    UsdToNanoCents(0.01),
	}
}

// The subsidy epoch of a payment's plan.
type LegacyProviderPaymentSubsidyWindow struct {
	StartTime       time.Time
	EndTime         time.Time
	ActiveUserCount int64
	NetRevenue      NanoCents
}

// The corrected amounts of one legacy attempt. Other payout components
// (bonus and sweep net revenue) are carried unchanged.
type LegacyProviderPaymentCorrection struct {
	Original           *AccountPayment
	SubsidyPayout      NanoCents
	ReliabilitySubsidy NanoCents
	Payout             NanoCents
	// at or below the transfer fee: the replacement keeps its review hold
	Dust bool
}

func (self *LegacyProviderPaymentCorrection) Changed() bool {
	return self.SubsidyPayout != self.Original.SubsidyPayout || self.ReliabilitySubsidy != self.Original.ReliabilitySubsidy
}

// Rounds a nonnegative rational half away from zero.
func roundNonnegativeRat(value *big.Rat) int64 {
	half := new(big.Rat).Add(value, big.NewRat(1, 2))
	return new(big.Int).Quo(half.Num(), half.Denom()).Int64()
}

// CorrectLegacyProviderPayment scales the subsidy back to the formula without
// its revenue term (the floor, or the active-user term when larger) when the
// revenue term set the plan's subsidy, and the reliability subsidy to the
// window's share of its period. Exact rational arithmetic; neither component
// can increase, since both factors are at most 1 and the components are integers.
func CorrectLegacyProviderPayment(payment *AccountPayment, window *LegacyProviderPaymentSubsidyWindow, policy *LegacyProviderPaymentCorrectionPolicy) *LegacyProviderPaymentCorrection {
	usd := func(nanoCents int64) *big.Rat { return big.NewRat(nanoCents, 1_000_000_000) }
	ratio := func(value float64) *big.Rat { return new(big.Rat).SetFloat64(value) }
	maxRat := func(a *big.Rat, b *big.Rat) *big.Rat {
		if a.Cmp(b) < 0 {
			return b
		}
		return a
	}

	floorUsd := maxRat(ratio(policy.MinPayoutUsd), new(big.Rat).Mul(ratio(policy.UsdPerActiveUser), big.NewRat(window.ActiveUserCount, 1)))
	revenueUsd := new(big.Rat).Mul(ratio(policy.SubscriptionNetRevenueFraction), usd(window.NetRevenue))
	subsidyUsd := maxRat(floorUsd, revenueUsd)
	subsidyFactor := big.NewRat(1, 1)
	if 0 < subsidyUsd.Sign() {
		subsidyFactor = new(big.Rat).Quo(floorUsd, subsidyUsd)
	}

	reliabilityFactor := big.NewRat(1, 1)
	if 0 < policy.ReliabilityPeriod {
		windowDuration := window.EndTime.Sub(window.StartTime)
		if windowDuration < 0 {
			windowDuration = 0
		}
		if windowDuration < policy.ReliabilityPeriod {
			reliabilityFactor = big.NewRat(int64(windowDuration), int64(policy.ReliabilityPeriod))
		}
	}

	subsidyPayout := roundNonnegativeRat(new(big.Rat).Mul(big.NewRat(payment.SubsidyPayout, 1), subsidyFactor))
	reliabilitySubsidy := roundNonnegativeRat(new(big.Rat).Mul(big.NewRat(payment.ReliabilitySubsidy, 1), reliabilityFactor))
	payout := payment.Payout - (payment.SubsidyPayout - subsidyPayout) - (payment.ReliabilitySubsidy - reliabilitySubsidy)
	return &LegacyProviderPaymentCorrection{
		Original:           payment,
		SubsidyPayout:      subsidyPayout,
		ReliabilitySubsidy: reliabilitySubsidy,
		Payout:             payout,
		Dust:               payout <= policy.TransferFee,
	}
}

// The subsidy epoch of each payment's plan. A payment whose plan does not have
// exactly one epoch is absent from the map, and its attempt stays held.
func GetLegacyProviderPaymentSubsidyWindows(ctx context.Context, paymentIds []server.Id) (windows map[server.Id]*LegacyProviderPaymentSubsidyWindow, returnErr error) {
	windows = map[server.Id]*LegacyProviderPaymentSubsidyWindow{}
	if len(paymentIds) == 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT p.payment_id, MIN(s.start_time), MIN(s.end_time), MIN(s.active_user_count), MIN(s.net_revenue_nano_cents)
			FROM account_payment p
			JOIN subsidy_payment s ON s.payment_plan_id = p.payment_plan_id
			WHERE p.payment_id = ANY($1)
			GROUP BY p.payment_id
			HAVING COUNT(*) = 1
			`,
			paymentIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				window := &LegacyProviderPaymentSubsidyWindow{}
				server.Raise(result.Scan(&paymentId, &window.StartTime, &window.EndTime, &window.ActiveUserCount, &window.NetRevenue))
				windows[paymentId] = window
			}
		})
	})
	return
}

// The sweeps each payment owns, counted from the payment index.
func CountProviderPaymentSweeps(ctx context.Context, paymentIds []server.Id) (sweepCount int64, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT COALESCE(SUM(s.n), 0)::bigint
			FROM unnest($1::uuid[]) AS payment(payment_id)
			CROSS JOIN LATERAL (
				SELECT COUNT(*) AS n FROM transfer_escrow_sweep WHERE payment_id = payment.payment_id
			) s
			`,
			paymentIds,
		).Scan(&sweepCount))
	})
	return
}

type legacyProviderPaymentReplacementDetails struct {
	OriginalPaymentId              server.Id `json:"original_payment_id"`
	ReplacementPaymentId           server.Id `json:"replacement_payment_id"`
	OriginalPayout                 NanoCents `json:"original_payout_nano_cents"`
	OriginalSubsidyPayout          NanoCents `json:"original_subsidy_payout_nano_cents"`
	OriginalReliabilitySubsidy     NanoCents `json:"original_reliability_subsidy_nano_cents"`
	ReplacementPayout              NanoCents `json:"replacement_payout_nano_cents"`
	ReplacementSubsidyPayout       NanoCents `json:"replacement_subsidy_payout_nano_cents"`
	ReplacementReliabilitySubsidy  NanoCents `json:"replacement_reliability_subsidy_nano_cents"`
	MinPayoutUsd                   float64   `json:"min_payout_usd"`
	UsdPerActiveUser               float64   `json:"usd_per_active_user"`
	SubscriptionNetRevenueFraction float64   `json:"subscription_net_revenue_fraction"`
	ReliabilityPeriodSeconds       int64     `json:"reliability_period_seconds"`
	Observation                    string    `json:"observation"`
}

// ReplaceLegacyProviderPayment cancels the exact observed legacy attempt and
// inserts its corrected replacement in one transaction, journaled on both.
// Refuses without a write when the attempt changed (including a retained
// request appearing for its key), carries a bonus, belongs to a plan whose
// sweep assignment is pending, or the correction would not reduce it.
func ReplaceLegacyProviderPayment(ctx context.Context, correction *LegacyProviderPaymentCorrection, policy *LegacyProviderPaymentCorrectionPolicy, observation string) (replacementPaymentId server.Id, returnErr error) {
	original := correction.Original
	if original == nil || original.WalletId == nil || original.BonusPayout != 0 || !correction.Changed() ||
		correction.SubsidyPayout > original.SubsidyPayout || correction.ReliabilitySubsidy > original.ReliabilitySubsidy ||
		correction.SubsidyPayout < 0 || correction.ReliabilitySubsidy < 0 || correction.Payout < 0 {
		return server.Id{}, ErrLegacyProviderPaymentReplacementChanged
	}
	// everything journaled is fixed before the transaction writes anything
	candidatePaymentId := server.NewId()
	details, err := json.Marshal(&legacyProviderPaymentReplacementDetails{
		OriginalPaymentId:              original.PaymentId,
		ReplacementPaymentId:           candidatePaymentId,
		OriginalPayout:                 original.Payout,
		OriginalSubsidyPayout:          original.SubsidyPayout,
		OriginalReliabilitySubsidy:     original.ReliabilitySubsidy,
		ReplacementPayout:              correction.Payout,
		ReplacementSubsidyPayout:       correction.SubsidyPayout,
		ReplacementReliabilitySubsidy:  correction.ReliabilitySubsidy,
		MinPayoutUsd:                   policy.MinPayoutUsd,
		UsdPerActiveUser:               policy.UsdPerActiveUser,
		SubscriptionNetRevenueFraction: policy.SubscriptionNetRevenueFraction,
		ReliabilityPeriodSeconds:       int64(policy.ReliabilityPeriod / time.Second),
		Observation:                    observation,
	})
	if err != nil || providerPaymentReceiptLimit < len(details) {
		return server.Id{}, ErrLegacyProviderPaymentReplacementChanged
	}
	wallet, err := legacyProviderPaymentWallet(ctx, original)
	if err != nil {
		return server.Id{}, err
	}
	server.OwnedTx(ctx, providerPaymentAttributionKeys(original.PaymentId, wallet.Blockchain, wallet.WalletAddress, ""), func(tx server.PgTx) {
		returnErr = nil
		if !lockLegacyProviderPaymentAttributionInTx(ctx, tx, original, wallet) {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}
		var exact bool
		server.Raise(tx.QueryRow(
			ctx,
			`
			SELECT EXISTS (
				SELECT 1 FROM account_payment p
				WHERE p.payment_id = $1 AND p.payment_plan_id = $2 AND p.payout_byte_count = $3 AND p.min_sweep_time = $4 AND
					p.subsidy_payout_nano_cents = $5 AND p.reliability_subsidy_nano_cents = $6 AND
					p.bonus_payout_nano_cents = 0 AND NOT p.attribution_review_required AND
					NOT EXISTS (SELECT 1 FROM account_payment_bonus b WHERE b.payment_id = p.payment_id OR b.origin_payment_id = p.payment_id) AND
					NOT `+paymentPlanAssignmentPendingSql+`
			)
			`,
			original.PaymentId,
			original.PaymentPlanId,
			original.PayoutByteCount,
			original.MinSweepTime,
			original.SubsidyPayout,
			original.ReliabilitySubsidy,
		).Scan(&exact))
		if !exact {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}

		now := server.NowUtc()
		// status only: the submission basis guard admits it and the key stays.
		// The row is locked, so this cannot miss; a miss writes nothing else.
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE account_payment SET canceled = true, cancel_time = $2 WHERE payment_id = $1 AND NOT canceled AND NOT completed`,
			original.PaymentId,
			now,
		))
		if tag.RowsAffected() != 1 {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}
		replacementPaymentId = candidatePaymentId
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO account_payment (
				payment_id,
				payment_plan_id,
				network_id,
				wallet_id,
				payout_byte_count,
				payout_nano_cents,
				subsidy_payout_nano_cents,
				reliability_subsidy_nano_cents,
				min_sweep_time,
				create_time,
				attribution_review_required
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, true)
			`,
			replacementPaymentId,
			original.PaymentPlanId,
			original.NetworkId,
			*original.WalletId,
			original.PayoutByteCount,
			correction.Payout,
			correction.SubsidyPayout,
			correction.ReliabilitySubsidy,
			original.MinSweepTime,
			now,
		))

		// the attempt with its retained key, as the reconciliation journals it
		retainProviderAttemptInTx(ctx, tx, original, "LEGACY_CORRECTED_REPLACED", string(details))
		for _, event := range []struct {
			paymentId server.Id
			eventType string
		}{
			{paymentId: original.PaymentId, eventType: legacyProviderPaymentReplacedEventType},
			{paymentId: replacementPaymentId, eventType: legacyProviderPaymentReplacementEventType},
		} {
			// keyed by the payment id; see legacyProviderPaymentReplacedSql
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details) VALUES ($1, $1, $2, $3)`,
				event.paymentId,
				event.eventType,
				string(details),
			))
		}
	}, server.TxReadCommitted)
	if returnErr != nil {
		replacementPaymentId = server.Id{}
	}
	return
}

// A journaled replacement and the state that decides its next step.
type LegacyProviderPaymentReplacement struct {
	OriginalPaymentId    server.Id
	ReplacementPaymentId server.Id
	Payout               NanoCents
	// the replacement still holds attribution review: moving, or dust
	ReviewHeld bool
	Completed  bool
	Canceled   bool
	// the original still owns a sweep
	OriginalOwnsSweeps bool
	// Nonzero revenue requires its original sweep provenance, not an anchor.
	OriginalOwnsRevenue bool
	// the replacement owns a sweep (its anchor, or moved sweeps)
	ReplacementOwnsSweeps bool
}

// Every journaled replacement, ordered by its original payment.
func GetLegacyProviderPaymentReplacements(ctx context.Context) (replacements []*LegacyProviderPaymentReplacement, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				(a.event_details::jsonb->>'original_payment_id')::uuid AS original_payment_id,
				r.payment_id,
				r.payout_nano_cents,
				r.attribution_review_required,
				r.completed,
				r.canceled,
				EXISTS (
					SELECT 1 FROM transfer_escrow_sweep s
					WHERE s.payment_id = (a.event_details::jsonb->>'original_payment_id')::uuid
				),
				EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id = r.payment_id),
				EXISTS (SELECT 1 FROM transfer_escrow_sweep s
					WHERE s.payment_id=(a.event_details::jsonb->>'original_payment_id')::uuid
					AND s.payout_net_revenue_nano_cents<>0)
			FROM audit_account_payment a
			JOIN account_payment r ON r.payment_id = a.payment_id
			WHERE a.event_type = $1
			ORDER BY 1
			`,
			legacyProviderPaymentReplacementEventType,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				replacement := &LegacyProviderPaymentReplacement{}
				server.Raise(result.Scan(
					&replacement.OriginalPaymentId,
					&replacement.ReplacementPaymentId,
					&replacement.Payout,
					&replacement.ReviewHeld,
					&replacement.Completed,
					&replacement.Canceled,
					&replacement.OriginalOwnsSweeps,
					&replacement.ReplacementOwnsSweeps,
					&replacement.OriginalOwnsRevenue,
				))
				replacements = append(replacements, replacement)
			}
		})
	})
	return
}

// Locks the journaled pair in payment id order and confirms the original is
// the canceled, keyed, unsubmitted legacy attempt and the replacement is its
// unsubmitted, review-held successor in the same plan and network.
func lockLegacyProviderPaymentReplacementInTx(ctx context.Context, tx server.PgTx, replacement *LegacyProviderPaymentReplacement) bool {
	return lockLegacyProviderPaymentReplacementReviewInTx(ctx, tx, replacement, true)
}

// Repair requires the exact released state, never a moving or submitted pair.
func lockLegacyProviderPaymentReplacementReviewInTx(ctx context.Context, tx server.PgTx, replacement *LegacyProviderPaymentReplacement, reviewHeld bool) bool {
	result, err := tx.Query(
		ctx,
		`SELECT payment_id FROM account_payment WHERE payment_id = ANY($1) ORDER BY payment_id FOR UPDATE`,
		[]server.Id{replacement.OriginalPaymentId, replacement.ReplacementPaymentId},
	)
	locked := 0
	server.WithPgResult(result, err, func() {
		for result.Next() {
			locked += 1
		}
	})
	if locked != 2 {
		return false
	}
	var paired bool
	server.Raise(tx.QueryRow(
		ctx,
		`
		SELECT EXISTS (
			SELECT 1
			FROM account_payment o
			JOIN account_payment r ON r.payment_id = $2
			JOIN audit_account_payment a ON a.payment_id = r.payment_id AND a.event_type = $3
				AND (a.event_details::jsonb->>'original_payment_id')::uuid = o.payment_id
			WHERE o.payment_id = $1 AND
				o.canceled AND NOT o.completed AND o.circle_idempotency_key IS NOT NULL AND
				o.payment_record IS NULL AND o.tx_hash IS NULL AND
				NOT r.completed AND NOT r.canceled AND r.attribution_review_required=$4 AND
				r.circle_idempotency_key IS NULL AND r.payment_record IS NULL AND r.tx_hash IS NULL AND
				r.network_id = o.network_id AND r.payment_plan_id = o.payment_plan_id
		)
		`,
		replacement.OriginalPaymentId,
		replacement.ReplacementPaymentId,
		legacyProviderPaymentReplacementEventType,
		reviewHeld,
	).Scan(&paired))
	return paired
}

// Old anchor releases can be repaired only before any submission. Restore the
// hold under both row locks before moving retained revenue; no amount changes.
func ReholdLegacyProviderPaymentReplacement(ctx context.Context, replacement *LegacyProviderPaymentReplacement) (held bool, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		held, returnErr = false, nil
		if !lockLegacyProviderPaymentReplacementReviewInTx(ctx, tx, replacement, false) {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}
		var revenue bool
		server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_escrow_sweep
			WHERE payment_id=$1 AND payout_net_revenue_nano_cents<>0)`, replacement.OriginalPaymentId).Scan(&revenue))
		if !revenue {
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET attribution_review_required=true WHERE payment_id=$1`, replacement.ReplacementPaymentId))
		held = true
	}, server.TxReadCommitted)
	return
}

// MoveLegacyProviderPaymentReplacementSweeps moves at most batchSize of the
// original's sweeps to its replacement in one transaction, in contract order.
// A sweep reserved by a pending payment plan refuses the whole batch
// (payment_plan_sweep_owner_guard).
func MoveLegacyProviderPaymentReplacementSweeps(ctx context.Context, replacement *LegacyProviderPaymentReplacement, batchSize int) (movedCount int64, returnErr error) {
	if batchSize <= 0 {
		return 0, ErrLegacyProviderPaymentReplacementChanged
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		movedCount = 0
		if !lockLegacyProviderPaymentReplacementInTx(ctx, tx, replacement) {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE transfer_escrow_sweep s
			SET payment_id = $2
			FROM (
				SELECT contract_id, balance_id, network_id
				FROM transfer_escrow_sweep
				WHERE payment_id = $1
				ORDER BY contract_id
				LIMIT $3
			) batch
			WHERE s.contract_id = batch.contract_id AND s.balance_id = batch.balance_id AND
				s.network_id = batch.network_id AND s.payment_id = $1
			`,
			replacement.OriginalPaymentId,
			replacement.ReplacementPaymentId,
			batchSize,
		))
		movedCount = tag.RowsAffected()
	}, server.TxReadCommitted)
	return
}

// The payments of the given ids that own a sweep the earning admission
// accepts, so an anchored replacement can take one.
func CountLegacyProviderPaymentAnchors(ctx context.Context, paymentIds []server.Id, cutoff *time.Time) (anchoredCount int, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT COUNT(*)
			FROM unnest($1::uuid[]) AS payment(payment_id)
			JOIN account_payment p ON p.payment_id = payment.payment_id
			WHERE EXISTS (
				SELECT 1 FROM transfer_escrow_sweep s
				JOIN transfer_contract c ON c.contract_id = s.contract_id
				WHERE s.payment_id = p.payment_id AND `+legacyProviderPaymentAnchorSweepSql("p.network_id", 2)+`
			)
			`,
			paymentIds,
			cutoff,
		).Scan(&anchoredCount))
	})
	return
}

// MoveLegacyProviderPaymentReplacementAnchor gives the replacement the
// original's first sweep, in contract order, that the earning admission
// accepts. A replacement that already owns a sweep is unchanged. The original
// keeps every other sweep.
func MoveLegacyProviderPaymentReplacementAnchor(ctx context.Context, replacement *LegacyProviderPaymentReplacement, cutoff *time.Time) (anchored bool, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		anchored = false
		if !lockLegacyProviderPaymentReplacementInTx(ctx, tx, replacement) {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}
		var owns bool
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM transfer_escrow_sweep WHERE payment_id = $1)`,
			replacement.ReplacementPaymentId,
		).Scan(&owns))
		if owns {
			return
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE transfer_escrow_sweep moved
			SET payment_id = $2
			FROM (
				SELECT s.contract_id, s.balance_id, s.network_id
				FROM transfer_escrow_sweep s
				JOIN transfer_contract c ON c.contract_id = s.contract_id
				WHERE s.payment_id = $1 AND `+legacyProviderPaymentAnchorSweepSql("(SELECT network_id FROM account_payment WHERE payment_id = $1)", 3)+`
				ORDER BY s.contract_id, s.balance_id, s.network_id
				LIMIT 1
			) anchor
			WHERE moved.contract_id = anchor.contract_id AND moved.balance_id = anchor.balance_id AND
				moved.network_id = anchor.network_id AND moved.payment_id = $1
			`,
			replacement.OriginalPaymentId,
			replacement.ReplacementPaymentId,
			cutoff,
		))
		if tag.RowsAffected() != 1 {
			returnErr = ErrLegacyProviderPaymentAnchorUnavailable
			return
		}
		anchored = true
	}, server.TxReadCommitted)
	return
}

type legacyProviderPaymentReattributionDetails struct {
	OriginalPaymentId        server.Id `json:"original_payment_id"`
	ReplacementPaymentId     server.Id `json:"replacement_payment_id"`
	RemainingSweepCount      int64     `json:"remaining_sweep_count"`
	RemainingPayoutByteCount int64     `json:"remaining_payout_byte_count"`
	ReplacementSweepCount    int64     `json:"replacement_sweep_count"`
	Reason                   string    `json:"reason"`
}

// ReleaseLegacyProviderPaymentReplacement releases the review hold. An anchored
// replacement needs its anchor, and journals on both payments, once, the sweeps
// the original keeps pending reattribution. A fully moved replacement needs
// every sweep of the original. A replacement at or below the transfer fee
// keeps its hold; the hold is journaled once.
func ReleaseLegacyProviderPaymentReplacement(ctx context.Context, replacement *LegacyProviderPaymentReplacement, policy *LegacyProviderPaymentCorrectionPolicy, anchored bool) (released bool, returnErr error) {
	earningPolicy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return false, err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		released = false
		if !lockLegacyProviderPaymentReplacementInTx(ctx, tx, replacement) {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}
		// counted from the (network, payment, bytes) index
		var remainingSweepCount, remainingByteCount, replacementSweepCount int64
		var payout NanoCents
		server.Raise(tx.QueryRow(
			ctx,
			`
			SELECT
				(SELECT COUNT(*) FROM transfer_escrow_sweep s WHERE s.network_id = o.network_id AND s.payment_id = o.payment_id),
				(SELECT COALESCE(SUM(s.payout_byte_count), 0)::bigint FROM transfer_escrow_sweep s WHERE s.network_id = o.network_id AND s.payment_id = o.payment_id),
				(SELECT COUNT(*) FROM transfer_escrow_sweep s WHERE s.network_id = r.network_id AND s.payment_id = r.payment_id),
				r.payout_nano_cents
			FROM account_payment o, account_payment r
			WHERE o.payment_id = $1 AND r.payment_id = $2
			`,
			replacement.OriginalPaymentId,
			replacement.ReplacementPaymentId,
		).Scan(&remainingSweepCount, &remainingByteCount, &replacementSweepCount, &payout))
		if replacementSweepCount == 0 || (!anchored && remainingSweepCount != 0) {
			returnErr = ErrLegacyProviderPaymentReplacementChanged
			return
		}
		// Ignore only the review bit being released; every actual earning and
		// component-provenance condition is the ordinary sender's same census.
		_, _, attributionErr := inspectProviderUsdcPaymentAttributionWithReview(ctx, tx, replacement.ReplacementPaymentId, earningPolicy, false, true)
		if attributionErr != nil {
			returnErr = attributionErr
			return
		}
		if anchored {
			details, err := json.Marshal(&legacyProviderPaymentReattributionDetails{
				OriginalPaymentId:        replacement.OriginalPaymentId,
				ReplacementPaymentId:     replacement.ReplacementPaymentId,
				RemainingSweepCount:      remainingSweepCount,
				RemainingPayoutByteCount: remainingByteCount,
				ReplacementSweepCount:    replacementSweepCount,
				Reason:                   "anchored replacement; the canceled original keeps these sweeps pending reattribution",
			})
			server.Raise(err)
			for _, paymentId := range []server.Id{replacement.OriginalPaymentId, replacement.ReplacementPaymentId} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details)
					SELECT $1::uuid, $2::uuid, $3::varchar, $4::text
					WHERE NOT EXISTS (SELECT 1 FROM audit_account_payment WHERE payment_id = $2::uuid AND event_type = $3::varchar)
					`,
					server.NewId(),
					paymentId,
					legacyProviderPaymentPendingReattributionEventType,
					string(details),
				))
			}
		}
		if payout <= policy.TransferFee {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details)
				SELECT $1::uuid, $2::uuid, $3::varchar, '{"reason":"replacement does not cover the transfer fee"}'
				WHERE NOT EXISTS (SELECT 1 FROM audit_account_payment WHERE payment_id = $2::uuid AND event_type = $3::varchar)
				`,
				server.NewId(),
				replacement.ReplacementPaymentId,
				legacyProviderPaymentHeldEventType,
			))
			return
		}
		// status only: the review flag is not part of the submission basis
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE account_payment SET attribution_review_required = false WHERE payment_id = $1 AND attribution_review_required`,
			replacement.ReplacementPaymentId,
		))
		released = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// Canceled payments that kept a processor record and never recorded a chain
// hash. Before 2026-08-17 an age-based cancel could cancel a payment whose
// Circle transfer then completed.
func GetCanceledProviderPaymentsWithRecord(ctx context.Context) (payments []*AccountPayment, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		paymentIds := []server.Id{}
		result, err := conn.Query(
			ctx,
			`
			SELECT payment_id FROM account_payment
			WHERE canceled AND NOT completed AND payment_record IS NOT NULL AND tx_hash IS NULL
			ORDER BY payment_id
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				server.Raise(result.Scan(&paymentId))
				paymentIds = append(paymentIds, paymentId)
			}
		})
		for _, paymentId := range paymentIds {
			payment, err := dbGetPayment(ctx, conn, paymentId)
			if err != nil {
				returnErr = err
				return
			}
			if payment != nil {
				payments = append(payments, payment)
			}
		}
	})
	return
}

// CompleteCanceledProviderPayment records the exact observed canceled payment
// as completed once its own Circle record completed with a chain hash. Status
// only, so the submission basis is unchanged; the account balance is credited
// exactly as an ordinary completion credits it.
func CompleteCanceledProviderPayment(ctx context.Context, expected *AccountPayment, paymentReceipt string, txHash string) (returnErr error) {
	if expected == nil || expected.PaymentId == (server.Id{}) || expected.NetworkId == (server.Id{}) || expected.PaymentRecord == nil || expected.TxHash != nil || txHash == "" ||
		len(txHash) > 1024 || providerPaymentReceiptLimit < len(paymentReceipt) {
		return ErrProviderPaymentAttemptChanged
	}
	keys := providerPaymentAttributionKeys(expected.PaymentId, "", "", *expected.PaymentRecord)
	keys = append(keys, accountBalanceOwnershipKeys([]server.Id{expected.NetworkId})...)
	server.OwnedTx(ctx, keys, func(tx server.PgTx) {
		returnErr = nil
		result, err := tx.Query(
			ctx,
			`
			SELECT payment_id FROM account_payment
			WHERE payment_id = $1 AND canceled AND NOT completed AND payment_record = $2 AND tx_hash IS NULL AND
				network_id = $3 AND payout_nano_cents = $4 AND token_amount IS NOT DISTINCT FROM $5::double precision AND
				circle_idempotency_key IS NOT DISTINCT FROM $6::uuid
			FOR UPDATE
			`,
			expected.PaymentId,
			*expected.PaymentRecord,
			expected.NetworkId,
			expected.Payout,
			expected.TokenAmount,
			expected.CircleIdempotencyKey,
		)
		matched := false
		server.WithPgResult(result, err, func() { matched = result.Next() })
		if !matched {
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		if providerPaymentRecordClaimedInTx(ctx, tx, *expected.PaymentRecord, expected) {
			returnErr = ErrProviderPaymentAttemptChanged
			return
		}
		retainProviderAttemptInTx(ctx, tx, expected, "CANCELED_PAID_COMPLETED", paymentReceipt)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE account_payment
			SET
				canceled = false,
				completed = true,
				complete_time = $2,
				payment_receipt = $3,
				tx_hash = $4,
				contract_retention_cursor = NULL,
				contract_retention_pending = NOT attribution_review_required
			WHERE payment_id = $1 AND canceled AND NOT completed
			`,
			expected.PaymentId,
			server.NowUtc(),
			paymentReceipt,
			txHash,
		))
		creditCompletedPaymentAccountBalanceInTx(ctx, tx, expected.PaymentId)
	}, server.TxReadCommitted)
	return
}
