//go:build sidecar_pending

package model

// 17.6's schedule rows for Team custom-domain certificates, on the real store with a scripted CA and a clock the test
// moves (TLS-M6-05): ordering desktops run sidecar.TlsCustomDomainOrderer's passes against the store through an
// in-process api, so every claim, progress event and register is the route's own transaction. The scripted CA
// counts newOrders and issues on a finalize of a ready order only once. No test reaches a real CA.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// One order at the scripted CA.
type testScriptedOrder struct {
	order   *sidecar.TlsAcmeOrder
	name    string
	account string
	chain   [][]byte
}

// A CA in memory (17.7's TlsAcmeClient): accounts by key, one pending authorization an order, which a challenge
// makes valid at once, a finalize of a ready order issuing once, and newOrder refusals a test scripts.
type testScriptedCa struct {
	world *testTlsCustomWorld

	stateLock     sync.Mutex
	accounts      map[string][]byte
	orders        map[string]*testScriptedOrder
	authorization map[string]*sidecar.TlsAcmeAuthorization
	newOrderCount int
	finalizeCount int
	issuedCount   int
	revoked       map[string]bool
	nextErrors    []error
	// hooks a test sets: called after a finalize issued, with no lock held
	afterFinalize func()
}

func newTestScriptedCa(world *testTlsCustomWorld) *testScriptedCa {
	return &testScriptedCa{
		world:         world,
		accounts:      map[string][]byte{},
		orders:        map[string]*testScriptedOrder{},
		authorization: map[string]*sidecar.TlsAcmeAuthorization{},
		revoked:       map[string]bool{},
	}
}

func testScriptedThumbprint(key crypto.Signer) []byte {
	return sidecar.TlsCustomKeyThumbprint(key.Public().(*ecdsa.PublicKey))
}

func (self *testScriptedCa) unauthorized(key crypto.Signer, accountUri string) error {
	if string(self.accounts[accountUri]) != string(testScriptedThumbprint(key)) {
		return &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemUnauthorized, Status: 403}
	}
	return nil
}

func (self *testScriptedCa) NewAccount(ctx context.Context, key crypto.Signer, onlyReturnExisting bool) (string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for uri, thumbprint := range self.accounts {
		if string(thumbprint) == string(testScriptedThumbprint(key)) {
			return uri, nil
		}
	}
	if onlyReturnExisting {
		return "", &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemAccountDoesNotExist, Status: 400}
	}
	uri := fmt.Sprintf("https://ca.example/acme/acct/%d", len(self.accounts)+1)
	self.accounts[uri] = testScriptedThumbprint(key)
	return uri, nil
}

func (self *testScriptedCa) Account(ctx context.Context, key crypto.Signer, accountUri string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.unauthorized(key, accountUri)
}

func (self *testScriptedCa) KeyChange(ctx context.Context, accountUri string, oldKey crypto.Signer, newKey crypto.Signer) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.unauthorized(oldKey, accountUri); err != nil {
		return err
	}
	self.accounts[accountUri] = testScriptedThumbprint(newKey)
	return nil
}

func (self *testScriptedCa) Deactivate(ctx context.Context, key crypto.Signer, accountUri string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.accounts, accountUri)
	return nil
}

func (self *testScriptedCa) NewOrder(ctx context.Context, key crypto.Signer, accountUri string, name string, profile string, replacesLeaf []byte) (*sidecar.TlsAcmeOrder, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.unauthorized(key, accountUri); err != nil {
		return nil, err
	}
	self.newOrderCount += 1
	if 0 < len(self.nextErrors) {
		err := self.nextErrors[0]
		self.nextErrors = self.nextErrors[1:]
		return nil, err
	}
	index := len(self.orders) + 1
	authorizationUrl := fmt.Sprintf("https://ca.example/acme/authz/%d", index)
	self.authorization[authorizationUrl] = &sidecar.TlsAcmeAuthorization{
		Url:    authorizationUrl,
		Status: sidecar.TlsAcmeStatusPending,
		Challenges: []*sidecar.TlsAcmeChallenge{
			{Type: sidecar.TlsAcmeChallengeTlsAlpn, Url: authorizationUrl + "/alpn", Token: fmt.Sprintf("token%d", index), Status: sidecar.TlsAcmeStatusPending},
			{Type: sidecar.TlsAcmeChallengeDns, Url: authorizationUrl + "/dns", Token: fmt.Sprintf("token%d", index), Status: sidecar.TlsAcmeStatusPending},
		},
	}
	order := &sidecar.TlsAcmeOrder{
		Url:            fmt.Sprintf("https://ca.example/acme/order/%d", index),
		Status:         sidecar.TlsAcmeStatusPending,
		Authorizations: []string{authorizationUrl},
		Finalize:       fmt.Sprintf("https://ca.example/acme/order/%d/finalize", index),
	}
	self.orders[order.Url] = &testScriptedOrder{order: order, name: name, account: accountUri}
	copied := *order
	return &copied, nil
}

// An order's status from its authorization.
func (self *testScriptedCa) statusWithLock(scripted *testScriptedOrder) {
	authorization := self.authorization[scripted.order.Authorizations[0]]
	if scripted.order.Status == sidecar.TlsAcmeStatusPending {
		switch authorization.Status {
		case sidecar.TlsAcmeStatusValid:
			scripted.order.Status = sidecar.TlsAcmeStatusReady
		case sidecar.TlsAcmeStatusInvalid, sidecar.TlsAcmeStatusDeactivated:
			scripted.order.Status = sidecar.TlsAcmeStatusInvalid
		}
	}
	if scripted.order.Status == sidecar.TlsAcmeStatusReady && authorization.Status == sidecar.TlsAcmeStatusDeactivated {
		scripted.order.Status = sidecar.TlsAcmeStatusInvalid
	}
}

func (self *testScriptedCa) Order(ctx context.Context, key crypto.Signer, accountUri string, orderUrl string) (*sidecar.TlsAcmeOrder, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	scripted, ok := self.orders[orderUrl]
	if !ok {
		return nil, sidecar.ErrTlsAcmeGone
	}
	self.statusWithLock(scripted)
	copied := *scripted.order
	return &copied, nil
}

func (self *testScriptedCa) Authorization(ctx context.Context, key crypto.Signer, accountUri string, url string) (*sidecar.TlsAcmeAuthorization, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	authorization, ok := self.authorization[url]
	if !ok {
		return nil, sidecar.ErrTlsAcmeGone
	}
	copied := *authorization
	return &copied, nil
}

func (self *testScriptedCa) Challenge(ctx context.Context, key crypto.Signer, accountUri string, url string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for authorizationUrl, authorization := range self.authorization {
		for _, challenge := range authorization.Challenges {
			if challenge.Url == url && authorization.Status == sidecar.TlsAcmeStatusPending {
				self.authorization[authorizationUrl].Status = sidecar.TlsAcmeStatusValid
			}
		}
	}
	return nil
}

func (self *testScriptedCa) Finalize(ctx context.Context, key crypto.Signer, accountUri string, order *sidecar.TlsAcmeOrder, csrDer []byte) (*sidecar.TlsAcmeOrder, error) {
	issued := false
	result, err := func() (*sidecar.TlsAcmeOrder, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.finalizeCount += 1
		scripted, ok := self.orders[order.Url]
		if !ok {
			return nil, sidecar.ErrTlsAcmeGone
		}
		self.statusWithLock(scripted)
		if scripted.order.Status != sidecar.TlsAcmeStatusReady {
			return nil, &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemOrderNotReady, Status: 403}
		}
		csr, err := x509.ParseCertificateRequest(csrDer)
		if err != nil {
			return nil, &sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemBadCsr, Status: 400}
		}
		serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
		template := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{},
			NotBefore:    self.world.now,
			NotAfter:     self.world.now.Add(45 * 24 * time.Hour),
			DNSNames:     csr.DNSNames,
		}
		leaf, err := x509.CreateCertificate(rand.Reader, template, self.world.caCert, csr.PublicKey, self.world.caKey)
		if err != nil {
			return nil, err
		}
		scripted.chain = [][]byte{leaf, self.world.caCert.Raw}
		scripted.order.Status = sidecar.TlsAcmeStatusValid
		scripted.order.Certificate = scripted.order.Url + "/cert"
		self.issuedCount += 1
		issued = true
		copied := *scripted.order
		return &copied, nil
	}()
	if issued && self.afterFinalize != nil {
		self.afterFinalize()
	}
	return result, err
}

func (self *testScriptedCa) Certificate(ctx context.Context, key crypto.Signer, accountUri string, url string) ([][]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, scripted := range self.orders {
		if scripted.order.Certificate == url {
			return scripted.chain, nil
		}
	}
	return nil, sidecar.ErrTlsAcmeGone
}

func (self *testScriptedCa) RenewalInfo(ctx context.Context, leaf []byte) (time.Time, time.Time, error) {
	return self.world.now.Add(29 * 24 * time.Hour), self.world.now.Add(31 * 24 * time.Hour), nil
}

func (self *testScriptedCa) Revoke(ctx context.Context, key crypto.Signer, accountUri string, leaf []byte, reason int) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.unauthorized(key, accountUri); err != nil {
		return err
	}
	self.revoked[string(leaf)] = true
	return nil
}

func (self *testScriptedCa) DeactivateAuthorization(ctx context.Context, key crypto.Signer, accountUri string, url string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if authorization, ok := self.authorization[url]; ok {
		authorization.Status = sidecar.TlsAcmeStatusDeactivated
	}
	return nil
}

func (self *testScriptedCa) KeyAuthorization(key crypto.Signer, token string) (string, error) {
	return token + "." + base64.RawURLEncoding.EncodeToString(testScriptedThumbprint(key)), nil
}

func (self *testScriptedCa) counts() (int, int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.newOrderCount, self.finalizeCount, self.issuedCount
}

// The routes as one desktop calls them, straight into the store.
type testTlsCustomApi struct {
	world  *testTlsCustomWorld
	caller *TlsCustomCaller
}

func (self *testTlsCustomApi) Work(ctx context.Context) (*sidecar.TlsCertsResult, error) {
	return self.world.store.Work(ctx, self.caller)
}

func (self *testTlsCustomApi) Claim(ctx context.Context, args *sidecar.TlsCustomClaimArgs) (*sidecar.TlsCustomClaimResult, error) {
	return self.world.store.Claim(ctx, self.caller, args)
}

func (self *testTlsCustomApi) Progress(ctx context.Context, args *sidecar.TlsCustomProgressArgs) (*sidecar.TlsCustomClaimResult, error) {
	result, err := self.world.store.Progress(ctx, self.caller, args)
	if err == nil && result == nil {
		result = &sidecar.TlsCustomClaimResult{}
	}
	return result, err
}

func (self *testTlsCustomApi) Register(ctx context.Context, args *sidecar.TlsCertRegisterArgs) error {
	return self.world.store.Register(ctx, self.caller, args)
}

func (self *testTlsCustomApi) Account(ctx context.Context, args *sidecar.TlsCustomAccountArgs) (*sidecar.TlsCustomAccount, error) {
	return self.world.store.AccountOp(ctx, self.caller, args)
}

func (self *testTlsCustomApi) Challenge(ctx context.Context, args *sidecar.TlsCustomChallengeArgs) (*sidecar.TlsCustomChallengeResult, error) {
	return nil, errors.New("no delegated challenge in this world")
}

func (self *testTlsCustomApi) ChallengeStatus(ctx context.Context, challengeId uint64) (*sidecar.TlsCustomChallengeResult, error) {
	return nil, errors.New("no delegated challenge in this world")
}

// The desktop's own view: every device of the world admitted and granted, port 443 held.
type testTlsCustomDevices struct {
	world   *testTlsCustomWorld
	desktop *testTlsCustomDevice
	devices map[server.Id]*testTlsCustomDevice
}

func (self *testTlsCustomDevices) Device(sidecarId sidecar.Id, ownerLabel string) ([]byte, bool) {
	device, ok := self.devices[server.Id(sidecarId)]
	if !ok {
		return nil, false
	}
	return []byte(device.identityKey.Public().(ed25519.PublicKey)), ownerLabel == "shop"
}

func (self *testTlsCustomDevices) Holds443(ownerLabel string) bool {
	return ownerLabel == "shop"
}

func (self *testTlsCustomDevices) SidecarId() sidecar.Id {
	return sidecar.Id(self.desktop.sidecarId)
}

func (self *testTlsCustomDevices) NetworkId() sidecar.Id {
	return sidecar.Id(self.world.networkId)
}

// The key files in memory, every desktop reading every repo.
type testTlsCustomKeys struct {
	stateLock sync.Mutex
	repos     map[string][]byte
	local     map[string][]byte
}

func (self *testTlsCustomKeys) ReadKeyFile(ctx context.Context, repoId sidecar.Id, domain string) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	data, ok := self.repos[repoId.String()+"/"+domain]
	if !ok {
		return nil, sidecar.ErrTlsCustomKeyUnavailable
	}
	return data, nil
}

func (self *testTlsCustomKeys) WriteKeyFile(ctx context.Context, repoId sidecar.Id, domain string, data []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.repos[repoId.String()+"/"+domain] = data
	return nil
}

func (self *testTlsCustomKeys) LocalKeyFile(domain string) []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.local[domain]
}

func (self *testTlsCustomKeys) SetLocalKeyFile(domain string, data []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.local[domain] = data
	return nil
}

// A TLS-ALPN-01 path that serves and probes at once.
type testTlsCustomAlpn struct{}

func (testTlsCustomAlpn) Serve(ctx context.Context, host string, ownerLabel string, keyAuthorization string, expireTime time.Time, claimSeq uint64) error {
	return nil
}

func (testTlsCustomAlpn) Clear(host string) {}

func (testTlsCustomAlpn) Probe(ctx context.Context, host string, keyAuthorization string, timeout time.Duration) error {
	return nil
}

// A world with a scripted CA, the account made by an admin's desktop through the orderer, and devices.
type testTlsCustomSchedule struct {
	*testTlsCustomWorld
	ca      *testScriptedCa
	keys    *testTlsCustomKeys
	devices map[server.Id]*testTlsCustomDevice
	account *sidecar.TlsCustomAccount
}

func newTestTlsCustomSchedule(t testing.TB, ctx context.Context) *testTlsCustomSchedule {
	t.Helper()
	world := newTestTlsCustomWorld(t, ctx)
	schedule := &testTlsCustomSchedule{
		testTlsCustomWorld: world,
		ca:                 newTestScriptedCa(world),
		keys:               &testTlsCustomKeys{repos: map[string][]byte{}, local: map[string][]byte{}},
		devices:            map[server.Id]*testTlsCustomDevice{},
	}
	admin := newTestTlsCustomDevice(t)
	orderer := schedule.orderer(t, admin, true)
	account, err := orderer.CreateAccount(ctx, world.domain, sidecar.NewId(), 1)
	if err != nil {
		t.Fatal(err)
	}
	schedule.account = account
	return schedule
}

// An ordering desktop's orderer.
func (self *testTlsCustomSchedule) orderer(t testing.TB, desktop *testTlsCustomDevice, admin bool) *sidecar.TlsCustomDomainOrderer {
	t.Helper()
	self.devices[desktop.sidecarId] = desktop
	orderer, err := sidecar.NewTlsCustomDomainOrderer(&sidecar.TlsCustomDomainOrdererDeps{
		Api:     &testTlsCustomApi{world: self.testTlsCustomWorld, caller: self.caller(desktop, admin)},
		Acme:    self.ca,
		Devices: &testTlsCustomDevices{world: self.testTlsCustomWorld, desktop: desktop, devices: self.devices},
		Keys:    self.keys,
		Alpn:    testTlsCustomAlpn{},
		Roots:   self.store.roots,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return orderer
}

// A phone of the network with its CSR queued.
func (self *testTlsCustomSchedule) phone(t testing.TB) sidecar.Id {
	t.Helper()
	phone := newTestTlsCustomDevice(t)
	self.devices[phone.sidecarId] = phone
	return self.queue(t, phone)
}

// The work list's host is ready for TLS-ALPN-01: every interface current for it.
func (self *testTlsCustomSchedule) ready(t testing.TB) {
	t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_dns_custom_host SET ready_time = $2 WHERE host = $1`, self.host, self.now))
	})
}

// 17.6's "custom order, two desktops": one certificate at the CA, one custom_issued row, the loser refused locked.
func TestTlsCustomScheduleTwoDesktopsOneCertificate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		schedule := newTestTlsCustomSchedule(t, ctx)
		schedule.ready(t)
		certId := schedule.phone(t)
		desktop1 := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		desktop2 := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		var waitGroup sync.WaitGroup
		reports := make([]*sidecar.TlsCustomPassReport, 2)
		for index, orderer := range []*sidecar.TlsCustomDomainOrderer{desktop1, desktop2} {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				reports[index], _ = orderer.RunOnce(ctx)
			}()
		}
		waitGroup.Wait()
		newOrders, _, issued := schedule.ca.counts()
		if newOrders != 1 || issued != 1 {
			row := schedule.row(t, certId)
			work, workErr := schedule.store.Work(ctx, schedule.caller(newTestTlsCustomDevice(t), false))
			t.Fatalf("%d newOrders, %d certificates; reports %+v %+v; row %s %v; work %+v %v", newOrders, issued, reports[0], reports[1], row.state, row.customReason, work, workErr)
		}
		if row := schedule.row(t, certId); row.state != sidecar.TlsCertStateCustomIssued {
			t.Fatalf("the row: %s", row.state)
		}
	})
}

// 17.6's "takeover after finalize": the holder dies after finalize; the next desktop registers the same certificate
// and sends no newOrder.
func TestTlsCustomScheduleTakeoverAfterFinalize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		schedule := newTestTlsCustomSchedule(t, ctx)
		schedule.ready(t)
		certId := schedule.phone(t)
		holderCtx, killHolder := context.WithCancel(ctx)
		schedule.ca.afterFinalize = func() {
			// the holder dies the moment the CA issued
			killHolder()
		}
		holder := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		holder.RunOnce(holderCtx)
		schedule.ca.afterFinalize = nil
		if row := schedule.row(t, certId); row.state != sidecar.TlsCertStateCustomOrdering || row.orderUrl == nil {
			t.Fatalf("after the death: %+v", row)
		}
		// the lease lapses and the other desktop resumes the recorded order
		schedule.now = schedule.now.Add(3 * time.Minute)
		other := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		if _, err := other.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		newOrders, _, issued := schedule.ca.counts()
		if newOrders != 1 || issued != 1 {
			t.Fatalf("%d newOrders, %d certificates", newOrders, issued)
		}
		if row := schedule.row(t, certId); row.state != sidecar.TlsCertStateCustomIssued {
			t.Fatalf("the row: %s", row.state)
		}
	})
}

// 17.6's "a 429": the CA refuses the newOrder; the account holds and no desktop sends anything until it ends.
func TestTlsCustomScheduleRateLimitHoldsEveryDesktop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		schedule := newTestTlsCustomSchedule(t, ctx)
		schedule.ready(t)
		certId := schedule.phone(t)
		schedule.ca.nextErrors = []error{&sidecar.TlsAcmeProblem{Type: sidecar.TlsAcmeProblemRateLimited, Status: 429, RetryAfter: 2 * time.Hour}}
		desktop1 := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		desktop2 := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		desktop1.RunOnce(ctx)
		desktop2.RunOnce(ctx)
		if newOrders, _, _ := schedule.ca.counts(); newOrders != 1 {
			t.Fatalf("%d newOrders during the hold", newOrders)
		}
		if row := schedule.row(t, certId); row.state != sidecar.TlsCertStateCustomQueued {
			t.Fatalf("the held row: %s", row.state)
		}
		schedule.now = schedule.now.Add(2*time.Hour + time.Minute)
		desktop2.RunOnce(ctx)
		if newOrders, _, issued := schedule.ca.counts(); newOrders != 2 || issued != 1 {
			t.Fatalf("after the hold: %d newOrders, %d certificates", newOrders, issued)
		}
	})
}

// 17.6's "revocation": the device is removed, its row goes custom_revoking, and a desktop revokes at the CA.
func TestTlsCustomScheduleRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		schedule := newTestTlsCustomSchedule(t, ctx)
		schedule.ready(t)
		certId := schedule.phone(t)
		desktop := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		if _, err := desktop.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		row := schedule.row(t, certId)
		server.Tx(ctx, func(tx server.PgTx) {
			TlsCustomRemoveRowsInTx(ctx, tx, &TlsCustomRemoval{NetworkId: schedule.networkId, SidecarId: &row.sidecarId})
		})
		report, err := desktop.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Revoked) != 1 || schedule.row(t, certId).state != sidecar.TlsCertStateRevoked || !schedule.ca.revoked[string(row.chain[0])] {
			t.Fatalf("revoked %v, row %s", report.Revoked, schedule.row(t, certId).state)
		}
	})
}

// 17.6's "ended under an order": a device removed with its order ready and its holder stalled; the holder wakes and
// finalizes; the revoke pass revokes that certificate, and only then is the next row claimed.
func TestTlsCustomScheduleEndedUnderAnOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		schedule := newTestTlsCustomSchedule(t, ctx)
		schedule.ready(t)
		certId := schedule.phone(t)
		removed := false
		schedule.ca.afterFinalize = func() {
			if removed {
				return
			}
			removed = true
			row := schedule.row(t, certId)
			server.Tx(ctx, func(tx server.PgTx) {
				TlsCustomRemoveRowsInTx(ctx, tx, &TlsCustomRemoval{NetworkId: schedule.networkId, SidecarId: &row.sidecarId})
			})
		}
		desktop := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		desktop.RunOnce(ctx)
		schedule.ca.afterFinalize = nil
		if row := schedule.row(t, certId); row.state != sidecar.TlsCertStateCustomRevoking && row.state != sidecar.TlsCertStateRevoked {
			t.Fatalf("the ended row: %s", row.state)
		}
		next := schedule.phone(t)
		for range 3 {
			desktop.RunOnce(ctx)
		}
		if schedule.row(t, certId).state != sidecar.TlsCertStateRevoked {
			t.Fatalf("not settled: %s", schedule.row(t, certId).state)
		}
		_, _, issued := schedule.ca.counts()
		if schedule.row(t, next).state != sidecar.TlsCertStateCustomIssued || issued != 2 {
			t.Fatalf("the next row: %s, %d certificates", schedule.row(t, next).state, issued)
		}
		revokedCount := 0
		for range schedule.ca.revoked {
			revokedCount += 1
		}
		if revokedCount != 1 {
			t.Fatalf("%d revoked", revokedCount)
		}
	})
}

// 17.6's "key change": one desktop rolls; the other's next order signs with the new key, the old refused by the CA.
func TestTlsCustomScheduleKeyChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		schedule := newTestTlsCustomSchedule(t, ctx)
		schedule.ready(t)
		admin := schedule.orderer(t, newTestTlsCustomDevice(t), true)
		work, err := schedule.store.Work(ctx, schedule.caller(newTestTlsCustomDevice(t), false))
		if err != nil || len(work.Accounts) != 1 {
			t.Fatalf("%+v, %v", work, err)
		}
		account := work.Accounts[0]
		oldFile := schedule.keys.LocalKeyFile(schedule.domain)
		if err := admin.RollAccount(ctx, account, testTlsCustomRepos{repoId: account.KeyRepoId}); err != nil {
			t.Fatal(err)
		}
		after, err := schedule.store.Work(ctx, schedule.caller(newTestTlsCustomDevice(t), false))
		if err != nil || after.Accounts[0].KeySeq != 2 || after.Accounts[0].NextKeyThumbprint != nil {
			t.Fatalf("after the change: %+v, %v", after.Accounts, err)
		}
		// the old key no longer speaks for the account
		parsed, err := sidecar.ParseTlsCustomKeyFile(oldFile, schedule.domain)
		if err != nil {
			t.Fatal(err)
		}
		oldKey, err := x509.ParsePKCS8PrivateKey(parsed.Key)
		if err != nil {
			t.Fatal(err)
		}
		if schedule.ca.Account(ctx, oldKey.(crypto.Signer), account.AccountUri) == nil {
			t.Fatal("the CA still takes the old key")
		}
		certId := schedule.phone(t)
		desktop := schedule.orderer(t, newTestTlsCustomDevice(t), false)
		if _, err := desktop.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if schedule.row(t, certId).state != sidecar.TlsCertStateCustomIssued {
			t.Fatalf("an order under the new key: %s", schedule.row(t, certId).state)
		}
	})
}

// A key change's repos: the account's own, this desktop's user owning it.
type testTlsCustomRepos struct {
	repoId sidecar.Id
}

func (self testTlsCustomRepos) RolloverDue(repoId sidecar.Id, shrunkAt uint64) (bool, bool) {
	return true, true
}

func (self testTlsCustomRepos) RollTarget(ctx context.Context, repoId sidecar.Id) (sidecar.Id, uint64, error) {
	return self.repoId, 2, nil
}

var _ = sha256.Size
