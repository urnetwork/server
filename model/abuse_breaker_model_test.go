package model

import (
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/smtpedgeservice"
)

// §21.5's breaker machine (abuse_breaker_model.go): the table on a fake
// clock. Pure: no store. The rows and the job run in taskworker/work's test.
// Addresses are RFC 5737's.

// §21.5's thresholds as pro.yml gives them: 0.10%, 0.20%, 0.30%, 0.08%, 3 days.
func testMailBreakerSettings() *MailBreakerSettings {
	settings := DefaultMailBreakerSettings()
	settings.RestrictRate = 1000
	settings.CommunityRate = 2000
	settings.AllRate = 3000
	settings.ClearRate = 800
	settings.ClearDays = 3
	return settings
}

var (
	testBreakerStart    = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	testBreakerAddress  = netip.MustParseAddr("192.0.2.10")
	testBreakerAddress2 = netip.MustParseAddr("192.0.2.11")
)

// A sample with every measure known: the Postmaster rate of the latest day
// and the day before, the complaint rate, and what is listed.
func testBreakerSample(postmaster int64, postmasterBefore int64, complaint int64, listed ...netip.Addr) *MailBreakerMeasures {
	return &MailBreakerMeasures{
		PostmasterRates: []MailRate{KnownMailRate(postmaster), KnownMailRate(postmasterBefore)},
		ComplaintRate:   KnownMailRate(complaint),
		ListingKnown:    true,
		ListedAddresses: listed,
	}
}

// Each trigger with its near miss.
func TestNextMailBreakerStateTriggers(t *testing.T) {
	settings := testMailBreakerSettings()
	cases := []struct {
		name     string
		measures *MailBreakerMeasures
		want     MailBreakerLevel
	}{
		{"nothing over", testBreakerSample(0, 0, 0), MailBreakerLevelNone},
		{"complaints one under 0.10%", testBreakerSample(0, 0, 999), MailBreakerLevelNone},
		{"complaints at 0.10%", testBreakerSample(0, 0, 1000), MailBreakerLevelRestrict},
		{"Postmaster at 0.10% two days running", testBreakerSample(1000, 1000, 0), MailBreakerLevelRestrict},
		{"Postmaster at 0.10% for one day", testBreakerSample(1000, 999, 0), MailBreakerLevelNone},
		{"Postmaster at 0.10% the day before only", testBreakerSample(999, 1000, 0), MailBreakerLevelNone},
		{"Postmaster at 0.19% for one day", testBreakerSample(1999, 0, 0), MailBreakerLevelNone},
		{"complaints one under 0.20%", testBreakerSample(0, 0, 1999), MailBreakerLevelRestrict},
		{"complaints at 0.20%", testBreakerSample(0, 0, 2000), MailBreakerLevelCommunity},
		{"Postmaster at 0.20% for one day", testBreakerSample(2000, 0, 0), MailBreakerLevelCommunity},
		{"a listed address", testBreakerSample(0, 0, 0, testBreakerAddress), MailBreakerLevelCommunity},
		{"complaints one under 0.30%", testBreakerSample(0, 0, 2999), MailBreakerLevelCommunity},
		{"complaints at 0.30%", testBreakerSample(0, 0, 3000), MailBreakerLevelAll},
		{"Postmaster at 0.30% for one day", testBreakerSample(3000, 0, 0), MailBreakerLevelAll},
		{"Postmaster one under 0.30%", testBreakerSample(2999, 0, 0), MailBreakerLevelCommunity},
	}
	for _, c := range cases {
		next := NextMailBreakerState(MailBreakerState{}, c.measures, testBreakerStart, settings)
		if next.Level != c.want {
			t.Errorf("%s: level %d, want %d", c.name, next.Level, c.want)
		}
	}
}

// Two days running is the latest two reported days: one reported day cannot
// make a run, and an unknown day in it breaks it.
func TestNextMailBreakerStateTwoDaysRunning(t *testing.T) {
	settings := testMailBreakerSettings()
	sample := func(rates ...MailRate) *MailBreakerMeasures {
		return &MailBreakerMeasures{PostmasterRates: rates, ComplaintRate: KnownMailRate(0), ListingKnown: true}
	}
	cases := []struct {
		name     string
		measures *MailBreakerMeasures
		want     MailBreakerLevel
	}{
		{"two days at the rate", sample(KnownMailRate(1500), KnownMailRate(1200)), MailBreakerLevelRestrict},
		{"one reported day", sample(KnownMailRate(1500)), MailBreakerLevelNone},
		{"the day before unknown", sample(KnownMailRate(1500), MailRate{}), MailBreakerLevelNone},
		{"the latest day unknown", sample(MailRate{}, KnownMailRate(1500)), MailBreakerLevelNone},
		{"three days, the oldest under", sample(KnownMailRate(1500), KnownMailRate(1200), KnownMailRate(0)), MailBreakerLevelRestrict},
		{"no reported day", sample(), MailBreakerLevelNone},
	}
	for _, c := range cases {
		if next := NextMailBreakerState(MailBreakerState{}, c.measures, testBreakerStart, settings); next.Level != c.want {
			t.Errorf("%s: level %d, want %d", c.name, next.Level, c.want)
		}
	}
	// a run of three, when the settings ask for it
	settings.PostmasterRunDays = 3
	if next := NextMailBreakerState(MailBreakerState{}, sample(KnownMailRate(1500), KnownMailRate(1200)), testBreakerStart, settings); next.Level != MailBreakerLevelNone {
		t.Errorf("two days against a run of three: level %d", next.Level)
	}
}

// Level 1 clears after 3 days of samples under 0.08%, and a sample at 0.09%
// restarts the count; a dip under the trigger alone clears nothing.
func TestNextMailBreakerStateLevelOneClears(t *testing.T) {
	settings := testMailBreakerSettings()
	now := testBreakerStart
	state := NextMailBreakerState(MailBreakerState{}, testBreakerSample(0, 0, 1000), now, settings)
	if state.Level != MailBreakerLevelRestrict || !state.CleanSinceTime.IsZero() {
		t.Fatalf("tripped: %+v", state)
	}
	step := func(duration time.Duration, measures *MailBreakerMeasures) {
		now = now.Add(duration)
		state = NextMailBreakerState(state, measures, now, settings)
	}

	// under the trigger and over the clear rate: the level holds with no count
	step(time.Hour, testBreakerSample(0, 0, 900))
	if state.Level != MailBreakerLevelRestrict || !state.CleanSinceTime.IsZero() {
		t.Fatalf("at 0.09%%: %+v", state)
	}
	// clean samples start the count
	step(time.Hour, testBreakerSample(799, 0, 799))
	cleanStart := now
	if !state.CleanSinceTime.Equal(cleanStart) {
		t.Fatalf("the count starts at %s, want %s", state.CleanSinceTime, cleanStart)
	}
	step(2*24*time.Hour, testBreakerSample(0, 0, 0))
	if state.Level != MailBreakerLevelRestrict || !state.CleanSinceTime.Equal(cleanStart) {
		t.Fatalf("two clean days: %+v", state)
	}
	// a day at 0.09% restarts it
	step(time.Hour, testBreakerSample(0, 0, 900))
	if state.Level != MailBreakerLevelRestrict || !state.CleanSinceTime.IsZero() || !state.CleanEndTime.Equal(now) {
		t.Fatalf("a sample at 0.09%% two days in: %+v", state)
	}
	// inside the holdoff a clean sample starts no count yet
	step(time.Minute, testBreakerSample(0, 0, 0))
	if !state.CleanSinceTime.IsZero() {
		t.Fatalf("a count restarted inside the holdoff: %+v", state)
	}
	step(time.Hour, testBreakerSample(0, 0, 0))
	cleanStart = now
	if !state.CleanSinceTime.Equal(cleanStart) {
		t.Fatalf("the count restarts at %s, want %s", state.CleanSinceTime, cleanStart)
	}
	// one second short of three days holds; three days clears
	step(3*24*time.Hour-time.Second, testBreakerSample(0, 0, 0))
	if state.Level != MailBreakerLevelRestrict {
		t.Fatalf("a second short of three clean days: %+v", state)
	}
	step(time.Second, testBreakerSample(0, 0, 0))
	if state.Level != MailBreakerLevelNone || !state.CleanSinceTime.IsZero() {
		t.Fatalf("three clean days: %+v", state)
	}
}

// Level 2 for a listed address needs the delisting, then 3 clean days: clean
// rates alone start no count while the address stands listed.
func TestNextMailBreakerStateLevelTwoNeedsDelistingThenCleanDays(t *testing.T) {
	settings := testMailBreakerSettings()
	now := testBreakerStart
	state := NextMailBreakerState(MailBreakerState{}, testBreakerSample(0, 0, 0, testBreakerAddress), now, settings)
	if state.Level != MailBreakerLevelCommunity || !slices.Equal(state.ListedAddresses, []netip.Addr{testBreakerAddress}) {
		t.Fatalf("listed: %+v", state)
	}
	step := func(duration time.Duration, measures *MailBreakerMeasures) {
		now = now.Add(duration)
		state = NextMailBreakerState(state, measures, now, settings)
	}
	// ten days listed with clean rates: no count
	step(10*24*time.Hour, testBreakerSample(0, 0, 0, testBreakerAddress))
	if state.Level != MailBreakerLevelCommunity || !state.CleanSinceTime.IsZero() {
		t.Fatalf("ten days listed: %+v", state)
	}
	// the listing source down: the address stays out, and nothing counts
	step(time.Hour, &MailBreakerMeasures{
		PostmasterRates: []MailRate{KnownMailRate(0), KnownMailRate(0)},
		ComplaintRate:   KnownMailRate(0),
	})
	if !slices.Equal(state.ListedAddresses, []netip.Addr{testBreakerAddress}) || !state.CleanSinceTime.IsZero() {
		t.Fatalf("an unknown listing: %+v", state)
	}
	// delisted: the address returns and the count starts
	step(time.Hour, testBreakerSample(0, 0, 0))
	if len(state.ListedAddresses) != 0 || !state.CleanSinceTime.Equal(now) || state.Level != MailBreakerLevelCommunity {
		t.Fatalf("delisted: %+v", state)
	}
	// listed again a day in: the count ends
	step(24*time.Hour, testBreakerSample(0, 0, 0, testBreakerAddress2))
	if !state.CleanSinceTime.IsZero() || !slices.Equal(state.ListedAddresses, []netip.Addr{testBreakerAddress2}) {
		t.Fatalf("listed again: %+v", state)
	}
	step(2*time.Hour, testBreakerSample(0, 0, 0))
	step(3*24*time.Hour, testBreakerSample(0, 0, 0))
	if state.Level != MailBreakerLevelNone {
		t.Fatalf("delisted and three clean days: %+v", state)
	}
}

// Level 3 clears only by a person: no run of clean samples takes it down, and
// none counts.
func TestNextMailBreakerStateLevelThreeOnlyByAPerson(t *testing.T) {
	settings := testMailBreakerSettings()
	now := testBreakerStart
	state := NextMailBreakerState(MailBreakerState{}, testBreakerSample(0, 0, 3000), now, settings)
	if state.Level != MailBreakerLevelAll {
		t.Fatalf("tripped: %+v", state)
	}
	for range 400 {
		now = now.Add(time.Hour)
		state = NextMailBreakerState(state, testBreakerSample(0, 0, 0), now, settings)
		if state.Level != MailBreakerLevelAll || !state.CleanSinceTime.IsZero() {
			t.Fatalf("level 3 after clean samples to %s: %+v", now, state)
		}
	}
	// a person's lift leaves the highest row still standing; from there the
	// machine clears by that level's rule
	state.Level = MailBreakerLevelCommunity
	now = now.Add(time.Hour)
	state = NextMailBreakerState(state, testBreakerSample(0, 0, 0), now, settings)
	state = NextMailBreakerState(state, testBreakerSample(0, 0, 0), now.Add(3*24*time.Hour), settings)
	if state.Level != MailBreakerLevelNone {
		t.Fatalf("level 2 after a person's lift and three clean days: %+v", state)
	}
}

// The levels ratchet, and a missing measure holds the level and the count: it
// never trips, never clears and never restarts.
func TestNextMailBreakerStateHoldsOnAMissingMeasure(t *testing.T) {
	settings := testMailBreakerSettings()
	now := testBreakerStart
	state := NextMailBreakerState(MailBreakerState{}, testBreakerSample(0, 0, 2000), now, settings)
	// a dip to level 1's band keeps level 2
	now = now.Add(time.Hour)
	state = NextMailBreakerState(state, testBreakerSample(0, 0, 1000), now, settings)
	if state.Level != MailBreakerLevelCommunity {
		t.Fatalf("a dip: %+v", state)
	}
	now = now.Add(time.Hour)
	state = NextMailBreakerState(state, testBreakerSample(0, 0, 0), now, settings)
	cleanStart := now
	if !state.CleanSinceTime.Equal(cleanStart) {
		t.Fatalf("a clean sample: %+v", state)
	}

	unknowns := []*MailBreakerMeasures{
		nil,
		{},
		// the complaint rate alone
		{ComplaintRate: KnownMailRate(0), ListingKnown: true},
		// no Postmaster day reported
		{PostmasterRates: []MailRate{}, ComplaintRate: KnownMailRate(0), ListingKnown: true},
		{PostmasterRates: []MailRate{{}, KnownMailRate(0)}, ComplaintRate: KnownMailRate(0), ListingKnown: true},
		// the listing source down
		{PostmasterRates: []MailRate{KnownMailRate(0)}, ComplaintRate: KnownMailRate(0)},
		{PostmasterRates: []MailRate{KnownMailRate(0)}, ListingKnown: true},
	}
	for i, measures := range unknowns {
		// thirty days of it
		next := NextMailBreakerState(state, measures, now.Add(30*24*time.Hour), settings)
		if !next.Equal(state) {
			t.Errorf("unknown sample %d: %+v, want the state held at %+v", i, next, state)
		}
	}
	// from no level, a sample with nothing known trips nothing
	for i, measures := range unknowns {
		if next := NextMailBreakerState(MailBreakerState{}, measures, now, settings); next.Level != MailBreakerLevelNone {
			t.Errorf("unknown sample %d tripped level %d", i, next.Level)
		}
	}
	// a known measure still trips beside unknown ones
	next := NextMailBreakerState(MailBreakerState{}, &MailBreakerMeasures{ComplaintRate: KnownMailRate(3000)}, now, settings)
	if next.Level != MailBreakerLevelAll {
		t.Errorf("complaints at 0.30%% with the rest unknown: level %d", next.Level)
	}
	// a source that answered listed counts though another did not answer
	next = NextMailBreakerState(MailBreakerState{}, &MailBreakerMeasures{ListedAddresses: []netip.Addr{testBreakerAddress}}, now, settings)
	if next.Level != MailBreakerLevelCommunity || !slices.Equal(next.ListedAddresses, []netip.Addr{testBreakerAddress}) {
		t.Errorf("a listing with another source down: %+v", next)
	}
	// the clean count, once every measure is back, runs from where it was
	next = NextMailBreakerState(state, testBreakerSample(0, 0, 0), cleanStart.Add(3*24*time.Hour), settings)
	if next.Level != MailBreakerLevelNone {
		t.Errorf("three clean days across an unknown stretch: %+v", next)
	}
}

// Thresholds pro.yml does not give whole make every rate unknown: nothing
// trips on a rate and nothing clears, while a listing still counts.
func TestNextMailBreakerStateUnusableThresholds(t *testing.T) {
	now := testBreakerStart
	unusable := []func(settings *MailBreakerSettings){
		func(settings *MailBreakerSettings) {
			settings.RestrictRate, settings.CommunityRate, settings.AllRate, settings.ClearRate = 0, 0, 0, 0
		},
		func(settings *MailBreakerSettings) { settings.ClearRate = 0 },
		func(settings *MailBreakerSettings) { settings.ClearRate = settings.RestrictRate + 1 },
		func(settings *MailBreakerSettings) { settings.CommunityRate = settings.RestrictRate - 1 },
		func(settings *MailBreakerSettings) { settings.AllRate = settings.CommunityRate - 1 },
		func(settings *MailBreakerSettings) { settings.AllRate = -1 },
	}
	for i, change := range unusable {
		settings := testMailBreakerSettings()
		change(settings)
		if next := NextMailBreakerState(MailBreakerState{}, testBreakerSample(9000, 9000, 9000), now, settings); next.Level != MailBreakerLevelNone {
			t.Errorf("thresholds %d: a rate tripped level %d", i, next.Level)
		}
		standing := MailBreakerState{Level: MailBreakerLevelRestrict, CleanSinceTime: now.Add(-30 * 24 * time.Hour)}
		if next := NextMailBreakerState(standing, testBreakerSample(0, 0, 0), now, settings); !next.Equal(standing) {
			t.Errorf("thresholds %d: %+v, want the state held", i, next)
		}
		if next := NextMailBreakerState(MailBreakerState{}, testBreakerSample(0, 0, 0, testBreakerAddress), now, settings); next.Level != MailBreakerLevelCommunity {
			t.Errorf("thresholds %d: a listing gave level %d", i, next.Level)
		}
	}
	// no clear days: the machine never clears
	settings := testMailBreakerSettings()
	settings.ClearDays = 0
	standing := MailBreakerState{Level: MailBreakerLevelRestrict, CleanSinceTime: now.Add(-30 * 24 * time.Hour)}
	if next := NextMailBreakerState(standing, testBreakerSample(0, 0, 0), now, settings); next.Level != MailBreakerLevelRestrict {
		t.Errorf("no clear days: %+v", next)
	}
}

// The levels are the send gate's, value for value, since smtpedge converts one
// to the other on its refresh. §21.5's tier table, which pauses restricted
// networks at level 1, Community at 2 and all but a trusted Team at 3 and
// halves Community's caps from 1, is the gate's breaker row and its tests'
// (smtpedgeservice's TestSmtpGateBreakerLevels and
// TestSmtpGateHalvesCommunityRatesAtLevelOne).
func TestMailBreakerLevelsAreTheSendGates(t *testing.T) {
	for level, gateLevel := range map[MailBreakerLevel]smtpedgeservice.SmtpBreakerLevel{
		MailBreakerLevelNone:      smtpedgeservice.SmtpBreakerLevelNone,
		MailBreakerLevelRestrict:  smtpedgeservice.SmtpBreakerLevelRestrict,
		MailBreakerLevelCommunity: smtpedgeservice.SmtpBreakerLevelCommunity,
		MailBreakerLevelAll:       smtpedgeservice.SmtpBreakerLevelAll,
	} {
		if int(level) != int(gateLevel) {
			t.Errorf("level %d is the gate's %d", level, gateLevel)
		}
	}
}

// A breaker row's target reads back as what wrote it, and nothing else reads.
func TestMailBreakerTargets(t *testing.T) {
	for _, level := range []MailBreakerLevel{MailBreakerLevelRestrict, MailBreakerLevelCommunity, MailBreakerLevelAll} {
		parsed, ok := parseMailBreakerTarget(MailBreakerLevelTarget(level))
		if !ok || parsed.word != mailBreakerTargetLevel || parsed.level != level {
			t.Errorf("level %d reads %+v, %t", level, parsed, ok)
		}
	}
	for _, address := range []netip.Addr{testBreakerAddress, netip.MustParseAddr("2001:db8::10"), netip.MustParseAddr("::ffff:192.0.2.10")} {
		parsed, ok := parseMailBreakerTarget(MailBreakerAddressTarget(address))
		if !ok || parsed.word != mailBreakerTargetAddress || parsed.address != address.Unmap() {
			t.Errorf("address %s reads %+v, %t", address, parsed, ok)
		}
	}
	cleanSince := time.Date(2026, 11, 2, 12, 30, 15, 0, time.UTC)
	parsed, ok := parseMailBreakerTarget(MailBreakerCleanTarget(cleanSince))
	if !ok || parsed.word != mailBreakerTargetClean || !parsed.cleanSinceTime.Equal(cleanSince) {
		t.Errorf("a clean time reads %+v, %t", parsed, ok)
	}
	for _, target := range []string{
		"", "level", "level:", "level:0", "level:4", "level:-1", "level:01", "level:2 ", "level:two",
		"address:", "address:192.0.2", "address:192.0.2.010", "address:::ffff:192.0.2.10", "address:shop.root.example",
		"clean:", "clean:2026-11-02", "clean:2026-11-02T12:30:15+01:00", "clean:2026-11-02T12:30:15.5Z",
		"shop.root.example", "other:1",
	} {
		if parsed, ok := parseMailBreakerTarget(target); ok {
			t.Errorf("%q reads as %+v", target, parsed)
		}
	}
}

// The listed addresses a state holds are sorted, each once and unmapped,
// whatever order the sources named them in.
func TestNextMailBreakerStateKeepsAddressesCanonical(t *testing.T) {
	settings := testMailBreakerSettings()
	mapped := netip.MustParseAddr("::ffff:192.0.2.10")
	next := NextMailBreakerState(MailBreakerState{}, testBreakerSample(0, 0, 0, testBreakerAddress2, mapped, testBreakerAddress, netip.Addr{}), testBreakerStart, settings)
	if !slices.Equal(next.ListedAddresses, []netip.Addr{testBreakerAddress, testBreakerAddress2}) {
		t.Errorf("listed %v", next.ListedAddresses)
	}
}

// pro.yml's keys lay the thresholds over the settings, and a file without
// them leaves the rates unusable.
func TestApplyProMailBreaker(t *testing.T) {
	c := &ProConfig{}
	parseProSidecarConfig([]byte(`
sidecar_abuse:
  breaker_restrict_rate: 1000
  breaker_community_rate: 2000
  breaker_all_rate: 3000
  breaker_clear_rate: 800
  breaker_clear_days: 3
`), c)
	settings := DefaultMailBreakerSettings()
	if settings.ratesUsable() {
		t.Fatal("the defaults carry usable rates, which are pro.yml's to give")
	}
	ApplyProMailBreaker(settings, &c.SidecarAbuse)
	if settings.RestrictRate != 1000 || settings.CommunityRate != 2000 || settings.AllRate != 3000 || settings.ClearRate != 800 || settings.ClearDays != 3 {
		t.Errorf("the settings read %+v", settings)
	}
	if !settings.ratesUsable() {
		t.Error("§21.5's rates are not usable")
	}
	empty := &ProConfig{}
	parseProSidecarConfig([]byte("sidecar_abuse:\n  mail_group_members: 100\n"), empty)
	settings = DefaultMailBreakerSettings()
	ApplyProMailBreaker(settings, &empty.SidecarAbuse)
	if settings.ratesUsable() || settings.ClearDays != 0 {
		t.Errorf("a file with no breaker keys reads %+v", settings)
	}
}

// A listing takes an address out of sending only, never out of ingress (owner
// decision 124): by a source check, the breaker state's readers are this file,
// the abuse worker that writes it and smtpedge's send gate, and nothing in
// api, connect's server or any other block reads it. connect cannot import
// server at all.
func TestMailBreakerStateReachesOnlyTheSendGate(t *testing.T) {
	readers := map[string]bool{
		"model":           true,
		"taskworker/work": true,
		"cli/smtpedge":    true,
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), "GetMailBreakerState(") && !strings.Contains(string(data), "GetMailBreakerRows(") {
			return nil
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir = filepath.ToSlash(dir)
		found[dir] = true
		if !readers[dir] {
			t.Errorf("%s reads the breaker state", filepath.ToSlash(filepath.Join(dir, filepath.Base(path))))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// the check reads the tree it means to: the gate and the job are among the readers
	if !found["cli/smtpedge"] || !found["taskworker/work"] {
		t.Fatalf("the walk found the readers %v", found)
	}
}
