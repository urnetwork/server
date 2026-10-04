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
	"gopkg.in/yaml.v3"
)

// Use evidence and routing authority are separate. RIS establishes the observed
// origin; a reviewed catalog establishes that origin's subscriber or other use.
type subscriberOriginCatalog struct {
	Version       int                     `yaml:"version"`
	Policy        string                  `yaml:"policy"`
	OriginSources []countryEvidenceSource `yaml:"origin_sources"`
	Operators     []subscriberOperator    `yaml:"operators"`
}

type subscriberOperator struct {
	Id        string   `yaml:"id"`
	Name      string   `yaml:"name"`
	ASNs      []uint32 `yaml:"asns"`
	Usage     string   `yaml:"usage"`
	Source    string   `yaml:"source"`
	Countries []string `yaml:"countries"`
}

type subscriberOriginRoute struct {
	asns []uint32
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
	seen := map[string]bool{}
	for _, source := range catalog.OriginSources {
		location, err := url.Parse(source.Url)
		digest, hashErr := hex.DecodeString(source.Sha256)
		if source.Id == "" || seen[source.Id] || strings.ContainsAny(source.Id, "/\\") || err != nil || location.Scheme != "https" || location.Host == "" || location.User != nil ||
			!filepath.IsLocal(source.File) || source.File == "." || hashErr != nil || len(digest) != 32 || source.Sha256 != strings.ToLower(source.Sha256) ||
			source.ObservedAt.IsZero() || !source.ObservedAt.Before(source.ExpiresAt) || source.ExpiresAt.Sub(source.ObservedAt) > 48*time.Hour {
			return catalog, errors.New("origin source requires unique id, public HTTPS URL, relative snapshot, SHA256 and at most 48-hour freshness")
		}
		seen[source.Id] = true
	}
	seen = map[string]bool{}
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
		}
		for _, country := range operator.Countries {
			if !knownCountryCode(country) {
				return catalog, errors.New("operator has an invalid country code")
			}
		}
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
func readSubscriberOrigins(ctx context.Context, reader io.Reader, at time.Time, routes map[netip.Prefix]subscriberOriginRoute) (int, error) {
	zip, err := gzip.NewReader(reader)
	if err != nil {
		return 0, errors.New("invalid compressed origin snapshot")
	}
	defer zip.Close()
	scanner := bufio.NewScanner(io.LimitReader(zip, (256<<20)+1))
	scanner.Buffer(make([]byte, 4096), 4096)
	rows, total, generated := 0, 0, false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		line := scanner.Text()
		total += len(line) + 1
		if total > 256<<20 {
			return 0, errors.New("origin snapshot exceeds decompressed size limit")
		}
		if strings.HasPrefix(line, "% This file was generated at ") {
			clock := strings.TrimSuffix(strings.TrimPrefix(line, "% This file was generated at "), ".")
			published, err := time.Parse("Mon Jan _2 15:04:05 MST 2006", clock)
			if err != nil || published.After(at) || at.Sub(published) > 48*time.Hour || generated {
				return 0, errors.New("origin snapshot has missing, conflicting, future or stale generation")
			}
			generated = true
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "%") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return 0, errors.New("origin snapshot has a malformed route")
		}
		prefix, err := netip.ParsePrefix(fields[1])
		peers, peerErr := strconv.ParseUint(fields[2], 10, 32)
		if err != nil || prefix != prefix.Masked() || peerErr != nil || peers == 0 {
			return 0, errors.New("origin snapshot has invalid prefix or peer count")
		}
		origins := fields[0]
		if strings.HasPrefix(origins, "{") && strings.HasSuffix(origins, "}") {
			origins = origins[1 : len(origins)-1]
		}
		route := routes[prefix]
		for _, value := range strings.Split(origins, ",") {
			asn, err := strconv.ParseUint(value, 10, 32)
			if err != nil || asn == 0 {
				return 0, errors.New("origin snapshot has invalid ASN")
			}
			if !slices.Contains(route.asns, uint32(asn)) {
				route.asns = append(route.asns, uint32(asn))
			}
		}
		rows++
		// A default route does not identify the entire Internet. RIS may also
		// observe IPv4-mapped/compatible, Teredo or 6to4 announcements; MMDB
		// aliases those addresses to IPv4, so they must not override native
		// IPv4 evidence or fail insertion into the complete origin tree.
		if prefix.Bits() == 0 || subscriberOriginAliasesIPv4(prefix) {
			continue
		}
		slices.Sort(route.asns)
		routes[prefix] = route
	}
	if err := scanner.Err(); err != nil {
		return 0, errors.New("origin snapshot is truncated or unreadable")
	}
	if !generated || rows == 0 {
		return 0, errors.New("origin snapshot lacks a complete dated route table")
	}
	return rows, nil
}

func subscriberOriginDecision(route subscriberOriginRoute, byASN map[uint32][]subscriberOperator) mmdbtype.Map {
	state := "subscriber"
	unknown, negative := false, false
	ids, sources := []string{}, []string{}
	asns, risks := mmdbtype.Slice{}, mmdbtype.Slice{}
	seen := map[string]bool{}
	for _, asn := range route.asns {
		asns = append(asns, mmdbtype.Uint32(asn))
		operators := byASN[asn]
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
			if slices.Contains([]string{"virtual_isp", "proxy", "vpn", "tor"}, operator.Usage) {
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
		// Routing identity remains useful when network use is unreviewed. A
		// later current-owner capture can prioritize these public ASNs without
		// exporting a provider address or treating routing as subscriber proof.
		return mmdbtype.Map{"state": mmdbtype.String(state), "asns": asns}
	}
	slices.Sort(ids)
	slices.Sort(sources)
	operators := mmdbtype.Slice{}
	for _, id := range ids {
		operators = append(operators, mmdbtype.String(id))
	}
	return mmdbtype.Map{"state": mmdbtype.String(state), "asns": asns, "operators": operators, "source": mmdbtype.String(strings.Join(slices.Compact(sources), " ")), "risk_evidence": risks}
}

// A subscriber identity supplies an inferred clean default, not a waiver of
// another discriminator. Registry unknown/missing child-use evidence alone is
// insufficient to reject the identified ISP. Known use and risk always survive.
func augmentSubscriberRecord(base, origin mmdbtype.Map) (mmdbtype.Map, error) {
	if _, augmented := base["origin_use_state"]; augmented || base["subscriber_evidence_kind"] == mmdbtype.String("isp_inferred") {
		return nil, errors.New("subscriber augmentation requires an unaugmented registration base, not stale inferred approvals")
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
	if origin == nil {
		return base, nil
	}
	data := make(mmdbtype.Map, len(base)+5)
	for key, value := range base {
		data[key] = value
	}
	data["origin_use_state"], data["origin_asns"] = origin["state"], origin["asns"]
	if origin["state"] == mmdbtype.String("unknown") {
		// Preserve independent direct subscriber approval and every existing
		// exclusion/risk discriminator. Unknown origin use is provenance only.
		return data, nil
	}
	data["origin_operator_ids"], data["origin_evidence_source"] = origin["operators"], origin["source"]
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

func augmentSubscriberDatabase(ctx context.Context, basePath, catalogPath, output string, at time.Time) error {
	if at.Unix() <= 0 {
		return errors.New("subscriber build requires a positive build time")
	}
	catalog, err := loadSubscriberOriginCatalog(catalogPath)
	if err != nil {
		return err
	}
	hashes := map[string]string{}
	for key, path := range map[string]string{"base_arin_mmdb": basePath, "subscriber_catalog": catalogPath} {
		hashes[key], err = hashArinBuildInput(ctx, path)
		if err != nil {
			return err
		}
	}
	// Reuse the bounded, rooted, regular-file and freshness checks for snapshots.
	sources := classificationRules{CountrySources: catalog.OriginSources}
	originHashes, err := sources.hashCountryEvidenceSources(ctx, catalogPath, at)
	if err != nil {
		return err
	}
	for key, value := range originHashes {
		hashes[strings.Replace(key, "country_evidence/", "origin/", 1)] = value
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return err
	}
	defer root.Close()
	routes := map[netip.Prefix]subscriberOriginRoute{}
	rows := 0
	for _, source := range catalog.OriginSources {
		f, err := root.Open(source.File)
		if err != nil {
			return err
		}
		n, parseErr := readSubscriberOrigins(ctx, f, at, routes)
		closeErr := f.Close()
		if parseErr != nil {
			return parseErr
		}
		if closeErr != nil {
			return closeErr
		}
		rows += n
	}
	if len(routes) == 0 {
		return errors.New("origin snapshots contain no usable routes")
	}
	usableRoutePrefixes := len(routes)
	byASN := map[uint32][]subscriberOperator{}
	for _, operator := range catalog.Operators {
		for _, asn := range operator.ASNs {
			byASN[asn] = append(byASN[asn], operator)
		}
	}
	originWriter, err := mmdbwriter.New(mmdbwriter.Options{BuildEpoch: at.Unix(), DatabaseType: "reviewed subscriber origins", IncludeReservedNetworks: true, RecordSize: 32, Description: map[string]string{"en": "reviewed origin network use"}})
	if err != nil {
		return err
	}
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
	for _, prefix := range prefixes {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, network, _ := net.ParseCIDR(prefix.String())
		if err := originWriter.Insert(network, subscriberOriginDecision(routes[prefix], byASN)); err != nil {
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
	baseLeaves, emitted, inferred := 0, 0, 0
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
			_, network, _ := net.ParseCIDR(prefix.String())
			if err := writer.Insert(network, data); err != nil {
				return err
			}
			states[fmt.Sprint(data["quality_state"])]++
			emitted++
			if data["subscriber_evidence_kind"] == mmdbtype.String("isp_inferred") {
				inferred++
			}
		}
	}
	if baseLeaves == 0 {
		return errors.New("subscriber base database is empty")
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
	for key, path := range map[string]string{"base_arin_mmdb": basePath, "subscriber_catalog": catalogPath} {
		digest, err := hashArinBuildInput(ctx, path)
		if err != nil {
			return err
		}
		if hashes[key] != digest {
			return errors.New("subscriber build input changed during generation")
		}
	}
	if _, err := sources.hashCountryEvidenceSources(ctx, catalogPath, at); err != nil {
		return err
	}
	return writeManifest(output, map[string]any{"source": "reviewed subscriber operators and RIPE RIS origins", "built_at": at, "classifier_version": 1, "quality_policy_version": 2, "subscriber_origin_policy": "identified-subscriber-default", "inputs_sha256": hashes, "origin_sources": catalog.OriginSources, "origin_rows": rows, "usable_origin_prefixes": usableRoutePrefixes, "reviewed_operators": len(catalog.Operators), "base_leaves": baseLeaves, "emitted_partitions": emitted, "quality_state_partitions": states, "isp_inferred_partitions": inferred}, "arin.mmdb")
}
