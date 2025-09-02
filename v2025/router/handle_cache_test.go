package router

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/session"
)

func TestCache(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type TestCacheResult struct {
			Message string `json:"message"`
		}

		var callCount atomic.Uint64
		impl := func(clientSession *session.ClientSession) (*TestCacheResult, error) {
			callCount.Add(1)
			return &TestCacheResult{
				Message: "hello!",
			}, nil
		}
		f := CacheNoAuth(impl, "cache_test", 60*time.Second)

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		// warm
		_, err := f(clientSession)
		assert.Equal(t, err, nil)

		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}

		for range 1024 {
			r, err := f(clientSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, r.Message, "hello!")

			timeout := time.Duration(16+mathrand.Intn(64)) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(timeout):
			}
		}

		assert.Equal(t, int(callCount.Load()), 1)
	})
}

func TestCacheWithAuth(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type TestCacheResult struct {
			Message string `json:"message"`
		}

		var callCount atomic.Uint64
		impl := func(clientSession *session.ClientSession) (*TestCacheResult, error) {
			callCount.Add(1)
			return &TestCacheResult{
				Message: fmt.Sprintf("hello u%s c%s!", clientSession.ByJwt.UserId, *clientSession.ByJwt.ClientId),
			}, nil
		}
		f := CacheWithAuth(impl, "cache_test_with_auth", 6000*time.Second)

		clientSessions := []*session.ClientSession{}
		for i := range 32 {
			byJwt := jwt.NewByJwt(
				server.NewId(),
				server.NewId(),
				fmt.Sprintf("test%d", i),
				false,
			).Client(
				server.NewId(),
				server.NewId(),
			)
			clientSession := session.Testing_CreateClientSession(ctx, byJwt)
			clientSessions = append(clientSessions, clientSession)

			// warm
			_, err := f(clientSession)
			assert.Equal(t, err, nil)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}

		for range 1024 {
			mathrand.Shuffle(len(clientSessions), func(i int, j int) {
				clientSessions[i], clientSessions[j] = clientSessions[j], clientSessions[i]
			})
			for _, clientSession := range clientSessions {
				r, err := f(clientSession)
				assert.Equal(t, err, nil)
				assert.Equal(t, r.Message, fmt.Sprintf("hello u%s c%s!", clientSession.ByJwt.UserId, *clientSession.ByJwt.ClientId))
			}
			timeout := time.Duration(16+mathrand.Intn(64)) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(timeout):
			}
		}

		assert.Equal(t, int(callCount.Load()), len(clientSessions))
	})
}
