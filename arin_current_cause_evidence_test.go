package server

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

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
