package quotahost

// QUOTA-M2-11: the Quota over a fake transport (NewApiQuota's tests): not ready before the first over-limit
// read, the last list kept and marked stale while api is away, every call failing closed with no transport,
// and the limits cache: a hit with no source call, a §21 tightening reaching the host within LimitsTtl and
// one load, a cold caller bounded by its own ctx, and a failed load not retried before LimitsTtl.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A stub of api's internal surface: each request recorded, each route answering what the test set.
type testingHttpQuotaApi struct {
	stateLock     sync.Mutex
	authorization []string
	paths         []string
	status        int
	body          map[string]string
	batches       []*QuotaApiUsageBatch
}

func (self *testingHttpQuotaApi) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.authorization = append(self.authorization, r.Header.Get("Authorization"))
	self.paths = append(self.paths, r.Method+" "+r.URL.Path)
	if r.URL.Path == QuotaApiUsageBatchPath {
		batch := &QuotaApiUsageBatch{}
		if err := json.NewDecoder(r.Body).Decode(batch); err != nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		self.batches = append(self.batches, batch)
	}
	if self.status != 0 && self.status != http.StatusOK {
		http.Error(w, "refused, as the test asked", self.status)
		return
	}
	for prefix, body := range self.body {
		if strings.HasPrefix(r.URL.Path, prefix) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
			return
		}
	}
	w.Write([]byte("{}"))
}

func (self *testingHttpQuotaApi) setStatus(status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.status = status
}

func (self *testingHttpQuotaApi) setBody(pathPrefix string, body string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.body[pathPrefix] = body
}

func newTestingHttpQuotaApi(t testing.TB, settings *QuotaApiSettings) (*testingHttpQuotaApi, QuotaApiTransport, string) {
	t.Helper()
	stub := &testingHttpQuotaApi{body: map[string]string{}}
	httpServer := httptest.NewServer(stub)
	t.Cleanup(httpServer.Close)
	const bearer = "synthetic-test-bearer-0123456789abcdefghijklmn"
	transport, err := NewHttpQuotaApi(httpServer.URL+"/", bearer, httpServer.Client(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return stub, transport, bearer
}

// Each call reaches its route with the host's bearer, and api's answers come back as the Quota's types.
func TestHttpQuotaApiCallsEachRouteWithItsBearer(t *testing.T) {
	stub, transport, bearer := newTestingHttpQuotaApi(t, nil)
	ctx := t.Context()
	networkId := server.NewId()
	balanceId := server.NewId()
	spentId := server.NewId()
	endTime := time.Unix(1_800_000_000, 0).UTC()
	stub.setBody(QuotaApiLimitsPath, `{"tier":"pro","limits":{"tls_endpoints":10}}`)
	stub.setBody(QuotaApiOverLimitPath, `{"storage":[],"spent":[{"network_id":"`+spentId.String()+`","refusal":{"limit_key":"data","limit":0,"used":0,"retry_after_ms":0,"upgrade":true}}]}`)
	stub.setBody(QuotaApiDrawOrderPath, `{"balances":[{"balance_id":"`+balanceId.String()+`","credit_byte_count":-5,"credit":true,"end_time":"`+endTime.Format(time.RFC3339)+`"}]}`)

	limits, err := transport.LoadLimits(ctx, networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, limits, &sidecar.QuotaLimits{Tier: "pro", Limits: map[string]int64{"tls_endpoints": 10}})
	overLimit, err := transport.LoadOverLimit(ctx)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(overLimit.StorageNetworkIdRefusals), 0)
	connect.AssertEqual(t, overLimit.SpentNetworkIdRefusals[connect.Id(spentId)].Upgrade, true)
	balances, err := transport.DrawOrder(ctx, networkId)
	connect.AssertEqual(t, err, nil)
	// a negative credit never reaches a reservation
	connect.AssertEqual(t, balances, []*LeaseBalance{{BalanceId: balanceId, CreditByteCount: 0, IsCredit: true, EndTime: endTime}})
	connect.AssertEqual(t, transport.MarkSpent(ctx, networkId), nil)
	batch := &sidecar.UsageBatch{
		BatchId:     connect.NewId(),
		Producer:    sidecar.QuotaProducerTlsEdge,
		WindowStart: endTime,
		Events:      []*sidecar.UsageEvent{{NetworkId: connect.Id(networkId), Meter: sidecar.QuotaMeterTls, TransferByteCount: 7}},
	}
	carves := []*model.UsageCarve{{UsageId: server.NewId(), NetworkId: networkId, Meter: sidecar.QuotaMeterTls, BalanceId: balanceId, ByteCount: 7}}
	connect.AssertEqual(t, transport.ApplyUsageBatch(ctx, batch, carves), nil)

	stub.stateLock.Lock()
	defer stub.stateLock.Unlock()
	connect.AssertEqual(t, stub.paths, []string{
		"GET " + QuotaApiLimitsPath + networkId.String(),
		"GET " + QuotaApiOverLimitPath,
		"GET " + QuotaApiDrawOrderPath + networkId.String(),
		"POST " + QuotaApiSpentPath + networkId.String(),
		"POST " + QuotaApiUsageBatchPath,
	})
	for _, authorization := range stub.authorization {
		connect.AssertEqual(t, authorization, "Bearer "+bearer)
	}
	// the batch arrives as it was sent, carves and all
	sent, sentCarves, err := stub.batches[0].UsageBatch()
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, sent, batch)
	connect.AssertEqual(t, sentCarves, carves)
}

// Every answer but a 200 the Quota can use fails the call with ErrQuotaApiRefused, whose text names the route
// and never the bearer or a network.
func TestHttpQuotaApiRefusalsFailTheCall(t *testing.T) {
	stub, transport, bearer := newTestingHttpQuotaApi(t, &QuotaApiSettings{MaxResponseByteCount: 256, MaxRefusalByteCount: 64})
	ctx := t.Context()
	networkId := server.NewId()
	check := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrQuotaApiRefused) {
			t.Fatalf("answered %v", err)
		}
		if strings.Contains(err.Error(), bearer) || strings.Contains(err.Error(), networkId.String()) {
			t.Fatalf("an error quoted the bearer or the network: %v", err)
		}
	}
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable, http.StatusBadRequest} {
		stub.setStatus(status)
		_, err := transport.LoadLimits(ctx, networkId)
		check(err)
		_, err = transport.LoadOverLimit(ctx)
		check(err)
		_, err = transport.DrawOrder(ctx, networkId)
		check(err)
		check(transport.MarkSpent(ctx, networkId))
		check(transport.ApplyUsageBatch(ctx, &sidecar.UsageBatch{BatchId: connect.NewId(), Producer: sidecar.QuotaProducerEvs}, nil))
	}
	stub.setStatus(http.StatusOK)
	// answers that do not parse, or that the Quota cannot use
	stub.setBody(QuotaApiLimitsPath, `{"limits":{}}`)
	_, err := transport.LoadLimits(ctx, networkId)
	check(err)
	stub.setBody(QuotaApiLimitsPath, `{"tier":`)
	_, err = transport.LoadLimits(ctx, networkId)
	check(err)
	stub.setBody(QuotaApiLimitsPath, `{"tier":"free","limits":{"`+strings.Repeat("k", 300)+`":1}}`)
	_, err = transport.LoadLimits(ctx, networkId)
	check(err)
	twice := `{"network_id":"` + networkId.String() + `","refusal":{"limit_key":"data"}}`
	stub.setBody(QuotaApiOverLimitPath, `{"storage":[`+twice+`,`+twice+`],"spent":[]}`)
	_, err = transport.LoadOverLimit(ctx)
	check(err)
	stub.setBody(QuotaApiOverLimitPath, `{"storage":[{"network_id":"`+networkId.String()+`"}],"spent":[]}`)
	_, err = transport.LoadOverLimit(ctx)
	check(err)
	stub.setBody(QuotaApiDrawOrderPath, `{"balances":[{"credit_byte_count":5}]}`)
	_, err = transport.DrawOrder(ctx, networkId)
	check(err)
}

// A transport needs a url and a bearer, and a block with no credential builds no Quota, so it stays not ready.
func TestBlockQuotaNeedsItsUrlAndCredential(t *testing.T) {
	if _, err := NewHttpQuotaApi("api.example", "bearer", nil, nil); err == nil {
		t.Fatal("a url with no scheme was taken")
	}
	if _, err := NewHttpQuotaApi("https://api.example", "", nil, nil); err == nil {
		t.Fatal("no bearer was taken")
	}
	if quota, err := NewBlockQuota(t.Context(), server.InternalGateCallerEvs, "", nil); err == nil || quota != nil {
		t.Fatal("a block with no api url built a Quota")
	}
	// the test vault holds no internal-gate-evs.yml
	if quota, err := NewBlockQuota(t.Context(), server.InternalGateCallerEvs, "https://api.example", nil); !errors.Is(err, server.ErrInternalGateCredentialRefused) || quota != nil {
		t.Fatalf("a block with no credential answered %v", err)
	}
}
