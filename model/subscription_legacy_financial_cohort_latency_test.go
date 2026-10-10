// Public-owner controls retain protocol delay, rollback, fallback and durable
// output work. This is an explicit local latency model, not a Main measurement.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"maps"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

// The private child database alone uses this proxy. Every real backend Ready
// reply receives the selected delay while enabled; neither SQL nor returned
// frame contents are retained. The one-shot SET barrier has no timer: only
// actual client closure or parent cancellation releases it.
type legacyCohortLatencyProxy struct {
	listener                   net.Listener
	target                     string
	ctx                        context.Context
	cancel                     context.CancelFunc
	enabled                    atomic.Bool
	delayNs                    atomic.Int64
	jitter                     atomic.Bool
	jitterCharged              [5]atomic.Int64
	jitterApplied              [5]atomic.Int64
	holdSet                    atomic.Bool
	held                       atomic.Int64
	heldClosed                 atomic.Int64
	ready                      atomic.Int64
	delayed                    atomic.Int64
	frontendBytes              atomic.Int64
	backendBytes               atomic.Int64
	begins                     atomic.Int64
	errorLock                  sync.Mutex
	errorClasses               map[string]int64
	commits                    atomic.Int64
	rollbacks                  atomic.Int64
	writeCommits               atomic.Int64
	writeRollbacks             atomic.Int64
	activeWritesClosed         atomic.Int64
	activeClosed               atomic.Int64
	commandCompleteObservation atomic.Pointer[legacyCohortCommandCompleteObservation]
	diagnosticCapture          atomic.Pointer[legacyCohortProxyDiagnosticCapture]
	diagnosticOrdinal          atomic.Uint64
	maxTxNs                    atomic.Int64
	stateLock                  sync.Mutex
	connectionKVs              map[net.Conn]bool
	accepted                   chan struct{}
	joined                     sync.WaitGroup
	closeOnce                  sync.Once
}

// A fixture can advance its private admission clock after an actual completed
// statement. The callback must not retain frame data or perform blocking work.
type legacyCohortCommandCompleteObservation struct {
	observe func([]byte)
}

// Counts bytes read during the enabled window without retaining request data.
type legacyCohortLatencyReader struct {
	reader io.Reader
	proxy  *legacyCohortLatencyProxy
}

func (self *legacyCohortLatencyReader) Read(p []byte) (int, error) {
	n, err := self.reader.Read(p)
	if self.proxy.enabled.Load() {
		self.proxy.frontendBytes.Add(int64(n))
	}
	return n, err
}

// Construction owns the accept loop; close joins every accepted connection.
func newLegacyCohortLatencyProxy(t testing.TB, ctx context.Context, target string) *legacyCohortLatencyProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("private latency proxy could not listen", err)
	}
	owned, cancel := context.WithCancel(ctx)
	proxy := &legacyCohortLatencyProxy{listener: listener, target: target, ctx: owned, cancel: cancel,
		connectionKVs: map[net.Conn]bool{}, accepted: make(chan struct{}), errorClasses: map[string]int64{}}
	go func() {
		defer close(proxy.accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.joined.Add(1)
			go func() { defer proxy.joined.Done(); proxy.forward(client) }()
		}
	}()
	return proxy
}

// Close also releases any reply barrier, then joins both forwarding directions.
func (self *legacyCohortLatencyProxy) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		_ = self.listener.Close()
		<-self.accepted
		self.stateLock.Lock()
		connections := make([]net.Conn, 0, len(self.connectionKVs))
		for conn := range self.connectionKVs {
			connections = append(connections, conn)
		}
		self.stateLock.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		self.joined.Wait()
	})
}

// Observes command boundaries at the proxy, not row-lock acquisition/release.
// A completed transaction interval excludes its last reply's modeled delay.
func (self *legacyCohortLatencyProxy) forward(client net.Conn) {
	defer client.Close()
	upstream, err := (&net.Dialer{}).DialContext(self.ctx, "tcp", self.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	self.stateLock.Lock()
	if self.ctx.Err() != nil {
		self.stateLock.Unlock()
		return
	}
	self.connectionKVs[client], self.connectionKVs[upstream] = true, true
	self.stateLock.Unlock()
	defer func() {
		self.stateLock.Lock()
		delete(self.connectionKVs, client)
		delete(self.connectionKVs, upstream)
		self.stateLock.Unlock()
	}()
	frontendDone := make(chan struct{})
	var frontend io.Reader = &legacyCohortLatencyReader{reader: client, proxy: self}
	go func() {
		defer close(frontendDone)
		_, _ = io.Copy(upstream, frontend)
		_ = upstream.Close()
	}()
	defer func() { _ = client.Close(); _ = upstream.Close(); <-frontendDone }()
	var transactionStarted time.Time
	transactionHadWrites := false
	defer func() {
		if !transactionStarted.IsZero() {
			self.activeClosed.Add(1)
			if transactionHadWrites {
				self.activeWritesClosed.Add(1)
			}
		}
	}()
	connectionOrdinal := self.diagnosticOrdinal.Add(1)
	var backendPid uint32
	var diagnostic *legacyCohortProxyDiagnosticCapture
	var connectionDiagnostic *legacyCohortConnectionDiagnosticCapture
	var diagnosticFrames legacyCohortDiagnosticFrames
	var pendingDiagnostic legacyCohortReadyDiagnostic
	observingReady := false
	defer func() {
		if observingReady {
			if pendingDiagnostic.StopCause == "" {
				pendingDiagnostic.StopCause = "forwarding_stopped"
			}
			connectionDiagnostic.finishReady(pendingDiagnostic)
		}
		if connectionDiagnostic != nil {
			connectionDiagnostic.close()
		}
	}()
	sawSet := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(upstream, header[:]); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 16*1024*1024 {
			return
		}
		body := make([]byte, int(length)-4)
		if _, err := io.ReadFull(upstream, body); err != nil {
			return
		}

		capture := diagnostic
		if capture == nil {
			capture = self.diagnosticCapture.Load()
		}
		var readyReceived time.Time
		if capture != nil && header[0] == 'Z' {
			readyReceived = time.Now()
		}
		// Only the process id crosses into diagnostic state. The other
		// BackendKeyData bytes remain solely in this forwarded frame.
		if header[0] == 'K' && len(body) == 8 {
			backendPid = binary.BigEndian.Uint32(body[:4])
			if connectionDiagnostic != nil {
				connectionDiagnostic.setBackendPid(backendPid)
			}
		}
		if diagnostic == nil && capture != nil {
			diagnostic = capture
			connectionDiagnostic = diagnostic.connection(connectionOrdinal, backendPid)
		}
		enabled := self.enabled.Load()
		if diagnostic != nil {
			diagnosticFrames.observe(header[0], body, enabled)
			if header[0] == 'Z' {
				if enabled {
					value := diagnosticFrames.ready(readyReceived, body)
					if diagnostic.beginReady(connectionDiagnostic) {
						pendingDiagnostic, observingReady = value, true
					}
				} else {
					diagnosticFrames.resetCycle()
				}
			}
		}
		if enabled {
			self.backendBytes.Add(int64(len(header) + len(body)))
			if header[0] == 'E' {
				self.errorLock.Lock()
				self.errorClasses[parallelCloseWireErrorClass(body)]++
				self.errorLock.Unlock()
			}
			if header[0] == 'C' {
				switch {
				case bytes.Equal(body, []byte("BEGIN\x00")):
					self.begins.Add(1)
					transactionStarted = time.Now()
					transactionHadWrites = false
				case bytes.Equal(body, []byte("SET\x00")):
					sawSet = !transactionStarted.IsZero()
				case bytes.HasPrefix(body, []byte("INSERT ")), bytes.HasPrefix(body, []byte("UPDATE ")), bytes.HasPrefix(body, []byte("DELETE ")):
					if !transactionStarted.IsZero() {
						transactionHadWrites = true
					}
				case bytes.Equal(body, []byte("COMMIT\x00")), bytes.Equal(body, []byte("ROLLBACK\x00")):
					if !transactionStarted.IsZero() {
						elapsed := time.Since(transactionStarted).Nanoseconds()
						for previous := self.maxTxNs.Load(); elapsed > previous; previous = self.maxTxNs.Load() {
							if self.maxTxNs.CompareAndSwap(previous, elapsed) {
								break
							}
						}
						transactionStarted = time.Time{}
					}
					if bytes.Equal(body, []byte("COMMIT\x00")) {
						self.commits.Add(1)
						if transactionHadWrites {
							self.writeCommits.Add(1)
						}
					} else {
						self.rollbacks.Add(1)
						if transactionHadWrites {
							self.writeRollbacks.Add(1)
						}
					}
					transactionHadWrites = false
				}
				if observation := self.commandCompleteObservation.Load(); observation != nil {
					observation.observe(body)
				}
			}
			if header[0] == 'Z' {
				ready := self.ready.Add(1)
				if sawSet && self.holdSet.CompareAndSwap(true, false) {
					self.held.Add(1)
					select {
					case <-frontendDone:
						self.heldClosed.Add(1)
						if observingReady {
							pendingDiagnostic.StopCause = "held_frontend_closed"
						}
					case <-self.ctx.Done():
						if observingReady {
							pendingDiagnostic.StopCause = "held_context_done"
						}
					}
					return
				}
				sawSet = false
				delay := time.Duration(self.delayNs.Load())
				jitterBucket := -1
				if self.jitter.Load() {
					// The observed Ready ordinal fixes a repeating 1..5ms
					// sequence. Scheduling decides which connection receives it.
					jitterBucket = int((ready - 1) % 5)
					delay = time.Duration(jitterBucket+1) * time.Millisecond
					self.jitterCharged[jitterBucket].Add(1)
				}
				if observingReady {
					pendingDiagnostic.RequestedDelayNs = int64(delay)
				}
				if delay > 0 {
					self.delayed.Add(1)
					if observingReady {
						pendingDiagnostic.TimerStart = time.Now()
					}
					select {
					case <-time.After(delay):
						if observingReady {
							pendingDiagnostic.TimerDone = time.Now()
						}
						if jitterBucket >= 0 {
							self.jitterApplied[jitterBucket].Add(1)
						}
					case <-frontendDone:
						if observingReady {
							pendingDiagnostic.TimerDone = time.Now()
							pendingDiagnostic.StopCause = "timer_frontend_closed"
						}
						return
					case <-self.ctx.Done():
						if observingReady {
							pendingDiagnostic.TimerDone = time.Now()
							pendingDiagnostic.StopCause = "timer_context_done"
						}
						return
					}
				}
			}
		}
		if _, err := client.Write(header[:]); err != nil {
			if observingReady {
				pendingDiagnostic.ForwardDone = time.Now()
				pendingDiagnostic.StopCause = "header_write_error"
			}
			return
		}
		if _, err := client.Write(body); err != nil {
			if observingReady {
				pendingDiagnostic.ForwardDone = time.Now()
				pendingDiagnostic.StopCause = "body_write_error"
			}
			return
		}
		if observingReady {
			pendingDiagnostic.ForwardDone = time.Now()
			pendingDiagnostic.Forwarded = true
			connectionDiagnostic.finishReady(pendingDiagnostic)
			observingReady = false
		}
	}
}

// Actual protocol counts include only enabled work, including canceled replies.
func (self *legacyCohortLatencyProxy) snapshot() map[string]int64 {
	counters := map[string]int64{
		"begin_commands_observed": self.begins.Load(),
		"ready_replies_observed":  self.ready.Load(), "ready_replies_charged_delay": self.delayed.Load(),
		"frontend_bytes_read": self.frontendBytes.Load(), "complete_backend_frame_bytes_read": self.backendBytes.Load(),
		"commit_commands_observed": self.commits.Load(), "rollback_commands_observed": self.rollbacks.Load(),
		"transactions_with_writes_committed":                 self.writeCommits.Load(),
		"transactions_with_writes_rolled_back":               self.writeRollbacks.Load(),
		"connections_closed_with_active_writes":              self.activeWritesClosed.Load(),
		"connections_closed_after_begin_without_end_command": self.activeClosed.Load(),
		"max_begin_to_end_command_observed_ns":               self.maxTxNs.Load(),
		"held_financial_set_replies":                         self.held.Load(), "held_reply_client_closures": self.heldClosed.Load(),
	}
	self.errorLock.Lock()
	defer self.errorLock.Unlock()
	for _, class := range []string{"serialization_failure", "deadlock_detected", "lock_not_available", "query_canceled", "in_failed_transaction", "unique_violation", "foreign_key_violation", "other", "malformed"} {
		counters["error_response_"+class] = self.errorClasses[class]
	}
	for index, bucket := range []string{"1ms", "2ms", "3ms", "4ms", "5ms"} {
		counters["ready_jitter_charged_"+bucket] = self.jitterCharged[index].Load()
		counters["ready_jitter_applied_"+bucket] = self.jitterApplied[index].Load()
	}
	return counters
}

// Rebinding is confined to the disposable database's in-memory resource stack.
func legacyCohortLatencyBind(t testing.TB, ctx context.Context) (*legacyCohortLatencyProxy, func()) {
	t.Helper()
	resource := server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName)
	proxy := newLegacyCohortLatencyProxy(t, ctx, resource.RequireString("authority"))
	values := maps.Clone(resource.Parse())
	values["authority"] = proxy.listener.Addr().String()
	encoded, err := yaml.Marshal(values)
	server.Raise(err)
	popPg := server.Vault.PushSimpleResource(server.DefaultPgVaultResourceName, encoded)
	server.PgReset()
	return proxy, func() { proxy.close(); server.PgReset(); popPg() }
}

// JSON keeps this test overlay compilable on the original public-owner source.
type legacyCohortLatencyCounts struct {
	Attempts  int `json:"financial_cohort_attempts"`
	Selected  int `json:"financial_cohort_selected"`
	Completed int `json:"financial_cohort_completed"`
	Fallbacks int `json:"financial_cohort_fallbacks"`
}

func legacyCohortLatencyCount(page any) legacyCohortLatencyCounts {
	raw, err := json.Marshal(page)
	server.Raise(err)
	var out legacyCohortLatencyCounts
	server.Raise(json.Unmarshal(raw, &out))
	return out
}

// A real reply is held until the bounded cohort client cancels and closes it.
// The parent remains live. One rollback must disable further cohorts on this
// page; the ordinary owner still completes every exact funded outcome. There
// is no elapsed-time threshold, fake query failure or repair of money/reports.
func TestLegacyFinancialCohortDeadlineFallbackRunsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, newLegacyFinancialCohortCooldown())
		f := legacyFinancialCohortSeed(t, ctx, 8)
		proxy, cleanup := legacyCohortLatencyBind(t, ctx)
		defer cleanup()
		before := contractClosedCounter.Snapshot()
		proxy.holdSet.Store(true)
		proxy.enabled.Store(true)
		started := time.Now()
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		elapsed := time.Since(started)
		proxy.enabled.Store(false)
		counts := legacyCohortLatencyCount(page)
		after := contractClosedCounter.Snapshot()
		if err != nil || ctx.Err() != nil || page.Visited != 8 || page.Completed != 8 || page.Failed != 0 || page.BusyOrGone != 0 {
			t.Fatal("child deadline prevented the ordinary funded page", page, err, ctx.Err())
		}
		if proxy.held.Load() != 1 || proxy.heldClosed.Load() != 1 {
			t.Fatal("financial reply was not released by actual client cancellation", proxy.snapshot())
		}
		if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != 8 || after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
			t.Fatal("deadline rollback changed committed per-contract ownership", before, after)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		drain := legacyCohortLatencyDrain(t, ctx, 9)
		if drain.Finished != 9 {
			t.Fatal("deadline recovery changed exact durable output owners", drain)
		}
		if replay, err := FlushLegacySettlements(ctx, 1, nil, 8); err != nil || replay.Visited != 0 || replay.Completed != 0 {
			t.Fatal("deadline recovery replay repeated financial work", replay, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		raw, err := json.Marshal(map[string]any{"page": page, "wall_ns": elapsed.Nanoseconds(), "protocol": proxy.snapshot(),
			"qualifier": "The held SET precedes grant acquisition; this proves child-deadline rollback/fallback ownership, not a grant-residence limit."})
		server.Raise(err)
		t.Logf("legacy_cohort_deadline_fallback=%s", raw)
		if counts.Attempts != 1 || counts.Selected != 8 || counts.Completed != 0 || counts.Fallbacks != 1 {
			t.Fatal("one child deadline retried a cohort or hid fallback work", counts)
		}
	})
}

func TestLegacyFinancialCohortLatencyZero(t *testing.T) {
	legacyFinancialCohortLatencyLoad(t, 0)
}
func TestLegacyFinancialCohortLatencyFiveMilliseconds(t *testing.T) {
	legacyFinancialCohortLatencyLoad(t, 5*time.Millisecond)
}
func TestLegacyFinancialCohortLatencyTwentyMilliseconds(t *testing.T) {
	legacyFinancialCohortLatencyLoad(t, 20*time.Millisecond)
}
func TestLegacyFinancialCohortLatencyFortyMilliseconds(t *testing.T) {
	legacyFinancialCohortLatencyLoad(t, 40*time.Millisecond)
}

// A finite four-slot same-payer load retains the normal shard owner, scheduler
// cursor format and all financial/post/owner work. Both source arms receive the
// same per-reply model; speed is reported, never asserted from machine time.
func legacyFinancialCohortLatencyLoad(t *testing.T, delay time.Duration) {
	if os.Getenv("URN_LEGACY_COHORT_LATENCY_NATIVE") != "1" || os.Getenv("URN_LEGACY_REQUIRE_PGSS") != "1" {
		t.Skip("explicit isolated protocol latency profile and statement statistics required")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, newLegacyFinancialCohortCooldown())
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
		})
		const count, shards = 128, 4
		f := legacyFinancialCohortSeed(t, ctx, count, shards)
		hook := &legacyHotpathRedisCountHook{}
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.RedisDoOnce(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error { client.AddHook(hook); return nil }))
		measured := hook.context(ctx)
		proxy, cleanup := legacyCohortLatencyBind(t, ctx)
		defer cleanup()
		beforeSql := legacyTargetSqlSnapshot(t, ctx)
		beforeCounter := contractClosedCounter.Snapshot()
		proxy.delayNs.Store(delay.Nanoseconds())
		proxy.enabled.Store(true)
		started := time.Now()
		cursors := make([]*LegacySettlementCursor, shards)
		payerCursors := make([]*LegacySettlementPayerCursor, shards)
		type pageResult struct {
			shard int
			page  LegacySettlementShardResult
			err   error
		}
		var pages []LegacySettlementShardResult
		completed, visits, busy, rounds, noProgressWaves := 0, 0, 0, 0, 0
		cohorts := legacyCohortLatencyCounts{}
		for completed < count {
			previous := completed
			rounds++
			if rounds > count {
				t.Fatal("finite latency profile did not drain", rounds, completed)
			}
			results := make(chan pageResult, shards)
			var joined sync.WaitGroup
			for shard := range shards {
				joined.Add(1)
				go func() {
					defer joined.Done()
					value := pageResult{shard: shard}
					server.HandleError(func() {
						value.page, value.err = FlushLegacySettlementShard(measured, shard, cursors[shard], payerCursors[shard], 256)
					}, func(err error) { value.err = err })
					results <- value
				}()
			}
			joined.Wait()
			close(results)
			for value := range results {
				if value.err != nil || value.page.Failed != 0 {
					t.Fatal("loaded latency owner failed", value.shard, value.page, value.err)
				}
				counts := legacyCohortLatencyCount(value.page)
				if counts.Fallbacks > 1 || counts.Selected > 8*counts.Attempts {
					t.Fatal("bounded page repeated fallback or exceeded the fixed cohort ceiling", counts)
				}
				cohorts.Attempts += counts.Attempts
				cohorts.Selected += counts.Selected
				cohorts.Completed += counts.Completed
				cohorts.Fallbacks += counts.Fallbacks
				pages = append(pages, value.page)
				completed += value.page.Completed
				visits += value.page.Visited
				busy += value.page.BusyOrGone
				wire, err := json.Marshal(value.page.Cursor)
				server.Raise(err)
				server.Raise(json.Unmarshal(wire, &cursors[value.shard]))
				wire, err = json.Marshal(value.page.PayerCursor)
				server.Raise(err)
				server.Raise(json.Unmarshal(wire, &payerCursors[value.shard]))
			}
			if completed <= previous {
				// A continued cursor may first reach EOF before a fresh page
				// revisits skipped grants. Retain that work within the cap.
				noProgressWaves++
			}
		}
		financialElapsed := time.Since(started)
		financialProtocol := proxy.snapshot()
		if completed != count {
			t.Fatal("latency load escaped its fixed cohort", completed)
		}
		drain := legacyCohortLatencyDrain(t, measured, count+2)
		includingOwners := time.Since(started)
		protocol := proxy.snapshot()
		proxy.enabled.Store(false)
		afterSql := legacyTargetSqlSnapshot(t, ctx)
		afterCounter := contractClosedCounter.Snapshot()
		commands, dispatches := hook.snapshot()
		if !beforeCounter.Stable || !afterCounter.Stable || afterCounter.Confirmed-beforeCounter.Confirmed != count || afterCounter.Uncertain != beforeCounter.Uncertain || afterCounter.Untracked != beforeCounter.Untracked {
			t.Fatal("latency model changed acknowledged financial ownership", beforeCounter, afterCounter)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		for _, balanceId := range f.balances {
			if Testing_NetEscrowByteCount(ctx, balanceId) != 0 {
				t.Fatal("latency profile left reclaimed admission capacity unavailable")
			}
		}
		for shard := range shards {
			if page, err := FlushLegacySettlementShard(ctx, shard, nil, nil, 256); err != nil || page.Completed != 0 || page.Visited != 0 {
				t.Fatal("latency profile replay repeated financial work", page, err)
			}
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		out := map[string]any{
			"profile": os.Getenv("URN_LEGACY_FINANCIAL_COHORT_PROFILE"), "contracts": count, "workers": shards, "shards": shards,
			"payer_networks": 1, "shared_grants": 2, "providers": 4, "grant_distribution": "15:1 within each shard",
			"modeled_delay_per_ready_ns": delay.Nanoseconds(), "cohort_limit": 8, "cohort_child_deadline_ns": int64(legacyFinancialCohortTimeout),
			"rounds": rounds, "zero_completion_waves": noProgressWaves, "visits": visits, "completed": completed, "busy": busy, "pages": pages, "cohorts": cohorts,
			"financial_and_joined_page_wall_ns": financialElapsed.Nanoseconds(), "including_owner_drain_wall_ns": includingOwners.Nanoseconds(),
			"closes_per_second": float64(completed) / financialElapsed.Seconds(), "closes_per_second_including_owners": float64(completed) / includingOwners.Seconds(),
			"finished_owners": drain.Finished, "pending_owners_after_drain": 0, "owner_drain": drain, "financial_protocol": financialProtocol, "including_owners_protocol": protocol,
			"redis_commands": commands, "redis_client_dispatches": dispatches, "sql": legacyTargetSqlDelta(t, beforeSql, afterSql),
			"qualifiers": []string{
				"The pg.yml proxy applies the real reply model to startup/prepare/execute/transaction Ready replies while enabled; it is not measured Main network latency.",
				"A separately configured pg_maintenance.yml direct claim pool bypasses this proxy. Wall/PGSS include that traffic; protocol counters and modeled delay cover only pg.yml. Backend byte counts exclude incomplete frames interrupted by cancellation.",
				"Both source arms use the same fixed funded work, public owner, four slots and complete durable output drain; no production scheduler gaps or new arrivals are modeled.",
				"All cohort timeout/rollback/fallback work remains inside the wall/SQL/protocol/phase denominators; no machine-dependent throughput threshold is asserted.",
				"The reported cohort child context bounds acquire/body and is clamped by its real parent; commit acknowledgement uses the existing separate timeout and is not a lock-hold guarantee. The literal statement500ms and lock250ms limits remain unchanged.",
				"Proxy BEGIN-to-end command spans are client/protocol observations, not grant row-lock residence or query CPU; disconnected active transactions are counted separately.",
				"SQL totals retain separate nested work; Redis hooks cover all three pools but dispatches are not network round trips.",
				"This finite sensitivity is not sustained fleet throughput, all-close workload mix, or proof of the overall fivefold target.",
			},
		}
		raw, err := json.Marshal(out)
		server.Raise(err)
		t.Logf("legacy_financial_cohort_latency=%s", raw)
	})
}

// Share the same due-aware real worker drain as the matched loaded fixture.
func legacyCohortLatencyDrain(t testing.TB, ctx context.Context, expected int) legacyFinancialDrainResult {
	t.Helper()
	result, err := legacyFinancialDrainOwners(t, ctx, expected, nil)
	if err != nil {
		t.Fatal("latency profile durable output failed", result, err)
	}
	return result
}
