// Correction of inflated legacy keyed provider payments
// (model/account_payment_legacy_correction.go) and recording of canceled
// payments that Circle completed.
//
// The correction takes the legacy reconciliation's evidence first: only an
// attempt that Circle's destination history affirmatively shows created no
// transfer (the reconciliation's absent decision) is replaced, so a replacement
// can never pay an attempt Circle already paid. Found, held and unchanged
// attempts, and attempts whose plan has no single subsidy epoch, are left to
// the reconciliation exactly as they are.
//
// A dry run reads Circle and the database and writes nothing. An applied run
// replaces each absent, reduced attempt, gives the replacement its earning
// authority, releases the replacement's review hold and schedules the ordinary
// AdvancePayment for it. A zero-revenue history may use one anchor sweep and
// journal the canceled original's remainder for later reattribution. Any
// nonzero sweep revenue instead requires full movement in bounded transactions.
// Every step is resumable: a later run continues from the journaled pairs.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type LegacyProviderPaymentCorrectionSettings struct {
	// report counts and totals only; write nothing
	DryRun bool
	// destination groups looked up in this call; 0 looks up every group
	GroupLimit int
	// pause between Circle lookups, well under Circle's GET limit
	LookupInterval time.Duration
	// allow one anchor for zero-revenue histories; otherwise move every sweep
	AnchorSweeps bool
	// full move: sweeps moved per transaction
	SweepBatchSize int
	// full move: sweep transactions in this call; 0 moves every sweep
	SweepBatchLimit int
	// a dry run also counts the sweeps the replacements would take
	CountSweeps bool
	// nil uses the configured subsidy policy
	Policy *model.LegacyProviderPaymentCorrectionPolicy
}

func DefaultLegacyProviderPaymentCorrectionSettings() *LegacyProviderPaymentCorrectionSettings {
	return &LegacyProviderPaymentCorrectionSettings{
		DryRun:          true,
		GroupLimit:      0,
		LookupInterval:  500 * time.Millisecond,
		AnchorSweeps:    true,
		SweepBatchSize:  10000,
		SweepBatchLimit: 0,
		CountSweeps:     false,
	}
}

// Counts of attempts unless named otherwise; amounts in nano cents.
type LegacyProviderPaymentCorrectionResult struct {
	AttemptCount       int
	GroupCount         int
	LookedUpGroupCount int
	DeferredGroupCount int
	// left to the reconciliation: Circle lists a transfer that is the attempt
	FoundCount int
	// left as is: an ambiguous group, a failed lookup, or an ineligible or retained group
	AmbiguousCount   int
	LookupErrorCount int
	IneligibleCount  int
	// Circle shows the attempt created no transfer
	AbsentCount int
	// absent, but its plan has no single subsidy epoch: left as is
	MissingWindowCount int
	// absent and already at the corrected amounts: left to the reconciliation
	UnchangedCount  int
	UnchangedPayout model.NanoCents
	// absent and reduced: replaced
	CorrectionCount      int
	OriginalPayout       model.NanoCents
	CorrectedPayout      model.NanoCents
	SubsidyReduction     model.NanoCents
	ReliabilityReduction model.NanoCents
	// corrections at or below the transfer fee, held after their move
	DustCount  int
	DustPayout model.NanoCents
	// sweeps the corrected attempts own (CountSweeps)
	SweepCount int64
	// anchored: corrections whose original owns a sweep the earning admission
	// accepts (a dry run), or anchors moved (applied)
	AnchorCount int
	// anchored: corrections without such a sweep, held
	AnchorMissingCount int
	// applied
	ReplacedCount   int
	RefusedCount    int
	MovedSweepCount int64
	// replacements whose original still owns sweeps when this call ends
	MovingCount    int
	ReleasedCount  int
	DustHeldCount  int
	ScheduledCount int
}

func CorrectLegacyProviderPaymentAttempts(
	clientSession *session.ClientSession,
	settings *LegacyProviderPaymentCorrectionSettings,
) (*LegacyProviderPaymentCorrectionResult, error) {
	return correctLegacyProviderPaymentAttempts(
		clientSession,
		settings,
		&CoreCircleApiClient{},
		getWalletIdByNetwork,
		server.NowUtc,
	)
}

func correctLegacyProviderPaymentAttempts(
	clientSession *session.ClientSession,
	settings *LegacyProviderPaymentCorrectionSettings,
	lister circleTransactionLister,
	payoutWalletId func(blockchain string) (string, error),
	now func() time.Time,
) (*LegacyProviderPaymentCorrectionResult, error) {
	ctx := clientSession.Ctx
	result := &LegacyProviderPaymentCorrectionResult{}
	policy := settings.Policy
	if policy == nil {
		policy = model.DefaultLegacyProviderPaymentCorrectionPolicy()
	}

	attempts, err := model.GetLegacyProviderPaymentAttempts(ctx)
	if err != nil {
		return nil, err
	}
	result.AttemptCount = len(attempts)
	if 0 < len(attempts) {
		if err := correctLegacyProviderPaymentGroups(clientSession, settings, policy, attempts, lister, payoutWalletId, now, result); err != nil {
			return result, err
		}
	}
	if settings.DryRun {
		return result, nil
	}
	err = advanceLegacyProviderPaymentReplacements(clientSession, settings, policy, result)
	return result, err
}

// Decides every destination group as the reconciliation does, then corrects
// the absent attempts of each decided group.
func correctLegacyProviderPaymentGroups(
	clientSession *session.ClientSession,
	settings *LegacyProviderPaymentCorrectionSettings,
	policy *model.LegacyProviderPaymentCorrectionPolicy,
	attempts []*model.LegacyProviderPaymentAttempt,
	lister circleTransactionLister,
	payoutWalletId func(blockchain string) (string, error),
	now func() time.Time,
	result *LegacyProviderPaymentCorrectionResult,
) error {
	ctx := clientSession.Ctx
	boundRecords, err := model.GetProviderPaymentProcessorRecords(ctx)
	if err != nil {
		return err
	}
	retainedDestinations, err := model.GetRetainedProviderPaymentDestinations(ctx)
	if err != nil {
		return err
	}
	paymentIds := make([]server.Id, 0, len(attempts))
	attemptsById := map[server.Id]*model.LegacyProviderPaymentAttempt{}
	for _, attempt := range attempts {
		paymentIds = append(paymentIds, attempt.Payment.PaymentId)
		attemptsById[attempt.Payment.PaymentId] = attempt
	}
	windows, err := model.GetLegacyProviderPaymentSubsidyWindows(ctx, paymentIds)
	if err != nil {
		return err
	}

	type destination struct {
		blockchain    string
		walletAddress string
	}
	groups := map[destination][]*model.LegacyProviderPaymentAttempt{}
	destinations := []destination{}
	for _, attempt := range attempts {
		key := destination{blockchain: attempt.Blockchain, walletAddress: attempt.WalletAddress}
		if _, ok := groups[key]; !ok {
			destinations = append(destinations, key)
		}
		groups[key] = append(groups[key], attempt)
	}
	result.GroupCount = len(destinations)

	correctedPaymentIds := []server.Id{}
	observedAt := now()
	lookedUp := 0
	for _, key := range destinations {
		if err := ctx.Err(); err != nil {
			return err
		}
		group := groups[key]
		eligible := true
		for _, attempt := range group {
			if !legacyProviderPaymentAttemptEligible(attempt, observedAt) {
				eligible = false
				break
			}
		}
		if !eligible {
			result.IneligibleCount += len(group)
			continue
		}
		blockchain, _ := formatBlockchain(key.blockchain)
		if retainedDestinations[[2]string{key.blockchain, key.walletAddress}] {
			result.AmbiguousCount += len(group)
			continue
		}
		if 0 < settings.GroupLimit && settings.GroupLimit <= lookedUp {
			result.DeferredGroupCount += 1
			continue
		}
		if 0 < lookedUp && 0 < settings.LookupInterval {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settings.LookupInterval):
			}
		}
		lookedUp += 1
		result.LookedUpGroupCount += 1

		walletId, err := payoutWalletId(blockchain)
		if err != nil {
			result.LookupErrorCount += len(group)
			continue
		}
		transactions, err := listLegacyProviderPaymentTransactions(ctx, lister, blockchain, key.walletAddress, group, boundRecords)
		if err != nil {
			glog.Warningf("[payout]legacy correction lookup failed for a %s destination group of %d: %s\n", blockchain, len(group), err)
			result.LookupErrorCount += len(group)
			continue
		}
		decision := decideLegacyProviderPaymentGroup(group, transactions, boundRecords, walletId)
		if 0 < len(decision.heldPaymentIds) {
			result.AmbiguousCount += len(decision.heldPaymentIds)
			continue
		}
		result.FoundCount += len(decision.foundTransactions)

		// journaled with each replacement; it carries no address or amount
		observation, err := json.Marshal(&struct {
			Lookup        string    `json:"lookup"`
			ObservedAt    time.Time `json:"observed_at"`
			ListedCount   int       `json:"listed_count"`
			GroupCount    int       `json:"group_count"`
			FailedRecords []string  `json:"failed_records"`
		}{
			Lookup:        "circle_destination_transactions",
			ObservedAt:    observedAt,
			ListedCount:   len(transactions),
			GroupCount:    len(group),
			FailedRecords: decision.failedRecords,
		})
		if err != nil {
			return err
		}
		for _, paymentId := range decision.absentPaymentIds {
			result.AbsentCount += 1
			payment := attemptsById[paymentId].Payment
			window, ok := windows[paymentId]
			if !ok {
				result.MissingWindowCount += 1
				continue
			}
			correction := model.CorrectLegacyProviderPayment(payment, window, policy)
			if !correction.Changed() {
				result.UnchangedCount += 1
				result.UnchangedPayout += payment.Payout
				continue
			}
			result.CorrectionCount += 1
			result.OriginalPayout += payment.Payout
			result.CorrectedPayout += correction.Payout
			result.SubsidyReduction += payment.SubsidyPayout - correction.SubsidyPayout
			result.ReliabilityReduction += payment.ReliabilitySubsidy - correction.ReliabilitySubsidy
			if correction.Dust {
				result.DustCount += 1
				result.DustPayout += correction.Payout
			}
			correctedPaymentIds = append(correctedPaymentIds, paymentId)
			if settings.DryRun {
				continue
			}
			if _, err := model.ReplaceLegacyProviderPayment(ctx, correction, policy, string(observation)); err != nil {
				result.RefusedCount += 1
				continue
			}
			result.ReplacedCount += 1
		}
	}
	if settings.DryRun && settings.CountSweeps && 0 < len(correctedPaymentIds) {
		sweepCount, err := model.CountProviderPaymentSweeps(ctx, correctedPaymentIds)
		if err != nil {
			return err
		}
		result.SweepCount = sweepCount
	}
	if settings.DryRun && settings.AnchorSweeps && 0 < len(correctedPaymentIds) {
		cutoff, err := legacyProviderPaymentEarningCutoff(ctx)
		if err != nil {
			return err
		}
		anchorCount, err := model.CountLegacyProviderPaymentAnchors(ctx, correctedPaymentIds, cutoff)
		if err != nil {
			return err
		}
		result.AnchorCount = anchorCount
		result.AnchorMissingCount = len(correctedPaymentIds) - anchorCount
	}
	return nil
}

// The earning admission's cutoff; nil without an earning policy.
func legacyProviderPaymentEarningCutoff(ctx context.Context) (*time.Time, error) {
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil || policy == nil {
		return nil, err
	}
	cutoff := policy.Cutoff
	return &cutoff, nil
}

// Moves, releases and schedules every journaled replacement, including those
// of earlier runs. The sweep batch limit bounds one call; a later call resumes.
func advanceLegacyProviderPaymentReplacements(
	clientSession *session.ClientSession,
	settings *LegacyProviderPaymentCorrectionSettings,
	policy *model.LegacyProviderPaymentCorrectionPolicy,
	result *LegacyProviderPaymentCorrectionResult,
) error {
	ctx := clientSession.Ctx
	replacements, err := model.GetLegacyProviderPaymentReplacements(ctx)
	if err != nil {
		return err
	}
	var cutoff *time.Time
	if settings.AnchorSweeps {
		if cutoff, err = legacyProviderPaymentEarningCutoff(ctx); err != nil {
			return err
		}
	}
	batches := 0
	for _, replacement := range replacements {
		if err := ctx.Err(); err != nil {
			return err
		}
		if replacement.Completed || replacement.Canceled {
			continue
		}
		if !replacement.ReviewHeld && replacement.OriginalOwnsRevenue {
			held, err := model.ReholdLegacyProviderPaymentReplacement(ctx, replacement)
			if err != nil || !held {
				result.RefusedCount += 1
				continue
			}
			replacement.ReviewHeld = true
		}
		// An anchor can leave zero-revenue history behind, never revenue that
		// the replacement amount still promises. Reuse bounded full movement.
		if replacement.ReviewHeld && settings.AnchorSweeps && !replacement.OriginalOwnsRevenue {
			if !replacement.ReplacementOwnsSweeps {
				anchored, err := model.MoveLegacyProviderPaymentReplacementAnchor(ctx, replacement, cutoff)
				if err != nil {
					if errors.Is(err, model.ErrLegacyProviderPaymentAnchorUnavailable) {
						result.AnchorMissingCount += 1
					} else {
						result.RefusedCount += 1
					}
					continue
				}
				if anchored {
					result.AnchorCount += 1
				}
			}
			released, err := model.ReleaseLegacyProviderPaymentReplacement(ctx, replacement, policy, true)
			if err != nil {
				result.RefusedCount += 1
				continue
			}
			if !released {
				result.DustHeldCount += 1
				continue
			}
			result.ReleasedCount += 1
		} else if replacement.ReviewHeld {
			for replacement.OriginalOwnsSweeps {
				if 0 < settings.SweepBatchLimit && settings.SweepBatchLimit <= batches {
					break
				}
				movedCount, err := model.MoveLegacyProviderPaymentReplacementSweeps(ctx, replacement, settings.SweepBatchSize)
				batches += 1
				if err != nil {
					result.RefusedCount += 1
					break
				}
				result.MovedSweepCount += movedCount
				if movedCount < int64(settings.SweepBatchSize) {
					// a canceled original gains no sweep; the release rechecks
					replacement.OriginalOwnsSweeps = false
				}
			}
			if replacement.OriginalOwnsSweeps {
				result.MovingCount += 1
				continue
			}
			released, err := model.ReleaseLegacyProviderPaymentReplacement(ctx, replacement, policy, false)
			if err != nil {
				result.RefusedCount += 1
				continue
			}
			if !released {
				result.DustHeldCount += 1
				continue
			}
			result.ReleasedCount += 1
		}
		// the ordinary fresh attempt; scheduling is run-once per payment
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleAdvancePayment(&AdvancePaymentArgs{PaymentId: replacement.ReplacementPaymentId}, clientSession, tx)
		})
		result.ScheduledCount += 1
	}
	return nil
}

type PaidCanceledProviderPaymentSettings struct {
	// report counts only; write nothing
	DryRun bool
	// pause between Circle lookups
	LookupInterval time.Duration
}

// Counts of payments; the amount in nano cents.
type PaidCanceledProviderPaymentResult struct {
	// canceled, with a processor record and no chain hash
	CandidateCount int
	// Circle shows the record COMPLETE with a chain hash and the exact submitted amount
	CompleteCount  int
	CompletePayout model.NanoCents
	// the record is not COMPLETE, or has no chain hash
	NotCompleteCount int
	// the record is another transfer, or its amount differs from the submitted amount
	MismatchCount    int
	LookupErrorCount int
	// applied
	RecordedCount int
	RefusedCount  int
}

func CompletePaidCanceledProviderPayments(
	clientSession *session.ClientSession,
	settings *PaidCanceledProviderPaymentSettings,
) (*PaidCanceledProviderPaymentResult, error) {
	return completePaidCanceledProviderPayments(clientSession, settings, NewCircleClient())
}

// Each candidate is decided from its own Circle record only. The submitted
// amount is the payout less the legacy fee, formatted as the legacy submit sent it.
func completePaidCanceledProviderPayments(
	clientSession *session.ClientSession,
	settings *PaidCanceledProviderPaymentSettings,
	circleClient CircleApi,
) (*PaidCanceledProviderPaymentResult, error) {
	ctx := clientSession.Ctx
	result := &PaidCanceledProviderPaymentResult{}
	payments, err := model.GetCanceledProviderPaymentsWithRecord(ctx)
	if err != nil {
		return nil, err
	}
	for index, payment := range payments {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.CandidateCount += 1
		if 0 < index && 0 < settings.LookupInterval {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(settings.LookupInterval):
			}
		}
		record := *payment.PaymentRecord
		transferResult, err := circleClient.GetTransaction(ctx, record)
		if err != nil || transferResult == nil {
			result.LookupErrorCount += 1
			continue
		}
		transfer := transferResult.Transaction
		if transfer.Id != record {
			result.MismatchCount += 1
			continue
		}
		if !strings.EqualFold(transfer.State, "COMPLETE") || transfer.TxHash == "" {
			result.NotCompleteCount += 1
			continue
		}
		if payment.TokenAmount == nil || len(transfer.Amounts) != 1 ||
			!circleDecimalEqual(transfer.Amounts[0], fmt.Sprintf("%f", *payment.TokenAmount)) {
			result.MismatchCount += 1
			continue
		}
		result.CompleteCount += 1
		result.CompletePayout += payment.Payout
		if settings.DryRun {
			continue
		}
		if err := model.CompleteCanceledProviderPayment(ctx, payment, string(transferResult.ResponseBodyBytes), transfer.TxHash); err != nil {
			result.RefusedCount += 1
			continue
		}
		result.RecordedCount += 1
	}
	return result, nil
}
