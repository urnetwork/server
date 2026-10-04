package main

import (
	"errors"
	"net/netip"
	"slices"
)

// Organization ancestry remains a separate authority for country and network
// risk. A network delegation may supply only the requested negative Quality
// fallback, and only through a fully containing authoritative ARIN chain.
type arinAllocationQualityParent struct {
	network       string
	organizations []arinOrganization
}

func arinOrganizationAncestors(organizations map[string]arinOrganization, handle string) ([]arinOrganization, error) {
	var ancestors []arinOrganization
	seen := map[string]bool{}
	for handle != "" {
		org, ok := organizations[handle]
		if !ok {
			return nil, errors.New("ARIN organization parent is absent")
		}
		if seen[handle] {
			return nil, errors.New("ARIN organization parent cycle")
		}
		seen[handle] = true
		ancestors = append(ancestors, org)
		handle = org.Parent
	}
	slices.Reverse(ancestors)
	return ancestors, nil
}

func (rules classificationRules) hasReviewedOrganization(organizations []arinOrganization) bool {
	for _, org := range organizations {
		for _, rule := range rules.Rules {
			if slices.Contains(rule.OrgHandles, org.Handle) || rule.pattern != nil && rule.pattern.MatchString(org.Name) {
				return true
			}
		}
	}
	return false
}

// The nearest reviewed network owner's organization chain is the boundary.
// A reviewed access ancestor stops the search but never allows its unreviewed
// child. Missing/referral/noncontaining links cannot be skipped to reach a
// more distant hosting owner. Network cycles were checked before this call.
func (rules classificationRules) allocationQualityParent(allocation arinAllocation, direct []arinOrganization, organizations map[string]arinOrganization, parents map[string]string, networks map[string][]arinAllocation) (arinAllocationQualityParent, error) {
	if arinBlockRegistrationScope(allocation.blockType) != "arin" || rules.hasReviewedOrganization(direct) {
		return arinAllocationQualityParent{}, nil
	}
	child := allocation.prefix
	for handle := parents[allocation.network]; handle != ""; handle = parents[handle] {
		var containing *arinAllocation
		for _, parent := range networks[handle] {
			if parent.prefix.Bits() > child.Bits() || !parent.prefix.Contains(child.Addr()) {
				continue
			}
			if arinBlockRegistrationScope(parent.blockType) != "arin" {
				return arinAllocationQualityParent{}, nil
			}
			if containing == nil || containing.prefix.Bits() < parent.prefix.Bits() {
				copy := parent
				containing = &copy
			}
		}
		if containing == nil {
			return arinAllocationQualityParent{}, nil
		}
		ancestors, err := arinOrganizationAncestors(organizations, containing.organization)
		if err != nil {
			return arinAllocationQualityParent{}, err
		}
		if rules.hasReviewedOrganization(ancestors) {
			return arinAllocationQualityParent{handle, ancestors}, nil
		}
		child = containing.prefix
	}
	return arinAllocationQualityParent{}, nil
}

func (rules classificationRules) classifyAllocation(allocation arinAllocation, direct []arinOrganization, parent arinAllocationQualityParent, address netip.Addr) arinClassification {
	classification := rules.classify(direct, address)
	classification = rules.classifyAllocationScope(allocation, classification)
	if classification.ruleName != "" || parent.network == "" {
		return classification
	}
	inherited := rules.classify(parent.organizations, address)
	if inherited.ruleName != "" && inherited.nonQuality {
		inherited.networkHandle = parent.network
		return inherited
	}
	return classification
}
