//go:build sidecar_pending

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// POST /network/member-remove's last Owner refusal (network_member_remove_controller.go) against an account
// deletion (§7.5's last Owner, owner decision 329), which needs MG6: the removal's count locks the team's Owners in
// the order the deletion does (model.CountNetworkOwnersInTx), so a removal and a deletion racing on a team's two
// Owners leave one.

// Starts a database interleaving, reporting a panic as its error through a buffered channel.
func runLastOwnerRouteTest(run func() error) <-chan error {
	done := make(chan error, 1)
	go func() {
		var returnErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				returnErr = fmt.Errorf("database interleaving panic: %v", recovered)
			}
			done <- returnErr
		}()
		returnErr = run()
	}()
	return done
}

// Bounds the wait for one interleaving to end.
func awaitLastOwnerRouteTest(done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		return errors.New("database interleaving did not finish")
	}
}

// Waits until a backend waits on a lock that blockerPid holds, and returns that backend's pid.
func waitForLastOwnerRouteLockWaiter(ctx context.Context, blockerPid int32) (waiterPid int32, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		found := false
		if r := server.HandleError(func() {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
						SELECT pid
						FROM pg_stat_activity
						WHERE
							datname = current_database() AND
							pid <> pg_backend_pid() AND
							wait_event_type = 'Lock' AND
							$1::integer = ANY(pg_blocking_pids(pid))
						ORDER BY pid
						LIMIT 1
					`,
					blockerPid,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&waiterPid))
						found = true
					}
				})
			})
		}); r != nil {
			return 0, fmt.Errorf("observe the waiters of %d: %v", blockerPid, r)
		}
		if found {
			return waiterPid, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("no backend waited on a lock of %d: %w", blockerPid, ctx.Err())
		case <-ticker.C:
		}
	}
}

// A team's two Owners: the first deletes its account while the second leaves the team. The deletion, past its last
// Owner check and holding the team's Owners, is held at its first delete; the removal's count of Owners waits on
// those locks, and once the deletion commits it counts the second Owner alone and refuses, so the team keeps an
// Owner. Before the count locked, it counted both Owners on its snapshot and the removal left the team ownerless.
func TestMemberRemoveWaitsForAnAccountDeletionOfTheOtherOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		first := newLastOwnerRoutePerson(t, ctx, "remove-race-first")
		second := newLastOwnerRoutePerson(t, ctx, "remove-race-second")
		teamNetworkId, teamName := first.createTeam(t, "remove-race-team")
		second.join(t, ctx, teamNetworkId, teamName, model.NetworkMemberRoleOwner)
		removals := &networkMemberRemovals{
			now:         server.NowUtc,
			requireTier: func(ctx context.Context, networkId server.Id) error { return nil },
		}

		// the holder keeps the first owner's user row, which its deletion
		// deletes first, after its last Owner check
		holderPids := make(chan int32, 1)
		releaseHolder := make(chan struct{})
		holderDone := runLastOwnerRouteTest(func() error {
			var returnErr error
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM network_user WHERE user_id = $1 FOR UPDATE`, first.userId))
				var pid int32
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
				holderPids <- pid
				select {
				case <-releaseHolder:
				case <-ctx.Done():
					returnErr = ctx.Err()
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			return returnErr
		})
		var holderPid int32
		select {
		case holderPid = <-holderPids:
		case <-ctx.Done():
			t.Fatal("the holder did not lock")
		}

		var removal *model.RemoveNetworkResult
		deleteDone := runLastOwnerRouteTest(func() error {
			removal = model.RemoveNetworkWithStoreSnapshot(ctx, first.networkId, &first.userId, nil)
			return nil
		})
		deletePid, waitErr := waitForLastOwnerRouteLockWaiter(ctx, holderPid)
		var answer *RemoveNetworkMemberResult
		var removeErr error
		var removeDone <-chan error
		if waitErr == nil {
			removeDone = runLastOwnerRouteTest(func() error {
				answer, removeErr = removals.remove(&RemoveNetworkMemberArgs{MemberId: second.member.MemberId}, second.teamSession)
				return nil
			})
			// the removal waits on the Owners the deletion holds
			_, waitErr = waitForLastOwnerRouteLockWaiter(ctx, deletePid)
		}
		if waitErr != nil {
			cancel()
		}
		close(releaseHolder)
		holderErr := awaitLastOwnerRouteTest(holderDone)
		deleteErr := awaitLastOwnerRouteTest(deleteDone)
		var removeDoneErr error
		if removeDone != nil {
			removeDoneErr = awaitLastOwnerRouteTest(removeDone)
		}
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if holderErr != nil || deleteErr != nil || removeDoneErr != nil {
			t.Fatalf("holder %v, deletion %v, removal %v", holderErr, deleteErr, removeDoneErr)
		}

		if removal.Outcome != model.RemoveNetworkRemoved {
			t.Fatalf("the deletion = %+v, want removed", removal)
		}
		if removeErr != nil || answer == nil || answer.Error == nil || answer.Error.Message != networkMemberRemoveLastOwnerMessage || answer.MemberId != nil {
			t.Fatalf("the removal answered %+v %v, want the last Owner's refusal", answer, removeErr)
		}
		if owners := lastOwnerRouteOwners(ctx, teamNetworkId); !slices.Equal(owners, []server.Id{second.userId}) {
			t.Fatalf("the team's Owners are %v, want the second owner alone", owners)
		}
	})
}
