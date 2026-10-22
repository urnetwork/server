// Exercise the actual check, durable state, scoped adapter interruption and
// public failure projection with local fixtures; no transport executes.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The wrapper changes only the test transport and waits for its actual parent
// boundary; projection still runs through the actual scoped adapter.
type pgEvaluationControlProbe struct {
	pgQuerySampleProbe
	t                  *testing.T
	expireAfterCheck   bool
	failSource         bool
	expireDuringFinish bool
	terminalErr        error
	expectComplete     bool
	returnedProof      *pgSampleCompletedCollection
	transportCalls     int
}

func (self *pgEvaluationControlProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	self.t.Helper()
	scoped, ok := env.runner.(*hostScopeRunner)
	if !ok {
		self.t.Fatal("control precondition: scoped adapter is required")
	}
	native, ok := scoped.probeRunner.(*runner)
	if !ok {
		self.t.Fatal("control precondition: private native runner seam required")
	}
	native.runSSH = func(context.Context, []string, string) (string, string, error) {
		self.transportCalls++
		if self.failSource {
			return "", "", errors.New("synthetic-source-failure")
		}
		return pgSampleTestEncode(self.t, pgSampleTestFrames(env.now())), "", nil
	}
	if self.terminalErr != nil || self.expireDuringFinish {
		syncs := 0
		self.pgQuerySampleProbe.syncAttemptFile = func(file *os.File) error {
			syncs++
			if syncs == 4 && self.expireDuringFinish {
				awaitPgEvaluationDeadline(self.t, ctx)
			}
			if syncs > 3 && self.terminalErr != nil {
				return self.terminalErr
			}
			return file.Sync()
		}
	}
	findings, err := self.pgQuerySampleProbe.check(ctx, env)
	if self.transportCalls != 1 || (!self.expireDuringFinish && ctx.Err() != nil) || (self.expireDuringFinish && !errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		self.t.Fatal("control precondition: actual check/source and configured owner boundary not reached")
	}
	self.returnedProof = env.pgSampleCompletion
	if self.expectComplete {
		if err != nil || self.returnedProof == nil || self.returnedProof.Samples != 12 {
			self.t.Fatal("control precondition: actual collector did not complete")
		}
		path := filepath.Join(env.cfg.stateDir, "pg-query-sample", "receipt-"+self.returnedProof.ReceiptSha256+".json")
		raw, readErr := os.ReadFile(path)
		hash := sha256.Sum256(raw)
		if readErr != nil || hex.EncodeToString(hash[:]) != self.returnedProof.ReceiptSha256 {
			self.t.Fatal("control precondition: immutable proof receipt not durable")
		}
		state, stateErr := pgSampleReadCadence(filepath.Dir(path), env.now())
		if stateErr != nil || state.ReceiptSHA256 != self.returnedProof.ReceiptSha256 || state.Outcome != "complete" {
			self.t.Fatal("control precondition: complete terminal state not bound to receipt")
		}
	}
	if self.expireAfterCheck {
		awaitPgEvaluationDeadline(self.t, ctx)
	}
	return findings, err
}

// Enable the same scoped branch as Main while leaving the primary permitted.
func pgEvaluationControlSettings(t *testing.T) SignalSettings {
	t.Helper()
	settings := syntheticSettings(nil)
	settings.SSHUser = "fixture"
	settings.Hosts[0].OverlayAddress = "192.0.2.1"
	settings.Now = time.Now
	settings.StateDir = t.TempDir()
	settings.PGQuerySampleContinuous = true
	settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
	if settings.Validate() != nil {
		t.Fatal("control precondition: synthetic native settings must pass adapter validation")
	}
	cfg := configFromSignalSettings(settings.withDefaults())
	primary := cfg.hostByRole("pg-primary")
	if primary == nil || primary.disabled || primary.addr(cfg.addressMode) == "" {
		t.Fatal("control precondition: permitted synthetic primary must have a selected native route")
	}
	return settings
}

// Observe the actual inherited adapter boundary, not an assumed sleep length.
// The finite existing deadline owns the wait; no budget is extended or renewed.
func awaitPgEvaluationDeadline(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, known := ctx.Deadline()
	remaining := time.Until(deadline)
	if !known || ctx.Err() != nil || remaining <= 0 || remaining > pgQuerySampleBudget {
		t.Fatal("control precondition: live inherited adapter deadline within existing budget required")
	}
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("control precondition: actual adapter deadline did not expire")
	}
}

func pgEvaluationControlAdapter(probe *pgEvaluationControlProbe) *signalAdapter {
	return &signalAdapter{number: "2.1a", key: "pg-query-sample", name: "PostgreSQL query sample", probe: probe}
}

// The causal assertion occurs after a real durable complete check, then parent
// expiry; no canceled findings may be released and timeout must stay visible.
func TestPgEvaluationCompleteCollectorThenDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := pgEvaluationControlSettings(t)
		probe := &pgEvaluationControlProbe{t: t, expireAfterCheck: true, expectComplete: true}
		signal := pgEvaluationControlAdapter(probe)
		alerts, err := signal.Run(t.Context(), settings)
		if !errors.Is(err, context.DeadlineExceeded) || len(alerts) != 0 {
			t.Fatal("interrupted evaluator changed parent deadline or released findings")
		}
		failure := visibilityAlert(settings, signal, err)
		if !strings.Contains(failure.Observed, "phase=post-collection-evaluation") || !strings.Contains(failure.Observed, "collector_complete=true collector_samples=12") || !strings.Contains(failure.Observed, "collector_receipt_sha256="+probe.returnedProof.ReceiptSha256) || !strings.Contains(failure.Observed, "evaluator_complete=unknown") {
			t.Fatal("complete collector was misclassified as unknown source failure after evaluator deadline")
		}
		if !failure.unobservable || failure.Class != "pg-query-sample-unavailable" || !strings.Contains(failure.Observed, "error_class=observation-timeout") {
			t.Fatal("collection proof cleared the actual evaluator timeout")
		}
	})
}

// Successful live evaluation stays ordinary. Mutable complete cadence state
// alone is not same-call authority for an unrelated later timeout.
func TestPgEvaluationLiveAndStaleStateControl(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := pgEvaluationControlSettings(t)
		probe := &pgEvaluationControlProbe{t: t, expectComplete: true}
		signal := pgEvaluationControlAdapter(probe)
		_, err := signal.Run(t.Context(), settings)
		if err != nil {
			t.Fatal(err)
		}
		failure := visibilityAlert(settings, signal, context.DeadlineExceeded)
		if strings.Contains(failure.Observed, "collector_complete=true") || !strings.Contains(failure.Observed, "phase=source-or-adapter") {
			t.Fatal("stale mutable cadence state invented same-call completion authority")
		}
	})
}

// An incomplete source and a failed terminal publication cannot acquire the
// proof, even if a receipt exists. Their old timeout/source behavior remains.
func TestPgEvaluationIncompleteOrUndurableControl(t *testing.T) {
	for _, terminalFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete-source", true: "terminal-publication-failure"}[terminalFailure], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				settings := pgEvaluationControlSettings(t)
				probe := &pgEvaluationControlProbe{t: t, expireAfterCheck: true, failSource: !terminalFailure}
				if terminalFailure {
					probe.terminalErr = errors.New("synthetic-terminal-sync-failure")
				}
				signal := pgEvaluationControlAdapter(probe)
				alerts, err := signal.Run(t.Context(), settings)
				if !errors.Is(err, context.DeadlineExceeded) || len(alerts) != 0 || probe.returnedProof != nil {
					t.Fatal("incomplete or undurable collector acquired successful evaluation authority")
				}
				failure := visibilityAlert(settings, signal, err)
				if strings.Contains(failure.Observed, "collector_complete=true") || !strings.Contains(failure.Observed, "phase=source-or-adapter") {
					t.Fatal("incomplete or undurable collector was classified complete")
				}
			})
		})
	}
}

// An interruption wrapper must preserve both original causes and redact text.
func TestPgEvaluationInterruptedErrorPreservesCauses(t *testing.T) {
	for _, sourceErr := range []error{nil, errors.New("private-error-canary")} {
		err := &pgSampleEvaluationInterrupted{contextErr: context.Canceled, sourceErr: sourceErr}
		if !errors.Is(err, context.Canceled) || (sourceErr != nil && !errors.Is(err, sourceErr)) || strings.Contains(err.Error(), "private-error-canary") {
			t.Fatal("interruption lost cause or exposed private source text")
		}
		children := err.Unwrap()
		wantChildren := 1
		if sourceErr != nil {
			wantChildren = 2
		}
		if len(children) != wantChildren || children[0] != context.Canceled {
			t.Fatal("interruption did not retain only available causes")
		}
		for _, child := range children {
			if child == nil {
				t.Fatal("interruption returned an invalid nil multi-error child")
			}
		}
	}
}

// Successful publication may itself return after the parent deadline. It still
// proves collection, never publication within budget or completed evaluation.
func TestPgEvaluationDeadlineDuringTerminalPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := pgEvaluationControlSettings(t)
		probe := &pgEvaluationControlProbe{t: t, expireDuringFinish: true, expectComplete: true}
		signal := pgEvaluationControlAdapter(probe)
		alerts, err := signal.Run(t.Context(), settings)
		if !errors.Is(err, context.DeadlineExceeded) || len(alerts) != 0 || probe.returnedProof == nil {
			t.Fatal("late terminal publication cleared deadline or lost complete collection")
		}
		failure := visibilityAlert(settings, signal, err)
		if !strings.Contains(failure.Observed, "phase=post-collection-evaluation") || !strings.Contains(failure.Observed, "collector_complete=true") || !strings.Contains(failure.Mechanism, "Publication before deadline is not established") {
			t.Fatal("late collector publication invented within-budget completion or lost evaluator timeout")
		}
	})
}

// Prove setup separately: public validation requires a native SSH user, and
// actual transport preflight requires a selected address before runSSH. Both
// route cases retain the host-scope wrapper and only an in-memory callback.
func TestPgEvaluationNativeFixtureSetup(t *testing.T) {
	settings := pgEvaluationControlSettings(t)
	missingUser := settings
	missingUser.SSHUser = ""
	missingUser.SSHDevUser = ""
	if missingUser.Validate() == nil {
		t.Fatal("missing native fixture user did not fail adapter validation")
	}
	for _, withRoute := range []bool{false, true} {
		current := settings
		current.Hosts = cloneRouterScopeHosts(settings.Hosts)
		if !withRoute {
			current.Hosts[0].OverlayAddress = ""
		}
		if current.Validate() != nil {
			t.Fatal("route control must pass adapter settings validation")
		}
		env, err := newProbeEnv(current.withDefaults())
		if err != nil {
			t.Fatal("private route control environment unavailable")
		}
		scoped, ok := env.runner.(*hostScopeRunner)
		if !ok {
			t.Fatal("route control lost the actual host-scope wrapper")
		}
		native, ok := scoped.probeRunner.(*runner)
		if !ok {
			t.Fatal("route control lost the actual native preflight")
		}
		calls := 0
		native.runSSH = func(context.Context, []string, string) (string, string, error) { calls++; return "", "", nil }
		primary := env.cfg.hostByRole("pg-primary")
		if primary == nil || primary.disabled {
			t.Fatal("route control lost its permitted primary")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		_, err = env.runner.sshTimeout(ctx, primary, "true", "", time.Second)
		cancel()
		if !withRoute {
			var unavailable *unreachableError
			if !errors.As(err, &unavailable) || calls != 0 {
				t.Fatal("missing route did not stop actual native transport before callback")
			}
		} else if err != nil || calls != 1 {
			t.Fatal("valid native fixture route did not reach exactly one private callback")
		}
	}
}
