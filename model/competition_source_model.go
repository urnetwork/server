// Canonical source identities bind every frozen repository, independent of a
// manifest's whitespace or a round's fresh workload and baseline identity.
package model

import (
	"errors"
	"fmt"
	"regexp"
)

var competitionSourceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Accepts only either complete supported graph. Callers with an authenticated
// execution mode must use the mode-bound variant rather than infer authority.
func CompetitionSourceLockDigest(commits map[string]string) (string, error) {
	return CompetitionSourceLockDigestForMode(commits, len(commits) == 10)
}

// Explicit staging retires operator-proxy; production retains its historical
// eleven-repository contract. Canonical bytes stay identical for existing locks.
func CompetitionSourceLockDigestForMode(commits map[string]string, staging bool) (string, error) {
	repositoryNames := []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"}
	if !staging {
		repositoryNames = append(repositoryNames, "operator-proxy")
	}
	if len(commits) != len(repositoryNames) {
		return "", fmt.Errorf("source lock requires exactly %d frozen repositories", len(repositoryNames))
	}
	for _, repository := range repositoryNames {
		if !competitionSourceCommitPattern.MatchString(commits[repository]) {
			return "", errors.New("source lock contains an invalid or absent commit")
		}
	}
	return CompetitionAutomationDigest(struct {
		Schema              int               `json:"schema"`
		DevelopmentSnapshot bool              `json:"development_snapshot"`
		Repositories        map[string]string `json:"repositories"`
	}{Schema: 1, Repositories: commits})
}
