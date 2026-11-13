// certs' purge chain on a private database (TLS-M5-17): scheduled only in a
// binary that links MG5, and each run deleting the rows whose certificate
// ended more than 30 days ago, and only those.
package work

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/task"
)

func TestTlsCertPurgeScheduledOnlyWithMg5(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleTlsCertPurge(clientSession, tx)
			})
		}
		want := 0
		if server.SidecarMg5Linked() {
			want = 1
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("tls_cert_purge").String(),
			).Scan(&count))
		})
		if count != want {
			t.Fatalf("%d runs queued, want %d", count, want)
		}
		if !server.SidecarMg5Linked() {
			result, err := TlsCertPurge(&TlsCertPurgeArgs{}, clientSession)
			if err != nil || result.RemovedCount != 0 || result.More {
				t.Fatalf("a run without MG5: %+v, %v", result, err)
			}
		}
	})
}

func TestTlsCertPurgeRemovesRowsPastTheirRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		store := model.NewCertsStore(nil)
		// a time of this test's own, so the store's other rows are not in the run
		now := time.Date(2051, time.March, 1, 0, 0, 0, 0, time.UTC)
		retention := DefaultTlsCertPurgeSettings().RowRetention
		if retention != 30*24*time.Hour {
			t.Fatalf("the retention is %v", retention)
		}
		insert := func(state sidecar.TlsCertState, notAfter time.Time) sidecar.Id {
			t.Helper()
			row := &sidecar.TlsCertRecord{
				CertId:              sidecartest.CertsIdAt(now.Add(-100 * 24 * time.Hour)),
				NetworkId:           sidecar.NewId(),
				ClientId:            sidecar.NewId(),
				SidecarId:           sidecar.NewId(),
				IdentifierSetSha256: make([]byte, 32),
				SpkiSha256:          make([]byte, 32),
				Attestation:         []byte("attestation"),
				State:               state,
			}
			if !notAfter.IsZero() {
				row.Serial = "0a"
				row.NotBefore = notAfter.Add(-45 * 24 * time.Hour)
				row.NotAfter = notAfter
			}
			if err := store.InsertCert(ctx, row); err != nil {
				t.Fatal(err)
			}
			return row.CertId
		}
		ended := insert(sidecar.TlsCertStateExpired, now.Add(-retention-time.Second))
		revoked := insert(sidecar.TlsCertStateRevoked, now.Add(-retention-time.Hour))
		atTheLine := insert(sidecar.TlsCertStateExpired, now.Add(-retention))
		recent := insert(sidecar.TlsCertStateExpired, now.Add(-time.Hour))
		live := insert(sidecar.TlsCertStateIssued, now.Add(time.Hour))
		failed := insert(sidecar.TlsCertStateFailed, time.Time{})

		result, err := tlsCertPurgeAt(clientSession, now)
		if err != nil || result.More || result.RemovedCount < 2 {
			t.Fatalf("the purge: %+v, %v", result, err)
		}
		for _, certId := range []sidecar.Id{ended, revoked} {
			if _, err := store.GetCert(ctx, certId); !errors.Is(err, certsservice.ErrCertsNotFound) {
				t.Fatalf("a row past its retention stayed: %v", err)
			}
		}
		for _, certId := range []sidecar.Id{atTheLine, recent, live, failed} {
			if _, err := store.GetCert(ctx, certId); err != nil {
				t.Fatalf("a row inside its retention went: %v", err)
			}
		}
	})
}

// The order, renewal and revocation chains on a private database and the test
// Redis (TLS-M5-17), the engine on a fake clock, a recording Push and a CA
// scripted in this file: each chain armed once and idle until its process
// installs the engine; an order to its certificate; a renewal past a spent
// budget; and the sweep reading other areas' rows itself, so a removed
// client's, a reaped ephemeral's, a revoked sidecar's, a deleted network's and
// a retired alias's certificates go with no call from the area that removed
// them, while a new client generation keeps its certificate.

// A CA that issues each CSR a leaf under one issuer and records revocations.
type testTlsCertAcme struct {
	clock     *sidecartest.FakeClock
	issuerKey *ecdsa.PrivateKey
	issuer    *x509.Certificate

	stateLock  sync.Mutex
	nextSerial int64
	orders     []*certsservice.CertsAcmeOrder
	revoked    []certsservice.CertsRevocationReason
}

func newTestTlsCertAcme(t testing.TB, clock *sidecartest.FakeClock) *testTlsCertAcme {
	t.Helper()
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Issuer"},
		NotBefore:             clock.Now().Add(-time.Hour),
		NotAfter:              clock.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		SubjectKeyId:          []byte{1, 2, 3, 4, 5, 6, 7, 8},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &issuerKey.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testTlsCertAcme{clock: clock, issuerKey: issuerKey, issuer: issuer, nextSerial: 0x4000}
}

// A leaf for publicKey and names with serial, valid 45 days from the clock's now.
func (self *testTlsCertAcme) leaf(publicKey any, names []string, serial *big.Int) ([]byte, error) {
	now := self.clock.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		DNSNames:     names,
		NotBefore:    now,
		NotAfter:     now.Add(45 * 24 * time.Hour),
	}
	return x509.CreateCertificate(rand.Reader, template, self.issuer, publicKey, self.issuerKey)
}

func (self *testTlsCertAcme) Order(ctx context.Context, order *certsservice.CertsAcmeOrder) (*certsservice.CertsAcmeIssued, error) {
	self.stateLock.Lock()
	self.orders = append(self.orders, order)
	self.nextSerial += 1
	serial := big.NewInt(self.nextSerial)
	self.stateLock.Unlock()
	csr, err := x509.ParseCertificateRequest(order.Csr)
	if err != nil {
		return nil, err
	}
	leaf, err := self.leaf(csr.PublicKey, csr.DNSNames, serial)
	if err != nil {
		return nil, err
	}
	now := self.clock.Now()
	return &certsservice.CertsAcmeIssued{
		Chain:      [][]byte{leaf, self.issuer.Raw},
		AccountUri: "https://ca.example.test/acct/1",
		Serial:     certsservice.CertsSerialText(serial),
		NotBefore:  now,
		NotAfter:   now.Add(45 * 24 * time.Hour),
	}, nil
}

func (self *testTlsCertAcme) RenewalInfo(ctx context.Context, ca certsservice.CertsCa, leaf []byte) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, errors.New("the CA has no ARI")
}

func (self *testTlsCertAcme) Revoke(ctx context.Context, ca certsservice.CertsCa, leaf []byte, reason certsservice.CertsRevocationReason) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.revoked = append(self.revoked, reason)
	return nil
}

func (self *testTlsCertAcme) revocations() []certsservice.CertsRevocationReason {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.revoked)
}

func (self *testTlsCertAcme) ordered() []*certsservice.CertsAcmeOrder {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.orders)
}

// The chains' engine on the real store and Redis, installed for the test.
type testTlsCertChains struct {
	t             testing.TB
	ctx           context.Context
	clientSession *session.ClientSession
	clock         *sidecartest.FakeClock
	push          *sidecartest.FakePush
	acme          *testTlsCertAcme
	store         *model.CertsStore
	ca            certsservice.CertsCa
	root          string
	service       *certsservice.CertsService
}

// budgetCap is the one CA's cap of new sets in its window of 90 days.
func newTestTlsCertChains(t testing.TB, budgetCap int) *testTlsCertChains {
	t.Helper()
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	t.Cleanup(clientSession.Cancel)
	// a CA and a root of this test's own, so no other run's budget key counts
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	ca := certsservice.CertsCaLetsEncrypt
	root := "c" + hex.EncodeToString(nonce) + ".example.test"
	settings := certsservice.DefaultCertsSettings()
	settings.RootDomain = root
	// a window longer than a certificate's life, so a unit spent stays spent through a renewal
	settings.Cas = map[certsservice.CertsCa]*certsservice.CertsCaSettings{
		ca: {Profile: "tlsserver", BudgetWindow: 90 * 24 * time.Hour, BudgetCap: budgetCap},
	}
	settings.BudgetTtl = 90 * 24 * time.Hour
	settings.CaOrder = []certsservice.CertsCa{ca}
	settings.EphemeralCaOrder = []certsservice.CertsCa{ca}

	clock := sidecartest.NewFakeClock(server.NowUtc().Truncate(time.Second))
	push := sidecartest.NewFakePush(sidecartest.NewFaultQueue())
	acme := newTestTlsCertAcme(t, clock)
	store := model.NewCertsStore(nil)
	host := &sidecar.ServiceHost{
		Redis: sidecarhost.NewRedis(nil),
		Push:  push,
		Clock: clock,
	}
	service, err := certsservice.NewCertsService(ctx, host, store, nil, acme, settings)
	if err != nil {
		t.Fatal(err)
	}
	SetTlsCertChains(&TlsCertChainDeps{Service: service})
	t.Cleanup(func() { SetTlsCertChains(nil) })
	return &testTlsCertChains{
		t: t, ctx: ctx, clientSession: clientSession, clock: clock, push: push, acme: acme, store: store, ca: ca, root: root, service: service,
	}
}

// One device of a network as other areas' rows hold it.
type testTlsCertDevice struct {
	networkId sidecar.Id
	clientId  sidecar.Id
	sidecarId sidecar.Id
	key       *ecdsa.PrivateKey
	names     []string
}

func (self *testTlsCertChains) exec(sql string, args ...any) {
	self.t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, sql, args...))
	})
}

// A network with its row.
func (self *testTlsCertChains) network() sidecar.Id {
	self.t.Helper()
	networkId := sidecar.NewId()
	self.exec(
		`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
		server.Id(networkId),
		"n"+hex.EncodeToString(networkId[10:]),
		server.NewId(),
	)
	return networkId
}

// A device of networkId with an active client, an active identity bound to
// that client, and names under label.
func (self *testTlsCertChains) device(networkId sidecar.Id, label string) *testTlsCertDevice {
	self.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		self.t.Fatal(err)
	}
	device := &testTlsCertDevice{
		networkId: networkId,
		clientId:  sidecar.NewId(),
		sidecarId: sidecar.NewId(),
		key:       key,
		names:     []string{"*." + label + "." + self.root, label + "." + self.root},
	}
	self.exec(
		`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
		server.Id(device.clientId),
		server.Id(networkId),
	)
	self.exec(
		`
			INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time)
			VALUES ($1, $2, $3, 'ed25519', $4, $5)
		`,
		server.Id(device.sidecarId),
		server.Id(networkId),
		make([]byte, 32),
		model.SidecarIdentityStatusActive,
		server.NowUtc(),
	)
	self.bind(device, 1, device.clientId)
	return device
}

// Binds the device's sidecar to clientId at generation.
func (self *testTlsCertChains) bind(device *testTlsCertDevice, generation int, clientId sidecar.Id) {
	self.t.Helper()
	self.exec(
		`
			INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
			VALUES ($1, $2, $3, $4, $5, $6)
		`,
		server.Id(device.sidecarId),
		generation,
		server.Id(clientId),
		make([]byte, 32),
		[]byte("signatures"),
		server.NowUtc(),
	)
}

func (self *testTlsCertChains) row(device *testTlsCertDevice, state sidecar.TlsCertState) *sidecar.TlsCertRecord {
	self.t.Helper()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: device.names}, device.key)
	if err != nil {
		self.t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&device.key.PublicKey)
	if err != nil {
		self.t.Fatal(err)
	}
	spkiSha256 := sha256.Sum256(spki)
	setSha256, err := sidecar.TlsIdentifierSetHash(device.names)
	if err != nil {
		self.t.Fatal(err)
	}
	return &sidecar.TlsCertRecord{
		CertId:              sidecartest.CertsIdAt(self.clock.Now()),
		NetworkId:           device.networkId,
		ClientId:            device.clientId,
		SidecarId:           device.sidecarId,
		IdentifierSetSha256: setSha256[:],
		SpkiSha256:          spkiSha256[:],
		Attestation:         []byte("attestation"),
		Csr:                 csr,
		State:               state,
	}
}

// The device's order, queued as POST /tls/cert-order writes it.
func (self *testTlsCertChains) queued(device *testTlsCertDevice) sidecar.Id {
	self.t.Helper()
	row := self.row(device, sidecar.TlsCertStateQueued)
	if err := self.store.InsertCert(self.ctx, row); err != nil {
		self.t.Fatal(err)
	}
	// the next row's id is after this one's
	self.clock.Advance(time.Millisecond)
	return row.CertId
}

// The device's certificate, issued with its chain kept in its row.
func (self *testTlsCertChains) issued(device *testTlsCertDevice) sidecar.Id {
	self.t.Helper()
	self.acme.stateLock.Lock()
	self.acme.nextSerial += 1
	serial := big.NewInt(self.acme.nextSerial)
	self.acme.stateLock.Unlock()
	leaf, err := self.acme.leaf(&device.key.PublicKey, device.names, serial)
	if err != nil {
		self.t.Fatal(err)
	}
	row := self.row(device, sidecar.TlsCertStateIssued)
	row.Ca = string(self.ca)
	row.AccountUri = "https://ca.example.test/acct/1"
	row.Profile = "tlsserver"
	row.Serial = certsservice.CertsSerialText(serial)
	row.NotBefore = self.clock.Now()
	row.NotAfter = self.clock.Now().Add(45 * 24 * time.Hour)
	row.Chain = [][]byte{leaf, self.acme.issuer.Raw}
	if err := self.store.InsertCert(self.ctx, row); err != nil {
		self.t.Fatal(err)
	}
	self.clock.Advance(time.Millisecond)
	return row.CertId
}

func (self *testTlsCertChains) state(certId sidecar.Id) sidecar.TlsCertState {
	self.t.Helper()
	row, err := self.store.GetCert(self.ctx, certId)
	if err != nil {
		self.t.Fatal(err)
	}
	return row.State
}

// The kinds of the TlsHints the device's client was told about certId.
func (self *testTlsCertChains) hintKinds(device *testTlsCertDevice, certId sidecar.Id) []protocol.TlsHintKind {
	self.t.Helper()
	kinds := []protocol.TlsHintKind{}
	for _, clientHint := range self.push.Hints(connect.Id(device.clientId)) {
		if clientHint.Topic != sidecar.PushTopicTls {
			self.t.Fatalf("a hint on topic %q", clientHint.Topic)
		}
		hint := &protocol.TlsHint{}
		if err := proto.Unmarshal(clientHint.Body, hint); err != nil {
			self.t.Fatal(err)
		}
		if sidecar.Id(hint.CertId) == certId {
			kinds = append(kinds, hint.Kind)
		}
	}
	return kinds
}

func TestTlsCertChainsArmedOnceAndIdleUntilInstalled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		SetTlsCertChains(nil)
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleTlsCertOrders(clientSession, tx)
				ScheduleTlsCertRenewals(clientSession, tx)
				ScheduleTlsCertRevocations(clientSession, tx)
			})
		}
		want := 0
		if server.SidecarMg5Linked() {
			want = 1
		}
		for _, runOnceKey := range []string{"tls_cert_orders", "tls_cert_renewals", "tls_cert_revocations"} {
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
					task.RunOnce(runOnceKey).String(),
				).Scan(&count))
			})
			if count != want {
				t.Fatalf("%s is armed %d times, want %d", runOnceKey, count, want)
			}
		}
		orders, err := TlsCertOrders(&TlsCertOrdersArgs{}, clientSession)
		if err != nil || !orders.Idle || orders.More || orders.Failed {
			t.Fatalf("orders with nothing installed: %+v, %v", orders, err)
		}
		renewals, err := TlsCertRenewals(&TlsCertRenewalsArgs{}, clientSession)
		if err != nil || !renewals.Idle || renewals.Failed {
			t.Fatalf("renewals with nothing installed: %+v, %v", renewals, err)
		}
		revocations, err := TlsCertRevocations(&TlsCertRevocationsArgs{}, clientSession)
		if err != nil || !revocations.Idle || revocations.Failed {
			t.Fatalf("revocations with nothing installed: %+v, %v", revocations, err)
		}
	})
}

// An order goes from its queued row to a certificate through the order chain,
// a due certificate renews from its stored CSR past the spent budget, and the
// old row expires at its end; the device's client is hinted at each move.
func TestTlsCertOrderAndRenewalChains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		chains := newTestTlsCertChains(t, 1)
		device := chains.device(chains.network(), "k1chain001")
		certId := chains.queued(device)
		// a second network's first certificate finds the window's one unit spent
		waiting := chains.device(chains.network(), "k1chain002")
		waitingId := chains.queued(waiting)

		orders, err := TlsCertOrders(&TlsCertOrdersArgs{}, chains.clientSession)
		if err != nil || orders.Idle || orders.Failed || orders.AdmittedCount != 1 || orders.IssuedCount != 1 || orders.WaitingCount != 1 {
			t.Fatalf("orders %+v, %v", orders, err)
		}
		if chains.state(certId) != sidecar.TlsCertStateIssued || chains.state(waitingId) != sidecar.TlsCertStateQueued {
			t.Fatalf("states %s, %s", chains.state(certId), chains.state(waitingId))
		}
		if kinds := chains.hintKinds(device, certId); !slices.Equal(kinds, []protocol.TlsHintKind{protocol.TlsHintKind_TLS_HINT_ORDER, protocol.TlsHintKind_TLS_HINT_ISSUED}) {
			t.Fatalf("hints %v", kinds)
		}

		renewals, err := TlsCertRenewals(&TlsCertRenewalsArgs{}, chains.clientSession)
		if err != nil || renewals.Idle || renewals.QueuedCount != 0 {
			t.Fatalf("renewals before their time %+v, %v", renewals, err)
		}
		chains.clock.Advance(30 * 24 * time.Hour)
		renewals, err = TlsCertRenewals(&TlsCertRenewalsArgs{}, chains.clientSession)
		if err != nil || renewals.QueuedCount != 1 || renewals.NextCertId != "" {
			t.Fatalf("renewals %+v, %v", renewals, err)
		}
		issued, err := chains.store.GetCert(chains.ctx, certId)
		if err != nil || issued.SuccessorCertId == (sidecar.Id{}) {
			t.Fatalf("the renewed row %+v, %v", issued, err)
		}
		renewalId := issued.SuccessorCertId
		// the budget's window has not moved on: the renewal goes, the new set still waits
		orders, err = TlsCertOrders(&TlsCertOrdersArgs{}, chains.clientSession)
		if err != nil || orders.IssuedCount != 1 || orders.WaitingCount != 1 {
			t.Fatalf("the renewal's orders %+v, %v", orders, err)
		}
		if chains.state(renewalId) != sidecar.TlsCertStateIssued || chains.state(waitingId) != sidecar.TlsCertStateQueued {
			t.Fatalf("states %s, %s", chains.state(renewalId), chains.state(waitingId))
		}
		sent := chains.acme.ordered()
		if len(sent) != 2 || string(sent[1].Csr) != string(issued.Csr) {
			t.Fatalf("the CA was sent %d orders, the renewal's not the stored CSR", len(sent))
		}
		if kinds := chains.hintKinds(device, renewalId); !slices.Equal(kinds, []protocol.TlsHintKind{
			protocol.TlsHintKind_TLS_HINT_ORDER, protocol.TlsHintKind_TLS_HINT_ORDER, protocol.TlsHintKind_TLS_HINT_ISSUED,
		}) {
			t.Fatalf("the renewal's hints %v", kinds)
		}

		chains.clock.Advance(15 * 24 * time.Hour)
		renewals, err = TlsCertRenewals(&TlsCertRenewalsArgs{}, chains.clientSession)
		if err != nil || renewals.ExpiredCount != 1 || chains.state(certId) != sidecar.TlsCertStateExpired {
			t.Fatalf("renewals at the old certificate's end %+v, %v", renewals, err)
		}
	})
}

// The sweep's source reads each cause from the rows of the area that owns it.
func TestTlsCertRevocationSourceReadsOtherAreasRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg1Linked() {
			return
		}
		chains := newTestTlsCertChains(t, 100)
		networkId := chains.network()
		live := chains.device(networkId, "k2sweep001")
		removed := chains.device(networkId, "k2sweep002")
		chains.exec(`UPDATE network_client SET active = false WHERE client_id = $1`, server.Id(removed.clientId))
		// a new generation: the client that ordered is gone and the sidecar's newest is active
		regenerated := chains.device(networkId, "k2sweep003")
		chains.exec(`UPDATE network_client SET active = false WHERE client_id = $1`, server.Id(regenerated.clientId))
		nextClientId := sidecar.NewId()
		chains.exec(`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`, server.Id(nextClientId), server.Id(networkId))
		chains.bind(regenerated, 2, nextClientId)
		revoked := chains.device(networkId, "k2sweep004")
		chains.exec(`UPDATE sidecar_identity SET status = $2 WHERE sidecar_id = $1`, server.Id(revoked.sidecarId), model.SidecarIdentityStatusRevoked)
		// a device whose set names the network's alias, since retired
		aliased := chains.device(networkId, "k2sweep005")
		aliased.names = append(aliased.names, "*.oldname."+chains.root, "oldname."+chains.root)
		chains.exec(
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, 'oldname', $2, $3, $4, $5)`,
			chains.root, server.Id(networkId), model.DnsLabelKindAlias, model.DnsLabelStateRetiring, server.NowUtc(),
		)
		// a live alias takes nothing with it
		liveAliased := chains.device(networkId, "k2sweep006")
		liveAliased.names = append(liveAliased.names, "*.newname."+chains.root, "newname."+chains.root)
		chains.exec(
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, 'newname', $2, $3, $4, $5)`,
			chains.root, server.Id(networkId), model.DnsLabelKindAlias, model.DnsLabelStateLive, server.NowUtc(),
		)
		// a network whose row is gone
		deleted := chains.device(sidecar.NewId(), "k2sweep007")

		devices := []*testTlsCertDevice{live, removed, regenerated, revoked, aliased, liveAliased, deleted}
		candidates := []*certsservice.CertsRevocationCandidate{}
		for _, device := range devices {
			candidates = append(candidates, &certsservice.CertsRevocationCandidate{Cert: chains.row(device, sidecar.TlsCertStateIssued), Names: device.names})
			chains.clock.Advance(time.Millisecond)
		}
		causes, err := NewTlsCertRevocationSource(nil).RevocationCauses(chains.ctx, candidates)
		if err != nil {
			t.Fatal(err)
		}
		want := map[int]certsservice.CertsRevocationCause{
			1: certsservice.CertsRevocationCauseRemoval,
			3: certsservice.CertsRevocationCauseRemoval,
			4: certsservice.CertsRevocationCauseAliasRetired,
			6: certsservice.CertsRevocationCauseDeletion,
		}
		for index, candidate := range candidates {
			cause, found := causes[candidate.Cert.CertId]
			if wantCause, wanted := want[index]; found != wanted || cause != wantCause {
				t.Errorf("device %d: cause %q (%v), want %q (%v)", index, cause, found, wantCause, wanted)
			}
		}
		if len(causes) != len(want) {
			t.Fatalf("causes %v", causes)
		}
	})
}

// The revocation chain takes a removed client's certificate, a reaped
// ephemeral's, a deleted network's and a retired alias's to their CA with no
// call from TEAM, the reaper, account deletion or DNS: each only changed its
// own rows. A row another path marked goes too, and a live device's stays.
func TestTlsCertRevocationChainSweeps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg1Linked() {
			return
		}
		chains := newTestTlsCertChains(t, 100)
		networkId := chains.network()
		live := chains.device(networkId, "k3sweep001")
		liveId := chains.issued(live)
		removed := chains.device(networkId, "k3sweep002")
		removedId := chains.issued(removed)
		ephemeral := chains.device(networkId, "k3sweep003")
		chains.exec(`INSERT INTO network_client_owner (client_id, ephemeral) VALUES ($1, true)`, server.Id(ephemeral.clientId))
		ephemeralId := chains.issued(ephemeral)
		aliased := chains.device(networkId, "k3sweep004")
		aliased.names = append(aliased.names, "*.oldalias."+chains.root, "oldalias."+chains.root)
		aliasedId := chains.issued(aliased)
		chains.exec(
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, 'oldalias', $2, $3, $4, $5)`,
			chains.root, server.Id(networkId), model.DnsLabelKindAlias, model.DnsLabelStateLive, server.NowUtc(),
		)
		otherNetworkId := chains.network()
		deleted := chains.device(otherNetworkId, "k3sweep005")
		deletedId := chains.issued(deleted)
		takenDown := chains.device(networkId, "k3sweep006")
		takenDownId := chains.issued(takenDown)

		// nothing has changed yet: a pass finds no cause
		result, err := TlsCertRevocations(&TlsCertRevocationsArgs{}, chains.clientSession)
		if err != nil || result.Idle || result.Failed || result.MarkedCount != 0 || result.RevokedCount != 0 {
			t.Fatalf("a pass with nothing to revoke: %+v, %v", result, err)
		}

		// each area changes only its own rows
		chains.exec(`UPDATE network_client SET active = false WHERE client_id = ANY($1)`, []server.Id{server.Id(removed.clientId), server.Id(ephemeral.clientId)})
		chains.exec(`UPDATE dns_label SET state = $3 WHERE root = $1 AND label = $2`, chains.root, "oldalias", model.DnsLabelStateRetiring)
		chains.exec(`DELETE FROM network WHERE network_id = $1`, server.Id(otherNetworkId))
		marked, err := model.RequestTlsCertRevocation(chains.ctx, &model.TlsCertRevocationRequest{NetworkId: server.Id(networkId), CertId: server.Id(takenDownId), Reason: "abuse:test"})
		if err != nil || marked != 1 {
			t.Fatalf("the takedown's mark: %d, %v", marked, err)
		}

		result, err = TlsCertRevocations(&TlsCertRevocationsArgs{}, chains.clientSession)
		if err != nil || result.Failed || result.MarkedCount != 4 || result.RevokedCount != 5 || result.WaitingCount != 0 || result.FailedCount != 0 ||
			result.AfterIssuedCertId != "" || result.AfterRevokingCertId != "" {
			t.Fatalf("the sweep: %+v, %v", result, err)
		}
		for _, certId := range []sidecar.Id{removedId, ephemeralId, aliasedId, deletedId, takenDownId} {
			if state := chains.state(certId); state != sidecar.TlsCertStateRevoked {
				t.Fatalf("state %s", state)
			}
		}
		if state := chains.state(liveId); state != sidecar.TlsCertStateIssued {
			t.Fatalf("the live device's certificate: %s", state)
		}
		reasons := chains.acme.revocations()
		if len(reasons) != 5 {
			t.Fatalf("reasons %v", reasons)
		}
		for _, reason := range reasons {
			if reason != certsservice.CertsRevocationCessationOfOperation {
				t.Fatalf("reasons %v", reasons)
			}
		}
		// a hint when the sweep marks, and one when the CA has taken it
		if kinds := chains.hintKinds(removed, removedId); !slices.Equal(kinds, []protocol.TlsHintKind{
			protocol.TlsHintKind_TLS_HINT_REVOKED, protocol.TlsHintKind_TLS_HINT_REVOKED,
		}) {
			t.Fatalf("hints %v", kinds)
		}
		if kinds := chains.hintKinds(takenDown, takenDownId); !slices.Equal(kinds, []protocol.TlsHintKind{protocol.TlsHintKind_TLS_HINT_REVOKED}) {
			t.Fatalf("the marked row's hints %v", kinds)
		}
		if len(chains.push.Hints(connect.Id(live.clientId))) != 0 {
			t.Fatal("the live device was hinted")
		}
		// no renewal follows a revoked certificate
		chains.clock.Advance(31 * 24 * time.Hour)
		renewals, err := TlsCertRenewals(&TlsCertRenewalsArgs{}, chains.clientSession)
		if err != nil || renewals.QueuedCount != 1 {
			t.Fatalf("renewals %+v, %v", renewals, err)
		}
	})
}

// A scan in batches: each run's result carries where the next reads from.
func TestTlsCertChainCursorsRoundTrip(t *testing.T) {
	certId := sidecar.NewId()
	if got := tlsCertCursor(tlsCertCursorText(certId)); got != certId {
		t.Fatalf("cursor %s read back %s", certId, got)
	}
	if tlsCertCursorText(sidecar.Id{}) != "" || tlsCertCursor("") != (sidecar.Id{}) || tlsCertCursor("not an id") != (sidecar.Id{}) {
		t.Fatal("the start is not the empty cursor")
	}
	labels, roots := tlsCertNameLabels("*.Scope.net.Root.example.")
	if !slices.Equal(labels, []string{"scope", "net", "root"}) || !slices.Equal(roots, []string{"net.root.example", "root.example", "example"}) {
		t.Fatalf("labels %v under %v", labels, roots)
	}
}
