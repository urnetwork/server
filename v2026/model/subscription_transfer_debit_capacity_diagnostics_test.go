// Attribute a distinct-payer fixture failure without changing admission or drain work.
package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026"
)

// Only this capacity fixture requests the optional failure observation.
func createAsyncDebitCapacityTestContract(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, amount ByteCount, index, total int) *TransferEscrow {
	return runDebitFixtureCreation(ctx, f, amount, CreateTransferEscrow, func(err error) {
		reportDebitFixtureCreationFailure(t, ctx, index, total, err)
	})
}

// Creation panics bypass the observer entirely. A returned error retains its
// exact identity even when the diagnostic callback itself fails.
func runDebitFixtureCreation(ctx context.Context, f netEscrowOrderingTestFixture, amount ByteCount,
	create func(context.Context, server.Id, server.Id, server.Id, server.Id, ByteCount) (*TransferEscrow, error),
	observe func(error),
) *TransferEscrow {
	escrow, err := create(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, amount)
	if err != nil {
		func() {
			defer func() { _ = recover() }()
			observe(err)
		}()
	}
	server.Raise(err)
	return escrow
}

// This runs only after public creation returned an error. These four existing
// collectors read bounded in-memory state; unrelated global collectors may do I/O.
func reportDebitFixtureCreationFailure(t interface{ Logf(string, ...any) }, ctx context.Context, index, total int, originalErr error) {
	remaining := time.Duration(0)
	deadline, bounded := ctx.Deadline()
	if bounded {
		remaining = time.Until(deadline)
	}
	t.Logf("distinct-payer setup creation failed: index=%d target=%d completed_settlement_calls=%d caller_bounded=%t caller_remaining=%s caller_error=%v returned_error_type=%T returned_deadline=%t returned_canceled=%t",
		index, total, index, bounded, remaining, ctx.Err(), originalErr, errors.Is(originalErr, context.DeadlineExceeded), errors.Is(originalErr, context.Canceled))
	families, err := newDebitFixtureDiagnosticRegistry().Gather()
	if err != nil {
		t.Logf("distinct-payer setup diagnostic metric error: %v", err)
		return
	}
	values := map[string]float64{}
	for _, family := range families {
		name := family.GetName()
		switch name {
		case "urnetwork_redis_contract_reservation_total", "urnetwork_redis_contract_compensation_balances_held", "urnetwork_redis_contract_compensation_calls_inflight", "urnetwork_redis_contract_publication_refusals_total":
		default:
			continue
		}
		for _, metric := range family.Metric {
			value := 0.0
			if metric.Counter != nil {
				value = metric.GetCounter().GetValue()
				if value == 0 {
					continue
				}
			} else if metric.Gauge != nil {
				value = metric.GetGauge().GetValue()
			} else {
				continue
			}
			labels := []string{}
			for _, label := range metric.Label {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			slices.Sort(labels)
			values[fmt.Sprintf("%s%v", name, labels)] = value
		}
	}
	t.Logf("distinct-payer setup Redis counters (cumulative) and held gauges (current): %v", values)
}

// Only these four collectors are in-memory observations; none claims a service lease.
func newDebitFixtureDiagnosticRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(redisContractReservationResults, redisDeferredCompensationBalancesHeld, redisDeferredCompensationCallsInflight, redisPendingPublicationRefusals)
	return registry
}

// Success returns the same escrow without invoking any diagnostic work.
func TestDebitFixtureObservationLeavesSuccessUntouched(t *testing.T) {
	ctx := t.Context()
	f := netEscrowOrderingTestFixture{sourceNetworkId: server.NewId(), sourceId: server.NewId(), destinationNetworkId: server.NewId(), destinationId: server.NewId()}
	want := &TransferEscrow{ContractId: server.NewId()}
	created, observed := 0, 0
	got := runDebitFixtureCreation(ctx, f, 100, func(caller context.Context, sourceNetworkId, sourceId, destinationNetworkId, destinationId server.Id, amount ByteCount) (*TransferEscrow, error) {
		created++
		if caller != ctx || sourceNetworkId != f.sourceNetworkId || sourceId != f.sourceId || destinationNetworkId != f.destinationNetworkId || destinationId != f.destinationId || amount != 100 {
			t.Fatal("fixture changed its public creation arguments")
		}
		return want, nil
	}, func(error) { observed++ })
	if got != want || created != 1 || observed != 0 {
		t.Fatal("success changed its result, call count, or diagnostic work", got, created, observed)
	}
}

// The returned creation error wins over an observer panic, including cancellation.
func TestDebitFixtureObservationPreservesReturnedFailure(t *testing.T) {
	for _, originalErr := range []error{context.Canceled, context.DeadlineExceeded, errors.Join(context.DeadlineExceeded, errors.New("synthetic creation uncertainty"))} {
		created, observed := 0, 0
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			runDebitFixtureCreation(t.Context(), netEscrowOrderingTestFixture{}, 100, func(context.Context, server.Id, server.Id, server.Id, server.Id, ByteCount) (*TransferEscrow, error) {
				created++
				return nil, originalErr
			}, func(got error) {
				observed++
				if got != originalErr {
					t.Error("observer did not receive the original creation error")
				}
				panic(errors.New("synthetic diagnostic failure"))
			})
		}()
		if recovered != originalErr || created != 1 || observed != 1 {
			t.Fatal("diagnostic replaced the original failure or changed its calls", originalErr, recovered, created, observed)
		}
	}
}

// Recovery belongs only to observation; an original creation panic passes through.
func TestDebitFixtureObservationDoesNotCatchCreationPanic(t *testing.T) {
	original := &struct{ name string }{name: "synthetic creation panic"}
	created, observed := 0, 0
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		runDebitFixtureCreation(t.Context(), netEscrowOrderingTestFixture{}, 100, func(context.Context, server.Id, server.Id, server.Id, server.Id, ByteCount) (*TransferEscrow, error) {
			created++
			panic(original)
		}, func(error) { observed++ })
	}()
	if recovered != original || created != 1 || observed != 0 {
		t.Fatal("fixture swallowed a creation panic or observed it as a returned error", recovered, created, observed)
	}
}

// The same canceled child and deadline reach creation; its parent and sibling stay live.
func TestDebitFixtureObservationKeepsCallerAuthority(t *testing.T) {
	parent, stopParent := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
	defer stopParent()
	child, stopChild := context.WithCancel(parent)
	defer stopChild()
	sibling, stopSibling := context.WithCancel(parent)
	defer stopSibling()
	deadline, bounded := child.Deadline()
	stopChild()
	observed := 0
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		runDebitFixtureCreation(child, netEscrowOrderingTestFixture{}, 100, func(caller context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, _ ByteCount) (*TransferEscrow, error) {
			gotDeadline, gotBounded := caller.Deadline()
			if caller != child || caller.Err() != context.Canceled || gotBounded != bounded || !gotDeadline.Equal(deadline) {
				t.Error("fixture changed caller identity, cancellation, or deadline")
			}
			return nil, caller.Err()
		}, func(error) { observed++ })
	}()
	afterDeadline, afterBounded := child.Deadline()
	if recovered != context.Canceled || observed != 1 || child.Err() != context.Canceled || afterBounded != bounded || !afterDeadline.Equal(deadline) || parent.Err() != nil || sibling.Err() != nil {
		t.Fatal("failure observation changed an owner's lifetime", recovered, observed, child.Err(), parent.Err(), sibling.Err())
	}
}

// Poison the global gatherer only inside a single-test subprocess, leaving
// concurrent parent tests and their registered collectors untouched.
func TestDebitFixtureObservationIgnoresGlobalCollectors(t *testing.T) {
	if !slices.Contains(os.Args, "debit-fixture-observer-child") {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDebitFixtureObservationIgnoresGlobalCollectors$", "-test.count=1", "-test.parallel=1", "-test.timeout=30s", "debit-fixture-observer-child")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated observer control failed: %v\n%s", err, output)
		}
		return
	}
	var globalCalls atomic.Int64
	previous := prometheus.DefaultGatherer
	prometheus.DefaultGatherer = prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		globalCalls.Add(1)
		panic("unrelated global collector must not run")
	})
	defer func() { prometheus.DefaultGatherer = previous }()
	originalErr := errors.New("synthetic creation error")
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		runDebitFixtureCreation(t.Context(), netEscrowOrderingTestFixture{}, 100, func(context.Context, server.Id, server.Id, server.Id, server.Id, ByteCount) (*TransferEscrow, error) {
			return nil, originalErr
		}, func(err error) { reportDebitFixtureCreationFailure(t, t.Context(), 7, 512, err) })
	}()
	if recovered != originalErr || globalCalls.Load() != 0 {
		t.Fatal("observer touched global collectors or replaced the creation error", recovered, globalCalls.Load())
	}
	want := []string{"urnetwork_redis_contract_compensation_balances_held", "urnetwork_redis_contract_compensation_calls_inflight", "urnetwork_redis_contract_publication_refusals_total", "urnetwork_redis_contract_reservation_total"}
	// Empty CounterVecs have descriptors but emit no family yet. Inspect the
	// selected descriptors without inventing label tuples or changing counters.
	descriptions := make(chan *prometheus.Desc)
	described := make(chan struct{})
	go func() {
		defer close(described)
		defer close(descriptions)
		newDebitFixtureDiagnosticRegistry().Describe(descriptions)
	}()
	seen := map[string]bool{}
	count := 0
	for description := range descriptions {
		count++
		matched := false
		for _, name := range want {
			if strings.Contains(description.String(), fmt.Sprintf("fqName: %q", name)) {
				seen[name], matched = true, true
			}
		}
		if !matched {
			t.Errorf("observer selected an unrelated collector: %s", description)
		}
	}
	<-described
	if count != len(want) || len(seen) != len(want) {
		t.Fatal("observer collector boundary changed", count, seen)
	}
}

// This recorder belongs to one pure test; no callback or resource is retained.
type debitFixtureLogRecorder struct{ messages []string }

// Retain only synthetic diagnostic output for the failure-context assertion.
func (self *debitFixtureLogRecorder) Logf(format string, args ...any) {
	self.messages = append(self.messages, fmt.Sprintf(format, args...))
}

// A reported setup failure identifies its index and existing caller authority.
func TestDebitFixtureObservationReportsSetupContext(t *testing.T) {
	parent, stopParent := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
	defer stopParent()
	child, stopChild := context.WithCancel(parent)
	stopChild()
	logger := &debitFixtureLogRecorder{}
	reportDebitFixtureCreationFailure(logger, child, 7, 512, context.Canceled)
	output := strings.Join(logger.messages, "\n")
	for _, want := range []string{"index=7 target=512 completed_settlement_calls=7", "caller_bounded=true caller_remaining=", "caller_error=context canceled", "returned_deadline=false returned_canceled=true", "urnetwork_redis_contract_compensation_balances_held", "urnetwork_redis_contract_compensation_calls_inflight"} {
		if !strings.Contains(output, want) {
			t.Errorf("failure diagnostic omitted %q", want)
		}
	}
	if child.Err() != context.Canceled || parent.Err() != nil {
		t.Fatal("reporting changed caller or parent cancellation")
	}
}
