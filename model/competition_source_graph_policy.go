// The source graph a round's evaluator copies, builds and measures is a frozen
// policy leaf of its own. It names a release of the evaluator programs, not a
// scoring contract: the scorer version of a round does not depend on it.
package model

import (
	"encoding/json"
	"errors"
)

// Omission is the immutable historical graph of ten repositories, which every
// round frozen before the sidecar module measured. The explicit version adds
// sidecar as the eleventh repository (QD-769); only the producer releases cut
// for that graph accept a request that names it, and they accept no other.
type CompetitionSourceGraphPolicy string

const (
	CompetitionSourceGraphPolicyLegacy  CompetitionSourceGraphPolicy = ""
	CompetitionSourceGraphPolicySidecar CompetitionSourceGraphPolicy = "sidecar/1"
)

// Legacy is represented by omission, never a newly introduced null or empty
// field. An explicit value must name a supported graph other than the legacy.
func (self *CompetitionSourceGraphPolicy) UnmarshalJSON(content []byte) error {
	var policy string
	if err := json.Unmarshal(content, &policy); err != nil {
		return err
	}
	if CompetitionSourceGraphPolicy(policy) != CompetitionSourceGraphPolicySidecar {
		return errors.New("explicit source graph policy must be sidecar/1")
	}
	*self = CompetitionSourceGraphPolicy(policy)
	return nil
}

// Refuses a value no release was cut for, so an unknown graph never falls back
// to the ten repositories.
func ValidateCompetitionSourceGraphPolicy(policy CompetitionSourceGraphPolicy) error {
	_, err := CompetitionSourceRepositoryNamesForGraphPolicy(policy)
	return err
}

// Returns an independently owned list of the repositories a round under the
// policy measures, in the order its evaluator programs hold them.
func CompetitionSourceRepositoryNamesForGraphPolicy(policy CompetitionSourceGraphPolicy) ([]string, error) {
	switch policy {
	case CompetitionSourceGraphPolicyLegacy:
		return CompetitionSourceRepositoryNamesBeforeSidecar(), nil
	case CompetitionSourceGraphPolicySidecar:
		return CompetitionSourceRepositoryNames(), nil
	default:
		return nil, errors.New("unsupported source graph policy")
	}
}

// A source lock belongs to a round only when it holds exactly the graph the
// round's policy names: a lock of the ten cannot stand under sidecar/1, and a
// lock that names sidecar cannot stand under the legacy graph.
func CompetitionSourceLockMatchesGraphPolicy(commits map[string]string, policy CompetitionSourceGraphPolicy) bool {
	repositoryNames, err := CompetitionSourceRepositoryNamesForGraphPolicy(policy)
	if err != nil || len(commits) != len(repositoryNames) {
		return false
	}
	for _, repository := range repositoryNames {
		if !competitionSourceCommitPattern.MatchString(commits[repository]) {
			return false
		}
	}
	return true
}
