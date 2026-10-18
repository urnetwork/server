// Actual writer-boundary regressions for recoverable complete Transfer refusal.
// Every original and reverse acknowledgement is produced by a real Client.
// The only held owner is the existing pre-write batch barrier; no wire, queue,
// acknowledgement, deadline or retry policy is manufactured by the fixture.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Each root names exactly which genuine message the selected native hop refuses.
type exchangeQueueRefusalKind int

const (
	exchangeQueueRefusalOriginal exchangeQueueRefusalKind = iota
	exchangeQueueRefusalReverseAck
	exchangeQueueRefusalNoAck
	exchangeQueueRefusalCapacity   = 4096
	exchangeQueueRefusalBatchCount = 256
	// 4,097 admitted fillers plus target/retry/controls can expose multiple
	// pooled roots each. The 65,536-root census has a finite overflow failure;
	// it is not a claim that payload reuse or automatic retry is unbounded.
	exchangeQueueRefusalWitnessLimit = 64 * 1024
)

// Scalar-only, bounded outcome observations; no message owner escapes here.
type exchangeQueueRefusalOutcome struct {
	ordinal   int
	result    pooledMessageSendResult
	byteCount int
}

// An instance-owned test controller, never a global hook or production owner.
type exchangeQueueRefusalFixture struct {
	origin   *p2OriginFixture
	op       ExchangeOp
	kind     exchangeQueueRefusalKind
	from, to *p2OriginEndpoint
	// Assigned before native workers or their Close wrappers are published.
	transport      *ResidentTransport
	connection     *ExchangeConnection
	published      chan *ExchangeConnection
	outcomes       chan exchangeQueueRefusalOutcome
	firstDequeued  chan struct{}
	releaseFirst   chan struct{}
	releaseOnce    sync.Once
	localClosed    chan struct{}
	closeObserved  chan struct{}
	localCloseOnce sync.Once
	dials          atomic.Int64
	lifecyclePhase atomic.Uint32
	firstDialState atomic.Uint32
	extraDialState atomic.Uint32
	extraDial      chan struct{}
	// Only the cancellation-order control substitutes this constructor parent.
	transportParent  context.Context
	beforeRootCancel func()
	afterRootCancel  func()
	publications     atomic.Int64
	outcomeOverflow  atomic.Bool
	refused          atomic.Int64
	targetWireBytes  atomic.Int64
	socketTimeout    atomic.Bool
	// One atomic word captures endpoint (bits0..1), phase (bits2..3),
	// canceled owner (bit4), and preceding refusals (bits5+), at first Close.
	firstCloseProof atomic.Int64
	// Only the exact connection writer mutates these two counters.
	outcomeOrdinal int
	batchOrdinal   int
	// The origin metadata lock guards endpoint acceptance observations.
	receiverPacks, receiverAccepted, senderAcks, senderAccepted int
}

// The phase and two cancellation states are sampled only at the fixture dial
// boundary. A phase crossing is retained as ambiguous, never classified away.
const (
	exchangeQueueRefusalPhaseSetup uint32 = iota
	exchangeQueueRefusalPhaseLiveOracle
	exchangeQueueRefusalPhaseTeardown
)

const (
	exchangeQueueRefusalDialRootDone uint32 = 1 << iota
	exchangeQueueRefusalDialCallerDone
)

const (
	exchangeQueueRefusalDialPhaseChanged uint32 = 16
	exchangeQueueRefusalDialObserved     uint32 = 32
)

// Two scalar slots plus one event retain the first and first-extra admissions.
// Every invocation still increments the strict lifetime census.
func (self *exchangeQueueRefusalFixture) observeDial(ctx context.Context) int64 {
	phase := self.lifecyclePhase.Load()
	state := exchangeQueueRefusalDialObserved | phase<<2
	if self.origin.ctx.Err() != nil {
		state |= exchangeQueueRefusalDialRootDone
	}
	if ctx.Err() != nil {
		state |= exchangeQueueRefusalDialCallerDone
	}
	if phase != self.lifecyclePhase.Load() {
		state |= exchangeQueueRefusalDialPhaseChanged
	}
	ordinal := self.dials.Add(1)
	switch ordinal {
	case 1:
		self.firstDialState.Store(state)
	case 2:
		self.extraDialState.Store(state)
		close(self.extraDial)
	default:
		self.outcomeOverflow.Store(true)
	}
	return ordinal
}

// Formatting is confined to test failure reporting, not a packet path.
func exchangeQueueRefusalDialDescription(state uint32) string {
	return fmt.Sprintf("observed=%t phase=%d rootCanceled=%t callerCanceled=%t phaseChanged=%t",
		state&exchangeQueueRefusalDialObserved != 0, (state>>2)&3,
		state&exchangeQueueRefusalDialRootDone != 0, state&exchangeQueueRefusalDialCallerDone != 0,
		state&exchangeQueueRefusalDialPhaseChanged != 0)
}

// This test-only wrapper preserves every pipe operation and deadline. It records
// whether a deadline or the accepted peer, rather than the queue-refusing owner,
// caused the generation to end. Endpoint 1 is the outbound constructor's pipe.
type exchangeQueueRefusalConn struct {
	net.Conn
	fixture  *exchangeQueueRefusalFixture
	endpoint int64
}

func (self *exchangeQueueRefusalConn) Read(buffer []byte) (int, error) {
	n, err := self.Conn.Read(buffer)
	self.observeError(err)
	return n, err
}

func (self *exchangeQueueRefusalConn) Write(buffer []byte) (int, error) {
	n, err := self.Conn.Write(buffer)
	self.observeError(err)
	return n, err
}

func (self *exchangeQueueRefusalConn) observeError(err error) {
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		self.fixture.socketTimeout.Store(true)
	}
}

func (self *exchangeQueueRefusalConn) Close() error {
	q := self.fixture
	phase := q.lifecyclePhase.Load()
	proof := self.endpoint | int64(phase)<<2 | q.refused.Load()<<5
	if q.origin.ctx.Err() != nil || (q.transport != nil && q.transport.ctx.Err() != nil) {
		proof |= 16
	}
	q.firstCloseProof.CompareAndSwap(0, proof)
	err := self.Conn.Close()
	if self.endpoint == 1 {
		self.fixture.localCloseOnce.Do(func() { close(self.fixture.localClosed) })
	}
	return err
}

// A refused original stays in the real sender's resend queue on the transport hop.
func TestExchangeTransportQueueRefusedOriginalRetriesOnSameGeneration(t *testing.T) {
	runExchangeQueueRefusal(t, ExchangeOpTransport, exchangeQueueRefusalOriginal)
}

// Losing a genuine reverse ACK makes the sender retry; the receiver deduplicates.
func TestExchangeTransportQueueRefusedAckRetriesWithoutDuplicateApplication(t *testing.T) {
	runExchangeQueueRefusal(t, ExchangeOpTransport, exchangeQueueRefusalReverseAck)
}

// NoAck route completion is counted loss, not application delivery or replay.
func TestExchangeTransportQueueRefusedNoAckIsCountedLoss(t *testing.T) {
	runExchangeQueueRefusal(t, ExchangeOpTransport, exchangeQueueRefusalNoAck)
}

// The forward owner's identical complete-message boundary preserves retry custody.
func TestExchangeForwardQueueRefusedOriginalRetriesOnSameGeneration(t *testing.T) {
	runExchangeQueueRefusal(t, ExchangeOpForward, exchangeQueueRefusalOriginal)
}

// The forwarded-ACK root exercises the actual destination resident and Client.
func TestExchangeForwardQueueRefusedAckRetriesWithoutDuplicateApplication(t *testing.T) {
	runExchangeQueueRefusal(t, ExchangeOpForward, exchangeQueueRefusalReverseAck)
}

// The forward writer must count exactly one NoAck refusal and keep its generation.
func TestExchangeForwardQueueRefusedNoAckIsCountedLoss(t *testing.T) {
	runExchangeQueueRefusal(t, ExchangeOpForward, exchangeQueueRefusalNoAck)
}

// Initializes the shared real-origin fixture without changing any production setting.
func newExchangeQueueRefusalFixture(t testing.TB, op ExchangeOp, kind exchangeQueueRefusalKind) *exchangeQueueRefusalFixture {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	f := &p2OriginFixture{
		t: t, ctx: ctx, cancel: cancel,
		coldEntered: make(chan struct{}), coldRelease: make(chan struct{}),
		retryRelease:   make(chan struct{}),
		poolViolations: clientconnect.MessagePoolViolationCount(),
		expected:       map[string]int{},
		state: p2OriginState{
			changed: make(chan struct{}), items: map[string]*p2OriginItem{},
			current:   map[clientconnect.Id]string{},
			lifecycle: map[p2OriginToken]string{}, noAckTokens: map[p2OriginToken]string{},
			messageLabels: map[clientconnect.Id][]string{}, witnesses: map[*byte][]byte{},
			witnessLimit: exchangeQueueRefusalWitnessLimit,
		},
	}
	return &exchangeQueueRefusalFixture{
		origin: f, op: op, kind: kind,
		published:     make(chan *ExchangeConnection, 1),
		outcomes:      make(chan exchangeQueueRefusalOutcome, 2*exchangeQueueRefusalCapacity),
		firstDequeued: make(chan struct{}), releaseFirst: make(chan struct{}),
		localClosed: make(chan struct{}), closeObserved: make(chan struct{}), extraDial: make(chan struct{}),
	}
}

// Uses the real owned service environment. This root is intentionally at least
// the unchanged 15-second queue timeout, not a short-timeout approximation.
func runExchangeQueueRefusal(t *testing.T, op ExchangeOp, kind exchangeQueueRefusalKind) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		q := newExchangeQueueRefusalFixture(t, op, kind)
		f := q.origin
		defer f.close()
		q.configure()
		f.setup()
		q.from, q.to = f.source, f.victim
		if kind == exchangeQueueRefusalReverseAck {
			q.from, q.to = f.victim, f.source
		}
		if f.settings.ExchangeBufferSize != exchangeQueueRefusalCapacity ||
			f.settings.ForwardBufferSize != exchangeQueueRefusalCapacity ||
			f.settings.ExchangeWriteBatchCount != exchangeQueueRefusalBatchCount ||
			f.settings.ExchangeWriteBatchByteCount != 256*1024 ||
			f.settings.WriteTimeout != 15*time.Second ||
			f.settings.ExchangeReadTimeout != 30*time.Second ||
			f.settings.ExchangePingTimeout != time.Second ||
			!f.settings.ForwardEnforceActiveContracts {
			t.Fatal("fixture changed production queues, batching, timeout or authorization")
		}

		f.send(q.from, q.to, "native-blocker", true)
		q.connection = q.takePublished()
		q.expectOutcome(pooledMessageSendDelivered, "first original")
		q.waitEvent(q.firstDequeued, "real writer's first pre-write batch")
		if cap(q.connection.send) != exchangeQueueRefusalCapacity ||
			q.connection.header.Op != op ||
			q.connection.afterSendDequeueForTest == nil {
			t.Fatal("constructor did not publish the real default-capacity writer")
		}
		// The writer has taken exactly the singleton and is now held. Waiting
		// for each real admission makes every remaining slot a genuine wire.
		for index := range exchangeQueueRefusalCapacity {
			label := fmt.Sprintf("native-fill-%04d", index)
			f.send(q.from, q.to, label, true)
			outcome := q.expectOutcome(pooledMessageSendDelivered, label)
			if outcome.byteCount <= 0 || outcome.byteCount > 1024 {
				t.Fatalf("filler exceeds the proved 256-frame batch bound: %+v", outcome)
			}
			f.expected[label] = 1
		}
		f.expected["native-blocker"] = 1
		messagesBefore, bytesBefore := exchangeQueueRefusalCounters(q.boundary())

		// For reverse-ACK loss this travels on the other, unheld hop. Its
		// actual receiver-generated ACK alone is the next selected offer.
		f.send(f.source, f.victim, "victim", kind == exchangeQueueRefusalNoAck)
		refusal := q.expectOutcome(pooledMessageSendDropped, "actual complete-message queue timeout")
		if refusal.ordinal != exchangeQueueRefusalCapacity+2 ||
			refusal.byteCount <= 0 || int64(refusal.byteCount) != q.targetWireBytes.Load() ||
			q.refused.Load() != 1 {
			t.Fatalf("refused offer was not the exact target after the full queue: %+v bytes=%d drops=%d",
				refusal, q.targetWireBytes.Load(), q.refused.Load())
		}
		q.checkCustodyBeforeRelease()
		// A separate source, resident, destination and hop remains productive
		// while the selected writer is still held. This is not a claim that
		// data can pass through the deliberately full selected socket.
		f.send(f.second, f.healthy, "native-independent", false)
		f.waitDelivered("native-independent", true)
		f.expected["native-independent"] = 1

		q.releaseOnce.Do(func() { close(q.releaseFirst) })
		f.wait(func(s *p2OriginState) bool {
			return exchangeQueueRefusalFillersDelivered(s) == exchangeQueueRefusalCapacity+1
		}, "every accepted default-queue filler reaches its real application")

		// A held natural retry owns its dedicated physical pump. Release it
		// before another control on that same route, otherwise the test would
		// manufacture a head-of-line block outside the target native writer.
		f.releaseRetryOnce.Do(func() { close(f.retryRelease) })
		if kind != exchangeQueueRefusalNoAck {
			f.waitDelivered("victim", true)
			f.expected["victim"] = 1
		} else {
			f.wait(func(s *p2OriginState) bool {
				item := s.items["victim"]
				return item.terminalCount == 1 &&
					item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] == 1 &&
					item.noAckPhases[clientconnect.NoAckSendPhaseCompleted] == 1
			}, "NoAck route completion, expressly not delivery")
			f.expected["victim"] = 0
		}
		f.send(q.from, q.to, "native-same-generation", false)
		f.waitDelivered("native-same-generation", true)
		f.expected["native-same-generation"] = 1
		q.requireSameGeneration()
		messagesAfter, bytesAfter := exchangeQueueRefusalCounters(q.boundary())
		if messagesAfter-messagesBefore != 1 ||
			bytesAfter-bytesBefore != float64(refusal.byteCount) {
			t.Fatalf("exact caller refusal counters=%g/%g want=1/%d",
				messagesAfter-messagesBefore, bytesAfter-bytesBefore, refusal.byteCount)
		}
		// The same-owner positive control has passed the serial writer, so
		// the refused forward's releasePayload and counter update happened.
		q.checkFinal()
	})
}

// Settings and endpoint/backends are selected before constructors publish workers.
// All other links keep the already reviewed real-origin fixture unchanged.
func (self *exchangeQueueRefusalFixture) configure() {
	f := self.origin
	cancelRoot := f.cancel
	var cancelOnce sync.Once
	f.cancel = func() {
		cancelOnce.Do(func() {
			self.lifecyclePhase.Store(exchangeQueueRefusalPhaseTeardown)
			// This explicitly owned worker must be canceled before its accepted-side
			// siblings. Root cancellation does not specify child publication order.
			if self.transport != nil {
				self.transport.Cancel()
			}
			if self.beforeRootCancel != nil {
				self.beforeRootCancel()
			}
			cancelRoot()
			if self.afterRootCancel != nil {
				self.afterRootCancel()
			}
		})
	}
	fromSuffix, toSuffix := byte(2), byte(16)
	if self.kind == exchangeQueueRefusalReverseAck {
		fromSuffix, toSuffix = toSuffix, fromSuffix
	}
	f.configureSettings = func(settings *ExchangeSettings) {
		settings.beforeExchangeConnectionRunForTest = self.publishConnection
		settings.DialContext = func(ctx context.Context, network, authority string) (net.Conn, error) {
			if network != "tcp" || authority != "native-refusal.example:1" {
				return nil, fmt.Errorf("unexpected synthetic native dial %s %s", network, authority)
			}
			if self.observeDial(ctx) != 1 {
				return nil, fmt.Errorf("replacement dial cannot qualify same-generation refusal: %s", exchangeQueueRefusalDialDescription(self.extraDialState.Load()))
			}
			local, remote := net.Pipe()
			localOwner := &exchangeQueueRefusalConn{Conn: local, fixture: self, endpoint: 1}
			remoteOwner := &exchangeQueueRefusalConn{Conn: remote, fixture: self, endpoint: 2}
			// The accepted side retains its real header, resident-generation,
			// authorization, framing and worker lifecycle. The fixture joins
			// this admitted Exchange worker before its final pool census.
			if !f.exchange.startConnectionWorker(func() { f.exchange.handleExchangeConnection(remoteOwner) }) {
				localOwner.Close()
				remoteOwner.Close()
				return nil, fmt.Errorf("accepted connection owner refused admission")
			}
			return localOwner, nil
		}
	}
	f.configureEndpoint = func(endpoint *p2OriginEndpoint) bool {
		f.exchange.stateLock.Lock()
		f.exchange.residents[endpoint.id] = endpoint.resident
		f.exchange.stateLock.Unlock()
		if self.op != ExchangeOpTransport || endpoint.id[15] != fromSuffix {
			return false
		}
		parent := f.ctx
		if self.transportParent != nil {
			parent = self.transportParent
		}
		self.transport = NewResidentTransportWithProperties(
			parent, f.exchange, endpoint.id, endpoint.resident.instanceId,
			clientconnect.TransferCarrierProperties{ReceiveReliability: clientconnect.CarrierReliabilityReliable},
		)
		endpoint.toClient, endpoint.toResident = self.transport.receive, self.transport.send
		return true
	}
	f.startNativeForward = func(source, destination *p2OriginEndpoint, forward *ResidentForward) bool {
		if self.op != ExchangeOpForward || source.id[15] != fromSuffix || destination.id[15] != toSuffix {
			return false
		}
		registration := exchangeQueueRefusalRegistration(destination)
		f.workers.Add(1)
		go self.runNativeWorker(func() {
			forward.runWithResidentLookup(func(ctx context.Context, clientId server.Id, ttl time.Duration) *model.NetworkClientResident {
				if clientId != destination.id {
					f.fail(fmt.Errorf("native forward lookup changed destination"))
					return nil
				}
				return registration
			})
		})
		return true
	}
	f.deliverOutgoing = func(source *p2OriginEndpoint, wire []byte) bool {
		if self.op != ExchangeOpTransport || source.id[15] != fromSuffix {
			return false
		}
		// Takes ownership on every outcome; never return the wire a second time.
		result := self.transport.sendReceivedMessage(f.ctx.Done(), wire, clientconnect.CarrierReliabilityReliable)
		self.observeTransportAdmission(result)
		return true
	}
	f.configureBeforeTripwire = func() {
		if self.op != ExchangeOpTransport {
			return
		}
		var owner *p2OriginEndpoint
		for _, endpoint := range f.endpoints {
			if endpoint.id[15] == fromSuffix {
				owner = endpoint
			}
		}
		if owner == nil || self.transport == nil {
			f.t.Fatal("native transport owner was not constructed")
		}
		registration := exchangeQueueRefusalRegistration(owner)
		if !model.NominateResident(f.ctx, nil, registration, f.settings.ExchangeResidentTtl) {
			f.t.Fatal("actual Redis resident registration refused setup")
		}
		resolved := model.GetResidentForClientWithInstance(f.ctx, owner.id, owner.resident.instanceId, 0)
		if resolved == nil || resolved.ResidentId != owner.resident.residentId ||
			resolved.ResidentHost != registration.ResidentHost {
			f.t.Fatal("actual instance lookup did not resolve the seeded resident")
		}
		f.workers.Add(1)
		go self.runNativeWorker(self.transport.Run)
		// Publishing only establishes constructor/header readiness. The test
		// takes the publication after its first real public Client send.
	}
	f.beforeRouteDelivery = func(wire p2OriginWire) {
		f.state.stateLock.Lock()
		item := f.state.items["victim"]
		target := item != nil && item.identitySet && wire.identity.message == item.identity.message
		f.state.stateLock.Unlock()
		if !target || wire.ack != (self.kind == exchangeQueueRefusalReverseAck) {
			return
		}
		// Only the first exact target can be refused. Natural data retries
		// are held separately by the sealed physical route gate.
		self.targetWireBytes.CompareAndSwap(0, int64(wire.byteCount))
	}
	f.observeReceiveProgress = self.observeProgress
	f.observeApplicationWithLock = func(s *p2OriginState, label string) {
		if label == "native-blocker" || strings.HasPrefix(label, "native-fill-") {
			s.nativeFillerApplications++
		}
	}
	f.joinNativeOwners = func(ctx context.Context) bool {
		f.exchange.Close()
		return f.exchange.WaitForIdle(ctx)
	}
	f.afterOwnersJoined = func(ctx context.Context) {
		if self.transport != nil {
			self.transport.Close()
			// Cleanup uses the exact seeded generation, not the transport
			// header template, whose resident id is filled only for each dial.
			for _, endpoint := range f.endpoints {
				if endpoint.id == self.transport.clientId {
					if err := model.RemoveResidentForClientWithDeadline(ctx, endpoint.id, endpoint.resident.residentId); err != nil {
						f.t.Errorf("exact seeded resident cleanup: %v", err)
					}
				}
			}
		}
		if self.outcomeOverflow.Load() {
			f.t.Error("bounded scalar observation overflowed")
		}
	}
	f.afterJoined = func(s *p2OriginState) {
		if self.dials.Load() != 1 || self.publications.Load() != 1 || self.refused.Load() != 1 {
			f.t.Errorf("joined dial/generation/refusal census=%d/%d/%d want=1/1/1; first={%s} extra={%s}", self.dials.Load(), self.publications.Load(), self.refused.Load(), exchangeQueueRefusalDialDescription(self.firstDialState.Load()), exchangeQueueRefusalDialDescription(self.extraDialState.Load()))
		}
		if self.socketTimeout.Load() {
			f.t.Error("invalid setup: a socket deadline expired in the actual native generation")
		}
		if item := s.items["victim"]; item != nil && self.kind == exchangeQueueRefusalReverseAck {
			if item.applications != 1 || item.acks < 2 || item.retryWrites < 1 ||
				self.receiverAccepted < 2 || self.senderAccepted < 1 {
				f.t.Errorf("joined genuine duplicate/ACK custody missing: item=%+v receiver=%d sender=%d",
					item, self.receiverAccepted, self.senderAccepted)
			}
		}
	}
}

// Only explicit fixture teardown of this exact child is an early cancellation.
func (self *exchangeQueueRefusalFixture) transportCanceledForCleanup() bool {
	return self.lifecyclePhase.Load() == exchangeQueueRefusalPhaseTeardown &&
		self.transport != nil && self.transport.ctx.Err() != nil
}

// A consuming input can return Done before root cancellation is published.
// Refusal or another live failure still invalidates the native fixture.
func (self *exchangeQueueRefusalFixture) observeTransportAdmission(result pooledMessageSendResult) {
	if result != pooledMessageSendDelivered && self.origin.ctx.Err() == nil &&
		!(result == pooledMessageSendDone && self.transportCanceledForCleanup()) {
		self.origin.fail(fmt.Errorf("native transport input did not admit its complete original: %v", result))
	}
}

// Matches the production Run exception boundary while making unexpected raises
// test failures. Done raises are benign after root cancellation or explicit
// teardown cancellation of this exact transport, never from a live owner.
// Every return/raise joins the registered worker; prior failure evidence is kept.
func (self *exchangeQueueRefusalFixture) runNativeWorker(run func()) {
	f := self.origin
	defer f.workers.Done()
	recovered := server.HandleError(run)
	if recovered == nil || ((f.ctx.Err() != nil || self.transportCanceledForCleanup()) && server.IsDoneError(recovered)) {
		return
	}
	err, ok := recovered.(error)
	if !ok {
		err = fmt.Errorf("native fixture worker raised: %v", recovered)
	}
	f.mutate(func(s *p2OriginState) {
		s.err = errors.Join(s.err, err)
	})
}

// The synthetic address is resolved only by the already-supported DialContext seam.
func exchangeQueueRefusalRegistration(endpoint *p2OriginEndpoint) *model.NetworkClientResident {
	return &model.NetworkClientResident{
		ClientId: endpoint.id, InstanceId: endpoint.resident.instanceId,
		ResidentId: endpoint.resident.residentId, ResidentHost: "native-refusal.example",
		ResidentService: "connect", ResidentBlock: "synthetic", ResidentInternalPorts: []int{1},
	}
}

// Called after the real header and before any connection worker starts. All
// callbacks are published once; the outcome callback never blocks or retains bytes.
func (self *exchangeQueueRefusalFixture) publishConnection(connection *ExchangeConnection) {
	ordinal := self.publications.Add(1)
	owner := self.origin.source
	if self.kind == exchangeQueueRefusalReverseAck {
		owner = self.origin.victim
	}
	if self.op == ExchangeOpForward {
		if owner == self.origin.source {
			owner = self.origin.victim
		} else {
			owner = self.origin.source
		}
	}
	if ordinal != 1 || connection.header.Op != self.op || owner == nil ||
		connection.header.ClientId != owner.id || connection.header.ResidentId != owner.resident.residentId {
		self.outcomeOverflow.Store(true)
		return
	}
	connection.afterSendDequeueForTest = func() { self.beforeBatchWrite(connection) }
	connection.afterSendMessageForTest = func(result pooledMessageSendResult, byteCount int) {
		self.outcomeOrdinal++
		if result == pooledMessageSendDropped {
			self.refused.Add(1)
		}
		select {
		case self.outcomes <- exchangeQueueRefusalOutcome{
			ordinal: self.outcomeOrdinal, result: result, byteCount: byteCount,
		}:
		default:
			self.outcomeOverflow.Store(true)
		}
	}
	select {
	case self.published <- connection:
	default:
		self.outcomeOverflow.Store(true)
	}
	f := self.origin
	f.workers.Add(1)
	go func() {
		defer f.workers.Done()
		defer close(self.closeObserved)
		self.observeConnectionClose()
	}()
}

// A captured live close stays evidence even when cancellation wins this wait.
// Only an already-canceled owner closing in explicit teardown is benign.
func (self *exchangeQueueRefusalFixture) observeConnectionClose() {
	f := self.origin
	select {
	case <-f.ctx.Done():
	case <-self.localClosed:
	}
	proof := self.firstCloseProof.Load()
	if proof == 0 {
		return
	}
	phase, ownerCanceled := uint32((proof>>2)&3), proof&16 != 0
	if phase == exchangeQueueRefusalPhaseTeardown && ownerCanceled {
		return
	}
	if proof>>5 == 1 && self.refused.Load() == 1 &&
		proof&3 == 1 && !self.socketTimeout.Load() {
		f.fail(fmt.Errorf("OLD discriminator: actual complete-message Dropped preceded the outbound owner's first socket close; no socket deadline expired"))
	} else {
		f.fail(fmt.Errorf("invalid setup: native generation ended without the queue-refusal close proof: drops=%d dropsAtFirstClose=%d firstCloser=%d socketTimeout=%t closePhase=%d ownerCanceled=%t",
			self.refused.Load(), proof>>5, proof&3, self.socketTimeout.Load(), phase, ownerCanceled))
	}
}

// The existing barrier is before WriteMessages/SetWriteDeadline. It never
// holds a socket write open. During drain, fixed-size real batches are paced
// by previous application completions so an unrelated shared-ingress overflow
// cannot be substituted for the target send-queue refusal.
func (self *exchangeQueueRefusalFixture) beforeBatchWrite(connection *ExchangeConnection) {
	self.batchOrdinal++
	if self.batchOrdinal == 1 {
		close(self.firstDequeued)
		select {
		case <-self.releaseFirst:
		case <-connection.Done():
		case <-self.origin.ctx.Done():
		}
		return
	}
	// With 4,096 fixed <=1-KiB wires already queued and no other producer
	// released, each next batch contains exactly 256 messages. The first
	// singleton plus sixteen batches completes every accepted filler.
	if self.batchOrdinal > 18 {
		return
	}
	want := min(1+(self.batchOrdinal-2)*exchangeQueueRefusalBatchCount, exchangeQueueRefusalCapacity+1)
	f := self.origin
	for {
		f.state.stateLock.Lock()
		changed := f.state.changed
		ready := exchangeQueueRefusalFillersDelivered(&f.state) >= want
		f.state.stateLock.Unlock()
		if ready {
			return
		}
		select {
		case <-changed:
		case <-connection.Done():
			return
		case <-f.ctx.Done():
			return
		}
	}
}

// The admitted filler set is fixed and finite; no queue-length polling is used.
func exchangeQueueRefusalFillersDelivered(state *p2OriginState) int {
	return state.nativeFillerApplications
}

// Publication contains the exact constructor-produced connection, not a replacement.
func (self *exchangeQueueRefusalFixture) takePublished() *ExchangeConnection {
	select {
	case connection := <-self.published:
		return connection
	case <-self.origin.ctx.Done():
		self.origin.t.Fatal("actual constructor/header publication did not complete")
		return nil
	}
}

// The outcome event is the actual timed send's disposition, not a synthetic drop.
func (self *exchangeQueueRefusalFixture) expectOutcome(want pooledMessageSendResult, description string) exchangeQueueRefusalOutcome {
	f := self.origin
	for {
		f.state.stateLock.Lock()
		changed, err := f.state.changed, f.state.err
		f.state.stateLock.Unlock()
		if err != nil {
			f.t.Fatal(err)
		}
		if self.outcomeOverflow.Load() {
			f.t.Fatal("bounded outcome/publication observation overflow")
		}
		select {
		case outcome := <-self.outcomes:
			if outcome.result != want {
				f.t.Fatalf("%s actual outcome=%+v want=%v", description, outcome, want)
			}
			return outcome
		case <-changed:
		case <-f.ctx.Done():
			f.t.Fatalf("%s did not produce its actual queue outcome: %v", description, f.ctx.Err())
		}
	}
}

// Test gates are positive events; cancellation is always an escape for cleanup.
func (self *exchangeQueueRefusalFixture) waitEvent(event <-chan struct{}, description string) {
	select {
	case <-event:
	case <-self.origin.ctx.Done():
		self.origin.t.Fatalf("%s: %v", description, self.origin.ctx.Err())
	}
}

// Observe real public Client acceptance metadata, never write its sequence or ACK state.
func (self *exchangeQueueRefusalFixture) observeProgress(event clientconnect.TransferProgressEvent) {
	f := self.origin
	if f.ctx.Err() != nil {
		return
	}
	f.mutate(func(s *p2OriginState) {
		item := s.items["victim"]
		if item == nil || !item.identitySet || event.MessageId != item.identity.message {
			return
		}
		switch event.Stage {
		case "receive_pack_begin", "receive_pack_end":
			if event.ClientId != clientconnect.Id(f.victim.id) ||
				event.PeerId != clientconnect.Id(f.source.id) || event.SequenceId != item.identity.sequence {
				s.err = fmt.Errorf("native receiver progress changed exact original identity")
				return
			}
			if event.Stage == "receive_pack_begin" {
				self.receiverPacks++
			} else if event.Success {
				self.receiverAccepted++
			}
		case "receive_ack_begin", "receive_ack_end":
			if event.ClientId != clientconnect.Id(f.source.id) ||
				event.PeerId != clientconnect.Id(f.victim.id) || event.SequenceId != item.identity.sequence || event.Selective {
				s.err = fmt.Errorf("native ACK progress changed reversed original identity")
				return
			}
			if event.Stage == "receive_ack_begin" {
				self.senderAcks++
			} else if event.Success && (event.Outcome == "accepted" || event.Outcome == "accepted_after_wait") {
				self.senderAccepted++
			}
		}
	})
}

// A dropped reverse ACK leaves the original retained even though its application ran.
func (self *exchangeQueueRefusalFixture) checkCustodyBeforeRelease() {
	f := self.origin
	f.state.stateLock.Lock()
	item := *f.state.items["victim"]
	receiverPacks, senderAcks := self.receiverPacks, self.senderAcks
	f.state.stateLock.Unlock()
	wantApplications, wantOriginalAcks := 0, 0
	if self.kind == exchangeQueueRefusalReverseAck {
		wantApplications, wantOriginalAcks = 1, 1
	}
	if item.applications != wantApplications || item.acks != wantOriginalAcks ||
		item.selectiveAcks != 0 || item.drops != 0 || senderAcks != 0 {
		f.t.Fatalf("wrong custody at native queue refusal: item=%+v receiver=%d sender=%d", item, receiverPacks, senderAcks)
	}
	if self.kind != exchangeQueueRefusalNoAck {
		if !item.initialOwnerSampled || !item.initialOwnerSnapshot.Complete ||
			item.initialOwnerSnapshot.SendAck.Owners != 1 || item.terminalCount != 0 {
			f.t.Fatalf("genuine unacknowledged original lost retry custody: %+v", item)
		}
	}
}

// Exact same constructor generation, not reconnect plus eventual application success.
func (self *exchangeQueueRefusalFixture) requireSameGeneration() {
	if self.dials.Load() != 1 || self.publications.Load() != 1 || self.connection == nil {
		self.origin.t.Fatal("a replacement generation concealed the refusal")
	}
	if self.firstCloseProof.Load() != 0 || self.socketTimeout.Load() {
		self.origin.t.Fatal("a socket close or deadline concealed the complete-message refusal")
	}
	select {
	case <-self.connection.Done():
		self.origin.t.Fatal("complete refusal retired the original native generation")
	default:
	}
	for _, endpoint := range self.origin.endpoints {
		if endpoint.resident.IsDone() || endpoint.client.IsDone() {
			self.origin.t.Fatal("complete refusal retired an endpoint or resident")
		}
	}
	self.lifecyclePhase.Store(exchangeQueueRefusalPhaseLiveOracle)
}

// Final and joined checks distinguish source completion, application effect and retry.
func (self *exchangeQueueRefusalFixture) checkFinal() {
	f := self.origin
	f.state.stateLock.Lock()
	defer f.state.stateLock.Unlock()
	item := f.state.items["victim"]
	if item.terminalCount != 1 || item.terminalErr != nil || item.drops != 0 ||
		self.refused.Load() != 1 {
		f.t.Fatalf("native refusal terminal/ownership census: %+v", item)
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted, clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
		clientconnect.SendPackLifecyclePhaseTerminal,
	} {
		if item.lifecycle[phase] != 1 || item.lifecycleErr[phase] != nil {
			f.t.Fatalf("native original lifecycle phase %v: %+v", phase, item)
		}
	}
	if self.kind == exchangeQueueRefusalNoAck {
		if item.wireWrites != 1 || item.retryWrites != 0 || item.applications != 0 ||
			item.acks != 0 || item.noAckErr != nil || item.noAckPhases[clientconnect.NoAckSendPhaseStarted] != 1 ||
			item.noAckPhases[clientconnect.NoAckSendPhaseCompleted] != 1 {
			f.t.Fatalf("NoAck completion was confused with delivery/retry: %+v", item)
		}
	} else if item.wireWrites < 2 || item.retryWrites < 1 || item.applications != 1 ||
		self.senderAccepted < 1 {
		f.t.Fatalf("genuine retry and accepted receiver ACK missing: item=%+v accepted=%d", item, self.senderAccepted)
	}
	if self.kind == exchangeQueueRefusalReverseAck && (item.acks < 2 || self.receiverAccepted < 2) {
		f.t.Fatalf("lost genuine ACK did not recover through duplicate/dedup: %+v", item)
	}
}

// There are exactly two fixed labels, selected by the actual writer owner.
func (self *exchangeQueueRefusalFixture) boundary() string {
	if self.op == ExchangeOpTransport {
		return "exchange_transport_send"
	}
	return "exchange_forward_send"
}

// Counter reads happen on the test owner, after a serial same-writer positive control.
func exchangeQueueRefusalCounters(boundary string) (float64, float64) {
	var messages, bytes dto.Metric
	_ = receiveQueueDroppedMessagesCounter.WithLabelValues(boundary).Write(&messages)
	_ = receiveQueueDroppedBytesCounter.WithLabelValues(boundary).Write(&bytes)
	return messages.GetCounter().GetValue(), bytes.GetCounter().GetValue()
}
