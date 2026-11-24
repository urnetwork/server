//go:build sidecar_pending

package api

// §14.8's sealed report copy end to end (plan-SMTP-report-seal-key, QD-075; §15.8, D-KEYS-17), against the database
// with MG5's report tables: a Team's admin inbox keyed at FakeGkm, gkm's drop-key route on the wire under api's own
// token, api's report-key route over its source on the platform listener's bearer, and the platform listener's intake
// on the model's store with its key source on that route. A DMARC aggregate at the network's token is counted and its
// raw bytes sealed to the inbox's newest drop key; GET /smtp/reports through api's router, over the reports read's §8
// check on the same verified rows (model.SmtpApiGateOf), serves the sealed copy to the inbox's reader, which opens it
// to the report's bytes under the drop key's private half, and refuses a sidecar that does not read the inbox. After a
// removal the inbox's generation does not cover, the next report's counts are kept and no copy is stored.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
)

// The stand-in cache's acl part for the network beside the fixture's newest GkmAuthView, as one snapshot holds both.
type testReportSealStates struct {
	states *sidecartest.FakeAclContractStates
	keys   *sidecartest.SmtpReportKeyFixture
}

func (self *testReportSealStates) ReportKeyState(networkId sidecar.Id) (*sidecar.AclState, sidecar.GkmAuthView, bool) {
	snapshot := sidecar.RequestAclGateSnapshot(self.states, networkId)
	if snapshot == nil {
		return nil, nil, false
	}
	return snapshot.Acl, self.keys.View(), true
}

// The test's issuer and svc- key, which api's own drop-key tokens are signed under, and their verifier.
func installReportSealTestSigner(tb testing.TB) *oauth.ServiceTokenVerifier {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(oauth.Testing_SetIssuer("https://auth.report.example"))
	tb.Cleanup(oauth.Testing_SetSignerKeys(&oauth.SignerKey{
		Kid:        oauth.SignerKeySetService.KidPrefix() + thumbprint,
		Alg:        "ES256",
		Set:        oauth.SignerKeySetService,
		PrivateKey: privateKey,
	}))
	verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: "https://auth.report.example",
		Keys:   oauth.PublicJwks(oauth.SignerKeySetService).Keys,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return verifier
}

func TestReportSealedToTheNetworkMailKeyReadsBackThroughApisGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(fmt.Sprintf(
			"sidecar:\n  preview_networks: [%q]\n  smtpedge: {enabled: true, networks: [%q]}\n",
			self.networkId,
			self.networkId,
		))()
		networkId := sidecar.Id(self.networkId)

		// the network's verified rows as api's gate cache holds them: a Team whose admin inbox one sidecar reads
		view := sidecartest.NewFakeAclView()
		admin := view.AddMember(bridge.MemberRole_MEMBER_ROLE_ADMIN)
		readerId, outsiderId := view.AddDevice(admin), view.AddDevice(admin)
		_, directory := testSmtpGateRouteRepo(sidecarprotocol.RepoKind_REPO_KIND_DIRECTORY, sidecarprotocol.RepoInbox_REPO_INBOX_NONE, 1)
		inboxId, inboxRow := testSmtpGateRouteRepo(sidecarprotocol.RepoKind_REPO_KIND_GENERAL, sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN, 2)
		rows := &sidecarprotocol.AclRows{
			HasDocument: true,
			Atoms:       []*sidecarprotocol.AclAtom{sidecartest.FakeAclSidecarAtom(readerId), sidecartest.FakeAclSidecarAtom(outsiderId)},
			Rules: []*sidecarprotocol.AclRule{
				testSmtpGateRouteRule(readerId, []*sidecarprotocol.AclTarget{{RepoId: inboxId.Bytes()}}, sidecarprotocol.AclAction_ACL_ACTION_READ),
			},
			Repos: []*sidecarprotocol.AclRepo{directory, inboxRow},
		}
		states := sidecartest.NewFakeAclContractStates()
		if err := states.Install(networkId, rows, view); err != nil {
			tb.Fatal(err)
		}
		defer model.SetSmtpApiGate(model.SmtpApiGateOf(states))()

		// the inbox keyed at FakeGkm, gkm's drop-key route over it taking only api's own deposit token for the inbox
		verifier := installReportSealTestSigner(tb)
		keys, err := sidecartest.NewSmtpReportKeyFixture(ctx, nil, &sidecartest.SmtpReportKeyFixtureArgs{
			RepoId: inboxId,
			Kind:   sidecarprotocol.RepoKind_REPO_KIND_GENERAL,
			Inbox:  sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN,
		})
		if err != nil {
			tb.Fatal(err)
		}
		gkm := httptest.NewServer(keys.DropKeyHandler(func(r *http.Request, repoId sidecar.Id) int {
			claims, err := verifier.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "gkm")
			if err != nil {
				return http.StatusUnauthorized
			}
			if claims.SidecarId != nil || claims.MemberId != nil || claims.NetworkId != self.networkId ||
				!slices.Equal(claims.Scopes(), []string{"repo:" + repoId.String() + ":deposit"}) {
				return http.StatusForbidden
			}
			return http.StatusOK
		}))
		defer gkm.Close()
		gkmReader, err := controller.NewSmtpReportGkmReader(gkm.URL, nil, keys.Clock)
		if err != nil {
			tb.Fatal(err)
		}
		source := controller.NewSmtpReportKeySource(&testReportSealStates{states: states, keys: keys}, gkmReader, keys.Clock, nil)

		// api's report-key route over the source, on a verify file holding the platform listener's digest
		bearerBytes := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
		if _, err := rand.Read(bearerBytes); err != nil {
			tb.Fatal(err)
		}
		platformBearer := base64.RawURLEncoding.EncodeToString(bearerBytes)
		digest := sha256.Sum256([]byte(platformBearer))
		gateVerifier, err := server.NewInternalGateVerifier([]byte("mxedge-platform: {class: mxedge-platform, sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"))
		if err != nil {
			tb.Fatal(err)
		}
		keyRoutes := handlers.NewInternalGateReportKeyHandlers(&handlers.InternalGateReportKeyDeps{Verifier: gateVerifier, Key: source.ReportSealKey}).Routes()
		apiGate := httptest.NewServer(router.NewRouter(ctx, keyRoutes))
		defer apiGate.Close()

		// the platform listener's intake on the model's store, its keys from api's route on its bearer
		keySource, err := smtpedgeservice.NewSmtpReportKeyGateSource(&smtpedgeservice.SmtpReportKeyGateSourceArgs{ApiUrl: apiGate.URL, Bearer: platformBearer, Clock: keys.Clock})
		if err != nil {
			tb.Fatal(err)
		}
		settings := smtpedgeservice.DefaultSmtpEdgeSettings()
		settings.Root = "root.example"
		settings.ReportHostName = "rpt.report.example"
		settings.SendingAddresses = []smtpedgeservice.SmtpSendingAddress{{Address: netip.MustParseAddr("192.0.2.10"), HeloName: "o1.out.report.example"}}
		txt := sidecartest.NewFakeSmtpDnsTxt()
		reporterKey := sidecartest.NewSmtpDkimFixtureRsaKey("rpt1", "reporter.example")
		txt.Set(reporterKey.RecordName(), reporterKey.TxtRecord())
		tokens := sidecartest.NewFakeSmtpNetTokens()
		recipient := tokens.Add(networkId) + "@rpt.report.example"
		host := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
		intake, err := smtpedgeservice.NewSmtpReportIntake(host.ServiceHost(), model.NewSmtpEdgeStore(nil), &smtpedgeservice.SmtpReportIntakeDeps{
			Tokens: tokens,
			Keys:   keySource,
			Txt:    txt,
			Sink:   sidecartest.NewFakeSmtpAbuseSink(),
		}, settings)
		if err != nil {
			tb.Fatal(err)
		}
		arrival := server.NowUtc().Add(-time.Hour).Truncate(time.Millisecond)
		report := func(reportId string) []byte {
			xml := sidecartest.BuildSmtpDmarcXml("reporter.example", reportId, arrival, []*sidecartest.SmtpFixtureDmarcRecord{
				{SourceIp: "192.0.2.10", Count: 5, HeaderFrom: "quietnet.root.example", Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: "quietnet.root.example", DkimSelector: "abcdefghijklmnopqrstuvwxyz", DkimResult: "pass"},
			})
			message := sidecartest.BuildSmtpAggregateMessage(reporterKey.Domain, recipient, "application/gzip", "report.xml.gz", sidecartest.SmtpFixtureGzip(xml))
			return sidecartest.SignSmtpDkimFixture(message, reporterKey, &sidecartest.SmtpDkimFixtureOptions{HeaderRelaxed: true, BodyRelaxed: true, Headers: []string{"from", "to", "subject", "date", "message-id"}})
		}
		accept := func(raw []byte, at time.Time) {
			outcome, err := intake.Accept(ctx, &smtpedgeservice.SmtpReportMessage{Recipient: recipient, ArrivalTime: at, Raw: raw})
			if err != nil || outcome != smtpedgeservice.SmtpReportOutcomeCounted {
				tb.Fatalf("the report: %s, %v", outcome, err)
			}
		}
		raw := report("seal-1")
		accept(raw, arrival)

		// read back through api's router by the inbox's reader, each caller a sidecar's bound client
		reader := newSidecarRouteClient(t, self, &readerId, nil)
		outsider := newSidecarRouteClient(t, self, &outsiderId, nil)
		for _, client := range []*sidecarRouteClient{reader, outsider} {
			if status, code, _ := client.bind(client.binding(1)); status != http.StatusOK {
				tb.Fatalf("bind: %d %s", status, code)
			}
		}
		reportsPath := fmt.Sprintf("/smtp/reports?since=%d", arrival.Add(-time.Minute).UnixMilli())
		read := func(client *sidecarRouteClient) (int, *model.SmtpReportsResult) {
			w := self.serve(http.MethodGet, reportsPath, client.authorization(), nil)
			page := &model.SmtpReportsResult{}
			if w.Code == http.StatusOK {
				if err := json.Unmarshal(w.Body.Bytes(), page); err != nil {
					tb.Fatal(err)
				}
			}
			return w.Code, page
		}
		status, page := read(reader)
		if status != http.StatusOK || len(page.Complaints) != 1 || len(page.Aggregates) != 1 {
			tb.Fatalf("the reader's reports: %d, %d complaints, %d aggregates", status, len(page.Complaints), len(page.Aggregates))
		}
		home := page.Complaints[0]
		if home.Type != smtpedgeservice.SmtpComplaintTypeRua || len(home.SealedReport) == 0 {
			tb.Fatalf("the sealed copy's row: %s, %d bytes", home.Type, len(home.SealedReport))
		}
		dropKey, err := keys.DropPrivateKey(ctx, 1)
		if err != nil {
			tb.Fatal(err)
		}
		opened, err := sidecartest.OpenSmtpSealedReportFixture(dropKey, networkId, arrival, home.SealedReport)
		if err != nil || string(opened) != string(raw) {
			tb.Fatalf("the copy does not open to the report under the inbox's drop key: %v", err)
		}
		if _, err := sidecartest.OpenSmtpSealedReportFixture(dropKey, sidecar.NewId(), arrival, home.SealedReport); err == nil {
			tb.Fatal("the copy opened as another network's")
		}
		if status, _ := read(outsider); status != http.StatusForbidden {
			tb.Fatalf("a sidecar that does not read the inbox: %d", status)
		}

		// a removal the inbox's generation does not cover: the next report counts and leaves no copy
		keys.Clock.Advance(time.Minute)
		keys.Shrink()
		later := arrival.Add(time.Second)
		accept(report("seal-2"), later)
		if status, page := read(reader); status != http.StatusOK || len(page.Complaints) != 1 || len(page.Aggregates) != 2 {
			tb.Fatalf("after the removal: %d, %d complaints, %d aggregates", status, len(page.Complaints), len(page.Aggregates))
		}
	})
}
