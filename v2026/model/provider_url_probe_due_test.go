// Durable URL scheduling is paced, quota-bound, and independent of process life.
package model

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Scheduled fixtures carry the actual supported URL contract. Copy first so
// concurrent retries never race while assembling immutable receipt evidence.
func testingSetUrlProbeHealth(ctx context.Context, health *ProviderEgressHealth) {
	reported := *health
	if !reported.CycleStartedAt.IsZero() && reported.Total == 1 {
		reported.UrlProbeEvidence = fp2TestUrlProbeEvidence(reported.MeasuredAt, reported.OKCount > 0)
	}
	SetProviderEgressHealth(ctx, &reported)
}

// Secure completion is empirical. Missing admission state is tracked apart,
// while every incomplete eligible provider belongs to warm-up or overdue.
func testingGetUrlProbeFleet(t testing.TB, ctx context.Context, now time.Time) ProviderUrlProbeFleet {
	t.Helper()
	fleet := GetProviderUrlProbeFleet(ctx, now)
	if fleet.Complete+fleet.Overdue+fleet.Warming != fleet.Eligible {
		t.Fatalf("fleet accounting omitted or duplicated eligible providers: %+v", fleet)
	}
	return fleet
}

// Ten accepted measured runs finish inside four hours, counting successes and errors.
// Every call models a fresh worker, so no process-local quota can satisfy it.
func TestUrlProbeDurablePacingAndQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		var cycleStartedAt, firstSuccessAt time.Time
		for turn := range 10 {
			due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
			if len(due) != 1 || due[0].ClientId != provider.clientId {
				t.Fatalf("turn %d: due=%+v", turn, due)
			}
			if turn == 0 {
				cycleStartedAt = due[0].CycleStartedAt
				firstSuccessAt = now
			}
			if !due[0].CycleStartedAt.Equal(cycleStartedAt) {
				t.Fatal("incomplete cycle lost progress on a new worker")
			}
			if due[0].OutcomeCount != turn {
				t.Fatalf("turn %d: durable source ordinal=%d", turn, due[0].OutcomeCount)
			}
			if duplicate := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1); len(duplicate) != 0 {
				t.Fatal("second worker claimed an already-reserved URL turn")
			}
			ok := 1
			if turn == 3 {
				ok = 0
			}
			result := &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.clientId,
				CycleStartedAt: cycleStartedAt, MeasuredAt: now, OKCount: ok, Total: 1}
			testingSetUrlProbeHealth(ctx, result)
			testingSetUrlProbeHealth(ctx, result)
			var successes, errors int
			var next time.Time
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT success_count,error_count,next_attempt_at FROM provider_egress_probe_cycle WHERE client_id=$1`, provider.clientId)
				server.WithPgResult(rows, err, func() {
					if !rows.Next() {
						t.Fatal("missing durable cycle")
					}
					server.Raise(rows.Scan(&successes, &errors, &next))
				})
			})
			wantErrors := 0
			if turn >= 3 {
				wantErrors = 1
			}
			if successes != turn+1-wantErrors || errors != wantErrors {
				t.Fatalf("turn %d replay changed progress: successes=%d errors=%d", turn, successes, errors)
			}
			if turn+1 == ProviderUrlProbeRunTarget {
				if !now.Before(firstSuccessAt.Add(ProviderEgressProbeRefreshAge)) || !next.Equal(firstSuccessAt.Add(ProviderEgressProbeRefreshAge-ProviderUrlProbeRenewalHeadroom)) {
					t.Fatalf("rolling quota missed its replacement deadline: completion=%s next=%s first=%s", now, next, firstSuccessAt)
				}
			} else {
				interval := 20 * time.Minute
				if ok == 0 {
					interval = time.Minute
				}
				if next.Sub(now) < interval*9/10 || next.Sub(now) > interval*11/10 {
					t.Fatalf("turn %d pace=%s expected %s +/-10%%", turn, next.Sub(now), interval)
				}
			}
			if early := ClaimProviderUrlProbeDue(ctx, next.Add(-time.Microsecond), 1, 0, 1); len(early) != 0 {
				t.Fatal("URL turn admitted before durable pace elapsed")
			}
			now = next
		}
		replacement := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(replacement) != 1 || replacement[0].RunsNeeded != 0 || replacement[0].OutcomeCount != 10 || !replacement[0].CycleStartedAt.Equal(cycleStartedAt) {
			t.Fatalf("early replacement changed measured quota or receipt identity: %+v", replacement)
		}
	})
}

// ARIN non-quality, TLS quarantine and ordinary failures still need URL
// measurements. Only reliability or ARIN risk exclude this revalidation queue.
func TestUrlProbeDueUsesReliabilityAndArinRisk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		clean := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		nonQuality := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{ArinNonQuality: true})
		risk := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{ArinRisk: true})
		tls := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		unreliable := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		egressTestBlackhole(ctx, clean.clientId)
		egressTestReliability(ctx, unreliable.clientId, 1, 0.1, 1)
		testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{ClientId: tls.clientId, MeasuredAt: server.NowUtc(), Total: 1, TLSAuthenticationFailure: true})
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		fleet := testingGetUrlProbeFleet(t, ctx, server.NowUtc().Add(time.Second))
		if fleet.Eligible != 3 || fleet.Due != 3 || fleet.Complete != 0 || fleet.RunsNeeded != 30 {
			t.Fatalf("URL fleet denominator includes excluded providers or lost quota: %+v", fleet)
		}
		due := ClaimProviderUrlProbeDue(ctx, server.NowUtc().Add(time.Second), 10, 0, 1)
		want := map[server.Id]bool{clean.clientId: true, nonQuality.clientId: true, tls.clientId: true}
		for _, provider := range due {
			if !want[provider.ClientId] {
				t.Fatalf("ineligible provider claimed: %s (risk=%s unreliable=%s)", provider.ClientId, risk.clientId, unreliable.clientId)
			}
			delete(want, provider.ClientId)
		}
		if len(want) > 0 {
			t.Fatalf("eligible URL revalidations missing: %v", want)
		}
	})
}

// Local infrastructure failures retry promptly but neither advance the success
// target nor enter the measured URL ratio. A crash still uses the claim lease.
func TestUrlProbeLocalFailurePreservesQuotaAndEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(due) != 1 {
			t.Fatalf("missing initial URL turn: %+v", due)
		}
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{ClientId: provider.clientId, AttemptAt: now, ProbeFailure: "tunnel_failed"})
		var successes, errors, history int
		var next time.Time
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT success_count,error_count,next_attempt_at,
				(SELECT COUNT(*) FROM provider_egress_health_history WHERE client_id=$1)
				FROM provider_egress_probe_cycle WHERE client_id=$1`, provider.clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing URL cycle after local failure")
				}
				server.Raise(rows.Scan(&successes, &errors, &next, &history))
			})
		})
		if successes != 0 || errors != 0 || history != 0 || next.Sub(now) < 54*time.Second || next.Sub(now) > 66*time.Second {
			t.Fatalf("local failure changed evidence or missed retry pace: successes=%d errors=%d history=%d next=%s", successes, errors, history, next.Sub(now))
		}
		retry := ClaimProviderUrlProbeDue(ctx, next, 1, 0, 1)
		if len(retry) != 1 || retry[0].RunsNeeded != 10 || retry[0].OutcomeCount != 0 || !retry[0].CycleStartedAt.Equal(due[0].CycleStartedAt) {
			t.Fatalf("local retry lost durable quota or source ordinal: %+v", retry)
		}
	})
}

// The index hint may move a provider into or out of the queue, but cannot
// manufacture a new cooldown, erase progress, or override authoritative gates.
func TestUrlProbeEligibilityHintPreservesPacing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(due) != 1 {
			t.Fatal("eligible provider was not seeded")
		}
		testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.clientId,
			CycleStartedAt: due[0].CycleStartedAt, MeasuredAt: now, OKCount: 0, Total: 1})
		claimAt := now.Add(2 * time.Minute)
		setRisk := func(risk, publishHint bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=$2 WHERE client_id=$1`, provider.clientId, risk))
				if publishHint {
					updateProviderUrlProbeEligibility(ctx, tx)
				}
			})
		}
		setRisk(true, false)
		if stale := ClaimProviderUrlProbeDue(ctx, claimAt, 1, 0, 1); len(stale) != 0 {
			t.Fatal("stale eligibility hint overrode the ARIN risk gate")
		}
		setRisk(true, true)
		if excluded := ClaimProviderUrlProbeDue(ctx, claimAt, 1, 0, 1); len(excluded) != 0 {
			t.Fatal("ineligible index row remained claimable")
		}
		setRisk(false, true)
		retry := ClaimProviderUrlProbeDue(ctx, claimAt, 1, 0, 1)
		if len(retry) != 1 || retry[0].OutcomeCount != 1 || retry[0].RunsNeeded != 9 || !retry[0].CycleStartedAt.Equal(due[0].CycleStartedAt) {
			t.Fatalf("eligibility transition reset or delayed durable progress: %+v", retry)
		}
	})
}

// Nine old successes cannot combine with a new one to satisfy a trailing
// four-hour target. The admission token and source ordinal still survive.
func TestUrlProbeRollingWindowExpiresOldSuccesses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc().Truncate(time.Microsecond)
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		startedAt := now.Add(-5 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2,next_attempt_at=$2 WHERE client_id=$1`, provider.clientId, startedAt))
		})
		for index := range 10 {
			measuredAt := startedAt.Add(time.Duration(index) * time.Minute)
			if index == 9 {
				measuredAt = now
			}
			testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.clientId,
				CycleStartedAt: startedAt, MeasuredAt: measuredAt, OKCount: 1, Total: 1})
		}
		due := ClaimProviderUrlProbeDue(ctx, now.Add(23*time.Minute), 1, 0, 1)
		if len(due) != 1 || due[0].RunsNeeded != 9 || due[0].OutcomeCount != 10 || !due[0].CycleStartedAt.Equal(startedAt) {
			t.Fatalf("aged successes satisfied a rolling quota or reset receipt identity: %+v", due)
		}
		fleet := testingGetUrlProbeFleet(t, ctx, now.Add(23*time.Minute))
		if fleet.Complete != 0 || fleet.RunsNeeded != 9 {
			t.Fatalf("fleet snapshot counted aged successes as rolling completion: %+v", fleet)
		}
	})
}

// Legacy one-URL status reports retain their audit receipt but cannot acquire
// the newer real-content/timing contract merely by carrying an admission token.
func TestUrlProbeUnversionedOutcomesNeverFillQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		initial := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(initial) != 1 {
			t.Fatal("missing initial URL admission")
		}
		for range 10 {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.clientId,
				CycleStartedAt: initial[0].CycleStartedAt, MeasuredAt: now, OKCount: 1, Total: 1})
		}
		now = now.Add(ProviderEgressProbeAttemptBackoff)
		due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(due) != 1 || due[0].RunsNeeded != 10 || due[0].OutcomeCount != 0 {
			t.Fatalf("unsupported history acquired current URL quota credit: %+v", due)
		}
		fleet := testingGetUrlProbeFleet(t, ctx, now)
		if fleet.Complete != 0 || fleet.QuotaComplete != 0 || fleet.RunsNeeded != 10 {
			t.Fatalf("census interpreted legacy status as current success evidence: %+v", fleet)
		}
		testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.clientId,
			CycleStartedAt: due[0].CycleStartedAt, MeasuredAt: now, OKCount: 1, Total: 1})
		due = ClaimProviderUrlProbeDue(ctx, now.Add(23*time.Minute), 1, 0, 1)
		if len(due) != 1 || due[0].RunsNeeded != 9 || due[0].OutcomeCount != 1 {
			t.Fatalf("compatible URL receipt did not advance its independent quota: %+v", due)
		}
	})
}

// Concurrent acknowledgements serialize before the rolling read, and replaying
// any accepted run cannot double its quota credit or durable source ordinal.
func TestUrlProbeConcurrentReceiptsKeepRollingProgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		token := now.Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle
				(client_id,cycle_started_at,next_attempt_at) VALUES($1,$2,$2)`, clientId, token))
		})
		var receipts sync.WaitGroup
		start := make(chan struct{})
		for index := range 20 {
			ok := index % 2
			health := ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId,
				CycleStartedAt: token, MeasuredAt: now, OKCount: ok, Total: 1}
			for range 2 {
				receipts.Add(1)
				go func() {
					defer receipts.Done()
					<-start
					testingSetUrlProbeHealth(ctx, &health)
				}()
			}
		}
		close(start)
		receipts.Wait()
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT success_count,error_count,outcome_count,next_attempt_at,
				(SELECT COUNT(*) FROM provider_egress_health_history WHERE client_id=$1)
				FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing durable receipt progress")
				}
				var successes, errors, ordinal, history int
				var next time.Time
				server.Raise(rows.Scan(&successes, &errors, &ordinal, &next, &history))
				if successes != 10 || errors != 10 || ordinal != 20 || history != 20 {
					t.Fatalf("concurrent receipts lost or duplicated progress: success=%d error=%d ordinal=%d history=%d", successes, errors, ordinal, history)
				}
				renewalAt := now.Add(ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom)
				if !next.Equal(renewalAt) {
					t.Fatalf("concurrent receipts lost oldest-measurement renewal pacing: got %s, want %s", next, renewalAt)
				}
			})
		})
	})
}

// Quota-complete providers remain active while any exact-URL or unidentified
// legacy TLS exception is unresolved. Local failure still uses the short pace.
func TestUrlProbeSecurityRechecksContinueAfterRollingQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		known := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		unknown := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		initial := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 1)
		if len(initial) != 2 {
			t.Fatalf("missing initial URL admissions: %+v", initial)
		}
		for _, provider := range initial {
			for range 10 {
				testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.ClientId,
					CycleStartedAt: provider.CycleStartedAt, MeasuredAt: now, OKCount: 1, Total: 1})
			}
		}
		destination := egresshealth.Destination{Name: "synthetic-security", Class: egresshealth.ClassSite, Url: "https://synthetic-security.example/page"}
		encoded, err := json.Marshal(destination)
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security
				(client_id,url_key,destination,measured_at,tls_failure) VALUES($1,$2,$3::jsonb,$4,true)`, known.clientId,
				egresshealth.UrlProbeDestinationKey(destination), string(encoded), now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_health SET
				tls_authentication_failure=true,legacy_tls_authentication_failure=true WHERE client_id=$1`, unknown.clientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1`, now))
		})
		fleet := testingGetUrlProbeFleet(t, ctx, now)
		if fleet.QuotaComplete != 2 || fleet.Complete != 0 || fleet.SecurityExceptions != 2 || fleet.SecurityUnknownTargets != 1 || fleet.RunsNeeded != 0 {
			t.Fatalf("quota success hid unresolved security: %+v", fleet)
		}
		due := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 1)
		if len(due) != 2 {
			t.Fatalf("ten successes suppressed TLS revalidation: %+v", due)
		}
		for _, provider := range due {
			if provider.RunsNeeded != 0 {
				t.Fatal("security recheck invented a URL quota deficit")
			}
			if provider.ClientId == known.clientId && (len(provider.SecurityDestinations) != 1 || provider.SecurityDestinations[0].Url != destination.Url) {
				t.Fatalf("security recheck lost its configured destination snapshot: %+v", provider)
			}
			if provider.ClientId == unknown.clientId && len(provider.SecurityDestinations) != 0 {
				t.Fatal("unknown legacy TLS target was fabricated")
			}
		}
		failedAt := now.Add(time.Second)
		SetProviderEgressProbeAttempt(ctx, &ProviderEgressProbeAttempt{ClientId: known.clientId, AttemptAt: failedAt, ProbeFailure: "tunnel_failed"})
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT next_attempt_at FROM provider_egress_probe_cycle WHERE client_id=$1`, known.clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing paced security retry")
				}
				var next time.Time
				server.Raise(rows.Scan(&next))
				if next.Sub(failedAt) < 54*time.Second || next.Sub(failedAt) > 66*time.Second {
					t.Fatalf("quota-full local TLS recheck failure was not paced: %s", next.Sub(failedAt))
				}
			})
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_url_security SET tls_failure=false WHERE client_id=$1`, known.clientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, known.clientId, failedAt))
		})
		if settled := ClaimProviderUrlProbeDue(ctx, failedAt, 1, 0, 1); len(settled) != 0 {
			t.Fatalf("fully satisfied provider kept opening URL turns: %+v", settled)
		}
		fleet = testingGetUrlProbeFleet(t, ctx, failedAt)
		if fleet.QuotaComplete != 2 || fleet.Complete != 1 || fleet.SecurityExceptions != 1 || fleet.SecurityUnknownTargets != 1 {
			t.Fatalf("exact URL recovery did not preserve independent legacy quarantine: %+v", fleet)
		}
	})
}

// Warm-up is tied to durable first eligibility, never process startup. Missing
// admission rows remain explicit deficits, and re-eligibility cannot reset age.
func TestUrlProbeFleetTracksMissingWarmupAndRollingExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		fleet := testingGetUrlProbeFleet(t, ctx, now)
		if fleet.Eligible != 1 || fleet.Warming != 1 || fleet.Overdue != 0 || fleet.MissingCycles != 0 || fleet.CohortStartedAtSeconds <= 0 {
			t.Fatalf("initial durable warmup is not visible: %+v", fleet)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, provider.clientId))
		})
		fleet = testingGetUrlProbeFleet(t, ctx, now)
		if fleet.Eligible != 1 || fleet.Due != 1 || fleet.MissingCycles != 1 || fleet.Overdue != 1 || fleet.Warming != 0 || fleet.RunsNeeded != 10 || fleet.CohortStartedAtSeconds != 0 {
			t.Fatalf("missing admission became healthy or received invented warmup: %+v", fleet)
		}
		token := now.Add(-5 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible)
				VALUES($1,$2,$2,true)`, provider.clientId, token))
		})
		for range 10 {
			testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.clientId,
				CycleStartedAt: token, MeasuredAt: now.Add(-239 * time.Minute), OKCount: 1, Total: 1})
		}
		fleet = testingGetUrlProbeFleet(t, ctx, now)
		if fleet.QuotaComplete != 1 || fleet.Complete != 1 || fleet.Overdue != 0 || fleet.Warming != 0 {
			t.Fatalf("recent rolling quota is incomplete: %+v", fleet)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, provider.clientId))
		})
		fleet = testingGetUrlProbeFleet(t, ctx, now)
		if fleet.Eligible != 1 || fleet.Due != 1 || fleet.MissingCycles != 1 || fleet.QuotaComplete != 1 || fleet.Complete != 1 || fleet.Overdue != 0 || fleet.Warming != 0 {
			t.Fatalf("missing admission state revoked real rolling evidence or disappeared: %+v", fleet)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible,outcome_count)
				VALUES($1,$2,$2,true,10)`, provider.clientId, token))
		})
		setRisk := func(risk bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=$2 WHERE client_id=$1`, provider.clientId, risk))
				updateProviderUrlProbeEligibility(ctx, tx)
			})
		}
		setRisk(true)
		if excluded := testingGetUrlProbeFleet(t, ctx, now); excluded.Eligible != 0 {
			t.Fatalf("risk-excluded provider remained in census: %+v", excluded)
		}
		setRisk(false)
		afterExpiry := now.Add(2 * time.Minute)
		due := ClaimProviderUrlProbeDue(ctx, afterExpiry, 1, 0, 1)
		if len(due) != 1 || !due[0].CycleStartedAt.Equal(token) || due[0].OutcomeCount != 10 || due[0].RunsNeeded != 10 {
			t.Fatalf("re-eligibility reset rolling receipt state: %+v", due)
		}
		fleet = testingGetUrlProbeFleet(t, ctx, afterExpiry)
		if fleet.Eligible != 1 || fleet.Overdue != 1 || fleet.Warming != 0 || fleet.Complete != 0 || fleet.RunsNeeded != 10 {
			t.Fatalf("expired rolling evidence received a new warmup: %+v", fleet)
		}
	})
}

// Logical slot ownership survives workers moving hosts; repartitioning cannot
// duplicate outstanding claims or reset receipt identity and source progress.
func TestUrlProbeSlotAffinityFairnessAndRedistribution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 32)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1::timestamp-(slot_id%17)*interval '1 second'`, now))
		})
		seen := map[server.Id]bool{}
		for shard := range 3 {
			want := []server.Id{}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT client_id FROM provider_egress_probe_cycle WHERE slot_id%3=$1 ORDER BY next_attempt_at,client_id`, shard)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var id server.Id
						server.Raise(rows.Scan(&id))
						want = append(want, id)
					}
				})
			})
			var received int
			for {
				due := ClaimProviderUrlProbeDue(ctx, now, 3, shard, 3)
				if len(due) == 0 {
					break
				}
				for _, provider := range due {
					if received >= len(want) || provider.ClientId != want[received] || seen[provider.ClientId] {
						t.Fatalf("slot affinity or oldest-first order changed: shard=%d received=%d due=%+v", shard, received, due)
					}
					if !provider.CycleStartedAt.Equal(now) || provider.OutcomeCount != 0 {
						t.Fatal("slot admission changed durable receipt identity")
					}
					seen[provider.ClientId] = true
					received++
				}
			}
			if received != len(want) {
				t.Fatalf("shard%d lost slot providers: received=%d expected=%d", shard, received, len(want))
			}
		}
		if len(seen) != 32 {
			t.Fatalf("slot partition lost providers: got%d", len(seen))
		}
		for shard := range 4 {
			if duplicate := ClaimProviderUrlProbeDue(ctx, now, 32, shard, 4); len(duplicate) != 0 {
				t.Fatalf("repartition duplicated a live claim: %+v", duplicate)
			}
		}
		seen = map[server.Id]bool{}
		for shard := range 4 {
			for _, provider := range ClaimProviderUrlProbeDue(ctx, now.Add(ProviderEgressProbeAttemptBackoff), 32, shard, 4) {
				if seen[provider.ClientId] || !provider.CycleStartedAt.Equal(now) || provider.OutcomeCount != 0 {
					t.Fatalf("redistribution duplicated or reset provider: %+v", provider)
				}
				seen[provider.ClientId] = true
			}
		}
		if len(seen) != 32 {
			t.Fatalf("redistribution lost providers after reservation expiry: got%d", len(seen))
		}
	})
}
