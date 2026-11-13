package gkm

import (
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The block's own authorization state on a host (QD-718): NewAuthState's gate cache over the host's Redis and first
// metadata cluster, reading the admission world's real log, under the host as a block builds it.
type authStateTestWorld struct {
	t         *testing.T
	net       *sidecartest.Net
	admission *sidecartest.AdmissionNet
	loads     chan *bridge.AuthPin
	gkmHost   *Host
}

func newAuthStateTestWorld(t *testing.T) *authStateTestWorld {
	net := sidecartest.NewNet(t.Context(), nil)
	t.Cleanup(net.Close)
	tier, err := net.AdmissionTier()
	if err != nil {
		t.Fatal(err)
	}
	tier.AclCompile = sidecartest.AclAdmissionCompile
	admission := sidecartest.NewAdmissionNet(t, tier)
	t.Cleanup(admission.Close)
	world := &authStateTestWorld{t: t, net: net, admission: admission, loads: make(chan *bridge.AuthPin, 256)}
	net.Fakes().Gates.OpenService(sidecar.ServiceGateGkm, connect.Id(admission.NetworkId()))

	settings := DefaultHostSettings()
	settings.Gate.Log = sidecar.NewNoopLogger()
	settings.Gate.PollInterval = 0
	settings.Gate.FailureBackoff = 0
	settings.Gate.OnLoad = func(networkId sidecar.Id, pin *bridge.AuthPin, err error) {
		if err != nil {
			t.Errorf("the gate's load: %v", err)
		}
		world.loads <- pin
	}
	state, closeState, err := NewAuthState(t.Context(), net.Host(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeState)
	world.gkmHost, err = NewHost(t.Context(), net.Host(), settings, &HostDeps{AuthState: state})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(world.gkmHost.Close)
	awaitHostTestProbe(t, world.gkmHost.Engine())
	return world
}

// Waits for the block's gate to hold the log's tip.
func (self *authStateTestWorld) awaitTip() {
	self.t.Helper()
	want := self.admission.Tip()
	deadline := time.After(hostTestStepTimeout)
	for {
		select {
		case pin := <-self.loads:
			if proto.Equal(pin, want) {
				return
			}
		case <-deadline:
			self.t.Fatalf("the gate never loaded %d", want.GetVersion())
		}
	}
}

// What api's script does after an entry, the part a gate hears: the INCR of {acl_<networkId>}v on the block's Redis.
func (self *authStateTestWorld) hint() {
	self.t.Helper()
	if _, err := self.net.Host().Redis.Increment(self.t.Context(), sidecar.AclVersionKey(self.admission.NetworkId()), 48*time.Hour); err != nil {
		self.t.Fatal(err)
	}
}

// A gkm token for the device's sidecar carrying the config's r, as the exchange mints it.
func (self *authStateTestWorld) token(device *sidecartest.AdmissionDevice) string {
	sidecarId := connect.Id(device.SidecarId)
	now := self.net.Clock().Now()
	return self.net.Fakes().Authenticator.Issue(&sidecar.Principal{
		NetworkId: connect.Id(self.admission.NetworkId()),
		ClientId:  connect.NewId(),
		SidecarId: &sidecarId,
		Audience:  gkmservice.GkmAudience,
		Scopes:    []string{"repo:" + self.admission.ConfigRepoId().String() + ":r"},
		IssuedAt:  now,
		ExpiresAt: now.Add(15 * time.Minute),
	})
}

// The device's ReaderKey upload under its token: the status.
func (self *authStateTestWorld) uploadReaderKey(device *sidecartest.AdmissionDevice) int {
	self.t.Helper()
	signer := &hostTestDevice{sidecarId: device.SidecarId, privateKey: device.Key}
	now := self.net.Clock().Now()
	signed, _, err := sidecar.NewGkmReaderKey(device.SidecarId, now, now.Add(6*24*time.Hour), signer.sign)
	if err != nil {
		self.t.Fatal(err)
	}
	body, err := proto.Marshal(signed)
	if err != nil {
		self.t.Fatal(err)
	}
	code, _ := hostTestServe(self.gkmHost.Handler(), http.MethodPost, "/gkm/reader-key", self.token(device), body)
	return code
}

// The device's read of a sidecar's ReaderKey under its token: the status.
func (self *authStateTestWorld) readReaderKey(device *sidecartest.AdmissionDevice, of *sidecartest.AdmissionDevice) int {
	self.t.Helper()
	code, _ := hostTestServe(self.gkmHost.Handler(), http.MethodGet, "/gkm/reader-key/"+sidecar.EvsKeyId(of.SidecarId), self.token(device), nil)
	return code
}

// The block's readiness with its own state, and a token through it: the host misses nothing and is ready; a network's
// first token is answered 503 and starts its load; then the bootstrap sidecar's write is honored under the identity
// key the log admits for it; a device the gate has not loaded is refused until {acl_<networkId>}v's INCR, which the
// block's own watch hears, loads its admission; and its revocation refuses it at the next INCR.
func TestHostIsReadyAndJudgesTokensByItsOwnAuthState(t *testing.T) {
	world := newAuthStateTestWorld(t)
	admission := world.admission
	owner := admission.Owner()
	if missing := world.gkmHost.Missing(); len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if !world.gkmHost.Ready() {
		t.Fatal("not ready with the block's own authorization state")
	}

	if code := world.uploadReaderKey(owner); code != http.StatusServiceUnavailable {
		t.Fatalf("a network's first token: %d", code)
	}
	world.awaitTip()
	if code := world.uploadReaderKey(owner); code != http.StatusOK {
		t.Fatalf("the bootstrap sidecar's upload once its network loaded: %d", code)
	}

	// a device admitted after the load: the gate has not read its admission, and it reads it by the INCR alone
	_, phone := admission.ConsentedMember("block state")
	if code := world.uploadReaderKey(phone); code != http.StatusForbidden {
		t.Fatalf("a device the gate has not loaded: %d", code)
	}
	world.hint()
	world.awaitTip()
	if code := world.uploadReaderKey(phone); code != http.StatusOK {
		t.Fatalf("the device once the gate loaded its admission: %d", code)
	}

	// its revocation, again by the INCR alone
	admission.MustLand(admission.Sign(owner, bridge.SignedType_SIGNED_SIDECAR_REVOKE, admission.Revocation(phone, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_LOST, 0)))
	world.hint()
	world.awaitTip()
	if code := world.uploadReaderKey(phone); code != http.StatusForbidden {
		t.Errorf("a revoked sidecar's upload: %d", code)
	}
	if code := world.readReaderKey(phone, owner); code != http.StatusForbidden {
		t.Errorf("a revoked sidecar's read: %d", code)
	}
	if code := world.readReaderKey(owner, owner); code != http.StatusOK {
		t.Errorf("another sidecar's read after the revocation: %d", code)
	}
}

// A host with no Redis or no clusters builds no state: nothing would feed or load the gate.
func TestAuthStateNeedsRedisAndBlobs(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil).ServiceHost()
	host.Redis = nil
	if state, _, err := NewAuthState(t.Context(), host, nil); err == nil || state != nil {
		t.Errorf("a state on a host with no Redis: %v", err)
	}
	host = sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil).ServiceHost()
	host.Blobs = nil
	if state, _, err := NewAuthState(t.Context(), host, nil); err == nil || state != nil {
		t.Errorf("a state on a host with no clusters: %v", err)
	}
	if state, _, err := NewAuthState(t.Context(), nil, nil); err == nil || state != nil {
		t.Errorf("a state on no host: %v", err)
	}
}
