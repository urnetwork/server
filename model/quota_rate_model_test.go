package model

// QUOTA-M2-03: Quota.CountRate on {sq_<network_id>} against the private stack's Redis, through
// .stack/server-test.sh. quotasuite's rate cases run here on the real script, so the fake and the
// server cannot drift apart unseen (§22.4); the rest pins what the suite cannot see from outside:
// the slot rule, the records the script leaves (a pass's only), the dark switch and the fail-open count.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
)

// A QuotaRateRedis that can fail, drop replies and record each call's keys.
type testingQuotaRateRedis struct {
	redis   QuotaRateRedis
	failing atomic.Bool
	// the next n calls run and then lose their reply
	dropReplies atomic.Int32

	stateLock sync.Mutex
	callKeys  [][]string
}

func newTestingQuotaRateRedis() *testingQuotaRateRedis {
	return &testingQuotaRateRedis{
		redis: NewQuotaRateServerRedis(10 * time.Second),
	}
}

func (self *testingQuotaRateRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.callKeys = append(self.callKeys, append([]string(nil), keys...))
	}()
	if self.failing.Load() {
		return nil, errors.New("redis is failing")
	}
	reply, err := self.redis.RunScript(ctx, script, keys, args)
	if err == nil && 0 < self.dropReplies.Load() {
		self.dropReplies.Add(-1)
		return nil, errors.New("the reply was lost")
	}
	return reply, err
}

func (self *testingQuotaRateRedis) calls() [][]string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][]string(nil), self.callKeys...)
}

var errTestingQuotaUnbuilt = errors.New("not part of this target")

// sidecar.Quota over model's parts: Limits as a case set them, CountRate through a QuotaRateCounter,
// ReportUsage through ApplyUsageBatch and OverLimit through GetQuotaOverLimit, all at the target's
// clock. Lease answers an error until QUOTA-M2-07 gives this target its leases, and QUOTA-M2-11's
// NewLocalQuota is the Quota hosts get.
type testingQuota struct {
	clock   *sidecartest.FakeClock
	counter *QuotaRateCounter

	stateLock     sync.Mutex
	networkLimits map[server.Id]*sidecar.QuotaLimits
}

func newTestingQuota(clock *sidecartest.FakeClock, redis QuotaRateRedis) *testingQuota {
	return &testingQuota{
		clock:         clock,
		counter:       NewQuotaRateCounter(redis, nil),
		networkLimits: map[server.Id]*sidecar.QuotaLimits{},
	}
}

func (self *testingQuota) setLimits(networkId server.Id, limits *sidecar.QuotaLimits) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkLimits[networkId] = &sidecar.QuotaLimits{Tier: limits.Tier, Limits: maps.Clone(limits.Limits)}
}

func (self *testingQuota) limits(networkId server.Id) *sidecar.QuotaLimits {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	limits, ok := self.networkLimits[networkId]
	if !ok {
		return &sidecar.QuotaLimits{Tier: string(NetworkTierFree), Limits: map[string]int64{}}
	}
	return &sidecar.QuotaLimits{Tier: limits.Tier, Limits: maps.Clone(limits.Limits)}
}

func (self *testingQuota) Limits(ctx context.Context, networkId connect.Id) (*sidecar.QuotaLimits, error) {
	if networkId == (connect.Id{}) {
		return nil, errors.New("a zero network id")
	}
	return self.limits(server.Id(networkId)), nil
}

func (self *testingQuota) CountRate(ctx context.Context, networkId connect.Id, keyAmounts map[string]int64, debitId connect.Id) (*sidecar.QuotaRefusal, error) {
	return self.counter.CountRate(ctx, server.Id(networkId), self.limits(server.Id(networkId)), keyAmounts, server.Id(debitId), self.clock.Now())
}

func (self *testingQuota) Lease(ctx context.Context, networkId connect.Id, meter string) (sidecar.QuotaLease, *sidecar.QuotaRefusal, error) {
	return nil, nil, errTestingQuotaUnbuilt
}

func (self *testingQuota) ReportUsage(ctx context.Context, batch *sidecar.UsageBatch) error {
	_, err := ApplyUsageBatch(ctx, batch, nil, self.clock.Now())
	return err
}

func (self *testingQuota) OverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error) {
	return GetQuotaOverLimit(ctx)
}

// Replaces a network's transfer balances, an open start or end reading a century back or ahead.
func testingSetQuotaBalances(ctx context.Context, now time.Time, networkId server.Id, balances []*sidecartest.QuotaBalance) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE network_id = $1`, networkId))
		for _, balance := range balances {
			balanceId := server.Id(balance.BalanceId)
			if balanceId == (server.Id{}) {
				balanceId = server.NewId()
			}
			startTime := balance.StartTime
			if startTime.IsZero() {
				startTime = now.AddDate(-100, 0, 0)
			}
			endTime := balance.EndTime
			if endTime.IsZero() {
				endTime = now.AddDate(100, 0, 0)
			}
			var grantKind *string
			if balance.Credit {
				credit := "team_credit"
				grantKind = &credit
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO transfer_balance (
						balance_id,
						network_id,
						start_time,
						end_time,
						start_balance_byte_count,
						net_revenue_nano_cents,
						balance_byte_count,
						pro,
						grant_kind
					)
					VALUES ($1, $2, $3, $4, $5, 0, $5, false, $6)
				`,
				balanceId,
				networkId,
				startTime.UTC(),
				endTime.UTC(),
				balance.ByteCount,
				grantKind,
			))
		}
	})
}

// A network's balances in draw order as Postgres holds them. Nothing reserves here before QUOTA-M2-07's
// leases, so no balance reads anything reserved.
func testingReadQuotaBalances(ctx context.Context, networkId server.Id) (balances []*sidecartest.QuotaBalance) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT balance_id, balance_byte_count, grant_kind IS NOT DISTINCT FROM 'team_credit', start_time, end_time
				FROM transfer_balance
				WHERE network_id = $1
				ORDER BY grant_kind IS NOT DISTINCT FROM 'team_credit', end_time, start_time, balance_id
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				balance := &sidecartest.QuotaBalance{}
				server.Raise(result.Scan(&balanceId, &balance.ByteCount, &balance.Credit, &balance.StartTime, &balance.EndTime))
				balance.BalanceId = connect.Id(balanceId)
				balances = append(balances, balance)
			}
		})
	})
	return
}

// Runs every shard's flush at clock until no page reports more, as the chains would.
func testingSettleServiceDebits(ctx context.Context, clock sidecar.Clock) error {
	for shard := range ServiceDebitShardCount {
		var after *server.Id
		for range 1000 {
			result, err := FlushServiceDebits(ctx, shard, after, clock)
			if err != nil {
				return err
			}
			if 0 < result.Failed {
				return fmt.Errorf("shard %d: %d balances failed", shard, result.Failed)
			}
			if result.LastBalanceId == nil {
				break
			}
			after = result.LastBalanceId
		}
	}
	return nil
}

// Forgets everything Redis holds, as a failover with no replica does: the test's own database.
func testingLoseRedis(ctx context.Context) {
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.FlushDB(ctx).Err())
	})
}

// quotasuite's target for this package: the Quota above, wrapped so cases can lose its responses.
func newTestingQuotaTarget(ctx context.Context) quotasuite.NewTarget {
	return func(t *testing.T) *quotasuite.Target {
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		faults := sidecartest.NewFaultQueue()
		redis := newTestingQuotaRateRedis()
		quota := newTestingQuota(clock, redis)
		return &quotasuite.Target{
			Quota:  sidecartest.NewFaultQuota(quota, faults),
			Clock:  clock,
			Faults: faults,
			SetLimits: func(networkId connect.Id, limits *sidecar.QuotaLimits) error {
				quota.setLimits(server.Id(networkId), limits)
				return nil
			},
			SetBalances: func(networkId connect.Id, balances []*sidecartest.QuotaBalance) error {
				testingSetQuotaBalances(ctx, clock.Now(), server.Id(networkId), balances)
				return nil
			},
			ReadBalances: func(networkId connect.Id) ([]*sidecartest.QuotaBalance, error) {
				return testingReadQuotaBalances(ctx, server.Id(networkId)), nil
			},
			Settle: func() error {
				return testingSettleServiceDebits(ctx, clock)
			},
			LoseRedis: func() error {
				testingLoseRedis(ctx)
				return nil
			},
			SetRedisFailing: func(failing bool) error {
				redis.failing.Store(failing)
				return nil
			},
		}
	}
}

// One network's counts on a counter at clock.
type testingRateNetwork struct {
	counter   *QuotaRateCounter
	clock     *sidecartest.FakeClock
	networkId server.Id
	limits    *sidecar.QuotaLimits
}

func newTestingRateNetwork(redis QuotaRateRedis, clock *sidecartest.FakeClock, limits map[string]int64) *testingRateNetwork {
	return &testingRateNetwork{
		counter:   NewQuotaRateCounter(redis, nil),
		clock:     clock,
		networkId: server.NewId(),
		limits:    &sidecar.QuotaLimits{Tier: string(NetworkTierFree), Limits: limits},
	}
}

func (self *testingRateNetwork) count(ctx context.Context, keyAmounts map[string]int64) (*sidecar.QuotaRefusal, error) {
	return self.counter.CountRate(ctx, self.networkId, self.limits, keyAmounts, server.NewId(), self.clock.Now())
}

// The shape evsservice.QuotaGate.ChargeClosure counts a ?closure=1 upload in.
func testingChargeClosure(eraserId server.Id, byteCount int64) map[string]int64 {
	return map[string]int64{
		sidecar.QuotaRateKeyClosureUpload(connect.Id(eraserId)): 1,
		sidecar.QuotaRateKeyClosureBytes(connect.Id(eraserId)):  byteCount,
	}
}

func testingRedisGet(ctx context.Context, key string) (value string, found bool) {
	server.Redis(ctx, func(client server.RedisClient) {
		result, err := client.Get(ctx, key).Result()
		if err == nil {
			value, found = result, true
		}
	})
	return
}

// quotasuite's rate cases, the body FakeQuota passes at T2, on the real script.
func TestQuotaRateConformsToTheSuite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer Testing_SetEnforceSidecarLimits(true)()
		quotasuite.RunRates(t, newTestingQuotaTarget(t.Context()))
	})
}

// A key built inside the script: a brace in a string literal, or the family's opening.
var testingQuotaRateScriptKeyBuilding = regexp.MustCompile(`["']\{|\{sq_`)

// §22.4's slot rule: the script builds no key and reads only KEYS, and every call, of every family and
// with the pacing, names keys under the one tag {sq_<network_id>}, so no call answers CROSSSLOT on
// production's cluster. The cluster run itself is T3's (TEST-M1-08).
func TestQuotaRateCallsShareOneTag(t *testing.T) {
	if testingQuotaRateScriptKeyBuilding.MatchString(quotaRateScriptSource) {
		t.Fatal("the script builds a key")
	}
	if !strings.Contains(quotaRateScriptSource, "KEYS[1]") || !strings.Contains(quotaRateScriptSource, "KEYS[3 + i]") {
		t.Fatal("the script reads its keys from somewhere other than KEYS")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		redis := newTestingQuotaRateRedis()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		network := newTestingRateNetwork(redis, clock, map[string]int64{
			"evs_drops_per_day":              5,
			"evs_drop_previews_per_day":      5,
			"evs_read_receipts_per_repo_day": 5,
			"evs_closure_bytes_per_day":      64 * 1024 * 1024,
		})
		eraserId := server.NewId()
		repoId := connect.NewId()
		for _, keyAmounts := range []map[string]int64{
			testingChargeClosure(eraserId, 1024),
			{sidecar.QuotaRateKeyClosureBytes(connect.Id(eraserId)): 512},
			{sidecar.QuotaRateKeyErasurePush: 1},
			{sidecar.QuotaRateKeyDrop: 1},
			{sidecar.QuotaRateKeyDropPreview(repoId): 1, sidecar.QuotaRateKeyReadReceipt(repoId): 2},
		} {
			if _, err := network.count(ctx, keyAmounts); err != nil {
				t.Fatal(err)
			}
		}
		calls := redis.calls()
		connect.AssertEqual(t, len(calls), 5)
		tag := fmt.Sprintf("{sq_%s}", network.networkId)
		for _, keys := range calls {
			if !sidecar.RedisKeysShareTag(keys) {
				t.Errorf("a call names %v", keys)
			}
			for _, key := range keys {
				if !strings.HasPrefix(key, tag) {
					t.Errorf("%s is not under %s", key, tag)
				}
			}
		}
	})
}

// All-or-none: a call one key refuses writes no counter of its other keys and records nothing under its
// debit id, so the same id asked again is judged afresh: counted once its window turned, and answered as
// passed, counting nothing, after that (DESIGN-DETAILS 13.2.1).
func TestQuotaRateRefusalCountsNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		network := newTestingRateNetwork(nil, clock, map[string]int64{
			"evs_drops_per_day":              3,
			"evs_read_receipts_per_repo_day": 2,
		})
		repoId := connect.NewId()
		// a receipt counted today, so a call adding two is refused
		refusal, err := network.count(ctx, map[string]int64{sidecar.QuotaRateKeyReadReceipt(repoId): 1})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		debitId := server.NewId()
		keyAmounts := map[string]int64{
			sidecar.QuotaRateKeyDrop:                1,
			sidecar.QuotaRateKeyReadReceipt(repoId): 2,
		}
		refusal, err = network.counter.CountRate(ctx, network.networkId, network.limits, keyAmounts, debitId, clock.Now())
		connect.AssertEqual(t, err, nil)
		if refusal == nil || refusal.LimitKey != "evs_read_receipts_per_repo_day" || refusal.Limit != 2 || refusal.Used != 1 {
			t.Fatalf("refusal %+v", refusal)
		}
		_, found := testingRedisGet(ctx, quotaRateCounterKey(network.networkId, sidecar.QuotaRateKeyDrop))
		connect.AssertEqual(t, found, false)
		_, found = testingRedisGet(ctx, quotaRateDebitKey(network.networkId, debitId))
		connect.AssertEqual(t, found, false)

		// the drops are all still there
		passed, err := network.count(ctx, map[string]int64{sidecar.QuotaRateKeyDrop: 3})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, passed, (*sidecar.QuotaRefusal)(nil))

		// the next day the same id is judged afresh and counts
		clock.Advance(24 * time.Hour)
		replayed, err := network.counter.CountRate(ctx, network.networkId, network.limits, keyAmounts, debitId, clock.Now())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, replayed, (*sidecar.QuotaRefusal)(nil))
		record, found := testingRedisGet(ctx, quotaRateDebitKey(network.networkId, debitId))
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, record, quotaRatePassedRecord)
		replayed, err = network.counter.CountRate(ctx, network.networkId, network.limits, keyAmounts, debitId, clock.Now())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, replayed, (*sidecar.QuotaRefusal)(nil))
		value, _ := testingRedisGet(ctx, quotaRateCounterKey(network.networkId, sidecar.QuotaRateKeyReadReceipt(repoId)))
		connect.AssertEqual(t, strings.HasSuffix(value, ":2"), true)
	})
}

// A reply lost after the script counted: the call passes open, and the retry under the same debit id
// answers the first result and counts nothing twice.
func TestQuotaRateDebitReplaysAcrossALostReply(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		redis := newTestingQuotaRateRedis()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		network := newTestingRateNetwork(redis, clock, map[string]int64{"evs_drops_per_day": 1})
		keyAmounts := map[string]int64{sidecar.QuotaRateKeyDrop: 1}
		debitId := server.NewId()

		failOpenBefore := testutil.ToFloat64(quotaRateFailOpenCounter)
		redis.dropReplies.Store(1)
		refusal, err := network.counter.CountRate(ctx, network.networkId, network.limits, keyAmounts, debitId, clock.Now())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		connect.AssertEqual(t, testutil.ToFloat64(quotaRateFailOpenCounter)-failOpenBefore, float64(1))

		refusal, err = network.counter.CountRate(ctx, network.networkId, network.limits, keyAmounts, debitId, clock.Now())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		value, _ := testingRedisGet(ctx, quotaRateCounterKey(network.networkId, sidecar.QuotaRateKeyDrop))
		connect.AssertEqual(t, strings.HasSuffix(value, ":1"), true)

		refusal, err = network.count(ctx, keyAmounts)
		connect.AssertEqual(t, err, nil)
		if refusal == nil || refusal.LimitKey != "evs_drops_per_day" || refusal.Used != 1 {
			t.Fatalf("a new debit past the limit answered %+v", refusal)
		}
	})
}

// No limit takes an eraser's allowance below evs_closure_bytes_floor, nor the floor below one full
// re-key, 1,088 KiB (owner decision 146), and a limit read through NetworkQuotaLimits keeps the floor
// whatever pro.yml says.
func TestQuotaRateAllowanceKeepsTheFloor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		for _, c := range []struct {
			limits map[string]int64
			want   int64
		}{
			{limits: map[string]int64{"evs_closure_bytes_per_day": 1, "evs_closure_bytes_floor": 4 * 1024 * 1024}, want: 4 * 1024 * 1024},
			{limits: map[string]int64{"evs_closure_bytes_per_day": 1, "evs_closure_bytes_floor": 0}, want: 1088 * 1024},
			{limits: map[string]int64{}, want: 1088 * 1024},
			{limits: map[string]int64{"evs_closure_bytes_per_day": 64 * 1024 * 1024, "evs_closure_bytes_floor": 4 * 1024 * 1024}, want: 64 * 1024 * 1024},
		} {
			network := newTestingRateNetwork(nil, clock, c.limits)
			eraserId := connect.NewId()
			refusal, err := network.count(ctx, map[string]int64{sidecar.QuotaRateKeyClosureBytes(eraserId): c.want})
			if err != nil || refusal != nil {
				t.Fatalf("%v: the allowance's bytes answered %+v, %v", c.limits, refusal, err)
			}
			refusal, err = network.count(ctx, map[string]int64{sidecar.QuotaRateKeyClosureBytes(eraserId): 1})
			if err != nil || refusal == nil || refusal.Limit != c.want || refusal.LimitKey != "evs_closure_bytes_per_day" {
				t.Fatalf("%v: one byte past answered %+v, %v", c.limits, refusal, err)
			}
		}

		// through pro.yml's reading and a network's limits
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"evs_closure_bytes_per_day": 1, "evs_closure_bytes_floor": 0})()
		limits := NetworkQuotaLimits(ctx, server.NewId())
		connect.AssertEqual(t, limits.Limits["evs_closure_bytes_per_day"], int64(1088*1024))
	})
}

// Redis lost forgets the window's counts, so the rates open afresh; a Redis that does not answer
// passes every call and counts it as failing open, refusing nothing (§20.7).
func TestQuotaRateOpensWhenRedisIsLost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		redis := newTestingQuotaRateRedis()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		network := newTestingRateNetwork(redis, clock, map[string]int64{"evs_drops_per_day": 2})
		keyAmounts := map[string]int64{sidecar.QuotaRateKeyDrop: 1}
		for range 2 {
			refusal, err := network.count(ctx, keyAmounts)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		}
		refusal, _ := network.count(ctx, keyAmounts)
		connect.AssertEqual(t, refusal != nil, true)

		testingLoseRedis(ctx)
		for range 2 {
			refusal, err := network.count(ctx, keyAmounts)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		}
		refusal, _ = network.count(ctx, keyAmounts)
		connect.AssertEqual(t, refusal != nil, true)

		failOpenBefore := testutil.ToFloat64(quotaRateFailOpenCounter)
		redis.failing.Store(true)
		for range 3 {
			refusal, err := network.count(ctx, keyAmounts)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		}
		connect.AssertEqual(t, testutil.ToFloat64(quotaRateFailOpenCounter)-failOpenBefore, float64(3))

		// a caller whose context ended gets its own error, not an open pass
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := network.counter.CountRate(canceled, network.networkId, network.limits, keyAmounts, server.NewId(), clock.Now())
		connect.AssertEqual(t, errors.Is(err, context.Canceled), true)
	})
}

// While enforce_sidecar_limits is off, the limits pro.yml alone sets (drops, previews, receipts) neither
// count nor refuse, with no Redis call, so an absent pro.yml locks no one out; the closure bounds still
// hold, since evs asks for them only at the limit and while spent, and spent is never dark.
func TestQuotaRateDarkLimitsCountNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(false)()
		redis := newTestingQuotaRateRedis()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		// zero everywhere, as with no pro.yml
		network := newTestingRateNetwork(redis, clock, map[string]int64{})
		repoId := connect.NewId()
		for _, keyAmounts := range []map[string]int64{
			{sidecar.QuotaRateKeyDrop: 1},
			{sidecar.QuotaRateKeyDropPreview(repoId): 1},
			{sidecar.QuotaRateKeyReadReceipt(repoId): 1},
			{sidecar.QuotaRateKeyDrop: 1, sidecar.QuotaRateKeyReadReceipt(repoId): 1},
		} {
			refusal, err := network.count(ctx, keyAmounts)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		}
		connect.AssertEqual(t, len(redis.calls()), 0)
		_, found := testingRedisGet(ctx, quotaRateCounterKey(network.networkId, sidecar.QuotaRateKeyDrop))
		connect.AssertEqual(t, found, false)

		// a caller's bug is an error whatever the switch
		_, err := network.count(ctx, map[string]int64{sidecar.QuotaRateKeyDrop: -1})
		connect.AssertEqual(t, err != nil, true)

		// the floor's allowance bounds an eraser while spent, and the push class still classes
		eraserId := server.NewId()
		refusal, err := network.count(ctx, map[string]int64{sidecar.QuotaRateKeyClosureBytes(connect.Id(eraserId)): 1088 * 1024})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		refusal, err = network.count(ctx, map[string]int64{sidecar.QuotaRateKeyClosureBytes(connect.Id(eraserId)): 1})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refusal != nil && refusal.LimitKey == "evs_closure_bytes_per_day", true)
		for range 60 {
			refusal, err = network.count(ctx, map[string]int64{sidecar.QuotaRateKeyErasurePush: 1})
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		}
		refusal, _ = network.count(ctx, map[string]int64{sidecar.QuotaRateKeyErasurePush: 1})
		connect.AssertEqual(t, refusal != nil && refusal.LimitKey == "evs_erasure_pushes_per_min", true)
	})
}

// FS-47: an upload past its eraser's 60 a minute, and past its daily allowance, answers 429 with
// retry_after_ms, the minute's at the minute's end and the allowance's at the next 00:00 UTC; the
// QuotaRefusal is the body (§20.7's Refusals).
func TestQuotaRateClosureRefusalsAnswer429(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		// a minute's top, so 60 paced uploads fit in it
		start := time.Date(2026, time.March, 4, 23, 50, 0, 0, time.UTC)
		clock := sidecartest.NewFakeClock(start)
		// 61 uploads' bytes, above the 1,088 KiB the allowance never reads below
		upload := int64(32 * 1024)
		network := newTestingRateNetwork(nil, clock, map[string]int64{
			"evs_closure_bytes_per_day": 61 * upload,
			"evs_closure_bytes_floor":   0,
		})
		eraserId := server.NewId()
		chargeObediently := func() *sidecar.QuotaRefusal {
			for range 100 {
				refusal, err := network.count(ctx, testingChargeClosure(eraserId, upload))
				connect.AssertEqual(t, err, nil)
				if refusal == nil || refusal.LimitKey != "evs_closure_uploads_per_s" {
					return refusal
				}
				clock.Advance(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
			}
			t.Fatal("no turn came")
			return nil
		}
		for i := range 60 {
			if refusal := chargeObediently(); refusal != nil {
				t.Fatalf("upload %d refused %+v", i, refusal)
			}
		}
		now := clock.Now()
		refusal := chargeObediently()
		if refusal == nil || refusal.LimitKey != "evs_closure_uploads_per_min" || refusal.Used != 60 {
			t.Fatalf("upload 61 answered %+v", refusal)
		}
		minuteEnd := start.Add(time.Minute)
		connect.AssertEqual(t, refusal.RetryAfterMs, sidecar.QuotaRetryAfterMs(clock.Now(), minuteEnd))
		quotaError := NewQuotaRefusalError(refusal)
		connect.AssertEqual(t, quotaError.HttpStatus(), http.StatusTooManyRequests)
		connect.AssertEqual(t, 0 < quotaError.RetryAfterSeconds(), true)
		connect.AssertEqual(t, now.Before(minuteEnd), true)

		// the next minute's upload spends the allowance's last; the one after waits for 00:00 UTC
		clock.AdvanceTo(minuteEnd)
		if refusal := chargeObediently(); refusal != nil {
			t.Fatalf("the next minute's upload refused %+v", refusal)
		}
		refusal = chargeObediently()
		if refusal == nil || refusal.LimitKey != "evs_closure_bytes_per_day" || refusal.Used != 61*upload || refusal.Upgrade {
			t.Fatalf("past the allowance answered %+v", refusal)
		}
		midnight := time.Date(2026, time.March, 5, 0, 0, 0, 0, time.UTC)
		connect.AssertEqual(t, refusal.RetryAfterMs, sidecar.QuotaRetryAfterMs(clock.Now(), midnight))
		connect.AssertEqual(t, NewQuotaRefusalError(refusal).HttpStatus(), http.StatusTooManyRequests)

		clock.AdvanceTo(midnight)
		if refusal := chargeObediently(); refusal != nil {
			t.Fatalf("the new day's upload refused %+v", refusal)
		}
	})
}

// §21.7's closure-growth case reads the network's closure bytes today: every eraser's, a refused call's
// none, and nothing on another day.
func TestNetworkClosureBytesToday(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		network := newTestingRateNetwork(nil, clock, map[string]int64{"evs_closure_bytes_per_day": 4 * 1024 * 1024})
		total, err := NetworkClosureBytesToday(ctx, network.networkId, clock.Now())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, total, int64(0))

		eraserA := connect.NewId()
		eraserB := connect.NewId()
		for _, c := range []struct {
			eraserId  connect.Id
			byteCount int64
			refused   bool
		}{
			{eraserId: eraserA, byteCount: 1000},
			{eraserId: eraserB, byteCount: 2000},
			{eraserId: eraserA, byteCount: 4 * 1024 * 1024, refused: true},
			{eraserId: eraserB, byteCount: 300},
		} {
			refusal, err := network.count(ctx, map[string]int64{sidecar.QuotaRateKeyClosureBytes(c.eraserId): c.byteCount})
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refusal != nil, c.refused)
		}
		total, err = NetworkClosureBytesToday(ctx, network.networkId, clock.Now())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, total, int64(3300))

		total, err = NetworkClosureBytesToday(ctx, network.networkId, clock.Now().Add(24*time.Hour))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, total, int64(0))
	})
}

// CountQuotaRate reads the network's limits from this process's caches, pro.yml's and its rows'.
func TestCountQuotaRateReadsTheNetworksLimits(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"evs_drops_per_day": 1})()
		networkId := server.NewId()
		refusal, err := CountQuotaRate(ctx, networkId, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, server.NewId())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		refusal, err = CountQuotaRate(ctx, networkId, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, server.NewId())
		connect.AssertEqual(t, err, nil)
		if refusal == nil || refusal.LimitKey != "evs_drops_per_day" || refusal.Limit != 1 || !refusal.Upgrade {
			t.Fatalf("the second drop answered %+v", refusal)
		}
		_, err = CountQuotaRate(ctx, server.Id{}, map[string]int64{sidecar.QuotaRateKeyDrop: 1}, server.NewId())
		connect.AssertEqual(t, err != nil, true)
	})
}
