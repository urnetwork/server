// Production-safety checks for the local dial guard: a process that is not in
// the local environment keeps the pools' own dial and lookup, through the real
// startup and first-use choice and the real pool constructors. Nothing here
// opens a socket: the pools are built with min_connections 0 on a
// documentation address, and the only dials use an already canceled context,
// which net.Dialer refuses before it creates a socket.
package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Compares two values field by field: functions by code pointer, so the same
// function or closure literal is equal and a wrapper is not, and everything
// else by value, recursing through pointers, interfaces, structs, slices and
// maps. The same pointer on both sides is the same object and is not walked.
// String values are never printed: a connection string carries a password.
func testEnvValueDiffs(path string, a reflect.Value, b reflect.Value, skipFields map[string]bool) []string {
	if a.IsValid() != b.IsValid() {
		return []string{path + ": validity differs"}
	}
	if !a.IsValid() {
		return nil
	}
	if a.Type() != b.Type() {
		return []string{fmt.Sprintf("%s: type %s vs %s", path, a.Type(), b.Type())}
	}
	switch a.Kind() {
	case reflect.Func:
		if a.IsNil() != b.IsNil() || (!a.IsNil() && a.Pointer() != b.Pointer()) {
			return []string{path + ": function differs"}
		}
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				return []string{path + ": nil-ness differs"}
			}
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return nil
		}
		return testEnvValueDiffs(path, a.Elem(), b.Elem(), skipFields)
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				return []string{path + ": nil-ness differs"}
			}
			return nil
		}
		return testEnvValueDiffs(path, a.Elem(), b.Elem(), skipFields)
	case reflect.Struct:
		diffs := []string{}
		for i := 0; i < a.NumField(); i++ {
			name := a.Type().Field(i).Name
			if skipFields[name] {
				continue
			}
			diffs = append(diffs, testEnvValueDiffs(path+"."+name, a.Field(i), b.Field(i), skipFields)...)
		}
		return diffs
	case reflect.Slice, reflect.Array:
		if a.Kind() == reflect.Slice && a.IsNil() != b.IsNil() {
			return []string{path + ": nil-ness differs"}
		}
		if a.Len() != b.Len() {
			return []string{path + ": length differs"}
		}
		diffs := []string{}
		for i := 0; i < a.Len(); i++ {
			diffs = append(diffs, testEnvValueDiffs(fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(i), skipFields)...)
		}
		return diffs
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return []string{path + ": map differs"}
		}
		if a.Pointer() == b.Pointer() {
			return nil
		}
		diffs := []string{}
		for _, key := range a.MapKeys() {
			other := b.MapIndex(key)
			if !other.IsValid() {
				diffs = append(diffs, fmt.Sprintf("%s[%v]: missing", path, key))
				continue
			}
			diffs = append(diffs, testEnvValueDiffs(fmt.Sprintf("%s[%v]", path, key), a.MapIndex(key), other, skipFields)...)
		}
		return diffs
	case reflect.Chan, reflect.UnsafePointer:
		if a.IsNil() != b.IsNil() {
			return []string{path + ": nil-ness differs"}
		}
	case reflect.Bool:
		if a.Bool() != b.Bool() {
			return []string{path + ": bool differs"}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if a.Int() != b.Int() {
			return []string{fmt.Sprintf("%s: %d vs %d", path, a.Int(), b.Int())}
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if a.Uint() != b.Uint() {
			return []string{fmt.Sprintf("%s: %d vs %d", path, a.Uint(), b.Uint())}
		}
	case reflect.Float32, reflect.Float64:
		if a.Float() != b.Float() {
			return []string{path + ": float differs"}
		}
	case reflect.String:
		if a.String() != b.String() {
			return []string{path + ": string differs"}
		}
	default:
		return []string{path + ": unhandled kind " + a.Kind().String()}
	}
	return nil
}

// Puts the process choice back in the deferred state of a service binary that
// started without WARP_ENV, and restores the test binary's choice afterwards.
func deferTestEnvProcessDialGuardChoice(t *testing.T) {
	t.Helper()
	previous := testEnvProcessDialGuard.Swap(nil)
	t.Cleanup(func() {
		testEnvProcessDialGuard.Store(previous)
	})
}

// Sets a variable for one test, or unsets it, and restores it afterwards.
func setTestEnvVariable(t *testing.T, name string, value string, unset bool) {
	t.Helper()
	t.Setenv(name, value)
	if unset {
		os.Unsetenv(name)
	}
}

// Every WARP_ENV value that is not local, in any case, yields no guard from the
// real os.Getenv-backed constructor, with stray test markers, an attestation
// and the portable flags present. The constructor's resolver is never
// consulted.
func TestNonLocalWarpEnvValuesNeverGuard(t *testing.T) {
	t.Setenv("WARP_VAULT_HOME", "/synthetic/vault")
	t.Setenv("WARP_SERVICE", "test")
	t.Setenv("WARP_BLOCK", "test")
	t.Setenv(testEnvAttestedAddrsEnvName, testEnvFixtureAttestation)
	t.Setenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES", "1")
	t.Setenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES", "1")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "/synthetic/private-root")
	for _, value := range []string{"<unset>", "", " ", "main", "Main", "locals", "lo cal", "beta", "test", "sim-testnet-op-1"} {
		setTestEnvVariable(t, "WARP_ENV", value, value == "<unset>")
		if guard := newTestEnvProcessDialGuard(); guard != nil {
			t.Errorf("WARP_ENV=%q: the real constructor returned a guard", value)
		}
		if guard := newTestEnvDialGuard(os.Getenv, testEnvUnusedResolver(t)); guard != nil {
			t.Errorf("WARP_ENV=%q: newTestEnvDialGuard returned a guard", value)
		}
	}
}

// The production sequence through the real environment and process choice. A
// binary that starts with WARP_ENV=main locks in a nil guard at startup; one
// that starts without WARP_ENV defers and locks in nil at its first guarded
// construction once main selected production. Afterwards a WARP_ENV=local
// cannot add a guard, and the steady-state lookup allocates nothing.
func TestProductionProcessLocksNilGuardThroughRealEnvironment(t *testing.T) {
	t.Setenv("WARP_VAULT_HOME", "/synthetic/vault")
	t.Setenv("WARP_SERVICE", "api")
	t.Setenv(testEnvAttestedAddrsEnvName, testEnvFixtureAttestation)
	for _, startupEnv := range []string{"main", ""} {
		deferTestEnvProcessDialGuardChoice(t)
		setTestEnvVariable(t, "WARP_ENV", startupEnv, startupEnv == "")
		startup := chooseTestEnvStartupDialGuard(false, os.Getenv("WARP_ENV"), newTestEnvProcessDialGuard)
		if (startup != nil) != (startupEnv != "") || (startup != nil && startup.guard != nil) {
			t.Fatalf("startup WARP_ENV=%q: startup choice = %+v", startupEnv, startup)
		}
		testEnvProcessDialGuard.Store(startup)
		t.Setenv("WARP_ENV", "main")
		if guard := currentTestEnvDialGuard(); guard != nil {
			t.Fatalf("startup WARP_ENV=%q: the first guarded construction chose a guard", startupEnv)
		}
		chosen := testEnvProcessDialGuard.Load()
		if chosen == nil || chosen.guard != nil {
			t.Fatalf("startup WARP_ENV=%q: after first use the choice = %+v; want a locked nil guard", startupEnv, chosen)
		}
		t.Setenv("WARP_ENV", "local")
		if guard := currentTestEnvDialGuard(); guard != nil {
			t.Fatalf("startup WARP_ENV=%q: a later WARP_ENV=local added a guard to a production process", startupEnv)
		}
		if testEnvProcessDialGuard.Load() != chosen {
			t.Fatalf("startup WARP_ENV=%q: a later call replaced the locked choice", startupEnv)
		}
		if allocs := testing.AllocsPerRun(1000, func() { _ = currentTestEnvDialGuard() }); allocs != 0 {
			t.Fatalf("startup WARP_ENV=%q: currentTestEnvDialGuard allocates %.1f per call after the choice", startupEnv, allocs)
		}
	}
}

// The pool configuration the constructor produced before the guard, for the
// synthetic resources pushTestEnvGuardPgResources installs.
func testEnvReferencePgPoolConfig(t *testing.T) *pgxpool.Config {
	t.Helper()
	reference, err := pgxpool.ParseConfig("postgres://synthetic:synthetic@192.0.2.10:5432/synthetic?" +
		"sslmode=disable&connect_timeout=5&pool_max_conns=1&pool_min_conns=0&pool_max_conn_lifetime=8h&" +
		"pool_max_conn_lifetime_jitter=15m&pool_max_conn_idle_time=60m&pool_health_check_period=1s")
	if err != nil {
		t.Fatal(err)
	}
	configurePgPoolReadOnly(reference)
	configurePgPoolLiveness(reference)
	configurePgPoolWriteTracking(reference)
	configurePgPoolStatementErrors(reference)
	return reference
}

// The four Redis pools the process builds.
func testEnvRedisPools() []*safeRedisClient {
	return []*safeRedisClient{
		{},
		{disableCommandRetry: true},
		{disableCommandRetry: true, contextTimeoutEnabled: true},
		{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true},
	}
}

// In a WARP_ENV=main process the real PostgreSQL pool constructor yields a
// configuration equal, field by field and function by function, to the
// pre-guard producer chain, and the real PostgreSQL pool and all four real
// Redis pools dial through the plain net.Dialer (a canceled context, so no
// socket). The same calls in a local process without an attestation are
// refused, so the check can tell the two apart.
func TestProductionPoolsKeepDefaultDialAndLookup(t *testing.T) {
	t.Setenv("WARP_ENV", "main")
	t.Setenv(testEnvAttestedAddrsEnvName, testEnvFixtureAttestation)
	setTestEnvVariable(t, "WARP_SERVICE", "", true)
	deferTestEnvProcessDialGuardChoice(t)
	if guard := currentTestEnvDialGuard(); guard != nil {
		t.Fatal("production chose a guard")
	}

	pushTestEnvGuardPgResources(t)
	t.Cleanup(Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.10:6379\npassword: \"\"\ndb: 3\n")))
	t.Cleanup(Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 1\n")))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	pgPool := &safePgPool{ctx: context.Background(), vaultResourceName: DefaultPgVaultResourceName, configResourceName: DefaultPgConfigResourceName}
	production := pgPool.open().Config()
	pgPool.reset()
	reference := testEnvReferencePgPoolConfig(t)
	skipFields := map[string]bool{"connString": true}
	if diffs := testEnvValueDiffs("pgxpool.Config", reflect.ValueOf(production), reflect.ValueOf(reference), skipFields); len(diffs) != 0 {
		t.Fatalf("production pool configuration differs from the pre-guard producers:\n%s", strings.Join(diffs, "\n"))
	}
	if _, err := production.ConnConfig.DialFunc(canceled, "tcp", "192.0.2.10:5432"); !errors.Is(err, context.Canceled) ||
		strings.Contains(err.Error(), "environment refused") {
		t.Fatalf("production PostgreSQL dial = %v; want the plain dialer's cancellation", err)
	}
	for index, pool := range testEnvRedisPools() {
		client := pool.open().(*redis.Client)
		_, err := client.Options().Dialer(canceled, "tcp", "192.0.2.10:6379")
		pool.close()
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "environment refused") {
			t.Errorf("production Redis pool %d dial = %v; want the plain dialer's cancellation", index, err)
		}
	}

	// control: the same constructors in a local process without an attestation
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvGetenv(map[string]string{"WARP_ENV": "local"}), testEnvUnusedResolver(t)))
	guarded := pgPool.open().Config()
	pgPool.reset()
	if diffs := testEnvValueDiffs("pgxpool.Config", reflect.ValueOf(guarded), reflect.ValueOf(reference), skipFields); len(diffs) == 0 {
		t.Fatal("control: a guarded configuration compared equal; the comparison cannot see the guard")
	}
	if _, err := guarded.ConnConfig.DialFunc(canceled, "tcp", "192.0.2.10:5432"); err == nil || !strings.Contains(err.Error(), "local environment refused") {
		t.Fatalf("control PostgreSQL dial = %v; want the guard's refusal", err)
	}
	for index, pool := range testEnvRedisPools() {
		client := pool.open().(*redis.Client)
		_, err := client.Options().Dialer(canceled, "tcp", "192.0.2.10:6379")
		pool.close()
		if err == nil || !strings.Contains(err.Error(), "local environment refused") {
			t.Errorf("control Redis pool %d dial = %v; want the guard's refusal", index, err)
		}
	}
}

// Every goroutine is held inside the constructor until all have entered it, so
// all of them race the CompareAndSwap with distinct guards: one wins, and every
// caller, losers included, returns the winner's guard.
func TestFirstGuardChoiceAgreesUnderForcedContention(t *testing.T) {
	const goroutineCount = 32
	var choice atomic.Pointer[testEnvDialGuardChoice]
	var enteredCount atomic.Int64
	allEntered := make(chan struct{})
	newGuard := func() *testEnvDialGuard {
		if enteredCount.Add(1) == goroutineCount {
			close(allEntered)
		}
		<-allEntered
		return &testEnvDialGuard{attestationErr: errors.New("synthetic unattested")}
	}
	guards := make([]*testEnvDialGuard, goroutineCount)
	var wait sync.WaitGroup
	for i := 0; i < goroutineCount; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			guards[i] = chooseTestEnvDialGuard(&choice, newGuard)
		}(i)
	}
	wait.Wait()
	chosen := choice.Load()
	if chosen == nil || chosen.guard == nil {
		t.Fatalf("choice = %+v", chosen)
	}
	for i, guard := range guards {
		if guard != chosen.guard {
			t.Fatalf("goroutine %d got %p; the choice is %p", i, guard, chosen.guard)
		}
	}
	if enteredCount.Load() != goroutineCount {
		t.Fatalf("constructor ran %d times; want %d", enteredCount.Load(), goroutineCount)
	}
}

// Goroutines making the first choice at once agree on one guard, or one nil,
// and once the choice is made the constructor never runs again.
func TestFirstGuardChoiceAgreesAndNeverReconstructs(t *testing.T) {
	for _, local := range []bool{false, true} {
		var choice atomic.Pointer[testEnvDialGuardChoice]
		var callCount atomic.Int64
		newGuard := func() *testEnvDialGuard {
			callCount.Add(1)
			if !local {
				return nil
			}
			return &testEnvDialGuard{attestationErr: errors.New("synthetic unattested")}
		}
		const goroutineCount = 64
		guards := make([]*testEnvDialGuard, goroutineCount)
		start := make(chan struct{})
		var wait sync.WaitGroup
		for i := 0; i < goroutineCount; i++ {
			wait.Add(1)
			go func(i int) {
				defer wait.Done()
				<-start
				guards[i] = chooseTestEnvDialGuard(&choice, newGuard)
			}(i)
		}
		close(start)
		wait.Wait()
		chosen := choice.Load()
		if chosen == nil || (chosen.guard != nil) != local {
			t.Fatalf("local=%t: choice = %+v", local, chosen)
		}
		for i, guard := range guards {
			if guard != chosen.guard {
				t.Fatalf("local=%t: goroutine %d got %p; the choice is %p", local, i, guard, chosen.guard)
			}
		}
		madeCount := callCount.Load()
		for i := 0; i < 1000; i++ {
			chooseTestEnvDialGuard(&choice, newGuard)
		}
		if callCount.Load() != madeCount {
			t.Fatalf("local=%t: the constructor ran after the choice", local)
		}
	}
}
