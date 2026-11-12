package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §13.15's certificate routes over the engine and its fake store (TLS-M5-16):
// the subject is the token's, an order needs its grant and its own set, an
// order id is its client's alone, and a revocation is its own client's or its
// network's credential's.

const tlsTestRoot = "root.example"

// The routes' world: an engine on a fake store, a fake authenticator, and
// each sidecar's subject as the projection would give it.
type tlsTestWorld struct {
	t       *testing.T
	ctx     context.Context
	host    *sidecartest.FakeHost
	store   *sidecartest.FakeCertsStore
	service *certsservice.CertsService
	// by sidecar id
	subjects   map[sidecar.Id]*TlsCertSubject
	subjectErr error
}

func newTlsTestWorld(t *testing.T) *tlsTestWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
	sidecartest.RegisterCertsBudgetTwins(host.Redis)
	store := sidecartest.NewFakeCertsStore(nil)
	settings := certsservice.DefaultCertsSettings()
	settings.RootDomain = tlsTestRoot
	service, err := certsservice.NewCertsService(ctx, host.ServiceHost(), store, nil, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	world := &tlsTestWorld{t: t, ctx: ctx, host: host, store: store, service: service, subjects: map[sidecar.Id]*TlsCertSubject{}}
	SetTlsCerts(&TlsCertsDeps{
		Service:       service,
		Store:         store,
		Authenticator: host.Authenticator,
		Subject: func(ctx context.Context, principal *sidecar.Principal, now time.Time) (*TlsCertSubject, error) {
			if world.subjectErr != nil {
				return nil, world.subjectErr
			}
			subject, ok := world.subjects[sidecar.Id(*principal.SidecarId)]
			if !ok {
				return &TlsCertSubject{}, nil
			}
			return subject, nil
		},
	})
	t.Cleanup(func() {
		SetTlsCerts(nil)
	})
	return world
}

// One sidecar of a network: its identity key, its own TLS key and its set.
type tlsTestDevice struct {
	networkId     sidecar.Id
	clientId      sidecar.Id
	sidecarId     sidecar.Id
	identityKey   ed25519.PrivateKey
	tlsKey        *ecdsa.PrivateKey
	identifierSet []string
	token         string
}

// A granted sidecar of networkId whose label is label, with a certs token.
func (self *tlsTestWorld) device(networkId sidecar.Id, label string) *tlsTestDevice {
	self.t.Helper()
	identifierSet, err := sidecar.TlsIdentifierSet(tlsTestRoot, label, "", nil)
	if err != nil {
		self.t.Fatal(err)
	}
	identityPublicKey, identityKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		self.t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		self.t.Fatal(err)
	}
	device := &tlsTestDevice{
		networkId:     networkId,
		clientId:      sidecar.NewId(),
		sidecarId:     sidecar.NewId(),
		identityKey:   identityKey,
		tlsKey:        tlsKey,
		identifierSet: identifierSet,
	}
	device.token = self.token(device, TlsCertsAudience)
	self.subjects[device.sidecarId] = &TlsCertSubject{
		IdentifierSet:     identifierSet,
		IdentityPublicKey: identityPublicKey,
		Granted:           true,
	}
	return device
}

// A service token of the device's for audience.
func (self *tlsTestWorld) token(device *tlsTestDevice, audience string) string {
	sidecarId := connect.Id(device.sidecarId)
	now := self.host.Clock.Now()
	return self.host.Authenticator.Issue(&sidecar.Principal{
		NetworkId: connect.Id(device.networkId),
		ClientId:  connect.Id(device.clientId),
		SidecarId: &sidecarId,
		Audience:  audience,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
}

// The device's order for names, attested for attestedSet by its identity key.
func (self *tlsTestWorld) orderArgs(device *tlsTestDevice, names []string) *sidecar.TlsCertOrderArgs {
	self.t.Helper()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, device.tlsKey)
	if err != nil {
		self.t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&device.tlsKey.PublicKey)
	if err != nil {
		self.t.Fatal(err)
	}
	spkiSha256 := sha256.Sum256(spki)
	setSha256, err := sidecar.TlsIdentifierSetHash(names)
	if err != nil {
		self.t.Fatal(err)
	}
	attestation, err := sidecar.SignTlsKeyAttestation(&protocol.TlsKeyAttestation{
		NetworkId:           device.networkId[:],
		SidecarId:           device.sidecarId[:],
		SpkiSha256:          spkiSha256[:],
		IdentifierSetSha256: setSha256[:],
		IssueTimeMillis:     uint64(self.host.Clock.Now().UnixMilli()),
	}, func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
		return &bridge.Signed{
			Body:      body,
			SignerId:  device.sidecarId[:],
			Signature: ed25519.Sign(device.identityKey, bridge.SignedBytes(purpose, signedType, body)),
			Type:      uint32(signedType),
		}, nil
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return &sidecar.TlsCertOrderArgs{Csr: csr, Attestation: attestation}
}

// Whether err declares status by the "<code> message" convention.
func tlsTestStatus(err error, status string) bool {
	return err != nil && strings.HasPrefix(err.Error(), status+" ")
}

func TestTlsCertOrderQueuesTheTokensSubject(t *testing.T) {
	world := newTlsTestWorld(t)
	device := world.device(sidecar.NewId(), "acme")
	args := world.orderArgs(device, device.identifierSet)

	result, err := TlsCertOrder(world.ctx, device.token, args)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != sidecar.TlsCertStateQueued || result.CertId == (sidecar.Id{}) || result.RetryAfterMs == 0 {
		t.Fatalf("the order answered %+v", result)
	}
	rows := world.store.Rows()
	if len(rows) != 1 || rows[0].NetworkId != device.networkId || rows[0].ClientId != device.clientId || rows[0].SidecarId != device.sidecarId {
		t.Fatalf("the row is not the token's subject: %+v", rows)
	}
	// the same request again answers its first row
	again, err := TlsCertOrder(world.ctx, device.token, args)
	if err != nil || again.CertId != result.CertId || len(world.store.Rows()) != 1 {
		t.Fatalf("a retried order: %+v, %v", again, err)
	}

	// its own order reads back; a stranger's id, of its network or not, is not found
	status, err := TlsCertOrderStatus(world.ctx, device.token, server.Id(result.CertId))
	if err != nil || status.State != sidecar.TlsCertStateQueued || status.RetryAfterMs == 0 {
		t.Fatalf("the order's status: %+v, %v", status, err)
	}
	sibling := world.device(device.networkId, "acme")
	stranger := world.device(sidecar.NewId(), "other")
	for _, other := range []*tlsTestDevice{sibling, stranger} {
		if _, err := TlsCertOrderStatus(world.ctx, other.token, server.Id(result.CertId)); !tlsTestStatus(err, "404") {
			t.Fatalf("a stranger's read of the order answered %v", err)
		}
	}
	if _, err := TlsCertOrderStatus(world.ctx, device.token, server.NewId()); !tlsTestStatus(err, "404") {
		t.Fatalf("an unknown order answered %v", err)
	}
}

func TestTlsCertOrderRefusals(t *testing.T) {
	world := newTlsTestWorld(t)
	device := world.device(sidecar.NewId(), "acme")
	other := world.device(sidecar.NewId(), "other")
	args := world.orderArgs(device, device.identifierSet)

	// no token, a token for another audience, a revoked one
	if _, err := TlsCertOrder(world.ctx, "", args); !tlsTestStatus(err, "401") {
		t.Fatalf("no token answered %v", err)
	}
	if _, err := TlsCertOrder(world.ctx, world.token(device, "evs"), args); !tlsTestStatus(err, "401") {
		t.Fatalf("another audience's token answered %v", err)
	}
	revoked := world.token(device, TlsCertsAudience)
	world.host.Authenticator.Revoke(revoked)
	if _, err := TlsCertOrder(world.ctx, revoked, args); !tlsTestStatus(err, "401") {
		t.Fatalf("a revoked token answered %v", err)
	}
	// a token that names no sidecar
	now := world.host.Clock.Now()
	noSidecar := world.host.Authenticator.Issue(&sidecar.Principal{
		NetworkId: connect.Id(device.networkId),
		ClientId:  connect.Id(device.clientId),
		Audience:  TlsCertsAudience,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
	if _, err := TlsCertOrder(world.ctx, noSidecar, args); !tlsTestStatus(err, "403") {
		t.Fatalf("a token with no sidecar answered %v", err)
	}

	// a caller with no grant
	world.subjects[device.sidecarId].Granted = false
	if _, err := TlsCertOrder(world.ctx, device.token, args); !tlsTestStatus(err, "403") {
		t.Fatalf("a caller with no grant answered %v", err)
	}
	world.subjects[device.sidecarId].Granted = true

	// a CSR for another network's set, attested by the caller's own key
	if _, err := TlsCertOrder(world.ctx, device.token, world.orderArgs(device, other.identifierSet)); !tlsTestStatus(err, "400") {
		t.Fatalf("a CSR for another network's set answered %v", err)
	}
	// another device's order under this caller's token: the attestation is not the token's sidecar's
	if _, err := TlsCertOrder(world.ctx, device.token, world.orderArgs(other, device.identifierSet)); !tlsTestStatus(err, "400") {
		t.Fatalf("another device's attestation answered %v", err)
	}
	if _, err := TlsCertOrder(world.ctx, device.token, nil); !tlsTestStatus(err, "400") {
		t.Fatalf("no body answered %v", err)
	}

	// a projection that cannot answer takes no order
	world.subjectErr = errors.New("no verified state")
	if _, err := TlsCertOrder(world.ctx, device.token, args); !tlsTestStatus(err, "503") {
		t.Fatalf("an order with no subject answered %v", err)
	}
	world.subjectErr = nil
	if rows := world.store.Rows(); len(rows) != 0 {
		t.Fatalf("a refused order wrote %d rows", len(rows))
	}

	// nothing installed: every route is unavailable
	SetTlsCerts(nil)
	clientSession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(device.networkId)})
	defer clientSession.Cancel()
	if _, err := TlsCertOrder(world.ctx, device.token, args); !tlsTestStatus(err, "503") {
		t.Fatalf("an order with nothing installed answered %v", err)
	}
	if _, err := TlsCertOrderStatus(world.ctx, device.token, server.NewId()); !tlsTestStatus(err, "503") {
		t.Fatalf("a status with nothing installed answered %v", err)
	}
	if _, err := TlsCerts(clientSession); !tlsTestStatus(err, "503") {
		t.Fatalf("a list with nothing installed answered %v", err)
	}
	if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: sidecar.NewId()}, clientSession); !tlsTestStatus(err, "503") {
		t.Fatalf("a revocation with nothing installed answered %v", err)
	}
	// deps missing a field install nothing
	SetTlsCerts(&TlsCertsDeps{Service: world.service, Store: world.store, Authenticator: world.host.Authenticator})
	if _, err := TlsCertOrder(world.ctx, device.token, args); !tlsTestStatus(err, "503") {
		t.Fatalf("an order on partial deps answered %v", err)
	}
}

// An issued row of the device's, as an order's end leaves it.
func (self *tlsTestWorld) issued(device *tlsTestDevice) sidecar.Id {
	self.t.Helper()
	now := self.host.Clock.Now()
	row := &sidecar.TlsCertRecord{
		CertId:              sidecartest.CertsIdAt(now),
		NetworkId:           device.networkId,
		ClientId:            device.clientId,
		SidecarId:           device.sidecarId,
		IdentifierSetSha256: make([]byte, 32),
		SpkiSha256:          make([]byte, 32),
		Attestation:         []byte("attestation"),
		Csr:                 []byte("csr"),
		Ca:                  string(certsservice.CertsCaLetsEncrypt),
		Serial:              "0a",
		NotBefore:           now,
		NotAfter:            now.Add(45 * 24 * time.Hour),
		State:               sidecar.TlsCertStateIssued,
	}
	if err := self.store.InsertCert(self.ctx, row); err != nil {
		self.t.Fatal(err)
	}
	return row.CertId
}

func (self *tlsTestWorld) state(certId sidecar.Id) sidecar.TlsCertState {
	self.t.Helper()
	row, err := self.store.GetCert(self.ctx, certId)
	if err != nil {
		self.t.Fatal(err)
	}
	return row.State
}

func TestTlsCertsListsTheBearersNetwork(t *testing.T) {
	world := newTlsTestWorld(t)
	device := world.device(sidecar.NewId(), "acme")
	other := world.device(sidecar.NewId(), "other")
	certId := world.issued(device)
	world.issued(other)

	clientId := server.Id(device.clientId)
	clientSession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(device.networkId), ClientId: &clientId})
	defer clientSession.Cancel()
	result, err := TlsCerts(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Certs) != 1 || result.Certs[0].CertId != certId || result.Certs[0].Csr != nil || len(result.Certs[0].Attestation) == 0 {
		t.Fatalf("the network's certificates: %+v", result.Certs)
	}
}

func TestTlsCertRevokeByOwnClientAndByItsNetwork(t *testing.T) {
	world := newTlsTestWorld(t)
	networkId := sidecar.NewId()
	device := world.device(networkId, "acme")
	sibling := world.device(networkId, "acme")
	stranger := world.device(sidecar.NewId(), "other")
	own := world.issued(device)
	siblings := world.issued(sibling)
	strangers := world.issued(stranger)

	clientSessionOf := func(device *tlsTestDevice) *session.ClientSession {
		clientId := server.Id(device.clientId)
		clientSession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(device.networkId), ClientId: &clientId})
		t.Cleanup(clientSession.Cancel)
		return clientSession
	}
	deviceSession := clientSessionOf(device)
	networkSession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(networkId)})
	defer networkSession.Cancel()

	// another client's certificate, of its network: refused; another network's: not found
	if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: siblings}, deviceSession); !tlsTestStatus(err, "403") {
		t.Fatalf("another client's certificate answered %v", err)
	}
	for _, clientSession := range []*session.ClientSession{deviceSession, networkSession} {
		if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: strangers}, clientSession); !tlsTestStatus(err, "404") {
			t.Fatalf("another network's certificate answered %v", err)
		}
	}
	if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: sidecar.NewId()}, deviceSession); !tlsTestStatus(err, "404") {
		t.Fatalf("an unknown certificate answered %v", err)
	}
	if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{}, deviceSession); !tlsTestStatus(err, "400") {
		t.Fatalf("no certificate answered %v", err)
	}
	if world.state(siblings) != sidecar.TlsCertStateIssued || world.state(strangers) != sidecar.TlsCertStateIssued {
		t.Fatal("a refused revocation moved a row")
	}

	// a network credential that is no admin's: an API key, and a team member whose role lacks devices
	apiKeySession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(networkId)})
	defer apiKeySession.Cancel()
	apiKeySession.ApiKeyAuthenticated = true
	memberId := server.NewId()
	memberSession := session.NewLocalClientSession(world.ctx, "", &session.ByJwt{NetworkId: server.Id(networkId), MemberId: &memberId})
	defer memberSession.Cancel()
	previousPermitted := tlsCertRevokeMemberPermitted
	defer func() {
		tlsCertRevokeMemberPermitted = previousPermitted
	}()
	memberPermitted := false
	tlsCertRevokeMemberPermitted = func(ctx context.Context, askedNetworkId server.Id, askedMemberId server.Id) bool {
		return memberPermitted && askedNetworkId == server.Id(networkId) && askedMemberId == memberId
	}
	for _, clientSession := range []*session.ClientSession{apiKeySession, memberSession} {
		if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: siblings}, clientSession); !tlsTestStatus(err, "403") {
			t.Fatalf("a credential that is no admin's answered %v", err)
		}
	}
	if world.state(siblings) != sidecar.TlsCertStateIssued {
		t.Fatal("a refused credential moved a row")
	}
	// the member's role gains devices: any of its network's, and still none of another's
	memberPermitted = true
	membersOwn := world.issued(sibling)
	if result, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: membersOwn}, memberSession); err != nil || result.State != sidecar.TlsCertStateRevoking {
		t.Fatalf("an admin member's revocation: %+v, %v", result, err)
	}
	if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: strangers}, memberSession); !tlsTestStatus(err, "404") {
		t.Fatalf("an admin member on another network's certificate answered %v", err)
	}

	// its own, twice: the second answers the state it is in
	for range 2 {
		result, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: own}, deviceSession)
		if err != nil || result.State != sidecar.TlsCertStateRevoking {
			t.Fatalf("its own certificate: %+v, %v", result, err)
		}
	}
	// the network's credential, any of its network's
	if result, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: siblings}, networkSession); err != nil || result.State != sidecar.TlsCertStateRevoking {
		t.Fatalf("the network's revocation: %+v, %v", result, err)
	}
	if world.state(own) != sidecar.TlsCertStateRevoking || world.state(siblings) != sidecar.TlsCertStateRevoking || world.state(strangers) != sidecar.TlsCertStateIssued {
		t.Fatal("the rows are not where the revocations left them")
	}

	// an order that never issued has nothing to revoke
	queued, err := TlsCertOrder(world.ctx, device.token, world.orderArgs(device, device.identifierSet))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TlsCertRevoke(&sidecar.TlsCertRevokeArgs{CertId: queued.CertId}, deviceSession); !tlsTestStatus(err, "409") {
		t.Fatalf("a queued order's revocation answered %v", err)
	}
}
