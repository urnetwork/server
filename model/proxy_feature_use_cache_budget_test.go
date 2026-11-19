// Checks the optional feature-use lookup when the Redis read consumes the flush budget.
package model

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// Acknowledgement comes before a cold Redis read. Only that read consumes the
// simulated budget. A PostgreSQL fallback cannot improve its optional label.
func TestProxyFeatureUseBudgetExpiresDuringRedisRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan bool, 1)
		installProxyAdmissionRedisHooks(t, true, entered, nil)
		ctx := server.WithoutPostgres(t.Context())
		settings := DefaultProxyFeatureUseSettings()
		planner := newDbProxyFeatureUsePlans(settings)
		store := &acknowledgedFeatureUseStore{acknowledged: make(chan struct{})}
		worker := newProxyFeatureUsesWithoutRun(ctx, settings, store, planner, time.Now)
		coldProxy, coldNetwork := server.NewId(), server.NewId()
		warmProxy, warmNetwork := server.NewId(), server.NewId()
		planner.networkIds[coldProxy] = coldNetwork
		planner.networkIds[warmProxy] = warmNetwork
		planner.networkPro[warmNetwork] = proxyFeatureUseNetworkPlan{pro: true, readTime: time.Now()}
		unknown := proxyFeatureUseMarks.WithLabelValues(FeatureSocksProxy, proxyFeatureUsePlanUnknown)
		pro := proxyFeatureUseMarks.WithLabelValues(FeatureSocksProxy, proxyFeatureUsePlanPro)
		unknownBefore, proBefore := testutil.ToFloat64(unknown), testutil.ToFloat64(pro)
		worker.Note(coldProxy, FeatureSocksProxy, time.Now())
		worker.Note(warmProxy, FeatureSocksProxy, time.Now())
		done := make(chan int, 1)
		go func() { done <- worker.flush(ctx) }()
		if deadlinePool := <-entered; deadlinePool {
			t.Fatal("optional accounting used the admission Redis pool")
		}
		select {
		case <-store.acknowledged:
		default:
			t.Fatal("plan lookup preceded mark acknowledgement")
		}
		if written := <-done; written != 2 || len(store.lastWrite()) != 2 || len(worker.take()) != 0 {
			t.Fatal("acknowledged marks were lost or requeued")
		}
		if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
			t.Errorf("spent Redis lookup entered PostgreSQL %d times", attempts)
		}
		if testutil.ToFloat64(unknown)-unknownBefore != 1 || testutil.ToFloat64(pro)-proBefore != 1 {
			t.Error("cold and previously known labels were not both preserved")
		}
		if _, found := planner.networkPro[coldNetwork]; found {
			t.Error("unread entitlement was cached as a plan")
		}
		if _, found := getProNetworkLocal(coldNetwork); found {
			t.Error("unread entitlement was cached in the process")
		}
	})
}

// A live authoritative entitlement read still requires the database when the
// cache reports an error. The guard records the attempted fallback before IO.
func TestProxyEntitlementLiveCacheErrorRetainsPostgresFallback(t *testing.T) {
	server.RedisReset()
	t.Cleanup(server.RedisReset)
	t.Cleanup(server.Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.1:6379\npassword: synthetic\ndb: 0\n")))
	t.Cleanup(server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 8\n")))
	ctx, cancel := context.WithTimeout(server.WithoutPostgres(t.Context()), time.Minute)
	defer cancel()
	hook := &proxyConfigErrorHook{}
	server.Redis(ctx, func(client server.RedisClient) { client.AddHook(hook) })
	networkId := server.NewId()
	result := server.HandleError(func() { IsProNetwork(ctx, networkId) })
	err, _ := result.(error)
	if hook.commands != 1 || !errors.Is(err, server.ErrPacketPostgres) || server.PacketPostgresAttempts(ctx) != 1 {
		t.Fatal("live cache error lost its authoritative PostgreSQL fallback")
	}
	if _, found := getProNetworkLocal(networkId); found {
		t.Fatal("failed authoritative fallback published a plan")
	}
}
