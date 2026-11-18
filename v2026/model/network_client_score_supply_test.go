// The admitted supply the score export measures per country target and
// request IP family, and the target country an empty answer names, without
// any client, network, provider or location identity. Every value below is
// synthetic.
package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
)

func supplyTestScore(families uint8, online bool, quality bool) *ClientScore {
	return &ClientScore{
		ClientId:       server.NewId(),
		IpFamilies:     families,
		Online:         online,
		PassesMinimums: map[string]bool{RankModeQuality: quality},
	}
}

// Admitted is what the export's non-forced union pool holds: online, or
// native in either mode. Each family counts the facets a request of that
// outcome family draws: any is dualstack and IPv4-only, v4 is IPv4-only, v6 is
// dualstack and IPv6-only. Departed country targets count zero; region, city
// and unknown targets are not countries.
func TestClientScoreSupplyCountsAdmittedProvidersPerCountryAndFamily(t *testing.T) {
	de, kr, departed, region, unknown := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	scores := func(list ...*ClientScore) map[server.Id]*ClientScore {
		m := map[server.Id]*ClientScore{}
		for _, score := range list {
			m[score.ClientId] = score
		}
		return m
	}
	dualstack := ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	locationClientScores := map[server.Id]map[server.Id]*ClientScore{
		de: scores(
			supplyTestScore(ClientScoreIpFamilyV4, true, false),
			supplyTestScore(ClientScoreIpFamilyV4, false, true),
			supplyTestScore(dualstack, true, true),
			supplyTestScore(ClientScoreIpFamilyV6, true, false),
			// not admitted: neither online nor native
			supplyTestScore(dualstack, false, false),
		),
		kr:     scores(supplyTestScore(ClientScoreIpFamilyV4, false, false)),
		region: scores(supplyTestScore(dualstack, true, true)),
	}
	countryCodes := map[server.Id]string{de: "de", kr: "kr", departed: "zz", unknown: "not-a-country"}
	supply := newClientScoreSupply(time.Unix(1_800_000_000, 0), locationClientScores, []clientScoreTarget{{id: departed}, {id: region}}, countryCodes)
	want := map[clientScoreSupplyKey]int{
		{"de", "any"}: 3, {"de", "v4"}: 2, {"de", "v6"}: 2,
		{"kr", "any"}: 0, {"kr", "v4"}: 0, {"kr", "v6"}: 0,
		{"zz", "any"}: 0, {"zz", "v4"}: 0, {"zz", "v6"}: 0,
	}
	if len(supply.countries) != len(want) {
		t.Fatalf("countries=%v, want %v", supply.countries, want)
	}
	for key, count := range want {
		if got, ok := supply.countries[key]; !ok || got != count {
			t.Fatalf("%v=%d (%t), want %d", key, got, ok, count)
		}
	}
	for family, count := range map[string]int{"any": 3, "v4": 2, "v6": 2} {
		if supply.fleet[family] != count {
			t.Fatalf("fleet %s=%d, want %d", family, supply.fleet[family], count)
		}
	}
}

// The scrape interval: the stats pusher gathers every 15 seconds
// (server.statsPushInterval).
const supplyTestScrapeInterval = 15 * time.Second

// The clock the simulated taskworkers share.
type supplyTestClock struct {
	stateLock sync.Mutex
	time      time.Time
}

func newSupplyTestClock(start time.Time) *supplyTestClock {
	return &supplyTestClock{time: start}
}

func (self *supplyTestClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.time
}

func (self *supplyTestClock) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.time = self.time.Add(duration)
}

// The publisher lease as Redis holds it, shared by simulated taskworkers on
// one clock, with the claim script's rules
// (clientScoreSupplyPublisherClaimScript), which
// TestClientScoreSupplyPublisherLeaseScriptMatchesItsModel holds Redis to.
type supplyTestLease struct {
	now func() time.Time

	stateLock sync.Mutex
	// the lease, "" when absent, and when it lapses
	value     string
	expiresAt time.Time
	// every claim fails with it, as with Redis unreachable
	err error
	// claims made, failed ones included
	claims int
}

func (self *supplyTestLease) claim(ctx context.Context, lease string, measuredAt time.Time) (string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.claims += 1
	if self.err != nil {
		return "", self.err
	}
	now := self.now()
	if self.value != "" && !now.Before(self.expiresAt) {
		self.value = ""
	}
	if self.value == lease {
		self.expiresAt = now.Add(clientScoreSupplyPublisherLeaseTtl)
		return lease, nil
	}
	if self.value != "" {
		measured, _, found := strings.Cut(self.value, ":")
		claimed := strconv.FormatInt(measuredAt.UnixMilli(), 10)
		if found && supplyTestDecimal(measured) && (len(claimed) < len(measured) || (len(measured) == len(claimed) && claimed <= measured)) {
			return self.value, nil
		}
	}
	self.value, self.expiresAt = lease, now.Add(clientScoreSupplyPublisherLeaseTtl)
	return lease, nil
}

func (self *supplyTestLease) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

func (self *supplyTestLease) claimCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.claims
}

// Digits only, at least one: the script's %d+ before the colon.
func supplyTestDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || '9' < c {
			return false
		}
	}
	return true
}

// A simulated taskworker named publisher, claiming the shared lease.
func supplyTestProcess(clock *supplyTestClock, publisher string, lease *supplyTestLease) *clientScoreSupplyCollector {
	return &clientScoreSupplyCollector{
		now:       clock.now,
		publisher: publisher,
		claim:     lease.claim,
		errorf:    func(string, ...any) {},
		infof:     func(string, ...any) {},
	}
}

// A measurement taken at measuredAt with count admitted providers in every
// family of each country.
func supplyTestMeasurement(measuredAt time.Time, count int, countries ...string) *clientScoreSupply {
	supply := &clientScoreSupply{
		measuredAt: measuredAt,
		countries:  map[clientScoreSupplyKey]int{},
		fleet:      map[string]int{},
	}
	for _, country := range countries {
		for _, family := range clientScoreSupplyFamilies {
			supply.countries[clientScoreSupplyKey{country, family}] = count
			supply.fleet[family] += count
		}
	}
	for _, family := range clientScoreSupplyFamilies {
		supply.fleet[family] += 0
	}
	return supply
}

// The series a measurement of countries countries publishes: three families
// each, the three fleet families and the measurement time.
func supplyTestSeriesOf(countries int) int {
	return 3*countries + 3 + 1
}

// What one scrape of the process publishes: its series and the measurement
// time they carry, zero without any.
func supplyTestScrape(t testing.TB, process *clientScoreSupplyCollector) (series int, measuredAt int64) {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(process)
	gathered, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range gathered {
		series += len(family.Metric)
		if family.GetName() == "urnetwork_client_score_supply_measured_timestamp_seconds" {
			measuredAt = int64(family.Metric[0].GetGauge().GetValue())
		}
	}
	return
}

// Two taskworkers each ran an export within the hour: the RunOnce score task
// lands on whichever claims it. Only the one holding the newer measurement
// publishes, whichever export finished last, so the fleet exports one copy of
// the series, the newest.
func TestClientScoreSupplyOnlyTheNewestExportPublishes(t *testing.T) {
	ctx := context.Background()
	for _, olderFinishesLast := range []bool{false, true} {
		clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
		lease := &supplyTestLease{now: clock.now}
		older := supplyTestProcess(clock, "taskworker-a", lease)
		newer := supplyTestProcess(clock, "taskworker-b", lease)
		olderMeasurement := supplyTestMeasurement(clock.now(), 4, "de", "kr")
		newerMeasurement := supplyTestMeasurement(clock.now().Add(10*time.Minute), 5, "de", "kr", "au")
		clock.advance(12 * time.Minute)
		if olderFinishesLast {
			newer.publish(ctx, newerMeasurement)
			older.publish(ctx, olderMeasurement)
		} else {
			older.publish(ctx, olderMeasurement)
			newer.publish(ctx, newerMeasurement)
		}
		clock.advance(supplyTestScrapeInterval)
		for _, process := range []*clientScoreSupplyCollector{older, newer, older} {
			series, measuredAt := supplyTestScrape(t, process)
			switch process {
			case older:
				if series != 0 {
					t.Fatalf("older finishes last %t: the process with the older measurement published %d series", olderFinishesLast, series)
				}
			case newer:
				if series != supplyTestSeriesOf(3) || measuredAt != newerMeasurement.measuredAt.Unix() {
					t.Fatalf("older finishes last %t: the newest measurement published %d series measured at %d", olderFinishesLast, series, measuredAt)
				}
			}
		}
	}
}

// The process holding the only measurement publishes it. Once another
// process's export measures again, the first stops at its next scrape, and the
// newer one publishes.
func TestClientScoreSupplyOlderPublisherStopsOnceTheNewerAppears(t *testing.T) {
	// a claim decides only the scrapes before the next one
	if supplyTestScrapeInterval <= clientScoreSupplyPublisherClaimMaxAge {
		t.Fatalf("a claim decides %s of scrapes, not less than the %s scrape interval", clientScoreSupplyPublisherClaimMaxAge, supplyTestScrapeInterval)
	}
	ctx := context.Background()
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	lease := &supplyTestLease{now: clock.now}
	older := supplyTestProcess(clock, "taskworker-a", lease)
	newer := supplyTestProcess(clock, "taskworker-b", lease)
	olderMeasurement := supplyTestMeasurement(clock.now(), 4, "de", "kr")
	older.publish(ctx, olderMeasurement)
	for range 4 {
		clock.advance(supplyTestScrapeInterval)
		if series, measuredAt := supplyTestScrape(t, older); series != supplyTestSeriesOf(2) || measuredAt != olderMeasurement.measuredAt.Unix() {
			t.Fatalf("the only measurement published %d series measured at %d", series, measuredAt)
		}
		if series, _ := supplyTestScrape(t, newer); series != 0 {
			t.Fatalf("a process without a measurement published %d series", series)
		}
	}
	clock.advance(time.Minute)
	newerMeasurement := supplyTestMeasurement(clock.now(), 5, "de", "kr", "au")
	newer.publish(ctx, newerMeasurement)
	clock.advance(supplyTestScrapeInterval)
	if series, _ := supplyTestScrape(t, older); series != 0 {
		t.Fatalf("one scrape after a newer measurement appeared, the older still published %d series", series)
	}
	if series, measuredAt := supplyTestScrape(t, newer); series != supplyTestSeriesOf(3) || measuredAt != newerMeasurement.measuredAt.Unix() {
		t.Fatalf("the newer measurement published %d series measured at %d", series, measuredAt)
	}
}

// Without Redis a claim fails, and every process with a current measurement
// publishes it, as before the lease: the probe takes the newest of them, so
// the supply never goes dark. A claim that panics fails the same way. Each
// process logs the failure once when it starts and once when it ends, and
// within a scrape of the end one process publishes again.
func TestClientScoreSupplyUnavailableLeaseKeepsTodaysBehavior(t *testing.T) {
	ctx := context.Background()
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	lease := &supplyTestLease{now: clock.now}
	lease.fail(errors.New("synthetic redis unavailable"))
	logs := map[string][]string{}
	var logsLock sync.Mutex
	process := func(publisher string) *clientScoreSupplyCollector {
		process := supplyTestProcess(clock, publisher, lease)
		process.errorf = func(format string, args ...any) {
			logsLock.Lock()
			defer logsLock.Unlock()
			logs[publisher] = append(logs[publisher], "error: "+fmt.Sprintf(format, args...))
		}
		process.infof = func(format string, args ...any) {
			logsLock.Lock()
			defer logsLock.Unlock()
			logs[publisher] = append(logs[publisher], "info: "+fmt.Sprintf(format, args...))
		}
		return process
	}
	older := process("taskworker-a")
	newer := process("taskworker-b")
	olderMeasurement := supplyTestMeasurement(clock.now(), 4, "de", "kr")
	older.publish(ctx, olderMeasurement)
	clock.advance(10 * time.Minute)
	newerMeasurement := supplyTestMeasurement(clock.now(), 5, "de", "kr", "au")
	newer.publish(ctx, newerMeasurement)
	for range 3 {
		clock.advance(supplyTestScrapeInterval)
		if series, measuredAt := supplyTestScrape(t, older); series != supplyTestSeriesOf(2) || measuredAt != olderMeasurement.measuredAt.Unix() {
			t.Fatalf("without the lease the older measurement published %d series measured at %d", series, measuredAt)
		}
		if series, measuredAt := supplyTestScrape(t, newer); series != supplyTestSeriesOf(3) || measuredAt != newerMeasurement.measuredAt.Unix() {
			t.Fatalf("without the lease the newer measurement published %d series measured at %d", series, measuredAt)
		}
	}

	panicking := supplyTestProcess(clock, "taskworker-c", lease)
	panicking.claim = func(context.Context, string, time.Time) (string, error) {
		panic("synthetic claim panic")
	}
	panicking.publish(ctx, supplyTestMeasurement(clock.now(), 6, "de"))
	if series, _ := supplyTestScrape(t, panicking); series != supplyTestSeriesOf(1) {
		t.Fatalf("a panicking claim published %d series", series)
	}

	lease.fail(nil)
	clock.advance(supplyTestScrapeInterval)
	supplyTestScrape(t, older)
	if series, _ := supplyTestScrape(t, newer); series != supplyTestSeriesOf(3) {
		t.Fatalf("once claims succeed the newest measurement published %d series", series)
	}
	clock.advance(supplyTestScrapeInterval)
	if series, _ := supplyTestScrape(t, older); series != 0 {
		t.Fatalf("a scrape after claims succeed again the older measurement still published %d series", series)
	}
	if series, _ := supplyTestScrape(t, newer); series != supplyTestSeriesOf(3) {
		t.Fatalf("a scrape after claims succeed again the newest measurement published %d series", series)
	}
	for _, publisher := range []string{"taskworker-a", "taskworker-b"} {
		got := logs[publisher]
		if len(got) != 2 || !strings.HasPrefix(got[0], "error: ") || !strings.Contains(got[0], "synthetic redis unavailable") || !strings.HasPrefix(got[1], "info: ") {
			t.Fatalf("%s logged %q, want the failure window's start and end once each", publisher, got)
		}
	}
}

// Six taskworkers each hold an export within the hour, as on Main on
// 2026-10-31, when six processes published 1,947 series for 339 country and
// family pairs. The score export lands on each in turn, nine minutes apart,
// and every process is scraped each interval: at every scrape the fleet
// publishes one process's series, the newest export's.
func TestClientScoreSupplyFleetPublishesOneCopy(t *testing.T) {
	ctx := context.Background()
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	lease := &supplyTestLease{now: clock.now}
	countries := []string{}
	for i := 0; i < 113; i++ {
		countries = append(countries, fmt.Sprintf("%c%c", 'a'+i/26, 'a'+i%26))
	}
	processes := []*clientScoreSupplyCollector{}
	for i := range 6 {
		processes = append(processes, supplyTestProcess(clock, fmt.Sprintf("taskworker-%d", i), lease))
	}
	for i, exporter := range processes {
		measurement := supplyTestMeasurement(clock.now(), i, countries...)
		exporter.publish(ctx, measurement)
		for range int(9 * time.Minute / supplyTestScrapeInterval) {
			clock.advance(supplyTestScrapeInterval)
			total, publishing := 0, 0
			for _, process := range processes {
				series, measuredAt := supplyTestScrape(t, process)
				total += series
				if 0 < series {
					publishing += 1
					if measuredAt != measurement.measuredAt.Unix() {
						t.Fatalf("export %d: the published measurement is from %d, not the newest export", i, measuredAt)
					}
				}
			}
			if publishing != 1 || total != supplyTestSeriesOf(len(countries)) {
				t.Fatalf("export %d: %d processes published %d series, want one publishing %d", i, publishing, total, supplyTestSeriesOf(len(countries)))
			}
		}
	}
}

// The holder stops: drained, crashed or cut off from Redis. Its lease lapses
// within a minute, while Mimir still reads its last series, and the newest
// measurement left takes over within a scrape; until then no other process
// publishes. A holder that comes back with a newer measurement takes the
// lease back.
func TestClientScoreSupplyNewestLeftTakesOverWhenTheHolderStops(t *testing.T) {
	ctx := context.Background()
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	lease := &supplyTestLease{now: clock.now}
	oldest := supplyTestProcess(clock, "taskworker-a", lease)
	middle := supplyTestProcess(clock, "taskworker-b", lease)
	holder := supplyTestProcess(clock, "taskworker-c", lease)
	// one scrape of each process an interval from now, in order, and the
	// series each published
	scrape := func(processes ...*clientScoreSupplyCollector) map[*clientScoreSupplyCollector]int {
		clock.advance(supplyTestScrapeInterval)
		published := map[*clientScoreSupplyCollector]int{}
		for _, process := range processes {
			published[process], _ = supplyTestScrape(t, process)
		}
		return published
	}
	oldest.publish(ctx, supplyTestMeasurement(clock.now(), 4, "de"))
	for range 20 {
		scrape(oldest, middle, holder)
	}
	middleMeasurement := supplyTestMeasurement(clock.now(), 5, "de")
	middle.publish(ctx, middleMeasurement)
	for range 20 {
		scrape(oldest, middle, holder)
	}
	holder.publish(ctx, supplyTestMeasurement(clock.now(), 6, "de"))
	for range 4 {
		if published := scrape(oldest, middle, holder); published[oldest] != 0 || published[middle] != 0 || published[holder] != supplyTestSeriesOf(1) {
			t.Fatalf("with the holder scraped, the processes published %d, %d and %d series", published[oldest], published[middle], published[holder])
		}
	}
	holderStopped := clock.now()

	takeover := time.Duration(0)
	for takeover == 0 {
		published := scrape(oldest, middle)
		elapsed := clock.now().Sub(holderStopped)
		if 0 < published[middle] {
			takeover = elapsed
		} else if 0 < published[oldest] && elapsed < clientScoreSupplyPublisherLeaseTtl {
			t.Fatalf("%s after the holder stopped, before its lease lapsed, the oldest measurement published", elapsed)
		}
		if 5*time.Minute <= elapsed {
			t.Fatalf("no process took over in %s", elapsed)
		}
	}
	if takeover < clientScoreSupplyPublisherLeaseTtl || clientScoreSupplyPublisherLeaseTtl+supplyTestScrapeInterval < takeover {
		t.Fatalf("the newest measurement left took over %s after the holder stopped, want within a scrape of the %s lease", takeover, clientScoreSupplyPublisherLeaseTtl)
	}
	if published := scrape(oldest, middle); published[oldest] != 0 || published[middle] != supplyTestSeriesOf(1) {
		t.Fatalf("a scrape after the takeover the oldest and the newest left published %d and %d series", published[oldest], published[middle])
	}
	if _, measuredAt := supplyTestScrape(t, middle); measuredAt != middleMeasurement.measuredAt.Unix() {
		t.Fatalf("the takeover published the measurement from %d", measuredAt)
	}

	if published := scrape(holder); published[holder] != supplyTestSeriesOf(1) {
		t.Fatalf("the holder back with the newest measurement published %d series", published[holder])
	}
	if published := scrape(oldest, middle, holder); published[oldest] != 0 || published[middle] != 0 || published[holder] != supplyTestSeriesOf(1) {
		t.Fatalf("a scrape after the holder came back the processes published %d, %d and %d series", published[oldest], published[middle], published[holder])
	}
}

// A claim is a Redis call: scrapes within a claim's window share it, and a
// process without a current measurement, never exported or more than an hour
// past its export, claims nothing.
func TestClientScoreSupplyClaimsAreBounded(t *testing.T) {
	ctx := context.Background()
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	lease := &supplyTestLease{now: clock.now}
	process := supplyTestProcess(clock, "taskworker-a", lease)
	for range 5 {
		supplyTestScrape(t, process)
	}
	if claims := lease.claimCount(); claims != 0 {
		t.Fatalf("a process that never exported claimed %d times", claims)
	}
	process.publish(ctx, supplyTestMeasurement(clock.now(), 4, "de"))
	for range 10 {
		supplyTestScrape(t, process)
	}
	if claims := lease.claimCount(); claims != 1 {
		t.Fatalf("an export and ten scrapes within one window claimed %d times, want once", claims)
	}
	clock.advance(clientScoreSupplyPublisherClaimMaxAge)
	supplyTestScrape(t, process)
	if claims := lease.claimCount(); claims != 2 {
		t.Fatalf("a scrape past the window claimed %d times in all, want twice", claims)
	}
	clock.advance(clientScoreSupplyMaxAge)
	for range 5 {
		if series, _ := supplyTestScrape(t, process); series != 0 {
			t.Fatalf("a measurement past its age published %d series", series)
		}
	}
	if claims := lease.claimCount(); claims != 2 {
		t.Fatalf("a measurement past its age claimed, %d claims in all", claims)
	}
}

// One series per country and family, the fleet families and the measurement
// time, whatever the export holds: about 250 countries at most, a code the
// directory cannot vouch for left out, and nothing at all once the measurement
// is older than an hour or before the first export.
func TestClientScoreSupplyCollectorCardinalityIsBounded(t *testing.T) {
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	now := clock.now()
	collector := supplyTestProcess(clock, "taskworker-a", &supplyTestLease{now: clock.now})
	if count := testutil.CollectAndCount(collector); count != 0 {
		t.Fatalf("a process that never exported published %d series", count)
	}
	countries := map[clientScoreSupplyKey]int{}
	for i := 0; i < 26*26; i++ {
		code := fmt.Sprintf("%c%c", 'a'+i/26, 'a'+i%26)
		for _, family := range clientScoreSupplyFamilies {
			countries[clientScoreSupplyKey{code, family}] = i
		}
	}
	countries[clientScoreSupplyKey{"not-a-country", "any"}] = 1
	countries[clientScoreSupplyKey{"DE", "any"}] = 1
	countries[clientScoreSupplyKey{"de", "dualstack"}] = 1
	collector.publish(context.Background(), &clientScoreSupply{measuredAt: now, countries: countries, fleet: map[string]int{"any": 9, "v4": 8, "v6": 7, "dualstack": 1}})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	gathered, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	series := 0
	for _, family := range gathered {
		for _, metric := range family.Metric {
			series++
			labels := map[string]string{}
			for _, pair := range metric.Label {
				labels[pair.GetName()] = pair.GetValue()
			}
			switch family.GetName() {
			case "urnetwork_client_score_admitted_providers":
				country := labels["country"]
				if len(labels) != 2 || len(country) != 2 || country != strings.ToLower(country) || !supplyTestFamily(labels["ip_family"]) {
					t.Fatalf("admitted series labels %v", labels)
				}
			case "urnetwork_client_score_admitted_providers_fleet":
				if len(labels) != 1 || !supplyTestFamily(labels["ip_family"]) {
					t.Fatalf("fleet series labels %v", labels)
				}
			case "urnetwork_client_score_supply_measured_timestamp_seconds":
				if len(labels) != 0 || metric.GetGauge().GetValue() != float64(now.Unix()) {
					t.Fatalf("measured series %v", metric)
				}
			default:
				t.Fatalf("unexpected family %s", family.GetName())
			}
			if metric.GetGauge() == nil {
				t.Fatalf("%s is not a gauge", family.GetName())
			}
		}
	}
	if want := 26*26*3 + 3 + 1; series != want {
		t.Fatalf("series=%d, want %d", series, want)
	}
	clock.advance(clientScoreSupplyMaxAge + time.Second)
	if count := testutil.CollectAndCount(collector); count != 0 {
		t.Fatalf("a measurement past its age still published %d series", count)
	}
}

func supplyTestFamily(family string) bool {
	return family == "any" || family == "v4" || family == "v6"
}

// A supply-empty answer names its requested country, a region's country for a
// region; every other answer leaves target_country empty, so the counter's
// added cardinality is the countries actually answered empty from supply.
func TestRecordFindProviders2EmptyOutcomeNamesTheSupplyEmptyTargetCountry(t *testing.T) {
	locationID := server.NewId()
	args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationID}}}
	for _, test := range []struct {
		targetKind, targetCountry, reason, want string
	}{
		{"country", "kr", "cache_empty", "kr"},
		{"region", "au", "cache_empty", "au"},
		{"city", "au", "cache_empty", ""},
		{"country", "kr", "cache_missing", ""},
		{"country", "kr", "filtered_destinations", ""},
		{"country", "", "cache_empty", ""},
	} {
		counter, err := findProviders2EmptyOutcomesTotal.GetMetricWithLabelValues("any", "location", "us", RankModeQuality, "false", "none", test.targetKind, test.want, test.reason)
		if err != nil {
			t.Fatal(err)
		}
		before := testutil.ToFloat64(counter)
		recordFindProviders2EmptyOutcome(args, "US", test.targetKind, test.targetCountry, test.reason)
		if got := testutil.ToFloat64(counter); got != before+1 {
			t.Fatalf("%+v: counter=%v, want %v", test, got, before+1)
		}
	}
}

// The requested country resolves through the directory, or the country list
// for a legacy entry; specs naming two countries, a group or the best
// available exits have no single target country.
func TestFindProviders2TargetCountryIsSingleAndResolved(t *testing.T) {
	kr, seoul, au, legacy := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	group := server.NewId()
	directory := map[server.Id]*locationDirectoryEntry{
		kr:    {CountryCode: "KR", LocationType: LocationTypeCountry},
		seoul: {CountryCode: "kr", LocationType: LocationTypeCity},
		au:    {CountryCode: "au", LocationType: LocationTypeCountry},
	}
	countries := map[string]server.Id{"jp": legacy}
	for _, test := range []struct {
		specs []*ProviderSpec
		want  string
	}{
		{[]*ProviderSpec{{LocationId: &kr}}, "kr"},
		{[]*ProviderSpec{{LocationId: &kr}, {LocationId: &seoul}}, "kr"},
		{[]*ProviderSpec{{LocationId: &legacy}}, "jp"},
		{[]*ProviderSpec{{LocationId: &kr}, {LocationId: &au}}, ""},
		{[]*ProviderSpec{{LocationId: &kr}, {LocationGroupId: &group}}, ""},
		{[]*ProviderSpec{{LocationId: &kr}, {BestAvailable: true}}, ""},
		{[]*ProviderSpec{{LocationGroupId: &group}}, ""},
		{nil, ""},
	} {
		if got := findProviders2TargetCountry(&FindProviders2Args{Specs: test.specs}, countries, directory); got != test.want {
			t.Fatalf("specs %d: target country %q, want %q", len(test.specs), got, test.want)
		}
	}
}
