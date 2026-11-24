package main

// The platform listener's report tokens (report_token_source.go; plan-SMTP-net-token, §14.6): the block resolves tokens
// only in a binary that links MG6, the column's group, and the source answers an error, never no network, where it does
// not. On the private Postgres with MG6: a zone's own token names its network and a token no zone holds none, and a
// read a context ended under is an error, which the intake answers as the listener's 451, never a raise past it. And
// through the intake as the block composes it, on model's store, a DMARC aggregate at a network's report token counts
// toward that network and is sealed to its mail key, which api's report-key route answers from gkm's real engine
// behind its own guard, under the deposit token api mints itself (QD-1229); another network's token counts toward that
// one alone, sealed to none where its rows name no mail key, and a token no zone holds is dropped unread with no key
// asked. Names are .example and addresses RFC 5737's.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/mxedgeservice"
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
	"github.com/urnetwork/server/sidecarhost"
)

// A network in root with its zone, and the label and report token the zone drew.
func testReportTokenZone(t testing.TB, ctx context.Context, root string) (server.Id, string, string) {
	t.Helper()
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "rpt-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	if _, err := model.EnsureNetworkZone(ctx, networkId, root); err != nil {
		t.Fatal(err)
	}
	label, err := model.EnsureNetworkLabel(ctx, networkId, root)
	if err != nil {
		t.Fatal(err)
	}
	var token *string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT report_token FROM network_dns_zone WHERE root = $1 AND network_id = $2`, root, networkId).Scan(&token))
	})
	if token == nil || len(*token) != 26 {
		t.Fatalf("the zone drew report token %v", token)
	}
	return networkId, label, *token
}

// One DMARC row from the report settings' sending address for headerFrom, signed and aligned under it.
func testReportTokenRecords(headerFrom string, count int) []*sidecartest.SmtpFixtureDmarcRecord {
	return []*sidecartest.SmtpFixtureDmarcRecord{
		{SourceIp: "192.0.2.10", Count: count, HeaderFrom: headerFrom, Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: headerFrom, DkimSelector: "abcdefghijklmnopqrstuvwxyz", DkimResult: "pass"},
	}
}

// The block resolves tokens only where its binary links MG6, keeping the intake's other seams, and the group it then
// waits on is MG6's; without MG6 the intake keeps no token source, and the source itself answers no network nowhere.
func TestReportTokensResolveOnlyInABinaryThatLinksMg6(t *testing.T) {
	groups := reportTokenGroups()
	if len(groups) != 1 || groups[0].Name != "MG6" || groups[0].Linked == nil || groups[0].Linked() != server.SidecarMg6Linked() {
		t.Fatalf("groups %+v", groups)
	}
	world := newTestReportWorld()
	intake, err := newPlatformReportIntake(sidecartest.NewFakeSmtpEdgeStore(nil, nil), testReportIntakeSettings(), world.txt, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := sidecartest.NewFakeSmtpReportKeys()
	if err := intake.sealThrough(keys); err != nil {
		t.Fatal(err)
	}
	if err := intake.resolveTokensThrough(nil); err == nil || intake.deps.Tokens != nil {
		t.Fatal("an intake resolved tokens through no source")
	}
	err = resolveBlockReportTokens(intake)
	if !server.SidecarMg6Linked() {
		if !errors.Is(err, errReportTokenUnlinked) || !strings.Contains(err.Error(), "MG6") || intake.deps.Tokens != nil {
			t.Fatalf("without MG6: %v, %v", err, intake.deps.Tokens)
		}
		_, ok, err := (&reportTokenSource{}).NetworkForReportToken(context.Background(), strings.Repeat("a", 26))
		if ok || !errors.Is(err, errReportTokenUnlinked) {
			t.Fatalf("the source without MG6: %t, %v", ok, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := intake.deps.Tokens.(*reportTokenSource); !ok {
		t.Fatalf("the intake's tokens are %T", intake.deps.Tokens)
	}
	if intake.deps.Keys != keys || intake.deps.Txt != world.txt || intake.deps.Labels == nil || intake.deps.Sink == nil {
		t.Fatal("resolving tokens lost a seam")
	}
}

// On Postgres, each network's zone token names that network and a token of a token's shape that no zone holds names
// none. A read a context ended under is an error, never a raise and never no network, and through the intake it is
// the listener's error, which it answers 451, naming no token.
func TestReportTokenSourceFindsTheZonesNetworkOnPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg6Linked() {
			return
		}
		ctx := context.Background()
		root := "root.example."
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		firstNetworkId, _, firstToken := testReportTokenZone(t, ctx, root)
		secondNetworkId, _, secondToken := testReportTokenZone(t, ctx, root)
		if firstToken == secondToken {
			t.Fatal("two zones drew one token")
		}
		source := &reportTokenSource{}
		for _, c := range []struct {
			token     string
			networkId server.Id
		}{
			{token: firstToken, networkId: firstNetworkId},
			{token: secondToken, networkId: secondNetworkId},
		} {
			networkId, ok, err := source.NetworkForReportToken(ctx, c.token)
			if err != nil || !ok || networkId != sidecar.Id(c.networkId) {
				t.Fatalf("a zone's token: %v, %t, %v", networkId, ok, err)
			}
		}
		if _, ok, err := source.NetworkForReportToken(ctx, strings.Repeat("a", 26)); ok || err != nil {
			t.Fatalf("a token no zone holds: %t, %v", ok, err)
		}

		ended, cancel := context.WithCancel(ctx)
		cancel()
		_, ok, err := source.NetworkForReportToken(ended, firstToken)
		if ok || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), firstToken) {
			t.Fatalf("a read a context ended under: %t, %v", ok, err)
		}
		world := newTestReportWorld()
		store := sidecartest.NewFakeSmtpEdgeStore(nil, nil)
		intake, err := newPlatformReportIntake(store, testReportIntakeSettings(), world.txt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolveBlockReportTokens(intake); err != nil {
			t.Fatal(err)
		}
		recipient := firstToken + "@rpt.report.example"
		raw := world.dmarcAggregate(recipient, "reporter.example", "ended-1", testReportTokenRecords("quietnet.root.example", 5))
		_, err = intake.Accept(ended, &mxedgeservice.PlatformMessage{Recipient: recipient, ArrivalTime: world.arrival, Raw: raw})
		if err == nil || strings.Contains(err.Error(), firstToken) {
			t.Fatalf("the listener's call under an ended context: %v", err)
		}
		if len(store.Aggregates()) != 0 || len(store.Complaints()) != 0 {
			t.Fatal("a report whose token was not read left a row")
		}
	})
}

// The test's issuer and svc- key, under which api mints its own drop-key tokens, and their verifier, which gkm's
// guard verifies with.
func testReportTokenSigner(t testing.TB) *oauth.ServiceTokenVerifier {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oauth.Testing_SetIssuer("https://auth.report.example"))
	t.Cleanup(oauth.Testing_SetSignerKeys(&oauth.SignerKey{
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
		t.Fatal(err)
	}
	return verifier
}

// gkm's view of the networks' verified states, from the rows api's gate cache holds: their acl part, and a zero
// ConfigState, which nothing api's deposit token reaches at gkm reads.
type testReportTokenGkmGate struct {
	states *sidecartest.FakeAclContractStates
}

func (self *testReportTokenGkmGate) State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus) {
	snapshot, status := self.states.State(networkId)
	if status != sidecar.AclGateLoaded || snapshot == nil {
		return nil, status
	}
	return &sidecar.AclGateSnapshot{Pin: snapshot.Pin, Acl: snapshot.Acl, Config: &sidecar.ConfigState{}}, sidecar.AclGateLoaded
}

func (self *testReportTokenGkmGate) Changed(networkId sidecar.Id) {
	self.states.Changed(networkId)
}

func (self *testReportTokenGkmGate) HonorScope(networkId sidecar.Id, subjectId sidecar.Id, repoId sidecar.Id, scope sidecar.AclEvsRights, audience sidecar.AclTokenAudience, now time.Time) sidecar.AclScopeAnswer {
	snapshot := sidecar.RequestAclGateSnapshot(self.states, networkId)
	if snapshot == nil {
		return sidecar.AclScopeUnknown
	}
	return sidecar.AclHonorScope(snapshot.Acl, subjectId, repoId, scope, audience, now)
}

// api's source's states: a network's acl part from the rows, beside the key fixture's newest GkmAuthView, which holds
// the inbox owner's identity key, as one snapshot holds both.
type testReportTokenKeyStates struct {
	states *sidecartest.FakeAclContractStates
	keys   *sidecartest.SmtpReportKeyFixture
}

func (self *testReportTokenKeyStates) ReportKeyState(networkId sidecar.Id) (*sidecar.AclState, sidecar.GkmAuthView, bool) {
	snapshot := sidecar.RequestAclGateSnapshot(self.states, networkId)
	if snapshot == nil {
		return nil, nil, false
	}
	return snapshot.Acl, self.keys.View(), true
}

// One request gkm answered.
type testReportTokenGkmRequest struct {
	path   string
	status int
	bearer string
}

// gkm's handler, recording each request's path, the status it answered and the bearer presented.
type testReportTokenGkmRecorder struct {
	handler http.Handler

	stateLock sync.Mutex
	requests  []*testReportTokenGkmRequest
}

// The status a handler wrote, 200 when it wrote none.
type testReportTokenStatusWriter struct {
	http.ResponseWriter
	status int
}

func (self *testReportTokenStatusWriter) WriteHeader(status int) {
	self.status = status
	self.ResponseWriter.WriteHeader(status)
}

func (self *testReportTokenGkmRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writer := &testReportTokenStatusWriter{ResponseWriter: w, status: http.StatusOK}
	self.handler.ServeHTTP(writer, r)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, &testReportTokenGkmRequest{
		path:   r.URL.Path,
		status: writer.status,
		bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
	})
}

func (self *testReportTokenGkmRecorder) answered() []*testReportTokenGkmRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// A repo row of the rows, created at position at by an admin of its own.
func testReportTokenRepo(kind sidecarprotocol.RepoKind, inbox sidecarprotocol.RepoInbox, at uint64) (sidecar.Id, *sidecarprotocol.AclRepo) {
	repoId := sidecar.NewId()
	creatorId := sidecar.NewId().Bytes()
	return repoId, &sidecarprotocol.AclRepo{
		RepoId: repoId.Bytes(),
		Kind:   kind,
		Inbox:  inbox,
		Admins: []*sidecarprotocol.AclRepoAdminRow{{SidecarId: creatorId, Signer: creatorId, At: at}},
	}
}

// The intake as the block composes it, on Postgres: a Team's aggregate at its zone's report token is counted toward
// it and its raw bytes sealed to the Team's admin inbox's drop key, which api read from gkm's real engine under the
// deposit token api mints itself, gkm admitting it at the drop-key route alone; the copy opens to the report under that
// key for the Team, and not for another network. A second network's token counts toward it alone, sealed to none since
// its rows name no mail key, and a token no zone holds is dropped unread with no key asked of api.
func TestReportAtANetworksTokenIsCountedAndSealedToItsMailKeyOnPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg6Linked() {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := testReportIntakeSettings()
		root := settings.Root + "."
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		teamNetworkId, teamLabel, teamToken := testReportTokenZone(t, ctx, root)
		plainNetworkId, plainLabel, plainToken := testReportTokenZone(t, ctx, root)

		// the Team's verified rows as api's gate cache and gkm's hold them, its admin inbox the network mail key's repo,
		// and the other network's, which name no mail key
		_, directory := testReportTokenRepo(sidecarprotocol.RepoKind_REPO_KIND_DIRECTORY, sidecarprotocol.RepoInbox_REPO_INBOX_NONE, 1)
		inboxId, inbox := testReportTokenRepo(sidecarprotocol.RepoKind_REPO_KIND_GENERAL, sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN, 2)
		states := sidecartest.NewFakeAclContractStates()
		teamView := sidecartest.NewFakeAclView()
		teamView.AddDevice(teamView.AddMember(bridge.MemberRole_MEMBER_ROLE_ADMIN))
		if err := states.Install(sidecar.Id(teamNetworkId), &sidecarprotocol.AclRows{HasDocument: true, Repos: []*sidecarprotocol.AclRepo{directory, inbox}}, teamView); err != nil {
			t.Fatal(err)
		}
		_, plainDirectory := testReportTokenRepo(sidecarprotocol.RepoKind_REPO_KIND_DIRECTORY, sidecarprotocol.RepoInbox_REPO_INBOX_NONE, 1)
		plainView := sidecartest.NewFakeAclView()
		plainView.AddDevice(plainView.AddMember(bridge.MemberRole_MEMBER_ROLE_ADMIN))
		if err := states.Install(sidecar.Id(plainNetworkId), &sidecarprotocol.AclRows{HasDocument: true, Repos: []*sidecarprotocol.AclRepo{plainDirectory}}, plainView); err != nil {
			t.Fatal(err)
		}

		// the inbox keyed by its owner's device, generation 1
		verifier := testReportTokenSigner(t)
		keys, err := sidecartest.NewSmtpReportKeyFixture(ctx, nil, &sidecartest.SmtpReportKeyFixtureArgs{
			RepoId: inboxId,
			Kind:   sidecarprotocol.RepoKind_REPO_KIND_GENERAL,
			Inbox:  sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN,
		})
		if err != nil {
			t.Fatal(err)
		}

		// gkm's engine on a host of its own, its tokens verified as its block verifies them, its state the rows'
		gkmHost := sidecartest.NewFakeHost(ctx, keys.Clock, nil, nil, nil)
		gkmHost.Gates.OpenService(sidecar.ServiceGateGkm, connect.Id(teamNetworkId))
		gkmServiceHost := gkmHost.ServiceHost()
		gkmServiceHost.Authenticator = sidecarhost.NewAuthenticatorWithVerifier(verifier)
		engine, err := gkmservice.NewGkmEngine(ctx, gkmServiceHost, nil, &gkmservice.GkmEngineDeps{
			AuthState: gkmservice.NewAclGkmAuthState(&testReportTokenGkmGate{states: states}, nil, keys.Clock),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		gkmRecorder := &testReportTokenGkmRecorder{handler: engine.Handler()}
		gkm := httptest.NewServer(gkmRecorder)
		defer gkm.Close()

		// the inbox's group in gkm's first metadata cluster as KF1 leaves it, and generation 1
		first, err := gkmHost.Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		put := func(key string, message proto.Message) {
			objectBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.PutIfNoneMatch(ctx, key, objectBytes, nil); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
		}
		groupId, _, ok := keys.Gkm.Newest(inboxId)
		if !ok {
			t.Fatal("the owner keyed no group")
		}
		placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: first.ClusterId(), CreatorSidecarId: keys.Owner.SidecarId}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.PutIfNoneMatch(ctx, sidecar.EvsPlaceKey(inboxId), placeBytes, nil); err != nil {
			t.Fatal(err)
		}
		put(gkmservice.GkmEvsRepoKey(inboxId), &sidecarprotocol.EvsRepoObject{
			RepoId:    inboxId.Bytes(),
			Kind:      sidecarprotocol.RepoKind_REPO_KIND_GENERAL,
			State:     sidecarprotocol.EvsRepoState_EVS_REPO_ACTIVE,
			Format:    1,
			NetworkId: sidecar.Id(teamNetworkId).Bytes(),
		})
		put(gkmservice.GkmGroupOwnerKey(groupId), &sidecarprotocol.GkmGroupOwner{RepoId: inboxId.Bytes(), NetworkId: sidecar.Id(teamNetworkId).Bytes()})
		formBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&sidecarprotocol.GroupForm{RepoId: inboxId.Bytes(), GroupId: groupId.Bytes(), Generation: 1, Auth: keys.View().Pin()})
		if err != nil {
			t.Fatal(err)
		}
		// the GroupForm's signing purpose, as KF1 signs it
		form, err := keys.Owner.Sign("group-form", bridge.SignedType_SIGNED_GROUP_FORM, formBytes, nil)
		if err != nil {
			t.Fatal(err)
		}
		put(gkmservice.GkmFormKey(inboxId, 1), form)
		reply, err := keys.DropKeyReply(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for index, signed := range reply.GetGenerations() {
			put(gkmservice.GkmGenerationKey(groupId, uint64(index+1)), signed)
		}

		// api's source reading gkm under its own tokens, and its report-key route on the platform listener's bearer,
		// counting the networks it was asked for
		gkmReader, err := controller.NewSmtpReportGkmReader(gkm.URL, nil, keys.Clock)
		if err != nil {
			t.Fatal(err)
		}
		source := controller.NewSmtpReportKeySource(&testReportTokenKeyStates{states: states, keys: keys}, gkmReader, keys.Clock, nil)
		askedLock := sync.Mutex{}
		askedNetworkIds := []sidecar.Id{}
		asked := func() []sidecar.Id {
			askedLock.Lock()
			defer askedLock.Unlock()
			return slices.Clone(askedNetworkIds)
		}
		bearerBytes := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
		if _, err := rand.Read(bearerBytes); err != nil {
			t.Fatal(err)
		}
		platformBearer := base64.RawURLEncoding.EncodeToString(bearerBytes)
		digest := sha256.Sum256([]byte(platformBearer))
		gateVerifier, err := server.NewInternalGateVerifier([]byte("mxedge-platform: {class: mxedge-platform, sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"))
		if err != nil {
			t.Fatal(err)
		}
		keyRoutes := handlers.NewInternalGateReportKeyHandlers(&handlers.InternalGateReportKeyDeps{
			Verifier: gateVerifier,
			Key: func(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpReportSealKey, error) {
				func() {
					askedLock.Lock()
					defer askedLock.Unlock()
					askedNetworkIds = append(askedNetworkIds, networkId)
				}()
				return source.ReportSealKey(ctx, networkId)
			},
		}).Routes()
		apiGate := httptest.NewServer(router.NewRouter(ctx, keyRoutes))
		defer apiGate.Close()
		keySource, err := smtpedgeservice.NewSmtpReportKeyGateSource(&smtpedgeservice.SmtpReportKeyGateSourceArgs{ApiUrl: apiGate.URL, Bearer: platformBearer, Clock: keys.Clock})
		if err != nil {
			t.Fatal(err)
		}

		// the intake as the block composes it: model's store, its keys from api's route, its tokens from the zones
		world := newTestReportWorld()
		intake, err := newPlatformReportIntake(model.NewSmtpEdgeStore(nil), settings, world.txt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := intake.sealThrough(keySource); err != nil {
			t.Fatal(err)
		}
		if err := resolveBlockReportTokens(intake); err != nil {
			t.Fatal(err)
		}

		// the Team's report at its token: counted toward it and sealed to its inbox's drop key
		teamRecipient := teamToken + "@rpt.report.example"
		teamDomain := teamLabel + ".root.example"
		teamRaw := world.dmarcAggregate(teamRecipient, "reporter.example", "token-team-1", testReportTokenRecords(teamDomain, 5))
		outcome, err := world.accept(t, intake, teamRecipient, teamRaw)
		if err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
			t.Fatalf("the Team's report: %v, %v", outcome, err)
		}
		teamReports := model.GetSmtpAggregateReports(ctx, teamNetworkId, time.Time{})
		if len(teamReports) != 1 || teamReports[0].ReportId != "token-team-1" || len(teamReports[0].Counts) != 1 ||
			teamReports[0].Counts[0].Domain != teamDomain || teamReports[0].Counts[0].MessageCount != 5 {
			t.Fatalf("the Team's reports %+v", teamReports)
		}
		teamComplaints := model.GetSmtpComplaints(ctx, teamNetworkId, time.Time{})
		if len(teamComplaints) != 1 || teamComplaints[0].Type != smtpedgeservice.SmtpComplaintTypeRua || len(teamComplaints[0].SealedReport) == 0 {
			t.Fatalf("the Team's sealed copies: %d", len(teamComplaints))
		}
		dropKey, err := keys.DropPrivateKey(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := sidecartest.OpenSmtpSealedReportFixture(dropKey, sidecar.Id(teamNetworkId), world.arrival, teamComplaints[0].SealedReport)
		if err != nil || string(opened) != string(teamRaw) {
			t.Fatalf("the copy does not open to the report under the Team's mail key: %v", err)
		}
		if _, err := sidecartest.OpenSmtpSealedReportFixture(dropKey, sidecar.Id(plainNetworkId), world.arrival, teamComplaints[0].SealedReport); err == nil {
			t.Fatal("the Team's copy opened as another network's")
		}
		// gkm answered api's drop-key reads under the token api minted itself: the Team, the inbox's deposit scope
		// alone, and no sidecar or member
		dropKeyPath := "/gkm/repos/" + sidecar.EvsKeyId(inboxId) + "/drop-key"
		reads := gkmRecorder.answered()
		if len(reads) == 0 {
			t.Fatal("api read no drop key from gkm")
		}
		for _, read := range reads {
			claims, err := verifier.Verify(read.bearer, gkmservice.GkmAudience)
			switch {
			case read.path != dropKeyPath || read.status != http.StatusOK:
				t.Fatalf("api asked gkm %s, answered %d", read.path, read.status)
			case err != nil:
				t.Fatalf("api's token does not verify: %v", err)
			case claims.SidecarId != nil || claims.MemberId != nil || claims.NetworkId != teamNetworkId ||
				!slices.Equal(claims.Scopes(), []string{"repo:" + inboxId.String() + ":deposit"}):
				t.Fatalf("api's token names a sidecar, a member, another network or another scope: %v", claims.Scopes())
			}
		}

		// the other network's report at its token: counted toward it alone, sealed to none
		plainRecipient := plainToken + "@rpt.report.example"
		plainDomain := plainLabel + ".root.example"
		plainRaw := world.dmarcAggregate(plainRecipient, "reporter.example", "token-plain-1", testReportTokenRecords(plainDomain, 3))
		if outcome, err := world.accept(t, intake, plainRecipient, plainRaw); err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
			t.Fatalf("the other network's report: %v, %v", outcome, err)
		}
		plainReports := model.GetSmtpAggregateReports(ctx, plainNetworkId, time.Time{})
		if len(plainReports) != 1 || len(plainReports[0].Counts) != 1 || plainReports[0].Counts[0].Domain != plainDomain || plainReports[0].Counts[0].MessageCount != 3 {
			t.Fatalf("the other network's reports %+v", plainReports)
		}
		if complaints := model.GetSmtpComplaints(ctx, plainNetworkId, time.Time{}); len(complaints) != 0 {
			t.Fatalf("%d sealed copies for a network with no mail key", len(complaints))
		}
		if len(model.GetSmtpAggregateReports(ctx, teamNetworkId, time.Time{})) != 1 || len(model.GetSmtpComplaints(ctx, teamNetworkId, time.Time{})) != 1 {
			t.Fatal("the other network's report reached the Team's rows")
		}
		if want := []sidecar.Id{sidecar.Id(teamNetworkId), sidecar.Id(plainNetworkId)}; !slices.Equal(asked(), want) {
			t.Fatalf("api was asked the keys of %v, want %v", asked(), want)
		}

		// a token of a token's shape that no zone holds: dropped unread, no row and no key asked
		unknownRecipient := strings.Repeat("a", 26) + "@rpt.report.example"
		unknownRaw := world.dmarcAggregate(unknownRecipient, "reporter.example", "token-unknown-1", testReportTokenRecords(teamDomain, 7))
		if outcome, err := world.accept(t, intake, unknownRecipient, unknownRaw); err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
			t.Fatalf("a token no zone holds: %v, %v", outcome, err)
		}
		if len(asked()) != 2 || len(model.GetSmtpAggregateReports(ctx, teamNetworkId, time.Time{})) != 1 || len(model.GetSmtpAggregateReports(ctx, plainNetworkId, time.Time{})) != 1 {
			t.Fatal("a report at a token no zone holds was read")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var found bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM smtp_dmarc_daily WHERE report_id = $1)`, "token-unknown-1").Scan(&found))
			if found {
				t.Error("a report at a token no zone holds was counted")
			}
		})
	})
}
