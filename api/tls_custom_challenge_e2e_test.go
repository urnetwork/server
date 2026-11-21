//go:build sidecar_pending

package api

// QD-1165's delegated DNS-01 handoff end to end on the private stack, every service of the path in process and each
// reached the way production reaches it:
//
//   - api: this package's router, over HTTP, its certs and custom routes installed on a fake host's authenticator,
//     the custom store on the stack's Postgres;
//   - taskworker: the queue's real worker with the certs worker's command registered, driven one evaluation at a
//     time where a test needs an order, and left running on its own where it races the routes;
//   - certs: server.NewCertsDnsClient on the certs bearer, the command's DNS client;
//   - dns: dnshost's API role over HTTP on the same Postgres, its challenge writes through §12.6's one write path
//     and its ready route over an SOA reader that serves the root's serial and the monitors' canary reports;
//   - the ordering desktops: HTTP clients of the public contract, POST /tls/custom-challenge {domain, value} and GET
//     /tls/custom-challenge/<serial> under certs tokens (QD-175), racing one another where a test says.
//
// What each test proves: a POST then a GET answers the write's real serial and ready; concurrent POSTs of one
// value under the claim queue one command and make one write, each answered its serial; a write whose answer certs
// never heard is settled from the journal, so at a cap of one write an hour the hourly limit is charged once and the
// POST still answers; a desktop that takes the claim over before the dispatch refuses the old holder's write; a
// restore's stale answer lets the same digest be written again at a serial the restored zone serves; an interrupted
// POST's retry recovers the receipt with nothing queued or written again; and desktops racing a running worker all
// read one write. The store's clock is the test's, from the database's now, so a takeover's lapsed lease and a
// readiness answer's age are moved rather than waited for; the dns block's commit times are real.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/task"
)

// The store's clock, which the test moves. Safe for concurrent use.
type tlsChallengeE2eClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *tlsChallengeE2eClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *tlsChallengeE2eClock) Advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// Every NS serves the root's newest serial at once.
type tlsChallengeE2eSoaReader struct{}

func (tlsChallengeE2eSoaReader) ReadSoa(ctx context.Context, root string, ns string) (uint32, error) {
	state, err := model.GetDnsRootState(ctx, root)
	if err != nil {
		return 0, err
	}
	return uint32(state.Serial), nil
}

// certs' transport to the dns block, which loses the answers of the next acme-challenge writes it is told to: the
// request reaches the block, which commits, and certs hears only a reset. Safe for concurrent use.
type tlsChallengeE2eTransport struct {
	stateLock sync.Mutex
	loseCount int
}

func (self *tlsChallengeE2eTransport) loseWriteAnswers(count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.loseCount = count
}

func (self *tlsChallengeE2eTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil || request.URL.Path != dnsservice.DnsAcmeChallengePath {
		return response, err
	}
	lose := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.loseCount <= 0 {
			return false
		}
		self.loseCount -= 1
		return true
	}()
	if !lose {
		return response, nil
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return nil, errors.New("connection reset by peer")
}

// One sidecar of the network: its ids, keys and certs token.
type tlsChallengeE2eDevice struct {
	sidecarId   server.Id
	clientId    server.Id
	identityKey ed25519.PrivateKey
	tlsKey      *ecdsa.PrivateKey
	admin       bool
	token       string
}

// The world every test runs in (see the file header).
type tlsChallengeE2eWorld struct {
	t         testing.TB
	ctx       context.Context
	root      string
	networkId server.Id
	domain    string
	host      string
	clock     *tlsChallengeE2eClock
	store     *model.TlsCustomStore
	settings  *model.TlsCustomChallengeSettings
	fakeHost  *sidecartest.FakeHost
	apiServer *httptest.Server
	dnsApi    http.Handler
	transport *tlsChallengeE2eTransport
	worker    *task.TaskWorker
	account   *sidecar.TlsCustomAccount
	certId    sidecar.Id
	desktop   *tlsChallengeE2eDevice

	stateLock sync.Mutex
	devices   map[server.Id]*tlsChallengeE2eDevice
	// each barrier the handoff passed, by name, for the test to wait on
	barriers map[string]chan server.Id
	// called at each barrier before it is noted; nil for none
	onBarrier func(name string, challengeId server.Id)
}

func newTlsChallengeE2eWorld(t testing.TB, ctx context.Context) *tlsChallengeE2eWorld {
	t.Helper()
	id := strings.ReplaceAll(server.NewId().String(), "-", "")
	world := &tlsChallengeE2eWorld{
		t:         t,
		ctx:       ctx,
		root:      "zone" + id[20:] + ".example.",
		networkId: server.NewId(),
		domain:    "corp" + id[20:] + ".example",
		clock:     &tlsChallengeE2eClock{now: server.NowUtc().Truncate(time.Microsecond)},
		settings:  model.DefaultTlsCustomChallengeSettings(),
		transport: &tlsChallengeE2eTransport{},
		devices:   map[server.Id]*tlsChallengeE2eDevice{},
		barriers:  map[string]chan server.Id{},
	}
	world.host = "www." + world.domain
	t.Cleanup(model.Testing_SetDnsChallengesPerNameHour(10))

	// the network's verified domain with its acme token, and its active host mapped to shop
	if err := model.EnsureDnsRoot(ctx, world.root); err != nil {
		t.Fatal(err)
	}
	model.Testing_CreateNetwork(ctx, world.networkId, "tlsce2e"+id[20:], server.NewId())
	label, err := model.EnsureNetworkLabel(ctx, world.networkId, world.root)
	if err != nil {
		t.Fatal(err)
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatal(err)
	}
	acmeToken := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(tokenBytes))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_domain (network_id, domain, challenge, verify_time, acme_token) VALUES ($1, $2, $3, $4, $5)`,
			world.networkId,
			world.domain,
			"challenge",
			server.NowUtc(),
			acmeToken,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, create_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			world.host,
			world.root,
			world.networkId,
			"shop."+label+"."+world.root,
			sidecar.DnsCustomHostStateActive,
			sidecar.DnsCustomHostModeStrong,
			server.NowUtc(),
		))
	})

	// api: the certs routes' deps over a fake store, for the authenticator, and the custom routes on Postgres
	fakeHost, _ := installTlsCertsForTest(t, ctx)
	world.fakeHost = fakeHost
	t.Cleanup(func() {
		controller.SetTlsCerts(nil)
	})
	world.store, err = model.NewTlsCustomStore(&model.TlsCustomStoreOptions{
		Roots: []string{strings.TrimSuffix(world.root, ".")},
		Now:   world.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.SetTlsCustom(&controller.TlsCustomDeps{
		Store:           world.store,
		Caller:          world.caller,
		Roots:           []string{strings.TrimSuffix(world.root, ".")},
		OrderRetryAfter: time.Second,
		Challenges:      world.settings,
	})
	t.Cleanup(func() {
		controller.SetTlsCustom(nil)
	})
	world.apiServer = httptest.NewServer(router.NewRouter(ctx, Routes()))
	t.Cleanup(world.apiServer.Close)

	// dns: the API role on the same database, its internal routes on each class's bearer
	dnsFake := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
	dnsServiceHost := dnsFake.ServiceHost()
	dnsServiceHost.Clock = sidecar.RealClock{}
	if err := dnsFake.Gates.Set("sidecar:\n  dns: {enabled: true, writes: true}\n"); err != nil {
		t.Fatal(err)
	}
	dnsSettings := dnsservice.DefaultDnsSettings()
	dnsSettings.Roots = []string{world.root}
	dnsSettings.SignaturesPerSecond = 1000
	dnsSettings.Tcp.ConnectionsPerSource = 2
	dnsSettings.Rrl.ResponsesPerSecond = 1000
	dnsSettings.Edge.UdpReaderCount = 1
	if err := dnsSettings.Validate(); err != nil {
		t.Fatal(err)
	}
	verify := ""
	for _, class := range []dnsservice.DnsCallerClass{dnsservice.DnsCallerCerts, dnsservice.DnsCallerMonitor, dnsservice.DnsCallerMonitorOutside} {
		verify += fmt.Sprintf("%s: {class: %s, sha256: [%x]}\n", class, class, sha256.Sum256([]byte(class)))
	}
	secretDirectory := t.TempDir()
	for name, content := range map[string]string{
		dnshost.HostVerifyFileName: verify,
		server.InternalGateCredentialFileName(server.InternalGateCallerDns): "current: " + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) + "\n",
	} {
		if err := writeTlsChallengeE2eFile(secretDirectory, name, content); err != nil {
			t.Fatal(err)
		}
	}
	projectionApi := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(projectionApi.Close)
	engine, err := dnshost.NewApiHost(ctx, dnsServiceHost, &dnshost.HostSettings{Dns: dnsSettings, SecretDirectory: secretDirectory, ApiUrl: projectionApi.URL}, &dnshost.HostDeps{SoaReader: tlsChallengeE2eSoaReader{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	dnsApi, ok := engine.(interface {
		Handler() http.Handler
		Ready() bool
		Probes() (int, chan struct{})
	})
	if !ok {
		t.Fatalf("the dns api role is a %T", engine)
	}
	for !dnsApi.Ready() {
		_, changed := dnsApi.Probes()
		if dnsApi.Ready() {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the dns api never became ready")
		}
	}
	world.dnsApi = dnsApi.Handler()
	dnsServer := httptest.NewServer(world.dnsApi)
	t.Cleanup(dnsServer.Close)

	// certs: its DNS client to the block on the certs bearer, the command's
	clientSettings := server.DefaultCertsDnsClientSettings()
	clientSettings.BaseUrl = dnsServer.URL
	clientSettings.Root = world.root
	dnsClient, err := server.NewCertsDnsClient(clientSettings, string(dnsservice.DnsCallerCerts), world.transport)
	if err != nil {
		t.Fatal(err)
	}
	installed := controller.SetTlsCustomChallengeWorker(&controller.TlsCustomChallengeWorkerDeps{Store: world.store, Dns: dnsClient, Settings: world.settings})
	t.Cleanup(func() {
		controller.UnsetTlsCustomChallengeWorker(installed)
	})

	// taskworker: the queue's worker with the command alone
	workerSettings := task.DefaultTaskWorkerSettings()
	workerSettings.ClaimRegisteredTargetsOnly = true
	workerSettings.PollTimeout = 100 * time.Millisecond
	workerSettings.DrainFinishTimeout = 30 * time.Second
	world.worker = task.NewTaskWorker(ctx, workerSettings)
	world.worker.AddTargets(task.NewTaskTargetWithPost(controller.TlsCustomChallengeWork, controller.TlsCustomChallengeWorkPost))
	t.Cleanup(world.worker.Close)

	controller.Testing_TlsCustomChallengeBarrier = world.barrier
	t.Cleanup(func() {
		controller.Testing_TlsCustomChallengeBarrier = nil
	})

	// the account an admin's desktop made, a phone's queued order, and the ordering desktop's claim on it
	admin := world.device(true)
	account, err := world.store.AccountOp(ctx, world.modelCaller(admin), &sidecar.TlsCustomAccountArgs{
		Domain:        world.domain,
		Op:            sidecar.TlsCustomAccountCreate,
		AccountUri:    "https://ca.example/acme/acct/1",
		KeyThumbprint: func() []byte { thumbprint := sha256.Sum256([]byte("account key")); return thumbprint[:] }(),
		KeyRepoId:     sidecar.NewId(),
	})
	if err != nil {
		t.Fatal(err)
	}
	world.account = account
	phone := world.device(false)
	queued, err := world.store.QueueOrder(ctx, world.modelCaller(phone), world.orderArgs(phone), server.NewId(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	world.certId = queued.CertId
	world.desktop = world.device(false)
	world.claim(world.desktop)
	return world
}

func writeTlsChallengeE2eFile(directory string, name string, content string) error {
	return os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600)
}

// A granted sidecar of the network with a certs token, an admin's when admin.
func (self *tlsChallengeE2eWorld) device(admin bool) *tlsChallengeE2eDevice {
	self.t.Helper()
	_, identityKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		self.t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		self.t.Fatal(err)
	}
	device := &tlsChallengeE2eDevice{sidecarId: server.NewId(), clientId: server.NewId(), identityKey: identityKey, tlsKey: tlsKey, admin: admin}
	sidecarId := connect.Id(device.sidecarId)
	now := self.fakeHost.Clock.Now()
	device.token = self.fakeHost.Authenticator.Issue(&sidecar.Principal{
		NetworkId: connect.Id(self.networkId),
		ClientId:  connect.Id(device.clientId),
		SidecarId: &sidecarId,
		Audience:  controller.TlsCertsAudience,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	})
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.devices[device.sidecarId] = device
	return device
}

// The caller api builds from a device's token, as the projection would: every device of the world holds the publish
// grant on shop's tls/ names.
func (self *tlsChallengeE2eWorld) modelCaller(device *tlsChallengeE2eDevice) *model.TlsCustomCaller {
	return &model.TlsCustomCaller{
		NetworkId:         self.networkId,
		ClientId:          device.clientId,
		SidecarId:         device.sidecarId,
		PublishGranted:    func(ownerLabel string) bool { return ownerLabel == "shop" },
		Admin:             device.admin,
		IdentityPublicKey: device.identityKey.Public().(ed25519.PublicKey),
	}
}

// TlsCustomDeps.Caller: the token's device, or a caller with no grant for a sidecar the world does not hold.
func (self *tlsChallengeE2eWorld) caller(ctx context.Context, principal *sidecar.Principal, now time.Time) (*model.TlsCustomCaller, error) {
	if principal == nil || principal.SidecarId == nil {
		return nil, errors.New("no sidecar")
	}
	self.stateLock.Lock()
	device, ok := self.devices[server.Id(*principal.SidecarId)]
	self.stateLock.Unlock()
	if !ok {
		return &model.TlsCustomCaller{NetworkId: server.Id(principal.NetworkId), ClientId: server.Id(principal.ClientId), SidecarId: server.Id(*principal.SidecarId)}, nil
	}
	return self.modelCaller(device), nil
}

// device's CSR for the world's host and its attestation of it.
func (self *tlsChallengeE2eWorld) orderArgs(device *tlsChallengeE2eDevice) *sidecar.TlsCertOrderArgs {
	self.t.Helper()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{self.host}}, device.tlsKey)
	if err != nil {
		self.t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&device.tlsKey.PublicKey)
	if err != nil {
		self.t.Fatal(err)
	}
	spkiSha256 := sha256.Sum256(spki)
	setSha256, err := sidecar.TlsIdentifierSetHash([]string{self.host})
	if err != nil {
		self.t.Fatal(err)
	}
	attestation, err := sidecar.SignTlsKeyAttestation(&protocol.TlsKeyAttestation{
		NetworkId:           self.networkId.Bytes(),
		SidecarId:           device.sidecarId.Bytes(),
		SpkiSha256:          spkiSha256[:],
		IdentifierSetSha256: setSha256[:],
		IssueTimeMillis:     uint64(self.clock.Now().UnixMilli()),
	}, func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
		return &bridge.Signed{
			Body:      body,
			SignerId:  device.sidecarId.Bytes(),
			Signature: ed25519.Sign(device.identityKey, bridge.SignedBytes(purpose, signedType, body)),
			Type:      uint32(signedType),
		}, nil
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return &sidecar.TlsCertOrderArgs{Csr: csr, Attestation: attestation}
}

// device's claim for an order of the world's row.
func (self *tlsChallengeE2eWorld) claim(device *tlsChallengeE2eDevice) *sidecar.TlsCustomClaimResult {
	self.t.Helper()
	claim, err := self.store.Claim(self.ctx, self.modelCaller(device), &sidecar.TlsCustomClaimArgs{
		Domain:  self.domain,
		Purpose: sidecar.TlsCustomPurposeOrder,
		CertId:  self.certId,
		KeySeq:  self.account.KeySeq,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return claim
}

// controller.Testing_TlsCustomChallengeBarrier: the test's callback, then the barrier noted for a waiter.
func (self *tlsChallengeE2eWorld) barrier(name string, challengeId server.Id) {
	self.stateLock.Lock()
	onBarrier := self.onBarrier
	passed, ok := self.barriers[name]
	if !ok {
		passed = make(chan server.Id, 64)
		self.barriers[name] = passed
	}
	self.stateLock.Unlock()
	if onBarrier != nil {
		onBarrier(name, challengeId)
	}
	// a barrier no test waits on fills its buffer and is passed on, never held
	select {
	case passed <- challengeId:
	default:
	}
}

// Waits for the handoff to pass the barrier name count times: the receipts' ids, in order.
func (self *tlsChallengeE2eWorld) waitBarrier(name string, count int) []server.Id {
	self.t.Helper()
	self.stateLock.Lock()
	passed, ok := self.barriers[name]
	if !ok {
		passed = make(chan server.Id, 64)
		self.barriers[name] = passed
	}
	self.stateLock.Unlock()
	challengeIds := []server.Id{}
	for range count {
		select {
		case challengeId := <-passed:
			challengeIds = append(challengeIds, challengeId)
		case <-self.ctx.Done():
			self.t.Fatalf("the handoff never passed %s", name)
		}
	}
	return challengeIds
}

// The command's queued runs.
func (self *tlsChallengeE2eWorld) queuedRunCount() int {
	self.t.Helper()
	count := 0
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			self.ctx,
			`SELECT count(*) FROM pending_task WHERE function_name = $1`,
			task.NewTaskTarget(controller.TlsCustomChallengeWork).TargetFunctionName(),
		).Scan(&count))
	})
	return count
}

// Runs the command's queued run as taskworker would, made claimable now: the queue's one-second eligibility block
// crossed explicitly rather than waited out.
func (self *tlsChallengeE2eWorld) runWorker() {
	self.t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`UPDATE pending_task SET run_at = $2 WHERE function_name = $1`,
			task.NewTaskTarget(controller.TlsCustomChallengeWork).TargetFunctionName(),
			time.Unix(1, 0).UTC(),
		))
	}, server.TxReadCommitted)
	finished, _, _, err := self.worker.EvalTasks(1)
	if err != nil || len(finished) != 1 {
		self.t.Fatalf("the command's run: %v finished, %v", finished, err)
	}
}

// What a route answered: its status, its body as the success's result or the refusal's.
type tlsChallengeE2eAnswer struct {
	status  int
	result  *sidecar.TlsCustomChallengeResult
	refusal *sidecar.TlsCustomRefusal
	err     error
}

// One request of the public contract under device's certs token.
func (self *tlsChallengeE2eWorld) call(ctx context.Context, device *tlsChallengeE2eDevice, method string, path string, body any) *tlsChallengeE2eAnswer {
	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return &tlsChallengeE2eAnswer{err: err}
		}
		bodyReader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, self.apiServer.URL+path, bodyReader)
	if err != nil {
		return &tlsChallengeE2eAnswer{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+device.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return &tlsChallengeE2eAnswer{err: err}
	}
	defer response.Body.Close()
	answer := &tlsChallengeE2eAnswer{status: response.StatusCode}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		answer.err = err
		return answer
	}
	if response.StatusCode == http.StatusOK {
		answer.result = &sidecar.TlsCustomChallengeResult{}
		answer.err = json.Unmarshal(data, answer.result)
		return answer
	}
	refusal := &sidecar.TlsCustomRefusal{}
	if json.Unmarshal(data, refusal) == nil && refusal.Reason != "" {
		answer.refusal = refusal
	}
	return answer
}

// POST /tls/custom-challenge as an ordering desktop sends it.
func (self *tlsChallengeE2eWorld) post(ctx context.Context, device *tlsChallengeE2eDevice, value string) *tlsChallengeE2eAnswer {
	return self.call(ctx, device, http.MethodPost, "/tls/custom-challenge", &sidecar.TlsCustomChallengeArgs{Domain: self.domain, Value: value})
}

// GET /tls/custom-challenge/<serial> as an ordering desktop asks it.
func (self *tlsChallengeE2eWorld) get(ctx context.Context, device *tlsChallengeE2eDevice, serial uint64) *tlsChallengeE2eAnswer {
	return self.call(ctx, device, http.MethodGet, fmt.Sprintf("/tls/custom-challenge/%d", serial), nil)
}

// A POST answered on its own goroutine.
func (self *tlsChallengeE2eWorld) postAsync(ctx context.Context, device *tlsChallengeE2eDevice, value string) chan *tlsChallengeE2eAnswer {
	answered := make(chan *tlsChallengeE2eAnswer, 1)
	go func() {
		answered <- self.post(ctx, device, value)
	}()
	return answered
}

// A GET answered on its own goroutine.
func (self *tlsChallengeE2eWorld) getAsync(ctx context.Context, device *tlsChallengeE2eDevice, serial uint64) chan *tlsChallengeE2eAnswer {
	answered := make(chan *tlsChallengeE2eAnswer, 1)
	go func() {
		answered <- self.get(ctx, device, serial)
	}()
	return answered
}

func (self *tlsChallengeE2eWorld) answer(answered chan *tlsChallengeE2eAnswer) *tlsChallengeE2eAnswer {
	self.t.Helper()
	select {
	case answer := <-answered:
		return answer
	case <-self.ctx.Done():
		self.t.Fatal("no answer")
		return nil
	}
}

// The serial a POST answered, failing the test on anything else.
func (self *tlsChallengeE2eWorld) serialOf(answer *tlsChallengeE2eAnswer) uint64 {
	self.t.Helper()
	if answer.err != nil || answer.status != http.StatusOK || answer.result == nil || answer.result.ChallengeId == 0 {
		self.t.Fatalf("the POST answered %d %+v %+v %v", answer.status, answer.result, answer.refusal, answer.err)
	}
	return answer.result.ChallengeId
}

// The readiness a GET answered for serial, failing the test on anything else.
func (self *tlsChallengeE2eWorld) readinessOf(answer *tlsChallengeE2eAnswer, serial uint64) string {
	self.t.Helper()
	if answer.err != nil || answer.status != http.StatusOK || answer.result == nil || answer.result.ChallengeId != serial {
		self.t.Fatalf("the GET answered %d %+v %+v %v", answer.status, answer.result, answer.refusal, answer.err)
	}
	return answer.result.Readiness
}

// Both monitors report the canary clean at the root's serial, through the block's own report route.
func (self *tlsChallengeE2eWorld) reportCanaryClean() {
	self.t.Helper()
	state, err := model.GetDnsRootState(self.ctx, self.root)
	if err != nil {
		self.t.Fatal(err)
	}
	for class, source := range map[dnsservice.DnsCallerClass]string{
		dnsservice.DnsCallerMonitor:        dnsservice.DnsCanarySourceSite,
		dnsservice.DnsCallerMonitorOutside: dnsservice.DnsCanarySourceOutside,
	} {
		body, err := json.Marshal(dnsservice.DnsCanaryReport{Root: self.root, Source: source, Ok: true, Serial: state.Serial})
		if err != nil {
			self.t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, dnsservice.DnsCanaryReportPath, bytes.NewReader(body)).WithContext(self.ctx)
		request.Header.Set("Authorization", "Bearer "+string(class))
		reply := httptest.NewRecorder()
		self.dnsApi.ServeHTTP(reply, request)
		if reply.Code != http.StatusNoContent {
			self.t.Fatalf("%s's report: %d", source, reply.Code)
		}
	}
}

// The journal's writes of the domain's acme name, oldest first: their serials, which the hourly cap counts.
func (self *tlsChallengeE2eWorld) journalWrites() []uint64 {
	self.t.Helper()
	match, err := json.Marshal(map[string]any{"upserts": []any{map[string]any{"owner": dns.CanonicalName(self.account.AcmeName), "type": dns.TypeTXT}}})
	if err != nil {
		self.t.Fatal(err)
	}
	serials := []uint64{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`SELECT serial FROM dns_zone_journal WHERE root = $1 AND network_id IS NULL AND diff @> $2::jsonb ORDER BY serial`,
			self.root,
			string(match),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var serial int64
				server.Raise(result.Scan(&serial))
				serials = append(serials, uint64(serial))
			}
		})
	})
	return serials
}

// What the acme name's TXT holds.
func (self *tlsChallengeE2eWorld) txt() []string {
	self.t.Helper()
	rdata := []string{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			self.ctx,
			`SELECT rdata FROM network_dns_record WHERE root = $1 AND owner = $2 AND rrtype = $3`,
			self.root,
			dns.CanonicalName(self.account.AcmeName),
			int32(dns.TypeTXT),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				server.Raise(result.Scan(&rdata))
			}
		})
	})
	return rdata
}

// The DNS-01 value of a key authorization.
func tlsChallengeE2eValue(keyAuthorization string) string {
	digest := sha256.Sum256([]byte(keyAuthorization))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// The test's own deadline, past every wait the handoff makes.
func tlsChallengeE2eContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Minute)
}

// POST then GET: the POST answers the serial the certs worker's write took at the dns block, the name serves the
// value, and the GET, once the worker has read /dns/ready, answers ready for that serial; another sidecar of the
// network reads no receipt of the desktop's.
func TestTlsCustomChallengeE2ePostThenGetReturnsTheSerial(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := tlsChallengeE2eContext()
		defer cancel()
		world := newTlsChallengeE2eWorld(t, ctx)
		value := tlsChallengeE2eValue("token1.thumbprint")
		posted := world.postAsync(ctx, world.desktop, value)
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		if count := world.queuedRunCount(); count != 1 {
			t.Fatalf("%d runs queued by the capture", count)
		}
		world.runWorker()
		serial := world.serialOf(world.answer(posted))
		if writes := world.journalWrites(); len(writes) != 1 || writes[0] != serial {
			t.Fatalf("the journal holds %v, the POST answered %d", writes, serial)
		}
		if txt := world.txt(); len(txt) != 1 || txt[0] != `"`+value+`"` {
			t.Fatalf("the acme name holds %v", txt)
		}
		world.reportCanaryClean()
		asked := world.getAsync(ctx, world.desktop, serial)
		world.waitBarrier(controller.TlsCustomChallengeBarrierReadyAsked, 1)
		world.runWorker()
		if readiness := world.readinessOf(world.answer(asked), serial); readiness != "ready" {
			t.Fatalf("the GET answered %q", readiness)
		}
		if other := world.get(ctx, world.device(false), serial); other.status != http.StatusNotFound {
			t.Fatalf("another sidecar's GET answered %d %+v", other.status, other.result)
		}
	})
}

// Eight POSTs of one value under the claim at once, as a desktop's retries race: every capture lands on one receipt
// and one queued run, one write is made, and every POST answers its serial.
func TestTlsCustomChallengeE2eConcurrentPostsWriteOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := tlsChallengeE2eContext()
		defer cancel()
		world := newTlsChallengeE2eWorld(t, ctx)
		value := tlsChallengeE2eValue("token2.thumbprint")
		const postCount = 8
		postedAll := []chan *tlsChallengeE2eAnswer{}
		for range postCount {
			postedAll = append(postedAll, world.postAsync(ctx, world.desktop, value))
		}
		challengeIds := world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, postCount)
		for _, challengeId := range challengeIds {
			if challengeId != challengeIds[0] {
				t.Fatalf("the captures made receipts %v", challengeIds)
			}
		}
		if count := world.queuedRunCount(); count != 1 {
			t.Fatalf("%d runs queued by %d captures", count, postCount)
		}
		world.runWorker()
		serials := map[uint64]bool{}
		for _, posted := range postedAll {
			serials[world.serialOf(world.answer(posted))] = true
		}
		writes := world.journalWrites()
		if len(writes) != 1 || len(serials) != 1 || !serials[writes[0]] {
			t.Fatalf("%d POSTs answered %v and the journal holds %v", postCount, serials, writes)
		}
	})
}

// certs never hears the dns block's answer to its write, which committed: the worker settles the write from the
// journal in the same run, and the POST answers its serial. At a cap of one write an hour, a second write would have
// been refused 429, so the cap was charged once.
func TestTlsCustomChallengeE2eLostAcknowledgementChargesOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := tlsChallengeE2eContext()
		defer cancel()
		world := newTlsChallengeE2eWorld(t, ctx)
		defer model.Testing_SetDnsChallengesPerNameHour(1)()
		world.transport.loseWriteAnswers(1)
		value := tlsChallengeE2eValue("token3.thumbprint")
		posted := world.postAsync(ctx, world.desktop, value)
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		world.runWorker()
		serial := world.serialOf(world.answer(posted))
		if writes := world.journalWrites(); len(writes) != 1 || writes[0] != serial {
			t.Fatalf("the journal holds %v, the POST answered %d", writes, serial)
		}
		if count := world.queuedRunCount(); count != 0 {
			t.Fatalf("%d runs still queued after the settle", count)
		}
	})
}

// Another desktop takes the claim over once the holder's lease lapsed, after the holder's capture and before the
// worker's dispatch: the worker refuses the old claim's write, whose POST answers 409 claim, and the new holder's
// POST writes. One write, the new holder's.
func TestTlsCustomChallengeE2eTakeoverBeforeDispatchIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := tlsChallengeE2eContext()
		defer cancel()
		world := newTlsChallengeE2eWorld(t, ctx)
		value := tlsChallengeE2eValue("token4.thumbprint")
		posted := world.postAsync(ctx, world.desktop, value)
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		world.clock.Advance(3 * time.Minute)
		takeover := world.device(false)
		world.claim(takeover)
		world.runWorker()
		refused := world.answer(posted)
		if refused.status != http.StatusConflict || refused.refusal == nil || refused.refusal.Reason != sidecar.TlsCustomRefusalClaim {
			t.Fatalf("the old holder's POST answered %d %+v %+v %v", refused.status, refused.result, refused.refusal, refused.err)
		}
		if writes := world.journalWrites(); len(writes) != 0 {
			t.Fatalf("the refused claim wrote %v", writes)
		}
		newer := world.postAsync(ctx, takeover, value)
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		world.runWorker()
		serial := world.serialOf(world.answer(newer))
		if writes := world.journalWrites(); len(writes) != 1 || writes[0] != serial {
			t.Fatalf("the journal holds %v, the new holder's POST answered %d", writes, serial)
		}
		// the old holder's retry is refused at its capture
		if again := world.post(ctx, world.desktop, value); again.status != http.StatusConflict || again.refusal == nil || again.refusal.Reason != sidecar.TlsCustomRefusalClaim {
			t.Fatalf("the old holder's retry answered %d %+v", again.status, again.refusal)
		}
	})
}

// A restore moves the zone past the written serial: the GET, once its answer is no longer fresh, reads stale from
// the block, which the receipt keeps as final; the desktop's POST of the same value then makes a new write above the
// restore, which reads ready.
func TestTlsCustomChallengeE2eStaleVerdictWritesTheDigestAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := tlsChallengeE2eContext()
		defer cancel()
		world := newTlsChallengeE2eWorld(t, ctx)
		value := tlsChallengeE2eValue("token5.thumbprint")
		posted := world.postAsync(ctx, world.desktop, value)
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		world.runWorker()
		first := world.serialOf(world.answer(posted))
		world.reportCanaryClean()
		asked := world.getAsync(ctx, world.desktop, first)
		world.waitBarrier(controller.TlsCustomChallengeBarrierReadyAsked, 1)
		world.runWorker()
		if readiness := world.readinessOf(world.answer(asked), first); readiness != "ready" {
			t.Fatalf("before the restore: %q", readiness)
		}

		// §12.6's restore
		if err := model.SetDnsRestoring(ctx, world.root, true); err != nil {
			t.Fatal(err)
		}
		restoreSerial, err := model.CommitDnsRestore(ctx, nil, world.root, server.NewId(), first)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.SetDnsRestoring(ctx, world.root, false); err != nil {
			t.Fatal(err)
		}
		// a fresh ready is answered as it is; past ReadyFreshness the worker reads again
		if fresh := world.get(ctx, world.desktop, first); world.readinessOf(fresh, first) != "ready" {
			t.Fatalf("a fresh answer: %+v", fresh.result)
		}
		world.clock.Advance(world.settings.ReadyFreshness)
		asked = world.getAsync(ctx, world.desktop, first)
		world.waitBarrier(controller.TlsCustomChallengeBarrierReadyAsked, 1)
		world.runWorker()
		if readiness := world.readinessOf(world.answer(asked), first); readiness != "stale" {
			t.Fatalf("after the restore: %q", readiness)
		}
		rewritten := world.postAsync(ctx, world.desktop, value)
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		world.runWorker()
		second := world.serialOf(world.answer(rewritten))
		if second <= restoreSerial {
			t.Fatalf("the rewrite took %d, at or below the restore's %d", second, restoreSerial)
		}
		world.reportCanaryClean()
		asked = world.getAsync(ctx, world.desktop, second)
		world.waitBarrier(controller.TlsCustomChallengeBarrierReadyAsked, 1)
		world.runWorker()
		if readiness := world.readinessOf(world.answer(asked), second); readiness != "ready" {
			t.Fatalf("the rewrite: %q", readiness)
		}
		if writes := world.journalWrites(); len(writes) != 2 || writes[1] != second {
			t.Fatalf("the journal holds %v", writes)
		}
		if old := world.get(ctx, world.desktop, first); old.status != http.StatusNotFound {
			t.Fatalf("the replaced serial answered %d %+v", old.status, old.result)
		}
	})
}

// The desktop's POST ends once its capture committed, before any answer; the worker writes meanwhile, and the
// desktop's retry is answered the same serial at once, with no run queued and nothing written again.
func TestTlsCustomChallengeE2eInterruptedPostRecoversTheReceipt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := tlsChallengeE2eContext()
		defer cancel()
		world := newTlsChallengeE2eWorld(t, ctx)
		value := tlsChallengeE2eValue("token6.thumbprint")
		postCtx, postCancel := context.WithCancel(ctx)
		defer postCancel()
		world.stateLock.Lock()
		world.onBarrier = func(name string, challengeId server.Id) {
			if name == controller.TlsCustomChallengeBarrierCaptured {
				postCancel()
			}
		}
		world.stateLock.Unlock()
		interrupted := world.post(postCtx, world.desktop, value)
		if interrupted.err == nil {
			t.Fatalf("the interrupted POST answered %d %+v", interrupted.status, interrupted.result)
		}
		world.stateLock.Lock()
		world.onBarrier = nil
		world.stateLock.Unlock()
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		world.runWorker()
		writes := world.journalWrites()
		if len(writes) != 1 {
			t.Fatalf("the journal holds %v", writes)
		}
		retried := world.post(ctx, world.desktop, value)
		if serial := world.serialOf(retried); serial != writes[0] {
			t.Fatalf("the retry answered %d, the write took %d", serial, writes[0])
		}
		world.waitBarrier(controller.TlsCustomChallengeBarrierCaptured, 1)
		if count := world.queuedRunCount(); count != 0 {
			t.Fatalf("the retry queued %d runs", count)
		}
		if again := world.journalWrites(); len(again) != 1 {
			t.Fatalf("the retry wrote: %v", again)
		}
	})
}

// Desktops racing a running worker: the holder's retries and GETs, and other sidecars' GETs of the same serial, all
// at once while the queue's worker runs on its own. Every POST answers the one write's serial, every holder's GET
// answers ready for it, and every other sidecar's GET finds nothing.
func TestTlsCustomChallengeE2eDesktopsRaceARunningWorker(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := tlsChallengeE2eContext()
		defer cancel()
		world := newTlsChallengeE2eWorld(t, ctx)
		world.reportCanaryClean()
		running := make(chan struct{})
		go func() {
			defer close(running)
			world.worker.Run()
		}()
		defer func() {
			world.worker.Drain()
			<-running
		}()
		value := tlsChallengeE2eValue("token7.thumbprint")
		const postCount = 6
		answers := make(chan *tlsChallengeE2eAnswer, postCount)
		var postGroup sync.WaitGroup
		for range postCount {
			postGroup.Add(1)
			go func() {
				defer postGroup.Done()
				answers <- world.post(ctx, world.desktop, value)
			}()
		}
		postGroup.Wait()
		close(answers)
		serials := map[uint64]bool{}
		for answer := range answers {
			serials[world.serialOf(answer)] = true
		}
		writes := world.journalWrites()
		if len(writes) != 1 || len(serials) != 1 || !serials[writes[0]] {
			t.Fatalf("POSTs answered %v and the journal holds %v", serials, writes)
		}
		world.reportCanaryClean()
		const getCount = 4
		others := []*tlsChallengeE2eDevice{}
		for range getCount {
			others = append(others, world.device(false))
		}
		type read struct {
			holder bool
			answer *tlsChallengeE2eAnswer
		}
		reads := make(chan read, 2*getCount)
		var getGroup sync.WaitGroup
		for index := range getCount {
			getGroup.Add(2)
			go func() {
				defer getGroup.Done()
				reads <- read{holder: true, answer: world.get(ctx, world.desktop, writes[0])}
			}()
			go func() {
				defer getGroup.Done()
				reads <- read{holder: false, answer: world.get(ctx, others[index], writes[0])}
			}()
		}
		getGroup.Wait()
		close(reads)
		for read := range reads {
			switch {
			case read.holder:
				// a GET whose wait ended before the worker's read answers waiting, and the desktop asks again
				if readiness := world.readinessOf(read.answer, writes[0]); readiness != "ready" && readiness != "waiting" {
					t.Fatalf("the holder's GET answered %q", readiness)
				}
			case read.answer.status != http.StatusNotFound:
				t.Fatalf("another sidecar's GET answered %d %+v", read.answer.status, read.answer.result)
			}
		}
		// the holder asks again until the worker's read lands, as its poll does, each ask waiting up to ReadyWait
		const askCount = 6
		for ask := 1; ; ask += 1 {
			if readiness := world.readinessOf(world.get(ctx, world.desktop, writes[0]), writes[0]); readiness == "ready" {
				break
			}
			if askCount <= ask {
				t.Fatalf("the holder's %d GETs never read ready", askCount)
			}
		}
		if again := world.journalWrites(); len(again) != 1 {
			t.Fatalf("the race wrote %v", again)
		}
	})
}
