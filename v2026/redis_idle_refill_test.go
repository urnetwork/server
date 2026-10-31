package server

// The ordinary pool's capacity policy against go-redis' refill of idle
// connections. Every socket is a net.Pipe inside a synctest bubble and every
// dial waits for the test's verdict, so each step runs the pool to a durable
// block before the next: no wall clock, no scheduler luck, no deployment.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/redis/go-redis/v9"
)

// A dial the pool waits on. The test connects (true) or refuses (false) it.
type idleRefillDial struct {
	verdict chan bool
}

// A synthetic Redis reached only through its dialer. A GET of a key in
// keyReleases waits until the test closes that key's channel.
type idleRefillPeer struct {
	dials       chan *idleRefillDial
	heldKeys    chan string
	keyReleases map[string]chan struct{}
	closed      chan struct{}
}

func newIdleRefillPeer(heldKeys ...string) *idleRefillPeer {
	peer := &idleRefillPeer{
		dials:       make(chan *idleRefillDial, 16),
		heldKeys:    make(chan string, 16),
		keyReleases: map[string]chan struct{}{},
		closed:      make(chan struct{}),
	}
	for _, key := range heldKeys {
		peer.keyReleases[key] = make(chan struct{})
	}
	return peer
}

func (self *idleRefillPeer) dial(ctx context.Context, _ string, _ string) (net.Conn, error) {
	dial := &idleRefillDial{verdict: make(chan bool, 1)}
	self.dials <- dial
	select {
	case connect := <-dial.verdict:
		if !connect {
			return nil, errors.New("synthetic dial refused")
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-self.closed:
		return nil, net.ErrClosed
	}
	peerConn, conn := net.Pipe()
	go func() {
		defer peerConn.Close()
		r := bufio.NewReader(peerConn)
		for {
			line, err := r.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "*") {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err != nil || n < 1 || 32 < n {
				return
			}
			args := make([]string, n)
			for i := range args {
				head, err := r.ReadString('\n')
				if err != nil || !strings.HasPrefix(head, "$") {
					return
				}
				length, err := strconv.Atoi(strings.TrimSpace(head[1:]))
				if err != nil || length < 0 || 16384 < length {
					return
				}
				buf := make([]byte, length+2)
				if _, err := io.ReadFull(r, buf); err != nil {
					return
				}
				args[i] = string(buf[:length])
			}
			reply := "+OK\r\n"
			switch strings.ToLower(args[0]) {
			case "hello":
				reply = "*0\r\n"
			case "get":
				if release, ok := self.keyReleases[args[1]]; ok {
					self.heldKeys <- args[1]
					select {
					case <-release:
					case <-self.closed:
						return
					}
				}
				reply = "$-1\r\n"
			}
			if _, err := io.WriteString(peerConn, reply); err != nil {
				return
			}
		}
	}()
	return conn, nil
}

// The ordinary pool keeps min_connections idle, and go-redis refills that
// floor by counting a slot before dialing it. With MaxActiveConns at the pool
// size, a refill could take the last slots after a caller holding a pool turn
// found no idle connection and before its own dial was counted: the caller was
// refused with ErrPoolExhausted although its turn stood for a connection. That
// failed TestNetworkPeerChurn, whose 32 peers share a 16 connection pool, on
// a loaded host. There the window is scheduling; here MaxConcurrentDials = 1
// holds the caller in it behind another dial while the refill counts the
// pool's last slot.
func TestRedisIdleRefillDoesNotRefuseCallerHoldingPoolTurn(t *testing.T) {
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 3\nmax_connections: 4\n"))
	defer popConfig()
	pool := &safeRedisClient{}
	constructed, ok := pool.open().(*redis.Client)
	if !ok {
		t.Fatal("ordinary constructor did not make a standalone pool")
	}
	options := *constructed.Options()
	pool.close()
	if options.PoolSize != 4 || options.MinIdleConns != 3 || options.MaxIdleConns != 4 {
		t.Fatal("ordinary pool lost its configured capacity", options.PoolSize, options.MinIdleConns, options.MaxIdleConns)
	}
	// The harness, not the policy: synthetic sockets, one attempt per dial
	// and one dial at a time.
	options.Protocol, options.DisableIdentity, options.Password = 2, true, ""
	options.Addr = deadlineTestRedisAddress
	options.DialerRetries = 1
	options.MaxConcurrentDials = 1

	synctest.Test(t, func(t *testing.T) {
		peer := newIdleRefillPeer("synthetic-hold-1", "synthetic-hold-2")
		options.Dialer = peer.dial
		client := redis.NewClient(&options)
		// Release every waiting dial and held key, so an early failure still
		// lets each goroutine of the bubble return.
		t.Cleanup(func() {
			close(peer.closed)
			client.Close()
		})

		takeDial := func(what string) *idleRefillDial {
			t.Helper()
			select {
			case dial := <-peer.dials:
				return dial
			default:
				t.Fatalf("no dial waits for %s", what)
				return nil
			}
		}
		noDial := func(what string) {
			t.Helper()
			select {
			case <-peer.dials:
				t.Fatalf("unexpected dial %s", what)
			default:
			}
		}
		takeHeldKey := func(key string) {
			t.Helper()
			select {
			case heldKey := <-peer.heldKeys:
				if heldKey != key {
					t.Fatalf("held %s, want %s", heldKey, key)
				}
			default:
				t.Fatalf("%s did not reach the peer", key)
			}
		}
		get := func(key string) chan error {
			result := make(chan error, 1)
			go func() {
				result <- client.Get(t.Context(), key).Err()
			}()
			return result
		}
		requireNil := func(result chan error, what string) {
			t.Helper()
			select {
			case err := <-result:
				if !errors.Is(err, redis.Nil) {
					t.Fatalf("%s: %v", what, err)
				}
			default:
				t.Fatalf("%s did not return", what)
			}
		}

		// the constructor counts and dials the idle floor
		synctest.Wait()
		refills := []*idleRefillDial{takeDial("refill 1"), takeDial("refill 2"), takeDial("refill 3")}
		noDial("beyond the idle floor")

		// a holds the last free turn, finds no idle connection and dials,
		// taking the only dial admission
		a := get("synthetic-a")
		synctest.Wait()
		aDial := takeDial("a")

		// a refused refill frees its turn and its slot and starts no refill
		refills[0].verdict <- false
		synctest.Wait()
		noDial("after a refused refill")

		// the turn holder takes the freed turn, finds no idle connection and
		// waits behind a's dial for admission to dial
		turnHolder := get("synthetic-turn-holder")
		synctest.Wait()
		noDial("while the turn holder waits for admission")
		select {
		case err := <-turnHolder:
			t.Fatalf("turn holder returned before it dialed: %v", err)
		default:
		}

		// two refills become idle connections; each caller that takes one
		// below the floor makes the refill count one more slot, until the
		// refill has counted every slot of the pool
		refills[1].verdict <- true
		refills[2].verdict <- true
		synctest.Wait()
		holder1 := get("synthetic-hold-1")
		synctest.Wait()
		takeHeldKey("synthetic-hold-1")
		takeDial("refill 4").verdict <- true
		synctest.Wait()
		close(peer.keyReleases["synthetic-hold-1"])
		synctest.Wait()
		requireNil(holder1, "hold 1")
		holder2 := get("synthetic-hold-2")
		synctest.Wait()
		takeHeldKey("synthetic-hold-2")
		lastRefill := takeDial("refill 5")
		noDial("beyond the pool size")

		// a's dial connects and passes its admission to the turn holder,
		// whose dial must proceed rather than be refused
		aDial.verdict <- true
		synctest.Wait()
		requireNil(a, "a")
		select {
		case err := <-turnHolder:
			t.Fatalf("caller holding a pool turn was refused: %v", err)
		default:
		}
		takeDial("the turn holder").verdict <- true
		synctest.Wait()
		requireNil(turnHolder, "turn holder")

		close(peer.keyReleases["synthetic-hold-2"])
		lastRefill.verdict <- true
		synctest.Wait()
		requireNil(holder2, "hold 2")
		noDial("after every caller returned")
	})
}
