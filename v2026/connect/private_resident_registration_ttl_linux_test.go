package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"google.golang.org/protobuf/proto"
)

// Private diagnostic only: real, separately owned Redis; no database fixture,
// shared resource, fake Redis, replaced model method, or production endpoint.
func privateRegistrationRedis(t *testing.T) server.RedisClient {
	t.Helper()
	authority := os.Getenv("PRIVATE_REGISTRATION_REDIS")
	if authority == "" {
		t.Skip("requires the private resident-registration Redis runner")
	}
	host, _, err := net.SplitHostPort(authority)
	if err != nil || host != "127.0.0.1" || os.Getenv("WARP_ENV") != "local" {
		t.Fatal("private Redis must have an exact local loopback endpoint")
	}
	server.RedisReset()
	popVault := server.Vault.PushSimpleResource("redis.yml", []byte(fmt.Sprintf("authority: %q\npassword: \"\"\ndb: 0\ncluster: false\n", authority)))
	popConfig := server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 4\nmax_retries: -1\n"))
	t.Cleanup(func() { server.RedisReset(); popConfig(); popVault() })
	var r server.RedisClient
	server.Redis(context.Background(), func(client server.RedisClient) { r = client })
	if err := r.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	return r
}

func privateRegistrationKey(id server.Id) string { return fmt.Sprintf("ncr_%s", id) }

func privateRegistrationNominee() *model.NetworkClientResident {
	return &model.NetworkClientResident{ClientId: server.NewId(), InstanceId: server.NewId(), ResidentId: server.NewId(), ResidentHost: "127.0.0.1", ResidentService: "connect", ResidentBlock: "private-ttl"}
}

func privateNominate(t *testing.T, r server.RedisClient, nominee *model.NetworkClientResident, replace *server.Id, ttl time.Duration) {
	t.Helper()
	if !model.NominateResident(context.Background(), replace, nominee, ttl) {
		t.Fatal("private nominee was refused")
	}
	t.Cleanup(func() { r.Del(context.Background(), privateRegistrationKey(nominee.ClientId)) })
}

func privateWait(t *testing.T, budget time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func privateJoin(t *testing.T, done <-chan struct{}, owner string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s failed its private cleanup join", owner)
	}
}

// Production runs forwards inside server.HandleError. Keep the private
// boundary narrower: only the exact expected cancellation errors are benign,
// and only after this forward's own context has actually been canceled.
func privateRunResidentForward(t *testing.T, f *ResidentForward) (canceled bool) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok || f.ctx.Err() == nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, server.DbContextDoneError)) {
				panic(recovered)
			}
			canceled = true
		}
	}()
	f.Run()
	return false
}

type privateRegistrationCancelBarrier struct {
	key, command string
	entered      chan struct{}
	release      chan struct{}
	once         sync.Once
}

func (h *privateRegistrationCancelBarrier) DialHook(next redis.DialHook) redis.DialHook {
	return next
}
func (h *privateRegistrationCancelBarrier) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *privateRegistrationCancelBarrier) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == h.command && (h.command == "ping" || (len(args) > 1 && args[1] == h.key)) {
			h.once.Do(func() { close(h.entered); <-h.release })
		}
		return next(ctx, cmd)
	}
}

func TestPrivateResidentRegistrationForwardCancellationAtLookupJoins(t *testing.T) {
	for _, command := range []string{"ping", "get"} {
		t.Run(command, func(t *testing.T) {
			r := privateRegistrationRedis(t)
			n := privateRegistrationNominee()
			privateNominate(t, r, n, nil, time.Second)
			barrier := &privateRegistrationCancelBarrier{key: privateRegistrationKey(n.ClientId), command: command, entered: make(chan struct{}), release: make(chan struct{})}
			r.AddHook(barrier)
			f := NewResidentForward(context.Background(), &Exchange{settings: privateTTLSettings()}, n.ClientId)
			var witnesses [][]byte
			for _, body := range []string{"pending-at-cancel", "queued-at-cancel"} {
				message := clientconnect.MessagePoolCopy([]byte(body))
				witnesses = append(witnesses, retainResidentPoolWitness(message))
				f.send <- message
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
			done := make(chan struct{})
			result := make(chan bool, 1)
			go func() { defer close(done); result <- privateRunResidentForward(t, f) }()
			defer func() {
				f.Cancel()
				release()
				privateJoin(t, done, "canceled lookup forward")
			}()
			privateJoin(t, barrier.entered, "real Redis command barrier")
			f.Cancel()
			release()
			privateJoin(t, done, "canceled lookup forward")
			cancellationRaised := <-result
			if command == "ping" && !cancellationRaised {
				t.Fatal("real canceled Redis PING did not exercise the expected cancellation boundary")
			}
			requireResidentPoolOwnersReturned(t, witnesses, "pending and queued owners after canceled lookup")
			t.Logf("real_redis_command=%s canceled_at_barrier=true exact_cancellation_raised=%t forward_joined=true pending_and_queued_payload_owners_returned=true", command, cancellationRaised)
		})
	}
}

type privateRegistrationHook struct {
	key           string
	gets, expires atomic.Int64
	afterGet      func(context.Context)
}

func (h *privateRegistrationHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *privateRegistrationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *privateRegistrationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if len(args) > 1 && args[1] == h.key {
			switch strings.ToLower(cmd.Name()) {
			case "get":
				h.gets.Add(1)
				if h.afterGet != nil {
					h.afterGet(ctx)
				}
			case "expire":
				h.expires.Add(1)
			}
		}
		return err
	}
}

// A bound, non-listening socket owns this port for the entire control. The
// kernel refuses TCP there; another local task cannot steal a released port.
func privateRefusedPort(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	addr, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return addr.(*syscall.SockaddrInet4).Port
}

func privateTTLSettings() *ExchangeSettings {
	s := DefaultExchangeSettingsWithBufferSize(4)
	s.ForwardBufferSize = 4
	s.ExchangeResidentTtl = time.Second
	s.ForwardIdleTimeout = 3 * time.Second
	s.ExchangeReconnectAfterErrorTimeout = 50 * time.Millisecond
	s.ExchangeConnectTimeout = 100 * time.Millisecond
	s.ExchangePingTimeout = time.Second
	s.ExchangeReadTimeout = 5 * time.Second
	s.ExchangeReadHeaderTimeout = time.Second
	s.ExchangeWriteHeaderTimeout = time.Second
	s.EnableNetworkPeers = false
	s.ForwardEnforceActiveContracts = false
	return s
}

func TestPrivateResidentRegistrationOwnerHeartbeatAndHandoff(t *testing.T) {
	r := privateRegistrationRedis(t)
	ctx := context.Background()
	n := privateRegistrationNominee()
	privateNominate(t, r, n, nil, time.Second)
	s := privateTTLSettings()
	e := &Exchange{settings: s}
	owner := &Resident{ctx: ctx, clientId: n.ClientId, instanceId: n.InstanceId, residentId: n.ResidentId}
	start, calls := time.Now(), 0
	for time.Since(start) < 2200*time.Millisecond {
		if !e.refreshResidentRegistration(owner) {
			t.Fatal("healthy owner's heartbeat was refused")
		}
		calls++
		time.Sleep(s.ExchangeResidentTtl / 4)
	}
	before := r.PTTL(ctx, privateRegistrationKey(n.ClientId)).Val()
	if before <= 0 {
		t.Fatal("healthy owner expired despite heartbeat")
	}
	replacement := *n
	replacement.ResidentId = server.NewId()
	privateNominate(t, r, &replacement, &n.ResidentId, time.Second)
	stale := *n
	stale.ResidentId = server.NewId()
	if model.NominateResident(ctx, &n.ResidentId, &stale, time.Second) {
		t.Fatal("stale nomination replaced the successor")
	}
	if e.refreshResidentRegistration(owner) {
		t.Fatal("retired owner was reported current")
	}
	model.RemoveResidentForClient(ctx, n.ClientId, n.ResidentId)
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, time.Second)
	defer cleanupCancel()
	if err := model.RemoveResidentForClientWithDeadline(cleanupCtx, n.ClientId, n.ResidentId); err != nil {
		t.Fatal(err)
	}
	got := model.GetResidentForClient(ctx, n.ClientId, 0)
	if got == nil || got.ResidentId != replacement.ResidentId {
		t.Fatal("old-owner cleanup removed the successor")
	}
	current := &Resident{ctx: ctx, clientId: replacement.ClientId, instanceId: replacement.InstanceId, residentId: replacement.ResidentId}
	if !e.refreshResidentRegistration(current) {
		t.Fatal("successor heartbeat was refused")
	}
	t.Logf("scaled_ttl_ms=1000 heartbeat_interval_ms=250 owner_heartbeat_calls=%d observation_ms=%d owner_pttl=%s replacement_cas=true stale_nomination_refused=true old_cleanup_preserved_replacement=true", calls, time.Since(start).Milliseconds(), before)
}

func TestPrivateResidentRegistrationForwardReadsMustNotRenewOrphan(t *testing.T) {
	r := privateRegistrationRedis(t)
	n := privateRegistrationNominee()
	n.ResidentInternalPorts = []int{privateRefusedPort(t)}
	privateNominate(t, r, n, nil, time.Second)
	hook := &privateRegistrationHook{key: privateRegistrationKey(n.ClientId)}
	r.AddHook(hook)
	s := privateTTLSettings()
	var refused atomic.Int64
	s.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: s.ExchangeConnectTimeout}).DialContext(ctx, network, address)
		if errors.Is(err, syscall.ECONNREFUSED) {
			refused.Add(1)
		}
		return conn, err
	}
	f := NewResidentForward(context.Background(), &Exchange{settings: s}, n.ClientId)
	message := clientconnect.MessagePoolCopy([]byte("one pending payload after owner retirement"))
	witness := retainResidentPoolWitness(message)
	f.send <- message
	started := time.Now()
	runDone, idleDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(runDone); privateRunResidentForward(t, f) }()
	go func() { defer close(idleDone); f.runIdleWatcher(server.NewId()) }()
	defer func() {
		f.Cancel()
		privateJoin(t, runDone, "forward")
		privateJoin(t, idleDone, "forward idle watcher")
	}()
	privateWait(t, time.Second, func() bool { return refused.Load() >= 3 }, "real refused forward retries were not observed")
	time.Sleep(time.Until(started.Add(2200 * time.Millisecond)))
	staleAlive := r.Exists(context.Background(), hook.key).Val() == 1
	ttl := r.PTTL(context.Background(), hook.key).Val()
	lookups, renewals := hook.gets.Load(), hook.expires.Load()
	t.Logf("scaled_ttl_ms=1000 observation_ms=%d orphan_present=%t pttl=%s real_refused_dials=%d route_gets=%d route_expires=%d owner_heartbeat_calls=0 accepted_payloads=1", time.Since(started).Milliseconds(), staleAlive, ttl, refused.Load(), lookups, renewals)
	privateJoin(t, runDone, "forward idle retirement")
	privateJoin(t, idleDone, "forward idle retirement watcher")
	requireResidentPoolOwnerReturned(t, witness, "expired forward pending payload")
	privateWait(t, 2*time.Second, func() bool { return r.Exists(context.Background(), hook.key).Val() == 0 }, "orphan did not expire after forward joined")
	t.Logf("idle_forward_joined=true pending_payload_returned=true orphan_eventually_expired=true elapsed_ms=%d", time.Since(started).Milliseconds())
	if staleAlive || renewals != 0 {
		t.Fatal("routing reads renewed an orphan registration beyond its owner TTL")
	}
}

func TestPrivateResidentRegistrationTransportReadMustNotRenewOrphan(t *testing.T) {
	r := privateRegistrationRedis(t)
	n := privateRegistrationNominee()
	n.ResidentInternalPorts = []int{privateRefusedPort(t)}
	privateNominate(t, r, n, nil, time.Second)
	hook := &privateRegistrationHook{key: privateRegistrationKey(n.ClientId)}
	r.AddHook(hook)
	// Age a real owner's lease first. One routing lookup must not grant the
	// expired owner a fresh second while its failed reconnect waits to nominate.
	time.Sleep(650 * time.Millisecond)
	s := privateTTLSettings()
	// Gate after the real kernel refusal, before returning the dial result to
	// Run. The SDK reconnect timer uses full jitter and cannot provide a strict
	// no-nomination interval. This scheduling barrier requires no fake result.
	dialRefused := make(chan struct{})
	s.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: s.ExchangeConnectTimeout}).DialContext(ctx, network, address)
		if errors.Is(err, syscall.ECONNREFUSED) {
			close(dialRefused)
		}
		<-ctx.Done()
		return conn, err
	}
	transport := NewResidentTransport(context.Background(), &Exchange{ctx: context.Background(), settings: s}, n.ClientId, n.InstanceId)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Match the production connect-handler recovery boundary: Redis
		// represents a context already canceled before a call as panic("Done").
		if recovered := server.HandleError(transport.Run); recovered != nil && !server.IsDoneError(recovered) {
			t.Errorf("transport raised an unexpected error: %v", recovered)
		}
	}()
	defer func() { transport.Cancel(); privateJoin(t, done, "transport") }()
	privateJoin(t, dialRefused, "transport real refused dial barrier")
	time.Sleep(550 * time.Millisecond)
	alive := r.Exists(context.Background(), hook.key).Val() == 1
	ttl := r.PTTL(context.Background(), hook.key).Val()
	transport.Cancel()
	privateJoin(t, done, "transport")
	t.Logf("scaled_ttl_ms=1000 initial_lease_age_ms=650 post_read_ms=550 orphan_present=%t pttl=%s route_gets=%d route_expires=%d canceled_before_nomination_io=true transport_joined=true", alive, ttl, hook.gets.Load(), hook.expires.Load())
	if alive || hook.expires.Load() != 0 {
		t.Fatal("transport routing read renewed an orphan registration")
	}
}

func TestPrivateResidentRegistrationForwardReplacementDeliversPendingFIFO(t *testing.T) {
	r := privateRegistrationRedis(t)
	n := privateRegistrationNominee()
	n.ResidentInternalPorts = []int{privateRefusedPort(t)}
	privateNominate(t, r, n, nil, time.Second)
	s := privateTTLSettings()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	destination := newResidentCallbackLifecycleFixture(t, ctx, s)
	destination.clientId, destination.instanceId, destination.residentId = n.ClientId, n.InstanceId, server.NewId()
	exchange := &Exchange{ctx: ctx, cancel: cancel, settings: s, residents: map[server.Id]*Resident{n.ClientId: destination}, connections: map[server.Id]map[server.Id]context.CancelFunc{}}
	destination.exchange = exchange
	send, receive, remove, err := destination.AddTransport()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptDone := make(chan struct{})
	var sockets sync.WaitGroup
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			sockets.Add(1)
			go func() { defer sockets.Done(); exchange.handleExchangeConnection(conn) }()
		}
	}()
	var refused atomic.Int64
	s.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: s.ExchangeConnectTimeout}).DialContext(ctx, network, address)
		if errors.Is(err, syscall.ECONNREFUSED) {
			refused.Add(1)
		}
		return conn, err
	}
	f := NewResidentForward(ctx, exchange, n.ClientId)
	done := make(chan struct{})
	go func() { defer close(done); privateRunResidentForward(t, f) }()
	defer func() {
		f.Cancel()
		cancel()
		listener.Close()
		privateJoin(t, done, "handoff forward")
		privateJoin(t, acceptDone, "handoff listener")
		socketDone := make(chan struct{})
		go func() { sockets.Wait(); close(socketDone) }()
		privateJoin(t, socketDone, "handoff sockets")
		remove()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		if err := destination.CloseAndWait(cleanupCtx); err != nil {
			t.Error(err)
		}
		returnReadyPooledMessages(send)
		returnReadyPooledMessages(receive)
	}()
	var bodies, witnesses [][]byte
	for _, marker := range []string{"first-pending", "second-queued"} {
		body, err := proto.Marshal(&protocol.TransferFrame{TransferPath: clientconnect.NewTransferPath(clientconnect.NewId(), clientconnect.Id(n.ClientId), clientconnect.Id{}).ToProtobuf(), Pack: &protocol.Pack{MessageId: []byte(marker)}})
		if err != nil {
			t.Fatal(err)
		}
		message := clientconnect.MessagePoolCopy(body)
		bodies = append(bodies, body)
		witnesses = append(witnesses, retainResidentPoolWitness(message))
		f.send <- message
	}
	privateWait(t, time.Second, func() bool { return refused.Load() >= 2 }, "handoff did not start at refused old owner")
	replacement := *n
	replacement.ResidentId = destination.residentId
	replacement.ResidentInternalPorts = []int{listener.Addr().(*net.TCPAddr).Port}
	privateNominate(t, r, &replacement, &n.ResidentId, time.Second)
	model.RemoveResidentForClient(ctx, n.ClientId, n.ResidentId)
	for _, body := range bodies {
		select {
		case got := <-send:
			equal := bytes.Equal(got, body)
			clientconnect.MessagePoolReturn(got)
			if !equal {
				t.Fatal("replacement delivery changed FIFO/payload")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("replacement failed to deliver queued payload")
		}
	}
	select {
	case got := <-send:
		clientconnect.MessagePoolReturn(got)
		t.Fatal("replacement duplicated a payload")
	case <-time.After(100 * time.Millisecond):
	}
	requireResidentPoolOwnersReturned(t, witnesses, "replacement-delivered pending FIFO")
	if got := model.GetResidentForClient(ctx, n.ClientId, 0); got == nil || got.ResidentId != replacement.ResidentId {
		t.Fatal("replacement lost ownership")
	}
	t.Logf("real_refused_old_owner_dials=%d real_tcp_replacement_delivery=2 fifo=true duplicates=0 stale_cleanup_preserved_replacement=true pooled_payloads_returned=true", refused.Load())
}
