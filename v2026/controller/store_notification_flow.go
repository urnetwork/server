package controller

// What became of each store notification: a Google Play real-time developer
// notification (POST /pay/play, store google) and an App Store Server
// Notification (POST /apple/notification, store apple).
//
// From 2026-10-19 to 10-27 Main refused every Play notification (vault
// google.yml had no webhook.audience) and nothing noticed for eight days: a
// purchase whose first notification came in that window and was cancelled
// before it was processed was never acknowledged, and Google refunded it
// (support inbox 8010, 8046, 8086, 8098). The App Store had the same blind
// spot. Each notification now ends in one outcome:
//
//   - accepted: verified and processed;
//   - refused_unavailable: our verification config is missing (the route
//     answers 503 and logs its fail-closed line, which pages as
//     payment-webhook-refused);
//   - refused_auth: Play's push carried no valid Google token for the
//     configured publisher;
//   - refused_audience: a Google token of the configured publisher, for
//     another audience than the subscription's (our config and Pub/Sub
//     disagree);
//   - refused_signature: an App Store JWS whose signature or chain does not
//     verify against the pinned Apple roots;
//   - refused_claims: a verified App Store notification for another app,
//     environment or version;
//   - refused_malformed: a body that is not a notification;
//   - error: verified, then processing failed (the store retries it).
//
// Each outcome is counted (urnetwork_store_notifications_total), each
// refusal or error but refused_unavailable is logged as one fixed line for the
// monitor's tail ("[store-notification]<store> <outcome>"), and an accepted one
// sets urnetwork_store_notification_last_accepted_timestamp_seconds and, at
// most once per mark interval per process, the store's row of
// store_notification_flow, which the monitor reads against the store's
// purchases.

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The outcomes of a store notification (see the file header).
const (
	StoreNotificationAccepted           = "accepted"
	StoreNotificationRefusedUnavailable = "refused_unavailable"
	StoreNotificationRefusedAuth        = "refused_auth"
	StoreNotificationRefusedAudience    = "refused_audience"
	StoreNotificationRefusedSignature   = "refused_signature"
	StoreNotificationRefusedClaims      = "refused_claims"
	StoreNotificationRefusedMalformed   = "refused_malformed"
	StoreNotificationError              = "error"
)

// The closed outcome label set.
var StoreNotificationOutcomes = []string{
	StoreNotificationAccepted,
	StoreNotificationRefusedUnavailable,
	StoreNotificationRefusedAuth,
	StoreNotificationRefusedAudience,
	StoreNotificationRefusedSignature,
	StoreNotificationRefusedClaims,
	StoreNotificationRefusedMalformed,
	StoreNotificationError,
}

// Store notifications by store (google, apple) and outcome.
var storeNotificationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "store",
	Name:      "notifications_total",
	Help:      "Store notifications by store (google: Play real-time developer notifications, apple: App Store Server Notifications) and outcome (accepted, refused_unavailable, refused_auth, refused_audience, refused_signature, refused_claims, refused_malformed, error)",
}, []string{"store", "outcome"})

// When this process last accepted a notification of the store.
var storeNotificationLastAccepted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "store",
	Name:      "notification_last_accepted_timestamp_seconds",
	Help:      "Unix time this process last accepted a store notification, by store (google, apple); the fleet's is the maximum, and store_notification_flow keeps it across restarts",
}, []string{"store"})

// Registers the metrics, every series from the start so the first refusal is
// an increase.
func init() {
	prometheus.MustRegister(storeNotificationsTotal, storeNotificationLastAccepted)
	for _, store := range model.StoreNotificationStores {
		for _, outcome := range StoreNotificationOutcomes {
			storeNotificationsTotal.WithLabelValues(store, outcome)
		}
	}
}

// The store notification recorder's tunables.
type StoreNotificationFlowSettings struct {
	// The durable mark of an accepted notification is written at most this
	// often per process and store. Non-positive writes it for every one.
	MarkEvery time.Duration
}

// A new instance on every call.
func DefaultStoreNotificationFlowSettings() *StoreNotificationFlowSettings {
	return &StoreNotificationFlowSettings{
		// a minute's lag in an alert whose window is a day, at most one
		// write a minute per api process
		MarkEvery: time.Minute,
	}
}

// Records what became of each notification of one process: the counts, the
// last accepted time and the throttled durable mark. Safe for concurrent use.
type storeNotificationRecorder struct {
	settings *StoreNotificationFlowSettings
	// writes the durable mark of an accepted notification
	markAccepted func(ctx context.Context, store string, acceptedAt time.Time)

	stateLock sync.Mutex
	// store -> when this process last took the slot of its mark
	storeMarkTimes map[string]time.Time
}

// A recorder at settings that writes its marks with markAccepted.
func newStoreNotificationRecorder(
	settings *StoreNotificationFlowSettings,
	markAccepted func(ctx context.Context, store string, acceptedAt time.Time),
) *storeNotificationRecorder {
	return &storeNotificationRecorder{
		settings:       settings,
		markAccepted:   markAccepted,
		storeMarkTimes: map[string]time.Time{},
	}
}

// The process's recorder. Replaceable only by tests.
var storeNotifications = newStoreNotificationRecorder(
	DefaultStoreNotificationFlowSettings(),
	func(ctx context.Context, store string, acceptedAt time.Time) {
		model.MarkStoreNotificationAcceptedInConn(nil, ctx, store, acceptedAt)
	},
)

// Records what became of one notification of a store. It never fails the
// notification: a mark that cannot be written is logged and written by the
// next accepted one.
func RecordStoreNotificationOutcome(ctx context.Context, store string, outcome string) {
	storeNotifications.record(ctx, store, outcome)
}

// See RecordStoreNotificationOutcome.
func (self *storeNotificationRecorder) record(ctx context.Context, store string, outcome string) {
	storeNotificationsTotal.WithLabelValues(store, outcome).Inc()
	switch outcome {
	case StoreNotificationAccepted:
		now := server.NowUtc()
		storeNotificationLastAccepted.WithLabelValues(store).Set(float64(now.Unix()))
		// take the store's slot, unless this process wrote the mark within
		// the interval
		due := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if markTime, ok := self.storeMarkTimes[store]; ok && now.Sub(markTime) < self.settings.MarkEvery {
				return false
			}
			self.storeMarkTimes[store] = now
			return true
		}()
		if !due {
			return
		}
		server.HandleError(func() {
			self.markAccepted(ctx, store, now)
		}, func(err error) {
			// give the slot back, so the next accepted notification writes it
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				delete(self.storeMarkTimes, store)
			}()
			glog.Warningf("[store-notification]%s accepted, but the time it was accepted was not kept: %s\n", store, err)
		})
	case StoreNotificationRefusedUnavailable:
		// the route logged its own fail-closed line, which pages as
		// payment-webhook-refused
	default:
		// one fixed line per refusal or error for the monitor's tail: no
		// token, payload or identity
		glog.Warningf("[store-notification]%s %s\n", store, outcome)
	}
}

// ----- Play's push verification, classified -----

// A refused Play push: why it was refused, as an outcome. Its text is the one
// the route has always answered.
type playAuthError struct {
	outcome string
	// the text, when it is not "Missing authorization."
	message string
}

// The route's text for the refusal.
func (self *playAuthError) Error() string {
	if self.message != "" {
		return self.message
	}
	return "Missing authorization."
}

// A refusal of the given outcome, with the route's usual text.
func playAuthRefused(outcome string) error {
	return &playAuthError{outcome: outcome}
}
