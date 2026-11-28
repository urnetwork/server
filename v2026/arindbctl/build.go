// Builds per-prefix exceptions from registration and GeoLite2 country evidence.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
)

type arinOrganization struct {
	Handle  string `xml:"handle"`
	Name    string `xml:"name"`
	Country string `xml:"iso3166-1>code2"`
	Parent  string `xml:"parentOrgHandle"`
}
type arinNetwork struct {
	Handle    string      `xml:"handle"`
	Parent    string      `xml:"parentNetHandle"`
	OrgHandle string      `xml:"orgHandle"`
	Blocks    []arinBlock `xml:"netBlocks>netBlock"`
}
type arinBlock struct {
	Type         string `xml:"type"`
	Length       *int   `xml:"cidrLength"`
	LegacyLength *int   `xml:"cidrLenth"`
	Start        string `xml:"startAddress"`
	End          string `xml:"endAddress"`
}
type arinAllocation struct {
	prefix       netip.Prefix
	organization string
	blockType    string
	network      string
}

// One effective prefix may retain several incomparable direct registrations.
type arinAllocationGroup struct {
	prefix netip.Prefix
	owners []arinAllocation
}

// ARIN's documented external-RIR entries are referrals, not authoritative
// customer-country facts. AV is a legacy ARIN registration, not a referral.
// Missing/new codes remain explicitly unknown instead of guessing geography.
// https://www.arin.net/reference/research/bulkwhois/#net-xml-elements
func arinBlockRegistrationScope(blockType string) string {
	switch blockType {
	case "A", "AV", "DA", "DS", "S":
		return "arin"
	case "AF", "AP", "FX", "LN", "LX", "PV", "PX", "RN", "RV", "RX":
		return "referral"
	case "AR":
		return "registry"
	case "IR", "IU":
		return "reserved"
	default:
		return "unknown"
	}
}

// Rules are reviewed config inputs. Specific prefix overrides take precedence
// over organization rules, allowing a verified access ISP inside a hosting parent.
type classificationRules struct {
	Version              uint32                  `yaml:"version"`
	QualityPolicyVersion uint32                  `yaml:"quality_policy_version"`
	Rules                []classificationRule    `yaml:"rules"`
	CountryPolicyVersion uint32                  `yaml:"country_policy_version"`
	CountrySources       []countryEvidenceSource `yaml:"country_sources"`
	CountryRules         []countryEvidenceRule   `yaml:"country_rules"`
	allocationRules      map[arinAllocationScopeKey][]int
}
type classificationRule struct {
	Name             string                          `yaml:"name"`
	OrgHandles       []string                        `yaml:"org_handles"`
	OrgNamePattern   string                          `yaml:"org_name_pattern"`
	Prefixes         []string                        `yaml:"prefixes"`
	AllocationScopes []classificationAllocationScope `yaml:"allocation_scopes"`
	NonQuality       *bool                           `yaml:"non_quality"`
	RiskCategory     string                          `yaml:"risk_category"`
	Reason           string                          `yaml:"reason"`
	Source           string                          `yaml:"source"`
	pattern          *regexp.Regexp
	prefixes         []netip.Prefix
}

// Truncation, malformed records, and empty responses fail the complete source.
func scanArinXml(ctx context.Context, path string, org func(arinOrganization) error, network func(arinNetwork) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := xml.NewDecoder(file)
	depth, roots, organizations, networks := 0, 0, 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("ARIN XML is truncated or malformed")
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				if value.Name.Local != "bulkwhois" || roots != 0 {
					return errors.New("ARIN XML has an unexpected root")
				}
				roots++
			}
			depth++
			if depth != 2 {
				continue
			}
			switch value.Name.Local {
			case "org":
				var record arinOrganization
				if err := decoder.DecodeElement(&record, &value); err != nil {
					return errors.New("ARIN organization is malformed")
				}
				depth--
				if record.Handle == "" {
					return errors.New("ARIN organization has no handle")
				}
				organizations++
				if org != nil {
					if err := org(record); err != nil {
						return err
					}
				}
			case "net":
				var record arinNetwork
				if err := decoder.DecodeElement(&record, &value); err != nil {
					return errors.New("ARIN network is malformed")
				}
				depth--
				if record.OrgHandle == "" || len(record.Blocks) == 0 {
					return errors.New("ARIN network has no owner or blocks")
				}
				networks++
				if network != nil {
					if err := network(record); err != nil {
						return err
					}
				}
			default:
				if err := decoder.Skip(); err != nil {
					return errors.New("ARIN source object is malformed")
				}
				depth--
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(value)) != "" {
				return errors.New("unexpected content outside ARIN root")
			}
		}
	}
	if roots != 1 || depth != 0 || organizations == 0 || networks == 0 {
		return errors.New("ARIN XML has no complete organization/network catalog")
	}
	return nil
}

// ARIN's decimal IPv4 octets may be padded with zeroes; normalize before parsing.
func parseArinAddress(text string) (netip.Addr, error) {
	if strings.Contains(text, ":") {
		return netip.ParseAddr(text)
	}
	parts := strings.Split(text, ".")
	if len(parts) != 4 {
		return netip.Addr{}, errors.New("invalid ARIN IPv4 address")
	}
	var address [4]byte
	for i, part := range parts {
		value, err := strconv.ParseUint(part, 10, 8)
		if err != nil {
			return netip.Addr{}, errors.New("invalid ARIN IPv4 octet")
		}
		address[i] = byte(value)
	}
	return netip.AddrFrom4(address), nil
}

// A malformed/mismatched source range must not broaden the affected prefix.
func (self arinBlock) prefix() (netip.Prefix, error) {
	length := self.Length
	if length == nil {
		length = self.LegacyLength
	}
	if length == nil {
		return netip.Prefix{}, errors.New("ARIN block has no CIDR length")
	}
	if self.Length != nil && self.LegacyLength != nil && *self.Length != *self.LegacyLength {
		return netip.Prefix{}, errors.New("ARIN block has conflicting CIDR lengths")
	}
	start, err := parseArinAddress(self.Start)
	if err != nil {
		return netip.Prefix{}, err
	}
	prefix := netip.PrefixFrom(start, *length)
	if !prefix.IsValid() || prefix.Masked().Addr() != start {
		return netip.Prefix{}, errors.New("ARIN block is not a canonical network")
	}
	if self.End != "" {
		end, err := parseArinAddress(self.End)
		if err != nil || !prefix.Contains(end) {
			return netip.Prefix{}, errors.New("ARIN block end is outside its prefix")
		}
		// All host bits must be set for one full CIDR block.
		bytes := start.AsSlice()
		for bit := *length; bit < start.BitLen(); bit++ {
			bytes[bit/8] |= 1 << (7 - bit%8)
		}
		last, _ := netip.AddrFromSlice(bytes)
		if last != end {
			return netip.Prefix{}, errors.New("ARIN block end does not cover its prefix")
		}
	}
	return prefix, nil
}

func loadClassificationRules(path string) (classificationRules, error) {
	var rules classificationRules
	if path == "" {
		return rules, errors.New("rules is required: provide the reviewed ARIN classifier configuration")
	}
	file, err := os.Open(path)
	if err != nil {
		return rules, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&rules); err != nil {
		return rules, fmt.Errorf("invalid ARIN classification rules: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return rules, errors.New("ARIN rules must contain one document")
	}
	if rules.Version != 1 || len(rules.Rules) == 0 {
		return rules, errors.New("ARIN classifier requires version1 and reviewed rules")
	}
	if rules.QualityPolicyVersion != 0 && rules.QualityPolicyVersion != 2 {
		return rules, errors.New("unsupported ARIN quality policy version")
	}
	names := map[string]bool{}
	for i := range rules.Rules {
		rule := &rules.Rules[i]
		if rule.Name == "" || names[rule.Name] || rule.Reason == "" || rule.Source == "" {
			return rules, errors.New("ARIN rule requires a unique name, reason and source")
		}
		if rule.NonQuality == nil {
			return rules, errors.New("ARIN rule requires an explicit non_quality decision")
		}
		if rule.RiskCategory != "" {
			if !*rule.NonQuality || !slices.Contains([]string{"virtual_isp", "proxy", "vpn", "tor"}, rule.RiskCategory) {
				return rules, errors.New("ARIN risk category requires an excluded virtual ISP, proxy, VPN or Tor rule")
			}
		}
		names[rule.Name] = true
		if len(rule.Prefixes) == 0 && len(rule.OrgHandles) == 0 && rule.OrgNamePattern == "" && len(rule.AllocationScopes) == 0 {
			return rules, errors.New("ARIN rule has no match criteria")
		}
		if len(rule.Prefixes) > 0 && (len(rule.OrgHandles) > 0 || rule.OrgNamePattern != "") {
			return rules, errors.New("prefix and organization rules must be separate")
		}
		if rule.OrgNamePattern != "" {
			rule.pattern, err = regexp.Compile(rule.OrgNamePattern)
			if err != nil {
				return rules, fmt.Errorf("invalid ARIN name rule %s", rule.Name)
			}
		}
		for _, text := range rule.Prefixes {
			prefix, err := netip.ParsePrefix(text)
			if err != nil || prefix != prefix.Masked() {
				return rules, fmt.Errorf("invalid ARIN prefix rule %s", rule.Name)
			}
			rule.prefixes = append(rule.prefixes, prefix)
		}
	}
	if err := rules.prepareAllocationScopes(); err != nil {
		return rules, err
	}
	if err := rules.validateCountryEvidence(); err != nil {
		return rules, err
	}
	return rules, nil
}

// The matched rule and organization remain auditable even when the direct
// allocation owner inherits its classification from a reviewed parent.
type arinClassification struct {
	qualityState  string
	nonQuality    bool
	reason        string
	ruleName      string
	source        string
	orgHandle     string
	networkHandle string
}

// Organizations arrive parent-first. A reviewed child and then a narrower
// prefix override ancestor use. Equally specific contradictory evidence is
// ambiguous under policy two, never an allow selected by file order.
func (self classificationRules) classify(organizations []arinOrganization, address netip.Addr) arinClassification {
	fromRule := func(rule classificationRule, owner string) arinClassification {
		state := ""
		if self.QualityPolicyVersion == 2 {
			state = "excluded"
			if !*rule.NonQuality {
				state = "subscriber"
			}
		}
		return arinClassification{nonQuality: *rule.NonQuality, qualityState: state,
			reason: rule.Reason, ruleName: rule.Name, source: rule.Source, orgHandle: owner}
	}
	mergeEqual := func(previous, next arinClassification) arinClassification {
		if self.QualityPolicyVersion != 2 || previous.qualityState == next.qualityState {
			return next
		}
		sources := []string{previous.source, next.source}
		slices.Sort(sources)
		return arinClassification{nonQuality: true, qualityState: "ambiguous", ruleName: "rule-conflict",
			reason: "equally specific reviewed rules disagree on network use", source: strings.Join(sources, " "), orgHandle: next.orgHandle}
	}
	classification := self.unknownClassification()
	for _, org := range organizations {
		// A subscriber parent does not establish the use of a reassigned child.
		if self.QualityPolicyVersion == 2 && !classification.nonQuality {
			classification = self.unknownClassification()
		}
		matched := false
		for _, rule := range self.Rules {
			if slices.Contains(rule.OrgHandles, org.Handle) || rule.pattern != nil && rule.pattern.MatchString(org.Name) {
				next := fromRule(rule, org.Handle)
				if matched {
					next = mergeEqual(classification, next)
				}
				classification, matched = next, true
			}
		}
	}
	best := -1
	for _, rule := range self.Rules {
		for _, prefix := range rule.prefixes {
			if prefix.Bits() >= best && prefix.Contains(address) {
				next := fromRule(rule, "")
				if prefix.Bits() == best {
					next = mergeEqual(classification, next)
				}
				classification, best = next, prefix.Bits()
			}
		}
	}
	return classification
}

func (self classificationRules) unknownClassification() arinClassification {
	if self.QualityPolicyVersion == 2 {
		return arinClassification{nonQuality: true, qualityState: "unknown", reason: "no reviewed subscriber-access evidence"}
	}
	return arinClassification{}
}

// Splits only the branch containing an override. An allow exception for a /24
// must neither clear its parent's whole /16 nor disappear inside that parent.
func splitClassificationPrefix(prefix netip.Prefix, boundary netip.Prefix) []netip.Prefix {
	if prefix.Bits() >= boundary.Bits() || !prefix.Contains(boundary.Addr()) {
		return []netip.Prefix{prefix}
	}
	left := netip.PrefixFrom(prefix.Addr(), prefix.Bits()+1)
	bytes := prefix.Addr().AsSlice()
	bytes[prefix.Bits()/8] |= 1 << (7 - prefix.Bits()%8)
	rightAddress, _ := netip.AddrFromSlice(bytes)
	right := netip.PrefixFrom(rightAddress, prefix.Bits()+1)
	if left.Contains(boundary.Addr()) {
		return append(splitClassificationPrefix(left, boundary), right)
	}
	return append([]netip.Prefix{left}, splitClassificationPrefix(right, boundary)...)
}

func (self classificationRules) partitions(prefix netip.Prefix) []netip.Prefix {
	partitions := []netip.Prefix{prefix}
	split := func(boundary netip.Prefix) {
		next := []netip.Prefix{}
		for _, partition := range partitions {
			next = append(next, splitClassificationPrefix(partition, boundary)...)
		}
		partitions = next
	}
	for _, rule := range self.Rules {
		for _, boundary := range rule.prefixes {
			split(boundary)
		}
	}
	for _, rule := range self.CountryRules {
		split(rule.prefix)
	}
	return partitions
}

// Bulk Whois can contain a complete reassignment beside its equal-prefix
// parent. Network ancestry selects the direct owners; incomparable registrations
// remain together for explicit country/classification consensus, never a tie.
func selectArinAllocations(ctx context.Context, allocations []arinAllocation, networkParents map[string]string) ([]arinAllocationGroup, error) {
	depths := map[string]int{}
	visiting := map[string]bool{}
	var depth func(string) (int, error)
	depth = func(handle string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if known, ok := depths[handle]; ok {
			return known, nil
		}
		parent, exists := networkParents[handle]
		if !exists || parent == "" {
			depths[handle] = 0
			return 0, nil
		}
		if visiting[handle] {
			return 0, errors.New("ARIN network parent cycle")
		}
		visiting[handle] = true
		parentDepth, err := depth(parent)
		delete(visiting, handle)
		if err != nil {
			return 0, err
		}
		depths[handle] = parentDepth + 1
		return parentDepth + 1, nil
	}
	for handle := range networkParents {
		if _, err := depth(handle); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(allocations, func(a, b arinAllocation) int {
		if a.prefix.Bits() != b.prefix.Bits() {
			return a.prefix.Bits() - b.prefix.Bits()
		}
		if order := a.prefix.Addr().Compare(b.prefix.Addr()); order != 0 {
			return order
		}
		if order := depths[a.network] - depths[b.network]; order != 0 {
			return order
		}
		return strings.Compare(a.network, b.network)
	})
	selected := make([]arinAllocationGroup, 0, len(allocations))
	for start := 0; start < len(allocations); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + 1
		for end < len(allocations) && allocations[start].prefix == allocations[end].prefix {
			end++
		}
		owners := allocations[start:end]
		if len(owners) > 1 {
			direct := make([]arinAllocation, 0, len(owners))
			for _, candidate := range owners {
				superseded := false
				for _, other := range owners {
					if candidate.network == other.network {
						if candidate.organization != other.organization || candidate.blockType != other.blockType {
							return nil, errors.New("contradictory ARIN allocation facts within one network record")
						}
						continue
					}
					for handle := networkParents[other.network]; handle != ""; handle = networkParents[handle] {
						if handle == candidate.network {
							superseded = true
							break
						}
					}
				}
				if !superseded && !slices.Contains(direct, candidate) {
					direct = append(direct, candidate)
				}
			}
			owners = direct
		}
		selected = append(selected, arinAllocationGroup{prefix: allocations[start].prefix, owners: owners})
		start = end
	}
	return selected, nil
}

// Writes a complete new database. ARIN prefixes are inserted broadest first;
// child allocations replace their parent's facts, including a clean exception.
func buildArinDatabase(ctx context.Context, source string, geolite2 string, rulesPath string, output string) error {
	return buildArinDatabaseAt(ctx, source, geolite2, rulesPath, output, time.Now().UTC())
}

// One explicit build instant binds source freshness and reproducible metadata.
func buildArinDatabaseAt(ctx context.Context, source string, geolite2 string, rulesPath string, output string, buildTime time.Time) error {
	if source == "" || geolite2 == "" {
		return errors.New("source and geolite2 are required")
	}
	if buildTime.Unix() <= 0 {
		return errors.New("a positive ARIN build time is required")
	}
	inputHashes := map[string]string{}
	inputPaths := map[string]string{"arin_xml": source, "geolite2": geolite2, "classification_rules": rulesPath}
	for name, path := range inputPaths {
		hash, err := hashArinBuildInput(ctx, path)
		if err != nil {
			return err
		}
		inputHashes[name] = hash
	}
	rules, err := loadClassificationRules(rulesPath)
	if err != nil {
		return err
	}
	countryHashes, err := rules.hashCountryEvidenceSources(ctx, rulesPath, buildTime)
	if err != nil {
		return err
	}
	for name, hash := range countryHashes {
		inputHashes[name] = hash
	}
	geoDb, err := server.OpenIpInfoDatabase(geolite2)
	if err != nil {
		return err
	}
	defer geoDb.Close()
	organizations := map[string]arinOrganization{}
	allocations := []arinAllocation{}
	networkParents := map[string]string{}
	if err := scanArinXml(ctx, source, func(org arinOrganization) error {
		if _, ok := organizations[org.Handle]; ok {
			return errors.New("duplicate ARIN organization handle")
		}
		org.Country = strings.ToLower(strings.TrimSpace(org.Country))
		organizations[org.Handle] = org
		return nil
	}, func(network arinNetwork) error {
		if network.Handle != "" {
			if _, exists := networkParents[network.Handle]; exists {
				return errors.New("duplicate ARIN network handle")
			}
			networkParents[network.Handle] = network.Parent
		}
		for _, block := range network.Blocks {
			prefix, err := block.prefix()
			if err != nil {
				return err
			}
			allocations = append(allocations, arinAllocation{prefix: prefix, organization: network.OrgHandle,
				blockType: strings.ToUpper(strings.TrimSpace(block.Type)), network: network.Handle})
		}
		return nil
	}); err != nil {
		return err
	}
	sourceAllocationCount := len(allocations)
	if err := rules.validateAllocationScopes(allocations); err != nil {
		return err
	}
	allocationGroups, err := selectArinAllocations(ctx, allocations, networkParents)
	if err != nil {
		return err
	}
	if err := rules.validateCountryEvidenceOwners(allocationGroups); err != nil {
		return err
	}
	// Retain exact network ancestry for the Quality-only hosting fallback.
	// Selection of the direct owner, countries and independent risk is unchanged.
	networkAllocations := map[string][]arinAllocation{}
	for _, parent := range networkParents {
		if parent != "" {
			networkAllocations[parent] = nil
		}
	}
	for _, allocation := range allocations {
		if _, isParent := networkAllocations[allocation.network]; isParent {
			networkAllocations[allocation.network] = append(networkAllocations[allocation.network], allocation)
		}
	}
	writer, err := mmdbwriter.New(mmdbwriter.Options{BuildEpoch: buildTime.Unix(), DatabaseType: "urnetwork arindb", IncludeReservedNetworks: true, RecordSize: 32, Description: map[string]string{"en": "ARIN registration and GeoLite2 classification exceptions"}})
	if err != nil {
		return err
	}
	if rules.QualityPolicyVersion == 2 {
		// Uncovered addresses also carry an explicit unknown policy decision.
		// Existing boolean readers then exclude them from Quality as well.
		for _, prefix := range []string{"0.0.0.0/0", "::/0"} {
			_, network, _ := net.ParseCIDR(prefix)
			if err := writer.Insert(network, mmdbtype.Map{
				"classifier_version": mmdbtype.Uint32(rules.Version), "quality_policy_version": mmdbtype.Uint32(2),
				"quality_state": mmdbtype.String("unknown"), "non_quality": mmdbtype.Bool(true), "risk": mmdbtype.Bool(false),
				"reason": mmdbtype.String("no authoritative allocation with reviewed subscriber-access evidence"),
			}); err != nil {
				return err
			}
		}
	}
	var partitions, riskCount, nonQualityCount int
	var multipleOwnerAllocations, conflictingCountryAllocations, unknownCountryAllocations, ambiguousQualityPartitions int
	countryEvidencePartitions := map[string]int{}
	qualityStates := map[string]int{}
	var networkRiskCount int
	registrationScopes := map[string]int{"arin": 0, "referral": 0, "registry": 0, "reserved": 0, "unknown": 0}
	for _, group := range allocationGroups {
		if err := ctx.Err(); err != nil {
			return err
		}
		type ownerEvidence struct {
			allocation        arinAllocation
			ancestors         []arinOrganization
			countries         []string
			registrationScope string
			registeredCountry string
			qualityParent     arinAllocationQualityParent
		}
		owners := make([]ownerEvidence, 0, len(group.owners))
		for _, allocation := range group.owners {
			org, ok := organizations[allocation.organization]
			if !ok {
				return errors.New("ARIN network owner is absent")
			}
			owner := ownerEvidence{allocation: allocation, registrationScope: arinBlockRegistrationScope(allocation.blockType)}
			if owner.registrationScope == "arin" {
				owner.registeredCountry = org.Country
			}
			seen := map[string]bool{}
			for current := org; ; {
				if seen[current.Handle] {
					return errors.New("ARIN organization parent cycle")
				}
				seen[current.Handle] = true
				owner.ancestors = append(owner.ancestors, current)
				if current.Country != "" {
					owner.countries = append(owner.countries, current.Country)
				}
				if current.Parent == "" {
					break
				}
				parent, ok := organizations[current.Parent]
				if !ok {
					return errors.New("ARIN organization parent is absent")
				}
				current = parent
			}
			slices.Reverse(owner.countries)
			slices.Reverse(owner.ancestors)
			owner.qualityParent, err = rules.allocationQualityParent(allocation, owner.ancestors, organizations, networkParents, networkAllocations)
			if err != nil {
				return err
			}
			owners = append(owners, owner)
		}
		firstOwner := owners[0]
		orgHandle, blockType := firstOwner.allocation.organization, firstOwner.allocation.blockType
		netHandle := firstOwner.allocation.network
		registrationScope, registeredCountry := firstOwner.registrationScope, firstOwner.registeredCountry
		countryCodes := firstOwner.countries
		countryAmbiguous := false
		for _, owner := range owners[1:] {
			if owner.allocation.network != netHandle {
				netHandle = ""
			}
			if owner.allocation.organization != orgHandle {
				orgHandle = ""
			}
			if owner.allocation.blockType != blockType {
				blockType = "ambiguous"
			}
			if owner.registrationScope != registrationScope {
				registrationScope = "ambiguous"
			}
			if owner.registeredCountry != firstOwner.registeredCountry {
				countryAmbiguous = true
				registeredCountry = ""
			}
			if !slices.Equal(owner.countries, countryCodes) {
				countryCodes = nil
			}
		}
		if len(owners) > 1 {
			multipleOwnerAllocations++
		}
		if countryAmbiguous {
			conflictingCountryAllocations++
		}
		if registeredCountry == "" {
			unknownCountryAllocations++
		}
		countries := mmdbtype.Slice{}
		for _, country := range countryCodes {
			countries = append(countries, mmdbtype.String(country))
		}
		registrationScopes[registrationScope]++
		for prefix, err := range geoDb.NetworksWithin(group.prefix) {
			if err != nil {
				return err
			}
			info, err := geoDb.GetIpInfo(prefix.Addr())
			if err != nil {
				return err
			}
			registrationMismatch := registeredCountry != "" && info.CountryCode != "" && registeredCountry != info.CountryCode
			for _, prefix := range rules.partitions(prefix) {
				countryEvidence := rules.countryEvidence(group, prefix.Addr())
				geographicRisk := countryEvidence.risk(info.CountryCode, registrationMismatch)
				riskAncestors := make([][]arinOrganization, 0, len(owners))
				for _, owner := range owners {
					riskAncestors = append(riskAncestors, owner.ancestors)
				}
				riskEvidence := rules.networkRiskEvidence(riskAncestors, prefix.Addr())
				risk := geographicRisk || len(riskEvidence) != 0
				classification := rules.classifyAllocation(firstOwner.allocation, firstOwner.ancestors, firstOwner.qualityParent, prefix.Addr())
				commonClassification := true
				qualityAmbiguous := classification.qualityState == "ambiguous"
				ownerRecords := mmdbtype.Slice{}
				sources := []string{}
				for _, owner := range owners {
					ownerClassification := rules.classifyAllocation(owner.allocation, owner.ancestors, owner.qualityParent, prefix.Addr())
					if ownerClassification != classification {
						commonClassification = false
					}
					if ownerClassification.nonQuality != classification.nonQuality || ownerClassification.qualityState != classification.qualityState {
						qualityAmbiguous = true
					}
					if ownerClassification.source != "" && !slices.Contains(sources, ownerClassification.source) {
						sources = append(sources, ownerClassification.source)
					}
					if len(owners) > 1 {
						ownerCountries := mmdbtype.Slice{}
						for _, country := range owner.countries {
							ownerCountries = append(ownerCountries, mmdbtype.String(country))
						}
						ownerRecords = append(ownerRecords, mmdbtype.Map{
							"org_handle": mmdbtype.String(owner.allocation.organization), "net_handle": mmdbtype.String(owner.allocation.network),
							"net_block_type": mmdbtype.String(owner.allocation.blockType), "registration_scope": mmdbtype.String(owner.registrationScope),
							"registered_country": mmdbtype.String(owner.registeredCountry), "org_country_codes": ownerCountries,
							"non_quality": mmdbtype.Bool(ownerClassification.nonQuality), "classification_rule": mmdbtype.String(ownerClassification.ruleName),
							"classification_source": mmdbtype.String(ownerClassification.source), "reason": mmdbtype.String(ownerClassification.reason),
							"classification_org_handle":     mmdbtype.String(ownerClassification.orgHandle),
							"classification_network_handle": mmdbtype.String(ownerClassification.networkHandle),
							"quality_state":                 mmdbtype.String(ownerClassification.qualityState),
						})
					}
				}
				if !commonClassification {
					if !qualityAmbiguous && (classification.nonQuality || rules.QualityPolicyVersion == 2) {
						slices.Sort(sources)
						classification = arinClassification{nonQuality: classification.nonQuality, qualityState: classification.qualityState,
							ruleName: "owner-consensus", source: strings.Join(sources, " "),
							reason: "all incomparable direct owners agree on reviewed network use; see owner_evidence"}
					} else {
						classification = rules.unknownClassification()
						if rules.QualityPolicyVersion == 2 {
							classification.qualityState = "ambiguous"
							classification.reason = "incomparable direct owners disagree on network use; see owner_evidence"
						}
					}
				}
				data := mmdbtype.Map{
					"org_country_codes": countries, "risk": mmdbtype.Bool(risk),
					"non_quality": mmdbtype.Bool(classification.nonQuality), "classifier_version": mmdbtype.Uint32(rules.Version),
					"org_handle": mmdbtype.String(orgHandle), "net_handle": mmdbtype.String(netHandle), "registered_country": mmdbtype.String(registeredCountry),
					"associated_country": mmdbtype.String(info.CountryCode), "registration_scope": mmdbtype.String(registrationScope),
					"net_block_type": mmdbtype.String(blockType), "reason": mmdbtype.String(classification.reason),
					"classification_rule": mmdbtype.String(classification.ruleName), "classification_source": mmdbtype.String(classification.source),
					"classification_org_handle":     mmdbtype.String(classification.orgHandle),
					"classification_network_handle": mmdbtype.String(classification.networkHandle),
					"multiple_registration_owners":  mmdbtype.Bool(len(owners) > 1), "country_ambiguous": mmdbtype.Bool(countryAmbiguous),
					"non_quality_ambiguous": mmdbtype.Bool(qualityAmbiguous),
				}
				if rules.QualityPolicyVersion == 2 {
					data["quality_policy_version"] = mmdbtype.Uint32(2)
					data["quality_state"] = mmdbtype.String(classification.qualityState)
					qualityStates[classification.qualityState]++
				}
				if len(riskEvidence) != 0 {
					data["network_risk"] = mmdbtype.Bool(true)
					data["network_risk_evidence"] = riskEvidence
					networkRiskCount++
				}
				data["geographic_risk"] = mmdbtype.Bool(geographicRisk)
				countryEvidence.addRecordFields(data)
				if countryEvidence.state != "" {
					data["registration_mismatch"] = mmdbtype.Bool(registrationMismatch)
					countryEvidencePartitions[countryEvidence.state]++
				}
				if len(ownerRecords) > 0 {
					data["owner_evidence"] = ownerRecords
				}
				_, network, err := net.ParseCIDR(prefix.String())
				if err != nil {
					return err
				}
				if err := writer.Insert(network, data); err != nil {
					return err
				}
				partitions++
				if risk {
					riskCount++
				}
				if classification.nonQuality {
					nonQualityCount++
				}
				if qualityAmbiguous {
					ambiguousQualityPartitions++
				}
			}
		}
	}
	file, err := os.Create(filepath.Join(output, "arin.mmdb"))
	if err != nil {
		return err
	}
	_, writeErr := writer.WriteTo(file)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	validation, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
	if err != nil {
		return err
	}
	err = validation.Verify()
	validation.Close()
	if err != nil {
		return err
	}
	for name, path := range inputPaths {
		hash, err := hashArinBuildInput(ctx, path)
		if err != nil {
			return err
		}
		if hash != inputHashes[name] {
			return errors.New("ARIN build input changed during generation; output not published")
		}
	}
	if _, err := rules.hashCountryEvidenceSources(ctx, rulesPath, buildTime); err != nil {
		return err
	}
	manifest := map[string]any{"source": "ARIN bulk Whois", "source_url": arinDownloadUrl, "inputs_sha256": inputHashes, "built_at": buildTime, "geolite2_build_time": geoDb.BuildTime().UTC(), "classifier_version": rules.Version, "organizations": len(organizations), "source_allocations": sourceAllocationCount, "allocations": len(allocationGroups), "coalesced_equal_prefix_allocations": sourceAllocationCount - len(allocationGroups), "multiple_owner_allocations": multipleOwnerAllocations, "conflicting_country_allocations": conflictingCountryAllocations, "unknown_country_allocations": unknownCountryAllocations, "registration_scope_allocations": registrationScopes, "emitted_partitions": partitions, "risk_partitions": riskCount, "non_quality_partitions": nonQualityCount, "ambiguous_non_quality_partitions": ambiguousQualityPartitions}
	if rules.QualityPolicyVersion == 2 {
		manifest["quality_policy_version"] = rules.QualityPolicyVersion
		manifest["quality_state_partitions"] = qualityStates
	}
	manifest["network_risk_partitions"] = networkRiskCount
	if len(rules.allocationRules) != 0 {
		manifest["reviewed_subscriber_allocations"] = len(rules.allocationRules)
	}
	if len(rules.CountryRules) > 0 {
		manifest["country_policy_version"] = rules.CountryPolicyVersion
		manifest["country_evidence_sources"] = rules.CountrySources
		manifest["country_evidence_partitions"] = countryEvidencePartitions
	}
	return writeManifest(output, manifest, "arin.mmdb")
}

// Hash input content without retaining large XML/MMDB files in memory.
func hashArinBuildInput(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
