package model

// Bounded verify next-hop cohort (verify_cohort_model.go): both admission
// caps and the growth gate, settlement-epoch rollover with carried members,
// the lifetime census bound, fail-safe refusal, and atomic admission from
// independent processes sharing one Redis database.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

const testVerifyCohortDeployment StDeploymentKey = "964:0xc18925925e2b7bb9059b7d696b8c92762ae86406"

func testVerifyCohortSettings(size int, lifetimeLimit int) *VerifySettings {
	settings := DefaultVerifySettings()
	settings.CohortSize = size
	settings.CohortLifetimeLimit = lifetimeLimit
	settings.CohortDeploymentKey = testVerifyCohortDeployment
	return settings
}

// The st sync task's mirror is the cohort's only epoch source.
func testVerifyCohortSetEpoch(ctx context.Context, epoch uint64) {
	SetStEpochSummaryCache(ctx, testVerifyCohortDeployment, &StEpochSummary{Epoch: epoch}, time.Hour)
}

func testVerifyCohortMembers(ctx context.Context, key string) map[string]bool {
	members := map[string]bool{}
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.SMembers(ctx, key).Result()
		server.Raise(err)
		for _, value := range values {
			members[value] = true
		}
	})
	return members
}

// Registers eligible providers on distinct exact addresses.
func testVerifyCohortProviders(ctx context.Context, settings *VerifySettings, count int) ([]server.Id, map[server.Id]netip.Addr) {
	providers := []server.Id{}
	ips := map[server.Id]netip.Addr{}
	for i := 0; i < count; i += 1 {
		clientId := server.NewId()
		ip := netip.AddrFrom4([4]byte{198, 18, byte(i / 250), byte(i%250 + 1)})
		testVerifySetProvideModes(ctx, clientId)
		FeedVerifyEgress(ctx, clientId, ip, settings)
		providers = append(providers, clientId)
		ips[clientId] = ip
	}
	return providers, ips
}

// Samples without exclusions and returns the distinct hops drawn.
func testVerifyCohortSample(t testing.TB, ctx context.Context, settings *VerifySettings, draws int) map[string]bool {
	hops := map[string]bool{}
	for i := 0; i < draws; i += 1 {
		nextHop, n := SampleVerifyNextHop(ctx, nil, settings)
		if nextHop == nil || n < 1 {
			t.Fatalf("draw %d assigned no hop (n=%d)", i, n)
		}
		hops[nextHop.String()] = true
	}
	return hops
}

func testVerifyCohortSubset(t testing.TB, name string, sub map[string]bool, set map[string]bool) {
	for member := range sub {
		if !set[member] {
			t.Fatalf("%s: %s is outside %v", name, member, set)
		}
	}
}

func TestVerifyCohortAdmissionCapsAndGrowthGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(3, 4)
		settings.ReliabilityAMin = 2
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 5)
		admit := func(candidate server.Id, skipGrowthGate bool, excluded ...string) (admitted bool, n int) {
			server.Redis(ctx, func(r server.RedisClient) {
				admitted, n = admitVerifyCohort(ctx, r, keys, candidate.String(), settings, skipGrowthGate, excluded)
			})
			return
		}
		a, b, c, d := server.NewId(), server.NewId(), server.NewId(), server.NewId()

		// an empty cohort admits its first member
		if ok, n := admit(a, false); !ok || n != 1 {
			t.Fatalf("first admission = %t n=%d", ok, n)
		}
		// a new member waits until the cohort averages a_min draws
		if ok, _ := admit(b, false); ok {
			t.Fatal("growth gate admitted a second member after one draw")
		}
		if ok, n := admit(a, false); !ok || n != 1 {
			t.Fatalf("existing member draw = %t n=%d", ok, n)
		}
		if ok, n := admit(b, false); !ok || n != 2 {
			t.Fatalf("admission at a_min average = %t n=%d", ok, n)
		}
		// a starved trail may pass the gate, never the size cap
		if ok, _ := admit(c, false); ok {
			t.Fatal("growth gate admitted below the a_min average")
		}
		if ok, n := admit(c, true); !ok || n != 3 {
			t.Fatalf("starved admission = %t n=%d", ok, n)
		}
		if ok, _ := admit(d, true); ok {
			t.Fatal("cohort admitted past its size")
		}
		// n is the cohort net of excluded members; a non-member exclusion is ignored
		if ok, n := admit(a, false, b.String(), d.String()); !ok || n != 2 {
			t.Fatalf("draw with exclusions = %t n=%d", ok, n)
		}

		members := testVerifyCohortMembers(ctx, keys.epoch)
		lifetime := testVerifyCohortMembers(ctx, keys.lifetime)
		if len(members) != 3 || !members[a.String()] || !members[b.String()] || !members[c.String()] || members[d.String()] {
			t.Fatalf("epoch cohort = %v", members)
		}
		if len(lifetime) != 3 || lifetime[d.String()] {
			t.Fatalf("lifetime set = %v", lifetime)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			draws, err := r.Get(ctx, keys.draws).Int()
			server.Raise(err)
			if draws != 5 {
				t.Fatalf("draw counter = %d, want 5 admitted draws", draws)
			}
			for _, key := range []string{keys.epoch, keys.draws} {
				if ttl, err := r.TTL(ctx, key).Result(); err != nil || ttl <= 0 {
					t.Fatalf("epoch key %s ttl = %v %v", key, ttl, err)
				}
			}
			// the census bound must never be evicted under volatile-ttl
			if ttl, err := r.TTL(ctx, keys.lifetime).Result(); err != nil || ttl != -1 {
				t.Fatalf("lifetime key ttl = %v %v, want none", ttl, err)
			}
		})
	})
}

func TestVerifyCohortLifetimeLimitSpansEpochs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(4, 4)
		admit := func(epoch uint64, candidate server.Id, skipGrowthGate bool) (admitted bool) {
			server.Redis(ctx, func(r server.RedisClient) {
				admitted, _ = admitVerifyCohort(ctx, r, newVerifyCohortKeys(settings.CohortDeploymentKey, epoch), candidate.String(), settings, skipGrowthGate, nil)
			})
			return
		}
		a, b, c, d, e := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, id := range []server.Id{a, b, c} {
			if !admit(1, id, true) {
				t.Fatal("epoch 1 admission refused")
			}
		}
		// a carried member re-enters without the growth gate and without
		// spending lifetime budget
		if !admit(2, a, false) || !admit(2, b, false) {
			t.Fatal("carried member refused in the next epoch")
		}
		if !admit(2, d, true) {
			t.Fatal("new member refused below the lifetime limit")
		}
		// the lifetime set is full: a new provider is refused although the
		// epoch cohort still has room, while carried members still enter
		if admit(2, e, true) {
			t.Fatal("new member admitted past the lifetime limit")
		}
		if !admit(2, c, false) {
			t.Fatal("carried member refused while the cohort has room")
		}
		epoch1 := testVerifyCohortMembers(ctx, newVerifyCohortKeys(settings.CohortDeploymentKey, 1).epoch)
		epoch2 := testVerifyCohortMembers(ctx, newVerifyCohortKeys(settings.CohortDeploymentKey, 2).epoch)
		lifetime := testVerifyCohortMembers(ctx, newVerifyCohortKeys(settings.CohortDeploymentKey, 2).lifetime)
		if len(epoch1) != 3 || len(epoch2) != 4 || epoch2[e.String()] || len(lifetime) != 4 || lifetime[e.String()] {
			t.Fatalf("epoch 1 %v, epoch 2 %v, lifetime %v", epoch1, epoch2, lifetime)
		}
		// another deployment's epochs restart at zero without inheriting this one
		other := newVerifyCohortKeys("964:0x0000000000000000000000000000000000000001", 1)
		if len(testVerifyCohortMembers(ctx, other.epoch)) != 0 || len(testVerifyCohortMembers(ctx, other.lifetime)) != 0 {
			t.Fatal("cohort keys are not deployment scoped")
		}
	})
}

// End to end through SampleVerifyNextHop: the epoch comes from the st mirror,
// carried members return first, departed ones are replaced only within the
// lifetime limit, and every hop of an epoch is a member of that epoch's cohort.
func TestVerifyCohortSamplingFollowsSettlementEpochs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(4, 6)
		providers, ips := testVerifyCohortProviders(ctx, settings, 12)
		keys := func(epoch uint64) verifyCohortKeys {
			return newVerifyCohortKeys(settings.CohortDeploymentKey, epoch)
		}

		testVerifyCohortSetEpoch(ctx, 7)
		hops7 := testVerifyCohortSample(t, ctx, settings, 100)
		cohort7 := testVerifyCohortMembers(ctx, keys(7).epoch)
		if len(cohort7) != 4 || len(testVerifyCohortMembers(ctx, keys(7).lifetime)) != 4 {
			t.Fatalf("epoch 7 cohort %v", cohort7)
		}
		testVerifyCohortSubset(t, "epoch 7 hops", hops7, cohort7)

		// rollover: each of the first draws re-admits a carried member
		testVerifyCohortSetEpoch(ctx, 8)
		testVerifyCohortSample(t, ctx, settings, 4)
		cohort8 := testVerifyCohortMembers(ctx, keys(8).epoch)
		if len(cohort8) != 4 {
			t.Fatalf("epoch 8 cohort after 4 draws %v, want the carried %v", cohort8, cohort7)
		}
		testVerifyCohortSubset(t, "epoch 8 cohort", cohort8, cohort7)
		hops8 := testVerifyCohortSample(t, ctx, settings, 96)
		testVerifyCohortSubset(t, "epoch 8 hops", hops8, cohort7)

		// two members leave: they are replaced, within the lifetime limit
		var original []server.Id
		for _, provider := range providers {
			if cohort7[provider.String()] {
				original = append(original, provider)
			}
		}
		for _, provider := range original[:2] {
			ClearVerifyEgress(ctx, provider, ips[provider], settings)
		}
		testVerifyCohortSetEpoch(ctx, 9)
		hops9 := testVerifyCohortSample(t, ctx, settings, 200)
		cohort9 := testVerifyCohortMembers(ctx, keys(9).epoch)
		lifetime := testVerifyCohortMembers(ctx, keys(9).lifetime)
		if len(cohort9) != 4 || len(lifetime) != 6 || cohort9[original[0].String()] || cohort9[original[1].String()] ||
			!cohort9[original[2].String()] || !cohort9[original[3].String()] {
			t.Fatalf("epoch 9 cohort %v lifetime %v", cohort9, lifetime)
		}
		testVerifyCohortSubset(t, "epoch 9 hops", hops9, cohort9)

		// the rest of the original members leave; the lifetime set is full,
		// so only the two members admitted in epoch 9 can be assigned
		for _, provider := range original[2:] {
			ClearVerifyEgress(ctx, provider, ips[provider], settings)
		}
		testVerifyCohortSetEpoch(ctx, 10)
		hops10 := testVerifyCohortSample(t, ctx, settings, 200)
		cohort10 := testVerifyCohortMembers(ctx, keys(10).epoch)
		if len(cohort10) != 2 || len(testVerifyCohortMembers(ctx, keys(10).lifetime)) != 6 {
			t.Fatalf("epoch 10 cohort %v", cohort10)
		}
		testVerifyCohortSubset(t, "epoch 10 cohort", cohort10, cohort9)
		testVerifyCohortSubset(t, "epoch 10 hops", hops10, cohort10)
		if len(testVerifyCohortMembers(ctx, keys(7).epoch)) != 4 {
			t.Fatal("a later epoch rewrote an earlier cohort")
		}
	})
}

// A trail that has used every member admits past the growth gate; once the
// cohort is full, exhausting it assigns nothing rather than any other provider.
func TestVerifyCohortTrailExclusions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(3, 3)
		testVerifyCohortProviders(ctx, settings, 8)
		testVerifyCohortSetEpoch(ctx, 1)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 1)

		trail := []server.Id{}
		for depth := 0; depth < 3; depth += 1 {
			nextHop, n := SampleVerifyNextHop(ctx, trail, settings)
			if nextHop == nil || n != 1 {
				t.Fatalf("depth %d hop %v n=%d, want a fresh member with n=1", depth, nextHop, n)
			}
			trail = append(trail, *nextHop)
		}
		if members := testVerifyCohortMembers(ctx, keys.epoch); len(members) != 3 {
			t.Fatalf("cohort %v", members)
		}
		if nextHop, _ := SampleVerifyNextHop(ctx, trail, settings); nextHop != nil {
			t.Fatalf("exhausted cohort assigned %s", nextHop)
		}
		if members := testVerifyCohortMembers(ctx, keys.epoch); len(members) != 3 {
			t.Fatalf("exhausted cohort grew: %v", members)
		}
	})
}

func TestVerifyCohortFailsSafe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(2, 4)
		providers, ips := testVerifyCohortProviders(ctx, settings, 6)
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 3)
		noLeak := func(name string) {
			if 0 < len(testVerifyCohortMembers(ctx, keys.epoch)) || 0 < len(testVerifyCohortMembers(ctx, keys.lifetime)) {
				t.Fatalf("%s admitted a provider", name)
			}
		}

		// unknown epoch: eligible providers exist, but nothing is sampled
		if nextHop, n := SampleVerifyNextHop(ctx, nil, settings); nextHop != nil || n != 0 {
			t.Fatalf("unknown epoch assigned %v", nextHop)
		}
		testVerifyCohortSetEpoch(ctx, 3)
		unconfigured := testVerifyCohortSettings(2, 4)
		unconfigured.CohortDeploymentKey = ""
		if nextHop, _ := SampleVerifyNextHop(ctx, nil, unconfigured); nextHop != nil {
			t.Fatalf("missing deployment assigned %v", nextHop)
		}
		noLeak("unknown epoch")

		// a Redis error panics before any admission (the request fails with
		// no hop), and never reaches the uniform sampler
		for _, corrupt := range []struct {
			name  string
			key   string
			value string
		}{{"epoch set", keys.epoch, "x"}, {"lifetime set", keys.lifetime, "x"}} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, corrupt.key, corrupt.value, 0).Err())
			})
			var nextHop *server.Id
			err := server.HandleError1(func() error {
				nextHop, _ = SampleVerifyNextHop(ctx, nil, settings)
				return nil
			}, func(err error) error { return err })
			if err == nil || nextHop != nil || !strings.Contains(err.Error(), "WRONGTYPE") {
				t.Fatalf("%s: corrupt cohort state assigned %v (err %v)", corrupt.name, nextHop, err)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Del(ctx, corrupt.key).Err())
			})
		}
		noLeak("Redis error")

		// a corrupt draw counter fails the admission script before its first
		// write, also on a starved draw, which skips the growth-gate compare
		for _, counter := range []string{"x", "1.5"} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, keys.draws, counter, 0).Err())
			})
			err := server.HandleError1(func() error {
				SampleVerifyNextHop(ctx, nil, settings)
				return nil
			}, func(err error) error { return err })
			if err == nil || !strings.Contains(err.Error(), "draw counter") {
				t.Fatalf("draw counter %q: %v", counter, err)
			}
			err = server.HandleError1(func() error {
				server.Redis(ctx, func(r server.RedisClient) {
					admitVerifyCohort(ctx, r, keys, providers[0].String(), settings, true, nil)
				})
				return nil
			}, func(err error) error { return err })
			if err == nil || !strings.Contains(err.Error(), "draw counter") {
				t.Fatalf("starved admission with draw counter %q: %v", counter, err)
			}
			noLeak("script error")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, keys.draws).Err())
		})

		// a full cohort whose members all left assigns nothing, although
		// other eligible providers remain
		hops := testVerifyCohortSample(t, ctx, settings, 40)
		members := testVerifyCohortMembers(ctx, keys.epoch)
		if len(members) != 2 {
			t.Fatalf("cohort %v", members)
		}
		testVerifyCohortSubset(t, "hops", hops, members)
		for _, provider := range providers {
			if members[provider.String()] {
				ClearVerifyEgress(ctx, provider, ips[provider], settings)
			}
		}
		if nextHop, _ := SampleVerifyNextHop(ctx, nil, settings); nextHop != nil {
			t.Fatalf("departed cohort fell back to %s", nextHop)
		}
		if after := testVerifyCohortMembers(ctx, keys.epoch); len(after) != 2 {
			t.Fatalf("full cohort changed: %v", after)
		}
	})
}

// Many concurrent samplers through the full path: the epoch cohort fills to
// exactly its size and no other provider is ever assigned.
func TestVerifyCohortConcurrentSamplingStaysBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortSettings(8, 10)
		testVerifyCohortProviders(ctx, settings, 40)
		testVerifyCohortSetEpoch(ctx, 2)

		var lock sync.Mutex
		hops := map[string]bool{}
		var wg sync.WaitGroup
		for worker := 0; worker < 32; worker += 1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i += 1 {
					nextHop, _ := SampleVerifyNextHop(ctx, nil, settings)
					if nextHop != nil {
						lock.Lock()
						hops[nextHop.String()] = true
						lock.Unlock()
					}
				}
			}()
		}
		wg.Wait()
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 2)
		members := testVerifyCohortMembers(ctx, keys.epoch)
		if len(members) != 8 || len(testVerifyCohortMembers(ctx, keys.lifetime)) != 8 {
			t.Fatalf("cohort %v", members)
		}
		testVerifyCohortSubset(t, "concurrent hops", hops, members)
	})
}

// Independent processes: each admits its own candidates plus a shared set,
// with every process released at one barrier. The epoch phase must end at
// exactly the cohort size and the next epoch at exactly the lifetime limit,
// with each reported admission present and nothing else.
type testVerifyCohortChildConfig struct {
	Pg            []byte     `json:"pg"`
	Redis         []byte     `json:"redis"`
	CohortSize    int        `json:"cohort_size"`
	LifetimeLimit int        `json:"lifetime_limit"`
	Epochs        []uint64   `json:"epochs"`
	Candidates    [][]string `json:"candidates"`
}

type testVerifyCohortChildReport struct {
	Ready    bool     `json:"ready,omitempty"`
	Admitted []string `json:"admitted,omitempty"`
	Failure  string   `json:"failure,omitempty"`
}

const testVerifyCohortChildEnv = "URNETWORK_VERIFY_COHORT_CHILD"

type testVerifyCohortChild struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	reports *json.Decoder
	output  *strings.Builder
}

func TestVerifyCohortAdmissionIndependentProcesses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		const processes, ownPerPhase, shared = 4, 40, 12
		config := testVerifyCohortChildConfig{
			Pg:            server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName).Bytes(),
			Redis:         server.Vault.RequireSimpleResource("redis.yml").Bytes(),
			CohortSize:    60,
			LifetimeLimit: 75,
			Epochs:        []uint64{11, 12},
		}
		sharedIds := make([][]string, len(config.Epochs))
		for phase := range config.Epochs {
			for i := 0; i < shared; i += 1 {
				sharedIds[phase] = append(sharedIds[phase], server.NewId().String())
			}
		}

		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		children := []*testVerifyCohortChild{}
		defer func() {
			for _, child := range children {
				_ = child.stdin.Close()
				_ = child.command.Process.Kill()
				_ = child.command.Wait()
			}
		}()
		candidates := map[string]bool{}
		for process := 0; process < processes; process += 1 {
			childConfig := config
			childConfig.Candidates = make([][]string, len(config.Epochs))
			for phase := range config.Epochs {
				own := append([]string{}, sharedIds[phase]...)
				for i := 0; i < ownPerPhase; i += 1 {
					own = append(own, server.NewId().String())
				}
				childConfig.Candidates[phase] = own
				for _, id := range own {
					candidates[id] = true
				}
			}
			reportReader, reportWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, executable, "-test.run=^TestVerifyCohortAdmissionChildProcess$", "-test.count=1", "-test.timeout=90s")
			command.Env = append(os.Environ(), testVerifyCohortChildEnv+"=1")
			command.ExtraFiles = []*os.File{reportWriter} // fd 3 in the child
			output := &strings.Builder{}
			command.Stdout, command.Stderr = output, output
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			reportWriter.Close()
			child := &testVerifyCohortChild{command: command, stdin: stdin, reports: json.NewDecoder(reportReader), output: output}
			children = append(children, child)
			if err := json.NewEncoder(stdin).Encode(childConfig); err != nil {
				t.Fatal(err)
			}
		}

		read := func(child *testVerifyCohortChild) testVerifyCohortChildReport {
			var report testVerifyCohortChildReport
			if err := child.reports.Decode(&report); err != nil || report.Failure != "" {
				_ = child.command.Wait()
				t.Fatalf("child report: %v %s\n%s", err, report.Failure, child.output.String())
			}
			return report
		}
		for phase, epoch := range config.Epochs {
			for _, child := range children {
				if !read(child).Ready {
					t.Fatal("child not ready")
				}
			}
			// one barrier releases every process together
			for _, child := range children {
				if _, err := io.WriteString(child.stdin, "go\n"); err != nil {
					t.Fatal(err)
				}
			}
			admitted := map[string]bool{}
			for _, child := range children {
				for _, id := range read(child).Admitted {
					if !candidates[id] {
						t.Fatalf("child admitted unknown id %s", id)
					}
					admitted[id] = true
				}
			}
			keys := newVerifyCohortKeys(testVerifyCohortDeployment, epoch)
			members := testVerifyCohortMembers(ctx, keys.epoch)
			lifetime := testVerifyCohortMembers(ctx, keys.lifetime)
			want := config.CohortSize
			if phase == 1 {
				// all phase-1 candidates are new: the lifetime limit binds first
				want = config.LifetimeLimit - config.CohortSize
			}
			if len(members) != want || len(admitted) != want {
				t.Fatalf("epoch %d: cohort %d admitted %d, want %d", epoch, len(members), len(admitted), want)
			}
			testVerifyCohortSubset(t, "reported admissions", admitted, members)
			testVerifyCohortSubset(t, "cohort", members, admitted)
			if len(lifetime) != min(config.LifetimeLimit, config.CohortSize*(phase+1)) {
				t.Fatalf("epoch %d lifetime %d", epoch, len(lifetime))
			}
		}
		for _, child := range children {
			_ = child.stdin.Close()
			if err := child.command.Wait(); err != nil {
				t.Fatalf("child exit: %v\n%s", err, child.output.String())
			}
		}
		children = nil
	})
}

// Invoked only by TestVerifyCohortAdmissionIndependentProcesses through its
// own test executable, with the parent's disposable resources on stdin.
func TestVerifyCohortAdmissionChildProcess(t *testing.T) {
	if os.Getenv(testVerifyCohortChildEnv) != "1" {
		t.Skip("owned subprocess only")
	}
	if err := server.ValidateTestProcessPipe(3, server.TestProcessPipeWrite); err != nil {
		t.Fatal(err)
	}
	reports := os.NewFile(3, "verify-cohort-reports")
	defer reports.Close()
	encoder := json.NewEncoder(reports)
	fail := func(err error) {
		_ = encoder.Encode(testVerifyCohortChildReport{Failure: err.Error()})
		t.Fatal(err)
	}
	stdin := bufio.NewReader(os.Stdin)
	line, err := stdin.ReadBytes('\n')
	if err != nil {
		fail(err)
	}
	var config testVerifyCohortChildConfig
	if err := json.Unmarshal(line, &config); err != nil {
		fail(err)
	}
	if err := server.ValidateTestEnvironmentChildResources(t.Context(), config.Pg, config.Redis); err != nil {
		fail(err)
	}
	pop := server.Vault.PushSimpleResource("redis.yml", config.Redis)
	defer pop()
	server.RedisReset()
	ctx := context.Background()
	settings := testVerifyCohortSettings(config.CohortSize, config.LifetimeLimit)

	for phase, epoch := range config.Epochs {
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, epoch)
		if err := encoder.Encode(testVerifyCohortChildReport{Ready: true}); err != nil {
			t.Fatal(err)
		}
		if command, err := stdin.ReadString('\n'); err != nil || command != "go\n" {
			fail(errors.Join(errors.New("barrier not released"), err))
		}
		var lock sync.Mutex
		admitted := []string{}
		work := make(chan string)
		var wg sync.WaitGroup
		for worker := 0; worker < 16; worker += 1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for candidate := range work {
					var ok bool
					server.Redis(ctx, func(r server.RedisClient) {
						// skip the growth gate: this exercises the two caps
						ok, _ = admitVerifyCohort(ctx, r, keys, candidate, settings, true, nil)
					})
					if ok {
						lock.Lock()
						admitted = append(admitted, candidate)
						lock.Unlock()
					}
				}
			}()
		}
		for _, candidate := range config.Candidates[phase] {
			work <- candidate
		}
		close(work)
		wg.Wait()
		if err := encoder.Encode(testVerifyCohortChildReport{Admitted: admitted}); err != nil {
			t.Fatal(fmt.Errorf("report phase %d: %w", phase, err))
		}
	}
}
