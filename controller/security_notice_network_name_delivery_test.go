// The delivery names the network a security notice is about (QD-1216): it
// reads the name of the outbox row's network as it stands when the notice is
// sent, so a personal network's notice names it, a renamed network's pending
// notice shows the current name, and the outbox stores no name.
package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A network name unique to the test.
func securityNoticeTestNetworkName(label string) string {
	return fmt.Sprintf("%s-%s", label, strings.ReplaceAll(server.NewId().String(), "-", "")[24:32])
}

// Delivers what is due once the coalescing window of a notice owed now has
// passed, and returns the sends of the template.
func deliverSecurityNoticesOf(t testing.TB, ctx context.Context, templateName string) []outboxTestSend {
	t.Helper()
	sender := newOutboxTestSender()
	deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(model.DefaultSecurityNoticeSettings().CoalesceWindow))
	sends := []outboxTestSend{}
	for _, send := range sender.sent() {
		if send.template.Name() == templateName {
			sends = append(sends, send)
		}
	}
	return sends
}

// Holds that the sent notice renders naming the network networkName, in its
// subject, its html part and its text part, and naming none of the others.
func requireSecurityNoticeNamesNetwork(t testing.TB, send outboxTestSend, networkName string, others ...string) {
	t.Helper()
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(send.template)
	if err != nil {
		t.Fatalf("%s: render: %v", send.template.Name(), err)
	}
	network := "your URnetwork network " + networkName
	for part, body := range map[string]string{"subject": subject, "html": bodyHtml, "text": bodyText} {
		if !strings.Contains(body, network) {
			t.Fatalf("%s to %s: the %s does not say %q:\n%s", send.template.Name(), send.userAuth, part, network, body)
		}
		for _, other := range others {
			if strings.Contains(body, other) {
				t.Fatalf("%s to %s: the %s names %s:\n%s", send.template.Name(), send.userAuth, part, other, body)
			}
		}
	}
}

// The stored fields of the network's unsent notices of the template.
func unsentSecurityNoticeJsons(t testing.TB, ctx context.Context, networkId server.Id, templateName string) []string {
	t.Helper()
	templateJsons := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT template_json::text FROM account_message_outbox WHERE network_id = $1 AND template_name = $2 AND sent_time IS NULL`,
			networkId,
			templateName,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var templateJson string
				server.Raise(result.Scan(&templateJson))
				templateJsons = append(templateJsons, templateJson)
			}
		})
	})
	return templateJsons
}

// An API key created in a personal network owes its owner a notice that names
// the network.
func TestSecurityApiKeyNoticeNamesAPersonalNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		networkName := securityNoticeTestNetworkName("named-personal")
		userAuth := model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: networkName,
		})
		created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "synthetic named key"}, clientSession)
		if err != nil || created.Error != nil {
			t.Fatalf("create api key: %v %+v", err, created)
		}

		sends := deliverSecurityNoticesOf(t, ctx, model.SecurityApiKeyTemplateName)
		if len(sends) != 1 || sends[0].userAuth != userAuth {
			t.Fatalf("delivered %d API key notices (%+v), want one to %s", len(sends), sends, userAuth)
		}
		requireSecurityNoticeNamesNetwork(t, sends[0], networkName)
		subject, _, _, err := RenderEmailTemplate(sends[0].template)
		if err != nil {
			t.Fatal(err)
		}
		if want := "An API key was created for your URnetwork network " + networkName; subject != want {
			t.Fatalf("subject = %q, want %q", subject, want)
		}
	})
}

// A notice owed before its network was renamed, and still waiting when the
// rename commits, names the network as it is called when the notice is sent;
// the outbox holds neither name.
func TestSecurityNoticeOfARenamedNetworkShowsItsCurrentName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		networkId := server.NewId()
		userId := server.NewId()
		oldName := securityNoticeTestNetworkName("named-before")
		newName := securityNoticeTestNetworkName("named-after")
		userAuth := model.Testing_CreateNetwork(ctx, networkId, oldName, userId)
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: oldName,
		})
		created, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "synthetic rename key"}, clientSession)
		if err != nil || created.Error != nil {
			t.Fatalf("create api key: %v %+v", err, created)
		}
		// the notice waits out its coalescing window
		owed := unsentSecurityNoticeJsons(t, ctx, networkId, model.SecurityApiKeyTemplateName)
		if len(owed) != 1 {
			t.Fatalf("the key owed %d notices, want 1", len(owed))
		}

		renamed, err := ChangeNetworkName(ChangeNetworkNameArgs{NetworkName: newName}, clientSession)
		if err != nil || renamed.Error != nil || renamed.NetworkName != newName {
			t.Fatalf("rename: %v %+v", err, renamed)
		}
		for _, templateJson := range unsentSecurityNoticeJsons(t, ctx, networkId, model.SecurityApiKeyTemplateName) {
			if strings.Contains(templateJson, oldName) || strings.Contains(templateJson, newName) {
				t.Fatalf("the outbox holds the network's name: %s", templateJson)
			}
		}

		sends := deliverSecurityNoticesOf(t, ctx, model.SecurityApiKeyTemplateName)
		if len(sends) != 1 || sends[0].userAuth != userAuth {
			t.Fatalf("delivered %d API key notices (%+v), want one to %s", len(sends), sends, userAuth)
		}
		requireSecurityNoticeNamesNetwork(t, sends[0], newName, oldName)
	})
}

// Each notice that can reach a team, owed for a network, is sent naming the
// network as it stands; a notice of a network that is gone goes out unnamed
// rather than not at all. The samples are named for the sample network in
// memory, which the outbox's stored fields do not carry, so neither names it.
func TestEverySecurityNoticeAboutANetworkIsSentNamingIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		networkName := securityNoticeTestNetworkName("named-every")
		model.Testing_CreateNetwork(ctx, networkId, networkName, server.NewId())
		goneNetworkId := server.NewId()

		owe := func(networkId server.Id, userAuth string, template Template) {
			server.Tx(ctx, func(tx server.PgTx) {
				if !addAccountMessageInTx(ctx, tx, &accountMessage{
					key:       "synthetic-named:" + server.NewId().String(),
					networkId: &networkId,
					userAuth:  userAuth,
					template:  template,
				}) {
					t.Fatalf("%s was not added", template.Name())
				}
			})
		}
		for templateName, sample := range securityNetworkNoticeSamples() {
			owe(networkId, "named-"+templateName+"@synthetic.example", sample)
			owe(goneNetworkId, "gone-"+templateName+"@synthetic.example", sample)
		}

		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, server.NowUtc())
		sentCount := 0
		for _, send := range sender.sent() {
			if _, ok := securityNetworkNoticeSamples()[send.template.Name()]; !ok {
				continue
			}
			sentCount += 1
			switch {
			case strings.HasPrefix(send.userAuth, "named-"):
				requireSecurityNoticeNamesNetwork(t, send, networkName, securityNoticeSampleNetworkName)
			case strings.HasPrefix(send.userAuth, "gone-"):
				subject, bodyHtml, bodyText, err := RenderEmailTemplate(send.template)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(subject, "your URnetwork network") {
					t.Fatalf("%s of a network that is gone: subject %q", send.template.Name(), subject)
				}
				for _, part := range []string{subject, bodyHtml, bodyText} {
					if strings.Contains(part, networkName) || strings.Contains(part, securityNoticeSampleNetworkName) {
						t.Fatalf("%s of a network that is gone names a network: %q", send.template.Name(), part)
					}
				}
			default:
				t.Fatalf("%s went to %s", send.template.Name(), send.userAuth)
			}
		}
		if want := 2 * len(securityNetworkNoticeSamples()); sentCount != want {
			t.Fatalf("sent %d notices, want %d", sentCount, want)
		}
	})
}

// A read of the name that fails leaves the template unnamed and returns the
// error, so the delivery tries the message again rather than send it without
// the name; a template about the account, or a row without a network, reads
// nothing.
func TestNameAccountMessageNetworkReturnsAFailedRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		networkName := securityNoticeTestNetworkName("named-read")
		model.Testing_CreateNetwork(ctx, networkId, networkName, server.NewId())

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		notice := &SecurityAuthKeyChangedTemplate{}
		if err := nameAccountMessageNetwork(canceledCtx, &networkId, notice); err == nil {
			t.Fatal("a failed read returned no error")
		}
		if notice.Network() != "your URnetwork network" {
			t.Fatalf("a failed read named the network %q", notice.Network())
		}
		// nothing to read: no error even where a read would fail
		if err := nameAccountMessageNetwork(canceledCtx, nil, notice); err != nil || notice.Network() != "your URnetwork network" {
			t.Fatalf("a row without a network: %v, %q", err, notice.Network())
		}
		if err := nameAccountMessageNetwork(canceledCtx, &networkId, securitySignInNoticeSample(model.SecuritySignInAdded)); err != nil {
			t.Fatalf("a notice about the account read its network: %v", err)
		}

		if err := nameAccountMessageNetwork(ctx, &networkId, notice); err != nil {
			t.Fatal(err)
		}
		if want := "your URnetwork network " + networkName; notice.Network() != want {
			t.Fatalf("named %q, want %q", notice.Network(), want)
		}
	})
}
