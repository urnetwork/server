package mxedgehost

// §15.6's wake hold on server's side (MAIL-M8-05; FS-36, I-12): a held session, its hint through the block's own hint
// writer and §18.7's enqueue, and the retry that takes the woken receiver, with every PostgreSQL acquisition in the
// process refused; and FS-36's forced router miss: the miss's hint to a token holder whose resident is live is due
// now, so the push sender submits one wake at once, with no deferral. The block gives the engine its writer.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A resolver that knows the receivers a test binds, as api's internal route would answer them.
type routerWakeTestResolver struct {
	stateLock        sync.Mutex
	sidecarClientIds map[connect.Id]connect.Id
}

func (self *routerWakeTestResolver) PushClientIds(ctx context.Context, sidecarIds []connect.Id) (map[connect.Id]connect.Id, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clientIds := map[connect.Id]connect.Id{}
	for _, sidecarId := range sidecarIds {
		if clientId, ok := self.sidecarClientIds[sidecarId]; ok {
			clientIds[sidecarId] = clientId
		}
	}
	return clientIds, nil
}

// The block's writer, telling the test each hint by sidecar once it has returned.
type routerWakeTestPush struct {
	sidecar.Push
	hinted chan error
}

func (self *routerWakeTestPush) HintSidecars(ctx context.Context, sidecarIds []connect.Id, hint *sidecar.ClientHint) error {
	err := self.Push.HintSidecars(ctx, sidecarIds, hint)
	self.hinted <- err
	return err
}

// A dialer that finds no receiver for its first misses dials, then ends TLS on the receiver as routerTestTlsDialer
// does. Each dial is signalled before it answers.
type routerWakeTestDialer struct {
	*routerTestTlsDialer
	dialing chan struct{}

	stateLock sync.Mutex
	misses    int
}

func (self *routerWakeTestDialer) Dial(ctx context.Context, request *mxedgeservice.MxEdgeDialRequest) (mxedgeservice.MxEdgeStream, uint64, error) {
	missed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if 0 < self.misses {
			self.misses -= 1
			return true
		}
		return false
	}()
	self.dialing <- struct{}{}
	if missed {
		return nil, 0, &sidecar.SocketCodeError{Err: sidecar.ErrSocketNoListener}
	}
	return self.routerTestTlsDialer.Dial(ctx, request)
}

// One router world: a fake host whose Redis runs the push scripts, the block's hint writer over it with resolver, the
// wake switch on, the test network's route listing one receiver bound to its client, and the engine on a TLS dialer
// that misses first.
type routerWakeTest struct {
	t          *testing.T
	ctx        context.Context
	clock      *sidecartest.FakeClock
	fakeHost   *sidecartest.FakeHost
	settings   *mxedgeservice.MxEdgeSettings
	dialer     *routerWakeTestDialer
	engine     *mxedgeservice.MxEdgeService
	listener   *routerTestListener
	push       *routerWakeTestPush
	chainPem   []byte
	receiverId connect.Id
	clientId   connect.Id
}

func newRouterWakeTest(t *testing.T, misses int) *routerWakeTest {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	start := sidecartest.DefaultClockStart()
	clock := sidecartest.NewFakeClock(start)
	fakeHost := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	sidecartest.RegisterPushScripts(fakeHost.Redis)
	networkId := sidecar.NewId()
	if err := fakeHost.Gates.Set("sidecar:\n  wake:\n    enabled: true\n"); err != nil {
		t.Fatal(err)
	}
	fakeHost.Gates.OpenService(sidecar.ServiceGateMxedge, connect.Id(networkId))
	fakeHost.Quota.SetBalance(connect.Id(networkId), 64*1024*1024)
	self := &routerWakeTest{
		t:          t,
		ctx:        ctx,
		clock:      clock,
		fakeHost:   fakeHost,
		receiverId: connect.NewId(),
		clientId:   connect.NewId(),
	}
	routeValue, err := json.Marshal(&model.MailRoute{
		NetworkId:          server.Id(networkId),
		Label:              routerTestLabel,
		Host:               "mx",
		ReceiverSidecarIds: []server.Id{server.Id(self.receiverId)},
	})
	if err != nil {
		t.Fatal(err)
	}
	inspect := sidecartest.InspectContext(ctx)
	if err := fakeHost.Redis.Set(inspect, model.MailRouteKey(routerTestLabel), routeValue, 0); err != nil {
		t.Fatal(err)
	}
	self.settings = routerTestSettings()
	self.settings.WakeHold = true

	serviceHost := fakeHost.ServiceHost()
	resolver := &routerWakeTestResolver{sidecarClientIds: map[connect.Id]connect.Id{self.receiverId: self.clientId}}
	writer, err := newRouterPush(serviceHost, resolver)
	if err != nil {
		t.Fatal(err)
	}
	self.push = &routerWakeTestPush{Push: writer, hinted: make(chan error, 16)}
	serviceHost.Push = self.push

	chainPem, keyPem := hostTestCertificatePem(t, []string{routerTestSni}, start.Add(-time.Hour), start.Add(24*time.Hour))
	certificate, err := tls.X509KeyPair(chainPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	self.chainPem = chainPem
	self.dialer = &routerWakeTestDialer{
		routerTestTlsDialer: &routerTestTlsDialer{t: t, certificate: certificate, now: start, received: make(chan []byte, 1)},
		dialing:             make(chan struct{}, 64),
		misses:              misses,
	}
	for range self.settings.Clients {
		self.dialer.clientIds = append(self.dialer.clientIds, sidecar.NewId())
	}
	engine, err := mxedgeservice.NewMxEdgeRouterService(ctx, serviceHost, sidecartest.NewFakeMxEdgeStore(nil), NewEngineDeps(serviceHost, nil),
		&mxedgeservice.MxEdgeRouterDeps{Dialer: self.dialer}, self.settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	self.engine = engine
	awaitHostTestTimerAt(t, ctx, clock, start.Add(hostTestProbeInterval))
	if !engine.Ready() {
		t.Fatal("the router is not ready")
	}
	self.listener = &routerTestListener{conns: make(chan net.Conn, 1), closed: make(chan struct{})}
	go engine.Serve(self.listener)
	return self
}

// Gives the receiver's client a wake token and a live resident, as step 0's apply and its publisher leave them.
func (self *routerWakeTest) giveToken(platform string) string {
	self.t.Helper()
	token := "synthetic-" + platform + "-token"
	inspect := sidecartest.InspectContext(self.ctx)
	if err := self.fakeHost.Redis.Set(inspect, pushservice.HintTokenKey(self.clientId), pushservice.WakeTokenValue(platform, token), 0); err != nil {
		self.t.Fatal(err)
	}
	if err := self.fakeHost.Redis.Set(inspect, pushservice.WakeTokenRevisionKey(self.clientId), []byte(strconv.Itoa(1)), 0); err != nil {
		self.t.Fatal(err)
	}
	if err := self.fakeHost.Redis.Set(inspect, pushservice.HintLiveKey(self.clientId), []byte("1"), 90*time.Second); err != nil {
		self.t.Fatal(err)
	}
	return token
}

// A sender's session to the 220 of STARTTLS, then its ClientHello by its own TLS client, which returns once the
// handshake through the splice has finished: the sender's end, its reader and the handshake's result.
func (self *routerWakeTest) send() (net.Conn, chan error, *tls.Conn) {
	self.t.Helper()
	serverConn, clientConn := net.Pipe()
	self.t.Cleanup(func() { clientConn.Close() })
	self.listener.conns <- &routerTestServerConn{Conn: serverConn, sender: &net.TCPAddr{IP: net.ParseIP(hostTestSender), Port: 2525}}
	reader := bufio.NewReader(clientConn)
	expect := func(want string) {
		self.t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil || line != want+"\r\n" {
			self.t.Fatalf("read %q, %v; want %q", line, err, want)
		}
	}
	expect("220 " + hostTestBannerHostName + " ESMTP")
	clientConn.Write([]byte("EHLO " + hostTestEhloName + "\r\n"))
	expect("250-" + hostTestBannerHostName)
	expect("250-ENHANCEDSTATUSCODES")
	expect("250 STARTTLS")
	quietAt := self.clock.Now().Add(self.settings.StartTlsQuietTimeout)
	clientConn.Write([]byte("STARTTLS\r\n"))
	awaitHostTestTimerAt(self.t, self.ctx, self.clock, quietAt)
	self.clock.Advance(self.settings.StartTlsQuietTimeout)
	expect("220 2.0.0 Ready to start TLS")
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(self.chainPem)
	start := sidecartest.DefaultClockStart()
	senderTls := tls.Client(&routerTestBufferedConn{Conn: clientConn, reader: reader}, &tls.Config{ServerName: routerTestSni, RootCAs: roots, Time: func() time.Time { return start }})
	handshake := make(chan error, 1)
	go func() {
		handshake <- senderTls.Handshake()
	}()
	return clientConn, handshake, senderTls
}

// The next hint by sidecar the block's writer finished, which must have gone through.
func (self *routerWakeTest) awaitHint() {
	self.t.Helper()
	select {
	case err := <-self.push.hinted:
		if err != nil {
			self.t.Errorf("the hold's hint: %v", err)
		}
	case <-self.ctx.Done():
		self.t.Fatal("no hint")
	}
}

// The next dial the engine began.
func (self *routerWakeTest) nextDial() {
	self.t.Helper()
	select {
	case <-self.dialing():
	case <-self.ctx.Done():
		self.t.Fatal("no dial")
	}
}

func (self *routerWakeTest) dialing() chan struct{} {
	return self.dialer.dialing
}

// A held session reaches no PostgreSQL (I-12): the miss, the hint through the block's writer and its enqueue, the
// wait, and the 2 s retry that takes the receiver and splices a real TLS handshake through, every acquisition in the
// process refused. The hint reached the receiver's client: its {scp_} stream holds the forced mail hint.
func TestRouterWakeHoldReachesNoPostgres(t *testing.T) {
	world := newRouterWakeTest(t, 1)
	world.giveToken(pushservice.WakePlatformFcm)
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()

	clientConn, handshake, senderTls := world.send()
	world.nextDial()
	missTime := world.clock.Now()
	// held: the retry's wait is armed, and the hint went out beside it
	awaitHostTestTimerAt(t, world.ctx, world.clock, missTime.Add(world.settings.WakeHoldFindInterval))
	world.awaitHint()
	world.clock.Advance(world.settings.WakeHoldFindInterval)
	world.nextDial()
	if err := <-handshake; err != nil {
		t.Fatalf("the TLS handshake through the held splice failed: %v", err)
	}
	if _, err := senderTls.Write([]byte("MAIL FROM:<a@sender.example>\n")); err != nil {
		t.Fatal(err)
	}
	if received := <-world.dialer.received; string(received) != "MAIL FROM:<a@sender.example>\n" {
		t.Fatalf("the receiver read %q", received)
	}
	if echo, err := io.ReadAll(senderTls); err != nil || string(echo) != "250 echoed MAIL FROM:<a@sender.example>\n" {
		t.Fatalf("the sender read %q, %v", echo, err)
	}
	clientConn.Close()
	world.engine.Drain()
	stop()

	if count := attempts(); count != 0 {
		t.Errorf("the held path reached PostgreSQL %d times (I-12)", count)
	}
	entries, err := world.fakeHost.Redis.StreamRange(sidecartest.InspectContext(world.ctx), pushservice.HintStreamKey(world.clientId), "", 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || string(entries[0].FieldValues["topic"]) != sidecar.PushTopicMail {
		t.Errorf("the receiver's hint stream holds %+v", entries)
	}
}

// FS-36's forced router miss: the receiver's client holds a token and its resident is live, which would put an
// ordinary mail hint's wake off 15 s; the router's hint is forced, so its wake is due now and the push sender submits
// one at once, at the miss's own instant, with the hold still waiting.
func TestRouterForcedMissWakesAtOnce(t *testing.T) {
	world := newRouterWakeTest(t, 16)
	token := world.giveToken(pushservice.WakePlatformFcm)
	senderSettings := pushservice.DefaultWakeSenderSettings()
	senderSettings.ReadBlock = 100 * time.Hour
	senderSettings.ClaimInterval = 100 * time.Hour
	senderSettings.GateInterval = 100 * time.Hour
	for shard := range senderSettings.Queue.ShardCount {
		if err := world.fakeHost.Redis.StreamGroupCreate(world.ctx, pushservice.WakeQueueKey(shard), senderSettings.ConsumerGroup); err != nil {
			t.Fatal(err)
		}
	}
	vendor := sidecartest.NewFakeWakeVendor(pushservice.WakePlatformFcm, world.clock)
	sender, err := pushservice.NewWakeSender(world.ctx, &pushservice.WakeSenderArgs{
		Redis:    world.fakeHost.Redis,
		Vendors:  []pushservice.WakeVendor{vendor},
		Consumer: "router-wake-test",
		Enabled:  func() bool { return true },
		Clock:    world.clock,
		Settings: senderSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	world.send()
	world.nextDial()
	missTime := world.clock.Now()
	awaitHostTestTimerAt(t, world.ctx, world.clock, missTime.Add(world.settings.WakeHoldFindInterval))
	world.awaitHint()
	var submission *sidecartest.FakeWakeSubmission
	select {
	case submission = <-vendor.Submitted():
	case <-time.After(hostTestStepTimeout):
		t.Fatal("no wake was submitted for the forced miss")
	}
	if submission.Token != token || !submission.At.Equal(missTime) || submission.Payload.TopicClass != sidecar.PushTopicMail {
		t.Errorf("submission %+v, want one to the receiver's token at the miss", submission)
	}
	if submissions := vendor.Submissions(); len(submissions) != 1 {
		t.Errorf("%d submissions", len(submissions))
	}
	// the hold still waits for its retry, the wake having cost it nothing
	pending := world.clock.PendingTimers()
	if !slices.ContainsFunc(pending, missTime.Add(world.settings.WakeHoldFindInterval).Equal) {
		t.Errorf("the hold's retry is no longer armed: %v", pending)
	}
}

// The block builds its own hint writer when no test gives one, so a hold's and a direct MX's hints go out.
func TestRouterHostGivesTheEngineAHintWriter(t *testing.T) {
	settings := DefaultHostSettings()
	settings.MxEdge = routerTestSettings()
	fakeHost := sidecartest.NewFakeHost(t.Context(), sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	routerHost, err := NewRouterHost(t.Context(), settings, nil, &RouterDeps{Redis: fakeHost.Redis, Gates: fakeHost.Gates})
	if err != nil {
		t.Fatal(err)
	}
	defer routerHost.Close()
	if _, ok := routerHost.host.Push.(*pushservice.HintWriter); !ok {
		t.Errorf("the block's push is %T", routerHost.host.Push)
	}
}
