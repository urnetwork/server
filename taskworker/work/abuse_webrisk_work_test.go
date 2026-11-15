package work

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	mathrand "math/rand"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// §21.6's hourly check (abuse_webrisk_work.go) on a fake Update API and a fake clock. The store cases run §21.8's
// real intake on a private database (-tags sidecar_pending) and skip without MG5; the rest run on a recording intake.
// This file also holds the fixtures abuse_feed_work_test.go and abuse_ct_work_test.go share. Names are .example.

const testAbuseScanRoot = "root.example"

// abuse.yml with the test root, a platform name so the ladders act, Web Risk on and one phishing feed.
const testAbuseScanConfig = `
web_risk: {enabled: true}
feeds:
  - {name: phish-feed, url: "https://feed.example/phish.txt", category: phishing}
protected_names: {roots: [root.example], platform: [platform.example]}
`

// Skips a store case in a build that does not link MG2 and MG5.
func requireAbuseScanStore(t testing.TB) {
	t.Helper()
	if !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
		t.Skip("MG2 and MG5 are pending: these cases run with -tags sidecar_pending")
	}
}

// Stands in abuse.yml for the rest of the test.
func pushTestAbuseScanConfig(t testing.TB, yaml string) {
	t.Helper()
	pop := server.Testing_PushAbuseConfig(yaml)
	t.Cleanup(pop)
}

// A clock a test moves.
type testAbuseScanClock struct {
	stateLock sync.Mutex
	at        time.Time
}

func newTestAbuseScanClock(at time.Time) *testAbuseScanClock {
	return &testAbuseScanClock{at: at}
}

func (self *testAbuseScanClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.at
}

func (self *testAbuseScanClock) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.at = self.at.Add(duration)
}

// A network with a label in the test root and a log, so the published list pages it. Labels draw from a fixed seed,
// so they are the same each run.
func newTestAbuseScanZone(t testing.TB, ctx context.Context) (networkId server.Id, label string) {
	t.Helper()
	restore := model.Testing_SetDnsLabelRandom(mathrand.New(mathrand.NewSource(13)))
	defer restore()
	networkId = server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "abuse-"+networkId.String()[:8], server.NewId())
	label, err := model.EnsureNetworkLabel(ctx, networkId, testAbuseScanRoot)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_acl_head (network_id, version, chain) VALUES ($1, 1, $2)`,
			networkId,
			make([]byte, 32),
		))
	})
	return networkId, label
}

// Serves hosts as networkId's published names for the rest of the test.
func setTestAbuseScanPublished(t testing.TB, networkIdHosts map[server.Id][]string, label string) {
	t.Helper()
	model.SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, now time.Time) ([]*model.TlsPublishedName, error) {
		names := []*model.TlsPublishedName{}
		for _, host := range networkIdHosts[networkId] {
			names = append(names, &model.TlsPublishedName{
				Host:     host,
				Port:     443,
				Label:    label,
				Name:     host + "." + label + "." + testAbuseScanRoot,
				Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC,
			})
		}
		return names, nil
	})
	t.Cleanup(func() {
		model.SetTlsPublishedNamesSource(nil)
	})
}

func testAbuseScanCount(t testing.TB, ctx context.Context, sql string, args ...any) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql, args...).Scan(&count))
	})
	return
}

// Host kills recorded on target.
func testAbuseScanKillCount(t testing.TB, ctx context.Context, target string) int {
	return testAbuseScanCount(t, ctx, `SELECT COUNT(*) FROM abuse_action WHERE kind = 'host_kill' AND target = $1`, target)
}

func testAbuseScanCaseCount(t testing.TB, ctx context.Context) int {
	return testAbuseScanCount(t, ctx, `SELECT COUNT(*) FROM abuse_case`)
}

// Lifts the kill standing on target as wrong, as review would.
func liftTestAbuseScanKillAsWrong(t testing.TB, ctx context.Context, target string) {
	t.Helper()
	var actionId, caseId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT action_id, case_id FROM abuse_action WHERE kind = 'host_kill' AND target = $1 AND review_state = 'pending'`,
			target,
		).Scan(&actionId, &caseId))
	})
	reviewerId := server.NewId()
	if _, err := controller.LiftAbuseAction(ctx, &model.AbuseLiftArgs{
		ActionId:       server.NewId(),
		CaseId:         caseId,
		LiftedActionId: actionId,
		ReviewState:    model.AbuseReviewStateLiftedWrong,
		ReviewerId:     &reviewerId,
		ReviewReason:   "a false positive",
	}); err != nil {
		t.Fatal(err)
	}
}

// A recording intake for the cases that need no store.
type testAbuseScanIntake struct {
	stateLock sync.Mutex
	reports   []*model.AbuseReport
	incidents []*model.AbuseRootIncidentArgs
	liftTimes map[string]time.Time
	// the next accepts that fail
	failCount int
}

func newTestAbuseScanIntake() *testAbuseScanIntake {
	return &testAbuseScanIntake{liftTimes: map[string]time.Time{}}
}

func (self *testAbuseScanIntake) intake() *abuseScanIntake {
	return &abuseScanIntake{
		accept: func(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if 0 < self.failCount {
				self.failCount -= 1
				return nil, errors.New("abuse intake: the store did not answer")
			}
			self.reports = append(self.reports, report)
			return &controller.AbuseIntakeResult{Joined: true, Actions: []*controller.AbuseActionResult{{}}}, nil
		},
		liftedWrongTime: func(ctx context.Context, host string) (time.Time, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			liftTime, ok := self.liftTimes[host]
			return liftTime, ok
		},
		openRootIncident: func(ctx context.Context, args *model.AbuseRootIncidentArgs, now time.Time) error {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.incidents = append(self.incidents, args)
			return nil
		},
	}
}

// The hosts reported so far, in order.
func (self *testAbuseScanIntake) reportedHosts() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hosts := []string{}
	for _, report := range self.reports {
		hosts = append(hosts, report.Url)
	}
	return hosts
}

// A fake Web Risk Update API: each threat list's prefixes, answered in full on every computeDiff, and the full hashes
// hashes.search lists. Expire times are the test clock's, five minutes on, so a pass an hour later searches again.
type testAbuseWebRiskApi struct {
	stateLock sync.Mutex
	now       func() time.Time
	prefixes  map[string][]string
	listed    map[[sha256.Size]byte]string
	down      bool
	// the next searches that fail
	searchFailCount int
	version         int
	// each computeDiff's version token, "" for a full fetch, and each search's prefix
	diffVersionTokens []string
	searchPrefixes    []string
}

func newTestAbuseWebRiskApi(now func() time.Time) *testAbuseWebRiskApi {
	return &testAbuseWebRiskApi{
		now:      now,
		prefixes: map[string][]string{},
		listed:   map[[sha256.Size]byte]string{},
	}
}

// Lists expression's full hash on threatType, and its prefix locally.
func (self *testAbuseWebRiskApi) list(threatType server.WebRiskThreatType, expression string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hash := sha256.Sum256([]byte(expression))
	self.prefixes[string(threatType)] = append(self.prefixes[string(threatType)], string(hash[:4]))
	self.listed[hash] = string(threatType)
	self.version += 1
}

// Lists expression's prefix on threatType with no full hash behind it.
func (self *testAbuseWebRiskApi) listPrefix(threatType server.WebRiskThreatType, expression string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hash := sha256.Sum256([]byte(expression))
	self.prefixes[string(threatType)] = append(self.prefixes[string(threatType)], string(hash[:4]))
	self.version += 1
}

// Takes expression off every list.
func (self *testAbuseWebRiskApi) unlist(expression string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hash := sha256.Sum256([]byte(expression))
	for threatType, prefixes := range self.prefixes {
		self.prefixes[threatType] = slices.DeleteFunc(prefixes, func(prefix string) bool {
			return prefix == string(hash[:4])
		})
	}
	delete(self.listed, hash)
	self.version += 1
}

func (self *testAbuseWebRiskApi) setDown(down bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.down = down
}

func (self *testAbuseWebRiskApi) seen() (diffVersionTokens []string, searchPrefixes []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.diffVersionTokens), slices.Clone(self.searchPrefixes)
}

func (self *testAbuseWebRiskApi) RoundTrip(request *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	answer := func(status int, body any) (*http.Response, error) {
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}, Request: request}, nil
	}
	if self.down {
		return answer(http.StatusServiceUnavailable, map[string]any{})
	}
	query := request.URL.Query()
	expires := self.now().Add(5 * time.Minute).Format(time.RFC3339Nano)
	switch request.URL.Path {
	case "/v1/threatLists:computeDiff":
		self.diffVersionTokens = append(self.diffVersionTokens, query.Get("versionToken"))
		prefixes := slices.Sorted(slices.Values(self.prefixes[query.Get("threatType")]))
		digest := sha256.New()
		for _, prefix := range prefixes {
			io.WriteString(digest, prefix)
		}
		body := map[string]any{
			"responseType":    "RESET",
			"newVersionToken": base64.StdEncoding.EncodeToString([]byte{byte(self.version)}),
			"checksum":        map[string]any{"sha256": base64.StdEncoding.EncodeToString(digest.Sum(nil))},
		}
		if 0 < len(prefixes) {
			body["additions"] = map[string]any{"rawHashes": []any{map[string]any{
				"prefixSize": 4,
				"rawHashes":  base64.StdEncoding.EncodeToString([]byte(strings.Join(prefixes, ""))),
			}}}
		}
		return answer(http.StatusOK, body)
	case "/v1/hashes:search":
		if 0 < self.searchFailCount {
			self.searchFailCount -= 1
			return answer(http.StatusInternalServerError, map[string]any{})
		}
		prefix, err := base64.StdEncoding.DecodeString(query.Get("hashPrefix"))
		if err != nil {
			return answer(http.StatusBadRequest, map[string]any{})
		}
		self.searchPrefixes = append(self.searchPrefixes, string(prefix))
		threats := []any{}
		for hash, threatType := range self.listed {
			if strings.HasPrefix(string(hash[:]), string(prefix)) && slices.Contains(query["threatTypes"], threatType) {
				threats = append(threats, map[string]any{
					"threatTypes": []string{threatType},
					"hash":        base64.StdEncoding.EncodeToString(hash[:]),
					"expireTime":  expires,
				})
			}
		}
		return answer(http.StatusOK, map[string]any{"threats": threats, "negativeExpireTime": expires})
	}
	return nil, errors.New("an unknown path")
}

func testAbuseWebRiskSettings() *AbuseWebRiskSettings {
	settings := DefaultAbuseWebRiskSettings()
	settings.Client.BaseUrl = "https://webrisk.example"
	settings.Client.ThreatTypes = []server.WebRiskThreatType{server.WebRiskThreatTypeSocialEngineering, server.WebRiskThreatTypeMalware}
	return settings
}

func testAbuseWebRiskDeps(clock *testAbuseScanClock, api *testAbuseWebRiskApi, intake *abuseScanIntake) *abuseWebRiskDeps {
	return &abuseWebRiskDeps{
		now:    clock.now,
		config: server.GetAbuseConfig,
		apiKey: func() (string, error) {
			return "test-web-risk-key", nil
		},
		transport: api,
		names:     model.ListAbuseScanNames,
		intake:    intake,
	}
}

func testAbuseWebRiskPrefix(expression string) string {
	hash := sha256.Sum256([]byte(expression))
	return string(hash[:4])
}

// The composed path on the real store: a name whose expressions match no local prefix sends nothing; a local match
// with no full hash behind it is searched and acts on nothing; a confirmed match is §21.8's Web Risk signal, killing
// its host once; a pass an hour later counts on the open case and kills nothing again; a second cycle sends the
// lists' versions and a cold start fetches them in full.
func TestAbuseWebRiskKillsAConfirmedHostOnce(t *testing.T) {
	requireAbuseScanStore(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		networkId, label := newTestAbuseScanZone(t, ctx)
		zone := label + "." + testAbuseScanRoot
		setTestAbuseScanPublished(t, map[server.Id][]string{networkId: {"shop", "blog"}}, label)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_dns_record (root, owner, rrtype, network_id, kind, ttl, rdata, create_time)
					VALUES ($1, $2, 16, $3, 'user', 300, '{}', $4)
				`,
				testAbuseScanRoot+".",
				"www."+zone+".",
				networkId,
				server.NowUtc(),
			))
		})

		clock := newTestAbuseScanClock(server.NowUtc())
		api := newTestAbuseWebRiskApi(clock.now)
		api.list(server.WebRiskThreatTypeSocialEngineering, "shop."+zone+"/")
		api.listPrefix(server.WebRiskThreatTypeSocialEngineering, "blog."+zone+"/")
		scan := newAbuseWebRiskScan(testAbuseWebRiskSettings(), testAbuseWebRiskDeps(clock, api, defaultAbuseScanIntake()))

		result := scan.run(ctx)
		if result.Idle || result.Failed || result.More || result.ConfirmedCount != 1 || result.ActedCount != 1 || result.NameCount != 3 {
			t.Fatalf("the first pass: %+v", result)
		}
		if count := testAbuseScanKillCount(t, ctx, "shop."+zone); count != 1 {
			t.Fatalf("the confirmed host has %d kills", count)
		}
		if count := testAbuseScanKillCount(t, ctx, "blog."+zone); count != 0 {
			t.Fatalf("a prefix match with no full hash took %d kills", count)
		}
		_, searchPrefixes := api.seen()
		if !slices.Equal(slices.Sorted(slices.Values(searchPrefixes)), slices.Sorted(slices.Values([]string{
			testAbuseWebRiskPrefix("shop." + zone + "/"),
			testAbuseWebRiskPrefix("blog." + zone + "/"),
		}))) {
			t.Fatalf("searches sent %d prefixes, want only the two that matched locally", len(searchPrefixes))
		}
		caseCount := testAbuseScanCaseCount(t, ctx)

		clock.advance(time.Hour)
		result = scan.run(ctx)
		if result.Failed || result.ConfirmedCount != 1 || result.ActedCount != 0 || result.ReportedCount != 1 {
			t.Fatalf("the repeat pass: %+v", result)
		}
		if count := testAbuseScanKillCount(t, ctx, "shop."+zone); count != 1 {
			t.Fatalf("a repeat pass killed again: %d kills", count)
		}
		if count := testAbuseScanCaseCount(t, ctx); count != caseCount {
			t.Fatalf("a repeat pass opened a case: %d, was %d", count, caseCount)
		}

		cold := newAbuseWebRiskScan(testAbuseWebRiskSettings(), testAbuseWebRiskDeps(clock, api, defaultAbuseScanIntake()))
		if result := cold.run(ctx); result.Failed {
			t.Fatalf("the cold start: %+v", result)
		}
		diffVersionTokens, _ := api.seen()
		if len(diffVersionTokens) != 6 || diffVersionTokens[0] != "" || diffVersionTokens[1] != "" ||
			diffVersionTokens[2] == "" || diffVersionTokens[3] == "" || diffVersionTokens[4] != "" || diffVersionTokens[5] != "" {
			t.Fatalf("version tokens: a cycle sends the lists' versions and a cold start none: %q", diffVersionTokens)
		}
	})
}

// A kill review lifted as wrong is not taken again while the listing stands, the hit counted as suppressed; once the
// list has gone a whole cycle without the host and lists it again, that is a new listing, and it kills.
func TestAbuseWebRiskDoesNotKillALiftedFalsePositiveAgain(t *testing.T) {
	requireAbuseScanStore(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		networkId, label := newTestAbuseScanZone(t, ctx)
		host := "shop." + label + "." + testAbuseScanRoot
		setTestAbuseScanPublished(t, map[server.Id][]string{networkId: {"shop"}}, label)
		clock := newTestAbuseScanClock(server.NowUtc())
		api := newTestAbuseWebRiskApi(clock.now)
		api.list(server.WebRiskThreatTypeSocialEngineering, host+"/")
		scan := newAbuseWebRiskScan(testAbuseWebRiskSettings(), testAbuseWebRiskDeps(clock, api, defaultAbuseScanIntake()))

		if result := scan.run(ctx); result.ActedCount != 1 {
			t.Fatalf("the first pass: %+v", result)
		}
		liftTestAbuseScanKillAsWrong(t, ctx, host)
		caseCount := testAbuseScanCaseCount(t, ctx)

		clock.advance(time.Hour)
		result := scan.run(ctx)
		if result.Failed || result.SuppressedCount != 1 || result.ReportedCount != 0 {
			t.Fatalf("the pass after the lift: %+v", result)
		}
		if count := testAbuseScanKillCount(t, ctx, host); count != 1 {
			t.Fatalf("a lifted false positive was killed again: %d kills", count)
		}
		if count := testAbuseScanCaseCount(t, ctx); count != caseCount {
			t.Fatalf("a suppressed hit opened a case")
		}

		api.unlist(host + "/")
		clock.advance(time.Hour)
		if result := scan.run(ctx); result.Failed || result.ConfirmedCount != 0 {
			t.Fatalf("the pass without the host: %+v", result)
		}
		api.list(server.WebRiskThreatTypeSocialEngineering, host+"/")
		clock.advance(time.Hour)
		if result := scan.run(ctx); result.Failed || result.ActedCount != 1 {
			t.Fatalf("the listing after a whole cycle without it: %+v", result)
		}
		if count := testAbuseScanKillCount(t, ctx, host); count != 2 {
			t.Fatalf("a new listing took %d kills in all, want 2", count)
		}
	})
}

// A hit on the root's own expression opens one root incident, with no network, and kills nothing; the next pass
// counts on the same incident.
func TestAbuseWebRiskRootHitOpensAnIncident(t *testing.T) {
	requireAbuseScanStore(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		networkId, label := newTestAbuseScanZone(t, ctx)
		setTestAbuseScanPublished(t, map[server.Id][]string{networkId: {"shop", "blog"}}, label)
		clock := newTestAbuseScanClock(server.NowUtc())
		api := newTestAbuseWebRiskApi(clock.now)
		api.list(server.WebRiskThreatTypeMalware, testAbuseScanRoot+"/")
		scan := newAbuseWebRiskScan(testAbuseWebRiskSettings(), testAbuseWebRiskDeps(clock, api, defaultAbuseScanIntake()))

		for range 2 {
			result := scan.run(ctx)
			if result.Failed || result.RootIncidentCount != 1 || result.ActedCount != 0 || result.ReportedCount != 0 {
				t.Fatalf("a root hit: %+v", result)
			}
			clock.advance(time.Hour)
		}
		if count := testAbuseScanCount(t, ctx, `SELECT COUNT(*) FROM abuse_case WHERE network_id IS NULL AND target = $1 AND category = 'malware'`, testAbuseScanRoot); count != 1 {
			t.Fatalf("%d root incidents, want one", count)
		}
		if count := testAbuseScanCount(t, ctx, `SELECT COUNT(*) FROM abuse_action`); count != 0 {
			t.Fatalf("a root hit took %d actions", count)
		}
	})
}

// Names by page from a list of pages, failing a page's first read when told to.
type testAbuseWebRiskNames struct {
	stateLock sync.Mutex
	pages     [][]*model.AbuseScanName
	// a page index whose next read fails
	failPage int
	// the page each read asked for
	readPages []int
}

func (self *testAbuseWebRiskNames) names(ctx context.Context, cursor *model.AbuseScanNamesCursor, rootNames []string, now time.Time, settings *model.AbuseScanSettings) ([]*model.AbuseScanName, *model.AbuseScanNamesCursor, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	page := 0
	if cursor != nil {
		page = cursor.RecordType
	}
	self.readPages = append(self.readPages, page)
	if page == self.failPage {
		self.failPage = -1
		return nil, nil, errors.New("a page that does not load")
	}
	if len(self.pages)-1 <= page {
		return self.pages[page], nil, nil
	}
	return self.pages[page], &model.AbuseScanNamesCursor{Records: true, RecordType: page + 1}, nil
}

func testAbuseScanNames(hosts ...string) []*model.AbuseScanName {
	names := []*model.AbuseScanName{}
	for _, host := range hosts {
		names = append(names, &model.AbuseScanName{NetworkId: server.NewId(), Name: host})
	}
	return names
}

// A page that fails ends the run with nothing acted on from it, and the next run reads that page again rather than
// starting over; a search that fails does the same.
func TestAbuseWebRiskResumesAFailedPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		api := newTestAbuseWebRiskApi(clock.now)
		api.list(server.WebRiskThreatTypeSocialEngineering, "two.acme.root.example/")
		api.list(server.WebRiskThreatTypeSocialEngineering, "three.acme.root.example/")
		names := &testAbuseWebRiskNames{
			pages: [][]*model.AbuseScanName{
				testAbuseScanNames("one.acme.root.example"),
				testAbuseScanNames("two.acme.root.example"),
				testAbuseScanNames("three.acme.root.example"),
			},
			failPage: 1,
		}
		intake := newTestAbuseScanIntake()
		deps := testAbuseWebRiskDeps(clock, api, intake.intake())
		deps.names = names.names
		scan := newAbuseWebRiskScan(testAbuseWebRiskSettings(), deps)

		if result := scan.run(ctx); !result.Failed || 0 < len(intake.reportedHosts()) {
			t.Fatalf("a failed page: %+v, reported %q", result, intake.reportedHosts())
		}
		clock.advance(10 * time.Minute)
		// the second page's search fails once: its page is read again in the next run
		api.stateLock.Lock()
		api.searchFailCount = 1
		api.stateLock.Unlock()
		if result := scan.run(ctx); !result.Failed || 0 < len(intake.reportedHosts()) {
			t.Fatalf("a failed search: %+v", result)
		}
		clock.advance(10 * time.Minute)
		if result := scan.run(ctx); result.Failed || result.More || result.ActedCount != 2 {
			t.Fatalf("the resumed run: %+v", result)
		}
		if !slices.Equal(names.readPages, []int{0, 1, 1, 1, 2}) {
			t.Fatalf("pages read %v: a failed page must be read again, not the scan restarted", names.readPages)
		}
		if !slices.Equal(intake.reportedHosts(), []string{"two.acme.root.example", "three.acme.root.example"}) {
			t.Fatalf("reported %q", intake.reportedHosts())
		}
	})
}

// A source that cannot be reached warns and acts on nothing, searching nothing; once it answers, the same scan runs.
func TestAbuseWebRiskSourceDownActsOnNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		api := newTestAbuseWebRiskApi(clock.now)
		api.list(server.WebRiskThreatTypeSocialEngineering, "shop.acme.root.example/")
		api.setDown(true)
		names := &testAbuseWebRiskNames{pages: [][]*model.AbuseScanName{testAbuseScanNames("shop.acme.root.example")}, failPage: -1}
		intake := newTestAbuseScanIntake()
		deps := testAbuseWebRiskDeps(clock, api, intake.intake())
		deps.names = names.names
		scan := newAbuseWebRiskScan(testAbuseWebRiskSettings(), deps)

		result := scan.run(ctx)
		_, searchPrefixes := api.seen()
		if !result.Failed || 0 < result.NameCount || 0 < len(searchPrefixes) || 0 < len(intake.reportedHosts()) || 0 < len(names.readPages) {
			t.Fatalf("a source down: %+v, %d searches, reported %q", result, len(searchPrefixes), intake.reportedHosts())
		}
		api.setDown(false)
		clock.advance(10 * time.Minute)
		if result := scan.run(ctx); result.Failed || result.ActedCount != 1 {
			t.Fatalf("the source back: %+v", result)
		}
	})
}

// The check idles with web_risk off, with no root, and with no key in vault.
func TestAbuseWebRiskIdlesUntilTurnedOn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		api := newTestAbuseWebRiskApi(clock.now)
		for _, yaml := range []string{
			"web_risk: {enabled: false}\nprotected_names: {roots: [root.example], platform: [platform.example]}\n",
			"web_risk: {enabled: true}\n",
		} {
			pop := server.Testing_PushAbuseConfig(yaml)
			result := newAbuseWebRiskScan(testAbuseWebRiskSettings(), testAbuseWebRiskDeps(clock, api, newTestAbuseScanIntake().intake())).run(ctx)
			pop()
			if !result.Idle {
				t.Fatalf("%q: %+v", yaml, result)
			}
		}
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		deps := testAbuseWebRiskDeps(clock, api, newTestAbuseScanIntake().intake())
		deps.apiKey = func() (string, error) {
			return "", server.ErrWebRiskClientRefused
		}
		if result := newAbuseWebRiskScan(testAbuseWebRiskSettings(), deps).run(ctx); !result.Idle {
			t.Fatalf("no key: %+v", result)
		}
		if diffVersionTokens, _ := api.seen(); 0 < len(diffVersionTokens) {
			t.Fatal("an idle check asked the API")
		}
	})
}

// A source's listings: the first cycle's hosts are listed since an unknown time; after a whole cycle, a new host is
// listed since its cycle began; a host a whole cycle did not list starts a new run; a cycle that did not end, or one
// past the bound, shows nothing about what it did not list.
func TestAbuseScanListingsTracksRunsOfListings(t *testing.T) {
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	listings := newAbuseScanListings(2)
	listings.startCycle(start)
	if since, handled := listings.list("a.root.example"); !since.IsZero() || handled {
		t.Fatalf("the first cycle: %v, %t", since, handled)
	}
	listings.handle("a.root.example")
	if _, handled := listings.list("a.root.example"); !handled {
		t.Fatal("a host acted on twice in one cycle")
	}
	listings.endCycle()

	second := start.Add(time.Hour)
	listings.startCycle(second)
	if since, _ := listings.list("a.root.example"); !since.IsZero() {
		t.Fatalf("a run since the first cycle: %v", since)
	}
	if since, handled := listings.list("b.root.example"); !since.Equal(second) || handled {
		t.Fatalf("a host new after a whole cycle: %v, %t", since, handled)
	}
	listings.endCycle()

	// a is not listed for a whole cycle, and is forgotten
	third := start.Add(2 * time.Hour)
	listings.startCycle(third)
	listings.list("b.root.example")
	listings.endCycle()
	fourth := start.Add(3 * time.Hour)
	listings.startCycle(fourth)
	if since, _ := listings.list("a.root.example"); !since.Equal(fourth) {
		t.Fatalf("a host back after a whole cycle without it: %v", since)
	}
	// past the bound a host goes unremembered, since an unknown time, and taints the next cycle
	if since, _ := listings.list("c.root.example"); !since.IsZero() {
		t.Fatalf("a host past the bound: %v", since)
	}
	listings.endCycle()
	fifth := start.Add(4 * time.Hour)
	listings.startCycle(fifth)
	if since, _ := listings.list("d.root.example"); !since.IsZero() {
		t.Fatalf("a host after a cycle that went past the bound: %v", since)
	}

	// a cycle that does not end shows nothing either
	unended := newAbuseScanListings(10)
	unended.startCycle(start)
	unended.endCycle()
	unended.startCycle(second)
	unended.startCycle(third)
	if since, _ := unended.list("a.root.example"); !since.IsZero() {
		t.Fatalf("a host after a cycle that did not end: %v", since)
	}
}

// A lift as wrong suppresses a host listed since before it, or since a time the process cannot see; a listing begun
// after it is reported. Root hits open incidents, and names under no root are dropped, each once a cycle.
func TestReportAbuseScanHitsSuppressesUntilTheListingChanges(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	intake := newTestAbuseScanIntake()
	intake.liftTimes["shop.acme.root.example"] = start.Add(30 * time.Minute)
	intake.liftTimes["blog.acme.root.example"] = start.Add(30 * time.Minute)
	listings := newAbuseScanListings(100)
	roots := []string{testAbuseScanRoot}
	hits := func(hosts ...string) []*abuseScanHit {
		hits := []*abuseScanHit{}
		for _, host := range hosts {
			hits = append(hits, &abuseScanHit{host: host, category: model.AbuseCategoryPhishing})
		}
		return hits
	}

	listings.startCycle(start)
	counts, err := reportAbuseScanHits(ctx, intake.intake(), listings, abuseScanWebRisk, model.AbuseReporterClassWebRisk, model.AbuseSignalWebRiskHit,
		hits("shop.acme.root.example", "shop.acme.root.example", testAbuseScanRoot, "example", "other.example"), roots, start)
	if err != nil || counts.Suppressed != 1 || counts.RootIncidents != 1 || counts.Dropped != 1 || counts.Reported != 0 {
		t.Fatalf("the first cycle: %+v, %v", counts, err)
	}
	listings.endCycle()

	// blog appears after a whole cycle without it, and after its lift: a new listing
	second := start.Add(time.Hour)
	listings.startCycle(second)
	counts, err = reportAbuseScanHits(ctx, intake.intake(), listings, abuseScanWebRisk, model.AbuseReporterClassWebRisk, model.AbuseSignalWebRiskHit,
		hits("shop.acme.root.example", "blog.acme.root.example"), roots, second)
	if err != nil || counts.Suppressed != 1 || counts.Reported != 1 || counts.Acted != 1 {
		t.Fatalf("the second cycle: %+v, %v", counts, err)
	}
	if !slices.Equal(intake.reportedHosts(), []string{"blog.acme.root.example"}) {
		t.Fatalf("reported %q", intake.reportedHosts())
	}

	// a report that fails is not marked, so the cycle's next try takes it
	third := start.Add(2 * time.Hour)
	listings.endCycle()
	listings.startCycle(third)
	intake.failCount = 1
	if _, err := reportAbuseScanHits(ctx, intake.intake(), listings, abuseScanFeed, model.AbuseReporterClassFeed, model.AbuseSignalFeedHit,
		hits("blog.acme.root.example"), roots, third); err == nil {
		t.Fatal("a failed report answered no error")
	}
	if counts, err := reportAbuseScanHits(ctx, intake.intake(), listings, abuseScanFeed, model.AbuseReporterClassFeed, model.AbuseSignalFeedHit,
		hits("blog.acme.root.example"), roots, third); err != nil || counts.Reported != 1 {
		t.Fatalf("the retry: %+v, %v", counts, err)
	}
	if len(intake.incidents) != 1 || intake.incidents[0].Root != testAbuseScanRoot || intake.incidents[0].Signal != model.AbuseSignalWebRiskHit {
		t.Fatalf("incidents %+v", intake.incidents)
	}
}
