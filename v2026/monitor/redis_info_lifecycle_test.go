// The public shared-INFO consumers must reject a completed callback whose
// parent ended before return, before changing baseline or healthy-row state.
package monitor

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Both consumers share complete, valid canonical rows. Only the middle node
// changes buffer pressure; every node and the fleet median remain observed.
func redisInfoLifecycleRows(broken bool) string {
	middle := "6381 100 1000 90 0 10"
	if broken {
		middle = "6381 100 1000 60 30 10"
	}
	return "6380 100 1000 90 0 10\n" + middle + "\n6382 100 1000 90 0 10"
}

func redisInfoLifecycleSettings(t *testing.T, source *syntheticSource) SignalSettings {
	t.Helper()
	settings := syntheticSettings(source)
	settings.StateDir = t.TempDir()
	// An unrelated disabled host selects the actual public scope adapter;
	// required Redis inventory remains permitted and complete.
	settings.disabledHosts = []HostSettings{{Name: "disabled-lifecycle.example"}}
	settings = settings.withDefaults().withRuntime()
	if err := settings.Validate(); err != nil {
		t.Fatal("control precondition: settings invalid")
	}
	if settings.runtime.baseline == nil || settings.runtime.baselineErr != nil {
		t.Fatal("control precondition: baseline unavailable")
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal("control precondition: environment unavailable")
	}
	if _, ok := env.runner.(*hostScopeRunner); !ok {
		t.Fatal("control precondition: actual scoped adapter missing")
	}
	return settings
}

// Observe both in-memory append counts and actual durable metric bytes. Do
// not print source errors, private state paths, or file bodies on failure.
type redisInfoLifecycleSnapshot struct {
	fileHashes map[string][32]byte
	appends    map[string]int
	samples    map[string]int
}

func redisInfoLifecycleState(t *testing.T, settings SignalSettings) redisInfoLifecycleSnapshot {
	t.Helper()
	store := settings.runtime.baseline
	store.stateLock.Lock()
	defer store.stateLock.Unlock()
	state := redisInfoLifecycleSnapshot{fileHashes: map[string][32]byte{}, appends: map[string]int{}, samples: map[string]int{}}
	entries, err := os.ReadDir(filepath.Join(settings.StateDir, "baseline"))
	if err != nil {
		t.Fatal("baseline snapshot unavailable")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatal("unexpected baseline directory")
		}
		body, err := os.ReadFile(filepath.Join(settings.StateDir, "baseline", entry.Name()))
		if err != nil {
			t.Fatal("baseline file snapshot unavailable")
		}
		state.fileHashes[entry.Name()] = sha256.Sum256(body)
	}
	for metric, count := range store.metricAppendCounts {
		state.appends[metric] = count
	}
	for metric, samples := range store.metricSamples {
		state.samples[metric] = len(samples)
	}
	return state
}

func TestRedisInfoLifecycleLateCensusPreservesBaseline(t *testing.T) {
	for _, consumer := range []string{"buffers", "connections"} {
		t.Run(consumer, func(t *testing.T) {
			calls := 0
			var endParent context.CancelFunc
			source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
				if !strings.Contains(command, "for p in") {
					return "", errors.New("synthetic history unavailable")
				}
				calls++
				if endParent != nil {
					endParent()
				}
				return redisInfoLifecycleRows(false), nil
			}}
			settings := redisInfoLifecycleSettings(t, source)
			var signal Signal = NewRedisBuffersSignal()
			if consumer == "connections" {
				signal = NewRedisConnectionsSignal()
			}
			if _, err := signal.Run(context.Background(), settings); err != nil {
				t.Fatal("control precondition: live complete census failed")
			}
			before := redisInfoLifecycleState(t, settings)
			if calls != 1 || before.appends["redis/used-median"] != 1 || before.appends["redis/clients-median"] != 1 || len(before.fileHashes) != 2 {
				t.Fatal("control precondition: complete census did not establish both persisted medians")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			endParent = cancel
			alerts, err := signal.Run(ctx, settings)
			if calls != 2 || !errors.Is(err, context.Canceled) || len(alerts) != 0 {
				t.Fatal("control precondition: late callback and public canceled result not reached")
			}
			if !reflect.DeepEqual(before, redisInfoLifecycleState(t, settings)) {
				t.Fatal("late completed census changed the persisted shared baseline after parent cancellation")
			}
		})
	}
}

func TestRedisInfoLifecycleLateHealthyCensusDoesNotRearmBufferBattery(t *testing.T) {
	broken := true
	censusCalls, batteryCalls := 0, 0
	var endParent context.CancelFunc
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "for p in") {
				censusCalls++
				if endParent != nil {
					endParent()
				}
				return redisInfoLifecycleRows(broken), nil
			}
			return "", nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if len(args) != 2 || args[0] != "INFO" || args[1] != "memory" {
				t.Fatal("unexpected buffer battery callback")
			}
			batteryCalls++
			return "", nil
		},
	}
	settings := redisInfoLifecycleSettings(t, source)
	signal := NewRedisBuffersSignal()
	first, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("control precondition: live trip failed")
	}
	requireAlertClass(t, first, "client-buffers")
	if batteryCalls != 1 || censusCalls != 1 {
		t.Fatal("control precondition: actual enabled buffer latch not tripped once")
	}
	broken = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endParent = cancel
	alerts, err := signal.Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || censusCalls != 2 || batteryCalls != 1 {
		t.Fatal("control precondition: canceled valid healthy return not reached")
	}
	endParent = nil
	broken = true
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("control precondition: subsequent live breach failed")
	}
	requireAlertClass(t, alerts, "client-buffers")
	if batteryCalls != 1 {
		t.Fatal("late healthy census rearmed the enabled buffer latch after parent cancellation")
	}
	// A real live healthy tick must still rearm: this prevents a fix from
	// passing by disabling normal latch recovery or diagnostic collection.
	broken = false
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal("live healthy control failed")
	}
	broken = true
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("live corrected trip failed")
	}
	requireAlertClass(t, alerts, "client-buffers")
	if batteryCalls != 2 {
		t.Fatal("live healthy census did not rearm enabled buffer latch")
	}
}

func TestRedisInfoLifecycleLateSourceErrorPreservesPrivateCause(t *testing.T) {
	for _, consumer := range []string{"buffers", "connections"} {
		t.Run(consumer, func(t *testing.T) {
			privateCause := errors.New("private-lifecycle-source-marker")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				calls++
				cancel()
				return redisInfoLifecycleRows(false), privateCause
			}}
			settings := redisInfoLifecycleSettings(t, source)
			before := redisInfoLifecycleState(t, settings)
			var signal Signal = NewRedisBuffersSignal()
			if consumer == "connections" {
				signal = NewRedisConnectionsSignal()
			}
			alerts, err := signal.Run(ctx, settings)
			if calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, privateCause) || len(alerts) != 0 {
				t.Fatal("late failed callback lost parent or independent source cause")
			}
			if strings.Contains(err.Error(), privateCause.Error()) {
				t.Fatal("private source error escaped canceled public error")
			}
			if !reflect.DeepEqual(before, redisInfoLifecycleState(t, settings)) {
				t.Fatal("late failed callback changed baseline")
			}
		})
	}
}

func TestRedisInfoLifecycleLivePartialKeepsIndependentPositive(t *testing.T) {
	privateCause := errors.New("private-lifecycle-partial-marker")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	deadline, bounded := ctx.Deadline()
	if !bounded || !time.Now().Before(deadline) {
		t.Fatal("control precondition: live bounded parent missing")
	}
	calls := map[string]int{}
	failedSibling, broken := true, true
	source := &syntheticSource{hostFn: func(h HostSettings, command string) (string, error) {
		if err := ctx.Err(); err != nil {
			t.Fatal("control precondition: live partial callback parent ended")
		}
		if strings.Contains(command, "for p in") {
			calls[h.Name]++
			if h.Name == "redis-partial.example" {
				if failedSibling {
					return "", privateCause
				}
				return "6480 100 1000 90 0 10", nil
			}
			return redisInfoLifecycleRows(broken), nil
		}
		return "", nil
	}}
	settings := redisInfoLifecycleSettings(t, source)
	// The original owner keeps the sole entry port. A second entry port is
	// rejected before collection and cannot exercise partial source semantics.
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "redis-partial.example", Roles: []string{"redis-cluster"}, RedisNodePorts: []int{6480}})
	if err := settings.Validate(); err != nil {
		t.Fatal("control precondition: final multi-host inventory invalid")
	}
	signal := NewRedisBuffersSignal()
	alerts, err := signal.Run(ctx, settings)
	if ctx.Err() != nil || !time.Now().Before(deadline) {
		t.Fatal("control precondition: partial run outlived parent")
	}
	if err != nil {
		t.Fatal("live partial source returned an unexpected execution error")
	}
	if len(calls) != 2 || calls["redis-1"] != 1 || calls["redis-partial.example"] != 1 {
		t.Fatal("control precondition: both inventory-owned census callbacks not reached once")
	}
	requireAlertClass(t, alerts, "client-buffers")
	unknown := false
	for _, alert := range alerts {
		unknown = unknown || alert.unobservable
		requireAlertOmits(t, alert, privateCause.Error())
	}
	if !unknown || len(redisInfoLifecycleState(t, settings).appends) != 0 {
		t.Fatal("live partial census lost its visibility gap or polluted fleet baseline")
	}

	// Correct the failed sibling and pressure together. The same live bounded
	// parent must still admit a complete healthy census and both medians.
	failedSibling, broken = false, false
	alerts, err = signal.Run(ctx, settings)
	if ctx.Err() != nil || !time.Now().Before(deadline) {
		t.Fatal("control precondition: corrected healthy run outlived parent")
	}
	if err != nil || len(alerts) != 0 {
		t.Fatal("corrected live complete census did not restore buffer health")
	}
	if len(calls) != 2 || calls["redis-1"] != 2 || calls["redis-partial.example"] != 2 {
		t.Fatal("control precondition: corrected census did not revisit both owners once")
	}
	state := redisInfoLifecycleState(t, settings)
	if state.appends["redis/used-median"] != 1 || state.appends["redis/clients-median"] != 1 || len(state.fileHashes) != 2 {
		t.Fatal("corrected live complete census did not restore persisted medians")
	}
}

func TestRedisInfoLifecycleCanceledStartsNoCensus(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		calls++
		return redisInfoLifecycleRows(false), nil
	}}
	settings := redisInfoLifecycleSettings(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewRedisBuffersSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 0 {
		t.Fatal("already canceled parent reached census or findings")
	}
}
