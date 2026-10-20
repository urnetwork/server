package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The Main cluster as it spans edge-6 (nodes 6380-6411) and edge-5 (nodes
// 6412-6427), with the lan hosts and routes the settings give each client.
var (
	redisDialTestLanHosts = map[string]string{
		"by-us-fmt-5-edge-5": "192.168.51.179",
		"by-us-fmt-5-edge-6": "192.168.51.193",
		"by-us-fmt-5-edge-0": "192.168.51.95",
	}
	redisDialTestLanRoutes = map[string]string{
		"by-us-fmt-5-edge-5": "192.168.51.179",
		"by-us-fmt-5-edge-6": "192.168.51.193",
		"by-us-fmt-5-edge-0": "192.168.51.95",
		"snow":               "172.28.208.185",
	}
	redisDialTestSilleRoutes = map[string]string{
		"by-us-fmt-5-edge-5": "172.28.208.176",
		"by-us-fmt-5-edge-6": "172.28.208.177",
		"by-us-fmt-5-edge-0": "192.168.51.95",
		"snow":               "172.28.208.185",
	}
)

func TestRedisNodeDialAddr(t *testing.T) {
	lanRoutes := redisNodeRoutes(redisDialTestLanHosts, redisDialTestLanRoutes)
	silleRoutes := redisNodeRoutes(redisDialTestLanHosts, redisDialTestSilleRoutes)
	for _, test := range []struct {
		name          string
		authorityHost string
		nodeRoutes    map[string]string
		addr          string
		want          string
	}{
		{"lan edge-6 node", "192.168.51.193", lanRoutes, "192.168.51.193:6380", "192.168.51.193:6380"},
		{"lan edge-5 node", "192.168.51.193", lanRoutes, "192.168.51.179:6412", "192.168.51.179:6412"},
		{"lan seed", "192.168.51.193", lanRoutes, "192.168.51.193:6379", "192.168.51.193:6379"},
		{"sille edge-6 node", "172.28.208.177", silleRoutes, "192.168.51.193:6411", "172.28.208.177:6411"},
		{"sille edge-5 node", "172.28.208.177", silleRoutes, "192.168.51.179:6427", "172.28.208.176:6427"},
		{"sille seed", "172.28.208.177", silleRoutes, "172.28.208.177:6379", "172.28.208.177:6379"},
		{"tunnel edge-6 node", "127.0.0.1", lanRoutes, "192.168.51.193:6380", "127.0.0.1:6380"},
		{"tunnel edge-5 node", "127.0.0.1", lanRoutes, "192.168.51.179:6412", "127.0.0.1:6412"},
		{"tunnel localhost", "localhost", lanRoutes, "192.168.51.179:6427", "localhost:6427"},
		{"tunnel ipv6 loopback", "::1", lanRoutes, "192.168.51.179:6412", "[::1]:6412"},
		{"unknown ip passes through", "192.168.51.193", lanRoutes, "10.9.8.7:6380", "10.9.8.7:6380"},
		{"lan host without a route passes through", "192.168.51.193", redisNodeRoutes(redisDialTestLanHosts, map[string]string{}), "192.168.51.179:6412", "192.168.51.179:6412"},
		{"empty announced host uses the authority", "192.168.51.193", lanRoutes, ":6400", "192.168.51.193:6400"},
		{"unknown endpoint uses the authority", "172.28.208.177", silleRoutes, "?:6400", "172.28.208.177:6400"},
		{"unparseable address passes through", "192.168.51.193", lanRoutes, "no-port", "no-port"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := redisNodeDialAddr(test.authorityHost, test.nodeRoutes, test.addr); got != test.want {
				t.Fatalf("redisNodeDialAddr(%q, %q) = %q, want %q", test.authorityHost, test.addr, got, test.want)
			}
		})
	}
}

func TestRedisNodeRoutes(t *testing.T) {
	got := redisNodeRoutes(redisDialTestLanHosts, map[string]string{
		"by-us-fmt-5-edge-5": "172.28.208.176",
		"by-us-fmt-5-edge-6": "",
		"snow":               "172.28.208.185",
	})
	want := map[string]string{"192.168.51.179": "172.28.208.176"}
	if len(got) != len(want) || got["192.168.51.179"] != want["192.168.51.179"] {
		t.Fatalf("redisNodeRoutes = %v, want %v", got, want)
	}
}

// redisDialTestPeers serves every dial from an in-memory RESP peer whose GET
// reply depends on the dialed port: the edge-6 node redirects to edge-5.
type redisDialTestPeers struct {
	mu     sync.Mutex
	dialed []string
	conns  []net.Conn
}

func (p *redisDialTestPeers) dial(_ context.Context, _ string, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	server, client := net.Pipe()
	p.mu.Lock()
	p.dialed = append(p.dialed, addr)
	p.conns = append(p.conns, server, client)
	p.mu.Unlock()
	go func() {
		defer server.Close()
		r := bufio.NewReader(server)
		for {
			args, err := redisDialTestReadCommand(r)
			if err != nil {
				return
			}
			reply := "+OK\r\n"
			switch strings.ToLower(args[0]) {
			case "hello":
				reply = "*0\r\n"
			case "ping":
				reply = "+PONG\r\n"
			case "get":
				switch port {
				case "6380":
					reply = "-MOVED 3999 192.168.51.179:6412\r\n"
				case "6412":
					reply = "$5\r\nedge5\r\n"
				default:
					reply = "$-1\r\n"
				}
			}
			if _, err := io.WriteString(server, reply); err != nil {
				return
			}
		}
	}()
	return client, nil
}

func (p *redisDialTestPeers) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		conn.Close()
	}
}

func redisDialTestReadCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, io.ErrUnexpectedEOF
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || n < 1 || n > 32 {
		return nil, io.ErrUnexpectedEOF
	}
	args := make([]string, n)
	for i := range args {
		head, err := r.ReadString('\n')
		if err != nil || !strings.HasPrefix(head, "$") {
			return nil, io.ErrUnexpectedEOF
		}
		length, err := strconv.Atoi(strings.TrimSpace(head[1:]))
		if err != nil || length < 0 || length > 16384 {
			return nil, io.ErrUnexpectedEOF
		}
		buf := make([]byte, length+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:length])
	}
	return args, nil
}

// A MOVED to an edge-5 node is followed through the same dialer, so the
// redirect lands where this client reaches edge-5: its lan ip on the lan,
// its vpn ip on sille, and loopback through a tunnel.
func TestRedisClusterDialerFollowsMovedToEdge5(t *testing.T) {
	for _, test := range []struct {
		name         string
		authority    string
		routes       map[string]string
		firstDial    string
		redirectDial string
	}{
		{"lan", "192.168.51.193:6379", redisDialTestLanRoutes, "192.168.51.193:6380", "192.168.51.179:6412"},
		{"sille", "172.28.208.177:6379", redisDialTestSilleRoutes, "172.28.208.177:6380", "172.28.208.176:6412"},
		{"tunnel", "127.0.0.1:6379", redisDialTestLanRoutes, "127.0.0.1:6380", "127.0.0.1:6412"},
	} {
		t.Run(test.name, func(t *testing.T) {
			peers := &redisDialTestPeers{}
			defer peers.close()
			client := redis.NewClusterClient(&redis.ClusterOptions{
				Addrs: []string{test.authority},
				ClusterSlots: func(context.Context) ([]redis.ClusterSlot, error) {
					// edge-6 claims every slot so the read lands there and
					// is redirected to edge-5
					return []redis.ClusterSlot{{
						Start: 0,
						End:   16383,
						Nodes: []redis.ClusterNode{{ID: "edge-6", Addr: "192.168.51.193:6380"}},
					}}, nil
				},
				Dialer:          redisClusterDialer(test.authority, redisNodeRoutes(redisDialTestLanHosts, test.routes), peers.dial),
				Protocol:        2,
				DisableIdentity: true,
				MaxRetries:      -1,
				MaxRedirects:    3,
				DialerRetries:   1,
				DialTimeout:     time.Second,
				ReadTimeout:     time.Second,
				WriteTimeout:    time.Second,
				PoolTimeout:     time.Second,
			})
			defer client.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			value, err := client.Get(ctx, "k").Result()
			if err != nil {
				t.Fatalf("get through the redirect: %v", err)
			}
			if value != "edge5" {
				t.Fatalf("get = %q, want the edge-5 node's reply", value)
			}

			peers.mu.Lock()
			dialed := slices.Clone(peers.dialed)
			peers.mu.Unlock()
			if !slices.Contains(dialed, test.firstDial) {
				t.Fatalf("dialed %v, want the edge-6 node at %s", dialed, test.firstDial)
			}
			if !slices.Contains(dialed, test.redirectDial) {
				t.Fatalf("dialed %v, want the MOVED edge-5 node at %s", dialed, test.redirectDial)
			}
			for _, addr := range dialed {
				if strings.HasPrefix(addr, "192.168.51.") && test.name != "lan" {
					t.Fatalf("%s client dialed lan address %s", test.name, addr)
				}
			}
		})
	}
}
