package controller

// A network name change or claim checks the name and writes it in one
// transaction, and a write that meets another network's name on the unique
// index is refused the way a taken name is. The check used to run before the
// write's transaction, so a name another network took in between ended in a
// unique violation: server.Tx reran the write once, the rerun repeated the
// violation, and the call ended in a 500.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Another network takes the name and commits between the availability check
// and the write (changeNetworkNameBeforeWrite). The change or claim is refused
// as a taken name, and nothing it would have written remains: the network
// keeps its name and its old name gets no reclaim cooldown.
func TestChangeNetworkNameRefusesNameTakenBeforeWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer func() {
			changeNetworkNameBeforeWrite = nil
		}()

		for _, c := range []struct {
			name   string
			change func(args ChangeNetworkNameArgs, clientSession *session.ClientSession) (*ChangeNetworkNameResult, error)
		}{
			{
				name:   "change",
				change: ChangeNetworkName,
			},
			{
				name:   "claim",
				change: ClaimNetworkName,
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			oldName := fmt.Sprintf("%s-race-network", c.name)
			model.Testing_CreateNetwork(ctx, networkId, oldName, userId)
			otherNetworkId := server.NewId()
			otherUserId := server.NewId()
			model.Testing_CreateNetwork(ctx, otherNetworkId, fmt.Sprintf("%s-race-other", c.name), otherUserId)
			takenName := fmt.Sprintf("%s-race-taken", c.name)

			hookCount := 0
			changeNetworkNameBeforeWrite = func() {
				hookCount += 1
				if hookCount != 1 {
					return
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`UPDATE network SET network_name = $2 WHERE network_id = $1`,
						otherNetworkId,
						takenName,
					))
				})
			}

			var result *ChangeNetworkNameResult
			var err error
			panicValue := func() (panicValue any) {
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				defer func() {
					panicValue = recover()
				}()
				result, err = c.change(
					ChangeNetworkNameArgs{
						NetworkName: takenName,
					},
					session.Testing_CreateClientSession(callCtx, &jwt.ByJwt{
						NetworkId: networkId,
						UserId:    userId,
					}),
				)
				return
			}()
			changeNetworkNameBeforeWrite = nil

			if panicValue != nil || err != nil {
				t.Errorf("%s: a name taken before the write ended with %v (panic %v), want the refusal", c.name, err, panicValue)
				continue
			}
			if hookCount != 1 {
				t.Errorf("%s: the hook between the check and the write ran %d times, want 1", c.name, hookCount)
			}
			if result == nil || result.Error == nil || result.Error.Message != "Network name not available." {
				t.Errorf("%s: a name taken before the write answered %+v, want the refusal", c.name, result)
			}
			network := model.GetNetwork(session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
				NetworkId: networkId,
				UserId:    userId,
			}))
			if network == nil || network.NetworkName != oldName {
				t.Errorf("%s: the refused write left the network as %+v, want it named %q", c.name, network, oldName)
			}
			otherNetwork := model.GetNetwork(session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
				NetworkId: otherNetworkId,
				UserId:    otherUserId,
			}))
			if otherNetwork == nil || otherNetwork.NetworkName != takenName {
				t.Errorf("%s: the other network is %+v, want it named %q", c.name, otherNetwork, takenName)
			}
			reclaimed := false
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT EXISTS (SELECT 1 FROM network_name_reclaim WHERE old_name = $1)`,
					oldName,
				).Scan(&reclaimed))
			})
			if reclaimed {
				t.Errorf("%s: the refused write left a reclaim cooldown on %q", c.name, oldName)
			}
		}
	})
}
