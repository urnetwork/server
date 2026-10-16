// Replacement residents publish one complete authorized reset before deltas.
package connect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The real worker and frame boundary run with an already-joined send client;
// this isolates the authoritative wire contract and exercises refused-send
// ownership without an unrelated transport or packet-loop fixture.
type streamAuthorizationResetFixture struct {
	ctx       context.Context
	snapshots chan []model.StreamHop
	frames    chan any
	clientId  server.Id
}

// An unbuffered follow-up snapshot is a barrier after the prior publication.
func newStreamAuthorizationResetFixture(t testing.TB, clientId server.Id) *streamAuthorizationResetFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	settings := clientconnect.DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	settings.Log = clientconnect.NewNoopLogger()
	client := clientconnect.NewClient(ctx, clientconnect.ControlId, clientconnect.NewNoContractClientOob(), settings)
	if err := client.CloseAndWait(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	fixture := &streamAuthorizationResetFixture{ctx: ctx, snapshots: make(chan []model.StreamHop), frames: make(chan any, 64), clientId: clientId}
	resident := &Resident{ctx: ctx, client: client, clientId: clientId}
	resident.beforeListenerFrameSendForTest = func(frame *protocol.Frame) {
		value, err := clientconnect.FromFrame(frame)
		if err != nil {
			fixture.frames <- err
		} else {
			fixture.frames <- value
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); resident.runStreamAuthorizations(fixture.snapshots) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("stream authorization worker did not stop")
		}
	})
	return fixture
}

// Sending twice establishes completion of the first snapshot without polling.
func (self *streamAuthorizationResetFixture) publish(t testing.TB, snapshot []model.StreamHop) any {
	t.Helper()
	for range 2 {
		select {
		case self.snapshots <- snapshot:
		case <-self.ctx.Done():
			t.Fatal("worker did not consume the next authoritative snapshot")
		}
	}
	select {
	case frame := <-self.frames:
		return frame
	default:
		t.Fatal("completed first snapshot emitted no authoritative reset")
		return nil
	}
}

// Real participants and connection generations produce the requested grant.
func authorizedResetHop(t testing.TB, clientId server.Id, version uint32) model.StreamHop {
	t.Helper()
	ctx := t.Context()
	peerId := server.NewId()
	networkId, userId := server.NewId(), server.NewId()
	for _, id := range []server.Id{clientId, peerId} {
		credential := session.NewByJwt(networkId, userId, "synthetic-stream-reset", false, false).Client(server.NewId(), id)
		if err := session.PublishConnectionAuthority(ctx, credential, server.NewId(), version, server.NowUtc().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	streamId := model.AddToStream(ctx, server.NewId(), clientId, peerId, nil)
	return model.NewStreamHop(nil, &peerId, streamId)
}

// The complete first set, including legacy streams, tells an existing client
// exactly which old streams survive. An eager empty reset loses that identity.
func TestResidentStreamAuthorizationFirstSnapshotResetsLegacyStreams(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		clientId := server.NewId()
		hop := authorizedResetHop(t, clientId, 0)
		fixture := newStreamAuthorizationResetFixture(t, clientId)
		frame := fixture.publish(t, []model.StreamHop{hop})
		reset, ok := frame.(*protocol.StreamReset)
		if !ok || len(reset.Streams) != 1 {
			t.Fatalf("first authoritative snapshot is %T, want one-stream reset", frame)
		}
		open := reset.Streams[0]
		if string(open.StreamId) != string(hop.StreamId().Bytes()) || string(open.DestinationId) != string(hop.DestinationId().Bytes()) || len(open.AuthorizationGeneration) != 0 {
			t.Fatal("reset changed a retained legacy stream or fabricated its lease")
		}
	})
}

// Leased survivors retain the same authorization generation in the full reset.
func TestResidentStreamAuthorizationFirstSnapshotPreservesLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		clientId := server.NewId()
		hop := authorizedResetHop(t, clientId, 1)
		grant, err := model.AuthorizeStream(t.Context(), hop.StreamId(), server.NowUtc())
		if err != nil || !grant.Allowed || grant.Legacy {
			t.Fatal("lease fixture did not establish authority", err)
		}
		fixture := newStreamAuthorizationResetFixture(t, clientId)
		frame := fixture.publish(t, []model.StreamHop{hop})
		reset, ok := frame.(*protocol.StreamReset)
		if !ok || len(reset.Streams) != 1 {
			t.Fatalf("first leased snapshot is %T, want one-stream reset", frame)
		}
		open := reset.Streams[0]
		if string(open.AuthorizationGeneration) != string(grant.Generation.Bytes()) || open.AuthorizationLeaseMillis != grant.LeaseMillis || open.AuthorizationDeadlineUnixMillis <= 0 {
			t.Fatal("replacement reset lost the retained stream's lease")
		}
	})
}

// An empty first snapshot is still authoritative removal of all old streams.
func TestResidentStreamAuthorizationEmptyFirstSnapshotResetsStreams(t *testing.T) {
	fixture := newStreamAuthorizationResetFixture(t, server.NewId())
	frame := fixture.publish(t, nil)
	if reset, ok := frame.(*protocol.StreamReset); !ok || len(reset.Streams) != 0 {
		t.Fatalf("empty first snapshot is %T, want empty reset", frame)
	}
}

// A failed first authority read is followed by a blocked retry. Its barrier
// proves that no partial or empty reset was emitted during the dependency gap.
type streamResetReadFailureHook struct {
	key     string
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

// The failure is scoped to one stream's authorization read.
func (self *streamResetReadFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if command.Name() == "mget" && len(command.Args()) > 1 && command.Args()[1] == self.key {
			switch self.calls.Add(1) {
			case 1:
				err := errors.New("synthetic stream authority read failure")
				command.SetErr(err)
				return err
			case 2:
				close(self.entered)
				select {
				case <-self.release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return next(ctx, command)
	}
}

// Dialing and unrelated pipelines retain the production driver behavior.
func (self *streamResetReadFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// No pipeline fault is needed at this boundary.
func (self *streamResetReadFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// An uncertain first grant cannot tear down surviving client transport state.
func TestResidentStreamAuthorizationFirstResetWaitsForCompleteAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		clientId := server.NewId()
		hop := authorizedResetHop(t, clientId, 0)
		hook := &streamResetReadFailureHook{key: fmt.Sprintf("{sa_%s}b", hop.StreamId()), entered: make(chan struct{}), release: make(chan struct{})}
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(hook.release) })
		server.Raise(server.RedisAuth(t.Context(), func(_ context.Context, r server.RedisClient) error { r.AddHook(hook); return nil }))
		fixture := newStreamAuthorizationResetFixture(t, clientId)
		for range 2 {
			select {
			case fixture.snapshots <- []model.StreamHop{hop}:
			case <-fixture.ctx.Done():
				t.Fatal("worker did not retry its uncertain first snapshot")
			}
		}
		select {
		case <-hook.entered:
		case <-fixture.ctx.Done():
			t.Fatal("retry did not reach its authority barrier")
		}
		select {
		case <-fixture.frames:
			t.Fatal("uncertain authority emitted a destructive partial reset")
		default:
		}
		releaseOnce.Do(func() { close(hook.release) })
		select {
		case frame := <-fixture.frames:
			if reset, ok := frame.(*protocol.StreamReset); !ok || len(reset.Streams) != 1 {
				t.Fatalf("recovered first snapshot is %T, want retained-stream reset", frame)
			}
		case <-fixture.ctx.Done():
			t.Fatal("recovered authority never produced its first reset")
		}
	})
}
