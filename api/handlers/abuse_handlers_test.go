//go:build sidecar_pending

package handlers

// §21.16's customer routes over the router (abuse_handlers.go; ABUSE-M5-11) on a private database and Redis: the
// report route's one answer whether or not a report joined, its limit on a fake clock with infrastructure
// uncounted, the limit shared in Redis by two api instances with no address kept there and a count not kept
// refusing (owner decision 536, QD-895), its refusals, the appeal route's answers for an unknown, a barred and
// another network's case alike, and the cases and status bodies. The router's class gates are api's
// (route_authz_test.go); the handlers' own refusal of a client token on cases and appeal is here.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

const testAbuseRouteRoot = "root.example"

// One call of the four routes on handlers, from remoteAddr with authorization.
func testAbuseRouteCall(t testing.TB, handlers *AbuseHandlers, method string, path string, authorization string, remoteAddr string, body string) *testAbuseRouteAnswer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("POST", "/abuse/report", handlers.Report),
		router.NewRoute("GET", "/abuse/cases", handlers.Cases),
		router.NewRoute("POST", "/abuse/appeal", handlers.Appeal),
		router.NewRoute("GET", "/abuse/status", handlers.Status),
	})
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = remoteAddr
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	return &testAbuseRouteAnswer{status: w.Code, body: w.Body.String(), header: w.Header()}
}

// A report body of urls in category.
func testAbuseReportBody(t testing.TB, category string, urls []string, headerBlocks []string) string {
	t.Helper()
	body, err := json.Marshal(&AbuseReportArgs{Category: category, Urls: urls, HeaderBlocks: headerBlocks})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Takes reports without a store, counting them.
type testAbuseReportSink struct {
	stateLock sync.Mutex
	reports   []*model.AbuseReport
}

func (self *testAbuseReportSink) accept(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reports = append(self.reports, report)
	return &controller.AbuseIntakeResult{}, nil
}

func (self *testAbuseReportSink) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.reports)
}

// The number of abuse_case rows.
func testAbuseRouteCaseCount(t testing.TB, ctx context.Context) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_case`).Scan(&count))
	})
	return
}

// A network and its two bearers: its own credential, and a client's token.
type testAbuseRouteNetwork struct {
	networkId     server.Id
	networkBearer string
	clientBearer  string
}

func newTestAbuseRouteNetwork(t testing.TB, ctx context.Context) *testAbuseRouteNetwork {
	t.Helper()
	networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "abuse"+strings.ReplaceAll(networkId.String()[24:], "-", ""), userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "abuse", "abuse")
	byJwt := session.NewByJwt(networkId, userId, "abuse", false, false)
	return &testAbuseRouteNetwork{
		networkId:     networkId,
		networkBearer: "Bearer " + byJwt.Testing_Sign(),
		clientBearer:  "Bearer " + byJwt.Client(deviceId, clientId).Testing_Sign(),
	}
}

// A case of networkId on host with a person's host kill at actionTime, so the network was told of it.
func testAbuseRouteNoticedCase(t testing.TB, ctx context.Context, networkId server.Id, host string, actionTime time.Time) (caseId server.Id) {
	t.Helper()
	settings := model.DefaultAbuseSettings()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      model.AbuseCategoryPhishing,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindHost, Name: host},
			ReporterClass: model.AbuseReporterClassWebRisk,
			JoinKind:      model.AbuseJoinKindHost,
			JoinKey:       host,
		}, actionTime, settings)
		if err != nil {
			return
		}
		reviewerId := server.NewId()
		_, _, _, err = model.RecordAbuseActionInTx(ctx, tx, &model.AbuseActionArgs{
			ActionId:   server.NewId(),
			CaseId:     caseId,
			Kind:       model.AbuseActionKindHostKill,
			Target:     host,
			ReviewerId: &reviewerId,
		}, actionTime, settings)
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// The answer to a report is one body whether or not the report joined a URnetwork name, and only a joined report
// leaves a case.
func TestAbuseReportAnswersAlikeJoinedOrNot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushAbuseConfig(fmt.Sprintf("protected_names: {roots: [%s], platform: [platform.example]}\n", testAbuseRouteRoot))()
		networkId := server.NewId()
		label, err := model.EnsureNetworkLabel(ctx, networkId, testAbuseRouteRoot)
		if err != nil {
			t.Fatal(err)
		}
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			RateLimitExcluded: func(addr netip.Addr) bool { return false },
		})

		joined := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.10:41000", testAbuseReportBody(t, "phishing", []string{
			"https://shop." + label + "." + testAbuseRouteRoot + "/canarypathzq7?canaryqueryzq7=1",
		}, nil))
		if joined.status != http.StatusOK || testAbuseRouteCaseCount(t, ctx) != 1 {
			t.Fatalf("a joined report: %d %q, %d cases", joined.status, joined.body, testAbuseRouteCaseCount(t, ctx))
		}
		for _, body := range []string{
			testAbuseReportBody(t, "phishing", []string{"https://shop.nolabelholdsthis." + testAbuseRouteRoot + "/login"}, nil),
			testAbuseReportBody(t, "malware", []string{"https://elsewhere.example/"}, nil),
			testAbuseReportBody(t, "spam", nil, []string{"From: someone@elsewhere.example\nSubject: hello\n\n"}),
		} {
			unjoined := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.10:41000", body)
			if unjoined.status != joined.status || unjoined.body != joined.body {
				t.Fatalf("an unjoined report answered %d %q, a joined one %d %q", unjoined.status, unjoined.body, joined.status, joined.body)
			}
		}
		if count := testAbuseRouteCaseCount(t, ctx); count != 1 {
			t.Fatalf("unjoined reports left %d cases", count)
		}

		var rows string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(string_agg(row_to_json(abuse_case)::text, ' '), '') FROM abuse_case`).Scan(&rows))
		})
		if strings.Contains(rows, "canarypathzq7") || strings.Contains(rows, "canaryqueryzq7") || strings.Contains(rows, "192.0.2.10") || !strings.Contains(rows, networkId.String()) {
			t.Fatalf("the case kept more than the join: %s", rows)
		}
	})
}

// A caller address hash's reports are limited on the clock the routes run on: a /29 counts as one caller, a
// refused report says when to retry, the window's end frees it, and an infrastructure address counts nothing.
func TestAbuseReportLimitOnAFakeClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clock := &testAbuseRouteClock{now: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
		sink := &testAbuseReportSink{}
		settings := DefaultAbuseRouteSettings()
		settings.ReportAddressLimit = 3
		settings.ReportAddressLookback = time.Hour
		excluded := netip.MustParsePrefix("198.51.100.0/24")
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			Now:               clock.Now,
			RateLimitExcluded: func(addr netip.Addr) bool { return excluded.Contains(addr) },
			AcceptReport:      sink.accept,
			Settings:          settings,
		})
		body := testAbuseReportBody(t, "phishing", []string{"https://elsewhere.example/"}, nil)
		report := func(remoteAddr string) *testAbuseRouteAnswer {
			return testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", remoteAddr, body)
		}

		for i := range 3 {
			if answer := report("192.0.2.10:41000"); answer.status != http.StatusOK {
				t.Fatalf("report %d: %d %q", i, answer.status, answer.body)
			}
		}
		refused := report("192.0.2.10:41001")
		if refused.status != http.StatusTooManyRequests || refused.header.Get("Retry-After") != "3600" {
			t.Fatalf("past the limit: %d, Retry-After %q", refused.status, refused.header.Get("Retry-After"))
		}
		// the same /29 is the same caller; the next /29 is another
		if answer := report("192.0.2.13:41000"); answer.status != http.StatusTooManyRequests {
			t.Fatalf("an address of the same /29: %d", answer.status)
		}
		if answer := report("192.0.2.17:41000"); answer.status != http.StatusOK {
			t.Fatalf("an address of the next /29: %d", answer.status)
		}
		if sink.count() != 4 {
			t.Fatalf("%d reports taken, want the four inside the limit", sink.count())
		}

		// a refused report counts too: one made just before the window's end keeps the caller past the limit
		clock.Advance(time.Hour - time.Minute)
		for range 3 {
			report("192.0.2.10:41000")
		}
		clock.Advance(time.Minute)
		if answer := report("192.0.2.10:41000"); answer.status != http.StatusTooManyRequests {
			t.Fatalf("with refusals inside the window: %d", answer.status)
		}
		clock.Advance(time.Hour)
		if answer := report("192.0.2.10:41000"); answer.status != http.StatusOK {
			t.Fatalf("past the window: %d", answer.status)
		}

		// IPv6 counts by /56
		for range 3 {
			report("[2001:db8:1:100::1]:41000")
		}
		if answer := report("[2001:db8:1:1ff::2]:41000"); answer.status != http.StatusTooManyRequests {
			t.Fatalf("an address of the same /56: %d", answer.status)
		}

		before := sink.count()
		for i := range 10 {
			if answer := report("198.51.100.7:41000"); answer.status != http.StatusOK {
				t.Fatalf("infrastructure report %d: %d", i, answer.status)
			}
		}
		if sink.count() != before+10 {
			t.Fatalf("infrastructure reports were not all taken")
		}
	})
}

// The report count's histories in the test's Redis, sorted.
func testAbuseReportLimitKeys(t testing.TB, ctx context.Context) []string {
	t.Helper()
	var keys []string
	var err error
	server.Redis(ctx, func(r server.RedisClient) {
		keys, err = r.Keys(ctx, abuseReportRateLimitKeyPrefix+"*").Result()
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(keys)
	return keys
}

// Two api processes' routes over one Redis count a caller once (owner decision 536, QD-895): a /29's 31st report
// in an hour is refused whichever instance takes it, though neither took more than 16; refusals on one instance
// keep the caller refused on the other after the first reports leave the window; and the next /29 is another
// caller. Each instance alone would have taken every one of its reports under the in-process count this replaces.
func TestAbuseReportLimitHoldsAcrossApiInstances(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clock := &testAbuseRouteClock{now: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
		sink := &testAbuseReportSink{}
		settings := DefaultAbuseRouteSettings()
		instances := []*AbuseHandlers{}
		for range 2 {
			instances = append(instances, NewAbuseHandlers(&AbuseHandlersDeps{
				Now:               clock.Now,
				RateLimitExcluded: func(addr netip.Addr) bool { return false },
				AcceptReport:      sink.accept,
				Settings:          settings,
			}))
		}
		body := testAbuseReportBody(t, "phishing", []string{"https://elsewhere.example/"}, nil)
		// the i-th request goes to instance i mod 2, from one of 192.0.2.8/29's eight addresses
		report := func(i int) *testAbuseRouteAnswer {
			return testAbuseRouteCall(t, instances[i%2], "POST", "/abuse/report", "", fmt.Sprintf("192.0.2.%d:%d", 8+i%8, 41000+i), body)
		}
		if settings.ReportAddressLimit != 30 || settings.ReportAddressLookback != time.Hour {
			t.Fatalf("the default limit is %d in %s, want 30 an hour", settings.ReportAddressLimit, settings.ReportAddressLookback)
		}

		for i := range settings.ReportAddressLimit {
			if answer := report(i); answer.status != http.StatusOK {
				t.Fatalf("report %d: %d %q", i+1, answer.status, answer.body)
			}
		}
		for i := settings.ReportAddressLimit; i < settings.ReportAddressLimit+2; i += 1 {
			if answer := report(i); answer.status != http.StatusTooManyRequests || answer.header.Get("Retry-After") != "3600" {
				t.Fatalf("report %d, on instance %d: %d, Retry-After %q", i+1, i%2, answer.status, answer.header.Get("Retry-After"))
			}
		}
		if sink.count() != settings.ReportAddressLimit {
			t.Fatalf("%d reports taken, want %d", sink.count(), settings.ReportAddressLimit)
		}
		if answer := testAbuseRouteCall(t, instances[1], "POST", "/abuse/report", "", "192.0.2.16:41000", body); answer.status != http.StatusOK {
			t.Fatalf("an address of the next /29: %d", answer.status)
		}

		// refused reports count too, wherever they were refused
		clock.Advance(time.Hour - time.Minute)
		for i := range settings.ReportAddressLimit {
			if answer := testAbuseRouteCall(t, instances[0], "POST", "/abuse/report", "", "192.0.2.9:41000", body); answer.status != http.StatusTooManyRequests {
				t.Fatalf("refusal %d near the window's end: %d", i+1, answer.status)
			}
		}
		clock.Advance(time.Minute)
		if answer := testAbuseRouteCall(t, instances[1], "POST", "/abuse/report", "", "192.0.2.14:41000", body); answer.status != http.StatusTooManyRequests {
			t.Fatalf("on the other instance once the first reports left the window: %d", answer.status)
		}
		clock.Advance(time.Hour)
		if answer := testAbuseRouteCall(t, instances[1], "POST", "/abuse/report", "", "192.0.2.14:41000", body); answer.status != http.StatusOK {
			t.Fatalf("past the window: %d", answer.status)
		}
	})
}

// The members of addr's report count history in the test's Redis.
func testAbuseReportLimitCount(t testing.TB, ctx context.Context, addr netip.Addr) int64 {
	t.Helper()
	addressHash := server.ClientIpHashForAddr(addr)
	var count int64
	var err error
	server.Redis(ctx, func(r server.RedisClient) {
		count, err = r.ZCard(ctx, abuseReportRateLimitKeyPrefix+"{address_"+hex.EncodeToString(addressHash[:])+"}.address").Result()
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// Each URL and each header block counts as one report (QD-1024): a request of 16 URLs spends 16 of the hour's 30;
// a body refused for its content counts once; a request that would pass the limit is refused whole with nothing
// taken, and the counts it made stand, so the caller stays refused; and the window's end frees the caller again.
func TestAbuseReportLimitCountsEachReport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := &testAbuseRouteClock{now: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
		sink := &testAbuseReportSink{}
		settings := DefaultAbuseRouteSettings()
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			Now:               clock.Now,
			RateLimitExcluded: func(addr netip.Addr) bool { return false },
			AcceptReport:      sink.accept,
			Settings:          settings,
		})
		caller := netip.MustParseAddr("192.0.2.10")
		urls := func(count int) []string {
			urls := []string{}
			for i := range count {
				urls = append(urls, fmt.Sprintf("https://h%d.elsewhere.example/", i))
			}
			return urls
		}
		headerBlocks := func(count int) []string {
			headerBlocks := []string{}
			for i := range count {
				headerBlocks = append(headerBlocks, fmt.Sprintf("From: s%d@elsewhere.example\nSubject: hello\n\n", i))
			}
			return headerBlocks
		}
		report := func(body string) *testAbuseRouteAnswer {
			return testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.10:41000", body)
		}

		if answer := report(testAbuseReportBody(t, "nonsense", urls(settings.ReportUrlMaxCount), nil)); answer.status != http.StatusBadRequest {
			t.Fatalf("a body refused for its content: %d", answer.status)
		}
		if count := testAbuseReportLimitCount(t, ctx, caller); count != 1 {
			t.Fatalf("a body refused for its content counted %d", count)
		}
		if answer := report(testAbuseReportBody(t, "phishing", urls(16), nil)); answer.status != http.StatusOK || sink.count() != 16 {
			t.Fatalf("16 URLs: %d, %d reports taken", answer.status, sink.count())
		}
		if count := testAbuseReportLimitCount(t, ctx, caller); count != 17 {
			t.Fatalf("16 URLs after a refused body counted %d, want 17", count)
		}
		if answer := report(testAbuseReportBody(t, "spam", urls(4), headerBlocks(4))); answer.status != http.StatusOK || sink.count() != 24 {
			t.Fatalf("4 URLs and 4 header blocks: %d, %d reports taken", answer.status, sink.count())
		}
		if count := testAbuseReportLimitCount(t, ctx, caller); count != 25 {
			t.Fatalf("after 24 reports and a refused body, %d counted, want 25", count)
		}

		// 16 more would pass 30: refused whole, nothing taken, and its counts up to the refusal stand
		refused := report(testAbuseReportBody(t, "phishing", urls(16), nil))
		if refused.status != http.StatusTooManyRequests || refused.header.Get("Retry-After") != "3600" || sink.count() != 24 {
			t.Fatalf("a request past the limit: %d, Retry-After %q, %d reports taken", refused.status, refused.header.Get("Retry-After"), sink.count())
		}
		if count := testAbuseReportLimitCount(t, ctx, caller); count != int64(settings.ReportAddressLimit+1) {
			t.Fatalf("after the refusal, %d counted, want the history held at %d", count, settings.ReportAddressLimit+1)
		}
		if answer := report(testAbuseReportBody(t, "phishing", urls(1), nil)); answer.status != http.StatusTooManyRequests || sink.count() != 24 {
			t.Fatalf("one URL after the refusal: %d, %d reports taken", answer.status, sink.count())
		}

		clock.Advance(settings.ReportAddressLookback)
		if answer := report(testAbuseReportBody(t, "spam", urls(16), headerBlocks(4))); answer.status != http.StatusOK || sink.count() != 44 {
			t.Fatalf("a full request past the window: %d, %d reports taken", answer.status, sink.count())
		}
		if count := testAbuseReportLimitCount(t, ctx, caller); count != 20 {
			t.Fatalf("past the window, %d counted, want 20", count)
		}
	})
}

// An infrastructure address (WARP_LIMIT_EXCLUDE_SUBNETS, through the dep that stands for it) is never counted: past
// twice the limit, as an IPv4-mapped IPv6 address too, every report is taken and Redis holds no history, while the
// first caller outside it is counted.
func TestAbuseReportLimitNeverCountsExcludedInfrastructure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sink := &testAbuseReportSink{}
		settings := DefaultAbuseRouteSettings()
		excluded := netip.MustParsePrefix("198.51.100.0/24")
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			RateLimitExcluded: func(addr netip.Addr) bool { return excluded.Contains(addr) },
			AcceptReport:      sink.accept,
			Settings:          settings,
		})
		body := testAbuseReportBody(t, "phishing", []string{"https://elsewhere.example/"}, nil)

		for i := range 2 * settings.ReportAddressLimit {
			remoteAddr := "198.51.100.7:41000"
			if i%2 == 1 {
				remoteAddr = "[::ffff:198.51.100.7]:41000"
			}
			if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", remoteAddr, body); answer.status != http.StatusOK {
				t.Fatalf("infrastructure report %d from %s: %d", i+1, remoteAddr, answer.status)
			}
		}
		if sink.count() != 2*settings.ReportAddressLimit {
			t.Fatalf("%d infrastructure reports taken", sink.count())
		}
		if keys := testAbuseReportLimitKeys(t, ctx); len(keys) != 0 {
			t.Fatalf("infrastructure reports left histories %v", keys)
		}

		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.10:41000", body); answer.status != http.StatusOK {
			t.Fatalf("a caller outside the infrastructure: %d", answer.status)
		}
		if keys := testAbuseReportLimitKeys(t, ctx); len(keys) != 1 {
			t.Fatalf("a counted caller left histories %v", keys)
		}
	})
}

// The count keeps no address in Redis (I-13, R-ABUSE-6; §2.8 api-14): one history per caller at
// abuse_report.{address_<hash>}.address, the hash the peppered one of the caller's /29 or /56, expiring within the
// window and holding one member per report, and no key or value in the store holds the address, its network, either
// one's bytes or an unpeppered hash of either.
func TestAbuseReportLimitKeepsNoAddressInRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseRouteSettings()
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			RateLimitExcluded: func(addr netip.Addr) bool { return false },
			AcceptReport:      (&testAbuseReportSink{}).accept,
			Settings:          settings,
		})
		body := testAbuseReportBody(t, "phishing", []string{"https://elsewhere.example/"}, nil)
		callers := []struct {
			remoteAddr string
			addr       netip.Addr
			network    netip.Addr
		}{
			{remoteAddr: "192.0.2.10:41000", addr: netip.MustParseAddr("192.0.2.10"), network: netip.MustParseAddr("192.0.2.8")},
			{remoteAddr: "[2001:db8:1:100::1]:41000", addr: netip.MustParseAddr("2001:db8:1:100::1"), network: netip.MustParseAddr("2001:db8:1:100::")},
		}
		reportCount := 3
		wantKeys := []string{}
		needles := [][]byte{}
		for _, caller := range callers {
			for range reportCount {
				if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", caller.remoteAddr, body); answer.status != http.StatusOK {
					t.Fatalf("a report from %s: %d", caller.remoteAddr, answer.status)
				}
			}
			addressHash := server.ClientIpHashForAddr(caller.addr)
			wantKeys = append(wantKeys, "abuse_report.{address_"+hex.EncodeToString(addressHash[:])+"}.address")
			for _, addr := range []netip.Addr{caller.addr, caller.network} {
				plainHash := sha256.Sum256(addr.AsSlice())
				needles = append(needles, []byte(addr.String()), addr.AsSlice(), []byte(hex.EncodeToString(plainHash[:])))
			}
		}
		slices.Sort(wantKeys)
		if keys := testAbuseReportLimitKeys(t, ctx); !slices.Equal(keys, wantKeys) {
			t.Fatalf("histories %v, want %v", keys, wantKeys)
		}

		findings := []string{}
		var err error
		server.Redis(ctx, func(r server.RedisClient) {
			var keys []string
			keys, err = r.Keys(ctx, "*").Result()
			if err != nil {
				return
			}
			for _, key := range keys {
				values := [][]byte{[]byte(key)}
				var keyType string
				keyType, err = r.Type(ctx, key).Result()
				if err != nil {
					return
				}
				switch keyType {
				case "string":
					var value []byte
					value, err = r.Get(ctx, key).Bytes()
					values = append(values, value)
				case "zset":
					var members []redis.Z
					members, err = r.ZRangeWithScores(ctx, key, 0, -1).Result()
					for _, member := range members {
						values = append(values, []byte(fmt.Sprint(member.Member)), []byte(strconv.FormatFloat(member.Score, 'f', -1, 64)))
					}
					if err == nil && slices.Contains(wantKeys, key) {
						if len(members) != reportCount {
							findings = append(findings, fmt.Sprintf("%s holds %d members, want %d", key, len(members), reportCount))
						}
						var ttl time.Duration
						ttl, err = r.PTTL(ctx, key).Result()
						if err == nil && (ttl <= 0 || settings.ReportAddressLookback < ttl) {
							findings = append(findings, fmt.Sprintf("%s expires in %s, want within %s", key, ttl, settings.ReportAddressLookback))
						}
					}
				case "hash":
					var fields map[string]string
					fields, err = r.HGetAll(ctx, key).Result()
					for field, value := range fields {
						values = append(values, []byte(field), []byte(value))
					}
				case "set":
					var members []string
					members, err = r.SMembers(ctx, key).Result()
					for _, member := range members {
						values = append(values, []byte(member))
					}
				case "list":
					var items []string
					items, err = r.LRange(ctx, key, 0, -1).Result()
					for _, item := range items {
						values = append(values, []byte(item))
					}
				default:
					findings = append(findings, fmt.Sprintf("%s is a %s, which this test cannot read", key, keyType))
				}
				if err != nil {
					return
				}
				for _, value := range values {
					for _, needle := range needles {
						if bytes.Contains(value, needle) {
							findings = append(findings, fmt.Sprintf("%s holds a caller's address, network or unpeppered hash", key))
						}
					}
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if 0 < len(findings) {
			t.Fatalf("the store:\n%s", strings.Join(findings, "\n"))
		}
	})
}

// A count that cannot be kept refuses the request with the route's 503, as a store that did not answer, and takes
// no report, so no report passes uncounted: a lookback too short for the count's history is such a count. With no
// limit nothing is counted.
func TestAbuseReportRefusedWhenItsCountIsNotKept(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sink := &testAbuseReportSink{}
		body := testAbuseReportBody(t, "phishing", []string{"https://elsewhere.example/"}, nil)
		unkept := DefaultAbuseRouteSettings()
		unkept.ReportAddressLookback = 0
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			RateLimitExcluded: func(addr netip.Addr) bool { return false },
			AcceptReport:      sink.accept,
			Settings:          unkept,
		})
		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.10:41000", body); answer.status != http.StatusServiceUnavailable || sink.count() != 0 {
			t.Fatalf("a count not kept: %d %q, %d reports taken", answer.status, answer.body, sink.count())
		}

		unlimited := DefaultAbuseRouteSettings()
		unlimited.ReportAddressLimit = 0
		unlimited.ReportAddressLookback = 0
		handlers = NewAbuseHandlers(&AbuseHandlersDeps{
			RateLimitExcluded: func(addr netip.Addr) bool { return false },
			AcceptReport:      sink.accept,
			Settings:          unlimited,
		})
		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.10:41000", body); answer.status != http.StatusOK || sink.count() != 1 {
			t.Fatalf("no limit: %d, %d reports taken", answer.status, sink.count())
		}
		if keys := testAbuseReportLimitKeys(t, ctx); len(keys) != 0 {
			t.Fatalf("histories %v", keys)
		}
	})
}

// A report the route does not take is refused whole before anything is taken: a category the form does not offer,
// nothing to report, too many or too long URLs or header blocks, and a body past its bound.
func TestAbuseReportRefusesWhatItDoesNotTake(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		sink := &testAbuseReportSink{}
		settings := DefaultAbuseRouteSettings()
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			RateLimitExcluded: func(addr netip.Addr) bool { return true },
			AcceptReport:      sink.accept,
			Settings:          settings,
		})
		tooManyUrls := []string{}
		for i := range settings.ReportUrlMaxCount + 1 {
			tooManyUrls = append(tooManyUrls, fmt.Sprintf("https://h%d.elsewhere.example/", i))
		}
		for name, body := range map[string]string{
			"brand":            testAbuseReportBody(t, "brand", []string{"https://elsewhere.example/"}, nil),
			"unknown category": testAbuseReportBody(t, "nonsense", []string{"https://elsewhere.example/"}, nil),
			"nothing":          testAbuseReportBody(t, "phishing", nil, nil),
			"too many urls":    testAbuseReportBody(t, "phishing", tooManyUrls, nil),
			"a long url":       testAbuseReportBody(t, "phishing", []string{"https://elsewhere.example/" + strings.Repeat("p", settings.ReportUrlMaxByteCount)}, nil),
			"an empty url":     testAbuseReportBody(t, "phishing", []string{""}, nil),
			"a long block":     testAbuseReportBody(t, "spam", nil, []string{strings.Repeat("h", settings.ReportHeaderBlockMaxByteCount+1)}),
		} {
			if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.30:41000", body); answer.status != http.StatusBadRequest {
				t.Errorf("%s: %d %q", name, answer.status, answer.body)
			}
		}
		huge := testAbuseReportBody(t, "spam", nil, []string{strings.Repeat("h", int(settings.ReportBodyMaxByteCount))})
		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.30:41000", huge); answer.status != http.StatusRequestEntityTooLarge {
			t.Errorf("a body past its bound: %d", answer.status)
		}
		if sink.count() != 0 {
			t.Fatalf("%d refused reports were taken", sink.count())
		}
		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/report", "", "192.0.2.30:41000", testAbuseReportBody(t, "phishing", []string{"https://a.elsewhere.example/"}, []string{"From: x@elsewhere.example\n\n"})); answer.status != http.StatusOK || sink.count() != 2 {
			t.Fatalf("a url and a header block: %d, %d reports", answer.status, sink.count())
		}
	})
}

// An appeal is filed once by the network's credential; an unknown case, a barred one and another network's answer
// one 404 alike; a client token is refused by the handler too; a reason is required.
func TestAbuseAppealRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		network := newTestAbuseRouteNetwork(t, ctx)
		other := newTestAbuseRouteNetwork(t, ctx)
		caseId := testAbuseRouteNoticedCase(t, ctx, network.networkId, "shop.appeal.example", now.Add(-time.Hour))
		barredCaseId := testAbuseRouteNoticedCase(t, ctx, network.networkId, "shop.barred.example", now.Add(-time.Hour))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE abuse_case SET barred = true WHERE case_id = $1`, barredCaseId))
		})
		otherCaseId := testAbuseRouteNoticedCase(t, ctx, other.networkId, "shop.other.example", now.Add(-time.Hour))
		handlers := NewAbuseHandlers(nil)
		appealBody := func(caseId server.Id, reason string) string {
			body, err := json.Marshal(&model.AbuseAppealArgs{CaseId: caseId, Reason: reason})
			if err != nil {
				t.Fatal(err)
			}
			return string(body)
		}

		filed := testAbuseRouteCall(t, handlers, "POST", "/abuse/appeal", network.networkBearer, "192.0.2.40:41000", appealBody(caseId, "the page was taken down"))
		result := &model.AbuseAppealResult{}
		if filed.status != http.StatusOK || json.Unmarshal([]byte(filed.body), result) != nil || result.CaseId != caseId || result.State != model.AbuseAppealStatePending {
			t.Fatalf("an appeal: %d %q", filed.status, filed.body)
		}
		again := testAbuseRouteCall(t, handlers, "POST", "/abuse/appeal", network.networkBearer, "192.0.2.40:41000", appealBody(caseId, "again"))
		if again.status != http.StatusOK || again.body != filed.body {
			t.Fatalf("filing again: %d %q, first %q", again.status, again.body, filed.body)
		}

		unknown := testAbuseRouteCall(t, handlers, "POST", "/abuse/appeal", network.networkBearer, "192.0.2.40:41000", appealBody(server.NewId(), "why"))
		if unknown.status != http.StatusNotFound {
			t.Fatalf("an unknown case: %d %q", unknown.status, unknown.body)
		}
		for name, caseId := range map[string]server.Id{"barred": barredCaseId, "another network's": otherCaseId} {
			answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/appeal", network.networkBearer, "192.0.2.40:41000", appealBody(caseId, "why"))
			if answer.status != unknown.status || answer.body != unknown.body {
				t.Errorf("a %s case: %d %q, an unknown one %d %q", name, answer.status, answer.body, unknown.status, unknown.body)
			}
		}

		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/appeal", network.clientBearer, "192.0.2.40:41000", appealBody(caseId, "why")); answer.status != http.StatusForbidden || strings.TrimSpace(answer.body) != router.ClientCredentialRefusedMessage {
			t.Fatalf("a client token: %d %q", answer.status, answer.body)
		}
		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/appeal", network.networkBearer, "192.0.2.40:41000", appealBody(caseId, " ")); answer.status != http.StatusBadRequest {
			t.Fatalf("a blank reason: %d", answer.status)
		}
		if answer := testAbuseRouteCall(t, handlers, "POST", "/abuse/appeal", "", "192.0.2.40:41000", appealBody(caseId, "why")); answer.status != http.StatusUnauthorized {
			t.Fatalf("no bearer: %d", answer.status)
		}
	})
}

// The cases route lists the network's case for its own credential and refuses a client token; status answers a
// client token with sidecar.AbuseStatus, the body GET /abuse/status's client decodes.
func TestAbuseCasesAndStatusRoutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		network := newTestAbuseRouteNetwork(t, ctx)
		caseId := testAbuseRouteNoticedCase(t, ctx, network.networkId, "shop.listed.example", now.Add(-time.Hour))
		handlers := NewAbuseHandlers(nil)

		cases := testAbuseRouteCall(t, handlers, "GET", "/abuse/cases", network.networkBearer, "192.0.2.50:41000", "")
		casesResult := &model.AbuseNetworkCasesResult{}
		if cases.status != http.StatusOK || json.Unmarshal([]byte(cases.body), casesResult) != nil || len(casesResult.Cases) != 1 || casesResult.Cases[0].CaseId != caseId {
			t.Fatalf("the cases: %d %q", cases.status, cases.body)
		}
		if answer := testAbuseRouteCall(t, handlers, "GET", "/abuse/cases", network.clientBearer, "192.0.2.50:41000", ""); answer.status != http.StatusForbidden {
			t.Fatalf("a client token's cases: %d", answer.status)
		}

		for _, bearer := range []string{network.clientBearer, network.networkBearer} {
			answer := testAbuseRouteCall(t, handlers, "GET", "/abuse/status", bearer, "192.0.2.50:41000", "")
			status := &sidecar.AbuseStatus{}
			if answer.status != http.StatusOK || json.Unmarshal([]byte(answer.body), status) != nil {
				t.Fatalf("the status: %d %q", answer.status, answer.body)
			}
			if status.Standing != sidecar.AbuseStandingProbation || len(status.Actions) != 1 || server.Id(status.Actions[0].CaseId) != caseId {
				t.Fatalf("the status: %q", answer.body)
			}
		}
		if answer := testAbuseRouteCall(t, handlers, "GET", "/abuse/status", "", "192.0.2.50:41000", ""); answer.status != http.StatusUnauthorized {
			t.Fatalf("no bearer's status: %d", answer.status)
		}
	})
}

// A lookup through the route answers the case, uncached, and writes its access log row first.
func TestAbuseActionLookupThroughTheRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "abuse"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
		caseId := testAbuseRouteNoticedCase(t, ctx, networkId, "shop.staff.example", now.Add(-time.Hour))
		reviewerId := server.NewId()
		verifier := testAbuseActionStaffFile(t, map[string]string{
			"reviewer-bearer": testAbuseActionStaffEntry(reviewerId, "reviewer", now.Add(24*time.Hour)),
		})
		handlers := NewAbuseActionHandlers(&AbuseActionHandlersDeps{
			LoadVerifier: func() (*controller.AbuseStaffVerifier, error) { return verifier, nil },
		})

		answer := testAbuseActionCall(t, handlers, "Bearer reviewer-bearer", `{"kind":"lookup_case","case_id":"`+caseId.String()+`"}`)
		if answer.status != http.StatusOK || answer.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("the lookup: %d %q", answer.status, answer.body)
		}
		result := struct {
			Kind   string `json:"kind"`
			Result struct {
				Case struct {
					CaseId    server.Id `json:"case_id"`
					NetworkId server.Id `json:"network_id"`
				} `json:"case"`
				Actions []json.RawMessage `json:"actions"`
			} `json:"result"`
		}{}
		if err := json.Unmarshal([]byte(answer.body), &result); err != nil || result.Kind != "lookup_case" || result.Result.Case.CaseId != caseId || result.Result.Case.NetworkId != networkId || len(result.Result.Actions) != 1 {
			t.Fatalf("the case read: %q, %v", answer.body, err)
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_access_log WHERE reviewer_id = $1 AND case_id = $2`, reviewerId, caseId).Scan(&count))
		})
		if count != 1 {
			t.Fatalf("%d access log rows", count)
		}
		if answer := testAbuseActionCall(t, handlers, "Bearer reviewer-bearer", `{"kind":"lookup_case","case_id":"`+server.NewId().String()+`"}`); answer.status != http.StatusNotFound {
			t.Fatalf("an unknown case: %d", answer.status)
		}
	})
}

// One call of the routes with a trusted flagger's key header, as testAbuseRouteCall makes it.
func testAbuseRouteCallWithFlaggerKey(t testing.TB, handlers *AbuseHandlers, remoteAddr string, flaggerKey string, body string) *testAbuseRouteAnswer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("POST", "/abuse/report", handlers.Report),
	})
	request := httptest.NewRequest("POST", "/abuse/report", strings.NewReader(body))
	request.RemoteAddr = remoteAddr
	if flaggerKey != "" {
		request.Header.Set(model.AbuseFlaggerKeyHeader, flaggerKey)
	}
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	return &testAbuseRouteAnswer{status: w.Code, body: w.Body.String(), header: w.Header()}
}

// A trusted flagger's key (ABUSE-M8-01): a listed key sets the class and the flagger on each report, a wrong or
// absent key is a plain report, the answer is one body for all three, and the caller's address limit still counts
// a flagger's reports.
func TestAbuseReportTakesAFlaggersKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		digest := sha256.Sum256([]byte("synthetic-flagger-key-a"))
		file := []byte("flaggers:\n  - id: flagger-a\n    sha256: " + hex.EncodeToString(digest[:]) + "\n")
		flaggers := model.NewAbuseFlaggerSource(func() (*model.AbuseFlaggerVerifier, error) {
			return model.NewAbuseFlaggerVerifier(file, []string{"flagger-a"})
		}, nil)
		sink := &testAbuseReportSink{}
		settings := DefaultAbuseRouteSettings()
		settings.ReportAddressLimit = 3
		handlers := NewAbuseHandlers(&AbuseHandlersDeps{
			RateLimitExcluded: func(addr netip.Addr) bool { return false },
			AcceptReport:      sink.accept,
			Flaggers:          flaggers,
			FlaggerSettings:   model.DefaultAbuseFlaggerSettings,
			Settings:          settings,
		})
		body := testAbuseReportBody(t, "phishing", []string{"https://elsewhere.example/"}, nil)
		answers := []*testAbuseRouteAnswer{}
		for _, key := range []string{"synthetic-flagger-key-a", "synthetic-flagger-key-b", ""} {
			answers = append(answers, testAbuseRouteCallWithFlaggerKey(t, handlers, "192.0.2.10:41000", key, body))
		}
		for _, answer := range answers {
			if answer.status != http.StatusOK || answer.body != answers[0].body {
				t.Fatalf("answers %d %q and %d %q", answer.status, answer.body, answers[0].status, answers[0].body)
			}
		}
		if len(sink.reports) != 3 {
			t.Fatalf("%d reports taken", len(sink.reports))
		}
		if report := sink.reports[0]; report.ReporterClass != model.AbuseReporterClassTrustedFlagger || report.FlaggerId != "flagger-a" {
			t.Fatalf("the flagger's report: %+v", report)
		}
		for _, report := range sink.reports[1:] {
			if report.ReporterClass != model.AbuseReporterClassPublic || report.FlaggerId != "" {
				t.Fatalf("a plain report: %+v", report)
			}
		}
		// the address limit counts a flagger as any caller
		if answer := testAbuseRouteCallWithFlaggerKey(t, handlers, "192.0.2.10:41000", "synthetic-flagger-key-a", body); answer.status != http.StatusTooManyRequests {
			t.Fatalf("a flagger past the address limit: %d", answer.status)
		}
	})
}
