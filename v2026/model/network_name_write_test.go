package model

// A network name is checked and written in one transaction, and a write that
// meets another network's name on the unique index is refused the way a taken
// name is. The checks used to run before the write's own transaction, so a
// name another network took in between ended in a unique violation: server.Tx
// reran the write once, the rerun repeated the violation, and the call ended
// in a 500. Each call runs under forcedFailureCallTimeout (see
// forced_statement_failure_test.go).

import (
	"context"
	"net/http"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

// The stored name of the network.
func networkNameOf(ctx context.Context, networkId server.Id) string {
	var networkName string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT network_name FROM network WHERE network_id = $1`,
			networkId,
		).Scan(&networkName))
	})
	return networkName
}

// Another network takes the name and commits between the update's
// availability check and its write (networkUpdateBeforeWrite). The update is
// refused as a taken name and keeps the network's name.
func TestNetworkUpdateRefusesNameTakenBeforeWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "update-race-network", userId)
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "update-race-other", server.NewId())
		byJwt := &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		}

		takenName := "update-race-taken"
		hookCount := 0
		networkUpdateBeforeWrite = func() {
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
		defer func() {
			networkUpdateBeforeWrite = nil
		}()

		var result *NetworkUpdateResult
		var err error
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			result, err = NetworkUpdate(
				NetworkUpdateArgs{
					NetworkName: takenName,
				},
				session.Testing_CreateClientSession(callCtx, byJwt),
			)
		})
		if panicValue != nil || err != nil {
			t.Fatalf("a name taken before the write ended with %v (panic %v), want the refusal", err, panicValue)
		}
		if hookCount != 1 {
			t.Fatalf("the hook between the check and the write ran %d times, want 1", hookCount)
		}
		if result == nil || result.Error == nil || result.Error.Message != "Network name not available" {
			t.Fatalf("a name taken before the write answered %+v, want the refusal", result)
		}
		if networkName := networkNameOf(ctx, networkId); networkName != "update-race-network" {
			t.Fatalf("the refused update left the network named %q", networkName)
		}
		if networkName := networkNameOf(ctx, otherNetworkId); networkName != takenName {
			t.Fatalf("the other network is named %q, want %q", networkName, takenName)
		}
	})
}

// The update stores the validated name, as network creation does. It checked
// the validated name but stored the request's text, so a name with capitals
// or spaces was stored as sent, beside the normalized names the checks and the
// unique index compare.
func TestNetworkUpdateStoresValidatedName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "validated-name-before", userId)

		result, err := NetworkUpdate(
			NetworkUpdateArgs{
				NetworkName: " Validated Name After ",
			},
			session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
				NetworkId: networkId,
				UserId:    userId,
			}),
		)
		if err != nil || result == nil || result.Error != nil {
			t.Fatalf("the update answered %+v, %v", result, err)
		}
		if networkName := networkNameOf(ctx, networkId); networkName != "validated-name-after" {
			t.Fatalf("the update stored %q, want the validated name", networkName)
		}
	})
}

// Another network holds the name when a create's transaction starts, having
// taken it after NetworkCreate's own check (these internal create functions do
// not repeat that check). Each create path refuses it as a taken name and
// writes nothing. The password path compared the request's text rather than
// the validated name, and the sign-in and wallet paths did not check in their
// transaction at all, so the insert met the unique index, server.Tx reran it,
// and the rerun repeated the violation.
func TestNetworkCreateRefusesNameTakenInTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		takenName := "create-race-taken"
		Testing_CreateNetwork(ctx, server.NewId(), takenName, server.NewId())
		// the request's text, which validates to the taken name
		requestName := "Create-Race-Taken"
		password := "synthetic-password-1"
		passwordUserAuth := "create-race-password@example.com"
		signInUserAuth := "create-race-sign-in@example.com"
		authJwt := "synthetic-jwt"
		authJwtType := SsoAuthTypeGoogle
		signer := newSolanaAcceptanceWalletSigner(t)
		walletAuth := signedAcceptanceWalletChallenge(t, ctx, signer)

		for _, c := range []struct {
			name   string
			create func(callCtx context.Context) networkCreateResult
			// counts the user rows the create would have written
			writtenSql string
			writtenArg string
		}{
			{
				name: "password",
				create: func(callCtx context.Context) networkCreateResult {
					return networkCreateUserAuth(
						callCtx,
						&NetworkCreateArgs{
							UserName:    "create-race",
							UserAuth:    &passwordUserAuth,
							Password:    &password,
							NetworkName: requestName,
							Terms:       true,
						},
						&passwordUserAuth,
						takenName,
						false,
						false,
					)
				},
				writtenSql: `SELECT COUNT(*) FROM network_user WHERE user_auth = $1`,
				writtenArg: passwordUserAuth,
			},
			{
				name: "sign-in",
				create: func(callCtx context.Context) networkCreateResult {
					return networkCreateAuthJwt(
						callCtx,
						&NetworkCreateArgs{
							UserName:    "create-race",
							AuthJwt:     &authJwt,
							AuthJwtType: &authJwtType,
							NetworkName: requestName,
							Terms:       true,
						},
						false,
						AuthJwt{
							AuthType: AuthTypeGoogle,
							UserAuth: signInUserAuth,
							UserName: "create-race",
						},
						takenName,
						signInUserAuth,
					)
				},
				writtenSql: `SELECT COUNT(*) FROM network_user WHERE user_auth = $1`,
				writtenArg: signInUserAuth,
			},
			{
				name: "wallet",
				create: func(callCtx context.Context) networkCreateResult {
					result, err := networkCreateWalletAuth(
						callCtx,
						&NetworkCreateArgs{
							UserName:    "create-race",
							WalletAuth:  walletAuth,
							NetworkName: requestName,
							Terms:       true,
						},
						takenName,
						false,
					)
					if err != nil {
						t.Errorf("wallet: the wallet auth was refused: %v", err)
					}
					return result
				},
				writtenSql: `SELECT COUNT(*) FROM network_user WHERE wallet_address = $1`,
				writtenArg: signer.address,
			},
		} {
			var result networkCreateResult
			panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
				result = c.create(callCtx)
			})
			if panicValue != nil {
				t.Errorf("%s: a taken name ended with %v, want the refusal", c.name, panicValue)
				continue
			}
			if result.Created || result.refusalStatus != http.StatusConflict || result.refusalMessage != "Network name not available" {
				t.Errorf(
					"%s: a taken name answered created %t, status %d, message %q, want the 409 name refusal",
					c.name,
					result.Created,
					result.refusalStatus,
					result.refusalMessage,
				)
			}
			if count := countRows(ctx, c.writtenSql, c.writtenArg); count != 0 {
				t.Errorf("%s: the refused create wrote %d user rows", c.name, count)
			}
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network WHERE network_name = $1`, takenName); count != 1 {
			t.Fatalf("%d networks hold the taken name, want 1", count)
		}
	})
}
