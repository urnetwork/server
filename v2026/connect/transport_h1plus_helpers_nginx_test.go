//go:build unix

package connect

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	connectlib "github.com/urnetwork/connect/v2026"
)

// Exercise production negotiation/carrier helpers through an actual pinned
// NGINX process. Endpoint JWT/device admission has separate integration tests;
// this fixture's synthetic auth gate ensures no 101 precedes authorization.
func TestH1PlusProductionHelpersThroughNginx(t *testing.T) {
	binary := h1plusNginxBinary(t)
	for _, protocol := range []string{connectlib.H1FramerProtocol, connectlib.H1FramerXlProtocol} {
		for _, fallback := range []bool{false, true} {
			name := strings.TrimPrefix(protocol, "urnetwork-") + "/accepted"
			if fallback {
				name = strings.TrimPrefix(protocol, "urnetwork-") + "/legacy_fallback"
			}
			t.Run(name, func(t *testing.T) {
				maximum := 65535
				if protocol == connectlib.H1FramerXlProtocol {
					maximum = 3 * 1024 * 1024
				}
				var mu sync.Mutex
				var requests []h1plusNginxRequest
				var workers sync.WaitGroup
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					workers.Add(1)
					defer workers.Done()
					mu.Lock()
					requests = append(requests, h1plusNginxRequest{path: r.URL.Path, upgrade: r.Header.Get("Upgrade"), remote: r.RemoteAddr, protoMajor: r.ProtoMajor, protoMinor: r.ProtoMinor})
					mu.Unlock()
					if r.Header.Get("X-UR-Network-Auth") != h1plusNginxAuth {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					var conn connectlib.H1MessageConn
					var err error
					if connectlib.IsFramedUpgrade(r, protocol) {
						if fallback {
							http.Error(w, "unsupported", http.StatusUpgradeRequired)
							return
						}
						var raw net.Conn
						raw, err = connectlib.AcceptFramedUpgrade(w, r, protocol, time.Second)
						if err == nil {
							conn, err = connectlib.NewFramedMessageConn(raw, protocol, maximum, nil)
						}
					} else {
						upgrader := websocket.Upgrader{}
						conn, err = upgrader.Upgrade(w, r, nil)
						if err == nil {
							conn.SetReadLimit(int64(maximum))
						}
					}
					if err != nil {
						return
					}
					defer conn.Close()
					_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
					_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					for {
						kind, message, err := connectlib.ReadH1PooledMessage(conn, int64(maximum))
						if err != nil {
							return
						}
						err = conn.WriteMessage(kind, message)
						connectlib.MessagePoolReturn(message)
						if err != nil {
							return
						}
					}
				}))
				t.Cleanup(func() { server.Close(); workers.Wait() })
				front := h1plusNginxProxy(t, binary, server.URL)
				address := "ws" + strings.TrimPrefix(front, "http") + "/connect"
				dialer := &websocket.Dialer{HandshakeTimeout: 3 * time.Second}
				// A failed authorization must produce neither 101 nor a second
				// WebSocket attempt, and must not suppress the subsequent probe.
				unauthorized, err := connectlib.DialH1Messages(context.Background(), address, nil, dialer, protocol, maximum, true, nil)
				if unauthorized != nil {
					unauthorized.Close()
				}
				if err == nil || unauthorized != nil || connectlib.HTTPUpgradeAllowsFallback(err) {
					t.Fatalf("unauthorized helper accepted/downgraded: %v", err)
				}
				stats := &connectlib.H1PlusStats{}
				conn, err := connectlib.DialH1Messages(context.Background(), address, http.Header{"X-Ur-Network-Auth": []string{h1plusNginxAuth}}, dialer, protocol, maximum, true, stats)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_, framed := conn.(*connectlib.FramedMessageConn)
				if framed == fallback {
					t.Fatalf("wrong carrier selected: framed=%v fallback=%v", framed, fallback)
				}
				messages := [][]byte{nil, []byte("first"), bytes.Repeat([]byte{0x83}, 1200), bytes.Repeat([]byte{0xd7}, 65535)}
				if protocol == connectlib.H1FramerXlProtocol {
					messages = append(messages, bytes.Repeat([]byte{0x29}, 65536), bytes.Repeat([]byte{0x8c}, maximum))
				}
				for i, want := range messages {
					if err := conn.WriteMessage(websocket.BinaryMessage, want); err != nil {
						t.Fatalf("write %d: %v", i, err)
					}
					kind, got, err := connectlib.ReadH1PooledMessage(conn, int64(maximum))
					match := kind == websocket.BinaryMessage && bytes.Equal(got, want)
					connectlib.MessagePoolReturn(got)
					if err != nil || !match {
						t.Fatalf("NGINX changed message %d (%d bytes): %v", i, len(want), err)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				wantRequests := 2
				if fallback {
					wantRequests++
				}
				if len(requests) != wantRequests || requests[0].upgrade != protocol || requests[1].upgrade != protocol {
					t.Fatalf("wrong auth/upgrade sequence: %+v", requests)
				}
				for _, request := range requests {
					if request.protoMajor != 1 || request.protoMinor != 1 {
						t.Fatal("NGINX upstream did not preserve H1")
					}
				}
				if fallback && (requests[2].upgrade != "websocket" || requests[1].remote == requests[2].remote) {
					t.Fatal("fallback did not use fresh masked WebSocket connection")
				}
				if stats.Snapshot().Attempts != 1 || (stats.Snapshot().Fallbacks != 0) != fallback {
					t.Fatalf("wrong upgrade diagnostics: %+v", stats.Snapshot())
				}
			})
		}
	}
}
