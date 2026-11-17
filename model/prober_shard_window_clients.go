// The prober retention's window step: a pass's window clients go an hour after
// its deadline, not with its generation.
//
// Each tunnel of a probe pass mints a derived client of the pass's own client
// (the taskworker's providerEgressCredentials) and retires it when the tunnel
// closes. On Main, 2026-11-17: about 4,700 a minute, 5,500 a pass and 6.8M a
// day; a window lives about 16 seconds (p50; p99 36 s). Until this step every
// retired window stayed until its pass's generation dropped, eight days and
// about 54M rows at the steady state, with about 0.9 location reliability rows
// each and a TLS certificate for each post-quantum turn's window. The drop's
// contract walk paces it, so on 2026-11-17 about 166M windows of 39,000
// passes back to 10-22 were waiting.
//
// Nothing reads a retired window once its pass settled. Its contracts carry
// their own network ids, so close, expiry, settlement and payout never read its
// row, as for a hosted proxy's retired windows (network_client_retired_hosted.go,
// which deletes those a day after their deactivation). It parents no client,
// earns nothing as a provider, and the location fold cannot write its row again
// once it is gone: both its reads join network_client. The pass's contracts are
// found by its payer network, so the drop's walk does not need the windows
// either. A window's key history goes by its own retention, which deletes a
// client's history only once its row is gone: the delete trigger records a
// child gone at its deactivation. So a post-quantum turn's window, which
// publishes its key (about 0.95M rows a day since 2026-11-14, about 2.2 KB
// each), loses its history seven days after its deactivation instead of after
// its generation's drop.
//
// The step takes the passes in the order of their creation, from a cursor that
// round trips through the task args. A pass is due once its deadline is
// WindowMinDeadlineAge past. A due pass that drained, whose funded contracts
// are all resolved and whose balances' escrows are all settled, loses its
// window clients and the rows keyed by them, in transactions of at most
// WindowBatchSize windows: client_tls_certificate, network_client_role,
// network_client_provider_intent, network_client_location_reliability and
// provide_key, then their Redis state after the commit. A window with a
// connected connection or a hosted proxy resource stays for the drop, which
// refuses such a pass as before. The pass's own client, identity, contracts and
// registry row stay for its generation. The step reads settlement itself: the
// drop's postponements overwrite the settle sweep's 'infinity' mark.
//
// A due pass not yet settled, or still live, is waited at: the run stops there
// and the next run reads it again. Past WindowSkipAge it is passed over, and its
// windows wait for its generation's drop. A pass whose registry row another
// transaction holds ends the run, and the next run comes back to it soon.
//
// The step runs in the mode of the retention's control (off, dry_run, on),
// unless its `windows` field turns it off, at most maxWindowShardsPerMinute
// passes a minute. A dry run reads one page and counts the windows a run would
// delete, and moves nothing.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Where the window step resumes: the last pass it cleared or passed over, in
// the order of creation (create_time, task_id, epoch). The zero value starts at
// the oldest pass.
type ProberShardWindowCursor struct {
	CreateTime time.Time `json:"create_time,omitzero"`
	TaskId     server.Id `json:"task_id"`
	Epoch      server.Id `json:"epoch"`
}

// The step label of the window step's transactions in
// urnetwork_prober_retention_batch_duration_seconds.
const proberShardRetentionStepWindows = "windows"

// What happened to a pass the window step took up.
const (
	// its window clients and their rows are gone
	proberShardWindowRemoved = "removed"
	// it is not settled, or still live, inside WindowSkipAge: the run stopped
	// at it
	proberShardWindowWaiting = "waiting"
	// it is not settled, or still live, past WindowSkipAge: its windows wait
	// for its generation's drop
	proberShardWindowSkipped = "skipped"
	// another transaction held its registry row: the next run comes back
	proberShardWindowBusy = "busy"
	// its identity went, or is going, with its generation
	proberShardWindowGone = "gone"
	// a step failed; the next run takes it up again
	proberShardWindowError = "error"
	// a dry run counted its windows
	proberShardWindowDryRun = "dry_run"
)

var proberShardWindowOutcomes = []string{
	proberShardWindowRemoved,
	proberShardWindowWaiting,
	proberShardWindowSkipped,
	proberShardWindowBusy,
	proberShardWindowGone,
	proberShardWindowError,
	proberShardWindowDryRun,
}

// Why a window run ended, besides the drop's stops: a pass's registry row was
// held, and the next run comes back to it soon.
const proberShardRetentionStopBusy = "busy"

var (
	proberShardWindowShardsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_window_shards_total",
		Help: "Prober shard passes the retention's window step took up, by outcome",
	}, []string{"outcome"})
	proberShardWindowClientsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_window_clients_total",
		Help: "Window clients of settled prober shard passes the retention's window step deleted; their rows by table are in urnetwork_prober_retention_deleted_rows_total",
	})
	proberShardWindowCursorGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_window_cursor_timestamp_seconds",
		Help: "Create time of the last prober shard pass the retention's window step cleared or passed over. Each taskworker keeps its own last run's value",
	})
)

func init() {
	for _, outcome := range proberShardWindowOutcomes {
		proberShardWindowShardsCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(
		proberShardWindowShardsCounter,
		proberShardWindowClientsCounter,
		proberShardWindowCursorGauge,
	)
}

// What one window run did.
type ProberShardWindowReport struct {
	// The mode of the last control read.
	Mode ProberShardRetentionMode
	// Passes taken up, by outcome.
	Shards map[string]int64
	// Window clients deleted.
	Clients int64
	// Window clients a dry run found a run would delete.
	Eligible int64
	// Rows deleted, by table, the window clients' included.
	Deleted map[string]int64
	// No pass is due before one the step waits at, or none is due at all.
	CaughtUp bool
	// Why the run ended.
	Stop string
}

func newProberShardWindowReport() *ProberShardWindowReport {
	return &ProberShardWindowReport{
		Shards:  map[string]int64{},
		Deleted: map[string]int64{},
		// every return sets its stop; a panic counts as an error
		Stop: proberShardRetentionStopError,
	}
}

// The next run waits the idle interval: the step caught up or waits at a pass,
// is off or its control unreadable, or only dry ran.
func (self *ProberShardWindowReport) Idle() bool {
	switch self.Stop {
	case proberShardRetentionStopCaughtUp, proberShardRetentionStopOff, proberShardRetentionStopControl, proberShardRetentionStopDryRun:
		return true
	default:
		return false
	}
}

func (self *ProberShardWindowReport) addShard(outcome string) {
	self.Shards[outcome] += 1
	proberShardWindowShardsCounter.WithLabelValues(outcome).Inc()
}

func (self *ProberShardWindowReport) addDeleted(table string, count int64) {
	if count == 0 {
		return
	}
	self.Deleted[table] += count
	proberShardRetentionDeletedRowsCounter.WithLabelValues(table).Add(float64(count))
}

// What the window step does with a pass it reached.
const (
	// its deadline is inside WindowMinDeadlineAge: no later pass is due either
	proberShardWindowActionNotDue = "not_due"
	// delete its window clients
	proberShardWindowActionRemove = "remove"
	// wait at it: it is not settled, or still live, inside WindowSkipAge
	proberShardWindowActionWait = "wait"
	// pass it over: the same past WindowSkipAge
	proberShardWindowActionSkip = "skip"
	// its identity went, or is going, with its generation
	proberShardWindowActionGone = "gone"
)

// What the window step does with a pass in state, with deadline, at now.
// settled is whether every contract the pass funded is resolved and every
// escrow of its balances settled; it is read only for a drained pass past
// WindowMinDeadlineAge. Pure.
func proberShardWindowAction(state string, deadline time.Time, settled bool, now time.Time, settings *ProberShardRetentionSettings) string {
	switch {
	case state != "active" && state != "draining":
		return proberShardWindowActionGone
	case !deadline.Before(now.Add(-settings.WindowMinDeadlineAge)):
		return proberShardWindowActionNotDue
	case state == "draining" && settled:
		return proberShardWindowActionRemove
	case deadline.Before(now.Add(-settings.WindowSkipAge)):
		return proberShardWindowActionSkip
	default:
		return proberShardWindowActionWait
	}
}

// The next passes after the cursor, live or drained, in the order of their
// creation. The registry holds a week or two of passes, so this reads it
// whole. $1, $2, $3 the cursor, $4 the page size.
const proberShardWindowPageSql = `
	SELECT task_id, epoch, shard_index, shard_count, network_id, client_id, state, create_time, deadline
	FROM prober_shard_run
	WHERE
		state IN ('active', 'draining') AND
		(create_time, task_id, epoch) > ($1::timestamp, $2::uuid, $3::uuid)
	ORDER BY create_time, task_id, epoch
	LIMIT $4
`

// The pass's registry row, locked unless another transaction holds it: the
// drop's identity transaction and the settle sweep lock it too.
const proberShardWindowLockSql = `
	SELECT network_id, client_id, state, deadline
	FROM prober_shard_run
	WHERE task_id = $1 AND epoch = $2
	FOR UPDATE SKIP LOCKED
`

// Deletes at most $3 window clients of the network $1: every retired client
// but the pass's own ($2), except one with a connected connection or a hosted
// proxy resource, which the drop refuses a pass for. A window another
// transaction holds is skipped rather than waited for.
const proberShardWindowDeleteSql = `
	WITH doomed AS MATERIALIZED (
		SELECT client.client_id
		FROM network_client AS client
		WHERE
			client.network_id = $1 AND
			client.client_id <> $2 AND
			NOT client.active AND
			NOT EXISTS (
				SELECT 1 FROM network_client_connection AS connection
				WHERE connection.client_id = client.client_id AND connection.connected
			) AND
			NOT EXISTS (
				SELECT 1 FROM proxy_device_config AS config
				WHERE config.client_id = client.client_id
			) AND
			NOT EXISTS (
				SELECT 1 FROM proxy_client AS proxy
				WHERE proxy.client_id = client.client_id
			)
		ORDER BY client.client_id
		LIMIT $3
		FOR UPDATE OF client SKIP LOCKED
	)
	DELETE FROM network_client
	USING doomed
	WHERE network_client.client_id = doomed.client_id
	RETURNING network_client.client_id
`

// The rows keyed by the deleted windows $1, each counted: those the drop's
// identity transaction deletes with a client (reapProberShard), but the
// connections, which the connection reap deletes eight hours after they close.
const proberShardWindowDeleteDependentsSql = `
	WITH
		certificates AS (DELETE FROM client_tls_certificate WHERE client_id = ANY($1) RETURNING 1),
		roles AS (DELETE FROM network_client_role WHERE client_id = ANY($1) RETURNING 1),
		intents AS (DELETE FROM network_client_provider_intent WHERE client_id = ANY($1) RETURNING 1),
		reliabilities AS (DELETE FROM network_client_location_reliability WHERE client_id = ANY($1) RETURNING 1),
		provide_keys AS (DELETE FROM provide_key WHERE client_id = ANY($1) RETURNING 1)
	SELECT
		(SELECT count(*) FROM certificates),
		(SELECT count(*) FROM roles),
		(SELECT count(*) FROM intents),
		(SELECT count(*) FROM reliabilities),
		(SELECT count(*) FROM provide_keys)
`

// The tables of proberShardWindowDeleteDependentsSql's counts, in order.
var proberShardWindowDependentTables = []string{
	"client_tls_certificate",
	"network_client_role",
	"network_client_provider_intent",
	"network_client_location_reliability",
	"provide_key",
}

// The window clients a run would delete from the network $1, whose own client
// is $2: proberShardWindowDeleteSql's set, unbounded, for a dry run.
const proberShardWindowCountSql = `
	SELECT count(*)
	FROM network_client AS client
	WHERE
		client.network_id = $1 AND
		client.client_id <> $2 AND
		NOT client.active AND
		NOT EXISTS (
			SELECT 1 FROM network_client_connection AS connection
			WHERE connection.client_id = client.client_id AND connection.connected
		) AND
		NOT EXISTS (
			SELECT 1 FROM proxy_device_config AS config
			WHERE config.client_id = client.client_id
		) AND
		NOT EXISTS (
			SELECT 1 FROM proxy_client AS proxy
			WHERE proxy.client_id = client.client_id
		)
`

// A pass as the page read it.
type proberShardWindowPass struct {
	key        ProberShardKey
	networkId  server.Id
	clientId   server.Id
	state      string
	createTime time.Time
	deadline   time.Time
}

// The cursor just past the pass.
func (self *proberShardWindowPass) cursor() ProberShardWindowCursor {
	return ProberShardWindowCursor{CreateTime: self.createTime, TaskId: self.key.TaskId, Epoch: self.key.Epoch}
}

// One run: takes up the due passes after cursor in the order of their
// creation, deleting each settled one's window clients, until it reaches a
// pass that is not due or that it waits at, the budget ends, the step turns
// off or ctx ends. A failed pass ends the run with its error, and next stays
// before it, so the retry takes it up again. next is where the next run
// resumes.
func RemoveProberShardWindows(
	ctx context.Context,
	now time.Time,
	cursor ProberShardWindowCursor,
	settings *ProberShardRetentionSettings,
) (report *ProberShardWindowReport, next ProberShardWindowCursor, returnErr error) {
	run := &proberShardWindowRun{
		settings: settings,
		now:      now.UTC(),
		runEnd:   time.Now().Add(settings.RunBudget),
		report:   newProberShardWindowReport(),
		cursor:   cursor,
	}
	report = run.report
	defer func() {
		next = run.cursor
		if glog.V(1) {
			glog.Infof(
				"[prober_retention]windows mode=%s stop=%s shards=%v clients=%d eligible=%d deleted=%v cursor=%s/%s\n",
				report.Mode,
				report.Stop,
				report.Shards,
				report.Clients,
				report.Eligible,
				report.Deleted,
				run.cursor.CreateTime.Format(time.RFC3339),
				run.cursor.Epoch,
			)
		}
	}()

	control, ok := run.readControl(ctx)
	if !ok {
		return
	}
	if control.Mode == ProberShardRetentionModeDryRun {
		returnErr = run.dryRun(ctx)
		return
	}
	for {
		if stop, ok := run.stopped(ctx); ok {
			report.Stop = stop
			return
		}
		passes, err := run.selectPage(ctx)
		if err != nil {
			returnErr = run.fail(ctx, err)
			return
		}
		if len(passes) == 0 {
			report.CaughtUp = true
			report.Stop = proberShardRetentionStopCaughtUp
			return
		}
		for _, pass := range passes {
			if stop, ok := run.stopped(ctx); ok {
				report.Stop = stop
				return
			}
			control, ok = run.readControl(ctx)
			if !ok {
				return
			}
			if control.Mode == ProberShardRetentionModeDryRun {
				// the next run reads the windows it would delete
				report.Stop = proberShardRetentionStopDryRun
				return
			}
			stop, err := run.takePass(ctx, pass, control)
			if err != nil {
				returnErr = run.fail(ctx, err)
				return
			}
			if stop != "" {
				report.Stop = stop
				return
			}
		}
	}
}

// One window run's clock, budget, cursor and report.
type proberShardWindowRun struct {
	settings *ProberShardRetentionSettings
	// The run's now, which the deadlines are measured against.
	now    time.Time
	runEnd time.Time
	report *ProberShardWindowReport
	// Just past the last pass cleared or passed over.
	cursor ProberShardWindowCursor
}

// Reads the control. False, with the run's stop set, when the window step must
// stop: the control is unreadable, the mode is off or the window step is off.
func (self *proberShardWindowRun) readControl(ctx context.Context) (*ProberShardRetentionControl, bool) {
	control, err := GetProberShardRetentionControl(ctx, self.settings)
	if err != nil {
		self.report.Stop = proberShardRetentionStopControl
		glog.V(1).Infof("[prober_retention]windows control unavailable, stopping: %s\n", err)
		return nil, false
	}
	self.report.Mode = control.Mode
	if control.Mode == ProberShardRetentionModeOff || !control.DropWindows {
		self.report.Stop = proberShardRetentionStopOff
		return nil, false
	}
	return control, true
}

// The run's stop, if ctx ended or the budget is spent.
func (self *proberShardWindowRun) stopped(ctx context.Context) (string, bool) {
	select {
	case <-ctx.Done():
		return proberShardRetentionStopCanceled, true
	default:
	}
	if !time.Now().Before(self.runEnd) {
		return proberShardRetentionStopBudget, true
	}
	return "", false
}

// The run's stop and error for a failure: none when ctx ended.
func (self *proberShardWindowRun) fail(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		self.report.Stop = proberShardRetentionStopCanceled
		return nil
	}
	self.report.Stop = proberShardRetentionStopError
	return err
}

// Waits delay, or until the run budget ends. False when ctx ended first.
func (self *proberShardWindowRun) pace(ctx context.Context, delay time.Duration) bool {
	delay = min(delay, time.Until(self.runEnd))
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Moves the cursor past the pass.
func (self *proberShardWindowRun) advance(pass *proberShardWindowPass) {
	self.cursor = pass.cursor()
	proberShardWindowCursorGauge.Set(float64(pass.createTime.Unix()))
}

// The next page of passes after the cursor.
func (self *proberShardWindowRun) selectPage(ctx context.Context) (passes []*proberShardWindowPass, returnErr error) {
	returnErr = proberShardRetentionRecover(func() {
		server.Db(ctx, func(conn server.PgConn) {
			passes = nil
			rows, err := conn.Query(
				ctx,
				proberShardWindowPageSql,
				self.cursor.CreateTime.UTC(),
				self.cursor.TaskId,
				self.cursor.Epoch,
				self.settings.ShardPageSize,
			)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					pass := &proberShardWindowPass{}
					server.Raise(rows.Scan(
						&pass.key.TaskId,
						&pass.key.Epoch,
						&pass.key.ShardIndex,
						&pass.key.ShardCount,
						&pass.networkId,
						&pass.clientId,
						&pass.state,
						&pass.createTime,
						&pass.deadline,
					))
					passes = append(passes, pass)
				}
			})
		})
	})
	return
}

// Takes one pass up. Its window clients go batch by batch while its action is
// to remove them, and the pass is paced to the control's rate. stop is the
// run's stop when the run ends at this pass, and empty when the cursor moved
// past it.
func (self *proberShardWindowRun) takePass(ctx context.Context, pass *proberShardWindowPass, control *ProberShardRetentionControl) (stop string, returnErr error) {
	// The page row decides a pass that is not due, or still live, without a
	// lock. A drained one is decided under its lock, with its settlement read.
	if pass.state == "active" || !pass.deadline.Before(self.now.Add(-self.settings.WindowMinDeadlineAge)) {
		switch proberShardWindowAction(pass.state, pass.deadline, false, self.now, self.settings) {
		case proberShardWindowActionNotDue:
			self.report.CaughtUp = true
			return proberShardRetentionStopCaughtUp, nil
		case proberShardWindowActionGone:
			self.report.addShard(proberShardWindowGone)
			self.advance(pass)
			return "", nil
		case proberShardWindowActionSkip:
			self.report.addShard(proberShardWindowSkipped)
			self.advance(pass)
			return "", nil
		default:
			self.report.addShard(proberShardWindowWaiting)
			self.report.CaughtUp = true
			return proberShardRetentionStopCaughtUp, nil
		}
	}

	start := time.Now()
	var removed int64
	for {
		batch, err := self.removeWindowBatch(ctx, pass)
		if err != nil {
			self.report.addShard(proberShardWindowError)
			return "", fmt.Errorf("prober shard %s window step: %w", pass.key.Epoch, err)
		}
		switch batch.action {
		case proberShardWindowActionGone:
			self.report.addShard(proberShardWindowGone)
			self.advance(pass)
			return "", nil
		case proberShardWindowActionNotDue:
			self.report.CaughtUp = true
			return proberShardRetentionStopCaughtUp, nil
		case proberShardWindowActionWait:
			self.report.addShard(proberShardWindowWaiting)
			self.report.CaughtUp = true
			return proberShardRetentionStopCaughtUp, nil
		case proberShardWindowActionSkip:
			self.report.addShard(proberShardWindowSkipped)
			self.advance(pass)
			return "", nil
		case proberShardWindowActionBusy:
			self.report.addShard(proberShardWindowBusy)
			return proberShardRetentionStopBusy, nil
		}
		// the batch committed: its windows' Redis state goes after it
		if 0 < len(batch.clientIds) {
			if err := proberShardRetentionRecover(func() {
				removeProberShardClientRedisState(ctx, batch.clientIds)
			}); err != nil {
				self.report.addShard(proberShardWindowError)
				return "", fmt.Errorf("prober shard %s window Redis state: %w", pass.key.Epoch, err)
			}
		}
		removed += int64(len(batch.clientIds))
		if len(batch.clientIds) < self.settings.WindowBatchSize {
			break
		}
		if stop, ok := self.stopped(ctx); ok {
			// the pass's next batch is the next run's
			return stop, nil
		}
	}
	self.report.addShard(proberShardWindowRemoved)
	self.advance(pass)
	if 0 < removed {
		paced := time.Duration(float64(time.Minute) / control.MaxWindowShardsPerMinute)
		if !self.pace(ctx, paced-time.Since(start)) {
			return proberShardRetentionStopCanceled, nil
		}
	}
	return "", nil
}

// Its registry row is held by another transaction: the next run comes back.
const proberShardWindowActionBusy = "busy"

// One window batch: the pass's action under its registry row's lock, and the
// window clients it deleted.
type proberShardWindowBatch struct {
	action    string
	clientIds []server.Id
}

// One window transaction for the pass: its registry row locked, settlement
// read, and at most WindowBatchSize of its window clients deleted with their
// rows, counted into the report once it commits.
func (self *proberShardWindowRun) removeWindowBatch(ctx context.Context, pass *proberShardWindowPass) (batch *proberShardWindowBatch, returnErr error) {
	start := time.Now()
	deleted := map[string]int64{}
	returnErr = proberShardRetentionRecover(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// a retried transaction starts over
			batch = &proberShardWindowBatch{}
			deleted = map[string]int64{}
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, self.settings.StatementTimeout.Milliseconds())))

			var networkId, clientId server.Id
			var state string
			var deadline time.Time
			found := false
			rows, err := tx.Query(ctx, proberShardWindowLockSql, pass.key.TaskId, pass.key.Epoch)
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					server.Raise(rows.Scan(&networkId, &clientId, &state, &deadline))
					found = true
				}
			})
			if !found {
				var registered bool
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM prober_shard_run WHERE task_id = $1 AND epoch = $2)`,
					pass.key.TaskId, pass.key.Epoch).Scan(&registered))
				if registered {
					batch.action = proberShardWindowActionBusy
				} else {
					batch.action = proberShardWindowActionGone
				}
				return
			}
			settled := false
			if proberShardWindowAction(state, deadline, true, self.now, self.settings) == proberShardWindowActionRemove {
				var unsettled bool
				server.Raise(tx.QueryRow(ctx, proberShardHasUnresolvedContractsSql, networkId).Scan(&unsettled))
				if !unsettled {
					server.Raise(tx.QueryRow(ctx, proberShardHasUnsettledEscrowSql, networkId).Scan(&unsettled))
				}
				settled = !unsettled
			}
			batch.action = proberShardWindowAction(state, deadline, settled, self.now, self.settings)
			if batch.action != proberShardWindowActionRemove {
				return
			}

			rows, err = tx.Query(ctx, proberShardWindowDeleteSql, networkId, clientId, self.settings.WindowBatchSize)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					batch.clientIds = append(batch.clientIds, id)
				}
			})
			deleted["network_client"] = int64(len(batch.clientIds))
			if len(batch.clientIds) == 0 {
				return
			}
			counts := make([]int64, len(proberShardWindowDependentTables))
			targets := make([]any, len(counts))
			for i := range counts {
				targets[i] = &counts[i]
			}
			server.Raise(tx.QueryRow(ctx, proberShardWindowDeleteDependentsSql, batch.clientIds).Scan(targets...))
			for i, table := range proberShardWindowDependentTables {
				deleted[table] = counts[i]
			}
		}, server.TxReadCommitted, server.TxLockTimeout(self.settings.LockTimeout))
	})
	outcome := "ok"
	if returnErr != nil {
		outcome = "error"
		batch = nil
	}
	proberShardRetentionBatchDuration.WithLabelValues(proberShardRetentionStepWindows, outcome).Observe(time.Since(start).Seconds())
	if returnErr == nil {
		// counted once the transaction committed
		for table, count := range deleted {
			self.report.addDeleted(table, count)
		}
		self.report.Clients += deleted["network_client"]
		proberShardWindowClientsCounter.Add(float64(deleted["network_client"]))
	}
	return
}

// A dry run: the first page of passes after the cursor, each due one counted
// as taken up and a settled one's windows counted, up to the first pass that
// is not due or that a run would wait at. It changes nothing, the cursor
// included, so the next dry run reads the same.
func (self *proberShardWindowRun) dryRun(ctx context.Context) error {
	passes, err := self.selectPage(ctx)
	if err != nil {
		return self.fail(ctx, err)
	}
	self.report.Stop = proberShardRetentionStopDryRun
	for _, pass := range passes {
		settled := false
		var eligible int64
		returnErr := proberShardRetentionRecover(func() {
			server.Db(ctx, func(conn server.PgConn) {
				settled = false
				eligible = 0
				if proberShardWindowAction(pass.state, pass.deadline, true, self.now, self.settings) != proberShardWindowActionRemove {
					return
				}
				var unsettled bool
				server.Raise(conn.QueryRow(ctx, proberShardHasUnresolvedContractsSql, pass.networkId).Scan(&unsettled))
				if !unsettled {
					server.Raise(conn.QueryRow(ctx, proberShardHasUnsettledEscrowSql, pass.networkId).Scan(&unsettled))
				}
				settled = !unsettled
				if settled {
					server.Raise(conn.QueryRow(ctx, proberShardWindowCountSql, pass.networkId, pass.clientId).Scan(&eligible))
				}
			})
		})
		if returnErr != nil {
			return self.fail(ctx, returnErr)
		}
		switch proberShardWindowAction(pass.state, pass.deadline, settled, self.now, self.settings) {
		case proberShardWindowActionNotDue, proberShardWindowActionWait:
			return nil
		case proberShardWindowActionRemove:
			self.report.addShard(proberShardWindowDryRun)
			self.report.Eligible += eligible
		}
	}
	return nil
}
