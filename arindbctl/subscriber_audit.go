package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/urnetwork/server"
)

// The audit measures a reviewed catalog against the same pinned routing and
// geography the build would use. It establishes which identities disagree with
// observed geography, which ASNs never originate routes, which routes lose
// their inference to visibility, validity or geography, and which unreviewed
// origins carry the most address space per associated country. It is a review
// queue and a consistency check, never reviewed subscriber evidence itself.
const reviewQueueDepth = 30

type subscriberCatalogAuditOperator struct {
	Id                   string             `json:"id"`
	Name                 string             `json:"name"`
	Usage                string             `json:"usage"`
	Countries            []string           `json:"countries"`
	ASNs                 []uint32           `json:"asns"`
	UnobservedASNs       []uint32           `json:"unobserved_asns"`
	Routes               int                `json:"routes"`
	Ipv4Addresses        uint64             `json:"ipv4_addresses"`
	Ipv6Networks48       float64            `json:"ipv6_48_networks"`
	CountryShares        map[string]float64 `json:"ipv4_country_shares"`
	OutsideReviewedShare float64            `json:"ipv4_outside_reviewed_share"`
	LowVisibilityRoutes  int                `json:"low_visibility_routes"`
	RpkiInvalidRoutes    int                `json:"rpki_invalid_routes"`
	SharedOriginRoutes   int                `json:"shared_origin_routes"`
	RegistryHolders      []string           `json:"registry_holders,omitempty"`
	UnreviewedSiblings   []uint32           `json:"registry_sibling_asns_unreviewed,omitempty"`
	OtherOperatorSibling map[uint32]string  `json:"registry_sibling_asns_other_operators,omitempty"`
	Flags                []string           `json:"flags"`
	ipv4Weight           float64
	outsideWeight        float64
	countryWeight        map[string]float64
	observed             map[uint32]bool
}

// Two reviewed operators whose routes nest: one originates more-specifics
// inside the other's aggregates. That is how one operator's sibling ASNs look
// when listed as separate catalog entries, and the more-specifics then stand
// on their own visibility and validity instead of inheriting the aggregate's.
type subscriberCatalogMergeCandidate struct {
	Operators           []string `json:"operators"`
	Reason              string   `json:"reason"`
	Routes              int      `json:"routes"`
	LowVisibilityRoutes int      `json:"low_visibility_routes"`
	SharedHolders       []string `json:"shared_registry_holders,omitempty"`
}

type subscriberCatalogReviewCandidate struct {
	ASN           uint32  `json:"asn"`
	Routes        int     `json:"routes"`
	Ipv4Addresses uint64  `json:"ipv4_addresses"`
	CountryShare  float64 `json:"ipv4_country_share"`
}

func auditSubscriberCatalog(ctx context.Context, catalogPath, geolite2Path, output string, at time.Time) error {
	if catalogPath == "" || geolite2Path == "" {
		return errors.New("rules and geolite2 are required for a subscriber catalog audit")
	}
	catalog, err := loadSubscriberOriginCatalog(catalogPath)
	if err != nil {
		return err
	}
	hashes := map[string]string{}
	for key, path := range map[string]string{"subscriber_catalog": catalogPath, "geolite2": geolite2Path} {
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
	holders, err := loadRegistryHolders(ctx, catalogPath, catalog.RegistrySources)
	if err != nil {
		return err
	}
	geo, err := server.OpenIpInfoDatabase(geolite2Path)
	if err != nil {
		return err
	}
	defer geo.Close()
	minimumPeers := catalog.minimumOriginPeers()
	reviewedIdentities := func(route subscriberOriginRoute) ([]string, bool) {
		ids := []string{}
		for _, origin := range route.origins {
			operators := catalog.byASN[origin.asn]
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
	nesting := map[string]*subscriberCatalogMergeCandidate{}
	operators := map[string]*subscriberCatalogAuditOperator{}
	for _, operator := range catalog.Operators {
		operators[operator.Id] = &subscriberCatalogAuditOperator{Id: operator.Id, Name: operator.Name, Usage: operator.Usage, Countries: operator.Countries, ASNs: slices.Clone(operator.ASNs),
			CountryShares: map[string]float64{}, Flags: []string{}, countryWeight: map[string]float64{}, observed: map[uint32]bool{}}
	}
	type unreviewedOrigin struct {
		routes  int
		weight  float64
		country map[string]float64
	}
	unreviewed := map[uint32]*unreviewedOrigin{}
	decisions := map[string]int{}
	withheld := map[string]int{}
	rpkiStates := map[string]int{}
	for _, prefix := range prefixes {
		if err := ctx.Err(); err != nil {
			return err
		}
		route := routes[prefix]
		validity := route.rpki
		if len(catalog.RpkiSources) != 0 {
			rpkiStates[validity]++
		}
		decision := subscriberOriginDecision(route, catalog.byASN, minimumPeers)
		decisions[string(decision["state"].(mmdbtype.String))]++
		if reason, ok := decision["withheld_reason"].(mmdbtype.String); ok {
			withheld[string(reason)]++
		}
		ipv4 := prefix.Addr().Is4()
		countryWeight := map[string]float64{}
		var total float64
		if ipv4 {
			for cell, err := range geo.NetworksWithin(prefix) {
				if err != nil {
					return err
				}
				info, err := geo.GetIpInfo(cell.Addr())
				if err != nil {
					return err
				}
				weight := math.Exp2(float64(32 - cell.Bits()))
				countryWeight[strings.ToLower(info.CountryCode)] += weight
				total += weight
			}
		}
		if child, reviewed := reviewedIdentities(route); reviewed {
			if covering, ok := coveringSubscriberOrigin(routes, prefix); ok {
				if parent, reviewed := reviewedIdentities(routes[covering]); reviewed && !slices.Equal(child, parent) {
					pair := append(slices.Clone(child), parent...)
					slices.Sort(pair)
					pair = slices.Compact(pair)
					key := strings.Join(pair, " ")
					candidate := nesting[key]
					if candidate == nil {
						candidate = &subscriberCatalogMergeCandidate{Operators: pair, Reason: "more-specifics-under-another-operator-aggregate"}
						nesting[key] = candidate
					}
					candidate.Routes++
					if route.ownVisibility() < minimumPeers {
						candidate.LowVisibilityRoutes++
					}
				}
			}
		}
		ids := map[string]bool{}
		for _, origin := range route.origins {
			for _, operator := range catalog.byASN[origin.asn] {
				ids[operator.Id] = true
				operators[operator.Id].observed[origin.asn] = true
			}
			if len(catalog.byASN[origin.asn]) == 0 && ipv4 {
				entry := unreviewed[origin.asn]
				if entry == nil {
					entry = &unreviewedOrigin{country: map[string]float64{}}
					unreviewed[origin.asn] = entry
				}
				entry.routes++
				entry.weight += total
				for country, weight := range countryWeight {
					entry.country[country] += weight
				}
			}
		}
		for id := range ids {
			stats := operators[id]
			stats.Routes++
			if ipv4 {
				stats.ipv4Weight += total
				reviewed := map[string]bool{}
				for _, country := range catalog.byId[id].Countries {
					reviewed[strings.ToLower(country)] = true
				}
				for country, weight := range countryWeight {
					stats.countryWeight[country] += weight
					if country != "" && !reviewed[country] {
						stats.outsideWeight += weight
					}
				}
			} else if prefix.Bits() <= 48 {
				stats.Ipv6Networks48 += math.Exp2(float64(48 - prefix.Bits()))
			} else {
				stats.Ipv6Networks48 += math.Exp2(-float64(prefix.Bits() - 48))
			}
			if route.visibility < minimumPeers {
				stats.LowVisibilityRoutes++
			}
			if validity == "invalid" {
				stats.RpkiInvalidRoutes++
			}
			if len(ids) > 1 {
				stats.SharedOriginRoutes++
			}
		}
	}
	report := []subscriberCatalogAuditOperator{}
	flagged, withoutRoutes := 0, 0
	for _, operator := range catalog.Operators {
		stats := operators[operator.Id]
		stats.Ipv4Addresses = uint64(stats.ipv4Weight)
		for country, weight := range stats.countryWeight {
			if stats.ipv4Weight > 0 {
				stats.CountryShares[country] = math.Round(10000*weight/stats.ipv4Weight) / 10000
			}
		}
		if stats.ipv4Weight > 0 {
			stats.OutsideReviewedShare = math.Round(10000*stats.outsideWeight/stats.ipv4Weight) / 10000
		}
		for _, asn := range operator.ASNs {
			if !stats.observed[asn] {
				stats.UnobservedASNs = append(stats.UnobservedASNs, asn)
			}
		}
		if stats.UnobservedASNs == nil {
			stats.UnobservedASNs = []uint32{}
		}
		if stats.Routes == 0 {
			stats.Flags = append(stats.Flags, "no-observed-routes")
			withoutRoutes++
		} else if len(stats.UnobservedASNs) != 0 {
			stats.Flags = append(stats.Flags, "unobserved-asns")
		}
		if stats.Routes != 0 && stats.ipv4Weight > 0 && stats.OutsideReviewedShare >= 0.5 {
			stats.Flags = append(stats.Flags, "identity-review-suggested")
			flagged++
		}
		if stats.SharedOriginRoutes != 0 {
			stats.Flags = append(stats.Flags, "shared-origin-routes")
		}
		if holders != nil {
			stats.RegistryHolders = holders.holdersOf(operator.ASNs)
			for _, sibling := range holders.siblings(operator.ASNs) {
				if others := catalog.byASN[sibling]; len(others) != 0 {
					if stats.OtherOperatorSibling == nil {
						stats.OtherOperatorSibling = map[uint32]string{}
					}
					stats.OtherOperatorSibling[sibling] = others[0].Id
				} else {
					stats.UnreviewedSiblings = append(stats.UnreviewedSiblings, sibling)
				}
			}
			if len(stats.UnreviewedSiblings) != 0 {
				stats.Flags = append(stats.Flags, "unreviewed-registry-siblings")
			}
			if len(stats.OtherOperatorSibling) != 0 {
				stats.Flags = append(stats.Flags, "registry-siblings-under-other-operators")
			}
		}
		report = append(report, *stats)
	}
	merges := []subscriberCatalogMergeCandidate{}
	for _, candidate := range nesting {
		if holders != nil {
			shared := map[string]int{}
			for _, id := range candidate.Operators {
				for _, holder := range holders.holdersOf(catalog.byId[id].ASNs) {
					shared[holder]++
				}
			}
			for holder, count := range shared {
				if count > 1 {
					candidate.SharedHolders = append(candidate.SharedHolders, holder)
				}
			}
			slices.Sort(candidate.SharedHolders)
		}
		merges = append(merges, *candidate)
	}
	if holders != nil {
		seen := map[string]bool{}
		for _, candidate := range merges {
			seen[strings.Join(candidate.Operators, " ")] = true
		}
		byHolder := map[string][]string{}
		for _, operator := range catalog.Operators {
			for _, holder := range holders.holdersOf(operator.ASNs) {
				if !slices.Contains(byHolder[holder], operator.Id) {
					byHolder[holder] = append(byHolder[holder], operator.Id)
				}
			}
		}
		for holder, ids := range byHolder {
			slices.Sort(ids)
			if len(ids) > 1 && !seen[strings.Join(ids, " ")] {
				seen[strings.Join(ids, " ")] = true
				merges = append(merges, subscriberCatalogMergeCandidate{Operators: ids, Reason: "shared-registry-holder", SharedHolders: []string{holder}})
			}
		}
	}
	sort.Slice(merges, func(i, j int) bool {
		if merges[i].Routes != merges[j].Routes {
			return merges[i].Routes > merges[j].Routes
		}
		return strings.Join(merges[i].Operators, " ") < strings.Join(merges[j].Operators, " ")
	})
	queue := map[string][]subscriberCatalogReviewCandidate{}
	for asn, entry := range unreviewed {
		for country, weight := range entry.country {
			if country == "" {
				continue
			}
			queue[country] = append(queue[country], subscriberCatalogReviewCandidate{ASN: asn, Routes: entry.routes, Ipv4Addresses: uint64(weight), CountryShare: math.Round(10000*weight/entry.weight) / 10000})
		}
	}
	for country, candidates := range queue {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Ipv4Addresses != candidates[j].Ipv4Addresses {
				return candidates[i].Ipv4Addresses > candidates[j].Ipv4Addresses
			}
			return candidates[i].ASN < candidates[j].ASN
		})
		if len(candidates) > reviewQueueDepth {
			candidates = candidates[:reviewQueueDepth]
		}
		queue[country] = candidates
	}
	content, err := json.MarshalIndent(map[string]any{
		"generated_at": at, "catalog_sha256": hashes["subscriber_catalog"], "geolite2_build_time": geo.BuildTime().UTC(),
		"minimum_origin_peers": minimumPeers, "origin_rows": rows, "usable_origin_prefixes": len(routes),
		"route_decisions": decisions, "withheld_routes": withheld, "rpki_route_validity": rpkiStates,
		"operators": report, "operator_merge_candidates": merges, "unreviewed_review_queue_by_country": queue,
		"summary": map[string]any{"operators": len(catalog.Operators), "identity_review_suggested": flagged, "operators_without_routes": withoutRoutes, "unreviewed_origin_asns": len(unreviewed), "operator_merge_candidates": len(merges)},
		"caveats": []string{
			"Address weights are routed IPv4 address counts, not subscribers, users or providers.",
			"GeoLite2 associated country is correlation evidence, not customer location.",
			"The review queue is a discovery input; a listed ASN is not a reviewed subscriber identity.",
			"Registry siblings and nested routes suggest one operator; merging entries or adding ASNs remains a reviewed catalog decision.",
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSyncedFile(filepath.Join(output, "catalog-audit.json"), append(content, '\n')); err != nil {
		return err
	}
	if _, err := catalog.hashEvidenceSources(ctx, catalogPath, at); err != nil {
		return err
	}
	return writeManifest(output, map[string]any{"source": "reviewed subscriber catalog audit", "generated_at": at, "inputs_sha256": hashes, "reviewed_operators": len(catalog.Operators), "usable_origin_prefixes": len(routes)}, "catalog-audit.json")
}
