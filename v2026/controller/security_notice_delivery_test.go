// The security notices reach their recipients through the account message
// outbox: the change owes the notice with its commit and sends nothing itself,
// and the delivery task emails it once its coalescing window has passed,
// retrying a failed send, which leaves the change as it is.
package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A hotkey network delegation, accepted through POST /sn/wallet, that gives the
// network's subnet earnings to a hotkey owes the payout wallet notice to the
// network owner's email, with the hotkey masked and the delegation's epochs.
func TestSecurityNoticeOwedForAHotkeyNetworkDelegation(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		hotkey, hotkeySs58 := snNetworkWalletTestKey(t, 191)
		coldkey, _ := snNetworkWalletTestKey(t, 192)
		chain := snHotkeyWalletConsentAppend(t, nil, f.rpc.domain.HotkeySubnet(), hotkey, coldkey, f.scope.Epoch, "security notice")
		f.retainHotkeyChain(t, f.owner, chain)
		set, _, _ := f.hotkeyDelegation(t, f.owner, hotkey, hotkeySs58, chain, f.scope.Epoch)
		f.accept(t, f.owner, set)

		var userAuths []string
		var templateJsons []string
		server.Db(t.Context(), func(conn server.PgConn) {
			result, err := conn.Query(
				t.Context(),
				`SELECT user_auth, template_json::text FROM account_message_outbox WHERE network_id = $1 AND template_name = $2`,
				f.owner.NetworkId,
				"security_payout_wallet_changed",
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var userAuth, templateJson string
					server.Raise(result.Scan(&userAuth, &templateJson))
					userAuths = append(userAuths, userAuth)
					templateJsons = append(templateJsons, templateJson)
				}
			})
		})
		if len(templateJsons) != 1 {
			t.Fatalf("the delegation owed %d payout wallet notices, want 1", len(templateJsons))
		}
		if _, userAuthType := model.NormalUserAuth(userAuths[0]); userAuthType != model.UserAuthTypeEmail {
			t.Fatalf("the notice went to %s, want the owner's email", userAuths[0])
		}
		var notice struct {
			Changes []map[string]any `json:"changes"`
		}
		if err := json.Unmarshal([]byte(templateJsons[0]), &notice); err != nil {
			t.Fatal(err)
		}
		masked := hotkeySs58[:4] + "…" + hotkeySs58[len(hotkeySs58)-4:]
		var hotkeyChange map[string]any
		for _, change := range notice.Changes {
			if change["destination"] == "subnet_hotkey" {
				hotkeyChange = change
			}
		}
		epochOf := func(value any) uint64 {
			number, _ := value.(float64)
			return uint64(number)
		}
		if hotkeyChange == nil ||
			hotkeyChange["new_chain"] != "TAO" ||
			hotkeyChange["new_wallet"] != masked ||
			hotkeyChange["previous_wallet"] != nil ||
			epochOf(hotkeyChange["from_epoch"]) != f.scope.Epoch ||
			epochOf(hotkeyChange["through_epoch"]) != f.scope.Epoch+100 {
			t.Fatalf("the notice lists %s, want the delegated hotkey %s for epochs %d to %d", templateJsons[0], masked, f.scope.Epoch, f.scope.Epoch+100)
		}
		for _, templateJson := range templateJsons {
			if strings.Contains(templateJson, hotkeySs58) {
				t.Fatal("the outbox holds the full hotkey")
			}
		}
	})
}

// A sign-in method added to an account with an email and a phone number is
// emailed once to the email address, after one failed send, and never sent to
// the phone number; the added sign-in stays added throughout.
func TestSecurityNoticeIsEmailedOnceAfterAFailedSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "synthetic-security-delivery", userId)
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: "synthetic-security-delivery",
		})
		requestSender := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(requestSender)
		defer SetMessageSender(previousSender)

		phone := "+1 415-555-0123"
		password := "synthetic-password-1"
		result, err := model.AddAuth(model.AddAuthMethod{UserAuth: &phone, Password: &password}, clientSession)
		if err != nil || result.Error != nil {
			t.Fatalf("add phone: %v %+v", err, result)
		}
		if sends := requestSender.sent(); len(sends) != 0 || requestSender.failed() != 0 {
			t.Fatal("the change sent its notice itself")
		}
		phoneBound := func() bool {
			networkUser := model.GetNetworkUser(ctx, userId)
			return networkUser != nil && slices.Contains(networkUser.AuthTypes, "phone")
		}

		sender := newOutboxTestSender()
		sender.failNext(1)
		now := server.NowUtc()
		// inside the coalescing window nothing is due
		deliverAccountMessagesAt(ctx, sender, now)
		if len(sender.sent()) != 0 || sender.failed() != 0 {
			t.Fatal("the notice was sent before its coalescing window passed")
		}
		// the first send fails; the change stands
		dueTime := now.Add(2 * time.Minute)
		deliverAccountMessagesAt(ctx, sender, dueTime)
		if sender.failed() != 1 || len(sender.sent()) != 0 {
			t.Fatalf("after the first delivery: %d failed, %d sent; want the notice's send to have failed", sender.failed(), len(sender.sent()))
		}
		if !phoneBound() {
			t.Fatal("a failed send undid the change it reported")
		}
		deliverAccountMessagesAt(ctx, sender, dueTime.Add(accountMessageRetryBaseDelay))
		deliverAccountMessagesAt(ctx, sender, dueTime.Add(accountMessageRetryMaxDelay))
		sends := sender.sent()
		if len(sends) != 1 {
			t.Fatalf("delivered %d notices (%+v), want one", len(sends), sends)
		}
		if sends[0].userAuth != userAuth || sends[0].template.Name() != "security_sign_in_changed" {
			t.Fatalf("delivered %s to %s, want the sign-in notice to %s", sends[0].template.Name(), sends[0].userAuth, userAuth)
		}
		for _, send := range sends {
			if _, userAuthType := model.NormalUserAuth(send.userAuth); userAuthType != model.UserAuthTypeEmail {
				t.Fatalf("a security notice went to %s, which is no email address", send.userAuth)
			}
		}
		if !phoneBound() {
			t.Fatal("the delivery undid the change it reported")
		}
	})
}
