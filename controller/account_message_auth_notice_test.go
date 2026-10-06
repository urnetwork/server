// The password-changed notice and the welcome go through the account message
// outbox: the request owes them with its commit and sends nothing itself, and
// the delivery task sends them, retrying a failed send.
package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A password reset sends nothing itself; its notice is delivered after a
// failed first send, once, to the admin.
func TestPasswordChangedNoticeIsDeliveredAfterAFailedSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "synthetic-password-notice", server.NewId())
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		resetResult, err := model.AuthPasswordResetCreateCode(model.AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, clientSession)
		connect.AssertEqual(t, err, nil)
		requestSender := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(requestSender)
		defer SetMessageSender(previousSender)

		result, err := AuthPasswordSet(model.AuthPasswordSetArgs{
			ResetCode: *resetResult.ResetCode,
			Password:  "synthetic-new-password",
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*model.AuthPasswordSetError)(nil))
		if sends := requestSender.sent(); len(sends) != 0 || requestSender.failed() != 0 {
			t.Fatal("the password reset request sent its notice itself")
		}

		sender := newOutboxTestSender()
		sender.failNext(1)
		now := server.NowUtc()
		deliverAccountMessagesAt(ctx, sender, now)
		deliverAccountMessagesAt(ctx, sender, now.Add(accountMessageRetryBaseDelay))
		deliverAccountMessagesAt(ctx, sender, now.Add(accountMessageRetryMaxDelay))
		sends := sender.sent()
		if len(sends) != 1 || sends[0].userAuth != userAuth {
			t.Fatalf("delivered %d notices (%+v), want one to %s", len(sends), sends, userAuth)
		}
		if _, ok := sends[0].template.(*AuthPasswordSetTemplate); !ok {
			t.Fatalf("delivered %T, want the password-changed notice", sends[0].template)
		}
	})
}

// The verification that completes a sign-up sends nothing itself; its welcome
// is delivered after a failed first send, once.
func TestWelcomeIsDeliveredAfterAFailedSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "synthetic-welcome-notice", userId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user_auth_password SET verified = false WHERE user_id = $1`, userId))
		})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		createResult, err := model.AuthVerifyCreateCode(model.AuthVerifyCreateCodeArgs{UserAuth: userAuth}, clientSession)
		connect.AssertEqual(t, err, nil)
		requestSender := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(requestSender)
		defer SetMessageSender(previousSender)

		result, err := AuthVerify(model.AuthVerifyArgs{UserAuth: userAuth, VerifyCode: *createResult.VerifyCode}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.NewAccount, true)
		if sends := requestSender.sent(); len(sends) != 0 || requestSender.failed() != 0 {
			t.Fatal("the verification request sent its welcome itself")
		}

		sender := newOutboxTestSender()
		sender.failNext(1)
		now := server.NowUtc()
		deliverAccountMessagesAt(ctx, sender, now)
		deliverAccountMessagesAt(ctx, sender, now.Add(accountMessageRetryBaseDelay))
		deliverAccountMessagesAt(ctx, sender, now.Add(accountMessageRetryMaxDelay))
		sends := sender.sent()
		if len(sends) != 1 || sends[0].userAuth != userAuth {
			t.Fatalf("delivered %d welcomes (%+v), want one to %s", len(sends), sends, userAuth)
		}
		if _, ok := sends[0].template.(*NetworkWelcomeTemplate); !ok {
			t.Fatalf("delivered %T, want the welcome", sends[0].template)
		}
	})
}
