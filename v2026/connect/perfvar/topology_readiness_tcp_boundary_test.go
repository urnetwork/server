//go:build !js && go1.25

package perfvar

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// These are controlled inner-TCP experiments, not a reconstruction of the
// archived H1 loss process. The production TUN and readiness server run
// unchanged. All endpoints and packets stay in memory.
type readinessTCPPacket struct {
	sequence uint32
	ack      uint32
	flags    byte
	payload  []byte
}

func parseReadinessTCPPacket(packet []byte) (readinessTCPPacket, bool) {
	if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 {
		return readinessTCPPacket{}, false
	}
	ipHeader := int(packet[0]&15) * 4
	packetSize := int(binary.BigEndian.Uint16(packet[2:4]))
	if ipHeader < 20 || packetSize > len(packet) || packetSize < ipHeader+20 {
		return readinessTCPPacket{}, false
	}
	tcp := packet[ipHeader:packetSize]
	tcpHeader := int(tcp[12]>>4) * 4
	if tcpHeader < 20 || len(tcp) < tcpHeader {
		return readinessTCPPacket{}, false
	}
	return readinessTCPPacket{
		sequence: binary.BigEndian.Uint32(tcp[4:8]),
		ack:      binary.BigEndian.Uint32(tcp[8:12]), flags: tcp[13],
		payload: tcp[tcpHeader:],
	}, true
}

type readinessTCPReadConn struct {
	net.Conn
	read   *atomic.Int64
	notify func()
}

func (c readinessTCPReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	c.notify()
	return n, err
}

type readinessTCPListener struct {
	net.Listener
	read   *atomic.Int64
	notify func()
}

func (l readinessTCPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &readinessTCPReadConn{Conn: conn, read: l.read, notify: l.notify}, nil
}

type readinessTCPBridge struct {
	app, origin                       *clientconnect.Tun
	wg                                sync.WaitGroup
	mu                                sync.Mutex
	base                              uint32
	baseSet                           bool
	emitted, injected                 [fullTunProbePayloadByteCount]bool
	emittedBytes, injectedBytes       int
	ackEmitted, ackInjected           uint32
	dataPacketCount, firstPayloadSize int
	holdForward, holdACK              bool
	heldPacket, heldACK               []byte
	heldForwardCount, heldACKCount    int
	read                              atomic.Int64
	stage                             atomic.Int32
	errs                              chan error
	changed                           chan struct{}
	// Protected by mu; called without mu after real packet injection but
	// before the fixture publishes its injection ledger.
	beforeInjectionPublication func(readinessTCPPacket, bool)
}

func newReadinessTCPBridge(t *testing.T, ctx context.Context) *readinessTCPBridge {
	t.Helper()
	b := &readinessTCPBridge{errs: make(chan error, 2), changed: make(chan struct{}, 1)}
	t.Cleanup(b.close)
	for _, target := range []**clientconnect.Tun{&b.app, &b.origin} {
		resources := mobileTunResourceProfile()
		settings := clientconnect.DefaultTunSettingsWithBufferSize(resources.ChannelSize)
		settings.Mtu = 1100
		// This single-flow byte-range fixture excludes the already separately
		// tested dial race, whose extra four-tuple would need its own ledger.
		settings.DialRace = 1
		applyTunResourceProfile(settings, resources)
		tun, err := clientconnect.CreateTun(ctx, settings)
		if err != nil {
			t.Fatal(err)
		}
		*target = tun
	}
	b.wg.Add(2)
	go b.forward(b.app, b.origin, false)
	go b.forward(b.origin, b.app, true)
	return b
}

func (b *readinessTCPBridge) notify() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

func (b *readinessTCPBridge) forward(from, to *clientconnect.Tun, reverse bool) {
	defer b.wg.Done()
	for {
		packet, err := from.Read()
		if err != nil {
			return
		}
		metadata, ok := parseReadinessTCPPacket(packet)
		if !ok {
			clientconnect.MessagePoolReturn(packet)
			b.errs <- fmt.Errorf("non-TCP packet in readiness-only bridge")
			return
		}
		b.mu.Lock()
		deliver := true
		if !reverse && metadata.flags&2 != 0 {
			b.base, b.baseSet = metadata.sequence+1, true
		}
		if reverse && b.baseSet && metadata.flags&16 != 0 {
			offset := metadata.ack - b.base
			if offset <= fullTunProbePayloadByteCount {
				b.ackEmitted = max(b.ackEmitted, offset)
				if b.holdACK && 0 < offset {
					deliver = false
					b.heldACK = append(b.heldACK[:0], packet...)
					b.heldACKCount++
				}
			}
		}
		if !reverse && b.baseSet && len(metadata.payload) != 0 {
			offset := int(metadata.sequence - b.base)
			if offset < 0 || len(b.emitted) < offset+len(metadata.payload) {
				b.mu.Unlock()
				clientconnect.MessagePoolReturn(packet)
				b.errs <- fmt.Errorf("payload range outside readiness request: %d+%d", offset, len(metadata.payload))
				return
			}
			for i := offset; i < offset+len(metadata.payload); i++ {
				b.emitted[i] = true
			}
			b.emittedBytes += len(metadata.payload)
			b.dataPacketCount++
			if b.firstPayloadSize == 0 {
				b.firstPayloadSize = len(metadata.payload)
			}
			if b.holdForward && offset == 0 {
				deliver = false
				if b.heldPacket == nil {
					b.heldPacket = bytes.Clone(packet)
				}
				b.heldForwardCount++
			}
		}
		b.mu.Unlock()
		if deliver {
			err = b.inject(to, packet, reverse)
		}
		clientconnect.MessagePoolReturn(packet)
		b.notify()
		if err != nil {
			b.errs <- err
			return
		}
	}
}

func (b *readinessTCPBridge) inject(to *clientconnect.Tun, packet []byte, reverse bool) error {
	metadata, ok := parseReadinessTCPPacket(packet)
	if !ok {
		return fmt.Errorf("invalid injected TCP packet")
	}
	if _, err := to.Write(packet); err != nil {
		return err
	}
	b.mu.Lock()
	if beforePublication := b.beforeInjectionPublication; beforePublication != nil {
		b.mu.Unlock()
		beforePublication(metadata, reverse)
		b.mu.Lock()
	}
	defer b.mu.Unlock()
	if reverse && b.baseSet && metadata.flags&16 != 0 {
		offset := metadata.ack - b.base
		if offset <= fullTunProbePayloadByteCount {
			b.ackInjected = max(b.ackInjected, offset)
		}
	}
	if !reverse && b.baseSet && len(metadata.payload) != 0 {
		offset := int(metadata.sequence - b.base)
		for i := offset; i < offset+len(metadata.payload); i++ {
			b.injected[i] = true
		}
		b.injectedBytes += len(metadata.payload)
	}
	return nil
}

func (b *readinessTCPBridge) release() error {
	b.mu.Lock()
	b.holdForward, b.holdACK = false, false
	packet, ack := b.heldPacket, b.heldACK
	b.heldPacket, b.heldACK = nil, nil
	b.mu.Unlock()
	if packet != nil {
		if err := b.inject(b.origin, packet, false); err != nil {
			return err
		}
	}
	if ack != nil {
		if err := b.inject(b.app, ack, true); err != nil {
			return err
		}
	}
	b.notify()
	return nil
}

type readinessTCPSnapshot struct {
	EmittedUnique, InjectedUnique, InjectedPrefix int
	EmittedBytes, InjectedBytes                   int
	ACKEmitted, ACKInjected                       uint32
	DataPackets, FirstPayloadSize                 int
	HeldForward, HeldACK                          int
	Read                                          int64
	Stage                                         int32
}

func (s readinessTCPSnapshot) heldBoundaryReady(initialCwnd int, holdForward bool) bool {
	// TCP establishment/packet progress need not wait for application Accept.
	if s.Stage == 0 || s.DataPackets < initialCwnd || s.FirstPayloadSize == 0 {
		return false
	}
	if holdForward {
		return 0 < s.HeldForward && 0 < s.InjectedUnique
	}
	// Tun.Write exposes the packet to the peer before inject publishes its
	// ledger. Peer read/ACK progress alone can therefore precede this record.
	return 0 < s.HeldACK && s.ACKEmitted == uint32(s.EmittedUnique) && s.Read == int64(s.EmittedUnique) &&
		s.InjectedUnique == s.EmittedUnique && s.InjectedPrefix == s.InjectedUnique
}

func (b *readinessTCPBridge) snapshot() readinessTCPSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := readinessTCPSnapshot{
		EmittedBytes: b.emittedBytes, InjectedBytes: b.injectedBytes,
		ACKEmitted: b.ackEmitted, ACKInjected: b.ackInjected,
		DataPackets: b.dataPacketCount, FirstPayloadSize: b.firstPayloadSize,
		HeldForward: b.heldForwardCount, HeldACK: b.heldACKCount,
		Read: b.read.Load(), Stage: b.stage.Load(),
	}
	for i := range b.emitted {
		if b.emitted[i] {
			s.EmittedUnique++
		}
		if b.injected[i] {
			s.InjectedUnique++
		}
		if s.InjectedPrefix == i && b.injected[i] {
			s.InjectedPrefix++
		}
	}
	return s
}

func (b *readinessTCPBridge) waitFor(t *testing.T, predicate func(readinessTCPSnapshot) bool) readinessTCPSnapshot {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		s := b.snapshot()
		if predicate(s) {
			return s
		}
		select {
		case <-b.changed:
		case err := <-b.errs:
			t.Fatal(err)
		case <-timer.C:
			t.Fatalf("TCP boundary not reached: %+v", s)
		}
	}
}

func (b *readinessTCPBridge) close() {
	if b.app != nil {
		_ = b.app.Close()
	}
	if b.origin != nil {
		_ = b.origin.Close()
	}
	b.wg.Wait()
	b.mu.Lock()
	b.heldPacket, b.heldACK = nil, nil
	b.mu.Unlock()
}

// The gate is released by observed packet/read boundaries. No sleep or random
// impairment chooses the held packet, and no production timeout changes.
// gVisor's runtime sleeper is not synctest-durable, so its real stack runs on
// the normal clock; bounded wall-clock waits only fail a stuck fixture.
func TestReadinessTCPBoundary(t *testing.T) {
	for _, mode := range []string{"clean", "missing-first-range", "withheld-inner-ack", "withheld-inner-ack-unpublished-injection", "missing-first-range-unpublished-accept"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := newReadinessTCPBridge(t, ctx)
			listener, err := b.origin.ListenTCP(&net.TCPAddr{IP: b.origin.LocalAddresses()[0].AsSlice()})
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Clone(deterministicPayload()[:fullTunProbePayloadByteCount])
			var acceptReached chan struct{}
			var releaseAccept func()
			var beforeAcceptPublication func()
			if mode == "missing-first-range-unpublished-accept" {
				acceptReached = make(chan struct{})
				acceptRelease := make(chan struct{})
				releaseAccept = sync.OnceFunc(func() { close(acceptRelease) })
				beforeAcceptPublication = sync.OnceFunc(func() {
					close(acceptReached)
					b.notify()
					select {
					case <-acceptRelease:
					case <-ctx.Done():
					}
				})
			}
			server := newReadinessEchoServer(readinessTCPListener{listener, &b.read, b.notify}, payload, &readinessEchoServerSettings{
				afterAcceptForTest: func(net.Conn) {
					if beforeAcceptPublication != nil {
						beforeAcceptPublication()
					}
					b.stage.CompareAndSwap(0, 1)
					b.notify()
				},
				afterCompleteRequest:  func() { b.stage.Store(2) },
				afterCompleteResponse: func() { b.stage.Store(3) },
			})
			defer server.CloseAndWait()
			if releaseAccept != nil {
				defer releaseAccept()
			}
			conn, err := b.app.DialContext(ctx, "tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			info, err := conn.(*clientconnect.TunTcpConn).TcpInfo()
			if err != nil {
				t.Fatal(err)
			}
			holdForward := mode == "missing-first-range" || mode == "missing-first-range-unpublished-accept"
			holdACK := mode == "withheld-inner-ack" || mode == "withheld-inner-ack-unpublished-injection"
			var publicationReached chan struct{}
			var releasePublication func()
			var beforePublication func(readinessTCPPacket, bool)
			if mode == "withheld-inner-ack-unpublished-injection" {
				publicationReached = make(chan struct{})
				publicationRelease := make(chan struct{})
				releasePublication = sync.OnceFunc(func() { close(publicationRelease) })
				defer releasePublication()
				pausePublication := sync.OnceFunc(func() {
					close(publicationReached)
					b.notify()
					select {
					case <-publicationRelease:
					case <-ctx.Done():
					}
				})
				beforePublication = func(packet readinessTCPPacket, reverse bool) {
					if !reverse && len(packet.payload) != 0 && b.snapshot().DataPackets == int(info.SndCwnd) {
						pausePublication()
					}
				}
			}
			b.mu.Lock()
			b.holdForward, b.holdACK = holdForward, holdACK
			b.beforeInjectionPublication = beforePublication
			b.mu.Unlock()
			if err := conn.SetDeadline(time.Now().Add(170880 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if err := writeFullTunAll(conn, payload); err != nil {
				t.Fatal(err)
			}
			if acceptReached != nil {
				// Dial and the initial TCP flight can complete before the
				// application accept callback publishes the server stage.
				unpublished := b.waitFor(t, func(s readinessTCPSnapshot) bool {
					select {
					case <-acceptReached:
					default:
						return false
					}
					return s.DataPackets >= int(info.SndCwnd) && s.FirstPayloadSize != 0 && 0 < s.HeldForward && 0 < s.InjectedUnique
				})
				if unpublished.Stage != 0 || unpublished.Read != 0 || unpublished.InjectedPrefix != 0 || unpublished.ACKEmitted != 0 {
					t.Fatalf("did not force the unpublished accept boundary: %+v", unpublished)
				}
				if unpublished.heldBoundaryReady(int(info.SndCwnd), holdForward) {
					t.Fatalf("unpublished acceptance accepted as a complete held boundary: %+v", unpublished)
				}
				t.Logf("TCP flight preceded server accept publication=%+v", unpublished)
				releaseAccept()
			}
			if publicationReached != nil {
				// The peer may read and ACK the last initial-flight segment
				// before inject returns to publish that segment in its ledger.
				// Force that ordering without holding a fixture lock across
				// peer progress or changing any production timeout.
				unpublished := b.waitFor(t, func(s readinessTCPSnapshot) bool {
					select {
					case <-publicationReached:
					default:
						return false
					}
					return 0 < s.HeldACK && s.ACKEmitted == uint32(s.EmittedUnique) && s.Read == int64(s.EmittedUnique)
				})
				if unpublished.InjectedUnique >= unpublished.EmittedUnique || unpublished.InjectedPrefix != unpublished.InjectedUnique || unpublished.ACKInjected != 0 || unpublished.Stage != 1 {
					t.Fatalf("did not force the unpublished injection boundary: %+v", unpublished)
				}
				if unpublished.heldBoundaryReady(int(info.SndCwnd), holdForward) {
					t.Fatalf("unpublished injection accepted as a complete held boundary: %+v", unpublished)
				}
				t.Logf("peer read/ACK preceded injection ledger publication=%+v", unpublished)
				releasePublication()
			}
			if mode != "clean" {
				partial := b.waitFor(t, func(s readinessTCPSnapshot) bool {
					return s.heldBoundaryReady(int(info.SndCwnd), holdForward)
				})
				if partial.Stage != 1 || (holdACK && partial.EmittedUnique >= len(payload)) {
					t.Fatalf("held TCP boundary did not leave an incomplete request: %+v", partial)
				}
				if holdForward {
					if partial.Read != 0 || partial.InjectedPrefix != 0 || partial.ACKEmitted != 0 || partial.InjectedUnique == partial.EmittedUnique {
						t.Fatalf("request gap not localized: %+v", partial)
					}
				} else if partial.EmittedUnique != partial.InjectedUnique || partial.InjectedPrefix != partial.InjectedUnique || partial.ACKInjected != 0 {
					t.Fatalf("feedback gap not localized: %+v", partial)
				}
				t.Logf("local Write accepted=%d initial cwnd=%d held boundary=%+v", len(payload), info.SndCwnd, partial)
				if err := b.release(); err != nil {
					t.Fatal(err)
				}
			}
			response := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, response); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(response, payload) {
				t.Fatal("echo mismatch")
			}
			if err := <-server.result; err != nil {
				t.Fatal(err)
			}
			complete := b.waitFor(t, func(s readinessTCPSnapshot) bool {
				return s.InjectedUnique == len(payload) && s.Read == int64(len(payload)) && s.ACKInjected == uint32(len(payload))
			})
			if complete.Stage != 3 || complete.InjectedPrefix != len(payload) {
				t.Fatalf("incomplete echo: %+v", complete)
			}
			t.Logf("released original flow=%+v", complete)
		})
	}
}
