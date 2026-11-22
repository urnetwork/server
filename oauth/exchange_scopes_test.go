package oauth

// The exchange's scopes from the projection (exchange_scopes.go; §7.8, FS-19's
// scope rows that are §7.8's own; ACL-M2-14 runs the table's). Each network's
// log is real: its genesis, base 1 and statements are signed as a first device
// signs them (sidecartest's ConfigLogSuiteWorld), appended through api's own
// sequencer on T2's fake Blobs, and read back by a real gate cache over the
// store (sidecar.AclGateCache over AclGateDirectSource), whose loads the tests
// wait on by its OnLoad hook, never a sleep.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

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
	faults         *sidecartest.FaultQueue
	loadPins       chan *bridge.AuthPin
	loadFailures   chan error
	tip            *bridge.AuthPin
}

// A personal network of a fresh owner at base 1, its owner's first device the
// genesis's sidecar, and a gate that has not yet heard of it.
func newExchangeScopesWorld(t testing.TB) *exchangeScopesWorld {
	return newExchangeScopesWorldFor(t, server.NewId(), server.NewId(), server.NewId())
}

func newExchangeScopesWorldFor(t testing.TB, networkId server.Id, ownerMemberId server.Id, ownerSidecarId server.Id) *exchangeScopesWorld {
	t.Helper()
	// Fixture clients have no database rows. Forwarding tests supply their own client-to-sidecar mappings.
	setExchangeScopeForwarding(t, nil)
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
		faults:         blobs.Faults(),
		loadPins:       make(chan *bridge.AuthPin, 64),
		loadFailures:   make(chan error, 64),
	}
	gateSettings := sidecar.DefaultAclGateSettings()
	gateSettings.PollInterval = 0
	gateSettings.FailureBackoff = 0
	gateSettings.Log = sidecar.NewNoopLogger()
	gateSettings.OnLoad = func(loadedNetworkId sidecar.Id, pin *bridge.AuthPin, err error) {
		if loadedNetworkId != sidecar.Id(networkId) {
			return
		}
		if err != nil {
			self.loadFailures <- err
		} else if pin != nil {
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

// QD-894: from base 1, repo:<config_repo_id>:admin goes to each config writer,
// at evs and gkm: the genesis's sidecar, §7.9's bootstrap, and the owner's
// second device alike, which keeps it once the first is revoked. A read_only
// device of the owner and an ephemeral one, no config writers, never get it;
// nor does any audience but evs and gkm; nor the first device once it is
// revoked.
func TestExchangeScopesEachConfigWriterGetsTheConfigAdmin(t *testing.T) {
	self := newExchangeScopesWorld(t)
	secondSidecarId, readOnlySidecarId, ephemeralSidecarId := server.NewId(), server.NewId(), server.NewId()
	self.admitOwnerDevice(secondSidecarId, false, 0, 0)
	self.admitOwnerDevice(readOnlySidecarId, false, 0, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_READ_ONLY))
	self.admitOwnerDevice(ephemeralSidecarId, true, self.world.Clock().Now().Add(24*time.Hour).UnixMilli(), 0)
	snapshot := self.load()
	source := self.source()
	configAdmin := repoScope(self.configRepoId(snapshot), "admin")
	configRead := repoScope(self.configRepoId(snapshot), "r")
	connect.AssertEqual(t, snapshot.Config.IsConfigWriter(sidecar.Id(secondSidecarId)), true)
	connect.AssertEqual(t, snapshot.Config.IsConfigWriter(sidecar.Id(readOnlySidecarId)), false)
	connect.AssertEqual(t, snapshot.Config.IsConfigWriter(sidecar.Id(ephemeralSidecarId)), false)

	for _, audience := range []string{"evs", "gkm"} {
		for _, writerSidecarId := range []server.Id{self.ownerSidecarId, secondSidecarId} {
			grant, err := source.Grant(self.ctx, self.request(&writerSidecarId, self.ownerMemberId, audience, configAdmin))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, grant.Scopes, []string{configAdmin})
			connect.AssertEqual(t, *grant.MemberId, self.ownerMemberId)
			// what is granted is what the gate's state honors (D-ACL-21)
			aclAudience, _ := exchangeAclAudience(audience)
			rights := snapshot.Acl.EvsAccess(sidecar.AclSidecarSubject(sidecar.Id(writerSidecarId)), sidecar.Id(self.configRepoId(snapshot)), aclAudience, self.world.Clock().Now()).Rights
			connect.AssertEqual(t, rights.Has(sidecar.AclEvsAdmin), true)
		}
		for name, sidecarId := range map[string]server.Id{"a read_only device": readOnlySidecarId, "an ephemeral device": ephemeralSidecarId} {
			_, err := source.Grant(self.ctx, self.request(&sidecarId, self.ownerMemberId, audience, configAdmin))
			requireExchangeRefused(t, name+" at "+audience, err, ErrInvalidScope)
			// beside a scope it holds, the admin alone is left out
			grant, err := source.Grant(self.ctx, self.request(&sidecarId, self.ownerMemberId, audience, configAdmin, configRead))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, grant.Scopes, []string{configRead})
		}
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
	// the config's admin outlives the bootstrap sidecar: the other config writer still gets it
	for _, audience := range []string{"evs", "gkm"} {
		grant, err := source.Grant(self.ctx, self.request(&secondSidecarId, self.ownerMemberId, audience, configAdmin))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{configAdmin})
		_, err = source.Grant(self.ctx, self.request(&readOnlySidecarId, self.ownerMemberId, audience, configAdmin))
		requireExchangeRefused(t, "a read_only device at "+audience+" once the first device is revoked", err, ErrInvalidScope)
	}
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

// An exchange retries a failed first read on demand without waiting for a projection change.
func TestExchangeScopesColdGateRefuses(t *testing.T) {
	self := newExchangeScopesWorld(t)
	source := self.source()
	request := self.request(&self.ownerSidecarId, self.ownerMemberId, "dns")

	_, err := self.faults.Add(&sidecartest.FaultPlan{
		Op: sidecartest.FaultOpGet, KeyPrefix: sidecar.ConfigEntryKey(sidecar.Id(self.networkId), 0), Result: sidecartest.FailBeforeApply,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Grant(self.ctx, request)
	requireExchangeRefused(t, "an unknown network", err, ErrExchangeUnavailable)
	select {
	case <-self.loadFailures:
	case <-time.After(30 * time.Second):
		t.Fatal("the first load did not report the interrupted read")
	}
	_, err = source.Grant(self.ctx, request)
	requireExchangeRefused(t, "the retry before verification", err, ErrExchangeUnavailable)
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
	connect.AssertEqual(t, notLoaded.changed, []sidecar.Id{sidecar.Id(self.networkId)})
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

// sockets is a service audience (QD-1029): an admitted sidecar's client gets a
// sockets token that grants no scope and names the sidecar's member from the
// state, a revoked sidecar's and an unadmitted client's are refused, and a
// scope asked at sockets is refused, since sockets takes none.
func TestExchangeScopesGrantSocketsToAnAdmittedSidecar(t *testing.T) {
	self := newExchangeScopesWorld(t)
	setExchangeScopeClientTaggedOrEphemeral(t, untaggedExchangeClient)
	secondSidecarId, revokedSidecarId := server.NewId(), server.NewId()
	self.admitOwnerDevice(secondSidecarId, false, 0, 0)
	self.admitOwnerDevice(revokedSidecarId, false, 0, 0)
	self.revoke(revokedSidecarId)
	repoId := self.createGeneralRepo()
	snapshot := self.load()
	source := self.source()

	for _, sidecarId := range []server.Id{self.ownerSidecarId, secondSidecarId} {
		grant, err := source.Grant(self.ctx, self.request(&sidecarId, self.ownerMemberId, "sockets"))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(grant.Scopes), 0)
		connect.AssertEqual(t, grant.ProjVersion, snapshot.Pin.GetVersion())
		if grant.MemberId == nil || *grant.MemberId != self.ownerMemberId || grant.SidecarId != nil {
			t.Fatalf("sidecar %s: member %v, sidecar %v", sidecarId, grant.MemberId, grant.SidecarId)
		}
	}

	_, err := source.Grant(self.ctx, self.request(&revokedSidecarId, self.ownerMemberId, "sockets"))
	requireExchangeRefused(t, "a revoked sidecar", err, ErrInvalidGrant)
	unboundSidecarId := server.NewId()
	for name, sidecarId := range map[string]*server.Id{"no sidecar": nil, "a sidecar the state does not hold": &unboundSidecarId} {
		_, err := source.Grant(self.ctx, self.request(sidecarId, self.ownerMemberId, "sockets"))
		requireExchangeRefused(t, name, err, ErrInvalidGrant)
	}
	_, err = source.Grant(self.ctx, self.request(&secondSidecarId, self.ownerMemberId, "sockets", repoScope(repoId, "r")))
	requireExchangeRefused(t, "a repo scope at sockets", err, ErrInvalidScope)
}

// The exchange with the projection's source installed: an unadmitted client's
// credential names its member, so its recovery scope reaches the token with
// that member and the state's position; a config writer's config admin does
// too, the bootstrap sidecar's here.
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

// A base folding through the tip whose rows add the config repo's AclRepo row, which no production state holds yet
// (question team-m2-15-config-repo-acl-row): what lets a test reach the config's r through the table at all.
func (self *exchangeScopesWorld) foldWithConfigRepoRow() {
	self.t.Helper()
	snapshot := self.load()
	state := snapshot.Config
	previous := state.Base()
	rows := state.Rows()
	acl := &protocol.AclRows{}
	if err := proto.Unmarshal(rows.GetAcl(), acl); err != nil {
		self.t.Fatal(err)
	}
	acl.Repos = append(acl.Repos, &protocol.AclRepo{
		RepoId: state.Genesis().GetConfigRepoId(),
		Kind:   protocol.RepoKind_REPO_KIND_CONFIG,
	})
	slices.SortFunc(acl.Repos, func(a *protocol.AclRepo, b *protocol.AclRepo) int {
		return bytes.Compare(a.GetRepoId(), b.GetRepoId())
	})
	aclBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(acl)
	if err != nil {
		self.t.Fatal(err)
	}
	rows.Acl = aclBytes
	rowsBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(rows)
	if err != nil {
		self.t.Fatal(err)
	}
	rowsSha256 := sha256.Sum256(rowsBytes)
	signedHeader := self.world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, &bridge.ProjectionHeader{
		NetworkId:        sidecar.Id(self.networkId).Bytes(),
		Version:          self.tip.GetVersion() + 1,
		PrevChain:        self.tip.GetChain(),
		FoldedThrough:    self.tip.GetVersion(),
		ConfigVersion:    previous.GetConfigVersion(),
		ConfigHeadSha256: previous.GetConfigHeadSha256(),
		RowsSha256:       rowsSha256[:],
		RowsByteCount:    uint32(len(rowsBytes)),
		Threshold:        previous.GetThreshold(),
	})
	pin, err := self.sequencer.AppendBase(self.ctx, self.networkId, &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rowsBytes})
	if err != nil {
		self.t.Fatalf("the base with the config row: %v", err)
	}
	self.world.SetTip(pin)
	self.tip = pin
}

// The owner's first device files its forwarding Delegation (§7.9's forwarding row), answering its delegation_id.
func (self *exchangeScopesWorld) fileForwardingDelegation(label string) []byte {
	self.t.Helper()
	readerKey := bytes.Repeat([]byte(label), 1216/len(label)+1)[:1216]
	signedBytes := self.world.SignAsOwner(bridge.SignedType_SIGNED_DELEGATION, &protocol.Delegation{
		Header:               self.world.Header(),
		Pubkey:               exchangeScopesIdentityKey(self.t),
		ReaderXwingPublicKey: readerKey,
		Forward:              &bridge.ForwardScope{ForwardSidecarId: sidecar.Id(self.ownerSidecarId).Bytes()},
	})
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		self.t.Fatal(err)
	}
	self.post(signedBytes)
	purpose, _ := sidecar.ConfigStatementPurpose(bridge.SignedType_SIGNED_DELEGATION)
	delegationId := bridge.SignedBytesSha256(purpose, bridge.SignedType_SIGNED_DELEGATION, signed.GetBody())
	return delegationId[:]
}

// Replaces the forwarding reads for one test: forwardingClients maps each forwarding client to the sidecar it speaks
// for, nil for one that speaks for none.
func setExchangeScopeForwarding(t testing.TB, forwardingClients map[server.Id]*server.Id) {
	previousForwardOnly := exchangeScopeClientForwardOnly
	previousSidecar := exchangeScopeForwardingSidecar
	exchangeScopeClientForwardOnly = func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
		_, forwardOnly := forwardingClients[clientId]
		return forwardOnly, nil
	}
	exchangeScopeForwardingSidecar = func(ctx context.Context, networkId server.Id, clientId server.Id) (*server.Id, error) {
		return forwardingClients[clientId], nil
	}
	t.Cleanup(func() {
		exchangeScopeClientForwardOnly = previousForwardOnly
		exchangeScopeForwardingSidecar = previousSidecar
	})
}

// §7.8's forwarding rule (TEAM-M8-02): a forwarding client of S gets the config's r alone, at evs and gkm, naming S as
// its sidecar, while S's forwarding Delegation is live, whatever S reads: not the general repo S administers, not the
// config's admin S holds as a config writer, no recovery scope, nothing at another audience. An older forwarding client, which
// speaks for no sidecar, is refused, and once S revokes its forwarding Delegation the config's r is refused too.
func TestExchangeScopesForwardingClientGetsTheConfigsReadAlone(t *testing.T) {
	self := newExchangeScopesWorld(t)
	setExchangeScopeClientTaggedOrEphemeral(t, untaggedExchangeClient)
	repoId := self.createGeneralRepo()
	delegationId := self.fileForwardingDelegation("forwarding keys one")
	self.foldWithConfigRepoRow()
	snapshot := self.load()
	source := self.source()
	if snapshot.Config.ForwardingDelegation(sidecar.Id(self.ownerSidecarId)) == nil {
		t.Fatal("no live forwarding Delegation")
	}
	configRead := repoScope(self.configRepoId(snapshot), "r")
	configAdmin := repoScope(self.configRepoId(snapshot), "admin")

	forwardingClientId, olderClientId := server.NewId(), server.NewId()
	setExchangeScopeForwarding(t, map[server.Id]*server.Id{forwardingClientId: &self.ownerSidecarId, olderClientId: nil})
	forwarding := func(audience string, scopes ...string) *ExchangeScopeRequest {
		request := self.request(nil, self.ownerMemberId, audience, scopes...)
		request.ClientId = forwardingClientId
		return request
	}

	// S itself reads the config and administers its repo
	grant, err := source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, "evs", configRead, repoScope(repoId, "admin")))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, grant.Scopes, []string{configRead, repoScope(repoId, "admin")})

	for _, audience := range []string{"evs", "gkm"} {
		grant, err := source.Grant(self.ctx, forwarding(audience, configRead, repoScope(repoId, "admin"), repoScope(repoId, "r"), configAdmin))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{configRead})
		connect.AssertEqual(t, *grant.SidecarId, self.ownerSidecarId)
		connect.AssertEqual(t, *grant.MemberId, self.ownerMemberId)
		connect.AssertEqual(t, grant.ProjVersion, snapshot.Pin.GetVersion())
	}
	for name, request := range map[string]*ExchangeScopeRequest{
		"S's general repo":      forwarding("evs", repoScope(repoId, "r")),
		"the config's admin":    forwarding("evs", configAdmin),
		"the config's rw":       forwarding("gkm", repoScope(self.configRepoId(snapshot), "rw")),
		"a recovery scope":      forwarding("gkm", recoveryScope(self.ownerMemberId)),
		"no scope":              forwarding("evs"),
		"the config's r at dns": forwarding("dns", configRead),
	} {
		_, err := source.Grant(self.ctx, request)
		requireExchangeRefused(t, name, err, ErrInvalidScope)
	}
	older := forwarding("evs", configRead)
	older.ClientId = olderClientId
	_, err = source.Grant(self.ctx, older)
	requireExchangeRefused(t, "an older forwarding client", err, ErrInvalidGrant)

	// ended: S's DelegationRevoke of it
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_DELEGATION_REVOKE, &protocol.DelegationRevoke{
		Header: self.world.Header(), DelegationId: delegationId,
	}))
	ended := self.load()
	connect.AssertEqual(t, ended.Config.ForwardingDelegation(sidecar.Id(self.ownerSidecarId)) == nil, true)
	_, err = source.Grant(self.ctx, forwarding("evs", configRead))
	requireExchangeRefused(t, "after its Delegation ended", err, ErrInvalidScope)
}

// The exchange with a forwarding client's token: its token names S as its sidecar, S's client as its subject and the
// config's r as its one scope.
func TestExchangeWithAForwardingClientsToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newExchangeFixture(t)
		self := newExchangeScopesWorldFor(t, fixture.networkId, fixture.userId, server.NewId())
		self.fileForwardingDelegation("forwarding keys")
		self.foldWithConfigRepoRow()
		snapshot := self.load()
		defer SetExchangeScopeSource(self.source())()
		sidecarClientId := server.NewId()
		previousSubject := exchangeClientSubject
		previousSidecarId := exchangeClientSidecarId
		exchangeClientSubject = func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, error) {
			return sidecarClientId, nil
		}
		// the forwarding client is bound to no sidecar
		exchangeClientSidecarId = func(ctx context.Context, networkId server.Id, clientId server.Id) *server.Id {
			return nil
		}
		t.Cleanup(func() {
			exchangeClientSubject = previousSubject
			exchangeClientSidecarId = previousSidecarId
		})
		setExchangeScopeForwarding(t, map[server.Id]*server.Id{fixture.clientId: &self.ownerSidecarId})

		configRead := repoScope(self.configRepoId(snapshot), "r")
		result, err := Exchange(t.Context(), fixture.args("evs", configRead))
		connect.AssertEqual(t, err, nil)
		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "evs")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Scopes(), []string{configRead})
		connect.AssertEqual(t, claims.ClientId, fixture.clientId)
		connect.AssertEqual(t, *claims.SubjectClientId, sidecarClientId)
		connect.AssertEqual(t, *claims.SidecarId, self.ownerSidecarId)
		connect.AssertEqual(t, claims.ProjVersion, snapshot.Pin.GetVersion())
	})
}

// Replaces the restored rows' Owners and Admins for one test: admins maps each admin's sidecar to its member.
func setExchangeScopeReseedAdmins(t testing.TB, admins map[server.Id]server.Id) {
	previous := exchangeScopeReseedAdminSidecar
	exchangeScopeReseedAdminSidecar = func(ctx context.Context, networkId server.Id, sidecarId server.Id) (*server.Id, bool, error) {
		memberId, admin := admins[sidecarId]
		if !admin {
			return nil, false, nil
		}
		return &memberId, true, nil
	}
	t.Cleanup(func() {
		exchangeScopeReseedAdminSidecar = previous
	})
}

// §7.8's re-seed mode (TEAM-M3-02, §19.10): repo:<id>:admin alone, at evs and gkm, for any repo, to the sidecars the
// restored rows list under Owners and Admins, with no projection read; everything else restore_required: another
// scope, another audience, no scope, another sidecar, a client bound to none, a forwarding client.
func TestExchangeScopesReseedModeMintsOnlyAdminsAdminScopes(t *testing.T) {
	networkId := server.NewId()
	adminSidecarId, otherSidecarId, memberId := server.NewId(), server.NewId(), server.NewId()
	setExchangeScopeReseedAdmins(t, map[server.Id]server.Id{adminSidecarId: memberId})
	forwardingClientId := server.NewId()
	setExchangeScopeForwarding(t, map[server.Id]*server.Id{forwardingClientId: &adminSidecarId})
	// no gate at all: re-seed mode reads none
	source := NewProjectionExchangeScopeSource(nil, nil)
	request := func(sidecarId *server.Id, audience string, scopes ...string) *ExchangeScopeRequest {
		clientId := server.NewId()
		return &ExchangeScopeRequest{NetworkId: networkId, ClientId: clientId, SubjectClientId: clientId, SidecarId: sidecarId, Audience: audience, Scopes: scopes}
	}
	repoAdmin, otherAdmin := repoScope(server.NewId(), "admin"), repoScope(server.NewId(), "admin")
	for _, audience := range []string{"evs", "gkm"} {
		grant, err := source.GrantReseed(context.Background(), request(&adminSidecarId, audience, repoAdmin, otherAdmin))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{repoAdmin, otherAdmin})
		connect.AssertEqual(t, *grant.MemberId, memberId)
		connect.AssertEqual(t, grant.ProjVersion, uint64(0))
	}
	forwarding := request(&adminSidecarId, "evs", repoAdmin)
	forwarding.ClientId = forwardingClientId
	for name, refused := range map[string]*ExchangeScopeRequest{
		"a read scope":        request(&adminSidecarId, "evs", repoScope(server.NewId(), "r")),
		"beside a read":       request(&adminSidecarId, "evs", repoAdmin, repoScope(server.NewId(), "r")),
		"at dns":              request(&adminSidecarId, "dns", repoAdmin),
		"no scope":            request(&adminSidecarId, "evs"),
		"another sidecar":     request(&otherSidecarId, "evs", repoAdmin),
		"no sidecar":          request(nil, "evs", repoAdmin),
		"a forwarding client": forwarding,
		"a recovery scope":    request(&adminSidecarId, "gkm", recoveryScope(memberId)),
	} {
		_, err := source.GrantReseed(context.Background(), refused)
		requireExchangeRefused(t, name, err, ErrRestoreRequired)
	}
}

// Through the exchange while sidecar.yml's reseed is on: the installed source's re-seed grant answers, an admin's
// sidecar getting its admin scope and a token naming it, and every other request restore_required.
func TestExchangeInReseedMode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newExchangeFixture(t)
		defer server.Testing_PushSidecarConfig("sidecar:\n  reseed: true\n")()
		setExchangeScopeReseedAdmins(t, map[server.Id]server.Id{fixture.sidecarId: fixture.userId})
		defer SetExchangeScopeSource(NewProjectionExchangeScopeSource(nil, nil))()

		repoAdmin := repoScope(server.NewId(), "admin")
		result, err := Exchange(t.Context(), fixture.args("evs", repoAdmin))
		connect.AssertEqual(t, err, nil)
		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "evs")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Scopes(), []string{repoAdmin})
		connect.AssertEqual(t, *claims.SidecarId, fixture.sidecarId)

		_, err = Exchange(t.Context(), fixture.args("evs", repoScope(server.NewId(), "r")))
		requireExchangeRefused(t, "a read scope", err, ErrRestoreRequired)
	})
}

// §7.8's re-seed mode ends per network at its resumed marker in Redis {acl_<networkId>}r (QD-724), while
// sidecar.yml's reseed stays on: a network whose adopted run is written but whose resume base has not landed is still
// restore_required, as it is while another network resumes; once its own resume base lands the exchange asks Grant
// again; and a marker that cannot be read mints nothing in either mode.
func TestExchangeLeavesReseedModeAtTheNetworksResumedMarker(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newExchangeFixture(t)
		defer server.Testing_PushSidecarConfig("sidecar:\n  reseed: true\n")()
		// a source with no re-seed grant: every request in re-seed mode is restore_required
		source := &grantEverythingSource{memberId: server.NewId()}
		defer SetExchangeScopeSource(source)()
		readScope := repoScope(server.NewId(), "r")
		uploads := model.NewConfigReseedUploads(nil)

		_, err := Exchange(t.Context(), fixture.args("evs", readScope))
		requireExchangeRefused(t, "before any upload", err, ErrRestoreRequired)

		connect.AssertEqual(t, uploads.MarkWritten(t.Context(), fixture.networkId, 5), nil)
		_, err = Exchange(t.Context(), fixture.args("evs", readScope))
		requireExchangeRefused(t, "written, not resumed", err, ErrRestoreRequired)

		// another network resumes
		otherNetworkId := server.NewId()
		connect.AssertEqual(t, uploads.MarkWritten(t.Context(), otherNetworkId, 5), nil)
		resumed, err := uploads.MarkResumed(t.Context(), otherNetworkId, 6)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, resumed, true)
		_, err = Exchange(t.Context(), fixture.args("evs", readScope))
		requireExchangeRefused(t, "while another network resumed", err, ErrRestoreRequired)
		connect.AssertEqual(t, len(source.requests), 0)

		// its own resume base lands
		resumed, err = uploads.MarkResumed(t.Context(), fixture.networkId, 6)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, resumed, true)
		result, err := Exchange(t.Context(), fixture.args("evs", readScope))
		connect.AssertEqual(t, err, nil)
		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "evs")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Scopes(), []string{readScope})
		connect.AssertEqual(t, len(source.requests), 1)
		connect.AssertEqual(t, source.requests[0].NetworkId, fixture.networkId)

		// the marker cannot be read: neither mode is guessed
		previous := exchangeReseedResumed
		exchangeReseedResumed = func(ctx context.Context, networkId server.Id) (bool, error) {
			return false, errors.New("redis is away")
		}
		defer func() {
			exchangeReseedResumed = previous
		}()
		result, err = Exchange(t.Context(), fixture.args("evs", readScope))
		if err == nil || errors.Is(err, ErrRestoreRequired) || result != nil {
			t.Fatalf("an unreadable marker: %v", err)
		}
		connect.AssertEqual(t, len(source.requests), 1)
	})
}

// With reseed off the marker is never read: no restore, no re-seed mode.
func TestExchangeReadsNoResumedMarkerOutsideARestore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newExchangeFixture(t)
		source := &grantEverythingSource{memberId: server.NewId()}
		defer SetExchangeScopeSource(source)()
		previous := exchangeReseedResumed
		exchangeReseedResumed = func(ctx context.Context, networkId server.Id) (bool, error) {
			t.Errorf("the marker was read")
			return false, nil
		}
		defer func() {
			exchangeReseedResumed = previous
		}()
		_, err := Exchange(t.Context(), fixture.args("evs", repoScope(server.NewId(), "r")))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(source.requests), 1)
	})
}

// A device of the network whose key the test holds, so its own statements can
// be signed: a member's first device, say, that then makes that member's vault.
type exchangeScopesDevice struct {
	sidecarId server.Id
	key       ed25519.PrivateKey
}

// A fresh device and its identity key.
func newExchangeScopesDevice(t testing.TB) *exchangeScopesDevice {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &exchangeScopesDevice{sidecarId: server.NewId(), key: privateKey}
}

// body signed by device under its type's purpose, as a Signed's bytes, the
// owner's first device's for a nil device.
func (self *exchangeScopesWorld) signAs(device *exchangeScopesDevice, signedType bridge.SignedType, body proto.Message) []byte {
	self.t.Helper()
	if device == nil {
		return self.world.SignAsOwner(signedType, body)
	}
	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		self.t.Fatal(err)
	}
	purpose, ok := sidecar.ConfigStatementPurpose(signedType)
	if !ok {
		self.t.Fatalf("no purpose for %s", signedType)
	}
	signed, err := proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.Signed{
		Body:      bodyBytes,
		SignerId:  sidecar.Id(device.sidecarId).Bytes(),
		Signature: ed25519.Sign(device.key, bridge.SignedBytes(purpose, signedType, bodyBytes)),
		Type:      uint32(signedType),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return signed
}

// signer's admission of device as a device of memberId, the owner's first
// device signing for a nil signer.
func (self *exchangeScopesWorld) admitDevice(signer *exchangeScopesDevice, device *exchangeScopesDevice, memberId server.Id, capabilities uint32) {
	self.t.Helper()
	self.post(self.signAs(signer, bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header:            self.world.Header(),
		NetworkId:         sidecar.Id(self.networkId).Bytes(),
		SidecarId:         sidecar.Id(device.sidecarId).Bytes(),
		IdentityPublicKey: device.key.Public().(ed25519.PublicKey),
		IdentityAlg:       "ed25519",
		MemberId:          sidecar.Id(memberId).Bytes(),
		Capabilities:      capabilities,
	}))
}

// signer's RepoCreate of a fresh repo of kind, owner-bound to signer's member
// where the kind is; the owner's first device signs for a nil signer.
func (self *exchangeScopesWorld) createRepo(signer *exchangeScopesDevice, kind protocol.RepoKind) server.Id {
	self.t.Helper()
	repoId := server.NewId()
	self.post(self.signAs(signer, bridge.SignedType_SIGNED_REPO_CREATE, &protocol.RepoCreate{
		Header: self.world.Header(), RepoId: sidecar.Id(repoId).Bytes(), Kind: kind,
	}))
	return repoId
}

// The e2e-gkm-concurrency second device's case (TestE2eGkmConcurrencySecondDevice)
// at the exchange: a personal network's owner makes its vault and mailbox on
// its first device, which then admits a second device of the owner, consented
// by §8.5's scoped clause. The exchange grants the second device the vault's and
// the mailbox's r and rw at evs and gkm, as the gate's state honors them
// (D-ACL-21), and refuses it the vault's admin, which only the vault's creator
// holds; an ephemeral device of the owner, which no consent clause holds for,
// gets the vault's deposit alone and its r refused.
func TestExchangeScopesASecondDeviceGetsItsOwnersVault(t *testing.T) {
	self := newExchangeScopesWorld(t)
	vaultId := self.createRepo(nil, protocol.RepoKind_REPO_KIND_VAULT)
	mailboxId := self.createRepo(nil, protocol.RepoKind_REPO_KIND_MAILBOX)
	secondSidecarId, ephemeralSidecarId := server.NewId(), server.NewId()
	self.admitOwnerDevice(secondSidecarId, false, 0, 0)
	self.admitOwnerDevice(ephemeralSidecarId, true, self.world.Clock().Now().Add(24*time.Hour).UnixMilli(), 0)
	snapshot := self.load()
	source := self.source()
	connect.AssertEqual(t, snapshot.Acl.ConsentedFor(sidecar.Id(secondSidecarId), sidecar.Id(self.ownerMemberId)), true)

	for _, audience := range []string{"evs", "gkm"} {
		aclAudience, _ := exchangeAclAudience(audience)
		scopes := []string{repoScope(vaultId, "r"), repoScope(vaultId, "rw"), repoScope(mailboxId, "rw")}
		grant, err := source.Grant(self.ctx, self.request(&secondSidecarId, self.ownerMemberId, audience, scopes...))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, scopes)
		connect.AssertEqual(t, *grant.MemberId, self.ownerMemberId)
		connect.AssertEqual(t, grant.ProjVersion, snapshot.Pin.GetVersion())
		for _, repoId := range []server.Id{vaultId, mailboxId} {
			rights := snapshot.Acl.EvsAccess(sidecar.AclSidecarSubject(sidecar.Id(secondSidecarId)), sidecar.Id(repoId), aclAudience, self.world.Clock().Now()).Rights
			connect.AssertEqual(t, rights.Has(sidecar.AclEvsRead) && rights.Has(sidecar.AclEvsWrite), true)
		}

		// the vault's admin is its creator's: beside rw it is left out, alone it is refused
		grant, err = source.Grant(self.ctx, self.request(&secondSidecarId, self.ownerMemberId, audience, repoScope(vaultId, "admin"), repoScope(vaultId, "rw")))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{repoScope(vaultId, "rw")})
		_, err = source.Grant(self.ctx, self.request(&secondSidecarId, self.ownerMemberId, audience, repoScope(vaultId, "admin")))
		requireExchangeRefused(t, "the second device's vault admin at "+audience, err, ErrInvalidScope)
		grant, err = source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, audience, repoScope(vaultId, "admin")))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{repoScope(vaultId, "admin")})

		_, err = source.Grant(self.ctx, self.request(&ephemeralSidecarId, self.ownerMemberId, audience, repoScope(vaultId, "r")))
		requireExchangeRefused(t, "the ephemeral device's vault read at "+audience, err, ErrInvalidScope)
		grant, err = source.Grant(self.ctx, self.request(&ephemeralSidecarId, self.ownerMemberId, audience, repoScope(vaultId, "deposit")))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{repoScope(vaultId, "deposit")})
	}
}

// Owner decision 126 at the exchange: a member's vault opens to the member's
// consented devices alone. Member m's first device, consented by its invite,
// makes m's vault; the network owner's device then admits another device of m,
// which no consent of m's covers, and m's first device admits a third, which
// the scoped clause consents. The exchange refuses the unconsented device the
// vault's r and rw, giving it the deposit m's other devices have, refuses the
// network owner's own device every role there, an admin of m's but no device of
// m's, and grants the third device r and rw; once m's first device files an
// AdmissionConfirm of the unconsented device, its r and rw are granted too.
func TestExchangeScopesAMembersVaultRefusesItsUnconsentedDevices(t *testing.T) {
	self := newExchangeScopesWorld(t)
	memberId := server.NewId()
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		Header: self.world.Header(), MemberId: sidecar.Id(memberId).Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER,
	}))
	first, unconsented, third := newExchangeScopesDevice(t), newExchangeScopesDevice(t), newExchangeScopesDevice(t)
	self.admitDevice(nil, first, memberId, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED))
	vaultId := self.createRepo(first, protocol.RepoKind_REPO_KIND_VAULT)
	self.admitDevice(nil, unconsented, memberId, 0)
	self.admitDevice(first, third, memberId, 0)
	snapshot := self.load()
	source := self.source()
	connect.AssertEqual(t, snapshot.Acl.ConsentedFor(sidecar.Id(unconsented.sidecarId), sidecar.Id(memberId)), false)
	connect.AssertEqual(t, snapshot.Acl.ConsentedFor(sidecar.Id(third.sidecarId), sidecar.Id(memberId)), true)
	readWrite := []string{repoScope(vaultId, "r"), repoScope(vaultId, "rw")}

	for _, audience := range []string{"evs", "gkm"} {
		for _, device := range []*exchangeScopesDevice{first, third} {
			grant, err := source.Grant(self.ctx, self.request(&device.sidecarId, memberId, audience, readWrite...))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, grant.Scopes, readWrite)
			connect.AssertEqual(t, *grant.MemberId, memberId)
		}
		_, err := source.Grant(self.ctx, self.request(&unconsented.sidecarId, memberId, audience, readWrite...))
		requireExchangeRefused(t, "the unconsented device at "+audience, err, ErrInvalidScope)
		grant, err := source.Grant(self.ctx, self.request(&unconsented.sidecarId, memberId, audience, repoScope(vaultId, "deposit"), repoScope(vaultId, "r")))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, []string{repoScope(vaultId, "deposit")})
		for _, role := range []string{"deposit", "r", "rw", "admin"} {
			_, err := source.Grant(self.ctx, self.request(&self.ownerSidecarId, self.ownerMemberId, audience, repoScope(vaultId, role)))
			requireExchangeRefused(t, "the network owner's device's "+role+" at "+audience, err, ErrInvalidScope)
		}
	}

	self.post(self.signAs(first, bridge.SignedType_SIGNED_ADMISSION_CONFIRM, &protocol.AdmissionConfirm{
		Header: self.world.Header(), SidecarId: sidecar.Id(unconsented.sidecarId).Bytes(),
	}))
	self.load()
	for _, audience := range []string{"evs", "gkm"} {
		grant, err := source.Grant(self.ctx, self.request(&unconsented.sidecarId, memberId, audience, readWrite...))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, grant.Scopes, readWrite)
	}
}

// The second device's case through the exchange itself, as the e2e's key
// client meets it: its client's token and proof buy a gkm token holding the
// vault's r and rw, naming it and its member at the state's position, where
// before the projection granted none of the scopes (400 invalid_scope); an
// ephemeral device's client still has its read refused.
func TestExchangeWithASecondDevicesVaultScopes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newExchangeFixture(t)
		userId := fixture.userId
		self := newExchangeScopesWorldFor(t, fixture.networkId, userId, server.NewId())
		vaultId := self.createRepo(nil, protocol.RepoKind_REPO_KIND_VAULT)
		secondSidecarId, ephemeralSidecarId := server.NewId(), server.NewId()
		self.admitOwnerDevice(secondSidecarId, false, 0, 0)
		self.admitOwnerDevice(ephemeralSidecarId, true, self.world.Clock().Now().Add(24*time.Hour).UnixMilli(), 0)
		snapshot := self.load()
		defer SetExchangeScopeSource(self.source())()

		fixture.sidecarId = secondSidecarId
		readWrite := []string{repoScope(vaultId, "r"), repoScope(vaultId, "rw")}
		result, err := Exchange(t.Context(), fixture.args("gkm", strings.Join(readWrite, " ")))
		connect.AssertEqual(t, err, nil)
		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "gkm")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Scopes(), readWrite)
		connect.AssertEqual(t, *claims.SidecarId, secondSidecarId)
		connect.AssertEqual(t, *claims.MemberId, userId)
		connect.AssertEqual(t, claims.ProjVersion, snapshot.Pin.GetVersion())

		fixture.sidecarId = ephemeralSidecarId
		_, err = Exchange(t.Context(), fixture.args("gkm", repoScope(vaultId, "r")))
		requireExchangeRefused(t, "the ephemeral device's vault read", err, ErrInvalidScope)
	})
}
