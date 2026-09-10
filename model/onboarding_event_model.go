package model

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// network_onboarding_event is the one product-event table (mmm/onboarding/PLAN.md
// "Optimization loop"): client events from POST /client/events and the
// server-written attribution/outcome events, 400-day retention. Per-network rows
// never leave the server; the nightly results job (S3) aggregates them.

type OnboardingEvent struct {
	EventId   server.Id
	NetworkId server.Id
	Name      string
	// At is the event time as the client saw it (clamped by the controller);
	// ReceivedAt is when the server stored it
	At         time.Time
	ReceivedAt time.Time
	Platform   string
	AppVersion string
	Locale     string
	// Tier is the price tier the network resolved to when the event was recorded
	Tier string
	// Path is the campaign path (A saw the in-app offer, B did not), "" before an
	// offer exists
	Path       string
	Experiment string
	Variant    string
	Session    string
	Props      map[string]any
}

// AddOnboardingEvents stores a batch in one transaction.
func AddOnboardingEvents(ctx context.Context, events []*OnboardingEvent) (returnErr error) {
	if len(events) == 0 {
		return nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for _, event := range events {
			if err := AddOnboardingEventInTx(tx, ctx, event); err != nil {
				returnErr = err
				return
			}
		}
	}, server.TxReadCommitted)
	return
}

// AddOnboardingEvent stores one event.
func AddOnboardingEvent(ctx context.Context, event *OnboardingEvent) error {
	return AddOnboardingEvents(ctx, []*OnboardingEvent{event})
}

func AddOnboardingEventInTx(tx server.PgTx, ctx context.Context, event *OnboardingEvent) error {
	if event.EventId == (server.Id{}) {
		event.EventId = server.NewId()
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = server.NowUtc()
	}
	if event.At.IsZero() {
		event.At = event.ReceivedAt
	}
	var props *string
	if 0 < len(event.Props) {
		propsJson, err := json.Marshal(event.Props)
		if err != nil {
			return err
		}
		propsStr := string(propsJson)
		props = &propsStr
	}
	_, err := tx.Exec(
		ctx,
		`
			INSERT INTO network_onboarding_event (
				event_id,
				network_id,
				name,
				at,
				received_at,
				platform,
				app_version,
				locale,
				tier,
				path,
				experiment,
				variant,
				session,
				props
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`,
		event.EventId,
		event.NetworkId,
		event.Name,
		event.At,
		event.ReceivedAt,
		event.Platform,
		event.AppVersion,
		event.Locale,
		event.Tier,
		event.Path,
		event.Experiment,
		event.Variant,
		event.Session,
		props,
	)
	return err
}

// AppOpenAttributionWindow is how long after a landing click an app open is
// attributed to it.
const AppOpenAttributionWindow = 48 * time.Hour

// AttributeAppOpen writes one app.opened event attributed to the network's most
// recent landing click inside the attribution window, unless that click already
// has one. One statement, index-backed on (network_id, at), so the auth-client
// path pays one cheap query. Returns true when an event was written.
func AttributeAppOpen(ctx context.Context, networkId server.Id, now time.Time) (attributed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		attributed = AttributeAppOpenInTx(tx, ctx, networkId, now)
	}, server.TxReadCommitted)
	return
}

func AttributeAppOpenInTx(tx server.PgTx, ctx context.Context, networkId server.Id, now time.Time) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_onboarding_event (
				event_id,
				network_id,
				name,
				at,
				received_at,
				platform,
				app_version,
				locale,
				tier,
				path,
				experiment,
				variant,
				session,
				props
			)
			SELECT
				$1,
				click.network_id,
				$4,
				$3,
				$3,
				'',
				'',
				'',
				click.tier,
				click.path,
				click.experiment,
				click.variant,
				'',
				jsonb_build_object('step', click.props->>'step')
			FROM (
				SELECT network_id, at, tier, path, experiment, variant, props
				FROM network_onboarding_event
				WHERE network_id = $2 AND name = $5 AND $6 <= at AND at <= $3
				ORDER BY at DESC
				LIMIT 1
			) click
			WHERE NOT EXISTS (
				SELECT 1
				FROM network_onboarding_event opened
				WHERE opened.network_id = $2 AND opened.name = $4 AND click.at <= opened.at
			)
		`,
		server.NewId(),
		networkId,
		now,
		EventAppOpened,
		EventLandingClicked,
		now.Add(-AppOpenAttributionWindow),
	))
	return tag.RowsAffected() == 1
}

// HasOnboardingEvent reports whether the network has at least one event of the
// name (connect.first is once per network, and the client may re-send it).
func HasOnboardingEvent(ctx context.Context, networkId server.Id, name string) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT 1
				FROM network_onboarding_event
				WHERE network_id = $1 AND name = $2
				LIMIT 1
			`,
			networkId,
			name,
		)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
	})
	return
}

// OnboardingEventRetention is how long event rows are kept. Pruning is the
// results job's (S3) responsibility; the value is pinned here so the two agree.
const OnboardingEventRetention = 400 * 24 * time.Hour

// PruneOnboardingEvents deletes rows older than the retention, in bounded chunks.
// Returns the number of rows removed.
func PruneOnboardingEvents(ctx context.Context, now time.Time, limit int) (removed int64) {
	if limit <= 0 {
		limit = 10000
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_onboarding_event
				WHERE event_id IN (
					SELECT event_id
					FROM network_onboarding_event
					WHERE at < $1
					LIMIT $2
				)
			`,
			now.Add(-OnboardingEventRetention),
			limit,
		))
		removed = tag.RowsAffected()
	}, server.TxReadCommitted)
	return
}

// ----- connection history: connect.day -----

// The client connection path writes one connect.day event per (network, UTC
// day) with at least one connection. This is the durable connection history
// the rollup reads for connect_7d, retention_d7 and retention_d30:
// network_client_connection rows are pruned 8 h after disconnect
// (RemoveDisconnectedNetworkClients), so they cover days, not the 31-day
// window the outcomes need. History starts at the deploy of this writer.
//
// The hot path pays nothing for a client whose day is already recorded (an
// in-process set, reset when the UTC day changes) and one index-backed
// conditional insert otherwise.

// ConnectDayStart is the UTC day a connection time falls in.
func ConnectDayStart(at time.Time) time.Time {
	return at.UTC().Truncate(24 * time.Hour)
}

// connectDayMaxCached bounds the per-process set of clients already recorded
// today; past it the set is cleared and a repeated connect pays the
// conditional insert again (which then matches zero rows).
const connectDayMaxCached = 250_000

type connectDayCache struct {
	mutex sync.Mutex
	day   time.Time
	seen  map[server.Id]struct{}
}

var connectDaySeen = &connectDayCache{}

// remember returns false when the client's connection for this UTC day is
// already recorded, true (and remembers it) when it still has to be written.
func (c *connectDayCache) remember(clientId server.Id, day time.Time) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if !c.day.Equal(day) || c.seen == nil || connectDayMaxCached <= len(c.seen) {
		c.day = day
		c.seen = map[server.Id]struct{}{}
	}
	if _, ok := c.seen[clientId]; ok {
		return false
	}
	c.seen[clientId] = struct{}{}
	return true
}

// forget drops a client from the day's set, so a failed write is retried on
// the next connect.
func (c *connectDayCache) forget(clientId server.Id, day time.Time) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.day.Equal(day) && c.seen != nil {
		delete(c.seen, clientId)
	}
}

// RecordConnectDay writes the client's network's connect.day event for the
// day of `connectTime` unless one exists. Its own short transaction after the
// connection is committed, so a failure here never fails or delays the
// connection: it is logged and retried on the client's next connect.
func RecordConnectDay(ctx context.Context, clientId server.Id, connectTime time.Time) {
	day := ConnectDayStart(connectTime)
	if !connectDaySeen.remember(clientId, day) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			connectDaySeen.forget(clientId, day)
			glog.Warningf("[onboarding]connect.day write failed for client %s: %v\n", clientId, r)
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_onboarding_event (
					event_id,
					network_id,
					name,
					at,
					received_at,
					platform,
					app_version,
					locale,
					tier,
					path,
					experiment,
					variant,
					session,
					props
				)
				SELECT
					$1,
					nc.network_id,
					$3,
					$4,
					$5,
					'', '', '', '', '', '', '', '',
					NULL
				FROM network_client nc
				WHERE
					nc.client_id = $2 AND
					NOT EXISTS (
						SELECT 1
						FROM network_onboarding_event e
						WHERE
							e.network_id = nc.network_id AND
							e.name = $3 AND
							$6 <= e.at AND
							e.at < $7
					)
			`,
			server.NewId(),
			clientId,
			EventConnectDay,
			connectTime,
			server.NowUtc(),
			day,
			day.Add(24*time.Hour),
		))
	}, server.TxReadCommitted)
}
