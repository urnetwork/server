// Rule (a) of the SN Top board (sn_top_admission_model.go, owner decision
// 2026-11-20): a provider's prefix counts toward its network's units only while
// the score export admitted it to Speed or Quality, or, in a country whose URL
// probes make no UDP leg, decided it Online. Before the rule every provider
// with a pin or a trail prefix counted, admitted or not, so a fleet of
// identities that fail every probe ranked by its prefixes alone. Pure: the
// inputs are built here.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func snTopAdmissionTestUdpOff(countryCode string) bool {
	return countryCode == "ru"
}

// Who counts: an admitted provider; an Online one only where probes make no
// UDP leg; nobody else, nor a provider the export did not decide.
func TestSnTopAdmissionCountsUnits(t *testing.T) {
	cases := []struct {
		name     string
		provider *SnTopAdmissionProvider
		want     bool
	}{
		{"admitted", &SnTopAdmissionProvider{CountryCode: "vn", Admitted: true, Online: true}, true},
		{"admitted in a udp-off country", &SnTopAdmissionProvider{CountryCode: "ru", Admitted: true, Online: true}, true},
		{"online in a udp-off country", &SnTopAdmissionProvider{CountryCode: "ru", Online: true}, true},
		{"online elsewhere", &SnTopAdmissionProvider{CountryCode: "vn", Online: true}, false},
		{"neither in a udp-off country", &SnTopAdmissionProvider{CountryCode: "ru"}, false},
		{"neither", &SnTopAdmissionProvider{CountryCode: "us"}, false},
		{"not decided", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			connect.AssertEqual(t, c.provider.CountsUnits(snTopAdmissionTestUdpOff), c.want)
		})
	}
	// with no udp-off countries only admission counts
	connect.AssertEqual(t, (&SnTopAdmissionProvider{CountryCode: "ru", Online: true}).CountsUnits(nil), false)
}

// The export decides on the strict score: native Speed or Quality is
// admission, a classified capability copy that passes is not, and Online is
// the URL bar's, never the country floor's. A provider in several location
// maps is decided once, under the country it is published in.
func TestNewSnTopAdmissionDecidesFromTheStrictScore(t *testing.T) {
	quality, speed, onlineOnly, floorOnly, capabilityOnly, neither := snTopTestId(1), snTopTestId(2), snTopTestId(3), snTopTestId(4), snTopTestId(5), snTopTestId(6)
	score := func(clientId server.Id, passes map[string]bool, online bool) *ClientScore {
		return &ClientScore{ClientId: clientId, PassesMinimums: passes, Online: online}
	}
	floor := score(floorOnly, nil, true)
	floor.countryFloor = true
	classified := score(capabilityOnly, map[string]bool{}, true)
	classified.capabilityCopy = &ClientScore{ClientId: capabilityOnly, PassesMinimums: map[string]bool{RankModeSpeed: true}, Online: true}
	city, country := snTopTestId(100), snTopTestId(101)
	locationClientScores := map[server.Id]map[server.Id]*ClientScore{
		city: {
			quality: score(quality, map[string]bool{RankModeQuality: true}, true),
		},
		country: {
			quality:        score(quality, map[string]bool{RankModeQuality: true}, true),
			speed:          score(speed, map[string]bool{RankModeSpeed: true}, true),
			onlineOnly:     score(onlineOnly, map[string]bool{}, true),
			floorOnly:      floor,
			capabilityOnly: classified,
			neither:        score(neither, nil, false),
		},
	}
	ru, upper := "ru", "VN"
	egresses := map[server.Id]*clientScoreEgress{
		onlineOnly: {publishedCountryCode: &ru},
		quality:    {publishedCountryCode: &upper},
	}
	measuredAt := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	admission := newSnTopAdmission(measuredAt, locationClientScores, egresses)

	connect.AssertEqual(t, admission.MeasuredAt, measuredAt)
	connect.AssertEqual(t, len(admission.Providers), 6)
	connect.AssertEqual(t, *admission.Providers[quality], SnTopAdmissionProvider{ClientId: quality, CountryCode: "vn", Admitted: true, Online: true})
	connect.AssertEqual(t, admission.Providers[speed].Admitted, true)
	connect.AssertEqual(t, *admission.Providers[onlineOnly], SnTopAdmissionProvider{ClientId: onlineOnly, CountryCode: "ru", Online: true})
	connect.AssertEqual(t, admission.Providers[floorOnly].Online, false)
	connect.AssertEqual(t, admission.Providers[capabilityOnly].Admitted, false)
	connect.AssertEqual(t, admission.Providers[neither].Admitted || admission.Providers[neither].Online, false)
	connect.AssertEqual(t, admission.Providers[neither].CountryCode, CtfCountryUnknown)
}

// The published set decodes to what was encoded; a truncated or malformed set
// is refused rather than read as fewer providers.
func TestSnTopAdmissionRoundTrip(t *testing.T) {
	a, b, c := snTopTestId(1), snTopTestId(2), snTopTestId(3)
	admission := &SnTopAdmission{
		MeasuredAt: time.Date(2026, 11, 20, 22, 10, 0, 0, time.UTC),
		Providers: map[server.Id]*SnTopAdmissionProvider{
			a: {ClientId: a, CountryCode: "ca", Admitted: true, Online: true},
			b: {ClientId: b, CountryCode: "ru", Online: true},
			c: {ClientId: c, CountryCode: "not a country"},
		},
	}
	encoded := encodeSnTopAdmission(admission)
	connect.AssertEqual(t, len(encoded), snTopAdmissionHeaderBytes+3*snTopAdmissionProviderBytes)
	decoded, err := decodeSnTopAdmission(encoded)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, decoded.MeasuredAt, admission.MeasuredAt)
	connect.AssertEqual(t, *decoded.Providers[a], *admission.Providers[a])
	connect.AssertEqual(t, *decoded.Providers[b], *admission.Providers[b])
	connect.AssertEqual(t, decoded.Providers[c].CountryCode, CtfCountryUnknown)

	_, err = decodeSnTopAdmission(encoded[:len(encoded)-1])
	connect.AssertNotEqual(t, err, nil)
	_, err = decodeSnTopAdmission(encoded[:4])
	connect.AssertNotEqual(t, err, nil)
	badFlags := append([]byte{}, encoded...)
	badFlags[len(badFlags)-1] |= 0x80
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
