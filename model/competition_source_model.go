// Canonical source identities bind every frozen repository, independent of a
// manifest's whitespace or a round's fresh workload and baseline identity.
package model

import (
	"errors"
	"fmt"
	"regexp"
)

var competitionSourceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Returns an independently owned list of the complete active competition graph:
// the ten repositories it has held and sidecar, which every server commit that
// requires the sidecar module needs beside it to build.
func CompetitionSourceRepositoryNames() []string {
	return append(CompetitionSourceRepositoryNamesBeforeSidecar(), "sidecar")
}

// Returns an independently owned list of the graph of a source whose server
// commit predates the sidecar module. Such a source stays valid: the database
// guards admit the ten, or the ten with sidecar
// (server.competitionSidecarSourceMigration), until a cleanup migration requires
// sidecar once every competition job runs a release that sends it.
func CompetitionSourceRepositoryNamesBeforeSidecar() []string {
	return []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"}
}

// The complete active graph is identical in production and staging. Execution
// mode remains separately authenticated by the source activation protocol.
func CompetitionSourceLockDigest(commits map[string]string) (string, error) {
	return CompetitionSourceLockDigestForMode(commits, false)
}

// Requires the ten live dependencies in either mode, alone or with sidecar, as
// the database guards do. Historical evidence is retained unchanged; an
// obsolete or unknown extra repository cannot authorize new work.
func CompetitionSourceLockDigestForMode(commits map[string]string, _ bool) (string, error) {
	repositoryNames := CompetitionSourceRepositoryNamesBeforeSidecar()
	if _, sidecar := commits["sidecar"]; sidecar {
		repositoryNames = CompetitionSourceRepositoryNames()
	}
	if len(commits) != len(repositoryNames) {
		return "", fmt.Errorf("source lock requires exactly the %d frozen repositories, or those with sidecar", len(CompetitionSourceRepositoryNamesBeforeSidecar()))
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
