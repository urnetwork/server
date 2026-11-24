package controller

// api's report seal key (plan-SMTP-report-seal-key, QD-075; §14.8, §15.8, D-KEYS-17) over a stand-in gate cache holding
// a Team's verified rows and a mail repo keyed at FakeGkm, read through gkm's drop-key route on the wire: the key is the
// admin inbox's newest drop key, read under api's own token (aud gkm, the repo's deposit scope alone, the asked network,
// no sidecar or member, a client id of its own per token) and kept at most 5 minutes; a removal stops the seal at once
// and its covering rotation reaches it within the bound; gkm's refusal is none, kept as a key is; gkm or a state not
// telling now is unavailable and kept not at all; rows naming no mail repo, or no gkm, answer none without a read.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
)

// The test's issuer and svc- key in place of auth.yml's, and a verifier of the tokens it signs.
func installSmtpReportKeyTestSigner(t *testing.T) *oauth.ServiceTokenVerifier {
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

// The stand-in cache's acl part for the network beside the fixture's newest GkmAuthView, as one snapshot holds both.
type testSmtpReportKeyStates struct {
	states *sidecartest.FakeAclContractStates
	keys   *sidecartest.SmtpReportKeyFixture
}

func (self *testSmtpReportKeyStates) ReportKeyState(networkId sidecar.Id) (*sidecar.AclState, sidecar.GkmAuthView, bool) {
	snapshot := sidecar.RequestAclGateSnapshot(self.states, networkId)
	if snapshot == nil {
		return nil, nil, false
	}
	return snapshot.Acl, self.keys.View(), true
}

// A Team whose admin inbox is keyed at FakeGkm, gkm's drop-key route over it on the wire, and api's source over both.
type testSmtpReportKeyWorld struct {
	t         *testing.T
	ctx       context.Context
	networkId sidecar.Id
	keys      *sidecartest.SmtpReportKeyFixture
	states    *sidecartest.FakeAclContractStates
	source    *SmtpReportKeySource
	// what gkm answers a read with besides 200, and each read's verified claims
	status    atomic.Int64
	claimLock sync.Mutex
	claims    []*oauth.ServiceTokenClaims
}

func newTestSmtpReportKeyWorld(t *testing.T) *testSmtpReportKeyWorld {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	verifier := installSmtpReportKeyTestSigner(t)
	inboxId := sidecar.NewId()
	keys, err := sidecartest.NewSmtpReportKeyFixture(ctx, nil, &sidecartest.SmtpReportKeyFixtureArgs{
		RepoId: inboxId,
		Kind:   sidecarprotocol.RepoKind_REPO_KIND_GENERAL,
		Inbox:  sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN,
	})
	if err != nil {
		t.Fatal(err)
	}
	self := &testSmtpReportKeyWorld{
		t:         t,
		ctx:       ctx,
		networkId: sidecar.NewId(),
		keys:      keys,
		states:    sidecartest.NewFakeAclContractStates(),
	}
	self.status.Store(http.StatusOK)
	self.install(true)
	gkm := httptest.NewServer(keys.DropKeyHandler(func(r *http.Request, repoId sidecar.Id) int {
		claims, err := verifier.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), smtpReportDropKeyAudience)
		if err != nil {
			t.Errorf("gkm was sent a token that does not verify: %v", err)
			return http.StatusUnauthorized
		}
		func() {
			self.claimLock.Lock()
			defer self.claimLock.Unlock()
			self.claims = append(self.claims, claims)
		}()
		return int(self.status.Load())
	}))
	t.Cleanup(gkm.Close)
	reader, err := NewSmtpReportGkmReader(gkm.URL, nil, keys.Clock)
	if err != nil {
		t.Fatal(err)
	}
	self.source = NewSmtpReportKeySource(&testSmtpReportKeyStates{states: self.states, keys: keys}, reader, keys.Clock, nil)
	return self
}

// Installs the Team's rows, with its admin inbox standing or without one.
func (self *testSmtpReportKeyWorld) install(withInbox bool) {
	self.t.Helper()
	view := sidecartest.NewFakeAclView()
	admin := view.AddMember(bridge.MemberRole_MEMBER_ROLE_ADMIN)
	deviceId := view.AddDevice(admin)
	repos := []*sidecarprotocol.AclRepo{{
		RepoId: sidecar.NewId().Bytes(),
		Kind:   sidecarprotocol.RepoKind_REPO_KIND_DIRECTORY,
		Admins: []*sidecarprotocol.AclRepoAdminRow{{SidecarId: deviceId.Bytes(), Signer: deviceId.Bytes(), At: 1}},
	}}
	if withInbox {
		repos = append(repos, &sidecarprotocol.AclRepo{
			RepoId: self.keys.RepoId.Bytes(),
			Kind:   sidecarprotocol.RepoKind_REPO_KIND_GENERAL,
			Inbox:  sidecarprotocol.RepoInbox_REPO_INBOX_ADMIN,
			Admins: []*sidecarprotocol.AclRepoAdminRow{{SidecarId: deviceId.Bytes(), Signer: deviceId.Bytes(), At: 2}},
		})
	}
	if err := self.states.Install(self.networkId, &sidecarprotocol.AclRows{HasDocument: true, Repos: repos}, view); err != nil {
		self.t.Fatal(err)
	}
}

// The source's answer for the world's network.
func (self *testSmtpReportKeyWorld) key() (*smtpedgeservice.SmtpReportSealKey, error) {
	return self.source.ReportSealKey(self.ctx, self.networkId)
}

// The drop key's public half of a generation.
func (self *testSmtpReportKeyWorld) dropPublicKey(generation uint64) []byte {
	self.t.Helper()
	privateKey, err := self.keys.DropPrivateKey(self.ctx, generation)
	if err != nil {
		self.t.Fatal(err)
	}
	return privateKey.PublicKey().Bytes()
}

// Each read's verified claims so far.
func (self *testSmtpReportKeyWorld) readClaims() []*oauth.ServiceTokenClaims {
	self.claimLock.Lock()
	defer self.claimLock.Unlock()
	return slices.Clone(self.claims)
}

func TestSmtpReportKeySourceAnswersTheVerifiedKeyAndKeepsItFiveMinutes(t *testing.T) {
	world := newTestSmtpReportKeyWorld(t)
	key, err := world.key()
	if err != nil || key == nil || key.Generation != 1 || key.RepoId != world.keys.RepoId || string(key.PublicKey) != string(world.dropPublicKey(1)) {
		t.Fatalf("the admin inbox's drop key: %+v, %v", key, err)
	}
	// kept: no read within the bound
	world.keys.Clock.Advance(4 * time.Minute)
	if again, err := world.key(); err != nil || again != key {
		t.Fatalf("the kept key: %v", err)
	}
	if world.keys.DropKeyReadCount() != 1 {
		t.Fatalf("%d reads of gkm within 5 minutes", world.keys.DropKeyReadCount())
	}
	// a removal generation 1 does not cover stops the kept key at once; gkm's newest is still generation 1, so none
	world.keys.Shrink()
	if key, err := world.key(); err != nil || key != nil {
		t.Fatalf("a key past a removal it does not cover: %+v, %v", key, err)
	}
	if world.keys.DropKeyReadCount() != 2 {
		t.Fatal("the removal did not send the source back to gkm")
	}
	// the covering rotation reaches the seal within the bound of that none
	if err := world.keys.Rotate(world.ctx); err != nil {
		t.Fatal(err)
	}
	if key, err := world.key(); err != nil || key != nil || world.keys.DropKeyReadCount() != 2 {
		t.Fatalf("a kept none read gkm again or answered a key: %+v, %v", key, err)
	}
	world.keys.Clock.Advance(5 * time.Minute)
	key, err = world.key()
	if err != nil || key == nil || key.Generation != 2 || string(key.PublicKey) != string(world.dropPublicKey(2)) {
		t.Fatalf("generation 2's key: %+v, %v", key, err)
	}
	// every read under api's own token: aud gkm, the inbox's deposit scope alone, the network, no sidecar or member,
	// and a client id of its own per token
	claims := world.readClaims()
	if len(claims) != 3 {
		t.Fatalf("%d reads", len(claims))
	}
	clientIds := map[server.Id]bool{}
	for _, claim := range claims {
		if claim.Audience() != "gkm" || !slices.Equal(claim.Scopes(), []string{"repo:" + world.keys.RepoId.String() + ":deposit"}) ||
			claim.NetworkId != server.Id(world.networkId) || claim.SidecarId != nil || claim.MemberId != nil ||
			claim.SubjectClientId != nil || claim.Act != nil || claim.ProjVersion != 0 {
			t.Fatalf("a token of %+v", claim)
		}
		clientIds[claim.ClientId] = true
	}
	if len(clientIds) != 3 {
		t.Fatal("two tokens named one client")
	}
}

func TestSmtpReportKeySourceSealsToNoneWhenGkmRefusesTheRead(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized} {
		world := newTestSmtpReportKeyWorld(t)
		world.status.Store(int64(status))
		if key, err := world.key(); err != nil || key != nil {
			t.Fatalf("%d: %+v, %v", status, key, err)
		}
		// kept as a key is: no read again within the bound
		world.status.Store(http.StatusOK)
		if key, err := world.key(); err != nil || key != nil || world.keys.DropKeyReadCount() != 1 {
			t.Fatalf("%d: a kept refusal: %+v, %v, %d reads", status, key, err, world.keys.DropKeyReadCount())
		}
		world.keys.Clock.Advance(5 * time.Minute)
		if key, err := world.key(); err != nil || key == nil {
			t.Fatalf("%d: past the bound: %+v, %v", status, key, err)
		}
	}
}

func TestSmtpReportKeySourceCannotTellWhileGkmOrTheStateIsAway(t *testing.T) {
	world := newTestSmtpReportKeyWorld(t)
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusInternalServerError} {
		world.status.Store(int64(status))
		if key, err := world.key(); !errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) || key != nil {
			t.Fatalf("gkm answering %d: %+v, %v", status, key, err)
		}
	}
	// nothing was kept: the next read answers
	reads := world.keys.DropKeyReadCount()
	world.status.Store(http.StatusOK)
	if key, err := world.key(); err != nil || key == nil || world.keys.DropKeyReadCount() != reads+1 {
		t.Fatalf("after gkm answered again: %+v, %v", key, err)
	}

	// a network the cache has not loaded: unavailable, with no read, and its load asked for
	otherNetworkId := sidecar.NewId()
	if key, err := world.source.ReportSealKey(world.ctx, otherNetworkId); !errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) || key != nil {
		t.Fatalf("a network not loaded: %+v, %v", key, err)
	}
	if !slices.Contains(world.states.ChangedNetworks(), otherNetworkId) {
		t.Fatal("no load was asked for")
	}
	// a verifier behind the generation's pin: unavailable
	world.keys.Clock.Advance(time.Minute)
	world.keys.Shrink()
	if err := world.keys.Rotate(world.ctx); err != nil {
		t.Fatal(err)
	}
	behind := NewSmtpReportKeySource(&testSmtpReportKeyLaggingStates{states: world.states, view: world.keys.ViewAt(1)}, world.source.gkm, world.keys.Clock, nil)
	if key, err := behind.ReportSealKey(world.ctx, world.networkId); !errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) || key != nil {
		t.Fatalf("a state behind the newest generation: %+v, %v", key, err)
	}
}

// The stand-in cache's acl part beside a fixed GkmAuthView, a verifier that lags.
type testSmtpReportKeyLaggingStates struct {
	states *sidecartest.FakeAclContractStates
	view   sidecar.GkmAuthView
}

func (self *testSmtpReportKeyLaggingStates) ReportKeyState(networkId sidecar.Id) (*sidecar.AclState, sidecar.GkmAuthView, bool) {
	snapshot := sidecar.RequestAclGateSnapshot(self.states, networkId)
	if snapshot == nil {
		return nil, nil, false
	}
	return snapshot.Acl, self.view, true
}

func TestSmtpReportKeySourceAnswersNoneWithoutAReadForNoMailRepoOrNoGkm(t *testing.T) {
	world := newTestSmtpReportKeyWorld(t)
	world.install(false)
	if key, err := world.key(); err != nil || key != nil || world.keys.DropKeyReadCount() != 0 {
		t.Fatalf("rows naming no mail repo: %+v, %v", key, err)
	}
	world.install(true)
	none := NewSmtpReportKeySource(&testSmtpReportKeyStates{states: world.states, keys: world.keys}, nil, world.keys.Clock, nil)
	if key, err := none.ReportSealKey(world.ctx, world.networkId); err != nil || key != nil || world.keys.DropKeyReadCount() != 0 {
		t.Fatalf("no gkm: %+v, %v", key, err)
	}
	if source := newApiSmtpReportKeySource(world.states, "", world.keys.Clock); source.gkm != nil {
		t.Fatal("a source with no gkm url reads gkm")
	}
	if source := newApiSmtpReportKeySource(world.states, "ftp://gkm.report.example", world.keys.Clock); source.gkm != nil {
		t.Fatal("a source with a gkm url that is no gkm's reads gkm")
	}
	if _, err := NewSmtpReportGkmReader("gkm.report.example/", nil, nil); err == nil {
		t.Fatal("a reader of a url that is none")
	}
	// the gate cache's own snapshot holds no ConfigState here, so the cache's states read it as not loaded
	if _, _, ok := SmtpReportKeyStatesOf(world.states).ReportKeyState(world.networkId); ok {
		t.Fatal("a snapshot with no ConfigState read as loaded")
	}
}

func TestSmtpReportKeySourceInstallsAndRestores(t *testing.T) {
	previous := CurrentSmtpReportKeySource()
	source := NewSmtpReportKeySource(nil, nil, nil, nil)
	restore := SetSmtpReportKeySource(source)
	if CurrentSmtpReportKeySource() != source {
		t.Fatal("the installed source is not current")
	}
	restore()
	if CurrentSmtpReportKeySource() != previous {
		t.Fatal("the restore left the source")
	}
	if key, err := source.ReportSealKey(context.Background(), sidecar.NewId()); !errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) || key != nil {
		t.Fatalf("a source with no states: %+v, %v", key, err)
	}
}

// api's projection installs the source over the gate cache it publishes, which answers a network that cache has not
// loaded unavailable, and its close takes the source out again.
func TestApiProjectionInstallsTheReportKeySource(t *testing.T) {
	before := CurrentSmtpReportKeySource()
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	closeViews, err := installApiProjection(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	source := CurrentSmtpReportKeySource()
	if source == nil || source == before {
		closeViews()
		t.Fatal("the projection installed no report key source")
	}
	if key, err := source.ReportSealKey(t.Context(), sidecar.NewId()); !errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) || key != nil {
		t.Errorf("a network the cache has not loaded: %+v, %v", key, err)
	}
	closeViews()
	if CurrentSmtpReportKeySource() != before {
		t.Fatal("the closed projection left its report key source installed")
	}
}
