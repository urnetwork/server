// Offline candidate inspection reports aggregates without retaining identities.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
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
		ClassifierVersion uint32            `json:"classifier_version"`
		InputHashes       map[string]string `json:"inputs_sha256"`
		Hashes            map[string]string `json:"sha256"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ClassifierVersion != 1 || len(manifest.InputHashes) != 3 {
		t.Fatal("candidate has no complete version-one input attestation")
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
	}{BuildEpoch: db.Metadata.BuildTime().Unix(), ByRule: map[string]count{}, ByCountry: map[string]count{}, ByScope: map[string]count{}}
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
		var record struct {
			RegisteredCountry string `maxminddb:"registered_country"`
			AssociatedCountry string `maxminddb:"associated_country"`
			Scope             string `maxminddb:"registration_scope"`
			Rule              string `maxminddb:"classification_rule"`
			Source            string `maxminddb:"classification_source"`
			Reason            string `maxminddb:"reason"`
			MultipleOwners    bool   `maxminddb:"multiple_registration_owners"`
			CountryAmbiguous  bool   `maxminddb:"country_ambiguous"`
			QualityAmbiguous  bool   `maxminddb:"non_quality_ambiguous"`
			Owners            []struct {
				Country    string `maxminddb:"registered_country"`
				NonQuality bool   `maxminddb:"non_quality"`
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
			countryAmbiguous, qualityAmbiguous := false, false
			for _, owner := range record.Owners[1:] {
				countryAmbiguous = countryAmbiguous || owner.Country != country
				qualityAmbiguous = qualityAmbiguous || owner.NonQuality != nonQuality
			}
			if countryAmbiguous {
				country = ""
			}
			if qualityAmbiguous {
				nonQuality = false
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
		if info.NonQuality && (record.Rule == "" || record.Source == "" || record.Reason == "") {
			t.Fatal("candidate hosting exception lacks reviewed rule provenance")
		}
		if record.Rule != "" && !info.NonQuality {
			counts.ReviewedAccessPrefixes++
		}
		counts.All = add(counts.All, info)
		counts.ByRule[record.Rule] = add(counts.ByRule[record.Rule], info)
		counts.ByCountry[record.AssociatedCountry] = add(counts.ByCountry[record.AssociatedCountry], info)
		counts.ByScope[record.Scope] = add(counts.ByScope[record.Scope], info)
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
