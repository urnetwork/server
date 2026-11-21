//go:build sidecar_pending && sidecar_testhooks

package quotahost

// QUOTA-M9-01's smtp meter on T3 (§20.6, §20.7, §14.4, §14.7, D-QUOTA-4, D-QUOTA-7, C9, X11; FI-13 (a)): smtpedge's
// engine, with the lease SMTP-M9-13 gave it and the {smq_} scripts SMTP-M9-11 gave it, takes a member's sends on this
// package's Quota over the private stack's Postgres and Redis. The sending network's connections share one
// Lease(network, QuotaMeterSmtp) in the process, every TLS record either way spent before it moves, and the engine's
// usage batcher sends its 60 s window under producer smtpedge; ApplyUsageBatch journals the lease's carve, and the
// debit flush takes it from the balance and adds it to network_service_usage under meter smtp. FI-13 (a)'s oracle
// reads the balances and every meter's count after each step. The bytes the lease paid for are the ones the
// connection's send record and {smq_}'s bytes of the day observed at its close. The Service contract (C9) the
// sidecar's stream to the edge rides escrows and settles nothing, so the lease is the one payer (D-QUOTA-4).
//
// The smtp_* keys (QUOTA-M1-01's, §20.4's numbers, which the T3 target's SetLimits gives as pro.yml would) reach the
// edge through Quota.Limits, which the send gate's memory holds as SMTP-M9-20's production gate refreshes it, off every
// preamble's path, and each preamble's {smq_} script applies what the gate holds then: a downgrade between two
// preambles applies at the second (§20.7's downgrade column). A QUOTA preamble refusal and a spent balance are
// separate answers: the first is a limit, answered before any lease is asked, with the network's balance untouched;
// the second is the lease's, after the script reserved, and the reservation is released so the day's connections,
// recipients and bytes stay as they were, the preamble's burst token being the one thing it took (§14.7, §14.9).
//
// What is not server's is a fake: the gate's memory (FakeSmtpGate, its limits Quota.Limits' answer), the store
// (FakeSmtpEdgeStore), the abuse sink, the standing, sidecar.yml's gates and the metrics, and the world, sidecartest's
// SmtpSendWorld: an MxWorld whose ScriptedMx takes the mail, the member's prover host with FakeSmtpProver and the
// root's own SmtpSender, and FakeSmtpProofVerifier, scripted to report MAIL forwarded where smtpproof's session would.
// No sockets layer runs here, so each stream reaches the engine through smtpedgeservice.SmtpEdgeTestListener on a
// ticket naming the network, the device's client and its sidecar, as sockets' Find fills one, as server/cli/smtpedge's
// s6_test.go reaches it. The {smq_} scripts run on the stack's Redis, which counts UTC days by its own clock;
// everything else runs on the T3 target's fake clock, moved first to the wall clock's second. The recording Quota is
// tls_meter_t3_test.go's, and the meter's oracle source and journal reads mx_meter_t3_test.go's.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The synthetic names and documentation addresses the tests use (connect/CODESTYLE.md's test data rule). The
// network's zone is sidecartest.SmtpSendWorldNetworkDomain, n1.root.example.
const (
	smtpMeterRoot            = "root.example"
	smtpMeterLabel           = "n1"
	smtpMeterHeloName        = "o1.out.root.example"
	smtpMeterRecipientDomain = "mail.example"
	smtpMeterMxHost          = "mx.mail.example"
)

var (
	// A report_token's form (plan-SMTP-net-token): 16 synthetic octets in lowercase unpadded base32, 26 characters,
	// whose canonical encoding the sender's signer holds Feedback-ID's to.
	smtpMeterReportToken = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(bytes.Repeat([]byte{0x5b}, 16))
	// The edge's one sending address and the recipient domain's MX, RFC 5737's.
	smtpMeterSendingAddress = netip.MustParseAddr("192.0.2.10")
	smtpMeterMxAddress      = netip.MustParseAddr("198.51.100.25")
	// The documentation prefixes an edge refuses to dial, where the world's MX is.
	smtpMeterDocumentationPrefixes = []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
	}
	// A call to a client the stream's ticket does not name as its listener.
	errSmtpMeterCallClient = errors.New("a call to a client the ticket does not name")
)

// Pro's smtp_* keys and the sidecar_abuse: ones the edge reads, §20.4's numbers as pro.yml's blocks give them. The
// T3 target's SetLimits stands for pro.yml, which the private stack's config does not carry (model's pro.yml tests
// skip there too), so model.Pro() reads no tier block.
func smtpMeterProLimits() *sidecar.QuotaLimits {
	return &sidecar.QuotaLimits{Tier: string(model.NetworkTierPro), Limits: map[string]int64{
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
		smtpedgeservice.SmtpLimitHardBouncePercent:             10,
		smtpedgeservice.SmtpLimitHardBounceFloor:               10,
	}}
}

// Community's: 5 connections and 25 recipients a day, 5 recipients a message, 5 MiB a message and a day, a burst of
// 5 in 10 min and one at a time (§20.4), the sidecar_abuse: keys as on every tier (D-QUOTA-7).
func smtpMeterCommunityLimits() *sidecar.QuotaLimits {
	limits := smtpMeterProLimits()
	limits.Tier = string(model.NetworkTierFree)
	limits.Limits[smtpedgeservice.SmtpLimitConnectionsPerDay] = 5
	limits.Limits[smtpedgeservice.SmtpLimitBytesPerDay] = 5 * 1024 * 1024
	limits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage] = 5
	limits.Limits[smtpedgeservice.SmtpLimitRecipientsPerDay] = 25
	limits.Limits[smtpedgeservice.SmtpLimitMessageBytes] = 5 * 1024 * 1024
	limits.Limits[smtpedgeservice.SmtpLimitBurst] = 5
	limits.Limits[smtpedgeservice.SmtpLimitConcurrentConnections] = 1
	return limits
}

// The Quota smtpedge holds: tls_meter_t3_test.go's recording one, which also counts the leases Quota refused.
type smtpMeterQuota struct {
	*tlsMeterQuota
	refusedLeaseCount atomic.Int64
}

func (self *smtpMeterQuota) Lease(ctx context.Context, networkId connect.Id, meter string) (sidecar.QuotaLease, *sidecar.QuotaRefusal, error) {
	lease, refusal, err := self.tlsMeterQuota.Lease(ctx, networkId, meter)
	if refusal != nil {
		self.refusedLeaseCount.Add(1)
		self.changed()
	}
	return lease, refusal, err
}

// The send gate's memory as SMTP-M9-20's production gate fills it, off every preamble's path: the network's facts,
// their limits Quota.Limits' answer for it as of the last refresh, §21.5's breaker at rest, Community's limits for a
// restricted network, and mail_send for the member's admitted devices.
type smtpMeterGate struct {
	*sidecartest.FakeSmtpGate
	quota     sidecar.Quota
	networkId sidecar.Id
	facts     smtpedgeservice.SmtpGateFacts
	member    func(sidecarId sidecar.Id) bool
}

// See smtpedgeservice.SmtpGate: allowed for a device of the member in the world's network, else no grant.
func (self *smtpMeterGate) SendFacts(networkId sidecar.Id, sidecarId sidecar.Id, now time.Time) (*smtpedgeservice.SmtpGateFacts, sidecar.AclDecision) {
	facts, _ := self.Facts(networkId)
	if networkId == self.networkId && self.member(sidecarId) {
		return facts, sidecar.AclDecision{Allowed: true, Reason: sidecar.AclReasonRule}
	}
	return facts, sidecar.AclDecision{Reason: sidecar.AclReasonNoGrant}
}

// Reads the network's limits again, as the gate's refresh does every §20.7's 5 s.
func (self *smtpMeterGate) refresh(t *testing.T, ctx context.Context) {
	t.Helper()
	limits, err := self.quota.Limits(ctx, connect.Id(self.networkId))
	if err != nil {
		t.Fatal(err)
	}
	facts := self.facts
	facts.Limits = limits
	self.SetFacts(self.networkId, &facts)
}

// The world of one test: the T3 target, smtpedge's engine on it, and the member's world sending through it.
type smtpMeterWorld struct {
	ctx      context.Context
	target   *quotasuite.Target
	quota    *smtpMeterQuota
	host     *sidecartest.FakeHost
	gate     *smtpMeterGate
	store    *sidecartest.FakeSmtpEdgeStore
	settings *smtpedgeservice.SmtpEdgeSettings
	engine   *smtpedgeservice.SmtpEdgeService
	listener *smtpedgeservice.SmtpEdgeTestListener
	world    *sidecartest.SmtpSendWorld
	oracle   *sidecartest.QuotaOracle
	mx       *sidecartest.ScriptedMx
	sender   *sidecartest.SmtpSendHost
	// the sending network, and urnetwork-edge's network and client, which every ticket names as the listener's
	networkId     sidecar.Id
	edgeNetworkId sidecar.Id
	edgeClientId  sidecar.Id
	// when the engine's usage batcher opened its first window
	start time.Time

	stateLock sync.Mutex
	dialers   []*smtpMeterDialer
}

// A ready engine on a fresh T3 target for networkId, at limits, with a balance of balanceByteCount (none when zero),
// its gate's memory loaded, and the member's prover host sending through it to mail.example's MX.
func newSmtpMeterWorld(t *testing.T, ctx context.Context, networkId sidecar.Id, limits *sidecar.QuotaLimits, balanceByteCount int64) *smtpMeterWorld {
	t.Helper()
	engineCtx, cancel := context.WithCancel(ctx)
	target := newT3Target(ctx)(t)
	// the wall clock's second, as server's S6 T3 starts its clock: the sender and the edge set their read deadlines
	// on this clock, and the in-process pipes honor one already past in real time at once
	target.Clock.AdvanceTo(time.Now().UTC().Truncate(time.Second))
	self := &smtpMeterWorld{
		ctx:           ctx,
		target:        target,
		quota:         &smtpMeterQuota{tlsMeterQuota: newTlsMeterQuota(target.Quota)},
		host:          sidecartest.NewFakeHost(engineCtx, target.Clock, nil, nil, nil),
		store:         sidecartest.NewFakeSmtpEdgeStore(target.Clock, nil),
		oracle:        sidecartest.NewQuotaOracle(&edgeMeterOracleSource{t3OracleSource: t3OracleSource{ctx: ctx}}),
		networkId:     networkId,
		edgeNetworkId: sidecar.NewId(),
		edgeClientId:  sidecar.NewId(),
	}
	if err := target.SetLimits(connect.Id(networkId), limits); err != nil {
		t.Fatal(err)
	}
	self.setBalance(t, balanceByteCount)

	self.world = sidecartest.NewSmtpSendWorld(t, &sidecartest.SmtpSendWorldSettings{
		// the session reports MAIL forwarded at the client record that carries it, after the handshake's three and
		// EHLO, as smtpproof's does, which is the engine's cue to debit the connection's unit (§14.7)
		Verifier: &sidecartest.FakeSmtpProofScript{MailForwardedAtClientRecord: 5},
		Clock:    target.Clock,
		Dialer:   self.newDialer,
	})
	if err := self.world.World.AddMx(smtpMeterHeloName, smtpMeterSendingAddress, nil); err != nil {
		t.Fatal(err)
	}
	self.world.World.SetPtr(smtpMeterSendingAddress, smtpMeterHeloName)
	self.mx = self.world.AddDomain(smtpMeterRecipientDomain, smtpMeterMxHost, smtpMeterMxAddress, nil, true)

	communityLimits := smtpMeterCommunityLimits()
	self.gate = &smtpMeterGate{
		FakeSmtpGate: sidecartest.NewFakeSmtpGate(),
		quota:        target.Quota,
		networkId:    networkId,
		facts: smtpedgeservice.SmtpGateFacts{
			MailEnableTime: target.Clock.Now().Add(-48 * time.Hour),
			Label:          smtpMeterLabel,
			ReportToken:    smtpMeterReportToken,
		},
		member: func(sidecarId sidecar.Id) bool { return self.world.MemberDeviceKey(sidecarId) != nil },
	}
	self.gate.SetBreaker(&smtpedgeservice.SmtpBreakerState{})
	self.gate.SetCommunityLimits(communityLimits)
	self.gate.refresh(t, ctx)
	self.host.Abuse.Set(connect.Id(networkId), sidecar.AbuseStandingStandard, true)
	self.host.Gates.OpenService(sidecar.ServiceGateSmtpedge, connect.Id(networkId))

	settings := smtpedgeservice.DefaultSmtpEdgeSettings()
	settings.Root = smtpMeterRoot
	settings.ReportHostName = "rpt." + smtpMeterRoot
	settings.SendingAddresses = []smtpedgeservice.SmtpSendingAddress{{Address: smtpMeterSendingAddress, HeloName: smtpMeterHeloName}}
	settings.XUrRefKeys = []*smtpedgeservice.SmtpXUrRefKey{{KeyId: 1, Key: bytes.Repeat([]byte{0x5a}, 32)}}
	settings.MxRefusedPrefixes = slices.DeleteFunc(slices.Clone(settings.MxRefusedPrefixes), func(prefix netip.Prefix) bool {
		return slices.Contains(smtpMeterDocumentationPrefixes, prefix)
	})
	self.settings = settings

	serviceHost := self.host.ServiceHost()
	serviceHost.Quota = self.quota
	// the {smq_} scripts on the stack's Redis, as the block runs them
	serviceHost.Redis = sidecarhost.NewRedis(nil)
	self.start = target.Clock.Now()
	engine, err := smtpedgeservice.NewSmtpEdgeServiceWithDeps(engineCtx, serviceHost, self.store, self.world.Verifier, &smtpedgeservice.SmtpEdgeDeps{
		Gate:     self.gate,
		Resolver: self.world.World,
		Dialer:   self.world.World,
		Ptr:      self.world.World,
		Lookups:  self.store,
		Halts:    self.store,
		Sink:     sidecartest.NewFakeSmtpAbuseSink(),
	}, settings)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	self.engine = engine
	t.Cleanup(func() {
		engine.Close()
		cancel()
		for _, dialer := range self.dialerList() {
			dialer.serving.Wait()
		}
	})
	self.listener = smtpedgeservice.NewSmtpEdgeTestListener(engine)
	guard, done := context.WithTimeout(ctx, tlsMeterGuardTimeout)
	defer done()
	if err := self.listener.AwaitStarted(guard); err != nil {
		t.Fatalf("the engine did not start: %v", err)
	}
	if !engine.Ready() {
		t.Fatal("the engine is not ready")
	}
	self.sender = self.world.AddHost(sidecartest.SmtpSendWorldProver(1024*1024), nil, nil)
	return self
}

// Replaces the network's balances with one of balanceByteCount, none when zero.
func (self *smtpMeterWorld) setBalance(t *testing.T, balanceByteCount int64) {
	t.Helper()
	balances := []*sidecartest.QuotaBalance{}
	if 0 < balanceByteCount {
		balances = append(balances, &sidecartest.QuotaBalance{ByteCount: balanceByteCount})
	}
	if err := self.target.SetBalances(connect.Id(self.networkId), balances); err != nil {
		t.Fatal(err)
	}
	self.oracle.Grant(connect.Id(self.networkId), balanceByteCount)
}

// Every dialer the world's hosts were given.
func (self *smtpMeterWorld) dialerList() []*smtpMeterDialer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.dialers)
}

// The dialer of the member's device sidecarId.
func (self *smtpMeterWorld) newDialer(sidecarId sidecar.Id) sidecar.SmtpEdgeDialer {
	dialer := &smtpMeterDialer{world: self, sidecarId: sidecarId, clientId: sidecar.NewId()}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dialers = append(self.dialers, dialer)
	return dialer
}

// One device's dialer onto the engine: each stream handed over as the ur/smtpedge listener hands one admitted on a
// ticket naming the network, this device's client and its sidecar, which sockets fills at Find from the caller's
// token (QD-989), and each of its calls landing on the engine's handler from that client.
type smtpMeterDialer struct {
	world     *smtpMeterWorld
	sidecarId sidecar.Id
	clientId  sidecar.Id
	serving   sync.WaitGroup
}

var _ sidecar.SmtpEdgeDialer = (*smtpMeterDialer)(nil)
var _ sidecar.SmtpProofTestCaller = (*smtpMeterDialer)(nil)

// See sidecar.SmtpEdgeDialer.
func (self *smtpMeterDialer) DialSmtpEdge(ctx context.Context) (sidecar.SmtpEdgeStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ticketId := make([]byte, 16)
	if _, err := rand.Read(ticketId); err != nil {
		return nil, err
	}
	now := self.world.target.Clock.Now()
	ticket := &protocol.SocketTicket{
		TicketId:              ticketId,
		NetworkId:             self.world.edgeNetworkId.Bytes(),
		Name:                  smtpedgeservice.SmtpEdgeSocketName,
		ListenerClientId:      self.world.edgeClientId.Bytes(),
		DialerClientId:        self.clientId.Bytes(),
		DialerNetworkId:       self.world.networkId.Bytes(),
		DialerSidecarId:       self.sidecarId.Bytes(),
		IssueTimeMillis:       uint64(now.UnixMilli()),
		ExpireTimeMillis:      uint64(now.Add(30 * time.Second).UnixMilli()),
		GrantExpireTimeMillis: uint64(now.Add(time.Hour).UnixMilli()),
	}
	sidecarEnd, edgeEnd := sidecartest.NewMxPipe(&net.TCPAddr{}, &net.TCPAddr{})
	self.serving.Add(1)
	go func() {
		defer self.serving.Done()
		self.world.listener.Deliver(edgeEnd, ticket)
	}()
	return &smtpMeterStream{Conn: sidecarEnd, dialer: self}, nil
}

// See sidecar.SmtpProofTestCaller: a call to the ticket's listener lands on the engine's handler for its type from
// this device's client, as the client's dispatcher hands one over; a call to any other client is refused.
func (self *smtpMeterDialer) Call(ctx context.Context, clientId sidecar.Id, messageType protocol.MessageType, body []byte, options *sidecar.CallOptions) ([]byte, error) {
	if clientId != self.world.edgeClientId {
		return nil, errSmtpMeterCallClient
	}
	return self.world.listener.Call(ctx, self.clientId, messageType, body)
}

// The device's end of one stream.
type smtpMeterStream struct {
	net.Conn
	dialer *smtpMeterDialer
}

// See sidecar.SmtpEdgeStream: the root's own channel, its calls to the ticket's listener through the dialer.
func (self *smtpMeterStream) ProofChannel(sendId []byte) sidecar.SmtpProofChannel {
	return sidecar.NewSmtpProofTestChannel(self.dialer, self.dialer.world.edgeClientId, sendId, self.dialer.world.world.Settings)
}

// See sidecar.SmtpEdgeStream: the self-report reaches the engine's AcceptOutcome from this device's client.
func (self *smtpMeterStream) SendOutcome(ctx context.Context, outcome *protocol.SmtpOutcome) error {
	body, err := proto.Marshal(outcome)
	if err != nil {
		return err
	}
	return self.dialer.world.engine.AcceptOutcome(self.dialer.clientId, body)
}

// See sidecar.SmtpEdgeStream.
func (self *smtpMeterStream) Abort() error {
	return self.Conn.Close()
}

// Sends a message to recipientCount recipients at mail.example from the member's prover host, and answers the
// domain's outcome.
func (self *smtpMeterWorld) send(t *testing.T, recipientCount int, subject string) *sidecar.SmtpDomainOutcome {
	t.Helper()
	recipients := []string{}
	for i := range recipientCount {
		recipients = append(recipients, "r"+strconv.Itoa(i+1)+"@"+smtpMeterRecipientDomain)
	}
	message := self.world.Message(recipients, nil, "", subject, "A body line.\r\n")
	result, err := self.sender.Sender.Send(self.ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Domains) != 1 || result.Domains[0].Domain != smtpMeterRecipientDomain {
		t.Fatalf("the send's outcomes: %+v", result.Domains)
	}
	return result.Domains[0]
}

// The engine's own counts of the preambles and connections it answered, for a failure's message.
func (self *smtpMeterWorld) engineCounts() []string {
	lines := []string{}
	for _, line := range self.host.Metrics.Dump() {
		if strings.Contains(line, "smtpedge_preambles_total") || strings.Contains(line, "smtpedge_connections_total") {
			lines = append(lines, line)
		}
	}
	return lines
}

// What the network's {smq_} holds (quota_scripts.go): connections and recipients MAIL debited and bytes observed at
// close, summed over the UTC days it holds, the burst tokens taken over its windows, and its live members.
type smtpMeterCounts struct {
	connectionCount int64
	recipientCount  int64
	byteCount       int64
	burstCount      int64
	memberCount     int
}

func smtpMeterReadCounts(t *testing.T, ctx context.Context, networkId sidecar.Id) *smtpMeterCounts {
	t.Helper()
	counts := &smtpMeterCounts{}
	server.Redis(ctx, func(client server.RedisClient) {
		fieldValues, err := client.HGetAll(ctx, "{smq_"+networkId.String()+"}").Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for field, value := range fieldValues {
			if strings.HasPrefix(field, "cc:") {
				counts.memberCount += 1
				continue
			}
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				continue
			}
			switch {
			case strings.HasPrefix(field, "c:"):
				counts.connectionCount += number
			case strings.HasPrefix(field, "r:"):
				counts.recipientCount += number
			case strings.HasPrefix(field, "b:"):
				counts.byteCount += number
			case strings.HasPrefix(field, "t:"):
				counts.burstCount += number
			}
		}
	})
	return counts
}

func (self *smtpMeterWorld) check(t *testing.T) {
	t.Helper()
	if err := self.oracle.Check(self.ctx); err != nil {
		t.Fatal(err)
	}
}

func (self *smtpMeterWorld) checkSettled(t *testing.T) {
	t.Helper()
	if err := self.target.Settle(); err != nil {
		t.Fatal(err)
	}
	if err := self.oracle.CheckSettled(self.ctx); err != nil {
		t.Fatal(err)
	}
}

// Moves the clock to the end of the engine's first usage window and answers the batch api took for it.
func (self *smtpMeterWorld) sendFirstWindow(t *testing.T) *tlsMeterReport {
	t.Helper()
	self.target.Clock.AdvanceTo(self.start.Add(sidecar.DefaultQuotaUsageBatcherSettings().WindowTimeout))
	report := self.quota.awaitReport(t, 1)
	if report.err != nil {
		t.Fatalf("api refused the window's batch: %v", report.err)
	}
	if report.batch.Producer != sidecar.QuotaProducerSmtpEdge {
		t.Fatalf("a batch of producer %q", report.batch.Producer)
	}
	return report
}

// The engine's send records, once its close has written its send log.
func (self *smtpMeterWorld) closeSends() []*smtpedgeservice.SmtpSendRecord {
	self.engine.Close()
	return self.store.Sends()
}

// The money path: a send's records both ways spent on the network's one lease on meter smtp, the same bytes its send
// record and {smq_}'s bytes of the day observed at its close, sent at the window's end under producer smtpedge,
// journaled as the lease's carve, and debited once by the flush, which counts them in network_service_usage under
// meter smtp; the Service contract the stream rides escrows nothing and its settlement takes nothing.
func TestSmtpMeterT3MoneyPath(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		balanceByteCount := int64(4 * 1024 * 1024)
		networkId := sidecar.NewId()

		// the sending network and urnetwork-edge's, each with a client, for the Service contract
		payerNetworkId := server.Id(networkId)
		edgeNetworkId := server.NewId()
		payerClientId := server.NewId()
		edgeClientId := server.NewId()
		model.Testing_CreateNetwork(ctx, payerNetworkId, "smtp-meter-"+payerNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(ctx, payerNetworkId, server.NewId(), payerClientId, "synthetic", "synthetic")
		model.Testing_CreateNetwork(ctx, edgeNetworkId, "smtp-meter-edge-"+edgeNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(ctx, edgeNetworkId, server.NewId(), edgeClientId, "synthetic", "synthetic")

		world := newSmtpMeterWorld(t, ctx, networkId, smtpMeterProLimits(), balanceByteCount)
		// the sidecar's stream to the edge rides a Service contract, as CreateContract issues it for the pair Find
		// recorded (§6.10.1): no escrow on either network
		contractId, _, err := model.CreateContractNoEscrowWithExpiration(ctx, payerNetworkId, payerClientId, edgeNetworkId, edgeClientId, model.ByteCount(1024*1024), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []server.Id{payerNetworkId, edgeNetworkId} {
			if open := model.GetOpenTransferByteCount(ctx, id); open != 0 {
				t.Fatalf("a Service contract escrowed %d", open)
			}
		}

		if outcome := world.send(t, 2, "QUOTA-M9-01"); outcome.Outcome != sidecar.SmtpOutcomeSent {
			t.Fatalf("the send's outcome %+v; the engine counted %q", outcome, world.engineCounts())
		}
		if transactions := world.mx.Transactions(); len(transactions) != 1 {
			t.Fatalf("the MX took %d transactions", len(transactions))
		}
		world.quota.awaitLeasesClosed(t, connect.Id(networkId))
		if meters := world.quota.leaseMeters(connect.Id(networkId)); !slices.Equal(meters, []string{sidecar.QuotaMeterSmtp}) {
			t.Fatalf("the connection's leases: %v", meters)
		}
		// the quota closed the connection with its octets before its lease went back
		counts := smtpMeterReadCounts(t, ctx, networkId)
		if counts.connectionCount != 1 || counts.recipientCount != 2 || counts.byteCount <= 0 || counts.memberCount != 0 {
			t.Fatalf("{smq_} holds %+v", counts)
		}
		meteredByteCount := counts.byteCount
		world.oracle.Meter(connect.Id(networkId), sidecar.QuotaMeterSmtp, meteredByteCount)
		world.check(t)

		// the contract settles from both parties' reports of those bytes: nothing moves
		for _, clientId := range []server.Id{payerClientId, edgeClientId} {
			if err := model.CloseContract(ctx, contractId, clientId, model.ByteCount(meteredByteCount), false); err != nil {
				t.Fatal(err)
			}
		}
		if open := model.GetOpenTransferByteCount(ctx, payerNetworkId); open != 0 {
			t.Fatalf("the settled Service contract left %d open", open)
		}
		if balances := t3ReadBalances(ctx, payerNetworkId); len(balances) != 1 || balances[0].ByteCount != balanceByteCount {
			t.Fatalf("the Service contract's settlement moved the balance: %+v", balances)
		}
		world.check(t)

		// the window's batch: the lease's bytes, journaled as its carve
		report := world.sendFirstWindow(t)
		if byteCount, other := edgeMeterBatchByteCount(report.batch, connect.Id(networkId), sidecar.QuotaMeterSmtp); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d smtp bytes (other events %t), the connection moved %d", byteCount, other, meteredByteCount)
		}
		if producer := tlsMeterBatchProducer(ctx, report.batch.BatchId); producer != sidecar.QuotaProducerSmtpEdge {
			t.Fatalf("service_usage_batch holds the batch as %q", producer)
		}
		pendingByteCount, batchIds, _ := edgeMeterJournal(ctx, connect.Id(networkId), sidecar.QuotaMeterSmtp)
		if pendingByteCount != meteredByteCount {
			t.Fatalf("%d smtp bytes journaled, the connection moved %d", pendingByteCount, meteredByteCount)
		}
		for _, batchId := range batchIds {
			if batchId != server.Id(report.batch.BatchId) {
				t.Fatalf("a journal row of batch %s, not the window's", batchId)
			}
		}
		world.check(t)

		// the flush: one debit, counted on the day it applied
		world.checkSettled(t)
		day := world.target.Clock.Now().UTC().Format("2006-01-02")
		if days := edgeMeterUsageDays(ctx, connect.Id(networkId), sidecar.QuotaMeterSmtp); len(days) != 1 || days[day] != meteredByteCount {
			t.Fatalf("network_service_usage holds %v under smtp, want %d on %s", days, meteredByteCount, day)
		}
		if balances := t3ReadBalances(ctx, payerNetworkId); len(balances) != 1 || balances[0].ByteCount != balanceByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, balanceByteCount-meteredByteCount)
		}
		if _, _, rowCount := edgeMeterJournal(ctx, connect.Id(networkId), sidecar.QuotaMeterSmtp); rowCount != 0 {
			t.Fatalf("%d journal rows left after the flush", rowCount)
		}
		// the send log's record names the same bytes
		if sends := world.closeSends(); len(sends) != 1 || int64(sends[0].ByteCount) != meteredByteCount || sends[0].NetworkId != networkId {
			t.Fatalf("the send log holds %+v, the lease paid for %d", sends, meteredByteCount)
		}
	})
}

// §20.7's downgrade column for smtp_*: Pro's limits pass a preamble of 8 recipients; once the network's tier drops
// to Community's limits (pro.yml's free block's, which Quota.Limits answers and the gate's refresh holds), the next
// preamble of 8 recipients is refused QUOTA before any lease is asked, with nothing more counted, while one of 3
// recipients passes at Community's limits. What the two sends moved debits once.
func TestSmtpMeterT3DowngradeAppliesAtTheNextPreamble(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		balanceByteCount := int64(4 * 1024 * 1024)
		networkId := sidecar.NewId()
		proLimits := smtpMeterProLimits()
		communityLimits := smtpMeterCommunityLimits()
		wideRecipientCount := 8
		narrowRecipientCount := 3
		if !(int64(narrowRecipientCount) <= communityLimits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage] &&
			communityLimits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage] < int64(wideRecipientCount) &&
			int64(wideRecipientCount) <= proLimits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage]) {
			t.Fatalf("the recipients a message, %d on Community and %d on Pro, do not part 3 and 8",
				communityLimits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage], proLimits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage])
		}
		world := newSmtpMeterWorld(t, ctx, networkId, proLimits, balanceByteCount)

		if outcome := world.send(t, wideRecipientCount, "on Pro"); outcome.Outcome != sidecar.SmtpOutcomeSent {
			t.Fatalf("the send on Pro: %+v", outcome)
		}
		world.quota.awaitLeasesClosed(t, connect.Id(networkId))
		onPro := smtpMeterReadCounts(t, ctx, networkId)
		if onPro.connectionCount != 1 || onPro.recipientCount != int64(wideRecipientCount) || onPro.memberCount != 0 {
			t.Fatalf("{smq_} holds %+v after the send on Pro", onPro)
		}

		// the downgrade: the tier's keys change, and the gate's next refresh holds Community's
		if err := world.target.SetLimits(connect.Id(networkId), communityLimits); err != nil {
			t.Fatal(err)
		}
		world.gate.refresh(t, ctx)
		sessionCount := len(world.world.Verifier.Sessions())
		if outcome := world.send(t, wideRecipientCount, "past Community"); outcome.Outcome == sidecar.SmtpOutcomeSent {
			t.Fatalf("a preamble past Community's recipients a message was sent: %+v", outcome)
		}
		if len(world.world.Verifier.Sessions()) != sessionCount {
			t.Fatal("a preamble refused for its recipients opened a proof session")
		}
		if leaseCount := len(world.quota.leaseMeters(connect.Id(networkId))); leaseCount != 1 || world.quota.refusedLeaseCount.Load() != 0 {
			t.Fatalf("a QUOTA refusal asked a lease: %d leases, %d refused", leaseCount, world.quota.refusedLeaseCount.Load())
		}
		if refused := smtpMeterReadCounts(t, ctx, networkId); *refused != *onPro {
			t.Fatalf("{smq_} holds %+v after the refusal, %+v before it", refused, onPro)
		}

		// a preamble within Community's limits passes at them
		if outcome := world.send(t, narrowRecipientCount, "within Community"); outcome.Outcome != sidecar.SmtpOutcomeSent {
			t.Fatalf("the send within Community: %+v", outcome)
		}
		world.quota.awaitLeasesClosed(t, connect.Id(networkId))
		onCommunity := smtpMeterReadCounts(t, ctx, networkId)
		if onCommunity.connectionCount != 2 || onCommunity.recipientCount != int64(wideRecipientCount+narrowRecipientCount) || onCommunity.memberCount != 0 {
			t.Fatalf("{smq_} holds %+v after the send within Community", onCommunity)
		}
		if transactions := world.mx.Transactions(); len(transactions) != 2 {
			t.Fatalf("the MX took %d transactions", len(transactions))
		}

		// both sends' bytes debit once
		meteredByteCount := onCommunity.byteCount
		world.oracle.Meter(connect.Id(networkId), sidecar.QuotaMeterSmtp, meteredByteCount)
		world.check(t)
		report := world.sendFirstWindow(t)
		if byteCount, other := edgeMeterBatchByteCount(report.batch, connect.Id(networkId), sidecar.QuotaMeterSmtp); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d smtp bytes (other events %t), the sends moved %d", byteCount, other, meteredByteCount)
		}
		world.check(t)
		world.checkSettled(t)
		if balances := t3ReadBalances(ctx, server.Id(networkId)); len(balances) != 1 || balances[0].ByteCount != balanceByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, balanceByteCount-meteredByteCount)
		}
	})
}

// A QUOTA preamble refusal and a spent balance are separate answers. A preamble past the network's smtp_* limit is
// refused QUOTA by the {smq_} script, no lease asked and the network not spent. With the balance gone, a preamble
// within the limits passes the script, which reserves, and its connection is refused at the lease: the network is
// marked spent, nothing reaches the MX inside TLS and the send is not sent, and the reservation goes back, so the
// day's connections, recipients and bytes and the live members are untouched, the preamble's burst token being all it
// took (§14.7, §14.9). Nothing debits.
func TestSmtpMeterT3SpentRefusesAtTheLeaseNotTheLimit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		networkId := sidecar.NewId()
		communityLimits := smtpMeterCommunityLimits()
		recipientsPerMessage := int(communityLimits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage])
		world := newSmtpMeterWorld(t, ctx, networkId, communityLimits, 4*1024*1024)

		// a limit: refused by the script before any lease, the balance untouched and the network not spent
		if outcome := world.send(t, recipientsPerMessage+1, "past the limit"); outcome.Outcome == sidecar.SmtpOutcomeSent {
			t.Fatalf("a preamble past the recipients a message was sent: %+v", outcome)
		}
		if leaseCount := len(world.quota.leaseMeters(connect.Id(networkId))); leaseCount != 0 || world.quota.refusedLeaseCount.Load() != 0 {
			t.Fatalf("a QUOTA refusal asked a lease: %d leases, %d refused", leaseCount, world.quota.refusedLeaseCount.Load())
		}
		if tlsMeterSpentTime(t, ctx, connect.Id(networkId)) != nil {
			t.Fatal("a QUOTA refusal marked the network spent")
		}
		limited := smtpMeterReadCounts(t, ctx, networkId)
		if limited.connectionCount != 0 || limited.recipientCount != 0 || limited.byteCount != 0 || limited.burstCount != 0 || limited.memberCount != 0 {
			t.Fatalf("{smq_} holds %+v after a refusal for the recipients a message", limited)
		}
		world.check(t)

		// a spent balance: the script passes, and the lease refuses
		world.setBalance(t, 0)
		sessionCount := len(world.world.Verifier.Sessions())
		if outcome := world.send(t, recipientsPerMessage, "spent"); outcome.Outcome == sidecar.SmtpOutcomeSent {
			t.Fatalf("a spent network's send was sent: %+v", outcome)
		}
		okCount := len(world.world.Verifier.Sessions()) - sessionCount
		if okCount <= 0 {
			t.Fatal("no preamble of the spent network passed the script")
		}
		if refusedCount := world.quota.refusedLeaseCount.Load(); refusedCount != int64(okCount) {
			t.Fatalf("%d leases refused for %d preambles the script passed", refusedCount, okCount)
		}
		if leaseCount := len(world.quota.leaseMeters(connect.Id(networkId))); leaseCount != 0 {
			t.Fatalf("a spent network holds %d leases", leaseCount)
		}
		if tlsMeterSpentTime(t, ctx, connect.Id(networkId)) == nil {
			t.Fatal("a refused lease left the network unspent")
		}
		if transactions := world.mx.Transactions(); len(transactions) != 0 {
			t.Fatalf("the MX took %d transactions from a spent network", len(transactions))
		}
		spent := smtpMeterReadCounts(t, ctx, networkId)
		if spent.connectionCount != 0 || spent.recipientCount != 0 || spent.byteCount != 0 || spent.memberCount != 0 || spent.burstCount != int64(okCount) {
			t.Fatalf("{smq_} holds %+v after %d preambles refused at the lease", spent, okCount)
		}
		world.check(t)
		world.checkSettled(t)
		if days := edgeMeterUsageDays(ctx, connect.Id(networkId), sidecar.QuotaMeterSmtp); len(days) != 0 {
			t.Fatalf("a spent network counts smtp usage: %v", days)
		}
	})
}
