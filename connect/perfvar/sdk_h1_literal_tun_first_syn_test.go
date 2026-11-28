// Exercise the native H1+ entry and its actual raw TCP race on isolated links.
package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	clientconnect "github.com/urnetwork/connect"
)

// A healthy framed upgrade uses a single raw TCP opening.
func TestSdkH1LiteralTunHealthyDialUsesOneRawAttempt(t *testing.T) {
	sdkH1LiteralTunFirstSyn(t, false)
}

// When the first SYN is lost, the second raw socket can carry the upgrade.
func TestSdkH1LiteralTunDialUsesEarlySecondRawAttempt(t *testing.T) {
	sdkH1LiteralTunFirstSyn(t, true)
}

func sdkH1LiteralTunFirstSyn(t *testing.T, loseInitialSyn bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(4013)["clean-lan"]
	profile.InnerMtu = carrierTunMtu(profile)
	path, err := newTunPath(ctx, profile, defaultTunResourceProfile())
	if err != nil {
		t.Fatal(err)
	}
	defer path.close()
	listener, err := path.right.ListenTCP(&net.TCPAddr{IP: path.endpointAddress(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer := sdkH1LiteralTunEndpoint(listener.Addr())
	localIp := path.left.LocalAddresses()[0].Unmap()
	serverTls, roots, err := newRoutePlatformH1Tls(peer.Addr())
	if err != nil {
		t.Fatal(err)
	}
	serverTls.NextProtos = []string{"http/1.1"}

	const bearer = "Bearer synthetic-h1-literal-control"
	const expected = "owned native h1 socket"
	var stateLock sync.Mutex
	tlsPeers := map[netip.AddrPort]bool{}
	serverTls.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		stateLock.Lock()
		defer stateLock.Unlock()
		tlsPeers[sdkH1LiteralTunEndpoint(hello.Conn.RemoteAddr())] = true
		return nil, nil
	}
	var upgradeCount atomic.Int64
	var upgradePeer atomic.Pointer[netip.AddrPort]
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgradeCount.Add(1)
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || r.ProtoMajor != 1 ||
			r.URL.Path != "/" || r.Header.Get("Authorization") != bearer ||
			!clientconnect.IsFramedUpgrade(r, clientconnect.H1FramerProtocol) {
			http.Error(w, "unexpected synthetic upgrade", http.StatusBadRequest)
			return
		}
		source, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "invalid synthetic peer", http.StatusBadRequest)
			return
		}
		conn, err := clientconnect.AcceptFramedUpgrade(w, r, clientconnect.H1FramerProtocol, 5*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		upgradePeer.Store(&source)
		framed, err := clientconnect.NewFramedMessageConn(conn, clientconnect.H1FramerProtocol, 4096, nil)
		if err != nil {
			t.Error(err)
			return
		}
		framed.SetReadDeadline(time.Now().Add(5 * time.Second))
		framed.SetWriteDeadline(time.Now().Add(5 * time.Second))
		kind, message, err := framed.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		defer clientconnect.MessagePoolReturn(message)
		if kind != websocket.BinaryMessage || string(message) != expected {
			t.Errorf("unexpected synthetic frame kind=%d bytes=%d", kind, len(message))
			return
		}
		if err := framed.WriteMessage(kind, message); err != nil {
			t.Error(err)
		}
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(tls.NewListener(listener, serverTls)) }()
	defer func() {
		server.Close()
		if err := <-serveDone; err != http.ErrServerClosed {
			t.Errorf("isolated synthetic server stopped: %v", err)
		}
	}()

	tap := time.Now()
	trace := &sdkH1LiteralTunOpeningTrace{device: localIp, peer: peer, tap: tap}
	var removers [2]func()
	for direction, link := range [2]*directionalLink{path.forwardLink, path.reverseLink} {
		remove, ok := link.installPacketTraceForTest(func(stage string, observation linkScheduleObservation, packet []byte) {
			trace.observe(direction, stage, observation, packet)
		})
		if !ok {
			t.Fatal("synthetic link hook is already owned")
		}
		defer remove()
		removers[direction] = remove
	}
	var dropped atomic.Pointer[nativeDuplicateAckTcpHeader]
	if loseInitialSyn {
		path.forwardLink.forcedLossForTest.Store(&linkLossTestHook{drop: func(packet []byte) bool {
			header, ok := nativeDuplicateAckHeader(packet)
			return ok && header.flags&0x17 == 0x02 && header.payloadByteCount == 0 &&
				netip.AddrFrom4(header.sourceIp) == localIp &&
				netip.AddrPortFrom(netip.AddrFrom4(header.destinationIp), header.destinationPort) == peer &&
				dropped.CompareAndSwap(nil, &header)
		}})
		defer path.forwardLink.forcedLossForTest.Store(nil)
	}

	settings := routeClientStrategySettings()
	settings.EnableNormal, settings.EnableResilient = true, false
	settings.MinNextConnectDelay, settings.MaxNextConnectDelay = 0, 0
	settings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	settings.ConnectSettings.DialContextSettings = &clientconnect.DialContextSettings{DialContext: path.left.DialContext}
	strategy := clientconnect.NewClientStrategy(ctx, settings)
	defer strategy.Close()
	headers := http.Header{"Authorization": {bearer}}
	connectStart := time.Since(tap).Nanoseconds()
	conn, dialer, err := strategy.H1DialContextWithDialer(ctx, "wss://"+peer.String()+"/", headers, 4096, true, nil)
	if err != nil {
		t.Fatalf("actual H1+ upgrade failed: %v", err)
	}
	defer conn.Close()
	connectEnd := time.Since(tap).Nanoseconds()
	if dialer == nil || dialer.Description != "normal" {
		t.Fatalf("unexpected selected H1 dialer: %+v", dialer)
	}
	winnerConn := conn.UnderlyingConn()
	winner := sdkH1LiteralTunEndpoint(winnerConn.LocalAddr())
	if sdkH1LiteralTunEndpoint(winnerConn.RemoteAddr()) != peer {
		t.Fatal("actual H1 connection missed the synthetic numeric authority")
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(expected)); err != nil {
		t.Fatal(err)
	}
	kind, reply, err := conn.ReadMessage()
	if err == nil {
		defer clientconnect.MessagePoolReturn(reply)
	}
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(reply, []byte(expected)) {
		t.Fatalf("native H1 framed echo failed: kind=%d bytes=%d err=%v", kind, len(reply), err)
	}
	if !path.waitForTerminalIdle(ctx) {
		t.Fatal("synthetic link packets did not finish")
	}
	for _, remove := range removers {
		remove()
	}

	stateLock.Lock()
	oneTlsPeer := len(tlsPeers) == 1 && tlsPeers[winner]
	stateLock.Unlock()
	authenticated := upgradePeer.Load()
	if trace.incomplete || !oneTlsPeer || upgradeCount.Load() != 1 || authenticated == nil || *authenticated != winner {
		t.Fatalf("one verified TLS and authenticated upgrade must belong to %s", winner)
	}
	var forward []sdkH1LiteralTunOpening
	for _, row := range trace.rows[:trace.count] {
		if !row.Reverse && connectStart <= row.StartNs && row.StartNs <= connectEnd {
			forward = append(forward, row)
		}
	}
	sort.Slice(forward, func(i, j int) bool { return forward[i].LinkSequence < forward[j].LinkSequence })
	if !loseInitialSyn {
		if len(forward) != 1 || forward[0].Terminal != "delivered" || forward[0].Local != winner {
			t.Fatalf("healthy H1+ must use one native opening: %+v", forward)
		}
		return
	}
	firstDrop := dropped.Load()
	if len(forward) < 2 || firstDrop == nil || forward[0].LinkSequence == 0 ||
		forward[0].Terminal != "loss-drop" || forward[0].Local.Port() != firstDrop.sourcePort ||
		forward[0].Sequence != firstDrop.sequence {
		t.Fatalf("required exact first-SYN loss was not observed: %+v", forward)
	}
	first, next := forward[0], forward[1]
	if next.LinkSequence <= first.LinkSequence || next.Terminal != "delivered" || next.Local != winner ||
		!sdkH1LiteralTunPeerAck(trace.rows[:trace.count], next) {
		t.Fatalf("actual next opening must own the SYN-ACK and H1 upgrade: %+v", forward)
	}
	if next.Local == first.Local && next.Sequence == first.Sequence {
		t.Fatalf("H1_LITERAL_TUN_FIRST_SYN_RETRY_PRECEDED_HEDGE: first=%s next=%s same_isn=%d tls_upgrades=1 native_frame_echo=true",
			first.Local, next.Local, next.Sequence)
	}
	if next.Local == first.Local {
		t.Fatalf("same local tuple acquired a conflicting opening generation: %+v", forward)
	}
	t.Logf("H1_LITERAL_TUN_SECOND_RAW_SOCKET_WON: first=%s next=%s tls_upgrades=1 native_frame_echo=true",
		first.Local, next.Local)
}

func sdkH1LiteralTunEndpoint(address net.Addr) netip.AddrPort {
	tcp := address.(*net.TCPAddr).AddrPort()
	return netip.AddrPortFrom(tcp.Addr().Unmap(), tcp.Port())
}

func sdkH1LiteralTunPeerAck(rows []sdkH1LiteralTunOpening, opening sdkH1LiteralTunOpening) bool {
	for _, candidate := range rows {
		if candidate.Reverse && candidate.Local == opening.Local && candidate.Peer == opening.Peer &&
			candidate.Ack == opening.Sequence+1 && candidate.Terminal == "delivered" && candidate.StartNs >= opening.StartNs {
			return true
		}
	}
	return false
}

// Only the isolated listener's scalar opening fields survive the borrowed hook.
// The fixed array and joined removers make an incomplete chronology a test failure.
type sdkH1LiteralTunOpeningTrace struct {
	device     netip.Addr
	peer       netip.AddrPort
	tap        time.Time
	stateLock  sync.Mutex
	rows       [8]sdkH1LiteralTunOpening
	count      int
	incomplete bool
	offered    [2]uint64
}

type sdkH1LiteralTunOpening struct {
	Local, Peer   netip.AddrPort
	Reverse       bool
	LinkSequence  uint64
	Sequence, Ack uint32
	StartNs       int64
	Terminal      string
}

func (self *sdkH1LiteralTunOpeningTrace) observe(direction int, stage string, observation linkScheduleObservation, packet []byte) {
	if packet == nil {
		if stage != "delivered" && stage != "receiver-drop" {
			return
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if observation.sequence != 0 && self.offered[direction] == observation.sequence {
			self.offered[direction] = 0
			self.finishWithLock(direction, observation.sequence, stage)
		}
		return
	}
	header, valid := nativeDuplicateAckHeader(packet)
	if !valid || header.payloadByteCount != 0 {
		return
	}
	local := netip.AddrPortFrom(netip.AddrFrom4(header.sourceIp), header.sourcePort)
	peer := netip.AddrPortFrom(netip.AddrFrom4(header.destinationIp), header.destinationPort)
	flags := byte(0x02)
	if direction == 1 {
		local, peer = peer, local
		flags = 0x12
	}
	if local.Addr() != self.device || peer != self.peer || header.flags&0x17 != flags {
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch stage {
	case "scheduled":
		if self.count == len(self.rows) {
			self.incomplete = true
			return
		}
		self.rows[self.count] = sdkH1LiteralTunOpening{
			Local: local, Peer: peer, Reverse: direction == 1,
			LinkSequence: observation.sequence, Sequence: header.sequence, Ack: header.ack,
			StartNs: observation.scheduleTime.Sub(self.tap).Nanoseconds(),
		}
		self.count++
	case "delivery-offer":
		self.offered[direction] = observation.sequence
	default:
		self.finishWithLock(direction, observation.sequence, stage)
	}
}

func (self *sdkH1LiteralTunOpeningTrace) finishWithLock(direction int, sequence uint64, stage string) {
	if sequence != 0 {
		for i := self.count - 1; i >= 0; i-- {
			row := &self.rows[i]
			if row.Reverse == (direction == 1) && row.LinkSequence == sequence {
				if row.Terminal != "" {
					self.incomplete = true
					return
				}
				row.Terminal = stage
				return
			}
		}
	}
	self.incomplete = true
}
