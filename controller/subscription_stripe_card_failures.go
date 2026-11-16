package controller

// Failed card checks at Stripe, watched for card testing (owner, 2026-11-13).
//
// Card testers run lists of stolen cards through a merchant's checkout to find
// the ones that still work. On our account in 2026-11: 20 cards in 92 seconds
// through two Checkout sessions made 2.5 minutes after their account was, and
// three sessions that tried 6 cards over 22 attempts. The yearly plan's free
// trial checks a card for nothing (a $0 SetupIntent), the kind of check card
// testers prefer. A failed check reaches the webhook as
// setup_intent.setup_failed (a card saved for later: the trial's check, the
// payment sheet's) or charge.failed (a payment), which used to be answered 200
// and otherwise ignored, so nothing showed the attack.
// Now each failed card check:
//
//   - is counted as urnetwork_subscription_stripe_failures_total{kind,reason_class}:
//     the kind of check (setup, charge) and a closed class of Stripe's decline
//     reason (stripeCardFailureReasonClass). No id, address or card detail is a
//     label. A failed bank debit or transfer is not a card check and is not
//     counted.
//   - counts toward its owner's failures in a sliding window in Redis: the
//     network the check is for when the object names it (network_id metadata,
//     or a customer the server made for a network), else its Stripe customer,
//     else its intent, which every attempt in one Checkout session shares. The
//     Stripe event id keys each failure, so a redelivered event counts once.
//   - when its owner's count passes BurstCount within BurstWindow, makes a
//     burst: one fixed line, "[stripe]card failure burst: ...", which the
//     monitor's log tail alerts on as payment-card-failure-burst
//     (monitor/tailer.go, SIGNALS.md §2.22), counted in
//     urnetwork_subscription_stripe_failure_bursts_total{owner}. A burst that
//     goes on is logged again at each further BurstCount failures, so a long
//     attack stays in view.
//
// The bookkeeping never fails the webhook: a count that cannot be kept is
// logged and the event is answered 200 as before, so Stripe never retries or
// disables the endpoint for it. Safe for concurrent use.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The kinds of failed card check (the kind label).
const (
	// setup_intent.setup_failed: a card saved for later payments
	stripeCardFailureKindSetup = "setup"
	// charge.failed: a payment
	stripeCardFailureKindCharge = "charge"
)

// The classes of decline reason (the reason_class label), a closed set.
const (
	stripeCardFailureReasonDeclined          = "declined"
	stripeCardFailureReasonInsufficientFunds = "insufficient_funds"
	stripeCardFailureReasonInvalidCard       = "invalid_card"
	stripeCardFailureReasonLostStolen        = "lost_stolen"
	stripeCardFailureReasonFraud             = "fraud"
	stripeCardFailureReasonBlocked           = "blocked"
	stripeCardFailureReasonAuthentication    = "authentication"
	stripeCardFailureReasonProcessing        = "processing"
	stripeCardFailureReasonOther             = "other"
)

// What a failed card check counts for (the owner label of a burst).
const (
	stripeCardFailureOwnerNetwork  = "network"
	stripeCardFailureOwnerCustomer = "customer"
	stripeCardFailureOwnerIntent   = "intent"
)

// Stripe's decline and error codes by the class each counts in. Stripe's own
// codes, a fact of its API: an unknown code counts as other.
var stripeCardFailureReasonClasses = map[string]string{
	"insufficient_funds":                    stripeCardFailureReasonInsufficientFunds,
	"withdrawal_count_limit_exceeded":       stripeCardFailureReasonInsufficientFunds,
	"incorrect_number":                      stripeCardFailureReasonInvalidCard,
	"invalid_number":                        stripeCardFailureReasonInvalidCard,
	"incorrect_cvc":                         stripeCardFailureReasonInvalidCard,
	"invalid_cvc":                           stripeCardFailureReasonInvalidCard,
	"incorrect_zip":                         stripeCardFailureReasonInvalidCard,
	"invalid_expiry_month":                  stripeCardFailureReasonInvalidCard,
	"invalid_expiry_year":                   stripeCardFailureReasonInvalidCard,
	"expired_card":                          stripeCardFailureReasonInvalidCard,
	"invalid_account":                       stripeCardFailureReasonInvalidCard,
	"lost_card":                             stripeCardFailureReasonLostStolen,
	"stolen_card":                           stripeCardFailureReasonLostStolen,
	"pickup_card":                           stripeCardFailureReasonLostStolen,
	"restricted_card":                       stripeCardFailureReasonLostStolen,
	"fraudulent":                            stripeCardFailureReasonFraud,
	"merchant_blacklist":                    stripeCardFailureReasonFraud,
	"security_violation":                    stripeCardFailureReasonFraud,
	"highest_risk_level":                    stripeCardFailureReasonBlocked,
	"elevated_risk_level":                   stripeCardFailureReasonBlocked,
	"rule":                                  stripeCardFailureReasonBlocked,
	"low_probability_of_authorization":      stripeCardFailureReasonBlocked,
	"authentication_required":               stripeCardFailureReasonAuthentication,
	"authentication_not_handled":            stripeCardFailureReasonAuthentication,
	"setup_intent_authentication_failure":   stripeCardFailureReasonAuthentication,
	"payment_intent_authentication_failure": stripeCardFailureReasonAuthentication,
	"approve_with_id":                       stripeCardFailureReasonAuthentication,
	"processing_error":                      stripeCardFailureReasonProcessing,
	"issuer_not_available":                  stripeCardFailureReasonProcessing,
	"try_again_later":                       stripeCardFailureReasonProcessing,
	"reenter_transaction":                   stripeCardFailureReasonProcessing,
	"card_declined":                         stripeCardFailureReasonDeclined,
	"generic_decline":                       stripeCardFailureReasonDeclined,
	"do_not_honor":                          stripeCardFailureReasonDeclined,
	"do_not_try_again":                      stripeCardFailureReasonDeclined,
	"call_issuer":                           stripeCardFailureReasonDeclined,
	"card_not_supported":                    stripeCardFailureReasonDeclined,
	"card_velocity_exceeded":                stripeCardFailureReasonDeclined,
	"currency_not_supported":                stripeCardFailureReasonDeclined,
	"duplicate_transaction":                 stripeCardFailureReasonDeclined,
	"invalid_amount":                        stripeCardFailureReasonDeclined,
	"new_account_information_available":     stripeCardFailureReasonDeclined,
	"no_action_taken":                       stripeCardFailureReasonDeclined,
	"not_permitted":                         stripeCardFailureReasonDeclined,
	"revocation_of_all_authorizations":      stripeCardFailureReasonDeclined,
	"revocation_of_authorization":           stripeCardFailureReasonDeclined,
	"service_not_allowed":                   stripeCardFailureReasonDeclined,
	"stop_payment_order":                    stripeCardFailureReasonDeclined,
	"transaction_not_allowed":               stripeCardFailureReasonDeclined,
	"testmode_decline":                      stripeCardFailureReasonDeclined,
}

// The kinds and classes, in label order, for the series made up front.
var (
	stripeCardFailureKinds   = []string{stripeCardFailureKindSetup, stripeCardFailureKindCharge}
	stripeCardFailureReasons = []string{
		stripeCardFailureReasonDeclined,
		stripeCardFailureReasonInsufficientFunds,
		stripeCardFailureReasonInvalidCard,
		stripeCardFailureReasonLostStolen,
		stripeCardFailureReasonFraud,
		stripeCardFailureReasonBlocked,
		stripeCardFailureReasonAuthentication,
		stripeCardFailureReasonProcessing,
		stripeCardFailureReasonOther,
	}
	stripeCardFailureOwners = []string{
		stripeCardFailureOwnerNetwork,
		stripeCardFailureOwnerCustomer,
		stripeCardFailureOwnerIntent,
	}
)

// Failed card checks by kind and reason class.
var stripeCardFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "subscription",
	Name:      "stripe_failures_total",
	Help:      "Failed Stripe card checks by kind (setup: setup_intent.setup_failed, a card saved for later such as the trial's check; charge: charge.failed) and reason_class (declined, insufficient_funds, invalid_card, lost_stolen, fraud, blocked, authentication, processing, other)",
}, []string{"kind", "reason_class"})

// Card failure bursts, by what the failures counted for.
var stripeCardFailureBurstsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "subscription",
	Name:      "stripe_failure_bursts_total",
	Help:      "Bursts of failed Stripe card checks for one owner within the burst window, by owner (network, customer, or intent: one checkout's attempts); each logs the line the monitor's payment-card-failure-burst alert reads",
}, []string{"owner"})

// Registers the metrics, every series from the start so the first failure is
// an increase.
func init() {
	prometheus.MustRegister(stripeCardFailuresTotal, stripeCardFailureBurstsTotal)
	for _, kind := range stripeCardFailureKinds {
		for _, reason := range stripeCardFailureReasons {
			stripeCardFailuresTotal.WithLabelValues(kind, reason)
		}
	}
	for _, owner := range stripeCardFailureOwners {
		stripeCardFailureBurstsTotal.WithLabelValues(owner)
	}
}

// The card-failure watch's tunables.
type stripeCardFailureSettings struct {
	// A burst is more than this many failed card checks of one owner within
	// BurstWindow. 5: a customer whose card is declined tries a few before
	// giving up, while the 2026-11 card testing ran 20 cards through two
	// Checkout sessions. Non-positive logs no burst; the failures are still
	// counted.
	BurstCount int
	// The trailing window the burst is counted over. 10 minutes: the 2026-11
	// attacks ran 20 cards in 92 seconds, and 22 attempts over three sessions.
	// Non-positive logs no burst.
	BurstWindow time.Duration
	// Bounds counting one failure in Redis, so a slow Redis never holds the
	// webhook.
	CountTimeout time.Duration
}

// A new instance on every call.
func defaultStripeCardFailureSettings() *stripeCardFailureSettings {
	return &stripeCardFailureSettings{
		BurstCount:   5,
		BurstWindow:  10 * time.Minute,
		CountTimeout: 2 * time.Second,
	}
}

// One failed card check, as the webhook reads it from the event.
type stripeCardFailure struct {
	// the Stripe event id, which keys the failure in its owner's window
	EventId string
	// when it failed: the event's time, else when the webhook received it
	Time        time.Time
	Kind        string
	ReasonClass string
	OwnerKind   string
	OwnerId     string
}

// The owner's key in Redis.
func (self *stripeCardFailure) ownerKey() string {
	return fmt.Sprintf("stripe-card-failures:%s:%s", self.OwnerKind, self.OwnerId)
}

// The fields of a charge.failed charge the watch reads.
type stripeCardFailureChargeObject struct {
	Id          string `json:"id"`
	FailureCode string `json:"failure_code"`
	Outcome     *struct {
		// issuer_declined, blocked, invalid, ...
		Type string `json:"type"`
		// the decline code for an issuer decline, else Radar's reason
		Reason string `json:"reason"`
	} `json:"outcome"`
	Customer             json.RawMessage   `json:"customer"`
	PaymentIntent        json.RawMessage   `json:"payment_intent"`
	Metadata             map[string]string `json:"metadata"`
	PaymentMethodDetails *struct {
		Type string `json:"type"`
	} `json:"payment_method_details"`
}

// The fields of a setup_intent.setup_failed SetupIntent the watch reads.
type stripeCardFailureSetupIntentObject struct {
	Id             string            `json:"id"`
	Customer       json.RawMessage   `json:"customer"`
	Metadata       map[string]string `json:"metadata"`
	LastSetupError *struct {
		Code          string `json:"code"`
		DeclineCode   string `json:"decline_code"`
		PaymentMethod *struct {
			Type string `json:"type"`
		} `json:"payment_method"`
	} `json:"last_setup_error"`
}

// The class of a failed card check's decline reason: a Radar or Stripe block
// whatever its code, else the class of Stripe's decline code when it is one
// the table knows, else of its error code, else other.
func stripeCardFailureReasonClass(code string, declineCode string, outcomeType string) string {
	if outcomeType == "blocked" {
		return stripeCardFailureReasonBlocked
	}
	for _, candidate := range []string{declineCode, code} {
		if reasonClass, ok := stripeCardFailureReasonClasses[candidate]; ok {
			return reasonClass
		}
	}
	return stripeCardFailureReasonOther
}

// Whether a failure that brought its owner's window to count is a burst to
// log: the first failure past burstCount, then each burstCount further.
func stripeCardFailureBurstDue(count int64, burstCount int) bool {
	if burstCount <= 0 || count <= int64(burstCount) {
		return false
	}
	return (count-int64(burstCount)-1)%int64(burstCount) == 0
}

// The fixed line of a burst, which the monitor's payment-card-failure-burst
// class reads: the count, the window, the owner and the latest failure's kind
// and class. The owner id is for whoever acts on it; the monitor leaves it out
// of the alert.
func stripeCardFailureBurstLine(failure *stripeCardFailure, count int64, window time.Duration) string {
	return fmt.Sprintf(
		"[stripe]card failure burst: %d failed card checks within %d minutes for %s %s (latest %s %s)",
		count,
		int64(window/time.Minute),
		failure.OwnerKind,
		failure.OwnerId,
		failure.Kind,
		failure.ReasonClass,
	)
}

// Watches the failed card checks of one process. Safe for concurrent use.
type stripeCardFailureRecorder struct {
	settings *stripeCardFailureSettings
	// the network the server made the Stripe customer for, nil for none
	customerNetworkId func(ctx context.Context, customerId string) *server.Id
	// adds the failure to its owner's window and returns how many of the
	// owner's failures the window holds, and whether this one is new there
	addToWindow func(ctx context.Context, failure *stripeCardFailure, now time.Time, window time.Duration) (count int64, added bool, err error)
	// writes a burst's line
	reportBurst func(line string)
	now         func() time.Time
}

// The process's recorder. Replaceable only by tests.
var stripeCardFailures = &stripeCardFailureRecorder{
	settings:          defaultStripeCardFailureSettings(),
	customerNetworkId: model.GetStripeCustomerNetworkId,
	addToWindow:       stripeCardFailureRedisAddToWindow,
	reportBurst: func(line string) {
		glog.Warningf("%s\n", line)
	},
	now: server.NowUtc,
}

// Counts one charge.failed or setup_intent.setup_failed event: its kind and
// reason class, then its owner's window, and logs a burst when one is due.
// Never fails: what cannot be read or counted is logged.
func (self *stripeCardFailureRecorder) record(ctx context.Context, event *StripeWebhookArgs) {
	server.HandleError(func() {
		failure := self.readFailure(ctx, event)
		if failure == nil {
			return
		}
		countFailure := func() {
			stripeCardFailuresTotal.WithLabelValues(failure.Kind, failure.ReasonClass).Inc()
		}
		if failure.OwnerKind == "" || self.settings.BurstCount <= 0 || self.settings.BurstWindow <= 0 {
			// nothing to count a burst for, or bursts are off
			countFailure()
			return
		}
		count, added, err := func() (int64, bool, error) {
			countCtx, cancel := context.WithTimeout(ctx, self.settings.CountTimeout)
			defer cancel()
			return self.addToWindow(countCtx, failure, self.now(), self.settings.BurstWindow)
		}()
		if err != nil {
			countFailure()
			glog.Warningf("[stripe]card failure %s (%s %s) was counted but not added to its owner's window: %s\n", event.Id, failure.Kind, failure.ReasonClass, err)
			return
		}
		if !added {
			// a redelivered event: counted when it first came
			return
		}
		countFailure()
		if !stripeCardFailureBurstDue(count, self.settings.BurstCount) {
			return
		}
		stripeCardFailureBurstsTotal.WithLabelValues(failure.OwnerKind).Inc()
		self.reportBurst(stripeCardFailureBurstLine(failure, count, self.settings.BurstWindow))
	}, func(err error) {
		glog.Warningf("[stripe]card failure %s could not be watched: %s\n", event.Id, err)
	})
}

// The failed card check the event reports, nil for an event that is not one
// (a failed bank debit, a body that does not parse).
func (self *stripeCardFailureRecorder) readFailure(ctx context.Context, event *StripeWebhookArgs) *stripeCardFailure {
	if event == nil || event.Data == nil || event.Id == "" {
		return nil
	}
	failure := &stripeCardFailure{
		EventId: event.Id,
		Time:    self.now(),
	}
	if 0 < event.Created {
		failure.Time = time.Unix(event.Created, 0).UTC()
	}
	var customerId string
	var metadata map[string]string
	var intentId string
	switch event.Type {
	case "charge.failed":
		var charge stripeCardFailureChargeObject
		if err := json.Unmarshal(event.Data.Object, &charge); err != nil {
			glog.Warningf("[stripe]card failure %s: the charge does not parse: %s\n", event.Id, err)
			return nil
		}
		if charge.PaymentMethodDetails != nil && charge.PaymentMethodDetails.Type != "" && charge.PaymentMethodDetails.Type != "card" {
			// a bank debit or transfer is not a card check
			return nil
		}
		failure.Kind = stripeCardFailureKindCharge
		outcomeType, outcomeReason := "", ""
		if charge.Outcome != nil {
			outcomeType, outcomeReason = charge.Outcome.Type, charge.Outcome.Reason
		}
		failure.ReasonClass = stripeCardFailureReasonClass(charge.FailureCode, outcomeReason, outcomeType)
		customerId = stripeIdFromRaw(charge.Customer)
		metadata = charge.Metadata
		intentId = stripeIdFromRaw(charge.PaymentIntent)
		if intentId == "" {
			intentId = charge.Id
		}
	case "setup_intent.setup_failed":
		var setupIntent stripeCardFailureSetupIntentObject
		if err := json.Unmarshal(event.Data.Object, &setupIntent); err != nil {
			glog.Warningf("[stripe]card failure %s: the setup intent does not parse: %s\n", event.Id, err)
			return nil
		}
		code, declineCode := "", ""
		if setupError := setupIntent.LastSetupError; setupError != nil {
			if setupError.PaymentMethod != nil && setupError.PaymentMethod.Type != "" && setupError.PaymentMethod.Type != "card" {
				return nil
			}
			code, declineCode = setupError.Code, setupError.DeclineCode
		}
		failure.Kind = stripeCardFailureKindSetup
		failure.ReasonClass = stripeCardFailureReasonClass(code, declineCode, "")
		customerId = stripeIdFromRaw(setupIntent.Customer)
		metadata = setupIntent.Metadata
		intentId = setupIntent.Id
	default:
		return nil
	}

	// the owner: the network when the object names it, else the customer,
	// else the intent every attempt of one checkout shares
	if networkId, err := server.ParseId(metadata[stripeMetadataNetworkId]); err == nil {
		failure.OwnerKind, failure.OwnerId = stripeCardFailureOwnerNetwork, networkId.String()
	} else if customerId != "" {
		networkId := func() (networkId *server.Id) {
			server.HandleError(func() {
				networkId = self.customerNetworkId(ctx, customerId)
			}, func(err error) {
				glog.Warningf("[stripe]card failure %s: the network of customer %s could not be read; counting the customer: %s\n", event.Id, customerId, err)
			})
			return
		}()
		if networkId != nil {
			failure.OwnerKind, failure.OwnerId = stripeCardFailureOwnerNetwork, networkId.String()
		} else {
			failure.OwnerKind, failure.OwnerId = stripeCardFailureOwnerCustomer, customerId
		}
	} else if intentId != "" {
		failure.OwnerKind, failure.OwnerId = stripeCardFailureOwnerIntent, intentId
	}
	// else no owner: counted, but nothing to count a burst for
	return failure
}

// Adds the failure to its owner's window in Redis, a sorted set of the
// owner's failures by time, keyed by event id: the failures older than the
// window leave, the set expires a window after its last failure, and a
// redelivered event is already a member. One key, so one slot; the commands
// are idempotent, so the client's retry of a lost reply cannot count twice.
func stripeCardFailureRedisAddToWindow(
	ctx context.Context,
	failure *stripeCardFailure,
	now time.Time,
	window time.Duration,
) (count int64, added bool, returnErr error) {
	key := failure.ownerKey()
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			var addCmd *redis.IntCmd
			var countCmd *redis.IntCmd
			_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				addCmd = pipe.ZAddNX(ctx, key, redis.Z{Score: float64(failure.Time.UnixMilli()), Member: failure.EventId})
				pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("(%d", now.Add(-window).UnixMilli()))
				countCmd = pipe.ZCard(ctx, key)
				pipe.Expire(ctx, key, window)
				return nil
			})
			server.Raise(err)
			count, added = countCmd.Val(), 0 < addCmd.Val()
		})
	}, func(err error) {
		count, added, returnErr = 0, false, err
	})
	return
}
