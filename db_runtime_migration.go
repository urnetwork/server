package server

// MinimumRuntimeMigrationVersion is the database migration head a binary
// requires before it reports ready: its migration count, except for published
// tails that no runtime code depends on. Each allowance is tied to that exact
// published tail; an appended migration or a changed identity still requires
// the full head.
//
//   - 793: the payer key and compatibility trigger (791) are required by all new
//     writers. The two following indexes serve only the optional payer
//     traversal, which checks their operative definitions before using them and
//     otherwise keeps the chronological closer.
//   - 926: statistics on transfer_contract's unresolved predicate
//     (transferContractUnresolvedPredicateStatisticsMigration). They only inform
//     the planner; no runtime code reads or requires the statistics object, so a
//     binary with 926 runs against head 925. The operator creates the object
//     when transfer_contract's vacuum allows its lock (the migration's runbook),
//     and db migrate then records 926.
func MinimumRuntimeMigrationVersion() int {
	return minimumRuntimeMigrationVersion(MigrationCount(), MigrationIdentity)
}

func minimumRuntimeMigrationVersion(count int, identity func(int) (string, error)) int {
	switch count {
	case 793:
		expected := [...]string{
			"82db54a16a981bf2d699b2f9f00460000abde6557358108c03fe5d5bbd54831a",
			"61114a2f525fe883b4579dce713105dbada969936b48d233a4ee7f8c38da0b38",
			"9185c045fdc7bfde979d56d41ee8f352d8136910ecf916e9faf7dbab7e96001d",
		}
		for offset, want := range expected {
			actual, err := identity(790 + offset)
			if err != nil || actual != want {
				return count
			}
		}
		return 791
	case 926:
		actual, err := identity(925)
		if err != nil || actual != "6784a656f5e50e536283f371c48b9e238e948e8301cfaa925a15fe7ca2f2d624" {
			return count
		}
		return 925
	}
	return count
}
