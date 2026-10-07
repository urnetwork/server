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
	if want == 793 {
		want = 791
	}
	if got := MinimumRuntimeMigrationVersion(); got != want {
		t.Fatalf("current runtime requires %d, want %d", got, want)
	}
}

func TestMinimumRuntimeMigrationVersionRetainsUnknownHead(t *testing.T) {
	for _, count := range []int{0, 790, 791, 792, 794, 795} {
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
