//go:build sidecar_pending

package model

// Team custom-domain certificates on a private database (TLS-M6-05; DESIGN-DETAILS 17.2.4 to 17.2.13, 17.6): the
// account row made by an admin's desktop alone; a device's CSR queued, taken again and superseded; the lock taken,
// renewed, refused while held and taken over once lapsed, every write under a stale claim refused and changing
// nothing; the one row in flight an account; order_recorded once; a chain registered only on the row's key and one
// name; the exact-set bucket certifying a host's first four devices at once and the fifth 34 hours on; a 429's hold;
// a row failed under an order going to custom_revoking and settled before the account orders again; the removal
// moves; the work list in plan order; the key change's row operations; and the listen facts' two maps. Names are
// under .example, keys generated.

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// A custom-domain world: a Team network with a verified domain, its active host www.<domain> mapped to shop, the
// store on a clock the test moves, and a CA of the test's own.
type testTlsCustomWorld struct {
	*testDnsCustomHostWorld
	ctx    context.Context
	store  *TlsCustomStore
	now    time.Time
	host   string
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
}

func newTestTlsCustomWorld(t testing.TB, ctx context.Context) *testTlsCustomWorld {
	t.Helper()
	dnsWorld := newTestDnsCustomHostWorld(t, ctx)
	host := "www." + dnsWorld.domain
	if _, err := dnsWorld.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: "shop." + dnsWorld.apex}); err != nil {
		t.Fatal(err)
	}
	if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: host, Pass: true}); err != nil {
		t.Fatal(err)
	}
	world := &testTlsCustomWorld{
		testDnsCustomHostWorld: dnsWorld,
		ctx:                    ctx,
		now:                    time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC),
		host:                   host,
	}
	store, err := NewTlsCustomStore(&TlsCustomStoreOptions{
		Roots: []string{strings.TrimSuffix(dnsWorld.root, ".")},
		Now:   func() time.Time { return world.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	world.store = store
	world.caKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test custom ca"},
		NotBefore:             world.now.Add(-time.Hour),
		NotAfter:              world.now.Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDer, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &world.caKey.PublicKey, world.caKey)
	if err != nil {
		t.Fatal(err)
	}
	world.caCert, err = x509.ParseCertificate(caDer)
	if err != nil {
		t.Fatal(err)
	}
	return world
}

// One device or desktop of the network: its sidecar, identity key and TLS key.
type testTlsCustomDevice struct {
	sidecarId   server.Id
	clientId    server.Id
	identityKey ed25519.PrivateKey
	tlsKey      *ecdsa.PrivateKey
}

func newTestTlsCustomDevice(t testing.TB) *testTlsCustomDevice {
	t.Helper()
	_, identityKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testTlsCustomDevice{sidecarId: server.NewId(), clientId: server.NewId(), identityKey: identityKey, tlsKey: tlsKey}
}

// The device as a caller holding the publish grant on shop's tls/ names, an admin when admin.
func (self *testTlsCustomWorld) caller(device *testTlsCustomDevice, admin bool) *TlsCustomCaller {
	return &TlsCustomCaller{
		NetworkId:         self.networkId,
		ClientId:          device.clientId,
		SidecarId:         device.sidecarId,
		PublishGranted:    func(ownerLabel string) bool { return ownerLabel == "shop" },
		Admin:             admin,
		IdentityPublicKey: device.identityKey.Public().(ed25519.PublicKey),
	}
}

// The device's CSR for host and its attestation of it.
func (self *testTlsCustomWorld) orderArgs(t testing.TB, device *testTlsCustomDevice, host string) *sidecar.TlsCertOrderArgs {
	t.Helper()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{host}}, device.tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&device.tlsKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	spkiSha256 := sha256.Sum256(spki)
	setSha256, err := sidecar.TlsIdentifierSetHash([]string{host})
	if err != nil {
		t.Fatal(err)
	}
	sign := func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
		return &bridge.Signed{
			Body:      body,
			SignerId:  device.sidecarId.Bytes(),
			Signature: ed25519.Sign(device.identityKey, bridge.SignedBytes(purpose, signedType, body)),
			Type:      uint32(signedType),
		}, nil
	}
	attestation, err := sidecar.SignTlsKeyAttestation(&protocol.TlsKeyAttestation{
		NetworkId:           self.networkId.Bytes(),
		SidecarId:           device.sidecarId.Bytes(),
		SpkiSha256:          spkiSha256[:],
		IdentifierSetSha256: setSha256[:],
		IssueTimeMillis:     uint64(self.now.UnixMilli()),
	}, sign)
	if err != nil {
		t.Fatal(err)
	}
	return &sidecar.TlsCertOrderArgs{Csr: csr, Attestation: attestation}
}

// A chain from the world's CA for publicKey naming names, valid from the world's now for 45 days.
func (self *testTlsCustomWorld) chain(t testing.TB, publicKey any, names []string, notAfter time.Time) [][]byte {
	t.Helper()
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    self.now,
		NotAfter:     notAfter,
		DNSNames:     names,
	}
	leaf, err := x509.CreateCertificate(rand.Reader, template, self.caCert, publicKey, self.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return [][]byte{leaf, self.caCert.Raw}
}

// The account, made by admin's desktop.
func (self *testTlsCustomWorld) createAccount(t testing.TB, admin *testTlsCustomDevice) *sidecar.TlsCustomAccount {
	t.Helper()
	thumbprint := sha256.Sum256([]byte("account key 1"))
	account, err := self.store.AccountOp(self.ctx, self.caller(admin, true), &sidecar.TlsCustomAccountArgs{
		Domain:        self.domain,
		Op:            sidecar.TlsCustomAccountCreate,
		AccountUri:    "https://ca.example/acme/acct/1",
		KeyThumbprint: thumbprint[:],
		KeyRepoId:     sidecar.NewId(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

// Queues device's CSR for the world's host, failing the test on a refusal.
func (self *testTlsCustomWorld) queue(t testing.TB, device *testTlsCustomDevice) sidecar.Id {
	t.Helper()
	result, err := self.store.QueueOrder(self.ctx, self.caller(device, false), self.orderArgs(t, device, self.host), server.NewId(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return result.CertId
}

// The refusal's status and reason, failing the test when err is none.
func testTlsCustomRefused(t testing.TB, err error) *sidecar.TlsCustomError {
	t.Helper()
	var refusal *sidecar.TlsCustomError
	if !errors.As(err, &refusal) {
		t.Fatalf("not a refusal: %v", err)
	}
	return refusal
}

// The row's state and order URL as the database holds them.
func (self *testTlsCustomWorld) row(t testing.TB, certId sidecar.Id) *tlsCustomCertRow {
	t.Helper()
	var row *tlsCustomCertRow
	server.Db(self.ctx, func(conn server.PgConn) {
		rows := getTlsCustomCertsInTx(self.ctx, conn, `cert_id = $1`, server.Id(certId))
		if len(rows) == 1 {
			row = rows[0]
		}
	})
	if row == nil {
		t.Fatalf("no row %s", certId)
	}
	return row
}

func TestTlsCustomAccountIsAnAdminDesktops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		desktop := newTestTlsCustomDevice(t)
		thumbprint := sha256.Sum256([]byte("account key 1"))
		args := &sidecar.TlsCustomAccountArgs{
			Domain:        world.domain,
			Op:            sidecar.TlsCustomAccountCreate,
			AccountUri:    "https://ca.example/acme/acct/1",
			KeyThumbprint: thumbprint[:],
			KeyRepoId:     sidecar.NewId(),
		}
		if _, err := world.store.AccountOp(ctx, world.caller(desktop, false), args); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalOrderer {
			t.Fatalf("a desktop that is no admin's: %v", err)
		}
		account, err := world.store.AccountOp(ctx, world.caller(desktop, true), args)
		if err != nil || account.KeySeq != 1 || account.AccountUri != args.AccountUri || account.AcmeName == "" {
			t.Fatalf("%+v, %v", account, err)
		}
		if _, err := world.store.AccountOp(ctx, world.caller(desktop, true), args); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalExists {
			t.Fatalf("a second create: %v", err)
		}
	})
}

func TestTlsCustomQueueOrderRefusesAndSupersedes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		device := newTestTlsCustomDevice(t)
		// no account yet
		_, err := world.store.QueueOrder(ctx, world.caller(device, false), world.orderArgs(t, device, world.host), server.NewId(), time.Second)
		if refusal := testTlsCustomRefused(t, err); refusal.Status != http.StatusConflict || refusal.Refusal.Reason != sidecar.TlsCustomRefusalNoAccount {
			t.Fatalf("no account: %+v", refusal)
		}
		world.createAccount(t, newTestTlsCustomDevice(t))
		// a host that is not active, no grant, another device's attestation
		_, err = world.store.QueueOrder(ctx, world.caller(device, false), world.orderArgs(t, device, "api."+world.domain), server.NewId(), time.Second)
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalHost {
			t.Errorf("an inactive host: %+v", refusal)
		}
		ungranted := world.caller(device, false)
		ungranted.PublishGranted = func(string) bool { return false }
		_, err = world.store.QueueOrder(ctx, ungranted, world.orderArgs(t, device, world.host), server.NewId(), time.Second)
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalGrant {
			t.Errorf("no grant: %+v", refusal)
		}
		other := newTestTlsCustomDevice(t)
		_, err = world.store.QueueOrder(ctx, world.caller(device, false), world.orderArgs(t, other, world.host), server.NewId(), time.Second)
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalCsr {
			t.Errorf("another device's key: %+v", refusal)
		}
		// queued, the same CSR answering its row, and a new key superseding a queued row
		args := world.orderArgs(t, device, world.host)
		first, err := world.store.QueueOrder(ctx, world.caller(device, false), args, server.NewId(), time.Second)
		if err != nil || first.State != sidecar.TlsCertStateCustomQueued {
			t.Fatalf("%+v, %v", first, err)
		}
		again, err := world.store.QueueOrder(ctx, world.caller(device, false), args, server.NewId(), time.Second)
		if err != nil || again.CertId != first.CertId {
			t.Fatalf("the same CSR again: %+v, %v", again, err)
		}
		rotated := &testTlsCustomDevice{sidecarId: device.sidecarId, clientId: device.clientId, identityKey: device.identityKey}
		rotated.tlsKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		second, err := world.store.QueueOrder(ctx, world.caller(rotated, false), world.orderArgs(t, rotated, world.host), server.NewId(), time.Second)
		if err != nil || second.CertId == first.CertId {
			t.Fatalf("a new key: %+v, %v", second, err)
		}
		if row := world.row(t, first.CertId); row.state != sidecar.TlsCertStateFailed || row.customReason == nil || *row.customReason != sidecar.TlsCustomReasonSuperseded {
			t.Errorf("the superseded row: %+v", row)
		}
	})
}

func TestTlsCustomClaimHoldsOneLockAndOneOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		account := world.createAccount(t, newTestTlsCustomDevice(t))
		desktop1 := newTestTlsCustomDevice(t)
		desktop2 := newTestTlsCustomDevice(t)
		phone1 := newTestTlsCustomDevice(t)
		phone2 := newTestTlsCustomDevice(t)
		row1 := world.queue(t, phone1)
		row2 := world.queue(t, phone2)
		claimArgs := func(certId sidecar.Id) *sidecar.TlsCustomClaimArgs {
			return &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: certId, KeySeq: account.KeySeq}
		}
		claim, err := world.store.Claim(ctx, world.caller(desktop1, false), claimArgs(row1))
		if err != nil || claim.ClaimSeq != 1 || !claim.MayOrder || claim.Record == nil || claim.Record.State != sidecar.TlsCertStateCustomOrdering {
			t.Fatalf("%+v, %v", claim, err)
		}
		// another desktop waits out the lease
		_, err = world.store.Claim(ctx, world.caller(desktop2, false), claimArgs(row1))
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalLocked || refusal.Refusal.RetryAfterMs == 0 {
			t.Fatalf("a held lock: %+v", refusal)
		}
		// the holder renews: the same sequence, no second newOrder
		renewed, err := world.store.Claim(ctx, world.caller(desktop1, false), claimArgs(row1))
		if err != nil || renewed.ClaimSeq != claim.ClaimSeq || renewed.MayOrder {
			t.Fatalf("a renewal: %+v, %v", renewed, err)
		}
		// a stale key_seq
		stale := claimArgs(row1)
		stale.KeySeq = 7
		_, err = world.store.Claim(ctx, world.caller(desktop1, false), stale)
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalKeySeq || refusal.Refusal.KeySeq != account.KeySeq {
			t.Fatalf("a stale key_seq: %+v", refusal)
		}
		// order_recorded once
		if _, err := world.store.Progress(ctx, world.caller(desktop1, false), &sidecar.TlsCustomProgressArgs{
			Domain: world.domain, ClaimSeq: claim.ClaimSeq, CertId: row1, Event: sidecar.TlsCustomEventOrderRecorded, OrderUrl: "https://ca.example/acme/order/1",
		}); err != nil {
			t.Fatal(err)
		}
		_, err = world.store.Progress(ctx, world.caller(desktop1, false), &sidecar.TlsCustomProgressArgs{
			Domain: world.domain, ClaimSeq: claim.ClaimSeq, CertId: row1, Event: sidecar.TlsCustomEventOrderRecorded, OrderUrl: "https://ca.example/acme/order/2",
		})
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalState {
			t.Fatalf("a second order recorded: %+v", refusal)
		}
		// the lease lapses: the other desktop takes over, finding the recorded order and no grant
		world.now = world.now.Add(3 * time.Minute)
		takeover, err := world.store.Claim(ctx, world.caller(desktop2, false), claimArgs(row1))
		if err != nil || takeover.ClaimSeq != claim.ClaimSeq+1 || takeover.MayOrder || takeover.OrderUrl != "https://ca.example/acme/order/1" {
			t.Fatalf("the takeover: %+v, %v", takeover, err)
		}
		// the stalled holder's every write is refused and changes nothing
		for _, event := range []string{sidecar.TlsCustomEventRelease, sidecar.TlsCustomEventFailed, sidecar.TlsCustomEventOrderInvalid} {
			_, err := world.store.Progress(ctx, world.caller(desktop1, false), &sidecar.TlsCustomProgressArgs{
				Domain: world.domain, ClaimSeq: claim.ClaimSeq, CertId: row1, Event: event, Reason: sidecar.TlsCustomReasonCsr,
			})
			if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalClaim {
				t.Errorf("%s under a stale claim: %+v", event, refusal)
			}
		}
		if row := world.row(t, row1); row.state != sidecar.TlsCertStateCustomOrdering || row.orderUrl == nil {
			t.Fatalf("after the stale writes: %+v", row)
		}
		// one order in flight an account: another row waits for it
		world.now = world.now.Add(3 * time.Minute)
		_, err = world.store.Claim(ctx, world.caller(desktop1, false), claimArgs(row2))
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalInFlight || refusal.Refusal.CertId != row1 {
			t.Fatalf("a second row in flight: %+v", refusal)
		}
	})
}

func TestTlsCustomRegisterTakesOnlyTheRowsKeyAndName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		account := world.createAccount(t, newTestTlsCustomDevice(t))
		desktop := newTestTlsCustomDevice(t)
		phone := newTestTlsCustomDevice(t)
		certId := world.queue(t, phone)
		claim, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: certId, KeySeq: account.KeySeq})
		if err != nil {
			t.Fatal(err)
		}
		lifetime := 45 * 24 * time.Hour
		other := newTestTlsCustomDevice(t)
		register := func(chain [][]byte, accountUri string, claimSeq uint64) error {
			return world.store.Register(ctx, world.caller(desktop, false), &sidecar.TlsCertRegisterArgs{CertId: certId, ClaimSeq: claimSeq, Chain: chain, AccountUri: accountUri})
		}
		unchained := world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, world.now.Add(lifetime))
		unchained[1] = world.chain(t, &other.tlsKey.PublicKey, []string{"other.example"}, world.now.Add(lifetime))[0]
		for _, c := range []struct {
			chain      [][]byte
			accountUri string
			claimSeq   uint64
			reason     string
		}{
			{chain: world.chain(t, &other.tlsKey.PublicKey, []string{world.host}, world.now.Add(lifetime)), accountUri: account.AccountUri, claimSeq: claim.ClaimSeq, reason: sidecar.TlsCustomRefusalChain},
			{chain: world.chain(t, &phone.tlsKey.PublicKey, []string{world.host, "api." + world.domain}, world.now.Add(lifetime)), accountUri: account.AccountUri, claimSeq: claim.ClaimSeq, reason: sidecar.TlsCustomRefusalChain},
			{chain: world.chain(t, &phone.tlsKey.PublicKey, []string{"*." + world.domain}, world.now.Add(lifetime)), accountUri: account.AccountUri, claimSeq: claim.ClaimSeq, reason: sidecar.TlsCustomRefusalChain},
			{chain: world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, world.now.Add(-time.Minute)), accountUri: account.AccountUri, claimSeq: claim.ClaimSeq, reason: sidecar.TlsCustomRefusalChain},
			{chain: unchained, accountUri: account.AccountUri, claimSeq: claim.ClaimSeq, reason: sidecar.TlsCustomRefusalChain},
			{chain: world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, world.now.Add(lifetime)), accountUri: "https://ca.example/acme/acct/9", claimSeq: claim.ClaimSeq, reason: sidecar.TlsCustomRefusalChain},
			{chain: world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, world.now.Add(lifetime)), accountUri: account.AccountUri, claimSeq: claim.ClaimSeq + 1, reason: sidecar.TlsCustomRefusalClaim},
		} {
			if refusal := testTlsCustomRefused(t, register(c.chain, c.accountUri, c.claimSeq)); refusal.Refusal.Reason != c.reason {
				t.Errorf("want %s: %+v", c.reason, refusal)
			}
		}
		if row := world.row(t, certId); row.state != sidecar.TlsCertStateCustomOrdering {
			t.Fatalf("a refused register moved the row: %s", row.state)
		}
		if err := register(world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, world.now.Add(lifetime)), account.AccountUri, claim.ClaimSeq); err != nil {
			t.Fatal(err)
		}
		row := world.row(t, certId)
		if row.state != sidecar.TlsCertStateCustomIssued || len(row.chain) != 2 || row.notAfter == nil {
			t.Fatalf("registered: %+v", row)
		}
		// the lock is free: another claim of the account takes it
		phone2 := newTestTlsCustomDevice(t)
		next := world.queue(t, phone2)
		if _, err := world.store.Claim(ctx, world.caller(newTestTlsCustomDevice(t), false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: next, KeySeq: account.KeySeq}); err != nil {
			t.Fatalf("after the register: %v", err)
		}
	})
}

// The exact-set bucket keeps one certificate back: a host's first four devices are certified at once, and the fifth
// is refused with a wait; the replay starts empty at its window's start, so a burst holds the bucket until it leaves
// the window, 5 × 34 h on, when the fifth is granted (17.2.8; 17.6's six devices).
func TestTlsCustomBudgetsCertifyFourDevicesAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		account := world.createAccount(t, newTestTlsCustomDevice(t))
		desktop := newTestTlsCustomDevice(t)
		certify := func() (sidecar.Id, error) {
			phone := newTestTlsCustomDevice(t)
			certId := world.queue(t, phone)
			claim, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: certId, KeySeq: account.KeySeq})
			if err != nil {
				return certId, err
			}
			err = world.store.Register(ctx, world.caller(desktop, false), &sidecar.TlsCertRegisterArgs{
				CertId: certId, ClaimSeq: claim.ClaimSeq, AccountUri: account.AccountUri,
				Chain: world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, world.now.Add(45*24*time.Hour)),
			})
			return certId, err
		}
		for index := range 4 {
			if _, err := certify(); err != nil {
				t.Fatalf("device %d: %v", index+1, err)
			}
			world.now = world.now.Add(time.Second)
		}
		fifth, err := certify()
		refusal := testTlsCustomRefused(t, err)
		if refusal.Refusal.Reason != sidecar.TlsCustomRefusalBudget || refusal.Refusal.RetryAfterMs < uint32((33*time.Hour).Milliseconds()) {
			t.Fatalf("the fifth device: %+v", refusal)
		}
		// a refused claim holds no one up: the row is queued, the lock free
		if row := world.row(t, fifth); row.state != sidecar.TlsCertStateCustomQueued {
			t.Fatalf("the refused row: %s", row.state)
		}
		world.now = world.now.Add(5*34*time.Hour + time.Minute)
		if _, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: fifth, KeySeq: account.KeySeq}); err != nil {
			t.Fatalf("once the burst left the window: %v", err)
		}
	})
}

// A CA's 429 on an ordinary newOrder holds ordinary orders; the row is queued again and the lock freed.
func TestTlsCustomRateLimitedHoldsNewOrders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		account := world.createAccount(t, newTestTlsCustomDevice(t))
		desktop := newTestTlsCustomDevice(t)
		certId := world.queue(t, newTestTlsCustomDevice(t))
		args := &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: certId, KeySeq: account.KeySeq}
		claim, err := world.store.Claim(ctx, world.caller(desktop, false), args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := world.store.Progress(ctx, world.caller(desktop, false), &sidecar.TlsCustomProgressArgs{
			Domain: world.domain, ClaimSeq: claim.ClaimSeq, CertId: certId, Event: sidecar.TlsCustomEventRateLimited, RetryAfterMs: 1000,
		}); err != nil {
			t.Fatal(err)
		}
		if row := world.row(t, certId); row.state != sidecar.TlsCertStateCustomQueued {
			t.Fatalf("after a 429: %s", row.state)
		}
		_, err = world.store.Claim(ctx, world.caller(newTestTlsCustomDevice(t), false), args)
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalHold || refusal.Refusal.RetryAfterMs < uint32(time.Minute.Milliseconds()) {
			t.Fatalf("a held account: %+v", refusal)
		}
	})
}

// A row that fails with an order recorded goes to custom_revoking, keeping its order; the account orders nothing
// else until the revoke pass settles it (17.2.4, 17.2.12, C10).
func TestTlsCustomFailedUnderAnOrderIsSettledFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		account := world.createAccount(t, newTestTlsCustomDevice(t))
		desktop := newTestTlsCustomDevice(t)
		certId := world.queue(t, newTestTlsCustomDevice(t))
		other := world.queue(t, newTestTlsCustomDevice(t))
		claim, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: certId, KeySeq: account.KeySeq})
		if err != nil {
			t.Fatal(err)
		}
		progress := func(event string, claimSeq uint64, rowId sidecar.Id) error {
			_, err := world.store.Progress(ctx, world.caller(desktop, false), &sidecar.TlsCustomProgressArgs{
				Domain: world.domain, ClaimSeq: claimSeq, CertId: rowId, Event: event, OrderUrl: "https://ca.example/acme/order/1", Reason: sidecar.TlsCustomReasonAttestation,
			})
			return err
		}
		if err := progress(sidecar.TlsCustomEventOrderRecorded, claim.ClaimSeq, certId); err != nil {
			t.Fatal(err)
		}
		if err := progress(sidecar.TlsCustomEventFailed, claim.ClaimSeq, certId); err != nil {
			t.Fatal(err)
		}
		if row := world.row(t, certId); row.state != sidecar.TlsCertStateCustomRevoking || row.orderUrl == nil {
			t.Fatalf("failed under an order: %+v", row)
		}
		_, err = world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: other, KeySeq: account.KeySeq})
		if refusal := testTlsCustomRefused(t, err); refusal.Refusal.Reason != sidecar.TlsCustomRefusalInFlight || refusal.Refusal.CertId != certId {
			t.Fatalf("another row before the settle: %+v", refusal)
		}
		revoke, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeRevoke, CertId: certId, KeySeq: account.KeySeq})
		if err != nil || revoke.OrderUrl == "" {
			t.Fatalf("the revoke claim: %+v, %v", revoke, err)
		}
		if err := progress(sidecar.TlsCustomEventRevoked, revoke.ClaimSeq, certId); err != nil {
			t.Fatal(err)
		}
		if row := world.row(t, certId); row.state != sidecar.TlsCertStateRevoked {
			t.Fatalf("after the settle: %s", row.state)
		}
		if _, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: other, KeySeq: account.KeySeq}); err != nil {
			t.Fatalf("the next row: %v", err)
		}
	})
}

// 17.2.4's removal moves: an issued row and an ordering row with an order go to custom_revoking, a queued one fails.
func TestTlsCustomRemovalMovesRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		account := world.createAccount(t, newTestTlsCustomDevice(t))
		desktop := newTestTlsCustomDevice(t)
		phone := newTestTlsCustomDevice(t)
		issued := world.queue(t, phone)
		claim, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: issued, KeySeq: account.KeySeq})
		if err != nil {
			t.Fatal(err)
		}
		if err := world.store.Register(ctx, world.caller(desktop, false), &sidecar.TlsCertRegisterArgs{
			CertId: issued, ClaimSeq: claim.ClaimSeq, AccountUri: account.AccountUri,
			Chain: world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, world.now.Add(45*24*time.Hour)),
		}); err != nil {
			t.Fatal(err)
		}
		queued := world.queue(t, newTestTlsCustomDevice(t))
		moved := 0
		server.Tx(ctx, func(tx server.PgTx) {
			moved = TlsCustomRemoveRowsInTx(ctx, tx, &TlsCustomRemoval{NetworkId: world.networkId, Host: world.host})
		})
		if moved != 2 {
			t.Fatalf("moved %d", moved)
		}
		if row := world.row(t, issued); row.state != sidecar.TlsCertStateCustomRevoking || row.revocationReason == nil || *row.revocationReason != string(sidecar.TlsCertRevocationReasonCessationOfOperation) {
			t.Errorf("the issued row: %+v", row)
		}
		if row := world.row(t, queued); row.state != sidecar.TlsCertStateFailed {
			t.Errorf("the queued row: %s", row.state)
		}
	})
}

// The key change's row operations under a rollover claim (17.2.13): one roll_begin, roll_end moving key_seq to the
// recorded next key, and claims at either sequence while the change is in flight.
func TestTlsCustomKeyChangeOps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		admin := newTestTlsCustomDevice(t)
		account := world.createAccount(t, admin)
		claim, err := world.store.Claim(ctx, world.caller(admin, true), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeRollover, KeySeq: 1})
		if err != nil {
			t.Fatal(err)
		}
		next := sha256.Sum256([]byte("account key 2"))
		repoId := sidecar.NewId()
		op := func(op string, thumbprint []byte) (*sidecar.TlsCustomAccount, error) {
			return world.store.AccountOp(ctx, world.caller(admin, true), &sidecar.TlsCustomAccountArgs{
				Domain: world.domain, Op: op, AccountUri: account.AccountUri, KeySeq: 1, KeyThumbprint: thumbprint, KeyRepoId: repoId, ClaimSeq: claim.ClaimSeq,
			})
		}
		begun, err := op(sidecar.TlsCustomAccountRollBegin, next[:])
		if err != nil || string(begun.NextKeyThumbprint) != string(next[:]) {
			t.Fatalf("roll_begin: %+v, %v", begun, err)
		}
		if _, err := op(sidecar.TlsCustomAccountRollBegin, next[:]); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalState {
			t.Fatalf("a second roll_begin: %v", err)
		}
		// an orderer holding the next key's file is accepted at key_seq + 1 while the change is in flight
		other := newTestTlsCustomDevice(t)
		world.now = world.now.Add(3 * time.Minute)
		if _, err := world.store.Claim(ctx, world.caller(other, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeRollover, KeySeq: 2}); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalOrderer {
			t.Fatalf("a rollover claim by no admin: %v", err)
		}
		world.now = world.now.Add(-3 * time.Minute)
		if _, err := op(sidecar.TlsCustomAccountRollEnd, []byte("not the next key's")); testTlsCustomRefused(t, err).Refusal.Reason != sidecar.TlsCustomRefusalState {
			t.Fatalf("roll_end with another key: %v", err)
		}
		ended, err := op(sidecar.TlsCustomAccountRollEnd, next[:])
		if err != nil || ended.KeySeq != 2 || string(ended.KeyThumbprint) != string(next[:]) || ended.NextKeyThumbprint != nil || ended.KeyRepoId != repoId {
			t.Fatalf("roll_end: %+v, %v", ended, err)
		}
	})
}

// The listen facts' two lists (17.2.10): a sidecar's custom_issued rows of active hosts on their owner label, and the
// hosts it holds an unlapsed order claim for.
func TestTlsCustomListenFacts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestTlsCustomWorld(t, ctx)
		account := world.createAccount(t, newTestTlsCustomDevice(t))
		desktop := newTestTlsCustomDevice(t)
		phone := newTestTlsCustomDevice(t)
		certId := world.queue(t, phone)
		claim, err := world.store.Claim(ctx, world.caller(desktop, false), &sidecar.TlsCustomClaimArgs{Domain: world.domain, Purpose: sidecar.TlsCustomPurposeOrder, CertId: certId, KeySeq: account.KeySeq})
		if err != nil {
			t.Fatal(err)
		}
		facts := func() (map[string]time.Time, map[string]uint64) {
			certified := map[string]time.Time{}
			acme := map[string]uint64{}
			server.Db(ctx, func(conn server.PgConn) {
				names, claims := getTlsCustomListenFactsInTx(ctx, conn, world.networkId, world.now)
				for _, name := range names {
					if name.Owner == "shop" {
						certified[server.Id(name.SidecarId).String()+" "+name.Name] = name.NotAfter
					}
				}
				for _, claim := range claims {
					if claim.Owner == "shop" {
						acme[server.Id(claim.SidecarId).String()+" "+claim.Name] = claim.ClaimSeq
					}
				}
			})
			return certified, acme
		}
		if _, acme := facts(); acme[desktop.sidecarId.String()+" "+world.host] != claim.ClaimSeq {
			t.Fatalf("the holder's claim: %v", acme)
		}
		notAfter := world.now.Add(45 * 24 * time.Hour)
		if err := world.store.Register(ctx, world.caller(desktop, false), &sidecar.TlsCertRegisterArgs{
			CertId: certId, ClaimSeq: claim.ClaimSeq, AccountUri: account.AccountUri,
			Chain: world.chain(t, &phone.tlsKey.PublicKey, []string{world.host}, notAfter),
		}); err != nil {
			t.Fatal(err)
		}
		certified, acme := facts()
		if got := certified[phone.sidecarId.String()+" "+world.host]; !got.Equal(notAfter.Truncate(time.Second)) {
			t.Fatalf("certified %v", certified)
		}
		if 0 < len(acme) {
			t.Fatalf("a freed lock's claim: %v", acme)
		}
	})
}

var _ = certsservice.CertsCustomSpendCertificate
