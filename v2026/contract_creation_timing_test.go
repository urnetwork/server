package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestContractCreationTimingPartitionsNestedStages(t *testing.T) {
	m := newContractCreationMetrics()
	now := time.Unix(100, 0)
	m.now = func() time.Time { return now }
	ctx, owner := beginContractCreationTiming(t.Context(), false, m)
	now = now.Add(time.Second)
	transaction := EnterContractCreationStage(ctx, ContractStageTransaction)
	now = now.Add(2 * time.Second)
	grant := EnterContractCreationStage(ctx, ContractStageGrantSelection)
	now = now.Add(3 * time.Second)
	snapshot := EnterContractCreationStage(ctx, ContractStageReservationSnapshot)
	now = now.Add(4 * time.Second)
	snapshot()
	now = now.Add(5 * time.Second)
	grant()
	now = now.Add(6 * time.Second)
	transaction()
	now = now.Add(7 * time.Second)
	owner.Finish(ContractCreationReply)
	owner.Finish(ContractCreationReply)
	transaction()
	EnterContractCreationStage(ctx, ContractStagePostCommit)()
	got := m.values
	if got.seconds[0][ContractStageOther] != 8 || got.seconds[0][ContractStageTransaction] != 8 || got.seconds[0][ContractStageGrantSelection] != 8 || got.seconds[0][ContractStageReservationSnapshot] != 4 {
		t.Fatalf("nested wall partition changed: %v", got.seconds[0])
	}
	if got.counts[0][ContractCreationReply] != 1 || got.inflight != [2][contractStageCount]int64{} {
		t.Fatal("completion duplicated or current occupancy leaked")
	}
}

func TestContractCreationTimingCancellationAndPanicRelease(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		m := newContractCreationMetrics()
		ctx, cancel := context.WithCancel(t.Context())
		ctx, owner := beginContractCreationTiming(ctx, true, m)
		func() {
			defer func() {
				if recover() != "same panic" {
					t.Error("panic changed")
				}
				owner.Finish(ContractCreationPanic)
			}()
			defer EnterContractCreationStage(ctx, ContractStagePayerGate)()
			if canceled {
				cancel()
			}
			panic("same panic")
		}()
		want := ContractCreationPanic
		if canceled {
			want = ContractCreationCanceled
		}
		if m.values.counts[1][want] != 1 || m.values.inflight != [2][contractStageCount]int64{} {
			t.Fatal("panic/cancellation occupancy or outcome changed")
		}
		cancel()
	}
}

func TestContractCreationTimingBoundedConcurrentCollection(t *testing.T) {
	m := newContractCreationMetrics()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(m)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	expect := map[string]int{
		"urnetwork_contract_creation_completed_stage_seconds_total":   38,
		"urnetwork_contract_creation_completed_total":                 10,
		"urnetwork_contract_creation_stage_inflight":                  38,
		"urnetwork_contract_creation_stage_timing_enabled":            1,
		"urnetwork_contract_creation_companion_origin_reads_total":    4,
		"urnetwork_contract_creation_companion_origin_outcomes_total": 8,
	}
	for _, f := range families {
		if len(f.Metric) != expect[f.GetName()] {
			t.Fatal("metric cardinality changed")
		}
	}
	if len(families) != len(expect) {
		t.Fatal("missing metric family")
	}
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			ctx, owner := beginContractCreationTiming(t.Context(), false, m)
			for range 5 {
				EnterContractCreationStage(ctx, ContractStagePayerGate)()
			}
			owner.Finish(ContractCreationRejected)
		})
	}
	for range 10 {
		if _, err := registry.Gather(); err != nil {
			t.Fatal(err)
		}
	}
	workers.Wait()
	if m.values.counts[0][ContractCreationRejected] != 64 || m.values.inflight != [2][contractStageCount]int64{} {
		t.Fatal("concurrent owner count/occupancy changed")
	}
	// Ordinary model callers must remain unobserved, rather than creating
	// partial controller calls or taking a global label from their context.
	EnterContractCreationStage(t.Context(), ContractStageTransaction)()
	EnterContractCreationStage(t.Context(), ContractCreationStage(255))()
	if m.values.counts[0][ContractCreationRejected] != 64 {
		t.Fatal("unowned call fabricated a controller completion")
	}
}

func TestContractCompanionOriginReadSpansAndAttempts(t *testing.T) {
	m := newContractCreationMetrics()
	now := time.Unix(100, 0)
	m.now = func() time.Time { return now }
	ctx, owner := beginContractCreationTiming(t.Context(), true, m)
	transaction := EnterContractCreationStage(ctx, ContractStageTransaction)
	plain := BeginContractCompanionOriginRead(ctx, ContractCompanionPlainOrigin)
	now = now.Add(2 * time.Second)
	plain()
	plain() // The deferred panic guard must not count time twice.
	fallback := BeginContractCompanionOriginRead(ctx, ContractCompanionFallbackOrigin)
	now = now.Add(3 * time.Second)
	fallback()
	RecordContractCompanionOriginOutcome(ctx, ContractCompanionOriginFound)
	transaction()
	owner.Finish(ContractCreationReply)
	if m.values.seconds[1][ContractStageCompanionPlainOriginRead] != 2 ||
		m.values.seconds[1][ContractStageCompanionFallbackOriginRead] != 3 ||
		m.values.companionReads[1] != [contractCompanionOriginPhaseCount]uint64{1, 1} ||
		m.values.companionOutcomes[1][ContractCompanionOriginFound] != 1 ||
		m.values.inflight != [2][contractStageCount]int64{} {
		t.Fatal("companion read span, attempt, or owner partition changed")
	}
}

func TestContractCompanionOriginReadPanicAndHandoff(t *testing.T) {
	m := newContractCreationMetrics()
	now := time.Unix(100, 0)
	m.now = func() time.Time { return now }
	ctx, owner := beginContractCreationTiming(t.Context(), false, m)
	func() {
		outcome := ContractCompanionOriginError
		defer func() { RecordContractCompanionOriginOutcome(ctx, outcome) }()
		defer func() {
			if recovered := recover(); recovered != "synthetic read failure" {
				t.Errorf("unexpected read panic: %v", recovered)
			}
		}()
		leave := BeginContractCompanionOriginRead(ctx, ContractCompanionPlainOrigin)
		defer leave()
		now = now.Add(time.Second)
		panic("synthetic read failure")
	}()
	RecordContractCompanionOriginOutcome(ctx, ContractCompanionOriginMissing)
	RecordContractCompanionOriginOutcome(ctx, ContractCompanionOriginPayerHandoff)
	owner.Finish(ContractCreationPanic)
	if m.values.seconds[0][ContractStageCompanionPlainOriginRead] != 1 ||
		m.values.companionReads[0][ContractCompanionPlainOrigin] != 1 ||
		m.values.companionOutcomes[0][ContractCompanionOriginError] != 1 ||
		m.values.companionOutcomes[0][ContractCompanionOriginMissing] != 1 ||
		m.values.companionOutcomes[0][ContractCompanionOriginPayerHandoff] != 1 ||
		m.values.inflight != [2][contractStageCount]int64{} {
		t.Fatal("panic closure or finite callback outcomes changed")
	}
}

func TestContractCompanionOriginTimingStopsWithOwner(t *testing.T) {
	m := newContractCreationMetrics()
	ctx, owner := beginContractCreationTiming(t.Context(), false, m)
	owner.Finish(ContractCreationReply)
	BeginContractCompanionOriginRead(ctx, ContractCompanionPlainOrigin)()
	RecordContractCompanionOriginOutcome(ctx, ContractCompanionOriginFound)
	BeginContractCompanionOriginRead(t.Context(), ContractCompanionFallbackOrigin)()
	RecordContractCompanionOriginOutcome(t.Context(), ContractCompanionOriginMissing)
	if m.values.companionReads != [2][contractCompanionOriginPhaseCount]uint64{} ||
		m.values.companionOutcomes != [2][contractCompanionOriginOutcomeCount]uint64{} ||
		m.values.inflight != [2][contractStageCount]int64{} {
		t.Fatal("finished or unowned companion callback changed metrics")
	}
}

func TestContractCreationTimingJoinedPostsKeepExclusiveOwner(t *testing.T) {
	type otherKey struct{}
	parent, cancel := context.WithTimeout(context.WithValue(t.Context(), otherKey{}, "retained"), 5*time.Second)
	defer cancel()
	m := newContractCreationMetrics()
	ctx, owner := beginContractCreationTiming(parent, false, m)
	defer owner.Finish(ContractCreationError)
	leavePosts := EnterContractCreationStage(ctx, ContractStagePostCommit)
	postCtx := WithoutContractCreationTiming(ctx)
	if postCtx.Value(otherKey{}) != "retained" || postCtx.Done() != ctx.Done() {
		t.Fatal("post context lost values or cancellation")
	}
	gotDeadline, gotOK := postCtx.Deadline()
	wantDeadline, wantOK := ctx.Deadline()
	if gotDeadline != wantDeadline || gotOK != wantOK || WithoutContractCreationTiming(postCtx) != postCtx || WithoutContractCreationTiming(parent) != parent {
		t.Fatal("post context changed deadline or unobserved context")
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	defer close(release)
	exited := make(chan error, 2)
	post := func(stage ContractCreationStage) PostFunction {
		return func() any {
			defer EnterContractCreationStage(postCtx, stage)()
			entered <- struct{}{}
			<-release
			exited <- postCtx.Err()
			return nil
		}
	}
	done := make(chan struct{})
	go func() {
		RunPosts(ctx, post(ContractStageReservationSnapshot), post(ContractStageClientFence))
		close(done)
	}()
	for range 2 {
		select {
		case <-entered:
		case <-parent.Done():
			t.Fatal("parallel posts did not start")
		}
	}
	m.mu.Lock()
	inflight := m.values.inflight
	m.mu.Unlock()
	var want [2][contractStageCount]int64
	want[0][ContractStagePostCommit] = 1
	if inflight != want {
		t.Fatal("parallel child stage overwrote the joined post owner")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("canceled owner failed to join posts")
	default:
	}
	// Release both callbacks without closing twice on the early-failure path.
	for range 2 {
		release <- struct{}{}
	}
	<-done
	for range 2 {
		if err := <-exited; err != context.Canceled {
			t.Fatal("post did not retain owning cancellation")
		}
	}
	leavePosts()
	owner.Finish(ContractCreationReply)
	if m.values.inflight != [2][contractStageCount]int64{} || m.values.counts[0][ContractCreationCanceled] != 1 || m.values.seconds[0][ContractStagePostCommit] <= 0 || m.values.seconds[0][ContractStageReservationSnapshot] != 0 || m.values.seconds[0][ContractStageClientFence] != 0 {
		t.Fatal("parallel posts corrupted the exclusive completion partition")
	}
}

func BenchmarkContractCreationTiming(b *testing.B) {
	m := newContractCreationMetrics()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		observed, owner := beginContractCreationTiming(ctx, false, m)
		for stage := ContractStageRelationship; stage < contractStageCount; stage++ {
			EnterContractCreationStage(observed, stage)()
		}
		owner.Finish(ContractCreationReply)
	}
}
