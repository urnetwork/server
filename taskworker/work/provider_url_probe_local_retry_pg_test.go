// Native ownership controls carry ordinary worker-local failure into the API model.
package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

// This bridge uses the ordinary public controller and real database owner.
// Authentication/HTTP transport are outside this local ownership control.
type localRetryControllerSink struct {
	*recordingEgressProbeIngest
	completion qualityprobe.UrlProbeCompletion
	receipt    *controller.RecordProviderEgressProbeAttemptResult
}

// Route the worker receipt through the actual public controller and database owner.
func (self *localRetryControllerSink) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	self.completion = completion
	id, err := server.ParseId(completion.ClientId)
	if err != nil {
		return err
	}
	self.receipt, err = controller.RecordProviderEgressProbeAttempt(ctx, &controller.RecordProviderEgressProbeAttemptArgs{
		ClientId: id, ClaimOrdinal: completion.ClaimOrdinal, CompletedAt: completion.CompletedAt,
		ProbeFailure: completion.ProbeFailure, AllowPacing: completion.AllowPacing,
	})
	return err
}

// Funding readiness refusal finishes its issued claim without a provider verdict.
func TestUrlCompletedLocalReadinessVetoReleasesFinishedLease(t *testing.T) {
	testingLocalWorkerCompletionReleasesFinishedLease(t, false)
}

// A claimed turn that never starts releases only its own live reservation.
func TestUrlCompletedLocalUnstartedHealthNotRunReleasesFinishedLease(t *testing.T) {
	testingLocalWorkerCompletionReleasesFinishedLease(t, true)
}

// Both worker branches preserve the same claim, evidence and retry ownership.
func testingLocalWorkerCompletionReleasesFinishedLease(t *testing.T, unstarted bool) {
	t.Helper()
	name := "readiness_veto"
	if unstarted {
		name = "unstarted_health_not_run"
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Add(-time.Second).Truncate(time.Microsecond)
		client, network := server.NewId(), server.NewId()
		location := &model.Location{LocationType: model.LocationTypeCity, City: "Synthetic City", Region: "Synthetic Region", Country: "Synthetic Country", CountryCode: "zz"}
		model.CreateLocation(ctx, location)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id) VALUES($1,$2)`, client, network))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provide_key(client_id,provide_mode,secret_key) VALUES($1,$2,'synthetic-key')`, client, model.ProvideModePublic))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
				(client_id,network_id,update_block_number,country_location_id,client_address_hash_count,location_count,connected)
				VALUES($1,$2,1,$3,1,1,true)`, client, network, location.CountryLocationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible)
				VALUES($1,$2,$3,true)`, client, now.Add(-8*time.Hour), now))
		})
		due := model.ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(due) != 1 || due[0].ClientId != client {
			t.Fatal("missing ordinary issued claim")
		}
		if duplicate := model.ClaimProviderUrlProbeDue(ctx, now.Add(90*time.Second), 1, 0, 1); len(duplicate) != 0 {
			t.Fatal("active lease was shortened")
		}
		sink := &localRetryControllerSink{recordingEgressProbeIngest: newRecordingEgressProbeIngest()}
		if unstarted {
			pass := &providerEgressProbePass{fullSink: testFullBatchSink(sink)}
			observation := newProviderUrlProbeSchedulerMetrics().begin()
			defer observation.close()
			if err := pass.completeUnstartedUrlClaims(ctx, []ingest.DueProvider{{ClientId: client.String(), CountryCode: "zz", ClaimOrdinal: due[0].ClaimOrdinal, ClaimedAt: due[0].ClaimedAt}}, 1, observation); err != nil {
				t.Fatal(err)
			}
		} else {
			// Real funding query against this unfunded synthetic network,
			// followed by the actual readiness-veto completion adapter.
			reporter := &providerEgressProbeReadinessReporter{egressProbeIngest: sink, readiness: newProviderEgressProbeReadiness(network)}
			err := reporter.ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: client.String(), ClaimOrdinal: due[0].ClaimOrdinal,
				CompletedAt: server.NowUtc(), ProbeFailure: prober.FailureTunnel, AllowPacing: true})
			if !errors.Is(err, errProviderEgressProbeUnfunded) {
				t.Fatalf("missing real readiness veto: %v", err)
			}
		}
		if sink.receipt == nil || sink.completion.AllowPacing || sink.completion.ClaimOrdinal != due[0].ClaimOrdinal {
			t.Fatal("worker branch changed local verdict or claim")
		}
		var next time.Time
		var count, history, successes, failures, health int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT next_attempt_at,completed_run_count,success_count,error_count,
				(SELECT count(*) FROM provider_egress_health_history WHERE client_id=$1),
				(SELECT count(*) FROM provider_egress_health WHERE client_id=$1)
				FROM provider_egress_probe_cycle WHERE client_id=$1`, client).Scan(&next, &count, &successes, &failures, &history, &health))
		})
		if delay := next.Sub(sink.receipt.ReceivedAt); delay < 54*time.Second || delay > 66*time.Second {
			t.Fatalf("real %s completion retained15m lease: delay=%s", name, delay)
		}
		if count != 1 || history != 0 || successes != 0 || failures != 0 || health != 0 || len(sink.health) != 0 || len(sink.calls) != 0 {
			t.Fatal("local completion fabricated a measured/provider verdict")
		}
		if retry := model.ClaimProviderUrlProbeDue(ctx, next, 1, 0, 1); len(retry) != 1 || retry[0].ClaimOrdinal != due[0].ClaimOrdinal+1 || retry[0].RunsNeeded != 10 {
			t.Fatalf("local completion could not retry without quota credit: %+v", retry)
		}
	})
}
