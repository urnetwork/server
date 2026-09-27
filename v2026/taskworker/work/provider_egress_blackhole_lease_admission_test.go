// A task-local admission cutoff preserves started checks and finalization;
// it must not sleep before ready work or consume the serial full lane's lease.
package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type testBlackholeLeaseAdmissionCase struct {
	selected                   int
	serial                     bool
	fullCount                  int
	fast                       bool
	passing                    bool
	mixed                      bool
	shortDeadline              bool
	noDeadline                 bool
	insufficient               bool
	cancelSecondWave           bool
	publicationFails           bool
	readinessConsumesAdmission bool
}

type testBlackholeLeaseAdmissionObservation struct {
	result       *ProviderEgressProbeResult
	err          error
	lease        time.Duration
	checkBudget  time.Duration
	fullBudget   time.Duration
	elapsed      time.Duration
	pinsElapsed  time.Duration
	started      int
	completed    int
	canceled     int
	firstStart   time.Duration
	lastStart    time.Duration
	runCalls     int
	submitCalls  int
	submitted    []ingest.BlackholeCheck
	submissionAt time.Duration
	fullStarted  int
	fullStartAt  time.Duration
	contextErr   error
}

// Use the actual validator, pass owner, fleet worker admission and publication.
// Only each provider's network work and the control-plane sink are synthetic.
func testBlackholeLeaseAdmissionRun(t *testing.T, test testBlackholeLeaseAdmissionCase) testBlackholeLeaseAdmissionObservation {
	t.Helper()
	var observation testBlackholeLeaseAdmissionObservation
	synctest.Test(t, func(t *testing.T) {
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		selected := test.selected
		if selected == 0 {
			selected = 250
		}
		args.Blackhole.Limit = selected
		args.Blackhole.Concurrency = min(16, selected)
		fullCount := test.fullCount
		if test.serial {
			if fullCount == 0 {
				fullCount = 1
			}
			args.Blackhole.Concurrency = args.Full.Concurrency
			args.Full.Limit = max(args.Full.Limit, fullCount)
		}
		if err := validateProviderEgressProbeArgsConfig(args); err != nil {
			t.Fatalf("synthetic settings rejected by actual validator: %v", err)
		}
		checkOptions := egresshealth.Options{
			PerRequestTimeout:      time.Duration(args.Blackhole.ProbeTimeoutSeconds) * time.Second,
			IpEchoUrl:              egresshealth.IpEchoPath,
			IpEchoTimeout:          time.Duration(args.Blackhole.IpEchoTimeoutSeconds) * time.Second,
			LoadAttempts:           args.LoadAttempts,
			LoadRetryMeanInterval:  time.Duration(args.LoadRetryMeanIntervalSeconds) * time.Second,
			TunnelRecreateAttempts: args.TunnelRecreateAttempts,
		}
		observation.checkBudget = checkOptions.RunBudget(egresshealth.BlackholeSampleSize) +
			max(checkOptions.PerRequestTimeout, checkOptions.IpEchoTimeout)
		observation.fullBudget = providerEgressProbeMinMaxTime(args) - observation.checkBudget
		observation.lease = time.Duration(args.MaxTimeSeconds) * time.Second
		if test.shortDeadline {
			observation.lease = 50 * time.Minute
		}
		if observation.checkBudget != 32*time.Minute+45*time.Second || observation.fullBudget <= 0 {
			t.Fatal("the default duration model changed; recheck the test's wave arithmetic")
		}
		residence := observation.checkBudget
		fullResidence := observation.fullBudget * time.Duration((fullCount+args.Full.Concurrency-1)/args.Full.Concurrency)
		fullCohortResidence := observation.fullBudget
		if test.fast {
			residence = time.Second
			fullResidence = time.Second
			fullCohortResidence = time.Second
		}
		due := make([]ingest.DueProvider, selected)
		for index := range due {
			due[index] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-lease-admission-%03d", index)}
		}
		fullDue := make([]ingest.DueProvider, fullCount)
		for index := range fullDue {
			fullDue[index] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-full-admission-%03d", index)}
		}
		startTime := time.Now()
		var ctx context.Context
		var cancel context.CancelFunc
		if test.noDeadline {
			ctx, cancel = context.WithCancel(context.Background())
		} else {
			ctx, cancel = context.WithTimeout(context.Background(), observation.lease)
		}
		defer cancel()
		if test.cancelSecondWave {
			timer := time.AfterFunc(observation.checkBudget+time.Second, cancel)
			defer timer.Stop()
		}
		var active, started, completed, canceled atomic.Int32
		var firstStart, lastStart atomic.Int64
		firstStart.Store(-1)
		originalTimeout := 37 * time.Second
		pass := &providerEgressProbePass{
			blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) { return due, nil },
			fullDue:      func(context.Context, int) ([]ingest.DueProvider, error) { return fullDue, nil },
			loadPins: func(pinCtx context.Context) (map[string][]string, error) {
				if test.insufficient {
					// Leave only 15 seconds beyond the modeled work, less than
					// the existing 30-second publication reserve. This is time
					// already spent on setup, not an invalid args snapshot.
					remaining := observation.checkBudget + 15*time.Second
					if test.serial {
						remaining += fullResidence
					}
					delay := observation.lease - remaining
					if delay <= 0 {
						t.Fatal("insufficient-time fixture must consume positive setup time")
					}
					timer := time.NewTimer(delay)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-pinCtx.Done():
						t.Fatal("setup unexpectedly consumed the parent lease")
					}
				}
				observation.pinsElapsed = time.Since(startTime)
				return nil, nil
			},
			blackholeOptions: fleetprobe.BlackholeOptions{
				Timeout:     originalTimeout,
				Concurrency: 23,
				CheckOne: func(checkCtx context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
					started.Add(1)
					active.Add(1)
					defer active.Add(-1)
					at := int64(time.Since(startTime))
					firstStart.CompareAndSwap(-1, at)
					lastStart.Store(at)
					checkedAt := time.Now()
					if checkCtx != ctx {
						t.Error("admission cutoff replaced the active check's original context")
					}
					timer := time.NewTimer(residence)
					defer timer.Stop()
					select {
					case <-timer.C:
						completed.Add(1)
					case <-checkCtx.Done():
						canceled.Add(1)
					}
					result := testBlackholeAdmissionDark(provider)
					if test.passing {
						result = testBlackholeAdmissionPass(provider)
					}
					if test.mixed {
						switch provider.ClientId {
						case due[0].ClientId:
							result = testBlackholeAdmissionPass(provider)
						case due[1].ClientId:
							result = testBlackholeAdmissionTls(provider)
						}
					}
					result.Check.CheckedAt = checkedAt
					return result
				},
			},
			runBlackhole: func(runCtx context.Context, providers []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
				observation.runCalls++
				if options.Concurrency != args.Blackhole.Concurrency ||
					options.Timeout != time.Duration(args.Blackhole.ProbeTimeoutSeconds)*time.Second ||
					options.IpEchoTimeout != time.Duration(args.Blackhole.IpEchoTimeoutSeconds)*time.Second ||
					options.LoadAttempts != args.LoadAttempts ||
					options.LoadRetryMeanInterval != time.Duration(args.LoadRetryMeanIntervalSeconds)*time.Second ||
					options.TunnelRecreateAttempts != args.TunnelRecreateAttempts {
					t.Error("lease admission changed a per-provider retry/security budget or worker pool")
				}
				return fleetprobe.RunBlackhole(runCtx, providers, options)
			},
			runFull: func(fullCtx context.Context, providers []prober.Provider, _ fleetprobe.FullOptions) (prober.Summary, error) {
				observation.fullStarted++
				observation.fullStartAt = time.Since(startTime)
				timer := time.NewTimer(fullCohortResidence)
				defer timer.Stop()
				select {
				case <-timer.C:
					return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
				case <-fullCtx.Done():
					return prober.Summary{}, fullCtx.Err()
				}
			},
			submitBlackholeChecks: func(submitCtx context.Context, checks []ingest.BlackholeCheck) error {
				observation.submitCalls++
				observation.submissionAt = time.Since(startTime)
				if active.Load() != 0 {
					t.Error("publication preceded worker join")
				}
				deadline, ok := submitCtx.Deadline()
				if !ok || time.Until(deadline) != 30*time.Second || submitCtx.Err() != nil {
					t.Error("the existing detached 30-second publication budget changed")
				}
				observation.submitted = append(observation.submitted, checks...)
				if test.publicationFails {
					<-submitCtx.Done()
					return submitCtx.Err()
				}
				timer := time.NewTimer(time.Second)
				defer timer.Stop()
				select {
				case <-timer.C:
					return nil
				case <-submitCtx.Done():
					return submitCtx.Err()
				}
			},
		}
		if test.readinessConsumesAdmission {
			reads := 0
			pass.readiness = &providerEgressProbeReadiness{
				minimum: 1,
				available: func(readCtx context.Context) (model.ByteCount, error) {
					reads++
					if reads == 2 {
						// The batch's positive readiness lookup can consume
						// the cutoff after it was planned but before admission.
						timer := time.NewTimer(observation.lease - observation.checkBudget - 30*time.Second + time.Second)
						defer timer.Stop()
						select {
						case <-timer.C:
						case <-readCtx.Done():
							return 0, readCtx.Err()
						}
					}
					return 1, nil
				},
			}
		}
		observation.result, observation.err = pass.run(ctx, args)
		observation.elapsed = time.Since(startTime)
		observation.contextErr = ctx.Err()
		observation.started = int(started.Load())
		observation.completed = int(completed.Load())
		observation.canceled = int(canceled.Load())
		observation.firstStart = time.Duration(firstStart.Load())
		observation.lastStart = time.Duration(lastStart.Load())
		if active.Load() != 0 {
			t.Error("owning pass returned before every started check joined")
		}
		if pass.blackholeOptions.AdmissionDone != nil || pass.blackholeOptions.MinimumAdmission != 0 ||
			pass.blackholeOptions.Timeout != originalTimeout || pass.blackholeOptions.Concurrency != 23 {
			t.Error("task-local admission escaped into the shared option template")
		}
	})
	return observation
}

func testBlackholeLeaseAdmissionCompleted(t *testing.T, observation testBlackholeLeaseAdmissionObservation, want int) {
	t.Helper()
	if observation.err != nil || observation.contextErr != nil || observation.canceled != 0 ||
		observation.started != want || observation.completed != want || len(observation.submitted) != want ||
		observation.result == nil || observation.result.Checked != want ||
		observation.runCalls != 1 || observation.submitCalls != 1 || observation.elapsed >= observation.lease {
		t.Fatalf("lease admission did not finish and publish the admitted checks safely: %+v", observation)
	}
}

// 75m - 32m45s/check - 30s/publication = 41m45s admission cutoff.
// Wave two starts as soon as wave one ends at 32m45s, finishes at 65m30s;
// no third wave starts, and the original task remains live during publication.
func TestBlackholeLeaseAdmissionNoFullStopsBeforeDoomedWave(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{})
	testBlackholeLeaseAdmissionCompleted(t, observation, 32)
	if observation.elapsed != 2*observation.checkBudget+time.Second ||
		observation.lastStart != observation.checkBudget || observation.firstStart != 0 ||
		observation.result.BlackholeNotMeasured != 32 || observation.result.BlackholeGuardTripped != 1 {
		t.Fatalf("default no-full wave/guard arithmetic changed: %+v", observation)
	}
}

// Full work needs one modeled full run after the blackhole batch. Only the
// initial eight can fit; the existing truncated-sample guard must hold their
// ordinary negatives even though eight is less than its configured minimum.
func TestBlackholeLeaseAdmissionSerialReservesFullLane(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{serial: true})
	testBlackholeLeaseAdmissionCompleted(t, observation, 8)
	if observation.fullStarted != 1 || observation.fullStartAt != observation.checkBudget+time.Second ||
		observation.elapsed != observation.checkBudget+time.Second+observation.fullBudget ||
		observation.result.BlackholeNotMeasured != 8 || observation.result.BlackholeGuardTripped != 0 ||
		observation.result.Submitted != 1 {
		t.Fatalf("serial full reservation or short-sample safety changed: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionShortSamplePreservesPassAndTls(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{serial: true, mixed: true})
	testBlackholeLeaseAdmissionCompleted(t, observation, 8)
	if observation.result.Dark != 1 || observation.result.BlackholeNotMeasured != 6 ||
		!observation.submitted[0].Ok || observation.submitted[1].Failure != egresshealth.FailureTlsAuthentication ||
		observation.submitted[1].NotMeasured {
		t.Fatalf("admission shortened an authenticated/positive evidence contract: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionUsesActualParentDeadline(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{shortDeadline: true, passing: true})
	testBlackholeLeaseAdmissionCompleted(t, observation, 16)
	if observation.elapsed != observation.checkBudget+time.Second || observation.result.BlackholeDue != 250 {
		t.Fatalf("admission used the args' 75m rather than the actual 50m deadline: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionFastNoFullProgressesImmediately(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{fast: true, passing: true})
	testBlackholeLeaseAdmissionCompleted(t, observation, 250)
	if observation.firstStart != 0 || observation.lastStart != 15*time.Second || observation.elapsed != 17*time.Second {
		t.Fatalf("fast ready work was paced by a fixed wait instead of worker readiness: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionFastSerialProgressesImmediately(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{serial: true, fast: true, passing: true})
	testBlackholeLeaseAdmissionCompleted(t, observation, 250)
	if observation.firstStart != 0 || observation.lastStart != 31*time.Second ||
		observation.fullStartAt != 33*time.Second || observation.elapsed != 34*time.Second {
		t.Fatalf("healthy serial admission inserted idle time: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionCompleteSingleWaveKeepsGuard(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{selected: 16})
	testBlackholeLeaseAdmissionCompleted(t, observation, 16)
	if observation.elapsed != observation.checkBudget+time.Second || observation.result.BlackholeNotMeasured != 16 ||
		observation.result.BlackholeGuardTripped != 1 {
		t.Fatalf("complete single-wave ordinary guard changed: %+v", observation)
	}
}

func testBlackholeLeaseAdmissionBudgetError(t *testing.T, observation testBlackholeLeaseAdmissionObservation) {
	t.Helper()
	if observation.err == nil || !strings.Contains(observation.err.Error(), "insufficient remaining task budget for blackhole admission") ||
		errors.Is(observation.err, context.DeadlineExceeded) || observation.contextErr != nil ||
		observation.runCalls != 0 || observation.started != 0 || observation.submitCalls != 0 ||
		observation.result == nil || !observation.result.Full || observation.result.Checked != 0 {
		t.Fatalf("no-work budget refusal became success, cancellation, or a hidden hot-loop candidate: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionInsufficientTimeCannotReturnHealthyEmpty(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{insufficient: true, passing: true})
	testBlackholeLeaseAdmissionBudgetError(t, observation)
	if observation.elapsed != observation.pinsElapsed {
		t.Fatalf("budget refusal waited instead of returning promptly: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionBudgetErrorDoesNotSuppressFull(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{serial: true, insufficient: true, passing: true})
	testBlackholeLeaseAdmissionBudgetError(t, observation)
	if observation.fullStarted != 1 || observation.fullStartAt != observation.pinsElapsed ||
		observation.result.Submitted != 1 || observation.elapsed != observation.pinsElapsed+observation.fullBudget {
		t.Fatalf("blackhole admission refusal hid independent full work: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionReservesActualFullWaveCount(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{serial: true, fullCount: 9, passing: true})
	testBlackholeLeaseAdmissionBudgetError(t, observation)
	// Nine selected providers with eight full slots form two independent guard
	// cohorts. The serial geometry must reserve both complete run waves while
	// refusing blackhole admission that would overrun the task lease.
	if observation.fullStarted != 2 || observation.fullStartAt != observation.fullBudget ||
		observation.result.Submitted != 9 || observation.elapsed != 2*observation.fullBudget {
		t.Fatalf("serial reservation did not retain both full guard cohorts: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionParentCancellationStillFinalizesSafePrefix(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{passing: true, cancelSecondWave: true})
	if !errors.Is(observation.err, context.Canceled) || observation.started != 32 ||
		observation.completed != 16 || observation.canceled != 16 || len(observation.submitted) != 16 ||
		observation.result == nil || observation.result.Checked != 16 ||
		observation.elapsed != observation.checkBudget+2*time.Second {
		t.Fatalf("parent cancellation or safe-prefix finalization changed: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionPublicationErrorIsBoundedAndReturned(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{passing: true, publicationFails: true})
	if !errors.Is(observation.err, context.DeadlineExceeded) || observation.contextErr != nil ||
		!strings.Contains(observation.err.Error(), "submit blackhole batch") ||
		observation.started != 32 || observation.completed != 32 || observation.submitCalls != 1 ||
		observation.elapsed != 2*observation.checkBudget+30*time.Second || observation.elapsed >= observation.lease {
		t.Fatalf("publication timeout lost its independent bound/error authority: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionUndeadlinedCallerKeepsProgress(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{noDeadline: true, fast: true, passing: true})
	testBlackholeLeaseAdmissionCompleted(t, observation, 250)
	if observation.elapsed != 17*time.Second {
		t.Fatalf("undeadlined private caller inherited an invented global timer: %+v", observation)
	}
}

func TestBlackholeLeaseAdmissionCutoffDuringReadinessCannotReturnHealthyEmpty(t *testing.T) {
	observation := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{
		readinessConsumesAdmission: true, passing: true,
	})
	if observation.err == nil || !strings.Contains(observation.err.Error(), "insufficient remaining task budget for blackhole admission") ||
		observation.contextErr != nil || observation.started != 0 || observation.submitCalls != 0 ||
		observation.result == nil || observation.result.Checked != 0 ||
		observation.elapsed != observation.lease-observation.checkBudget-30*time.Second+time.Second {
		t.Fatalf("cutoff reached during readiness became a successful empty saturated pass: %+v", observation)
	}
}
