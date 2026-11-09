package sidecart3

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// D-TEST-2 on T3: every store fake sidecartest ships is held to its real store here, by a test of
// server's that runs its suite, or waits, reported missing, on the task whose store has not landed.
func TestEveryStoreSuiteRunsOnT3(t *testing.T) {
	RunSuites(t, Suites(), "..")
}

// RunSuites' check bites: a store fake no suite runs on fails it, and so do a suite with no T3 entry, an
// entry naming no test or a test that does not call its suite, and an entry saying two ways it runs.
func TestSuiteProblemsFailAFakeWithNoSuite(t *testing.T) {
	storeSuites := []*sidecartest.StoreSuite{
		{Name: "redissuite.Run", Fakes: []string{"FakeRedis"}},
		{Name: "RunSocketStoreSuite", Fakes: []string{"FakeSocketStore"}},
		{Name: "RunDnsStoreSuite", Fakes: []string{"FakeDnsStore"}},
	}
	storeFakes := []string{"FakeRedis", "FakeSocketStore", "FakeDnsStore", "FakeNewStore"}
	suites := []*Suite{
		{Name: "redissuite.Run", Package: "sidecarhost", Test: "TestRedisConformsOnTheTestStore"},
		{Name: "RunSocketStoreSuite", Package: "model", Test: "TestSocketStorePickIsOneScript"},
		{Name: "RunCertsStoreSuite", WaitsOn: "TLS-M5-16", Package: "model", Test: "TestNothing"},
	}
	problems := SuiteProblems(storeSuites, storeFakes, suites, "..")
	for _, want := range []string{
		"FakeNewStore stands for a store and no suite holds it to the real one (D-TEST-2)",
		"RunDnsStoreSuite runs on no real store at T3 and waits on no task (D-TEST-2)",
		"RunCertsStoreSuite is no suite sidecartest lists",
		"RunCertsStoreSuite says 2 ways it runs, not one",
		"TestSocketStorePickIsOneScript does not call sidecartest.RunSocketStoreSuite",
	} {
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, want) }) {
			t.Errorf("no problem says %q: %q", want, problems)
		}
	}
	if problems := SuiteProblems(storeSuites[:1], storeFakes[:1], suites[:1], ".."); len(problems) != 0 {
		t.Errorf("a whole list has problems: %q", problems)
	}
	missing := SuiteProblems(storeSuites[:1], storeFakes[:1], []*Suite{{Name: "redissuite.Run", Package: "sidecarhost", Test: "TestNoSuchTest"}}, "..")
	if len(missing) != 1 || !strings.Contains(missing[0], "no test TestNoSuchTest in") {
		t.Errorf("a test that is not there: %q", missing)
	}
}

// sidecar-t3.sh's suites cadence runs each test Suites names, in its package, so no suite is listed as
// running on T3 while nothing runs it.
func TestSidecarT3RunsEverySuiteTest(t *testing.T) {
	script, err := os.ReadFile("../sidecar-t3.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, suite := range Suites() {
		if suite.Test == "" {
			continue
		}
		if !strings.Contains(string(script), suite.Test) {
			t.Errorf("sidecar-t3.sh does not run %s", suite)
		}
		pattern := "./" + suite.Package
		if suite.Package == "." {
			pattern = "go_t3 . "
		}
		if !strings.Contains(string(script), pattern) {
			t.Errorf("sidecar-t3.sh runs nothing in %s for %s", suite.Package, suite)
		}
	}
	// R-TEST-9: a failed attempt fails the run, and server's TestEnv reruns none
	if !strings.Contains(string(script), "export WARP_TEST_ENV_FAIL_FAST=1") {
		t.Error("sidecar-t3.sh leaves TestEnv's reruns on")
	}
	if !strings.Contains(string(script), "sidecar_testhooks,sidecar_pending") {
		t.Error("sidecar-t3.sh does not build with sidecar_testhooks and sidecar_pending")
	}
}

// With WARP_TEST_ENV_FAIL_FAST=1, as sidecar-t3.sh sets it, a failed TestEnv attempt is not retried.
func TestFailFastLeavesNoRerun(t *testing.T) {
	t.Setenv("WARP_TEST_ENV_FAIL_FAST", "1")
	if rerunCount := server.DefaultTestEnv().RerunCount; rerunCount != 0 {
		t.Errorf("TestEnv reruns a failed attempt %d times", rerunCount)
	}
}

// Each tag a call names moves to a tag of the run's own on the master, keys sharing a tag still share
// one, and an untagged key stays untagged, so the cluster refuses what the engine's keys would make it.
func TestCrossslotKeysSpreadOverTheMasters(t *testing.T) {
	masters := []*CrossslotMaster{
		{SlotRanges: [][2]int{{0, 5460}}},
		{SlotRanges: [][2]int{{5461, 10922}}},
		{SlotRanges: [][2]int{{10923, 16383}}},
	}
	keys := []string{"{scp_a}ver", "{scp_a}n", "{scp_b}ver", "plain"}
	for index, master := range masters {
		spread := CrossslotKeys(keys, master, "lint")
		if len(spread) != len(keys) {
			t.Fatalf("spread %q", spread)
		}
		if sidecar.RedisHashTag(spread[0]) != sidecar.RedisHashTag(spread[1]) {
			t.Errorf("master %d: keys that shared a tag part: %q", index, spread)
		}
		if sidecar.RedisHashTag(spread[0]) == sidecar.RedisHashTag(spread[2]) {
			t.Errorf("master %d: keys of two tags share one: %q", index, spread)
		}
		for _, key := range spread[:3] {
			if !master.holds(sidecartest.RedisClusterSlot(key)) {
				t.Errorf("master %d does not hold %q", index, key)
			}
		}
		if spread[3] != "lint:plain" || strings.Contains(spread[3], "{") {
			t.Errorf("an untagged key became %q", spread[3])
		}
		if sidecar.RedisKeysShareTag(spread[:2]) != true || sidecar.RedisKeysShareTag(spread[1:3]) != false {
			t.Errorf("master %d: the tag rule reads otherwise after the spread: %q", index, spread)
		}
	}
}

// Calls of each shape are kept once: the first script of a name, the first mget or del of a key count,
// and nothing of one key.
func TestCrossslotCallsKeepEachShapeOnce(t *testing.T) {
	calls := NewCrossslotCalls()
	script := &sidecar.RedisScript{Name: "toy", Source: "return 1"}
	calls.addScript(script, []string{"{a}x"}, []string{"1"})
	calls.addScript(script, []string{"{b}x"}, []string{"2"})
	calls.addKeys("mget", []string{"{a}x", "{a}y"})
	calls.addKeys("mget", []string{"{b}x", "{b}y"})
	calls.addKeys("mget", []string{"{a}x", "{a}y", "{a}z"})
	calls.addKeys("del", []string{"{a}x"})
	ops := []string{}
	for _, call := range calls.Calls() {
		ops = append(ops, call.Op+":"+strings.Join(call.Keys, ","))
	}
	want := []string{"script:toy:{a}x", "mget:{a}x,{a}y", "mget:{a}x,{a}y,{a}z"}
	if !slices.Equal(ops, want) {
		t.Errorf("kept %q, want %q", ops, want)
	}
}

// On the one Redis node test-env.sh attests, the runtime lint refuses to run rather than pass: one node
// accepts what 3 masters refuse (QD-028).
func TestRunCrossslotRefusesOneNode(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		calls := NewCrossslotCalls()
		calls.addKeys("mget", []string{"{a}x", "{a}y"})
		recorder := record(t, func(tb testing.TB) {
			RunCrossslot(tb, calls, "refusal")
		})
		if !recorder.fatal || !strings.Contains(recorder.text(), "QD-028") {
			t.Errorf("the lint ran on one node: %s", recorder.text())
		}
	})
}

// The dial guard stays on under T3: a dial to an address test-env.sh did not attest is refused before
// any socket opens.
func TestADialOutsideTheAttestedListIsRefused(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		conn, err := server.Testing_DialTestEnv(t.Context(), "redis", "tcp", "127.0.0.1:59999")
		if err == nil {
			conn.Close()
			t.Fatal("a dial outside the attested list went through")
		}
		if err := server.RequireAttestedLocalEndpoints(t.Context()); err != nil {
			t.Errorf("the attested endpoints are not in force: %v", err)
		}
	})
}
