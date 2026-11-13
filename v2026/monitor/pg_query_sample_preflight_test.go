package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stale loaded generation stopped the real sampler before cadence admission,
// but its first-cadence alert discarded that reason and implied unknown source
// contact. Preserve the previous successful clocks without fabricating a turn.
func TestPgQuerySampleGenerationPreflightPreservesCoverageClocks(t *testing.T) {
	for _, test := range []struct {
		name, cause string
		current     bool
		loadErr     error
	}{
		{"stale", "stale", false, nil},
		{"unobservable", "unobservable", false, errors.New("private-loader-canary")},
		{"error_wins_over_current", "unobservable", true, errors.New("private-loader-canary")},
		{"deadline", "unobservable", false, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			contacts, checks, syncs := 0, 0, 0
			env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
				contacts++
				return "", "", nil
			})
			env.cfg.pgQuerySampleContinuous = true
			env.cfg.pgQuerySampleUntil = time.Time{}
			now := env.now()
			dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			terminal := now.Add(-time.Hour)
			prior := pgSampleCadenceState{Schema: 1, Mode: "continuous", AttemptedAt: terminal.Add(-25 * time.Second), TerminalAt: terminal, CompletedAt: terminal, NextEligibleAt: terminal.Add(pgQuerySampleCadence), Outcome: "complete", ReceiptSHA256: strings.Repeat("a", 64)}
			if err := pgSampleWriteCadence(dir, prior, nil); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "continuous.json"))
			if err != nil {
				t.Fatal(err)
			}
			env.cfg.routerGenerationCheck = func(context.Context) (bool, error) {
				checks++
				return test.current, test.loadErr
			}
			probe := pgQuerySampleProbe{syncAttemptFile: func(*os.File) error { syncs++; return nil }}
			findings, err := probe.check(context.Background(), env)
			if err == nil || len(findings) != 0 || contacts != 0 || checks != 1 || syncs != 0 {
				t.Fatal("generation failure admitted contact, fabricated observation, or reached durable admission")
			}
			after, readErr := os.ReadFile(filepath.Join(dir, "continuous.json"))
			entries, listErr := os.ReadDir(dir)
			if readErr != nil || listErr != nil || !bytes.Equal(before, after) || len(entries) != 1 {
				t.Fatal("generation failure changed cadence or created a receipt")
			}
			settings := syntheticSettings(nil)
			settings.StateDir, settings.Now, settings.PGQuerySampleContinuous = env.cfg.stateDir, env.now, true
			signal := NewPgQuerySampleSignal()
			alert := pgSampleFailureAlert(settings, signal, fmt.Errorf("private-wrapper-canary: %w", err))
			for _, want := range []string{"phase=settings-generation", "cause=" + test.cause, "source_contact_attempted=false", "last_attempted_at=" + prior.AttemptedAt.Format(time.RFC3339Nano), "last_completed_at=" + terminal.Format(time.RFC3339Nano), "next_eligible_at=" + prior.NextEligibleAt.Format(time.RFC3339Nano)} {
				if !strings.Contains(alert.Observed, want) {
					t.Fatalf("missing finite coverage field %q", want)
				}
			}
			if alert.Class != "pg-query-sample-unavailable" || alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Target != signal.ID() || len(newCadenceAlertGate().filter(signal, Alerts{alert})) != 1 {
				t.Fatal("local coverage failure lost stable first-cadence visibility")
			}
			requireAlertOmits(t, alert, "private-loader-canary", "private-wrapper-canary", env.cfg.pgPassword, env.cfg.stateDir)
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("raw local error escaped preflight")
			}
			if test.loadErr == context.DeadlineExceeded && (!errors.Is(err, context.DeadlineExceeded) || !strings.Contains(alert.Observed, "error_class=observation-timeout")) {
				t.Fatal("wrapped deadline lost finite timeout classification")
			}
		})
	}
}

func TestPgQuerySampleCurrentGenerationCompletesAndKeepsCadence(t *testing.T) {
	contacts := 0
	now := time.Now().UTC().Truncate(time.Second)
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		contacts++
		return pgSampleTestEncode(t, pgSampleTestFrames(now)), "", nil
	})
	env.now = func() time.Time { return now }
	env.cfg.pgQuerySampleContinuous = true
	env.cfg.pgQuerySampleUntil = time.Time{}
	current := false
	env.cfg.routerGenerationCheck = func(context.Context) (bool, error) { return current, nil }
	probe := pgQuerySampleProbe{}
	if _, err := probe.check(context.Background(), env); err == nil || contacts != 0 {
		t.Fatal("stale generation contacted")
	}
	current = true // A fresh process can load this generation without resetting state.
	findings, err := probe.check(context.Background(), env)
	if err != nil || contacts != 1 {
		t.Fatal("current generation did not admit its due turn")
	}
	for _, f := range findings {
		if f.class == "pg-query-sample-unavailable" {
			t.Fatal("complete current-generation sample misreported as unavailable")
		}
	}
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	state, err := pgSampleReadCadence(dir, now)
	if err != nil || state.Outcome != "complete" || !state.CompletedAt.Equal(now) || !state.NextEligibleAt.Equal(now.Add(pgQuerySampleCadence)) || len(state.ReceiptSHA256) != 64 {
		t.Fatal("successful turn lost completion or durable cadence")
	}
	if _, err := os.Stat(filepath.Join(dir, "receipt-"+state.ReceiptSHA256+".json")); err != nil {
		t.Fatal("completion lacks immutable receipt")
	}
	if _, err := probe.check(context.Background(), env); err != nil || contacts != 1 {
		t.Fatal("current generation bypassed durable floor")
	}
}

func TestPgQuerySampleLocalPrerequisitesAreFiniteAndPrecontact(t *testing.T) {
	for _, test := range []struct {
		name, phase, cause string
		configure          func(*testing.T, *probeEnv, *pgQuerySampleProbe)
	}{
		{"missing_primary", "inventory-preflight", "primary-unavailable", func(_ *testing.T, env *probeEnv, _ *pgQuerySampleProbe) { env.cfg.hosts = nil }},
		{"disabled_primary", "inventory-preflight", "primary-disabled", func(_ *testing.T, env *probeEnv, _ *pgQuerySampleProbe) {
			env.cfg.hostByRole("pg-primary").disabled = true
		}},
		{"directory", "local-state", "directory-unavailable", func(t *testing.T, env *probeEnv, _ *pgQuerySampleProbe) {
			if err := os.WriteFile(filepath.Join(env.cfg.stateDir, "pg-query-sample"), []byte("private-state-canary"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"cadence_lock", "cadence-admission", "lock-unavailable", func(t *testing.T, env *probeEnv, _ *pgQuerySampleProbe) {
			env.cfg.pgQuerySampleContinuous = true
			if err := os.WriteFile(filepath.Join(env.cfg.stateDir, "provider-reports"), []byte("private-state-canary"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"cadence_state", "cadence-admission", "state-unavailable", func(t *testing.T, env *probeEnv, _ *pgQuerySampleProbe) {
			env.cfg.pgQuerySampleContinuous = true
			dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "continuous.json"), []byte("private-state-canary"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"marker", "one-shot-admission", "marker-unavailable", func(_ *testing.T, _ *probeEnv, probe *pgQuerySampleProbe) {
			probe.syncAttemptFile = func(*os.File) error { return errors.New("private-sync-canary") }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			contacts := 0
			env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { contacts++; return "", "", nil })
			probe := pgQuerySampleProbe{}
			test.configure(t, env, &probe)
			_, err := probe.check(context.Background(), env)
			if err == nil || contacts != 0 {
				t.Fatal("failed local prerequisite admitted source contact")
			}
			alert := pgSampleFailureAlert(syntheticSettings(nil), NewPgQuerySampleSignal(), err)
			for _, want := range []string{"phase=" + test.phase, "cause=" + test.cause, "source_contact_attempted=false"} {
				if !strings.Contains(alert.Observed, want) {
					t.Fatalf("missing finite local prerequisite %q", want)
				}
			}
			requireAlertOmits(t, alert, "private-state-canary", "private-sync-canary", env.cfg.stateDir)
		})
	}
}

func TestPgQuerySamplePreflightCancellationAndUnknownContact(t *testing.T) {
	contacts := 0
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { contacts++; return "", "", nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (pgQuerySampleProbe{}).check(ctx, env)
	if !errors.Is(err, context.Canceled) || contacts != 0 {
		t.Fatal("canceled local preflight admitted contact or lost cancellation")
	}
	alert := pgSampleFailureAlert(syntheticSettings(nil), NewPgQuerySampleSignal(), err)
	if !strings.Contains(alert.Observed, "phase=local-preflight cause=context-ended") || !strings.Contains(alert.Observed, "source_contact_attempted=false") {
		t.Fatal("local cancellation phase lost")
	}
	for _, err := range []error{
		errors.New("bounded PG sample inventory changed: private-source-canary"),
		errors.New("monitor: bounded PG sample immutable receipt unavailable"),
		&pgSamplePreflightError{reason: 255, err: errors.New("private-source-canary")},
		(*pgSamplePreflightError)(nil),
	} {
		alert := pgSampleFailureAlert(syntheticSettings(nil), NewPgQuerySampleSignal(), err)
		if !strings.Contains(alert.Observed, "phase=source-or-adapter cause=unknown") || !strings.Contains(alert.Observed, "source_contact_attempted=unknown") {
			t.Fatal("untyped, invalid, or postcontact failure invented a precontact diagnosis")
		}
		requireAlertOmits(t, alert, "private-source-canary")
	}
}
