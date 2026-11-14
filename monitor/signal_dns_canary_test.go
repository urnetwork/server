package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/pager"
)

const dnsCanaryTestRoot = "zone.example."

// The world one check sees, every seam a fake: the block, each NS by its first address, the validator
// and the public resolvers. Safe for concurrent use.
type dnsCanaryTestWorld struct {
	stateLock    sync.Mutex
	written      []string
	reports      []*dnsservice.DnsCanaryReport
	writeErr     error
	reportErr    error
	nsReads      map[string]*dnsCanaryNsRead
	validations  map[string]*dnsCanaryValidation
	validatorErr error
	resolved     map[netip.AddrPort][]string
	authentic    bool
	// a validating resolver refusing the canary as bogus: SERVFAIL, but the values with checking disabled
	bogusAt map[netip.AddrPort]bool
	// the questions asked with checking disabled
	uncheckedCount int
	// the clean-days gauges' last values, by root
	cleanDays map[string]int
	dsReady   map[string]bool
}

func (self *dnsCanaryTestWorld) publishCleanDays(source string, root string, cleanDays int, dsReady bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.cleanDays[root] = cleanDays
	self.dsReady[root] = dsReady
}

func (self *dnsCanaryTestWorld) writeCanary(ctx context.Context, root string, value string) (uint64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.writeErr != nil {
		return 0, self.writeErr
	}
	self.written = append(self.written, value)
	return uint64(len(self.written)), nil
}

func (self *dnsCanaryTestWorld) report(ctx context.Context, report *dnsservice.DnsCanaryReport) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.reportErr != nil {
		return self.reportErr
	}
	self.reports = append(self.reports, report)
	return nil
}

func (self *dnsCanaryTestWorld) read(ctx context.Context, root string, addresses []netip.AddrPort) (*dnsCanaryNsRead, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	read, ok := self.nsReads[addresses[0].String()]
	if !ok {
		return nil, errors.New("no answer")
	}
	return read, nil
}

func (self *dnsCanaryTestWorld) validate(ctx context.Context, root string, addresses []netip.AddrPort, anchors []string) (*dnsCanaryValidation, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.validatorErr != nil {
		return nil, self.validatorErr
	}
	validation, ok := self.validations[addresses[0].String()]
	if !ok {
		return &dnsCanaryValidation{}, nil
	}
	return validation, nil
}

func (self *dnsCanaryTestWorld) resolve(ctx context.Context, resolver netip.AddrPort, name string, checkingDisabled bool) (*dnsCanaryResolution, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if checkingDisabled {
		self.uncheckedCount += 1
	}
	values, ok := self.resolved[resolver]
	if !ok {
		return nil, errors.New("no answer")
	}
	if self.bogusAt[resolver] && !checkingDisabled {
		return &dnsCanaryResolution{rcode: dns.RcodeServerFailure, values: []string{}}, nil
	}
	return &dnsCanaryResolution{rcode: dns.RcodeSuccess, values: slices.Clone(values), authenticated: self.authentic && !checkingDisabled}, nil
}

// The fixed NS and resolvers, documentation addresses.
var (
	dnsCanaryTestNs1       = netip.MustParseAddrPort("192.0.2.53:53")
	dnsCanaryTestNs2       = netip.MustParseAddrPort("198.51.100.53:53")
	dnsCanaryTestResolvers = []netip.AddrPort{netip.MustParseAddrPort("203.0.113.1:53"), netip.MustParseAddrPort("203.0.113.2:53")}
	dnsCanaryTestNow       = time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
)

// A KSK's DNSKEY RDATA, a fresh key: the anchor dns.yml names.
func dnsCanaryTestAnchor(t testing.TB) string {
	t.Helper()
	record := &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: dnsCanaryTestRoot, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600},
		Flags:     dns.ZONE | dns.SEP,
		Protocol:  3,
		Algorithm: dns.ECDSAP256SHA256,
	}
	if _, err := record.Generate(256); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.TrimPrefix(record.String(), record.Header().String()))
}

// A check of source over a healthy world at dnsCanaryTestNow: both NS at one serial, a window with 10 d
// left, a fresh canary validating at each, the resolvers holding it.
func newDnsCanaryTestCheck(t testing.TB, source string) (*dnsCanaryCheck, *dnsCanaryTestWorld) {
	t.Helper()
	dnsSettings := dnsservice.DefaultDnsSettings()
	dnsSettings.Roots = []string{dnsCanaryTestRoot}
	dnsSettings.SignaturesPerSecond = 1
	dnsSettings.Tcp.ConnectionsPerSource = 1
	dnsSettings.Rrl.ResponsesPerSecond = 1
	dnsSettings.TrustAnchors = map[string][]string{dnsCanaryTestRoot: {dnsCanaryTestAnchor(t)}}
	if err := dnsSettings.Validate(); err != nil {
		t.Fatal(err)
	}
	settings := DefaultDnsCanarySettings()
	settings.Source = source
	settings.Dns = dnsSettings
	settings.NsAddresses = map[string][]netip.AddrPort{"ns1": {dnsCanaryTestNs1}, "ns2": {dnsCanaryTestNs2}}
	settings.PublicResolvers = dnsCanaryTestResolvers
	fresh := strconv.FormatInt(dnsCanaryTestNow.Add(-20*time.Second).UnixMilli(), 10) + " site"
	world := &dnsCanaryTestWorld{
		nsReads:     map[string]*dnsCanaryNsRead{},
		validations: map[string]*dnsCanaryValidation{},
		resolved:    map[netip.AddrPort][]string{},
		bogusAt:     map[netip.AddrPort]bool{},
		cleanDays:   map[string]int{},
		dsReady:     map[string]bool{},
	}
	for _, address := range []netip.AddrPort{dnsCanaryTestNs1, dnsCanaryTestNs2} {
		world.nsReads[address.String()] = &dnsCanaryNsRead{soaSerial: 41, windowEnd: dnsCanaryTestNow.Add(10 * 24 * time.Hour)}
		world.validations[address.String()] = &dnsCanaryValidation{answered: true, secure: true, values: []string{fresh}}
	}
	for _, resolver := range dnsCanaryTestResolvers {
		world.resolved[resolver] = []string{fresh}
	}
	check := &dnsCanaryCheck{settings: settings, api: world, ns: world, validator: world, resolver: world, clean: newDnsCanaryCleanCounter(), gauges: world}
	return check, world
}

// The findings' classes, sorted.
func dnsCanaryTestClasses(findings []finding) []string {
	classes := []string{}
	for _, finding := range findings {
		classes = append(classes, finding.class)
	}
	slices.Sort(classes)
	return classes
}

// A good minute: the site writes a canary of its unix milliseconds, raises nothing, and reports ok at the
// NS's serial; the outside copy writes nothing and reports as outside.
func TestDnsCanaryGoodMinuteReportsOk(t *testing.T) {
	check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	if findings := check.run(t.Context(), dnsCanaryTestNow); len(findings) != 0 {
		t.Fatalf("findings %v", dnsCanaryTestClasses(findings))
	}
	if len(world.written) != 1 || world.written[0] != strconv.FormatInt(dnsCanaryTestNow.UnixMilli(), 10)+" site" {
		t.Errorf("written %v", world.written)
	}
	if len(world.reports) != 1 || !world.reports[0].Ok || world.reports[0].Serial != 41 || world.reports[0].Source != dnsservice.DnsCanarySourceSite {
		t.Errorf("reports %+v", world.reports)
	}

	outside, outsideWorld := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceOutside)
	if findings := outside.run(t.Context(), dnsCanaryTestNow); len(findings) != 0 {
		t.Fatalf("outside findings %v", dnsCanaryTestClasses(findings))
	}
	if len(outsideWorld.written) != 0 || len(outsideWorld.reports) != 1 || outsideWorld.reports[0].Source != dnsservice.DnsCanarySourceOutside {
		t.Errorf("the outside copy wrote %v and reported %+v", outsideWorld.written, outsideWorld.reports)
	}
}

// Each failure raises its class, the page classes at page tier, and every one but a stale resolver turns
// the verdict to failing.
func TestDnsCanaryEachFailureRaisesItsClass(t *testing.T) {
	cases := []struct {
		name     string
		change   func(world *dnsCanaryTestWorld)
		classes  []string
		verdict  bool
		pageTier bool
	}{
		{name: "a broken signature", change: func(world *dnsCanaryTestWorld) {
			world.validations[dnsCanaryTestNs2.String()].secure = false
			world.validations[dnsCanaryTestNs2.String()].bogus = true
		}, classes: []string{dnsCanaryClassSignature}, pageTier: true},
		{name: "an unsigned answer under the anchor", change: func(world *dnsCanaryTestWorld) {
			world.validations[dnsCanaryTestNs1.String()].secure = false
		}, classes: []string{dnsCanaryClassSignature}, pageTier: true},
		{name: "both NS down", change: func(world *dnsCanaryTestWorld) {
			delete(world.nsReads, dnsCanaryTestNs1.String())
			delete(world.nsReads, dnsCanaryTestNs2.String())
		}, classes: []string{dnsCanaryClassNsDown}, pageTier: true},
		{name: "one NS down", change: func(world *dnsCanaryTestWorld) {
			delete(world.nsReads, dnsCanaryTestNs1.String())
		}, classes: []string{dnsCanaryClassNsFailing}, pageTier: true},
		{name: "one NS serving an old canary", change: func(world *dnsCanaryTestWorld) {
			world.validations[dnsCanaryTestNs1.String()].values = []string{strconv.FormatInt(dnsCanaryTestNow.Add(-10*time.Minute).UnixMilli(), 10) + " site"}
		}, classes: []string{dnsCanaryClassNsFailing}, pageTier: true},
		{name: "serials apart", change: func(world *dnsCanaryTestWorld) {
			world.nsReads[dnsCanaryTestNs2.String()].soaSerial = 39
		}, classes: []string{dnsCanaryClassDivergence}, verdict: true},
		{name: "a window with 7 d left", change: func(world *dnsCanaryTestWorld) {
			world.nsReads[dnsCanaryTestNs1.String()].windowEnd = dnsCanaryTestNow.Add(7 * 24 * time.Hour)
		}, classes: []string{dnsCanaryClassKeyWindow}, verdict: true},
		{name: "a resolver without the canary", change: func(world *dnsCanaryTestWorld) {
			delete(world.resolved, dnsCanaryTestResolvers[1])
		}, classes: []string{dnsCanaryClassPropagation}, verdict: true},
		{name: "a blind validator", change: func(world *dnsCanaryTestWorld) {
			world.validatorErr = errors.New("no unbound-host")
		}, classes: []string{dnsCanaryClassUnobservable, dnsCanaryClassUnobservable}},
	}
	for _, c := range cases {
		check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
		c.change(world)
		findings := check.run(t.Context(), dnsCanaryTestNow)
		if got := dnsCanaryTestClasses(findings); !slices.Equal(got, c.classes) {
			t.Errorf("%s: classes %v, want %v", c.name, got, c.classes)
			continue
		}
		if c.pageTier && findings[0].tier != tierPage {
			t.Errorf("%s: tier %s", c.name, findings[0].tier)
		}
		if len(world.reports) != 1 || world.reports[0].Ok != c.verdict {
			t.Errorf("%s: reports %+v", c.name, world.reports)
		}
	}
}

// A write or report the block refused is a blind check, never an incident of the zone, and the DS turns
// the resolvers' check into validation.
func TestDnsCanaryBlockRefusalsAreBlindAndTheDsValidates(t *testing.T) {
	check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	world.writeErr = errors.New("503")
	world.reportErr = errors.New("503")
	findings := check.run(t.Context(), dnsCanaryTestNow)
	if got := dnsCanaryTestClasses(findings); !slices.Equal(got, []string{dnsCanaryClassUnobservable, dnsCanaryClassUnobservable}) {
		t.Fatalf("classes %v", got)
	}
	for _, finding := range findings {
		if !finding.unobservable {
			t.Errorf("%+v is not a visibility finding", finding)
		}
	}

	ds, dsWorld := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	ds.settings.Dns.DsPublished = []string{dnsCanaryTestRoot}
	if got := dnsCanaryTestClasses(ds.run(t.Context(), dnsCanaryTestNow)); !slices.Equal(got, []string{dnsCanaryClassPropagation, dnsCanaryClassPropagation}) {
		t.Errorf("unvalidated resolvers after the DS: %v", got)
	}
	dsWorld.authentic = true
	if got := ds.run(t.Context(), dnsCanaryTestNow); len(got) != 0 {
		t.Errorf("validating resolvers after the DS: %v", dnsCanaryTestClasses(got))
	}
}

// FS-50's dns case at 02:00: both NS failing reaches the rota's pager sink at once and pages the second
// person at 30 minutes unacknowledged; a blind minute holds the page open, and good minutes resolve it.
func TestDnsCanaryNsDownAt0200PagesAndABlindMinuteHoldsIt(t *testing.T) {
	check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	signal := NewDnsCanarySignal()
	harness := newPagerHarness()
	alerts := func(findings []finding) Alerts {
		alerts := Alerts{}
		for _, finding := range findings {
			alerts = append(alerts, alertFromFinding(SignalSettings{Environment: "synthetic", Now: harness.clock.Now}, signal.Number(), signal.Key(), signal.Name(), finding))
		}
		return alerts
	}
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)

	delete(world.nsReads, dnsCanaryTestNs1.String())
	delete(world.nsReads, dnsCanaryTestNs2.String())
	notifier.Handle(signal, SignalExecution{Alerts: alerts(check.run(t.Context(), dnsCanaryTestNow))})
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	if page := harness.sink.Notifications()[0].Page; page.Class != dnsCanaryClassNsDown || page.BothAtOnce {
		t.Fatalf("paged %+v", page)
	}
	harness.clock.Advance(30 * time.Minute)
	harness.sink.Tick()
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeSecondPerson)

	// the NS come back, but the validator is blind: nothing clears
	healthy, healthyWorld := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	healthyWorld.validatorErr = errors.New("no unbound-host")
	for range 3 {
		notifier.Handle(signal, SignalExecution{Alerts: alerts(healthy.run(t.Context(), dnsCanaryTestNow))})
	}
	flushPager(t, notifier)
	if open := harness.sink.OpenKeys(); len(open) != 1 {
		t.Fatalf("a blind minute closed the page: %+v", open)
	}
	healthyWorld.validatorErr = nil
	for range 3 {
		notifier.Handle(signal, SignalExecution{Alerts: alerts(healthy.run(t.Context(), dnsCanaryTestNow))})
	}
	flushPager(t, notifier)
	if open := harness.sink.OpenKeys(); len(open) != 0 {
		t.Fatalf("good minutes left the page open: %+v", open)
	}
}

// With no block the canary reports itself configured off, which raises no incident; with a block that
// does not read it reports itself blind, which holds every other incident open.
func TestDnsCanaryWithNoSettingsIsOffAndWithBadSettingsBlind(t *testing.T) {
	now := func() time.Time { return dnsCanaryTestNow }
	findings, err := dnsCanaryProbe{}.check(t.Context(), &probeEnv{cfg: &monitorConfig{}, now: now})
	if err != nil || len(findings) != 1 || !findings[0].configuredOff || findings[0].class != dnsCanaryClassUnobservable {
		t.Fatalf("no block: findings %+v %v", findings, err)
	}
	broken := &monitorConfig{dnsCanary: &DnsCanarySettings{ConfigError: "dns_canary.bearer_file is not in vault"}}
	findings, err = dnsCanaryProbe{}.check(t.Context(), &probeEnv{cfg: broken, now: now})
	if err != nil || len(findings) != 1 || findings[0].configuredOff || !findings[0].unobservable || findings[0].observed != broken.dnsCanary.ConfigError {
		t.Fatalf("a block that does not read: findings %+v %v", findings, err)
	}
	if block := loadDnsCanarySettings(&dnsCanaryYaml{Source: "elsewhere"}); block == nil || block.ConfigError == "" {
		t.Errorf("an unknown source %+v", block)
	}
	if block := loadDnsCanarySettings(nil); block != nil {
		t.Errorf("no block %+v", block)
	}
}

// unbound-host -v's lines: secure, bogus, insecure under an anchor, and no answer; and the configuration
// a run writes, pinned to one NS with the KSK as its only anchor.
func TestDnsCanaryReadsUnboundHost(t *testing.T) {
	cases := []struct {
		output    string
		answered  bool
		secure    bool
		bogus     bool
		firstText string
	}{
		{output: "canary.zone.example has TXT record \"1762480800000 site\" (secure)\n", answered: true, secure: true, firstText: "1762480800000 site"},
		{output: "canary.zone.example has TXT record \"1762480800000 site\" (BOGUS (security failure))\nvalidation failure <canary.zone.example. TXT IN>: signature crypto failed\n", answered: true, bogus: true, firstText: "1762480800000 site"},
		{output: "canary.zone.example has TXT record \"1762480800000 site\" (insecure)\n", answered: true, bogus: true, firstText: "1762480800000 site"},
		{output: "Host canary.zone.example not found: 2(SERVFAIL).\n", answered: false},
	}
	for _, c := range cases {
		validation := dnsCanaryParseUnbound(c.output)
		if validation.answered != c.answered || validation.secure != c.secure || validation.bogus != c.bogus {
			t.Errorf("%q: %+v", c.output, validation)
		}
		if c.firstText != "" && (len(validation.values) == 0 || validation.values[0] != c.firstText) {
			t.Errorf("%q: values %v", c.output, validation.values)
		}
	}
	config := dnsCanaryUnboundConfig(dnsCanaryTestRoot, []netip.AddrPort{dnsCanaryTestNs1}, "/anchors.key")
	for _, want := range []string{`trust-anchor-file: "/anchors.key"`, `name: "zone.example."`, "stub-addr: 192.0.2.53@53"} {
		if !strings.Contains(config, want) {
			t.Errorf("config lacks %q:\n%s", want, config)
		}
	}
	anchors := dnsCanaryUnboundAnchors(dnsCanaryTestRoot, []string{"257 3 13 abc="})
	if anchors != "zone.example. 3600 IN DNSKEY 257 3 13 abc=\n" {
		t.Errorf("anchors %q", anchors)
	}
	var ran []string
	unbound := &dnsCanaryUnbound{path: "/unbound-host", run: func(ctx context.Context, path string, args ...string) (string, error) {
		ran = append([]string{path}, args...)
		return "canary.zone.example has TXT record \"1 site\" (secure)\n", nil
	}}
	validation, err := unbound.validate(t.Context(), dnsCanaryTestRoot, []netip.AddrPort{dnsCanaryTestNs1}, []string{"257 3 13 abc="})
	if err != nil || !validation.secure || len(ran) == 0 || ran[0] != "/unbound-host" || ran[len(ran)-1] != "canary.zone.example." {
		t.Errorf("validate %+v %v ran %v", validation, err, ran)
	}
}

// Every NS and resolver holds a canary written 20 s before now, so the minute at now is clean.
func (self *dnsCanaryTestWorld) freshAt(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	fresh := strconv.FormatInt(now.Add(-20*time.Second).UnixMilli(), 10) + " site"
	for _, validation := range self.validations {
		validation.values = []string{fresh}
	}
	for resolver := range self.resolved {
		self.resolved[resolver] = []string{fresh}
	}
	for _, read := range self.nsReads {
		read.windowEnd = now.Add(10 * 24 * time.Hour)
	}
}

// The check's minute at now on a world fresh then, and the root's clean days and readiness it reported.
func dnsCanaryTestMinute(t testing.TB, check *dnsCanaryCheck, world *dnsCanaryTestWorld, now time.Time) (int, bool) {
	t.Helper()
	world.freshAt(now)
	check.run(t.Context(), now)
	world.stateLock.Lock()
	defer world.stateLock.Unlock()
	return world.cleanDays[dnsCanaryTestRoot], world.dsReady[dnsCanaryTestRoot]
}

// D-DNS-5's count, hour by hour on the fake clock with the gap widened to fit, in memory (the next test
// holds the state directory's file to the same count): the DS is not reported ready on the 29th clean
// day and is on the 30th, and a failed canary resets the count, which then starts again from the next
// clean minute.
func TestDnsCanaryFailedCanaryResetsTheCleanDays(t *testing.T) {
	check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	check.settings.CleanGap = 2 * time.Hour
	now := dnsCanaryTestNow
	dnsCanaryTestMinute(t, check, world, now)
	for range 29 * 24 {
		now = now.Add(time.Hour)
		dnsCanaryTestMinute(t, check, world, now)
	}
	days, ready := dnsCanaryTestMinute(t, check, world, now.Add(30*time.Minute))
	if days != 29 || ready {
		t.Fatalf("29 clean days: %d, ready %v", days, ready)
	}
	for range 24 {
		now = now.Add(time.Hour)
		days, ready = dnsCanaryTestMinute(t, check, world, now)
	}
	if days != 30 || !ready {
		t.Fatalf("30 clean days: %d, ready %v", days, ready)
	}

	// one NS's canary fails validation: the count is gone at once
	now = now.Add(time.Hour)
	world.freshAt(now)
	world.validations[dnsCanaryTestNs2.String()].secure = false
	world.validations[dnsCanaryTestNs2.String()].bogus = true
	check.run(t.Context(), now)
	if days, ready := world.cleanDays[dnsCanaryTestRoot], world.dsReady[dnsCanaryTestRoot]; days != 0 || ready {
		t.Fatalf("after a failed canary: %d, ready %v", days, ready)
	}
	world.validations[dnsCanaryTestNs2.String()].secure = true
	world.validations[dnsCanaryTestNs2.String()].bogus = false
	for range 25 {
		now = now.Add(time.Hour)
		days, ready = dnsCanaryTestMinute(t, check, world, now)
	}
	if days != 1 || ready {
		t.Fatalf("a day after the failure: %d, ready %v", days, ready)
	}
}

// The run lives in the state directory, so a new process's counter goes on with it; a minute no NS
// proved clean, one NS down, neither extends nor ends it; a stretch past the gap with no clean minute
// restarts it; and a file that does not read restarts it too.
func TestDnsCanaryCleanDaysOutliveARestartAndLapseOverAGap(t *testing.T) {
	check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	stateDir := t.TempDir()
	check.stateDir = stateDir
	check.settings.CleanGap = 2 * time.Hour
	now := dnsCanaryTestNow
	for range 3 * 24 {
		now = now.Add(time.Hour)
		dnsCanaryTestMinute(t, check, world, now)
	}
	// a new process: the counter starts empty and reads the file
	check.clean = newDnsCanaryCleanCounter()
	now = now.Add(time.Hour)
	if days, _ := dnsCanaryTestMinute(t, check, world, now); days != 3 {
		t.Fatalf("after a restart: %d days", days)
	}

	// one NS down for an hour: no failure, no proof
	now = now.Add(time.Hour)
	world.freshAt(now)
	delete(world.nsReads, dnsCanaryTestNs1.String())
	check.run(t.Context(), now)
	if days := world.cleanDays[dnsCanaryTestRoot]; days != 3 {
		t.Fatalf("an unobserved minute inside the gap: %d days", days)
	}
	world.nsReads[dnsCanaryTestNs1.String()] = &dnsCanaryNsRead{soaSerial: 41, windowEnd: now.Add(10 * 24 * time.Hour)}
	now = now.Add(time.Hour)
	if days, _ := dnsCanaryTestMinute(t, check, world, now); days != 3 {
		t.Fatalf("a clean minute after it: %d days", days)
	}

	// the monitor stops for longer than the gap
	now = now.Add(3 * time.Hour)
	if days, _ := dnsCanaryTestMinute(t, check, world, now); days != 0 {
		t.Fatalf("after a gap past CleanGap: %d days", days)
	}
	for range 25 {
		now = now.Add(time.Hour)
		dnsCanaryTestMinute(t, check, world, now)
	}
	if days := world.cleanDays[dnsCanaryTestRoot]; days != 1 {
		t.Fatalf("a day into the new run: %d", days)
	}

	if err := os.WriteFile(dnsCanaryCleanPath(stateDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if days, _ := dnsCanaryTestMinute(t, check, world, now); days != 0 {
		t.Fatalf("a file that does not read: %d days", days)
	}
	now = now.Add(time.Hour)
	if days, _ := dnsCanaryTestMinute(t, check, world, now); days != 0 {
		t.Fatalf("an hour into the restarted run: %d days", days)
	}
	if info, err := os.Stat(filepath.Dir(dnsCanaryCleanPath(stateDir))); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the counter's directory: %v %v", info, err)
	}
}

// Until the DS a public resolver checks propagation only: an answer without AD raises nothing, and a
// SERVFAIL is propagation, never asked again. Once dns.yml's ds_published lists the root, an answer
// without AD warns, and a SERVFAIL the resolver answers with checking disabled is the canary failing
// for every validator: a page, which also ends the clean-days run.
func TestDnsCanaryPublicResolversFailingValidationRaiseAnIncidentOnlyWithTheDs(t *testing.T) {
	check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	world.authentic = false
	if got := check.run(t.Context(), dnsCanaryTestNow); len(got) != 0 {
		t.Fatalf("unvalidated answers before the DS: %v", dnsCanaryTestClasses(got))
	}
	world.bogusAt[dnsCanaryTestResolvers[0]] = true
	if got := dnsCanaryTestClasses(check.run(t.Context(), dnsCanaryTestNow)); !slices.Equal(got, []string{dnsCanaryClassPropagation}) {
		t.Fatalf("a SERVFAIL before the DS: %v", got)
	}
	if world.uncheckedCount != 0 {
		t.Errorf("asked with checking disabled before the DS: %d", world.uncheckedCount)
	}

	ds, dsWorld := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	ds.settings.Dns.DsPublished = []string{dnsCanaryTestRoot}
	dsWorld.authentic = true
	if got := ds.run(t.Context(), dnsCanaryTestNow); len(got) != 0 {
		t.Fatalf("validated answers after the DS: %v", dnsCanaryTestClasses(got))
	}
	if days := dsWorld.cleanDays[dnsCanaryTestRoot]; days != 0 || dsWorld.dsReady[dnsCanaryTestRoot] {
		t.Fatalf("a first clean minute: %d days", days)
	}
	dsWorld.bogusAt[dnsCanaryTestResolvers[0]] = true
	findings := ds.run(t.Context(), dnsCanaryTestNow.Add(time.Minute))
	if got := dnsCanaryTestClasses(findings); !slices.Equal(got, []string{dnsCanaryClassSignature}) {
		t.Fatalf("a bogus canary at a public resolver after the DS: %v", got)
	}
	if findings[0].tier != tierPage || !strings.HasPrefix(findings[0].target, dnsCanaryTestResolvers[0].Addr().String()) {
		t.Errorf("finding %+v", findings[0])
	}
	if dsWorld.uncheckedCount != 1 {
		t.Errorf("questions with checking disabled: %d", dsWorld.uncheckedCount)
	}
	if run := ds.clean.memoryRuns[dnsservice.DnsCanarySourceSite+" "+dnsCanaryTestRoot]; run != nil {
		t.Errorf("a bogus public answer left the run %+v", run)
	}
	// a SERVFAIL with nothing behind it is the resolver failing, not the canary
	delete(dsWorld.resolved, dnsCanaryTestResolvers[0])
	if got := dnsCanaryTestClasses(ds.run(t.Context(), dnsCanaryTestNow)); !slices.Equal(got, []string{dnsCanaryClassPropagation}) {
		t.Fatalf("a resolver down after the DS: %v", got)
	}
}

// Ready never below the setting, and never with none; the process's gauges carry both series by source
// and root.
func TestDnsCanaryDsReadyGauges(t *testing.T) {
	if dnsCanaryDsReady(29, 30) || !dnsCanaryDsReady(30, 30) || !dnsCanaryDsReady(31, 30) || dnsCanaryDsReady(100, 0) {
		t.Fatalf("readiness")
	}
	registry := prometheus.NewRegistry()
	gauges := newPrometheusDnsCanaryGauges(registry)
	gauges.publishCleanDays(dnsservice.DnsCanarySourceSite, dnsCanaryTestRoot, 30, true)
	again := newPrometheusDnsCanaryGauges(registry)
	again.publishCleanDays(dnsservice.DnsCanarySourceOutside, dnsCanaryTestRoot, 4, false)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := []string{}
			for _, label := range metric.GetLabel() {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			slices.Sort(labels)
			values[family.GetName()+" "+strings.Join(labels, ",")] = metric.GetGauge().GetValue()
		}
	}
	want := map[string]float64{
		dnsCanaryCleanDaysMetricName + " root=" + dnsCanaryTestRoot + ",source=site":    30,
		dnsDsReadyMetricName + " root=" + dnsCanaryTestRoot + ",source=site":            1,
		dnsCanaryCleanDaysMetricName + " root=" + dnsCanaryTestRoot + ",source=outside": 4,
		dnsDsReadyMetricName + " root=" + dnsCanaryTestRoot + ",source=outside":         0,
	}
	for key, value := range want {
		if got, ok := values[key]; !ok || got != value {
			t.Errorf("%s: %v %v", key, got, ok)
		}
	}
}

// The registered signal's probe keeps its run in the state directory and reports it to its gauges; a bare probe, as
// check-burn runs one, reads and writes neither, so a rollout gate's one-shot run never moves the monitor's count.
func TestDnsCanaryOnlyTheRegisteredProbeKeepsTheCount(t *testing.T) {
	block := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`{"serial":1}`))
	}))
	defer block.Close()
	check, world := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	settings := check.settings
	settings.ApiUrl = block.URL
	// no NS address and no resolver, so nothing is dialed: each NS fails its read
	settings.NsAddresses = map[string][]netip.AddrPort{}
	settings.PublicResolvers = nil
	stateDir := t.TempDir()
	env := &probeEnv{cfg: &monitorConfig{dnsCanary: settings, stateDir: stateDir}, now: func() time.Time { return dnsCanaryTestNow }}
	if _, err := (dnsCanaryProbe{}).check(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dnsCanaryCleanPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a bare probe touched the count: %v", err)
	}
	probe := dnsCanaryProbe{clean: newDnsCanaryCleanCounter(), gauges: func() dnsCanaryGauges { return world }}
	if _, err := probe.check(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dnsCanaryCleanPath(stateDir)); err != nil {
		t.Fatalf("the registered probe kept no count: %v", err)
	}
	if days, ok := world.cleanDays[dnsCanaryTestRoot]; !ok || days != 0 || world.dsReady[dnsCanaryTestRoot] {
		t.Errorf("gauges %v %v", world.cleanDays, world.dsReady)
	}
}
