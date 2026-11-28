// A slow source validator must not own the supervisor's independent heartbeat.
package controller

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"strings"
	"testing"
	"time"
)

// Model a held season advisory lock as a deterministic refusal, not a timer.
type supervisorScopeRenewTestTx struct {
	server.PgTx
	lease                        model.CompetitionSupervisorLease
	scopeError                   error
	missing, invalid, zeroUpdate bool
	rowLocks, updates            int
}

// Any attempted season-lock acquisition reproduces contention immediately.
func (self *supervisorScopeRenewTestTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "pg_advisory") {
		return pgconn.CommandTag{}, self.scopeError
	}
	if strings.HasPrefix(query, "UPDATE competition_supervisor_lease") {
		self.updates++
		if len(args) != 7 || args[5] != self.lease.OwnerId || args[6] != self.lease.FencingToken {
			return pgconn.CommandTag{}, errors.New("wrong fenced update")
		}
		if self.zeroUpdate {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	if !strings.Contains(query, "set_config(") {
		return pgconn.CommandTag{}, errors.New("unexpected renewal statement")
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

// Preserve the real owner/fence row predicates and both authority reads.
func (self *supervisorScopeRenewTestTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	return supervisorScopeRenewTestRow{scan: func(destinations []any) error {
		if len(args) < 5 || args[3] != self.lease.OwnerId || args[4] != self.lease.FencingToken {
			return pgx.ErrNoRows
		}
		if strings.Contains(query, "FOR UPDATE") {
			self.rowLocks++
			if self.missing {
				return pgx.ErrNoRows
			}
			*destinations[0].(*time.Time) = self.lease.LeaseExpiresAt
			return nil
		}
		if !strings.Contains(query, "SELECT EXISTS") {
			return errors.New("unexpected renewal read")
		}
		*destinations[0].(*bool) = !self.invalid
		return nil
	}}
}

// The fake changes only database facts, not the real renewal implementation.
type supervisorScopeRenewTestRow struct{ scan func([]any) error }

// Scan is the sole point where a test row returns its database fact.
func (self supervisorScopeRenewTestRow) Scan(destinations ...any) error {
	return self.scan(destinations)
}

// Source scope may remain held throughout this entire successful renewal.
func TestCompetitionSupervisorRenewalDoesNotWaitForSourceScope(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	settings := &Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "local", SeasonId: "synthetic-season"}}
	lease := model.CompetitionSupervisorLease{Environment: "local", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season", OwnerId: "synthetic-owner", FencingToken: 7, LeaseExpiresAt: now.Add(time.Minute)}
	tx := &supervisorScopeRenewTestTx{lease: lease, scopeError: errors.New("synthetic source scope remains held")}
	store := PostgresStore{now: func() time.Time { return now.Add(time.Second) }}
	var result *model.CompetitionSupervisorLease
	err := captureDatabaseError(func() {
		result = store.renewAutomationLeaseInTx(t.Context(), tx, settings, lease, time.Minute, newCompetitionSupervisorRenewObservation())
	})
	if err != nil || result == nil || result.FencingToken != lease.FencingToken || !result.LeaseExpiresAt.After(lease.LeaseExpiresAt) || tx.rowLocks != 1 || tx.updates != 1 {
		t.Fatal("source validator blocked independent fenced renewal", result, err, tx.rowLocks, tx.updates)
	}
}

// Removing unrelated serialization cannot revive stale, expired or lost owners.
func TestCompetitionSupervisorScopeIndependentRenewalPreservesFences(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	settings := &Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "local", SeasonId: "synthetic-season"}}
	for _, failure := range []string{"missing", "expired", "invalid", "update-lost"} {
		lease := model.CompetitionSupervisorLease{Environment: "local", CompetitionId: settings.CompetitionId, SeasonId: "synthetic-season", OwnerId: "synthetic-owner", FencingToken: 7, LeaseExpiresAt: now.Add(time.Minute)}
		tx := &supervisorScopeRenewTestTx{lease: lease, scopeError: errors.New("synthetic source scope remains held"), missing: failure == "missing", invalid: failure == "invalid", zeroUpdate: failure == "update-lost"}
		observedNow := now
		if failure == "expired" {
			observedNow = lease.LeaseExpiresAt
		}
		store := PostgresStore{now: func() time.Time { return observedNow }}
		var result *model.CompetitionSupervisorLease
		err := captureDatabaseError(func() {
			result = store.renewAutomationLeaseInTx(t.Context(), tx, settings, lease, time.Minute, newCompetitionSupervisorRenewObservation())
		})
		if !errors.Is(err, ErrLeaseLost) || result != nil {
			t.Fatalf("%s owner regained authority: result=%v err=%v", failure, result, err)
		}
		if failure != "update-lost" && tx.updates != 0 {
			t.Fatalf("%s reached a lease update", failure)
		}
	}
}
