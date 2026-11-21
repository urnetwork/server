package model

// Who holds an email or phone, and when a hold on it yields.
//
// An account holds an email or phone in three ways:
//   - a network_user_auth_password row, verified (a code sent to the address
//     proved it) or not;
//   - an Apple or Google sign-in row with that email (the provider verified
//     it);
//   - network_user.user_auth, the identity the account was created with.
//
// A hold is pending when it rests on an unverified password row alone: an email
// or phone sign-up nobody verified (the row, and the legacy column of the
// sign-up), or an email or phone added to an existing account and not verified
// yet. Anyone can make such a hold with someone else's address, so it grants
// nothing: it never signs in or joins an Apple or Google sign-in. And it
// yields:
//   - at once, to a proof of the address. An Apple or Google sign-up or link
//     with the email releases it. A verification code sent to the address
//     releases it too, or hands a sign-up to the verifier with every earlier
//     credential ended (auth_verify_claim.go);
//   - to another claim (a sign-up, an address added to another account) once
//     it is older than UnverifiedUserAuthClaimTimeout, so an unverified sign-up
//     cannot hold someone else's address for good.
//
// A hold that yields is released: its row is deleted and a sign-up's legacy
// column is cleared, so the address is free. A hold that does not yield
// refuses the claim, as a held address always did.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// How long a pending hold keeps another claim on its address out. A person who
// signs up checks their mail within minutes or hours; after this, a sign-up of
// the same address by its owner, an add to another account, or a password
// reset of an abandoned sign-up goes ahead.
const UnverifiedUserAuthClaimTimeout = 72 * time.Hour

// One account's hold on an email or phone.
type userAuthHold struct {
	userId server.Id
	// the account has a network_user_auth_password row for the address
	passwordRow        bool
	passwordVerified   bool
	passwordCreateTime time.Time
	// the account has an Apple or Google sign-in with the address
	sso bool
	// network_user.user_auth is the address
	legacy bool
	// network_user.auth_type
	createAuthType string
}

func isSsoAuthType(authType string) bool {
	return authType == AuthTypeApple || authType == AuthTypeGoogle
}

// The account was created with the address by email or phone: the legacy
// column holds it, and the account was not created by Apple or Google.
func (self *userAuthHold) signUp() bool {
	return self.legacy && !isSsoAuthType(self.createAuthType)
}

// The account holds the address by a proof: a verified sign-in, an Apple or
// Google sign-in with the email, or an account created by Apple or Google with
// it.
func (self *userAuthHold) proven() bool {
	return self.sso ||
		(self.passwordRow && self.passwordVerified) ||
		(self.legacy && isSsoAuthType(self.createAuthType))
}

// The hold rests on an unverified sign-in alone. An address the legacy column
// holds without a row (a sign-in its account removed) is not pending: that
// column cannot tell whether it was ever verified, so it keeps refusing others.
func (self *userAuthHold) pending() bool {
	return self.passwordRow && !self.proven()
}

func (self *userAuthHold) stale(now time.Time) bool {
	return self.pending() && !now.Before(self.passwordCreateTime.Add(UnverifiedUserAuthClaimTimeout))
}

// Whether the hold yields to a claim on its address: to a proof at once, to
// another claim once it is stale.
func (self *userAuthHold) yields(proof bool, now time.Time) bool {
	return self.pending() && (proof || self.stale(now))
}

// Reads every account's hold on userAuth, a normalized email or phone, in the
// caller's transaction. The rows are locked, so a concurrent claim of the same
// address waits for this one.
func userAuthHoldsInTx(ctx context.Context, tx server.PgTx, userAuth string) []*userAuthHold {
	holds := []*userAuthHold{}
	holdOf := func(userId server.Id) *userAuthHold {
		for _, hold := range holds {
			if hold.userId == userId {
				return hold
			}
		}
		hold := &userAuthHold{userId: userId}
		holds = append(holds, hold)
		return hold
	}

	// the account rows first, then their sign-in rows, the order a
	// verification locks them in (authVerifyHoldInTx)
	result, err := tx.Query(
		ctx,
		`
			SELECT user_id, auth_type
			FROM network_user
			WHERE user_auth = $1
			FOR UPDATE
		`,
		userAuth,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userId server.Id
			var authType string
			server.Raise(result.Scan(&userId, &authType))
			hold := holdOf(userId)
			hold.legacy = true
			hold.createAuthType = authType
		}
	})

	result, err = tx.Query(
		ctx,
		`
			SELECT user_id, verified, create_time
			FROM network_user_auth_password
			WHERE user_auth = $1
			ORDER BY create_time, user_id
			FOR UPDATE
		`,
		userAuth,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userId server.Id
			var verified bool
			var createTime time.Time
			server.Raise(result.Scan(&userId, &verified, &createTime))
			hold := holdOf(userId)
			hold.passwordRow = true
			hold.passwordVerified = verified
			hold.passwordCreateTime = createTime
		}
	})

	result, err = tx.Query(
		ctx,
		`
			SELECT user_id
			FROM network_user_auth_sso
			WHERE user_auth = $1
			ORDER BY create_time, user_id
			FOR UPDATE
		`,
		userAuth,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userId server.Id
			server.Raise(result.Scan(&userId))
			holdOf(userId).sso = true
		}
	})

	return holds
}

// The holds of other accounts that a claim on userAuth by userId releases, in
// the caller's transaction, or the refusal when one of them does not yield.
// proof is whether the claim proves the address (an Apple or Google sign-in
// that verified the email). Nothing is written: the caller releases the holds
// (releaseUserAuthHoldsInTx) once nothing else refuses its change.
func userAuthClaimInTx(
	ctx context.Context,
	tx server.PgTx,
	userAuth string,
	userId server.Id,
	proof bool,
) ([]*userAuthHold, error) {
	now := server.NowUtc()
	released := []*userAuthHold{}
	for _, hold := range userAuthHoldsInTx(ctx, tx, userAuth) {
		if hold.userId == userId {
			continue
		}
		if !hold.yields(proof, now) {
			return nil, fmt.Errorf("user_auth %s already exists for a different user", userAuth)
		}
		released = append(released, hold)
	}
	return released, nil
}

// Claims userAuth for userId in the caller's transaction: releases the holds
// of other accounts that yield to the claim, or refuses it with no change.
func claimUserAuthInTx(
	ctx context.Context,
	tx server.PgTx,
	userAuth string,
	userId server.Id,
	proof bool,
) error {
	released, err := userAuthClaimInTx(ctx, tx, userAuth, userId, proof)
	if err != nil {
		return err
	}
	releaseUserAuthHoldsInTx(ctx, tx, userAuth, released)
	return nil
}

func releaseUserAuthHoldsInTx(ctx context.Context, tx server.PgTx, userAuth string, holds []*userAuthHold) {
	for _, hold := range holds {
		releaseUserAuthHoldInTx(ctx, tx, userAuth, hold)
	}
}

// Releases a pending hold on userAuth in the caller's transaction: its
// unverified row is deleted, and a sign-up's legacy column is cleared, so the
// address no longer names the account anywhere. An account left with another
// sign-in method owes its admin the sign-in notice of the removal; a sign-up
// that was never verified is left with none, and has no one to tell.
func releaseUserAuthHoldInTx(ctx context.Context, tx server.PgTx, userAuth string, hold *userAuthHold) {
	watch := watchSignInMethodsInTx(ctx, tx, hold.userId)
	_, userAuthType := NormalUserAuth(userAuth)

	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_user_auth_password
			WHERE user_id = $1 AND user_auth = $2 AND NOT verified
		`,
		hold.userId,
		userAuth,
	))
	if hold.signUp() {
		// the sign-up's legacy copies of the address and of its password
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_user
				SET
					user_auth = NULL,
					password_hash = NULL,
					password_salt = NULL
				WHERE user_id = $1 AND user_auth = $2
			`,
			hold.userId,
			userAuth,
		))
	}

	if userHasSignInMethodInTx(ctx, tx, hold.userId) {
		method := SecuritySignInMethodEmail
		if userAuthType == UserAuthTypePhone {
			method = SecuritySignInMethodPhone
		}
		// made by no request of the account's own, so no way it was made
		watch.noticeInTx(ctx, tx, &SecuritySignInChange{
			ChangeTime: server.NowUtc(),
			Action:     SecuritySignInRemoved,
			Method:     method,
			Identity:   maskUserAuth(userAuth),
		})
	}

	glog.Infof("[auth]released an unverified %s sign-in of user %s\n", userAuthType, hold.userId)
}

// Whether the user has any sign-in method left, read in the caller's
// transaction.
func userHasSignInMethodInTx(ctx context.Context, tx server.PgTx, userId server.Id) (has bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT
				EXISTS (SELECT 1 FROM network_user_auth_password WHERE user_id = $1) OR
				EXISTS (SELECT 1 FROM network_user_auth_sso WHERE user_id = $1) OR
				EXISTS (SELECT 1 FROM network_user_auth_wallet WHERE user_id = $1) OR
				EXISTS (SELECT 1 FROM network_user_auth_seedphrase WHERE user_id = $1)
		`,
		userId,
	).Scan(&has))
	return
}
