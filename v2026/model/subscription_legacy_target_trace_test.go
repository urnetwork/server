package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func legacyTraceTestPlan(id server.Id) legacyTargetTracePlan {
	capture := "0123456789abcdef0123456789abcdef"
	return legacyTargetTracePlan{Capture: capture, TargetDigest: legacyTargetTraceDigest(capture, id),
		Shard: int(id[15]) % LegacySettlementShardCount, Starts: time.Now().Add(-time.Minute), Expires: time.Now().Add(time.Minute), MaxPages: 2}
}

func legacyTraceTestContext(ctx context.Context, id server.Id, origin string, after *LegacySettlementCursor) (context.Context, *legacyTargetTrace) {
	trace := newLegacyTargetTrace(legacyTraceTestPlan(id), "fedcba9876543210fedcba9876543210", origin, after, make(chan legacyTargetTraceEnvelope, 64))
	return context.WithValue(ctx, legacyTargetTraceKey{}, trace), trace
}

func legacyTraceHas(trace *LegacySettlementTrace, stage, state string) bool {
	for _, event := range trace.Events {
		if event.Stage == stage && event.State == state {
			return true
		}
	}
	return false
}

func TestLegacyTargetTracePrivatePlanAndFiniteActivation(t *testing.T) {
	id := server.NewId()
	plan := legacyTraceTestPlan(id)
	data, err := json.Marshal(plan)
	if err != nil || parseLegacyTargetTracePlan(data) == nil {
		t.Fatal("valid private plan rejected")
	}
	for _, change := range []func(*legacyTargetTracePlan){
		func(p *legacyTargetTracePlan) { p.MaxPages = 257 },
		func(p *legacyTargetTracePlan) { p.Expires = p.Starts.Add(31 * time.Minute) },
		func(p *legacyTargetTracePlan) { p.Shard = 16 },
		func(p *legacyTargetTracePlan) { p.Capture = "not-a-private-capture" },
		func(p *legacyTargetTracePlan) { p.TargetDigest = "short" },
	} {
		bad := plan
		change(&bad)
		raw, _ := json.Marshal(bad)
		if parseLegacyTargetTracePlan(raw) != nil {
			t.Fatal("invalid trace authority accepted")
		}
	}
	if parseLegacyTargetTracePlan(append(data, []byte(` {}`)...)) != nil || parseLegacyTargetTracePlan(bytes.Repeat([]byte(" "), 1025)) != nil {
		t.Fatal("unbounded/trailing plan accepted")
	}
	path := filepath.Join(t.TempDir(), "private-plan.json")
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("private plan fixture unavailable")
	}
	t.Setenv("URN_LEGACY_SETTLEMENT_TRACE_FILE", path)
	runtime := &legacyTargetTraceRuntime{output: make(chan legacyTargetTraceEnvelope, 64)}
	if runtime.begin((plan.Shard+1)%16, nil, "automatic_page", nil) != nil {
		t.Fatal("wrong shard activated")
	}
	first := runtime.begin(plan.Shard, nil, "automatic_page", nil)
	if first == nil {
		t.Fatal("matching shard not activated")
	}
	if os.Remove(path) != nil {
		t.Fatal("could not remove read-once fixture")
	}
	second := runtime.begin(plan.Shard, nil, "automatic_page", nil)
	if second == nil || second.result.Run == first.result.Run || runtime.begin(plan.Shard, nil, "automatic_page", nil) != nil {
		t.Fatal("finite per-process invocation cap or unique opaque runs lost")
	}
}

func TestLegacyTargetTraceBlockedPublisherBoundsAndPrivacy(t *testing.T) {
	id := server.NewId()
	plan := legacyTraceTestPlan(id)
	// No consumer exists. Publication must return immediately at every event;
	// no timeout/sleep is used as a claim about nonblocking admission.
	trace := newLegacyTargetTrace(plan, "fedcba9876543210fedcba9876543210", "automatic_page", nil, make(chan legacyTargetTraceEnvelope))
	ctx := context.Background()
	if trace.selectTarget(ctx, server.NewId(), false) != ctx {
		t.Fatal("neighbor acquired a trace")
	}
	ctx = trace.selectTarget(ctx, id, false)
	for range 100 {
		traceLegacySettlement(ctx, "admission_release", "returned")
	}
	got := trace.finish(LegacySettlementFlushResult{}, nil)
	if len(got.Events) != legacyTargetTraceEventLimit || !got.Capped || got.Dropped != legacyTargetTraceEventLimit || got.Selected != 1 {
		t.Fatal("blocked publication was not explicitly bounded")
	}
	encoded, err := json.Marshal(LegacySettlementFlushResult{Trace: got, Timings: (&legacySettlementTimingObserver{}).snapshot()})
	if err != nil || len(encoded) > 8192 || bytes.Contains(encoded, []byte(id.String())) || bytes.Contains(encoded, []byte(plan.TargetDigest)) {
		t.Fatal("trace leaked identity or exceeded finite result envelope")
	}
	other := &LegacySettlementCursor{NextAttemptTime: time.Now(), ContractId: id}
	before := legacyTargetTraceCursor(plan.Capture, other)
	other.HeadAfter = &LegacySettlementPosition{NextAttemptTime: other.NextAttemptTime, ContractId: id}
	if legacyTargetTraceCursor(plan.Capture, other) == before {
		t.Fatal("cursor fingerprint omitted the head position")
	}
}

func TestLegacyTargetTracePreservesPanicAndUnavailableCause(t *testing.T) {
	id := server.NewId()
	ctx, trace := legacyTraceTestContext(t.Context(), id, "automatic_page", nil)
	ctx = trace.selectTarget(ctx, id, true)
	want := errors.New("synthetic-private-data-must-not-be-emitted")
	var got any
	func() {
		defer func() { got = recover() }()
		defer enterLegacyTargetTrace(ctx, "financial_body")()
		panic(want)
	}()
	if got != want || !legacyTraceHas(trace.snapshot(), "financial_body", "unavailable") {
		t.Fatal("observer changed panic identity or invented a cause")
	}
	encoded, _ := json.Marshal(trace.snapshot())
	if bytes.Contains(encoded, []byte(want.Error())) {
		t.Fatal("unclassified error text leaked")
	}
	called := false
	child := server.PostFunction(func() any { called = true; return nil })
	returned := observeLegacySettlementPost(ctx, legacySettlementClock, func() any { return child })()
	post, ok := returned.(server.PostFunction)
	if !ok || called {
		t.Fatal("trace changed the callback generation")
	}
	post()
	if !called {
		t.Fatal("trace lost the original callback")
	}
}

func TestLegacyTargetTraceEnteredBeforeBlockedPostAndConcurrentSnapshot(t *testing.T) {
	id := server.NewId()
	ctx, trace := legacyTraceTestContext(t.Context(), id, "automatic_page", nil)
	ctx = trace.selectTarget(ctx, id, true)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		defer close(done)
		observeLegacySettlementPost(ctx, legacySettlementMirror, func() any { close(entered); <-release; return nil })()
	}()
	<-entered
	got := trace.snapshot()
	if !legacyTraceHas(got, "mirror_post", "entered") || legacyTraceHas(got, "mirror_post", "returned") {
		t.Fatal("blocked stage falsely completed")
	}
	unblock()
	<-done
	if !legacyTraceHas(trace.snapshot(), "mirror_post", "returned") {
		t.Fatal("returned stage not recorded")
	}
}

func TestLegacyTargetTraceFullWidthResultEnvelope(t *testing.T) {
	id := server.NewId()
	cursor := &LegacySettlementCursor{NextAttemptTime: time.Date(2099, 12, 31, 23, 59, 59, 999999000, time.UTC), ContractId: id,
		PassEndTime: time.Date(2099, 12, 31, 23, 59, 59, 999999000, time.UTC),
		HeadAfter:   &LegacySettlementPosition{NextAttemptTime: time.Date(2099, 12, 31, 23, 59, 58, 999999000, time.UTC), ContractId: server.NewId()}}
	trace := newLegacyTargetTrace(legacyTraceTestPlan(id), "fedcba9876543210fedcba9876543210", "automatic_page", cursor, make(chan legacyTargetTraceEnvelope))
	trace.now = func() time.Time { return trace.started.Add(30 * time.Minute) }
	// Five database summaries carry count/duration fields; ordinary events
	// carry the full-width elapsed clock and a maximum page visit ordinal.
	for _, stage := range []string{"db_acquire", "db_begin", "db_commit_call", "db_rollback_call", "db_retry_wait"} {
		trace.record(256, stage, "observed", 256, 1_800_000_000)
	}
	for range legacyTargetTraceEventLimit {
		trace.record(256, "financial_body", "constraint_refused", 0)
	}
	phase := LegacySettlementPhaseDuration{Count: 256, ElapsedMs: 1_800_000, MaxMs: 1_800_000}
	result := LegacySettlementFlushResult{Cursor: cursor, Visited: 256, BusyOrGone: 256, More: true, HeadVisited: 64, HeadBusyOrGone: 64,
		BusyIntentUnavailable: 64, BusyContractUnavailable: 64, BusyGrantSetMismatch: 64, BusyAdmissionDeferred: 64,
		HeadBusyIntentUnavailable: 16, HeadBusyContractUnavailable: 16, HeadBusyGrantSetMismatch: 16, HeadBusyAdmissionDeferred: 16,
		HeadGrantWaitAttempted: 4, HeadGrantWaitTimedOut: 4,
		Timings: &LegacySettlementTimings{Selection: phase, Financial: phase, JoinedPosts: phase, Mirror: phase, ColdCensus: phase, Clock: phase, Stream: phase}}
	result.Trace = trace.finish(result, nil)
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 8192 {
		t.Fatalf("full finite result exceeded existing whole-result cap: bytes=%d", len(encoded))
	}
	var decoded LegacySettlementFlushResult
	if json.Unmarshal(encoded, &decoded) != nil || len(decoded.Trace.Events) != 48 || !decoded.Trace.Capped || decoded.Cursor.HeadAfter == nil {
		t.Fatal("full result lost trace/cursor fields")
	}
}

func TestLegacyTargetTraceConcurrentLoadingSkipsObservation(t *testing.T) {
	t.Setenv("URN_LEGACY_SETTLEMENT_TRACE_FILE", filepath.Join(t.TempDir(), "not-yet-loaded"))
	// The loading invocation has reserved the sole plan read, but has not
	// published it. Another invocation must neither wait nor reopen the file.
	runtime := &legacyTargetTraceRuntime{loaded: true}
	if runtime.begin(0, nil, "automatic_page", nil) != nil || runtime.pages != 0 || runtime.output != nil {
		t.Fatal("concurrent loading altered a page or allocated publication")
	}
}

// A payer turn spans original shards, but only the private selected target can
// emit events. Both cross-shard origins retain finite activation and lane names.
func TestLegacyTargetTracePayerTurnActivationAndSelection(t *testing.T) {
	// A preloaded runtime skips reading this synthetic path, while the
	// explicit nonempty setting still admits the production diagnostic.
	t.Setenv("URN_LEGACY_SETTLEMENT_TRACE_FILE", filepath.Join(t.TempDir(), "preloaded-private-plan.json"))
	id := server.NewId()
	plan := legacyTraceTestPlan(id)
	runtime := &legacyTargetTraceRuntime{loaded: true, plan: &plan, output: make(chan legacyTargetTraceEnvelope, 64)}
	if runtime.begin(-1, nil, "unrecognized_turn", nil) != nil {
		t.Fatal("unknown origin bypassed the original shard scope")
	}
	for _, origin := range []string{"payer_turn", "payer_page"} {
		trace := runtime.begin(-1, nil, origin, nil)
		if trace == nil || trace.result.Origin != origin {
			t.Fatal("declared payer producer could not activate", origin)
		}
		ctx := t.Context()
		if trace.selectTarget(ctx, server.NewId(), false) != ctx {
			t.Fatal("payer producer selected an unrelated private target", origin)
		}
		trace.selectTarget(ctx, id, false)
		trace.selectTarget(ctx, id, true)
		result := trace.finish(LegacySettlementFlushResult{Visited: 1024, Completed: 1024}, nil)
		if result.Selected != 2 || !legacyTraceHas(result, "selected", "forward") ||
			!legacyTraceHas(result, "selected", "head") || legacyTraceHas(result, "selected", "explicit_owner") || !result.PageReturned {
			t.Fatal("payer producer mislabeled its automatic visits", origin, result)
		}
	}
	if runtime.begin(-1, nil, "payer_turn", nil) != nil || runtime.pages != plan.MaxPages {
		t.Fatal("multi-page turn bypassed the finite invocation cap")
	}
}
