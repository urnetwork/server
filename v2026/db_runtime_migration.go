package server

// The payer key and compatibility trigger are required by all new writers.
// The two following indexes serve only the optional payer traversal, which
// checks their operative definitions before using them and otherwise keeps
// the chronological closer. Keep this allowance tied to that exact published
// tail; an appended migration or changed identity still requires the full head.
func MinimumRuntimeMigrationVersion() int {
	return minimumRuntimeMigrationVersion(MigrationCount(), MigrationIdentity)
}

func minimumRuntimeMigrationVersion(count int, identity func(int) (string, error)) int {
	if count != 793 {
		return count
	}
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
}
