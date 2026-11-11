package quotahost

// QUOTA-M2-07: edge leases (DESIGN-DETAILS 13.2.2, 13.6's quotahost/lease_test.go row) against the private
// stack's Redis through .stack/server-test.sh, the balances from a source the test holds, on a fake clock:
// Spend's T1 row; attribution by rank across draws made out of order; renewal at the mark and at DrawAhead of
// a fast rate; a reservation unanswered three times abandoned; expiry dropping a draw ExpiryMargin before its
// token's end and counting its spent bytes unbilled; close settling the highest rank first. The batch half is
// lease_batch_test.go's.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

// Balances a test sets by hand, and the spent marks a lease writes.
type testingBalanceSource struct {
	stateLock       sync.Mutex
	networkBalances map[server.Id][]*LeaseBalance
	failing         bool
	readCount       int
	spentNetworkIds []server.Id
}

func newTestingBalanceSource() *testingBalanceSource {
	return &testingBalanceSource{networkBalances: map[server.Id][]*LeaseBalance{}}
}

func (self *testingBalanceSource) set(networkId server.Id, balances ...*LeaseBalance) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkBalances[networkId] = balances
}

func (self *testingBalanceSource) DrawOrder(ctx context.Context, networkId server.Id) ([]*LeaseBalance, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readCount += 1
	if self.failing {
		return nil, errors.New("the balance source is failing")
	}
	balances := []*LeaseBalance{}
	for _, balance := range self.networkBalances[networkId] {
		copied := *balance
		balances = append(balances, &copied)
	}
	return balances, nil
}

func (self *testingBalanceSource) MarkSpent(ctx context.Context, networkId server.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.spentNetworkIds = append(self.spentNetworkIds, networkId)
	return nil
}

func (self *testingBalanceSource) spentMarks() []server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]server.Id(nil), self.spentNetworkIds...)
}

// A QuotaRateRedis on the private stack that counts calls by operation, and can fail every call, or lose the
// reply of the next ones after they ran.
type testingLeaseRedis struct {
	redis       model.QuotaRateRedis
	failing     atomic.Bool
	failExtends atomic.Bool
	dropReplies atomic.Int32

	stateLock      sync.Mutex
	operationCalls []string
}

func newTestingLeaseRedis() *testingLeaseRedis {
	return &testingLeaseRedis{redis: model.NewQuotaRateServerRedis(10 * time.Second)}
}

func (self *testingLeaseRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	operation := ""
	if 0 < len(args) {
		operation = args[0]
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		token := ""
		if 1 < len(args) {
			token = args[1]
		}
		self.operationCalls = append(self.operationCalls, operation+" "+token)
	}()
	if self.failing.Load() || (operation == "extend" && self.failExtends.Load()) {
		return nil, errors.New("redis is failing")
	}
	reply, err := self.redis.RunScript(ctx, script, keys, args)
	if err == nil && 0 < self.dropReplies.Load() {
		self.dropReplies.Add(-1)
		return nil, errors.New("the reply was lost")
	}
	return reply, err
}

func (self *testingLeaseRedis) calls() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.operationCalls...)
}

func (self *testingLeaseRedis) callCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.operationCalls)
}

// Small draws, so a test moves a few bytes.
func testingLeaseSettings() *LeaseSettings {
	settings := DefaultLeaseSettings()
	settings.DrawByteCount = 1000
	settings.CommunityDrawByteCount = 400
	return settings
}

type testingLeaseFixture struct {
	set      *quotaLeaseSet
	redis    *testingLeaseRedis
	balances *testingBalanceSource
	clock    *sidecartest.FakeClock
}

func newTestingLeaseFixture(t testing.TB, settings *LeaseSettings) *testingLeaseFixture {
	t.Helper()
	if settings == nil {
		settings = testingLeaseSettings()
	}
	redis := newTestingLeaseRedis()
	balances := newTestingBalanceSource()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	set, err := newQuotaLeaseSet(t.Context(), redis, balances, func(ctx context.Context, networkId server.Id) (string, error) {
		return string(model.NetworkTierPro), nil
	}, clock, settings)
	if err != nil {
		t.Fatal(err)
	}
	set.manualTicks = true
	t.Cleanup(func() {
		set.cancel()
		set.entryGroup.Wait()
	})
	return &testingLeaseFixture{set: set, redis: redis, balances: balances, clock: clock}
}

// A balance in draw order: credit lines last, then by end.
func testingLeaseBalance(credit int64, isCredit bool, endTime time.Time) *LeaseBalance {
	return &LeaseBalance{BalanceId: server.NewId(), CreditByteCount: credit, IsCredit: isCredit, EndTime: endTime}
}

// What a balance's reservations hold, by token.
func testingLeaseTokens(ctx context.Context, balanceId server.Id) map[string]string {
	var tokenAmounts map[string]string
	server.Redis(ctx, func(client server.RedisClient) {
		var err error
		tokenAmounts, err = client.HGetAll(ctx, "{escrow_"+balanceId.String()+"}approx_contracts").Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
	})
	return tokenAmounts
}

func testingLeaseReserved(ctx context.Context, balanceId server.Id) int64 {
	total := int64(0)
	for _, amount := range testingLeaseTokens(ctx, balanceId) {
		value, err := strconv.ParseInt(amount, 10, 64)
		server.Raise(err)
		total += value
	}
	return total
}

func testingOpenLease(t testing.TB, fixture *testingLeaseFixture, networkId server.Id) *quotaLeaseHandle {
	t.Helper()
	lease, refusal, err := fixture.set.Lease(t.Context(), connect.Id(networkId), sidecar.QuotaMeterTls)
	if err != nil || refusal != nil {
		t.Fatalf("the lease: %+v, %v", refusal, err)
	}
	return lease.(*quotaLeaseHandle)
}

// Runs one round of the entry's goroutine's work, as its wake or its tick would.
func testingTick(t testing.TB, handle *quotaLeaseHandle) {
	t.Helper()
	handle.entry.tick(t.Context())
}

// §22.5's T1 row: Spend is atomic, with no lock, no I/O and no allocation. Concurrent spenders under the race
// detector take exactly what the lease holds, no host call is made, and Postgres is never reached.
func TestLeaseSpendIsAtomicWithNoIo(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		settings := testingLeaseSettings()
		settings.DrawByteCount = 100000
		fixture := newTestingLeaseFixture(t, settings)
		networkId := server.NewId()
		fixture.balances.set(networkId, testingLeaseBalance(100000, false, time.Now().Add(time.Hour)))
		handle := testingOpenLease(t, fixture, networkId)
		calls := fixture.redis.callCount()
		attempts, stop := server.DenyPostgresForTest(t)
		defer stop()

		var group sync.WaitGroup
		var taken atomic.Int64
		for range 8 {
			group.Add(1)
			go func() {
				defer group.Done()
				for range 2000 {
					if handle.Spend(5) {
						taken.Add(5)
					}
				}
			}()
		}
		group.Wait()
		connect.AssertEqual(t, taken.Load(), int64(80000))
		connect.AssertEqual(t, handle.entry.spent.Load(), int64(80000))
		connect.AssertEqual(t, handle.entry.available.Load(), int64(20000))
		if allocs := testing.AllocsPerRun(1000, func() { handle.Spend(1) }); allocs != 0 {
			t.Fatalf("Spend allocated %f times a call", allocs)
		}
		connect.AssertEqual(t, fixture.redis.callCount(), calls)
		connect.AssertEqual(t, attempts(), uint64(0))
	})
}

// §22.5's hot-path gate on Spend's composed entry, budgets.yml's quota.spend row: no PostgreSQL acquisition
// and no Redis call, a lease holding bytes.
func TestLeaseSpendHoldsToItsInlinePathRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		fixture.balances.set(networkId, testingLeaseBalance(5000, false, time.Now().Add(time.Hour)))
		handle := testingOpenLease(t, fixture, networkId)
		defer handle.Close()
		calls := fixture.redis.callCount()
		result := sidecart3.InlinePath(t, "quota.spend", sidecartest.NewFakeRedis(t.Context(), nil, nil, nil, nil), func(redis sidecar.Redis) {
			for range 100 {
				if !handle.Spend(1) {
					t.Error("a spend within the draw refused")
				}
			}
		})
		connect.AssertEqual(t, result.RedisCallCount, 0)
		connect.AssertEqual(t, fixture.redis.callCount(), calls)
	})
}

// Draws made out of order are attributed by rank: a lease that drew on the credit line, then on a data pack
// once it had room, debits the pack first, and at close the unspent rest returns from the line (E-a, E-b).
func TestLeaseAttributesByRankAcrossDraws(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		now := time.Now()
		line := testingLeaseBalance(10000, true, now.Add(24*time.Hour))
		pack := testingLeaseBalance(5000, false, now.Add(365*24*time.Hour))
		fixture.balances.set(networkId, line)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, testingLeaseReserved(ctx, line.BalanceId), int64(1000))
		// past the mark with the pack now in window: the renewal draws on the pack, which sorts first
		fixture.balances.set(networkId, pack, line)
		fixture.clock.Advance(settingsRefresh(fixture))
		connect.AssertEqual(t, handle.Spend(800), true)
		testingTick(t, handle)
		connect.AssertEqual(t, testingLeaseReserved(ctx, pack.BalanceId), int64(1000))
		connect.AssertEqual(t, handle.Spend(500), true)

		var carves []*model.UsageCarve
		batch := &sidecar.UsageBatch{
			BatchId:  connect.NewId(),
			Producer: sidecar.QuotaProducerTlsEdge,
			Events:   []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterTls, TransferByteCount: 1300}},
		}
		err := fixture.set.reportUsage(ctx, batch, func(ctx context.Context, sent *sidecar.UsageBatch, sentCarves []*model.UsageCarve) error {
			carves = sentCarves
			connect.AssertEqual(t, sent.Events[0].TransferByteCount, int64(1300))
			return nil
		})
		connect.AssertEqual(t, err, nil)
		balanceCarved := map[server.Id]int64{}
		for _, carve := range carves {
			balanceCarved[carve.BalanceId] += carve.ByteCount
		}
		// the pack's 1000 first though drawn second, then 300 of the line
		connect.AssertEqual(t, balanceCarved, map[server.Id]int64{pack.BalanceId: 1000, line.BalanceId: 300})
		handle.Close()
		// the line keeps nothing: its 700 unspent return, its 300 sit carved
		connect.AssertEqual(t, testingLeaseReserved(ctx, line.BalanceId), int64(300))
		connect.AssertEqual(t, testingLeaseReserved(ctx, pack.BalanceId), int64(1000))
	})
}

// A reading's refresh age, so a test's next draw reads the source afresh.
func settingsRefresh(fixture *testingLeaseFixture) time.Duration {
	return fixture.set.settings.BalanceRefreshAge
}

// Renewal at the mark: a slow lease renews with a quarter of its draw left; a fast one draws DrawAhead of its
// rate, rounded up to a multiple of the draw, and renews with RenewAhead of it left.
func TestLeaseRenewsAtTheMarkAndAtAFastRate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		balance := testingLeaseBalance(1000000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(1000))
		connect.AssertEqual(t, handle.Spend(700), true)
		testingTick(t, handle)
		// 300 left, above the 250 mark: no renewal
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(1000))
		connect.AssertEqual(t, handle.Spend(50), true)
		fixture.clock.Advance(time.Second)
		testingTick(t, handle)
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(2000))

		// 1,000 bytes a second: a draw of 4 s of it, a renewal with 2 s of it left
		fixture.clock.Advance(time.Second)
		testingTick(t, handle)
		for range 3 {
			connect.AssertEqual(t, handle.Spend(1000), true)
			fixture.clock.Advance(time.Second)
			testingTick(t, handle)
		}
		entry := handle.entry
		entry.stateLock.Lock()
		lastDraw := entry.lastDrawByteCount
		entry.stateLock.Unlock()
		connect.AssertEqual(t, lastDraw, int64(4000))
		connect.AssertEqual(t, entry.renewMark.Load(), int64(2000))
		handle.Close()
	})
}

// A reservation Redis never answers is tried ReserveRetryCount times under one name and abandoned: the
// Lease is an error, with no refusal and no spent mark, and a reply lost after the script ran leaves a token
// no draw holds, to lapse at its lifetime.
func TestLeaseReservationUnansweredIsAbandoned(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		balance := testingLeaseBalance(5000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		fixture.redis.dropReplies.Store(3)
		lease, refusal, err := fixture.set.Lease(ctx, connect.Id(networkId), sidecar.QuotaMeterTls)
		if err == nil || refusal != nil || lease != nil {
			t.Fatalf("an unanswered draw answered %v, %+v, %v", lease, refusal, err)
		}
		calls := fixture.redis.calls()
		connect.AssertEqual(t, len(calls), 3)
		for _, call := range calls {
			connect.AssertEqual(t, call, calls[0])
		}
		connect.AssertEqual(t, len(fixture.balances.spentMarks()), 0)
		// the script ran: one token of the abandoned name holds the draw, and no lease spends it
		tokens := testingLeaseTokens(ctx, balance.BalanceId)
		connect.AssertEqual(t, len(tokens), 1)
		server.Redis(ctx, func(client server.RedisClient) {
			expiry, err := client.ZScore(ctx, "{escrow_"+balance.BalanceId.String()+"}approx_expiry", model.QuotaLeaseToken(leaseIdOf(calls[0]), 1)).Result()
			server.Raise(err)
			if gap := time.Duration(expiry)*time.Millisecond - time.Duration(time.Now().UnixMilli())*time.Millisecond; gap < 14*time.Minute || 16*time.Minute < gap {
				t.Fatalf("the abandoned token lapses in %s", gap)
			}
		})
	})
}

// The lease id in a call's "reserve lease/<lease_id>/<n>".
func leaseIdOf(call string) server.Id {
	const prefix = "reserve lease/"
	leaseId, err := server.ParseId(call[len(prefix) : len(prefix)+36])
	server.Raise(err)
	return leaseId
}

// Twin LeaseTwin-SpendPastExpiry: a draw stops being spendable ExpiryMargin before its token could lapse,
// whatever the host's reach to Redis: with every extend unanswered, at TokenTtl less the margin the draw is
// dropped, Spend refuses, and the bytes spent and not yet planned are counted unbilled.
func TestLeaseStopsSpendingBeforeExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		fixture.balances.set(networkId, testingLeaseBalance(5000, false, time.Now().Add(time.Hour)))
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, handle.Spend(100), true)
		fixture.redis.failExtends.Store(true)
		settings := fixture.set.settings
		unbilled := testutil.ToFloat64(quotaLeaseUnbilledByteCounter.WithLabelValues(sidecar.QuotaMeterTls))
		fixture.clock.Advance(settings.TokenTtl - settings.ExpiryMargin - time.Second)
		testingTick(t, handle)
		connect.AssertEqual(t, handle.Spend(1), true)
		fixture.clock.Advance(time.Second)
		testingTick(t, handle)
		connect.AssertEqual(t, handle.entry.available.Load(), int64(0))
		connect.AssertEqual(t, handle.Spend(1), false)
		connect.AssertEqual(t, testutil.ToFloat64(quotaLeaseUnbilledByteCounter.WithLabelValues(sidecar.QuotaMeterTls))-unbilled, float64(101))
		handle.entry.stateLock.Lock()
		connect.AssertEqual(t, len(handle.entry.draws), 0)
		handle.entry.stateLock.Unlock()
	})
}

// An extend answering 0 drops its draw: Redis lost the token, so the host stops spending it at once.
func TestLeaseExtendAnsweringZeroDropsTheDraw(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		balance := testingLeaseBalance(5000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, handle.Spend(10), true)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Del(ctx, "{escrow_"+balance.BalanceId.String()+"}approx_contracts").Err())
		})
		fixture.clock.Advance(fixture.set.settings.ExtendInterval)
		testingTick(t, handle)
		connect.AssertEqual(t, handle.entry.available.Load(), int64(0))
		connect.AssertEqual(t, handle.Spend(1), false)
	})
}

// Close settles each draw, the highest rank first, to the spent bytes not yet carved that it is attributed:
// the credit line's draw is settled before the pack's, and the unspent rest returns from the line.
func TestLeaseCloseSettlesTheHighestRankFirst(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		now := time.Now()
		pack := testingLeaseBalance(600, false, now.Add(30*24*time.Hour))
		line := testingLeaseBalance(10000, true, now.Add(24*time.Hour))
		fixture.balances.set(networkId, pack, line)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, testingLeaseReserved(ctx, pack.BalanceId), int64(600))
		connect.AssertEqual(t, testingLeaseReserved(ctx, line.BalanceId), int64(400))
		connect.AssertEqual(t, handle.Spend(700), true)
		calls := fixture.redis.callCount()
		handle.Close()
		connect.AssertEqual(t, handle.Spend(1), false)
		closeCalls := fixture.redis.calls()[calls:]
		// the line is settled to its 100 spent bytes; the pack keeps its 600 whole and is not asked
		connect.AssertEqual(t, len(closeCalls), 1)
		connect.AssertEqual(t, closeCalls[0][:len("settle")], "settle")
		connect.AssertEqual(t, testingLeaseReserved(ctx, line.BalanceId), int64(100))
		connect.AssertEqual(t, testingLeaseReserved(ctx, pack.BalanceId), int64(600))

		// a lease with nothing spent releases every draw, the line's first
		otherNetworkId := server.NewId()
		otherPack := testingLeaseBalance(300, false, now.Add(30*24*time.Hour))
		otherLine := testingLeaseBalance(10000, true, now.Add(24*time.Hour))
		fixture.balances.set(otherNetworkId, otherPack, otherLine)
		other := testingOpenLease(t, fixture, otherNetworkId)
		calls = fixture.redis.callCount()
		other.Close()
		closeCalls = fixture.redis.calls()[calls:]
		connect.AssertEqual(t, len(closeCalls), 2)
		connect.AssertEqual(t, closeCalls[0], "release "+model.QuotaLeaseToken(other.entry.leaseId, 2))
		connect.AssertEqual(t, closeCalls[1], "release "+model.QuotaLeaseToken(other.entry.leaseId, 1))
		connect.AssertEqual(t, testingLeaseReserved(ctx, otherPack.BalanceId), int64(0))
		connect.AssertEqual(t, testingLeaseReserved(ctx, otherLine.BalanceId), int64(0))
	})
}

// A draw that reserves nothing while Redis answered refuses the lease, limit_key data with upgrade, and marks
// the network spent; a balance source that cannot be read is an error, marking nothing; handles of one
// process share one lease, the last Close settling it.
func TestLeaseRefusalMarksSpentAndHandlesShareTheLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		emptyId := server.NewId()
		lease, refusal, err := fixture.set.Lease(ctx, connect.Id(emptyId), sidecar.QuotaMeterTls)
		if err != nil || lease != nil || refusal == nil || refusal.LimitKey != "data" || !refusal.Upgrade {
			t.Fatalf("an empty network's lease: %v, %+v, %v", lease, refusal, err)
		}
		connect.AssertEqual(t, fixture.balances.spentMarks(), []server.Id{emptyId})

		failingId := server.NewId()
		fixture.balances.stateLock.Lock()
		fixture.balances.failing = true
		fixture.balances.stateLock.Unlock()
		if _, refusal, err := fixture.set.Lease(ctx, connect.Id(failingId), sidecar.QuotaMeterTls); err == nil || refusal != nil {
			t.Fatalf("an unreadable source answered %+v, %v", refusal, err)
		}
		connect.AssertEqual(t, len(fixture.balances.spentMarks()), 1)
		fixture.balances.stateLock.Lock()
		fixture.balances.failing = false
		fixture.balances.stateLock.Unlock()

		networkId := server.NewId()
		balance := testingLeaseBalance(5000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		first := testingOpenLease(t, fixture, networkId)
		calls := fixture.redis.callCount()
		second := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, fixture.redis.callCount(), calls)
		connect.AssertEqual(t, first.entry, second.entry)
		connect.AssertEqual(t, first.Spend(10), true)
		first.Close()
		connect.AssertEqual(t, first.Spend(1), false)
		connect.AssertEqual(t, second.Spend(5), true)
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(1000))
		second.Close()
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(15))
		if _, _, err := fixture.set.Lease(ctx, connect.Id(networkId), sidecar.QuotaMeterEvs); err == nil {
			t.Fatal("a lease on evs's meter")
		}
	})
}

// FloorTtl must outlive twice a reading's use, and the settings' other bounds hold.
func TestLeaseSettingsValidate(t *testing.T) {
	connect.AssertEqual(t, DefaultLeaseSettings().Validate(), nil)
	settings := DefaultLeaseSettings()
	settings.FloorTtl = settings.MaxCreditAge
	if settings.Validate() == nil {
		t.Fatal("a floor shorter than twice a reading's use")
	}
	settings = DefaultLeaseSettings()
	settings.ExtendInterval = settings.TokenTtl
	if settings.Validate() == nil {
		t.Fatal("extends slower than a token's spendable life")
	}
}
