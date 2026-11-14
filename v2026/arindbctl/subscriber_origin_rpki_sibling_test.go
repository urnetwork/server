package main

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// A reviewed operator identity can share peer visibility, but a sibling ASN
// still needs its own authorization. The aggregate max-length exception is
// limited to identical complete origin sets.
func TestSubscriberOriginAggregateRPKIScope(t *testing.T) {
	for _, tc := range []struct {
		name             string
		childASNs        []uint32
		parentMaxLength  int
		authorizeSibling bool
		wantRawRPKI      string
		wantRPKI         string
		wantOriginState  string
		wantQualityState string
		wantWithheld     string
	}{
		{
			name:      "unauthorized sibling keeps invalid origin",
			childASNs: []uint32{64509}, parentMaxLength: 25,
			wantRawRPKI: "invalid", wantRPKI: "invalid",
			wantOriginState: "withheld", wantQualityState: "unknown",
			wantWithheld: "rpki-invalid-origin",
		},
		{
			name:      "unauthorized sibling added to child set keeps invalid origin",
			childASNs: []uint32{64500, 64509}, parentMaxLength: 25,
			wantRawRPKI: "invalid", wantRPKI: "invalid",
			wantOriginState: "withheld", wantQualityState: "unknown",
			wantWithheld: "rpki-invalid-origin",
		},
		{
			name:      "same ASN retains aggregate max length exception",
			childASNs: []uint32{64500}, parentMaxLength: 24,
			wantRawRPKI: "invalid", wantRPKI: "valid-aggregate",
			wantOriginState: "subscriber", wantQualityState: "subscriber",
		},
		{
			name:      "authorized sibling retains aggregate visibility",
			childASNs: []uint32{64509}, parentMaxLength: 25, authorizeSibling: true,
			wantRawRPKI: "valid", wantRPKI: "valid",
			wantOriginState: "subscriber", wantQualityState: "subscriber",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := netip.MustParsePrefix("192.0.2.0/24")
			child := netip.MustParsePrefix("192.0.2.0/25")
			byASN := subscriberFixtureByASN()
			byASN[64509] = byASN[64500]
			routes := map[netip.Prefix]subscriberOriginRoute{
				parent: subscriberFixtureRoute(300, 64500),
				child:  subscriberFixtureRoute(1, tc.childASNs...),
			}
			authorizations := &rpkiAuthorizations{}
			if err := authorizations.add(64500, parent, tc.parentMaxLength); err != nil {
				t.Fatal(err)
			}
			if tc.authorizeSibling {
				if err := authorizations.add(64509, parent, 25); err != nil {
					t.Fatal(err)
				}
			}
			if got := authorizations.routeValidity(parent, routes[parent]); got != "valid" {
				t.Fatalf("parent authorization=%s, want valid", got)
			}
			if got := authorizations.routeValidity(child, routes[child]); got != tc.wantRawRPKI {
				t.Fatalf("raw child authorization=%s, want %s", got, tc.wantRawRPKI)
			}
			before := slices.Clone(routes[child].origins)
			if _, err := resolveSubscriberOriginEvidence(t.Context(), routes, authorizations, byASN); err != nil {
				t.Fatal(err)
			}
			route := routes[child]
			if route.rpki != tc.wantRPKI || route.visibility != 300 || !slices.Equal(route.origins, before) {
				t.Fatalf("resolved child=%+v, want RPKI=%s, inherited visibility=300 and original observations", route, tc.wantRPKI)
			}
			decision := subscriberOriginDecision(route, byASN, defaultMinimumOriginPeers)
			if decision["state"] != mmdbtype.String(tc.wantOriginState) {
				t.Fatalf("origin decision=%+v, want state=%s", decision, tc.wantOriginState)
			}
			got, err := augmentSubscriberRecord(subscriberFixtureRecord("unknown", false), decision)
			if err != nil {
				t.Fatal(err)
			}
			withheld, _ := got["origin_withheld_reason"].(mmdbtype.String)
			if got["quality_state"] != mmdbtype.String(tc.wantQualityState) || got["non_quality"] != mmdbtype.Bool(tc.wantQualityState != "subscriber") || got["risk"] != mmdbtype.Bool(false) || string(withheld) != tc.wantWithheld {
				t.Fatalf("augmented record=%+v, want quality=%s and withheld=%s", got, tc.wantQualityState, tc.wantWithheld)
			}
			if tc.wantWithheld != "" && got["subscriber_evidence_kind"] != nil {
				t.Fatal("unauthorized sibling retained an inferred subscriber approval")
			}
		})
	}
}
