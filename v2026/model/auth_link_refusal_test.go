package model

// Linking a wallet or a sign-in identity that another account already holds
// is refused at once, with the refusal the client can act on. The conflicts
// below used to reach a unique index: the failed statement aborted the
// transaction, its commit rolled back, server.Tx retried that for a minute,
// and the call ended in a 500 without the refusal. Each call runs under
// forcedFailureCallTimeout, far inside that minute (see
// forced_statement_failure_test.go).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// A legacy account can hold a wallet on network_user alone (set before
// network_user_auth_wallet existed). Another account linking that wallet is
// refused like any other wallet the first account holds, and nothing is
// written.
func TestAddWalletAuthRefusesLegacyWalletHolderAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		holderUserId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "legacy-wallet-holder", holderUserId)
		takerUserId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "legacy-wallet-taker", takerUserId)

		signer := newSolanaAcceptanceWalletSigner(t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_user SET wallet_address = $2, wallet_blockchain = $3 WHERE user_id = $1`,
				holderUserId,
				signer.address,
				SOL.String(),
			))
		})
		walletAuth := signedAcceptanceWalletChallenge(t, ctx, signer)

		var err error
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			err = addWalletAuth(&AddWalletAuthArgs{
				WalletAuth: walletAuth,
				UserId:     takerUserId,
			}, callCtx)
		})
		if panicValue != nil || err == nil || !strings.Contains(err.Error(), "already linked to another account") {
			t.Fatalf("linking a legacy-held wallet ended with %v (panic %v), want the refusal", err, panicValue)
		}

		walletAuths, getErr := getWalletAuthsByAddress(ctx, signer.address)
		if getErr != nil || len(walletAuths) != 0 {
			t.Fatalf("the refused link wrote %d wallet auth rows (%v), want none", len(walletAuths), getErr)
		}
	})
}

// The same sign-in identity can be held under two auth types: this user holds
// it as apple, another user as google. This user adding it as google is
// refused for the other user's row, which the availability check used to miss
// when this user's own row came first.
func TestAddSsoAuthRefusesIdentityAnotherUserHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "sso-identity-user", userId)
		otherUserId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "sso-identity-other", otherUserId)

		userAuth := "sso-identity@example.com"
		server.Tx(ctx, func(tx server.PgTx) {
			// this user's row first
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_user_auth_sso (user_id, auth_type, user_auth, auth_jwt) VALUES ($1, $2, $3, 'synthetic-jwt')`,
				userId,
				AuthTypeApple,
				userAuth,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_user_auth_sso (user_id, auth_type, user_auth, auth_jwt) VALUES ($1, $2, $3, 'synthetic-jwt')`,
				otherUserId,
				AuthTypeGoogle,
				userAuth,
			))
		})

		var err error
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			err = addSsoAuth(&AddSsoAuthArgs{
				ParsedAuthJwt: AuthJwt{
					AuthType: AuthTypeGoogle,
					UserAuth: userAuth,
					UserName: "sso-identity",
				},
				AuthJwt:     "synthetic-jwt",
				AuthJwtType: SsoAuthTypeGoogle,
				UserId:      userId,
			}, callCtx)
		})
		if panicValue != nil || err == nil || !strings.Contains(err.Error(), "already exists for a different user") {
			t.Fatalf("adding another user's identity ended with %v (panic %v), want the refusal", err, panicValue)
		}

		ssoAuths, getErr := getSsoAuths(ctx, userId)
		if getErr != nil || len(ssoAuths) != 1 || ssoAuths[0].AuthType != SsoAuthTypeApple {
			t.Fatalf("this user's sso auths after the refusal = %v (%v), want only its apple row", ssoAuths, getErr)
		}
	})
}

// A failed insert of the sso row raises with its own error. It used to read
// as "already exists", because the affected row count of a failed insert is
// zero, and the commit then rolled back.
func TestAddSsoAuthRaisesFailedInsert(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, server.NewId(), "sso-failed-insert", userId)

		restore := forceStatementFailures(ctx, "network_user_auth_sso", "INSERT")
		var err error
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			err = addSsoAuth(&AddSsoAuthArgs{
				ParsedAuthJwt: AuthJwt{
					AuthType: AuthTypeGoogle,
					UserAuth: "sso-failed-insert@example.com",
					UserName: "sso-failed-insert",
				},
				AuthJwt:     "synthetic-jwt",
				AuthJwtType: SsoAuthTypeGoogle,
				UserId:      userId,
			}, callCtx)
		})
		restore()
		panicErr, _ := panicValue.(error)
		if err != nil || !isForcedFailure(panicValue, "P0001", "injected failure on INSERT network_user_auth_sso") || errors.Is(panicErr, pgx.ErrTxCommitRollback) {
			t.Fatalf("a failed sso insert ended with %v (panic %v), want the insert's own failure raised", err, panicValue)
		}
	})
}
