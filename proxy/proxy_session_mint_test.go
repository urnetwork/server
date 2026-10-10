package proxy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A never-cancelled parent exposes directly acquired child cancellation owners.
// Failed construction must return all of them without relying on parent shutdown.
type proxySessionOwnerContext struct {
	context.Context
	done   chan struct{}
	active atomic.Int64
}

func (self *proxySessionOwnerContext) Done() <-chan struct{} { return self.done }
func (self *proxySessionOwnerContext) AfterFunc(func()) func() bool {
	self.active.Add(1)
	var stopped atomic.Bool
	return func() bool {
		if !stopped.CompareAndSwap(false, true) {
			return false
		}
		self.active.Add(-1)
		return true
	}
}

func TestProxySessionMintRefusalAcquiresNoUnreleasedOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		ctx := t.Context()
		network, user, client, device := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, network, "proxy-session-refusal", user)
		model.Testing_CreateDevice(ctx, network, device, client, "hosted", "test")
		claims := session.NewByJwt(network, user, "proxy-session-refusal", false, false).Client(device, client)
		if _, err := session.MintHostedSession(ctx, claims); err != nil {
			t.Fatal(err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Set(ctx, session.SessionMarkerKey(network, *claims.SessionId), "1", time.Hour).Err()
		}))
		owned := &proxySessionOwnerContext{Context: context.Background(), done: make(chan struct{})}
		config := &model.ProxyDeviceConfig{ProxyDeviceConnection: model.ProxyDeviceConnection{ClientId: client, InstanceId: server.NewId()}}
		result, err := NewProxyDevice(owned, config, nil, DefaultProxyDeviceSettings())
		if result != nil || !errors.Is(err, session.ErrSessionRevoked) {
			t.Fatal("revoked hosted session reached device construction", err)
		}
		if active := owned.active.Load(); active != 0 {
			t.Fatalf("failed hosted mint retained %d parent cancellation owners", active)
		}
	})
}
