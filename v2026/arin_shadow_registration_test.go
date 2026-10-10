package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func TestArinRegistrationAttributionCountsPrivacyAndBounds(t *testing.T) {
	r, _, _ := captureFixture(t)
	s, err := r.NewCurrentCaptureStream(context.Background(), []string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	registration := newArinShadowRegistration("GOOGL-2", "NET-192-0-2-0-1", "PARENT-1", "reviewed-192.0.2.0/24")
	if !validArinShadowRegistration(registration) || registration.RuleSHA256 == "" {
		t.Fatal("public provenance missing")
	}
	s.addRegistration(arinShadowFacts{state: "subscriber", verified: true, registration: registration})
	s.addRegistration(arinShadowFacts{state: "excluded", risk: true, proxyRisk: true, registration: registration})
	s.addRegistration(arinShadowFacts{state: "ambiguous"})
	for i := 1; i < arinShadowRegistrationLimit; i++ {
		s.addRegistration(arinShadowFacts{state: "unknown", registration: newArinShadowRegistration(fmt.Sprintf("ORG-%d", i), "", "", "")})
	}
	s.addRegistration(arinShadowFacts{state: "subscriber", registration: newArinShadowRegistration("OVERFLOW", "", "", "")})
	// A known tuple still increments after the cardinality bound is reached.
	s.addRegistration(arinShadowFacts{state: "subscriber", registration: registration})
	s.finishRegistrations()
	if len(s.report.RegistrationCounts) != arinShadowRegistrationLimit || s.report.RegistrationCounts[0].Connections != 3 || s.report.RegistrationCounts[0].Subscriber != 2 || s.report.RegistrationCounts[0].Risk != 1 || s.report.RegistrationUnattributedConnections != 1 || s.report.RegistrationOverflowConnections != 1 {
		t.Fatal("aggregate counters or bound changed")
	}
	data, _ := json.Marshal(s.report)
	if strings.Contains(string(data), "192.0.2.0") || strings.Contains(string(data), "reviewed-") {
		t.Fatal("literal rule/prefix escaped")
	}
	for _, value := range []string{"192.0.2.1", "2001:db8::1", "lower-case", "ORG/SECRET", strings.Repeat("A", 65)} {
		if newArinShadowRegistration(value, "", "", "") != (ArinShadowRegistration{}) || validArinShadowRegistration(ArinShadowRegistration{OrgHandle: value}) {
			t.Fatal("unbounded/private handle accepted")
		}
	}
}

func TestArinRegistrationActualDecoderPreservesClassificationAndAmbiguity(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		record := mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(false), "org_handle": mmdbtype.String("DIRECT-1"), "net_handle": mmdbtype.String("NET-192-0-2-0-1"), "classification_org_handle": mmdbtype.String("PARENT-1"), "classification_rule": mmdbtype.String("reviewed-192.0.2.0/24"), "multiple_registration_owners": mmdbtype.Bool(multiple)}
		data := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record})
		db, err := mmdb.OpenBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		facts, _, err := shadowFacts(db, netip.MustParseAddr("192.0.2.42"))
		db.Close()
		if err != nil || !facts.verified || facts.state != "subscriber" || facts.risk {
			t.Fatal("attribution altered classification")
		}
		if multiple {
			if facts.registration != (ArinShadowRegistration{}) {
				t.Fatal("arbitrary incomparable owner attributed")
			}
		} else if facts.registration.OrgHandle != "DIRECT-1" || facts.registration.ClassificationOrgHandle != "PARENT-1" || facts.registration.NetHandle != "NET-192-0-2-0-1" || len(facts.registration.RuleSHA256) != 64 {
			t.Fatal("exact public registration decoding missing")
		}
	}
}

func TestArinCaptureMaximumRegistrationWireFits(t *testing.T) {
	now := time.Now().UTC()
	row := arinShadowCaptureWireRow{ConnectionId: NewId(), ClientId: NewId(), HandlerId: NewId(), ActualAt: now, ObservedAt: now, CapturedAt: now, State: "ambiguous", Reason: "qualified", Registration: newArinShadowRegistration(strings.Repeat("A", 64), strings.Repeat("N", 64), strings.Repeat("C", 64), strings.Repeat("R", 64))}
	row.Origin = newArinShadowOrigin([]uint32{4294967288, 4294967289, 4294967290, 4294967291, 4294967292, 4294967293, 4294967294, 4294967295}, "subscriber")
	reply := arinShadowCaptureRPCReply{Rows: make([]arinShadowCaptureWireRow, ArinShadowCaptureBatchLimit)}
	for i := range reply.Rows {
		reply.Rows[i] = row
	}
	data, err := json.Marshal(reply)
	// Leave a conservative 4KiB for the authenticated outer reply identity.
	if err != nil || len(data)+4096 > ArinShadowRPCResponseLimit {
		t.Fatalf("bounded full response exceeds cap: %d %v", len(data), err)
	}
}
