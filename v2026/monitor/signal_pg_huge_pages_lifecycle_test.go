// Completion-clock cancellation stays inside the huge-pages observation's
// lifetime, including when the public adapter has no host-scope wrapper.
package monitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The PostgreSQL source returns with a live parent. The actual completion Now
// hook then cancels before parsing; the unchanged live case proves the fixture
// reaches the intended supported or malformed outcome through each public path.
func pgHugePagesCompletionCancellationForTest(t *testing.T, status string) {
	t.Helper()
	now := syntheticSettings(nil).Now()
	for _, scoped := range []bool{false, true} {
		for _, cancelCompletion := range []bool{false, true} {
			rows := pgHugePagesTestRows(status)
			if status == "malformed" {
				rows = []Row{{"private-huge-pages-completion-canary"}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			calls, completionClocks := 0, 0
			settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
				calls++
				if ctx.Err() != nil || query != pgHugePagesQuery {
					t.Fatal("huge-pages completion fixture lost its live owned source")
				}
				return rows, nil
			}})
			if scoped {
				settings.ExcludedHosts = []string{"redis-1"}
			}
			settings.Now = func() time.Time {
				if calls > 0 {
					completionClocks++
					if cancelCompletion {
						cancel()
					}
				}
				return now
			}
			alerts, err := NewPgHugePagesSignal().Run(ctx, settings)
			cancel()
			if calls != 1 || completionClocks == 0 {
				t.Fatal("huge-pages completion fixture did not reach the owned clock boundary")
			}
			if cancelCompletion {
				if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
					t.Fatalf("huge-pages completion cancellation published a verdict: scoped=%v status=%s", scoped, status)
				}
				continue
			}
			if err != nil {
				t.Fatalf("live huge-pages completion failed: scoped=%v status=%s", scoped, status)
			}
			switch status {
			case "on":
				if len(alerts) != 0 {
					t.Fatal("live supported-on completion lost healthy authority")
				}
			case "off":
				if len(alerts) != 1 {
					t.Fatal("live supported-off completion lost its incident")
				}
				alert := requireAlertClass(t, alerts, "pg-huge-pages-disabled")
				if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Target != "pg-1" {
					t.Fatal("live huge-pages completion changed the setting policy")
				}
			case "malformed":
				if len(alerts) != 1 {
					t.Fatal("live malformed completion lost its visibility finding")
				}
				alert := requireAlertClass(t, alerts, "pg-huge-pages-unobservable")
				if !alert.unobservable {
					t.Fatal("live malformed completion could clear the setting incident")
				}
				requireAlertOmits(t, alert, "private-huge-pages-completion-canary")
			default:
				t.Fatal("unknown huge-pages completion fixture")
			}
		}
	}
}

// A canceled on observation must not grant successful healthy completion.
func TestPgHugePagesCompletionCancellationOn(t *testing.T) {
	pgHugePagesCompletionCancellationForTest(t, "on")
}

// A canceled off observation must not publish a setting incident.
func TestPgHugePagesCompletionCancellationOff(t *testing.T) {
	pgHugePagesCompletionCancellationForTest(t, "off")
}

// A canceled malformed observation is lifecycle failure, not successful unknown.
func TestPgHugePagesCompletionCancellationMalformed(t *testing.T) {
	pgHugePagesCompletionCancellationForTest(t, "malformed")
}
