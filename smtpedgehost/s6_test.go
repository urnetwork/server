//go:build sidecar_pending && sidecar_testhooks

package smtpedgehost

// S6, FS-123 and §14's privacy row at T3 (SMTP-M9-31; §22.7, FS-123, D-SMTP-20, R-SMTP-7, R-SMTP-10, I-2, I-12, I-13):
// the rows T2 runs on the stub edge, sidecartest's smtp_send_rows.go, run here unchanged on the block's engine as main
// composes it (NewBlockHost) over the private stack's Postgres, the test's own database, and its Redis, through
// .stack/server-test.sh with -tags sidecar_pending,sidecar_testhooks:
//
//   - the store is model.NewSmtpEdgeStore: each connection's send record reaches smtp_send through the send log, and
//     the hold a row asks for is a halt in smtp_halt, which the engine's next reload holds in memory and {smq_};
//   - the quota is the {smq_} scripts on the stack's Redis;
//   - the gate is the block's own (gate.go) over model.GetSmtpGateFacts: the network's label, mail_network's enable
//     time and its zone's report token, which the issued Feedback-ID carries, for a root, a network and a zone made as
//     the dns block makes them;
//   - each of the member's devices sends with the root's own SmtpSender, its parts and SHTS encoded by the root's
//     smtpProofChannel (sidecar.NewSmtpProofTestChannel), its TLS FakeSmtpProver's crypto/tls client.
//
// What stands in, each where it is built:
//
//   - the stream: no sockets layer runs here (the ur/smtpedge listener needs platform clients on an exchange, whose T3
//     is SOCK-M5-17's), so each stream reaches the engine through smtpedgeservice.SmtpEdgeTestListener on a ticket
//     naming the member's network, the device's client and its sidecar, as sockets' Find fills one, and each part's
//     and SHTS's call lands on the engine's handler as a client's dispatcher hands one over;
//   - mail_send: the block's gate reads no projection yet (plan-SOCK-gate-data's route), so the test answers it for the
//     member's admitted devices;
//   - Quota, sidecar.yml's service gates, §21.7's standing and the metrics: sidecartest's, as tlsedge's T3 has them;
//   - the world: sidecartest's MxWorld for DNSSEC, the MXs and the sending address's FCrDNS, and FakeSmtpProofVerifier,
//     since no setup has run (SMTP-M9-27).
//
// After each row the block closes, which writes its send log, and smtp_send holds one record for each preamble the
// engine answered OK, which is each proof session it opened, with the domain and recipients that preamble declared.
// The privacy row's capture reads, beside the sender's logs and the verifier's sessions, every byte of each stream
// either way, every call, answer and outcome the engine took, the block's log lines and metrics, and every key of the
// test's Redis and row of the sidecar tables (sidecart3.CaptureStoreRows). T6 repeats S6 and is external.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecart3"
)

// A guard on what a test waits for, never a pause: each wait ends at the event it waits on.
const t3SendGuardTimeout = 60 * time.Second

var (
	// The block's one sending address, RFC 5737's.
	t3SendSendingAddress = netip.MustParseAddr("192.0.2.10")
	// The documentation prefixes a sending host refuses to dial, where the rows' MXs are.
	t3SendDocumentationPrefixes = []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
	}
	// A call to a client the stream's ticket does not name as its listener.
	errT3SendCallClient = errors.New("a call to a client the ticket does not name")
)

// Pro's smtp_* numbers (§20.4), which Quota.Limits answers for the world's network.
func t3SendLimits() *sidecar.QuotaLimits {
	return &sidecar.QuotaLimits{Tier: "pro", Limits: map[string]int64{
		smtpedgeservice.SmtpLimitConnectionsPerDay:             200,
		smtpedgeservice.SmtpLimitBytesPerDay:                   1024 * 1024 * 1024,
		smtpedgeservice.SmtpLimitRecipientsPerMessage:          50,
		smtpedgeservice.SmtpLimitRecipientsPerDay:              1000,
		smtpedgeservice.SmtpLimitMessageBytes:                  25 * 1024 * 1024,
		smtpedgeservice.SmtpLimitBurst:                         50,
		smtpedgeservice.SmtpLimitBurstWindow:                   (10 * time.Minute).Milliseconds(),
		smtpedgeservice.SmtpLimitConcurrentConnections:         4,
		smtpedgeservice.SmtpLimitCommunityConnectionsPerHour:   500,
		smtpedgeservice.SmtpLimitCommunityMedianMultiple:       3,
		smtpedgeservice.SmtpLimitDestinationConnectionsPerHour: 200,
		smtpedgeservice.SmtpLimitDestinationConcurrency:        10,
	}}
}

// A root of the test's own under the documentation suffix, and a network in it with its zone, label and report token
// made as the dns block makes them, its mail enabled by POST /mail/enable's write and dated two days before start,
// past the gate's MailEnableDelay.
func newT3SendNetwork(t testing.TB, ctx context.Context, start time.Time) (string, sidecar.Id, string) {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	root := "t" + hex.EncodeToString(suffix) + ".example"
	if err := model.EnsureDnsRoot(ctx, root+"."); err != nil {
		t.Fatal(err)
	}
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "smtp-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	if _, err := model.EnsureNetworkZone(ctx, networkId, root); err != nil {
		t.Fatal(err)
	}
	label, err := model.EnsureNetworkLabel(ctx, networkId, root)
	if err != nil {
		t.Fatal(err)
	}
	var enableErr error
	server.Tx(ctx, func(tx server.PgTx) {
		if _, _, enableErr = model.EnableMailInTx(ctx, tx, networkId); enableErr != nil {
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE mail_network SET create_time = $2 WHERE network_id = $1`, networkId, start.Add(-48*time.Hour)))
	})
	if enableErr != nil {
		t.Fatal(enableErr)
	}
	return root, sidecar.Id(networkId), label
}

// This fixture supplies mail_send for the member's admitted devices in a network with no Team
// scopes. The host gate loads its other facts from the private stack.
type t3SendGate struct {
	*hostGate
	networkId sidecar.Id
	member    func(sidecarId sidecar.Id) bool
}

// See smtpedgeservice.SmtpGate: allowed for a device of the member in the world's network, else no grant.
func (self *t3SendGate) SendFacts(networkId sidecar.Id, sidecarId sidecar.Id, now time.Time) (*smtpedgeservice.SmtpGateFacts, sidecar.AclDecision) {
	facts, _ := self.Facts(networkId)
	if networkId == self.networkId && self.member(sidecarId) {
		return facts, sidecar.AclDecision{Allowed: true, Reason: sidecar.AclReasonRule}
	}
	return facts, sidecar.AclDecision{Reason: sidecar.AclReasonNoGrant}
}

// The block a row's world sends through, and what the T3 checks read after the row.
type t3SendBlock struct {
	t      testing.TB
	ctx    context.Context
	cancel context.CancelFunc
	clock  *sidecartest.FakeClock
	start  time.Time
	fakes  *sidecartest.FakeHost
	host   *Host
	gate   *t3SendGate
	// the engine's stream and dispatcher, which this test stands in for
	listener  *smtpedgeservice.SmtpEdgeTestListener
	store     *model.SmtpEdgeStore
	settings  *smtpedgeservice.SmtpEdgeSettings
	networkId sidecar.Id
	// urnetwork-edge's network and the edge's client, which every ticket names as the listener's
	edgeNetworkId sidecar.Id
	edgeClientId  sidecar.Id
	world         *sidecartest.SmtpSendWorld
	closeOnce     sync.Once
	// the privacy row's capture, and what its scans of the stores read; nil for the other rows
	capture     *sidecartest.PrivacyCapture
	storeReport *sidecart3.CaptureStoresReport

	stateLock sync.Mutex
	dialers   []*t3SendDialer
	streams   []*t3SendStream
	// what the engine took beyond the streams, as CC1 items: each call's body and answer, each outcome
	exchanges []*sidecartest.PrivacyItem
}

// A row's world whose edge is the block's engine on the stack's Postgres and Redis, for the root and network
// newT3SendNetwork makes, the engine ready and its gate holding the network's facts. rowSettings are the row's.
func newT3SendBlock(t testing.TB, rowSettings *sidecartest.SmtpSendWorldSettings) *t3SendBlock {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now().UTC().Truncate(time.Second)
	clock := sidecartest.NewFakeClock(start)
	root, networkId, label := newT3SendNetwork(t, ctx, start)
	self := &t3SendBlock{
		t:             t,
		ctx:           ctx,
		cancel:        cancel,
		clock:         clock,
		start:         start,
		networkId:     networkId,
		edgeNetworkId: sidecar.NewId(),
		edgeClientId:  sidecar.NewId(),
	}

	settings := &sidecartest.SmtpSendWorldSettings{}
	if rowSettings != nil {
		*settings = *rowSettings
	}
	settings.Clock = clock
	settings.NetworkDomain = label + "." + root
	settings.Dialer = self.newDialer
	settings.Hold = self.hold
	settings.Settle = self.close
	self.world = sidecartest.NewSmtpSendWorld(t, settings)
	// the sending address and the A record and PTR its FCrDNS reads
	heloName := "o1.out." + root
	if err := self.world.World.AddMx(heloName, t3SendSendingAddress, nil); err != nil {
		t.Fatal(err)
	}
	self.world.World.SetPtr(t3SendSendingAddress, heloName)

	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(fmt.Sprintf(`
root: %s
report_host: rpt.%s
helo_domain: out.%s
sending_addresses:
  - {address: %s, helo: %s}
`, root, root, root, t3SendSendingAddress, heloName)), 0)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := "issue_key_id: 1\nkeys:\n  - {id: 1, key: " + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)) + "}\n"
	if err := model.ApplySmtpXUrRefKeyFile(config.Settings, []byte(keyFile)); err != nil {
		t.Fatal(err)
	}
	config.Settings.MxRefusedPrefixes = slices.DeleteFunc(slices.Clone(config.Settings.MxRefusedPrefixes), func(prefix netip.Prefix) bool {
		return slices.Contains(t3SendDocumentationPrefixes, prefix)
	})
	self.settings = config.Settings

	self.fakes = sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	self.fakes.Quota.SetLimits(connect.Id(networkId), t3SendLimits())
	// a balance the connections' leases draw on (§20.6)
	self.fakes.Quota.SetBalance(connect.Id(networkId), 1024*1024*1024)
	self.fakes.Gates.OpenService(sidecar.ServiceGateSmtpedge, connect.Id(networkId))
	self.fakes.Abuse.Set(connect.Id(networkId), sidecar.AbuseStandingStandard, true)

	HostSettings := DefaultHostSettings(config)
	self.gate = &t3SendGate{
		hostGate:  newHostGate(ctx, &modelGateSource{root: root, quota: self.fakes.Quota}, clock, HostSettings.Gate),
		networkId: networkId,
		member:    func(sidecarId sidecar.Id) bool { return self.world.MemberDeviceKey(sidecarId) != nil },
	}
	// the gate's memory as a first preamble's miss fills it, by the gate's own loads, run here before the row's first
	// preamble: the model's facts for the network, and §21.5's breaker
	self.gate.load(networkId, clock.Now())
	self.gate.loadBreaker()
	facts, ok := self.gate.Facts(networkId)
	if !ok || facts.Label != label || len(facts.ReportToken) != 26 || !facts.MailEnableTime.Equal(start.Add(-48*time.Hour)) ||
		facts.Limits == nil || facts.Limits.Tier != "pro" {
		t.Fatalf("the gate loaded %+v, %t", facts, ok)
	}
	if _, ok := self.gate.Breaker(); !ok {
		t.Fatal("the gate loaded no breaker state")
	}

	var log sidecar.Logger = &testLog{}
	if settings.Capture != nil {
		log = settings.Capture.Logger("smtpedge block")
	}
	self.store = model.NewSmtpEdgeStore(nil)
	self.host, err = NewBlockHost(ctx, HostSettings, &HostDeps{
		Quota:    self.fakes.Quota,
		Abuse:    self.fakes.Abuse,
		Store:    self.store,
		Gate:     self.gate,
		Resolver: self.world.World,
		Ptr:      self.world.World,
		Dialer:   self.world.World,
		Verifier: self.world.Verifier,
		Gates:    self.fakes.Gates,
		Redis:    sidecarhost.NewRedis(nil),
		Metrics:  self.fakes.Metrics,
		Clock:    clock,
		Log:      log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(self.close)
	self.listener = smtpedgeservice.NewSmtpEdgeTestListener(self.host.engine)
	guard, done := context.WithTimeout(ctx, t3SendGuardTimeout)
	defer done()
	if err := self.listener.AwaitStarted(guard); err != nil {
		t.Fatalf("the engine did not start: %v", err)
	}
	if !self.host.engine.Ready() {
		t.Fatal("the engine is not ready")
	}
	// only what this test stands in for is missing
	if missing := self.host.Missing(); len(missing) != 1 || !strings.Contains(missing[0], "platform client") {
		t.Fatalf("the block is missing %q", missing)
	}

	if capture := settings.Capture; capture != nil {
		capture.AddSource(self.privacyItems)
		capture.AddSource(self.world.VerifierPrivacyItems)
		capture.AddSource(func() []*sidecartest.PrivacyItem {
			items := []*sidecartest.PrivacyItem{}
			for _, line := range self.fakes.Metrics.Dump() {
				items = append(items, &sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassMetric, Name: "smtpedge metric", Body: []byte(line)})
			}
			return items
		})
		storeSettings := sidecart3.DefaultCaptureStoresSettings()
		storeSettings.SidecarColumnsPath = "../local/sidecar-columns.yml"
		self.storeReport, err = sidecart3.CaptureStoreRows(ctx, capture, storeSettings)
		if err != nil {
			t.Fatal(err)
		}
		self.capture = capture
	}
	return self
}

// After the privacy row: a scan of the stores reads the test's Redis and the sidecar tables, smtp_send among them,
// failing to read nothing and finding nothing there, and a canary planted in a Redis key is found, so the clean scan
// read what it says it read.
func (self *t3SendBlock) checkStoreCapture() {
	self.t.Helper()
	if self.capture == nil {
		return
	}
	stores := func(findings []*sidecartest.PrivacyFinding) []*sidecartest.PrivacyFinding {
		return slices.DeleteFunc(findings, func(finding *sidecartest.PrivacyFinding) bool {
			return finding.Class.Point() != "CC3"
		})
	}
	if findings := stores(self.capture.Scan()); 0 < len(findings) {
		self.t.Errorf("the stores hold %v", findings)
	}
	_, redisKeyCount, rowCount := self.storeReport.Counts()
	if redisKeyCount == 0 || rowCount == 0 || 0 < len(self.storeReport.Errors()) || slices.Contains(self.storeReport.TablesNotRead(), "smtp_send") {
		self.t.Fatalf("the scan read %d Redis keys and %d rows, failed %v, and did not read %q",
			redisKeyCount, rowCount, self.storeReport.Errors(), self.storeReport.TablesNotRead())
	}
	canary := self.capture.Canary("planted in redis")
	if err := sidecarhost.NewRedis(nil).Set(context.Background(), "{t3_send_canary}", canary, time.Hour); err != nil {
		self.t.Fatal(err)
	}
	if !slices.ContainsFunc(stores(self.capture.Scan()), func(finding *sidecartest.PrivacyFinding) bool {
		return finding.Class == sidecartest.PrivacyClassRedis
	}) {
		self.t.Error("the scan missed a canary planted in Redis")
	}
}

// The hold a row asks for, as §14.8's ladder sets one: a halt on the network written to smtp_halt through the block's
// store, starting at the engine's next reload, HaltReloadInterval on, which holds it in memory and {smq_}. Every
// preamble from then answers HALTED for the halt's whole SmtpHaltDuration, whatever wait the row asked for.
func (self *t3SendBlock) hold(wait time.Duration) time.Duration {
	self.t.Helper()
	count := self.listener.ProbeCount()
	reload := self.clock.Now().Add(self.settings.HaltReloadInterval)
	halt := &smtpedgeservice.SmtpHalt{
		HaltId:    sidecar.NewId(),
		NetworkId: self.networkId,
		Trigger:   smtpedgeservice.SmtpHaltTriggerFraud,
		StartTime: reload,
	}
	if err := self.store.SetHalt(self.ctx, halt); err != nil {
		self.t.Fatal(err)
	}
	self.clock.Advance(self.settings.HaltReloadInterval)
	guard, done := context.WithTimeout(self.ctx, t3SendGuardTimeout)
	defer done()
	if err := self.listener.AwaitProbes(guard, count+1); err != nil {
		self.t.Fatalf("no reload after the halt: %v", err)
	}
	return smtpedgeservice.SmtpHaltDuration
}

// Ends the block, which writes its send log, its gate and every stream it served. Safe more than once.
func (self *t3SendBlock) close() {
	self.closeOnce.Do(func() {
		if self.host != nil {
			self.host.Close()
		}
		self.cancel()
		if self.gate != nil {
			self.gate.wait()
		}
		for _, dialer := range self.dialerList() {
			dialer.serving.Wait()
		}
	})
}

// Every dialer the world's hosts were given.
func (self *t3SendBlock) dialerList() []*t3SendDialer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.dialers)
}

// After a row: the block closed, smtp_send holds one record for each proof session the engine opened, every preamble
// it answered OK, each the network's and naming that preamble's domain, with no more proven recipients than the
// preambles to that domain declared (smtp-3 keeps the proven count: none for a send that stopped before RCPT).
func (self *t3SendBlock) checkSendLog() {
	self.t.Helper()
	self.close()
	sends, err := self.store.FindSends(context.Background(), &smtpedgeservice.SmtpJoinKey{
		NetworkId: self.networkId,
		Since:     self.start.Add(-time.Hour),
		Until:     self.clock.Now().Add(time.Hour),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	declaredDomains := []string{}
	domainDeclared := map[string]uint64{}
	for _, session := range self.world.Verifier.Sessions() {
		preamble := session.Preamble()
		declaredDomains = append(declaredDomains, preamble.RecipientDomain)
		domainDeclared[preamble.RecipientDomain] += uint64(preamble.RecipientCount)
	}
	storedDomains := []string{}
	domainProven := map[string]uint64{}
	for _, send := range sends {
		if send.NetworkId != self.networkId || send.MxHost == "" || send.Result == "" || len(send.MessageIdHash) == 0 {
			self.t.Errorf("a send record %+v", send)
		}
		storedDomains = append(storedDomains, send.RecipientDomain)
		domainProven[send.RecipientDomain] += uint64(send.RecipientCount)
	}
	slices.Sort(declaredDomains)
	slices.Sort(storedDomains)
	if len(declaredDomains) == 0 || !slices.Equal(declaredDomains, storedDomains) {
		self.t.Errorf("smtp_send holds sends to %q for preambles to %q", storedDomains, declaredDomains)
	}
	for domain, proven := range domainProven {
		if domainDeclared[domain] < proven {
			self.t.Errorf("smtp_send holds %d proven recipients at %s, past the %d declared", proven, domain, domainDeclared[domain])
		}
	}
}

// What the engine read and answered, as CC1 items: each stream's bytes either way, each call and answer, each outcome.
func (self *t3SendBlock) privacyItems() []*sidecartest.PrivacyItem {
	streams, items := func() ([]*t3SendStream, []*sidecartest.PrivacyItem) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return slices.Clone(self.streams), slices.Clone(self.exchanges)
	}()
	for _, stream := range streams {
		sent, received := stream.kept()
		items = append(items,
			&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassHandler, Name: "smtpedge stream from the device", Body: sent},
			&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassHandler, Name: "smtpedge stream to the device", Body: received},
		)
	}
	return items
}

// Keeps a copy of body, something the engine took, under name.
func (self *t3SendBlock) addExchange(name string, body []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.exchanges = append(self.exchanges, &sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassHandler, Name: name, Body: bytes.Clone(body)})
}

// The dialer of the member's device sidecarId.
func (self *t3SendBlock) newDialer(sidecarId sidecar.Id) sidecar.SmtpEdgeDialer {
	dialer := &t3SendDialer{block: self, sidecarId: sidecarId, clientId: sidecar.NewId()}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dialers = append(self.dialers, dialer)
	return dialer
}

// One device's dialer onto the block's engine: each stream handed over as the ur/smtpedge listener hands one admitted on
// a ticket naming the member's network, this device's client and its sidecar, which sockets fills at Find from the
// caller's token (QD-989), and each of its calls landing on the engine's handler from that client.
type t3SendDialer struct {
	block     *t3SendBlock
	sidecarId sidecar.Id
	clientId  sidecar.Id
	serving   sync.WaitGroup
}

var _ sidecar.SmtpEdgeDialer = (*t3SendDialer)(nil)
var _ sidecar.SmtpProofTestCaller = (*t3SendDialer)(nil)

// See sidecar.SmtpEdgeDialer.
func (self *t3SendDialer) DialSmtpEdge(ctx context.Context) (sidecar.SmtpEdgeStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ticketId := make([]byte, 16)
	if _, err := rand.Read(ticketId); err != nil {
		return nil, err
	}
	now := self.block.clock.Now()
	ticket := &protocol.SocketTicket{
		TicketId:              ticketId,
		NetworkId:             self.block.edgeNetworkId.Bytes(),
		Name:                  smtpedgeservice.SmtpEdgeSocketName,
		ListenerClientId:      self.block.edgeClientId.Bytes(),
		DialerClientId:        self.clientId.Bytes(),
		DialerNetworkId:       self.block.networkId.Bytes(),
		DialerSidecarId:       self.sidecarId.Bytes(),
		IssueTimeMillis:       uint64(now.UnixMilli()),
		ExpireTimeMillis:      uint64(now.Add(30 * time.Second).UnixMilli()),
		GrantExpireTimeMillis: uint64(now.Add(time.Hour).UnixMilli()),
	}
	sidecarEnd, edgeEnd := sidecartest.NewMxPipe(&net.TCPAddr{}, &net.TCPAddr{})
	stream := &t3SendStream{Conn: sidecarEnd, dialer: self}
	func() {
		self.block.stateLock.Lock()
		defer self.block.stateLock.Unlock()
		self.block.streams = append(self.block.streams, stream)
	}()
	self.serving.Add(1)
	go func() {
		defer self.serving.Done()
		self.block.listener.Deliver(edgeEnd, ticket)
	}()
	return stream, nil
}

// See sidecar.SmtpProofTestCaller: a call to the ticket's listener lands on the engine's handler for its type from this
// device's client, as the client's dispatcher hands one over; a call to any other client is refused.
func (self *t3SendDialer) Call(ctx context.Context, clientId sidecar.Id, messageType protocol.MessageType, body []byte, options *sidecar.CallOptions) ([]byte, error) {
	if clientId != self.block.edgeClientId {
		return nil, errT3SendCallClient
	}
	self.block.addExchange("smtpedge call "+messageType.String(), body)
	answer, err := self.block.listener.Call(ctx, self.clientId, messageType, body)
	if err == nil {
		self.block.addExchange("smtpedge answer "+messageType.String(), answer)
	}
	return answer, err
}

// The device's end of one stream, every byte either way kept for the privacy capture.
type t3SendStream struct {
	net.Conn
	dialer *t3SendDialer

	stateLock sync.Mutex
	// what the device wrote, which the engine read, and what it read, which the engine wrote
	sent     bytes.Buffer
	received bytes.Buffer
}

// The stream's Write, what it wrote kept.
func (self *t3SendStream) Write(b []byte) (int, error) {
	n, err := self.Conn.Write(b)
	if 0 < n {
		self.keep(&self.sent, b[:n])
	}
	return n, err
}

// The stream's Read, what it read kept.
func (self *t3SendStream) Read(b []byte) (int, error) {
	n, err := self.Conn.Read(b)
	if 0 < n {
		self.keep(&self.received, b[:n])
	}
	return n, err
}

// Appends b to buffer under the stream's lock.
func (self *t3SendStream) keep(buffer *bytes.Buffer, b []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	buffer.Write(b)
}

// Copies of what was written and read.
func (self *t3SendStream) kept() ([]byte, []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return bytes.Clone(self.sent.Bytes()), bytes.Clone(self.received.Bytes())
}

// See sidecar.SmtpEdgeStream: the root's own channel, its calls to the ticket's listener through the dialer.
func (self *t3SendStream) ProofChannel(sendId []byte) sidecar.SmtpProofChannel {
	return sidecar.NewSmtpProofTestChannel(self.dialer, self.dialer.block.edgeClientId, sendId, self.dialer.block.world.Settings)
}

// See sidecar.SmtpEdgeStream: the self-report reaches the engine's AcceptOutcome from this device's client, as the
// edge's dispatcher would hand it a NO_REPLY envelope.
func (self *t3SendStream) SendOutcome(ctx context.Context, outcome *protocol.SmtpOutcome) error {
	body, err := proto.Marshal(outcome)
	if err != nil {
		return err
	}
	self.dialer.block.addExchange("smtpedge outcome", body)
	return self.dialer.block.host.engine.AcceptOutcome(self.dialer.clientId, body)
}

// See sidecar.SmtpEdgeStream.
func (self *t3SendStream) Abort() error {
	return self.Conn.Close()
}

// Runs row in a TestEnv of its own on a world newT3SendBlock makes, then checks the send log the block wrote.
func t3SendRun(t *testing.T, row func(t testing.TB, newWorld sidecartest.SmtpSendWorldMaker)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var block *t3SendBlock
		defer func() {
			if block != nil {
				block.close()
			}
		}()
		row(t, func(t testing.TB, settings *sidecartest.SmtpSendWorldSettings) *sidecartest.SmtpSendWorld {
			if block != nil {
				t.Fatal("a row made a second world")
			}
			block = newT3SendBlock(t, settings)
			return block.world
		})
		if block == nil {
			t.Fatal("the row made no world")
		}
		block.checkSendLog()
		block.checkStoreCapture()
	})
}

// Each row below is sidecartest's, run on the block; smtp_send_rows.go says what it holds.

func TestT3S6NoRecordReachesTheMxBeforeItsPart(t *testing.T) {
	t3SendRun(t, sidecartest.RunS6NoRecordReachesTheMxBeforeItsPart)
}

func TestT3S6NoUnauthenticatedMx(t *testing.T) {
	t3SendRun(t, sidecartest.RunS6NoUnauthenticatedMx)
}

func TestT3S6TooLargeForTheNonChunkingDomainAlone(t *testing.T) {
	t3SendRun(t, sidecartest.RunS6TooLargeForTheNonChunkingDomainAlone)
}

func TestT3Fs123SentFromADesktop(t *testing.T) {
	t3SendRun(t, sidecartest.RunFs123SentFromADesktop)
}

func TestT3Fs123QueuedFromAPhone(t *testing.T) {
	t3SendRun(t, sidecartest.RunFs123QueuedFromAPhone)
}

func TestT3Fs123OtherOutcomesReachTheResult(t *testing.T) {
	t3SendRun(t, sidecartest.RunFs123OtherOutcomesReachTheResult)
}

func TestT3SmtpSendCarriesNoCanaryPastTheDevice(t *testing.T) {
	t3SendRun(t, sidecartest.RunSmtpSendCarriesNoCanaryPastTheDevice)
}
