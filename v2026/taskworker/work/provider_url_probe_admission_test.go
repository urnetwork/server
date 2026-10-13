package work

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type urlAdmissionCompletionSink struct {
	*recordingEgressProbeIngest
	mu          sync.Mutex
	completions []qualityprobe.UrlProbeCompletion
	complete    func(context.Context, qualityprobe.UrlProbeCompletion) error
}

func (self *urlAdmissionCompletionSink) ReportUrlProbeCompletion(ctx context.Context, value qualityprobe.UrlProbeCompletion) error {
	if self.complete != nil {
		if err := self.complete(ctx, value); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	self.completions = append(self.completions, value)
	return nil
}

func urlAdmissionPass() (*providerEgressProbePass, *ProviderEgressProbeArgs, *urlAdmissionCompletionSink) {
	pass, args, inner := testUrlProbePass()
	args.UrlProbe.Limit, args.UrlProbe.Concurrency, args.UrlProbe.ProbeTimeoutSeconds = 2, 2, 60
	args.TunnelRecreateAttempts, args.MaxTimeSeconds, args.LoadAttempts = 2, 900, 1
	sink := &urlAdmissionCompletionSink{recordingEgressProbeIngest: inner}
	pass.fullSink = testFullBatchSink(sink)
	return pass, args, sink
}

// The real synchronous Due seam can return a decoded response after its
// context was canceled. Each issued identity still needs a terminal receipt.
func TestUrlProbeLateDueCompletesWithoutMeasurement(t *testing.T) {
	for _, mode := range []string{"reserve", "canceled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pass, args, sink := urlAdmissionPass()
				start := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
				defer cancel()
				reserve := providerUrlProbeRunBudget(args) + 3*providerEgressControlPlaneTimeout
				var opened atomic.Int32
				pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
					switch mode {
					case "reserve":
						time.Sleep(900*time.Second - reserve + time.Millisecond)
					case "canceled":
						time.Sleep(time.Second)
						cancel()
					case "expired":
						time.Sleep(901 * time.Second)
					}
					return []ingest.DueProvider{{ClientId: "synthetic-a", ClaimOrdinal: 31, ClaimedAt: start}, {ClientId: "synthetic-b", ClaimOrdinal: 47, ClaimedAt: start}}, nil
				}
				pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
					opened.Add(1)
					return prober.Summary{Attempted: 1, Failed: 1}, nil
				}
				before := [3]float64{testutil.ToFloat64(urlProbeTurns.WithLabelValues("attempted")), testutil.ToFloat64(urlProbeTurns.WithLabelValues("accepted")), testutil.ToFloat64(urlProbeTurns.WithLabelValues("local_failure"))}
				result, err := pass.run(ctx, args)
				if opened.Load() != 0 {
					t.Errorf("fresh provider turn started after the admission boundary: %d", opened.Load())
				}
				if result == nil || result.UrlDue != 2 || result.Attempted != 0 || result.Submitted != 0 || result.Failed != 0 || result.UrlNotMeasured != 0 || !result.Backlog {
					t.Errorf("unstarted claims fabricated URL outcomes or were discarded: %+v", result)
				}
				after := [3]float64{testutil.ToFloat64(urlProbeTurns.WithLabelValues("attempted")), testutil.ToFloat64(urlProbeTurns.WithLabelValues("accepted")), testutil.ToFloat64(urlProbeTurns.WithLabelValues("local_failure"))}
				if before != after {
					t.Errorf("unstarted claims entered measured-turn counters: %v -> %v", before, after)
				}
				if mode == "expired" {
					if !errors.Is(err, context.DeadlineExceeded) || len(sink.completions) != 0 {
						t.Errorf("expired publication deadline was extended or hidden: %v %+v", err, sink.completions)
					}
				} else {
					if mode == "reserve" && err != nil || mode == "canceled" && !errors.Is(err, context.Canceled) {
						t.Errorf("pass cause changed: %v", err)
					}
					if len(sink.completions) != 2 {
						t.Errorf("issued claims lost terminal receipts: %d", len(sink.completions))
					}
					seen := map[string]int64{}
					for _, completion := range sink.completions {
						seen[completion.ClientId] = completion.ClaimOrdinal
						if completion.ProbeFailure != prober.FailureHealthNotRun || completion.AllowPacing || completion.CompletedAt.Before(start) {
							t.Errorf("receipt acquired provider verdict or lost time: %+v", completion)
						}
					}
					if seen["synthetic-a"] != 31 || seen["synthetic-b"] != 47 {
						t.Errorf("receipt identity changed: %v", seen)
					}
				}
				if len(sink.health) != 0 || len(sink.calls) != 0 {
					t.Error("unstarted claim used health or legacy attempt ingest")
				}
			})
		})
	}
}

func TestUrlProbeDueKeepsOwnerBudgetWithoutCutoffError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, _ := urlAdmissionPass()
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		end, _ := ctx.Deadline()
		pass.fullDue = func(claimCtx context.Context, _ int) ([]ingest.DueProvider, error) {
			got, ok := claimCtx.Deadline()
			want := end
			if !ok || !got.Equal(want) {
				t.Errorf("Due parent owner was replaced: got %v want %v", got, want)
			}
			time.Sleep(591 * time.Second)
			return nil, nil
		}
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			t.Error("opened after failed Due")
			return prober.Summary{}, nil
		}
		result, err := pass.run(ctx, args)
		if err != nil || result.UrlDue != 0 || ctx.Err() != nil {
			t.Errorf("normal cutoff became task error or changed owner: result=%+v err=%v owner=%v", result, err, ctx.Err())
		}
	})
}

func TestUrlProbeUnstartedCompletionJoinsAndReportsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "failed"}[fail], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pass, args, sink := urlAdmissionPass()
				ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
				defer cancel()
				end, _ := ctx.Deadline()
				release := make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(release) })
				entered := make(chan struct{}, 2)
				wantErr := errors.New("synthetic completion acknowledgment failure")
				sink.complete = func(completionCtx context.Context, _ qualityprobe.UrlProbeCompletion) error {
					if deadline, ok := completionCtx.Deadline(); !ok || deadline.After(end) {
						t.Error("cleanup extended original owner deadline")
					}
					entered <- struct{}{}
					<-release
					if fail {
						return wantErr
					}
					return nil
				}
				pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
					time.Sleep(591 * time.Second)
					return []ingest.DueProvider{{ClientId: "synthetic-a", ClaimOrdinal: 1}, {ClientId: "synthetic-b", ClaimOrdinal: 2}}, nil
				}
				var opened atomic.Int32
				pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
					opened.Add(1)
					return prober.Summary{}, nil
				}
				done := make(chan error, 1)
				go func() { _, err := pass.run(ctx, args); done <- err }()
				time.Sleep(592 * time.Second)
				synctest.Wait()
				if opened.Load() != 0 {
					t.Errorf("opened %d late turns", opened.Load())
				}
				select {
				case err := <-done:
					t.Fatalf("pass returned before completion owners joined: %v", err)
				default:
				}
				if len(entered) != 2 {
					t.Errorf("unstarted completions not bounded parallel owners: %d", len(entered))
				}
				once.Do(func() { close(release) })
				err := <-done
				if fail && !errors.Is(err, wantErr) || !fail && err != nil {
					t.Errorf("completion error authority changed: %v", err)
				}
			})
		})
	}
}

func TestUrlProbeHealthyClaimStillMeasuresAndPublishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		var calls int
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			calls++
			if calls > 1 {
				return nil, nil
			}
			return []ingest.DueProvider{{ClientId: "synthetic-healthy", ClaimOrdinal: 7}}, nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			p := providers[0]
			time.Sleep(time.Second)
			if err := options.HealthResults.SubmitEgressHealth(ctx, p.ClientId, testHealthRun(1, 0)); err != nil {
				return prober.Summary{}, err
			}
			err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: p.ClientId, ClaimOrdinal: p.ClaimOrdinal, CompletedAt: time.Now(), AllowPacing: true})
			return prober.Summary{Attempted: 1, Submitted: 1}, err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		result, err := pass.run(ctx, args)
		if err != nil || result.Submitted != 1 || result.Attempted != 1 || len(sink.completions) != 1 || !sink.completions[0].AllowPacing || sink.health["synthetic-healthy"] == nil || sink.health["synthetic-healthy"].OkCount != 0 {
			t.Fatalf("measured negative or its identity changed: %+v %v", result, err)
		}
	})
}

// No endpoint is contacted. Existing policy still owns a complete 60-second
// request, even though every individual network phase is capped at 15 seconds.
func TestUrlProbeAdmissionKeepsRequestAndReservePolicy(t *testing.T) {
	_, args, _ := urlAdmissionPass()
	if budget := providerUrlProbeRunBudget(args); budget != 220*time.Second || budget+3*providerEgressControlPlaneTimeout != 310*time.Second {
		t.Fatalf("reserve policy changed: %s", budget)
	}
	reserve := providerUrlProbeRunBudget(args) + 3*providerEgressControlPlaneTimeout
	if model.ProviderUrlProbeRenewalHeadroom-reserve != 50*time.Second {
		t.Fatal("default turn and publication no longer fit the renewal scheduling margin")
	}
	opts := fleetprobe.EgressHealthOptions(time.Duration(args.UrlProbe.ProbeTimeoutSeconds)*time.Second, false)
	if opts.ColdStartTimeout != 60*time.Second || opts.PerRequestTimeout != egresshealth.DefaultPerRequestTimeout {
		t.Fatalf("network owner policy changed: %+v", opts)
	}
}

func TestUrlProbeUnstartedCompletionPanicKeepsSiblingOwned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, sink := urlAdmissionPass()
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		want := errors.New("synthetic datastore failure")
		sink.complete = func(_ context.Context, value qualityprobe.UrlProbeCompletion) error {
			if value.ClientId == "synthetic-panic" {
				panic(want)
			}
			time.Sleep(time.Second)
			return nil
		}
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			time.Sleep(591 * time.Second)
			return []ingest.DueProvider{{ClientId: "synthetic-panic", ClaimOrdinal: 11}, {ClientId: "synthetic-sibling", ClaimOrdinal: 12}}, nil
		}
		var opened atomic.Int32
		pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			opened.Add(1)
			return prober.Summary{}, nil
		}
		result, err := pass.run(ctx, args)
		if !errors.Is(err, want) || opened.Load() != 0 || result.UrlDue != 2 || result.Attempted != 0 || len(sink.completions) != 1 || sink.completions[0].ClaimOrdinal != 12 {
			t.Fatalf("cleanup panic lost error/sibling ownership: %+v %v %+v", result, err, sink.completions)
		}
	})
}
