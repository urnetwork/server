// Finite genuine-ACK loss and explicit-owner-close controls for forward refusal.
// Uses real endpoint feedback without adding a production hook.
package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// All fields are guarded by the enclosing fixture's metadata lock.
type p2R4AckState struct {
	refusals, admissions, callbacks          int
	refusedBytes                             int
	receiverPackBegins, receiverPackAccepted int
	senderAckBegins, senderAckAccepted       int
}

// The lost object is an actual receiver-produced ACK, not a constructed frame.
func TestP2RealOriginForwardedAckRefusalRecoversWithOneApplication(t *testing.T) {
	var ackState p2R4AckState
	runP2R4Origin(t, func(f *p2OriginFixture) {
		// Global drop counters must not overlap A's data-callback sample and
		// V's later ACK refusal. Retain only this genuine outgoing ACK at its
		// dedicated physical pump until that exact callback return is observed.
		f.beforeRouteDelivery = func(wire p2OriginWire) {
			if !wire.ack {
				return
			}
			for {
				f.state.stateLock.Lock()
				changed := f.state.changed
				item := f.state.items["victim"]
				ready := item == nil || wire.identity.message != item.identity.message || item.ingress >= 1
				f.state.stateLock.Unlock()
				if ready {
					return
				}
				select {
				case <-changed:
				case <-f.ctx.Done():
					return
				}
			}
		}
		previousJoined := f.afterJoined
		f.afterJoined = func(s *p2OriginState) {
			previousJoined(s)
			item := s.items["victim"]
			if item == nil || item.applications != 1 || item.drops != 0 ||
				item.wireWrites < 2 || item.retryWrites < 1 || item.acks < 2 || item.selectiveAcks != 0 ||
				item.terminalCount != 1 || item.terminalErr != nil ||
				ackState.refusals != 1 || ackState.admissions < 1 ||
				ackState.senderAckAccepted < 1 || ackState.receiverPackAccepted < 2 {
				f.t.Errorf("R4 joined original/duplicate/ACK census: item=%+v ack=%+v", item, ackState)
			}
		}
		f.configureBeforeTripwire = func() {
			// ACK V16 -> A2 and cold V16 -> C18 both use V's actual shard 2.
			// Keep all original links; add actual owners before the tripwire.
			f.cold = f.newEndpoint(18)
			f.coldOwner = f.victim
			for _, pair := range [][2]*p2OriginEndpoint{
				{f.victim, f.cold}, {f.victim, f.healthy},
			} {
				f.link(pair[0], pair[1])
				f.link(pair[1], pair[0])
			}
		}
		f.afterForward = func(endpoint *p2OriginEndpoint, observation p2OriginForwardObservation) {
			if !observation.wire.ack || f.ctx.Err() != nil {
				return
			}
			f.mutate(func(s *p2OriginState) {
				item := s.items["victim"]
				if item == nil || observation.wire.identity.message != item.identity.message {
					return
				}
				wire := observation.wire
				if endpoint != f.victim || wire.identity.source != clientconnect.Id(f.victim.id) ||
					wire.identity.destination != clientconnect.Id(f.source.id) ||
					wire.identity.sequence != item.identity.sequence || wire.selective ||
					observation.path.SourceId != wire.identity.source ||
					observation.path.DestinationId != wire.identity.destination {
					s.err = fmt.Errorf("R4 genuine ACK did not keep exact reversed identity/cumulative custody")
					return
				}
				ackState.callbacks++
				if observation.messagesAfter == observation.messagesBefore {
					if observation.retired {
						s.err = fmt.Errorf("R4 recovered ACK callback had a retired owner")
					}
					ackState.admissions++
					return
				}
				ackState.refusals++
				ackState.refusedBytes = observation.wireBytes
				if observation.retired {
					s.err = fmt.Errorf("P2 R4 OLD discriminator: full genuine receiver ACK retired its resident")
					return
				}
				if observation.messagesAfter-observation.messagesBefore != 1 ||
					observation.bytesAfter-observation.bytesBefore != float64(observation.wireBytes) {
					s.err = fmt.Errorf("R4 refusal counters did not name one exact genuine ACK")
				}
				a, b := observation.before, observation.after
				if !validResidentPayloadGroup(a) || !validResidentPayloadGroup(b) ||
					a.Messages != b.Messages || a.LogicalBytes != b.LogicalBytes ||
					a.BackingCharge != b.BackingCharge ||
					b.Admitted-a.Admitted != 1 || b.Released-a.Released != 1 {
					s.err = fmt.Errorf("R4 ACK refusal did not return exactly its added pooled share: %+v -> %+v", a, b)
				}
				if item.applications != 1 || item.drops != 0 || item.terminalCount != 0 ||
					item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] != 0 ||
					ackState.senderAckBegins != 0 || ackState.senderAckAccepted != 0 {
					s.err = fmt.Errorf("R4 refused ACK was confused with refused data or released original custody")
				}
			})
		}
		// Existing public metadata seam only; never gate, sleep or retain bytes here.
		f.observeReceiveProgress = func(event clientconnect.TransferProgressEvent) {
			if event.Stage != "receive_pack_begin" && event.Stage != "receive_pack_end" &&
				event.Stage != "receive_ack_begin" && event.Stage != "receive_ack_end" {
				return
			}
			// Fixture shutdown is an explicit lifecycle, not a pressure failure.
			// Final application and pooled-owner census still runs after joins.
			if f.ctx.Err() != nil {
				return
			}
			f.mutate(func(s *p2OriginState) {
				item := s.items["victim"]
				if item == nil || !item.identitySet || event.MessageId != item.identity.message {
					return
				}
				if event.SequenceId != item.identity.sequence {
					s.err = fmt.Errorf("R4 endpoint progress changed original sequence")
					return
				}
				switch event.Stage {
				case "receive_pack_begin", "receive_pack_end":
					if event.ClientId != clientconnect.Id(f.victim.id) ||
						event.PeerId != clientconnect.Id(f.source.id) ||
						event.SequenceNumber != item.identity.number || event.NoAck {
						s.err = fmt.Errorf("R4 receiver Pack progress changed exact original identity")
						return
					}
					if event.Stage == "receive_pack_begin" {
						ackState.receiverPackBegins++
					} else if event.Success {
						ackState.receiverPackAccepted++
					} else {
						s.err = fmt.Errorf("R4 real receiver refused original/duplicate Pack: %+v", event)
					}
				case "receive_ack_begin", "receive_ack_end":
					if event.ClientId != clientconnect.Id(f.source.id) ||
						event.PeerId != clientconnect.Id(f.victim.id) || event.Selective {
						s.err = fmt.Errorf("R4 sender ACK progress changed reversed identity/cumulative custody")
						return
					}
					if event.Stage == "receive_ack_begin" {
						ackState.senderAckBegins++
					} else if event.Success &&
						(event.Outcome == "accepted" || event.Outcome == "accepted_after_wait") {
						ackState.senderAckAccepted++
					} else {
						s.err = fmt.Errorf("R4 genuine recovered ACK failed actual sender handoff: %+v", event)
					}
				}
			})
		}
	}, func(f *p2OriginFixture) {
		owner := f.victim
		shardCount := len(owner.resident.forwardIngress)
		ackShard := int(f.source.id[15]) % shardCount
		coldShard := int(f.cold.id[15]) % shardCount
		if shardCount != 16 || ackShard != 2 || coldShard != ackShard ||
			int(f.healthy.id[15])%shardCount == ackShard {
			f.t.Fatal("R4 actual ACK destination, cold destination and healthy shard topology mismatch")
		}
		f.send(owner, f.cold, "r4-ack-cold-first", true)
		f.waitIngress("r4-ack-cold-first", 1)
		p2R4WaitEvent(f, f.coldEntered, "receiver's actual cold allowance reader")
		queue := owner.resident.forwardIngress[coldShard].queue
		capacity := cap(queue)
		if capacity != 256 || capacity != f.settings.ResidentForwardQueueSize/shardCount {
			f.t.Fatalf("R4 actual default capacity=%d settings=%+v", capacity, f.settings)
		}
		f.expected["r4-ack-cold-first"] = 1
		for index := range capacity {
			destination := f.cold
			if index == capacity-1 {
				destination = f.source
			}
			label := fmt.Sprintf("r4-ack-fill-%d", index)
			f.send(owner, destination, label, true)
			f.waitIngress(label, 1)
			f.expected[label] = 1
		}
		// One held consumer gives a stable census; this is not a polling loop.
		if len(queue) != capacity {
			f.t.Fatal("R4 genuine ACK-origin shard was not actually saturated")
		}
		messagesBefore, bytesBefore := p2OriginDrops()
		f.send(f.source, f.victim, "victim", false)
		f.expected["victim"] = 1
		f.wait(func(s *p2OriginState) bool {
			return ackState.refusals == 1 && s.items["victim"].ingress >= 1 &&
				ackState.receiverPackAccepted == 1
		}, "exact genuine ACK refusal after one actual application")
		f.requireLive()
		f.state.stateLock.Lock()
		item, initialAckState := *f.state.items["victim"], ackState
		f.state.stateLock.Unlock()
		messagesAfter, bytesAfter := p2OriginDrops()
		if messagesAfter-messagesBefore != 1 || bytesAfter-bytesBefore != float64(initialAckState.refusedBytes) ||
			initialAckState.refusedBytes <= 0 || initialAckState.callbacks != 1 || initialAckState.admissions != 0 ||
			initialAckState.senderAckBegins != 0 || initialAckState.senderAckAccepted != 0 ||
			item.applications != 1 || item.drops != 0 || item.acks != 1 || item.selectiveAcks != 0 ||
			item.terminalCount != 0 || item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] != 0 ||
			!item.initialOwnerSampled || !item.initialOwnerSnapshot.Complete ||
			item.initialOwnerSnapshot.SendAck.Owners != 1 {
			f.t.Fatalf("R4 first ACK loss/custody did not have the forced disposition: item=%+v ack=%+v", item, initialAckState)
		}

		// Same ACK-origin Resident, original data source, and distinct source all
		// deliver real applications and reverse ACKs before the cold owner moves.
		for _, flow := range []struct {
			source *p2OriginEndpoint
			label  string
		}{
			{source: f.victim, label: "r4-healthy-ack-origin"},
			{source: f.source, label: "r4-healthy-original-source"},
			{source: f.second, label: "r4-healthy-second-source"},
		} {
			f.send(flow.source, f.healthy, flow.label, false)
			f.waitDelivered(flow.label, true)
			f.expected[flow.label] = 1
		}
		f.requireLive()
		f.wait(func(s *p2OriginState) bool {
			return s.items["victim"].retryWrites >= 1 && s.retryHeld
		}, "actual original sender retry retained before real receiver")
		f.state.stateLock.Lock()
		item, heldAckState := *f.state.items["victim"], ackState
		f.state.stateLock.Unlock()
		if item.terminalCount != 0 || item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] != 0 ||
			item.applications != 1 || heldAckState.senderAckBegins != 0 ||
			heldAckState.senderAckAccepted != 0 || heldAckState.receiverPackAccepted != 1 {
			f.t.Fatal("R4 original custody ended, or duplicate bypassed the physical retry gate")
		}

		f.releaseColdOnce.Do(func() { close(f.coldRelease) })
		for label := range f.expected {
			if label != "victim" {
				f.waitDelivered(label, label == "r4-healthy-ack-origin" ||
					label == "r4-healthy-original-source" || label == "r4-healthy-second-source")
			}
		}
		f.releaseRetryOnce.Do(func() { close(f.retryRelease) })
		f.wait(func(s *p2OriginState) bool {
			item := s.items["victim"]
			return item.applications == 1 && item.acks >= 2 && item.ingress >= 2 &&
				item.terminalCount == 1 && item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] == 1 &&
				ackState.receiverPackBegins >= 2 && ackState.receiverPackAccepted >= 2 &&
				ackState.admissions >= 1 && ackState.senderAckBegins >= 1 && ackState.senderAckAccepted >= 1
		}, "genuine duplicate, regenerated ACK, sender acceptance and original terminal")
		f.requireLive()
		f.state.stateLock.Lock()
		defer f.state.stateLock.Unlock()
		item = *f.state.items["victim"]
		if ackState.refusals != 1 || ackState.callbacks != ackState.refusals+ackState.admissions ||
			item.retryWrites < 1 || item.wireWrites < 2 || item.drops != 0 ||
			item.applications != 1 || item.selectiveAcks != 0 || item.terminalErr != nil {
			f.t.Fatalf("R4 genuine ACK recovery disposition: item=%+v ack=%+v", item, ackState)
		}
		for _, phase := range []clientconnect.SendPackLifecyclePhase{
			clientconnect.SendPackLifecyclePhaseStarted,
			clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
			clientconnect.SendPackLifecyclePhaseTerminal,
		} {
			if item.lifecycle[phase] != 1 || item.lifecycleErr[phase] != nil {
				f.t.Fatalf("R4 actual original lifecycle phase %d: %+v", phase, item)
			}
		}
		// Joined fixture cleanup also checks the exact one application effect,
		// all enabled payload ledgers, bounded witnesses and pool violations.
	})
}

// A decoded real original waits before admission; explicit owner close fences it.
func TestP2RealOriginCloseBeforeForwardAdmissionReturnsBorrowedOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	runP2R4Origin(t, func(f *p2OriginFixture) {
		f.beforeForward = func(endpoint *p2OriginEndpoint, wire p2OriginWire) {
			if endpoint == f.source && len(wire.labels) == 1 && wire.labels[0] == "r4-close-before-admission" {
				close(entered)
				select {
				case <-release:
				case <-f.ctx.Done():
				}
			}
		}
	}, func(f *p2OriginFixture) {
		defer releaseOnce.Do(func() { close(release) })
		label := "r4-close-before-admission"
		before := p2OriginIngressGroup(f.ledger, f.source.id[15])
		messagesBefore, bytesBefore := p2OriginDrops()
		f.send(f.source, f.cold, label, true)
		f.expected[label] = 0
		p2R4WaitEvent(f, entered, "actual decoder callback before admission")
		closed, result := p2R4StartClose(f, f.source, label)
		p2R4WaitEvent(f, closed, "explicit Close before callback admission")
		releaseOnce.Do(func() { close(release) })
		f.waitIngress(label, 1)
		p2R4WaitClose(f, result)
		after := p2OriginIngressGroup(f.ledger, f.source.id[15])
		queue := f.source.resident.forwardIngress[int(f.cold.id[15])%len(f.source.resident.forwardIngress)].queue
		messagesAfter, bytesAfter := p2OriginDrops()
		if queue != nil || before != after || messagesAfter != messagesBefore || bytesAfter != bytesBefore {
			f.t.Fatalf("closed admission retained/queued/counted an unowned share: %+v -> %+v", before, after)
		}
		p2R4RequireNoAckCompletion(f, label)
		p2R4RequireOtherOwnersLive(f, f.source)
		f.send(f.second, f.healthy, "r4-before-close-healthy", false)
		f.waitDelivered("r4-before-close-healthy", true)
		f.expected["r4-before-close-healthy"] = 1
	})
}

// The real callback owns admission before lazy worker registration and owner close.
func TestP2RealOriginAdmittedLazyWorkerJoinsAcrossClose(t *testing.T) {
	runP2R4Origin(t, nil, func(f *p2OriginFixture) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		f.source.resident.beforeForwardIngressWorkerStartForTest = func() {
			close(entered)
			select {
			case <-release:
			case <-f.ctx.Done():
			}
		}
		label := "r4-admitted-lazy-close"
		before := p2OriginIngressGroup(f.ledger, f.source.id[15])
		messagesBefore, bytesBefore := p2OriginDrops()
		f.send(f.source, f.cold, label, true)
		f.expected[label] = 0
		p2R4WaitEvent(f, entered, "actual admitted callback before lazy worker Add")
		shardIndex := int(f.cold.id[15]) % len(f.source.resident.forwardIngress)
		if f.source.resident.forwardIngress[shardIndex].queue != nil {
			f.t.Fatal("lazy worker already published before the actual registration barrier")
		}
		closed, result := p2R4StartClose(f, f.source, label)
		p2R4WaitEvent(f, closed, "explicit Close while admitted callback owns the registration fence")
		releaseOnce.Do(func() { close(release) })
		f.waitIngress(label, 1)
		p2R4WaitClose(f, result)
		queue := f.source.resident.forwardIngress[shardIndex].queue
		after := p2OriginIngressGroup(f.ledger, f.source.id[15])
		messagesAfter, bytesAfter := p2OriginDrops()
		// ctx.Done and the now-ready queue can both win. These assertions cover
		// ownership after the joined final drain; they do not claim both branches.
		if queue == nil || cap(queue) != 256 || len(queue) != 0 ||
			after.Messages != before.Messages || after.LogicalBytes != before.LogicalBytes ||
			after.BackingCharge != before.BackingCharge ||
			after.Admitted-before.Admitted != 1 || after.Released-before.Released != 1 ||
			messagesAfter != messagesBefore || bytesAfter != bytesBefore {
			f.t.Fatalf("admitted lazy-close owner did not reconcile after join: %+v -> %+v", before, after)
		}
		p2R4RequireNoAckCompletion(f, label)
		p2R4RequireOtherOwnersLive(f, f.source)
		f.send(f.second, f.healthy, "r4-lazy-close-healthy", false)
		f.waitDelivered("r4-lazy-close-healthy", true)
		f.expected["r4-lazy-close-healthy"] = 1
	})
}

// Accepted genuine backlog is canceled by explicit lifecycle shutdown, not pressure.
func TestP2RealOriginAcceptedForwardBacklogDrainsOnExplicitClose(t *testing.T) {
	runP2R4Origin(t, nil, func(f *p2OriginFixture) {
		messagesBefore, bytesBefore := p2OriginDrops()
		before := p2OriginIngressGroup(f.ledger, f.source.id[15])
		labels := []string{"r4-close-cold-first"}
		f.send(f.source, f.cold, labels[0], true)
		f.waitIngress(labels[0], 1)
		f.expected[labels[0]] = 0
		p2R4WaitEvent(f, f.coldEntered, "actual accepted first owner at cold reader")
		shardIndex := int(f.cold.id[15]) % len(f.source.resident.forwardIngress)
		queue := f.source.resident.forwardIngress[shardIndex].queue
		if cap(queue) != 256 {
			f.t.Fatal("backlog control changed the actual default queue capacity")
		}
		const backlog = 4
		for index := range backlog {
			label := fmt.Sprintf("r4-close-backlog-%d", index)
			destination := f.cold
			if index == backlog-1 {
				destination = f.collision
			}
			f.send(f.source, destination, label, true)
			f.waitIngress(label, 1)
			f.expected[label] = 0
			labels = append(labels, label)
		}
		held := p2OriginIngressGroup(f.ledger, f.source.id[15])
		if len(queue) != backlog || held.Messages-before.Messages != backlog+1 ||
			held.Admitted-before.Admitted != backlog+1 || held.Released != before.Released ||
			!validResidentPayloadGroup(held) {
			f.t.Fatalf("accepted real backlog did not have the held ownership census: %+v -> %+v", before, held)
		}
		closed, result := p2R4StartClose(f, f.source, "")
		p2R4WaitEvent(f, closed, "explicit owner close cancels real held reader")
		// The existing cold seam selects its manager context cancellation, then
		// delegates to the real Redis reader. performActiveContractRead rejects
		// any result after cancellation. No false positive allowance is installed.
		p2R4WaitClose(f, result)
		after := p2OriginIngressGroup(f.ledger, f.source.id[15])
		messagesAfter, bytesAfter := p2OriginDrops()
		if len(queue) != 0 || after.Messages != before.Messages ||
			after.LogicalBytes != before.LogicalBytes || after.BackingCharge != before.BackingCharge ||
			after.Admitted-before.Admitted != backlog+1 || after.Released-before.Released != backlog+1 ||
			messagesAfter != messagesBefore || bytesAfter != bytesBefore {
			f.t.Fatalf("explicit close did not drain accepted owners without pressure drops: %+v -> %+v", before, after)
		}
		for _, label := range labels {
			p2R4RequireNoAckCompletion(f, label)
		}
		p2R4RequireOtherOwnersLive(f, f.source)
		f.send(f.second, f.healthy, "r4-backlog-close-healthy", false)
		f.waitDelivered("r4-backlog-close-healthy", true)
		f.expected["r4-backlog-close-healthy"] = 1
	})
}

// R1–R3 retain their original initializer and every original predicate unchanged.
func runP2R4Origin(t *testing.T, configure, run func(*p2OriginFixture)) {
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
		// Called only after actual owner joins, with the fixture metadata lock.
		f.afterJoined = func(s *p2OriginState) {
			for label, item := range s.items {
				if !item.noAck {
					continue
				}
				if item.wireWrites != 1 || item.retryWrites != 0 || item.acks != 0 ||
					item.selectiveAcks != 0 || item.terminalCount != 1 || item.terminalErr != nil ||
					item.noAckPhases[clientconnect.NoAckSendPhaseStarted] != 1 ||
					item.noAckPhases[clientconnect.NoAckSendPhaseCompleted] != 1 || item.noAckErr != nil {
					f.t.Errorf("R4 joined NoAck physical/completion census for %q: %+v", label, item)
				}
			}
		}
		if configure != nil {
			configure(f)
		}
		f.setup()
		run(f)
	})
}

// A positive barrier event gives ordering; the context is only a failure guard.
func p2R4WaitEvent(f *p2OriginFixture, event <-chan struct{}, description string) {
	f.t.Helper()
	select {
	case <-event:
	case <-f.ctx.Done():
		f.t.Fatalf("R4 waiting for %s: %v", description, f.ctx.Err())
	}
}

// Close is owned by a fixture worker, outside the Client's callback worker tree.
// The existing close-join seam reports only after admission close and cancellation.
func p2R4StartClose(f *p2OriginFixture, endpoint *p2OriginEndpoint, requiredCallbackReturn string) (<-chan struct{}, <-chan error) {
	closed := make(chan struct{})
	result := make(chan error, 1)
	var closedOnce sync.Once
	endpoint.resident.beforeClientCloseJoinForTest = func() {
		closedOnce.Do(func() { close(closed) })
	}
	f.workers.Add(1)
	go func() {
		defer f.workers.Done()
		err := endpoint.resident.CloseAndWait(f.ctx)
		if err == nil && requiredCallbackReturn != "" {
			// Record the actual return against the same metadata lock as the
			// callback-return observation, not a later test-goroutine snapshot.
			f.mutate(func(s *p2OriginState) {
				item := s.items[requiredCallbackReturn]
				if item == nil || item.ingress != 1 {
					err = fmt.Errorf("R4 CloseAndWait returned before actual callback %q", requiredCallbackReturn)
					if s.err == nil {
						s.err = err
					}
				}
			})
		}
		result <- err
	}()
	return closed, result
}

// Completion is an actual owner join, not an elapsed negative observation.
func p2R4WaitClose(f *p2OriginFixture, result <-chan error) {
	f.t.Helper()
	select {
	case err := <-result:
		if err != nil {
			f.t.Fatalf("R4 explicit resident join: %v", err)
		}
	case <-f.ctx.Done():
		f.t.Fatalf("R4 explicit resident join did not complete: %v", f.ctx.Err())
	}
}

// First-route success for actual NoAck is deliberately not application custody.
func p2R4RequireNoAckCompletion(f *p2OriginFixture, label string) {
	f.wait(func(s *p2OriginState) bool {
		item := s.items[label]
		return item.terminalCount == 1 && item.noAckPhases[clientconnect.NoAckSendPhaseCompleted] == 1 &&
			item.lifecycle[clientconnect.SendPackLifecyclePhaseTerminal] == 1
	}, label+" actual NoAck route completion")
	f.state.stateLock.Lock()
	item := *f.state.items[label]
	f.state.stateLock.Unlock()
	if !item.noAck || item.wireWrites != 1 || item.retryWrites != 0 || item.ingress != 1 ||
		item.applications != 0 || item.acks != 0 || item.selectiveAcks != 0 || item.drops != 0 ||
		item.terminalErr != nil || item.noAckErr != nil ||
		item.noAckPhases[clientconnect.NoAckSendPhaseStarted] != 1 {
		f.t.Fatalf("R4 explicit-close NoAck custody for %q: %+v", label, item)
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted,
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
		clientconnect.SendPackLifecyclePhaseTerminal,
	} {
		if item.lifecycle[phase] != 1 || item.lifecycleErr[phase] != nil {
			f.t.Fatalf("R4 close NoAck lifecycle phase %d for %q: %+v", phase, label, item)
		}
	}
}

// Explicit Close legitimately retires that Resident and its forwards. It must not
// retire the Exchange, other Residents, or independently owned original Clients.
func p2R4RequireOtherOwnersLive(f *p2OriginFixture, closed *p2OriginEndpoint) {
	if !closed.resident.IsDone() || f.exchange.ctx.Err() != nil {
		f.t.Fatal("R4 explicit owner-close scope was not exactly the intended lifecycle")
	}
	for _, endpoint := range f.endpoints {
		if endpoint.client.IsDone() {
			f.t.Fatal("R4 explicit Resident close retired an independently owned original Client")
		}
		if endpoint != closed && (endpoint.resident.IsDone() || endpoint.resident.TransportCount() != 1) {
			f.t.Fatal("R4 explicit Resident close retired an unrelated Resident/transport")
		}
	}
}
