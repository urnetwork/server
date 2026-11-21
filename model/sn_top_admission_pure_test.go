// Rule (a) of the SN Top board (sn_top_admission_model.go, owner decisions
// 2026-11-20): a provider's prefix counts toward its network's units only
// while the score export admits it to Speed or Quality, smoothed over a
// rolling window with hysteresis; Online counts instead where the country's
// probes make no UDP leg, and admission with UDP forgiven where UDP fails at
// the network level. Before the rule every provider with a pin or a trail
// prefix counted, admitted or not, so a fleet of identities that fail every
// probe ranked by its prefixes alone. Pure: the inputs are built here.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Who counts, by the smoothed decisions: an admitted provider; an Online one
// only where probes make no UDP leg; one admitted with UDP forgiven only where
// UDP fails at the network level; nobody else, nor a provider the export did
// not decide.
func TestSnTopAdmissionCountsUnits(t *testing.T) {
	steady := func(admitted, online, tcpAdmitted bool) *SnTopAdmissionProvider {
		return Testing_SnTopAdmissionProvider(snTopTestId(1), "us", admitted, online, tcpAdmitted)
	}
	none := SnTopAdmissionExcuses{}
	udpOff := SnTopAdmissionExcuses{UdpProbesDisabled: true}
	networkLevel := SnTopAdmissionExcuses{UdpNetworkLevel: true}
	cases := []struct {
		name     string
		provider *SnTopAdmissionProvider
		excuses  SnTopAdmissionExcuses
		want     bool
	}{
		{"admitted", steady(true, true, true), none, true},
		{"online, udp-off country", steady(false, true, false), udpOff, true},
		{"online elsewhere", steady(false, true, false), none, false},
		{"udp forgiven, network-level udp failure", steady(false, true, true), networkLevel, true},
		{"udp forgiven, no network-level failure", steady(false, true, true), none, false},
		{"online, network-level only", steady(false, true, false), networkLevel, false},
		{"neither", steady(false, false, false), SnTopAdmissionExcuses{UdpProbesDisabled: true, UdpNetworkLevel: true}, false},
		{"not decided", nil, udpOff, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			connect.AssertEqual(t, c.provider.CountsUnits(c.excuses), c.want)
		})
	}
}

// One bad decision does not take a unit: the track withholds only after
// about three hours of sustained failure (a mean below 0.35 at a two-hour
// half-life) and restores only after about three hours of sustained
// admission (0.65).
func TestSnTopAdmissionTrackHysteresis(t *testing.T) {
	step := 5 * time.Minute
	track := newSnTopAdmissionTrack(true)
	// one failed export, then admitted again: still counting throughout
	track = track.fold(false, step)
	connect.AssertEqual(t, track.Counts, true)
	track = track.fold(true, step)
	connect.AssertEqual(t, track.Counts, true)

	// sustained failure: counting until the mean falls below 0.35
	failedFor := time.Duration(0)
	for track.Counts {
		track = track.fold(false, step)
		failedFor += step
	}
	if failedFor < 170*time.Minute || 195*time.Minute < failedFor {
		t.Fatalf("withheld after %s of failure, want about 3h", failedFor)
	}
	// one admitted export does not restore it
	track = track.fold(true, step)
	connect.AssertEqual(t, track.Counts, false)
	// a provider that failed for half a day takes about three hours of
	// admission to count again (from the band's edge, about two)
	track = track.fold(false, 12*time.Hour)
	admittedFor := time.Duration(0)
	for !track.Counts {
		track = track.fold(true, step)
		admittedFor += step
	}
	if admittedFor < 170*time.Minute || 195*time.Minute < admittedFor {
		t.Fatalf("restored after %s of admission, want about 3h", admittedFor)
	}
	// the elapsed time, not the number of exports, sets the weight
	slow := newSnTopAdmissionTrack(true).fold(false, 4*time.Hour)
	connect.AssertEqual(t, slow.Counts, false)
}

// The export decides on the strict score: native Speed or Quality is
// admission; a classified copy that passes once UDP is forgiven is TCP
// admission; Online is the URL bar's, never the country floor's. A provider in
// several location maps is decided once, under its published country, with a
// reason from its decision; its tracks fold from the previous set.
func TestNewSnTopAdmissionDecidesFromTheStrictScore(t *testing.T) {
	quality, speed, udpOnly, onlineOnly, floorOnly, risky := snTopTestId(1), snTopTestId(2), snTopTestId(3), snTopTestId(4), snTopTestId(5), snTopTestId(6)
	score := func(clientId server.Id, passes map[string]bool, online bool) *ClientScore {
		return &ClientScore{ClientId: clientId, PassesMinimums: passes, Online: online}
	}
	floor := score(floorOnly, nil, true)
	floor.countryFloor = true
	udp := score(udpOnly, map[string]bool{}, true)
	udp.CapabilityDowngrades = ClientScoreCapabilityDowngradeUdp
	udp.capabilityCopy = &ClientScore{ClientId: udpOnly, PassesMinimums: map[string]bool{RankModeSpeed: true}, Online: true}
	city, country := snTopTestId(100), snTopTestId(101)
	locationClientScores := map[server.Id]map[server.Id]*ClientScore{
		city: {
			quality: score(quality, map[string]bool{RankModeQuality: true}, true),
		},
		country: {
			quality:    score(quality, map[string]bool{RankModeQuality: true}, true),
			speed:      score(speed, map[string]bool{RankModeSpeed: true}, true),
			udpOnly:    udp,
			onlineOnly: score(onlineOnly, map[string]bool{}, true),
			floorOnly:  floor,
			risky:      score(risky, nil, false),
		},
	}
	ru, upper, evidence := "ru", "VN", true
	egresses := map[server.Id]*clientScoreEgress{}
	for _, clientId := range []server.Id{quality, speed, udpOnly, onlineOnly, floorOnly, risky} {
		egresses[clientId] = &clientScoreEgress{egressQuality: &evidence}
	}
	egresses[onlineOnly].publishedCountryCode = &ru
	egresses[quality].publishedCountryCode = &upper
	decisions := map[server.Id]providerEgressDecision{
		udpOnly:    {reason: ProviderExcludedLowSuccess},
		onlineOnly: {reason: ProviderExcludedLowSuccess, online: true},
		floorOnly:  {reason: ProviderExcludedLowSuccess},
		risky:      {reason: ProviderExcludedArinRisk},
	}
	measuredAt := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	admission := newSnTopAdmission(measuredAt, locationClientScores, egresses, decisions, nil)

	connect.AssertEqual(t, admission.MeasuredAt, measuredAt)
	connect.AssertEqual(t, len(admission.Providers), 6)
	q := admission.Providers[quality]
	connect.AssertEqual(t, q.CountryCode, "vn")
	connect.AssertEqual(t, q.Admitted && q.TcpAdmitted && q.AdmittedTrack.Counts, true)
	connect.AssertEqual(t, q.Reason, SnTopUnitReasonNone)
	connect.AssertEqual(t, admission.Providers[speed].Admitted, true)
	u := admission.Providers[udpOnly]
	connect.AssertEqual(t, u.Admitted, false)
	connect.AssertEqual(t, u.TcpAdmitted && u.UdpDowngraded && u.TcpAdmittedTrack.Counts, true)
	connect.AssertEqual(t, u.Reason, SnTopUnitReasonUdp)
	o := admission.Providers[onlineOnly]
	connect.AssertEqual(t, o.CountryCode, "ru")
	connect.AssertEqual(t, o.Online && !o.Admitted && !o.TcpAdmitted, true)
	connect.AssertEqual(t, o.Reason, SnTopUnitReasonUrl)
	connect.AssertEqual(t, admission.Providers[floorOnly].Online, false)
	connect.AssertEqual(t, admission.Providers[risky].Reason, SnTopUnitReasonRisk)

	// the next export folds into the tracks: a provider that just failed
	// still counts
	speedFailed := map[server.Id]map[server.Id]*ClientScore{country: {speed: score(speed, map[string]bool{}, true)}}
	next := newSnTopAdmission(measuredAt.Add(5*time.Minute), speedFailed, egresses, map[server.Id]providerEgressDecision{speed: {reason: ProviderExcludedLowSuccess}}, admission)
	s := next.Providers[speed]
	connect.AssertEqual(t, s.Admitted, false)
	connect.AssertEqual(t, s.AdmittedTrack.Counts, true)
	connect.AssertEqual(t, len(next.Providers), 1)
	// after a day's gap the history is stale and the track starts again
	late := newSnTopAdmission(measuredAt.Add(25*time.Hour), speedFailed, egresses, nil, admission)
	connect.AssertEqual(t, late.Providers[speed].AdmittedTrack.Counts, false)
}

// The published set decodes to what was encoded; a truncated or malformed set
// is refused rather than read as fewer providers.
func TestSnTopAdmissionRoundTrip(t *testing.T) {
	a, b, c := snTopTestId(1), snTopTestId(2), snTopTestId(3)
	tracked := Testing_SnTopAdmissionProvider(b, "ru", false, true, true)
	tracked.AdmittedTrack = SnTopAdmissionTrack{Score: 0.5, Counts: true}
	admission := &SnTopAdmission{
		MeasuredAt: time.Date(2026, 11, 20, 22, 10, 0, 0, time.UTC),
		Providers: map[server.Id]*SnTopAdmissionProvider{
			a: Testing_SnTopAdmissionProvider(a, "ca", true, true, true),
			b: tracked,
			c: {ClientId: c, CountryCode: "not a country", Reason: SnTopUnitReasonRisk},
		},
	}
	encoded := encodeSnTopAdmission(admission)
	connect.AssertEqual(t, len(encoded), snTopAdmissionHeaderBytes+3*snTopAdmissionProviderBytes)
	decoded, err := decodeSnTopAdmission(encoded)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, decoded.MeasuredAt, admission.MeasuredAt)
	connect.AssertEqual(t, *decoded.Providers[a], *admission.Providers[a])
	got := decoded.Providers[b]
	connect.AssertEqual(t, got.Reason, SnTopUnitReasonUdp)
	connect.AssertEqual(t, got.TcpAdmitted && got.UdpDowngraded && got.Online && got.AdmittedTrack.Counts, true)
	snTopNear(t, got.AdmittedTrack.Score, 128.0/255)
	connect.AssertEqual(t, decoded.Providers[c].CountryCode, CtfCountryUnknown)
	connect.AssertEqual(t, decoded.Providers[c].Reason, SnTopUnitReasonRisk)

	for _, bad := range [][]byte{
		encoded[:len(encoded)-1],
		encoded[:4],
		append([]byte{1}, encoded[1:]...),
	} {
		_, err = decodeSnTopAdmission(bad)
		connect.AssertNotEqual(t, err, nil)
	}
	badFlags := append([]byte{}, encoded...)
	badFlags[snTopAdmissionHeaderBytes+18] |= 0x80
	_, err = decodeSnTopAdmission(badFlags)
	connect.AssertNotEqual(t, err, nil)
}

// A set older than SnTopAdmissionMaxAge, or none, is not fresh.
func TestSnTopAdmissionFresh(t *testing.T) {
	now := time.Date(2026, 11, 20, 22, 30, 0, 0, time.UTC)
	var missing *SnTopAdmission
	connect.AssertEqual(t, missing.Fresh(now), false)
	connect.AssertEqual(t, (&SnTopAdmission{MeasuredAt: now.Add(-SnTopAdmissionMaxAge)}).Fresh(now), true)
	connect.AssertEqual(t, (&SnTopAdmission{MeasuredAt: now.Add(-SnTopAdmissionMaxAge - time.Second)}).Fresh(now), false)
}

// The rule ranks the live board from its first settlement epoch on.
func TestSnTopUnitRuleLive(t *testing.T) {
	connect.AssertEqual(t, SnTopUnitRuleLive(SnTopUnitRuleFirstEpoch-1), false)
	connect.AssertEqual(t, SnTopUnitRuleLive(SnTopUnitRuleFirstEpoch), true)
	connect.AssertEqual(t, SnTopUnitRuleLive(SnTopUnitRuleFirstEpoch+1), true)
}

func snTopAdmissionTestRow(projection *SnTopProjection, networkId server.Id) *SnTopNetworkProjection {
	for _, row := range projection.Networks {
		if row.NetworkId == networkId {
			return row
		}
	}
	return nil
}

// A withheld provider shows no unit, pinned or trail, and an admitted one keeps
// its unit: of three networks with one prefix each, the withheld network has
// no score, and the other two rank on their own prefix alone.
func TestSnTopProjectionWithheldProviderShowsNoUnit(t *testing.T) {
	withheld, admitted, measured := snTopTestId(1), snTopTestId(2), snTopTestId(3)
	pinned := snTopProvider(snTopTestId(101), withheld, snTopTestPrefix(1), 1, false)
	pinned.UnitWithheld = true
	trail := snTopProvider(snTopTestId(301), measured, snTopTestPrefix(9), 1, true)
	trail.TrailPrefixes = [][32]byte{snTopTestPrefix(3)}
	trailWithheld := snTopProvider(snTopTestId(302), measured, snTopTestPrefix(10), 1, true)
	trailWithheld.TrailPrefixes = [][32]byte{snTopTestPrefix(4), snTopTestPrefix(5)}
	trailWithheld.UnitWithheld = true
	projection := ProjectSnTop(&SnTopProjectionInput{
		TempoIndex: 10,
		Providers: []SnTopProvider{
			pinned,
			snTopProvider(snTopTestId(201), admitted, snTopTestPrefix(2), 1, false),
			trail,
			trailWithheld,
		},
	})
	connect.AssertEqual(t, projection.TotalRanked, 2)
	if row := snTopAdmissionTestRow(projection, withheld); row != nil {
		connect.AssertEqual(t, row.Prefixes, 0)
		connect.AssertEqual(t, row.Position, 0)
	}
	rowAdmitted := snTopRowOf(t, projection, admitted)
	snTopNear(t, rowAdmitted.RawLastTempo, 1)
	connect.AssertEqual(t, rowAdmitted.Prefixes, 1)
	// the withheld provider's two trail prefixes add nothing to its network
	rowMeasured := snTopRowOf(t, projection, measured)
	snTopNear(t, rowMeasured.RawLastTempo, 1)
	connect.AssertEqual(t, rowMeasured.Prefixes, 1)
	// a withheld provider leaves its network verify eligible and measurable
	connect.AssertEqual(t, rowMeasured.Measurable, true)
}

// The marks still apply under the rule: a provider whose pin or trail is the
// zero hash (a declared proxy egress, or a probed exit that differs from its
// networks) shows no unit even when admitted, and a withheld provider shows
// none even with a real prefix. Only an admitted provider with a real prefix
// counts.
func TestSnTopWithheldAndZeroHashCompose(t *testing.T) {
	prefix := snTopTestPrefix(7)
	cases := []struct {
		name     string
		provider SnTopProvider
		want     int
	}{
		{"admitted, real pin", SnTopProvider{PrefixHash: prefix, HasPrefix: true, Presence: 1}, 1},
		{"admitted, zero pin", SnTopProvider{PrefixHash: [32]byte{}, HasPrefix: true, Presence: 1}, 0},
		{"admitted, zero hops", SnTopProvider{Measured: true, TrailPrefixes: [][32]byte{{}}, PrefixHash: prefix, HasPrefix: true, Presence: 1}, 0},
		{"withheld, real pin", SnTopProvider{PrefixHash: prefix, HasPrefix: true, Presence: 1, UnitWithheld: true}, 0},
		{"withheld, real hops", SnTopProvider{Measured: true, TrailPrefixes: [][32]byte{prefix}, Presence: 1, UnitWithheld: true}, 0},
		{"withheld, zero pin", SnTopProvider{PrefixHash: [32]byte{}, HasPrefix: true, Presence: 1, UnitWithheld: true}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			connect.AssertEqual(t, len(snTopProviderContribution(&c.provider)), c.want)
		})
	}
}
