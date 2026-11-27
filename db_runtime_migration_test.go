package server

import (
	"errors"
	"strconv"
	"testing"
)

func TestMinimumRuntimeMigrationVersionExactPayerTail(t *testing.T) {
	if got := minimumRuntimeMigrationVersion(793, MigrationIdentity); got != 791 {
		t.Fatalf("exact published payer tail requires %d, want 791", got)
	}
	want := MigrationCount()
	switch want {
	case 793:
		want = 791
	case 926:
		want = 925
	case 1009:
		want = 1008
	}
	if got := MinimumRuntimeMigrationVersion(); got != want {
		t.Fatalf("current runtime requires %d, want %d", got, want)
	}
}

// The real list: its last migration, 1011, is the fleet's URL outcome buckets
// (ProviderEgressUrlFleetBucketSql). Every URL window reads the table, in the
// api, the taskworker and the other services that score providers, so their
// binaries require the full head 1011. Pinned to this list: whoever appends
// migration 1012 updates this test, and decides there whether binaries with
// it run against a head without it.
func TestMinimumRuntimeMigrationVersionCurrentList(t *testing.T) {
	if count, required := MigrationCount(), MinimumRuntimeMigrationVersion(); count != 1011 || required != 1011 {
		t.Fatalf("%d migrations require head %d, want 1011 requiring 1011", count, required)
	}
}

// Migration 1009 only replaces the close evidence guard's function body: a
// binary that carries it runs against head 1008, only for that exact published
// migration, and only while it is the last.
func TestMinimumRuntimeMigrationVersionEvidenceWindowGuardTail(t *testing.T) {
	if got := minimumRuntimeMigrationVersion(1009, MigrationIdentity); got != 1008 {
		t.Fatalf("the published evidence window guard migration requires %d, want 1008", got)
	}
	for _, fail := range []bool{false, true} {
		got := minimumRuntimeMigrationVersion(1009, func(index int) (string, error) {
			if index == 1008 {
				if fail {
					return "", errors.New("identity unavailable")
				}
				return "different published migration", nil
			}
			return MigrationIdentity(index)
		})
		if got != 1009 {
			t.Fatalf("changed migration 1009/error=%v lowered head to %d", fail, got)
		}
	}
	// A migration appended after the guard is required in full, though the
	// guard (identity(1008)) is unchanged.
	got := minimumRuntimeMigrationVersion(1010, func(index int) (string, error) {
		if index == 1009 {
			return "appended migration", nil
		}
		return MigrationIdentity(index)
	})
	if got != 1010 {
		t.Fatalf("a migration appended after the guard lowered head 1010 to %d", got)
	}
}

// Migration 926 only creates planner statistics: a binary that carries it runs
// against head 925, and only for that exact published migration.
func TestMinimumRuntimeMigrationVersionStatisticsTail(t *testing.T) {
	if got := minimumRuntimeMigrationVersion(926, MigrationIdentity); got != 925 {
		t.Fatalf("the published statistics migration requires %d, want 925", got)
	}
	for _, fail := range []bool{false, true} {
		got := minimumRuntimeMigrationVersion(926, func(index int) (string, error) {
			if index == 925 {
				if fail {
					return "", errors.New("identity unavailable")
				}
				return "different published migration", nil
			}
			return MigrationIdentity(index)
		})
		if got != 926 {
			t.Fatalf("changed migration 926/error=%v lowered head to %d", fail, got)
		}
	}
}

func TestMinimumRuntimeMigrationVersionRetainsUnknownHead(t *testing.T) {
	for _, count := range []int{0, 790, 791, 792, 794, 795, 796, 924, 925, 927, 928, 1007, 1008, 1010, 1011} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			calls := 0
			got := minimumRuntimeMigrationVersion(count, func(int) (string, error) {
				calls++
				return "", nil
			})
			if got != count || calls != 0 {
				t.Fatalf("head %d became %d after %d identity reads", count, got, calls)
			}
		})
	}
}

func TestMinimumRuntimeMigrationVersionRejectsChangedPayerTail(t *testing.T) {
	for _, changed := range []int{790, 791, 792} {
		for _, fail := range []bool{false, true} {
			got := minimumRuntimeMigrationVersion(793, func(index int) (string, error) {
				if index == changed {
					if fail {
						return "", errors.New("identity unavailable")
					}
					return "different published migration", nil
				}
				return MigrationIdentity(index)
			})
			if got != 793 {
				t.Fatalf("changed migration %d/error=%v lowered head to %d", changed, fail, got)
			}
		}
	}
}
