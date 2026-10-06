// The account messages the model owes itself: the password-changed notice in
// the password reset's transaction, the welcome in the transaction of the
// verification that completes a sign-up. A deferred constraint trigger on the
// outbox fails the commit of the transaction that adds the message, after
// everything in it ran, to show the message and the change commit together.
package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The outbox messages of the template for the network.
func modelOutboxMessageCount(ctx context.Context, networkId server.Id, templateName string) int {
	return countRows(
		ctx,
		`SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1 AND template_name = $2`,
		networkId,
		templateName,
	)
}

// Fails, at commit, every transaction that adds an account message for the
// network, until the returned function removes the fault.
func failModelOutboxCommits(ctx context.Context, networkId server.Id) (remove func()) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION synthetic_model_outbox_fault() RETURNS trigger AS $fault$
			BEGIN
				RAISE EXCEPTION 'synthetic failure at commit';
			END
			$fault$ LANGUAGE plpgsql;
			CREATE CONSTRAINT TRIGGER synthetic_model_outbox_fault
				AFTER INSERT ON account_message_outbox
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW WHEN (NEW.network_id = '%s')
				EXECUTE FUNCTION synthetic_model_outbox_fault();
		`, networkId)))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				DROP TRIGGER synthetic_model_outbox_fault ON account_message_outbox;
				DROP FUNCTION synthetic_model_outbox_fault();
			`))
		})
	}
}

// A reset owes one password-changed notice to the admin's address, committed
// with the new password: a reset whose transaction fails keeps the old password
// and owes nothing, and the retry with the same code owes one notice.
func TestAuthPasswordSetOwesThePasswordChangedNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userAuth := Testing_CreateNetwork(ctx, networkId, "synthetic-password-set", server.NewId())
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		resetResult, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, resetResult.ResetCode, nil)
		setArgs := AuthPasswordSetArgs{
			ResetCode: *resetResult.ResetCode,
			Password:  "synthetic-new-password",
		}

		removeFault := failModelOutboxCommits(ctx, networkId)
		var setPanic any
		func() {
			defer func() {
				setPanic = recover()
			}()
			AuthPasswordSet(setArgs, clientSession)
		}()
		if err, ok := setPanic.(error); !ok || !strings.Contains(err.Error(), "synthetic failure at commit") {
			t.Fatalf("password set panic = %v, want the commit failure", setPanic)
		}
		connect.AssertEqual(t, modelOutboxMessageCount(ctx, networkId, AuthPasswordSetTemplateName), 0)
		oldLogin, err := AuthLoginWithPassword(AuthLoginWithPasswordArgs{UserAuth: userAuth, Password: "password"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, oldLogin.Error, nil)
		removeFault()

		setResult, err := AuthPasswordSet(setArgs, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, setResult.Error, nil)
		connect.AssertEqual(t, modelOutboxMessageCount(ctx, networkId, AuthPasswordSetTemplateName), 1)
		notice := GetAccountMessageByKey(ctx, AuthPasswordSetTemplateName, setResultResetId(t, ctx, *resetResult.ResetCode))
		if notice == nil || notice.UserAuth != userAuth || notice.DeliverTime == nil {
			t.Fatalf("notice = %+v, want due for %s", notice, userAuth)
		}

		// the used code changes nothing more
		if _, err := AuthPasswordSet(setArgs, clientSession); err == nil {
			t.Fatal("a used reset code set the password again")
		}
		connect.AssertEqual(t, modelOutboxMessageCount(ctx, networkId, AuthPasswordSetTemplateName), 1)
	})
}

// The id of the reset with the code, the password-changed notice's key.
func setResultResetId(t testing.TB, ctx context.Context, resetCode string) (key string) {
	var resetId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT user_auth_reset_id FROM user_auth_reset WHERE reset_code = $1`, resetCode)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&resetId))
			}
		})
	})
	return resetId.String()
}

// The verification that completes an email sign-up owes one welcome, committed
// with it; a re-verification, a phone added later and an email added to a
// Google network owe none, and a verification whose transaction fails owes
// none and leaves the sign-up to complete.
func TestAuthVerifyOwesTheWelcomeOnlyForANewAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		userAuth := Testing_CreateNetwork(ctx, networkId, "synthetic-welcome", userId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_user_auth_password SET verified = false WHERE user_id = $1`,
				userId,
			))
		})

		removeFault := failModelOutboxCommits(ctx, networkId)
		var verifyPanic any
		func() {
			defer func() {
				verifyPanic = recover()
			}()
			verifyWithNewCode(t, ctx, userAuth)
		}()
		if err, ok := verifyPanic.(error); !ok || !strings.Contains(err.Error(), "synthetic failure at commit") {
			t.Fatalf("verification panic = %v, want the commit failure", verifyPanic)
		}
		connect.AssertEqual(t, modelOutboxMessageCount(ctx, networkId, NetworkWelcomeTemplateName), 0)
		removeFault()

		connect.AssertEqual(t, verifyWithNewCode(t, ctx, userAuth).NewAccount, true)
		connect.AssertEqual(t, modelOutboxMessageCount(ctx, networkId, NetworkWelcomeTemplateName), 1)
		welcome := GetAccountMessageByKey(ctx, NetworkWelcomeTemplateName, userId.String())
		if welcome == nil || welcome.UserAuth != userAuth || welcome.DeliverTime == nil {
			t.Fatalf("welcome = %+v, want due for %s", welcome, userAuth)
		}

		connect.AssertEqual(t, verifyWithNewCode(t, ctx, userAuth).NewAccount, false)
		phone := "+1 555-555-0123"
		addUnverifiedUserAuth(t, ctx, userId, phone)
		connect.AssertEqual(t, verifyWithNewCode(t, ctx, phone).NewAccount, false)
		connect.AssertEqual(t, modelOutboxMessageCount(ctx, networkId, NetworkWelcomeTemplateName), 1)

		ssoNetworkId := server.NewId()
		ssoUserId := server.NewId()
		Testing_CreateNetworkSso(
			ssoNetworkId,
			ssoUserId,
			AuthJwt{
				AuthType: SsoAuthTypeGoogle,
				UserAuth: fmt.Sprintf("sso-%s@synthetic.example", ssoNetworkId),
			},
			ctx,
		)
		addedEmail := fmt.Sprintf("added-%s@synthetic.example", ssoNetworkId)
		addUnverifiedUserAuth(t, ctx, ssoUserId, addedEmail)
		connect.AssertEqual(t, verifyWithNewCode(t, ctx, addedEmail).NewAccount, false)
		connect.AssertEqual(t, modelOutboxMessageCount(ctx, ssoNetworkId, NetworkWelcomeTemplateName), 0)
	})
}
