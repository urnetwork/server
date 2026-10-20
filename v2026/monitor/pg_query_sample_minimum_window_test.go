package monitor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// These are reduction inputs, not claimed native process observations. The
// separate FIFO/native-child regression proves how production obtains them.
func TestPgQuerySampleMinimumWindowQualificationControls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			name      string
			remaining int64
			missing   bool
			spawn     string
			joined    bool
			expire    bool
			want      bool
		}{
			{name: "below", remaining: 21999, spawn: "started", joined: true, expire: true, want: true},
			{name: "exact boundary", remaining: 22000, spawn: "started", joined: true, expire: true},
			{name: "above boundary", remaining: 24397, spawn: "started", joined: true, expire: true},
			{name: "missing budget", missing: true, spawn: "started", joined: true, expire: true},
			{name: "unstarted", remaining: 21000, spawn: "not_attempted", expire: true},
			{name: "unknown spawn", remaining: 21000, spawn: "unknown", joined: true, expire: true},
			{name: "unjoined", remaining: 21000, spawn: "started", expire: true},
			{name: "non-owner deadline", remaining: 21000, spawn: "started", joined: true},
			{name: "invalid negative", remaining: -1, spawn: "started", joined: true, expire: true},
		} {
			env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
				t.Fatal("unexpected transport callback")
				return "", "", nil
			})
			env.cfg.pgQuerySampleUntil = time.Time{}
			env.cfg.pgQuerySampleContinuous = true
			env.now = time.Now
			// Both later forty-second children must inherit this earlier owner.
			// Equal virtual deadlines could otherwise run independent callbacks.
			ownerCtx, cancelOwner := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancelOwner()
			env.runner.(*runner).runSSH = func(ctx context.Context, _ []string, _ string) (string, string, error) {
				observation := pgSampleTransportFromContext(ctx)
				if observation == nil {
					t.Fatal("fixture did not reach the real PG observation context")
				}
				observation.receipt.Phase = "ssh_running"
				observation.receipt.Spawn = c.spawn
				observation.receipt.Joined = c.joined
				if !c.missing {
					remaining := c.remaining
					observation.receipt.SpawnRemainingBudgetMs = &remaining
				}
				if c.expire {
					<-ctx.Done() // virtual owner expiry, with its callback complete
					if ctx.Err() != context.DeadlineExceeded || ownerCtx.Err() != context.DeadlineExceeded {
						t.Fatal("fixture owner ended without its deadline")
					}
				}
				return "", "", context.DeadlineExceeded
			}
			findings, err := (pgQuerySampleProbe{}).check(ownerCtx, env)
			cancelOwner()
			if err != nil || len(findings) != 1 || findings[0].class != "pg-query-sample-unavailable" || findings[0].tier != tierWarn || findings[0].sustain != 1 || !strings.Contains(findings[0].observed, "reason=deadline phase=host_transport cause=observation-timeout") {
				t.Fatalf("%s changed the original incomplete observation: error=%v finding_count=%d", c.name, err, len(findings))
			}
			receipt, raw := pgTransportReadControlReceipt(t, env)
			if receipt.Complete || receipt.Samples != 0 || receipt.TransportObservation.OwnerDeadlineExceeded != c.expire {
				t.Fatalf("%s lost its source or owner boundary: complete=%t samples=%d owner_deadline=%t want_owner_deadline=%t", c.name, receipt.Complete, receipt.Samples, receipt.TransportObservation.OwnerDeadlineExceeded, c.expire)
			}
			want := fmt.Sprintf("collection_budget=below-mandatory-waits minimum_wait_ms=22000 spawn_remaining_budget_ms=%d", c.remaining)
			if strings.Contains(findings[0].observed, want) != c.want || strings.Contains(string(raw), want) != c.want {
				t.Fatalf("%s lost or invented the mandatory-wait budget qualification", c.name)
			}
			if !c.want && (strings.Contains(findings[0].observed, "collection_budget=") || strings.Contains(string(raw), "collection_budget=")) {
				t.Fatalf("%s claimed an unsupported sampling-window shortfall", c.name)
			}
			state, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), time.Now())
			if err != nil || !state.NextEligibleAt.Equal(time.Now().Add(pgQuerySampleCadence)) {
				t.Fatalf("%s changed the terminal cadence floor", c.name)
			}
			findings, err = (pgQuerySampleProbe{}).check(t.Context(), env)
			if err != nil || len(findings) != 1 || !strings.Contains(findings[0].observed, "cadence-not-due") {
				t.Fatalf("%s allowed an early retry", c.name)
			}
		}
	})
}

// The qualifier's lower bound belongs to the unchanged complete SQL workload;
// it excludes all connection, reader, history, activity and transfer overhead.
func TestPgQuerySampleMinimumWindowMatchesRequiredWaits(t *testing.T) {
	sql := pgQuerySampleSQL("fixture")
	if pgQuerySampleCount != 12 || strings.Count(sql, "SELECT pg_sleep(2);") != 11 || pgQuerySampleBudget != 40*time.Second {
		t.Fatal("minimum-window qualification changed the required SQL or owner")
	}
}
