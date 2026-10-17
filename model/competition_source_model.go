// Canonical source identities bind every frozen repository, independent of a
// manifest's whitespace or a round's fresh workload and baseline identity.
package model

import (
	"errors"
	"regexp"
)

var competitionSourceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Hashes the compact evaluator-lock object. Artifact file digests remain
// separate: pretty-printed copies need not have this canonical content hash.
func CompetitionSourceLockDigest(commits map[string]string) (string, error) {
	repositoryNames := []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp"}
	if len(commits) != len(repositoryNames) {
		return "", errors.New("source lock requires exactly the frozen ten repositories")
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
