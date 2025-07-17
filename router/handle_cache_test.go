package router

import (
	"context"
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestCache(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type TestCacheResult struct {
			Message string `json:"message"`
		}

		impl := func(clientSession *session.ClientSession) (*TestCacheResult, error) {
			return &TestCacheResult{
				Message: "hello!",
			}, nil
		}
		f := CacheNoAuth(impl, "cache_test", 1*time.Second)

		clientSession := session.Testing_CreateClientSession(ctx, nil)

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

	})
}
