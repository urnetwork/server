package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

// A positive inference needs a globally visible route. RIS peers that see a
// prefix/origin pair measure that visibility; single-peer announcements include
// leaks, hijacks and local-only more-specifics. Explicit catalogs may lower it.
const defaultMinimumOriginPeers uint32 = 10

const originCountryPolicyWithhold = "withhold-outside-reviewed-countries"

// Use evidence and routing authority are separate. RIS establishes the observed
// origin; a reviewed catalog establishes that origin's subscriber or other use.
type subscriberOriginCatalog struct {
	Version             int                     `yaml:"version"`
	Policy              string                  `yaml:"policy"`
	MinimumOriginPeers  *uint32                 `yaml:"minimum_origin_peers"`
	OriginCountryPolicy string                  `yaml:"origin_country_policy"`
	OriginSources       []countryEvidenceSource `yaml:"origin_sources"`
	AddressRiskSources  []addressRiskSource     `yaml:"address_risk_sources"`
	RpkiSources         []rpkiSource            `yaml:"rpki_sources"`
	RegistrySources     []registrySource        `yaml:"registry_sources"`
	Operators           []subscriberOperator    `yaml:"operators"`
	byASN               map[uint32][]subscriberOperator
	byId                map[string]subscriberOperator
}

type subscriberOperator struct {
	Id        string   `yaml:"id"`
	Name      string   `yaml:"name"`
	ASNs      []uint32 `yaml:"asns"`
	Usage     string   `yaml:"usage"`
	Source    string   `yaml:"source"`
	Countries []string `yaml:"countries"`
}

// One observed origin and the number of RIS peers that saw it.
type subscriberOrigin struct {
	asn   uint32
	peers uint32
}

// visibility is the effective peer count used for inference: the least visible
// origin of this route, or an identical-origin covering route's visibility
// when that aggregate is better seen (an operator's own more-specific).
type subscriberOriginRoute struct {
	origins    []subscriberOrigin
	visibility uint32
	rpki       string
}

func (self subscriberOriginRoute) asns() []uint32 {
	asns := make([]uint32, 0, len(self.origins))
	for _, origin := range self.origins {
		asns = append(asns, origin.asn)
	}
	return asns
}

func (self subscriberOriginRoute) ownVisibility() uint32 {
	var least uint32
	for i, origin := range self.origins {
		if i == 0 || origin.peers < least {
			least = origin.peers
		}
	}
	return least
}

func (self *subscriberOriginRoute) addOrigin(asn uint32, peers uint32) {
	for i := range self.origins {
		if self.origins[i].asn == asn {
			self.origins[i].peers = max(self.origins[i].peers, peers)
			return
		}
	}
	self.origins = append(self.origins, subscriberOrigin{asn: asn, peers: peers})
	slices.SortFunc(self.origins, func(a, b subscriberOrigin) int { return int(a.asn) - int(b.asn) })
}

func subscriberOriginNetworkUseRisk(usage string) bool {
	return slices.Contains([]string{"virtual_isp", "proxy", "vpn", "tor"}, usage)
}

func (self subscriberOriginCatalog) minimumOriginPeers() uint32 {
	if self.MinimumOriginPeers == nil {
		return defaultMinimumOriginPeers
	}
	return *self.MinimumOriginPeers
}

// Reviewed countries of every operator identified on a route. The catalog
// records review context; outside that context the ISP inference is withheld.
func (self subscriberOriginCatalog) reviewedCountries(operatorIds mmdbtype.Slice) map[string]bool {
	countries := map[string]bool{}
	for _, value := range operatorIds {
		id, _ := value.(mmdbtype.String)
		for _, country := range self.byId[string(id)].Countries {
			countries[strings.ToLower(country)] = true
		}
	}
	return countries
}

func validateSubscriberEvidenceSource(source countryEvidenceSource, seen map[string]bool) error {
	location, err := url.Parse(source.Url)
	digest, hashErr := hex.DecodeString(source.Sha256)
	if source.Id == "" || seen[source.Id] || strings.ContainsAny(source.Id, "/\\") || err != nil || location.Scheme != "https" || location.Host == "" || location.User != nil ||
		!filepath.IsLocal(source.File) || source.File == "." || hashErr != nil || len(digest) != 32 || source.Sha256 != strings.ToLower(source.Sha256) ||
		source.ObservedAt.IsZero() || !source.ObservedAt.Before(source.ExpiresAt) || source.ExpiresAt.Sub(source.ObservedAt) > 48*time.Hour {
		return errors.New("evidence source requires unique id, public HTTPS URL, relative snapshot, SHA256 and at most 48-hour freshness")
	}
	seen[source.Id] = true
	return nil
}

func loadSubscriberOriginCatalog(path string) (subscriberOriginCatalog, error) {
	var catalog subscriberOriginCatalog
	f, err := os.Open(path)
	if err != nil {
		return catalog, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<20 {
		return catalog, errors.New("subscriber origin catalog must be a bounded regular file")
	}
	decoder := yaml.NewDecoder(io.LimitReader(f, 16<<20))
	decoder.KnownFields(true)
	if err := decoder.Decode(&catalog); err != nil {
		return catalog, errors.New("invalid subscriber origin catalog")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return catalog, errors.New("subscriber origin catalog requires one document")
	}
	if catalog.Version != 1 || catalog.Policy != "identified-subscriber-default" || len(catalog.OriginSources) == 0 || len(catalog.Operators) == 0 {
		return catalog, errors.New("subscriber origin catalog requires version1, identified-subscriber-default, sources and operators")
	}
	if catalog.MinimumOriginPeers != nil && *catalog.MinimumOriginPeers == 0 {
		return catalog, errors.New("minimum_origin_peers must be at least one observed RIS peer")
	}
	if catalog.OriginCountryPolicy != "" && catalog.OriginCountryPolicy != originCountryPolicyWithhold {
		return catalog, errors.New("unsupported origin country policy")
	}
	seen := map[string]bool{}
	for _, source := range catalog.OriginSources {
		if err := validateSubscriberEvidenceSource(source, seen); err != nil {
			return catalog, err
		}
	}
	for _, source := range catalog.AddressRiskSources {
		if err := validateSubscriberEvidenceSource(source.countryEvidenceSource, seen); err != nil {
			return catalog, err
		}
		if !slices.Contains(addressRiskFormats, source.Format) || !subscriberOriginNetworkUseRisk(source.Category) || strings.TrimSpace(source.Reason) == "" {
			return catalog, errors.New("address risk source requires a supported list format, a virtual ISP, proxy, VPN or Tor category and a reason")
		}
	}
	for _, source := range catalog.RpkiSources {
		if err := validateSubscriberEvidenceSource(source.countryEvidenceSource, seen); err != nil {
			return catalog, err
		}
		if source.Format != rpkiFormatClientJson && source.Format != rpkiFormatRoutinatorCsv {
			return catalog, errors.New("RPKI source requires the rpki-client-json or routinator-csv format")
		}
	}
	for _, source := range catalog.RegistrySources {
		if err := validateSubscriberEvidenceSource(source.countryEvidenceSource, seen); err != nil {
			return catalog, err
		}
		if source.Format != registryFormatNroDelegatedStats {
			return catalog, errors.New("registry source requires the nro-delegated-stats format")
		}
	}
	seen = map[string]bool{}
	catalog.byASN = map[uint32][]subscriberOperator{}
	catalog.byId = map[string]subscriberOperator{}
	for _, operator := range catalog.Operators {
		if operator.Id == "" || seen[operator.Id] || strings.TrimSpace(operator.Name) == "" || strings.TrimSpace(operator.Source) == "" || len(operator.ASNs) == 0 || len(operator.Countries) == 0 ||
			!slices.Contains([]string{"subscriber", "hosting", "transit", "virtual_isp", "proxy", "vpn", "tor"}, operator.Usage) {
			return catalog, errors.New("operator requires unique identity, reviewed use, source, countries and ASNs")
		}
		seen[operator.Id] = true
		asns := map[uint32]bool{}
		for _, asn := range operator.ASNs {
			if !publicOriginASN(asn) || asns[asn] {
				return catalog, errors.New("operator requires distinct public ASNs")
			}
			asns[asn] = true
			catalog.byASN[asn] = append(catalog.byASN[asn], operator)
		}
		for _, country := range operator.Countries {
			if !knownCountryCode(country) {
				return catalog, errors.New("operator has an invalid country code")
			}
		}
		catalog.byId[operator.Id] = operator
	}
	return catalog, nil
}

func publicOriginASN(asn uint32) bool {
	return asn != 0 && asn != 23456 && asn != 65535 && asn != 4294967295 && !(64512 <= asn && asn <= 65534) && !(4200000000 <= asn && asn <= 4294967294)
}

// Keep routing observations outside the IPv4 subtree and its MMDB aliases.
// These are address-encoding aliases, not independent native IPv6 networks.
func subscriberOriginAliasesIPv4(prefix netip.Prefix) bool {
	if prefix.Addr().Is4() {
		return false
	}
	root := netip.MustParsePrefix("::/96")
	if prefix.Overlaps(root) {
		return true
	}
	for _, alias := range []netip.Prefix{netip.MustParsePrefix("::ffff:0:0/96"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2002::/16")} {
		if prefix.Bits() >= alias.Bits() && alias.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

// All routes participate, including unidentified more-specific origins. Dropping
// unknown routes would let a broader identified ISP swallow another operator.
// Multiple-origin records are merged independent of file/line order.
// Returns the row count and the snapshot's own generation time.
func readSubscriberOrigins(ctx context.Context, reader io.Reader, at time.Time, routes map[netip.Prefix]subscriberOriginRoute) (int, time.Time, error) {
	zip, err := gzip.NewReader(reader)
	if err != nil {
		return 0, time.Time{}, errors.New("invalid compressed origin snapshot")
	}
	defer zip.Close()
	scanner := bufio.NewScanner(io.LimitReader(zip, (256<<20)+1))
	scanner.Buffer(make([]byte, 4096), 4096)
	rows, total, generated := 0, 0, false
	var generation time.Time
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return 0, time.Time{}, err
		}
		line := scanner.Text()
		total += len(line) + 1
		if total > 256<<20 {
			return 0, time.Time{}, errors.New("origin snapshot exceeds decompressed size limit")
		}
		if strings.HasPrefix(line, "% This file was generated at ") {
			clock := strings.TrimSuffix(strings.TrimPrefix(line, "% This file was generated at "), ".")
			published, err := time.Parse("Mon Jan _2 15:04:05 MST 2006", clock)
			if err != nil || published.After(at) || at.Sub(published) > 48*time.Hour || generated {
				return 0, time.Time{}, errors.New("origin snapshot has missing, conflicting, future or stale generation")
			}
			generated, generation = true, published
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "%") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return 0, time.Time{}, errors.New("origin snapshot has a malformed route")
		}
		prefix, err := netip.ParsePrefix(fields[1])
		peers, peerErr := strconv.ParseUint(fields[2], 10, 32)
		if err != nil || prefix != prefix.Masked() || peerErr != nil || peers == 0 {
			return 0, time.Time{}, errors.New("origin snapshot has invalid prefix or peer count")
		}
		origins := fields[0]
		if strings.HasPrefix(origins, "{") && strings.HasSuffix(origins, "}") {
			origins = origins[1 : len(origins)-1]
		}
		route := routes[prefix]
		for _, value := range strings.Split(origins, ",") {
			asn, err := strconv.ParseUint(value, 10, 32)
			if err != nil || asn == 0 {
				return 0, time.Time{}, errors.New("origin snapshot has invalid ASN")
			}
			route.addOrigin(uint32(asn), uint32(peers))
		}
		rows++
		// A default route does not identify the entire Internet. RIS may also
		// observe IPv4-mapped/compatible, Teredo or 6to4 announcements; MMDB
		// aliases those addresses to IPv4, so they must not override native
		// IPv4 evidence or fail insertion into the complete origin tree.
		if prefix.Bits() == 0 || subscriberOriginAliasesIPv4(prefix) {
			continue
		}
		routes[prefix] = route
	}
	if err := scanner.Err(); err != nil {
		return 0, time.Time{}, errors.New("origin snapshot is truncated or unreadable")
	}
	if !generated || rows == 0 {
		return 0, time.Time{}, errors.New("origin snapshot lacks a complete dated route table")
	}
	return rows, generation, nil
}

func sortedSubscriberOriginPrefixes(routes map[netip.Prefix]subscriberOriginRoute) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(routes))
	for prefix := range routes {
		prefixes = append(prefixes, prefix)
	}
	slices.SortFunc(prefixes, func(a, b netip.Prefix) int {
		if a.Bits() != b.Bits() {
			return a.Bits() - b.Bits()
		}
		return a.Addr().Compare(b.Addr())
	})
	return prefixes
}

// The nearest covering observed route, if any.
func coveringSubscriberOrigin(routes map[netip.Prefix]subscriberOriginRoute, prefix netip.Prefix) (netip.Prefix, bool) {
	for bits := prefix.Bits() - 1; bits >= 1; bits-- {
		candidate := netip.PrefixFrom(prefix.Addr(), bits).Masked()
		if _, ok := routes[candidate]; ok {
			return candidate, true
		}
	}
	return netip.Prefix{}, false
}

// An operator's own more-specific inherits the visibility and origin validity
// of its identically originated aggregate; the aggregate establishes the
// identity and carries the Internet's traffic, the more-specific only engineers
// it. A different origin set inherits nothing, so an unauthorized or barely
// seen announcement by another party still stands on its own evidence.
func resolveSubscriberOriginEvidence(ctx context.Context, routes map[netip.Prefix]subscriberOriginRoute, authorizations *rpkiAuthorizations, byASN map[uint32][]subscriberOperator) ([]netip.Prefix, error) {
	prefixes := sortedSubscriberOriginPrefixes(routes)
	for _, prefix := range prefixes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		route := routes[prefix]
		route.visibility = route.ownVisibility()
		route.rpki = authorizations.routeValidity(prefix, route)
		if covering, ok := coveringSubscriberOrigin(routes, prefix); ok {
			parent := routes[covering]
			if slices.Equal(parent.asns(), route.asns()) || sameReviewedOperators(parent, route, byASN) {
				route.visibility = max(route.visibility, parent.visibility)
				if route.rpki == "invalid" && (parent.rpki == "valid" || parent.rpki == "valid-aggregate") {
					route.rpki = "valid-aggregate"
				}
			}
		}
		routes[prefix] = route
	}
	return prefixes, nil
}

// Sibling ASNs of one reviewed operator are the same identity. Any unreviewed
// origin on either side keeps the routes distinct.
func sameReviewedOperators(a, b subscriberOriginRoute, byASN map[uint32][]subscriberOperator) bool {
	identities := func(route subscriberOriginRoute) ([]string, bool) {
		ids := []string{}
		for _, origin := range route.origins {
			operators := byASN[origin.asn]
			if len(operators) == 0 {
				return nil, false
			}
			for _, operator := range operators {
				ids = append(ids, operator.Id)
			}
		}
		slices.Sort(ids)
		return slices.Compact(ids), true
	}
	left, ok := identities(a)
	right, reviewed := identities(b)
	return ok && reviewed && slices.Equal(left, right)
}

func subscriberOriginDecision(route subscriberOriginRoute, byASN map[uint32][]subscriberOperator, minimumPeers uint32) mmdbtype.Map {
	rpkiValidity := route.rpki
	state := "subscriber"
	unknown, negative := false, false
	ids, sources := []string{}, []string{}
	asns, risks := mmdbtype.Slice{}, mmdbtype.Slice{}
	seen := map[string]bool{}
	for _, origin := range route.origins {
		asns = append(asns, mmdbtype.Uint32(origin.asn))
		operators := byASN[origin.asn]
		if len(operators) == 0 {
			unknown = true
		}
		for _, operator := range operators {
			negative = negative || operator.Usage != "subscriber"
			if seen[operator.Id] {
				continue
			}
			seen[operator.Id] = true
			ids, sources = append(ids, operator.Id), append(sources, operator.Source)
			if subscriberOriginNetworkUseRisk(operator.Usage) {
				risks = append(risks, mmdbtype.Map{"rule": mmdbtype.String("origin-" + operator.Id), "category": mmdbtype.String(operator.Usage), "source": mmdbtype.String(operator.Source), "reason": mmdbtype.String("reviewed origin operator network use")})
			}
		}
	}
	if negative {
		state = "excluded"
	} else if len(ids) == 0 {
		state = "unknown"
	} else if unknown {
		state = "ambiguous"
	}
	if state == "unknown" {
		return mmdbtype.Map{"state": mmdbtype.String(state)}
	}
	slices.Sort(ids)
	slices.Sort(sources)
	operators := mmdbtype.Slice{}
	for _, id := range ids {
		operators = append(operators, mmdbtype.String(id))
	}
	decision := mmdbtype.Map{"state": mmdbtype.String(state), "asns": asns, "operators": operators, "source": mmdbtype.String(strings.Join(slices.Compact(sources), " ")), "risk_evidence": risks, "peers": mmdbtype.Uint32(route.visibility)}
	if rpkiValidity != "" && rpkiValidity != "unchecked" {
		decision["rpki"] = mmdbtype.String(rpkiValidity)
	}
	// Negative and conflicting evidence applies at any visibility or validity;
	// only the positive inference needs a route the Internet actually sees
	// from an authorized origin.
	if state == "subscriber" && route.visibility < minimumPeers {
		return withholdSubscriberOrigin(decision, "insufficient-origin-visibility")
	}
	if state == "subscriber" && rpkiValidity == "invalid" {
		return withholdSubscriberOrigin(decision, "rpki-invalid-origin")
	}
	return decision
}

// A withheld decision records the identified origin without inferring from it.
func withholdSubscriberOrigin(origin mmdbtype.Map, reason string) mmdbtype.Map {
	withheld := make(mmdbtype.Map, len(origin)+1)
	for key, value := range origin {
		withheld[key] = value
	}
	withheld["state"] = mmdbtype.String("withheld")
	withheld["withheld_reason"] = mmdbtype.String(reason)
	delete(withheld, "risk_evidence")
	return withheld
}

// A subscriber identity supplies an inferred clean default, not a waiver of
// another discriminator. Registry unknown/missing child-use evidence alone is
// insufficient to reject the identified ISP. Known use and risk always survive.
func augmentSubscriberRecord(base, origin mmdbtype.Map) (mmdbtype.Map, error) {
	if _, augmented := base["origin_use_state"]; augmented || base["subscriber_evidence_kind"] == mmdbtype.String("isp_inferred") {
		return nil, errors.New("subscriber augmentation requires an unaugmented registration base, not stale inferred approvals")
	}
	if _, augmented := base["address_risk_source_ids"]; augmented {
		return nil, errors.New("subscriber augmentation requires an unaugmented registration base, not prior address-level risk")
	}
	if base["classifier_version"] != mmdbtype.Uint32(1) || base["quality_policy_version"] != mmdbtype.Uint32(2) {
		return nil, errors.New("subscriber augmentation requires a policy-two base database")
	}
	state, ok := base["quality_state"].(mmdbtype.String)
	nonQuality, hasNonQuality := base["non_quality"].(mmdbtype.Bool)
	_, hasRisk := base["risk"].(mmdbtype.Bool)
	if !ok || !hasNonQuality || !hasRisk || !slices.Contains([]mmdbtype.String{"subscriber", "unknown", "excluded", "ambiguous"}, state) || bool(nonQuality) != (state != "subscriber") {
		return nil, errors.New("subscriber augmentation found inconsistent base evidence")
	}
	if origin == nil || origin["state"] == mmdbtype.String("unknown") {
		return base, nil
	}
	data := make(mmdbtype.Map, len(base)+7)
	for key, value := range base {
		data[key] = value
	}
	data["origin_use_state"], data["origin_asns"], data["origin_operator_ids"], data["origin_evidence_source"] = origin["state"], origin["asns"], origin["operators"], origin["source"]
	if peers, ok := origin["peers"].(mmdbtype.Uint32); ok {
		data["origin_peers"] = peers
	}
	if validity, ok := origin["rpki"].(mmdbtype.String); ok {
		data["origin_rpki_validity"] = validity
	}
	if origin["state"] == mmdbtype.String("withheld") {
		// The identity is recorded for review; the base decision is unchanged.
		data["origin_withheld_reason"] = origin["withheld_reason"]
		return data, nil
	}
	if risks, ok := origin["risk_evidence"].(mmdbtype.Slice); ok && len(risks) != 0 {
		old, _ := data["network_risk_evidence"].(mmdbtype.Slice)
		data["network_risk_evidence"] = append(slices.Clone(old), risks...)
		data["network_risk"], data["risk"] = mmdbtype.Bool(true), mmdbtype.Bool(true)
	}
	if origin["state"] == mmdbtype.String("excluded") || origin["state"] == mmdbtype.String("ambiguous") {
		if state != "excluded" && state != "ambiguous" {
			data["quality_state"], data["non_quality"] = origin["state"], mmdbtype.Bool(true)
			data["non_quality_ambiguous"] = mmdbtype.Bool(origin["state"] == mmdbtype.String("ambiguous"))
			data["reason"] = mmdbtype.String("additional origin network-use evidence excludes subscriber default")
		}
		return data, nil
	}
	if state == "unknown" {
		data["quality_state"], data["non_quality"] = mmdbtype.String("subscriber"), mmdbtype.Bool(false)
		data["subscriber_evidence_kind"] = mmdbtype.String("isp_inferred")
		data["classification_rule"] = mmdbtype.String("identified-subscriber-isp-default")
		data["classification_source"] = origin["source"]
		data["reason"] = mmdbtype.String("identified subscriber ISP defaults clean in the absence of additional network-use discrimination")
	}
	return data, nil
}

// Reads every pinned origin snapshot and resolves route visibility and validity.
func loadSubscriberOriginRoutes(ctx context.Context, catalog subscriberOriginCatalog, catalogPath string, at time.Time) (map[netip.Prefix]subscriberOriginRoute, []netip.Prefix, int, error) {
	authorizations, err := loadRpkiAuthorizations(ctx, catalogPath, catalog.RpkiSources)
	if err != nil {
		return nil, nil, 0, err
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return nil, nil, 0, err
	}
	defer root.Close()
	routes := map[netip.Prefix]subscriberOriginRoute{}
	rows := 0
	for _, source := range catalog.OriginSources {
		f, err := root.Open(source.File)
		if err != nil {
			return nil, nil, 0, err
		}
		n, _, parseErr := readSubscriberOrigins(ctx, f, at, routes)
		closeErr := f.Close()
		if parseErr != nil {
			return nil, nil, 0, parseErr
		}
		if closeErr != nil {
			return nil, nil, 0, closeErr
		}
		rows += n
	}
	if len(routes) == 0 {
		return nil, nil, 0, errors.New("origin snapshots contain no usable routes")
	}
	prefixes, err := resolveSubscriberOriginEvidence(ctx, routes, authorizations, catalog.byASN)
	if err != nil {
		return nil, nil, 0, err
	}
	return routes, prefixes, rows, nil
}

// The snapshot hashes reuse the bounded, rooted, regular-file and freshness checks.
func (self subscriberOriginCatalog) evidenceSources() classificationRules {
	sources := slices.Clone(self.OriginSources)
	for _, source := range self.AddressRiskSources {
		sources = append(sources, source.countryEvidenceSource)
	}
	for _, source := range self.RpkiSources {
		sources = append(sources, source.countryEvidenceSource)
	}
	for _, source := range self.RegistrySources {
		sources = append(sources, source.countryEvidenceSource)
	}
	return classificationRules{CountrySources: sources}
}

func (self subscriberOriginCatalog) hashEvidenceSources(ctx context.Context, catalogPath string, at time.Time) (map[string]string, error) {
	hashes, err := self.evidenceSources().hashCountryEvidenceSources(ctx, catalogPath, at)
	if err != nil {
		return nil, err
	}
	named := map[string]string{}
	for _, source := range self.OriginSources {
		named["origin/"+source.Id] = hashes["country_evidence/"+source.Id]
	}
	for _, source := range self.AddressRiskSources {
		named["address_risk/"+source.Id] = hashes["country_evidence/"+source.Id]
	}
	for _, source := range self.RpkiSources {
		named["rpki/"+source.Id] = hashes["country_evidence/"+source.Id]
	}
	for _, source := range self.RegistrySources {
		named["registry/"+source.Id] = hashes["country_evidence/"+source.Id]
	}
	return named, nil
}

func augmentSubscriberDatabase(ctx context.Context, basePath, catalogPath, geolite2Path, output string, at time.Time) error {
	if at.Unix() <= 0 {
		return errors.New("subscriber build requires a positive build time")
	}
	catalog, err := loadSubscriberOriginCatalog(catalogPath)
	if err != nil {
		return err
	}
	if (catalog.OriginCountryPolicy != "") != (geolite2Path != "") {
		return errors.New("the reviewed origin country policy and the geolite2 input must be supplied together")
	}
	inputPaths := map[string]string{"base_arin_mmdb": basePath, "subscriber_catalog": catalogPath}
	if geolite2Path != "" {
		inputPaths["geolite2"] = geolite2Path
	}
	hashes := map[string]string{}
	for key, path := range inputPaths {
		hashes[key], err = hashArinBuildInput(ctx, path)
		if err != nil {
			return err
		}
	}
	sourceHashes, err := catalog.hashEvidenceSources(ctx, catalogPath, at)
	if err != nil {
		return err
	}
	for key, value := range sourceHashes {
		hashes[key] = value
	}
	routes, prefixes, rows, err := loadSubscriberOriginRoutes(ctx, catalog, catalogPath, at)
	if err != nil {
		return err
	}
	usableRoutePrefixes := len(routes)
	minimumPeers := catalog.minimumOriginPeers()
	rpkiStates := map[string]int{}
	originWriter, err := mmdbwriter.New(mmdbwriter.Options{BuildEpoch: at.Unix(), DatabaseType: "reviewed subscriber origins", IncludeReservedNetworks: true, RecordSize: 32, Description: map[string]string{"en": "reviewed origin network use"}})
	if err != nil {
		return err
	}
	for _, prefix := range prefixes {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, network, _ := net.ParseCIDR(prefix.String())
		if len(catalog.RpkiSources) != 0 {
			rpkiStates[routes[prefix].rpki]++
		}
		if err := originWriter.Insert(network, subscriberOriginDecision(routes[prefix], catalog.byASN, minimumPeers)); err != nil {
			return err
		}
	}
	var originBytes bytes.Buffer
	if _, err := originWriter.WriteTo(&originBytes); err != nil {
		return err
	}
	origins, err := mmdb.OpenBytes(originBytes.Bytes())
	if err != nil {
		return err
	}
	defer origins.Close()
	routes, prefixes, originWriter = nil, nil, nil
	var geo *server.IpInfoDatabase
	if geolite2Path != "" {
		geo, err = server.OpenIpInfoDatabase(geolite2Path)
		if err != nil {
			return err
		}
		defer geo.Close()
	}
	base, err := mmdb.Open(basePath)
	if err != nil {
		return err
	}
	defer base.Close()
	if err := base.Verify(); err != nil {
		return err
	}
	writer, err := mmdbwriter.New(mmdbwriter.Options{BuildEpoch: at.Unix(), DatabaseType: "urnetwork arindb", IncludeReservedNetworks: true, RecordSize: 32, Description: map[string]string{"en": "registration and reviewed global subscriber origin evidence"}})
	if err != nil {
		return err
	}
	states := map[string]int{}
	withheld := map[string]int{}
	baseLeaves, emitted, inferred := 0, 0, 0
	insert := func(prefix netip.Prefix, data mmdbtype.Map) error {
		_, network, _ := net.ParseCIDR(prefix.String())
		if err := writer.Insert(network, data); err != nil {
			return err
		}
		states[fmt.Sprint(data["quality_state"])]++
		emitted++
		if data["subscriber_evidence_kind"] == mmdbtype.String("isp_inferred") {
			inferred++
		}
		if reason, ok := data["origin_withheld_reason"].(mmdbtype.String); ok {
			withheld[string(reason)]++
		}
		return nil
	}
	for original := range base.Networks(mmdb.IncludeNetworksWithoutData()) {
		if err := ctx.Err(); err != nil {
			return err
		}
		var record mmdbtype.Map
		if err := original.Decode(&record); err != nil {
			return err
		}
		baseLeaves++
		for origin := range origins.NetworksWithin(original.Prefix(), mmdb.IncludeNetworksWithoutData()) {
			var evidence mmdbtype.Map
			if err := origin.Decode(&evidence); err != nil {
				return err
			}
			data, err := augmentSubscriberRecord(record, evidence)
			if err != nil {
				return err
			}
			prefix := origin.Prefix()
			if prefix.Bits() < original.Prefix().Bits() {
				prefix = original.Prefix()
			}
			if err := insert(prefix, data); err != nil {
				return err
			}
			if geo == nil || data["subscriber_evidence_kind"] != mmdbtype.String("isp_inferred") {
				continue
			}
			// An inferred approval outside every reviewed operator country is
			// withheld for review at the associated geography's own boundary.
			reviewed := catalog.reviewedCountries(evidence["operators"].(mmdbtype.Slice))
			for cell, err := range geo.NetworksWithin(prefix) {
				if err != nil {
					return err
				}
				info, err := geo.GetIpInfo(cell.Addr())
				if err != nil {
					return err
				}
				country := strings.ToLower(info.CountryCode)
				if country == "" || reviewed[country] {
					continue
				}
				outside, err := augmentSubscriberRecord(record, withholdSubscriberOrigin(evidence, "outside-reviewed-countries"))
				if err != nil {
					return err
				}
				outside["associated_country"] = mmdbtype.String(info.CountryCode)
				if err := insert(cell, outside); err != nil {
					return err
				}
			}
		}
	}
	if baseLeaves == 0 {
		return errors.New("subscriber base database is empty")
	}
	addressRiskEntries, err := applyAddressRiskSources(ctx, writer, catalog, catalogPath)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(output, "arin.mmdb"))
	if err != nil {
		return err
	}
	_, writeErr := writer.WriteTo(f)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
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
	for key, path := range inputPaths {
		digest, err := hashArinBuildInput(ctx, path)
		if err != nil {
			return err
		}
		if hashes[key] != digest {
			return errors.New("subscriber build input changed during generation")
		}
	}
	if _, err := catalog.hashEvidenceSources(ctx, catalogPath, at); err != nil {
		return err
	}
	manifest := map[string]any{"source": "reviewed subscriber operators and RIPE RIS origins", "built_at": at, "classifier_version": 1, "quality_policy_version": 2, "subscriber_origin_policy": "identified-subscriber-default", "minimum_origin_peers": minimumPeers, "inputs_sha256": hashes, "origin_sources": catalog.OriginSources, "origin_rows": rows, "usable_origin_prefixes": usableRoutePrefixes, "reviewed_operators": len(catalog.Operators), "base_leaves": baseLeaves, "emitted_partitions": emitted, "quality_state_partitions": states, "isp_inferred_partitions": inferred, "withheld_partitions": withheld}
	if catalog.OriginCountryPolicy != "" {
		manifest["origin_country_policy"] = catalog.OriginCountryPolicy
		manifest["geolite2_build_time"] = geo.BuildTime().UTC()
	}
	if len(catalog.AddressRiskSources) != 0 {
		manifest["address_risk_sources"] = catalog.AddressRiskSources
		manifest["address_risk_entries"] = addressRiskEntries
	}
	if len(catalog.RpkiSources) != 0 {
		manifest["rpki_sources"] = catalog.RpkiSources
		manifest["rpki_route_validity"] = rpkiStates
	}
	return writeManifest(output, manifest, "arin.mmdb")
}
