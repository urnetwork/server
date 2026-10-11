package controller

// The dashboard's api key listing answers with the network's keys, and a
// listing that fails with its error message instead of a 500.

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A key row the listing cannot read is answered with "Error getting account
// api keys" and no keys, and the failure is logged. The model used to assign
// the scan error to a variable that shadowed its error result, so the failure
// escaped the listing as a panic (a 500) and this answer never ran.
func TestGetApiKeysAnswersUnreadableKeyWithError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "readable"}, userSession)
		if err != nil {
			t.Fatalf("could not create an api key: %s", err)
		}

		// a readable listing answers its keys
		result, err := GetApiKeys(userSession)
		if err != nil || result.Error != nil || len(result.ApiKeys) != 1 || result.ApiKeys[0].Id != created.Id {
			t.Fatalf("the readable listing answered %+v %v", result, err)
		}

		// a key whose null name cannot be scanned into a string
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`ALTER TABLE account_api_key ALTER COLUMN name DROP NOT NULL`,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO account_api_key (api_key_id, network_id, api_key, name)
				VALUES ($1, $2, $3, NULL)
				`,
				server.NewId(),
				networkId,
				"unreadable",
			))
		})

		result, err, panicValue := func() (listResult *GetApiKeysResult, listErr error, panicValue any) {
			defer func() {
				panicValue = recover()
			}()
			listResult, listErr = GetApiKeys(userSession)
			return
		}()
		if panicValue != nil {
			t.Fatalf("the failed listing escaped as a panic: %v", panicValue)
		}
		if err != nil {
			t.Fatalf("the failed listing failed the call: %s", err)
		}
		if result.Error == nil || result.Error.Message != "Error getting account api keys" || result.ApiKeys != nil {
			t.Fatalf("the failed listing answered %+v", result)
		}
	})
}
