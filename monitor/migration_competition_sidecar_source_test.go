package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The successor's query is the intake-first custody with the two source guard
// bodies replaced by the widened ones and nothing else: every other predicate
// survives byte for byte, and neither registered body is still required.
func TestMigrationsCompetitionSidecarSourceReplacesOnlyTheTwoGuardBodies(t *testing.T) {
	registeredGraph, _ := server.CompetitionSourceRetirementBodiesSql()
	_, _, registeredSource := server.CompetitionTrustedInstrumentBodiesSql()
	widenedGraph, widenedSource := server.CompetitionSidecarSourceBodiesSql()
	if registeredGraph == widenedGraph || registeredSource == widenedSource {
		t.Fatal("the permissive step changes no guard body")
	}
	predecessor := competitionIntakeFirstArtifactQuery()
	query := competitionSidecarSourceArtifactQuery()
	if query != competitionSidecarSourceArtifactQuery() {
		t.Fatal("the successor query is nondeterministic")
	}
	restored := query
	for _, bodies := range [][2]string{{registeredGraph, widenedGraph}, {registeredSource, widenedSource}} {
		registered := snMainnetMigrationLiteral(bodies[0])
		widened := snMainnetMigrationLiteral(bodies[1])
		if strings.Contains(query, "p.prosrc="+registered) {
			t.Fatal("the successor still requires a registered source guard body")
		}
		if strings.Count(query, "p.prosrc="+widened) != strings.Count(predecessor, "p.prosrc="+registered) || !strings.Contains(query, "p.prosrc="+widened) {
			t.Fatal("the successor does not pin a widened body wherever its predecessor pinned the registered one")
		}
		restored = strings.ReplaceAll(restored, widened, registered)
	}
	if restored != predecessor {
		t.Fatal("the successor changes more of the intake-first custody than the two source guard bodies")
	}
}

// The successor is registered exactly when this binary links MG0, at the
// step's version, and the intake-first contract is retired there and not before.
func TestMigrationsCompetitionSidecarSourceFollowsItsRegistration(t *testing.T) {
	version := server.CompetitionSidecarSourceMigrationVersion()
	if (0 < version) != server.SidecarMg0Linked() {
		t.Fatalf("the permissive step is at version %d in a binary where MG0 linked is %t", version, server.SidecarMg0Linked())
	}
	predecessor := migrationCurrentSuffixTestContract(t, server.CompetitionIntakeFirstMigrationVersion())
	if predecessor.artifact.removedVersion != version {
		t.Fatalf("the intake-first contract is retired at %d, not %d", predecessor.artifact.removedVersion, version)
	}
	registered := 0
	for _, contract := range migrationCurrentSuffixContracts {
		if contract.query == competitionSidecarSourceArtifactQuery() {
			registered++
			if contract.artifact.requiredVersion != version || contract.artifact.removedVersion != 0 || contract.artifact.rowColumn != version-589 {
				t.Fatalf("the successor is at version %d, column %d, removed at %d", contract.artifact.requiredVersion, contract.artifact.rowColumn, contract.artifact.removedVersion)
			}
		}
	}
	want := 0
	if 0 < version {
		want = 1
	}
	if registered != want || len(competitionSidecarSourceMigrationContracts()) != want {
		t.Fatalf("the successor is registered %d times at version %d", registered, version)
	}
}
