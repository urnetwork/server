//go:build sidecar_pending

package api

// §14.14's two checked routes through the router the API serves (Routes()), against the database with MG1, MG2, MG5
// and MG9, over §8's checks as api's process installs them: model.SmtpApiGateOf over its gate cache
// (controller.installApiProjection; config_log_install_smtp_test.go holds that install), here over a stand-in cache
// holding a Team's verified rows. With no check installed, as api's process ran before, a publish and a reports read
// both answer 503; with api's checks and the network not loaded yet, both still 503; once loaded, the sidecar the rows
// grant mail_send publishes its selector, a sidecar with no mail_send is refused 403 mail_send and writes nothing,
// the admin inbox's reader reads the network's reports with their sealed copy, and a sidecar that does not read that
// repo is refused 403 smtp_reports_read. Each caller is a sidecar's bound strict_exp client, as a daemon holds one.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One selector to publish at domain, its key RSA-2048.
func testSmtpGateRoutePublishArgs(t testing.TB, domain string) (*sidecar.SmtpDkimKeysArgs, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	selector := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(random)
	return &sidecar.SmtpDkimKeysArgs{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: selector, Domain: domain, PublicKey: der}}}, selector
}

// The code a refusal's body carries.
func testSmtpGateRouteCode(body []byte) string {
	refusal := struct {
		Code string `json:"code"`
	}{}
	_ = json.Unmarshal(body, &refusal)
	return refusal.Code
}

// A rule of the rows: src's derived sidecar atom given action on targets.
func testSmtpGateRouteRule(src sidecar.Id, targets []*sidecarprotocol.AclTarget, action sidecarprotocol.AclAction) *sidecarprotocol.AclRule {
	return &sidecarprotocol.AclRule{
		RuleId:   sidecar.NewId().Bytes(),
		SrcAtoms: [][]byte{sidecartest.FakeAclSidecarAtom(src).GetAtom()},
		Targets:  targets,
		Actions:  []uint32{uint32(action)},
	}
}

// A repo row created at position at by an admin of its own.
func testSmtpGateRouteRepo(kind sidecarprotocol.RepoKind, inbox sidecarprotocol.RepoInbox, at uint64) (sidecar.Id, *sidecarprotocol.AclRepo) {
	repoId := sidecar.NewId()
	creatorId := sidecar.NewId().Bytes()
	return repoId, &sidecarprotocol.AclRepo{
		RepoId: repoId.Bytes(),
		Kind:   kind,
		Inbox:  inbox,
		Admins: []*sidecarprotocol.AclRepoAdminRow{{SidecarId: creatorId, Signer: creatorId, At: at}},
	}
}

func TestSmtpRoutesPublishAndReadThroughApisChecks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(fmt.Sprintf(
			"sidecar:\n  preview_networks: [%q]\n  smtpedge: {enabled: true, networks: [%q]}\n",
			self.networkId,
			self.networkId,
		))()
		root := fmt.Sprintf("g%s.example.", strings.ReplaceAll(self.networkId.String(), "-", "")[20:])
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			tb.Fatal(err)
		}
		defer model.Testing_SetDnsUserRoot(root)()
		var label string
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			label, err = model.EnsureNetworkLabelInTx(ctx, tx, self.networkId, root)
			if err != nil {
				tb.Fatal(err)
			}
		})
		domain := label + "." + strings.TrimSuffix(root, ".")

		// the network's verified state as api's gate cache holds it: a Team, its admin inbox read by one sidecar, and
		// mail_send for another
		view := sidecartest.NewFakeAclView()
		admin := view.AddMember(bridge.MemberRole_MEMBER_ROLE_ADMIN)
		senderId, readerId := view.AddDevice(admin), view.AddDevice(admin)
		_, directory := testSmtpGateRouteRepo(sidecarprotocol.RepoKind_REPO_KIND_DIRECTORY, sidecarprotocol.RepoInbox_REPO_INBOX_NONE, 1)
		adminInbox, adminInboxRow := testSmtpGateRouteRepo(sidecarprotocol.RepoKind_REPO_KIND_GENERAL, sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN, 2)
		rows := &sidecarprotocol.AclRows{
			HasDocument: true,
			Atoms:       []*sidecarprotocol.AclAtom{sidecartest.FakeAclSidecarAtom(senderId), sidecartest.FakeAclSidecarAtom(readerId)},
			Rules: []*sidecarprotocol.AclRule{
				testSmtpGateRouteRule(senderId, []*sidecarprotocol.AclTarget{{MailSend: true}}, sidecarprotocol.AclAction_ACL_ACTION_SEND),
				testSmtpGateRouteRule(readerId, []*sidecarprotocol.AclTarget{{RepoId: adminInbox.Bytes()}}, sidecarprotocol.AclAction_ACL_ACTION_READ),
			},
			Repos: []*sidecarprotocol.AclRepo{directory, adminInboxRow},
		}
		states := sidecartest.NewFakeAclContractStates()

		// each a sidecar's bound client, as its daemon holds one
		sender := newSidecarRouteClient(t, self, &senderId, nil)
		reader := newSidecarRouteClient(t, self, &readerId, nil)
		for _, client := range []*sidecarRouteClient{sender, reader} {
			if status, code, _ := client.bind(client.binding(1)); status != http.StatusOK {
				tb.Fatalf("bind: %d %s", status, code)
			}
		}
		arrival := server.NowUtc().Add(-time.Hour).Truncate(time.Millisecond)
		complaint := &smtpedgeservice.SmtpComplaint{
			ComplaintId:  sidecar.NewId(),
			NetworkId:    sidecar.Id(self.networkId),
			Reporter:     "loop.example",
			Type:         smtpedgeservice.SmtpComplaintTypeAbuse,
			ArrivalTime:  arrival,
			Domain:       domain,
			JoinResult:   "high",
			SealedReport: []byte("sealed under the network mail key"),
		}
		if err := model.InsertSmtpComplaint(ctx, complaint); err != nil {
			tb.Fatal(err)
		}
		reportsPath := fmt.Sprintf("/smtp/reports?since=%d", arrival.Add(-time.Minute).UnixMilli())
		publish := func(client *sidecarRouteClient, args *sidecar.SmtpDkimKeysArgs) (int, []byte) {
			w := self.serve(http.MethodPost, "/smtp/dkim-keys", client.authorization(), args)
			return w.Code, w.Body.Bytes()
		}
		reports := func(client *sidecarRouteClient) (int, []byte) {
			w := self.serve(http.MethodGet, reportsPath, client.authorization(), nil)
			return w.Code, w.Body.Bytes()
		}
		args, selector := testSmtpGateRoutePublishArgs(tb, domain)

		// no check installed, as api's process ran before it installed one: both routes answer 503
		defer model.SetSmtpApiGate(nil)()
		if status, body := publish(sender, args); status != http.StatusServiceUnavailable {
			tb.Errorf("a publish with no check: %d %s", status, body)
		}
		if status, body := reports(reader); status != http.StatusServiceUnavailable {
			tb.Errorf("a reports read with no check: %d %s", status, body)
		}

		// api's checks over its gate cache, which has not loaded the network yet
		defer model.SetSmtpApiGate(model.SmtpApiGateOf(states))()
		if status, body := publish(sender, args); status != http.StatusServiceUnavailable {
			tb.Errorf("a publish before the load: %d %s", status, body)
		}
		if status, body := reports(reader); status != http.StatusServiceUnavailable {
			tb.Errorf("a reports read before the load: %d %s", status, body)
		}
		if key, _ := model.FindSmtpDkimKey(ctx, selector, domain); key != nil {
			tb.Fatal("a publish answered 503 wrote its selector")
		}

		// loaded: the sender publishes, and a sidecar with no mail_send does not
		if err := states.Install(sidecar.Id(self.networkId), rows, view); err != nil {
			tb.Fatal(err)
		}
		otherArgs, otherSelector := testSmtpGateRoutePublishArgs(tb, domain)
		if status, body := publish(reader, otherArgs); status != http.StatusForbidden || testSmtpGateRouteCode(body) != model.SmtpApiCodeMailSend {
			tb.Errorf("a publish with no mail_send: %d %s", status, body)
		}
		if key, _ := model.FindSmtpDkimKey(ctx, otherSelector, domain); key != nil {
			tb.Fatal("a publish with no mail_send wrote its selector")
		}
		status, body := publish(sender, args)
		published := &sidecar.SmtpDkimKeysResult{}
		if status != http.StatusOK || json.Unmarshal(body, published) != nil || len(published.Published) != 1 || published.Published[0] != selector {
			tb.Fatalf("the sender's publish: %d %s", status, body)
		}
		if key, err := model.FindSmtpDkimKey(ctx, selector, domain); err != nil || key == nil || key.SidecarId != senderId {
			tb.Fatalf("the published row %+v, %v", key, err)
		}

		// the admin inbox's reader reads the reports and their sealed copy; the sender, which does not read it, is refused
		status, body = reports(reader)
		page := &model.SmtpReportsResult{}
		if status != http.StatusOK || json.Unmarshal(body, page) != nil || len(page.Complaints) != 1 ||
			page.Complaints[0].ComplaintId != server.Id(complaint.ComplaintId) || string(page.Complaints[0].SealedReport) != string(complaint.SealedReport) {
			tb.Fatalf("the reader's reports: %d %s", status, body)
		}
		if status, body := reports(sender); status != http.StatusForbidden || testSmtpGateRouteCode(body) != model.SmtpApiCodeReportsRead {
			tb.Errorf("a reports read by a sidecar that does not read the mail key's repo: %d %s", status, body)
		}
	})
}
