package main

import (
	"bytes"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// RIS can report one reviewed ISP's sibling ASNs on the same prefix with very
// different peer counts. The less visible sibling is not a competing identity.
func TestSubscriberOriginVisibilityUsesReviewedIdentity(t *testing.T) {
	byASN := subscriberFixtureByASN()
	byASN[64509] = byASN[64500]
	byASN[64510] = []subscriberOperator{{Id: "other-access", Usage: "subscriber", Source: "https://evidence.example/other"}}
	byASN[64511] = byASN[64510]
	byASN[64506] = append(append([]subscriberOperator{}, byASN[64500]...), byASN[64510]...)
	byASN[64507] = []subscriberOperator{byASN[64510][0], byASN[64500][0]}
	for _, tc := range []struct {
		name, rows, state, reason string
		peers                     uint32
	}{
		{"same reviewed ISP", "64500 192.0.2.0/24 334\n64509 192.0.2.0/24 1\n", "subscriber", "", 334},
		{"same ISP reversed input", "64509 192.0.2.0/24 1\n64500 192.0.2.0/24 334\n", "subscriber", "", 334},
		{"independent ISP remains weak", "64500 192.0.2.0/24 334\n64510 192.0.2.0/24 1\n", "withheld", "insufficient-origin-visibility", 1},
		{"two well observed identities", "64500 192.0.2.0/24 334\n64509 192.0.2.0/24 1\n64510 192.0.2.0/24 20\n64511 192.0.2.0/24 2\n", "subscriber", "", 20},
		{"independent weak identity among siblings", "64500 192.0.2.0/24 334\n64509 192.0.2.0/24 1\n64510 192.0.2.0/24 2\n", "withheld", "insufficient-origin-visibility", 2},
		{"peer observations are not summed", "64500 192.0.2.0/24 6\n64509 192.0.2.0/24 5\n", "withheld", "insufficient-origin-visibility", 6},
		{"unknown competing origin stays ambiguous", "64500 192.0.2.0/24 334\n64509 192.0.2.0/24 1\n64501 192.0.2.0/24 2\n", "ambiguous", "", 1},
		{"explicit hosting remains a veto", "64500 192.0.2.0/24 334\n64509 192.0.2.0/24 1\n64503 192.0.2.0/24 2\n", "excluded", "", 1},
		{"explicit virtual use remains a veto", "64500 192.0.2.0/24 334\n64509 192.0.2.0/24 1\n64502 192.0.2.0/24 2\n", "excluded", "", 1},
		{"identical reviewed identity sets", "64506 192.0.2.0/24 334\n64507 192.0.2.0/24 1\n", "subscriber", "", 334},
		{"partially overlapping identities stay distinct", "64500 192.0.2.0/24 334\n64506 192.0.2.0/24 1\n", "withheld", "insufficient-origin-visibility", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2026, 10, 4, 18, 10, 0, 0, time.UTC)
			routes := map[netip.Prefix]subscriberOriginRoute{}
			data := subscriberFixtureGzip(t, at, tc.rows)
			if _, _, err := readSubscriberOrigins(t.Context(), bytes.NewReader(data), at, routes); err != nil {
				t.Fatal(err)
			}
			prefix := netip.MustParsePrefix("192.0.2.0/24")
			before := routes[prefix]
			before.origins = append([]subscriberOrigin{}, before.origins...)
			if _, err := resolveSubscriberOriginEvidence(t.Context(), routes, nil, byASN); err != nil {
				t.Fatal(err)
			}
			route := routes[prefix]
			if route.visibility != tc.peers {
				t.Errorf("effective identity visibility=%d, want=%d", route.visibility, tc.peers)
			}
			if !reflect.DeepEqual(route.origins, before.origins) || route.ownVisibility() != before.ownVisibility() {
				t.Fatal("effective visibility mutated raw ASN observations")
			}
			decision := subscriberOriginDecision(route, byASN, defaultMinimumOriginPeers)
			if decision["state"] != mmdbtype.String(tc.state) {
				t.Errorf("decision=%+v, want state=%s", decision, tc.state)
			}
			reason, _ := decision["withheld_reason"].(mmdbtype.String)
			if string(reason) != tc.reason {
				t.Errorf("withheld reason=%q, want=%q", reason, tc.reason)
			}
			got, err := augmentSubscriberRecord(subscriberFixtureRecord("unknown", false), decision)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.state
			if want == "withheld" {
				want = "unknown"
			}
			if got["quality_state"] != mmdbtype.String(want) {
				t.Errorf("augmented state=%v, want=%s", got["quality_state"], want)
			}
			if tc.name == "explicit virtual use remains a veto" && got["network_risk"] != mmdbtype.Bool(true) {
				t.Fatal("virtual ISP discriminator lost network risk")
			}
		})
	}
}

func TestSubscriberOriginSiblingVisibilityRetainsRPKIAndRegistrationVetoes(t *testing.T) {
	byASN := subscriberFixtureByASN()
	byASN[64509] = byASN[64500]
	prefix := netip.MustParsePrefix("192.0.2.0/24")
	for _, authorizeSibling := range []bool{false, true} {
		route := subscriberFixtureRoute(334, 64500)
		route.addOrigin(64509, 1)
		routes := map[netip.Prefix]subscriberOriginRoute{prefix: route}
		authorizations := &rpkiAuthorizations{}
		if err := authorizations.add(64500, prefix, 24); err != nil {
			t.Fatal(err)
		}
		if authorizeSibling {
			if err := authorizations.add(64509, prefix, 24); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := resolveSubscriberOriginEvidence(t.Context(), routes, authorizations, byASN); err != nil {
			t.Fatal(err)
		}
		route = routes[prefix]
		if route.visibility != 334 {
			t.Errorf("effective visibility=%d, want=334", route.visibility)
		}
		decision := subscriberOriginDecision(route, byASN, defaultMinimumOriginPeers)
		if !authorizeSibling {
			if route.rpki != "invalid" || decision["state"] != mmdbtype.String("withheld") || decision["withheld_reason"] != mmdbtype.String("rpki-invalid-origin") {
				t.Errorf("unexpected unauthorized sibling decision: %+v", decision)
			}
		} else if route.rpki != "valid" || decision["state"] != mmdbtype.String("subscriber") {
			t.Errorf("authorized visible identity was withheld: %+v", decision)
		}
		for _, prior := range []string{"excluded", "ambiguous"} {
			got, err := augmentSubscriberRecord(subscriberFixtureRecord(prior, true), decision)
			if err != nil || got["quality_state"] != mmdbtype.String(prior) || got["risk"] != mmdbtype.Bool(true) {
				t.Fatalf("effective visibility lost prior %s or risk: %+v, %v", prior, got, err)
			}
		}
	}
}
