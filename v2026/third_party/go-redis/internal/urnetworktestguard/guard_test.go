package urnetworktestguard

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Only the exact opt-in allows the suites to run: no value, an empty one, a
// truthy one or a near miss keeps them off localhost's Redis ports.
func TestAllowedOnlyWithTheExactOptIn(t *testing.T) {
	for _, c := range []struct {
		value   string
		set     bool
		allowed bool
	}{
		{set: false, allowed: false},
		{value: "", set: true, allowed: false},
		{value: "1", set: true, allowed: false},
		{value: "true", set: true, allowed: false},
		{value: "disposable", set: true, allowed: false},
		{value: EnvValue + " ", set: true, allowed: false},
		{value: "DISPOSABLE-LOCALHOST", set: true, allowed: false},
		{value: EnvValue, set: true, allowed: true},
	} {
		t.Setenv(EnvName, c.value)
		if !c.set {
			os.Unsetenv(EnvName)
		}
		if allowed := Allowed(); allowed != c.allowed {
			t.Errorf("%s set %t to %q: allowed %t, want %t", EnvName, c.set, c.value, allowed, c.allowed)
		}
	}
}

// Every suite in this module that dials localhost's Redis ports runs through
// Run from its TestMain: the root package's and the doctests'.
func TestEveryLocalRedisSuiteRunsThroughTheGuard(t *testing.T) {
	call := regexp.MustCompile(`(?s)func TestMain\(m \*testing\.M\) \{\s*os\.Exit\(urnetworktestguard\.Run\(m\)\)\s*\}`)
	for _, path := range []string{
		filepath.Join("..", "..", "urnetwork_test_guard_test.go"),
		filepath.Join("..", "..", "doctests", "urnetwork_test_guard_test.go"),
	} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !call.Match(source) {
			t.Errorf("%s has no TestMain that runs through urnetworktestguard.Run", path)
		}
	}
}
