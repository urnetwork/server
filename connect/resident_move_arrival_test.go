// Arrival cleanup owns one move generation, even when nomination overlaps a
// successor publication. These composed controls use the real model and
// resident lifecycle with an in-memory Redis command transport and no sockets.
package connect

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Only the commands used by real move intent and resident model operations
// are accepted. The lock serializes Redis's atomic value comparisons.
type residentMoveArrivalRedis struct {
	stateLock       sync.Mutex
	valueKVs        map[string]string
	intentRemovals  int
	nominations     int
	unexpectedCalls int
	dialAttempts    atomic.Int64
}

// Every attempted dial fails before touching a socket.
func (self *residentMoveArrivalRedis) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		self.dialAttempts.Add(1)
		return nil, errors.New("synthetic arrival transport forbids dialing")
	}
}

// Replaces transport only; the caller still constructs and decodes real driver
// commands through server.Redis and the production model functions.
func (self *residentMoveArrivalRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return self.execute
}

// This path must not bypass the synthetic transport if a lifecycle adds a
// pipelined read. Unexpected commands fail closed and are counted below.
func (self *residentMoveArrivalRedis) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		var firstErr error
		for _, command := range commands {
			if err := self.execute(ctx, command); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
}

// Implements GET, SET, GETDEL and the server's one-key compare scripts. A
// GETDEL remains unconditional so the original cleanup deterministically fails.
func (self *residentMoveArrivalRedis) execute(ctx context.Context, command redis.Cmder) (returnErr error) {
	defer func() { command.SetErr(returnErr) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	args := command.Args()
	argString := func(value any) string {
		if raw, ok := value.([]byte); ok {
			return string(raw)
		}
		return fmt.Sprint(value)
	}
	if command.Name() == "ping" {
		command.(*redis.StatusCmd).SetVal("PONG")
		return nil
	}
	if len(args) < 2 {
		self.unexpectedCalls++
		return errors.New("synthetic arrival command is missing its key")
	}
	switch command.Name() {
	case "get", "getdel":
		key := argString(args[1])
		value, ok := self.valueKVs[key]
		if command.Name() == "getdel" {
			if strings.HasPrefix(key, "move_intent_") {
				self.intentRemovals++
			}
			delete(self.valueKVs, key)
		}
		if !ok {
			return redis.Nil
		}
		command.(*redis.StringCmd).SetVal(value)
	case "set":
		self.valueKVs[argString(args[1])] = argString(args[2])
		command.(*redis.StatusCmd).SetVal("OK")
	case "eval":
		if len(args) < 5 || argString(args[2]) != "1" {
			self.unexpectedCalls++
			return errors.New("synthetic arrival script must declare one key")
		}
		script, key, expected := argString(args[1]), argString(args[3]), argString(args[4])
		current, ok := self.valueKVs[key]
		result := int64(0)
		switch {
		case len(args) == 7 && strings.Contains(script, "redis.call('SET', key, new_value)"):
			if strings.HasPrefix(key, "ncr_") {
				self.nominations++
			}
			if ok && current == expected {
				self.valueKVs[key] = argString(args[5])
				result = 1
			}
		case len(args) == 5 && strings.Contains(script, "redis.call('DEL', key)"):
			if strings.HasPrefix(key, "move_intent_") {
				self.intentRemovals++
			}
			if ok && current == expected {
				delete(self.valueKVs, key)
				result = 1
			}
		default:
			self.unexpectedCalls++
			return errors.New("synthetic arrival script is outside the tested model boundary")
		}
		command.(*redis.Cmd).SetVal(result)
	default:
		self.unexpectedCalls++
		return errors.New("synthetic arrival command is outside the tested model boundary")
	}
	return nil
}

// Owns synthetic resources and joins the real resident workers before resetting
// the shared Redis pool. Serial tests never inspect real vault credentials.
type residentMoveArrivalFixture struct {
	ctx      context.Context
	exchange *Exchange
	redis    *residentMoveArrivalRedis
	current  *model.NetworkClientResident
	intent   *model.ResidentMoveIntent
}

// The only replaced model dependency is the constructor's PostgreSQL profile
// load. A process-wide acquisition tripwire proves no hidden database fallback.
func newResidentMoveArrivalFixture(t *testing.T) *residentMoveArrivalFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	postgresAttempts, stopPostgres := server.DenyPostgresForTest(t)
	t.Cleanup(stopPostgres)
	t.Setenv("WARP_HOST", "arrival.example")
	t.Setenv("WARP_SERVICE", "connect")
	t.Cleanup(server.Vault.PushSimpleResource("redis.yml", []byte("authority: 192.0.2.1:0\npassword: synthetic\ndb: 0\ncluster: false\n")))
	t.Cleanup(server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 1\nmax_retries: -1\n")))
	t.Cleanup(server.Config.PushSimpleResource("settings.yml", []byte("all: {}\n")))
	t.Cleanup(model.Testing_SetEnforceConcurrentClients(false))
	server.RedisReset()
	transport := &residentMoveArrivalRedis{valueKVs: map[string]string{}}
	server.Redis(ctx, func(client server.RedisClient) { client.AddHook(transport) })
	if err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		client.AddHook(transport)
		return nil
	}, server.OptRedisNoPreflight()); err != nil {
		t.Fatal(err)
	}
	exchange := newResidentConstructionExchange(ctx)
	exchange.exchangeId = server.NewId()
	exchange.settings.EnableMove = true
	exchange.settings.ExchangeResidentTtl = time.Hour
	exchange.settings.StreamHopsPollInterval = time.Hour
	exchange.residentProfileForTest = func(context.Context, server.Id) (server.Id, bool, model.NetworkPeerCategory, *model.NetworkPeer, bool) {
		return server.Id{}, false, model.NetworkPeerCategory(0), nil, false
	}
	current := &model.NetworkClientResident{
		ClientId: server.NewId(), InstanceId: server.NewId(), ResidentId: server.NewId(),
		ResidentHost: "source.example", ResidentService: "connect", ResidentBlock: "synthetic",
	}
	var residentBytes bytes.Buffer
	if err := gob.NewEncoder(&residentBytes).Encode(current); err != nil {
		t.Fatal(err)
	}
	transport.valueKVs[fmt.Sprintf("ncr_%s", current.ClientId)] = residentBytes.String()
	fixture := &residentMoveArrivalFixture{
		ctx: ctx, exchange: exchange, redis: transport, current: current,
		intent: &model.ResidentMoveIntent{
			MoveId: server.NewId(), Kind: model.ResidentMoveKindAdvisory,
			FromResidentId: current.ResidentId, FromExchangeId: server.NewId(), TargetHost: exchange.host,
		},
	}
	t.Cleanup(func() {
		exchange.Close()
		if !exchange.WaitForIdle(ctx) {
			t.Error("synthetic arrival resident workers did not join")
		}
		server.RedisReset()
		if transport.dialAttempts.Load() != 0 || transport.unexpectedCalls != 0 || postgresAttempts() != 0 {
			t.Errorf("arrival escaped offline boundaries: redis_dials=%d unexpected_commands=%d postgres_acquisitions=%d", transport.dialAttempts.Load(), transport.unexpectedCalls, postgresAttempts())
		}
	})
	return fixture
}

// Publishes through the same intent model used by source exchanges.
func (self *residentMoveArrivalFixture) publish(t *testing.T, intent *model.ResidentMoveIntent) {
	t.Helper()
	if err := model.SetResidentMoveIntent(self.ctx, self.current.ClientId, intent, time.Hour); err != nil {
		t.Fatal(err)
	}
}

// Reads the authoritative synthetic store through the production decoder.
func (self *residentMoveArrivalFixture) requireIntent(t *testing.T, expected *model.ResidentMoveIntent) {
	t.Helper()
	intent, err := model.GetResidentMoveIntent(self.ctx, self.current.ClientId)
	if err != nil || !reflect.DeepEqual(intent, expected) {
		t.Fatalf("arrival cleanup changed the expected move generation: present=%t err=%v", intent != nil, err)
	}
}

// A successor is published after A has nominated a real resident but before
// A's takeover returns. Unconditional GETDEL loses B at this exact barrier.
func TestResidentMoveArrivalPreservesSuccessorIntent(t *testing.T) {
	fixture := newResidentMoveArrivalFixture(t)
	fixture.publish(t, fixture.intent)
	prepared, release := make(chan *Resident, 1), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fixture.exchange.afterResidentWorkersStartedForTest = func(resident *Resident) {
		prepared <- resident
		select {
		case <-release:
		case <-fixture.ctx.Done():
		}
	}
	finished := make(chan bool, 1)
	go func() {
		finished <- fixture.exchange.takeOverMovedResident(fixture.ctx, fixture.current.ClientId, fixture.current.InstanceId, fixture.current, false)
	}()
	var resident *Resident
	select {
	case resident = <-prepared:
	case <-fixture.ctx.Done():
		t.Fatal("takeover did not reach real nomination publication")
	}
	if resident.arrivedMoveId != fixture.intent.MoveId || fixture.exchange.localResident(fixture.current.ClientId) != resident || resident.IsDone() {
		t.Fatal("takeover did not publish the live resident for the selected move")
	}
	successor := *fixture.intent
	successor.MoveId, successor.FromResidentId, successor.FromExchangeId = server.NewId(), resident.residentId, fixture.exchange.exchangeId
	fixture.publish(t, &successor)
	releaseOnce.Do(func() { close(release) })
	select {
	case arrived := <-finished:
		if !arrived {
			t.Fatal("successful real nomination was not accepted as an arrival")
		}
	case <-fixture.ctx.Done():
		t.Fatal("takeover did not finish after nomination release")
	}
	fixture.requireIntent(t, &successor)
	if fixture.redis.nominations != 1 || fixture.redis.intentRemovals != 0 || fixture.exchange.ctx.Err() != nil || resident.IsDone() {
		t.Fatal("arrival removed a successor, repeated nomination, or canceled its live owner")
	}
}

// A normal arrival removes A and retains its successfully nominated resident.
func TestResidentMoveArrivalRemovesConsumedIntent(t *testing.T) {
	fixture := newResidentMoveArrivalFixture(t)
	fixture.publish(t, fixture.intent)
	if !fixture.exchange.takeOverMovedResident(fixture.ctx, fixture.current.ClientId, fixture.current.InstanceId, fixture.current, true) {
		t.Fatal("healthy arrival was refused")
	}
	fixture.requireIntent(t, nil)
	resident := fixture.exchange.localResident(fixture.current.ClientId)
	if resident == nil || resident.IsDone() || resident.arrivedMoveId != fixture.intent.MoveId || fixture.redis.intentRemovals != 1 || fixture.redis.nominations != 1 {
		t.Fatal("healthy arrival did not retain one live resident and consume one intent")
	}
}

// No intent grants no nomination and performs no cleanup.
func TestResidentMoveArrivalWithoutIntent(t *testing.T) {
	fixture := newResidentMoveArrivalFixture(t)
	if fixture.exchange.takeOverMovedResident(fixture.ctx, fixture.current.ClientId, fixture.current.InstanceId, fixture.current, false) {
		t.Fatal("arrival without intent replaced the remote resident")
	}
	if fixture.redis.nominations != 0 || fixture.redis.intentRemovals != 0 || fixture.exchange.localResident(fixture.current.ClientId) != nil {
		t.Fatal("absent intent mutated arrival ownership")
	}
}

// Drain admission refusal must leave the intent available to a sibling.
func TestResidentMoveArrivalRefusedNominationKeepsIntent(t *testing.T) {
	fixture := newResidentMoveArrivalFixture(t)
	fixture.publish(t, fixture.intent)
	fixture.exchange.settings.EnableDrainCoordination = true
	fixture.exchange.draining.Store(true)
	if fixture.exchange.takeOverMovedResident(fixture.ctx, fixture.current.ClientId, fixture.current.InstanceId, fixture.current, false) {
		t.Fatal("draining exchange accepted an arrival")
	}
	fixture.requireIntent(t, fixture.intent)
	if fixture.redis.intentRemovals != 0 || fixture.redis.nominations != 0 || fixture.exchange.ctx.Err() != nil {
		t.Fatal("refusal cleaned up another arrival's intent or canceled the exchange")
	}
}

// Cancellation after reservation aborts real construction and leaves both the
// intent and independent exchange alive while reclaiming the reserved resident.
func TestResidentMoveArrivalCanceledNominationKeepsIntent(t *testing.T) {
	fixture := newResidentMoveArrivalFixture(t)
	fixture.publish(t, fixture.intent)
	callerCtx, cancelCaller := context.WithCancel(fixture.ctx)
	defer cancelCaller()
	var abandoned *Resident
	fixture.exchange.beforeResidentProfileForTest = func(resident *Resident) {
		abandoned = resident
		cancelCaller()
	}
	if fixture.exchange.takeOverMovedResident(callerCtx, fixture.current.ClientId, fixture.current.InstanceId, fixture.current, false) {
		t.Fatal("canceled nomination accepted an arrival")
	}
	fixture.requireIntent(t, fixture.intent)
	if abandoned == nil || !abandoned.IsDone() || abandoned.residentController.ctx.Err() == nil || fixture.redis.intentRemovals != 0 || fixture.exchange.ctx.Err() != nil || fixture.exchange.localResident(fixture.current.ClientId) != nil {
		t.Fatal("canceled arrival retained construction, changed the intent, or canceled the exchange")
	}
	if current := model.GetResidentForClient(fixture.ctx, fixture.current.ClientId, 0); current != nil {
		t.Fatal("canceled arrival retained its Redis reservation")
	}
}
