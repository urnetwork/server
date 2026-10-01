package main

import (
	"net/netip"
	"slices"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// Positive network-use exclusions are independent of geographic evidence and
// Quality overrides. A subscriber label or matching country cannot waive an
// observed proxy network. There are deliberately no negative risk rules.
func (self classificationRules) networkRiskEvidence(owners [][]arinOrganization, address netip.Addr) mmdbtype.Slice {
	evidence := mmdbtype.Slice{}
	for _, rule := range self.Rules {
		if rule.RiskCategory == "" {
			continue
		}
		matchedOwners := []string{}
		matched := false
		for _, ancestors := range owners {
			for _, org := range ancestors {
				if slices.Contains(rule.OrgHandles, org.Handle) || rule.pattern != nil && rule.pattern.MatchString(org.Name) {
					matched = true
					if !slices.Contains(matchedOwners, org.Handle) {
						matchedOwners = append(matchedOwners, org.Handle)
					}
				}
			}
		}
		for _, prefix := range rule.prefixes {
			matched = matched || prefix.Contains(address)
		}
		if !matched {
			continue
		}
		slices.Sort(matchedOwners)
		ownerRecords := mmdbtype.Slice{}
		for _, owner := range matchedOwners {
			ownerRecords = append(ownerRecords, mmdbtype.String(owner))
		}
		evidence = append(evidence, mmdbtype.Map{
			"rule": mmdbtype.String(rule.Name), "category": mmdbtype.String(rule.RiskCategory),
			"source": mmdbtype.String(rule.Source), "reason": mmdbtype.String(rule.Reason),
			"org_handles": ownerRecords,
		})
	}
	return evidence
}
