package main

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
)

// An operator's subscriber publication can cover part of a diversified owner's
// holdings. Bind each reviewed positive to the exact registration block and
// direct owner. These are not global prefix overrides: a separately registered
// child, even with the same organization, needs its own reviewed scope.
type classificationAllocationScope struct {
	NetHandle string `yaml:"net_handle"`
	OrgHandle string `yaml:"org_handle"`
	Prefix    string `yaml:"prefix"`
}

type arinAllocationScopeKey struct {
	network, organization string
	prefix                netip.Prefix
}

func allocationScopeKey(allocation arinAllocation) arinAllocationScopeKey {
	return arinAllocationScopeKey{allocation.network, allocation.organization, allocation.prefix}
}

func (rules *classificationRules) prepareAllocationScopes() error {
	rules.allocationRules = map[arinAllocationScopeKey][]int{}
	for index, rule := range rules.Rules {
		if len(rule.AllocationScopes) == 0 {
			continue
		}
		if rules.QualityPolicyVersion != 2 || rule.NonQuality == nil || *rule.NonQuality || rule.RiskCategory != "" {
			return errors.New("allocation scopes require an explicit subscriber policy-two decision")
		}
		if len(rule.OrgHandles) != 0 || rule.OrgNamePattern != "" || len(rule.Prefixes) != 0 {
			return errors.New("allocation scopes cannot be combined with unscoped match criteria")
		}
		seen := map[arinAllocationScopeKey]bool{}
		for _, scope := range rule.AllocationScopes {
			prefix, err := netip.ParsePrefix(scope.Prefix)
			if err != nil || prefix != prefix.Masked() || prefix.Addr().Is4In6() ||
				strings.TrimSpace(scope.NetHandle) == "" || strings.TrimSpace(scope.OrgHandle) == "" ||
				scope.NetHandle != strings.TrimSpace(scope.NetHandle) || scope.OrgHandle != strings.TrimSpace(scope.OrgHandle) {
				return errors.New("allocation scope requires exact network, owner and canonical prefix")
			}
			key := arinAllocationScopeKey{scope.NetHandle, scope.OrgHandle, prefix}
			if seen[key] {
				return errors.New("allocation scope repeats a registration block")
			}
			seen[key] = true
			rules.allocationRules[key] = append(rules.allocationRules[key], index)
		}
	}
	return nil
}

// Check the complete source before emitting anything. A changed owner, resized
// block, absent network or foreign-RIR referral invalidates the reviewed scope.
// Exact-key indexes keep this linear in bulk source size, not rules times rows.
func (rules classificationRules) validateAllocationScopes(allocations []arinAllocation) error {
	if len(rules.allocationRules) == 0 {
		return nil
	}
	found := map[arinAllocationScopeKey]bool{}
	for _, allocation := range allocations {
		key := allocationScopeKey(allocation)
		if len(rules.allocationRules[key]) == 0 {
			continue
		}
		if arinBlockRegistrationScope(allocation.blockType) != "arin" {
			return errors.New("subscriber allocation scope is not authoritative ARIN registration")
		}
		found[key] = true
	}
	if len(found) != len(rules.allocationRules) {
		return errors.New("subscriber allocation scope no longer matches its reviewed network, owner and prefix")
	}
	return nil
}

func (rules classificationRules) classifyAllocationScope(allocation arinAllocation, prior arinClassification) arinClassification {
	if arinBlockRegistrationScope(allocation.blockType) != "arin" || prior.qualityState == "ambiguous" {
		return prior
	}
	indices := rules.allocationRules[allocationScopeKey(allocation)]
	if len(indices) == 0 {
		return prior
	}
	// An exact direct-use exclusion or a prefix finding conflicts with approval.
	// A reviewed subscriber delegation can override only a negative ancestor.
	if prior.nonQuality && prior.ruleName != "" && (prior.orgHandle == "" || prior.orgHandle == allocation.organization) {
		sources := []string{prior.source}
		for _, index := range indices {
			sources = append(sources, rules.Rules[index].Source)
		}
		slices.Sort(sources)
		return arinClassification{nonQuality: true, qualityState: "ambiguous", ruleName: "rule-conflict",
			reason: "reviewed subscriber allocation conflicts with direct network-use evidence",
			source: strings.Join(slices.Compact(sources), " "), orgHandle: allocation.organization, networkHandle: allocation.network}
	}
	// All allocation-scoped rules are positive; retain all independently reviewed
	// sources when more than one rule supports this exact registration block.
	rule := rules.Rules[indices[0]]
	sources := []string{}
	for _, index := range indices {
		sources = append(sources, rules.Rules[index].Source)
	}
	slices.Sort(sources)
	return arinClassification{qualityState: "subscriber", ruleName: rule.Name, reason: rule.Reason,
		source: strings.Join(slices.Compact(sources), " "), orgHandle: allocation.organization, networkHandle: allocation.network}
}
