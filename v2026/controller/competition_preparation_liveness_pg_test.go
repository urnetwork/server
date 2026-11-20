// An independent source transaction must not serialize the watchdog's reads.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Keep the real advisory lock held until the monitor independently observes
// the already-activated fixture and refuses it for the correct lifecycle reason.
func TestCompetitionPreparationLivenessPgObservesLifecycleWithScopeHeld(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-preparation-monitor", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := fixture.store.CurrentSourceActivation(ctx, fixture.settings)
		if err != nil {
			t.Fatal(err)
		}
		held, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		var releaseOnce sync.Once
		unlock := func() { releaseOnce.Do(func() { close(release) }) }
		defer func() {
			unlock()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		go func() {
			done <- captureDatabaseError(func() {
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					automationScopeLockInTx(ctx, tx, fixture.settings)
					close(held)
					select {
					case <-release:
					case <-ctx.Done():
						panic(ctx.Err())
					}
				}, server.OptNoRetry())
			})
		}()
		select {
		case <-held:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		err = checkCompetitionPreparationLiveness(ctx, fixture.settings, CompetitionPreparationRequest{Operation: "propagate_and_qualify", Intent: *intent, Lease: *lease})
		if !errors.Is(err, ErrConflict) {
			t.Fatal("monitor waited on source scope or missed activation", err)
		}
	})
}

// Real PostgreSQL resolves both digest types, zero/nonzero predecessor pointers
// and lifecycle flags; the result remains one boolean despite a large payload.
// Transaction-local shadow tables contain only synthetic facts and disappear
// at commit, without changing the fixture's durable activation authority.
func TestCompetitionPreparationLivenessPgProjectsLargeImmutableIntent(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-projection-monitor", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent := model.CompetitionSourceActivation{Environment: lease.Environment, CompetitionId: lease.CompetitionId, SeasonId: lease.SeasonId, Generation: 2, State: "prepared", Commits: preparationLivenessTestCommits(), Manifest: []byte(strings.Repeat("x", 1024*1024))}
		intent.IntentSha256, err = competitionPreparationIntentDigest(intent)
		if err != nil {
			t.Fatal(err)
		}
		request := CompetitionPreparationRequest{Intent: intent, Lease: *lease}
		err = captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := tx.Exec(ctx, `CREATE TEMP TABLE competition_source_activation(environment text,competition_id text,season_id text,generation bigint,intent_sha256 char(64),intent_json jsonb,activated_at timestamp,abandoned_at timestamp) ON COMMIT DROP; CREATE TEMP TABLE competition_source_pointer(environment text,competition_id text,season_id text,generation bigint) ON COMMIT DROP`)
				server.Raise(err)
				content, err := json.Marshal(intent)
				server.Raise(err)
				_, err = tx.Exec(ctx, `INSERT INTO competition_source_activation(environment,competition_id,season_id,generation,intent_sha256,intent_json) VALUES($1,$2,$3,$4,$5,$6)`, lease.Environment, lease.CompetitionId, lease.SeasonId, intent.Generation, intent.IntentSha256, content)
				server.Raise(err)
				server.Raise(checkCompetitionPreparationLivenessInTx(ctx, tx, fixture.settings, request))
				_, err = tx.Exec(ctx, `INSERT INTO competition_source_pointer VALUES($1,$2,$3,1)`, lease.Environment, lease.CompetitionId, lease.SeasonId)
				server.Raise(err)
				if err := checkCompetitionPreparationLivenessInTx(ctx, tx, fixture.settings, request); !errors.Is(err, ErrConflict) {
					panic("changed predecessor was not rejected")
				}
				request.Intent.ExpectedGeneration = 1
				request.Intent.IntentSha256, err = competitionPreparationIntentDigest(request.Intent)
				server.Raise(err)
				content, err = json.Marshal(request.Intent)
				server.Raise(err)
				_, err = tx.Exec(ctx, `UPDATE competition_source_activation SET intent_json=$1,intent_sha256=$2`, content, request.Intent.IntentSha256)
				server.Raise(err)
				server.Raise(checkCompetitionPreparationLivenessInTx(ctx, tx, fixture.settings, request))
				_, err = tx.Exec(ctx, `UPDATE competition_source_activation SET abandoned_at=now()`)
				server.Raise(err)
				if err := checkCompetitionPreparationLivenessInTx(ctx, tx, fixture.settings, request); !errors.Is(err, ErrConflict) {
					panic("abandoned intent was not rejected")
				}
			}, server.OptNoRetry())
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
