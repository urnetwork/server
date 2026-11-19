// Separates Redis errors while the request still has time from an expired
// caller. The PostgreSQL guard counts a fallback before any connection opens.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

type proxyConfigErrorHook struct {
	proxyAdmissionRedisHook
	commands int
}

func (self *proxyConfigErrorHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if command.Name() != "get" {
			return next(ctx, command)
		}
		self.commands++
		err := errors.New("synthetic cache read failed")
		command.SetErr(err)
		return err
	}
}

func TestProxyAdmissionConfigCacheErrorAndEntryDeadline(t *testing.T) {
	server.RedisReset()
	t.Cleanup(server.RedisReset)
	t.Cleanup(server.Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.1:6379\npassword: synthetic\ndb: 0\n")))
	t.Cleanup(server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 8\n")))
	hook := &proxyConfigErrorHook{}
	ctx, cancel := context.WithTimeout(server.WithoutPostgres(t.Context()), time.Minute)
	defer cancel()
	if err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		client.AddHook(hook)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// With the feature switch off, even a cold proxy skips the entitlement lookup.
	config := Pro()
	enforce := config.EnforceFeatures
	config.EnforceFeatures = false
	t.Cleanup(func() { config.EnforceFeatures = enforce })
	if !ProxyFeatureAllowedForAdmission(ctx, server.NewId(), FeatureSocksProxy) || hook.commands != 0 || server.PacketPostgresAttempts(ctx) != 0 {
		t.Fatal("disabled feature enforcement touched Redis or PostgreSQL")
	}

	raised := server.HandleError(func() { GetProxyDeviceConfigForAdmission(ctx, server.NewId()) })
	err, _ := raised.(error)
	if hook.commands != 1 || server.PacketPostgresAttempts(ctx) != 1 || !errors.Is(err, server.ErrPacketPostgres) {
		t.Fatal("an in-budget cache error must fall through once to the guarded PostgreSQL read")
	}

	expired, stop := context.WithDeadline(server.WithoutPostgres(t.Context()), time.Now().Add(-time.Second))
	defer stop()
	raised = server.HandleError(func() { GetProxyDeviceConfigForAdmission(expired, server.NewId()) })
	err, _ = raised.(error)
	if hook.commands != 1 || server.PacketPostgresAttempts(expired) != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("an expired admission must return its deadline before Redis or PostgreSQL work")
	}
}
