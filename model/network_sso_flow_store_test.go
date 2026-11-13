package model

// §7.6's flow store on Redis (QD-720; §2.8's api-3), on the local test stack's
// Redis: a flow waits under {sso_<state>} with its ttl and is taken once.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A flow as BeginNetworkSso makes one, with a fresh state.
func testNetworkSsoFlow(t testing.TB) *NetworkSsoFlow {
	t.Helper()
	linkingUserId := server.NewId()
	return &NetworkSsoFlow{
		State:         "state-" + strings.ReplaceAll(server.NewId().String(), "-", ""),
		IdpId:         server.NewId(),
		NetworkId:     server.NewId(),
		Nonce:         "synthetic-nonce",
		CodeVerifier:  "synthetic-verifier",
		RedirectUri:   "https://api.example/auth/sso/callback",
		LinkingUserId: &linkingUserId,
		CreateTime:    server.NowUtc(),
	}
}

func testAssertNetworkSsoFlowEqual(t testing.TB, flow *NetworkSsoFlow, expected *NetworkSsoFlow) {
	t.Helper()
	if flow == nil {
		t.Fatal("no flow was answered")
	}
	if flow.State != expected.State || flow.IdpId != expected.IdpId || flow.NetworkId != expected.NetworkId ||
		flow.Nonce != expected.Nonce || flow.CodeVerifier != expected.CodeVerifier || flow.RedirectUri != expected.RedirectUri ||
		flow.LinkingUserId == nil || *flow.LinkingUserId != *expected.LinkingUserId || !flow.CreateTime.Equal(expected.CreateTime) {
		t.Fatalf("the flow answered is not the flow kept: %+v", flow)
	}
}

// The value and remaining life of the flow's key, nil for no key.
func testNetworkSsoFlowKeyState(t testing.TB, ctx context.Context, state string) (value []byte, ttl time.Duration) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		value, err = r.Get(ctx, networkSsoFlowKey(state)).Bytes()
		if errors.Is(err, server.RedisNil) {
			value = nil
			return
		}
		server.Raise(err)
		ttl, err = r.PTTL(ctx, networkSsoFlowKey(state)).Result()
		server.Raise(err)
	})
	return
}

// Production's store is the Redis one, there from the start.
func TestNetworkSsoFlowStoreIsRedisFromTheStart(t *testing.T) {
	if _, ok := CurrentNetworkSsoFlowStore().(*RedisNetworkSsoFlowStore); !ok {
		t.Fatalf("the process's flow store is %T", CurrentNetworkSsoFlowStore())
	}
}

// A flow waits as JSON under {sso_<state>} with its ttl, is answered whole to
// the first take, and is gone for every take after it.
func TestNetworkSsoFlowStoreTakesAFlowOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkIdpSettings()
		store := NewRedisNetworkSsoFlowStore(settings)
		flow := testNetworkSsoFlow(t)
		if networkSsoFlowKey(flow.State) != "{sso_"+flow.State+"}" {
			t.Fatalf("the flow's key is %s", networkSsoFlowKey(flow.State))
		}
		if settings.FlowTtl != 10*time.Minute {
			t.Fatalf("the default ttl is %s", settings.FlowTtl)
		}
		if err := store.Put(ctx, flow, settings.FlowTtl); err != nil {
			t.Fatal(err)
		}

		value, ttl := testNetworkSsoFlowKeyState(t, ctx, flow.State)
		if value == nil || ttl <= 0 || settings.FlowTtl < ttl {
			t.Fatalf("the key holds %d bytes with %s to live", len(value), ttl)
		}
		fields := map[string]any{}
		if err := json.Unmarshal(value, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"state", "idp_id", "network_id", "nonce", "code_verifier", "redirect_uri", "linking_user_id", "create_time", "expire_time"} {
			if _, ok := fields[name]; !ok {
				t.Errorf("the stored flow has no %s", name)
			}
		}
		if len(fields) != 9 {
			t.Errorf("the stored flow has %d fields", len(fields))
		}

		taken, err := store.Take(ctx, flow.State)
		if err != nil {
			t.Fatal(err)
		}
		testAssertNetworkSsoFlowEqual(t, taken, flow)
		if value, _ := testNetworkSsoFlowKeyState(t, ctx, flow.State); value != nil {
			t.Fatal("the take left the flow's key")
		}
		for range 2 {
			replayed, err := store.Take(ctx, flow.State)
			if replayed != nil || err != nil {
				t.Fatalf("a replayed state was answered %+v %v", replayed, err)
			}
		}
		// another instance's store reads the same keys
		if replayed, err := NewRedisNetworkSsoFlowStore(settings).Take(ctx, flow.State); replayed != nil || err != nil {
			t.Fatalf("a replayed state was answered at another instance: %+v %v", replayed, err)
		}
	})
}

// A flow begun at one api instance is finished at another, once.
func TestNetworkSsoFlowStoreIsSharedAcrossInstances(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkIdpSettings()
		began, finished := NewRedisNetworkSsoFlowStore(settings), NewRedisNetworkSsoFlowStore(settings)
		flow := testNetworkSsoFlow(t)
		if err := began.Put(ctx, flow, settings.FlowTtl); err != nil {
			t.Fatal(err)
		}
		taken, err := finished.Take(ctx, flow.State)
		if err != nil {
			t.Fatal(err)
		}
		testAssertNetworkSsoFlowEqual(t, taken, flow)
		if replayed, err := began.Take(ctx, flow.State); replayed != nil || err != nil {
			t.Fatalf("the instance that began it took it again: %+v %v", replayed, err)
		}
	})
}

// On the store's clock a flow is answered until its ttl and never from it on,
// and the refused take removes it.
func TestNetworkSsoFlowStoreRefusesAnExpiredFlow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkIdpSettings()
		store := NewRedisNetworkSsoFlowStore(settings)
		// Redis's own clock is the real one, so the fake starts there
		start := server.NowUtc()
		now := start
		store.Testing_SetNow(func() time.Time { return now })

		live, expired := testNetworkSsoFlow(t), testNetworkSsoFlow(t)
		for _, flow := range []*NetworkSsoFlow{live, expired} {
			if err := store.Put(ctx, flow, settings.FlowTtl); err != nil {
				t.Fatal(err)
			}
		}

		now = start.Add(settings.FlowTtl - time.Microsecond)
		taken, err := store.Take(ctx, live.State)
		if err != nil {
			t.Fatal(err)
		}
		testAssertNetworkSsoFlowEqual(t, taken, live)

		now = start.Add(settings.FlowTtl)
		if taken, err := store.Take(ctx, expired.State); taken != nil || err != nil {
			t.Fatalf("a flow at its ttl was answered %+v %v", taken, err)
		}
		if value, _ := testNetworkSsoFlowKeyState(t, ctx, expired.State); value != nil {
			t.Fatal("the refused take left the expired flow's key")
		}
		now = start
		if taken, err := store.Take(ctx, expired.State); taken != nil || err != nil {
			t.Fatalf("an expired flow came back %+v %v", taken, err)
		}
	})
}

// The key's own expiry ends a flow whatever the store's clock reads: a key
// whose Redis expiry has passed is gone.
func TestNetworkSsoFlowStoreKeyExpiresInRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkIdpSettings()
		store := NewRedisNetworkSsoFlowStore(settings)
		flow := testNetworkSsoFlow(t)
		if err := store.Put(ctx, flow, settings.FlowTtl); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			// an expiry in the past is one Redis applies at once
			server.Raise(r.ExpireAt(ctx, networkSsoFlowKey(flow.State), time.Unix(1, 0)).Err())
		})
		if taken, err := store.Take(ctx, flow.State); taken != nil || err != nil {
			t.Fatalf("a flow whose key expired was answered %+v %v", taken, err)
		}
	})
}

// Of many takes of one state released together, across instances, exactly one
// is answered the flow.
func TestNetworkSsoFlowStoreConcurrentTakesAnswerOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkIdpSettings()
		flow := testNetworkSsoFlow(t)
		if err := NewRedisNetworkSsoFlowStore(settings).Put(ctx, flow, settings.FlowTtl); err != nil {
			t.Fatal(err)
		}
		takeCount := 24
		flows := make([]*NetworkSsoFlow, takeCount)
		errs := make([]error, takeCount)
		release := make(chan struct{})
		var ready sync.WaitGroup
		var done sync.WaitGroup
		for i := range takeCount {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				store := NewRedisNetworkSsoFlowStore(settings)
				ready.Done()
				<-release
				flows[i], errs[i] = store.Take(ctx, flow.State)
			}()
		}
		ready.Wait()
		close(release)
		done.Wait()
		answeredCount := 0
		for i := range takeCount {
			if errs[i] != nil {
				t.Fatalf("take %d: %v", i, errs[i])
			}
			if flows[i] != nil {
				answeredCount += 1
				testAssertNetworkSsoFlowEqual(t, flows[i], flow)
			}
		}
		if answeredCount != 1 {
			t.Fatalf("%d of %d concurrent takes were answered the flow", answeredCount, takeCount)
		}
	})
}

// What the store refuses to keep or to look up: a flow that would never
// expire, a state that is no key, and a state that already holds a flow.
func TestNetworkSsoFlowStoreRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkIdpSettings()
		store := NewRedisNetworkSsoFlowStore(settings)

		for _, ttl := range []time.Duration{0, -time.Second} {
			flow := testNetworkSsoFlow(t)
			if err := store.Put(ctx, flow, ttl); err == nil {
				t.Errorf("a flow was kept with ttl %s", ttl)
			}
			if value, _ := testNetworkSsoFlowKeyState(t, ctx, flow.State); value != nil {
				t.Errorf("a refused flow with ttl %s left a key", ttl)
			}
		}
		if err := store.Put(ctx, nil, settings.FlowTtl); err == nil {
			t.Error("no flow was kept")
		}

		for _, state := range []string{
			"",
			"with space",
			"brace}other",
			"{tag}",
			"new\nline",
			"star*",
			"colon:",
			strings.Repeat("a", settings.FlowStateMaxLength+1),
		} {
			flow := testNetworkSsoFlow(t)
			flow.State = state
			if err := store.Put(ctx, flow, settings.FlowTtl); err == nil {
				t.Errorf("a flow was kept under the state %q", state)
			}
			if taken, err := store.Take(ctx, state); taken != nil || err != nil {
				t.Errorf("the state %q was answered %+v %v", state, taken, err)
			}
		}
		longest := testNetworkSsoFlow(t)
		longest.State = strings.Repeat("a", settings.FlowStateMaxLength-len(longest.State)) + longest.State
		if err := store.Put(ctx, longest, settings.FlowTtl); err != nil {
			t.Errorf("a state of the longest length: %v", err)
		}

		first, second := testNetworkSsoFlow(t), testNetworkSsoFlow(t)
		second.State = first.State
		if err := store.Put(ctx, first, settings.FlowTtl); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, second, settings.FlowTtl); err == nil {
			t.Error("a second flow replaced the first under its state")
		}
		taken, err := store.Take(ctx, first.State)
		if err != nil {
			t.Fatal(err)
		}
		testAssertNetworkSsoFlowEqual(t, taken, first)
	})
}

// A value under a flow's key that is not that state's flow is answered as an
// error, never as a flow, and is gone.
func TestNetworkSsoFlowStoreRefusesAForeignValue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultNetworkIdpSettings()
		store := NewRedisNetworkSsoFlowStore(settings)

		other := testNetworkSsoFlow(t)
		otherJson, err := json.Marshal(&redisNetworkSsoFlow{NetworkSsoFlow: *other, ExpireTime: server.NowUtc().Add(settings.FlowTtl)})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range [][]byte{[]byte("not json"), otherJson} {
			state := testNetworkSsoFlow(t).State
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, networkSsoFlowKey(state), value, settings.FlowTtl).Err())
			})
			if taken, err := store.Take(ctx, state); taken != nil || err == nil {
				t.Errorf("a foreign value was answered %+v %v", taken, err)
			}
			if value, _ := testNetworkSsoFlowKeyState(t, ctx, state); value != nil {
				t.Error("a foreign value stayed under the key")
			}
		}
	})
}
