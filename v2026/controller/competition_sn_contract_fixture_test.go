// Contract tests share the exact companion selected by the isolated workspace.
// Ordinary sibling checkouts retain their checked-in relative specification.
package controller

import (
	"os"
	"testing"
)

// This is a test-layout override, never a production schema or API selector.
func competitionTestSnContractPath() string {
	if path := os.Getenv("COMPETITION_TEST_SN_CONTRACT"); path != "" {
		return path
	}
	return "../../sn/api/competition.yml"
}

// Readers consume the actual selected file, never a replacement schema stub.
func readCompetitionTestSnContract() ([]byte, error) {
	return os.ReadFile(competitionTestSnContractPath())
}

// The isolated suite must not accidentally fall back to a different checkout.
func TestCompetitionDistributedRecoveryContractPathUsesExplicitCompanion(t *testing.T) {
	t.Setenv("COMPETITION_TEST_SN_CONTRACT", "/synthetic/frozen-companion/api/competition.yml")
	if path := competitionTestSnContractPath(); path != "/synthetic/frozen-companion/api/competition.yml" {
		t.Fatal("explicit frozen companion path was replaced", path)
	}
}

// Existing local developer layouts preserve the original default exactly.
func TestCompetitionDistributedRecoveryContractPathKeepsSiblingDefault(t *testing.T) {
	t.Setenv("COMPETITION_TEST_SN_CONTRACT", "")
	if path := competitionTestSnContractPath(); path != "../../sn/api/competition.yml" {
		t.Fatal("normal sibling companion default changed", path)
	}
}
