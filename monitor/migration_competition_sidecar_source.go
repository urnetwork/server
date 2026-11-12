package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// MG0's permissive step (server.competitionSidecarSourceMigration) replaces the
// source graph and source authorization bodies, which the intake-first contract
// pins by exact text. So it retires that contract and republishes its whole
// custody with the two widened bodies.
//
// The step is pending until M0's Gn-out and its version follows its position
// (server/db_migrations_sidecar.go). While this binary does not link MG0 the
// version is zero: the intake-first contract is never retired and no successor
// is registered.
func competitionSidecarSourceArtifactQuery() string {
	registeredGraph, _ := server.CompetitionSourceRetirementBodiesSql()
	_, _, registeredSource := server.CompetitionTrustedInstrumentBodiesSql()
	widenedGraph, widenedSource := server.CompetitionSidecarSourceBodiesSql()
	query := competitionIntakeFirstArtifactQuery()
	for _, bodies := range [][2]string{
		{registeredGraph, widenedGraph},
		{registeredSource, widenedSource},
	} {
		registered := snMainnetMigrationLiteral(bodies[0])
		if !strings.Contains(query, registered) {
			panic("intake-first custody does not pin the registered source guard body")
		}
		query = strings.ReplaceAll(query, registered, snMainnetMigrationLiteral(bodies[1]))
	}
	return query
}

// The successor contract at the step's version, or none while the step is not
// registered: no initializer registers a zero-version contract.
func competitionSidecarSourceMigrationContracts() []snMainnetMigrationContract {
	version := server.CompetitionSidecarSourceMigrationVersion()
	if version < 1 {
		return nil
	}
	return []snMainnetMigrationContract{
		migrationCurrentSuffix(version, "source guards of the ten live repositories or those ten with sidecar, with retained intake custody",
			competitionSidecarSourceArtifactQuery()),
	}
}
