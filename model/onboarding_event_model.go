package model

import (
	"context"
	"encoding/json"
	"fmt"
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
// Clients reconnect about twice an hour, each time to any of the fleet's
// Connect processes, and a network can have many clients. The day is
// deduplicated per network in three layers:
//   - each process remembers the networks it saw recorded or admitted today,
//     so a repeated connect costs nothing;
//   - a Redis claim per network day admits one writer across the fleet, and
//     becomes a recorded marker once the event is durable;
//   - the conditional insert under the network day's insert lock stays the
//     exact guard (see lockOnboardingEventInsertInTx), so a lost claim, an
//     unavailable Redis or a concurrent retry never stores a second event.
// PostgreSQL then sees about one write per network per day. Remembering
// clients per process instead cost one write transaction per client per
// process per day, about 300 for every event stored.

// ConnectDayStart is the UTC day a connection time falls in.
func ConnectDayStart(at time.Time) time.Time {
	return at.UTC().Truncate(24 * time.Hour)
}

// connectDayMaxCached bounds the per-process set of networks seen today. Past
// it, a network is not remembered and each of its connects reads the Redis
// marker instead. The set is never cleared mid-day: clearing it made every
// remembered entry pay its database write again.
const connectDayMaxCached = 250_000

// Process memory of the network days already recorded or admitted to a writer.
// Safe for concurrent use.
type connectDayCache struct {
	stateLock      sync.Mutex
	day            time.Time
	seenNetworkIds map[server.Id]struct{}
}

var connectDaySeen = &connectDayCache{}

// connectDayPostCommitTimeout matches the existing finite post-commit
// projection budget used by clockTransferPost. The connection is already
// durable before this optional analytics write begins, so caller cancellation
// must not discard it; the bound prevents a stalled database from retaining
// the connection goroutine indefinitely.
const connectDayPostCommitTimeout = 10 * time.Second

// A claim outlives the bounded write and expires soon after a writer that
// stopped without releasing it, so a later connect retries the day.
const connectDayClaimTtl = time.Minute

// The recorded marker covers the rest of its UTC day from any write time, with
// an hour for clock skew between processes.
const connectDayRecordedTtl = 25 * time.Hour

const connectDayRecordedValue = "recorded"

// Releasing a claim after a failed write runs on its own budget, since the
// failure may be the write budget itself.
const connectDayReleaseTimeout = time.Second

// Connect traffic can arrive in large cohorts. Keep optional analytics writes
// off that path and bound their pressure on the operator's PostgreSQL pool.
const (
	connectDayWriteQueueSize = 512
	connectDayWriteWorkers   = 4
)

type connectDayWriteJob struct {
	ctx         context.Context
	networkId   server.Id
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

// Returns false when the network's day is already recorded or admitted in this
// process, true (remembering it while there is room) when it still has to be
// checked. Another day replaces the set.
func (self *connectDayCache) remember(networkId server.Id, day time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.day.Equal(day) || self.seenNetworkIds == nil {
		self.day = day
		self.seenNetworkIds = map[server.Id]struct{}{}
	}
	if _, ok := self.seenNetworkIds[networkId]; ok {
		return false
	}
	if len(self.seenNetworkIds) < connectDayMaxCached {
		self.seenNetworkIds[networkId] = struct{}{}
	}
	return true
}

// Drops a network from the day's set, so the next connect checks the day again.
func (self *connectDayCache) forget(networkId server.Id, day time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.day.Equal(day) && self.seenNetworkIds != nil {
		delete(self.seenNetworkIds, networkId)
	}
}

// RecordConnectDay writes the network's connect.day event for the day of
// `connectTime` unless one exists. `networkId` is the connecting client's
// authenticated network. Runs after the connection is committed, off the
// connection path: a failure never fails or delays the connection, and is
// logged and retried on a later connect of the network.
func RecordConnectDay(ctx context.Context, networkId server.Id, clientId server.Id, connectTime time.Time) {
	recordConnectDay(ctx, networkId, clientId, connectTime, nil)
}

// Owns an optional completion signal, transferring it to the worker only when
// admitted. Tests can join queued writes before destroying their database.
func recordConnectDay(ctx context.Context, networkId server.Id, clientId server.Id, connectTime time.Time, done chan<- struct{}) {
	defer func() {
		if done != nil {
			close(done)
		}
	}()
	day := ConnectDayStart(connectTime)
	if !connectDaySeen.remember(networkId, day) {
		return
	}
	startConnectDayWriteWorkers()
	job := connectDayWriteJob{
		ctx:         context.WithoutCancel(ctx),
		networkId:   networkId,
		clientId:    clientId,
		connectTime: connectTime,
		day:         day,
		done:        done,
	}
	select {
	case connectDayWriteJobs <- job:
		done = nil
	default:
		// No write was admitted; a later connection may retry it.
		connectDaySeen.forget(networkId, day)
	}
}

// One key per network and UTC day holds a writer's claim token or the
// recorded marker.
func connectDayKey(networkId server.Id, day time.Time) string {
	return fmt.Sprintf("{ocd_%s}%s", networkId, day.UTC().Format("20060102"))
}

// Returns the key's holder after the call: the caller's token when the claim is
// now the caller's, the recorded marker, or another writer's token.
const connectDayClaimScript = `
local holder = redis.call('GET', KEYS[1])
if holder then return holder end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return ARGV[1]
`

// Replaces any claim with the recorded marker once the event is durable.
const connectDayMarkScript = `
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return 1
`

// Claims the network day across the fleet. Returns the token the caller now
// holds, else the current holder: the recorded marker or another writer's token.
func claimConnectDay(ctx context.Context, networkId server.Id, day time.Time) (token string, holder string, err error) {
	candidate := server.NewId().String()
	err = server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		var claimErr error
		holder, claimErr = client.Eval(
			ctx,
			connectDayClaimScript,
			[]string{connectDayKey(networkId, day)},
			candidate,
			connectDayClaimTtl.Milliseconds(),
		).Text()
		return claimErr
	})
	if err == nil && holder == candidate {
		token = candidate
	}
	return
}

// Lets every process skip the day. A lost marker costs each process at most
// one more locked conditional insert that finds the event.
func markConnectDayRecorded(ctx context.Context, networkId server.Id, day time.Time) error {
	return server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return client.Eval(
			ctx,
			connectDayMarkScript,
			[]string{connectDayKey(networkId, day)},
			connectDayRecordedValue,
			connectDayRecordedTtl.Milliseconds(),
		).Err()
	})
}

// Drops only the caller's own claim, so the next connect of the network in any
// process retries at once rather than after the claim expires.
func releaseConnectDayClaim(job connectDayWriteJob, token string) {
	if token == "" {
		return
	}
	releaseCtx, releaseCancel := context.WithTimeout(job.ctx, connectDayReleaseTimeout)
	defer releaseCancel()
	err := server.RedisWithDeadline(releaseCtx, func(client server.RedisClient) error {
		return server.RedisRemoveIfEqual(client, releaseCtx, connectDayKey(job.networkId, job.day), []byte(token)).Err()
	})
	if err != nil && glog.V(1) {
		glog.Infof("[onboarding]connect.day claim release failed, it expires in %s: %s\n", connectDayClaimTtl, err)
	}
}

// Completes the admitted job after the day is recorded, found recorded, or left
// for a later connect, including any retries the transaction needs.
func recordConnectDayWrite(job connectDayWriteJob) {
	if job.done != nil {
		defer close(job.done)
	}
	writeCtx, writeCancel := context.WithTimeout(
		job.ctx,
		connectDayPostCommitTimeout,
	)
	defer writeCancel()
	claimToken := ""
	defer func() {
		if r := recover(); r != nil {
			releaseConnectDayClaim(job, claimToken)
			connectDaySeen.forget(job.networkId, job.day)
			glog.Warningf("[onboarding]connect.day write failed for client %s: %v\n", job.clientId, r)
		}
	}()

	claimToken, holder, claimErr := claimConnectDay(writeCtx, job.networkId, job.day)
	switch {
	case claimErr != nil:
		// The locked conditional insert alone keeps the day exact without Redis.
		if glog.V(1) {
			glog.Infof("[onboarding]connect.day claim unavailable, writing unclaimed: %s\n", claimErr)
		}
	case holder == connectDayRecordedValue:
		return
	case claimToken == "":
		// Another process is writing the day. A later connect checks again,
		// after that write is marked or its claim expires.
		connectDaySeen.forget(job.networkId, job.day)
		return
	}

	if !recordConnectDayEvent(writeCtx, job) {
		// The client is not in the network; another client may still record the day.
		releaseConnectDayClaim(job, claimToken)
		connectDaySeen.forget(job.networkId, job.day)
		return
	}
	if err := markConnectDayRecorded(writeCtx, job.networkId, job.day); err != nil && glog.V(1) {
		glog.Infof("[onboarding]connect.day mark failed, the claim expires in %s: %s\n", connectDayClaimTtl, err)
	}
}

// Writes the day's event in one read committed transaction under the network
// day's insert lock, unless the network already has one. Returns whether the
// network's day is recorded, which is false only when the client is not in
// the network. Concurrent writers of the network day take turns on the lock
// (see lockOnboardingEventInsertInTx), so at most one event is stored.
func recordConnectDayEvent(ctx context.Context, job connectDayWriteJob) (recorded bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		recorded = false
		lockOnboardingEventInsertInTx(tx, ctx, EventConnectDay, job.networkId, job.day.Format(time.DateOnly))
		if insertConnectDayInTx(tx, ctx, job.clientId, job.networkId, job.connectTime, job.day) {
			recorded = true
			return
		}
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM network_onboarding_event e
					WHERE
						e.network_id = $1::uuid AND
						(e.network_id, e.at) >= ($1::uuid, $2::timestamp) AND
						(e.network_id, e.at) < ($1::uuid, $3::timestamp) AND
						e.name = $4::varchar(64)
				)
			`,
			job.networkId,
			job.day,
			job.day.Add(24*time.Hour),
			EventConnectDay,
		).Scan(&recorded))
	}, server.TxReadCommitted)
	return
}

// The conditional insert alone: the network's connect.day for the day of
// `connectTime` (`day`), unless the day already has one, while the client still
// belongs to `networkId`. Returns whether it inserted the event. Two concurrent
// transactions can both find the day empty and both insert, so callers hold the
// network's connect.day insert lock for the day first, and the network filter
// keeps the insert inside that lock.
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
) (inserted bool) {
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
	return tag.RowsAffected() == 1
}
