package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// A temporary provider ingress callback identifies the authenticated owner of
// the exact one-byte registration datagram. The newest generated Client can
// be unrelated to this socket. No return packet or aggregate counter chooses
// the expected owner; the callback is removed before measured traffic starts.
type fullTunUDPRegistrationOwner struct {
	flow     fullTunBridgeFlowKey
	observed chan struct{}
	lock     sync.Mutex
	owner    clientconnect.Id
	conflict bool
}

func newFullTunUDPRegistrationOwner(source, destination net.Addr) (*fullTunUDPRegistrationOwner, error) {
	flow, err := fullTunBridgeUdpFlowKey(source, destination)
	if err != nil {
		return nil, err
	}
	return &fullTunUDPRegistrationOwner{flow: flow, observed: make(chan struct{})}, nil
}

func (self *fullTunUDPRegistrationOwner) observe(source clientconnect.TransferPath, frames []*protocol.Frame, _ clientconnect.Peer) {
	if source.SourceId == (clientconnect.Id{}) {
		return
	}
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
			continue
		}
		message, err := clientconnect.FromFrame(frame)
		if err != nil {
			continue
		}
		packet := message.(*protocol.IpPacketToProvider).GetIpPacket().GetPacketBytes()
		ipPath, payload, err := clientconnect.ParseIpPathWithPayload(packet)
		if err != nil || len(payload) != 1 || payload[0] != 1 || fullTunBridgeFlowKeyFromIpPath(ipPath) != self.flow {
			continue
		}
		self.lock.Lock()
		if self.owner == (clientconnect.Id{}) {
			self.owner = source.SourceId
			close(self.observed)
		} else if self.owner != source.SourceId {
			self.conflict = true
		}
		self.lock.Unlock()
	}
}

// Read after the origin received its registration and the setup source
// boundary completed. Replays by the same owner agree; conflicting observed
// authenticated owners fail closed instead of selecting one.
func (self *fullTunUDPRegistrationOwner) wait(ctx context.Context) (clientconnect.Id, error) {
	select {
	case <-ctx.Done():
		return clientconnect.Id{}, ctx.Err()
	case <-self.observed:
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.conflict {
		return clientconnect.Id{}, errors.New("UDP registration has conflicting authenticated owners")
	}
	return self.owner, nil
}

func udpRegistrationTestFixture(t *testing.T) (*fullTunUDPRegistrationOwner, []byte) {
	t.Helper()
	tracker, err := newFullTunUDPRegistrationOwner(
		&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 41000},
		&net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 42000},
	)
	if err != nil {
		t.Fatal(err)
	}
	packet := bridgeAdmissionTestPacket(false, 41000)[:29]
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	binary.BigEndian.PutUint16(packet[24:26], uint16(len(packet)-20))
	packet[28] = 1
	return tracker, packet
}

func udpRegistrationTestFrame(t *testing.T, packet []byte, version int) *protocol.Frame {
	t.Helper()
	frame := clientconnect.RequireToFrame(&protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}}, version)
	// Legacy serialization owns a pooled buffer; v2 borrows the unpooled
	// packet. Capture the original slice because malformed-frame cases replace
	// MessageBytes. Returning an unpooled slice is a no-op.
	messageBytes := frame.MessageBytes
	t.Cleanup(func() { clientconnect.MessagePoolReturn(messageBytes) })
	return frame
}

func TestFullTunUDPRegistrationFrameReleasesPoolOwnership(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, replaceBytes := range []bool{false, true} {
			t.Run(fmt.Sprintf("protocol_%d/replace_%t", version, replaceBytes), func(t *testing.T) {
				before := capturePerfvarResourceLifecycle()
				var original []byte
				t.Run("borrower", func(t *testing.T) {
					_, packet := udpRegistrationTestFixture(t)
					frame := udpRegistrationTestFrame(t, packet, version)
					original = frame.MessageBytes
					if replaceBytes {
						// Malformed-frame fixtures replace the field, but the
						// original allocation remains the fixture's responsibility.
						frame.MessageBytes = []byte{0xff}
					} else {
						clear(frame.MessageBytes)
					}
				})
				after := capturePerfvarResourceLifecycle()
				if after.PoolOutstandingCount != before.PoolOutstandingCount ||
					!maps.Equal(after.PoolOutstandingSizeCounts, before.PoolOutstandingSizeCounts) {
					// Keep this regression self-contained even against the old
					// leaking helper; the measured failure remains below.
					clientconnect.MessagePoolReturn(original)
					t.Fatalf("registration fixture retained pool ownership: %d -> %d classes=%v -> %v",
						before.PoolOutstandingCount, after.PoolOutstandingCount,
						before.PoolOutstandingSizeCounts, after.PoolOutstandingSizeCounts)
				}
				if version == 1 && after.PoolTakenCount-before.PoolTakenCount != 1 {
					t.Fatal("legacy registration fixture did not exercise its pooled serialization")
				}
			})
		}
	}
}

func TestFullTunUDPRegistrationOwnerUsesAuthenticatedExactRegistration(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("protocol_%d", version), func(t *testing.T) {
			tracker, packet := udpRegistrationTestFixture(t)
			owner := clientconnect.NewId()
			frame := udpRegistrationTestFrame(t, packet, version)
			tracker.observe(clientconnect.SourceId(owner), []*protocol.Frame{frame, frame}, clientconnect.Peer{})
			// Borrowed frame and packet lifetimes end at callback return.
			clear(frame.MessageBytes)
			clear(packet)
			got, err := tracker.wait(t.Context())
			if err != nil || got != owner {
				t.Fatalf("registration identity was not retained independently: matches=%t err=%v", got == owner, err)
			}
		})
	}
}

func TestFullTunUDPRegistrationOwnerRejectsOtherTraffic(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func([]byte, *protocol.Frame, *clientconnect.TransferPath)
	}{
		{"source_ip", func(packet []byte, _ *protocol.Frame, _ *clientconnect.TransferPath) { packet[12]++ }},
		{"destination_ip", func(packet []byte, _ *protocol.Frame, _ *clientconnect.TransferPath) { packet[16]++ }},
		{"source_port", func(packet []byte, _ *protocol.Frame, _ *clientconnect.TransferPath) { packet[21]++ }},
		{"destination_port", func(packet []byte, _ *protocol.Frame, _ *clientconnect.TransferPath) { packet[23]++ }},
		{"protocol", func(packet []byte, _ *protocol.Frame, _ *clientconnect.TransferPath) { packet[9] = 6 }},
		{"payload", func(packet []byte, _ *protocol.Frame, _ *clientconnect.TransferPath) { packet[28] = 2 }},
		{"return_direction", func(_ []byte, frame *protocol.Frame, _ *clientconnect.TransferPath) {
			frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
		}},
		{"malformed", func(_ []byte, frame *protocol.Frame, _ *clientconnect.TransferPath) {
			frame.MessageBytes = []byte{0xff}
		}},
		{"unauthenticated", func(_ []byte, _ *protocol.Frame, source *clientconnect.TransferPath) {
			source.SourceId = clientconnect.Id{}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			tracker, packet := udpRegistrationTestFixture(t)
			frame, source := udpRegistrationTestFrame(t, packet, 2), clientconnect.SourceId(clientconnect.NewId())
			change.mutate(packet, frame, &source)
			tracker.observe(source, []*protocol.Frame{nil, frame}, clientconnect.Peer{})
			select {
			case <-tracker.observed:
				t.Fatal("unrelated traffic selected the expected owner")
			default:
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if owner, err := tracker.wait(ctx); owner != (clientconnect.Id{}) || !errors.Is(err, context.Canceled) {
				t.Fatalf("missing registration did not preserve cancellation: %v", err)
			}
		})
	}
}

func TestFullTunUDPRegistrationOwnerRejectsAmbiguousRegistration(t *testing.T) {
	tracker, packet := udpRegistrationTestFixture(t)
	frame := udpRegistrationTestFrame(t, packet, 2)
	tracker.observe(clientconnect.SourceId(clientconnect.NewId()), []*protocol.Frame{frame}, clientconnect.Peer{})
	tracker.observe(clientconnect.SourceId(clientconnect.NewId()), []*protocol.Frame{frame}, clientconnect.Peer{})
	if owner, err := tracker.wait(t.Context()); err == nil || owner != (clientconnect.Id{}) {
		t.Fatal("conflicting authenticated owners selected an arbitrary return flow")
	}
}

// Resolving the registration owner must not weaken the exact gate: two of
// three real returns plus one same-tuple return from another owner still fail.
func TestFullTunUDPRegistrationOwnerDoesNotHideReturnShortfall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registration, packet := udpRegistrationTestFixture(t)
		owner := clientconnect.NewId()
		registration.observe(clientconnect.SourceId(owner), []*protocol.Frame{udpRegistrationTestFrame(t, packet, 2)}, clientconnect.Peer{})
		registered, err := registration.wait(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		flow := providerReturnTrackerTestFlow(187)
		flow.DestinationId = registered
		foreign := flow
		foreign.DestinationId = clientconnect.NewId()
		returns := newProviderReturnSendTracker()
		defer returns.close()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		window, ok := returns.beginFlowWindow(ctx, flow)
		if !ok {
			t.Fatal("begin registered return window")
		}
		observeProviderReturnStarted(returns, 1, flow, 2, 2056)
		observeProviderReturnCompleted(returns, 1, flow, 2, 2056, true)
		observeProviderReturnStarted(returns, 2, foreign, 1, 1028)
		observeProviderReturnCompleted(returns, 2, foreign, 1, 1028, true)
		boundary, exact := returns.flowBoundary(ctx, window, 3, 3084)
		if exact || !boundary.snapshotPresent || boundary.packetCount != 2 || boundary.packetByteCount != 2056 ||
			len(boundary.entries) != 1 || returns.invalid.Load() || ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("registered flow's true shortfall was hidden: exact=%t boundary=%+v", exact, boundary)
		}
	})
}
