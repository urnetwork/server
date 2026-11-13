// Future coverage instruments add only their fixed dedicated helper surface.
// Mutable configuration and candidate lists cannot define the helper census.
package controller

import (
	"slices"
	"strings"

	"github.com/urnetwork/server/v2026/model"
)

// Derivation owns its additional slice while omission retains the old value.
func competitionWorkCoveragePatchPolicy(policy PatchPolicy, coverage *model.CompetitionWorkCoveragePolicy) (PatchPolicy, error) {
	paths, err := model.CompetitionWorkCoverageProtectedHelperPaths(coverage)
	if err != nil || coverage == nil {
		return policy, err
	}
	prefix := ""
	if policy.PathBase == competitionMonoRootPathBase {
		prefix = "server/"
	}
	policy.ForbiddenPaths = append([]string{}, policy.ForbiddenPaths...)
	for _, path := range paths {
		if !slices.Contains(policy.ForbiddenPaths, prefix+path) {
			policy.ForbiddenPaths = append(policy.ForbiddenPaths, prefix+path)
		}
	}
	return policy, model.ValidateCompetitionWorkCoveragePatchPolicy(policy, coverage)
}

// A complete explicit prohibition additionally rejects case aliases. This is
// restrictive parsing only, never a selector or evidence of coverage authority.
func competitionWorkCoveragePathProtected(value string, policy PatchPolicy) bool {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	paths, err := model.CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	if err != nil {
		return false
	}
	prefix := ""
	if policy.PathBase == competitionMonoRootPathBase {
		prefix = "server/"
	}
	for _, path := range paths {
		if !slices.Contains(policy.ForbiddenPaths, prefix+path) {
			return false
		}
	}
	for _, path := range paths {
		if strings.EqualFold(value, prefix+path) {
			return true
		}
	}
	return false
}
