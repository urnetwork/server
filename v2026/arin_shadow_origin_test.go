package server

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func TestArinOriginAttributionCountsAllStatesAndBounds(t *testing.T) {
	r, _, _ := captureFixture(t)
	s, err := r.NewCurrentCaptureStream(context.Background(), []string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	origin := newArinShadowOrigin([]uint32{12345, 12346}, "unknown")
	for _, state := range []string{"subscriber", "unknown", "excluded", "ambiguous"} {
		s.addOrigin(arinShadowFacts{state: state, risk: state == "excluded", proxyRisk: state == "excluded", origin: origin})
	}
	for i := 1; i < arinShadowOriginLimit; i++ {
		s.addOrigin(arinShadowFacts{state: "unknown", origin: newArinShadowOrigin([]uint32{uint32(i)}, "unknown")})
	}
	s.addOrigin(arinShadowFacts{state: "unknown", origin: newArinShadowOrigin([]uint32{500000}, "unknown")})
	s.addOrigin(arinShadowFacts{state: "unknown"})
	// Existing groups keep exact counts after the distinct-group cap is reached.
	s.addOrigin(arinShadowFacts{state: "unknown", origin: origin})
	s.finishOrigins()
	first := s.report.OriginCounts[0]
	if len(s.report.OriginCounts) != arinShadowOriginLimit || first.Connections != 5 || first.Unknown != 2 || first.Subscriber != 1 || first.Excluded != 1 || first.Ambiguous != 1 || first.Risk != 1 || first.ProxyRisk != 1 || !slices.Equal(first.ASNs, origin.ASNs) || s.report.OriginOverflowConnections != 1 || s.report.OriginUnattributedConnections != 1 {
		t.Fatal("origin states or bounded connection accounting lost")
	}
	if s.report.Providers != 0 || s.report.CapturedConnections != 0 {
		t.Fatal("attribution invented population evidence")
	}
	for _, invalid := range []ArinShadowOrigin{
		{UseState: "unknown"}, {ASNs: []uint32{12345}},
		{ASNs: []uint32{0}, UseState: "unknown"},
		{ASNs: []uint32{12345, 12345}, UseState: "unknown"},
		{ASNs: []uint32{12346, 12345}, UseState: "unknown"},
		{ASNs: []uint32{12345}, UseState: "192.0.2.1"},
		{ASNs: []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9}, UseState: "unknown"},
	} {
		if validArinShadowOrigin(invalid) || len(newArinShadowOrigin(invalid.ASNs, invalid.UseState).ASNs) != 0 {
			t.Fatal("invalid or truncated origin attribution accepted")
		}
	}
}

func TestArinOriginWithheldIdentityRemainsDistinctForResearch(t *testing.T) {
	r, _, _ := captureFixture(t)
	s, err := r.NewCurrentCaptureStream(context.Background(), []string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	for _, originState := range []string{"unknown", "withheld"} {
		s.addOrigin(arinShadowFacts{state: "unknown", origin: newArinShadowOrigin([]uint32{12345}, originState)})
	}
	s.finishOrigins()
	if len(s.report.OriginCounts) != 2 || s.report.OriginUnattributedConnections != 0 || s.report.OriginCounts[0].UseState != "unknown" || s.report.OriginCounts[1].UseState != "withheld" {
		t.Fatal("withheld identity was discarded or combined with unknown use")
	}
	for _, row := range s.report.OriginCounts {
		if row.Connections != 1 || row.Unknown != 1 || row.Subscriber != 0 || row.Risk != 0 {
			t.Fatal("withheld origin attribution changed classification")
		}
	}
}

func TestArinOriginActualDecoderKeepsIndependentClassification(t *testing.T) {
	for _, originState := range []string{"unknown", "withheld"} {
		t.Run(originState, func(t *testing.T) { testArinOriginActualDecoder(t, originState) })
	}
}

func testArinOriginActualDecoder(t *testing.T, originState string) {
	for _, state := range []string{"subscriber", "unknown", "excluded", "ambiguous"} {
		for _, risk := range []bool{false, true} {
			record := mmdbtype.Map{
				"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
				"quality_state": mmdbtype.String(state), "non_quality": mmdbtype.Bool(state != "subscriber"), "risk": mmdbtype.Bool(risk),
				"origin_asns": mmdbtype.Slice{mmdbtype.Uint32(12345), mmdbtype.Uint32(12346)}, "origin_use_state": mmdbtype.String(originState),
			}
			data := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record})
			db, err := mmdb.OpenBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			facts, info, err := shadowFacts(db, netip.MustParseAddr("192.0.2.42"))
			db.Close()
			if err != nil || facts.state != state || facts.risk != risk || facts.verified != (state == "subscriber") || info.QualityVerified() != (state == "subscriber" && !risk) || !slices.Equal(facts.origin.ASNs, []uint32{12345, 12346}) || facts.origin.UseState != originState {
				t.Fatal("routing identity changed independent subscriber/risk facts", err)
			}
			if facts.registration != (ArinShadowRegistration{}) {
				t.Fatal("origin ASN manufactured ARIN registration identity")
			}
			encoded, err := json.Marshal(facts.origin)
			if err != nil || strings.Contains(string(encoded), "192.0.2") {
				t.Fatal("address escaped public origin identity")
			}
		}
	}
}
