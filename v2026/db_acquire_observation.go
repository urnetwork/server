package server

// Invocation-owned acquisition observations do not control admission, retries,
// cancellation or connection lifetime. The caller reads only after unwinding.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A stopped context and a missing deadline remain distinct. No cause text,
// identifier, absolute deadline or context reference escapes the observation.
type DbContextBudget struct {
	AtUnixNano  int64  `json:"at_unix_ns"`
	HasDeadline bool   `json:"has_deadline"`
	RemainingNs int64  `json:"remaining_ns"`
	State       string `json:"state"`
}

// Samples only finite state and remaining allowance; concurrent cancellation
// can occur between the state and deadline reads.
func ObserveDbContextBudget(ctx context.Context) DbContextBudget {
	at := time.Now()
	state := "live"
	switch ctx.Err() {
	case nil:
	case context.Canceled:
		state = "canceled"
	case context.DeadlineExceeded:
		state = "deadline"
	default:
		state = "other"
	}
	budget := DbContextBudget{AtUnixNano: at.UnixNano(), State: state}
	if deadline, ok := ctx.Deadline(); ok {
		budget.HasDeadline = true
		budget.RemainingNs = deadline.Sub(at).Nanoseconds()
	}
	return budget
}

// These are concurrent observations of the exact acquired-from pool, not
// exclusive work by this caller. Successful-acquire counters exclude failures.
type DbAcquirePoolSnapshot struct {
	AtUnixNano   int64 `json:"at_unix_ns"`
	Acquired     int32 `json:"acquired"`
	Constructing int32 `json:"constructing"`
	Idle         int32 `json:"idle"`
	Total        int32 `json:"total"`
	Maximum      int32 `json:"maximum"`
	Acquires     int64 `json:"acquires"`
	Empty        int64 `json:"empty"`
	Canceled     int64 `json:"canceled"`
	Created      int64 `json:"created"`
}

// Uses only the supplied generation, without opening or consulting a wrapper.
func observeDbAcquirePool(pool *pgxpool.Pool) DbAcquirePoolSnapshot {
	at := time.Now()
	stats := pool.Stat()
	return DbAcquirePoolSnapshot{
		AtUnixNano: at.UnixNano(), Acquired: stats.AcquiredConns(),
		Constructing: stats.ConstructingConns(), Idle: stats.IdleConns(),
		Total: stats.TotalConns(), Maximum: stats.MaxConns(),
		Acquires: stats.AcquireCount(), Empty: stats.EmptyAcquireCount(),
		Canceled: stats.CanceledAcquireCount(), Created: stats.NewConnsCount(),
	}
}

// Only the last attempt is retained, with an explicit return/admission state.
// PoolReturn is present only on refusal. Acquire includes pgx validation and
// construction admission; it is never labeled as resource-queue time.
type DbAcquireObservation struct {
	Attempt        uint64                 `json:"attempt"`
	Returned       bool                   `json:"returned"`
	Admitted       bool                   `json:"admitted"`
	PoolOpenNs     int64                  `json:"pool_open_ns"`
	AcquireNs      int64                  `json:"acquire_ns"`
	BeforeOpen     DbContextBudget        `json:"before_open"`
	BeforeAcquire  DbContextBudget        `json:"before_acquire"`
	AtReturn       DbContextBudget        `json:"at_return"`
	PoolEntry      DbAcquirePoolSnapshot  `json:"pool_entry"`
	PoolReturn     *DbAcquirePoolSnapshot `json:"pool_return,omitempty"`
	openStarted    time.Time
	acquireStarted time.Time
}

// Detailed snapshots are independent of ordinary phase timing opt-in.
func (self *DbTiming) acquireObservation() *DbAcquireObservation {
	if self == nil {
		return nil
	}
	return self.Acquire
}

// Reuse keeps one attempt and cannot retain an earlier refusal after success.
func (self *DbAcquireObservation) begin(ctx context.Context) {
	if self != nil {
		*self = DbAcquireObservation{Attempt: self.Attempt + 1}
		self.BeforeOpen = ObserveDbContextBudget(ctx)
		self.openStarted = time.Now()
	}
}

// Ends initialization time before sampling the actual pool's entry state.
func (self *DbAcquireObservation) opened(ctx context.Context, pool *pgxpool.Pool) {
	if self != nil {
		self.PoolOpenNs = time.Since(self.openStarted).Nanoseconds()
		self.PoolEntry = observeDbAcquirePool(pool)
		self.BeforeAcquire = ObserveDbContextBudget(ctx)
	}
}

// Starts immediately before the pgx call, after entry observations.
func (self *DbAcquireObservation) startAcquire() {
	if self != nil {
		self.acquireStarted = time.Now()
	}
}

// Return time includes failures; successful calls need no return pool snapshot.
func (self *DbAcquireObservation) finish(ctx context.Context, pool *pgxpool.Pool, admitted bool) {
	if self != nil {
		self.AcquireNs = time.Since(self.acquireStarted).Nanoseconds()
		self.Returned, self.Admitted = true, admitted
		self.AtReturn = ObserveDbContextBudget(ctx)
		if !admitted {
			// Never reacquire safePgPool's reset mutex or sample its replacement.
			snapshot := observeDbAcquirePool(pool)
			self.PoolReturn = &snapshot
		}
	}
}
