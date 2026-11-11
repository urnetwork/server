package model

// The reads behind the service reminders (controller/service_reminders_controller.go).

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
)

// SupporterWindowEnding is a network's last supporter window.
type SupporterWindowEnding struct {
	NetworkId server.Id
	EndTime   time.Time
	Market    string
	// the coin of a crypto invoice the window was paid with, when its market
	// does not name it: "sol" for a solana window a native SOL invoice paid
	// (its transaction id is the invoice id), else ""
	CryptoAsset string
}

// ListLastSupporterWindowsEnding are the supporter windows on `markets` that
// end in [endFrom, endTo) with no later supporter window on any market: the
// network has not renewed (yet).
func ListLastSupporterWindowsEnding(ctx context.Context, endFrom time.Time, endTo time.Time, markets []string, limit int) (windows []*SupporterWindowEnding) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT ON (s.network_id) s.network_id, s.end_time, s.market,
					COALESCE((
						SELECT intent.asset FROM crypto_payment_intent intent
						WHERE s.market = 'solana'
						  AND intent.intent_id = (
							CASE WHEN s.transaction_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
							THEN s.transaction_id::uuid END
						  )
					), '')
				FROM subscription_renewal s
				WHERE
					s.subscription_type = 'supporter' AND
					s.market = ANY($3::text[]) AND
					$1 <= s.end_time AND s.end_time < $2 AND
					NOT EXISTS (
						SELECT 1 FROM subscription_renewal later
						WHERE
							later.network_id = s.network_id AND
							later.subscription_type = 'supporter' AND
							s.end_time < later.end_time
					)
				ORDER BY s.network_id, s.end_time DESC
				LIMIT $4
			`,
			endFrom,
			endTo,
			markets,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				w := &SupporterWindowEnding{}
				server.Raise(result.Scan(&w.NetworkId, &w.EndTime, &w.Market, &w.CryptoAsset))
				windows = append(windows, w)
			}
		})
	})
	return
}

// StoreTrial is a trial the app recorded starting (purchase.completed{trial}).
type StoreTrial struct {
	NetworkId server.Id
	StartedAt time.Time
	Store     string
}

// ListOpenStoreTrialsStarted are the first trial purchases on `stores` recorded
// in [startedFrom, startedTo) whose outcome (trial.converted or
// trial.cancelled) is not recorded yet.
func ListOpenStoreTrialsStarted(ctx context.Context, startedFrom time.Time, startedTo time.Time, stores []string, limit int) (trials []*StoreTrial) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT trial.network_id, trial.at, trial.store
				FROM (
					SELECT DISTINCT ON (network_id) network_id, at, COALESCE(props->>'store', '') AS store
					FROM network_onboarding_event
					WHERE name = $1 AND (props->>'trial') = 'true'
					ORDER BY network_id, at
				) trial
				WHERE
					$2 <= trial.at AND trial.at < $3 AND
					trial.store = ANY($4::text[]) AND
					NOT EXISTS (
						SELECT 1 FROM network_onboarding_event outcome
						WHERE
							outcome.network_id = trial.network_id AND
							outcome.name = ANY($5::text[])
					)
				ORDER BY trial.at, trial.network_id
				LIMIT $6
			`,
			onboarding.EventPurchaseCompleted,
			startedFrom,
			startedTo,
			stores,
			[]string{EventTrialConverted, EventTrialCancelled},
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				t := &StoreTrial{}
				server.Raise(result.Scan(&t.NetworkId, &t.StartedAt, &t.Store))
				trials = append(trials, t)
			}
		})
	})
	return
}
