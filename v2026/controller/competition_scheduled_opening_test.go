// Isolated stores bind announced schedules at both Go and direct SQL boundaries.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A round may be prepared before admission, but neither an early nor a delayed
// writer may replace its exact immutable opening or shorten its full window.
func TestCompetitionScheduledOpeningIsBoundInStoreAndDatabase(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, staging := range []bool{false, true} {
			fixture := newCompetitionAutomationFixtureWithMode(t, staging)
			fixture.now = fixture.round.RevealAt
			sealed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-schedule-owner", 5*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			intent := competitionAutomationSourceIntent(t, fixture, sealed, 2)
			intent.OpensAt = fixture.now.Add(48 * time.Hour)
			stored, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
			if err != nil {
				t.Fatal(err)
			}
			acknowledgeCompetitionAutomationSource(t, fixture, stored, "api", "worker")
			fixture.now = stored.PreparedAt.Add(sourcePreparationInterval(stored))
			lease, err = fixture.store.AcquireAutomationLease(ctx, fixture.settings, lease.OwnerId, 5*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			active, err := fixture.store.ActivateSource(ctx, fixture.settings, *lease, stored.Generation, stored.ExpectedGeneration)
			if err != nil {
				t.Fatal(err)
			}
			qualified, err := sourceSettingsFromIntent(fixture.settings, active)
			if err != nil {
				t.Fatal(err)
			}
			qualified.automationTargetRoundId = stored.TargetRoundId
			window := 7 * 24 * time.Hour
			if staging {
				window = time.Duration(stored.StagingRoundWindowSeconds) * time.Second
			}
			for _, offset := range []time.Duration{-time.Minute, time.Minute} {
				wrongOpening := stored.OpensAt.Add(offset)
				if _, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, stored.Generation, wrongOpening); !errors.Is(err, ErrReviewNotReady) {
					t.Fatalf("Go accepted a moved schedule staging=%t: %v", staging, err)
				}
				wrong, err := fixture.store.prepareRound(ctx, qualified, GenerateRoundArgs{OpensAt: wrongOpening, ClosesAt: wrongOpening.Add(window), RevealAt: wrongOpening.Add(window)}, staging)
				if err != nil {
					t.Fatal(err)
				}
				databaseErr := captureDatabaseError(func() {
					server.Tx(ctx, func(tx server.PgTx) {
						server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
						 VALUES($1,$2,2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,false)`, wrong.RoundId, wrong.CompetitionId, staging, wrong.WorkloadCommitment, wrong.SeedNonce, wrong.SeedCiphertext,
							wrong.ProvidersSha256, wrong.ProvidersPath, string(wrong.PolicyJson), wrong.OpensAt, wrong.ClosesAt, wrong.RevealAt, wrong.CreatedAt))
					})
				})
				if databaseErr == nil || !strings.Contains(databaseErr.Error(), "qualified full-window schedule") {
					t.Fatalf("SQL accepted a moved schedule staging=%t: %v", staging, databaseErr)
				}
			}
			created, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, stored.Generation, stored.OpensAt)
			if err != nil || created == nil || created.Status != "scheduled" || !created.OpensAt.Equal(stored.OpensAt) || created.ClosesAt.Sub(created.OpensAt) != window {
				t.Fatalf("exact scheduled round rejected staging=%t: %+v %v", staging, created, err)
			}
			round, err := fixture.store.CurrentAutomationRound(ctx, fixture.settings)
			if err != nil || submissionWithinEpoch(round, stored.OpensAt.Add(-time.Microsecond)) || !submissionWithinEpoch(round, stored.OpensAt) || submissionWithinEpoch(round, stored.OpensAt.Add(window)) {
				t.Fatalf("scheduled admission boundaries drifted staging=%t: %v", staging, err)
			}
		}
	})
}
