package perf

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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
		}},
		{name: "signin", rate: rates["signin"], workers: 4, op: func(ctx context.Context) {
			network := nextNetwork()
			start := time.Now()
			credential, err := self.signIn(network.network)
			self.ops.observe("steady.mint.signin", start, err)
			if err == nil {
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
	running := startLoads(self.ctx, loads)
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
			}
			steady.Series = append(steady.Series, point)
			self.logf("steady %3.0fs: Redis %.2f cores (max node %.2f), %.0f commands/s, %.1f MB, outbox %d, hot network %d live", point.Seconds, point.RedisCores, point.MaxNodeCores, point.Commands, float64(point.UsedMemory)/1e6, point.Outbox, point.HotLive)
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
