// Completion cancellation is lifecycle even when source validation fails.
// All source frames and clock transitions are synthetic and synchronous.
package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type pgFreezeAgeCompletionSource struct {
	*syntheticSource
	read func(context.Context, string) ([]Row, error)
}

func (self *pgFreezeAgeCompletionSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	return self.read(ctx, query)
}

// Exclude only an unrelated inventory member, so the actual scope adapter is
// enabled while the unique primary and the complete relation census stay live.
func pgFreezeAgeCompletionSettings(t *testing.T, source SignalSource, scoped bool) SignalSettings {
	t.Helper()
	settings := syntheticSettings(source)
	if scoped {
		var err error
		settings, err = ExcludeHosts(settings, "redis-1")
		if err != nil {
			t.Fatal("could not enable synthetic freeze-age scope")
		}
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal("could not construct synthetic freeze-age environment")
	}
	_, wrapped := env.runner.(*hostScopeRunner)
	if wrapped != scoped {
		t.Fatal("freeze-age fixture did not select the intended public adapter")
	}
	return settings
}

func pgFreezeAgeCompletionRows(now time.Time, frame string) []Row {
	rows := pgFreezeAgeTestRows(now, pgFreezeAgeTestPass())
	switch frame {
	case "malformed":
		rows[1] = append(rows[1], pgFreezeAgePrivateCanary)
	case "stale":
		rows[0][2] = fmt.Sprint(now.Add(-31 * time.Second).UnixMicro())
	case "over-cap":
		rows[0][8] = "4097"
	case "quiet":
		rows[1][2] = "200000000"
	}
	return rows
}

func pgFreezeAgeCompletionPublic(t *testing.T, alerts Alerts, err error) {
	t.Helper()
	var jsonl bytes.Buffer
	if renderErr := WriteAlertsJSONL(&jsonl, alerts); renderErr != nil {
		t.Fatal("could not render synthetic freeze-age alerts")
	}
	public := jsonl.String() + AlertsMarkdown(alerts)
	if err != nil {
		public += err.Error()
	}
	if strings.Contains(public, pgFreezeAgePrivateCanary) {
		t.Fatal("private freeze-age frame entered public completion output")
	}
}

// The source callback observes the actual live child context. Only the first
// completion clock call cancels the parent, after the successful source read.
func TestPgFreezeAgeCompletionCancellationPublicMatrix(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, frame := range []string{"malformed", "stale", "over-cap", "risk", "quiet"} {
			for _, cancelAtCompletion := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				now := syntheticSettings(nil).Now()
				calls, completionCalls := 0, 0
				sourceReturned := false
				source := &pgFreezeAgeCompletionSource{syntheticSource: &syntheticSource{}, read: func(sourceCtx context.Context, query string) ([]Row, error) {
					calls++
					if calls != 1 || sourceCtx.Err() != nil || ctx.Err() != nil || query != pgFreezeAgeQuery {
						t.Fatal("freeze-age completion fixture did not read once with a live parent")
					}
					sourceReturned = true
					return pgFreezeAgeCompletionRows(now, frame), nil
				}}
				settings := pgFreezeAgeCompletionSettings(t, source, scoped)
				settings.Now = func() time.Time {
					if sourceReturned && completionCalls == 0 {
						completionCalls++
						if cancelAtCompletion {
							cancel()
						}
					}
					return now
				}
				signal := NewPgFreezeAgeSignal()
				alerts, err := signal.Run(ctx, settings)
				pgFreezeAgeCompletionPublic(t, alerts, err)
				if calls != 1 || completionCalls != 1 {
					t.Fatal("freeze-age completion hook missed the successful source boundary")
				}
				if cancelAtCompletion {
					if len(alerts) != 0 || !errors.Is(err, context.Canceled) || err != ctx.Err() {
						t.Fatalf("freeze-age completion cancellation published a finding or lost parent cause: frame=%s scoped=%t", frame, scoped)
					}
					alerts, err = signal.Run(ctx, settings)
					pgFreezeAgeCompletionPublic(t, alerts, err)
					if len(alerts) != 0 || !errors.Is(err, context.Canceled) || calls != 1 {
						t.Fatal("ended freeze-age parent admitted another source read or finding")
					}
					cancel()
					continue
				}
				if err != nil || ctx.Err() != nil {
					t.Fatal("live freeze-age completion became a lifecycle failure")
				}
				cancel()
				if frame == "quiet" {
					if len(alerts) != 0 {
						t.Fatal("complete in-band freeze age lost its healthy control")
					}
					continue
				}
				class := "pg-freeze-age-unobservable"
				if frame == "risk" {
					class = "pg-freeze-age-risk"
				}
				if len(alerts) != 1 {
					t.Fatal("live freeze-age completion changed its finding cardinality")
				}
				alert := pgFreezeAgeTestClass(t, alerts, class)
				if alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.unobservable != (frame != "risk") {
					t.Fatal("live freeze-age completion changed its risk or visibility policy")
				}
				if frame == "risk" && alert.Target != "pg-1/database-1234/relation-10" {
					t.Fatal("live freeze-age risk lost its relation identity")
				}
				wantReason := map[string]string{"malformed": "invalid-frame-width", "stale": "source-clock-unavailable", "over-cap": "invalid-or-incomplete-census"}[frame]
				if wantReason != "" && alert.Observed != "reason="+wantReason {
					t.Fatal("live invalid freeze-age frame lost its fixed visibility reason")
				}
			}
		}
	}
}

// The next sample arrives only one second after the canceled fifteen-minute
// tick, within the prior sample's comparison bound. Missing reset therefore
// cannot hide behind the separate long-gap rejection. Do not rerun the ended
// parent here: its admission guard would independently reset the comparison.
func TestPgFreezeAgeCompletionCancellationBreaksProgress(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, frame := range []string{"malformed", "stale", "over-cap", "risk", "quiet"} {
			now := syntheticSettings(nil).Now()
			row := pgFreezeAgeTestPass()
			rows := pgFreezeAgeTestRows(now, row)
			calls, completionCalls := 0, 0
			var completionCancel context.CancelFunc
			source := &pgFreezeAgeCompletionSource{syntheticSource: &syntheticSource{}, read: func(sourceCtx context.Context, query string) ([]Row, error) {
				if sourceCtx.Err() != nil || query != pgFreezeAgeQuery {
					t.Fatal("freeze-age continuity fixture used an ended source context")
				}
				calls++
				return rows, nil
			}}
			settings := pgFreezeAgeCompletionSettings(t, source, scoped)
			settings.Now = func() time.Time {
				if completionCancel != nil && calls == 2 {
					completionCalls++
					completionCancel()
					completionCancel = nil
				}
				return now
			}
			signal := NewPgFreezeAgeSignal()
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 {
				t.Fatal("initial freeze-age progress sample failed")
			}
			pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk")
			now = now.Add(15 * time.Minute)
			rows = pgFreezeAgeCompletionRows(now, frame)
			ctx, cancel := context.WithCancel(context.Background())
			completionCancel = cancel
			alerts, err = signal.Run(ctx, settings)
			cancel()
			pgFreezeAgeCompletionPublic(t, alerts, err)
			if len(alerts) != 0 || !errors.Is(err, context.Canceled) || completionCalls != 1 || calls != 2 {
				t.Fatal("canceled freeze-age progress sample published a completion result")
			}
			now = now.Add(time.Second)
			row[13] = "2"
			rows = pgFreezeAgeTestRows(now, row)
			alerts, err = signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || calls != 3 {
				t.Fatalf("freeze-age progress bridged a completion cancellation: frame=%s scoped=%t", frame, scoped)
			}
			pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk")
			now = now.Add(15 * time.Minute)
			row[13] = "3"
			rows = pgFreezeAgeTestRows(now, row)
			alerts, err = signal.Run(context.Background(), settings)
			pgFreezeAgeCompletionPublic(t, alerts, err)
			if err != nil || len(alerts) != 0 || calls != 4 {
				t.Fatal("fresh same-pass positive work could not recover after cancellation")
			}
		}
	}
}

// The canceled sample cannot erase the lifetime risk ledger or count as a
// complete recovery. Exercise the real sustain gate and notifier state without
// posting messages; two later affirmative below-limit samples still recover.
func TestPgFreezeAgeCompletionCancellationRetainsRiskLedger(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		now := syntheticSettings(nil).Now()
		var rows []Row
		calls, completionCalls := 0, 0
		var completionCancel context.CancelFunc
		source := &pgFreezeAgeCompletionSource{syntheticSource: &syntheticSource{}, read: func(sourceCtx context.Context, query string) ([]Row, error) {
			if sourceCtx.Err() != nil || query != pgFreezeAgeQuery {
				t.Fatal("freeze-age ledger fixture used an ended source context")
			}
			calls++
			return rows, nil
		}}
		settings := pgFreezeAgeCompletionSettings(t, source, scoped)
		settings.Now = func() time.Time {
			if completionCancel != nil && calls == 3 {
				completionCalls++
				completionCancel()
				completionCancel = nil
			}
			return now
		}
		signal := NewPgFreezeAgeSignal()
		gate := newCadenceAlertGate()
		notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
		raised, cleared := 0, 0
		for tick := 0; tick < 7; tick++ {
			rows = pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(10, 200000001, 200000000))
			ctx := context.Background()
			var cancel context.CancelFunc
			if tick == 2 {
				rows = pgFreezeAgeCompletionRows(now, "malformed")
				ctx, cancel = context.WithCancel(ctx)
				completionCancel = cancel
			} else if tick == 3 || tick == 4 {
				rows = pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(11, 1, 200000000))
			} else if tick >= 5 {
				rows = pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(10, 200000000, 200000000))
			}
			alerts, err := signal.Run(ctx, settings)
			if cancel != nil {
				cancel()
			}
			pgFreezeAgeCompletionPublic(t, alerts, err)
			if calls != tick+1 {
				t.Fatal("freeze-age ledger fixture changed its source call count")
			}
			if tick == 2 {
				if len(alerts) != 0 || !errors.Is(err, context.Canceled) || completionCalls != 1 {
					t.Fatal("canceled freeze-age ledger sample became a complete result")
				}
			} else if err != nil {
				t.Fatal("live freeze-age ledger sample failed")
			} else if tick < 2 {
				pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk")
			} else if tick == 3 || tick == 4 {
				if len(alerts) != 1 {
					t.Fatal("completion cancellation erased the previous freeze-age risk target")
				}
				alert := pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable")
				if !alert.unobservable || alert.Observed != "reason=previous-risk-target-unobserved" {
					t.Fatal("missing postcancel freeze-age risk acquired recovery authority")
				}
			} else if len(alerts) != 0 {
				t.Fatal("complete below-limit freeze age failed recovery")
			}
			ready, held := gate.split(signal, alerts)
			for _, transition := range notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held, Failed: err != nil}) {
				if transition.alert.Class != "pg-freeze-age-risk" {
					continue
				}
				if transition.kind == slackRaised {
					raised++
					if tick != 1 {
						t.Fatal("freeze-age completion changed sustain two")
					}
				}
				if transition.kind == slackCleared {
					cleared++
					if tick != 6 {
						t.Fatal("cancellation or missing source cleared the freeze-age incident")
					}
				}
			}
			now = now.Add(15 * time.Minute)
		}
		if raised != 1 || cleared != 1 {
			t.Fatal("freeze-age completion changed the real notifier recovery contract")
		}
	}
}
