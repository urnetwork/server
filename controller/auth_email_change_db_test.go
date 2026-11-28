package controller

// The email change routes against the test env: the codes go to the right
// addresses, each with its own role and code, the change completes with them,
// and the other sessions are signed out once.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestAuthEmailChangeSendsBothCodesAndSignsOutOthers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		currentEmail := model.Testing_CreateNetwork(ctx, networkId, "emailroute"+networkId.String()[:8], userId)
		newEmail := "changed-" + server.NewId().String()[:8] + "@example.com"
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})
		settings := model.DefaultEmailChangeSettings()
		sender := &emailChangeTestSender{}

		started, err := authEmailChange(&AuthEmailChangeArgs{NewEmail: newEmail}, clientSession, settings, sender)
		if err != nil || started.Error != nil {
			t.Fatalf("start: %+v %v", started, err)
		}
		if started.CurrentEmail != model.MaskEmailAddress(currentEmail) || started.NewEmail != newEmail || started.ExpiresAtMillis <= server.NowUtc().UnixMilli() {
			t.Fatalf("start = %+v", started)
		}
		if len(sender.sends) != 2 {
			t.Fatalf("%d codes sent, want 2", len(sender.sends))
		}
		codes := map[string]*AuthEmailChangeCodeTemplate{}
		for _, send := range sender.sends {
			codes[send.userAuth] = send.template.(*AuthEmailChangeCodeTemplate)
		}
		currentSend, newSend := codes[currentEmail], codes[newEmail]
		if currentSend == nil || newSend == nil || !currentSend.Current || newSend.Current || currentSend.OtherEmail != model.MaskEmailAddress(newEmail) || newSend.OtherEmail != model.MaskEmailAddress(currentEmail) {
			t.Fatalf("the code emails = %+v", codes)
		}

		status, err := AuthEmailChangeStatus(clientSession)
		if err != nil || !status.Pending || status.NewEmail != newEmail || status.CurrentConfirmed || status.NewConfirmed {
			t.Fatalf("status: %+v %v", status, err)
		}

		signOuts := 0
		signOut := func(signOutSession *session.ClientSession, change *model.EmailChangeConfirm) bool {
			signOuts += 1
			return signOutSession == clientSession && change.Complete
		}
		confirmed, err := authEmailChangeConfirm(&AuthEmailChangeConfirmArgs{CurrentCode: currentSend.Code}, clientSession, settings, signOut)
		if err != nil || confirmed.Error != nil || !confirmed.CurrentConfirmed || confirmed.Complete || signOuts != 0 {
			t.Fatalf("the current code: %+v %v", confirmed, err)
		}
		confirmed, err = authEmailChangeConfirm(&AuthEmailChangeConfirmArgs{NewCode: newSend.Code}, clientSession, settings, signOut)
		if err != nil || confirmed.Error != nil || !confirmed.Complete || confirmed.Email != newEmail || !confirmed.OtherSessionsSignedOut || signOuts != 1 {
			t.Fatalf("the new code: %+v %v (sign-outs %d)", confirmed, err, signOuts)
		}

		// a send that fails cancels the change: one code alone cannot complete it
		failing := &emailChangeTestSender{fail: func(userAuth string) bool { return userAuth != newEmail }}
		again, err := authEmailChange(&AuthEmailChangeArgs{NewEmail: "again-" + server.NewId().String()[:8] + "@example.com"}, clientSession, settings, failing)
		if err != nil || again.Error == nil || again.Error.Code != AuthEmailChangeErrorCodeSendFailed {
			t.Fatalf("a failed send: %+v %v", again, err)
		}
		if status, _ := AuthEmailChangeStatus(clientSession); status.Pending {
			t.Fatal("a change whose code was not sent still waits")
		}
	})
}
