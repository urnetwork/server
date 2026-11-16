// Reviewed country evidence refines only its bound authoritative allocation.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	"golang.org/x/text/language"
)

const maxCountryEvidenceSourceBytes = 16 * 1024 * 1024

// Snapshots are reviewed local inputs, never fetched implicitly by a build.
type countryEvidenceSource struct {
	Id         string    `yaml:"id" json:"id"`
	Url        string    `yaml:"url" json:"url"`
	File       string    `yaml:"file" json:"file"`
	Sha256     string    `yaml:"sha256" json:"sha256"`
	ObservedAt time.Time `yaml:"observed_at" json:"observed_at"`
	ExpiresAt  time.Time `yaml:"expires_at" json:"expires_at"`
}

// Both identities are required; a corporate parent cannot waive a reassignment.
type countryEvidenceOwner struct {
	NetHandle string `yaml:"net_handle"`
	OrgHandle string `yaml:"org_handle"`
}

// A complete country set and an explicit uncertainty assertion are exclusive.
// Owners name the entire direct-owner set of one containing ARIN allocation.
type countryEvidenceRule struct {
	Name         string                 `yaml:"name"`
	Prefix       string                 `yaml:"prefix"`
	Owners       []countryEvidenceOwner `yaml:"owners"`
	SourceId     string                 `yaml:"source_id"`
	CountryCodes []string               `yaml:"country_codes"`
	Uncertainty  string                 `yaml:"uncertainty"`
	Reason       string                 `yaml:"reason"`
	prefix       netip.Prefix
}

// No matched rule means the existing registration policy, not policy-two unknown.
type arinCountryEvidence struct {
	state        string
	countryCodes []string
	rules        []countryEvidenceRule
}

// Exclude unknown, macro-region and alias codes from complete country assertions.
func knownCountryCode(code string) bool {
	region, err := language.ParseRegion(code)
	return err == nil && len(code) == 2 && region.IsCountry() && strings.EqualFold(region.String(), code)
}

// Parses the additive input schema without changing legacy hosting rules.
func (self *classificationRules) validateCountryEvidence() error {
	if self.CountryPolicyVersion == 0 && len(self.CountrySources) == 0 && len(self.CountryRules) == 0 {
		return nil
	}
	if self.CountryPolicyVersion != 2 || len(self.CountrySources) == 0 || len(self.CountryRules) == 0 {
		return errors.New("country evidence requires policy version 2, sources and rules")
	}
	sourceIdBools := map[string]bool{}
	for _, source := range self.CountrySources {
		if source.Id == "" || sourceIdBools[source.Id] || strings.ContainsAny(source.Id, "/\\") {
			return errors.New("country source requires a unique simple id")
		}
		sourceIdBools[source.Id] = true
		location, err := url.Parse(source.Url)
		if err != nil || location.Scheme != "https" || location.Host == "" || location.User != nil {
			return errors.New("country source requires a public HTTPS evidence URL")
		}
		if !filepath.IsLocal(source.File) || source.File == "." {
			return errors.New("country source snapshot must be relative to the rules directory")
		}
		digest, err := hex.DecodeString(source.Sha256)
		if err != nil || len(digest) != sha256.Size || source.Sha256 != strings.ToLower(source.Sha256) {
			return errors.New("country source requires a lowercase SHA256 digest")
		}
		if source.ObservedAt.IsZero() || !source.ObservedAt.Before(source.ExpiresAt) {
			return errors.New("country source requires ordered observation and expiry times")
		}
	}
	ruleNameBools := map[string]bool{}
	usedSourceIdBools := map[string]bool{}
	for i := range self.CountryRules {
		rule := &self.CountryRules[i]
		if strings.TrimSpace(rule.Name) == "" || ruleNameBools[rule.Name] || strings.TrimSpace(rule.Reason) == "" || !sourceIdBools[rule.SourceId] {
			return errors.New("country rule requires a unique name, reason and known source")
		}
		ruleNameBools[rule.Name] = true
		usedSourceIdBools[rule.SourceId] = true
		prefix, err := netip.ParsePrefix(rule.Prefix)
		if err != nil || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return errors.New("country rule requires a canonical prefix")
		}
		rule.prefix = prefix
		if len(rule.Owners) == 0 {
			return errors.New("country rule requires its complete direct owner set")
		}
		ownerBools := map[countryEvidenceOwner]bool{}
		for _, owner := range rule.Owners {
			if owner.NetHandle == "" || owner.OrgHandle == "" || ownerBools[owner] {
				return errors.New("country rule requires distinct network and organization identities")
			}
			ownerBools[owner] = true
		}
		rule.Uncertainty = strings.TrimSpace(rule.Uncertainty)
		if (len(rule.CountryCodes) == 0) == (rule.Uncertainty == "") {
			return errors.New("country rule requires countries or uncertainty, exclusively")
		}
		for i, code := range rule.CountryCodes {
			code = strings.ToLower(code)
			if !knownCountryCode(code) {
				return errors.New("country rule has an unknown country code")
			}
			rule.CountryCodes[i] = code
		}
		slices.Sort(rule.CountryCodes)
		if len(slices.Compact(slices.Clone(rule.CountryCodes))) != len(rule.CountryCodes) {
			return errors.New("country rule repeats a country code")
		}
	}
	if len(usedSourceIdBools) != len(sourceIdBools) {
		return errors.New("country source has no reviewed rule")
	}
	return nil
}

// Rooted file access rejects symlink escapes as well as lexical parent paths.
// Recheck these hashes after generation; a stale/changed source stops publication.
func (self classificationRules) hashCountryEvidenceSources(ctx context.Context, rulesPath string, buildTime time.Time) (map[string]string, error) {
	return self.hashEvidenceSourcesWithin(ctx, rulesPath, buildTime, maxCountryEvidenceSourceBytes)
}

// Subscriber evidence includes registry dumps of hundreds of megabytes, so its
// bound is a parameter; every other check is shared with country evidence.
func (self classificationRules) hashEvidenceSourcesWithin(ctx context.Context, rulesPath string, buildTime time.Time, maxBytes int64) (map[string]string, error) {
	hashes := map[string]string{}
	if len(self.CountrySources) == 0 {
		return hashes, nil
	}
	root, err := os.OpenRoot(filepath.Dir(rulesPath))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, source := range self.CountrySources {
		if source.ObservedAt.After(buildTime) || !buildTime.Before(source.ExpiresAt) {
			return nil, errors.New("country evidence is future-dated or expired at build time")
		}
		digest, err := func() (string, error) {
			file, err := root.OpenFile(source.File, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return "", errors.New("country source snapshot is absent or outside the rules directory")
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBytes {
				return "", errors.New("country source snapshot must be a nonempty bounded regular file")
			}
			hash := sha256.New()
			reader := io.LimitReader(file, maxBytes+1)
			buffer := make([]byte, 64*1024)
			var total int64
			for {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				n, err := reader.Read(buffer)
				total += int64(n)
				_, _ = hash.Write(buffer[:n])
				if total > maxBytes {
					return "", errors.New("country source snapshot exceeds its size bound")
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					return "", err
				}
			}
			if total == 0 {
				return "", errors.New("country source snapshot became empty while reading")
			}
			return hex.EncodeToString(hash.Sum(nil)), nil
		}()
		if err != nil {
			return nil, err
		}
		if digest != source.Sha256 {
			return nil, errors.New("country source snapshot digest differs from its reviewed evidence")
		}
		hashes["country_evidence/"+source.Id] = digest
	}
	return hashes, nil
}

// Matching all current direct owners prevents stale or partial authority waivers.
func (self countryEvidenceRule) matchesOwners(group arinAllocationGroup) bool {
	if len(self.Owners) != len(group.owners) {
		return false
	}
	for _, allocation := range group.owners {
		if arinBlockRegistrationScope(allocation.blockType) != "arin" || !slices.Contains(self.Owners, countryEvidenceOwner{
			NetHandle: allocation.network, OrgHandle: allocation.organization,
		}) {
			return false
		}
	}
	return true
}

// Each rule must fit one current bound allocation; broader prefixes cannot become
// organization-wide exceptions. More-specific direct children still replace it.
func (self classificationRules) validateCountryEvidenceOwners(groups []arinAllocationGroup) error {
	for _, rule := range self.CountryRules {
		found := false
		for _, group := range groups {
			if group.prefix.Bits() <= rule.prefix.Bits() && group.prefix.Contains(rule.prefix.Addr()) && rule.matchesOwners(group) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("country rule %s lacks its exact containing authoritative allocation", rule.Name)
		}
	}
	return nil
}

// Longest-prefix precedence is independent of input order. Conflicting equally
// specific assertions stay ambiguous, never an invented union of countries.
func (self classificationRules) countryEvidence(group arinAllocationGroup, address netip.Addr) arinCountryEvidence {
	evidence := arinCountryEvidence{}
	best := -1
	for _, rule := range self.CountryRules {
		if rule.prefix.Bits() < best || !rule.prefix.Contains(address) || !rule.matchesOwners(group) {
			continue
		}
		if rule.prefix.Bits() > best {
			evidence.rules = nil
			best = rule.prefix.Bits()
		}
		evidence.rules = append(evidence.rules, rule)
	}
	if len(evidence.rules) == 0 {
		return evidence
	}
	slices.SortFunc(evidence.rules, func(a, b countryEvidenceRule) int { return strings.Compare(a.Name, b.Name) })
	first := evidence.rules[0]
	evidence.state, evidence.countryCodes = "known", first.CountryCodes
	if first.Uncertainty != "" {
		evidence.state = "unknown"
	}
	for _, rule := range evidence.rules[1:] {
		if (rule.Uncertainty == "") != (first.Uncertainty == "") || !slices.Equal(rule.CountryCodes, first.CountryCodes) {
			evidence.state, evidence.countryCodes = "ambiguous", nil
			break
		}
	}
	return evidence
}

// Missing GeoLite geography stays unknown even with a complete reviewed set.
func (self arinCountryEvidence) risk(associatedCountry string, registrationMismatch bool) bool {
	if self.state == "" {
		return registrationMismatch
	}
	return self.state == "known" && knownCountryCode(associatedCountry) && !slices.Contains(self.countryCodes, strings.ToLower(associatedCountry))
}

// Additive provenance leaves the existing explicit boolean reader unchanged.
func (self arinCountryEvidence) addRecordFields(data mmdbtype.Map) {
	if self.state == "" {
		return
	}
	countries := mmdbtype.Slice{}
	for _, country := range self.countryCodes {
		countries = append(countries, mmdbtype.String(country))
	}
	records := mmdbtype.Slice{}
	for _, rule := range self.rules {
		ruleCountries := mmdbtype.Slice{}
		for _, country := range rule.CountryCodes {
			ruleCountries = append(ruleCountries, mmdbtype.String(country))
		}
		owners := mmdbtype.Slice{}
		for _, owner := range rule.Owners {
			owners = append(owners, mmdbtype.Map{"net_handle": mmdbtype.String(owner.NetHandle), "org_handle": mmdbtype.String(owner.OrgHandle)})
		}
		records = append(records, mmdbtype.Map{
			"rule": mmdbtype.String(rule.Name), "prefix": mmdbtype.String(rule.Prefix), "owners": owners,
			"source_id": mmdbtype.String(rule.SourceId), "reason": mmdbtype.String(rule.Reason),
			"country_codes": ruleCountries, "uncertainty": mmdbtype.String(rule.Uncertainty),
		})
	}
	data["country_policy_version"] = mmdbtype.Uint32(2)
	data["country_evidence_state"] = mmdbtype.String(self.state)
	data["credible_country_codes"] = countries
	data["country_evidence"] = records
}
