package perf

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The steady phase: every workload at its production shape at once, the
// session maintenance task at its 30 second cadence, and a time series of the
// cluster, the index outbox and the hot network's live sessions.
func (self *harness) runSteadyPhase() {
	profile := self.config.profile
	rates := profile.rates
	working := self.checkWorkingSet()
	large := self.ensureLarge()

	hot := self.newNetwork("hot")
	code := self.newNetwork("code")
	codeCreator, err := self.signIn(code)
	self.must(err, "the code fleet's sign-in")

	all := append(append([]*credential{}, working.clients...), working.signIns...)
	authClients := &clientQueue{}
	children := &clientQueue{}
	proxies := &clientQueue{}
	// sign-ins the steady phase minted, by network, for single revocations
	var revocableLock sync.Mutex
	revocable := map[server.Id][]*credential{}
	var networkCursor atomic.Int64
	nextNetwork := func() *workingNetwork {
		return working.networks[int(networkCursor.Add(1))%len(working.networks)]
	}
	// the distinct networks with a registered mint in each 30 s maintenance
	// interval: each leaves an index outbox row that one repair must drain
	minting := newMintTracker(30 * time.Second)
	hotRate := productionRates["hotproxy"] * profile.compression
	codeRate := productionRates["code"] * profile.compression * 5

	loads := []*load{
		{name: "authclient", rate: rates["authclient"], workers: 32, op: func(ctx context.Context) {
			network := nextNetwork()
			apiKey := rand.IntN(2) == 0
			var clientSession *session.ClientSession
			op := "steady.authclient.jwt"
			if apiKey {
				clientSession = self.apiKeySession(network.network)
				op = "steady.authclient.apikey"
			} else {
				clientSession = self.credentialSession(network.signIn)
			}
			defer clientSession.Cancel()
			start := time.Now()
			_, clientId, _, err := self.authClient(clientSession, false)
			self.ops.observe(op, start, err)
			if err == nil {
				minting.note(network.network.id)
				authClients.push(queuedClient{created: time.Now(), network: network.network, clientId: clientId})
			}
		}},
		{name: "authclient.remove", rate: rates["authclient"], workers: 32, op: func(ctx context.Context) {
			entry, ok := authClients.popDue(profile.clientLifetime)
			if !ok {
				return
			}
			remover := self.apiKeySession(entry.network)
			defer remover.Cancel()
			start := time.Now()
			err := self.removeClient(remover, entry.clientId)
			self.ops.observe("steady.remove.client", start, err)
		}},
		{name: "child", rate: rates["child"], workers: 32, op: func(ctx context.Context) {
			network := nextNetwork()
			if len(network.apiKeyClients) == 0 {
				return
			}
			parent := network.apiKeyClients[rand.IntN(len(network.apiKeyClients))]
			start := time.Now()
			_, clientId, err := self.childMint(parent)
			self.ops.observe("steady.mint.child", start, err)
			if err == nil {
				minting.note(network.network.id)
				children.push(queuedClient{created: time.Now(), network: network.network, clientId: clientId, remover: self.credentialSession(parent)})
			}
		}},
		{name: "child.remove", rate: rates["child"], workers: 32, op: func(ctx context.Context) {
			entry, ok := children.popDue(profile.clientLifetime)
			if !ok {
				return
			}
			defer entry.remover.Cancel()
			start := time.Now()
			err := self.removeClient(entry.remover, entry.clientId)
			self.ops.observe("steady.remove.child", start, err)
		}},
		{name: "refresh", rate: rates["refresh"], workers: 4, op: func(ctx context.Context) {
			network := nextNetwork()
			start := time.Now()
			_, err := self.refresh(network.signIn, network.network)
			self.ops.observe("steady.refresh", start, err)
			if err == nil {
				minting.note(network.network.id)
			}
		}},
		{name: "signin", rate: rates["signin"], workers: 4, op: func(ctx context.Context) {
			network := nextNetwork()
			start := time.Now()
			credential, err := self.signIn(network.network)
			self.ops.observe("steady.mint.signin", start, err)
			if err == nil {
				minting.note(network.network.id)
				revocableLock.Lock()
				revocable[network.network.id] = append(revocable[network.network.id], credential)
				revocableLock.Unlock()
			}
		}},
		{name: "hotproxy.create", rate: hotRate, workers: 8, op: func(ctx context.Context) {
			clientSession := self.apiKeySession(hot)
			defer clientSession.Cancel()
			start := time.Now()
			credential, clientId, _, err := self.authClient(clientSession, true)
			self.ops.observe("steady.hotproxy.create", start, err)
			if err != nil {
				return
			}
			// the proxy device opens its client
			start = time.Now()
			_, err = self.hostedMint(hot, *credential.claims.DeviceId, clientId)
			self.ops.observe("steady.hotproxy.device_open", start, err)
			minting.note(hot.id)
			proxies.push(queuedClient{created: time.Now(), network: hot, clientId: clientId})
		}},
		{name: "hotproxy.remove", rate: hotRate, workers: 8, op: func(ctx context.Context) {
			entry, ok := proxies.popDue(profile.proxyLifetime)
			if !ok {
				return
			}
			remover := self.apiKeySession(hot)
			defer remover.Cancel()
			start := time.Now()
			err := self.removeClient(remover, entry.clientId)
			self.ops.observe("steady.hotproxy.remove", start, err)
		}},
		{name: "code", rate: codeRate, workers: 2, op: func(ctx context.Context) {
			start := time.Now()
			authCode, err := self.codeCreate(codeCreator)
			self.ops.observe("steady.code.create", start, err)
			if err != nil {
				return
			}
			start = time.Now()
			_, err = self.codeLogin(authCode)
			self.ops.observe("steady.code.login", start, err)
			if err == nil {
				minting.note(code.id)
			}
		}},
		{name: "api", rate: rates["api"], workers: 64, op: func(ctx context.Context) {
			credential := all[rand.IntN(len(all))]
			start := time.Now()
			err := self.apiAuth(credential.jwt)
			self.ops.observe("steady.api.auth", start, err)
		}},
		{name: "lease", rate: rates["lease"], workers: 64, op: func(ctx context.Context) {
			credential := working.clients[rand.IntN(len(working.clients))]
			start := time.Now()
			err := self.leaseCheck(credential.claims)
			self.ops.observe("steady.lease.check", start, err)
		}},
		{name: "hint", rate: rates["hint"], workers: 16, op: func(ctx context.Context) {
			network := working.networks[rand.IntN(len(working.networks))]
			start := time.Now()
			_, _, err := session.NetworkSessionRevision(ctx, network.network.id)
			self.ops.observe("steady.hint.small", start, err)
		}},
		// connect's hint reads of one large network: each of ~20 connect
		// processes at least once a minute, and once per 5 s while its
		// revision moves
		{name: "hint.large", rate: 4, workers: 4, op: func(ctx context.Context) {
			start := time.Now()
			_, _, err := session.NetworkSessionRevision(ctx, large.network.id)
			self.ops.observe("steady.hint.large", start, err)
		}},
		{name: "list", rate: rates["list"], workers: 2, op: func(ctx context.Context) {
			actor := self.credentialSession(large.actor)
			defer actor.Cancel()
			start := time.Now()
			_, err := session.GetNetworkSessions(actor)
			self.ops.observe("steady.list.large", start, err)
		}},
		{name: "revoke", rate: rates["revoke"], workers: 2, op: func(ctx context.Context) {
			var target *credential
			var owner *workingNetwork
			revocableLock.Lock()
			for _, network := range working.networks {
				if list := revocable[network.network.id]; 0 < len(list) {
					target, owner = list[0], network
					revocable[network.network.id] = list[1:]
					break
				}
			}
			revocableLock.Unlock()
			if target == nil {
				return
			}
			actor := self.credentialSession(owner.signIn)
			defer actor.Cancel()
			start := time.Now()
			_, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *target.claims.SessionId, OperationId: server.NewId()}, actor)
			self.ops.observe("steady.revoke.single", start, err)
		}},
		{name: "maintenance", rate: 1.0 / 30, workers: 1, op: func(ctx context.Context) {
			self.runMaintenance("steady")
		}},
		// a bare Redis round trip on the harness's own pool, beside the
		// measured paths: what the network path and Redis add under this load
		{name: "redis.rtt", rate: 20, workers: 2, op: func(ctx context.Context) {
			credential := all[rand.IntN(len(all))]
			start := time.Now()
			err := self.admin.client.HMGet(ctx, self.recordKey(credential.claims), "n:user").Err()
			self.ops.observe("steady.redis.rtt", start, err)
		}},
	}

	steady := &steadyReport{
		DurationSeconds: profile.steadyDuration.Seconds(),
		HotRate:         hotRate,
		CodeRate:        codeRate,
		LargeSessions:   int(self.liveSessionCount(large.network.id)),
	}
	steady.OutboxStart = self.outboxRows()
	before, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	running := startRecordedLoads(self.ctx, loads, self.ops)
	previousProcess := processCpuSeconds()
	seriesDone := make(chan struct{})
	seriesStopped := make(chan struct{})
	go func() {
		defer close(seriesStopped)
		previous := before
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-seriesDone:
				return
			case <-ticker.C:
			}
			snapshot, err := self.admin.snapshot(self.ctx)
			if err != nil {
				continue
			}
			cost := self.costSince(snapshot, previous)
			previous = snapshot
			processNow := processCpuSeconds()
			harnessCores := (processNow - previousProcess) / cost.Wall
			previousProcess = processNow
			containers := containerCores()
			point := &seriesPoint{
				Seconds:      snapshot.at.Sub(before.at).Seconds(),
				RedisCores:   cost.CpuSeconds / cost.Wall,
				MaxNodeCores: cost.MaxNodeCpu / cost.Wall,
				UsedMemory:   cost.UsedMemory,
				Commands:     float64(cost.TotalCommands) / cost.Wall,
				Outbox:       self.outboxRows(),
				HotLive:      self.liveSessionCount(hot.id),
				LargeLive:    self.liveSessionCount(large.network.id),
				Connections:  float64(cost.Connections) / cost.Wall,
				HarnessCores: harnessCores,
				PgCores:      containers["urnetwork-local-pg"],
				RedisDocker:  containers["urnetwork-session-perf-redis"],
			}
			steady.Series = append(steady.Series, point)
			self.logf("steady %3.0fs: Redis %.2f cores (max node %.2f), %.0f commands/s, %.1f MB, outbox %d, hot network %d live; harness %.2f cores, PostgreSQL container %.2f, Redis container %.2f", point.Seconds, point.RedisCores, point.MaxNodeCores, point.Commands, float64(point.UsedMemory)/1e6, point.Outbox, point.HotLive, point.HarnessCores, point.PgCores, point.RedisDocker)
		}
	}()
	time.Sleep(profile.steadyDuration)
	close(seriesDone)
	<-seriesStopped
	steady.Loads = running.finish()
	after, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	steady.Cost = self.costSince(after, before)
	steady.OutboxEnd = self.outboxRows()
	steady.HotLiveEnd = self.liveSessionCount(hot.id)
	steady.HotHeldEnd = self.heldCount(hot.id)
	steady.PendingRemovals = authClients.size() + children.size() + proxies.size()
	steady.MintingNetworksPerInterval = minting.counts()
	self.report.Steady = steady
	self.logf("steady loads: %s", describeLoads(steady.Loads))
}

// The session maintenance task's work for every shard, with its budgets:
// five seconds for a shard, one second for each stage of shard zero and 100
// ms for the backlog gauges (taskworker/work/session_work.go).
func (self *harness) runMaintenance(prefix string) {
	for shard := 0; shard < session.SessionIndexShards; shard++ {
		ctx, cancel := context.WithTimeout(self.ctx, 5*time.Second)
		stage := func(name string, budget time.Duration, run func(ctx context.Context) error) {
			stageCtx, stageCancel := context.WithTimeout(ctx, budget)
			defer stageCancel()
			start := time.Now()
			err := run(stageCtx)
			self.ops.observe(prefix+".maintenance."+name, start, err)
		}
		if shard != 0 {
			stage("sweep", 5*time.Second, func(ctx context.Context) error {
				_, err := session.SweepSessionIndexShard(ctx, shard, 32, server.NowUtc())
				return err
			})
			cancel()
			continue
		}
		stage("sweep0", time.Second, func(ctx context.Context) error {
			_, err := session.SweepSessionIndexShard(ctx, 0, 32, server.NowUtc())
			return err
		})
		stage("receipts", time.Second, func(ctx context.Context) error {
			return session.ExpireSessionReceipts(ctx, server.NowUtc(), 128)
		})
		stage("repair", time.Second, func(ctx context.Context) error {
			return session.RepairSessionIndexes(ctx, 64)
		})
		stage("recover", time.Second, func(ctx context.Context) error {
			return model.RecoverSessionOperations(ctx, 32)
		})
		stage("observe", 100*time.Millisecond, func(ctx context.Context) error {
			return session.ObserveSessionMaintenance(ctx, server.NowUtc())
		})
		cancel()
	}
}

func (self *harness) outboxRows() int64 {
	var count int64
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT count(*) FROM network_session_index_outbox`).Scan(&count))
	})
	return count
}

// The index repair at outbox backlogs: one shard-zero repair (64 rows) and
// the backlog gauges, each under its production budget.
func (self *harness) runOutboxPhase() {
	for _, rows := range self.config.profile.outboxSeeds {
		existing := self.outboxRows()
		if existing < int64(rows) {
			self.seedOutbox(int(int64(rows) - existing))
		}
		report := &outboxReport{Rows: int(self.outboxRows())}
		for range 3 {
			ctx, cancel := context.WithTimeout(self.ctx, time.Second)
			start := time.Now()
			err := session.RepairSessionIndexes(ctx, 64)
			cancel()
			report.RepairMs = append(report.RepairMs, float64(time.Since(start).Microseconds())/1000)
			self.ops.observe(fmt.Sprintf("outbox.repair@%d", rows), start, err)
			if err != nil {
				report.RepairError = classify(err)
			}
		}
		ctx, cancel := context.WithTimeout(self.ctx, 100*time.Millisecond)
		start := time.Now()
		err := session.ObserveSessionMaintenance(ctx, server.NowUtc())
		cancel()
		report.ObserveMs = float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			report.ObserveError = classify(err)
		}
		self.ops.observe(fmt.Sprintf("outbox.observe@%d", rows), start, err)
		report.RowsAfter = int(self.outboxRows())
		self.report.Outbox = append(self.report.Outbox, report)
		self.logf("outbox at %d rows: repair %v ms (%s), gauges %.1f ms (%s), %d rows after", report.Rows, report.RepairMs, report.RepairError, report.ObserveMs, report.ObserveError, report.RowsAfter)
	}
}

// Outbox rows as distinct minting networks leave them, one row a network.
func (self *harness) seedOutbox(rows int) {
	now := server.NowUtc()
	for start := 0; start < rows; start += 5000 {
		count := min(5000, rows-start)
		networks := make([]server.Id, count)
		revisions := make([]server.Id, count)
		dues := make([]time.Time, count)
		updates := make([]time.Time, count)
		for index := range count {
			networks[index] = server.NewId()
			revisions[index] = server.NewId()
			dues[index] = now.Add(90 * 24 * time.Hour)
			updates[index] = now.Add(-time.Duration(rows-start-index) * time.Millisecond)
		}
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO network_session_index_outbox(network_id,revision,next_review_time,update_time) SELECT * FROM unnest($1::uuid[],$2::uuid[],$3::timestamp[],$4::timestamp[]) ON CONFLICT(network_id) DO NOTHING`, networks, revisions, dues, updates))
		})
	}
}

// The 30-day idle reap (taskworker RemoveDisconnectedNetworkClients) of a
// network whose hosted clients each own a session, with the task's own
// windows. R-free expects the reap to free their sessions; the reap bypasses
// the model's removal functions.
func (self *harness) runReapPhase() {
	reap := self.largeNetwork("reap", liveCapProfile)
	runParallel(len(reap.clients), 16, func(index int) {
		self.leaseCheck(reap.clients[index].claims)
	})
	liveBefore := self.liveSessionCount(reap.network.id)
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_client SET auth_time=$2 WHERE network_id=$1 AND source_client_id IS NULL`, reap.network.id, server.NowUtc().Add(-model.TopLevelClientIdleExpiration-24*time.Hour)))
	})
	self.measure("reap.idle", len(reap.clients), func() {
		now := server.NowUtc()
		start := time.Now()
		model.RemoveDisconnectedNetworkClients(self.ctx, now.Add(-8*time.Hour), now.Add(-model.NetworkClientReapAfterDeactivate), now.Add(-model.TopLevelClientIdleExpiration))
		self.ops.observe("reap.idle", start, nil)
	})
	liveAfter := self.liveSessionCount(reap.network.id)
	var inactive int64
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT count(*) FROM network_client WHERE network_id=$1 AND NOT active`, reap.network.id).Scan(&inactive))
	})
	freed := liveBefore - liveAfter
	self.report.addOutcome(fmt.Sprintf("clients deactivated by the idle reap of %d", len(reap.clients)), "deactivated", int(inactive), len(reap.clients)-int(inactive))
	self.report.addOutcome("sessions freed by the idle reap (R-free expects all)", "freed", int(freed), int(inactive-freed))
	refused, accepted := self.probe(reap.clients, 500)
	self.report.addOutcome("idle-reaped clients' credentials at their next check", "refused", refused, accepted)
	self.logf("idle reap: %d of %d clients deactivated, %d sessions freed (%d live before, %d after)", inactive, len(reap.clients), freed, liveBefore, liveAfter)
}

// Distinct networks with a registered mint, per interval since the start.
type mintTracker struct {
	lock     sync.Mutex
	start    time.Time
	interval time.Duration
	windows  map[int]map[server.Id]bool
}

func newMintTracker(interval time.Duration) *mintTracker {
	return &mintTracker{start: time.Now(), interval: interval, windows: map[int]map[server.Id]bool{}}
}

func (self *mintTracker) note(networkId server.Id) {
	self.lock.Lock()
	defer self.lock.Unlock()
	window := int(time.Since(self.start) / self.interval)
	if self.windows[window] == nil {
		self.windows[window] = map[server.Id]bool{}
	}
	self.windows[window][networkId] = true
}

// Every complete interval's count.
func (self *mintTracker) counts() []int {
	self.lock.Lock()
	defer self.lock.Unlock()
	complete := int(time.Since(self.start) / self.interval)
	counts := []int{}
	for window := 0; window < complete; window++ {
		counts = append(counts, len(self.windows[window]))
	}
	return counts
}

// This process's CPU seconds, user and system.
func processCpuSeconds() float64 {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}

// Cores each local container uses now, by name, from docker stats. Best
// effort: an empty map when docker does not answer.
func containerCores() map[string]float64 {
	cores := map[string]float64{}
	output, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}").Output()
	if err != nil {
		return cores
	}
	for _, line := range strings.Split(string(output), "\n") {
		name, percent, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSuffix(percent, "%"), 64)
		if err == nil {
			cores[name] = value / 100
		}
	}
	return cores
}
