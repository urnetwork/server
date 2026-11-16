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
	}
	if got := MinimumRuntimeMigrationVersion(); got != want {
		t.Fatalf("current runtime requires %d, want %d", got, want)
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
	for _, count := range []int{0, 790, 791, 792, 794, 795, 796, 924, 925, 927, 928} {
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
