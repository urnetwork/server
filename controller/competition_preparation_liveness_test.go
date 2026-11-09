// Watchdog observations must progress while synchronous source authorization
// holds its season lock, without accepting revoked lifecycle authority.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The held scope is an immediate tripwire rather than a timing assertion.
type preparationLivenessTestTx struct {
	server.PgTx
	intent                                          model.CompetitionSourceActivation
	leaseValid, held, missing, abandoned, activated bool
	pointer                                         int64
}

// Production-shaped graph, entirely synthetic, without live source identities.
func preparationLivenessTestCommits() map[string]string {
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = strings.Repeat("a", 40)
	}
	return commits
}

// Guard installation is permitted, but any serialization is the old defect.
func (self *preparationLivenessTestTx) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	if !strings.Contains(query, "set_config(") {
		return pgconn.CommandTag{}, errors.New("synthetic source scope remains held")
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

// Only independent authority and lifecycle reads may reach the monitor.
func (self *preparationLivenessTestTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	return supervisorScopeRenewTestRow{scan: func(destinations []any) error {
		switch {
		case strings.Contains(query, "FOR UPDATE"), strings.Contains(query, "pg_advisory"):
			return errors.New("synthetic source scope remains held")
		case strings.Contains(query, "FROM competition_supervisor_lease"):
			*destinations[0].(*bool) = self.leaseValid
		case strings.Contains(query, "FROM competition_automation_hold"):
			*destinations[0].(*bool) = self.held
		case strings.Contains(query, "FROM competition_source_pointer"):
			if self.pointer == 0 {
				return pgx.ErrNoRows
			}
			*destinations[0].(*int64) = self.pointer
		case strings.Contains(query, "FROM competition_source_activation"):
			if self.missing {
				return pgx.ErrNoRows
			}
			content, err := json.Marshal(self.intent)
			if err != nil {
				return err
			}
			*destinations[0].(*[]byte) = content
			if self.activated {
				now := time.Now()
				*destinations[1].(**time.Time) = &now
			}
			if self.abandoned {
				now := time.Now()
				*destinations[2].(**time.Time) = &now
			}
		default:
			return errors.New("unexpected watchdog statement")
		}
		return nil
	}}
}

// A held source lock is modeled throughout the real monitor's database path.
func TestCompetitionPreparationLivenessIgnoresHeldSourceScope(t *testing.T) {
	settings := &Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "local", SeasonId: "synthetic-season"}}
	intent := model.CompetitionSourceActivation{Environment: "local", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season", Generation: 1, State: "prepared", Commits: preparationLivenessTestCommits()}
	digest, err := competitionPreparationIntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.IntentSha256 = digest
	request := CompetitionPreparationRequest{Intent: intent, Lease: model.CompetitionSupervisorLease{Environment: "local", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season", OwnerId: "synthetic-owner", FencingToken: 3}}
	tx := &preparationLivenessTestTx{intent: intent, leaseValid: true}
	if err := captureDatabaseError(func() { automationScopeLockInTx(t.Context(), tx, settings) }); err == nil {
		t.Fatal("old full-authorization watchdog did not encounter held scope")
	}
	if err := checkCompetitionPreparationLivenessInTx(t.Context(), tx, settings, request); err != nil {
		t.Fatal("healthy authorization canceled by its independent watchdog", err)
	}
}

// Removing the unrelated scope lock must not weaken revocation checks.
func TestCompetitionPreparationLivenessPreservesRevocations(t *testing.T) {
	settings := &Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "local", SeasonId: "synthetic-season"}}
	intent := model.CompetitionSourceActivation{Environment: "local", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season", Generation: 1, State: "prepared", Commits: preparationLivenessTestCommits()}
	digest, err := competitionPreparationIntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.IntentSha256 = digest
	request := CompetitionPreparationRequest{Intent: intent, Lease: model.CompetitionSupervisorLease{Environment: "local", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season", OwnerId: "synthetic-owner", FencingToken: 3}}
	for _, failure := range []string{"lease", "hold", "missing", "abandoned", "activated", "digest", "pointer"} {
		tx := &preparationLivenessTestTx{intent: intent, leaseValid: failure != "lease", held: failure == "hold", missing: failure == "missing", abandoned: failure == "abandoned", activated: failure == "activated"}
		if failure == "digest" {
			tx.intent.Reason = "changed"
		}
		if failure == "pointer" {
			tx.pointer = 2
		}
		if err := checkCompetitionPreparationLivenessInTx(t.Context(), tx, settings, request); err == nil {
			t.Error("revoked observation accepted", failure)
		}
	}
}
