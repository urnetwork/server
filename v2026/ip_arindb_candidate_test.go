// Offline candidate inspection reports aggregates without retaining identities.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// The builders stamp their manifests with these sources: arindbctl/build.go
// for the registration database, and arindbctl/subscriber_origin.go for the
// subscriber augmentation that `arindbctl update` publishes on top of it.
const (
	arinCandidateRegistrationSource = "ARIN bulk Whois"
	arinCandidateAugmentedSource    = "reviewed subscriber operators and RIPE RIS origins"
)

// The augmentation cannot run without its registration base, the reviewed
// catalog and both RIS origin snapshots (arindbctl/update.go). Every other
// evidence source is best effort, keyed "<role>/<source id>", so the input
// count varies with upstream availability.
var (
	arinCandidateAugmentedRequiredInputs = []string{"base_arin_mmdb", "subscriber_catalog", "origin/ris-ipv4", "origin/ris-ipv6"}
	arinCandidateAugmentedEvidenceRoles  = []string{"origin", "address_risk", "rpki", "registry", "label", "hosting_prefix", "registry_assignment"}
)

type arinCandidateManifest struct {
	Source               string            `json:"source"`
	ClassifierVersion    uint32            `json:"classifier_version"`
	QualityPolicyVersion uint32            `json:"quality_policy_version"`
	CountryPolicyVersion uint32            `json:"country_policy_version"`
	OriginCountryPolicy  string            `json:"origin_country_policy"`
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

func arinCandidateDigest(value string) bool {
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size
}

// Chooses the attestation by the builder that wrote the manifest and returns
// the registration manifest, which attests the country evidence records carry.
// An augmented resource ships its base's as registration-manifest.json.
func attestArinCandidateManifest(manifest arinCandidateManifest, readRegistration func() ([]byte, error)) (arinCandidateManifest, error) {
	switch manifest.Source {
	case arinCandidateRegistrationSource:
		return manifest, attestArinCandidateRegistration(manifest)
	case arinCandidateAugmentedSource:
		data, err := readRegistration()
		if err != nil {
			return arinCandidateManifest{}, errors.New("candidate augmentation has no registration manifest")
		}
		var registration arinCandidateManifest
		if err := json.Unmarshal(data, &registration); err != nil {
			return arinCandidateManifest{}, errors.New("candidate registration manifest is unreadable")
		}
		return registration, attestArinCandidateAugmentation(manifest, registration)
	}
	return arinCandidateManifest{}, errors.New("candidate manifest is not from a known builder")
}

// The registration build attests exactly its ARIN source, GeoLite2, the
// reviewed rules and each country evidence source.
func attestArinCandidateRegistration(manifest arinCandidateManifest) error {
	if manifest.Source != arinCandidateRegistrationSource || manifest.ClassifierVersion != 1 || len(manifest.InputHashes) != 3+len(manifest.CountrySources) {
		return errors.New("candidate has no complete version-one input attestation")
	}
	if manifest.CountryPolicyVersion != 0 && manifest.CountryPolicyVersion != 2 ||
		(manifest.CountryPolicyVersion == 2) != (len(manifest.CountrySources) > 0) {
		return errors.New("candidate has inconsistent country-policy input attestation")
	}
	seen := map[string]bool{}
	for _, source := range manifest.CountrySources {
		if source.Id == "" || seen[source.Id] || source.ObservedAt.IsZero() || source.ObservedAt.After(manifest.BuiltAt) || !manifest.BuiltAt.Before(source.ExpiresAt) {
			return errors.New("candidate country source is duplicated, future-dated or expired at build time")
		}
		if !arinCandidateDigest(source.Sha256) || manifest.InputHashes["country_evidence/"+source.Id] != source.Sha256 {
			return errors.New("candidate country source has no matching input digest")
		}
		seen[source.Id] = true
	}
	for _, key := range []string{"arin_xml", "geolite2", "classification_rules"} {
		if !arinCandidateDigest(manifest.InputHashes[key]) {
			return errors.New("candidate has a malformed input hash")
		}
	}
	return nil
}

// The augmentation attests its required inputs and only known best-effort
// roles, and binds itself to the exact registration database it augmented.
func attestArinCandidateAugmentation(manifest, registration arinCandidateManifest) error {
	if manifest.ClassifierVersion != 1 || manifest.QualityPolicyVersion != 2 || manifest.CountryPolicyVersion != 0 || len(manifest.CountrySources) != 0 {
		return errors.New("candidate augmentation is not version-one policy-two")
	}
	for _, key := range arinCandidateAugmentedRequiredInputs {
		if !arinCandidateDigest(manifest.InputHashes[key]) {
			return errors.New("candidate augmentation lacks a required input attestation")
		}
	}
	for key, digest := range manifest.InputHashes {
		role, id, sourced := strings.Cut(key, "/")
		known := key == "base_arin_mmdb" || key == "subscriber_catalog" || key == "geolite2" ||
			sourced && id != "" && !strings.ContainsAny(id, "/\\") && slices.Contains(arinCandidateAugmentedEvidenceRoles, role)
		if !known || !arinCandidateDigest(digest) {
			return errors.New("candidate augmentation attests an unknown or malformed input")
		}
	}
	if (manifest.OriginCountryPolicy != "") != (manifest.InputHashes["geolite2"] != "") {
		return errors.New("candidate origin country policy and its GeoLite2 input are not attested together")
	}
	if err := attestArinCandidateRegistration(registration); err != nil {
		return err
	}
	if registration.QualityPolicyVersion != 2 || registration.Hashes["arin.mmdb"] != manifest.InputHashes["base_arin_mmdb"] {
		return errors.New("candidate augmentation is not bound to its policy-two registration base")
	}
	return nil
}

// What a record's quality decision may cite. A registration rule carries its
// own rule and source. The augmentation's hosting-prefix and address-risk
// layers cite attested sources instead, and its origin layer cites reviewed
// operators, including for an inferred subscriber.
type arinCandidateProvenance struct {
	Rule, Source, Reason                   string
	HostingSourceIds, AddressRiskSourceIds []string
	OriginUseState, OriginSource           string
	OriginOperatorIds                      []string
	EvidenceKind                           string
}

func (p arinCandidateProvenance) attestsExclusion() bool {
	return p.Reason != "" && (p.Rule != "" && p.Source != "" || len(p.HostingSourceIds) > 0 || len(p.AddressRiskSourceIds) > 0 ||
		p.OriginUseState == "excluded" && p.OriginSource != "" && len(p.OriginOperatorIds) > 0)
}

// Every cited source must be an input the manifest attests. A registration
// manifest attests no augmentation source and no origin evidence.
func (p arinCandidateProvenance) citesOnlyAttested(manifest arinCandidateManifest) bool {
	for role, ids := range map[string][]string{"hosting_prefix": p.HostingSourceIds, "address_risk": p.AddressRiskSourceIds} {
		for _, id := range ids {
			if id == "" || manifest.InputHashes[role+"/"+id] == "" {
				return false
			}
		}
	}
	return p.OriginUseState == "" || manifest.Source == arinCandidateAugmentedSource
}

// The augmentation may replace the decision that agreeing direct owners
// reached (arindbctl augmentSubscriberRecord, hostingPrefixRecord and
// addressRiskRecord): reviewed origin evidence infers a subscriber over
// unknown owners, and origin, hosting-prefix or address-risk evidence excludes
// or conflicts with unknown or subscriber owners. An agreed exclusion stands.
func (p arinCandidateProvenance) replacesOwnerDecision(agreed string) bool {
	inferred := agreed == "unknown" && p.OriginUseState == "subscriber" && p.EvidenceKind == "isp_inferred"
	contrary := p.OriginUseState == "excluded" || p.OriginUseState == "ambiguous" || len(p.HostingSourceIds) > 0 || len(p.AddressRiskSourceIds) > 0
	return inferred || contrary && (agreed == "unknown" || agreed == "subscriber")
}

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
	var manifest arinCandidateManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	registration, err := attestArinCandidateManifest(manifest, func() ([]byte, error) {
		return os.ReadFile(filepath.Join(directory, "registration-manifest.json"))
	})
	if err != nil {
		t.Fatal(err)
	}
	countrySourceIdBools := map[string]bool{}
	for _, source := range registration.CountrySources {
		countrySourceIdBools[source.Id] = true
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
			HostingSourceIds     []string `maxminddb:"hosting_prefix_source_ids"`
			AddressRiskSourceIds []string `maxminddb:"address_risk_source_ids"`
			OriginUseState       string   `maxminddb:"origin_use_state"`
			OriginSource         string   `maxminddb:"origin_evidence_source"`
			OriginOperatorIds    []string `maxminddb:"origin_operator_ids"`
			EvidenceKind         string   `maxminddb:"subscriber_evidence_kind"`
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
			if registration.CountryPolicyVersion != 2 || record.Scope != "arin" || len(record.CountryEvidence) == 0 || record.RegistrationMismatch != wantRisk {
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
		provenance := arinCandidateProvenance{record.Rule, record.Source, record.Reason, record.HostingSourceIds, record.AddressRiskSourceIds, record.OriginUseState, record.OriginSource, record.OriginOperatorIds, record.EvidenceKind}
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
			// Augmentation never resolves owners that disagree.
			quality := info.NonQuality == nonQuality && record.QualityAmbiguous == qualityAmbiguous ||
				!qualityAmbiguous && provenance.replacesOwnerDecision(record.Owners[0].QualityState)
			if record.RegisteredCountry != country || record.CountryAmbiguous != countryAmbiguous || !quality {
				t.Fatal("candidate substituted an arbitrary owner for fact consensus")
			}
		}
		if record.CountryAmbiguous {
			counts.CountryAmbiguousPrefixes++
		}
		if record.QualityAmbiguous {
			counts.QualityAmbiguousPrefixes++
		}
		if !provenance.citesOnlyAttested(manifest) {
			t.Fatal("candidate record cites evidence its manifest does not attest")
		}
		if info.NonQuality && info.QualityState != "unknown" && info.QualityState != "ambiguous" && !provenance.attestsExclusion() {
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

// Synthetic manifests shaped like the 3-input registration and the 30- and
// 38-input augmented releases. Augmented releases accept whatever best-effort
// evidence was available, never a missing required input or a foreign base.
func TestArinCandidateManifestAttestationFollowsItsBuilder(t *testing.T) {
	digest := func(name string) string {
		sum := sha256.Sum256([]byte(name))
		return hex.EncodeToString(sum[:])
	}
	registration := func() arinCandidateManifest {
		return arinCandidateManifest{Source: arinCandidateRegistrationSource, ClassifierVersion: 1, QualityPolicyVersion: 2,
			InputHashes: map[string]string{"arin_xml": digest("arin_xml"), "geolite2": digest("geolite2"), "classification_rules": digest("classification_rules")},
			Hashes:      map[string]string{"arin.mmdb": digest("registration arin.mmdb")}}
	}
	augmented := func(labels int) arinCandidateManifest {
		inputs := map[string]string{"rpki/validator": digest("rpki")}
		for _, key := range arinCandidateAugmentedRequiredInputs {
			inputs[key] = digest(key)
		}
		for role, n := range map[string]int{"address_risk": 5, "hosting_prefix": 7, "label": labels, "registry": 2, "registry_assignment": 5} {
			for i := range n {
				inputs[fmt.Sprintf("%s/source-%d", role, i)] = digest(fmt.Sprint(role, i))
			}
		}
		inputs["base_arin_mmdb"] = digest("registration arin.mmdb")
		return arinCandidateManifest{Source: arinCandidateAugmentedSource, ClassifierVersion: 1, QualityPolicyVersion: 2, InputHashes: inputs}
	}
	attest := func(manifest arinCandidateManifest, base *arinCandidateManifest) error {
		_, err := attestArinCandidateManifest(manifest, func() ([]byte, error) {
			if base == nil {
				return nil, os.ErrNotExist
			}
			return json.Marshal(base)
		})
		return err
	}
	type attestationCase struct {
		name   string
		labels int
		change func(manifest, base *arinCandidateManifest)
		ok     bool
	}
	cases := []attestationCase{
		{"30-input release", 6, func(*arinCandidateManifest, *arinCandidateManifest) {}, true},
		{"38-input release", 14, func(*arinCandidateManifest, *arinCandidateManifest) {}, true},
		{"only required inputs", 0, func(m, _ *arinCandidateManifest) {
			for key := range m.InputHashes {
				if !slices.Contains(arinCandidateAugmentedRequiredInputs, key) {
					delete(m.InputHashes, key)
				}
			}
		}, true},
		{"origin country policy with GeoLite2", 6, func(m, _ *arinCandidateManifest) {
			m.OriginCountryPolicy, m.InputHashes["geolite2"] = "reviewed", digest("geolite2")
		}, true},
		{"GeoLite2 without its policy", 6, func(m, _ *arinCandidateManifest) { m.InputHashes["geolite2"] = digest("geolite2") }, false},
		{"policy without GeoLite2", 6, func(m, _ *arinCandidateManifest) { m.OriginCountryPolicy = "reviewed" }, false},
		{"unknown role", 6, func(m, _ *arinCandidateManifest) { m.InputHashes["country_evidence/source"] = digest("country") }, false},
		{"registration input in an augmentation", 6, func(m, _ *arinCandidateManifest) { m.InputHashes["arin_xml"] = digest("arin_xml") }, false},
		{"empty source id", 6, func(m, _ *arinCandidateManifest) { m.InputHashes["label/"] = digest("label") }, false},
		{"nested source id", 6, func(m, _ *arinCandidateManifest) { m.InputHashes["label/a/b"] = digest("label") }, false},
		{"malformed evidence digest", 6, func(m, _ *arinCandidateManifest) { m.InputHashes["label/source-0"] = "00" }, false},
		{"not policy two", 6, func(m, _ *arinCandidateManifest) { m.QualityPolicyVersion = 0 }, false},
		{"missing registration manifest", 6, func(_, b *arinCandidateManifest) { *b = arinCandidateManifest{} }, false},
		{"registration base missing an input", 6, func(_, b *arinCandidateManifest) { delete(b.InputHashes, "classification_rules") }, false},
		{"foreign registration base", 6, func(_, b *arinCandidateManifest) { b.Hashes["arin.mmdb"] = digest("another registration") }, false},
		{"registration base not policy two", 6, func(_, b *arinCandidateManifest) { b.QualityPolicyVersion = 0 }, false},
		{"registration base from another builder", 6, func(_, b *arinCandidateManifest) { b.Source = arinCandidateAugmentedSource }, false},
		{"unknown builder", 6, func(m, _ *arinCandidateManifest) { m.Source = "arindbctl update" }, false},
	}
	for _, key := range arinCandidateAugmentedRequiredInputs {
		cases = append(cases, attestationCase{"missing " + key, 14, func(m, _ *arinCandidateManifest) { delete(m.InputHashes, key) }, false})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manifest, base := augmented(test.labels), registration()
			test.change(&manifest, &base)
			var read *arinCandidateManifest
			if base.Source != "" {
				read = &base
			}
			if err := attest(manifest, read); (err == nil) != test.ok {
				t.Fatalf("attested %t, want %t: %v", err == nil, test.ok, err)
			}
		})
	}
	if n := len(augmented(6).InputHashes); n != 30 {
		t.Fatalf("30-input shape has %d inputs", n)
	}
	if n := len(augmented(14).InputHashes); n != 38 {
		t.Fatalf("38-input shape has %d inputs", n)
	}
	// The registration-only release keeps its exact three-input rule.
	if err := attest(registration(), nil); err != nil {
		t.Fatal(err)
	}
	short := registration()
	delete(short.InputHashes, "geolite2")
	if attest(short, nil) == nil {
		t.Fatal("registration release missing an input attested")
	}
}

// Each augmentation layer's decision is attributable to evidence its manifest
// attests; the registration rule remains the only form a registration has.
func TestArinCandidateProvenanceAcceptsEachAttestedLayer(t *testing.T) {
	augmented := arinCandidateManifest{Source: arinCandidateAugmentedSource, InputHashes: map[string]string{"hosting_prefix/cloud": "attested", "address_risk/relays": "attested"}}
	registration := arinCandidateManifest{Source: arinCandidateRegistrationSource, InputHashes: map[string]string{"arin_xml": "attested"}}
	origin := arinCandidateProvenance{Reason: "reason", OriginUseState: "excluded", OriginSource: "source", OriginOperatorIds: []string{"operator"}}
	withheld := origin
	withheld.OriginUseState = "withheld"
	unidentified := origin
	unidentified.OriginOperatorIds = nil
	for _, test := range []struct {
		name            string
		provenance      arinCandidateProvenance
		manifest        arinCandidateManifest
		attested, cites bool
	}{
		{"registration rule", arinCandidateProvenance{Rule: "rule", Source: "source", Reason: "reason"}, registration, true, true},
		{"hosting prefix", arinCandidateProvenance{Reason: "reason", HostingSourceIds: []string{"cloud"}}, augmented, true, true},
		{"address risk", arinCandidateProvenance{Reason: "reason", AddressRiskSourceIds: []string{"relays"}}, augmented, true, true},
		{"origin operator", origin, augmented, true, true},
		{"no reason", arinCandidateProvenance{Rule: "rule", Source: "source", HostingSourceIds: []string{"cloud"}}, augmented, false, true},
		{"rule without source", arinCandidateProvenance{Rule: "rule", Reason: "reason"}, registration, false, true},
		{"withheld origin alone", withheld, augmented, false, true},
		{"origin without operators", unidentified, augmented, false, true},
		{"unattested hosting source", arinCandidateProvenance{Reason: "reason", HostingSourceIds: []string{"relays"}}, augmented, true, false},
		{"hosting source in a registration release", arinCandidateProvenance{Reason: "reason", HostingSourceIds: []string{"cloud"}}, registration, true, false},
		{"origin evidence in a registration release", origin, registration, true, false},
	} {
		if test.provenance.attestsExclusion() != test.attested || test.provenance.citesOnlyAttested(test.manifest) != test.cites {
			t.Errorf("%s: attests exclusion %t, cites only attested %t", test.name, test.provenance.attestsExclusion(), test.provenance.citesOnlyAttested(test.manifest))
		}
	}
	inferred := arinCandidateProvenance{Rule: "rule", Source: "source", Reason: "reason", OriginUseState: "subscriber", OriginSource: "source", OriginOperatorIds: []string{"operator"}, EvidenceKind: "isp_inferred"}
	approved := inferred
	approved.EvidenceKind = ""
	conflicting := withheld
	conflicting.OriginUseState = "ambiguous"
	for _, test := range []struct {
		name       string
		provenance arinCandidateProvenance
		agreed     string
		replaces   bool
	}{
		{"subscriber inferred over unknown owners", inferred, "unknown", true},
		{"origin exclusion over subscriber owners", origin, "subscriber", true},
		{"origin conflict over unknown owners", conflicting, "unknown", true},
		{"hosting prefix over subscriber owners", arinCandidateProvenance{Reason: "reason", HostingSourceIds: []string{"cloud"}}, "subscriber", true},
		{"address risk over unknown owners", arinCandidateProvenance{Reason: "reason", AddressRiskSourceIds: []string{"relays"}}, "unknown", true},
		{"inference over subscriber owners", inferred, "subscriber", false},
		{"inference over excluded owners", inferred, "excluded", false},
		{"subscriber origin without inference", approved, "unknown", false},
		{"withheld origin", withheld, "unknown", false},
		{"exclusion over excluded owners", origin, "excluded", false},
		{"registration rule alone", arinCandidateProvenance{Rule: "rule", Source: "source", Reason: "reason"}, "unknown", false},
	} {
		if test.provenance.replacesOwnerDecision(test.agreed) != test.replaces {
			t.Errorf("%s: replaces owner decision %t", test.name, !test.replaces)
		}
	}
}
