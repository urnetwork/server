// Optional real-source audits expose relationships only as aggregate counts.
package main

import (
	"encoding/json"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
)

// No live source identities or addresses become fixtures or diagnostic output.
func TestArinSourceTopologyAudit(t *testing.T) {
	path := os.Getenv("ARIN_AUDIT_SOURCE_PATH")
	if path == "" {
		t.Skip("set ARIN_AUDIT_SOURCE_PATH for an aggregate offline source audit")
	}
	type network struct {
		parent, owner string
	}
	type allocation struct {
		network, kind string
	}
	networks := map[string]network{}
	organizations := map[string]arinOrganization{}
	prefixes := map[netip.Prefix][]allocation{}
	counts := map[string]int64{}
	rules, err := loadClassificationRules(os.Getenv("ARIN_REVIEWED_RULES_PATH"))
	if err != nil {
		t.Fatal("set ARIN_REVIEWED_RULES_PATH to classify the audited source")
	}
	if err := scanArinXml(t.Context(), path, func(org arinOrganization) error {
		counts["organizations"]++
		org.Country = strings.ToLower(strings.TrimSpace(org.Country))
		organizations[org.Handle] = org
		if org.Parent != "" {
			counts["organization_with_parent"]++
		}
		return nil
	}, func(record arinNetwork) error {
		counts["networks"]++
		if record.Parent != "" {
			counts["network_with_parent"]++
		}
		if record.Handle == "" {
			counts["network_without_handle"]++
		}
		if _, ok := networks[record.Handle]; ok {
			counts["duplicate_network_handle"]++
		}
		networks[record.Handle] = network{parent: record.Parent, owner: record.OrgHandle}
		for _, block := range record.Blocks {
			prefix, err := block.prefix()
			if err != nil {
				return err
			}
			prefixes[prefix] = append(prefixes[prefix], allocation{network: record.Handle, kind: block.Type})
			counts["allocations"]++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	descends := func(child, ancestor string) bool {
		seen := map[string]bool{}
		for child != "" && !seen[child] {
			if child == ancestor {
				return true
			}
			seen[child] = true
			child = networks[child].parent
		}
		return false
	}
	facts := func(owner string, address netip.Addr) ([]string, arinClassification) {
		countries := []string{}
		ancestors := []arinOrganization{}
		seen := map[string]bool{}
		for owner != "" && !seen[owner] {
			seen[owner] = true
			org := organizations[owner]
			ancestors = append(ancestors, org)
			if org.Country != "" {
				countries = append(countries, org.Country)
			}
			owner = org.Parent
		}
		slices.Reverse(countries)
		slices.Reverse(ancestors)
		return countries, rules.classify(ancestors, address)
	}
	for prefix, allocations := range prefixes {
		if len(allocations) < 2 {
			continue
		}
		counts["duplicate_prefix_groups"]++
		for i, a := range allocations {
			for _, b := range allocations[i+1:] {
				counts["duplicate_prefix_pairs"]++
				if descends(a.network, b.network) || descends(b.network, a.network) {
					counts["duplicate_prefix_related_pairs"]++
				} else {
					counts["duplicate_prefix_unrelated_pairs"]++
					if networks[a.network].owner == networks[b.network].owner && a.kind == b.kind {
						counts["unrelated_same_owner_and_type_pairs"]++
					} else if networks[a.network].owner == networks[b.network].owner {
						counts["unrelated_same_owner_different_type_pairs"]++
					} else {
						counts["unrelated_different_owner_pairs"]++
						ownerA, ownerB := networks[a.network].owner, networks[b.network].owner
						if networks[a.network].parent == networks[b.network].parent {
							counts["unrelated_same_network_parent"]++
						}
						if organizations[ownerA].Parent == organizations[ownerB].Parent {
							counts["unrelated_same_org_parent"]++
						}
						countriesA, classificationA := facts(ownerA, prefix.Addr())
						countriesB, classificationB := facts(ownerB, prefix.Addr())
						if slices.Equal(countriesA, countriesB) {
							counts["unrelated_country_chain_agrees"]++
						} else {
							counts["unrelated_country_chain_conflicts"]++
						}
						if classificationA == classificationB {
							counts["unrelated_classification_provenance_agrees"]++
						} else {
							counts["unrelated_classification_provenance_conflicts"]++
						}
						if slices.Equal(countriesA, countriesB) && classificationA == classificationB && a.kind == b.kind {
							counts["unrelated_all_classification_facts_agree"]++
						}
						if prefix.Bits() == prefix.Addr().BitLen() {
							counts["unrelated_host_prefix"]++
						}
					}
				}
				if networks[a.network].owner == networks[b.network].owner && a.kind == b.kind {
					counts["duplicate_prefix_same_owner_and_type_pairs"]++
				}
				counts["duplicate_type_pair_"+a.kind+"_"+b.kind]++
			}
		}
	}
	encoded, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("source_topology_aggregates=%s", encoded)
}
