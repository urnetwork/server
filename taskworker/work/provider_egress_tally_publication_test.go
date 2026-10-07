// Real PostgreSQL ownership distinguishes an auxiliary tally wait from the
// acknowledged health and exact-claim completion boundaries. Fixtures are local.
package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

// The single measured turn closes healthAcknowledged only after the real
// reporter accepts health. Exact completion is retained separately by value.
type tallyPublicationIngest struct {
	*recordingEgressProbeIngest
	healthAcknowledged chan struct{}
	completion         chan qualityprobe.UrlProbeCompletion
}

// Acknowledges one synthetic measured result without contacting an API.
func (self *tallyPublicationIngest) SubmitEgressHealth(ctx context.Context, clientId string, value *egresshealth.Result) error {
	err := self.recordingEgressProbeIngest.SubmitEgressHealth(ctx, clientId, value)
	if err == nil {
		close(self.healthAcknowledged)
	}
	return err
}

// Retains the terminal claim identity; no quota state is manufactured here.
func (self *tallyPublicationIngest) ReportUrlProbeCompletion(_ context.Context, completion qualityprobe.UrlProbeCompletion) error {
	self.completion <- completion
	return nil
}

// A real tally row lock must not hold an already-measured URL turn's terminal
// handback or worker ownership. PostgreSQL's blocker relation, not elapsed
// latency, makes the synchronous production path fail this regression.
func TestProviderEgressTallyLockDoesNotHoldUrlCompletion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		day := time.Date(2026, time.October, 6, 0, 0, 0, 0, time.UTC)
		place := model.ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic tally place"}
		model.AddProviderEgressRunTally(ctx, day, model.ProviderEgressRunTally{Place: place}, nil)
		inner := &tallyPublicationIngest{
			recordingEgressProbeIngest: newRecordingEgressProbeIngest(),
			healthAcknowledged:         make(chan struct{}),
			completion:                 make(chan qualityprobe.UrlProbeCompletion, 1),
		}
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		args.Full.Limit, args.Full.Concurrency = 1, 1
		deadline, _ := ctx.Deadline()
		wantCompletion := qualityprobe.UrlProbeCompletion{
			ClientId: "synthetic-tally-provider", ClaimOrdinal: 71,
			CompletedAt: day.Add(time.Hour), AllowPacing: true,
		}
		pass := &providerEgressProbePass{
			urlProbes: true, fullSink: testFullBatchSink(inner), fullReleaseDeadline: deadline,
			loadScoring: func(context.Context) (*model.ProviderEgressHealthScoring, *model.ProviderEgressSiteSettings) {
				return nil, model.DefaultProviderEgressSiteSettings()
			},
			recordTally: func(ctx context.Context, _ time.Time, run model.ProviderEgressRunTally, loads []model.ProviderEgressSiteLoad) {
				// Freeze only the calendar key so a midnight crossing cannot
				// bypass the row held by this fixture. The writer is production.
				recordProviderEgressRunTally(ctx, day, run, loads)
			},
			runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
				if len(providers) != 1 || providers[0].ClientId != wantCompletion.ClientId || providers[0].ClaimOrdinal != wantCompletion.ClaimOrdinal {
					t.Error("the synthetic exact claim changed before measurement")
				}
				run := &egresshealth.Result{Total: 1, OkCount: 1,
					Checks: []egresshealth.CheckResult{{Name: "synthetic-tally.example", Class: "site", Ok: true}}}
				if err := options.HealthResults.SubmitEgressHealth(ctx, wantCompletion.ClientId, run); err != nil {
					return prober.Summary{}, err
				}
				if err := options.Attempts.(qualityprobe.UrlProbeCompletionReporter).ReportUrlProbeCompletion(ctx, wantCompletion); err != nil {
					return prober.Summary{}, err
				}
				return prober.Summary{Attempted: 1, Submitted: 1}, nil
			},
		}
		completed := make(chan struct{})
		var outcome providerEgressFullOutcome
		var blocked bool
		var retainedCompletion *qualityprobe.UrlProbeCompletion
		started := false
		defer func() {
			cancel()
			if started {
				<-completed
			}
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			var holderPid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_place_tally
				SET update_time=update_time+interval '1 microsecond'
				WHERE tally_day=$1 AND country_code=$2 AND region=$3`, day, place.CountryCode, place.Region))
			started = true
			go func() {
				defer close(completed)
				outcome = pass.runFullBatch(ctx, args, nil, nil, []ingest.DueProvider{{
					ClientId: wantCompletion.ClientId, ClaimOrdinal: wantCompletion.ClaimOrdinal,
					CountryCode: place.CountryCode, Region: place.Region,
				}})
			}()
			// The holder returns (and therefore releases its lock) before the
			// outer join, including every assertion or cancellation path.
			select {
			case <-inner.healthAcknowledged:
			case <-ctx.Done():
				t.Errorf("health acknowledgment did not arrive: %v", ctx.Err())
				return
			}
			server.Db(ctx, func(conn server.PgConn) {
				for {
					select {
					case <-completed:
						return
					case <-ctx.Done():
						t.Errorf("publication discriminator expired: %v", ctx.Err())
						return
					default:
					}
					server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (
						SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
						AND $1=ANY(pg_blocking_pids(pid))
						AND query LIKE '%INSERT INTO provider_egress_place_tally%')`, holderPid).Scan(&blocked))
					if blocked {
						select {
						case got := <-inner.completion:
							retainedCompletion = &got
						default:
							t.Error("acknowledged URL health is missing its exact terminal completion while the auxiliary tally waits on the held daily-place row")
						}
						select {
						case <-completed:
						default:
							t.Error("acknowledged URL turn still owns its worker while the auxiliary tally waits on the held daily-place row")
						}
						return
					}
					select {
					case <-completed:
						return
					case <-ctx.Done():
					case <-time.After(5 * time.Millisecond):
					}
				}
			})
		}, server.TxReadCommitted, server.OptNoRetry())
		<-completed
		if outcome.err != nil || outcome.summary.Attempted != 1 || outcome.summary.Submitted != 1 || outcome.summary.Failed != 0 {
			t.Fatalf("the joined measured turn changed: %+v", outcome)
		}
		if retainedCompletion == nil {
			select {
			case got := <-inner.completion:
				retainedCompletion = &got
			default:
				t.Fatal("the joined turn lost its exact terminal handback")
			}
		}
		if *retainedCompletion != wantCompletion {
			t.Fatalf("exact terminal handback=%+v, want=%+v", retainedCompletion, wantCompletion)
		}
		select {
		case <-inner.completion:
			t.Fatal("the single turn published a duplicate terminal handback")
		default:
		}
	})
}
