// Reconciliation of legacy keyed provider payments (model/account_payment_legacy_attempt.go).
//
// Circle cannot be asked about an idempotency key: its transaction object does
// not carry one. The evidence is instead the complete Circle transaction history
// of the attempt's destination address, read once per destination group:
//   - the attempt submitted exactly the legacy amount (payout minus the fixed
//     0.01 fee, formatted to six decimals) to the payment's pinned wallet, from
//     the configured developer payout wallet, after the payment was planned
//   - a listed transfer already bound to an attempt (a current record, or one
//     retained by an outcome journal) belongs to that attempt's own key
//   - an inbound mirror or an end-user custody transfer is never a provider
//     payout; every provider payout appears as an outbound developer row
//   - a terminal FAILED, DENIED or CANCELLED transfer without a chain hash moved
//     no funds
//
// Every other listed transfer is live and unbound. A destination group of legacy
// attempts is decided only when each live unbound transfer is attributed to
// exactly one attempt of the group by its exact amount: those attempts bind the
// transfer, and the rest are affirmatively absent, so their keys are released
// for a fresh attempt with a retained request. Anything else holds the whole
// group: an unattributable transfer, an amount shared by two attempts, a
// pending attempt with a retained request to the same destination, a lookup
// failure, or an attempt whose wallet is missing, foreign or too recent.
// Nothing here submits to Circle; a released attempt is submitted only by the
// ordinary AdvancePayment path.
package controller

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The fixed transfer fee every legacy submit subtracted from the payout.
const legacyProviderPaymentFeeUsdc = 0.01

// Transfers created this long before a payment was planned cannot be its
// attempt; the margin absorbs clock differences between hosts and Circle.
const legacyProviderPaymentWindowMargin = 24 * time.Hour

// Attempts planned more recently than this are not reconciled.
const legacyProviderPaymentMinAge = 24 * time.Hour

type LegacyProviderPaymentReconcileSettings struct {
	// report counts only; write nothing
	DryRun bool
	// destination groups looked up in this call; 0 looks up every group
	GroupLimit int
	// pause between Circle lookups, well under Circle's GET limit
	LookupInterval time.Duration
}

func DefaultLegacyProviderPaymentReconcileSettings() *LegacyProviderPaymentReconcileSettings {
	return &LegacyProviderPaymentReconcileSettings{
		DryRun:         true,
		GroupLimit:     0,
		LookupInterval: 500 * time.Millisecond,
	}
}

// Counts of attempts, except the group counts.
type LegacyProviderPaymentReconcileResult struct {
	AttemptCount       int
	GroupCount         int
	LookedUpGroupCount int
	// bound to their Circle transfer
	FoundCount int
	// key released for a fresh attempt
	AbsentCount int
	// held: an unattributable or shared-amount transfer, or a retained attempt in flight
	AmbiguousCount int
	// held: the Circle lookup failed
	LookupErrorCount int
	// held: wallet missing, foreign or unsupported, or the payment is too recent
	IneligibleCount int
	// held: the payment changed between observation and write
	RefusedCount int
	// groups left for a later call by GroupLimit
	DeferredGroupCount int
}

type legacyProviderPaymentDecision struct {
	// payment id -> transfer to bind
	foundTransactions map[server.Id]*CircleListedTransaction
	absentPaymentIds  []server.Id
	heldPaymentIds    []server.Id
	reason            string
	failedRecords     []string
}

// The exact amount string a legacy submit sent for this payout.
func legacyProviderPaymentAmount(payout model.NanoCents) (amount float64, formatted string) {
	amount = model.NanoCentsToUsd(payout) - legacyProviderPaymentFeeUsdc
	return amount, fmt.Sprintf("%f", amount)
}

func circleDecimalEqual(a string, b string) bool {
	x, ok := new(big.Rat).SetString(strings.TrimSpace(a))
	if !ok {
		return false
	}
	y, ok := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok {
		return false
	}
	return x.Cmp(y) == 0
}

func circleTerminalFailure(transaction *CircleListedTransaction) bool {
	switch strings.ToUpper(transaction.State) {
	case "FAILED", "DENIED", "CANCELLED":
		return transaction.TxHash == ""
	default:
		return false
	}
}

// decideLegacyProviderPaymentGroup decides every attempt of one destination
// group from the destination's complete Circle listing. It is pure: the caller
// supplies the bound records and the configured payout wallet.
func decideLegacyProviderPaymentGroup(
	attempts []*model.LegacyProviderPaymentAttempt,
	transactions []*CircleListedTransaction,
	boundRecords map[string]bool,
	payoutWalletId string,
) *legacyProviderPaymentDecision {
	decision := &legacyProviderPaymentDecision{
		foundTransactions: map[server.Id]*CircleListedTransaction{},
	}
	hold := func(reason string) *legacyProviderPaymentDecision {
		held := &legacyProviderPaymentDecision{
			foundTransactions: map[server.Id]*CircleListedTransaction{},
			reason:            reason,
		}
		for _, attempt := range attempts {
			held.heldPaymentIds = append(held.heldPaymentIds, attempt.Payment.PaymentId)
		}
		return held
	}
	if len(attempts) == 0 {
		return decision
	}
	if payoutWalletId == "" {
		return hold("no configured payout wallet")
	}

	windowStart := attempts[0].Payment.CreateTime
	for _, attempt := range attempts {
		windowStart = server.MinTime(windowStart, attempt.Payment.CreateTime)
	}
	windowStart = windowStart.Add(-legacyProviderPaymentWindowMargin)

	live := []*CircleListedTransaction{}
	for _, transaction := range transactions {
		if transaction.CreateDate.Before(windowStart) {
			continue
		}
		if strings.EqualFold(transaction.TransactionType, "INBOUND") || strings.EqualFold(transaction.CustodyType, "ENDUSER") {
			continue
		}
		if boundRecords[transaction.Id] {
			continue
		}
		if circleTerminalFailure(transaction) {
			decision.failedRecords = append(decision.failedRecords, transaction.Id)
			continue
		}
		live = append(live, transaction)
	}

	// attribute each live transfer to the one attempt it can be
	attributed := map[server.Id]*CircleListedTransaction{}
	for _, transaction := range live {
		if transaction.WalletId != payoutWalletId || !strings.EqualFold(transaction.CustodyType, "DEVELOPER") ||
			!strings.EqualFold(transaction.TransactionType, "OUTBOUND") || !strings.EqualFold(transaction.Operation, "TRANSFER") ||
			len(transaction.Amounts) != 1 {
			return hold("a live transfer to the destination is not attributable to a legacy payout")
		}
		var matched *model.LegacyProviderPaymentAttempt
		matches := 0
		for _, attempt := range attempts {
			_, amount := legacyProviderPaymentAmount(attempt.Payment.Payout)
			if circleDecimalEqual(transaction.Amounts[0], amount) &&
				!transaction.CreateDate.Before(attempt.Payment.CreateTime.Add(-legacyProviderPaymentWindowMargin)) {
				matched = attempt
				matches += 1
			}
		}
		if matches != 1 {
			return hold("a live transfer matches no legacy attempt, or several")
		}
		if _, ok := attributed[matched.Payment.PaymentId]; ok {
			return hold("several live transfers match one legacy attempt")
		}
		attributed[matched.Payment.PaymentId] = transaction
	}
	for _, attempt := range attempts {
		if transaction, ok := attributed[attempt.Payment.PaymentId]; ok {
			decision.foundTransactions[attempt.Payment.PaymentId] = transaction
		} else {
			// every live unbound transfer to the destination belongs to another
			// attempt, so this attempt created none
			decision.absentPaymentIds = append(decision.absentPaymentIds, attempt.Payment.PaymentId)
		}
	}
	return decision
}

// The attempt can be decided from its destination's listing.
func legacyProviderPaymentAttemptEligible(attempt *model.LegacyProviderPaymentAttempt, now time.Time) bool {
	payment := attempt.Payment
	if payment.WalletId == nil || attempt.WalletAddress == "" || attempt.WalletNetworkId == nil || *attempt.WalletNetworkId != payment.NetworkId {
		return false
	}
	if _, err := formatBlockchain(attempt.Blockchain); err != nil {
		return false
	}
	if amount, _ := legacyProviderPaymentAmount(payment.Payout); !(0 < amount) {
		return false
	}
	return payment.CreateTime.Before(now.Add(-legacyProviderPaymentMinAge))
}

// ReconcileLegacyProviderPaymentAttempts reconciles every legacy keyed provider
// payment against Circle, destination by destination. A dry run looks up and
// decides the same groups and reports counts without writing.
func ReconcileLegacyProviderPaymentAttempts(
	clientSession *session.ClientSession,
	settings *LegacyProviderPaymentReconcileSettings,
) (*LegacyProviderPaymentReconcileResult, error) {
	return reconcileLegacyProviderPaymentAttempts(
		clientSession,
		settings,
		&CoreCircleApiClient{},
		getWalletIdByNetwork,
		server.NowUtc,
	)
}

func reconcileLegacyProviderPaymentAttempts(
	clientSession *session.ClientSession,
	settings *LegacyProviderPaymentReconcileSettings,
	lister circleTransactionLister,
	payoutWalletId func(blockchain string) (string, error),
	now func() time.Time,
) (*LegacyProviderPaymentReconcileResult, error) {
	ctx := clientSession.Ctx
	result := &LegacyProviderPaymentReconcileResult{}

	attempts, err := model.GetLegacyProviderPaymentAttempts(ctx)
	if err != nil {
		return nil, err
	}
	result.AttemptCount = len(attempts)
	if len(attempts) == 0 {
		return result, nil
	}
	boundRecords, err := model.GetProviderPaymentProcessorRecords(ctx)
	if err != nil {
		return nil, err
	}
	retainedDestinations, err := model.GetRetainedProviderPaymentDestinations(ctx)
	if err != nil {
		return nil, err
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

	observedAt := now()
	lookedUp := 0
	for _, key := range destinations {
		if err := ctx.Err(); err != nil {
			return result, err
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
			// a retained attempt to the destination may have an unbound transfer
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
				return result, ctx.Err()
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
		transactions, err := lister.ListDestinationTransactions(ctx, blockchain, key.walletAddress)
		if err != nil {
			glog.Warningf("[payout]legacy attempt lookup failed for a %s destination group of %d: %s\n", blockchain, len(group), err)
			result.LookupErrorCount += len(group)
			continue
		}
		decision := decideLegacyProviderPaymentGroup(group, transactions, boundRecords, walletId)
		if 0 < len(decision.heldPaymentIds) {
			glog.Infof("[payout]legacy attempt group of %d held: %s\n", len(group), decision.reason)
			result.AmbiguousCount += len(decision.heldPaymentIds)
			continue
		}

		for _, attempt := range group {
			payment := attempt.Payment
			transaction, found := decision.foundTransactions[payment.PaymentId]
			// journaled with the decision; it carries no address or amount
			type legacyAttemptObservation struct {
				Lookup           string    `json:"lookup"`
				ObservedAt       time.Time `json:"observed_at"`
				ListedCount      int       `json:"listed_count"`
				GroupCount       int       `json:"group_count"`
				FailedRecords    []string  `json:"failed_records"`
				Record           string    `json:"record,omitempty"`
				RecordState      string    `json:"record_state,omitempty"`
				RecordCreateDate time.Time `json:"record_create_date,omitzero"`
			}
			observed := legacyAttemptObservation{
				Lookup:        "circle_destination_transactions",
				ObservedAt:    observedAt,
				ListedCount:   len(transactions),
				GroupCount:    len(group),
				FailedRecords: slices.Clone(decision.failedRecords),
			}
			if found {
				observed.Record = transaction.Id
				observed.RecordState = transaction.State
				observed.RecordCreateDate = transaction.CreateDate
			}
			observation, err := json.Marshal(&observed)
			if err != nil {
				return result, err
			}
			if settings.DryRun {
				if found {
					result.FoundCount += 1
				} else {
					result.AbsentCount += 1
				}
				continue
			}
			if found {
				amount, _ := legacyProviderPaymentAmount(payment.Payout)
				err = model.BindLegacyProviderPaymentRecord(ctx, payment, transaction.Id, amount, string(observation))
			} else {
				err = model.ReleaseLegacyProviderPaymentKey(ctx, payment, string(observation))
			}
			if err != nil {
				result.RefusedCount += 1
				continue
			}
			if found {
				boundRecords[transaction.Id] = true
				result.FoundCount += 1
			} else {
				result.AbsentCount += 1
			}
			// bring the payment's AdvancePayment forward
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleAdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, clientSession, tx)
			})
		}
	}
	return result, nil
}
