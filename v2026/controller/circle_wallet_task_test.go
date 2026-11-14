// Wallet backfill must not acknowledge failed users or outlive cancellation.
package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Both failures stay discoverable while the healthy middle user is processed.
// The old log-only path incorrectly returned success for this exact batch.
func TestPopulateAccountWalletsRetainsPartialFailures(t *testing.T) {
	owner := session.NewLocalClientSession(t.Context(), "", nil)
	defer owner.Cancel()
	users := []model.CircleUC{{UserId: server.NewId()}, {UserId: server.NewId()}, {UserId: server.NewId()}}
	first, last := errors.New("synthetic first wallet failure"), errors.New("synthetic last wallet failure")
	visited := []server.Id{}
	err := populateAccountWalletUsers(users, owner, 0, func(user model.CircleUC, actual *session.ClientSession) error {
		if actual != owner {
			t.Fatal("batch changed its task session")
		}
		visited = append(visited, user.UserId)
		switch user.UserId {
		case users[0].UserId:
			return first
		case users[2].UserId:
			return last
		default:
			return nil
		}
	})
	if !errors.Is(err, first) || !errors.Is(err, last) || !reflect.DeepEqual(visited, []server.Id{users[0].UserId, users[1].UserId, users[2].UserId}) {
		t.Fatal("partial batch was acknowledged or healthy user was skipped", err, visited)
	}
}

// Cancellation at the first user's barrier prevents any later user call and
// joins the original failure. No sleep decides whether the second call ran.
func TestPopulateAccountWalletsStopsAtCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	users := []model.CircleUC{{UserId: server.NewId()}, {UserId: server.NewId()}}
	failure := errors.New("synthetic wallet observation failure")
	calls := 0
	err := populateAccountWalletUsers(users, owner, 0, func(model.CircleUC, *session.ClientSession) error {
		calls++
		cancel()
		return failure
	})
	if calls != 1 || !errors.Is(err, failure) || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost the failure or started another user", calls, err)
	}
	if err := populateAccountWalletUsers(users, owner, time.Hour, func(model.CircleUC, *session.ClientSession) error {
		t.Fatal("canceled throttle started a user")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled batch did not retain cancellation", err)
	}
}

// The real entry point accepts a newly provisioned database without contacting
// Circle; an empty batch must not become a perpetually retrying failed task.
func TestPopulateAccountWalletsEmptyBatchSucceeds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		owner := session.NewLocalClientSession(t.Context(), "", nil)
		defer owner.Cancel()
		result, err := PopulateAccountWallets(&PopulateAccountWalletsArgs{}, owner)
		if err != nil || result == nil {
			t.Fatal("empty wallet backfill failed", result, err)
		}
	})
}
