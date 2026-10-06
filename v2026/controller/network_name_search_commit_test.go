package controller

// A name change or claim moves the network in the in-memory network name
// search only once its transaction has committed. It used to move it at the
// write, before the commit, so a transaction that then failed left this
// process's index on a name the database never kept, until a restart. The test
// uses an in-memory search that never loads or polls
// (model.Testing_InMemoryNetworkNameSearch), and a test trigger on the search's
// update log fails the transaction after the rename statement.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A change or claim whose transaction fails after the rename statement leaves
// the in-memory index on the old name; one that commits moves it to the new
// name.
func TestChangeNetworkNameIndexesNameInMemoryAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		inMemorySearch, restoreSearch := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restoreSearch()

		exec := func(sql string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql))
			})
		}
		// fails every insert into the search's update log until the returned
		// func removes the trigger
		failUpdateLogInserts := func() func() {
			exec(`
				CREATE OR REPLACE FUNCTION forced_statement_failure() RETURNS trigger
				LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION 'injected failure on % %', TG_OP, TG_TABLE_NAME;
				END
				$$
			`)
			exec(`
				CREATE TRIGGER forced_statement_failure
				BEFORE INSERT ON search_value_update
				FOR EACH ROW EXECUTE FUNCTION forced_statement_failure()
			`)
			return func() {
				exec(`DROP TRIGGER forced_statement_failure ON search_value_update`)
			}
		}
		// whether the in-memory search holds the network under exactly the name
		found := func(networkName string, networkId server.Id) bool {
			_, ok := inMemorySearch.AroundIds(ctx, networkName, 0)[networkId]
			return ok
		}

		for _, c := range []struct {
			name     string
			change   func(args ChangeNetworkNameArgs, clientSession *session.ClientSession) (*ChangeNetworkNameResult, error)
			oldName  string
			newName  string
			rollback bool
		}{
			{
				name:     "change rolled back",
				change:   ChangeNetworkName,
				oldName:  "alpha-change-network",
				newName:  "bravo-change-renamed",
				rollback: true,
			},
			{
				name:     "change committed",
				change:   ChangeNetworkName,
				oldName:  "charlie-change-network",
				newName:  "delta-change-renamed",
				rollback: false,
			},
			{
				name:     "claim rolled back",
				change:   ClaimNetworkName,
				oldName:  "echo-claim-network",
				newName:  "foxtrot-claim-renamed",
				rollback: true,
			},
			{
				name:     "claim committed",
				change:   ClaimNetworkName,
				oldName:  "golf-claim-network",
				newName:  "hotel-claim-renamed",
				rollback: false,
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, c.oldName, userId)
			// indexed the way network create indexes it
			inMemorySearch.Add(ctx, c.oldName, networkId, 0)

			restore := func() {}
			if c.rollback {
				restore = failUpdateLogInserts()
			}
			var result *ChangeNetworkNameResult
			panicValue := func() (panicValue any) {
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				defer func() {
					panicValue = recover()
				}()
				result, _ = c.change(
					ChangeNetworkNameArgs{
						NetworkName: c.newName,
					},
					session.Testing_CreateClientSession(callCtx, &jwt.ByJwt{
						NetworkId: networkId,
						UserId:    userId,
					}),
				)
				return
			}()
			restore()

			if c.rollback {
				panicErr, _ := panicValue.(error)
				var pgErr *pgconn.PgError
				if !errors.As(panicErr, &pgErr) || pgErr.Code != "P0001" {
					t.Errorf("%s: the rename ended with %v, want the forced failure", c.name, panicValue)
				}
			} else if panicValue != nil || result == nil || result.Error != nil {
				t.Errorf("%s: the rename answered %+v (panic %v)", c.name, result, panicValue)
			}
			wantOld := c.rollback
			if old, renamed := found(c.oldName, networkId), found(c.newName, networkId); old != wantOld || renamed != !wantOld {
				t.Errorf("%s: the in-memory index holds the old name %t and the new name %t, want %t and %t", c.name, old, renamed, wantOld, !wantOld)
			}
		}
	})
}
