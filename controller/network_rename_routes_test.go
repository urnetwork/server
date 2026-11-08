package controller

// The three rename routes (§7.12) each release the old name to a 24-hour
// tombstone and refuse a name a tombstone holds. Before model.RenameNetwork only
// the change wrote one, so a claim or an update released its name at once.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestRenameRoutesHoldAndRefuseAReleasedName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()

		for _, c := range []struct {
			name   string
			rename func(clientSession *session.ClientSession, name string) (refusal string)
		}{
			{
				name: "change",
				rename: func(clientSession *session.ClientSession, name string) string {
					result, err := ChangeNetworkName(ChangeNetworkNameArgs{NetworkName: name}, clientSession)
					if err != nil {
						t.Fatal(err)
					}
					if result.Error != nil {
						return result.Error.Message
					}
					return ""
				},
			},
			{
				name: "claim",
				rename: func(clientSession *session.ClientSession, name string) string {
					result, err := ClaimNetworkName(ClaimNetworkNameArgs{NetworkName: name}, clientSession)
					if err != nil {
						t.Fatal(err)
					}
					if result.Error != nil {
						return result.Error.Message
					}
					return ""
				},
			},
			{
				name: "update",
				rename: func(clientSession *session.ClientSession, name string) string {
					result, err := UpdateNetworkName(&UpdateNetworkNameArgs{NetworkName: name}, clientSession)
					if err != nil {
						t.Fatal(err)
					}
					if result.Error != nil {
						return result.Error.Message
					}
					return ""
				},
			},
		} {
			firstName := fmt.Sprintf("released-%s-first", c.name)
			firstNetworkId := server.NewId()
			firstUserId := server.NewId()
			model.Testing_CreateNetwork(ctx, firstNetworkId, firstName, firstUserId)
			before := server.NowUtc()
			if refusal := c.rename(session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: firstNetworkId,
				UserId:    firstUserId,
			}), fmt.Sprintf("released-%s-moved", c.name)); refusal != "" {
				t.Fatalf("%s: the rename was refused: %s", c.name, refusal)
			}

			var coolDownUntil time.Time
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT cool_down_until FROM network_name_reclaim WHERE old_name = $1`, firstName)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&coolDownUntil))
					}
				})
			})
			if coolDownUntil.Before(before.Add(24*time.Hour)) || server.NowUtc().Add(24*time.Hour).Before(coolDownUntil) {
				t.Fatalf("%s: the released name is held until %s", c.name, coolDownUntil)
			}

			secondNetworkId := server.NewId()
			secondUserId := server.NewId()
			model.Testing_CreateNetwork(ctx, secondNetworkId, fmt.Sprintf("released-%s-second", c.name), secondUserId)
			if refusal := c.rename(session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: secondNetworkId,
				UserId:    secondUserId,
			}), firstName); refusal == "" {
				t.Fatalf("%s: another network took the held name", c.name)
			}
		}
	})
}
