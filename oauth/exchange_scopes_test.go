package oauth

// The exchange's scopes from the projection (exchange_scopes.go; §7.8, FS-19's
// scope rows that are §7.8's own; ACL-M2-14 runs the table's). Each network's
// log is real: its genesis, base 1 and statements are signed as a first device
// signs them (sidecartest's ConfigLogSuiteWorld), appended through api's own
// sequencer on T2's fake Blobs, and read back by a real gate cache over the
// store (sidecar.AclGateCache over AclGateDirectSource), whose loads the tests
// wait on by its OnLoad hook, never a sleep.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The suite's view of api's sequencer: its ids are sidecar's.
type exchangeScopesSequencer struct {
	sequencer *model.ConfigLogSequencer
}

func (self exchangeScopesSequencer) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	return self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
}

func (self exchangeScopesSequencer) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	return self.sequencer.AppendBase(ctx, server.Id(networkId), &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
}

func (self exchangeScopesSequencer) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	return self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
}

// One network's log at api and a gate cache over the store it is in.
type exchangeScopesWorld struct {
	t              testing.TB
	ctx            context.Context
	networkId      server.Id
	ownerMemberId  server.Id
	ownerSidecarId server.Id
	world          *sidecartest.ConfigLogSuiteWorld
	sequencer      *model.ConfigLogSequencer
	cache          *sidecar.AclGateCache
	loadPins       chan *bridge.AuthPin
	tip            *bridge.AuthPin
}

// A personal network of a fresh owner at base 1, its owner's first device the
// genesis's sidecar, and a gate that has not yet heard of it.
func newExchangeScopesWorld(t testing.TB) *exchangeScopesWorld {
	return newExchangeScopesWorldFor(t, server.NewId(), server.NewId(), server.NewId())
}

func newExchangeScopesWorldFor(t testing.TB, networkId server.Id, ownerMemberId server.Id, ownerSidecarId server.Id) *exchangeScopesWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	world := sidecartest.NewConfigLogSuiteWorldFor(ctx, cluster, sidecar.Id(networkId), sidecar.Id(ownerMemberId), sidecar.Id(ownerSidecarId))
	settings := model.DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := model.NewConfigLogSequencer(cluster, settings, nil)
	if err := world.Start(exchangeScopesSequencer{sequencer: sequencer}); err != nil {
		t.Fatalf("base 1: %v", err)
	}
	self := &exchangeScopesWorld{
		t:              t,
		ctx:            ctx,
		networkId:      networkId,
		ownerMemberId:  ownerMemberId,
		ownerSidecarId: ownerSidecarId,
		world:          world,
		sequencer:      sequencer,
		loadPins:       make(chan *bridge.AuthPin, 64),
	}
	gateSettings := sidecar.DefaultAclGateSettings()
	gateSettings.PollInterval = 0
	gateSettings.FailureBackoff = 0
	gateSettings.Log = sidecar.NewNoopLogger()
	gateSettings.OnLoad = func(loadedNetworkId sidecar.Id, pin *bridge.AuthPin, err error) {
		if loadedNetworkId == sidecar.Id(networkId) && err == nil && pin != nil {
			self.loadPins <- pin
		}
	}
	self.cache = sidecar.NewAclGateCache(ctx, sidecar.NewAclGateDirectSource(nil, blobs, gateSettings), world.Clock(), gateSettings)
	t.Cleanup(self.cache.Close)
	tip, err := sequencer.Tip(ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	self.tip = tip
	return self
}

// The source over this world's gate, on its clock.
func (self *exchangeScopesWorld) source() *ProjectionExchangeScopeSource {
	return NewProjectionExchangeScopeSource(self.cache, &ExchangeScopeSettings{Clock: self.world.Clock()})
}

// Appends signed at the tip and moves the world's tip to it.
func (self *exchangeScopesWorld) post(signed []byte) *bridge.AuthPin {
	self.t.Helper()
	pin, err := self.sequencer.AppendStatement(self.ctx, self.networkId, signed)
	if err != nil {
		self.t.Fatalf("append: %v", err)
	}
	self.world.SetTip(pin)
	self.tip = pin
	return pin
}

// Has the gate load the network through the tip, waiting on its OnLoad hook.
func (self *exchangeScopesWorld) load() *sidecar.AclGateSnapshot {
	self.t.Helper()
	self.cache.Changed(sidecar.Id(self.networkId))
	for {
		select {
		case pin := <-self.loadPins:
			if pin.GetVersion() < self.tip.GetVersion() {
				continue
			}
			snapshot, status := self.cache.State(sidecar.Id(self.networkId))
			connect.AssertEqual(self.t, status, sidecar.AclGateLoaded)
			return snapshot
		case <-time.After(30 * time.Second):
			// a failure's bound only: every load is signalled
			self.t.Fatalf("the gate did not load position %d", self.tip.GetVersion())
		}
	}
}

// A new Ed25519 identity key.
func exchangeScopesIdentityKey(t testing.TB) ed25519.PublicKey {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

// The owner's admission of another device of its member.
func (self *exchangeScopesWorld) admitOwnerDevice(sidecarId server.Id, ephemeral bool, notAfter int64, capabilities uint32) {
	self.t.Helper()
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header:            self.world.Header(),
		NetworkId:         sidecar.Id(self.networkId).Bytes(),
		SidecarId:         sidecar.Id(sidecarId).Bytes(),
		IdentityPublicKey: exchangeScopesIdentityKey(self.t),
		IdentityAlg:       "ed25519",
		MemberId:          sidecar.Id(self.ownerMemberId).Bytes(),
		Ephemeral:         ephemeral,
		NotAfter:          notAfter,
		Capabilities:      capabilities,
	}))
}

// The owner's removal of a device.
func (self *exchangeScopesWorld) revoke(sidecarId server.Id) {
	self.t.Helper()
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
		Header:    self.world.Header(),
		SidecarId: sidecar.Id(sidecarId).Bytes(),
		Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
	}))
}

// The owner's first device files a general repo, becoming its admin.
func (self *exchangeScopesWorld) createGeneralRepo() server.Id {
	self.t.Helper()
	repoId := server.NewId()
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_REPO_CREATE, &protocol.RepoCreate{
		Header: self.world.Header(), RepoId: sidecar.Id(repoId).Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL,
	}))
	return repoId
}

// The network's config repo, as its genesis names it.
func (self *exchangeScopesWorld) configRepoId(snapshot *sidecar.AclGateSnapshot) server.Id {
	self.t.Helper()
	configRepoId, err := sidecar.IdFromBytes(snapshot.Config.Genesis().GetConfigRepoId())
	if err != nil {
		self.t.Fatal(err)
	}
	return server.Id(configRepoId)
}

// A request of a client bound to sidecarId (nil for none) whose credential
// names memberId.
func (self *exchangeScopesWorld) request(sidecarId *server.Id, memberId server.Id, audience string, scopes ...string) *ExchangeScopeRequest {
	clientId := server.NewId()
	return &ExchangeScopeRequest{
		NetworkId:       self.networkId,
		ClientId:        clientId,
		SubjectClientId: clientId,
		SidecarId:       sidecarId,
		MemberId:        &memberId,
		Audience:        audience,
		Scopes:          scopes,
	}
}

// Replaces the owner-row read for one test.
func setExchangeScopeClientTaggedOrEphemeral(t testing.TB, taggedOrEphemeral func(ctx context.Context, clientId server.Id) (bool, error)) {
	previous := exchangeScopeClientTaggedOrEphemeral
	exchangeScopeClientTaggedOrEphemeral = taggedOrEphemeral
	t.Cleanup(func() {
		exchangeScopeClientTaggedOrEphemeral = previous
	})
}

func untaggedExchangeClient(ctx context.Context, clientId server.Id) (bool, error) {
	return false, nil
}

func recoveryScope(memberId server.Id) string {
	return ServiceTokenRecoveryScopePrefix + memberId.String()
}

func repoScope(repoId server.Id, role string) string {
	return ServiceTokenRepoScopePrefix + repoId.String() + ":" + role
}

// §7.8: an unadmitted client of member M gets recovery:<M> at gkm and nothing
// else: not at another audience, not another member's, no repo scope, and
// nothing for a tagged or ephemeral client, a member the state does not hold,
// or a sidecar the state holds a revoked row of.
func TestExchangeScopesUnadmittedClientGetsOnlyItsRecoveryScopeAtGkm(t *testing.T) {
	self := newExchangeScopesWorld(t)
	setExchangeScopeClientTaggedOrEphemeral(t, untaggedExchangeClient)
	revokedSidecarId := server.NewId()
	self.admitOwnerDevice(revokedSidecarId, false, 0, 0)
	self.revoke(revokedSidecarId)
	repoId := self.createGeneralRepo()
	snapshot := self.load()
	source := self.source()
	newSidecarId := server.NewId()

	for _, sidecarId := range []*server.Id{nil, &newSidecarId} {
		grant, err := source.Grant(self.ctx, self.request(sidecarId, self.ownerMemberId, "gkm", recoveryScope(self.ownerMemberId)))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{recoveryScope(self.ownerMemberId)})
		connect.AssertEqual(t, *grant.MemberId, self.ownerMemberId)
		connect.AssertEqual(t, grant.ProjVersion, snapshot.Pin.GetVersion())

		// asked beside a repo scope, the recovery scope alone is granted
		grant, err = source.Grant(self.ctx, self.request(sidecarId, self.ownerMemberId, "gkm", repoScope(repoId, "r"), recoveryScope(self.ownerMemberId)))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{recoveryScope(self.ownerMemberId)})
	}

	otherMemberId := server.NewId()
	for name, request := range map[string]*ExchangeScopeRequest{
		"at evs":                 self.request(&newSidecarId, self.ownerMemberId, "evs", recoveryScope(self.ownerMemberId)),
		"at dns":                 self.request(&newSidecarId, self.ownerMemberId, "dns", recoveryScope(self.ownerMemberId)),
		"another member's":       self.request(&newSidecarId, self.ownerMemberId, "gkm", recoveryScope(otherMemberId)),
		"a repo scope":           self.request(&newSidecarId, self.ownerMemberId, "gkm", repoScope(repoId, "r")),
		"no scope":               self.request(&newSidecarId, self.ownerMemberId, "gkm"),
		"a member not admitted":  self.request(&newSidecarId, otherMemberId, "gkm", recoveryScope(otherMemberId)),
		"a revoked sidecar's":    self.request(&revokedSidecarId, self.ownerMemberId, "gkm", recoveryScope(self.ownerMemberId)),
		"with no member at all":  {NetworkId: self.networkId, ClientId: server.NewId(), SubjectClientId: server.NewId(), Audience: "gkm", Scopes: []string{recoveryScope(self.ownerMemberId)}},
		"another network's":      {NetworkId: server.NewId(), ClientId: server.NewId(), SubjectClientId: server.NewId(), MemberId: &self.ownerMemberId, Audience: "gkm"},
		"the api's audience":     self.request(&newSidecarId, self.ownerMemberId, "api", recoveryScope(self.ownerMemberId)),
		"an unknown audience":    self.request(&newSidecarId, self.ownerMemberId, "smtp", recoveryScope(self.ownerMemberId)),
		"a recovery scope typo":  self.request(&newSidecarId, self.ownerMemberId, "gkm", "recovery:"+self.ownerMemberId.String()+"x"),
		"a recovery scope blank": self.request(&newSidecarId, self.ownerMemberId, "gkm", "recovery:"),
	} {
		grant, err := source.Grant(self.ctx, request)
		if err == nil {
			t.Errorf("%s: granted %v", name, grant.Scopes)
		}
	}

	setExchangeScopeClientTaggedOrEphemeral(t, func(ctx context.Context, clientId server.Id) (bool, error) {
		return true, nil
	})
	_, err := source.Grant(self.ctx, self.request(&newSidecarId, self.ownerMemberId, "gkm", recoveryScope(self.ownerMemberId)))
	requireExchangeRefused(t, "a tagged or ephemeral client", err, ErrInvalidGrant)

	setExchangeScopeClientTaggedOrEphemeral(t, func(ctx context.Context, clientId server.Id) (bool, error) {
		return false, errors.New("the owner row cannot be read")
	})
	_, err = source.Grant(self.ctx, self.request(&newSidecarId, self.ownerMemberId, "gkm", recoveryScope(self.ownerMemberId)))
	requireExchangeRefused(t, "an unreadable owner row", err, ErrExchangeUnavailable)
}

// §7.9's bootstrap: from base 1, repo:<config_repo_id>:admin goes to the
// genesis's sidecar alone, at evs and gkm. The owner's second device, a config
// writer too, never gets it; nor does any audience but evs and gkm; nor the
// first device once it is revoked.
func TestExchangeScopesBootstrapSidecarAloneGetsTheConfigAdmin(t *testing.T) {
	self := newExchangeScopesWorld(t)
	secondSidecarId := server.NewId()
	self.admitOwnerDevice(secondSidecarId, false, 0, 0)
	snapshot := self.load()
	source := self.source()
	configAdmin := repoScope(self.configRepoId(snapshot), "admin")
	connect.AssertEqual(t, snapshot.Config.IsConfigWriter(sidecar.Id(secondSidecarId)), true)

	for _, audience := range []string{"evs", "gkm"} {
		grant, err := source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, audience, configAdmin))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{configAdmin})
		connect.AssertEqual(t, *grant.MemberId, self.ownerMemberId)

		_, err = source.Grant(self.ctx, self.request(&secondSidecarId, self.ownerMemberId, audience, configAdmin))
		requireExchangeRefused(t, "the second device at "+audience, err, ErrInvalidScope)
	}
	_, err := source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "dns", configAdmin))
	requireExchangeRefused(t, "at dns", err, ErrInvalidScope)
	// the id in its other form is the same scope
	upper := ServiceTokenRepoScopePrefix + strings.ToUpper(self.configRepoId(snapshot).String()) + ":admin"
	if _, err := sidecar.ParseId(strings.ToUpper(self.configRepoId(snapshot).String())); err == nil {
		grant, err := source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "evs", upper))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{upper})
	}

	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
		Header:    self.world.Header(),
		SidecarId: sidecar.Id(self.ownerSidecarId).Bytes(),
		Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
	}))
	self.load()
	_, err = source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "evs", configAdmin))
	requireExchangeRefused(t, "the revoked first device", err, ErrInvalidGrant)
}

// proj_v is the position of the state the scopes came from, and moves with it;
// a repo scope the table gives is granted, and one it does not refuses only
// itself.
func TestExchangeScopesProjVersionIsTheStateRead(t *testing.T) {
	self := newExchangeScopesWorld(t)
	repoId := self.createGeneralRepo()
	snapshot := self.load()
	source := self.source()
	otherRepoId := server.NewId()

	grant, err := source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "evs", repoScope(repoId, "admin"), repoScope(otherRepoId, "r"), repoScope(repoId, "r")))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, grant.Scopes, []string{repoScope(repoId, "admin"), repoScope(repoId, "r")})
	connect.AssertEqual(t, grant.ProjVersion, snapshot.Pin.GetVersion())
	connect.AssertEqual(t, grant.ProjVersion, self.tip.GetVersion())

	_, err = source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "evs", repoScope(otherRepoId, "r")))
	requireExchangeRefused(t, "a repo the rows do not hold", err, ErrInvalidScope)

	// no scope asked: a token that names the sidecar and grants nothing
	grant, err = source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "dns"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(grant.Scopes), 0)

	self.admitOwnerDevice(server.NewId(), false, 0, 0)
	moved := self.load()
	grant, err = source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "evs", repoScope(repoId, "r")))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, grant.ProjVersion, moved.Pin.GetVersion())
	connect.AssertEqual(t, grant.ProjVersion, snapshot.Pin.GetVersion()+1)
}

// A cold gate refuses rather than guesses: a network the gate never heard of is
// refused and told to the gate, which loads it, and then it is answered; one
// that is tracked and not loaded is refused.
func TestExchangeScopesColdGateRefuses(t *testing.T) {
	self := newExchangeScopesWorld(t)
	source := self.source()
	request := self.request(&self.ownerSidecarId, self.ownerMemberId, "dns")

	_, err := source.Grant(self.ctx, request)
	requireExchangeRefused(t, "an unknown network", err, ErrExchangeUnavailable)
	select {
	case pin := <-self.loadPins:
		connect.AssertEqual(t, pin.GetVersion(), self.tip.GetVersion())
	case <-time.After(30 * time.Second):
		t.Fatal("the refusal did not start the network's load")
	}
	grant, err := source.Grant(self.ctx, request)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, grant.ProjVersion, self.tip.GetVersion())

	notLoaded := &testExchangeGate{status: sidecar.AclGateNotLoaded}
	_, err = NewProjectionExchangeScopeSource(notLoaded, nil).Grant(self.ctx, request)
	requireExchangeRefused(t, "a network not loaded", err, ErrExchangeUnavailable)
	connect.AssertEqual(t, len(notLoaded.changed), 0)
	unknown := &testExchangeGate{status: sidecar.AclGateUnknown}
	_, err = NewProjectionExchangeScopeSource(unknown, nil).Grant(self.ctx, request)
	requireExchangeRefused(t, "an unknown network", err, ErrExchangeUnavailable)
	connect.AssertEqual(t, unknown.changed, []sidecar.Id{sidecar.Id(self.networkId)})
	_, err = NewProjectionExchangeScopeSource(nil, nil).Grant(self.ctx, request)
	requireExchangeRefused(t, "no gate", err, ErrExchangeUnavailable)

	// the token route answers it 503, retryable
	w := httptest.NewRecorder()
	writeExchangeError(w, err)
	connect.AssertEqual(t, w.Code, http.StatusServiceUnavailable)
	connect.AssertEqual(t, strings.Contains(w.Body.String(), "temporarily_unavailable"), true)
}

// A gate whose answer the test sets.
type testExchangeGate struct {
	snapshot *sidecar.AclGateSnapshot
	status   sidecar.AclGateStatus
	changed  []sidecar.Id
}

func (self *testExchangeGate) State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus) {
	return self.snapshot, self.status
}

func (self *testExchangeGate) Changed(networkId sidecar.Id) {
	self.changed = append(self.changed, networkId)
}

// A sidecar the state holds but whose admission no longer stands takes no
// token, whatever it asks: revoked, and an ephemeral one past its not_after.
func TestExchangeScopesRefuseALapsedAdmission(t *testing.T) {
	self := newExchangeScopesWorld(t)
	removedSidecarId := server.NewId()
	self.admitOwnerDevice(removedSidecarId, false, 0, 0)
	self.revoke(removedSidecarId)
	ephemeralSidecarId := server.NewId()
	notAfter := self.world.Clock().Now().Add(10 * time.Minute)
	self.admitOwnerDevice(ephemeralSidecarId, true, notAfter.UnixMilli(), 0)
	self.load()
	source := self.source()

	_, err := source.Grant(self.ctx, self.request(&removedSidecarId, self.ownerMemberId, "dns"))
	requireExchangeRefused(t, "a revoked sidecar", err, ErrInvalidGrant)

	grant, err := source.Grant(self.ctx, self.request(&ephemeralSidecarId, self.ownerMemberId, "dns"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(grant.Scopes), 0)
	self.world.Clock().AdvanceTo(notAfter)
	_, err = source.Grant(self.ctx, self.request(&ephemeralSidecarId, self.ownerMemberId, "dns"))
	requireExchangeRefused(t, "an ephemeral sidecar at its not_after", err, ErrInvalidGrant)
}

// The exchange with the projection's source installed: an unadmitted client's
// credential names its member, so its recovery scope reaches the token with
// that member and the state's position; the bootstrap sidecar's config admin
// does too.
func TestExchangeWithTheProjectionsScopes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newExchangeFixture(t)
		userId := fixture.userId
		self := newExchangeScopesWorldFor(t, fixture.networkId, userId, server.NewId())
		setExchangeScopeClientTaggedOrEphemeral(t, untaggedExchangeClient)
		snapshot := self.load()
		defer SetExchangeScopeSource(self.source())()

		result, err := Exchange(t.Context(), fixture.args("gkm", recoveryScope(userId)))
		connect.AssertEqual(t, err, nil)
		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "gkm")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Scopes(), []string{recoveryScope(userId)})
		connect.AssertEqual(t, *claims.MemberId, userId)
		connect.AssertEqual(t, claims.ProjVersion, snapshot.Pin.GetVersion())

		_, err = Exchange(t.Context(), fixture.args("evs", recoveryScope(userId)))
		requireExchangeRefused(t, "the recovery scope at evs", err, ErrInvalidGrant)

		fixture.sidecarId = self.ownerSidecarId
		configAdmin := repoScope(self.configRepoId(snapshot), "admin")
		result, err = Exchange(t.Context(), fixture.args("evs", configAdmin))
		connect.AssertEqual(t, err, nil)
		claims, err = newTestServiceTokenVerifier(t).Verify(result.AccessToken, "evs")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Scopes(), []string{configAdmin})
		connect.AssertEqual(t, *claims.SidecarId, self.ownerSidecarId)
	})
}
