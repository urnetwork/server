//go:build linux

// Explicit kernel/journal transitions reproduce cache-starved control admission
// without native writes, timing races, production identities or readiness waivers.
package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/competitionagent"
)

// Every independent fixture owns its complete episode journal and kernel seam.
type cacheTestJournal struct {
	state cacheState
	saves []cacheState
	err   error
}

// The fixture starts with an explicit schema rather than an implied valid state.
func (self *cacheTestJournal) Load() (cacheState, error) { return self.state, nil }

// A failed durable debit never authorizes the subsequent kernel write.
func (self *cacheTestJournal) Save(state cacheState) error {
	if self.err != nil {
		return self.err
	}
	self.state = state
	self.saves = append(self.saves, state)
	return nil
}

// The callback fixes the exact observation/write ordering; no sleep is needed.
type cacheTestKernel struct {
	snapshot cacheSnapshot
	observed int
	requests []int64
	observe  func(int) (cacheSnapshot, error)
	reclaim  func(int64) (bool, error)
}

// Tests can force a post-observation failure independently of kernel issue.
func (self *cacheTestKernel) Observe(ctx context.Context) (cacheSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return cacheSnapshot{}, err
	}
	self.observed++
	if self.observe != nil {
		return self.observe(self.observed)
	}
	return self.snapshot, nil
}

// One entry records one attempted kernel request, not its assumed effect.
func (self *cacheTestKernel) Reclaim(ctx context.Context, amount int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	self.requests = append(self.requests, amount)
	if self.reclaim != nil {
		return self.reclaim(amount)
	}
	return true, nil
}

// Synthetic charge reproduces a real reserve deficit with plenty of idle cache.
func cacheStarvedSnapshot() cacheSnapshot {
	maximum := competitionagent.DefaultManagementBoundarySettings().PrivateMemoryBytes
	return cacheSnapshot{PrivateMaximumBytes: maximum, PrivateCurrentBytes: maximum - 100*1024*1024,
		EngineFileBytes: 2 * 1024 * 1024 * 1024, InactiveFileBytes: 1024 * 1024 * 1024}
}

// Requesting half a GiB cannot turn a partial ten-MiB reclaim into a healthy reserve.
func TestCacheMaintenanceRequiresActualPostReclaimReserve(t *testing.T) {
	settings := defaultCacheSettings()
	journal := &cacheTestJournal{state: cacheState{Schema: 1}}
	kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
	kernel.reclaim = func(amount int64) (bool, error) {
		if journal.state.Attempts != 1 || journal.state.RequestedBytes != amount || !journal.state.Active {
			t.Fatal("kernel issue preceded its durable conservative debit")
		}
		kernel.snapshot.PrivateCurrentBytes -= 10 * 1024 * 1024
		return true, nil
	}
	result, err := maintainCache(t.Context(), settings, kernel, journal)
	if err != nil || len(kernel.requests) != 1 || result.RequestedBytes != settings.MaximumReclaimBytes ||
		result.ControlMarginObserved || result.ActualHeadroomBytes != 110*1024*1024 || result.Status != "target_pending" || !journal.state.Active {
		t.Fatal("requested or estimated cache was credited as real control headroom", result, err)
	}
}

// Lost acknowledgments spend the same finite budget after process reconstruction.
func TestCacheMaintenancePersistsEpisodeBudgetAcrossUncertainRetries(t *testing.T) {
	settings := defaultCacheSettings()
	journal := &cacheTestJournal{state: cacheState{Schema: 1}}
	kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot(), reclaim: func(int64) (bool, error) {
		return true, errors.New("synthetic lost kernel acknowledgment")
	}}
	for range settings.MaximumEpisodeAttempts {
		if _, err := maintainCache(t.Context(), settings, kernel, journal); err == nil {
			t.Fatal("uncertain kernel acknowledgment became a successful pass")
		}
	}
	result, err := maintainCache(t.Context(), settings, kernel, journal)
	if err == nil || result.Status != "episode_budget_exhausted" || len(kernel.requests) != settings.MaximumEpisodeAttempts ||
		journal.state.RequestedBytes != settings.MaximumEpisodeBytes {
		t.Fatal("unchanged pressure caused unbounded automatic reclaim", result, err)
	}
	independent := &cacheTestJournal{state: cacheState{Schema: 1}}
	other := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
	if _, err := maintainCache(t.Context(), settings, other, independent); err != nil || len(other.requests) != 1 {
		t.Fatal("one host's exhausted episode denied an independent owner's maintenance", err)
	}
}

// A restored trigger margin is not proof of the stronger episode-rearm target.
func TestCacheMaintenanceRearmsOnlyAfterObservedTargetHeadroom(t *testing.T) {
	settings := defaultCacheSettings()
	journal := &cacheTestJournal{state: cacheState{Schema: 1, Active: true, Attempts: settings.MaximumEpisodeAttempts, RequestedBytes: settings.MaximumEpisodeBytes}}
	kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
	kernel.snapshot.PrivateCurrentBytes = kernel.snapshot.PrivateMaximumBytes - settings.TriggerHeadroomBytes
	if _, err := maintainCache(t.Context(), settings, kernel, journal); err == nil || len(kernel.requests) != 0 || !journal.state.Active {
		t.Fatal("partial margin restored a consumed episode budget")
	}
	kernel.snapshot.PrivateCurrentBytes = kernel.snapshot.PrivateMaximumBytes - settings.TargetHeadroomBytes
	if result, err := maintainCache(t.Context(), settings, kernel, journal); err != nil || result.Status != "target_observed" || journal.state.Active || len(kernel.requests) != 0 {
		t.Fatal("positive actual recovery did not rearm future maintenance", result, err)
	}
	kernel.snapshot = cacheStarvedSnapshot()
	if _, err := maintainCache(t.Context(), settings, kernel, journal); err != nil || len(kernel.requests) != 1 || journal.state.Attempts != 1 {
		t.Fatal("a new positively separated episode could not run", err)
	}
}

// Dirty, writeback and page rounding all constrain the sole positive request.
func TestCacheMaintenanceBoundsCleanInactiveFilePages(t *testing.T) {
	settings := defaultCacheSettings()
	journal := &cacheTestJournal{state: cacheState{Schema: 1}}
	kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
	kernel.snapshot.InactiveFileBytes = 300 * 1024 * 1024
	kernel.snapshot.DirtyBytes, kernel.snapshot.WritebackBytes = 64*1024*1024, 32*1024*1024
	result, err := maintainCache(t.Context(), settings, kernel, journal)
	if err != nil || len(kernel.requests) != 1 || kernel.requests[0] != 204*1024*1024 || result.RequestedBytes != kernel.requests[0] {
		t.Fatal("reclaim exceeded positively observed clean inactive pages", result, err)
	}
}

// Every invalid observation fails before journal debit or kernel mutation.
func TestCacheMaintenanceRefusesMissingAndForeignResourceEvidence(t *testing.T) {
	for _, change := range []func(*cacheSnapshot){
		func(value *cacheSnapshot) { value.PrivateMaximumBytes++ },
		func(value *cacheSnapshot) { value.PrivateCurrentBytes = -1 },
		func(value *cacheSnapshot) { value.ShmemBytes = 1 },
		func(value *cacheSnapshot) { value.InactiveFileBytes = value.EngineFileBytes + 1 },
		func(value *cacheSnapshot) { value.DirtyBytes = -1 },
	} {
		journal := &cacheTestJournal{state: cacheState{Schema: 1}}
		kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
		change(&kernel.snapshot)
		if _, err := maintainCache(t.Context(), defaultCacheSettings(), kernel, journal); err == nil || len(kernel.requests) != 0 || len(journal.saves) != 0 {
			t.Fatal("invalid or foreign resource evidence authorized mutation", err)
		}
	}
}

// Persistence and post-read failures have distinct edges and neither invents recovery.
func TestCacheMaintenanceRefusesUndurableDebitAndUnknownPostObservation(t *testing.T) {
	settings := defaultCacheSettings()
	journal := &cacheTestJournal{state: cacheState{Schema: 1}, err: errors.New("synthetic journal failure")}
	kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
	if _, err := maintainCache(t.Context(), settings, kernel, journal); err == nil || len(kernel.requests) != 0 {
		t.Fatal("failed durable debit authorized a kernel write")
	}
	journal.err = nil
	kernel.observed = 0
	kernel.observe = func(call int) (cacheSnapshot, error) {
		if call > 1 {
			return cacheSnapshot{}, errors.New("synthetic missing post-reclaim proof")
		}
		return cacheStarvedSnapshot(), nil
	}
	result, err := maintainCache(t.Context(), settings, kernel, journal)
	if err == nil || result.After != nil || result.ControlMarginObserved || result.ActualHeadroomBytes != 0 || !journal.state.Active {
		t.Fatal("missing post-read retained optimistic availability", result, err)
	}
}

// The installed units use the existing aggregate and no managed-agent gate.
func TestCacheMaintenanceUnitStartsIndependentlyInsideExistingBudget(t *testing.T) {
	settings := defaultCacheSettings()
	service, err := renderCacheUnit(settings, "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Slice=urnetwork-management-private.slice\n", "MemoryMax=33554432\n", "MemorySwapMax=0\n", "Restart=no\n",
		"CapabilityBoundingSet=\n", "ReadOnlyPaths=/sys/fs/cgroup\n", "ReadWritePaths=" + cachePrivatePath + "/" + cacheEngineName + "/memory.reclaim\n"} {
		if !strings.Contains(service, required) {
			t.Fatal("bounded companion unit omitted its existing-resource confinement", required)
		}
	}
	for _, forbidden := range []string{"--serve", "--start", "--cancel", "memory.max=", "drop_caches", "Requires=", "ExecStartPre="} {
		if strings.Contains(service, forbidden) {
			t.Fatal("maintenance introduced an admission cycle or another mutation", forbidden)
		}
	}
	timer, err := renderCacheUnit(settings, "", false)
	if err != nil || !strings.Contains(timer, "OnUnitInactiveSec=") || !strings.Contains(timer, "Unit=urnetwork-management-cache.service\n") {
		t.Fatal("timer can overlap or escape its joined service owner", err)
	}
	if _, err := renderCacheUnit(settings, "/synthetic/settings%other.json", true); err == nil {
		t.Fatal("settings path expanded manager authority")
	}
}

// Numeric policy cannot disable a bound, widen the real reserve, or overflow time.
func TestCacheMaintenanceSettingsPreserveFiniteReserveOwner(t *testing.T) {
	if err := defaultCacheSettings().validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*cacheSettings){
		func(value *cacheSettings) {
			value.TriggerHeadroomBytes = competitionagent.DefaultAgentSettings().ManagementMemoryHeadroomBytes
		},
		func(value *cacheSettings) { value.TargetHeadroomBytes = value.TriggerHeadroomBytes },
		func(value *cacheSettings) { value.MaximumReclaimBytes = 0 },
		func(value *cacheSettings) { value.MinimumReclaimBytes = 0 },
		func(value *cacheSettings) { value.MaximumEpisodeBytes = value.MaximumReclaimBytes - 1 },
		func(value *cacheSettings) { value.MaximumEpisodeAttempts = 0 },
		func(value *cacheSettings) { value.PassTimeout = 0 },
		func(value *cacheSettings) { value.PassTimeout = time.Duration(math.MaxInt64) },
		func(value *cacheSettings) {
			value.ServiceMemoryBytes = competitionagent.DefaultAgentSettings().ManagementMemoryHeadroomBytes
		},
		func(value *cacheSettings) { value.MaximumCgroupEntries = math.MaxInt },
	} {
		settings := defaultCacheSettings()
		change(&settings)
		if err := settings.validate(); err == nil {
			t.Fatal("invalid policy disabled or overflowed a finite owner")
		}
	}
}
