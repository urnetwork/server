// The API key security notice reaches its recipient through the account message
// outbox (owner decision 2026-10-29, "Yes, API keys too"): by email, once its
// coalescing window has passed, and never by SMS.
package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// An API key created on an account with an email address and a phone number is
// emailed once to the email address after the coalescing window, naming the key
// and never carrying it, and nothing is sent to the phone number.
func TestSecurityApiKeyNoticeIsEmailedNeverTexted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "synthetic-api-key-delivery", userId)
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: "synthetic-api-key-delivery",
		})
		phone := "+1 415-555-0124"
		password := "synthetic-password-1"
		if result, err := model.AddAuth(model.AddAuthMethod{UserAuth: &phone, Password: &password}, clientSession); err != nil || result.Error != nil {
			t.Fatalf("add phone: %v %+v", err, result)
		}
		// whatever the phone sign-in owed goes out first, on its own
		deliverAccountMessagesAt(ctx, newOutboxTestSender(), server.NowUtc().Add(2*time.Minute))

		created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "synthetic delivery key"}, clientSession)
		if err != nil || created.Error != nil || created.ApiKey == "" {
			t.Fatalf("create api key: %v %+v", err, created)
		}

		sender := newOutboxTestSender()
		now := server.NowUtc()
		// inside the coalescing window nothing is due
		deliverAccountMessagesAt(ctx, sender, now)
		if sends := sender.sent(); len(sends) != 0 {
			t.Fatalf("sent %d messages before the coalescing window passed", len(sends))
		}
		deliverAccountMessagesAt(ctx, sender, now.Add(2*time.Minute))
		sends := sender.sent()
		apiKeySends := []outboxTestSend{}
		for _, send := range sends {
			if _, userAuthType := model.NormalUserAuth(send.userAuth); userAuthType != model.UserAuthTypeEmail {
				t.Fatalf("%s went to %s, which is no email address", send.template.Name(), send.userAuth)
			}
			if send.template.Name() == "security_api_key_changed" {
				apiKeySends = append(apiKeySends, send)
			}
		}
		if len(apiKeySends) != 1 || len(sends) != 1 {
			t.Fatalf("delivered %d API key notices in %d sends, want one notice and nothing else", len(apiKeySends), len(sends))
		}
		if apiKeySends[0].userAuth != userAuth {
			t.Fatalf("the notice went to %s, want %s", apiKeySends[0].userAuth, userAuth)
		}

		subject, bodyHtml, bodyText, err := RenderEmailTemplate(apiKeySends[0].template)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(bodyText, "synthetic delivery key") {
			t.Fatalf("the email does not name the key:\n%s", bodyText)
		}
		for _, part := range []string{subject, bodyHtml, bodyText} {
			if strings.Contains(part, created.ApiKey) || strings.Contains(part, created.Id.String()) {
				t.Fatal("the email carries the key or its id")
			}
		}
	})
}
