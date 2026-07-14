package router

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
		connect.AssertEqual(t, err, nil)

		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}

		for range 1024 {
			r, err := f(clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, r.Message, "hello!")

			timeout := time.Duration(16+mathrand.Intn(64)) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(timeout):
			}
		}

		connect.AssertEqual(t, int(callCount.Load()), 1)
	})
}

func TestCacheWithNetworkAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type TestCacheResult struct {
			Message string `json:"message"`
		}

		var callCount atomic.Uint64
		impl := func(clientSession *session.ClientSession) (*TestCacheResult, error) {
			callCount.Add(1)
			return &TestCacheResult{
				Message: fmt.Sprintf("hello %s!", clientSession.ByJwt.NetworkId),
			}, nil
		}
		f := CacheWithNetworkAuth(impl, "cache_test_with_network_auth", 6000*time.Second)

		newSession := func(networkId server.Id, i int) *session.ClientSession {
			byJwt := jwt.NewByJwt(
				networkId,
				server.NewId(),
				fmt.Sprintf("test%d", i),
				false, // guest
				false, // pro
			)
			byJwt = byJwt.Client(
				server.NewId(),
				server.NewId(),
			)
			return session.Testing_CreateClientSession(ctx, byJwt)
		}

		networkId := server.NewId()
		clientSessions := []*session.ClientSession{}
		for i := range 32 {
			clientSessions = append(clientSessions, newSession(networkId, i))
		}

		// warm once for the whole network
		_, err := f(clientSessions[0])
		connect.AssertEqual(t, err, nil)

		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}

		// every client in the network shares the one cached entry
		for range 64 {
			mathrand.Shuffle(len(clientSessions), func(i int, j int) {
				clientSessions[i], clientSessions[j] = clientSessions[j], clientSessions[i]
			})
			for _, clientSession := range clientSessions {
				r, err := f(clientSession)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, r.Message, fmt.Sprintf("hello %s!", networkId))
			}
		}
		connect.AssertEqual(t, int(callCount.Load()), 1)

		// a different network computes and caches its own entry
		otherNetworkId := server.NewId()
		otherSession := newSession(otherNetworkId, 999)
		r, err := f(otherSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, r.Message, fmt.Sprintf("hello %s!", otherNetworkId))
		connect.AssertEqual(t, int(callCount.Load()), 2)
	})
}

func TestCacheWithNetworkAuthInput(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type TestCacheInput struct {
			LastN int `json:"last_n"`
		}
		type TestCacheResult struct {
			Message string `json:"message"`
		}

		var callCount atomic.Uint64
		impl := func(input *TestCacheInput, clientSession *session.ClientSession) (*TestCacheResult, error) {
			callCount.Add(1)
			return &TestCacheResult{
				Message: fmt.Sprintf("%s n=%d", clientSession.ByJwt.NetworkId, input.LastN),
			}, nil
		}
		f := CacheWithNetworkAuthInput(impl, "cache_test_with_network_auth_input", 6000*time.Second)

		networkId := server.NewId()
		newSession := func(i int) *session.ClientSession {
			byJwt := jwt.NewByJwt(
				networkId,
				server.NewId(),
				fmt.Sprintf("test%d", i),
				false, // guest
				false, // pro
			)
			byJwt = byJwt.Client(
				server.NewId(),
				server.NewId(),
			)
			return session.Testing_CreateClientSession(ctx, byJwt)
		}

		clientSessions := []*session.ClientSession{}
		for i := range 8 {
			clientSessions = append(clientSessions, newSession(i))
		}

		inputs := []*TestCacheInput{
			{LastN: 24},
			{LastN: 90},
		}

		// warm each input once for the whole network
		for _, input := range inputs {
			_, err := f(input, clientSessions[0])
			connect.AssertEqual(t, err, nil)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}

		// every client in the network shares one cached entry per input
		for range 64 {
			for _, clientSession := range clientSessions {
				for _, input := range inputs {
					r, err := f(input, clientSession)
					connect.AssertEqual(t, err, nil)
					connect.AssertEqual(t, r.Message, fmt.Sprintf("%s n=%d", networkId, input.LastN))
				}
			}
		}
		connect.AssertEqual(t, int(callCount.Load()), len(inputs))
	})
}

func TestCacheWithAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
				false, // guest
				false, // pro
			)
			byJwt = byJwt.Client(
				server.NewId(),
				server.NewId(),
			)
			clientSession := session.Testing_CreateClientSession(ctx, byJwt)
			clientSessions = append(clientSessions, clientSession)

			// warm
			_, err := f(clientSession)
			connect.AssertEqual(t, err, nil)
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
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, r.Message, fmt.Sprintf("hello u%s c%s!", clientSession.ByJwt.UserId, *clientSession.ByJwt.ClientId))
			}
			timeout := time.Duration(16+mathrand.Intn(64)) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(timeout):
			}
		}

		connect.AssertEqual(t, int(callCount.Load()), len(clientSessions))
	})
}
