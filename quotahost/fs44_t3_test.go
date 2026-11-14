//go:build sidecar_pending

package quotahost

// FS-44 (§20.6, FI-13 (a); QUOTA-M2-13) on T3: the schedules sidecartest/fault's fs44_test.go runs on the fake
// Quota, here on NewLocalQuota's parts over the private stack's Postgres and Redis, with FI-13's oracle
// reading transfer_balance and service_debit_journal after every step. The producer rows run the real
// QuotaUsageBatcher on the fake clock: killed before ReportUsage, a response lost and retried, a drain, api
// unavailable for 11 min. The lease rows of DESIGN-DETAILS 13.6: a host killed between a carve and its post
// debits nothing and its carved token is gone once its lifetime passes; a post whose answer is lost is
// retried, and a third copy of the carve and of the batch delivered after the flush moves and debits nothing
// more; two hosts on one balance, one holding an old reading across the other's flushed debit, are held to
// the balance by the floor, which ends at zero.
//
// Redis keeps a token's lifetime on its own clock, so a lifetime passing is the test setting the token's
// expiry into the past, which the next operation on the balance then finds, as it would 24 h later.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Postgres and Redis as FI-13's oracle reads them: each balance less the debits journaled against it and
// not yet applied, with what Redis reserves of it, and the network's stored count. No schedule here leaves a
// network without an in-window balance, the one case model counts bytes unbilled in a metric alone, and a
// lease's own unbilled bytes are a schedule's to name as lost; so the reading answers none unbilled.
type t3OracleSource struct {
	ctx context.Context
}

func (self *t3OracleSource) ReadQuota(ctx context.Context, networkId connect.Id) (*sidecartest.QuotaOracleReading, error) {
	balances := t3ReadBalances(self.ctx, server.Id(networkId))
	server.Db(self.ctx, func(conn server.PgConn) {
		for _, balance := range balances {
			var pendingByteCount int64
			server.Raise(conn.QueryRow(
				self.ctx,
				`SELECT COALESCE(SUM(debit_byte_count), 0) FROM service_debit_journal WHERE balance_id = $1 AND NOT applied`,
				server.Id(balance.BalanceId),
			).Scan(&pendingByteCount))
			balance.ByteCount -= pendingByteCount
		}
	})
	reading := &sidecartest.QuotaOracleReading{Balances: balances, UnbilledKnown: true}
	state, err := model.GetNetworkQuotaState(self.ctx, server.Id(networkId))
	if err != nil {
		return nil, err
	}
	if state != nil {
		reading.StoredByteCount = state.StoredByteCount
	}
	return reading, nil
}

func newT3Oracle(ctx context.Context) *sidecartest.QuotaOracle {
	return sidecartest.NewQuotaOracle(&t3OracleSource{ctx: ctx})
}

// Every token the balance's reservations hold in Redis, with its amount.
func t3Tokens(ctx context.Context, balanceId server.Id) map[string]string {
	var tokenAmounts map[string]string
	server.Redis(ctx, func(client server.RedisClient) {
		var err error
		tokenAmounts, err = client.HGetAll(ctx, "{escrow_"+balanceId.String()+"}approx_contracts").Result()
		server.Raise(err)
	})
	return tokenAmounts
}

// Every token of the balance reaches its lifetime: its expiry is set into the past, and one reservation
// asked and returned makes Redis find them.
func t3LapseTokens(ctx context.Context, balanceId server.Id) {
	server.Redis(ctx, func(client server.RedisClient) {
		tokens, err := client.ZRange(ctx, "{escrow_"+balanceId.String()+"}approx_expiry", 0, -1).Result()
		server.Raise(err)
		for _, token := range tokens {
			server.Raise(client.ZAddXX(ctx, "{escrow_"+balanceId.String()+"}approx_expiry", redis.Z{Score: 1, Member: token}).Err())
		}
	})
	redis := model.NewQuotaRateServerRedis(10 * time.Second)
	probe := model.QuotaLeaseToken(server.NewId(), 1)
	_, err := model.ReserveLease(ctx, redis, balanceId, probe, 1, 1, time.Minute, 2*time.Minute)
	server.Raise(err)
	server.Raise(model.ReleaseLease(ctx, redis, balanceId, probe, time.Minute))
}

// The producer rows' world: an evs producer on §20.6's defaults over a T3 target, waiting at its first
// window's end, and one network with a balance.
type fs44T3World struct {
	ctx       context.Context
	target    *quotasuite.Target
	metrics   *sidecartest.FakeMetrics
	oracle    *sidecartest.QuotaOracle
	batcher   *sidecar.QuotaUsageBatcher
	start     time.Time
	networkId connect.Id
	balanceId connect.Id
}

const fs44T3BalanceByteCount = int64(1 << 30)

func newFs44T3World(t *testing.T, ctx context.Context) *fs44T3World {
	target := newT3Target(ctx)(t)
	world := &fs44T3World{
		ctx:       ctx,
		target:    target,
		metrics:   sidecartest.NewFakeMetrics(nil),
		oracle:    newT3Oracle(ctx),
		start:     target.Clock.Now(),
		networkId: connect.NewId(),
		balanceId: connect.NewId(),
	}
	if err := target.SetBalances(world.networkId, []*sidecartest.QuotaBalance{{BalanceId: world.balanceId, ByteCount: fs44T3BalanceByteCount}}); err != nil {
		t.Fatal(err)
	}
	world.oracle.Grant(world.networkId, fs44T3BalanceByteCount)
	host := &sidecar.ServiceHost{Quota: target.Quota, Metrics: world.metrics, Clock: target.Clock}
	batcher, err := sidecar.NewQuotaUsageBatcher(ctx, host, sidecar.QuotaProducerEvs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(batcher.Close)
	world.batcher = batcher
	world.awaitTimerAt(t, world.at(60*time.Second))
	return world
}

func (self *fs44T3World) at(timeout time.Duration) time.Time {
	return self.start.Add(timeout)
}

// Returns once the clock has a timer pending at at. The real-time bound only fails a test that would hang.
func (self *fs44T3World) awaitTimerAt(t *testing.T, at time.Time) {
	t.Helper()
	for {
		pending, added := self.target.Clock.WatchTimers()
		if slices.ContainsFunc(pending, func(pendingTime time.Time) bool { return pendingTime.Equal(at) }) {
			return
		}
		select {
		case <-added:
		case <-time.After(30 * time.Second):
			t.Fatalf("no timer at %s, pending %v", at, pending)
		}
	}
}

// Moves the clock to the producer's timer at at, and returns once the round it starts has ended in the
// producer's next wait, at next, with FI-13 (a)'s upper bound checked there.
func (self *fs44T3World) step(t *testing.T, at time.Duration, next time.Duration) {
	t.Helper()
	self.awaitTimerAt(t, self.at(at))
	self.target.Clock.AdvanceTo(self.at(at))
	self.awaitTimerAt(t, self.at(next))
	self.check(t)
}

func (self *fs44T3World) check(t *testing.T) {
	t.Helper()
	if err := self.oracle.Check(self.ctx); err != nil {
		t.Fatal(err)
	}
}

// The debit flush, then FI-13 (a) whole.
func (self *fs44T3World) checkSettled(t *testing.T) {
	t.Helper()
	if err := self.target.Settle(); err != nil {
		t.Fatal(err)
	}
	if err := self.oracle.CheckSettled(self.ctx); err != nil {
		t.Fatal(err)
	}
}

func (self *fs44T3World) meter(t *testing.T, byteCount int64) {
	t.Helper()
	if err := self.batcher.AddTransfer(self.networkId, sidecar.QuotaMeterEvs, byteCount); err != nil {
		t.Fatal(err)
	}
	self.oracle.Meter(self.networkId, sidecar.QuotaMeterEvs, byteCount)
}

// What the flush has taken from the balance in Postgres.
func (self *fs44T3World) debited(t *testing.T) int64 {
	t.Helper()
	if err := self.target.Settle(); err != nil {
		t.Fatal(err)
	}
	balances := t3ReadBalances(self.ctx, server.Id(self.networkId))
	return fs44T3BalanceByteCount - balances[0].ByteCount
}

func (self *fs44T3World) batchCount() (count int) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT COUNT(*) FROM service_usage_batch`).Scan(&count))
	})
	return
}

func (self *fs44T3World) loseNextAnswer(t *testing.T) {
	t.Helper()
	if _, err := self.target.Faults.Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpHost, KeyPrefix: "quota.ReportUsage", Result: sidecartest.ApplyThenDrop}); err != nil {
		t.Fatal(err)
	}
}

func fs44T3KilledBeforeReportUsage(t *testing.T, ctx context.Context) {
	world := newFs44T3World(t, ctx)
	world.meter(t, 100)
	world.step(t, 60*time.Second, 120*time.Second)
	world.checkSettled(t)
	world.meter(t, 40)
	world.target.Clock.Advance(30 * time.Second)
	world.batcher.Close()
	world.oracle.Lose(world.networkId, sidecar.QuotaMeterEvs, 40)
	world.target.Clock.Advance(24 * time.Hour)
	world.checkSettled(t)
	if got := world.debited(t); got != 100 {
		t.Fatalf("killed before ReportUsage: %d debited, want the first window's 100", got)
	}
}

func fs44T3LostResponseRetried(t *testing.T, ctx context.Context) {
	world := newFs44T3World(t, ctx)
	before := world.batchCount()
	world.loseNextAnswer(t)
	world.meter(t, 100)
	world.step(t, 60*time.Second, 65*time.Second)
	world.step(t, 65*time.Second, 120*time.Second)
	world.checkSettled(t)
	if got := world.debited(t); got != 100 {
		t.Fatalf("a lost response retried: %d debited", got)
	}
	if got := world.batchCount() - before; got != 1 {
		t.Fatalf("a lost response retried: %d batch rows", got)
	}
	if got := world.metrics.Value("urnetwork_quota_usage_send_failures_total", sidecar.QuotaProducerEvs); got != 1 {
		t.Fatalf("a lost response retried: %v failed sends", got)
	}
	if unfired := world.target.Faults.Unfired(); len(unfired) != 0 {
		t.Fatalf("the lost answer never fired: %v", unfired)
	}
}

func fs44T3DrainLosesNothing(t *testing.T, ctx context.Context) {
	world := newFs44T3World(t, ctx)
	world.meter(t, 100)
	world.step(t, 60*time.Second, 120*time.Second)
	world.meter(t, 40)
	world.target.Clock.Advance(10 * time.Second)
	world.loseNextAnswer(t)
	drained := make(chan error, 1)
	go func() {
		drained <- world.batcher.Drain(ctx)
	}()
	world.awaitTimerAt(t, world.at(75*time.Second))
	world.check(t)
	world.target.Clock.AdvanceTo(world.at(75 * time.Second))
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the drain never returned")
	}
	world.batcher.Close()
	world.checkSettled(t)
	if got := world.debited(t); got != 140 {
		t.Fatalf("a drain: %d debited, want 140", got)
	}
}

func fs44T3ApiUnavailableElevenMinutes(t *testing.T, ctx context.Context) {
	world := newFs44T3World(t, ctx)
	before := world.batchCount()
	if _, err := world.target.Faults.Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpHost, KeyPrefix: "quota.ReportUsage", Result: sidecartest.FailBeforeApply, Repeat: 119}); err != nil {
		t.Fatal(err)
	}
	world.meter(t, 100)
	for at := 60 * time.Second; at < 660*time.Second; at += 5 * time.Second {
		world.step(t, at, at+5*time.Second)
	}
	world.step(t, 660*time.Second, 720*time.Second)
	world.oracle.Lose(world.networkId, sidecar.QuotaMeterEvs, 100)
	if got := world.metrics.Value("urnetwork_quota_usage_batches_dropped_total", sidecar.QuotaProducerEvs); got != 1 {
		t.Fatalf("api unavailable: dropped %v batches", got)
	}
	if got := world.metrics.Value("urnetwork_quota_usage_dropped_bytes_total", sidecar.QuotaProducerEvs); got != 100 {
		t.Fatalf("api unavailable: dropped %v bytes", got)
	}
	world.meter(t, 7)
	world.step(t, 720*time.Second, 780*time.Second)
	world.checkSettled(t)
	if got := world.batchCount() - before; got != 1 {
		t.Fatalf("api unavailable: %d batch rows, want the later window's one", got)
	}
	if got := world.debited(t); got != 7 {
		t.Fatalf("api unavailable: %d debited, want the later window's 7", got)
	}
}

// FS-44's producer rows on Postgres.
func TestFs44T3Producer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		ctx := t.Context()
		fs44T3KilledBeforeReportUsage(t, ctx)
		fs44T3LostResponseRetried(t, ctx)
		fs44T3DrainLosesNothing(t, ctx)
		fs44T3ApiUnavailableElevenMinutes(t, ctx)
	})
}

type fs44Post struct {
	batch  *sidecar.UsageBatch
	carves []*model.UsageCarve
}

// A host's QuotaSource whose next posts a schedule loses, and whose balance reading it can hold still.
type fs44Source struct {
	QuotaSource

	stateLock sync.Mutex
	// each next post's fate, first first: fs44PostKilled or fs44PostAnswerLost; past them posts answer
	plan  []string
	posts []*fs44Post
	// when set, what DrawOrder answers: the reading the host took before
	networkIdHeldBalances map[server.Id][]*LeaseBalance
}

const (
	// the host died after its carve: the post never left
	fs44PostKilled = "killed"
	// api applied the batch and the answer never came
	fs44PostAnswerLost = "answer-lost"
)

func (self *fs44Source) setPlan(plan ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.plan = plan
}

func (self *fs44Source) takePosts() []*fs44Post {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.posts)
}

func (self *fs44Source) ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error {
	fate := ""
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.posts = append(self.posts, &fs44Post{batch: batch, carves: carves})
		if 0 < len(self.plan) {
			fate = self.plan[0]
			self.plan = self.plan[1:]
		}
	}()
	if fate == fs44PostKilled {
		return errors.New("the host died before its post")
	}
	err := self.QuotaSource.ApplyUsageBatch(ctx, batch, carves)
	if err == nil && fate == fs44PostAnswerLost {
		return errors.New("the post's answer was lost")
	}
	return err
}

// Holds the network's reading where it is now: every later DrawOrder answers it.
func (self *fs44Source) holdReading(ctx context.Context, networkId server.Id) error {
	balances, err := self.QuotaSource.DrawOrder(ctx, networkId)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.networkIdHeldBalances == nil {
		self.networkIdHeldBalances = map[server.Id][]*LeaseBalance{}
	}
	self.networkIdHeldBalances[networkId] = balances
	return nil
}

func (self *fs44Source) DrawOrder(ctx context.Context, networkId server.Id) ([]*LeaseBalance, error) {
	self.stateLock.Lock()
	held, ok := self.networkIdHeldBalances[networkId]
	self.stateLock.Unlock()
	if ok {
		balances := []*LeaseBalance{}
		for _, balance := range held {
			copied := *balance
			balances = append(balances, &copied)
		}
		return balances, nil
	}
	return self.QuotaSource.DrawOrder(ctx, networkId)
}

// One host: a T3 target on a source the schedule holds.
func newFs44T3Host(t *testing.T, ctx context.Context) (*quotasuite.Target, *fs44Source) {
	var source *fs44Source
	target := newT3TargetOn(ctx, func(t *testing.T, clock sidecar.Clock) QuotaSource {
		source = &fs44Source{QuotaSource: NewLocalQuotaSource(clock)}
		return source
	})(t)
	return target, source
}

func fs44T3TlsBatch(networkId connect.Id, byteCount int64) *sidecar.UsageBatch {
	return &sidecar.UsageBatch{
		BatchId:  connect.NewId(),
		Producer: sidecar.QuotaProducerTlsEdge,
		Events:   []*sidecar.UsageEvent{{NetworkId: networkId, Meter: sidecar.QuotaMeterTls, TransferByteCount: byteCount}},
	}
}

// A pro network with one balance on the host, known to the oracle.
func fs44T3LeaseNetwork(t *testing.T, target *quotasuite.Target, oracle *sidecartest.QuotaOracle, byteCount int64) (networkId connect.Id, balanceId connect.Id) {
	t.Helper()
	networkId = connect.NewId()
	balanceId = connect.NewId()
	if err := target.SetLimits(networkId, &sidecar.QuotaLimits{Tier: "pro"}); err != nil {
		t.Fatal(err)
	}
	if err := target.SetBalances(networkId, []*sidecartest.QuotaBalance{{BalanceId: balanceId, ByteCount: byteCount}}); err != nil {
		t.Fatal(err)
	}
	oracle.Grant(networkId, byteCount)
	return
}

func fs44T3OpenLease(t *testing.T, ctx context.Context, target *quotasuite.Target, networkId connect.Id) sidecar.QuotaLease {
	t.Helper()
	lease, refusal, err := target.Quota.Lease(ctx, networkId, sidecar.QuotaMeterTls)
	if err != nil || refusal != nil {
		t.Fatalf("lease: %v, %+v", err, refusal)
	}
	return lease
}

// A host killed between a carve and its post: no debit, and the carved token is gone at its lifetime.
func fs44T3KilledBetweenCarveAndPost(t *testing.T, ctx context.Context) {
	target, source := newFs44T3Host(t, ctx)
	oracle := newT3Oracle(ctx)
	room := int64(1 << 20)
	networkId, balanceId := fs44T3LeaseNetwork(t, target, oracle, room)
	lease := fs44T3OpenLease(t, ctx, target, networkId)
	if !lease.Spend(3000) {
		t.Fatal("spend refused")
	}
	oracle.Meter(networkId, sidecar.QuotaMeterTls, 3000)
	source.setPlan(fs44PostKilled)
	if err := target.Quota.ReportUsage(ctx, fs44T3TlsBatch(networkId, 3000)); err == nil {
		t.Fatal("a post that never left answered nil")
	}
	posts := source.takePosts()
	if len(posts) != 1 || len(posts[0].carves) != 1 || posts[0].carves[0].ByteCount != 3000 {
		t.Fatalf("the carve before the kill: %+v", posts)
	}
	carveToken := model.QuotaCarveToken(posts[0].carves[0].UsageId)
	if got := t3Tokens(ctx, server.Id(balanceId))[carveToken]; got != "3000" {
		t.Fatalf("the carved token holds %q", got)
	}
	// the host is dead: its bytes are lost, nothing was journaled, and a flush finds nothing
	oracle.Lose(networkId, sidecar.QuotaMeterTls, 3000)
	if err := t3FlushServiceDebits(ctx, target.Clock); err != nil {
		t.Fatal(err)
	}
	if err := oracle.Check(ctx); err != nil {
		t.Fatal(err)
	}
	t3LapseTokens(ctx, server.Id(balanceId))
	if tokens := t3Tokens(ctx, server.Id(balanceId)); len(tokens) != 0 {
		t.Fatalf("tokens past their lifetime: %v", tokens)
	}
	if err := oracle.CheckSettled(ctx); err != nil {
		t.Fatal(err)
	}
	if balances := t3ReadBalances(ctx, server.Id(networkId)); balances[0].ByteCount != room || balances[0].ReservedByteCount != 0 {
		t.Fatalf("after the kill and the lifetime: %+v", balances[0])
	}
}

// A post whose answer is lost, retried; then a third copy of the carve and of the batch delivered after
// the flush: one debit, and the lease token moved once.
func fs44T3LostAnswerAndAThirdCopy(t *testing.T, ctx context.Context) {
	target, source := newFs44T3Host(t, ctx)
	oracle := newT3Oracle(ctx)
	room := int64(1 << 20)
	networkId, balanceId := fs44T3LeaseNetwork(t, target, oracle, room)
	lease := fs44T3OpenLease(t, ctx, target, networkId)
	if !lease.Spend(4000) {
		t.Fatal("spend refused")
	}
	oracle.Meter(networkId, sidecar.QuotaMeterTls, 4000)
	before := t3Tokens(ctx, server.Id(balanceId))
	if len(before) != 1 {
		t.Fatalf("the lease's tokens: %v", before)
	}
	var leaseToken string
	var leaseAmount string
	for leaseToken, leaseAmount = range before {
	}
	batch := fs44T3TlsBatch(networkId, 4000)
	source.setPlan(fs44PostAnswerLost)
	if err := target.Quota.ReportUsage(ctx, batch); err == nil {
		t.Fatal("a post whose answer was lost answered nil")
	}
	if err := oracle.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := target.Quota.ReportUsage(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := oracle.Check(ctx); err != nil {
		t.Fatal(err)
	}
	posts := source.takePosts()
	if len(posts) != 2 || len(posts[0].carves) != 1 || posts[0].carves[0].UsageId != posts[1].carves[0].UsageId {
		t.Fatalf("the retry's post: %d posts", len(posts))
	}
	carve := posts[0].carves[0]
	if err := t3FlushServiceDebits(ctx, target.Clock); err != nil {
		t.Fatal(err)
	}
	flushed := t3Tokens(ctx, server.Id(balanceId))
	if _, ok := flushed[model.QuotaCarveToken(carve.UsageId)]; ok {
		t.Fatalf("the flush left the carved token: %v", flushed)
	}
	// the third copy of the carve, as it was first sent
	expect, err := strconv.ParseInt(leaseAmount, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	redis := model.NewQuotaRateServerRedis(10 * time.Second)
	settings := t3LeaseSettings()
	moved, _, err := model.CarveLease(ctx, redis, server.Id(balanceId), carve.UsageId, []*model.LeaseCarvePart{{
		Token:           leaseToken,
		ByteCount:       model.ByteCount(carve.ByteCount),
		ExpectByteCount: model.ByteCount(expect),
	}}, settings.CarvedTokenTtl, settings.TokenTtl)
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatal("a third copy of the carve moved bytes after the flush")
	}
	after := t3Tokens(ctx, server.Id(balanceId))
	if after[leaseToken] != flushed[leaseToken] || len(after) != len(flushed) {
		t.Fatalf("the third copy changed the tokens: %v, were %v", after, flushed)
	}
	// and of the batch
	if err := target.Quota.ReportUsage(ctx, batch); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err := target.Settle(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckSettled(ctx); err != nil {
		t.Fatal(err)
	}
	if balances := t3ReadBalances(ctx, server.Id(networkId)); balances[0].ByteCount != room-4000 || balances[0].ReservedByteCount != 0 {
		t.Fatalf("after the third copies: %+v", balances[0])
	}
}

// FS-44's twin for balances: two hosts on one balance. The second took its reading before the first's
// debit was flushed and still holds it; the floor the flush left clamps its reservation to what the balance
// holds, and the balance ends at zero, never below.
func fs44T3TwoHostsOnOneBalance(t *testing.T, ctx context.Context) {
	first, _ := newFs44T3Host(t, ctx)
	second, secondSource := newFs44T3Host(t, ctx)
	oracle := newT3Oracle(ctx)
	draw := first.LeaseDrawByteCount
	room := draw + draw/2
	networkId, balanceId := fs44T3LeaseNetwork(t, first, oracle, room)
	if err := second.SetLimits(networkId, &sidecar.QuotaLimits{Tier: "pro"}); err != nil {
		t.Fatal(err)
	}
	// the second host reads the balance whole, and keeps that reading
	if err := secondSource.holdReading(ctx, server.Id(networkId)); err != nil {
		t.Fatal(err)
	}
	lease := fs44T3OpenLease(t, ctx, first, networkId)
	if !lease.Spend(draw) {
		t.Fatal("the first host's draw refused")
	}
	oracle.Meter(networkId, sidecar.QuotaMeterTls, draw)
	if err := first.Quota.ReportUsage(ctx, fs44T3TlsBatch(networkId, draw)); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err := first.Settle(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckSettled(ctx); err != nil {
		t.Fatal(err)
	}
	if balances := t3ReadBalances(ctx, server.Id(networkId)); balances[0].ByteCount != room-draw || balances[0].ReservedByteCount != 0 {
		t.Fatalf("after the first host's flush: %+v", balances[0])
	}
	// the second host reserves on its old reading of the whole balance
	secondLease := fs44T3OpenLease(t, ctx, second, networkId)
	if got := t3ReservedByteCount(ctx, server.Id(balanceId)); got != room-draw {
		t.Fatalf("the second host reserved %d of the %d the balance holds: the floor did not clamp it", got, room-draw)
	}
	if !secondLease.Spend(room - draw) {
		t.Fatal("the second host's clamped draw refused")
	}
	if secondLease.Spend(1) {
		t.Fatal("the second host spent past the balance")
	}
	oracle.Meter(networkId, sidecar.QuotaMeterTls, room-draw)
	if err := second.Quota.ReportUsage(ctx, fs44T3TlsBatch(networkId, room-draw)); err != nil {
		t.Fatal(err)
	}
	secondLease.Close()
	if err := second.Settle(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckSettled(ctx); err != nil {
		t.Fatal(err)
	}
	if balances := t3ReadBalances(ctx, server.Id(networkId)); balances[0].ByteCount != 0 || balances[0].ReservedByteCount != 0 {
		t.Fatalf("the balance ended at %+v, want zero", balances[0])
	}
}

// FS-44's lease rows on Postgres and Redis.
func TestFs44T3LeaseRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		ctx := t.Context()
		fs44T3KilledBetweenCarveAndPost(t, ctx)
		fs44T3LostAnswerAndAThirdCopy(t, ctx)
		fs44T3TwoHostsOnOneBalance(t, ctx)
	})
}
