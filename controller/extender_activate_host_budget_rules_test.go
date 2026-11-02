package controller

import (
	"bytes"
	"fmt"
	mathrand "math/rand/v2"
	"net/netip"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The rules of the per-host activation budget (connect#230), without a
// database: the numbers, the account's limit, what counts as a proven host,
// the key of an address's budget, and a fleet's cold start under them.

// The numbers the owner settled on 2026-11-01, kept together in the settings:
// 6 an hour per family per address, the account's floor of 60 plus 6 per proven
// host up to 1200, a proven host passed between an hour and 7 days ago, 32
// counted per /24 or /48.
func TestExtenderActivateSettingsHoldThePerHostNumbers(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	connect.AssertEqual(t, settings.ClientRateLimit, 6)
	connect.AssertEqual(t, settings.AddressRateLimit, 6)
	connect.AssertEqual(t, settings.AddressIpv4PrefixBitCount, 32)
	connect.AssertEqual(t, settings.AddressIpv6PrefixBitCount, 64)
	connect.AssertEqual(t, settings.AccountRateLimit, 60)
	connect.AssertEqual(t, settings.AccountRateLimitPerProvenHost, 6)
	connect.AssertEqual(t, settings.AccountRateLimitCeiling, 1200)
	connect.AssertEqual(t, settings.RateLimitWindow, time.Hour)
	connect.AssertEqual(t, settings.ProvenHostMinAge, time.Hour)
	connect.AssertEqual(t, settings.ProvenHostMaxAge, 7*24*time.Hour)
	connect.AssertEqual(t, settings.ProvenHostsPerBlock, 32)
	connect.AssertEqual(t, settings.ProvenHostBlockIpv4PrefixBitCount, 24)
	connect.AssertEqual(t, settings.ProvenHostBlockIpv6PrefixBitCount, 48)
	connect.AssertEqual(t, settings.ProvenHostCountTimeout, time.Minute)
}

// The account's limit is min(60 + 6 × proven hosts, 1200) per family an hour:
// the floor with none, 6 more per proven host, and the ceiling from 190 on.
func TestExtenderActivateAccountLimitGrowsWithProvenHosts(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	cases := []struct {
		provenHostCount int
		want            int
	}{
		{provenHostCount: -1, want: 60},
		{provenHostCount: 0, want: 60},
		{provenHostCount: 1, want: 66},
		{provenHostCount: 10, want: 120},
		{provenHostCount: 60, want: 420},
		{provenHostCount: 189, want: 1194},
		{provenHostCount: 190, want: 1200},
		{provenHostCount: 191, want: 1200},
		{provenHostCount: 500, want: 1200},
	}
	for _, c := range cases {
		if got := settings.accountRateLimit(c.provenHostCount); got != c.want {
			t.Errorf("%d proven hosts give a limit of %d, want %d", c.provenHostCount, got, c.want)
		}
	}
	// a ceiling set below the floor never takes the account below it
	settings.AccountRateLimitCeiling = 10
	connect.AssertEqual(t, settings.accountRateLimit(100), 60)
}

// What counts as a proven host (connect#230): a host of the family whose first
// activation in the last 7 days is at least an hour old, once per address
// (/32, /64), and at most 32 per /24 or /48.
func TestExtenderActivateProvenHostCountRules(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	candidate := func(ip string, age time.Duration) *model.NetworkExtenderProvenHostCandidate {
		return &model.NetworkExtenderProvenHostCandidate{
			ExtenderId:        server.NewId(),
			Ip:                netip.MustParseAddr(ip),
			FirstActivateTime: now.Add(-age),
		}
	}
	block := func(prefix string, count int, age time.Duration) []*model.NetworkExtenderProvenHostCandidate {
		candidates := []*model.NetworkExtenderProvenHostCandidate{}
		for i := range count {
			candidates = append(candidates, candidate(fmt.Sprintf(prefix, i+1), age))
		}
		return candidates
	}
	cases := []struct {
		name       string
		ipVersion  int
		candidates []*model.NetworkExtenderProvenHostCandidate
		want       int
	}{
		{name: "none", ipVersion: 4, want: 0},
		{name: "passed 59 minutes ago", ipVersion: 4, candidates: block("198.51.100.%d", 1, 59*time.Minute), want: 0},
		{name: "passed an hour ago", ipVersion: 4, candidates: block("198.51.100.%d", 1, time.Hour), want: 1},
		{name: "passed 7 days ago", ipVersion: 4, candidates: block("198.51.100.%d", 1, 7*24*time.Hour), want: 1},
		{name: "passed 7 days and a minute ago", ipVersion: 4, candidates: block("198.51.100.%d", 1, 7*24*time.Hour+time.Minute), want: 0},
		{name: "an ipv6 host counted for ipv4", ipVersion: 4, candidates: block("2001:db8:%x::1", 1, 2*time.Hour), want: 0},
		{name: "an ipv4 host counted for ipv6", ipVersion: 6, candidates: block("198.51.100.%d", 1, 2*time.Hour), want: 0},
		{name: "an ipv4-mapped host counted for ipv4", ipVersion: 4, candidates: block("::ffff:198.51.100.%d", 1, 2*time.Hour), want: 1},
		{
			name:      "two keys on one ipv4 address",
			ipVersion: 4,
			candidates: []*model.NetworkExtenderProvenHostCandidate{
				candidate("198.51.100.7", 2*time.Hour),
				candidate("198.51.100.7", 3*time.Hour),
			},
			want: 1,
		},
		{
			name:      "two keys in one ipv6 /64",
			ipVersion: 6,
			candidates: []*model.NetworkExtenderProvenHostCandidate{
				candidate("2001:db8:1:2::1", 2*time.Hour),
				candidate("2001:db8:1:2:ffff::9", 2*time.Hour),
			},
			want: 1,
		},
		{
			name:      "two /64s in one /48",
			ipVersion: 6,
			candidates: []*model.NetworkExtenderProvenHostCandidate{
				candidate("2001:db8:1:2::1", 2*time.Hour),
				candidate("2001:db8:1:3::1", 2*time.Hour),
			},
			want: 2,
		},
		{name: "40 addresses in one /24", ipVersion: 4, candidates: block("198.51.100.%d", 40, 2*time.Hour), want: 32},
		{
			name:       "40 addresses in one /24 and 5 in another",
			ipVersion:  4,
			candidates: append(block("198.51.100.%d", 40, 2*time.Hour), block("203.0.113.%d", 5, 2*time.Hour)...),
			want:       37,
		},
		{name: "40 /64s in one /48", ipVersion: 6, candidates: block("2001:db8:1:%x::1", 40, 2*time.Hour), want: 32},
		{name: "40 /48s", ipVersion: 6, candidates: block("2001:db8:%x::1", 40, 2*time.Hour), want: 40},
	}
	for _, c := range cases {
		if got := settings.provenHostCount(c.candidates, c.ipVersion, now); got != c.want {
			t.Errorf("%s: %d proven hosts, want %d", c.name, got, c.want)
		}
	}
}

// The address budget of a caller is kept under the peppered hash of its /32
// or /64 (connect#230): one id per address, the same for every client and
// account, and no address in it. An ipv4-mapped caller is its ipv4 address,
// and a zone names no other address.
func TestExtenderActivateAddressBudgetIdIsTheAddressHash(t *testing.T) {
	// the pepper the hash is keyed with, synthetic and test only
	t.Cleanup(server.Vault.PushSimpleResource(
		"client.yml",
		[]byte("client_ip_hash_pepper: synthetic-extender-activation-budget-test-only\n"),
	))
	settings := DefaultExtenderActivateSettings()
	id := func(ip string) server.Id {
		return settings.addressBudgetId(netip.MustParseAddr(ip))
	}
	same := [][2]string{
		{"198.51.100.7", "198.51.100.7"},
		{"198.51.100.7", "::ffff:198.51.100.7"},
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:ffff"},
		{"2001:db8:1:2::1", "2001:db8:1:2::1%eth0"},
	}
	for _, pair := range same {
		if id(pair[0]) != id(pair[1]) {
			t.Errorf("%s and %s have different address budgets", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"198.51.100.7", "198.51.100.8"},
		{"2001:db8:1:2::1", "2001:db8:1:3::1"},
		{"198.51.100.7", "2001:db8:1:2::1"},
	}
	for _, pair := range different {
		if id(pair[0]) == id(pair[1]) {
			t.Errorf("%s and %s share an address budget", pair[0], pair[1])
		}
	}
	// no byte run of the address, or of its /64, is in the id
	for _, ip := range []string{"198.51.100.7", "2001:db8:1:2::1"} {
		addr := netip.MustParseAddr(ip)
		addressBytes := addr.AsSlice()
		if addr.Is6() {
			addressBytes = addressBytes[:8]
		}
		budgetId := id(ip)
		if bytes.Contains(budgetId[:], addressBytes) {
			t.Errorf("the address budget id %s holds the address %s", budgetId, ip)
		}
	}
}

// One fleet's cold start in the simulation below.
type testActivationColdStart struct {
	name      string
	hostCount int
	// the server's limits: the per-host defaults, or today's fixed account
	// limit of 60
	settings *ExtenderActivateSettings
	// hosts that passed both families two hours before the start
	provenHostCount int
	// hosts whose probes always fail
	failingHostCount int
	// the activator as built, which keeps each family's schedule and waits a
	// refusal's retry_after_seconds; else the one before, which posts both
	// families every pass and backs off 10 minutes doubling to 6 hours
	perFamilyClient bool
	horizon         time.Duration
}

// The settings of the server before connect#230: the account's limit fixed at
// 60 an hour per family, and no address budget.
func testTodayExtenderActivateSettings() *ExtenderActivateSettings {
	settings := DefaultExtenderActivateSettings()
	settings.AddressRateLimit = 1 << 30
	settings.AccountRateLimitPerProvenHost = 0
	settings.AccountRateLimitCeiling = settings.AccountRateLimit
	return settings
}

// One budget read over attempt times kept in memory, as the database read
// keeps them (model.CheckAndRecordAccountActionBudgets): the attempts at or
// after the window's start, the limit raised past the floor by `extendLimit`,
// and when there is no room, the wait until the attempt whose leaving makes
// room leaves (model.AccountActionBudgetRetryAfter). The attempts are in time
// order, and the ones before the window are dropped.
func testColdStartBudgetRead(
	attemptTimes *[]time.Time,
	limit int,
	extendLimit func() int,
	window time.Duration,
	now time.Time,
) (allow bool, retryAfter time.Duration) {
	cutoff := now.Add(-window)
	first := sort.Search(len(*attemptTimes), func(i int) bool {
		return !(*attemptTimes)[i].Before(cutoff)
	})
	*attemptTimes = (*attemptTimes)[first:]
	count := len(*attemptTimes)
	if limit <= count && extendLimit != nil {
		limit = max(limit, extendLimit())
	}
	if count < limit {
		return true, 0
	}
	boundaryTime := (*attemptTimes)[count-max(limit, 0)]
	return false, model.AccountActionBudgetRetryAfter(&boundaryTime, window, now)
}

// The cold-start simulation of the proposal (activation_coldstart_sim.py,
// connect#230) over the real account limit, the real proven host count and
// the real retry-after rule, on a fake clock of one-minute ticks. One account,
// its hosts dual-stack with a client and an address each, all starting at
// minute 0, in an order shuffled every minute by a fixed seed. Each post
// spends the client's, the address's and the account's budget of its family,
// and a refusal spends nothing. Returns the minute every passing host has
// both families active, -1 past the horizon, and the probes spent.
//
// Both families are simulated with ipv6 documentation addresses, each host in
// a /48 of its own, so the per-block rule does not bind, as in the proposal's
// simulation.
func runTestActivationColdStart(scenario testActivationColdStart) (doneMinute int, probeCount int) {
	settings := scenario.settings
	window := settings.RateLimitWindow
	startTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	random := mathrand.New(mathrand.NewPCG(7, 7))
	ipVersions := []int{4, 6}

	type hostFamily struct {
		ip                 netip.Addr
		activated          bool
		firstActivateTime  *time.Time
		nextActivateTime   time.Time
		backoff            time.Duration
		clientAttemptTimes []time.Time
		// one address per host, so the host's address budget is its own
		addressAttemptTimes []time.Time
	}
	type host struct {
		failing           bool
		ipVersionFamilies map[int]*hostFamily
		// the schedule of the client before connect#230, one for both families
		nextActivateTime time.Time
		backoff          time.Duration
	}
	hosts := []*host{}
	for i := range scenario.hostCount {
		h := &host{
			failing:           i < scenario.failingHostCount,
			ipVersionFamilies: map[int]*hostFamily{},
			backoff:           10 * time.Minute,
		}
		for _, ipVersion := range ipVersions {
			family := &hostFamily{
				ip:      netip.MustParseAddr(fmt.Sprintf("2001:db8:%x::%x", i+1, ipVersion)),
				backoff: 10 * time.Minute,
			}
			if i < scenario.provenHostCount {
				provenTime := startTime.Add(-2 * time.Hour)
				family.firstActivateTime = &provenTime
			}
			h.ipVersionFamilies[ipVersion] = family
		}
		hosts = append(hosts, h)
	}
	ipVersionAccountAttemptTimes := map[int]*[]time.Time{
		4: &[]time.Time{},
		6: &[]time.Time{},
	}

	// the account's proven hosts of each family, counted once a minute when a
	// spend reaches the floor, as the process reuses a count for a minute
	// (ProvenHostCountTimeout). A host that passes within the minute is not
	// proven for another hour, so the count is exact.
	ipVersionProvenHostCounts := map[int]int{}
	ipVersionProvenHostCountTimes := map[int]time.Time{}
	provenHostCount := func(ipVersion int, now time.Time) int {
		if countTime, ok := ipVersionProvenHostCountTimes[ipVersion]; ok && countTime.Equal(now) {
			return ipVersionProvenHostCounts[ipVersion]
		}
		candidates := []*model.NetworkExtenderProvenHostCandidate{}
		for _, h := range hosts {
			family := h.ipVersionFamilies[ipVersion]
			if family.firstActivateTime != nil {
				candidates = append(candidates, &model.NetworkExtenderProvenHostCandidate{
					Ip:                family.ip,
					FirstActivateTime: *family.firstActivateTime,
				})
			}
		}
		ipVersionProvenHostCounts[ipVersion] = settings.provenHostCount(candidates, 6, now)
		ipVersionProvenHostCountTimes[ipVersion] = now
		return ipVersionProvenHostCounts[ipVersion]
	}

	// one post of one family: whether it passed, and the wait its refusal asked
	// for when a budget refused it
	post := func(h *host, ipVersion int, now time.Time) (passed bool, retryAfter time.Duration) {
		family := h.ipVersionFamilies[ipVersion]
		// a limit that cannot grow, as today's, reads no proven hosts
		var accountExtendLimit func() int
		if 0 < settings.AccountRateLimitPerProvenHost {
			accountExtendLimit = func() int {
				return settings.accountRateLimit(provenHostCount(ipVersion, now))
			}
		}
		budgets := []struct {
			attemptTimes *[]time.Time
			limit        int
			extendLimit  func() int
		}{
			{attemptTimes: &family.clientAttemptTimes, limit: settings.ClientRateLimit},
			{attemptTimes: &family.addressAttemptTimes, limit: settings.AddressRateLimit},
			{attemptTimes: ipVersionAccountAttemptTimes[ipVersion], limit: settings.AccountRateLimit, extendLimit: accountExtendLimit},
		}
		for _, budget := range budgets {
			if allow, retryAfter := testColdStartBudgetRead(budget.attemptTimes, budget.limit, budget.extendLimit, window, now); !allow {
				return false, retryAfter
			}
		}
		for _, budget := range budgets {
			*budget.attemptTimes = append(*budget.attemptTimes, now)
		}
		probeCount += 1
		if h.failing {
			return false, 0
		}
		family.activated = true
		if family.firstActivateTime == nil {
			activateTime := now
			family.firstActivateTime = &activateTime
		}
		return true, 0
	}
	// the hold of a refusal's wait, as the activator bounds it
	// (connect/net_extender_activate.go): at least a minute, at most 6 hours
	retryAfterHold := func(retryAfter time.Duration) time.Duration {
		return min(max(retryAfter, time.Minute), 6*time.Hour)
	}

	for minute := 0; time.Duration(minute)*time.Minute <= scenario.horizon; minute += 1 {
		now := startTime.Add(time.Duration(minute) * time.Minute)
		for _, i := range random.Perm(len(hosts)) {
			h := hosts[i]
			if scenario.perFamilyClient {
				for _, ipVersion := range ipVersions {
					family := h.ipVersionFamilies[ipVersion]
					if family.activated || now.Before(family.nextActivateTime) {
						continue
					}
					passed, retryAfter := post(h, ipVersion, now)
					switch {
					case passed:
					case 0 < retryAfter:
						family.nextActivateTime = now.Add(retryAfterHold(retryAfter))
					default:
						family.nextActivateTime = now.Add(family.backoff)
						family.backoff = min(2*family.backoff, 6*time.Hour)
					}
				}
			} else {
				if (h.ipVersionFamilies[4].activated && h.ipVersionFamilies[6].activated) || now.Before(h.nextActivateTime) {
					continue
				}
				// a pass posts every family, and holds both when either fails
				passed := true
				for _, ipVersion := range ipVersions {
					if familyPassed, _ := post(h, ipVersion, now); !familyPassed {
						passed = false
					}
				}
				if !passed {
					h.nextActivateTime = now.Add(h.backoff)
					h.backoff = min(2*h.backoff, 6*time.Hour)
				}
			}
		}
		done := true
		for _, h := range hosts {
			if !h.failing && !(h.ipVersionFamilies[4].activated && h.ipVersionFamilies[6].activated) {
				done = false
				break
			}
		}
		if done {
			return minute, probeCount
		}
	}
	return -1, probeCount
}

// A fleet's cold start under the activation budgets as built (connect#230): the
// proposal's simulation, run over the real limit, proven host count and
// retry-after rule. With the per-host budget and the activator as built, a
// fleet that has passed activation restarts in the minute it starts, and 1,000
// new hosts all activate within 160 minutes. Before, at the fixed limit of 60
// an hour with the old activator, 300 hosts took more than ten hours and
// 1,000 more than three days. The table of every scenario is logged.
func TestExtenderActivateColdStartSimulation(t *testing.T) {
	built := DefaultExtenderActivateSettings()
	today := testTodayExtenderActivateSettings()
	scenarios := []testActivationColdStart{}
	for _, hostCount := range []int{100, 300, 1000} {
		scenarios = append(
			scenarios,
			testActivationColdStart{name: "today's limit, old activator", hostCount: hostCount, settings: today, horizon: 120 * time.Hour},
			testActivationColdStart{name: "today's limit, activator as built", hostCount: hostCount, settings: today, perFamilyClient: true, horizon: 120 * time.Hour},
			testActivationColdStart{name: "per-host budget, old activator, new fleet", hostCount: hostCount, settings: built, horizon: 72 * time.Hour},
			testActivationColdStart{name: "per-host budget, activator as built, new fleet", hostCount: hostCount, settings: built, perFamilyClient: true, horizon: 72 * time.Hour},
			testActivationColdStart{name: "per-host budget, activator as built, new fleet, 10% failing", hostCount: hostCount, settings: built, failingHostCount: hostCount / 10, perFamilyClient: true, horizon: 72 * time.Hour},
			testActivationColdStart{name: "per-host budget, old activator, proven fleet restart", hostCount: hostCount, settings: built, provenHostCount: hostCount, horizon: 72 * time.Hour},
			testActivationColdStart{name: "per-host budget, activator as built, proven fleet restart", hostCount: hostCount, settings: built, provenHostCount: hostCount, perFamilyClient: true, horizon: 72 * time.Hour},
		)
	}

	table := []string{}
	doneMinutes := map[string]int{}
	for _, scenario := range scenarios {
		doneMinute, probeCount := runTestActivationColdStart(scenario)
		key := fmt.Sprintf("%s, %d hosts", scenario.name, scenario.hostCount)
		doneMinutes[key] = doneMinute
		done := "not done"
		if 0 <= doneMinute {
			done = fmt.Sprintf("%d min", doneMinute)
		}
		table = append(table, fmt.Sprintf("%-62s %5d hosts: %-9s %6d probes", scenario.name, scenario.hostCount, done, probeCount))
	}
	t.Logf("the minutes until every passing host has both families active:\n%s", strings.Join(table, "\n"))

	for _, hostCount := range []int{100, 300, 1000} {
		for _, name := range []string{
			"per-host budget, old activator, proven fleet restart",
			"per-host budget, activator as built, proven fleet restart",
		} {
			if doneMinute := doneMinutes[fmt.Sprintf("%s, %d hosts", name, hostCount)]; doneMinute != 0 {
				t.Errorf("%s, %d hosts: done at minute %d, want at once", name, hostCount, doneMinute)
			}
		}
	}
	if doneMinute := doneMinutes["per-host budget, activator as built, new fleet, 1000 hosts"]; doneMinute < 0 || 160 < doneMinute {
		t.Errorf("1,000 new hosts were done at minute %d, want within 160", doneMinute)
	}
	if doneMinute := doneMinutes["per-host budget, activator as built, new fleet, 300 hosts"]; doneMinute < 0 || 75 < doneMinute {
		t.Errorf("300 new hosts were done at minute %d, want within 75", doneMinute)
	}
	// the simulation reproduces what the proposal measured of today
	if doneMinute := doneMinutes["today's limit, old activator, 300 hosts"]; doneMinute < 600 {
		t.Errorf("300 hosts at today's limit were done at minute %d, want past ten hours", doneMinute)
	}
	if doneMinute := doneMinutes["today's limit, old activator, 1000 hosts"]; 0 <= doneMinute && doneMinute < 3*24*60 {
		t.Errorf("1,000 hosts at today's limit were done at minute %d, want past three days", doneMinute)
	}
}
