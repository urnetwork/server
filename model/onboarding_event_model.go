package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

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

// AddOnboardingEvents stores a batch in one transaction. A failed insert
// raises with its own error (see AddOnboardingEventInTx).
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

// Stores one event in the caller's transaction. Returns only a failure to
// encode the event's props. A failed insert raises, which ends the transaction
// at once with its own error; returning it let the caller's callback go on to
// a commit that postgres rolled back.
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
	))
	return nil
}

// AppOpenAttributionWindow is how long after a landing click an app open is
// attributed to it.
const AppOpenAttributionWindow = 48 * time.Hour

// AttributeAppOpen writes one app.opened event attributed to the network's most
// recent landing click inside the attribution window, unless that click already
// has one. The network's insert lock, then one statement index-backed on
// (network_id, at), so the auth-client path pays two cheap queries. Returns true
// when an event was written.
func AttributeAppOpen(ctx context.Context, networkId server.Id, now time.Time) (attributed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		attributed = AttributeAppOpenInTx(tx, ctx, networkId, now)
	}, server.TxReadCommitted)
	return
}

// Requires a read committed transaction. The network's app.opened insert lock
// makes concurrent attributions take turns, and the later one observes the
// attribution the earlier one committed (see lockOnboardingEventInsertInTx).
func AttributeAppOpenInTx(tx server.PgTx, ctx context.Context, networkId server.Id, now time.Time) bool {
	lockOnboardingEventInsertInTx(tx, ctx, EventAppOpened, networkId, "")
	return insertAppOpenAttributionInTx(tx, ctx, networkId, now)
}

// The conditional insert alone. Two concurrent transactions can both find the
// click unattributed and both insert, so callers hold the network's app.opened
// insert lock first.
func insertAppOpenAttributionInTx(tx server.PgTx, ctx context.Context, networkId server.Id, now time.Time) bool {
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
				$1::uuid,
				click.network_id,
				$4::varchar(64),
				$3::timestamp,
				$3::timestamp,
				'',
				'',
				'',
				click.tier,
				click.path,
				click.experiment,
				click.variant,
				'',
				jsonb_strip_nulls(jsonb_build_object(
					'step', click.props->>'step',
					'flow_step', click.props->>'flow_step'
				))
			FROM (
				SELECT network_id, at, tier, path, experiment, variant, props
				FROM network_onboarding_event
				WHERE network_id = $2::uuid AND name = $5::varchar(64) AND $6::timestamp <= at AND at <= $3::timestamp
				ORDER BY at DESC
				LIMIT 1
			) click
			WHERE NOT EXISTS (
				SELECT 1
				FROM network_onboarding_event opened
				WHERE opened.network_id = $2::uuid AND opened.name = $4::varchar(64) AND click.at <= opened.at
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

// Orders the conditional inserts that keep one event per key: app.opened per
// network (scope ""), connect.day per network and UTC day (scope the day).
// Each insert reads that its event is missing and writes it in one statement,
// so two concurrent writers of a key can both read it missing and both insert.
// Writers that hold this transaction-scoped advisory lock take turns instead.
// Both names are server-only and written only after this lock, so every writer
// of a key takes it.
//
// The caller's transaction must be read committed. The insert is a later
// statement than the lock, so its snapshot is taken after the lock is granted,
// and PostgreSQL makes a commit visible before it releases the committing
// transaction's locks, so the insert observes an event that an earlier holder
// committed. Repeatable read and serializable keep the snapshot of their first
// statement, this one or an earlier one, which precedes the wait and could miss
// that event, so any isolation but read committed is refused.
//
// This replaced serializable isolation. On the primary every serializable
// transaction registers in the shared SSI state (waits on LWLock
// SerializableXactHash) and takes predicate locks at page and relation
// granularity, which also conflict across networks and fail writers as
// pivots. The lock touches one key, and waits only for other writers of that
// key, each a short insert transaction.
func lockOnboardingEventInsertInTx(tx server.PgTx, ctx context.Context, name string, networkId server.Id, scope string) {
	var isolation string
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT current_setting('transaction_isolation') FROM pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"network-onboarding-event-insert/v1:"+name+":"+networkId.String()+":"+scope,
	).Scan(&isolation))
	if isolation != "read committed" {
		panic(fmt.Errorf("%s insert lock requires read committed isolation, not %s", name, isolation))
	}
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

// connectDayPostCommitTimeout matches the existing finite post-commit
// projection budget used by clockTransferPost. The connection is already
// durable before this optional analytics write begins, so caller cancellation
// must not discard it; the bound prevents a stalled database from retaining
// the connection goroutine indefinitely.
const connectDayPostCommitTimeout = 10 * time.Second

// Connect traffic can arrive in large cohorts. Keep optional analytics writes
// off that path and bound their pressure on the operator's PostgreSQL pool.
const (
	connectDayWriteQueueSize = 512
	connectDayWriteWorkers   = 4
)

type connectDayWriteJob struct {
	ctx         context.Context
	clientId    server.Id
	connectTime time.Time
	day         time.Time
	done        chan<- struct{}
}

var connectDayWriteJobs = make(chan connectDayWriteJob, connectDayWriteQueueSize)
var connectDayWriteWorkersOnce sync.Once

func startConnectDayWriteWorkers() {
	connectDayWriteWorkersOnce.Do(func() {
		for range connectDayWriteWorkers {
			go server.HandleError(func() {
				for job := range connectDayWriteJobs {
					recordConnectDayWrite(job)
				}
			})
		}
	})
}

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
	recordConnectDay(ctx, clientId, connectTime, nil)
}

// Owns an optional completion signal, transferring it to the worker only when
// admitted. Tests can join queued writes before destroying their database.
func recordConnectDay(ctx context.Context, clientId server.Id, connectTime time.Time, done chan<- struct{}) {
	defer func() {
		if done != nil {
			close(done)
		}
	}()
	day := ConnectDayStart(connectTime)
	if !connectDaySeen.remember(clientId, day) {
		return
	}
	startConnectDayWriteWorkers()
	job := connectDayWriteJob{ctx: context.WithoutCancel(ctx), clientId: clientId, connectTime: connectTime, day: day, done: done}
	select {
	case connectDayWriteJobs <- job:
		done = nil
	default:
		// No durable write was admitted; a later connection may retry it.
		connectDaySeen.forget(clientId, day)
	}
}

// Completes the admitted job after committing or forgetting a failed write,
// including any retries the transaction needs.
func recordConnectDayWrite(job connectDayWriteJob) {
	if job.done != nil {
		defer close(job.done)
	}
	writeCtx, writeCancel := context.WithTimeout(
		job.ctx,
		connectDayPostCommitTimeout,
	)
	defer writeCancel()
	defer func() {
		if r := recover(); r != nil {
			connectDaySeen.forget(job.clientId, job.day)
			glog.Warningf("[onboarding]connect.day write failed for client %s: %v\n", job.clientId, r)
		}
	}()
	// The cache deduplicates clients, not networks: two clients of one network
	// can both find its day empty. The network's connect.day insert lock for
	// the day makes them take turns (see lockOnboardingEventInsertInTx).
	server.Tx(writeCtx, func(tx server.PgTx) {
		var networkId server.Id
		err := tx.QueryRow(
			writeCtx,
			`SELECT network_id FROM network_client WHERE client_id = $1`,
			job.clientId,
		).Scan(&networkId)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		lockOnboardingEventInsertInTx(tx, writeCtx, EventConnectDay, networkId, job.day.Format(time.DateOnly))
		insertConnectDayInTx(tx, writeCtx, job.clientId, networkId, job.connectTime, job.day)
	}, server.TxReadCommitted)
}

// The conditional insert alone: the network's connect.day for the day of
// `connectTime` (`day`), unless the day already has one, while the client still
// belongs to `networkId`. Two concurrent transactions can both find the day
// empty and both insert, so callers hold the network's connect.day insert lock
// for the day first, and the network filter keeps the insert inside that lock.
//
// The day bound is a row comparison on (network_id, at), which only the
// network's own index can use. A new UTC day lies past the last ANALYZE of
// `at`, and a plain range let PostgreSQL estimate the whole day at one row
// under (name, at), so each probe read every network's connect.day row of
// the day. The row comparison's estimate depends only on network_id.
func insertConnectDayInTx(
	tx server.PgTx,
	ctx context.Context,
	clientId server.Id,
	networkId server.Id,
	connectTime time.Time,
	day time.Time,
) {
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
				$1::uuid,
				nc.network_id,
				$3::varchar(64),
				$4::timestamp,
				$5::timestamp,
				'', '', '', '', '', '', '', '',
				NULL
			FROM network_client nc
			WHERE
				nc.client_id = $2::uuid AND
				nc.network_id = $8::uuid AND
				NOT EXISTS (
					SELECT 1
					FROM network_onboarding_event e
					WHERE
						e.network_id = nc.network_id AND
						(e.network_id, e.at) >= (nc.network_id, $6::timestamp) AND
						(e.network_id, e.at) < (nc.network_id, $7::timestamp) AND
						e.name = $3::varchar(64)
				)
		`,
		server.NewId(),
		clientId,
		EventConnectDay,
		connectTime,
		server.NowUtc(),
		day,
		day.Add(24*time.Hour),
		networkId,
	))
}
