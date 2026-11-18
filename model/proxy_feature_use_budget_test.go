// The optional labels of marks whose write was acknowledged share the
// writer's remaining flush budget.
package model

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// Releases the planner only after the test store records a successful write.
type acknowledgedFeatureUseStore struct {
	testProxyFeatureUseStore
	acknowledged chan struct{}
}

// Signals exactly one flush acknowledgement.
func (self *acknowledgedFeatureUseStore) markAll(ctx context.Context, marks []*ProxyFeatureUseMark) error {
	err := self.testProxyFeatureUseStore.markAll(ctx, marks)
	if err == nil {
		close(self.acknowledged)
	}
	return err
}

// Runs the production plan reader after the acknowledged flush loses its remaining budget.
type expiredFeatureUsePlanReader struct {
	actual       *dbProxyFeatureUsePlans
	acknowledged <-chan struct{}
	atExpiry     func()
	returned     bool
}

// Records whether the planner returned normally to the recovery boundary.
func (self *expiredFeatureUsePlanReader) plans(ctx context.Context, proxyIds []server.Id) map[server.Id]string {
	<-self.acknowledged
	// synctest advances the real worker timer only after the write has
	// acknowledged and this reader blocks on the flush context.
	<-ctx.Done()
	if self.atExpiry != nil {
		self.atExpiry()
	}
	plans := self.actual.plans(ctx, proxyIds)
	self.returned = true
	return plans
}

// A spent budget cannot turn an acknowledged mark into a failed flush.
func TestProxyFeatureUseColdPlanAfterFlushBudget(t *testing.T) {
	checkAcknowledgedFeatureUsePlans(t, false)
}

// Available classifications survive when a sibling needs a mapping or entitlement.
func TestProxyFeatureUseMixedPlansAfterFlushBudget(t *testing.T) {
	checkAcknowledgedFeatureUsePlans(t, true)
}

// Exercises the production flush, plan caches, optional lookup and counters.
func checkAcknowledgedFeatureUsePlans(t *testing.T, mixed bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx := server.WithoutPostgres(t.Context())
		settings := DefaultProxyFeatureUseSettings()
		actual := newDbProxyFeatureUsePlans(settings)
		store := &acknowledgedFeatureUseStore{acknowledged: make(chan struct{})}
		planner := &expiredFeatureUsePlanReader{actual: actual, acknowledged: store.acknowledged}
		worker := newProxyFeatureUsesWithoutRun(ctx, settings, store, planner, time.Now)
		now := time.Now()
		coldProxyId := server.NewId()
		coldNetworkId := server.NewId()
		actual.networkIds[coldProxyId] = coldNetworkId
		worker.Note(coldProxyId, FeatureSocksProxy, now)
		unknown, pro, free := 1, 0, 0
		if mixed {
			warmProxyId, warmNetworkId := server.NewId(), server.NewId()
			actual.networkIds[warmProxyId] = warmNetworkId
			actual.networkPro[warmNetworkId] = proxyFeatureUseNetworkPlan{pro: true, readTime: now}
			localProxyId, localNetworkId := server.NewId(), server.NewId()
			actual.networkIds[localProxyId] = localNetworkId
			// An independent entitlement refresh can finish during a flush.
			planner.atExpiry = func() {
				setProNetworkLocal(localNetworkId, proEntitlement{pro: false, version: 1})
			}
			worker.Note(warmProxyId, FeatureSocksProxy, now)
			worker.Note(localProxyId, FeatureSocksProxy, now)
			worker.Note(server.NewId(), FeatureSocksProxy, now)
			unknown, pro, free = 2, 1, 1
		}
		expected := map[string]int{proxyFeatureUsePlanUnknown: unknown, proxyFeatureUsePlanPro: pro, proxyFeatureUsePlanFree: free}
		before := map[string]float64{}
		for plan := range expected {
			before[plan] = testutil.ToFloat64(proxyFeatureUseMarks.WithLabelValues(FeatureSocksProxy, plan))
		}
		written := worker.flush(ctx)
		if !planner.returned {
			t.Fatal("post-write planner raised after its flush budget expired")
		}
		if written != unknown+pro+free || len(store.lastWrite()) != written || len(worker.take()) != 0 {
			t.Fatal("acknowledged marks were lost or requeued")
		}
		if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
			t.Fatalf("expired plan lookup attempted PostgreSQL %d times", attempts)
		}
		if _, found := actual.networkPro[coldNetworkId]; found {
			t.Fatal("unread entitlement entered the plan cache")
		}
		for plan, want := range expected {
			after := testutil.ToFloat64(proxyFeatureUseMarks.WithLabelValues(FeatureSocksProxy, plan))
			if got := after - before[plan]; got != float64(want) {
				t.Errorf("plan %s counted %v marks, want %d", plan, got, want)
			}
		}
	})
}
