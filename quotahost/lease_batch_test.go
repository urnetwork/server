package quotahost

// QUOTA-M2-07: the lease half of ReportUsage (DESIGN-DETAILS 13.2.2's a batch, 13.6's quotahost/lease_test.go
// row): a batch's plan kept by its id through its retries; a carve whose sources are gone dropping its part
// unbilled; a carve whose answer was lost repeated as it was and moving once; a plan given up at its horizon;
// and the post carrying each lease meter's carved sum and no more, against the private stack's Redis.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The posts a test's batches make, and whether the next ones fail after being sent.
type testingLeasePoster struct {
	failCount int
	posts     []*testingLeasePost
}

type testingLeasePost struct {
	batch  *sidecar.UsageBatch
	carves []*model.UsageCarve
}

func (self *testingLeasePoster) post(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error {
	self.posts = append(self.posts, &testingLeasePost{batch: batch, carves: carves})
	if 0 < self.failCount {
		self.failCount -= 1
		return errors.New("the post's answer was lost")
	}
	return nil
}

func testingTlsBatch(batchId connect.Id, networkId server.Id, byteCount int64) *sidecar.UsageBatch {
	return &sidecar.UsageBatch{
		BatchId:  batchId,
		Producer: sidecar.QuotaProducerTlsEdge,
		Events:   []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterTls, TransferByteCount: byteCount}},
	}
}

func testingCarvedSum(carves []*model.UsageCarve) int64 {
	sum := int64(0)
	for _, carve := range carves {
		sum += carve.ByteCount
	}
	return sum
}

func testingUnbilled() float64 {
	return testutil.ToFloat64(quotaLeaseUnbilledByteCounter.WithLabelValues(sidecar.QuotaMeterTls))
}

// Twin LeaseTwin-Replan: a batch is planned once and keeps its plan through its retries, so bytes spent after
// its first try are not attributed to it, and a batch sent again after its post answered is posted as it was,
// never planned again.
func TestBatchPlanKeepsItsId(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		balance := testingLeaseBalance(5000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, handle.Spend(300), true)

		poster := &testingLeasePoster{failCount: 1}
		batchId := connect.NewId()
		// the engine counted 500 while the lease had spent 300: the plan takes the 300 it can attribute
		if err := fixture.set.reportUsage(ctx, testingTlsBatch(batchId, networkId, 500), poster.post); err == nil {
			t.Fatal("a post whose answer was lost answered nil")
		}
		connect.AssertEqual(t, handle.Spend(200), true)
		connect.AssertEqual(t, fixture.set.reportUsage(ctx, testingTlsBatch(batchId, networkId, 500), poster.post), nil)
		connect.AssertEqual(t, len(poster.posts), 2)
		for _, post := range poster.posts {
			connect.AssertEqual(t, testingCarvedSum(post.carves), int64(300))
			connect.AssertEqual(t, post.batch.Events[0].TransferByteCount, int64(300))
			connect.AssertEqual(t, post.carves[0].UsageId, model.ServiceUsageId(server.Id(batchId), networkId, sidecar.QuotaMeterTls, balance.BalanceId))
		}
		// the carve ran once: 300 carved and 700 left of the draw's token
		tokens := testingLeaseTokens(ctx, balance.BalanceId)
		connect.AssertEqual(t, tokens[model.QuotaCarveToken(poster.posts[0].carves[0].UsageId)], "300")
		connect.AssertEqual(t, tokens[model.QuotaLeaseToken(handle.entry.leaseId, 1)], "700")

		// sent again after its post answered: posted as it was, nothing planned or carved
		calls := fixture.redis.callCount()
		connect.AssertEqual(t, fixture.set.reportUsage(ctx, testingTlsBatch(batchId, networkId, 500), poster.post), nil)
		connect.AssertEqual(t, fixture.redis.callCount(), calls)
		connect.AssertEqual(t, len(poster.posts), 2)

		// the next batch takes the 200 spent since
		next := &testingLeasePoster{}
		connect.AssertEqual(t, fixture.set.reportUsage(ctx, testingTlsBatch(connect.NewId(), networkId, 200), next.post), nil)
		connect.AssertEqual(t, testingCarvedSum(next.posts[0].carves), int64(200))
		connect.AssertEqual(t, testingLeaseTokens(ctx, balance.BalanceId)[model.QuotaLeaseToken(handle.entry.leaseId, 1)], "500")
		handle.Close()
	})
}

// A carve whose sources are gone answers {0, 0}: its part is dropped from the plan and counted unbilled, its
// draw is dropped as expired, and the post carries no carve and no bytes for it (QD-653: no lease byte is
// debited without its carve).
func TestLeaseCarveOfAGoneTokenDropsItsPart(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		balance := testingLeaseBalance(5000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, handle.Spend(100), true)
		// Redis lost the balance's reservations, as at a failover
		server.Redis(ctx, func(client server.RedisClient) {
			base := "{escrow_" + balance.BalanceId.String() + "}"
			server.Raise(client.Del(ctx, base+"approx_reserved", base+"approx_contracts", base+"approx_expiry").Err())
		})
		unbilled := testingUnbilled()
		poster := &testingLeasePoster{}
		connect.AssertEqual(t, fixture.set.reportUsage(ctx, testingTlsBatch(connect.NewId(), networkId, 100), poster.post), nil)
		connect.AssertEqual(t, len(poster.posts), 1)
		connect.AssertEqual(t, len(poster.posts[0].carves), 0)
		connect.AssertEqual(t, poster.posts[0].batch.Events[0].TransferByteCount, int64(0))
		connect.AssertEqual(t, testingUnbilled()-unbilled, float64(100))
		// the draw is gone: nothing left to spend, and no token was written
		connect.AssertEqual(t, handle.entry.available.Load(), int64(0))
		connect.AssertEqual(t, handle.Spend(1), false)
		connect.AssertEqual(t, len(testingLeaseTokens(ctx, balance.BalanceId)), 0)
	})
}

// A carve whose answer was lost is repeated with the same arguments before any other operation names its
// token, and moves once: the retry finds its destination (13.2.2's case 1), and the lease token keeps what
// the first try left.
func TestLeaseCarveRepeatedMovesOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		balance := testingLeaseBalance(5000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, handle.Spend(400), true)
		poster := &testingLeasePoster{}
		batch := testingTlsBatch(connect.NewId(), networkId, 400)
		fixture.redis.dropReplies.Store(1)
		if err := fixture.set.reportUsage(ctx, batch, poster.post); err == nil {
			t.Fatal("a carve whose answer was lost answered nil")
		}
		connect.AssertEqual(t, len(poster.posts), 0)
		// an extend due meanwhile repeats the carve first, as it was
		fixture.clock.Advance(fixture.set.settings.ExtendInterval)
		calls := fixture.redis.callCount()
		testingTick(t, handle)
		tickCalls := fixture.redis.calls()[calls:]
		connect.AssertEqual(t, len(tickCalls), 2)
		connect.AssertEqual(t, tickCalls[0][:len("carve")], "carve")
		connect.AssertEqual(t, tickCalls[1], "extend "+model.QuotaLeaseToken(handle.entry.leaseId, 1))
		connect.AssertEqual(t, fixture.set.reportUsage(ctx, batch, poster.post), nil)
		connect.AssertEqual(t, len(poster.posts), 1)
		connect.AssertEqual(t, testingCarvedSum(poster.posts[0].carves), int64(400))
		tokens := testingLeaseTokens(ctx, balance.BalanceId)
		connect.AssertEqual(t, tokens[model.QuotaLeaseToken(handle.entry.leaseId, 1)], "600")
		connect.AssertEqual(t, tokens[model.QuotaCarveToken(poster.posts[0].carves[0].UsageId)], "400")
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(1000))
		handle.Close()
		// close keeps nothing of the draw: its 600 return, the 400 stay carved for the flush
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(400))
	})
}

// A plan its batcher stopped sending is given up at PlanHorizon: its uncarved part is counted unbilled and
// leaves the token as the host knows it, a carve sent and unanswered makes the token's next operation an
// extend, whose answer the host takes, and the dead bytes return at close.
func TestLeasePlanGivenUpAtItsHorizon(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		balance := testingLeaseBalance(5000, false, time.Now().Add(time.Hour))
		fixture.balances.set(networkId, balance)
		handle := testingOpenLease(t, fixture, networkId)
		connect.AssertEqual(t, handle.Spend(250), true)
		poster := &testingLeasePoster{}
		fixture.redis.failing.Store(true)
		if err := fixture.set.reportUsage(ctx, testingTlsBatch(connect.NewId(), networkId, 250), poster.post); err == nil {
			t.Fatal("a carve Redis never answered answered nil")
		}
		fixture.redis.failing.Store(false)
		unbilled := testingUnbilled()
		fixture.clock.Advance(fixture.set.settings.PlanHorizon)
		// any batch gives up the plans past their horizon first
		other := &testingLeasePoster{}
		connect.AssertEqual(t, fixture.set.reportUsage(ctx, testingTlsBatch(connect.NewId(), networkId, 0), other.post), nil)
		connect.AssertEqual(t, testingUnbilled()-unbilled, float64(250))
		handle.entry.stateLock.Lock()
		draw := handle.entry.draws[0]
		needsExtend := draw.needsExtend
		handle.entry.stateLock.Unlock()
		connect.AssertEqual(t, needsExtend, true)
		// the extend learns the token kept its 1000: the 250 the abandoned carve would have moved are dead
		testingTick(t, handle)
		handle.entry.stateLock.Lock()
		connect.AssertEqual(t, draw.amount, int64(1000))
		connect.AssertEqual(t, draw.deadByteCount, int64(250))
		connect.AssertEqual(t, draw.needsExtend, false)
		handle.entry.stateLock.Unlock()
		connect.AssertEqual(t, handle.entry.available.Load(), int64(750))
		handle.Close()
		connect.AssertEqual(t, testingLeaseReserved(ctx, balance.BalanceId), int64(0))
		connect.AssertEqual(t, len(poster.posts), 0)
	})
}

// A batch's other meters pass through as the engine counted them; a lease meter's event carries its carved
// sum and no more, so ApplyUsageBatch's rule that a lease meter's bytes equal its carves holds.
func TestLeasePostCarriesTheCarvedSum(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newTestingLeaseFixture(t, nil)
		networkId := server.NewId()
		fixture.balances.set(networkId, testingLeaseBalance(5000, false, time.Now().Add(time.Hour)))
		handle := testingOpenLease(t, fixture, networkId)
		defer handle.Close()
		connect.AssertEqual(t, handle.Spend(120), true)
		batch := &sidecar.UsageBatch{
			BatchId:  connect.NewId(),
			Producer: sidecar.QuotaProducerTlsEdge,
			Events: []*sidecar.UsageEvent{
				{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterTls, TransferByteCount: 100},
				{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterTls, TransferByteCount: 100},
				{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterEvs, StoredByteDelta: 7},
			},
		}
		poster := &testingLeasePoster{}
		connect.AssertEqual(t, fixture.set.reportUsage(ctx, batch, poster.post), nil)
		sent := poster.posts[0].batch
		connect.AssertEqual(t, sent.BatchId, batch.BatchId)
		connect.AssertEqual(t, sent.Events[0].TransferByteCount, int64(100))
		connect.AssertEqual(t, sent.Events[1].TransferByteCount, int64(20))
		connect.AssertEqual(t, sent.Events[2], batch.Events[2])
		connect.AssertEqual(t, testingCarvedSum(poster.posts[0].carves), int64(120))
		// the engine's batch is left as it was
		connect.AssertEqual(t, batch.Events[1].TransferByteCount, int64(100))
	})
}
