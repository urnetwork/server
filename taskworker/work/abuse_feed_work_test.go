package work

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.8's feeds (abuse_feed_work.go) on a fake feed server and clock: a listed host under the root kills once, the
// root itself opens an incident, names outside the root and comments are dropped, a feed past its bound is cut and
// counted, and a feed down or with no category acts on nothing. The store case runs §21.8's real intake (-tags
// sidecar_pending); the rest run on the recording intake abuse_webrisk_work_test.go holds.

// Serves each url's text, or a status for a url told to fail, and counts fetches.
type testAbuseFeedServer struct {
	stateLock sync.Mutex
	bodies    map[string]string
	statuses  map[string]int
	fetches   map[string]int
}

func newTestAbuseFeedServer() *testAbuseFeedServer {
	return &testAbuseFeedServer{bodies: map[string]string{}, statuses: map[string]int{}, fetches: map[string]int{}}
}

func (self *testAbuseFeedServer) set(feedUrl string, body string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.bodies[feedUrl] = body
}

func (self *testAbuseFeedServer) fail(feedUrl string, status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statuses[feedUrl] = status
}

func (self *testAbuseFeedServer) fetchCount(feedUrl string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.fetches[feedUrl]
}

func (self *testAbuseFeedServer) RoundTrip(request *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	feedUrl := request.URL.String()
	self.fetches[feedUrl] += 1
	status := http.StatusOK
	if failStatus, ok := self.statuses[feedUrl]; ok {
		status = failStatus
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(self.bodies[feedUrl])),
		Header:     http.Header{},
		Request:    request,
	}, nil
}

const testAbuseFeedUrl = "https://feed.example/phish.txt"

func testAbuseFeedDeps(clock *testAbuseScanClock, feeds *testAbuseFeedServer, intake *abuseScanIntake) *abuseFeedDeps {
	return &abuseFeedDeps{
		now:       clock.now,
		config:    server.GetAbuseConfig,
		transport: feeds,
		intake:    intake,
	}
}

// The composed path on the real store: a feed's url under the root kills its host once, a repeat pass adds no case,
// the root itself opens an incident and kills nothing, and lines outside the root or not naming a host are dropped.
func TestAbuseFeedsKillAListedHostOnce(t *testing.T) {
	requireAbuseScanStore(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		_, label := newTestAbuseScanZone(t, ctx)
		host := "login." + label + "." + testAbuseScanRoot
		feeds := newTestAbuseFeedServer()
		feeds.set(testAbuseFeedUrl, strings.Join([]string{
			"# a phishing feed",
			"https://" + host + "/verify?account=1",
			"hxxp://elsewhere.example/" + host,
			"http://" + testAbuseScanRoot + "/",
			"not a url at all",
			"",
		}, "\n"))
		clock := newTestAbuseScanClock(server.NowUtc())
		scan := newAbuseFeedScan(DefaultAbuseFeedsSettings(), testAbuseFeedDeps(clock, feeds, defaultAbuseScanIntake()))

		result := scan.run(ctx)
		if result.Idle || result.Failed || result.FeedCount != 1 || result.HostCount != 2 || result.ActedCount != 1 || result.RootIncidentCount != 1 {
			t.Fatalf("the first pass: %+v", result)
		}
		if count := testAbuseScanKillCount(t, ctx, host); count != 1 {
			t.Fatalf("the listed host has %d kills", count)
		}
		caseCount := testAbuseScanCaseCount(t, ctx)

		clock.advance(time.Hour)
		result = scan.run(ctx)
		if result.Failed || result.ActedCount != 0 || result.ReportedCount != 1 {
			t.Fatalf("the repeat pass: %+v", result)
		}
		if count := testAbuseScanKillCount(t, ctx, host); count != 1 {
			t.Fatalf("a repeat pass killed again: %d kills", count)
		}
		if count := testAbuseScanCaseCount(t, ctx); count != caseCount {
			t.Fatalf("a repeat pass opened a case: %d, was %d", count, caseCount)
		}
		if count := testAbuseScanCount(t, ctx, `SELECT COUNT(*) FROM abuse_case WHERE network_id IS NULL`); count != 1 {
			t.Fatalf("%d root incidents, want one", count)
		}
	})
}

// A feed past its byte bound is cut there, its partial last line dropped, and what came before acted on; a feed past
// its line bound likewise. A cut read ends no cycle, so it shows nothing about the hosts it did not reach.
func TestAbuseFeedsCutAFeedPastItsBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		first := "one.acme." + testAbuseScanRoot + "\n"
		second := "two.acme." + testAbuseScanRoot + "\n"
		third := "three.acme." + testAbuseScanRoot + "\n"
		feeds := newTestAbuseFeedServer()
		feeds.set(testAbuseFeedUrl, first+second+third)

		settings := DefaultAbuseFeedsSettings()
		// the bound falls inside the second line
		settings.FeedMaxByteCount = int64(len(first) + 4)
		intake := newTestAbuseScanIntake()
		scan := newAbuseFeedScan(settings, testAbuseFeedDeps(clock, feeds, intake.intake()))
		result := scan.run(ctx)
		if result.Failed || result.CutCount != 1 || result.HostCount != 1 {
			t.Fatalf("a feed past its byte bound: %+v", result)
		}
		if !slices.Equal(intake.reportedHosts(), []string{"one.acme." + testAbuseScanRoot}) {
			t.Fatalf("reported %q", intake.reportedHosts())
		}
		if listings := scan.feedListings[testAbuseFeedUrl]; listings == nil || listings.completeCycle != 0 {
			t.Fatal("a cut read ended a cycle")
		}

		// exactly at the bound with nothing after it is whole
		settings.FeedMaxByteCount = int64(len(first + second + third))
		intake = newTestAbuseScanIntake()
		scan = newAbuseFeedScan(settings, testAbuseFeedDeps(clock, feeds, intake.intake()))
		if result := scan.run(ctx); result.CutCount != 0 || result.HostCount != 3 {
			t.Fatalf("a feed at its bound: %+v", result)
		}
		if listings := scan.feedListings[testAbuseFeedUrl]; listings.completeCycle != 1 {
			t.Fatal("a whole read ended no cycle")
		}

		settings = DefaultAbuseFeedsSettings()
		settings.FeedMaxLineCount = 2
		intake = newTestAbuseScanIntake()
		scan = newAbuseFeedScan(settings, testAbuseFeedDeps(clock, feeds, intake.intake()))
		if result := scan.run(ctx); result.CutCount != 1 || result.HostCount != 2 {
			t.Fatalf("a feed past its line bound: %+v", result)
		}

		// a line past the line bound names nothing and is read past
		settings = DefaultAbuseFeedsSettings()
		settings.LineMaxByteCount = 64
		feeds.set(testAbuseFeedUrl, "https://long.acme."+testAbuseScanRoot+"/"+strings.Repeat("a", 200)+"\n"+second)
		intake = newTestAbuseScanIntake()
		scan = newAbuseFeedScan(settings, testAbuseFeedDeps(clock, feeds, intake.intake()))
		if result := scan.run(ctx); result.CutCount != 0 || !slices.Equal(intake.reportedHosts(), []string{"two.acme." + testAbuseScanRoot}) {
			t.Fatalf("a long line: %+v, reported %q", result, intake.reportedHosts())
		}

		settings = DefaultAbuseFeedsSettings()
		settings.FeedHitMaxCount = 1
		feeds.set(testAbuseFeedUrl, first+second+third)
		intake = newTestAbuseScanIntake()
		scan = newAbuseFeedScan(settings, testAbuseFeedDeps(clock, feeds, intake.intake()))
		if result := scan.run(ctx); result.CutCount != 1 || result.HostCount != 1 {
			t.Fatalf("a feed past its hit bound: %+v", result)
		}
	})
}

// A feed that answers anything but a 200 warns and acts on nothing, its listings left as they were; a feed with no
// category the worker takes is skipped and not fetched; with no feed or no root the feeds idle.
func TestAbuseFeedsActOnNothingFromAFeedThatFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		feeds := newTestAbuseFeedServer()
		feeds.set(testAbuseFeedUrl, "one.acme."+testAbuseScanRoot+"\n")
		feeds.fail(testAbuseFeedUrl, http.StatusServiceUnavailable)
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		intake := newTestAbuseScanIntake()
		scan := newAbuseFeedScan(DefaultAbuseFeedsSettings(), testAbuseFeedDeps(clock, feeds, intake.intake()))
		result := scan.run(ctx)
		if !result.Failed || result.FailedCount != 1 || 0 < len(intake.reportedHosts()) {
			t.Fatalf("a feed down: %+v, reported %q", result, intake.reportedHosts())
		}
		if _, ok := scan.feedListings[testAbuseFeedUrl]; ok {
			t.Fatal("a feed that failed started a cycle")
		}

		pop := server.Testing_PushAbuseConfig(`
feeds:
  - {name: no-category, url: "https://feed.example/none.txt"}
  - {name: spam, url: "https://feed.example/spam.txt", category: spam}
protected_names: {roots: [root.example], platform: [platform.example]}
`)
		result = scan.run(ctx)
		pop()
		if result.SkippedCount != 2 || 0 < feeds.fetchCount("https://feed.example/none.txt") || 0 < feeds.fetchCount("https://feed.example/spam.txt") {
			t.Fatalf("feeds with no category: %+v", result)
		}

		for _, yaml := range []string{
			"feeds: []\nprotected_names: {roots: [root.example], platform: [platform.example]}\n",
			"feeds:\n  - {name: phish-feed, url: \"https://feed.example/phish.txt\", category: phishing}\n",
		} {
			pop := server.Testing_PushAbuseConfig(yaml)
			result := scan.run(ctx)
			pop()
			if !result.Idle {
				t.Fatalf("%q: %+v", yaml, result)
			}
		}
	})
}

// A feed's lifted false positive is not reported again while the feed lists it, and the feed dropping it for a
// whole read, then listing it again, is a new listing.
func TestAbuseFeedsDoNotReportALiftedFalsePositiveAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		host := "one.acme." + testAbuseScanRoot
		feeds := newTestAbuseFeedServer()
		feeds.set(testAbuseFeedUrl, host+"\n")
		intake := newTestAbuseScanIntake()
		scan := newAbuseFeedScan(DefaultAbuseFeedsSettings(), testAbuseFeedDeps(clock, feeds, intake.intake()))

		if result := scan.run(ctx); result.ReportedCount != 1 {
			t.Fatalf("the first pass: %+v", result)
		}
		intake.liftTimes[host] = clock.now().Add(time.Minute)
		clock.advance(time.Hour)
		if result := scan.run(ctx); result.SuppressedCount != 1 || result.ReportedCount != 0 {
			t.Fatalf("the pass after the lift: %+v", result)
		}
		feeds.set(testAbuseFeedUrl, "other.acme."+testAbuseScanRoot+"\n")
		clock.advance(time.Hour)
		scan.run(ctx)
		feeds.set(testAbuseFeedUrl, host+"\n")
		clock.advance(time.Hour)
		if result := scan.run(ctx); result.ReportedCount != 1 || result.SuppressedCount != 0 {
			t.Fatalf("the listing back after a whole read without it: %+v", result)
		}
		if !slices.Equal(intake.reportedHosts(), []string{host, "other.acme." + testAbuseScanRoot, host}) {
			t.Fatalf("reported %q", intake.reportedHosts())
		}
	})
}
