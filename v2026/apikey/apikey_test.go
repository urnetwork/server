package apikey_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/apikey"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestFetchNetworkByApiKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "testnetwork"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		clientId := server.NewId()
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		// create some api keys
		key, err := apikey.Testing_CreateApiKey(networkId, ctx)
		connect.AssertEqual(t, err, nil)

		// fetch api key
		network := apikey.GetNetworkByApiKey(key.ApiKey, ctx)
		connect.AssertNotEqual(t, network, nil)
		connect.AssertEqual(t, network.NetworkId, networkId)
		connect.AssertEqual(t, network.UserId, userId)
		// connect.AssertEqual(t, key1.ApiKeyId, key1Result.Id)

		err = model.DeleteApiKey(&key.Id, userSession)
		connect.AssertEqual(t, err, nil)

		// attempt fetch deleted api key
		keyDeleted := apikey.GetNetworkByApiKey(key.ApiKey, ctx)
		connect.AssertEqual(t, keyDeleted, nil)
	})
}

// A failed insert of the test key surfaces at once with its own error. Its
// error used to be assigned to the result while the transaction went on to a
// commit that server.Tx retried for a minute; the call here has a deadline far
// inside that minute.
func TestTestingCreateApiKeyInsertFailureSurfacesAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "test", server.NewId())

		// every insert into account_api_key fails, in this test's own database
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				CREATE FUNCTION api_key_test_fail_insert() RETURNS trigger
				LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION 'injected failure on % %', TG_OP, TG_TABLE_NAME;
				END
				$$
				`,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				CREATE TRIGGER api_key_test_fail_insert
				BEFORE INSERT ON account_api_key
				FOR EACH ROW EXECUTE FUNCTION api_key_test_fail_insert()
				`,
			))
		})

		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		panicValue := func() (panicValue any) {
			defer func() {
				panicValue = recover()
			}()
			apikey.Testing_CreateApiKey(networkId, callCtx)
			return
		}()
		var pgErr *pgconn.PgError
		if panicErr, ok := panicValue.(error); !ok || !errors.As(panicErr, &pgErr) || pgErr.Message != "injected failure on INSERT account_api_key" {
			t.Fatalf("the failed insert ended with %v, want the injected failure", panicValue)
		}
	})
}
