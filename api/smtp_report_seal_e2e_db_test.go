//go:build sidecar_pending

package api

// §14.8's sealed report copy end to end through gkm's own guard (QD-1229; plan-SMTP-report-seal-key, §7.8, §10.14,
// §15.8, D-KEYS-17), against the database with MG5's report tables. smtp_report_seal_route_db_test.go reads the drop
// key from FakeGkm's handler behind a stand-in check; here gkm is its real engine (gkmservice.GkmEngine) behind its
// real guard, authenticating with the svc- verifier gkm's block uses (sidecarhost.NewAuthenticatorWithVerifier) and
// judging by AclGkmAuthState over the network's verified rows, the same rows api's gate cache holds. The Team's admin
// inbox is keyed by its owner's device (sidecartest.SmtpReportKeyFixture), and its group lies in gkm's first metadata
// cluster as KF1 and the owner's rotations leave it: place/, repo/, the GroupForm, the owner marker and each gen/<g>.
//
// api mints its own deposit token for each read (aud gkm, repo:<inbox>:deposit alone, no sidecar or member), gkm
// admits it at the drop-key route, and api checks the GenRotates gkm serves and answers the inbox's drop key on its
// report-key route; the platform listener's intake seals a DMARC aggregate at the network's token to it, and GET
// /smtp/reports through api's router serves the copy to the inbox's reader, who opens it to the report's bytes. The
// same token is refused at every other gkm route it is tried on. After a removal the inbox's generation does not
// cover, the next report counts with no copy; once the owner has rotated, gkm holds the new generation and api's
// 5-minute answer has lapsed, the next report is sealed to the new generation's key, which the old one does not open.

import (
	"context"
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
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// gkm's own view of the network's verified state, from the rows api's gate cache holds: their acl part, and a zero
// ConfigState as the config part, which nothing api's deposit token reaches at gkm reads.
type testReportSealGkmGate struct {
	states *sidecartest.FakeAclContractStates
}

func (self *testReportSealGkmGate) State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus) {
	snapshot, status := self.states.State(networkId)
	if status != sidecar.AclGateLoaded || snapshot == nil {
		return nil, status
	}
	return &sidecar.AclGateSnapshot{Pin: snapshot.Pin, Acl: snapshot.Acl, Config: &sidecar.ConfigState{}}, sidecar.AclGateLoaded
}

func (self *testReportSealGkmGate) Changed(networkId sidecar.Id) {
	self.states.Changed(networkId)
}

// ACL's own scope check over the rows, which a sidecar's token alone asks.
func (self *testReportSealGkmGate) HonorScope(networkId sidecar.Id, subjectId sidecar.Id, repoId sidecar.Id, scope sidecar.AclEvsRights, audience sidecar.AclTokenAudience, now time.Time) sidecar.AclScopeAnswer {
	snapshot := sidecar.RequestAclGateSnapshot(self.states, networkId)
	if snapshot == nil {
		return sidecar.AclScopeUnknown
	}
	return sidecar.AclHonorScope(snapshot.Acl, subjectId, repoId, scope, audience, now)
}

// One request gkm answered: its route's path, its status, and the bearer as presented.
type testReportSealGkmRequest struct {
	method string
	path   string
	status int
	bearer string
}

// gkm's handler, recording what it answered each request.
type testReportSealGkmRecorder struct {
	handler http.Handler

	stateLock sync.Mutex
	requests  []*testReportSealGkmRequest
}

// The status a handler wrote, 200 when it wrote none.
type testReportSealStatusWriter struct {
	http.ResponseWriter
	status int
}

func (self *testReportSealStatusWriter) WriteHeader(status int) {
	self.status = status
	self.ResponseWriter.WriteHeader(status)
}

func (self *testReportSealGkmRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writer := &testReportSealStatusWriter{ResponseWriter: w, status: http.StatusOK}
	self.handler.ServeHTTP(writer, r)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, &testReportSealGkmRequest{
		method: r.Method,
		path:   r.URL.Path,
		status: writer.status,
		bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
	})
}

// The requests gkm has answered so far, in order.
func (self *testReportSealGkmRecorder) answered() []*testReportSealGkmRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

func TestReportSealedThroughGkmsGuardReadsBackThroughApisGate(t *testing.T) {
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

		// the network's verified rows as api's gate cache and gkm's hold them: a Team whose admin inbox one sidecar
		// reads
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

		// the inbox keyed by its owner's device, generation 1
		verifier := installReportSealTestSigner(tb)
		keys, err := sidecartest.NewSmtpReportKeyFixture(ctx, nil, &sidecartest.SmtpReportKeyFixtureArgs{
			RepoId: inboxId,
			Kind:   sidecarprotocol.RepoKind_REPO_KIND_GENERAL,
			Inbox:  sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN,
		})
		if err != nil {
			tb.Fatal(err)
		}

		// gkm's engine on a host of its own, its tokens verified as its block verifies them, its state the rows'
		gkmHost := sidecartest.NewFakeHost(ctx, keys.Clock, nil, nil, nil)
		gkmHost.Gates.OpenService(sidecar.ServiceGateGkm, connect.Id(networkId))
		gkmServiceHost := gkmHost.ServiceHost()
		gkmServiceHost.Authenticator = sidecarhost.NewAuthenticatorWithVerifier(verifier)
		engine, err := gkmservice.NewGkmEngine(ctx, gkmServiceHost, nil, &gkmservice.GkmEngineDeps{
			AuthState: gkmservice.NewAclGkmAuthState(&testReportSealGkmGate{states: states}, nil, keys.Clock),
		})
		if err != nil {
			tb.Fatal(err)
		}
		defer engine.Close()
		gkmRecorder := &testReportSealGkmRecorder{handler: engine.Handler()}
		gkm := httptest.NewServer(gkmRecorder)
		defer gkm.Close()

		// the inbox's group in gkm's first metadata cluster as KF1 leaves it, and each generation the owner claims
		first, err := gkmHost.Blobs.FirstCluster()
		if err != nil {
			tb.Fatal(err)
		}
		put := func(key string, message proto.Message) {
			objectBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
			if err != nil {
				tb.Fatal(err)
			}
			if _, err := first.PutIfNoneMatch(ctx, key, objectBytes, nil); err != nil {
				tb.Fatalf("put %s: %v", key, err)
			}
		}
		groupId, _, ok := keys.Gkm.Newest(inboxId)
		if !ok {
			tb.Fatal("the owner keyed no group")
		}
		placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: first.ClusterId(), CreatorSidecarId: keys.Owner.SidecarId}).Marshal()
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := first.PutIfNoneMatch(ctx, sidecar.EvsPlaceKey(inboxId), placeBytes, nil); err != nil {
			tb.Fatal(err)
		}
		put(gkmservice.GkmEvsRepoKey(inboxId), &sidecarprotocol.EvsRepoObject{
			RepoId:    inboxId.Bytes(),
			Kind:      sidecarprotocol.RepoKind_REPO_KIND_GENERAL,
			State:     sidecarprotocol.EvsRepoState_EVS_REPO_ACTIVE,
			Format:    1,
			NetworkId: networkId.Bytes(),
		})
		put(gkmservice.GkmGroupOwnerKey(groupId), &sidecarprotocol.GkmGroupOwner{RepoId: inboxId.Bytes(), NetworkId: networkId.Bytes()})
		formBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&sidecarprotocol.GroupForm{RepoId: inboxId.Bytes(), GroupId: groupId.Bytes(), Generation: 1, Auth: keys.View().Pin()})
		if err != nil {
			tb.Fatal(err)
		}
		// the GroupForm's signing purpose, as KF1 signs it
		form, err := keys.Owner.Sign("group-form", bridge.SignedType_SIGNED_GROUP_FORM, formBytes, nil)
		if err != nil {
			tb.Fatal(err)
		}
		put(gkmservice.GkmFormKey(inboxId, 1), form)
		filedCount := uint64(0)
		fileGenerations := func() {
			reply, err := keys.DropKeyReply(ctx, filedCount)
			if err != nil {
				tb.Fatal(err)
			}
			for _, signed := range reply.GetGenerations() {
				filedCount += 1
				put(gkmservice.GkmGenerationKey(groupId, filedCount), signed)
			}
		}
		fileGenerations()

		// api's source reading gkm under its own tokens, and its report-key route on the platform listener's bearer
		gkmReader, err := controller.NewSmtpReportGkmReader(gkm.URL, nil, keys.Clock)
		if err != nil {
			tb.Fatal(err)
		}
		source := controller.NewSmtpReportKeySource(&testReportSealStates{states: states, keys: keys}, gkmReader, keys.Clock, nil)
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
		intakeHost := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
		intake, err := smtpedgeservice.NewSmtpReportIntake(intakeHost.ServiceHost(), model.NewSmtpEdgeStore(nil), &smtpedgeservice.SmtpReportIntakeDeps{
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
		dropKeyPath := "/gkm/repos/" + sidecar.EvsKeyId(inboxId) + "/drop-key"
		// gkm's drop-key reads since the count before, each api's own token as gkm's verifier reads it: 200, the
		// asked network, the inbox's deposit scope alone, and no sidecar or member
		checkDropKeyReads := func(before int, what string) []*testReportSealGkmRequest {
			reads := []*testReportSealGkmRequest{}
			for _, request := range gkmRecorder.answered()[before:] {
				if request.path != dropKeyPath {
					tb.Fatalf("%s: api asked gkm %s %s", what, request.method, request.path)
				}
				claims, err := verifier.Verify(request.bearer, gkmservice.GkmAudience)
				switch {
				case request.status != http.StatusOK:
					tb.Fatalf("%s: gkm answered api's drop-key read %d", what, request.status)
				case err != nil:
					tb.Fatalf("%s: api's token does not verify: %v", what, err)
				case claims.SidecarId != nil || claims.MemberId != nil || claims.NetworkId != self.networkId ||
					!slices.Equal(claims.Scopes(), []string{"repo:" + inboxId.String() + ":deposit"}):
					tb.Fatalf("%s: api's token names a sidecar, a member, another network or another scope: %v", what, claims.Scopes())
				}
				reads = append(reads, request)
			}
			if len(reads) == 0 {
				tb.Fatalf("%s: api read no drop key from gkm", what)
			}
			return reads
		}

		raw := report("seal-gkm-1")
		accept(raw, arrival)
		reads := checkDropKeyReads(0, "the first report")

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
		if len(page.Complaints[0].SealedReport) == 0 {
			tb.Fatal("the report was kept with no sealed copy: gkm's drop key did not reach api")
		}
		dropKey1, err := keys.DropPrivateKey(ctx, 1)
		if err != nil {
			tb.Fatal(err)
		}
		opened, err := sidecartest.OpenSmtpSealedReportFixture(dropKey1, networkId, arrival, page.Complaints[0].SealedReport)
		if err != nil || string(opened) != string(raw) {
			tb.Fatalf("the copy does not open to the report under the inbox's drop key: %v", err)
		}
		if status, _ := read(outsider); status != http.StatusForbidden {
			tb.Fatalf("a sidecar that does not read the inbox: %d", status)
		}

		// api's own token reaches gkm's drop key alone: every other route it is tried on refuses it
		token := reads[len(reads)-1].bearer
		groupPath := "/gkm/groups/" + sidecar.EvsKeyId(groupId)
		for _, probe := range []struct{ method, path string }{
			{method: http.MethodGet, path: "/gkm/repos/" + sidecar.EvsKeyId(inboxId) + "/forms"},
			{method: http.MethodGet, path: groupPath},
			{method: http.MethodGet, path: groupPath + "/gen?since=0"},
			{method: http.MethodGet, path: groupPath + "/wraps/1"},
			{method: http.MethodPost, path: groupPath + "/gen/2"},
			{method: http.MethodPost, path: "/gkm/reader-key"},
			{method: http.MethodGet, path: "/gkm/reader-key/" + sidecar.EvsKeyId(readerId)},
			{method: http.MethodGet, path: "/gkm/recovery/" + sidecar.EvsKeyId(admin) + "/wraps"},
		} {
			request, err := http.NewRequestWithContext(ctx, probe.method, gkm.URL+probe.path, nil)
			if err != nil {
				tb.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := gkm.Client().Do(request)
			if err != nil {
				tb.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				tb.Fatalf("api's token at %s %s: %d, want 403", probe.method, probe.path, response.StatusCode)
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, gkm.URL+dropKeyPath+"?since=0", nil)
		if err != nil {
			tb.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := gkm.Client().Do(request)
		if err != nil {
			tb.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			tb.Fatalf("api's token at the drop key once more: %d", response.StatusCode)
		}

		// a removal the inbox's generation does not cover: the next report counts and leaves no copy
		keys.Clock.Advance(time.Minute)
		keys.Shrink()
		before := len(gkmRecorder.answered())
		accept(report("seal-gkm-2"), arrival.Add(time.Second))
		checkDropKeyReads(before, "the report after the removal")
		if status, page := read(reader); status != http.StatusOK || len(page.Complaints) != 1 || len(page.Aggregates) != 2 {
			tb.Fatalf("after the removal: %d, %d complaints, %d aggregates", status, len(page.Complaints), len(page.Aggregates))
		}

		// the owner rotates for the removal and gkm holds generation 2: once api's answer of none has lapsed, the
		// next report is sealed to the new generation, which the old drop key does not open
		if err := keys.Rotate(ctx); err != nil {
			tb.Fatal(err)
		}
		fileGenerations()
		if filedCount != 2 {
			tb.Fatalf("gkm holds %d generations, want 2", filedCount)
		}
		keys.Clock.Advance(controller.DefaultSmtpReportKeySourceSettings().CacheLifetime + time.Second)
		before = len(gkmRecorder.answered())
		rotatedRaw := report("seal-gkm-3")
		accept(rotatedRaw, arrival.Add(2*time.Second))
		checkDropKeyReads(before, "the report after the rotation")
		status, page = read(reader)
		if status != http.StatusOK || len(page.Complaints) != 2 || len(page.Aggregates) != 3 {
			tb.Fatalf("after the rotation: %d, %d complaints, %d aggregates", status, len(page.Complaints), len(page.Aggregates))
		}
		rotated := page.Complaints[1].SealedReport
		dropKey2, err := keys.DropPrivateKey(ctx, 2)
		if err != nil {
			tb.Fatal(err)
		}
		opened, err = sidecartest.OpenSmtpSealedReportFixture(dropKey2, networkId, arrival.Add(2*time.Second), rotated)
		if err != nil || string(opened) != string(rotatedRaw) {
			tb.Fatalf("the copy does not open under the rotated drop key: %v", err)
		}
		if _, err := sidecartest.OpenSmtpSealedReportFixture(dropKey1, networkId, arrival.Add(2*time.Second), rotated); err == nil {
			tb.Fatal("the old generation's drop key opened a copy sealed after the removal")
		}
	})
}
