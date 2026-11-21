package model

// What a verification code does to an account's email or phone (AuthVerify).
//
// A code proves control of the address it was sent to
// (auth_verify_code_binding.go), not of the account that holds the address. An
// address pending there (auth_user_auth_claim.go) was put there by whoever made
// the claim, and so was everything else the account had before it:
//   - A caller signed in to the account, or who proved the account's password
//     on this client network just before the code was sent, controls the
//     account; so does any caller when the account already holds the address
//     by an Apple or Google sign-in. The address is verified and nothing else
//     changes, except that a sign-up's own password, set by whoever signed up,
//     is retired unless this client proved it.
//   - An email or phone sign-up verified by anyone else is the verifier's from
//     then on: its earlier credentials stop working (its password, every
//     session, its sign-in codes and API keys, and every other sign-in
//     method), and the verifier is signed in. They set a password with a
//     password reset.
//   - An address added to another account never joins it. The claim is
//     released and the verifier told so, unless the code followed a password
//     sign-in on another network: that verifier signs in again.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type authVerifyOutcome int

const (
	// sign in: the address is verified already, or is an identity the account
	// holds without a sign-in row
	authVerifySignIn authVerifyOutcome = iota
	// verify the address, and keep its password
	authVerifyVerify
	// verify the address, and retire the password a sign-up set
	authVerifyVerifyRetiringPassword
	// verify the address of a sign-up and give the account to the verifier:
	// every earlier credential ends (takeOverAccountInTx)
	authVerifyTakeOver
	// release the address from an account it was added to, and refuse
	authVerifyRelease
	// refuse, and change nothing
	authVerifyRefuse
)

// What the caller proved besides the address.
type authVerifyProof struct {
	// the caller presented a live credential of the account
	signedIn bool
	// the code was sent right after this client network proved the account's
	// password
	password bool
	// the code was sent right after another client network proved the
	// account's password
	passwordElsewhere bool
}

// The outcome of a valid code for the address the hold describes.
// accountProven is whether the account holds another address by a proof (a
// verified email or phone, or an Apple or Google sign-in).
func decideAuthVerify(hold *userAuthHold, accountProven bool, proof authVerifyProof) authVerifyOutcome {
	if !hold.passwordRow || hold.passwordVerified {
		return authVerifySignIn
	}
	if proof.signedIn || proof.password || hold.proven() {
		if hold.signUp() && !proof.password {
			return authVerifyVerifyRetiringPassword
		}
		return authVerifyVerify
	}
	if hold.signUp() && !accountProven {
		// the account's first verified address
		return authVerifyTakeOver
	}
	if proof.passwordElsewhere {
		// most likely the account's own owner, signed in with its password on
		// another network: they sign in again to verify
		return authVerifyRefuse
	}
	return authVerifyRelease
}

// The refusal of an address added to an account, verified by someone who has
// not signed in to it.
const AuthVerifyReleasedMessage = "This email or phone was added to an account but never verified there, so it has been removed from that account. You can now sign up with it, or add it to your own account."

// The refusal of an address added to an account, verified with a code that
// followed a password sign-in on another network.
const AuthVerifySignInAgainMessage = "Sign in with your password again to verify this email or phone."

// The account's hold on the verified address, its other addresses, and
// whether it holds another address by a proof (decideAuthVerify), read in the
// caller's transaction. The account's rows are locked.
func authVerifyHoldInTx(ctx context.Context, tx server.PgTx, userId server.Id, userAuth string) (hold *userAuthHold, otherUserAuths []string, accountProven bool) {
	hold = &userAuthHold{userId: userId}

	var createAuthType string
	var legacyUserAuth *string
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT auth_type, user_auth FROM network_user WHERE user_id = $1 FOR UPDATE`,
		userId,
	).Scan(&createAuthType, &legacyUserAuth))
	hold.createAuthType = createAuthType
	if legacyUserAuth != nil {
		if normalLegacyUserAuth, _ := NormalUserAuth(*legacyUserAuth); normalLegacyUserAuth == userAuth {
			hold.legacy = true
		} else if normalLegacyUserAuth != "" {
			otherUserAuths = append(otherUserAuths, normalLegacyUserAuth)
		}
	}

	result, err := tx.Query(
		ctx,
		`
			SELECT user_auth, verified, create_time
			FROM network_user_auth_password
			WHERE user_id = $1
			FOR UPDATE
		`,
		userId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var rowUserAuth string
			var verified bool
			var createTime time.Time
			server.Raise(result.Scan(&rowUserAuth, &verified, &createTime))
			if rowUserAuth == userAuth {
				hold.passwordRow = true
				hold.passwordVerified = verified
				hold.passwordCreateTime = createTime
			} else {
				otherUserAuths = append(otherUserAuths, rowUserAuth)
				if verified {
					accountProven = true
				}
			}
		}
	})

	result, err = tx.Query(
		ctx,
		`SELECT user_auth FROM network_user_auth_sso WHERE user_id = $1`,
		userId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var ssoUserAuth string
			server.Raise(result.Scan(&ssoUserAuth))
			if ssoUserAuth == userAuth {
				hold.sso = true
			} else {
				accountProven = true
			}
		}
	})
	return
}

// Retires the password of the user's sign-in at userAuth, and the account's
// legacy copy of it, in the caller's transaction: no password signs in with it
// until one is set with a password reset.
func retirePasswordInTx(ctx context.Context, tx server.PgTx, userId server.Id, userAuth string) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_user_auth_password
			SET password_hash = NULL, password_salt = NULL
			WHERE user_id = $1 AND user_auth = $2
		`,
		userId,
		userAuth,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_user
			SET password_hash = NULL, password_salt = NULL
			WHERE user_id = $1
		`,
		userId,
	))
}

// Gives a sign-up, whose first address is verified now, to the person who
// verified it without proving they control the account, in the caller's
// transaction, which holds the network's session lifecycle lock exclusively.
// The account holds no address by a proof (decideAuthVerify), so nothing it
// has was set up by the address's owner. Every credential created before the
// returned cutoff is refused, as after a password reset; the account's sign-in
// codes and API keys end; and every other sign-in method (an unverified email
// or phone, a wallet, a seed phrase) is removed. The caller retires the
// address's password and marks it verified. The account's admin is owed the
// API key and sign-in notices, as for any such change.
func takeOverAccountInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	userId server.Id,
	userAuth string,
	operationId server.Id,
) (cutoff time.Time) {
	var credentialChangeTime time.Time
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT credential_change_time FROM network_user WHERE user_id = $1`,
		userId,
	).Scan(&credentialChangeTime))
	cutoff = networkSignOutCutoffInTx(ctx, tx, credentialChangeTime.UTC())

	// the password reset's rotation (AuthPasswordSet)
	server.Raise(session.JournalSessionRetirementInTx(ctx, tx, networkId, operationId, "reset", cutoff))
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE auth_code SET remaining_uses = 0 WHERE network_id = $1 AND create_time < $2 AND active`,
		networkId,
		cutoff,
	))
	server.Raise(session.RotateCredentialRecordsInTx(ctx, tx, userId, cutoff))
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`,
		userId,
		cutoff,
	))

	// an API key is a credential no rotation ends
	type deletedApiKey struct {
		name       string
		createTime time.Time
	}
	deletedApiKeys := []deletedApiKey{}
	result, err := tx.Query(
		ctx,
		`
			DELETE FROM account_api_key
			WHERE network_id = $1
			RETURNING COALESCE(name, ''), create_time
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var apiKey deletedApiKey
			server.Raise(result.Scan(&apiKey.name, &apiKey.createTime))
			deletedApiKeys = append(deletedApiKeys, apiKey)
		}
	})
	for _, apiKey := range deletedApiKeys {
		oweSecurityApiKeyNoticeInTx(ctx, tx, networkId, newSecurityApiKeyChange(ctx, SecurityApiKeyDeleted, apiKey.name, &apiKey.createTime))
	}

	// every other sign-in method
	watch := watchSignInMethodsInTx(ctx, tx, userId)
	changes := []*SecuritySignInChange{}
	result, err = tx.Query(
		ctx,
		`
			DELETE FROM network_user_auth_password
			WHERE user_id = $1 AND user_auth <> $2
			RETURNING auth_type, user_auth
		`,
		userId,
		userAuth,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var authType string
			var removedUserAuth string
			server.Raise(result.Scan(&authType, &removedUserAuth))
			changes = append(changes, newSecuritySignInChange(ctx, SecuritySignInRemoved, authType, maskUserAuth(removedUserAuth)))
		}
	})
	result, err = tx.Query(
		ctx,
		`
			DELETE FROM network_user_auth_wallet
			WHERE user_id = $1
			RETURNING blockchain, wallet_address
		`,
		userId,
	)
	walletRemoved := false
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var blockchain string
			var walletAddress string
			server.Raise(result.Scan(&blockchain, &walletAddress))
			changes = append(changes, newSecuritySignInChange(ctx, SecuritySignInRemoved, securityWalletSignInMethod(blockchain), MaskWalletAddress(walletAddress)))
			walletRemoved = true
		}
	})
	if walletRemoved {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_user SET wallet_address = NULL, wallet_blockchain = NULL WHERE user_id = $1`,
			userId,
		))
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM network_user_auth_seedphrase WHERE user_id = $1`,
		userId,
	))
	if 0 < tag.RowsAffected() {
		changes = append(changes, newSecuritySignInChange(ctx, SecuritySignInRemoved, SecuritySignInMethodSeedphrase, ""))
	}
	watch.noticeInTx(ctx, tx, changes...)
	return
}

// The caller's own credential of the account on a route that serves signed-out
// callers: a signed network or client token of networkId and userId, not an API
// key or a lag guest token, or nil. Its signature and audience are checked
// here; its state in the caller's transaction (authVerifySignedInTx).
func presentedAccountCredential(clientSession *session.ClientSession, networkId server.Id, userId server.Id) *session.ByJwt {
	token, ok := strings.CutPrefix(http.Header(clientSession.Header).Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil
	}
	presented, err := session.ParseByJwtForAudience(clientSession.Ctx, token, session.ByJwtAudienceApi)
	if err != nil || presented.NetworkId != networkId || presented.UserId != userId || presented.LagGuest {
		return nil
	}
	return presented
}

// Whether the presented credential is live, in the caller's transaction. A
// session store that cannot answer raises, as the verification's own mint
// does.
func authVerifySignedInTx(ctx context.Context, tx server.PgTx, presented *session.ByJwt) bool {
	if presented == nil {
		return false
	}
	err := session.ValidateByJwtStateInTx(ctx, tx, presented, false)
	if errors.Is(err, session.ErrAuthUnavailable) {
		server.Raise(err)
	}
	return err == nil
}

// Whether the code is still unused, read in the caller's transaction with its
// row locked, so concurrent verifications of one code use it once.
func verifyCodeUnusedInTx(ctx context.Context, tx server.PgTx, userAuthVerifyId server.Id) bool {
	var used bool
	err := tx.QueryRow(
		ctx,
		`SELECT used FROM user_auth_verify WHERE user_auth_verify_id = $1 FOR UPDATE`,
		userAuthVerifyId,
	).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	server.Raise(err)
	return !used
}

func useVerifyCodeInTx(ctx context.Context, tx server.PgTx, userAuthVerifyId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE user_auth_verify SET used = true WHERE user_auth_verify_id = $1`,
		userAuthVerifyId,
	))
}
