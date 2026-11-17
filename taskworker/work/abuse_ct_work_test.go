package work

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// §21.6's CT watch (abuse_ct_work.go) on fake CT sources: a name the screen's lists refuse goes to §21.8 as a report
// with no machine action, names the lists allow or that no network holds go nowhere, the cursor rides the task's args
// and resumes after a batch that failed, and a change of the lists reads CT again from the start. The store case
// runs §21.8's real intake (-tags sidecar_pending); the rest run on fakes.

// A CT source that answers batches of certificates in order, each batch's cursor naming it, and fails the reads
// it is told to, counted from its first.
type testAbuseCtSource struct {
	stateLock sync.Mutex
	name      string
	batches   [][]*sidecar.TlsCtCertificate
	// the reads that fail, by their index
	failReads []int
	// the cursor each read asked from
	readCursors []string
}

func (self *testAbuseCtSource) Name() string {
	return self.name
}

func (self *testAbuseCtSource) Issuances(ctx context.Context, domain string, cursor string) ([]*sidecar.TlsCtCertificate, string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	read := len(self.readCursors)
	self.readCursors = append(self.readCursors, cursor)
	if slices.Contains(self.failReads, read) {
		return nil, "", sidecar.ErrTlsCtSource
	}
	next := 0
	for index := range self.batches {
		if cursor == testAbuseCtCursor(index) {
			next = index + 1
		}
	}
	if len(self.batches) <= next {
		return nil, cursor, nil
	}
	return self.batches[next], testAbuseCtCursor(next), nil
}

func (self *testAbuseCtSource) cursors() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.readCursors)
}

func testAbuseCtCursor(index int) string {
	return "issuance-" + string(rune('a'+index))
}

func testAbuseCtCertificate(names ...string) *sidecar.TlsCtCertificate {
	return &sidecar.TlsCtCertificate{Source: "fake", DnsNames: names}
}

// A recording accept, and the network a label names.
type testAbuseCtIntake struct {
	stateLock     sync.Mutex
	reportedNames []string
	labelNetworks map[string]server.Id
}

func (self *testAbuseCtIntake) deps(clock *testAbuseScanClock, sources ...sidecar.TlsCtSource) *abuseCtWatchDeps {
	return &abuseCtWatchDeps{
		now:    clock.now,
		config: server.GetAbuseConfig,
		sources: func() ([]sidecar.TlsCtSource, error) {
			return sources, nil
		},
		labelNetwork: func(ctx context.Context, root string, label string) (server.Id, bool, error) {
			networkId, ok := self.labelNetworks[label]
			return networkId, ok, nil
		},
		screenFacts: func(ctx context.Context, networkId server.Id, now time.Time) *model.NetworkScreenFacts {
			return &model.NetworkScreenFacts{}
		},
		accept: func(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if report.Category != model.AbuseCategoryBrand || report.ReporterClass != model.AbuseReporterClassCtWatch {
				return nil, errors.New("a ct report of another category or class")
			}
			self.reportedNames = append(self.reportedNames, report.Url)
			return &controller.AbuseIntakeResult{Joined: true}, nil
		},
	}
}

func (self *testAbuseCtIntake) reported() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.reportedNames)
}

// The composed path on the real store: a host label holding a brand and a keyword, which the lists now refuse, opens
// one case of category brand with no action; an allowed host, the network's own label, the root, a label no network
// holds and a name under another domain open nothing. The cursor moves past the batch.
func TestAbuseCtWatchReportsARefusedNameWithNoAction(t *testing.T) {
	requireAbuseScanStore(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		_, label := newTestAbuseScanZone(t, ctx)
		if verdict := sidecar.ScreenName(label, &sidecar.ScreenFacts{}); verdict != sidecar.ScreenVerdictAllow {
			t.Fatalf("the fixture's label screens %s", verdict)
		}
		zone := label + "." + testAbuseScanRoot
		source := &testAbuseCtSource{
			name: sidecar.TlsCtSourceCertSpotter,
			batches: [][]*sidecar.TlsCtCertificate{{
				testAbuseCtCertificate("paypal-login."+zone, "shop."+zone),
				testAbuseCtCertificate("*."+zone, zone),
				testAbuseCtCertificate(testAbuseScanRoot, "*."+testAbuseScanRoot),
				testAbuseCtCertificate("paypal-login.nobodyholds."+testAbuseScanRoot, "paypal-login.elsewhere.example"),
			}},
		}
		clock := newTestAbuseScanClock(server.NowUtc())
		deps := defaultAbuseCtWatchDeps()
		deps.now = clock.now
		deps.sources = func() ([]sidecar.TlsCtSource, error) {
			return []sidecar.TlsCtSource{source}, nil
		}
		watch := &abuseCtWatch{settings: DefaultAbuseCtWatchSettings(), deps: deps}

		result := watch.run(ctx, &AbuseCtWatchArgs{})
		if result.Failed || result.Idle || result.CertificateCount != 4 || result.RefusedCount != 1 || result.ReportedCount != 1 {
			t.Fatalf("the watch: %+v", result)
		}
		if count := testAbuseScanCount(t, ctx, `SELECT COUNT(*) FROM abuse_case WHERE category = 'brand' AND reporter_class = 'ct_watch' AND target = $1`, "paypal-login."+zone); count != 1 {
			t.Fatalf("%d brand cases for the refused name, want one", count)
		}
		if count := testAbuseScanCaseCount(t, ctx); count != 1 {
			t.Fatalf("%d cases, want only the refused name's", count)
		}
		if count := testAbuseScanCount(t, ctx, `SELECT COUNT(*) FROM abuse_action`); count != 0 {
			t.Fatalf("a ct report took %d actions", count)
		}
		if cursor := result.Cursors[testAbuseScanRoot+" "+sidecar.TlsCtSourceCertSpotter]; cursor != testAbuseCtCursor(0) {
			t.Fatalf("the cursor %q did not move past the batch", cursor)
		}
	})
}

// A batch that fails, at the source or at intake, leaves the cursor before it, and the next run, taking the result's
// cursors as its args, reads from there rather than from the start. A source that fails acts on nothing.
func TestAbuseCtWatchResumesAfterAFailedBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		key := testAbuseScanRoot + " " + sidecar.TlsCtSourceCrtSh
		batches := [][]*sidecar.TlsCtCertificate{
			{testAbuseCtCertificate("paypal.zephyr." + testAbuseScanRoot)},
			{testAbuseCtCertificate("paypal-secure.zephyr." + testAbuseScanRoot)},
			{testAbuseCtCertificate("shop.zephyr." + testAbuseScanRoot)},
		}
		newIntake := func() *testAbuseCtIntake {
			return &testAbuseCtIntake{labelNetworks: map[string]server.Id{"zephyr": server.NewId()}}
		}

		// the source fails at its second read: the first batch stands, and the next run reads after it
		source := &testAbuseCtSource{name: sidecar.TlsCtSourceCrtSh, batches: batches, failReads: []int{1}}
		intake := newIntake()
		watch := &abuseCtWatch{settings: DefaultAbuseCtWatchSettings(), deps: intake.deps(clock, source)}
		result := watch.run(ctx, &AbuseCtWatchArgs{})
		if !result.Failed || result.Cursors[key] != testAbuseCtCursor(0) || !slices.Equal(intake.reported(), []string{"paypal.zephyr." + testAbuseScanRoot}) {
			t.Fatalf("a source failing at its second read: %+v, reported %q", result, intake.reported())
		}
		result = watch.run(ctx, &AbuseCtWatchArgs{Cursors: result.Cursors, ListsSha256: result.ListsSha256})
		if result.Failed || result.Cursors[key] != testAbuseCtCursor(2) {
			t.Fatalf("the resumed run: %+v", result)
		}
		if cursors := source.cursors(); !slices.Equal(cursors, []string{"", testAbuseCtCursor(0), testAbuseCtCursor(0), testAbuseCtCursor(1), testAbuseCtCursor(2)}) {
			t.Fatalf("reads from %q: the resumed run must read after the first batch", cursors)
		}
		if !slices.Equal(intake.reported(), []string{"paypal.zephyr." + testAbuseScanRoot, "paypal-secure.zephyr." + testAbuseScanRoot}) {
			t.Fatalf("reported %q", intake.reported())
		}

		// intake fails on the second batch's name: the cursor stays before that batch, which the next run reads again
		source = &testAbuseCtSource{name: sidecar.TlsCtSourceCrtSh, batches: batches}
		intake = newIntake()
		watch = &abuseCtWatch{settings: DefaultAbuseCtWatchSettings(), deps: intake.deps(clock, source)}
		accept := watch.deps.accept
		acceptCount := 0
		watch.deps.accept = func(ctx context.Context, report *model.AbuseReport) (*controller.AbuseIntakeResult, error) {
			acceptCount += 1
			if acceptCount == 2 {
				return nil, errors.New("abuse intake: the store did not answer")
			}
			return accept(ctx, report)
		}
		result = watch.run(ctx, &AbuseCtWatchArgs{})
		if !result.Failed || result.Cursors[key] != testAbuseCtCursor(0) {
			t.Fatalf("a batch whose report failed moved the cursor: %+v", result)
		}
		result = watch.run(ctx, &AbuseCtWatchArgs{Cursors: result.Cursors, ListsSha256: result.ListsSha256})
		if result.Failed || result.Cursors[key] != testAbuseCtCursor(2) {
			t.Fatalf("the run after the failed report: %+v", result)
		}
		if !slices.Equal(intake.reported(), []string{"paypal.zephyr." + testAbuseScanRoot, "paypal-secure.zephyr." + testAbuseScanRoot}) {
			t.Fatalf("reported %q", intake.reported())
		}

		// a source that fails at once acts on nothing and keeps its cursor
		source = &testAbuseCtSource{name: sidecar.TlsCtSourceCrtSh, batches: batches, failReads: []int{0}}
		intake = newIntake()
		watch = &abuseCtWatch{settings: DefaultAbuseCtWatchSettings(), deps: intake.deps(clock, source)}
		result = watch.run(ctx, &AbuseCtWatchArgs{Cursors: map[string]string{key: testAbuseCtCursor(0)}, ListsSha256: abuseCtScreenListsSha256()})
		if !result.Failed || result.Cursors[key] != testAbuseCtCursor(0) || 0 < len(intake.reported()) {
			t.Fatalf("a source down: %+v, reported %q", result, intake.reported())
		}
	})
}

// Cursors read under another version of the lists are dropped, so the watch reads CT again from the start; with no
// root the watch idles and keeps its cursors; a cursor for a source no longer watched goes.
func TestAbuseCtWatchReadsAgainWhenTheListsChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
		source := &testAbuseCtSource{name: sidecar.TlsCtSourceCertSpotter}
		intake := &testAbuseCtIntake{labelNetworks: map[string]server.Id{}}
		watch := &abuseCtWatch{settings: DefaultAbuseCtWatchSettings(), deps: intake.deps(clock, source)}
		key := testAbuseScanRoot + " " + sidecar.TlsCtSourceCertSpotter
		old := map[string]string{key: testAbuseCtCursor(0), "gone.example crtsh": "7"}

		pop := server.Testing_PushAbuseConfig("feeds: []\n")
		idle := watch.run(ctx, &AbuseCtWatchArgs{Cursors: old, ListsSha256: "an older list"})
		pop()
		if !idle.Idle || idle.Cursors[key] != testAbuseCtCursor(0) || idle.ListsSha256 != "an older list" || 0 < len(source.cursors()) {
			t.Fatalf("no root: %+v", idle)
		}

		pushTestAbuseScanConfig(t, testAbuseScanConfig)
		result := watch.run(ctx, &AbuseCtWatchArgs{Cursors: old, ListsSha256: "an older list"})
		if result.Failed || result.ListsSha256 != abuseCtScreenListsSha256() {
			t.Fatalf("new lists: %+v", result)
		}
		if cursors := source.cursors(); len(cursors) != 1 || cursors[0] != "" {
			t.Fatalf("new lists read from %q, want the start", cursors)
		}
		if _, ok := result.Cursors["gone.example crtsh"]; ok {
			t.Fatal("a cursor for a root no longer watched stayed")
		}

		// the same lists keep the cursors
		source.readCursors = nil
		kept := watch.run(ctx, &AbuseCtWatchArgs{Cursors: map[string]string{key: testAbuseCtCursor(0)}, ListsSha256: abuseCtScreenListsSha256()})
		if cursors := source.cursors(); kept.Failed || len(cursors) != 1 || cursors[0] != testAbuseCtCursor(0) {
			t.Fatalf("the same lists read from %q", cursors)
		}
	})
}

// What a certificate's name reports: the label when the lists refuse it, the whole name for a refused host label,
// nothing for an allowed name, the root, another root's name, a malformed name or a label no network holds; a
// verified brand domain lets the brand's own name pass, and a reviewer's screen_allow the one name it names
// (ABUSE-M5-08).
func TestAbuseCtWatchRefusedName(t *testing.T) {
	ctx := context.Background()
	clock := newTestAbuseScanClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	verifiedNetworkId := server.NewId()
	allowedNetworkId := server.NewId()
	intake := &testAbuseCtIntake{labelNetworks: map[string]server.Id{
		"zephyr":        server.NewId(),
		"paypal-secure": server.NewId(),
		"verified":      verifiedNetworkId,
		"allowed":       allowedNetworkId,
	}}
	deps := intake.deps(clock)
	deps.screenFacts = func(ctx context.Context, networkId server.Id, now time.Time) *model.NetworkScreenFacts {
		switch networkId {
		case verifiedNetworkId:
			return &model.NetworkScreenFacts{VerifiedDomains: []string{"paypal.com"}}
		case allowedNetworkId:
			return &model.NetworkScreenFacts{AllowedNames: []string{"paypal-login"}}
		}
		return &model.NetworkScreenFacts{}
	}
	watch := &abuseCtWatch{settings: DefaultAbuseCtWatchSettings(), deps: deps}
	cases := map[string]string{
		"shop.zephyr.root.example":            "",
		"paypal-login.zephyr.root.example":    "paypal-login.zephyr.root.example",
		"paypal.zephyr.root.example":          "paypal.zephyr.root.example",
		"*.paypal-secure.root.example":        "paypal-secure.root.example",
		"www.paypal-secure.root.example":      "paypal-secure.root.example",
		"paypal.verified.root.example":        "",
		"paypal-login.verified.root.example":  "paypal-login.verified.root.example",
		"paypal-login.allowed.root.example":   "",
		"paypal-secure.allowed.root.example":  "paypal-secure.allowed.root.example",
		"_acme-challenge.zephyr.root.example": "",
		"root.example":                        "",
		"*.root.example":                      "",
		"paypal.other.example":                "",
		"paypal..zephyr.root.example":         "",
		"paypal.nobody.root.example":          "",
	}
	for dnsName, want := range cases {
		got, err := watch.refusedName(ctx, testAbuseScanRoot, dnsName, clock.now(), map[server.Id]*model.NetworkScreenFacts{}, &AbuseCtWatchResult{})
		if err != nil || got != want {
			t.Errorf("%s: %q, %v, want %q", dnsName, got, err, want)
		}
	}
}
