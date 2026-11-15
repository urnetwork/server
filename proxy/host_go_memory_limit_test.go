package proxy

import (
	"errors"
	"testing"

	"github.com/urnetwork/server"
)

const hostGoMemoryLimitTestCap = int64(10 * 1024 * 1024 * 1024)

// Runs the composed path for a host against a pushed vault resource and a
// 10 GiB cgroup cap, and returns the limit set and the setter's calls.
func hostGoMemoryLimitTestApply(t *testing.T, resource string, host string, readCap func() (int64, error)) (applied int64, setLimits []int64) {
	t.Helper()
	if resource != "" {
		pop := server.Vault.PushSimpleResource(hostGoMemoryLimitResourceName, []byte(resource))
		defer pop()
	}
	if readCap == nil {
		readCap = func() (int64, error) { return hostGoMemoryLimitTestCap, nil }
	}
	applied = applyHostGoMemoryLimit(host, "example.com", readCap, func(limit int64) int64 {
		setLimits = append(setLimits, limit)
		return 6500 * 1024 * 1024
	}, DefaultHostGoMemoryLimitSettings())
	return
}

// The named host's processes take its limit, by the short name warp gives
// the container or the fully qualified one; every other host keeps the
// service's GOMEMLIMIT, and so does every host without the resource.
func TestHostGoMemoryLimitAppliesOnlyToTheNamedHost(t *testing.T) {
	const resource = "go_memory_limit:\n  crisp.example.com: 8000MiB\n"
	const want = int64(8000 * 1024 * 1024)
	for _, host := range []string{"crisp", "crisp.example.com"} {
		applied, setLimits := hostGoMemoryLimitTestApply(t, resource, host, nil)
		if applied != want || len(setLimits) != 1 || setLimits[0] != want {
			t.Errorf("%s: applied %d with calls %v, want %d once", host, applied, setLimits, want)
		}
	}
	for _, host := range []string{"fireside", "fireside.example.com", "crisp.other.example"} {
		applied, setLimits := hostGoMemoryLimitTestApply(t, resource, host, nil)
		if applied != 0 || len(setLimits) != 0 {
			t.Errorf("%s: applied %d with calls %v, want the service's limit untouched", host, applied, setLimits)
		}
	}
	if applied, setLimits := hostGoMemoryLimitTestApply(t, "", "crisp", nil); applied != 0 || len(setLimits) != 0 {
		t.Errorf("without the resource: applied %d with calls %v, want the service's limit untouched", applied, setLimits)
	}
}

// An entry the container cannot hold, or one that does not parse, keeps the
// service's limit: above 0.9 of the cap, below a quarter of it (a size
// missing its unit reads as bytes), a cap that is unreadable or unlimited, a
// value that is not a positive size, and a key that does not map hosts to
// sizes.
func TestHostGoMemoryLimitRefusesWhatTheContainerCannotHold(t *testing.T) {
	for _, c := range []struct {
		resource string
		readCap  func() (int64, error)
	}{
		{resource: "go_memory_limit:\n  crisp.example.com: 9300MiB\n"},
		{resource: "go_memory_limit:\n  crisp.example.com: 80000MiB\n"},
		{resource: "go_memory_limit:\n  crisp.example.com: 0MiB\n"},
		{resource: "go_memory_limit:\n  crisp.example.com: -1GiB\n"},
		{resource: "go_memory_limit:\n  crisp.example.com: lots\n"},
		{resource: "go_memory_limit:\n  crisp.example.com: 8000\n"},
		{resource: "go_memory_limit: 8000MiB\n"},
		{resource: "go_memory_limit:\n  crisp.example.com: 8000MiB\n", readCap: func() (int64, error) { return 0, errors.New("no cgroup") }},
		{resource: "go_memory_limit:\n  crisp.example.com: 8000MiB\n", readCap: func() (int64, error) { return 0, nil }},
	} {
		applied, setLimits := hostGoMemoryLimitTestApply(t, c.resource, "crisp", c.readCap)
		if applied != 0 || len(setLimits) != 0 {
			t.Errorf("%q: applied %d with calls %v, want the service's limit untouched", c.resource, applied, setLimits)
		}
	}
	// at the bounds: 0.9 and a quarter of 10 GiB
	if applied, _ := hostGoMemoryLimitTestApply(t, "go_memory_limit:\n  crisp.example.com: 9216MiB\n", "crisp", nil); applied != 9216*1024*1024 {
		t.Errorf("a limit at 0.9 of the cap applied %d", applied)
	}
	if applied, _ := hostGoMemoryLimitTestApply(t, "go_memory_limit:\n  crisp.example.com: 2560MiB\n", "crisp", nil); applied != 2560*1024*1024 {
		t.Errorf("a limit at a quarter of the cap applied %d", applied)
	}
}

// The cgroup cap files: v2 "max" and v1's largest value are unlimited.
func TestHostGoMemoryLimitReadsTheCgroupCap(t *testing.T) {
	for _, c := range []struct {
		text string
		want int64
	}{
		{text: "10737418240\n", want: hostGoMemoryLimitTestCap},
		{text: "max\n", want: 0},
		{text: "9223372036854771712\n", want: 0},
	} {
		cgroupCap, err := parseHostGoMemoryLimitCgroupCap(c.text)
		if err != nil || cgroupCap != c.want {
			t.Errorf("%q: cap %d (%v), want %d", c.text, cgroupCap, err, c.want)
		}
	}
	if _, err := parseHostGoMemoryLimitCgroupCap("ten gigabytes"); err == nil {
		t.Error("an unparseable cap read as a number")
	}
}

// The bounds, set by the defaults.
func TestHostGoMemoryLimitDefaults(t *testing.T) {
	settings := DefaultHostGoMemoryLimitSettings()
	if settings.MaxCgroupMemoryFraction != 0.9 || settings.MinCgroupMemoryFraction != 0.25 {
		t.Fatalf("the cap fractions are %v and %v, want 0.9 and 0.25", settings.MaxCgroupMemoryFraction, settings.MinCgroupMemoryFraction)
	}
	if DefaultHostGoMemoryLimitSettings() == DefaultHostGoMemoryLimitSettings() {
		t.Fatal("the defaults share one instance")
	}
}
