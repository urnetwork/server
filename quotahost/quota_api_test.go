package quotahost

// QUOTA-M2-11: the Quota over a fake transport (NewApiQuota's tests): not ready before the first over-limit
// read, the last list kept and marked stale while api is away, every call failing closed with no transport,
// and the limits cache: a hit with no source call, a §21 tightening reaching the host within LimitsTtl and
// one load, a cold caller bounded by its own ctx, and a failed load not retried before LimitsTtl.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// api's internal surface as a test holds it.
type testingQuotaApi struct {
	stateLock      sync.Mutex
	networkLimits  map[server.Id]*sidecar.QuotaLimits
	overLimit      *sidecar.QuotaOverLimit
	down           bool
	limitsCalls    int
	block          chan struct{}
	appliedBatches []*sidecar.UsageBatch
}

func newTestingQuotaApi() *testingQuotaApi {
	return &testingQuotaApi{networkLimits: map[server.Id]*sidecar.QuotaLimits{}}
}

var errTestingApiDown = errors.New("api is down")

func (self *testingQuotaApi) setDown(down bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.down = down
}

func (self *testingQuotaApi) setLimits(networkId server.Id, limits *sidecar.QuotaLimits) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkLimits[networkId] = limits
}

func (self *testingQuotaApi) calls() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.limitsCalls
}

func (self *testingQuotaApi) DrawOrder(ctx context.Context, networkId server.Id) ([]*LeaseBalance, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.down {
		return nil, errTestingApiDown
	}
	return nil, nil
}

func (self *testingQuotaApi) MarkSpent(ctx context.Context, networkId server.Id) error {
	return nil
}

func (self *testingQuotaApi) LoadLimits(ctx context.Context, networkId server.Id) (*sidecar.QuotaLimits, error) {
	var block chan struct{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.limitsCalls += 1
		block = self.block
	}()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.down {
		return nil, errTestingApiDown
	}
	limits, ok := self.networkLimits[networkId]
	if !ok {
		return &sidecar.QuotaLimits{Tier: string(model.NetworkTierFree), Limits: map[string]int64{}}, nil
	}
	return cloneQuotaLimits(limits), nil
}

func (self *testingQuotaApi) LoadOverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.down || self.overLimit == nil {
		return nil, errTestingApiDown
	}
	return cloneQuotaOverLimit(self.overLimit), nil
}

func (self *testingQuotaApi) ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.down {
		return errTestingApiDown
	}
	self.appliedBatches = append(self.appliedBatches, batch)
	return nil
}

// A Redis no call here reaches.
type testingUnreachedRedis struct{}

func (testingUnreachedRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	return nil, errors.New("no Redis in this test")
}

func newTestingApiQuota(t testing.TB, api *testingQuotaApi) (*HostQuota, *sidecartest.FakeClock) {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	var transport QuotaApiTransport
	if api != nil {
		transport = api
	}
	quota, err := NewApiQuota(t.Context(), transport, testingUnreachedRedis{}, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { quota.Drain(context.Background()) })
	return quota, clock
}

// Not ready until the first over-limit list is read; while api is away the last list answers, marked stale,
// and the Quota stays ready, so the engine's own readiness decides.
func TestApiQuotaOverLimitKeepsItsLastListWhileApiIsAway(t *testing.T) {
	api := newTestingQuotaApi()
	quota, clock := newTestingApiQuota(t, api)
	if _, err := quota.OverLimit(t.Context()); err == nil {
		t.Fatal("an over-limit read with nothing to read answered")
	}
	connect.AssertEqual(t, quota.Ready(), ErrQuotaNotReady)
	spentId := connect.NewId()
	func() {
		api.stateLock.Lock()
		defer api.stateLock.Unlock()
		api.overLimit = &sidecar.QuotaOverLimit{
			StorageNetworkIdRefusals: map[connect.Id]*sidecar.QuotaRefusal{},
			SpentNetworkIdRefusals:   map[connect.Id]*sidecar.QuotaRefusal{spentId: {LimitKey: "data", Upgrade: true}},
		}
	}()
	overLimit, err := quota.OverLimit(t.Context())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, overLimit.SpentNetworkIdRefusals[spentId] != nil, true)
	connect.AssertEqual(t, quota.Ready(), nil)
	stale, _ := quota.OverLimitStale()
	connect.AssertEqual(t, stale, false)

	api.setDown(true)
	clock.Advance(7 * time.Second)
	overLimit, err = quota.OverLimit(t.Context())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, overLimit.SpentNetworkIdRefusals[spentId] != nil, true)
	stale, age := quota.OverLimitStale()
	connect.AssertEqual(t, stale, true)
	connect.AssertEqual(t, age, 7*time.Second)
	connect.AssertEqual(t, quota.Ready(), nil)
}

// A host with no transport fails closed on every call that needs api, and is never ready.
func TestApiQuotaWithNoTransportFailsClosed(t *testing.T) {
	quota, _ := newTestingApiQuota(t, nil)
	ctx := t.Context()
	networkId := connect.NewId()
	if _, err := quota.Limits(ctx, networkId); !errors.Is(err, ErrQuotaApiUnavailable) {
		t.Fatalf("limits answered %v", err)
	}
	if _, err := quota.OverLimit(ctx); !errors.Is(err, ErrQuotaApiUnavailable) {
		t.Fatalf("over-limit answered %v", err)
	}
	if _, err := quota.CountRate(ctx, networkId, map[string]int64{sidecar.QuotaRateKeyErasurePush: 1}, connect.NewId()); err == nil {
		t.Fatal("a rate counted with no limits")
	}
	if lease, refusal, err := quota.Lease(ctx, networkId, sidecar.QuotaMeterTls); err == nil || lease != nil || refusal != nil {
		t.Fatalf("a lease with no balances: %v, %+v, %v", lease, refusal, err)
	}
	batch := &sidecar.UsageBatch{BatchId: connect.NewId(), Producer: sidecar.QuotaProducerEvs}
	if err := quota.ReportUsage(ctx, batch); !errors.Is(err, ErrQuotaApiUnavailable) {
		t.Fatalf("a batch answered %v", err)
	}
	connect.AssertEqual(t, quota.Ready(), ErrQuotaNotReady)
}

// A cached network's limits answer with no source call however old they are, a refresh running behind:
// a tightening reaches the host within LimitsTtl and one load.
func TestQuotaLimitsServeTheCacheAndRefreshBehindIt(t *testing.T) {
	api := newTestingQuotaApi()
	quota, clock := newTestingApiQuota(t, api)
	ctx := t.Context()
	networkId := connect.NewId()
	api.setLimits(server.Id(networkId), &sidecar.QuotaLimits{Tier: string(model.NetworkTierPro), Limits: map[string]int64{"tls_endpoints": 10}})
	limits, err := quota.Limits(ctx, networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, limits.Limits["tls_endpoints"], int64(10))
	connect.AssertEqual(t, api.calls(), 1)

	// §21 tightens
	api.setLimits(server.Id(networkId), &sidecar.QuotaLimits{Tier: string(model.NetworkTierPro), Limits: map[string]int64{"tls_endpoints": 1}})
	clock.Advance(quota.settings.LimitsTtl - time.Millisecond)
	limits, _ = quota.Limits(ctx, networkId)
	connect.AssertEqual(t, limits.Limits["tls_endpoints"], int64(10))
	connect.AssertEqual(t, api.calls(), 1)
	clock.Advance(time.Millisecond)
	// stale: served, and a refresh starts behind it
	limits, _ = quota.Limits(ctx, networkId)
	connect.AssertEqual(t, limits.Limits["tls_endpoints"], int64(10))
	quota.limitsLoadGroup.Wait()
	connect.AssertEqual(t, api.calls(), 2)
	limits, _ = quota.Limits(ctx, networkId)
	connect.AssertEqual(t, limits.Limits["tls_endpoints"], int64(1))

	// the answer is the caller's own copy
	limits.Limits["tls_endpoints"] = 99
	again, _ := quota.Limits(ctx, networkId)
	connect.AssertEqual(t, again.Limits["tls_endpoints"], int64(1))
}

// A network with nothing cached waits for its first load only as long as the caller's ctx allows, and a
// load that failed is not tried again before LimitsTtl: its error answers meanwhile.
func TestQuotaLimitsColdCallerIsBoundedByItsContext(t *testing.T) {
	api := newTestingQuotaApi()
	api.block = make(chan struct{})
	quota, clock := newTestingApiQuota(t, api)
	networkId := connect.NewId()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := quota.Limits(ctx, networkId); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cold caller whose ctx ended answered %v", err)
	}
	// the load runs on: a caller with time gets its answer
	func() {
		api.stateLock.Lock()
		defer api.stateLock.Unlock()
		close(api.block)
		api.block = nil
	}()
	limits, err := quota.Limits(t.Context(), networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, limits.Tier, string(model.NetworkTierFree))
	connect.AssertEqual(t, api.calls(), 1)

	other := connect.NewId()
	api.setDown(true)
	if _, err := quota.Limits(t.Context(), other); !errors.Is(err, errTestingApiDown) {
		t.Fatalf("a failed load answered %v", err)
	}
	calls := api.calls()
	api.setDown(false)
	if _, err := quota.Limits(t.Context(), other); !errors.Is(err, errTestingApiDown) {
		t.Fatalf("a load retried at once answered %v", err)
	}
	connect.AssertEqual(t, api.calls(), calls)
	clock.Advance(quota.settings.LimitsTtl)
	limits, err = quota.Limits(t.Context(), other)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, limits.Tier, string(model.NetworkTierFree))
}

// A batch reaches api through the transport once its lease half posts it, and a zero id is refused before.
func TestApiQuotaReportsUsageThroughItsTransport(t *testing.T) {
	api := newTestingQuotaApi()
	quota, _ := newTestingApiQuota(t, api)
	batch := &sidecar.UsageBatch{
		BatchId:  connect.NewId(),
		Producer: sidecar.QuotaProducerEvs,
		Events:   []*sidecar.UsageEvent{{NetworkId: connect.NewId(), Meter: sidecar.QuotaMeterEvs, TransferByteCount: 10}},
	}
	connect.AssertEqual(t, quota.ReportUsage(t.Context(), batch), nil)
	connect.AssertEqual(t, len(api.appliedBatches), 1)
	connect.AssertEqual(t, api.appliedBatches[0].Events[0].TransferByteCount, int64(10))
	if err := quota.ReportUsage(t.Context(), &sidecar.UsageBatch{Producer: sidecar.QuotaProducerEvs}); err == nil {
		t.Fatal("a zero batch id was posted")
	}
}
