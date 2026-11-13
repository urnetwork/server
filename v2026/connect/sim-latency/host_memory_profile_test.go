// Isolated Go guards execute the real host-check shell/jq memory predicate with synthetic facts; no Docker, host probe or configuration is accessed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Each child owns a cancellation deadline and bounded pipe-drain grace; non-positive durations are unsupported.
type hostMemoryPredicateSettings struct {
	ChildTimeout time.Duration
	WaitDelay    time.Duration
}

// Actual jq work is finite and local; these bounds contain unexpected subprocess failures, never determine a positive test result.
func defaultHostMemoryPredicateSettings() hostMemoryPredicateSettings {
	return hostMemoryPredicateSettings{ChildTimeout: 5 * time.Second, WaitDelay: time.Second}
}

// Synthetic reported facts and independently expected root configuration can disagree without bypassing the real shell equality guards.
type hostMemoryPredicateFixture struct {
	Active           int64
	Evidence         int64
	Total            int64
	Reserve          int64
	Capacity         int64
	Host             int64
	CapacityPassed   bool
	ExpectedActive   int64
	ExpectedEvidence int64
	ExpectedTotal    int64
	ExpectedReserve  int64
}

// The synthetic host sits exactly on the required reserve boundary; no actual host identity or observed physical capacity is copied into fixtures.
func approvedHostMemoryFixture(evidenceGiB int64) hostMemoryPredicateFixture {
	active := int64(96 * 1024 * 1024 * 1024)
	evidence := evidenceGiB * 1024 * 1024 * 1024
	reserve := int64(24 * 1024 * 1024 * 1024)
	return hostMemoryPredicateFixture{Active: active, Evidence: evidence, Total: active + evidence, Reserve: reserve, Capacity: reserve, Host: active + evidence + reserve, CapacityPassed: true, ExpectedActive: active, ExpectedEvidence: evidence, ExpectedTotal: active + evidence, ExpectedReserve: reserve}
}

// The exact marked source block runs unchanged under Bash and actual jq, with process-group cancellation rather than a text-only assertion.
func executeHostMemoryPredicate(t *testing.T, fixture hostMemoryPredicateFixture) bool {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("actual jq is required; skipping is not permitted")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source path unavailable")
	}
	content, err := os.ReadFile(filepath.Join(filepath.Dir(source), "evaluator", "host-self-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const start = "management_memory_reserved=false\n"
	const end = "direct_local_mounts=false\n"
	if strings.Count(string(content), start) != 1 || strings.Count(string(content), end) != 1 {
		t.Fatal("actual memory predicate source boundaries are not unique")
	}
	_, after, _ := strings.Cut(string(content), start)
	block, _, ok := strings.Cut(after, end)
	if !ok {
		t.Fatal("actual memory predicate source block unavailable")
	}
	report, err := json.Marshal(map[string]any{"memory_capacity_passed": fixture.CapacityPassed, "active_memory_limit_bytes": fixture.Active, "evidence_memory_limit_bytes": fixture.Evidence, "total_evaluation_memory_limit_bytes": fixture.Total, "minimum_management_memory_reserve_bytes": fixture.Reserve, "capacity_reserve_bytes": fixture.Capacity, "host_memory_bytes": fixture.Host})
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultHostMemoryPredicateSettings()
	ctx, cancel := context.WithTimeout(context.Background(), settings.ChildTimeout)
	defer cancel()
	script := "set -uo pipefail\nresource_boundary_json=$1\nactive_memory_limit_bytes=$2\nevidence_memory_limit_bytes=$3\ntotal_evaluation_memory_limit_bytes=$4\nmanagement_memory_reserve_bytes=$5\ncapacity_reserve_bytes=$6\nexpected_active_memory_limit_bytes=$7\nexpected_evidence_memory_limit_bytes=$8\nexpected_total_evaluation_memory_limit_bytes=$9\nexpected_management_memory_reserve_bytes=${10}\n" + start + block + "printf '%s\\n' \"$management_memory_reserved\"\n"
	arguments := []string{"-c", script, "host-memory-predicate", string(report)}
	for _, value := range []int64{fixture.Active, fixture.Evidence, fixture.Total, fixture.Reserve, fixture.Capacity, fixture.ExpectedActive, fixture.ExpectedEvidence, fixture.ExpectedTotal, fixture.ExpectedReserve} {
		arguments = append(arguments, strconv.FormatInt(value, 10))
	}
	command := exec.CommandContext(ctx, "bash", arguments...)
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = settings.WaitDelay
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual shell/jq predicate setup failed: %v; %s", err, output)
	}
	switch strings.TrimSpace(string(output)) {
	case "true":
		return true
	case "false":
		return false
	default:
		t.Fatalf("unexpected predicate output: %q", output)
		return false
	}
}

// Historical approved four-GiB facts remain valid only when the protected expected configuration also names four GiB.
func TestHostSelfCheckAcceptsApprovedFourGiBEvidenceProfile(t *testing.T) {
	if !executeHostMemoryPredicate(t, approvedHostMemoryFixture(4)) {
		t.Fatal("approved four-GiB profile refused")
	}
}

// This deterministically reproduces the stale literal: old code refuses the otherwise exact approved five-GiB profile.
func TestHostSelfCheckAcceptsApprovedFiveGiBEvidenceProfile(t *testing.T) {
	if !executeHostMemoryPredicate(t, approvedHostMemoryFixture(5)) {
		t.Fatal("approved five-GiB profile refused")
	}
}

// Finite approved profiles are not interchangeable; the exact protected expected configuration remains authoritative.
func TestHostSelfCheckRefusesExpectedReportedEvidenceMismatch(t *testing.T) {
	for _, c := range []struct{ Reported, Expected int64 }{{Reported: 4, Expected: 5}, {Reported: 5, Expected: 4}} {
		fixture := approvedHostMemoryFixture(c.Reported)
		fixture.ExpectedEvidence = c.Expected * 1024 * 1024 * 1024
		fixture.ExpectedTotal = fixture.Active + fixture.ExpectedEvidence
		if executeHostMemoryPredicate(t, fixture) {
			t.Errorf("reported%d expected%d profile accepted", c.Reported, c.Expected)
		}
	}
}

// An arbitrary matching configured number is not an approved evidence profile.
func TestHostSelfCheckRefusesUnapprovedEvidenceProfiles(t *testing.T) {
	for _, value := range []int64{0, 6, 7, -1} {
		if executeHostMemoryPredicate(t, approvedHostMemoryFixture(value)) {
			t.Errorf("unapproved evidence profile%d accepted", value)
		}
	}
}

// Matching expected and reported totals cannot forge the required active-plus-evidence sum.
func TestHostSelfCheckRefusesForgedResourceSum(t *testing.T) {
	fixture := approvedHostMemoryFixture(5)
	fixture.Total -= 1024 * 1024 * 1024
	fixture.ExpectedTotal = fixture.Total
	fixture.Host = fixture.Total + fixture.Capacity
	if executeHostMemoryPredicate(t, fixture) {
		t.Fatal("forged matching total accepted")
	}
}

// A lower matching configured reserve cannot weaken the unchanged minimum management budget.
func TestHostSelfCheckRefusesInsufficientManagementReserve(t *testing.T) {
	fixture := approvedHostMemoryFixture(5)
	fixture.Reserve = 23 * 1024 * 1024 * 1024
	fixture.ExpectedReserve = fixture.Reserve
	if executeHostMemoryPredicate(t, fixture) {
		t.Fatal("subminimum management reserve accepted")
	}
}

// Capacity must be both genuinely derived from physical memory and at least the required reserve, never negative or merely asserted sufficient.
func TestHostSelfCheckRefusesForgedOrInsufficientCapacity(t *testing.T) {
	for _, c := range []struct {
		Name      string
		Capacity  int64
		HostDelta int64
	}{{Name: "shortfall", Capacity: 23 * 1024 * 1024 * 1024}, {Name: "forged-physical", Capacity: 24 * 1024 * 1024 * 1024, HostDelta: -1024 * 1024 * 1024}, {Name: "negative", Capacity: -1}} {
		fixture := approvedHostMemoryFixture(5)
		fixture.Capacity = c.Capacity
		fixture.Host = fixture.Total + fixture.Capacity + c.HostDelta
		if executeHostMemoryPredicate(t, fixture) {
			t.Errorf("%s capacity accepted", c.Name)
		}
	}
}

// The two approved evidence sizes do not relax the fixed active limit or the independently reported admission decision.
func TestHostSelfCheckRefusesActiveLimitOrAdmissionFailure(t *testing.T) {
	for _, c := range []struct {
		Name   string
		Active int64
		Passed bool
	}{{Name: "wrong-active", Active: 95 * 1024 * 1024 * 1024, Passed: true}, {Name: "admission-failed", Active: 96 * 1024 * 1024 * 1024, Passed: false}} {
		fixture := approvedHostMemoryFixture(5)
		fixture.Active = c.Active
		fixture.ExpectedActive = c.Active
		fixture.Total = fixture.Active + fixture.Evidence
		fixture.ExpectedTotal = fixture.Total
		fixture.Host = fixture.Total + fixture.Capacity
		fixture.CapacityPassed = c.Passed
		if executeHostMemoryPredicate(t, fixture) {
			t.Errorf("%s facts accepted", c.Name)
		}
	}
}
