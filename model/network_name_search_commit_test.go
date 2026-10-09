package model

// The in-memory index of the network name search changes only once the
// transaction that renames or removes a network has committed. It used to
// change at the write, before the commit, so a transaction that then failed
// left this process's index on a name the database never kept: the search's
// poll replays only newer committed update records, and a rolled-back write
// leaves none, so the entry stayed wrong until a restart. Each test uses an
// in-memory search that never loads or polls (Testing_InMemoryNetworkNameSearch),
// and a test trigger on the search's update log fails the transaction after the
// rename or removal statement. Each call runs under forcedFailureCallTimeout
// (see forced_statement_failure_test.go).

import (
	"context"
	"testing"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Whether the in-memory network name search holds the network under exactly
// the name.
func networkNameInMemory(ctx context.Context, networkName string, networkId server.Id) bool {
	_, ok := networkNameSearch().AroundIds(ctx, networkName, 0)[networkId]
	return ok
}

// A rename whose transaction fails after the rename statement leaves the
// in-memory index on the old name; a rename that commits moves it to the new
// name.
func TestNetworkUpdateIndexesNameInMemoryAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restoreSearch := Testing_InMemoryNetworkNameSearch(ctx)
		defer restoreSearch()

		for _, c := range []struct {
			name     string
			oldName  string
			newName  string
			rollback bool
		}{
			{
				name:     "rolled back",
				oldName:  "update-rollback-before",
				newName:  "update-rollback-renamed",
				rollback: true,
			},
			{
				name:     "committed",
				oldName:  "update-commit-before",
				newName:  "update-commit-renamed",
				rollback: false,
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, c.oldName, userId)
			// indexed the way network create indexes it
			networkNameSearch().Add(ctx, c.oldName, networkId, 0)

			restore := func() {}
			if c.rollback {
				restore = forceStatementFailures(ctx, "search_value_update", "INSERT")
			}
			var result *NetworkUpdateResult
			panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
				result, _ = NetworkUpdate(
					NetworkUpdateArgs{
						NetworkName: c.newName,
					},
					session.Testing_CreateClientSession(callCtx, &session.ByJwt{
						NetworkId: networkId,
						UserId:    userId,
					}),
				)
			})
			restore()

			if c.rollback {
				if !isForcedFailure(panicValue, "P0001", "") {
					t.Errorf("%s: the rename ended with %v, want the forced failure", c.name, panicValue)
				}
				if networkName := networkNameOf(ctx, networkId); networkName != c.oldName {
					t.Errorf("%s: the network is named %q after the rollback, want %q", c.name, networkName, c.oldName)
				}
			} else if panicValue != nil || result == nil || result.Error != nil {
				t.Errorf("%s: the rename answered %+v (panic %v)", c.name, result, panicValue)
			}
			wantOld := c.rollback
			if old, renamed := networkNameInMemory(ctx, c.oldName, networkId), networkNameInMemory(ctx, c.newName, networkId); old != wantOld || renamed != !wantOld {
				t.Errorf("%s: the in-memory index holds the old name %t and the new name %t, want %t and %t", c.name, old, renamed, wantOld, !wantOld)
			}
		}
	})
}

// An account removal whose transaction fails after the removal statements
// leaves the network's name in the in-memory index; a removal that commits
// takes it out.
func TestRemoveNetworkUnindexesNameInMemoryAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restoreSearch := Testing_InMemoryNetworkNameSearch(ctx)
		defer restoreSearch()

		for _, c := range []struct {
			name        string
			networkName string
			rollback    bool
		}{
			{
				name:        "rolled back",
				networkName: "remove-rollback-network",
				rollback:    true,
			},
			{
				name:        "committed",
				networkName: "remove-commit-network",
				rollback:    false,
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, c.networkName, userId)
			networkNameSearch().Add(ctx, c.networkName, networkId, 0)

			restore := func() {}
			if c.rollback {
				restore = forceStatementFailures(ctx, "search_value_update", "INSERT")
			}
			var outcome RemoveNetworkOutcome
			panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
				outcome, _ = RemoveNetworkWithStoreSnapshot(callCtx, networkId, &userId, nil)
			})
			restore()

			networkCount := countRows(ctx, `SELECT COUNT(*) FROM network WHERE network_id = $1`, networkId)
			if c.rollback {
				if !isForcedFailure(panicValue, "P0001", "") || networkCount != 1 {
					t.Errorf("%s: the removal ended with %v and left %d networks, want the forced failure and the network", c.name, panicValue, networkCount)
				}
			} else if panicValue != nil || outcome != RemoveNetworkRemoved || networkCount != 0 {
				t.Errorf("%s: the removal answered %q (panic %v) and left %d networks", c.name, outcome, panicValue, networkCount)
			}
			if indexed := networkNameInMemory(ctx, c.networkName, networkId); indexed != c.rollback {
				t.Errorf("%s: the in-memory index holds the name %t, want %t", c.name, indexed, c.rollback)
			}
		}
	})
}
