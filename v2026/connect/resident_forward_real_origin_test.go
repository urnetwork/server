// Real origin/receiver Transfer coverage reused from the sealed R4 fixture.
// Optional test-owned backends exercise real native owners and headers; nil
// preserves the original bounded-route fixture. No production API is exported.
package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// Stable logical identity; retry tags, ciphertext and Head may legitimately change.
type p2OriginIdentity struct {
	source, destination, sequence, message   clientconnect.Id
	number                                   uint64
	lane                                     uint32
	forceStream, companion, sessionCompanion bool
	sessionRole                              protocol.SequenceRole
}

// One decoded, independently allocated observation; no callback bytes escape here.
type p2OriginWire struct {
	identity  p2OriginIdentity
	labels    []string
	noAck     bool
	ack       bool
	selective bool
	byteCount int
}

// Optional follow-on metadata hooks never replace a production handler or queue.
type p2OriginForwardObservation struct {
	path                                                   clientconnect.TransferPath
	wire                                                   p2OriginWire
	wireBytes                                              int
	messagesBefore, messagesAfter, bytesBefore, bytesAfter float64
	before, after                                          residentPayloadGroup
	retired                                                bool
}

// Keeps exact per-original event counts, not sampled queue-length progress.
type p2OriginItem struct {
	identity                                                                                                 p2OriginIdentity
	identitySet                                                                                              bool
	wireWrites, wireAttempts, retryWrites, ingressStarted, ingress, drops, applications, acks, selectiveAcks int
	terminalCount                                                                                            int
	terminalErr                                                                                              error
	lastDropBytes                                                                                            int
	noAck                                                                                                    bool
	lifecycle                                                                                                [4]int
	lifecycleErr                                                                                             [4]error
	noAckPhases                                                                                              [3]int
	noAckErr                                                                                                 error
	initialOwnerSnapshot                                                                                     clientconnect.TransferPayloadOwnerSnapshot
	initialOwnerSampled                                                                                      bool
}

// Namespaces independently assigned lifecycle tokens by their original Client.
type p2OriginToken struct {
	client clientconnect.Id
	token  uint64
}

// All observations are metadata or exact test-owned pooled witnesses.
type p2OriginState struct {
	stateLock     sync.Mutex
	changed       chan struct{}
	err           error
	items         map[string]*p2OriginItem
	current       map[clientconnect.Id]string
	lifecycle     map[p2OriginToken]string
	noAckTokens   map[p2OriginToken]string
	messageLabels map[clientconnect.Id][]string
	witnesses     map[*byte][]byte
	retryHeld     bool
	// Zero keeps the sealed R4 bound; full native-queue roots set a finite cap.
	witnessLimit             int
	nativeFillerApplications int
}

// One actual Client and its actual platform Resident, with public carrier routes.
type p2OriginEndpoint struct {
	id                    server.Id
	client                *clientconnect.Client
	resident              *Resident
	payload               *clientconnect.TransferPayloadOwnerLedger
	toClient              chan []byte
	toResident            chan []byte
	incomingForward       chan []byte
	removeTransport       func()
	removeIncomingForward func()
}

// Exact test owner; all route pumps, clients and resident work join before census.
type p2OriginFixture struct {
	t                                                testing.TB
	ctx                                              context.Context
	cancel                                           context.CancelFunc
	exchange                                         *Exchange
	settings                                         *ExchangeSettings
	ledger                                           *residentPayloadLedger
	sdkLedger                                        *clientconnect.TransferPayloadOwnerLedger
	state                                            p2OriginState
	endpoints                                        []*p2OriginEndpoint
	source, second, cold, victim, healthy, collision *p2OriginEndpoint
	forwards                                         []*ResidentForward
	outgoing                                         []clientconnect.Route
	workers                                          sync.WaitGroup
	coldEntered                                      chan struct{}
	coldRelease                                      chan struct{}
	retryRelease                                     chan struct{}
	releaseColdOnce, releaseRetryOnce                sync.Once
	postgresAttempts                                 func() uint64
	stopPostgresTripwire                             func()
	poolViolations                                   uint64
	expected                                         map[string]int
	coldOwner                                        *p2OriginEndpoint
	configureBeforeTripwire                          func()
	beforeForward                                    func(*p2OriginEndpoint, p2OriginWire)
	afterForward                                     func(*p2OriginEndpoint, p2OriginForwardObservation)
	observeReceiveProgress                           func(clientconnect.TransferProgressEvent)
	afterJoined                                      func(*p2OriginState)
	beforeRouteDelivery                              func(p2OriginWire)
	configureSettings                                func(*ExchangeSettings)
	configureEndpoint                                func(*p2OriginEndpoint) bool
	startNativeForward                               func(*p2OriginEndpoint, *p2OriginEndpoint, *ResidentForward) bool
	deliverOutgoing                                  func(*p2OriginEndpoint, []byte) bool
	afterOwnersJoined                                func(context.Context)
	observeApplicationWithLock                       func(*p2OriginState, string)
	joinNativeOwners                                 func(context.Context) bool
}

// Real warmed original retains custody and automatically retries the refused Pack.
func TestP2RealOriginWarmAckRetryPreservesUnrelatedApplications(t *testing.T) {
	runP2RealOriginRefusal(t, true, false)
}

// The first original on a cold victim sequence recovers without a generation reset.
func TestP2RealOriginColdAckRetryPreservesUnrelatedApplications(t *testing.T) {
	runP2RealOriginRefusal(t, false, false)
}

// Actual NoAck route completion remains distinct from delivery after relay refusal.
func TestP2RealOriginNoAckRefusalIsCountedLossNotDelivery(t *testing.T) {
	runP2RealOriginRefusal(t, false, true)
}

// Both positive scenarios use the same saturation and application-level controls.
func runP2RealOriginRefusal(t *testing.T, warm, noAck bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
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
			},
		}
		defer f.close()
		f.setup()
		if warm {
			f.send(f.source, f.victim, "warm", false)
			f.waitDelivered("warm", true)
			f.expected["warm"] = 1
		}

		f.send(f.source, f.cold, "cold-first", true)
		f.waitIngress("cold-first", 1)
		select {
		case <-f.coldEntered:
		case <-f.ctx.Done():
			t.Fatal("actual cold source frame did not reach the real allowance seam")
		}
		queue := f.source.resident.forwardIngress[int(f.cold.id[15])%len(f.source.resident.forwardIngress)].queue
		capacity := cap(queue)
		if capacity != (f.settings.ResidentForwardQueueSize+f.settings.ResidentForwardQueueShardCount-1)/f.settings.ResidentForwardQueueShardCount {
			t.Fatalf("actual configured shard capacity=%d settings=%+v", capacity, f.settings)
		}
		t.Logf("default ingress=%d shards=%d actual cold capacity=%d output=%d",
			f.settings.ResidentForwardQueueSize, f.settings.ResidentForwardQueueShardCount,
			capacity, f.settings.ForwardBufferSize)
		f.expected["cold-first"] = 1
		for index := range capacity {
			destination := f.cold
			if index == capacity-1 {
				destination = f.collision
			}
			label := fmt.Sprintf("fill-%d", index)
			f.send(f.source, destination, label, true)
			f.waitIngress(label, 1)
			f.expected[label] = 1
		}
		// Sole consumer is explicitly held: this is a stable invariant, not polling.
		if len(queue) != capacity {
			t.Fatal("actual callback shard was not full at its held cold owner")
		}
		f.send(f.source, f.victim, "victim", noAck)
		f.wait(func(s *p2OriginState) bool { return s.items["victim"].drops == 1 }, "exact initial refusal")
		f.requireLive()
		f.checkRefusedCustody(noAck, true)

		// These must complete before either held owner is released. The same-source
		// route is distinct, but both use that source's same resident Client pump.
		f.send(f.source, f.healthy, "healthy-same-source", false)
		f.waitDelivered("healthy-same-source", true)
		f.send(f.second, f.healthy, "healthy-second-source", false)
		f.waitDelivered("healthy-second-source", true)
		f.expected["healthy-same-source"], f.expected["healthy-second-source"] = 1, 1
		f.requireLive()
		if !noAck {
			// The sender's real recovery timer/worker performs this write. Its
			// dedicated physical-route pump retains it before callback admission.
			f.wait(func(s *p2OriginState) bool {
				item := s.items["victim"]
				return item.retryWrites >= 1 && s.retryHeld
			}, "original sender automatic retry")
			f.checkRefusedCustody(false, false)
		}

		f.releaseColdOnce.Do(func() { close(f.coldRelease) })
		for label := range f.expected {
			f.waitDelivered(label, label == "warm" || label == "healthy-same-source" || label == "healthy-second-source")
		}
		if noAck {
			// Future app progress and joined final census bound the loss assertion;
			// absence for a short timeout is never used as a no-retry proof.
			f.send(f.source, f.victim, "following-noack", true)
			f.waitDelivered("following-noack", false)
			f.send(f.source, f.victim, "following-ack", false)
			f.waitDelivered("following-ack", true)
			f.expected["victim"] = 0
			f.expected["following-noack"], f.expected["following-ack"] = 1, 1
			f.wait(func(s *p2OriginState) bool {
				item := s.items["victim"]
				return item.noAckPhases[clientconnect.NoAckSendPhaseCompleted] == 1 &&
					item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] == 1 &&
					item.terminalCount == 1
			}, "actual NoAck original completion")
		} else {
			f.releaseRetryOnce.Do(func() { close(f.retryRelease) })
			f.waitDelivered("victim", true)
			f.expected["victim"] = 1
		}
		f.requireLive()
		f.checkFinal(noAck)
	})
}

// Construct owners before the tripwire; no ordinary packet may acquire PostgreSQL.
func (self *p2OriginFixture) setup() {
	self.settings = DefaultExchangeSettings()
	self.settings.EnableNetworkPeers = false
	self.settings.KeyEventDelivery.Enabled = false
	self.settings.ContractHoleCompatibilityFallback = true
	self.ledger = &residentPayloadLedger{}
	self.sdkLedger = &clientconnect.TransferPayloadOwnerLedger{}
	self.settings.payloadOwnerLedger = self.ledger
	self.settings.SDKPayloadOwnerLedger = self.sdkLedger
	if self.configureSettings != nil {
		self.configureSettings(self.settings)
	}
	self.exchange = NewExchange(self.ctx, "p2-real-origin.example", "connect", "synthetic", nil, nil, self.settings)
	if !self.settings.ForwardEnforceActiveContracts || self.settings.ForwardTimeout != 0 ||
		self.settings.ResidentForwardQueueShardCount != 16 {
		self.t.Fatal("fixture must preserve frozen authorization, zero-wait output and shard settings")
	}
	for _, suffix := range []byte{2, 3, 0, 16, 1, 32} {
		self.newEndpoint(suffix)
	}
	self.source, self.second, self.cold, self.victim, self.healthy, self.collision =
		self.endpoints[0], self.endpoints[1], self.endpoints[2], self.endpoints[3], self.endpoints[4], self.endpoints[5]
	for _, endpoints := range [][2]*p2OriginEndpoint{
		{self.source, self.cold}, {self.source, self.victim}, {self.source, self.healthy},
		{self.source, self.collision}, {self.second, self.healthy},
	} {
		self.link(endpoints[0], endpoints[1])
		self.link(endpoints[1], endpoints[0])
	}
	if self.configureBeforeTripwire != nil {
		self.configureBeforeTripwire()
	}
	var coldOnce sync.Once
	coldOwner := self.source
	if self.coldOwner != nil {
		coldOwner = self.coldOwner
	}
	manager := coldOwner.resident.residentContractManager
	originalRead := manager.readContract
	manager.readContract = func(ctx context.Context, source, destination server.Id) residentContractAllowance {
		if destination == self.cold.id {
			coldOnce.Do(func() {
				close(self.coldEntered)
				select {
				case <-self.coldRelease:
				case <-ctx.Done():
				}
			})
		}
		return originalRead(ctx, source, destination)
	}
	self.postgresAttempts, self.stopPostgresTripwire = server.DenyPostgresForTest(self.t)
}

// Endpoint policy explicitly removes crypto/contract setup, not relay authorization.
// All default queue, resend, ACK lifetime, compression and window settings remain.
func (self *p2OriginFixture) newEndpoint(suffix byte) *p2OriginEndpoint {
	id := server.NewId()
	id[15] = suffix
	endpoint := &p2OriginEndpoint{id: id, payload: &clientconnect.TransferPayloadOwnerLedger{}}
	self.endpoints = append(self.endpoints, endpoint)
	settings := clientconnect.DefaultClientSettings()
	settings.Log = clientconnect.NewNoopLogger()
	settings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	settings.SkipClientKeyPublication = true
	settings.PayloadOwnerLedger = endpoint.payload
	settings.SendBufferSettings.NoAckSendObserver = self.observeNoAck
	settings.SendBufferSettings.SendPackLifecycleObserver = self.observeLifecycle
	settings.SendBufferSettings.TransferWireMessageObserver = self.observeWrite
	if self.observeReceiveProgress != nil {
		settings.ReceiveBufferSettings.ProgressObserver = self.observeReceiveProgress
	}
	endpoint.client = clientconnect.NewClient(self.ctx, clientconnect.Id(id), clientconnect.NewNoContractClientOob(), settings)
	endpoint.resident = NewResident(self.ctx, self.exchange, id, server.NewId(), server.NewId())
	var err error
	if self.configureEndpoint == nil || !self.configureEndpoint(endpoint) {
		endpoint.toClient, endpoint.toResident, endpoint.removeTransport, err =
			endpoint.resident.AddTransportWithProperties(clientconnect.TransferCarrierProperties{
				ReceiveReliability: clientconnect.CarrierReliabilityReliable,
			})
		if err != nil {
			self.t.Fatal(err)
		}
	}
	endpoint.incomingForward, endpoint.removeIncomingForward, err = endpoint.resident.AddForward()
	if err != nil {
		self.t.Fatal(err)
	}
	endpoint.client.RouteManager().UpdateTransportWithProperties(
		clientconnect.NewReceiveGatewayTransportWithType(clientconnect.TransportTypeH1),
		[]clientconnect.Route{endpoint.toClient},
		clientconnect.TransferCarrierProperties{ReceiveReliability: clientconnect.CarrierReliabilityReliable},
	)
	endpoint.client.AddReceiveCallback(func(source clientconnect.TransferPath, frames []*protocol.Frame, peer clientconnect.Peer) {
		for _, frame := range frames {
			message, err := clientconnect.FromFrame(frame)
			if err != nil {
				self.fail(fmt.Errorf("decode actual application: %w", err))
				continue
			}
			app, ok := message.(*protocol.SimpleMessage)
			if !ok {
				self.fail(fmt.Errorf("unexpected application %T", message))
				continue
			}
			self.mutate(func(s *p2OriginState) {
				item := s.items[app.Content]
				if item == nil {
					s.err = fmt.Errorf("unknown actual application %q", app.Content)
					return
				}
				if item.identity.source != source.SourceId || item.identity.destination != clientconnect.Id(endpoint.id) ||
					item.identity.lane != peer.TransferKey.LogicalLane || item.identity.forceStream != peer.TransferKey.ForceStream ||
					item.identity.companion != peer.TransferKey.CompanionContract {
					s.err = fmt.Errorf("application source/destination/lane changed for %q", app.Content)
				}
				item.applications++
				if self.observeApplicationWithLock != nil {
					self.observeApplicationWithLock(s, app.Content)
				}
				if item.applications != 1 {
					s.err = fmt.Errorf("duplicate application %q", app.Content)
				}
			})
		}
	})
	// Preserve the constructor's exact handler via a test-only public callback
	// subscription wrapper. It adds an after-return observation, not admission.
	endpoint.resident.clientForwardUnsub()
	endpoint.resident.clientForwardUnsub = endpoint.resident.client.AddForwardCallback(func(path clientconnect.TransferPath, wire []byte) {
		decoded, err := p2DecodeOriginWire(wire)
		if err != nil {
			self.fail(err)
			endpoint.resident.handleClientForward(path, wire)
			return
		}
		self.mutate(func(s *p2OriginState) {
			p2OriginKeepWitness(s, wire)
			for _, label := range decoded.labels {
				if item := s.items[label]; item != nil {
					item.ingressStarted++
				}
			}
		})
		if self.beforeForward != nil {
			self.beforeForward(endpoint, decoded)
		}
		messagesBefore, bytesBefore := p2OriginDrops()
		before := p2OriginIngressGroup(self.ledger, endpoint.id[15])
		endpoint.resident.handleClientForward(path, wire)
		after := p2OriginIngressGroup(self.ledger, endpoint.id[15])
		retired := endpoint.resident.IsDone()
		messagesAfter, bytesAfter := p2OriginDrops()
		self.mutate(func(s *p2OriginState) {
			if decoded.ack {
				return
			}
			for _, label := range decoded.labels {
				item := s.items[label]
				if item == nil {
					s.err = fmt.Errorf("callback observed unknown app %q", label)
					continue
				}
				item.ingress++
				if messagesAfter == messagesBefore {
					continue
				}
				item.drops++
				item.lastDropBytes = len(wire)
				if retired {
					s.err = fmt.Errorf("P2 OLD discriminator: full complete frame %q retired its resident", label)
					continue
				}
				if len(decoded.labels) != 1 || messagesAfter-messagesBefore != 1 ||
					bytesAfter-bytesBefore != float64(len(wire)) {
					s.err = fmt.Errorf("complete refusal accounting for %q was not one exact wire", label)
				}
				a, b := before, after
				if !validResidentPayloadGroup(a) || !validResidentPayloadGroup(b) || a.Messages != b.Messages ||
					a.LogicalBytes != b.LogicalBytes || a.BackingCharge != b.BackingCharge ||
					b.Admitted-a.Admitted != 1 || b.Released-a.Released != 1 {
					s.err = fmt.Errorf("refusal did not roll back exactly its added ingress share: %+v -> %+v", a, b)
				}
			}
		})
		if self.afterForward != nil {
			self.afterForward(endpoint, p2OriginForwardObservation{
				path: path, wire: decoded, wireBytes: len(wire),
				messagesBefore: messagesBefore, messagesAfter: messagesAfter,
				bytesBefore: bytesBefore, bytesAfter: bytesAfter,
				before: before, after: after, retired: retired,
			})
		}
	})
	return endpoint
}

// One logical direction has a dedicated bounded physical route so retaining its
// retry cannot manufacture a test-only head-of-line block for unrelated peers.
func (self *p2OriginFixture) link(source, destination *p2OriginEndpoint) {
	source.client.ContractManager().AddNoContractPeer(clientconnect.Id(destination.id))
	destination.client.ContractManager().AddNoContractPeer(clientconnect.Id(source.id))
	server.Redis(self.ctx, func(client server.RedisClient) {
		residentPacketSeedHole(self.ctx, client, source.id, destination.id, "1")
	})
	outgoing := make(clientconnect.Route, 128)
	self.outgoing = append(self.outgoing, outgoing)
	source.client.RouteManager().UpdateTransport(
		clientconnect.NewSendClientTransport(clientconnect.DestinationId(clientconnect.Id(destination.id))),
		[]clientconnect.Route{outgoing},
	)
	forward := NewResidentForward(source.resident.ctx, self.exchange, destination.id)
	source.resident.stateLock.Lock()
	source.resident.forwards[destination.id] = forward
	source.resident.stateLock.Unlock()
	self.forwards = append(self.forwards, forward)
	nativeForward := self.startNativeForward != nil && self.startNativeForward(source, destination, forward)
	self.workers.Add(1)
	go func() {
		defer self.workers.Done()
		victimSeen := false
		for {
			select {
			case <-self.ctx.Done():
				return
			case wire := <-outgoing:
				decoded, err := p2DecodeOriginWire(wire)
				if err != nil {
					self.fail(err)
					clientconnect.MessagePoolReturn(wire)
					continue
				}
				hold := false
				self.mutate(func(s *p2OriginState) {
					p2OriginKeepWitness(s, wire)
					if decoded.ack {
						self.recordAckWithLock(s, decoded)
					} else {
						self.recordPhysicalWithLock(s, decoded)
					}
					for _, label := range decoded.labels {
						if label == "victim" && !decoded.noAck {
							if victimSeen {
								hold, s.retryHeld = true, true
							}
							victimSeen = true
						}
					}
				})
				if self.beforeRouteDelivery != nil {
					self.beforeRouteDelivery(decoded)
				}
				if hold {
					select {
					case <-self.retryRelease:
					case <-self.ctx.Done():
						clientconnect.MessagePoolReturn(wire)
						return
					}
				}
				if self.deliverOutgoing != nil && self.deliverOutgoing(source, wire) {
					continue
				}
				select {
				case source.toResident <- wire:
				case <-self.ctx.Done():
					clientconnect.MessagePoolReturn(wire)
					return
				}
			}
		}
	}()
	if nativeForward {
		return
	}
	self.workers.Add(1)
	go func() {
		defer self.workers.Done()
		for {
			select {
			case <-self.ctx.Done():
				return
			case wire := <-forward.send:
				shared := clientconnect.MessagePoolShareReadOnly(wire)
				forward.releasePayload(wire)
				select {
				case destination.incomingForward <- shared:
				case <-self.ctx.Done():
					clientconnect.MessagePoolReturn(shared)
					return
				}
			}
		}
	}()
}

// Uses the actual public send, preserving rejected input ownership.
func (self *p2OriginFixture) send(source, destination *p2OriginEndpoint, label string, noAck bool) {
	self.mutate(func(s *p2OriginState) {
		if s.items[label] != nil {
			s.err = fmt.Errorf("duplicate test label %q", label)
		}
		s.items[label] = &p2OriginItem{noAck: noAck}
		s.current[clientconnect.Id(source.id)] = label
	})
	frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: label})
	self.mutate(func(s *p2OriginState) { p2OriginKeepWitness(s, frame.MessageBytes) })
	var options []any
	if noAck {
		options = append(options, clientconnect.NoAck())
	}
	accepted, err := source.client.SendWithTimeoutDetailed(frame, clientconnect.Id(destination.id), func(err error) {
		self.mutate(func(s *p2OriginState) {
			item := s.items[label]
			item.terminalCount++
			item.terminalErr = err
		})
	}, 5*time.Second, options...)
	if !accepted || err != nil {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		self.t.Fatalf("actual public send %q accepted=%t err=%v", label, accepted, err)
	}
}

// Wire observation proves recovery was generated by the sender, not fixture replay.
func (self *p2OriginFixture) observeWrite(observation clientconnect.TransferWireMessageObservation) {
	wire, err := p2DecodeOriginWire(observation.TransferFrameBytes)
	if err != nil {
		self.fail(err)
		return
	}
	self.mutate(func(s *p2OriginState) {
		for _, label := range wire.labels {
			item := s.items[label]
			if item == nil {
				s.err = fmt.Errorf("original writer observed unknown app %q", label)
				continue
			}
			if item.identitySet && item.identity != wire.identity {
				s.err = fmt.Errorf("automatic retry changed logical identity for %q", label)
			}
			item.identity, item.identitySet = wire.identity, true
			if label == "victim" && !item.initialOwnerSampled {
				// The original sender worker has serialized and charged this item,
				// but has not exposed its first wire to any physical route yet.
				item.initialOwnerSnapshot = self.source.payload.Snapshot()
				item.initialOwnerSampled = true
			}
			item.wireAttempts++
			if observation.Resend {
				item.retryWrites++
			}
			if item.noAck != wire.noAck {
				s.err = fmt.Errorf("requested NoAck=%t wire NoAck=%t for %q", item.noAck, wire.noAck, label)
			}
		}
	})
}

// The actual accepted route, not an optional writer observer, is the census for
// every Pack. Ordinary NoAck fast writes intentionally omit the wire observer.
func (self *p2OriginFixture) recordPhysicalWithLock(s *p2OriginState, wire p2OriginWire) {
	for _, label := range wire.labels {
		item := s.items[label]
		if item == nil {
			s.err = fmt.Errorf("physical route observed unknown app %q", label)
			continue
		}
		if item.identitySet && item.identity != wire.identity {
			s.err = fmt.Errorf("physical wire changed logical identity for %q", label)
		}
		item.identity, item.identitySet = wire.identity, true
		item.wireWrites++
		if item.noAck != wire.noAck {
			s.err = fmt.Errorf("physical NoAck=%t requested=%t for %q", wire.noAck, item.noAck, label)
		}
		if label == "victim" && !item.initialOwnerSampled {
			if !item.noAck {
				s.err = fmt.Errorf("Ack victim reached route without original writer custody observation")
			}
			item.initialOwnerSnapshot = self.source.payload.Snapshot()
			item.initialOwnerSampled = true
		}
	}
	s.messageLabels[wire.identity.message] = append([]string(nil), wire.labels...)
}

// Started callbacks are synchronous inside the public call; later phases use the
// captured per-Client token, never the next send's mutable current-label slot.
func (self *p2OriginFixture) observeLifecycle(observation clientconnect.SendPackLifecycleObservation) {
	self.mutate(func(s *p2OriginState) {
		key := p2OriginToken{client: observation.ClientId, token: observation.Token}
		if observation.Phase == clientconnect.SendPackLifecyclePhaseStarted {
			s.lifecycle[key] = s.current[observation.ClientId]
		}
		label := s.lifecycle[key]
		item := s.items[label]
		if item == nil || int(observation.Phase) >= len(item.lifecycle) {
			s.err = fmt.Errorf("unpaired lifecycle event %+v", observation)
			return
		}
		item.lifecycle[observation.Phase]++
		item.lifecycleErr[observation.Phase] = observation.Err
		if observation.AckRequired == item.noAck || item.lifecycle[observation.Phase] != 1 {
			s.err = fmt.Errorf("incorrect or duplicate lifecycle event for %q", label)
		}
	})
}

// These phase pairs prove first-route disposition, expressly not application delivery.
func (self *p2OriginFixture) observeNoAck(observation clientconnect.NoAckSendObservation) {
	self.mutate(func(s *p2OriginState) {
		key := p2OriginToken{client: observation.ClientId, token: observation.Token}
		if observation.Phase == clientconnect.NoAckSendPhaseStarted {
			s.noAckTokens[key] = s.current[observation.ClientId]
		}
		label := s.noAckTokens[key]
		item := s.items[label]
		if item == nil || int(observation.Phase) >= len(item.noAckPhases) || !item.noAck {
			s.err = fmt.Errorf("unpaired NoAck observation %+v", observation)
			return
		}
		item.noAckPhases[observation.Phase]++
		item.noAckErr = observation.Err
		if item.noAckPhases[observation.Phase] != 1 {
			s.err = fmt.Errorf("duplicate NoAck lifecycle phase for %q", label)
		}
	})
}

// Every actual receiver ACK is checked before forwarding, including selective ACKs.
// A cumulative ACK cannot cover an undelivered original in this one-item-at-a-time fixture.
func (self *p2OriginFixture) recordAckWithLock(s *p2OriginState, wire p2OriginWire) {
	labels := s.messageLabels[wire.identity.message]
	if len(labels) == 0 {
		s.err = fmt.Errorf("actual ACK has no observed original message %s", wire.identity.message)
		return
	}
	for _, label := range labels {
		item := s.items[label]
		if item.identity.sequence != wire.identity.sequence ||
			item.identity.source != wire.identity.destination || item.identity.destination != wire.identity.source {
			s.err = fmt.Errorf("ACK changed sequence/source/destination for %q", label)
		}
		if item.noAck {
			s.err = fmt.Errorf("NoAck original %q received a Transfer ACK", label)
		}
		if label == "victim" && item.drops != 0 && item.ingressStarted == 1 {
			s.err = fmt.Errorf("ACK escaped for refused victim before any retry admission")
		}
		if wire.selective {
			item.selectiveAcks++
		} else {
			item.acks++
			if item.applications != 1 {
				s.err = fmt.Errorf("cumulative ACK escaped before actual application delivery for %q", label)
			}
			for earlierLabel, earlier := range s.items {
				if !earlier.noAck && earlier.identitySet &&
					earlier.identity.sequence == item.identity.sequence &&
					earlier.identity.source == item.identity.source &&
					earlier.identity.destination == item.identity.destination &&
					earlier.identity.number <= item.identity.number && earlier.applications != 1 {
					s.err = fmt.Errorf("cumulative ACK covered undelivered original %q", earlierLabel)
				}
			}
		}
	}
}

// Decodes both public protocol encodings without retaining borrowed source bytes.
func p2DecodeOriginWire(bytes []byte) (p2OriginWire, error) {
	var frame protocol.TransferFrame
	if err := clientconnect.ProtoUnmarshal(bytes, &frame); err != nil {
		return p2OriginWire{}, err
	}
	if frame.TransferPath == nil {
		return p2OriginWire{}, fmt.Errorf("missing actual Transfer path")
	}
	source, err := clientconnect.IdFromBytes(frame.TransferPath.SourceId)
	if err != nil {
		return p2OriginWire{}, err
	}
	destination, err := clientconnect.IdFromBytes(frame.TransferPath.DestinationId)
	if err != nil {
		return p2OriginWire{}, err
	}
	pack, ack := frame.Pack, frame.Ack
	if frame.Frame != nil {
		switch frame.Frame.MessageType {
		case protocol.MessageType_TransferPack:
			pack = &protocol.Pack{}
			if err := clientconnect.ProtoUnmarshal(frame.Frame.MessageBytes, pack); err != nil {
				return p2OriginWire{}, err
			}
		case protocol.MessageType_TransferAck:
			ack = &protocol.Ack{}
			if err := clientconnect.ProtoUnmarshal(frame.Frame.MessageBytes, ack); err != nil {
				return p2OriginWire{}, err
			}
		}
	}
	result := p2OriginWire{byteCount: len(bytes), identity: p2OriginIdentity{
		source: source, destination: destination,
		sessionRole: frame.GetSessionRole(), sessionCompanion: frame.GetSessionCompanion(),
	}}
	if ack != nil {
		result.ack, result.selective = true, ack.Selective
		result.identity.sequence, err = clientconnect.IdFromBytes(ack.SequenceId)
		if err != nil {
			return result, err
		}
		result.identity.message, err = clientconnect.IdFromBytes(ack.MessageId)
		return result, err
	}
	if pack == nil {
		return result, fmt.Errorf("actual wire is neither Pack nor ACK")
	}
	result.identity.sequence, err = clientconnect.IdFromBytes(pack.SequenceId)
	if err != nil {
		return result, err
	}
	result.identity.message, err = clientconnect.IdFromBytes(pack.MessageId)
	if err != nil {
		return result, err
	}
	result.identity.number, result.identity.lane = pack.SequenceNumber, pack.LogicalLane
	result.identity.forceStream, result.identity.companion = pack.ForceStream, pack.CompanionContract
	result.noAck = pack.Nack
	for _, appFrame := range pack.Frames {
		message, err := clientconnect.FromFrame(appFrame)
		if err != nil {
			return result, err
		}
		app, ok := message.(*protocol.SimpleMessage)
		if !ok {
			return result, fmt.Errorf("unexpected original frame %T", message)
		}
		result.labels = append(result.labels, app.Content)
	}
	return result, nil
}

// Fixed counters; differences are checked only at the forced full-shard boundary.
func p2OriginDrops() (float64, float64) {
	var messages, bytes dto.Metric
	_ = receiveQueueDroppedMessagesCounter.WithLabelValues(receiveQueueBoundaryResidentClientForward).Write(&messages)
	_ = receiveQueueDroppedBytesCounter.WithLabelValues(receiveQueueBoundaryResidentClientForward).Write(&bytes)
	return messages.GetCounter().GetValue(), bytes.GetCounter().GetValue()
}

// Borrows input bytes and retains at most one test-owned share for each exact
// visible root. Joined cleanup returns these after every production owner ends.
func p2OriginKeepWitness(s *p2OriginState, bytes []byte) {
	if len(bytes) == 0 || s.witnesses[&bytes[0]] != nil {
		return
	}
	witnessLimit := s.witnessLimit
	if witnessLimit == 0 {
		witnessLimit = 4096
	}
	if len(s.witnesses) >= witnessLimit {
		s.err = fmt.Errorf("bounded pooled witness inventory overflow")
		return
	}
	s.witnesses[&bytes[0]] = clientconnect.MessagePoolShareReadOnly(bytes)
}

// At the initial refusal, the cold worker barrier and serial source callback
// exclude concurrent writes to this exact ingress group. Other owners/output
// stages are deliberately excluded; final full-ledger snapshots happen joined.
func p2OriginIngressGroup(ledger *residentPayloadLedger, owner byte) residentPayloadGroup {
	group := &ledger.shards[int(owner)%residentPayloadShardCount].groups[residentPayloadForwardIngress]
	return residentPayloadGroup{
		Messages: group.messages.Load(), LogicalBytes: group.logical.Load(),
		BackingCharge: group.backing.Load(), Admitted: group.admitted.Load(), Released: group.released.Load(),
	}
}

// Mutations notify under the same lock used by the predicate/subscription.
func (self *p2OriginFixture) mutate(update func(*p2OriginState)) {
	self.state.stateLock.Lock()
	defer self.state.stateLock.Unlock()
	update(&self.state)
	close(self.state.changed)
	self.state.changed = make(chan struct{})
}

// Failure is reported by the test goroutine, never by Goexit in a product worker.
func (self *p2OriginFixture) fail(err error) {
	self.mutate(func(s *p2OriginState) {
		if s.err == nil {
			s.err = err
		}
	})
}

// Positive event/state waits only; the context is an ordinary failure guard.
func (self *p2OriginFixture) wait(predicate func(*p2OriginState) bool, description string) {
	self.t.Helper()
	for {
		self.state.stateLock.Lock()
		changed := self.state.changed
		err := self.state.err
		ready := predicate(&self.state)
		self.state.stateLock.Unlock()
		if err != nil {
			self.t.Fatal(err)
		}
		if ready {
			return
		}
		select {
		case <-changed:
		case <-self.ctx.Done():
			self.t.Fatalf("wait for %s: %v", description, self.ctx.Err())
		}
	}
}

// Callback-return observation is distinct from source route completion.
func (self *p2OriginFixture) waitIngress(label string, count int) {
	self.wait(func(s *p2OriginState) bool { return s.items[label].ingress >= count }, label+" callback return")
}

// Application effects and actual peer ACK termination are both required for Ack sends.
func (self *p2OriginFixture) waitDelivered(label string, ack bool) {
	self.wait(func(s *p2OriginState) bool {
		item := s.items[label]
		if item.applications != 1 {
			return false
		}
		return !ack || item.acks > 0 && item.terminalCount == 1 &&
			item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] == 1
	}, label+" application/terminal")
	self.state.stateLock.Lock()
	item := *self.state.items[label]
	self.state.stateLock.Unlock()
	if item.terminalErr != nil {
		self.t.Fatalf("%q actual completion error: %v", label, item.terminalErr)
	}
}

// The writer snapshot proves initial retention; absence of a terminal event and
// the later genuine retry prove continued original custody. Do not race a live
// ledger sample against automatic envelope rewriting or unrelated Ack cleanup.
func (self *p2OriginFixture) checkRefusedCustody(noAck, checkLedger bool) {
	self.state.stateLock.Lock()
	item := *self.state.items["victim"]
	self.state.stateLock.Unlock()
	if item.applications != 0 || item.acks != 0 || item.selectiveAcks != 0 || item.drops != 1 {
		self.t.Fatalf("refused original escaped its ownership boundary: %+v", item)
	}
	if checkLedger {
		snapshot := item.initialOwnerSnapshot
		want := int64(1)
		if noAck {
			want = 0
		}
		if !item.initialOwnerSampled || !snapshot.Complete || snapshot.SendAck.Owners != want {
			self.t.Fatalf("source retained Ack owner=%+v want=%d", snapshot, want)
		}
	}
	if !noAck && (item.terminalCount != 0 || item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] != 0) {
		self.t.Fatal("refused Ack original completed before real receiver authority")
	}
}

// No owner identity is replaced by the fixture, and all must stay live.
func (self *p2OriginFixture) requireLive() {
	if self.exchange.ctx.Err() != nil {
		self.t.Fatal("complete-frame refusal retired Exchange")
	}
	for _, endpoint := range self.endpoints {
		if endpoint.resident.IsDone() || endpoint.client.IsDone() || endpoint.resident.TransportCount() != 1 {
			self.t.Fatal("complete-frame refusal retired an original endpoint/resident/transport")
		}
	}
	for _, forward := range self.forwards {
		if forward.IsDone() {
			self.t.Fatal("complete-frame refusal retired an existing forward")
		}
	}
}

// Final behavior checks do not infer delivery from first-route write completion.
func (self *p2OriginFixture) checkFinal(noAck bool) {
	self.state.stateLock.Lock()
	defer self.state.stateLock.Unlock()
	if self.state.err != nil {
		self.t.Fatal(self.state.err)
	}
	item := self.state.items["victim"]
	if warm := self.state.items["warm"]; warm != nil &&
		(item.identity.sequence != warm.identity.sequence || item.identity.number <= warm.identity.number) {
		self.t.Fatal("warm retry case did not reuse the acknowledged original sequence")
	}
	if item.drops != 1 || item.lastDropBytes <= 0 || item.terminalCount != 1 || item.terminalErr != nil {
		self.t.Fatalf("victim disposition: %+v", item)
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted,
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
		clientconnect.SendPackLifecyclePhaseTerminal,
	} {
		if item.lifecycle[phase] != 1 || item.lifecycleErr[phase] != nil {
			self.t.Fatalf("victim lifecycle phase %d: %+v", phase, item)
		}
	}
	if noAck {
		if item.wireWrites != 1 || item.retryWrites != 0 || item.applications != 0 || item.acks != 0 ||
			item.noAckPhases[clientconnect.NoAckSendPhaseStarted] != 1 ||
			item.noAckPhases[clientconnect.NoAckSendPhaseCompleted] != 1 || item.noAckErr != nil {
			self.t.Fatalf("NoAck route completion was confused with delivery/retry: %+v", item)
		}
	} else if item.retryWrites < 1 || item.wireWrites < 2 || item.applications != 1 || item.acks == 0 {
		self.t.Fatalf("actual automatic retry/receiver ACK proof missing: %+v", item)
	}
}

// Stop admission, release barriers, join producers/consumers, then reconcile exact
// pooled witnesses and all enabled ledgers. No running worker is sampled as empty.
func (self *p2OriginFixture) close() {
	self.releaseColdOnce.Do(func() { close(self.coldRelease) })
	self.releaseRetryOnce.Do(func() { close(self.retryRelease) })
	self.cancel()
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer joinCancel()
	joined := true
	for _, endpoint := range self.endpoints {
		if endpoint.client != nil {
			endpoint.client.Cancel()
		}
		if endpoint.resident != nil {
			endpoint.resident.Close()
		}
	}
	if err := waitForWorkerGroup(joinCtx, &self.workers, "p2 real-origin physical pumps"); err != nil {
		joined = false
		self.t.Error(err)
	}
	for _, endpoint := range self.endpoints {
		if endpoint.client != nil {
			if err := endpoint.client.CloseAndWait(joinCtx); err != nil {
				joined = false
				self.t.Error(err)
			}
		}
		if endpoint.resident != nil {
			if err := endpoint.resident.CloseAndWait(joinCtx); err != nil {
				joined = false
				self.t.Error(err)
			}
		}
		if joined && endpoint.removeIncomingForward != nil {
			endpoint.removeIncomingForward()
		}
		if endpoint.removeTransport != nil {
			endpoint.removeTransport()
		}
	}
	if joined && self.joinNativeOwners != nil && !self.joinNativeOwners(joinCtx) {
		joined = false
		self.t.Error("native accepted connection owners did not join before queue drainage")
	}
	if joined {
		for _, forward := range self.forwards {
			forward.Close()
		}
		for _, route := range self.outgoing {
			returnReadyPooledMessages(route)
		}
		for _, endpoint := range self.endpoints {
			returnReadyPooledMessages(endpoint.toClient)
			returnReadyPooledMessages(endpoint.toResident)
		}
	}
	if self.exchange != nil {
		self.exchange.Close()
		if !self.exchange.WaitForIdle(joinCtx) {
			joined = false
			self.t.Error("Exchange did not join")
		}
	}
	if joined && self.afterOwnersJoined != nil {
		self.afterOwnersJoined(joinCtx)
	}
	if self.postgresAttempts != nil && self.postgresAttempts() != 0 {
		self.t.Errorf("packet/teardown PostgreSQL attempts=%d", self.postgresAttempts())
	}
	if self.stopPostgresTripwire != nil {
		self.stopPostgresTripwire()
	}
	if !joined {
		self.t.Error("ownership not qualified: an owner did not join")
		return
	}
	for _, endpoint := range self.endpoints {
		if endpoint.resident == nil {
			continue
		}
		if attempts := server.PacketPostgresAttempts(endpoint.resident.residentContractManager.ctx); attempts != 0 {
			self.t.Errorf("packet-context PostgreSQL attempts=%d", attempts)
		}
		snapshot := endpoint.payload.Snapshot()
		if !snapshot.Complete || snapshot.SendAck.Owners != 0 || snapshot.Forward.Owners != 0 ||
			snapshot.SendAck.AdmittedTotal != snapshot.SendAck.ReleasedTotal ||
			snapshot.Forward.AdmittedTotal != snapshot.Forward.ReleasedTotal {
			self.t.Errorf("joined original Client payload ledger: %+v", snapshot)
		}
	}
	if self.sdkLedger != nil {
		snapshot := self.sdkLedger.Snapshot()
		if !snapshot.Complete || snapshot.SendAck.Owners != 0 || snapshot.Forward.Owners != 0 {
			self.t.Errorf("joined resident Client payload ledger: %+v", snapshot)
		}
	}
	if self.ledger != nil {
		snapshot := self.ledger.snapshot()
		if !snapshot.Complete {
			self.t.Error("joined resident payload ledger incomplete")
		}
		for _, group := range snapshot.Groups {
			if group.Messages != 0 || group.LogicalBytes != 0 || group.BackingCharge != 0 || group.Admitted != group.Released {
				self.t.Errorf("joined resident payload group: %+v", group)
			}
		}
	}
	self.state.stateLock.Lock()
	defer self.state.stateLock.Unlock()
	for _, witness := range self.state.witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			self.t.Error("joined physical wire retained an owner besides its unique witness")
		}
	}
	if !self.t.Failed() {
		if self.state.err != nil {
			self.t.Error(self.state.err)
		}
		for label, expected := range self.expected {
			if got := self.state.items[label].applications; got != expected {
				self.t.Errorf("joined application count %q=%d want=%d", label, got, expected)
			}
		}
		item := self.state.items["victim"]
		if item != nil && item.noAck && (item.wireWrites != 1 || item.retryWrites != 0 || item.acks != 0) {
			self.t.Errorf("joined NoAck original acquired replay/ACK: %+v", item)
		}
		if self.afterJoined != nil {
			self.afterJoined(&self.state)
		}
	}
	if got := clientconnect.MessagePoolViolationCount(); got != self.poolViolations {
		self.t.Errorf("pooled ownership violations changed %d -> %d", self.poolViolations, got)
	}
}
