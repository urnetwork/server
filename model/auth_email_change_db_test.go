package model

// The verified email change (auth_email_change.go, support inbox 33, 6446)
// against the test env. Before it, an account could not change its email at
// all: AddAuth refuses a second email sign-in, and the last sign-in method
// cannot be removed. Each test drives StartEmailChange and ConfirmEmailChange
// and reads back the sign-in rows, the notices and the codes.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/urnetwork/server"
)

// A network with a verified email sign-in (<network id>@bringyour.com), its
// admin, and its address.
func emailChangeTestNetwork(t testing.TB, ctx context.Context) (networkId server.Id, userId server.Id, userAuth string) {
	t.Helper()
	networkId = server.NewId()
	userId = server.NewId()
	userAuth = Testing_CreateNetwork(ctx, networkId, "emailchange-"+networkId.String(), userId)
	return
}

// The email sign-in of the user: its address and whether it is verified.
func emailChangeTestSignIn(t testing.TB, ctx context.Context, userId server.Id) (userAuth string, verified bool, found bool) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		userAuth, verified, found = emailSignInForUpdateInTx(ctx, tx, userId)
	})
	return
}

func emailChangeTestAccountUserAuth(ctx context.Context, userId server.Id) (userAuth *string) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT user_auth FROM network_user WHERE user_id = $1`, userId).Scan(&userAuth))
	})
	return
}

// The sign-in notices owed to an address: each listed change's action, its
// identity and its new identity.
func emailChangeTestNotices(t testing.TB, ctx context.Context, networkId server.Id, recipient string) []*SecuritySignInChange {
	t.Helper()
	notices := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT template_json::text FROM account_message_outbox WHERE network_id = $1 AND user_auth = $2 AND template_name = $3`, networkId, recipient, SecuritySignInTemplateName)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var notice string
				server.Raise(result.Scan(&notice))
				notices = append(notices, notice)
			}
		})
	})
	changes := []*SecuritySignInChange{}
	for _, raw := range notices {
		notice := &SecuritySignInNotice{}
		if err := json.Unmarshal([]byte(raw), notice); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, notice.Changes...)
	}
	return changes
}

func emailChangeTestHasChange(changes []*SecuritySignInChange, action string, identity string, newIdentity string) bool {
	for _, change := range changes {
		if change.Action == action && change.Method == SecuritySignInMethodEmail && change.Identity == identity && change.NewIdentity == newIdentity {
			return true
		}
	}
	return false
}

// The root cause: the email sign-in changes only when both codes are
// confirmed, keeps its password, and the account's own email follows. Both
// addresses hear of the change, a reset code still waiting for the old
// address stops working, and the codes complete the change once.
func TestEmailChangeNeedsBothAddresses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultEmailChangeSettings()
		networkId, userId, currentEmail := emailChangeTestNetwork(t, ctx)
		newEmail := emailClaimTestAddress("changed")

		var passwordHashBefore []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT password_hash FROM network_user_auth_password WHERE user_id = $1 AND auth_type = 'email'`, userId).Scan(&passwordHashBefore))
		})
		// a reset code sent to the old address before the change
		resetId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO user_auth_reset (user_auth_reset_id, user_id, reset_code) VALUES ($1, $2, $3)`, resetId, userId, "reset-"+resetId.String()))
		})

		start, err := StartEmailChange(ctx, settings, userId, networkId, "  "+newEmail+" ")
		if err != nil {
			t.Fatal(err)
		}
		if start.CurrentEmail != currentEmail || start.NewEmail != newEmail || start.CurrentCode == start.NewCode && start.CurrentCode == "" {
			t.Fatalf("start = %+v", start)
		}
		if !emailChangeTestHasChange(emailChangeTestNotices(t, ctx, networkId, currentEmail), SecuritySignInChangeRequested, maskEmailAddress(currentEmail), maskEmailAddress(newEmail)) {
			t.Fatal("the request owed no notice to the current address")
		}
		if state := GetEmailChange(ctx, userId, networkId); state == nil || state.CurrentEmail != currentEmail || state.NewEmail != newEmail || state.CurrentConfirmed || state.NewConfirmed {
			t.Fatalf("state = %+v", state)
		}

		// the current address alone changes nothing
		confirm, err := ConfirmEmailChange(ctx, settings, userId, networkId, start.CurrentCode, "")
		if err != nil || !confirm.CurrentConfirmed || confirm.NewConfirmed || confirm.Complete {
			t.Fatalf("the current code: %+v %v", confirm, err)
		}
		if userAuth, _, _ := emailChangeTestSignIn(t, ctx, userId); userAuth != currentEmail {
			t.Fatalf("the sign-in changed on one code: %s", userAuth)
		}

		// the new address completes it
		confirm, err = ConfirmEmailChange(ctx, settings, userId, networkId, "", start.NewCode[:2]+" "+start.NewCode[2:])
		if err != nil || !confirm.Complete || confirm.Email != newEmail {
			t.Fatalf("the new code: %+v %v", confirm, err)
		}
		userAuth, verified, found := emailChangeTestSignIn(t, ctx, userId)
		if !found || userAuth != newEmail || !verified {
			t.Fatalf("the email sign-in is %s (verified %t)", userAuth, verified)
		}
		if accountUserAuth := emailChangeTestAccountUserAuth(ctx, userId); accountUserAuth == nil || *accountUserAuth != newEmail {
			t.Fatalf("the account email is %v", accountUserAuth)
		}
		var passwordHashAfter []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT password_hash FROM network_user_auth_password WHERE user_id = $1 AND auth_type = 'email'`, userId).Scan(&passwordHashAfter))
		})
		if string(passwordHashAfter) != string(passwordHashBefore) {
			t.Fatal("the password changed")
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_auth = $1`, currentEmail); count != 0 {
			t.Fatal("the old address still signs in")
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM user_auth_reset WHERE user_auth_reset_id = $1 AND NOT used`, resetId); count != 0 {
			t.Fatal("the reset code sent to the old address still works")
		}
		// both addresses hear of the change
		for _, recipient := range []string{currentEmail, newEmail} {
			if !emailChangeTestHasChange(emailChangeTestNotices(t, ctx, networkId, recipient), SecuritySignInChanged, maskEmailAddress(currentEmail), maskEmailAddress(newEmail)) {
				t.Fatalf("no change notice owed to %s", recipient)
			}
		}
		// the change is done: nothing waits, and the codes confirm nothing
		if state := GetEmailChange(ctx, userId, networkId); state != nil {
			t.Fatalf("a completed change still waits: %+v", state)
		}
		if _, err := ConfirmEmailChange(ctx, settings, userId, networkId, start.CurrentCode, start.NewCode); !errors.Is(err, ErrEmailChangeNoChange) {
			t.Fatalf("the codes again: %v", err)
		}
	})
}

// A change asked for by anyone but the owner, of an account without a
// verified email sign-in, to the same address, to a malformed address, or to
// an address another account proved, is refused and sends nothing.
func TestEmailChangeRefusesWhatItMustNotChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultEmailChangeSettings()
		networkId, userId, currentEmail := emailChangeTestNetwork(t, ctx)
		_, otherUserId, otherEmail := emailChangeTestNetwork(t, ctx)

		for _, c := range []struct {
			name      string
			userId    server.Id
			networkId server.Id
			newEmail  string
			err       error
		}{
			{"another user", otherUserId, networkId, emailClaimTestAddress("x"), ErrEmailChangeOwnerOnly},
			{"the same address", userId, networkId, " " + currentEmail, ErrEmailChangeSameEmail},
			{"not an address", userId, networkId, "not-an-email", ErrEmailChangeInvalidEmail},
			{"a phone number", userId, networkId, "+1 650 555 0100", ErrEmailChangeInvalidEmail},
			{"another account's verified email", userId, networkId, otherEmail, ErrEmailChangeEmailInUse},
		} {
			if _, err := StartEmailChange(ctx, settings, c.userId, c.networkId, c.newEmail); !errors.Is(err, c.err) {
				t.Errorf("%s: %v, want %v", c.name, err, c.err)
			}
		}
		if state := GetEmailChange(ctx, userId, networkId); state != nil {
			t.Fatalf("a refused change waits: %+v", state)
		}

		// an account without an email sign-in, and one whose email is not
		// verified yet
		seedNetworkId, seedUserId, _, _ := emailClaimSeedphraseAccount(t, ctx)
		if _, err := StartEmailChange(ctx, settings, seedUserId, seedNetworkId, emailClaimTestAddress("seed")); !errors.Is(err, ErrEmailChangeNoEmail) {
			t.Errorf("no email sign-in: %v", err)
		}
		unverifiedEmail := emailClaimTestAddress("unverified")
		unverifiedNetworkId, unverifiedUserId := emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 1, nil), unverifiedEmail, "unverified-password")
		if _, err := StartEmailChange(ctx, settings, unverifiedUserId, unverifiedNetworkId, emailClaimTestAddress("next")); !errors.Is(err, ErrEmailChangeEmailUnverified) {
			t.Errorf("an unverified email: %v", err)
		}
	})
}

// Wrong codes end the change after TryLimit tries, even for the right codes;
// a code is right only in its own slot; a new request replaces the waiting
// one; and an address another account proves meanwhile ends the change
// unmade, while a pending hold of another account yields to it.
func TestEmailChangeCodesAreBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultEmailChangeSettings()
		// this test asks for more changes than a day's budget allows
		settings.RequestLimit = 10
		settings.CodeSendLimit = 10
		networkId, userId, currentEmail := emailChangeTestNetwork(t, ctx)

		start, err := StartEmailChange(ctx, settings, userId, networkId, emailClaimTestAddress("bounded"))
		if err != nil {
			t.Fatal(err)
		}
		wrongTries := 0
		// the new address's code in the current slot is wrong
		if start.CurrentCode != start.NewCode {
			confirm, err := ConfirmEmailChange(ctx, settings, userId, networkId, start.NewCode, "")
			if !errors.Is(err, ErrEmailChangeCodeInvalid) || confirm.CurrentConfirmed {
				t.Fatalf("a code in the other slot: %+v %v", confirm, err)
			}
			wrongTries += 1
		}
		wrong := accountCodeTestWrong(start.CurrentCode)
		for wrongTries < settings.TryLimit-1 {
			if _, err := ConfirmEmailChange(ctx, settings, userId, networkId, wrong, ""); !errors.Is(err, ErrEmailChangeCodeInvalid) {
				t.Fatalf("wrong try %d: %v", wrongTries, err)
			}
			wrongTries += 1
		}
		if _, err := ConfirmEmailChange(ctx, settings, userId, networkId, wrong, ""); !errors.Is(err, ErrEmailChangeTooManyTries) {
			t.Fatalf("the last wrong try: %v", err)
		}
		if _, err := ConfirmEmailChange(ctx, settings, userId, networkId, start.CurrentCode, start.NewCode); !errors.Is(err, ErrEmailChangeNoChange) {
			t.Fatalf("the right codes after the tries: %v", err)
		}

		// a new request replaces the waiting one
		first, err := StartEmailChange(ctx, settings, userId, networkId, emailClaimTestAddress("first"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := StartEmailChange(ctx, settings, userId, networkId, emailClaimTestAddress("second"))
		if err != nil {
			t.Fatal(err)
		}
		if first.CurrentCode != second.CurrentCode || first.NewCode != second.NewCode {
			if confirm, err := ConfirmEmailChange(ctx, settings, userId, networkId, first.CurrentCode, first.NewCode); err == nil && confirm.Complete {
				t.Fatal("the replaced request's codes completed the change")
			}
		}
		if state := GetEmailChange(ctx, userId, networkId); state == nil || state.RequestId != second.RequestId {
			t.Fatalf("the waiting change = %+v, want the second", state)
		}

		// another account proves the new address before the change is made
		taken := emailClaimTestAddress("taken")
		third, err := StartEmailChange(ctx, settings, userId, networkId, taken)
		if err != nil {
			t.Fatal(err)
		}
		takenNetworkId := server.NewId()
		takenUserId := server.NewId()
		Testing_CreateNetwork(ctx, takenNetworkId, "taken-"+takenNetworkId.String(), takenUserId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user_auth_password SET user_auth = $2 WHERE user_id = $1`, takenUserId, taken))
		})
		confirm, err := ConfirmEmailChange(ctx, settings, userId, networkId, third.CurrentCode, third.NewCode)
		if !errors.Is(err, ErrEmailChangeEmailInUse) || confirm.Complete {
			t.Fatalf("a taken address: %+v %v", confirm, err)
		}
		if userAuth, _, _ := emailChangeTestSignIn(t, ctx, userId); userAuth != currentEmail {
			t.Fatalf("the email changed to a taken address: %s", userAuth)
		}
		if state := GetEmailChange(ctx, userId, networkId); state != nil {
			t.Fatalf("a change to a taken address still waits: %+v", state)
		}

		// a pending hold (an unverified sign-up) yields to the code's proof
		pending := emailClaimTestAddress("pending")
		_, pendingUserId := emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 2, nil), pending, "pending-password")
		fourth, err := StartEmailChange(ctx, settings, userId, networkId, pending)
		if err != nil {
			t.Fatal(err)
		}
		if confirm, err := ConfirmEmailChange(ctx, settings, userId, networkId, fourth.CurrentCode, fourth.NewCode); err != nil || !confirm.Complete {
			t.Fatalf("an address with a pending hold: %+v %v", confirm, err)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1`, pendingUserId); count != 0 {
			t.Fatal("the pending hold was not released")
		}
	})
}

// A request spends the account's request budget and each address's code
// budget, and a refused request spends none of them.
func TestEmailChangeBudgets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultEmailChangeSettings()
		settings.CodeSendLimit = 1
		networkId, userId, _ := emailChangeTestNetwork(t, ctx)

		if _, err := StartEmailChange(ctx, settings, userId, networkId, emailClaimTestAddress("budget")); err != nil {
			t.Fatal(err)
		}
		// the current address has had its one code
		_, err := StartEmailChange(ctx, settings, userId, networkId, emailClaimTestAddress("again"))
		var rateLimit interface{ RetryAfterSeconds() int }
		if !errors.As(err, &rateLimit) || rateLimit.RetryAfterSeconds() <= 0 {
			t.Fatalf("a second code to the current address: %v", err)
		}
		// the refusal rolled back the request it counted
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_action_attempt WHERE user_id = $1 AND action = $2`, userId, AccountActionEmailChangeRequest); count != 1 {
			t.Fatalf("%d requests counted, want 1", count)
		}

		settings = DefaultEmailChangeSettings()
		settings.RequestLimit = 1
		otherNetworkId, otherUserId, _ := emailChangeTestNetwork(t, ctx)
		if _, err := StartEmailChange(ctx, settings, otherUserId, otherNetworkId, emailClaimTestAddress("one")); err != nil {
			t.Fatal(err)
		}
		if _, err := StartEmailChange(ctx, settings, otherUserId, otherNetworkId, emailClaimTestAddress("two")); !errors.As(err, &rateLimit) {
			t.Fatalf("a second request of the day: %v", err)
		}
	})
}
