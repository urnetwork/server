package monitor

import (
	"testing"
)

// A contract's row column is its version: the suffix emits one expression per
// version in version order, a true placeholder for a version with no
// contract, whatever order the contracts are listed in.
func TestMigrationCurrentSuffixQueriesFollowVersions(t *testing.T) {
	contracts := []snMainnetMigrationContract{
		migrationCurrentSuffix(migrationCurrentSuffixFirstVersion+2, "third", "third_query"),
		migrationCurrentSuffix(migrationCurrentSuffixFirstVersion, "first", "first_query"),
	}
	if got, want := migrationCurrentSuffixVersionQueries(contracts), "first_query,\ntrue,\nthird_query"; got != want {
		t.Fatalf("suffix expressions %q, want %q", got, want)
	}
	for _, contract := range contracts {
		if contract.artifact.rowColumn != contract.artifact.requiredVersion-589 {
			t.Fatalf("v%d reads column %d", contract.artifact.requiredVersion, contract.artifact.rowColumn)
		}
	}
}

// Two contracts of one version would read the same column.
func TestMigrationCurrentSuffixQueriesRefuseDuplicateVersions(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicated suffix version was emitted")
		}
	}()
	migrationCurrentSuffixVersionQueries([]snMainnetMigrationContract{
		migrationCurrentSuffix(migrationCurrentSuffixFirstVersion, "first", "first_query"),
		migrationCurrentSuffix(migrationCurrentSuffixFirstVersion, "again", "again_query"),
	})
}

// The published suffix, contiguous from its first version, emits exactly its
// contracts' expressions in list order, with no placeholder.
func TestMigrationCurrentSuffixQueriesKeepThePublishedSuffix(t *testing.T) {
	published := []snMainnetMigrationContract{}
	for _, contract := range migrationCurrentSuffixContracts {
		if contract.artifact.requiredVersion <= 857 {
			published = append(published, contract)
		}
	}
	if len(published) != 857-migrationCurrentSuffixFirstVersion+1 {
		t.Fatalf("published suffix has %d contracts", len(published))
	}
	expressions := ""
	for index, contract := range published {
		if index != 0 {
			expressions += ",\n"
		}
		expressions += contract.query
	}
	if migrationCurrentSuffixVersionQueries(published) != expressions {
		t.Fatal("the published suffix changed its emitted expressions")
	}
}
