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

// The failed H1 replay dropped four consecutive uplink TCP payload segments,
// then kept receiving reverse payload/ACKs without another uplink transmission.
// Exercise that exact directional-link queue-refusal boundary with a real
// Connect/gVisor socket. Reverse bytes must reach the sending socket's Read,
// not just a link queue. All stack timer and congestion settings stay default.
func TestH1OuterTCPTailDropRecoversDuringReverseTraffic(t *testing.T) {
	for _, arm := range []struct {
		name    string
		reverse bool
		drops   uint64
		delay   time.Duration
		bytes   int
	}{
		{"quiet", false, 4, 20 * time.Millisecond, 4 * 1280},
		{"reverse-data", true, 4, 20 * time.Millisecond, 4 * 1280},
		{"high-rtt-no-loss", false, 0, 250 * time.Millisecond, 4 * 1280},
		{"delayed-ack-no-loss", false, 0, 20 * time.Millisecond, 128},
		{"sack-gap", false, 1, 20 * time.Millisecond, 4 * 1280},
	} {
		t.Run(arm.name, func(t *testing.T) {
			reverse := arm.reverse
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			profile := initialNetworkProfiles(20260810)["clean-lan"]
			profile.InnerMtu = 1360
			profile.Forward.BaseDelay, profile.Reverse.BaseDelay = arm.delay, arm.delay
			profile.Forward.Jitter, profile.Reverse.Jitter = 0, 0
			profile.Forward.AllowQueueDrops = true
			path, err := newTunPath(ctx, profile, mobileTunResourceProfile())
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
			// One acknowledged warm byte starts both directional RTT estimators.
			if _, err = client.Write([]byte{0x71}); err != nil {
				t.Fatal(err)
			}
			warm := make([]byte, 1)
			if _, err = io.ReadFull(peer, warm); err != nil {
				t.Fatal(err)
			}
			if _, err = peer.Write(warm); err != nil {
				t.Fatal(err)
			}
			if _, err = io.ReadFull(client, warm); err != nil {
				t.Fatal(err)
			}
			if !path.waitForTerminalIdle(ctx) {
				t.Fatal("warm link did not drain")
			}

			var armed atomic.Bool
			var dropped, payloadAttempts, reverseAckPackets, consumedReverse atomic.Uint64
			var firstMissing atomic.Uint32
			path.network.setPacketObserver(func(source string, packet []byte) {
				if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 {
					return
				}
				ipLen := int(packet[0]&15) * 4
				if len(packet) < ipLen+20 {
					return
				}
				tcpLen := int(packet[ipLen+12]>>4) * 4
				payload := int(binary.BigEndian.Uint16(packet[2:4])) - ipLen - tcpLen
				if source == "right" {
					if dropped.Load() > 0 && payload > 0 && binary.BigEndian.Uint32(packet[ipLen+8:]) == firstMissing.Load() {
						reverseAckPackets.Add(1)
					}
					return
				}
				if source != "left" {
					return
				}
				// readTun invokes this callback immediately before this packet's
				// submit. Its sole source reader cannot interleave another packet.
				refuse := false
				if armed.Load() && payload > 0 {
					payloadAttempts.Add(1)
					if dropped.Load() < arm.drops {
						if dropped.Load() == 0 {
							firstMissing.Store(binary.BigEndian.Uint32(packet[ipLen+4:]))
						}
						dropped.Add(1)
						refuse = true
					}
				}
				path.forwardLink.stateLock.Lock()
				path.forwardLink.profile.QueuePacketCount = profile.Forward.QueuePacketCount
				if refuse {
					path.forwardLink.profile.QueuePacketCount = 0
				}
				path.forwardLink.stateLock.Unlock()
			})
			defer path.network.setPacketObserver(nil)
			var workers sync.WaitGroup
			stop := make(chan struct{})
			defer func() { close(stop); client.Close(); peer.Close(); workers.Wait() }()
			if reverse {
				workers.Add(2)
				go func() {
					defer workers.Done()
					body := bytes.Repeat([]byte{0x39}, 128)
					tick := time.NewTicker(5 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-stop:
							return
						case <-tick.C:
							if _, err := peer.Write(body); err != nil {
								return
							}
						}
					}
				}()
				go func() {
					defer workers.Done()
					body := make([]byte, 4096)
					for {
						n, err := client.Read(body)
						consumedReverse.Add(uint64(n))
						if err != nil {
							return
						}
					}
				}()
			}
			beforeRetransmits := path.left.Stats().TCP.Retransmits.Value()
			beforeTimeouts := path.left.Stats().TCP.Timeouts.Value()
			beforeSackRecovery := path.left.Stats().TCP.SACKRecovery.Value()
			beforeDrops := path.forwardLink.counters.queueDropPacketCount.Load()
			armed.Store(true)
			body := bytes.Repeat([]byte{0x5a}, arm.bytes)
			started := time.Now()
			if n, err := client.Write(body); err != nil || n != len(body) {
				t.Fatalf("local socket admission n=%d err=%v", n, err)
			}
			// Five seconds is below the unchanged 30s logical ACK lifetime and
			// many warm-path RTOs. This is a progress gate, not a timer setting.
			peer.SetReadDeadline(started.Add(5 * time.Second))
			received := make([]byte, len(body))
			n, readErr := io.ReadFull(peer, received)
			if arm.drops == 0 && readErr == nil {
				// Wait for the real delayed TCP ACK before evaluating no-loss
				// timer behavior; no synthetic ACK or extra sender write is used.
				if !path.waitForTerminalIdle(ctx) {
					t.Fatal("clean delivery did not drain")
				}
				time.Sleep(250 * time.Millisecond)
			}
			info, infoErr := client.(*clientconnect.TunTcpConn).TcpInfo()
			retransmits := path.left.Stats().TCP.Retransmits.Value() - beforeRetransmits
			timeouts := path.left.Stats().TCP.Timeouts.Value() - beforeTimeouts
			sackRecovery := path.left.Stats().TCP.SACKRecovery.Value() - beforeSackRecovery
			t.Logf("outer-tail-drop reverse=%t dropped=%d payload_attempts=%d reverse_ack_packets=%d consumed_reverse=%d received=%d elapsed=%s cwnd=%d RTO=%s retransmits=%d timeouts=%d sack_recovery=%d", reverse, dropped.Load(), payloadAttempts.Load(), reverseAckPackets.Load(), consumedReverse.Load(), n, time.Since(started), info.SndCwnd, info.RTO, retransmits, timeouts, sackRecovery)
			if dropped.Load() != arm.drops || path.forwardLink.counters.queueDropPacketCount.Load()-beforeDrops != arm.drops || infoErr != nil {
				t.Fatalf("fault did not inject exact queue drops: dropped=%d want=%d info_err=%v", dropped.Load(), arm.drops, infoErr)
			}
			if reverse && (reverseAckPackets.Load() == 0 || consumedReverse.Load() == 0) {
				t.Fatal("reverse ACK-bearing bytes never reached the sender application")
			}
			if readErr != nil || !bytes.Equal(received, body) || (arm.drops > 0 && retransmits == 0) {
				t.Fatalf("outer TCP did not recover before logical ACK lifetime: read=%v retransmits=%d", readErr, retransmits)
			}
			if arm.drops == 0 && (retransmits != 0 || timeouts != 0) {
				t.Fatalf("clean path gained recovery: retransmits=%d timeouts=%d", retransmits, timeouts)
			}
			if arm.name == "sack-gap" && sackRecovery == 0 {
				t.Fatal("single-gap control did not enter SACK recovery")
			}
		})
	}
}
