//go:build linux

// Bounded engine-cache maintenance preserves the independent admission proof:
// only a fresh kernel observation, never requested reclaim, supplies headroom.
package main

import (
	"context"
	"errors"
	"math"
	"os"
	"time"

	"github.com/urnetwork/server/competitionagent"
)

// Each host owns its maintenance policy. Nonpositive bounds refuse work;
// none of these fields changes the qualified management caps or admission reserve.
type cacheSettings struct {
	TriggerHeadroomBytes   int64         `json:"trigger_headroom_bytes"`   // Start below 768 MiB, before the real control reserve is threatened.
	TargetHeadroomBytes    int64         `json:"target_headroom_bytes"`    // Actual 1 GiB headroom rearms a later pressure episode.
	MaximumReclaimBytes    int64         `json:"maximum_reclaim_bytes"`    // At most 512 MiB in one kernel request.
	MinimumReclaimBytes    int64         `json:"minimum_reclaim_bytes"`    // Refuse less than 64 MiB rather than churn tiny requests.
	MaximumEpisodeBytes    int64         `json:"maximum_episode_bytes"`    // At most 1 GiB requested until real headroom returns.
	MaximumEpisodeAttempts int           `json:"maximum_episode_attempts"` // At most two issued or uncertain requests per episode.
	PassTimeout            time.Duration `json:"pass_timeout"`             // Fifteen seconds for one observation/reclaim owner.
	Interval               time.Duration `json:"interval"`                 // Thirty seconds between joined same-cgroup passes.
	StopTimeout            time.Duration `json:"stop_timeout"`             // Five seconds total for child TERM/KILL and positive join.
	ServiceMemoryBytes     int64         `json:"service_memory_bytes"`     // The companion's 32 MiB remains inside the private aggregate.
	ServiceTasksMaximum    int           `json:"service_tasks_maximum"`    // Thirty-two threads/tasks, not an additional aggregate allowance.
	DocumentBytes          int64         `json:"document_bytes"`           // Bound every kernel, settings and journal document to 64 KiB.
	MaximumStatKeys        int           `json:"maximum_stat_keys"`        // At most 256 unique kernel counter names.
	MaximumCgroupEntries   int           `json:"maximum_cgroup_entries"`   // Bound the daemon's entire leaf census, including kernel files.
}

// The sole defaults constructor is also used by unit rendering and tests.
func defaultCacheSettings() cacheSettings {
	return cacheSettings{TriggerHeadroomBytes: 768 * 1024 * 1024, TargetHeadroomBytes: 1024 * 1024 * 1024,
		MaximumReclaimBytes: 512 * 1024 * 1024, MinimumReclaimBytes: 64 * 1024 * 1024,
		MaximumEpisodeBytes: 1024 * 1024 * 1024, MaximumEpisodeAttempts: 2,
		PassTimeout: 15 * time.Second, Interval: 30 * time.Second, StopTimeout: 5 * time.Second,
		ServiceMemoryBytes: 32 * 1024 * 1024, ServiceTasksMaximum: 32, DocumentBytes: 64 * 1024, MaximumStatKeys: 256, MaximumCgroupEntries: 512}
}

// Qualified ceilings and the real control margin come from their existing owners.
func (self cacheSettings) validate() error {
	profile, agent := competitionagent.DefaultManagementBoundarySettings(), competitionagent.DefaultAgentSettings()
	if self.TriggerHeadroomBytes <= agent.ManagementMemoryHeadroomBytes || self.TargetHeadroomBytes <= self.TriggerHeadroomBytes ||
		self.TargetHeadroomBytes >= profile.PrivateMemoryBytes || self.MinimumReclaimBytes <= 0 || self.MaximumReclaimBytes < self.MinimumReclaimBytes ||
		self.MaximumEpisodeBytes < self.MaximumReclaimBytes || self.MaximumEpisodeBytes > profile.PrivateMemoryBytes || self.MaximumEpisodeAttempts <= 0 ||
		self.PassTimeout <= 0 || self.StopTimeout <= 0 || self.PassTimeout > time.Duration(math.MaxInt64)-self.StopTimeout || self.Interval <= self.PassTimeout+self.StopTimeout ||
		self.ServiceMemoryBytes <= 0 || self.ServiceMemoryBytes >= agent.ManagementMemoryHeadroomBytes || self.ServiceTasksMaximum <= 0 ||
		self.DocumentBytes <= 0 || self.DocumentBytes >= self.ServiceMemoryBytes || self.MaximumStatKeys <= 0 || self.MaximumCgroupEntries <= 0 || self.MaximumCgroupEntries == math.MaxInt {
		return errors.New("cache maintenance settings have no finite reserve-preserving owner")
	}
	return nil
}

// The whole private charge is measured; inactive pages are only an issue bound.
type cacheSnapshot struct {
	PrivateCurrentBytes int64 `json:"private_current_bytes"`
	PrivateMaximumBytes int64 `json:"private_maximum_bytes"`
	EngineFileBytes     int64 `json:"engine_file_bytes"`
	InactiveFileBytes   int64 `json:"inactive_file_bytes"`
	ShmemBytes          int64 `json:"shmem_bytes"`
	DirtyBytes          int64 `json:"dirty_bytes"`
	WritebackBytes      int64 `json:"writeback_bytes"`
}

// A fresh absolute charge, not an estimate of reclaimability, proves the margin.
func (self cacheSnapshot) headroom() (int64, error) {
	if self.PrivateMaximumBytes != competitionagent.DefaultManagementBoundarySettings().PrivateMemoryBytes || self.PrivateCurrentBytes < 0 ||
		self.PrivateCurrentBytes > self.PrivateMaximumBytes || self.EngineFileBytes < 0 || self.InactiveFileBytes < 0 ||
		self.InactiveFileBytes > self.EngineFileBytes || self.ShmemBytes != 0 || self.DirtyBytes < 0 || self.WritebackBytes < 0 {
		return 0, errors.New("cache observation differs from the fixed private/file-only boundary")
	}
	return self.PrivateMaximumBytes - self.PrivateCurrentBytes, nil
}

// Subtraction is conservative even when dirty pages are not on the inactive list.
func (self cacheSnapshot) inactiveCleanBytes() int64 {
	available := self.InactiveFileBytes
	for _, charge := range []int64{self.DirtyBytes, self.WritebackBytes} {
		if charge >= available {
			return 0
		}
		available -= charge
	}
	return available
}

// A durable pre-issue debit survives process death or an unacknowledged write.
// Only positively observed target headroom permits a new episode.
type cacheState struct {
	Schema         int   `json:"schema"`
	Active         bool  `json:"active"`
	Attempts       int   `json:"attempts"`
	RequestedBytes int64 `json:"requested_bytes"`
}

// Kernel and journal seams are per owner, never command-line native-proof waivers.
type cacheKernel interface {
	Observe(context.Context) (cacheSnapshot, error)
	Reclaim(context.Context, int64) (bool, error)
}

type cacheJournal interface {
	Load() (cacheState, error)
	Save(cacheState) error
}

// Reports observations only; this process cannot mint grading or host readiness.
type cacheResult struct {
	Status                string         `json:"status"`
	RequestedBytes        int64          `json:"requested_bytes"`
	Issued                bool           `json:"issued"`
	Before                *cacheSnapshot `json:"before,omitempty"`
	After                 *cacheSnapshot `json:"after,omitempty"`
	ActualHeadroomBytes   int64          `json:"actual_headroom_bytes"`
	ControlMarginObserved bool           `json:"private_control_margin_observed"`
	EpisodeAttempts       int            `json:"episode_attempts"`
	EpisodeRequestedBytes int64          `json:"episode_requested_bytes"`
}

// One pass performs at most one positive kernel write and never assumes its effect.
func maintainCache(parent context.Context, settings cacheSettings, kernel cacheKernel, journal cacheJournal) (result cacheResult, resultErr error) {
	result.Status = "invalid_owner"
	if err := settings.validate(); err != nil || kernel == nil || journal == nil {
		return result, errors.New("cache maintenance has no valid concrete owner")
	}
	ctx, cancel := context.WithTimeout(parent, settings.PassTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Status = "journal_unavailable"
	state, err := journal.Load()
	if err != nil {
		return result, err
	}
	if state.Schema != 1 || state.Attempts < 0 || state.RequestedBytes < 0 || !state.Active && (state.Attempts != 0 || state.RequestedBytes != 0) {
		return result, errors.New("cache maintenance journal is invalid")
	}
	result.Status = "initial_observation_unavailable"
	before, err := kernel.Observe(ctx)
	if err != nil {
		return result, err
	}
	result.Status = "initial_evidence_invalid"
	headroom, err := before.headroom()
	if err != nil {
		return result, err
	}
	result.Before, result.ActualHeadroomBytes = &before, headroom
	result.ControlMarginObserved = headroom >= competitionagent.DefaultAgentSettings().ManagementMemoryHeadroomBytes
	if headroom >= settings.TargetHeadroomBytes {
		if state.Active {
			if err := journal.Save(cacheState{Schema: 1}); err != nil {
				return result, err
			}
		}
		result.Status = "target_observed"
		return result, nil
	}
	if !state.Active && headroom >= settings.TriggerHeadroomBytes {
		result.Status = "above_trigger"
		return result, nil
	}
	result.EpisodeAttempts, result.EpisodeRequestedBytes = state.Attempts, state.RequestedBytes
	if state.Attempts >= settings.MaximumEpisodeAttempts || state.RequestedBytes >= settings.MaximumEpisodeBytes {
		result.Status = "episode_budget_exhausted"
		return result, errors.New("cache maintenance episode budget exhausted before observed recovery")
	}
	amount := min(settings.MaximumReclaimBytes, settings.MaximumEpisodeBytes-state.RequestedBytes, settings.TargetHeadroomBytes-headroom, before.inactiveCleanBytes())
	pageBytes := int64(os.Getpagesize())
	amount = amount / pageBytes * pageBytes
	if amount < settings.MinimumReclaimBytes {
		result.Status = "inactive_clean_cache_unavailable"
		return result, errors.New("cache maintenance has insufficient positively observed clean inactive file pages")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	state.Active, state.Attempts, state.RequestedBytes = true, state.Attempts+1, state.RequestedBytes+amount
	result.Status = "pre_issue_debit_unavailable"
	if err := journal.Save(state); err != nil {
		return result, err
	}
	result.RequestedBytes, result.EpisodeAttempts, result.EpisodeRequestedBytes = amount, state.Attempts, state.RequestedBytes
	result.Issued, resultErr = kernel.Reclaim(ctx, amount)
	// A missing post-observation cannot retain the earlier margin as current.
	result.ActualHeadroomBytes, result.ControlMarginObserved = 0, false
	result.Status = "post_observation_unavailable"
	after, err := kernel.Observe(ctx)
	if err != nil {
		return result, errors.Join(resultErr, err)
	}
	headroom, err = after.headroom()
	if err != nil {
		return result, errors.Join(resultErr, err)
	}
	result.After, result.ActualHeadroomBytes = &after, headroom
	result.ControlMarginObserved = headroom >= competitionagent.DefaultAgentSettings().ManagementMemoryHeadroomBytes
	result.Status = "target_pending"
	if headroom >= settings.TargetHeadroomBytes {
		result.Status = "target_observed"
		if err := journal.Save(cacheState{Schema: 1}); err != nil {
			return result, errors.Join(resultErr, err)
		}
	}
	return result, errors.Join(resultErr, ctx.Err())
}
