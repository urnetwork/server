// Offline candidate inspection reports aggregates without retaining identities.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"golang.org/x/text/language"
)

// Opt-in offline release attestation uses the actual runtime decoder. Output is
// aggregate classification evidence only: no addresses, provider IDs, or names.
func TestArinCandidateReadback(t *testing.T) {
	directory := os.Getenv("ARIN_CANDIDATE_PATH")
	if directory == "" {
		t.Skip("set ARIN_CANDIDATE_PATH to an unpublished candidate directory")
	}
	manifestBytes, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ClassifierVersion    uint32            `json:"classifier_version"`
		QualityPolicyVersion uint32            `json:"quality_policy_version"`
		CountryPolicyVersion uint32            `json:"country_policy_version"`
		InputHashes          map[string]string `json:"inputs_sha256"`
		Hashes               map[string]string `json:"sha256"`
		BuiltAt              time.Time         `json:"built_at"`
		CountrySources       []struct {
			Id         string    `json:"id"`
			Sha256     string    `json:"sha256"`
			ObservedAt time.Time `json:"observed_at"`
			ExpiresAt  time.Time `json:"expires_at"`
		} `json:"country_evidence_sources"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ClassifierVersion != 1 || len(manifest.InputHashes) != 3+len(manifest.CountrySources) {
		t.Fatal("candidate has no complete version-one input attestation")
	}
	if manifest.CountryPolicyVersion != 0 && manifest.CountryPolicyVersion != 2 ||
		(manifest.CountryPolicyVersion == 2) != (len(manifest.CountrySources) > 0) {
		t.Fatal("candidate has inconsistent country-policy input attestation")
	}
	countrySourceIdBools := map[string]bool{}
	for _, source := range manifest.CountrySources {
		if source.Id == "" || countrySourceIdBools[source.Id] || source.ObservedAt.IsZero() || source.ObservedAt.After(manifest.BuiltAt) || !manifest.BuiltAt.Before(source.ExpiresAt) {
			t.Fatal("candidate country source is duplicated, future-dated or expired at build time")
		}
		digest, err := hex.DecodeString(source.Sha256)
		if err != nil || len(digest) != sha256.Size || manifest.InputHashes["country_evidence/"+source.Id] != source.Sha256 {
			t.Fatal("candidate country source has no matching input digest")
		}
		countrySourceIdBools[source.Id] = true
	}
	for _, key := range []string{"arin_xml", "geolite2", "classification_rules"} {
		hash, err := hex.DecodeString(manifest.InputHashes[key])
		if err != nil || len(hash) != sha256.Size {
			t.Fatal("candidate has a malformed input hash")
		}
	}
	path := filepath.Join(directory, "arin.mmdb")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, file)
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != manifest.Hashes["arin.mmdb"] {
		t.Fatal("candidate output hash does not match its manifest")
	}
	db, err := mmdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Verify(); err != nil {
		t.Fatal(err)
	}
	if db.Metadata.DatabaseType != string(schemaTypeArinDb) || db.Metadata.BuildTime().Unix() <= 0 {
		t.Fatal("candidate database metadata is not runtime-compatible")
	}
	type count struct {
		Prefixes   int64 `json:"prefixes"`
		Risk       int64 `json:"risk"`
		NonQuality int64 `json:"non_quality"`
	}
	counts := struct {
		BuildEpoch               int64            `json:"build_epoch"`
		ReviewedAccessPrefixes   int64            `json:"reviewed_access_prefixes"`
		MultipleOwnerPrefixes    int64            `json:"multiple_owner_prefixes"`
		CountryAmbiguousPrefixes int64            `json:"country_ambiguous_prefixes"`
		QualityAmbiguousPrefixes int64            `json:"non_quality_ambiguous_prefixes"`
		All                      count            `json:"all"`
		ByRule                   map[string]count `json:"by_rule"`
		ByCountry                map[string]count `json:"by_associated_country"`
		ByScope                  map[string]count `json:"by_registration_scope"`
		ByCountryEvidence        map[string]count `json:"by_country_evidence_state"`
		ByQualityState           map[string]count `json:"by_quality_state"`
	}{BuildEpoch: db.Metadata.BuildTime().Unix(), ByRule: map[string]count{}, ByCountry: map[string]count{}, ByScope: map[string]count{}, ByCountryEvidence: map[string]count{}, ByQualityState: map[string]count{}}
	add := func(value count, info *ArinInfo) count {
		value.Prefixes++
		if info.Risk {
			value.Risk++
		}
		if info.NonQuality {
			value.NonQuality++
		}
		return value
	}
	for result := range db.Networks() {
		type countryOwner struct {
			Net string `maxminddb:"net_handle"`
			Org string `maxminddb:"org_handle"`
		}
		var record struct {
			Net                 string `maxminddb:"net_handle"`
			Org                 string `maxminddb:"org_handle"`
			RegisteredCountry   string `maxminddb:"registered_country"`
			AssociatedCountry   string `maxminddb:"associated_country"`
			Scope               string `maxminddb:"registration_scope"`
			Rule                string `maxminddb:"classification_rule"`
			Source              string `maxminddb:"classification_source"`
			Reason              string `maxminddb:"reason"`
			MultipleOwners      bool   `maxminddb:"multiple_registration_owners"`
			CountryAmbiguous    bool   `maxminddb:"country_ambiguous"`
			QualityAmbiguous    bool   `maxminddb:"non_quality_ambiguous"`
			NetworkRisk         bool   `maxminddb:"network_risk"`
			GeographicRisk      bool   `maxminddb:"geographic_risk"`
			NetworkRiskEvidence []struct {
				Rule     string `maxminddb:"rule"`
				Category string `maxminddb:"category"`
				Source   string `maxminddb:"source"`
				Reason   string `maxminddb:"reason"`
			} `maxminddb:"network_risk_evidence"`
			RegistrationMismatch bool     `maxminddb:"registration_mismatch"`
			CountryPolicyVersion uint32   `maxminddb:"country_policy_version"`
			CountryEvidenceState string   `maxminddb:"country_evidence_state"`
			CredibleCountries    []string `maxminddb:"credible_country_codes"`
			CountryEvidence      []struct {
				Rule        string         `maxminddb:"rule"`
				Prefix      string         `maxminddb:"prefix"`
				SourceId    string         `maxminddb:"source_id"`
				Reason      string         `maxminddb:"reason"`
				Countries   []string       `maxminddb:"country_codes"`
				Uncertainty string         `maxminddb:"uncertainty"`
				Owners      []countryOwner `maxminddb:"owners"`
			} `maxminddb:"country_evidence"`
			Owners []struct {
				Net          string `maxminddb:"net_handle"`
				Org          string `maxminddb:"org_handle"`
				Country      string `maxminddb:"registered_country"`
				NonQuality   bool   `maxminddb:"non_quality"`
				QualityState string `maxminddb:"quality_state"`
			} `maxminddb:"owner_evidence"`
		}
		if err := result.Decode(&record); err != nil {
			t.Fatal("candidate record cannot be decoded")
		}
		info, err := getArinInfoFromDatabase(db, schemaTypeArinDb, result.Prefix().Addr())
		if err != nil {
			t.Fatal("candidate record is rejected by the runtime reader")
		}
		if info.ClassifierVersion != 1 || info.DatabaseBuildEpoch != counts.BuildEpoch {
			t.Fatal("candidate record has a missing or incompatible version/generation")
		}
		wantRisk := record.Scope == "arin" && record.RegisteredCountry != "" && record.AssociatedCountry != "" && record.RegisteredCountry != record.AssociatedCountry
		if record.CountryPolicyVersion == 2 {
			if manifest.CountryPolicyVersion != 2 || record.Scope != "arin" || len(record.CountryEvidence) == 0 || record.RegistrationMismatch != wantRisk {
				t.Fatal("candidate country refinement lost its authority or raw registration evidence")
			}
			owners := []countryOwner{{Net: record.Net, Org: record.Org}}
			if record.MultipleOwners {
				owners = nil
				for _, owner := range record.Owners {
					owners = append(owners, countryOwner{Net: owner.Net, Org: owner.Org})
				}
			}
			first := record.CountryEvidence[0]
			firstPrefix, err := netip.ParsePrefix(first.Prefix)
			if err != nil {
				t.Fatal("candidate country evidence has an invalid prefix")
			}
			state := "known"
			countries := first.Countries
			if first.Uncertainty != "" {
				state = "unknown"
			}
			for _, evidence := range record.CountryEvidence {
				prefix, err := netip.ParsePrefix(evidence.Prefix)
				if err != nil || prefix != prefix.Masked() || prefix != firstPrefix || prefix.Bits() > result.Prefix().Bits() || !prefix.Contains(result.Prefix().Addr()) ||
					evidence.Rule == "" || evidence.Reason == "" || !countrySourceIdBools[evidence.SourceId] || len(evidence.Owners) != len(owners) ||
					(len(evidence.Countries) == 0) == (evidence.Uncertainty == "") {
					t.Fatal("candidate country refinement lacks scoped reviewed evidence")
				}
				if !slices.IsSorted(evidence.Countries) || len(slices.Compact(slices.Clone(evidence.Countries))) != len(evidence.Countries) {
					t.Fatal("candidate country set is not canonical")
				}
				for _, country := range evidence.Countries {
					region, err := language.ParseRegion(country)
					if err != nil || len(country) != 2 || !region.IsCountry() || strings.ToLower(region.String()) != country {
						t.Fatal("candidate country set contains an unknown country")
					}
				}
				for _, owner := range owners {
					if owner.Net == "" || owner.Org == "" || !slices.Contains(evidence.Owners, owner) {
						t.Fatal("candidate country refinement does not bind every direct owner")
					}
				}
				if (evidence.Uncertainty == "") != (first.Uncertainty == "") || !slices.Equal(evidence.Countries, first.Countries) {
					state, countries = "ambiguous", nil
				}
			}
			if record.CountryEvidenceState != state || !slices.Equal(record.CredibleCountries, countries) {
				t.Fatal("candidate country refinement substituted a country set for conflicting evidence")
			}
			region, regionErr := language.ParseRegion(record.AssociatedCountry)
			countryKnown := regionErr == nil && len(record.AssociatedCountry) == 2 && region.IsCountry() && strings.EqualFold(region.String(), record.AssociatedCountry)
			wantRisk = state == "known" && countryKnown && !slices.Contains(countries, record.AssociatedCountry)
		} else if record.CountryPolicyVersion != 0 || record.CountryEvidenceState != "" || len(record.CountryEvidence) > 0 || len(record.CredibleCountries) > 0 {
			t.Fatal("candidate uses unsupported or unversioned country evidence")
		}
		if info.QualityPolicyVersion != manifest.QualityPolicyVersion {
			t.Fatal("candidate record and manifest disagree on subscriber policy")
		}
		if record.NetworkRisk != (len(record.NetworkRiskEvidence) != 0) {
			t.Fatal("network risk has no matching reviewed evidence")
		}
		for _, evidence := range record.NetworkRiskEvidence {
			if evidence.Rule == "" || evidence.Source == "" || evidence.Reason == "" || !slices.Contains([]string{"virtual_isp", "proxy", "vpn", "tor"}, evidence.Category) {
				t.Fatal("candidate network risk has invalid attribution")
			}
		}
		if info.QualityPolicyVersion == 2 && record.GeographicRisk != wantRisk {
			t.Fatal("candidate mixed geographic and network-use risk provenance")
		}
		wantRisk = wantRisk || record.NetworkRisk
		if info.Risk != wantRisk {
			t.Fatal("candidate risk disagrees with its authoritative country evidence")
		}
		if record.MultipleOwners != (len(record.Owners) > 1) {
			t.Fatal("candidate discarded its incomparable ownership evidence")
		}
		if record.MultipleOwners {
			counts.MultipleOwnerPrefixes++
			country := record.Owners[0].Country
			nonQuality := record.Owners[0].NonQuality
			countryAmbiguous, qualityAmbiguous := false, record.Owners[0].QualityState == "ambiguous"
			for _, owner := range record.Owners[1:] {
				countryAmbiguous = countryAmbiguous || owner.Country != country
				qualityAmbiguous = qualityAmbiguous || owner.NonQuality != nonQuality || owner.QualityState != record.Owners[0].QualityState
			}
			if countryAmbiguous {
				country = ""
			}
			if qualityAmbiguous {
				nonQuality = info.QualityPolicyVersion == 2
			}
			if record.RegisteredCountry != country || info.NonQuality != nonQuality || record.CountryAmbiguous != countryAmbiguous || record.QualityAmbiguous != qualityAmbiguous {
				t.Fatal("candidate substituted an arbitrary owner for fact consensus")
			}
		}
		if record.CountryAmbiguous {
			counts.CountryAmbiguousPrefixes++
		}
		if record.QualityAmbiguous {
			counts.QualityAmbiguousPrefixes++
		}
		if info.NonQuality && info.QualityState != "unknown" && info.QualityState != "ambiguous" && (record.Rule == "" || record.Source == "" || record.Reason == "") {
			t.Fatal("candidate network-use exclusion lacks reviewed rule provenance")
		}
		if record.Rule != "" && !info.NonQuality {
			counts.ReviewedAccessPrefixes++
		}
		counts.ByQualityState[info.QualityState] = add(counts.ByQualityState[info.QualityState], info)
		counts.All = add(counts.All, info)
		counts.ByRule[record.Rule] = add(counts.ByRule[record.Rule], info)
		counts.ByCountry[record.AssociatedCountry] = add(counts.ByCountry[record.AssociatedCountry], info)
		counts.ByScope[record.Scope] = add(counts.ByScope[record.Scope], info)
		counts.ByCountryEvidence[record.CountryEvidenceState] = add(counts.ByCountryEvidence[record.CountryEvidenceState], info)
	}
	if counts.All.Prefixes == 0 || counts.All.NonQuality == 0 || counts.ReviewedAccessPrefixes == 0 {
		t.Fatal("candidate lacks real hosting records or the expected consumer control")
	}
	encoded, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("candidate_classification_aggregates=%s", encoded)
}
