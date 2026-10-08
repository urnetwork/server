package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func currentCauseEvidenceRecord() mmdbtype.Map {
	return mmdbtype.Map{
		"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
		"quality_state": mmdbtype.String("unknown"), "non_quality": mmdbtype.Bool(true), "risk": mmdbtype.Bool(false),
		"origin_asns": mmdbtype.Slice{mmdbtype.Uint32(12345)}, "origin_use_state": mmdbtype.String("unknown"),
	}
}

func decodeCurrentCauseEvidence(t *testing.T, record mmdbtype.Map) (*ArinCurrentCause, error) {
	t.Helper()
	db, err := mmdb.OpenBytes(testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return currentArinCauseFromDatabase(db, netip.MustParseAddr("192.0.2.42"))
}

func TestArinCurrentCauseRiskCategoriesRemainDistinct(t *testing.T) {
	for _, category := range []string{"proxy", "residential_proxy", "virtual_isp", "vpn", "tor"} {
		t.Run(category, func(t *testing.T) {
			r := currentCauseEvidenceRecord()
			r["quality_state"], r["risk"] = mmdbtype.String("excluded"), mmdbtype.Bool(true)
			r["network_risk_evidence"] = mmdbtype.Slice{
				mmdbtype.Map{"category": mmdbtype.String(category)},
				mmdbtype.Map{"category": mmdbtype.String(category)},
				mmdbtype.Map{"category": mmdbtype.String("private-192.0.2.42-category")},
			}
			got, err := decodeCurrentCauseEvidence(t, r)
			if err != nil || !got.ProxyRisk || !slices.Equal(got.NetworkRiskCategories, []string{category}) || got.OtherNetworkRiskEvidence != 1 {
				t.Fatal("risk category was conflated, lost or invented", got, err)
			}
			encoded, _ := json.Marshal(got)
			if bytes.Contains(encoded, []byte("192.0.2.")) {
				t.Fatal("unknown risk text leaked")
			}
		})
	}
}

func TestArinCurrentCauseHostingMarkerAndOriginEvidence(t *testing.T) {
	r := currentCauseEvidenceRecord()
	r["quality_state"] = mmdbtype.String("excluded")
	r["hosting_prefix_source_ids"] = mmdbtype.Slice{mmdbtype.String("private-192.0.2.42-source")}
	r["origin_use_state"] = mmdbtype.String("withheld")
	r["origin_withheld_reason"] = mmdbtype.String("rpki-invalid-origin")
	r["origin_rpki_validity"], r["origin_peers"] = mmdbtype.String("invalid"), mmdbtype.Uint32(23)
	got, err := decodeCurrentCauseEvidence(t, r)
	if err != nil || !got.HostingPrefixEvidence || got.OriginRPKIValidity != "invalid" || !got.OriginVisibilityKnown || got.OriginVisibilityPeers != 23 || got.OriginWithheldReason != "rpki-invalid-origin" {
		t.Fatal("affirmative provenance lost", got, err)
	}
	encoded, _ := json.Marshal(got)
	if bytes.Contains(encoded, []byte("192.0.2.")) {
		t.Fatal("hosting source text leaked")
	}
	delete(r, "hosting_prefix_source_ids")
	delete(r, "origin_rpki_validity")
	delete(r, "origin_peers")
	delete(r, "origin_withheld_reason")
	r["origin_use_state"] = mmdbtype.String("unknown")
	got, err = decodeCurrentCauseEvidence(t, r)
	if err != nil || got.HostingPrefixEvidence || got.OriginRPKIValidity != "" || got.OriginVisibilityKnown || got.OriginVisibilityPeers != 0 {
		t.Fatal("absent evidence became a definite provenance claim", got, err)
	}
}

func TestArinCurrentCauseEvidenceBoundsAndClosedValues(t *testing.T) {
	for _, kind := range []string{"risk_overflow", "hosting_overflow", "hosting_empty", "rpki_unknown"} {
		t.Run(kind, func(t *testing.T) {
			r := currentCauseEvidenceRecord()
			switch kind {
			case "risk_overflow":
				var values mmdbtype.Slice
				for range 65 {
					values = append(values, mmdbtype.Map{"category": mmdbtype.String("proxy")})
				}
				r["network_risk_evidence"] = values
			case "hosting_overflow":
				var values mmdbtype.Slice
				for range 65 {
					values = append(values, mmdbtype.String("fixture"))
				}
				r["hosting_prefix_source_ids"] = values
			case "hosting_empty":
				r["hosting_prefix_source_ids"] = mmdbtype.Slice{mmdbtype.String("")}
			case "rpki_unknown":
				r["origin_rpki_validity"] = mmdbtype.String("private-192.0.2.42")
			}
			if _, err := decodeCurrentCauseEvidence(t, r); err == nil {
				t.Fatal("unbounded or unsupported evidence accepted")
			}
		})
	}
	_, _, _, cause, _ := currentCauseFixture(t)
	cause.Risk, cause.ProxyRisk = true, true
	for _, categories := range [][]string{{"vpn", "proxy"}, {"proxy", "proxy"}, {"arbitrary"}} {
		cause.NetworkRiskCategories = categories
		if validArinCurrentCause(*cause) {
			t.Fatal("noncanonical category set accepted")
		}
	}
}

func TestArinCurrentCauseMixedOriginsAreNotCollapsed(t *testing.T) {
	r := currentCauseEvidenceRecord()
	r["quality_state"], r["origin_use_state"] = mmdbtype.String("ambiguous"), mmdbtype.String("ambiguous")
	r["origin_asns"] = mmdbtype.Slice{mmdbtype.Uint32(12345), mmdbtype.Uint32(23457)}
	r["origin_rpki_validity"] = mmdbtype.String("not-found")
	got, err := decodeCurrentCauseEvidence(t, r)
	if err != nil || got.State != "ambiguous" || got.Origin.UseState != "ambiguous" || !slices.Equal(got.Origin.ASNs, []uint32{12345, 23457}) || got.OriginRPKIValidity != "not-found" {
		t.Fatal("mixed origins collapsed or inferred clean", got, err)
	}
}

func TestArinCurrentCauseAddressFamiliesPreserveClassification(t *testing.T) {
	r := currentCauseEvidenceRecord()
	r["origin_use_state"] = mmdbtype.String("withheld")
	r["origin_withheld_reason"] = mmdbtype.String("rpki-invalid-origin")
	r["origin_rpki_validity"] = mmdbtype.String("invalid")
	db, err := mmdb.OpenBytes(testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{
		"192.0.2.0/24": r, "2001:db8::/32": r,
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reply := ArinCurrentCauseReply{ExpectedEpoch: db.Metadata.BuildTime().Unix()}
	for _, sample := range []struct{ address, family string }{
		{"192.0.2.42", "ipv4"}, {"::ffff:192.0.2.42", "ipv4"}, {"2001:db8::42", "ipv6"},
	} {
		cause, err := currentArinCauseFromDatabase(db, netip.MustParseAddr(sample.address))
		if err != nil || cause.AddressFamily != sample.family || cause.State != "unknown" || !cause.NonQuality || cause.Verified || cause.Risk ||
			cause.OriginWithheldReason != "rpki-invalid-origin" || cause.OriginRPKIValidity != "invalid" {
			t.Fatal("address family changed classifier state or origin evidence", sample.family, cause, err)
		}
		reply.Rows = append(reply.Rows, ArinCurrentCauseRow{ConnectionId: NewId(), Reason: "qualified", Cause: cause})
	}
	report, err := AggregateArinCurrentCauses(reply)
	if err != nil || report.RequestedConnections != 3 || report.QualifiedConnections != 3 || len(report.Causes) != 2 {
		t.Fatal("family partition was lost", report, err)
	}
	counts := map[string]int{}
	for _, row := range report.Causes {
		counts[row.Cause.AddressFamily] += row.Connections
	}
	if counts["ipv4"] != 2 || counts["ipv6"] != 1 {
		t.Fatal("mapped IPv4 or IPv6 family was misreported", counts)
	}
	encoded, _ := json.Marshal(report)
	for _, private := range []string{"192.0.2.42", "2001:db8::42", "connection_id", "client_id", "handler_id"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("address-family diagnostic leaked private data")
		}
	}
	for _, address := range []netip.Addr{{}, netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("::"),
		netip.MustParseAddr("::ffff:0.0.0.0"), netip.MustParseAddr("fe80::1%fixture"),
		netip.MustParseAddr("::ffff:192.0.2.42%fixture")} {
		if _, err := currentArinCauseFromDatabase(db, address); err == nil {
			t.Fatal("unsupported connection address accepted")
		}
	}
}

func TestArinCurrentCauseAddressFamilyCannotMismatchOwner(t *testing.T) {
	request, owner, fact, cause, now := currentCauseFixture(t)
	for _, family := range []string{"", "v4", "unknown", "192.0.2.42"} {
		cause.AddressFamily = family
		if validArinCurrentCause(*cause) {
			t.Fatal("unclosed address family accepted")
		}
	}
	cause.AddressFamily = "ipv6"
	owners := func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
		return []ArinShadowCaptureTarget{{request.Connections[0], owner}}, nil
	}
	facts := func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
		return []ArinShadowCaptureFacts{fact}, nil
	}
	lookup := func(netip.Addr) (*ArinCurrentCause, error) { return cause, nil }
	reply, err := readCurrentArinCauses(t.Context(), request, owners, facts, lookup, func() time.Time { return now })
	if err != nil || len(reply.Rows) != 1 || reply.Rows[0].Reason != "lookup_unavailable" || reply.Rows[0].Cause != nil {
		t.Fatal("cause from a different address family was qualified", reply, err)
	}
}
