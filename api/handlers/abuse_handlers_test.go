//go:build sidecar_pending

package handlers

// §21.16's customer routes over the router (abuse_handlers.go; ABUSE-M5-11) on a private database and Redis: the
// report route's one answer whether or not a report joined, its limit on a fake clock with infrastructure
// uncounted, its refusals, the appeal route's answers for an unknown, a barred and another network's case alike,
// and the cases and status bodies. The router's class gates are api's (route_authz_test.go); the handlers' own
// refusal of a client token on cases and appeal is here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

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
