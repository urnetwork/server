// Open-set cancellation controls exercise the public source and the next
// accepted decision; no database, process, timer or remote resource is used.
package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A source-success barrier snapshots Err before cancellation and delays its
// return. This represents cancellation immediately after that successful check;
// the serialized state owner must read context again before committing.
type openSetCancelSnapshotContext struct {
	context.Context
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (self *openSetCancelSnapshotContext) Err() error {
	err := self.Context.Err()
	if self.armed.CompareAndSwap(true, false) {
		close(self.entered)
		<-self.release
	}
	return err
}

// Compare both count state and the subsequent public classification. Canceled
// exact, low and capped rows have no authority to update or reset either field.
func TestOpenContractsEndedSourceCannotCommitTrend(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, test := range []struct {
			name  string
			seed  int
			ended int
			next  int
			want  string
		}{
			{name: "high must not hide rise", seed: 160000, ended: 200000, next: 180000, want: "rising (previous 160000)"},
			{name: "low must not create rise", seed: 160000, ended: 100000, next: 150001},
			{name: "cap must not reset exact pair", seed: 160000, ended: pgOpenSetScanCap, next: 180000, want: "rising (previous 160000)"},
			{name: "first exact cannot consume warmup", ended: 200000, next: 180000, want: "trend is warming up"},
		} {
			t.Run(test.name+"/scoped="+strconv.FormatBool(scoped), func(t *testing.T) {
				phase, reads := 0, 0
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if !strings.Contains(query, "WHERE open = true") || !strings.Contains(query, "LIMIT 250001") {
						t.Fatal("control did not reach bounded open-set source")
					}
					reads++
					count := test.seed
					if phase == 1 {
						count = test.ended
						cancel()
					}
					if phase == 2 {
						count = test.next
					}
					return []Row{{strconv.Itoa(count), "1000", "0"}}, nil
				}}
				settings := syntheticSettings(source)
				if scoped {
					settings.ExcludedHosts = []string{"redis-1"}
				}
				signal := NewOpenContractsSignal()
				probe := signal.(*signalAdapter).probe.(*pgOpenSetProbe)
				if test.seed != 0 {
					if _, err := signal.Run(context.Background(), settings); err != nil {
						t.Fatal("accepted seed failed")
					}
				}
				beforeInitialized, beforeCount := probe.initialized, probe.lastCount
				phase = 1
				alerts, err := signal.Run(ctx, settings)
				if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
					t.Fatal("ended source published an observation")
				}
				if probe.initialized != beforeInitialized || probe.lastCount != beforeCount {
					t.Errorf("ended observation changed last exact open-set count: initialized=%t count=%d", probe.initialized, probe.lastCount)
				}
				phase = 2
				alerts, err = signal.Run(context.Background(), settings)
				if err != nil {
					t.Fatal("live continuation failed")
				}
				if test.want == "" {
					if len(alerts) != 0 {
						t.Error("canceled low count manufactured a rise")
					}
				} else if len(alerts) != 1 || !strings.Contains(alerts[0].Symptom, test.want) {
					t.Errorf("live continuation lost accepted trend: alerts=%d want=%s", len(alerts), test.want)
				}
				if !probe.initialized || probe.lastCount != test.next {
					t.Error("live continuation did not commit exactly once")
				}
				wantReads := 2
				if test.seed != 0 {
					wantReads++
				}
				if reads != wantReads {
					t.Error("control changed source-call count")
				}
			})
		}
	}
}

// Cancellation after the post-source context snapshot still cannot update or
// reset state: the owning lock has its own acceptance check.
func TestOpenContractsCancellationBeforeSerializedCommit(t *testing.T) {
	for _, ended := range []int{200000, pgOpenSetScanCap} {
		t.Run(strconv.Itoa(ended), func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &openSetCancelSnapshotContext{Context: parent, entered: make(chan struct{}), release: make(chan struct{})}
			signal := NewOpenContractsSignal()
			probe := signal.(*signalAdapter).probe.(*pgOpenSetProbe)
			probe.initialized, probe.lastCount = true, 160000
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				ctx.armed.Store(true)
				return []Row{{strconv.Itoa(ended), "1000", "0"}}, nil
			}}
			type result struct {
				alerts Alerts
				err    error
			}
			done := make(chan result, 1)
			go func() {
				alerts, err := signal.Run(ctx, syntheticSettings(source))
				done <- result{alerts: alerts, err: err}
			}()
			<-ctx.entered
			cancel()
			close(ctx.release)
			observed := <-done
			if !errors.Is(observed.err, context.Canceled) || len(observed.alerts) != 0 {
				t.Fatal("serialized cancellation published a verdict")
			}
			if !probe.initialized || probe.lastCount != 160000 {
				t.Error("serialized cancellation changed last exact open-set count")
			}
		})
	}
}

// Entry ownership applies to scoped and compatibility adapters. A pre-ended
// parent must make no count-source call and preserve established trend state.
func TestOpenContractsPreCanceledParentDoesNotReadOrCommit(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(strconv.FormatBool(scoped), func(t *testing.T) {
			reads := 0
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { reads++; return []Row{{"200000", "1000", "0"}}, nil }}
			settings := syntheticSettings(source)
			if scoped {
				settings.ExcludedHosts = []string{"redis-1"}
			}
			signal := NewOpenContractsSignal()
			probe := signal.(*signalAdapter).probe.(*pgOpenSetProbe)
			probe.initialized, probe.lastCount = true, 160000
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			alerts, err := signal.Run(ctx, settings)
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 || reads != 0 || !probe.initialized || probe.lastCount != 160000 {
				t.Fatal("pre-ended parent read or changed open-set state")
			}
		})
	}
}
