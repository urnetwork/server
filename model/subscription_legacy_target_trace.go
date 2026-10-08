package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

const legacyTargetTraceEventLimit = 48

// This diagnostic is disabled unless an operator supplies one private, finite
// plan. It observes natural visits; it cannot select or schedule a contract.
// The file is loaded once at page entry, outside all financial/Redis ownership.
// A process generation accepts at most one plan and 256 matching-shard pages.
// Restarting a process does not extend the plan's absolute 30-minute window.
type legacyTargetTracePlan struct {
	Capture      string    `json:"capture"`
	TargetDigest string    `json:"target_digest"`
	Shard        int       `json:"shard"`
	Starts       time.Time `json:"starts"`
	Expires      time.Time `json:"expires"`
	MaxPages     int       `json:"max_pages"`
}

type LegacySettlementTraceEvent struct {
	Sequence   int    `json:"n"`
	Attempt    int    `json:"a,omitempty"`
	Stage      string `json:"stage"`
	State      string `json:"state"`
	ElapsedUs  int64  `json:"us"`
	Count      uint64 `json:"count,omitempty"`
	DurationUs int64  `json:"duration_us,omitempty"`
}

// Private task results retain a bounded copy even when the log consumer is
// blocked. Fingerprints bind full saved cursors without exposing any row key.
// A returned callback is not a claim that its external projection succeeded.
type LegacySettlementTrace struct {
	Capture      string                       `json:"capture"`
	Run          string                       `json:"run"`
	StartedAt    string                       `json:"started_at"`
	Origin       string                       `json:"origin"`
	InputCursor  string                       `json:"input_cursor"`
	NextCursor   string                       `json:"next_cursor,omitempty"`
	Selected     int                          `json:"selected"`
	PageReturned bool                         `json:"page_returned"`
	PageCause    string                       `json:"page_cause"`
	Events       []LegacySettlementTraceEvent `json:"events"`
	Dropped      int                          `json:"dropped"`
	Capped       bool                         `json:"capped"`
}

type legacyTargetTraceEnvelope struct {
	Kind        string                     `json:"kind"`
	Capture     string                     `json:"capture"`
	Run         string                     `json:"run"`
	StartedAt   string                     `json:"started_at"`
	Origin      string                     `json:"origin"`
	InputCursor string                     `json:"input_cursor,omitempty"`
	NextCursor  string                     `json:"next_cursor,omitempty"`
	Dropped     int                        `json:"dropped"`
	Capped      bool                       `json:"capped"`
	Event       LegacySettlementTraceEvent `json:"event"`
}

type legacyTargetTraceRuntime struct {
	stateLock sync.Mutex
	loaded    bool
	plan      *legacyTargetTracePlan
	pages     int
	output    chan legacyTargetTraceEnvelope
}

var legacyTargetTraceRuntimeState legacyTargetTraceRuntime

type legacyTargetTraceKey struct{}
type legacyTargetTraceAttemptKey struct{}
type legacyTargetTraceAttempt struct {
	trace   *legacyTargetTrace
	ordinal int
}
type legacyTargetTrace struct {
	stateLock sync.Mutex
	plan      legacyTargetTracePlan
	started   time.Time
	now       func() time.Time
	output    chan<- legacyTargetTraceEnvelope
	result    LegacySettlementTrace
}

func parseLegacyTargetTracePlan(data []byte) *legacyTargetTracePlan {
	if len(data) == 0 || len(data) > 1024 {
		return nil
	}
	var plan legacyTargetTracePlan
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&plan) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil
	}
	capture, e1 := hex.DecodeString(plan.Capture)
	target, e2 := hex.DecodeString(plan.TargetDigest)
	if e1 != nil || e2 != nil || len(capture) != 16 || len(target) != 32 ||
		hex.EncodeToString(capture) != plan.Capture || hex.EncodeToString(target) != plan.TargetDigest ||
		plan.Shard < 0 || plan.Shard >= LegacySettlementShardCount || plan.MaxPages < 1 || plan.MaxPages > 256 ||
		plan.Starts.IsZero() || !plan.Expires.After(plan.Starts) || plan.Expires.Sub(plan.Starts) > 30*time.Minute {
		return nil
	}
	return &plan
}

func legacyTargetTraceDigest(capture string, id server.Id) string {
	digest := sha256.Sum256(append([]byte(capture+"\x00"), id[:]...))
	return hex.EncodeToString(digest[:])
}

func legacyTargetTraceCursor(capture string, cursor *LegacySettlementCursor) string {
	data, _ := json.Marshal(cursor)
	digest := sha256.Sum256(append([]byte(capture+"\x00cursor\x00"), data...))
	return hex.EncodeToString(digest[:])
}

// No new goroutine, dependency call, or config parsing is introduced on the
// default-disabled path. Invalid/unreadable private configuration disables the
// observer rather than changing settlement control flow or logging its value.
func (self *legacyTargetTraceRuntime) begin(shard int, after *LegacySettlementCursor, origin string, explicit *server.Id) *legacyTargetTrace {
	path := os.Getenv("URN_LEGACY_SETTLEMENT_TRACE_FILE")
	if path == "" {
		return nil
	}
	load := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.loaded {
			return false
		}
		self.loaded = true
		return true
	}()
	if load {
		// Concurrent callers see a nil plan and skip this observation. No
		// financial owner waits for another invocation's file or entropy I/O.
		plan := func() *legacyTargetTracePlan {
			entry, err := os.Lstat(path)
			if err != nil || !entry.Mode().IsRegular() {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1024 {
				return nil
			}
			data, err := io.ReadAll(io.LimitReader(file, 1025))
			if err != nil {
				return nil
			}
			return parseLegacyTargetTracePlan(data)
		}()
		func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.plan = plan }()
	}
	plan := func() *legacyTargetTracePlan { self.stateLock.Lock(); defer self.stateLock.Unlock(); return self.plan }()
	now := time.Now()
	if plan == nil || shard != plan.Shard && !(shard == -1 && (origin == "payer_page" || origin == "payer_turn")) || now.Before(plan.Starts) || !now.Before(plan.Expires) {
		return nil
	}
	if explicit != nil && legacyTargetTraceDigest(plan.Capture, *explicit) != plan.TargetDigest {
		return nil
	}
	var run [16]byte
	if _, err := rand.Read(run[:]); err != nil {
		return nil
	}
	var startPublisher bool
	output := func() chan legacyTargetTraceEnvelope {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.pages >= plan.MaxPages {
			return nil
		}
		self.pages++
		if self.output == nil {
			self.output = make(chan legacyTargetTraceEnvelope, 64)
			startPublisher = true
		}
		return self.output
	}()
	if output == nil {
		return nil
	}
	if startPublisher {
		go publishLegacyTargetTrace(output, plan.Expires)
	}
	return newLegacyTargetTrace(*plan, hex.EncodeToString(run[:]), origin, after, output)
}

func publishLegacyTargetTrace(input <-chan legacyTargetTraceEnvelope, expires time.Time) {
	defer func() { _ = recover() }() // A failed log sink cannot terminate the worker.
	// Only this diagnostic goroutine performs serialization/log I/O. A blocked
	// sink consumes one goroutine and a bounded queue; financial owners drop.
	// The channel is never closed while a late joined callback can publish.
	logger := log.New(os.Stderr, "", 0) // Do not hold the application logger mutex.
	timer := time.NewTimer(max(time.Duration(0), time.Until(expires.Add(time.Minute))))
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return
		case event := <-input:
			encoded, err := json.Marshal(event)
			if err == nil {
				logger.Printf("legacy_target_trace %s", encoded)
			}
		}
	}
}

func newLegacyTargetTrace(plan legacyTargetTracePlan, run, origin string, after *LegacySettlementCursor, output chan<- legacyTargetTraceEnvelope) *legacyTargetTrace {
	started := time.Now()
	trace := &legacyTargetTrace{plan: plan, started: started, now: time.Now, output: output,
		result: LegacySettlementTrace{Capture: plan.Capture, Run: run, Origin: origin, StartedAt: started.UTC().Format(time.RFC3339Nano), InputCursor: legacyTargetTraceCursor(plan.Capture, after), PageCause: "unavailable", Events: make([]LegacySettlementTraceEvent, 0, legacyTargetTraceEventLimit)}}
	trace.record(0, "page", "entered", 0)
	return trace
}

func (self *legacyTargetTrace) record(attempt int, stage, state string, count uint64, durationUs ...int64) {
	if self == nil {
		return
	}
	elapsedUs := max(int64(0), self.now().Sub(self.started).Microseconds())
	envelope, ok := func() (legacyTargetTraceEnvelope, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.result.Events) >= legacyTargetTraceEventLimit {
			self.result.Capped = true
			return legacyTargetTraceEnvelope{}, false
		}
		event := LegacySettlementTraceEvent{Sequence: len(self.result.Events) + 1, Attempt: attempt, Stage: stage, State: state, ElapsedUs: elapsedUs, Count: count}
		if len(durationUs) > 0 {
			event.DurationUs = durationUs[0]
		}
		self.result.Events = append(self.result.Events, event)
		envelope := legacyTargetTraceEnvelope{Kind: "legacy_target_trace_v1", Capture: self.result.Capture, Run: self.result.Run, Origin: self.result.Origin, StartedAt: self.result.StartedAt, Dropped: self.result.Dropped, Capped: self.result.Capped, Event: event}
		if stage == "page" {
			envelope.InputCursor = self.result.InputCursor
			envelope.NextCursor = self.result.NextCursor
		}
		return envelope, true
	}()
	if !ok {
		return
	}
	select {
	case self.output <- envelope:
	default:
		func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.result.Dropped++ }()
	}
}

func (self *legacyTargetTrace) selectTarget(ctx context.Context, id server.Id, head bool) context.Context {
	if self == nil || legacyTargetTraceDigest(self.plan.Capture, id) != self.plan.TargetDigest {
		return ctx
	}
	self.stateLock.Lock()
	self.result.Selected++
	ordinal := self.result.Selected
	self.stateLock.Unlock()
	lane := "forward"
	if head {
		lane = "head"
	}
	if self.result.Origin != "automatic_page" && self.result.Origin != "payer_page" && self.result.Origin != "payer_turn" {
		lane = "explicit_owner"
	}
	self.record(ordinal, "selected", lane, 0)
	return context.WithValue(ctx, legacyTargetTraceAttemptKey{}, &legacyTargetTraceAttempt{trace: self, ordinal: ordinal})
}

func legacyTargetTraceOf(ctx context.Context) *legacyTargetTraceAttempt {
	attempt, _ := ctx.Value(legacyTargetTraceAttemptKey{}).(*legacyTargetTraceAttempt)
	return attempt
}

func traceLegacySettlement(ctx context.Context, stage, state string) {
	if attempt := legacyTargetTraceOf(ctx); attempt != nil {
		attempt.trace.record(attempt.ordinal, stage, state, 0)
	}
}

// A completed stage means the function returned; swallowed dependency failures
// remain cause-unavailable. Panic identity and control flow are preserved.
func enterLegacyTargetTrace(ctx context.Context, stage string) func() {
	if legacyTargetTraceOf(ctx) == nil {
		return func() {}
	}
	traceLegacySettlement(ctx, stage, "entered")
	return func() {
		if value := recover(); value != nil {
			cause := "unavailable"
			if err, ok := value.(error); ok {
				cause = legacyTargetTraceCause(err)
			}
			traceLegacySettlement(ctx, stage, cause)
			panic(value)
		}
		traceLegacySettlement(ctx, stage, "returned")
	}
}

func legacyTargetTraceCause(err error) string {
	if err == nil {
		return "none"
	}
	var refusal legacySettlementDrainRefusal
	if errors.As(err, &refusal) {
		switch refusal {
		case "busy_intent_or_absent", "intent_absent", "busy_contract_or_absent", "contract_absent", "payer_mismatch", "terminal", "disputed", "held", "not_due", "intent_policy_changed", "reports_changed", "debit_present", "reservation_mode_changed":
			return string(refusal)
		}
	}
	if errors.Is(err, errContractInsufficientEscrow) {
		return "insufficient_escrow"
	}
	if errors.Is(err, errLegacySettlementGrantWaitBusy) {
		return "grant_wait_refused"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55P03":
			return "lock_unavailable"
		case "57014":
			return "query_canceled"
		case "40001":
			return "serialization"
		case "40P01":
			return "deadlock"
		case "23514":
			return "constraint_refused"
		}
	}
	return "unavailable"
}

func traceLegacySettlementResult(ctx context.Context, completed, busy bool, gate legacySettlementBusyGate, err error) {
	state := "unavailable"
	if err != nil {
		state = legacyTargetTraceCause(err)
	} else if completed {
		state = "completed"
	} else if busy {
		switch gate {
		case legacySettlementBusyIntent:
			state = "intent_unavailable"
		case legacySettlementBusyContract:
			state = "contract_unavailable"
		case legacySettlementBusyGrantSet:
			state = "grant_set_mismatch"
		case legacySettlementBusyAdmission:
			state = "admission_deferred"
		}
	}
	traceLegacySettlement(ctx, "attempt_result", state)
}

func legacyTargetTraceDatabase(ctx context.Context) (*server.DbTiming, func()) {
	attempt := legacyTargetTraceOf(ctx)
	if attempt == nil {
		return nil, func() {}
	}
	timing := &server.DbTiming{}
	traceLegacySettlement(ctx, "transaction", "entered")
	return timing, func() {
		// These are completed client calls (including errors), not server time
		// or proof of rollback/commit acknowledgement. The confirmed-commit
		// marker below has separate transaction-owned authority.
		for phase, name := range []string{"db_acquire", "db_begin", "db_commit_call", "db_rollback_call", "db_retry_wait"} {
			sample := timing.Phases[phase]
			if sample.Count > 0 {
				attempt.trace.record(attempt.ordinal, name, "observed", sample.Count, sample.Duration.Microseconds())
			}
		}
		traceLegacySettlement(ctx, "transaction", "unwound")
	}
}

func (self *legacyTargetTrace) finish(result LegacySettlementFlushResult, err error) *LegacySettlementTrace {
	if self == nil {
		return nil
	}
	cause := legacyTargetTraceCause(err)
	cursor := legacyTargetTraceCursor(self.plan.Capture, result.Cursor)
	self.stateLock.Lock()
	self.result.PageReturned = true
	self.result.PageCause = cause
	self.result.NextCursor = cursor
	self.stateLock.Unlock()
	self.record(0, "page", "returned", 0)
	return self.snapshot()
}

func (self *legacyTargetTrace) snapshot() *LegacySettlementTrace {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result := self.result
	result.Events = append([]LegacySettlementTraceEvent(nil), result.Events...)
	return &result
}

func legacyTargetTracePostStage(phase legacySettlementTimingPhase) string {
	switch phase {
	case legacySettlementMirror:
		return "mirror_post"
	case legacySettlementClock:
		return "clock_post"
	case legacySettlementStream:
		return "stream_post"
	case legacySettlementColdCensus:
		return "cold_census"
	}
	return ""
}
