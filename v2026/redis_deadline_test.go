package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// All sockets are net.Pipe or a resolver callback; none can contact Main.
type deadlineRedisPeer struct {
	stop        chan struct{}
	workers     sync.WaitGroup
	mu          sync.Mutex
	connections []net.Conn
	closing     bool
	dials       atomic.Int64
	commands    sync.Map
	stall       string
	redirect    bool
}

func newDeadlineRedisPeer(t *testing.T, stall string) *deadlineRedisPeer {
	t.Helper()
	p := &deadlineRedisPeer{stop: make(chan struct{}), stall: stall}
	t.Cleanup(p.close)
	return p
}

func (p *deadlineRedisPeer) close() {
	p.mu.Lock()
	if !p.closing {
		p.closing = true
		close(p.stop)
		for _, c := range p.connections {
			c.Close()
		}
	}
	p.mu.Unlock()
	p.workers.Wait()
}

func (p *deadlineRedisPeer) count(command string) int64 {
	if v, ok := p.commands.Load(command); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}
func (p *deadlineRedisPeer) dial(context.Context, string, string) (net.Conn, error) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	p.dials.Add(1)
	peer, client := net.Pipe()
	p.connections = append(p.connections, peer, client)
	p.workers.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.workers.Done()
		defer peer.Close()
		r := bufio.NewReader(peer)
		for {
			line, err := r.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "*") {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err != nil || n < 1 || n > 32 {
				return
			}
			args := make([]string, n)
			for i := range args {
				head, err := r.ReadString('\n')
				if err != nil || !strings.HasPrefix(head, "$") {
					return
				}
				length, err := strconv.Atoi(strings.TrimSpace(head[1:]))
				if err != nil || length < 0 || length > 16384 {
					return
				}
				buf := make([]byte, length+2)
				if _, err := io.ReadFull(r, buf); err != nil {
					return
				}
				args[i] = string(buf[:length])
			}
			command := strings.ToLower(args[0])
			v, _ := p.commands.LoadOrStore(command, &atomic.Int64{})
			v.(*atomic.Int64).Add(1)
			if command == p.stall {
				<-p.stop
				return
			}
			reply := "+OK\r\n"
			switch command {
			case "hello":
				reply = "*0\r\n"
			case "ping":
				reply = "+PONG\r\n"
			case "get":
				if p.redirect {
					reply = "-MOVED 42 other-retirement.invalid:6379\r\n"
				} else {
					reply = "$-1\r\n"
				}
			case "eval":
				reply = ":0\r\n"
			}
			if _, err := io.WriteString(peer, reply); err != nil {
				return
			}
			if command == "hello" && p.stall == "write" {
				<-p.stop
				return
			}
		}
	}()
	return client, nil
}

func deadlineTestRedisOptions(p *deadlineRedisPeer) *redis.Options {
	return &redis.Options{Addr: "retirement.invalid:6379", Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, MaxRetries: -1, DialerRetries: 1,
		ReadTimeout: time.Second, WriteTimeout: time.Second, PoolTimeout: time.Second, DialTimeout: time.Second, PoolSize: 8, MaxActiveConns: 8, Dialer: p.dial}
}
func installDeadlineTestClient(t *testing.T, client RedisClient) {
	t.Helper()
	original := safeDeadlineClient
	safeDeadlineClient = &safeRedisClient{client: client, disableCommandRetry: true, contextTimeoutEnabled: true}
	t.Cleanup(func() { safeDeadlineClient = original; client.Close() })
}
func checkDeadlineFault(t *testing.T, run func(context.Context) error) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	err := run(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline fault lost returned cause: %v", err)
	}
	if elapsed < 850*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("controlled one-second operation escaped bound: %s", elapsed)
	}
	t.Logf("bounded_fault_elapsed_seconds=%.6f", elapsed.Seconds())
	return elapsed
}

func TestRedisWithDeadlineRejectsMissingOrExpiredDeadline(t *testing.T) {
	calls := 0
	callback := func(RedisClient) error { calls++; return nil }
	if err := RedisWithDeadline(context.Background(), callback); err == nil {
		t.Fatal("unbounded operation admitted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	cancel()
	if err := RedisWithDeadline(ctx, callback); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled operation admitted")
	}
	if calls != 0 {
		t.Fatal("invalid operation reached callback")
	}
}

func TestRedisWithDeadlineKeepsOrdinaryPoolsUnchanged(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		calls := 0
		err := RedisWithDeadline(ctx, func(client RedisClient) error {
			calls++
			switch r := client.(type) {
			case *redis.Client:
				o := r.Options()
				if !o.ContextTimeoutEnabled || o.MaxRetries != 0 || o.DialerRetries != 1 || o.MinIdleConns != 0 || o.PoolSize > 8 || o.ReadTimeout != time.Second || o.WriteTimeout != time.Second || o.PoolTimeout != time.Second || o.DialTimeout != time.Second {
					t.Fatal("optional standalone pool lost finite policy")
				}
			case *redis.ClusterClient:
				o := r.Options()
				if !o.ContextTimeoutEnabled || !o.DisableRoutingPolicies || o.MaxRetries > 0 || o.MaxRedirects != 0 || o.DialerRetries != 1 || o.MinIdleConns != 0 || o.PoolSize > 8 || o.ReadTimeout != time.Second || o.WriteTimeout != time.Second || o.PoolTimeout != time.Second || o.DialTimeout != time.Second {
					t.Fatal("optional cluster pool lost finite policy")
				}
			default:
				t.Fatalf("unexpected optional pool %T", client)
			}
			return errors.New("synthetic optional operation failure")
		})
		if err == nil || calls != 1 {
			t.Fatal("optional callback failure was retried or swallowed")
		}
		for _, pool := range []*safeRedisClient{safeClient, safeNoCommandRetryClient} {
			client := pool.open()
			if client == safeDeadlineClient.current() {
				t.Fatal("optional work reused ordinary pool")
			}
			switch r := client.(type) {
			case *redis.Client:
				if r.Options().ContextTimeoutEnabled || r.Options().ReadTimeout != 15*time.Second {
					t.Fatal("ordinary standalone policy changed")
				}
			case *redis.ClusterClient:
				if r.Options().ContextTimeoutEnabled || r.Options().ReadTimeout != 15*time.Second {
					t.Fatal("ordinary cluster policy changed")
				}
			}
		}
		RedisReset()
		if safeDeadlineClient.current() != nil {
			t.Fatal("reset retained optional pool")
		}
	})
}

func TestRedisWithDeadlineNativeIoFaults(t *testing.T) {
	for _, phase := range []string{"hello", "write", "ping", "get", "eval"} {
		t.Run(phase, func(t *testing.T) {
			peer := newDeadlineRedisPeer(t, phase)
			client := redis.NewClient(deadlineTestRedisOptions(peer))
			installDeadlineTestClient(t, client)
			calls := 0
			checkDeadlineFault(t, func(ctx context.Context) error {
				return RedisWithDeadline(ctx, func(r RedisClient) error {
					calls++
					if phase == "eval" {
						return RedisRemoveIfEqual(r, ctx, "synthetic-resident", []byte("captured")).Err()
					}
					return r.Get(ctx, "synthetic-resident").Err()
				})
			})
			want := 0
			if phase == "get" || phase == "eval" {
				want = 1
			}
			if calls != want || peer.dials.Load() != 1 {
				t.Fatalf("fault caused callback/dial replay: calls=%d dials=%d", calls, peer.dials.Load())
			}
		})
	}
}

func TestRedisWithDeadlinePoolWaitFault(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "")
	options := deadlineTestRedisOptions(peer)
	options.PoolSize, options.MaxActiveConns = 1, 1
	client := redis.NewClient(options)
	installDeadlineTestClient(t, client)
	held := client.Conn()
	defer held.Close()
	warm, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := held.Ping(warm).Err(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	checkDeadlineFault(t, func(ctx context.Context) error {
		return RedisWithDeadline(ctx, func(RedisClient) error { calls++; return nil })
	})
	if calls != 0 || peer.dials.Load() != 1 {
		t.Fatal("pool exhaustion created extra work")
	}
}

func TestRedisWithDeadlineDialAndResolverFaults(t *testing.T) {
	for _, phase := range []string{"dial", "resolver"} {
		t.Run(phase, func(t *testing.T) {
			peer := newDeadlineRedisPeer(t, "")
			options := deadlineTestRedisOptions(peer)
			var dials, dns atomic.Int64
			var resolverCalls sync.WaitGroup
			var resolverMu sync.Mutex
			resolverClosing := false
			closeResolver := func() {
				resolverMu.Lock()
				resolverClosing = true
				resolverMu.Unlock()
				resolverCalls.Wait()
			}
			t.Cleanup(closeResolver)
			if phase == "dial" {
				options.Dialer = func(ctx context.Context, _, _ string) (net.Conn, error) {
					dials.Add(1)
					<-ctx.Done()
					return nil, ctx.Err()
				}
			} else {
				dialer := NewDialer(time.Second)
				dialer.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
					resolverMu.Lock()
					if resolverClosing {
						resolverMu.Unlock()
						return nil, context.Canceled
					}
					resolverCalls.Add(1)
					resolverMu.Unlock()
					defer resolverCalls.Done()
					dns.Add(1)
					<-ctx.Done()
					return nil, ctx.Err()
				}}
				options.Dialer = func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					return dialer.DialContext(ctx, network, address)
				}
			}
			client := redis.NewClient(options)
			installDeadlineTestClient(t, client)
			calls := 0
			checkDeadlineFault(t, func(ctx context.Context) error {
				return RedisWithDeadline(ctx, func(RedisClient) error { calls++; return nil })
			})
			client.Close()
			// A canceled Go lookup can return before its internal completion.
			// Close admission and join admitted callbacks. Later callbacks touch
			// only the stable gate, without entering the group or counters.
			closeResolver()
			if calls != 0 || dials.Load() != 1 || (phase == "resolver" && dns.Load() == 0) {
				t.Fatalf("setup fault lost single-attempt bound: calls=%d dials=%d dns=%d", calls, dials.Load(), dns.Load())
			}
		})
	}
}

func TestRedisWithDeadlineClusterRedirectDoesNotReplay(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "")
	peer.redirect = true
	client := redis.NewClusterClient(deadlineTestClusterOptions(t, peer))
	installDeadlineTestClient(t, client)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls := 0
	err := RedisWithDeadline(ctx, func(r RedisClient) error { calls++; return r.Get(ctx, "synthetic-resident").Err() })
	if err == nil || calls != 1 || peer.count("get") != 1 || peer.dials.Load() != 1 {
		t.Fatalf("redirect replayed optional read: err=%v calls=%d gets=%d dials=%d", err, calls, peer.count("get"), peer.dials.Load())
	}
}

func TestRedisWithDeadlineClusterStateWaitHonorsContext(t *testing.T) {
	var lookups atomic.Int64
	peer := newDeadlineRedisPeer(t, "")
	options := deadlineTestClusterOptions(t, peer)
	options.Dialer = func(context.Context, string, string) (net.Conn, error) { return nil, fmt.Errorf("unexpected dial") }
	options.ClusterSlots = func(ctx context.Context) ([]redis.ClusterSlot, error) {
		lookups.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	client := redis.NewClusterClient(options)
	installDeadlineTestClient(t, client)
	calls := 0
	checkDeadlineFault(t, func(ctx context.Context) error {
		return RedisWithDeadline(ctx, func(RedisClient) error { calls++; return nil })
	})
	if calls != 0 || lookups.Load() != 1 {
		t.Fatal("cluster-state fault was replayed or reached callback")
	}
}

// Construct options through the real production pool open(), using only local
// synthetic resource bytes. Replace transport and slot discovery before the
// first command; policy/timeout/retry/cap fields remain producer-owned.
func deadlineTestClusterOptions(t *testing.T, peer *deadlineRedisPeer) *redis.ClusterOptions {
	t.Helper()
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: true\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 0\n"))
	defer popConfig()
	pool := &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true}
	constructed, ok := pool.open().(*redis.ClusterClient)
	if !ok {
		t.Fatal("production optional cluster constructor changed type")
	}
	options := *constructed.Options()
	pool.close()
	if !options.ContextTimeoutEnabled || !options.DisableRoutingPolicies || options.MaxRedirects != 0 || options.MaxRetries > 0 || options.DialerRetries != 1 || options.MinIdleConns != 0 || options.PoolSize < 1 || options.PoolSize > 8 || options.MaxActiveConns < 1 || options.MaxActiveConns > 8 || options.ReadTimeout != time.Second || options.WriteTimeout != time.Second || options.PoolTimeout != time.Second || options.DialTimeout != time.Second {
		t.Fatal("actual production cluster options lost the bounded optional policy")
	}
	// open() already normalized zero redirects. Reconstructing a client
	// requires the -1 input sentinel; raw zero would restore default retries.
	options.MaxRedirects = -1
	options.Protocol, options.DisableIdentity = 2, true // synthetic RESP peer only
	options.Password = ""                               // synthetic peer has no auth; no real secret is read
	options.Dialer = peer.dial
	// PING has no key and can select a bootstrap node. Use the same synthetic
	// node for bootstrap and slots so one logical node owns both PING and GET.
	// A non-loopback name also avoids slot-origin loopback rewriting.
	const address = "retirement.invalid:6379"
	options.Addrs = []string{address}
	options.ClusterSlots = func(context.Context) ([]redis.ClusterSlot, error) {
		return []redis.ClusterSlot{{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{Addr: address}}}}, nil
	}
	return &options
}

func TestRedisWithDeadlineColdClusterSkipsDetachedCommandMetadata(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "command")
	client := redis.NewClusterClient(deadlineTestClusterOptions(t, peer))
	installDeadlineTestClient(t, client)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := RedisWithDeadline(ctx, func(r RedisClient) error {
		if err := r.Get(ctx, "synthetic-resident").Err(); !errors.Is(err, RedisNil) {
			return fmt.Errorf("synthetic missing read changed: %w", err)
		}
		return RedisRemoveIfEqual(r, ctx, "synthetic-resident", []byte("original")).Err()
	})
	if err != nil || peer.count("command") != 0 || peer.count("ping") != 1 || peer.count("get") != 1 || peer.count("eval") != 1 {
		t.Fatalf("cold single-key cleanup escaped into detached metadata: err=%v command=%d", err, peer.count("command"))
	}
}

func TestRedisWithDeadlineResetStopsOwnedDialMaintenance(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "")
	options := deadlineTestRedisOptions(peer)
	options.PoolSize, options.MaxActiveConns = 1, 1 // trigger recovery after one failed admission
	var dials atomic.Int64
	entered, returned := make(chan struct{}), make(chan struct{})
	options.Dialer = func(ctx context.Context, _, _ string) (net.Conn, error) {
		attempt := dials.Add(1)
		if attempt == 1 {
			return nil, errors.New("synthetic connection failure")
		}
		if attempt == 2 {
			defer close(returned)
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > time.Second {
				t.Error("maintenance dial lost native cap")
			}
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, errors.New("unexpected post-reset maintenance")
	}
	client := redis.NewClient(options)
	installDeadlineTestClient(t, client)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := RedisWithDeadline(ctx, func(RedisClient) error { t.Error("failed admission reached callback"); return nil }); err == nil {
		t.Fatal("dial failure was swallowed")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance control did not start")
	}
	start := time.Now()
	RedisReset()
	if safeDeadlineClient.current() != nil {
		t.Fatal("reset retained optional pool")
	}
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("process-owned maintenance exceeded native deadline after reset")
	}
	// go-redis's recovery loop has a one-second cooldown before checking closed.
	// It is owned by the pool, not a late capture or a Tunnel.Close join.
	time.Sleep(1100 * time.Millisecond)
	if dials.Load() != 2 {
		t.Fatal("reset allowed a later maintenance attempt")
	}
	// Closing fixture admission is idempotent and cannot admit a late native
	// recovery dial, even when a prior assertion exits through t.Cleanup.
	peer.close()
	if conn, err := peer.dial(context.Background(), "tcp", "retirement.invalid:6379"); conn != nil || !errors.Is(err, net.ErrClosed) || peer.dials.Load() != 0 {
		t.Fatal("closed fixture admitted a late dial")
	}
	peer.close()
	t.Logf("reset_and_maintenance_quiescence_seconds=%.6f", time.Since(start).Seconds())
}
