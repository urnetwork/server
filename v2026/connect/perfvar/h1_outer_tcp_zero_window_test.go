package perfvar

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// A PTO scheduling fix must not turn duplicate reverse ACKs into permission
// to send through a closed receive window. Only the receiver's buffer is
// constrained; both TCP timer configurations remain the shipped defaults.
func TestH1OuterTCPReverseTrafficRespectsZeroWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	profile.Forward.BaseDelay, profile.Reverse.BaseDelay = 20*time.Millisecond, 20*time.Millisecond
	profile.Forward.Jitter, profile.Reverse.Jitter = 0, 0
	path, err := newTunPathWithSettings(ctx, profile, mobileTunResourceProfile(), func(left bool, settings *clientconnect.TunSettings) {
		if !left {
			settings.TcpReceiveBuffer = clientconnect.TcpBufferRange{Min: 4096, Default: 4096, Max: 4096}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer path.close()
	listener, err := path.right.ListenTCP(&net.TCPAddr{IP: path.endpointAddress(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); accepted <- conn }()
	client, err := path.left.DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer := <-accepted
	if peer == nil {
		t.Fatal("accept failed")
	}
	defer peer.Close()
	deadline, _ := ctx.Deadline()
	client.SetDeadline(deadline)
	peer.SetDeadline(deadline)
	var consumed, zeroAdvertisements, payloadWhileClosed, probes atomic.Uint64
	var zeroEnd atomic.Uint32
	var haveZero, closedSeen, reopen atomic.Bool
	zeroConsumed := make(chan struct{})
	path.network.setPacketObserver(func(source string, packet []byte) {
		if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 {
			return
		}
		ipLen := int(packet[0]&15) * 4
		if len(packet) < ipLen+20 {
			return
		}
		payload := int(binary.BigEndian.Uint16(packet[2:4])) - ipLen - int(packet[ipLen+12]>>4)*4
		if source == "right" {
			if binary.BigEndian.Uint16(packet[ipLen+14:]) == 0 && payload > 0 {
				zeroAdvertisements.Add(1)
				if !haveZero.Load() {
					zeroEnd.Store(binary.BigEndian.Uint32(packet[ipLen+4:]) + uint32(payload))
					haveZero.Store(true)
				}
			}
			return
		}
		if source != "left" || !haveZero.Load() || reopen.Load() {
			return
		}
		// ACKing the payload carrying the zero window proves the local TCP
		// endpoint processed that reverse segment, beyond link admission.
		if int32(binary.BigEndian.Uint32(packet[ipLen+8:])-zeroEnd.Load()) >= 0 && closedSeen.CompareAndSwap(false, true) {
			close(zeroConsumed)
		}
		if closedSeen.Load() && payload > 0 {
			if payload == 1 {
				probes.Add(1)
			} else {
				payloadWhileClosed.Add(uint64(payload))
			}
		}
	})
	defer path.network.setPacketObserver(nil)
	var workers sync.WaitGroup
	stop := make(chan struct{})
	defer func() { close(stop); client.Close(); peer.Close(); workers.Wait() }()
	workers.Add(2)
	go func() {
		defer workers.Done()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		payload := bytes.Repeat([]byte{0x63}, 128)
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := peer.Write(payload); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer workers.Done()
		payload := make([]byte, 4096)
		for {
			n, err := client.Read(payload)
			consumed.Add(uint64(n))
			if err != nil {
				return
			}
		}
	}()
	body := bytes.Repeat([]byte{0x71}, 32*1024)
	if n, err := client.Write(body); err != nil || n != len(body) {
		t.Fatalf("socket admission n=%d err=%v", n, err)
	}
	select {
	case <-zeroConsumed:
	case <-ctx.Done():
		t.Fatal("no endpoint-consumed zero-window advertisement")
	}
	before := consumed.Load()
	gate := time.NewTimer(time.Second)
	defer gate.Stop()
	select {
	case <-gate.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if consumed.Load() <= before || zeroAdvertisements.Load() < 2 {
		t.Fatal("reverse TCP stopped during zero-window gate")
	}
	if payloadWhileClosed.Load() != 0 || probes.Load() > 10 {
		t.Fatalf("closed-window traffic escaped: payload=%d probes=%d", payloadWhileClosed.Load(), probes.Load())
	}
	reopen.Store(true)
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	received := make([]byte, len(body))
	if _, err := io.ReadFull(peer, received); err != nil || !bytes.Equal(received, body) {
		t.Fatalf("window reopening did not release exact data: %v", err)
	}
	t.Logf("zero-window reverse_consumed=%d zero_advertisements=%d probes=%d closed_payload=%d recovered=%d", consumed.Load(), zeroAdvertisements.Load(), probes.Load(), payloadWhileClosed.Load(), len(received))
}
