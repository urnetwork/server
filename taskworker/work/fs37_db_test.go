//go:build sidecar_pending

package work

// FS-37 (§22.8, FI-2; §18.8's lease row, D-SDK-17; SDK-M4-08) on T3: the T2 schedules' clauses
// (sidecar/sidecartest/fault/fs37_test.go) on server's own lease, reap, close and idle sweep, in a
// private database, on one fake clock with no sleeps. A page's poll is pushservice's real long-poll
// over sidecartest's Redis on that clock, so its marker {scp_<C>}hb lives 300 s of fake time, and its
// LeaseTouch is the model's own lease write at the fake time (model.Testing_TouchClientLeaseAt); each
// reaper pass is model.ReapEphemeralClients at the fake time, a minute apart; the beacon is the
// model's CloseEphemeralClient, which POST /sidecar/close calls. A reaped client is inactive, which is
// what the router's state check refuses its token on (PROTO-M1-10's sidecar_sync_route_test.go shows
// the 401 itself). FI-2's lease clauses are checked after every pass, as on T2.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const (
	fs37ReapInterval = 60 * time.Second
	fs37PollInterval = 25 * time.Second
)

// One FS-37 world on T3: a network, the fake clock, the long-poll on its Redis, and what the
// schedule saw.
type fs37DbWorld struct {
	t           testing.TB
	ctx         context.Context
	networkId   server.Id
	userSession *session.ClientSession
	clock       *sidecartest.FakeClock
	longPoll    *pushservice.LongPoll
	settings    *model.NetworkClientLeaseSettings
	start       time.Time
	nextReap    time.Time
	// each client's last poll, and each reap pass's reaps with their times
	lastPolls map[server.Id]time.Time
	reapTimes map[server.Id][]time.Time
}

func newFs37DbWorld(t testing.TB, ctx context.Context) *fs37DbWorld {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "fs37"+networkId.String()[:8], userId)
	userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: "fs37"})
	// the clock starts after the setup's real writes, so no row the schedule makes is from its future
	clock := sidecartest.NewFakeClock(server.NowUtc().Add(time.Second))
	redis := sidecartest.NewFakeRedis(ctx, clock, nil, nil, nil)
	t.Cleanup(redis.Close)
	world := &fs37DbWorld{
		t:           t,
		ctx:         ctx,
		networkId:   networkId,
		userSession: userSession,
		clock:       clock,
		settings:    model.DefaultNetworkClientLeaseSettings(),
		start:       clock.Now(),
		nextReap:    clock.Now().Add(fs37ReapInterval),
		lastPolls:   map[server.Id]time.Time{},
		reapTimes:   map[server.Id][]time.Time{},
	}
	world.settings.ReapPassTimeout = time.Minute
	longPoll, err := pushservice.NewLongPoll(ctx, redis, clock, nil, func(ctx context.Context, clientId connect.Id) error {
		return model.Testing_TouchClientLeaseAt(ctx, server.Id(clientId), clock.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(longPoll.Close)
	world.longPoll = longPoll
	return world
}

// A new top-level client of the network, by the real mint, and for an ephemeral one its lease at
// the clock's now with the close capability.
func (self *fs37DbWorld) addClient(ephemeral bool) (server.Id, string) {
	self.t.Helper()
	result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "page", DeviceSpec: "fs37"}, self.userSession)
	if err != nil || result.Error != nil || result.ClientId == nil {
		self.t.Fatalf("mint: %v %+v", err, result)
	}
	clientId := *result.ClientId
	capability := ""
	if ephemeral {
		server.Tx(self.ctx, func(tx server.PgTx) {
			capability = model.InsertClientLeaseInTx(self.ctx, tx, clientId, self.clock.Now())
		})
	}
	return clientId, capability
}

// One poll of the client on the long-poll, its marker and lease write on the fake clock. A poll with
// an empty cursor answers at once, so the schedule's clock stands for each hold.
func (self *fs37DbWorld) poll(clientId server.Id) {
	self.t.Helper()
	self.lastPolls[clientId] = self.clock.Now()
	if _, err := self.longPoll.Poll(self.ctx, connect.Id(clientId), nil); err != nil {
		self.t.Fatal(err)
	}
}

// Moves the clock to at, running a reaper pass at each minute on the way and FI-2's lease clauses
// after each.
func (self *fs37DbWorld) advanceTo(at time.Time) {
	self.t.Helper()
	for !self.nextReap.After(at) {
		self.clock.AdvanceTo(self.nextReap)
		before := map[server.Id]bool{}
		for clientId := range self.lastPolls {
			before[clientId] = self.active(clientId)
		}
		var after *model.ReapEphemeralClientsCursor
		for range 100 {
			result, err := model.ReapEphemeralClients(self.ctx, self.clock.Now(), after, self.settings)
			if err != nil {
				self.t.Fatal(err)
			}
			if result.Cursor == nil {
				break
			}
			after = result.Cursor
		}
		for clientId, wasActive := range before {
			if wasActive && !self.active(clientId) {
				self.reapTimes[clientId] = append(self.reapTimes[clientId], self.clock.Now())
			}
		}
		self.nextReap = self.nextReap.Add(fs37ReapInterval)
		self.requireOutcomes()
	}
	self.clock.AdvanceTo(at)
}

// Polls each client every 25 s until the clock reaches at.
func (self *fs37DbWorld) pollUntil(at time.Time, clientIds ...server.Id) {
	self.t.Helper()
	for self.clock.Now().Before(at) {
		for _, clientId := range clientIds {
			self.poll(clientId)
		}
		self.advanceTo(self.clock.Now().Add(fs37PollInterval))
	}
}

func (self *fs37DbWorld) active(clientId server.Id) (active bool) {
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&active))
			}
		})
	})
	return
}

// The client's lease seen_time, false for none.
func (self *fs37DbWorld) leaseSeenTime(clientId server.Id) (seenTime time.Time, found bool) {
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT seen_time FROM network_client_lease WHERE client_id = $1`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&seenTime))
				found = true
			}
		})
	})
	return
}

// FI-2's lease clauses after a pass: a client is reaped at most once, never within 30 min of its
// last poll, and stays reaped.
func (self *fs37DbWorld) requireOutcomes() {
	self.t.Helper()
	for clientId, times := range self.reapTimes {
		if 1 < len(times) {
			self.t.Fatalf("FI-2: %s was reaped twice", clientId)
		}
		if after := times[0].Sub(self.lastPolls[clientId]); after <= 30*time.Minute {
			self.t.Fatalf("FI-2: %s was reaped %s after its last poll", clientId, after)
		}
		if self.active(clientId) {
			self.t.Fatalf("FI-2: %s came back after its reap", clientId)
		}
	}
}

// The client's one reap, 30-36 min after its last poll, its lease gone with it.
func (self *fs37DbWorld) requireReapedAfterItsLastPoll(clientId server.Id) {
	self.t.Helper()
	times := self.reapTimes[clientId]
	if len(times) != 1 {
		self.t.Fatalf("%s's reaps: %v", clientId, times)
	}
	after := times[0].Sub(self.lastPolls[clientId])
	if after <= 30*time.Minute || 36*time.Minute < after {
		self.t.Fatalf("%s was reaped %s after its last poll, not 30-36 min", clientId, after)
	}
	if _, found := self.leaseSeenTime(clientId); found {
		self.t.Fatalf("%s's lease outlived its reap", clientId)
	}
}

// A connection of the client opened at connectTime, closed at disconnectTime unless that is zero.
func (self *fs37DbWorld) connection(clientId server.Id, connectTime time.Time) (disconnect func(at time.Time)) {
	self.t.Helper()
	handlerId := model.CreateNetworkClientHandler(self.ctx)
	connectionId, _, _, _, err := model.ConnectNetworkClient(self.ctx, clientId, "192.0.2.7:1", handlerId)
	if err != nil {
		self.t.Fatal(err)
	}
	set := func(connected bool, disconnectTime *time.Time) {
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
					UPDATE network_client_connection
					SET connect_time = $2, connected = $3, disconnect_time = $4
					WHERE connection_id = $1
				`,
				connectionId,
				connectTime,
				connected,
				disconnectTime,
			))
		})
	}
	set(true, nil)
	return func(at time.Time) {
		set(false, &at)
	}
}

func TestFs37PageKilledWithNoUnloadReapsOnceOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newFs37DbWorld(t, ctx)
		page, _ := world.addClient(true)
		world.pollUntil(world.start.Add(2*time.Hour), page)
		if !world.active(page) {
			t.Fatal("a polling page was reaped")
		}
		stats := world.longPoll.Stats()
		if stats.LeaseTouchCount < 23 || 25 < stats.LeaseTouchCount || stats.LeaseTouchFailedCount != 0 {
			t.Fatalf("2 h of polls wrote the lease %d times, %d failed", stats.LeaseTouchCount, stats.LeaseTouchFailedCount)
		}

		// killed with no unload: no more polls
		lastPoll := world.lastPolls[page]
		world.advanceTo(lastPoll.Add(40 * time.Minute))
		world.requireReapedAfterItsLastPoll(page)
	})
}

func TestFs37CloseBeaconReapsAtOnceOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newFs37DbWorld(t, ctx)
		page, capability := world.addClient(true)
		other, _ := world.addClient(true)
		world.pollUntil(world.start.Add(10*time.Minute), page, other)

		reaped, err := model.CloseEphemeralClient(ctx, capability)
		if err != nil || !reaped || world.active(page) {
			t.Fatalf("the beacon: %v %v", reaped, err)
		}
		if _, found := world.leaseSeenTime(page); found {
			t.Fatal("the closed page's lease stayed")
		}
		// a retry, a wrong capability and a malformed one each do nothing
		for _, body := range []string{capability, "AAAAAAAAAAAAAAAAAAAAAA", "not a capability"} {
			if reaped, err := model.CloseEphemeralClient(ctx, body); err != nil || reaped {
				t.Fatalf("a beacon of %q reaped: %v %v", body, reaped, err)
			}
		}
		if !world.active(other) {
			t.Fatal("the other page was reaped")
		}
		// the closed client's polls write nothing
		world.poll(page)
		if _, found := world.leaseSeenTime(page); found {
			t.Fatal("a closed client's poll wrote a lease")
		}
		world.advanceTo(world.clock.Now().Add(5 * time.Minute))
	})
}

func TestFs37OldGenerationPollsKeepNothingAliveOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newFs37DbWorld(t, ctx)
		first, _ := world.addClient(true)
		world.pollUntil(world.start.Add(30*time.Minute), first)
		// suspended: reaped
		world.advanceTo(world.lastPolls[first].Add(40 * time.Minute))
		world.requireReapedAfterItsLastPoll(first)

		// its replacement polls, then stops, while the old generation polls on
		replacement, _ := world.addClient(true)
		world.pollUntil(world.clock.Now().Add(20*time.Minute), replacement)
		replacementLastPoll := world.lastPolls[replacement]
		for world.clock.Now().Before(replacementLastPoll.Add(40 * time.Minute)) {
			seen, ok := world.leaseSeenTime(replacement)
			// the old generation's poll is past the router's refusal here: even so it writes nothing
			if _, err := world.longPoll.Poll(ctx, connect.Id(first), nil); err != nil {
				t.Fatal(err)
			}
			if _, found := world.leaseSeenTime(first); found {
				t.Fatal("an old generation's poll wrote a lease")
			}
			if after, afterOk := world.leaseSeenTime(replacement); afterOk && (!ok || !after.Equal(seen)) {
				t.Fatal("an old generation's poll moved the replacement's lease")
			}
			world.advanceTo(world.clock.Now().Add(fs37PollInterval))
		}
		world.requireReapedAfterItsLastPoll(replacement)
		if world.active(first) || len(world.reapTimes[first]) != 1 {
			t.Fatal("the old generation came back")
		}
	})
}

func TestFs37TwoTabsReapIndependentlyOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newFs37DbWorld(t, ctx)
		first, _ := world.addClient(true)
		second, _ := world.addClient(true)
		world.pollUntil(world.start.Add(2*time.Hour), first, second)
		world.pollUntil(world.lastPolls[first].Add(time.Hour), second)
		world.requireReapedAfterItsLastPoll(first)
		if !world.active(second) {
			t.Fatal("the polling tab was reaped")
		}
		world.advanceTo(world.lastPolls[second].Add(40 * time.Minute))
		world.requireReapedAfterItsLastPoll(second)
	})
}

func TestFs37ConnectedEphemeralSurvivesAShortDisconnectOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newFs37DbWorld(t, ctx)
		agent, _ := world.addClient(true)
		world.lastPolls[agent] = world.start
		disconnect := world.connection(agent, world.start)
		world.advanceTo(world.start.Add(2 * time.Hour))
		if !world.active(agent) {
			t.Fatal("a connected client was reaped")
		}
		disconnectTime := world.clock.Now()
		disconnect(disconnectTime)
		world.advanceTo(disconnectTime.Add(2 * time.Minute))
		if !world.active(agent) {
			t.Fatal("a client 2 min after its disconnect was reaped")
		}
		world.advanceTo(disconnectTime.Add(37 * time.Minute))
		times := world.reapTimes[agent]
		if len(times) != 1 {
			t.Fatalf("reaps %v", times)
		}
		if after := times[0].Sub(disconnectTime); after <= 35*time.Minute || 36*time.Minute < after {
			t.Fatalf("reaped %s after its disconnect, not 35-36 min", after)
		}
	})
}

// plan-SDK-http-only-idle-reap's case (QD-224): a member page Worker, a lease-less HTTPS-only client,
// refreshing every 3 days for 31 days stays active through the daily idle sweep, since a strict_exp
// refresh moves auth_time; its twin, a page that only polls, is marked inactive 30 days on.
func TestFs37PageWorkerRefreshingForThirtyOneDaysStaysActiveOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		model.Testing_ResetClientAuthTimeGate()
		defer model.Testing_ResetClientAuthTimeGate()
		world := newFs37DbWorld(t, ctx)
		worker, _ := world.addClient(false)
		polling, _ := world.addClient(false)
		day := 24 * time.Hour
		for elapsed := day; elapsed <= 31*day; elapsed += day {
			world.clock.AdvanceTo(world.start.Add(elapsed))
			now := world.clock.Now()
			world.poll(worker)
			world.poll(polling)
			if (elapsed/day)%3 == 0 {
				if err := model.RefreshStrictExpClientAuthTime(ctx, worker, now); err != nil {
					t.Fatal(err)
				}
			}
			model.RemoveDisconnectedNetworkClients(ctx, now.Add(-8*time.Hour), now.Add(-model.NetworkClientReapAfterDeactivate), now.Add(-model.TopLevelClientIdleExpiration))
			if !world.active(worker) {
				t.Fatalf("the refreshing page Worker was marked inactive at day %d", elapsed/day)
			}
			// minted just before the clock's start, so its 30 days end at day 30
			if elapsed < 30*day && !world.active(polling) {
				t.Fatalf("the polling page was marked inactive at day %d", elapsed/day)
			}
		}
		if world.active(polling) {
			t.Fatal("a page that only polls stayed active past 30 days")
		}
	})
}

// The taskworker job itself, at real time: a lease past the threshold is reaped by one run.
func TestFs37ReapJobRunsTheReapOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newFs37DbWorld(t, ctx)
		stale, _ := world.addClient(false)
		server.Tx(ctx, func(tx server.PgTx) {
			model.InsertClientLeaseInTx(ctx, tx, stale, server.NowUtc().Add(-2*time.Hour))
		})
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		result, err := ReapEphemeralClients(&ReapEphemeralClientsArgs{}, clientSession)
		if err != nil || result.ReapedCount != 1 || world.active(stale) {
			t.Fatalf("the job: %+v %v", result, err)
		}
	})
}
