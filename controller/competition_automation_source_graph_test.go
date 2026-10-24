// Private database fixtures prove both modes require the complete live graph.
package controller

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Both Go and direct SQL require the exact graph for the explicitly configured mode.
func TestCompetitionAutomationSourceGraphIsModeBoundInStoreAndDatabase(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, staging := range []bool{true, false} {
			fixture := newCompetitionAutomationFixtureWithMode(t, staging)
			fixture.now = fixture.round.RevealAt
			sealed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-graph-supervisor", 5*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			intent := competitionAutomationSourceIntent(t, fixture, sealed, 2)
			wantCount := 10
			if len(intent.Commits) != wantCount || len(intent.GitRefs) != wantCount+1 || intent.Commits["operator-proxy"] != "" || intent.Commits["gvisor"] == "" {
				t.Fatalf("invalid graph fixture staging=%t: %+v", staging, intent.Commits)
			}
			for _, mutation := range []string{"retired-repository", "missing-gvisor", "unknown-repository", "cross-mode"} {
				invalid := intent
				invalid.Commits, invalid.GitRefs = maps.Clone(intent.Commits), maps.Clone(intent.GitRefs)
				switch mutation {
				case "cross-mode":
					invalid.Staging = !staging
				case "retired-repository":
					invalid.Commits["operator-proxy"] = fixture.settings.BaseSha
					invalid.GitRefs["operator-proxy"] = model.CompetitionSourceGitRef{Branch: intent.GitRefs["server"].Branch, Commit: fixture.settings.BaseSha, ExpectedCommit: fixture.settings.BaseSha}
				case "missing-gvisor":
					delete(invalid.Commits, "gvisor")
					delete(invalid.GitRefs, "gvisor")
				case "unknown-repository":
					invalid.Commits["synthetic-unknown"] = invalid.Commits["gvisor"]
					invalid.GitRefs["synthetic-unknown"] = invalid.GitRefs["gvisor"]
					delete(invalid.Commits, "gvisor")
					delete(invalid.GitRefs, "gvisor")
				}
				if _, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, invalid); err == nil {
					t.Fatalf("Go accepted %s staging=%t", mutation, staging)
				}
				content, err := json.Marshal(invalid)
				if err != nil {
					t.Fatal(err)
				}
				databaseErr := captureDatabaseError(func() {
					server.Tx(ctx, func(tx server.PgTx) {
						server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
						server.RaisePgResult(tx.Exec(ctx, `SELECT competition_v2_source_authorized($1::jsonb,$2,$3,$4)`, string(content), intent.Environment, intent.CompetitionId, intent.SeasonId))
					})
				})
				expectedError := "repository graph"
				if mutation == "cross-mode" {
					expectedError = "source mode"
				}
				if databaseErr == nil || !strings.Contains(databaseErr.Error(), expectedError) {
					t.Fatalf("SQL accepted %s staging=%t: %v", mutation, staging, databaseErr)
				}
			}
			stored, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
			if err != nil || stored == nil || stored.SourceLockSha256 != intent.SourceLockSha256 {
				t.Fatalf("valid unchanged source rejected staging=%t: %+v %v", staging, stored, err)
			}
			acknowledgeCompetitionAutomationSource(t, fixture, stored, "api", "worker")
			active, err := fixture.store.ActivateSource(ctx, fixture.settings, *lease, stored.Generation, stored.ExpectedGeneration)
			if err != nil || active == nil || len(active.Commits) != wantCount || !maps.Equal(active.Commits, intent.Commits) || active.SourceGeneration != intent.SourceGeneration {
				t.Fatalf("mode-specific no-op carry-forward failed staging=%t: %+v %v", staging, active, err)
			}
		}
	})
}

// Runtime derivation cannot reinterpret an obsolete or shortened graph using a valid hash from another mode.
func TestCompetitionAutomationRuntimeSourceGraphRejectsRetiredStagingPin(t *testing.T) {
	settings, store, _ := newAutomationStagingApiFixture(t)
	valid := *store.intent
	if _, err := sourceSettingsFromIntent(settings, &valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Commits = maps.Clone(valid.Commits)
	invalid.Commits["operator-proxy"] = strings.Repeat("c", 40)
	if _, err := sourceSettingsFromIntent(settings, &invalid); err == nil {
		t.Fatal("runtime accepted the retired staging repository")
	}
	if sourceModeMatches(settings, &invalid) {
		t.Fatal("stored-source mode check accepted an obsolete staging graph")
	}
}
