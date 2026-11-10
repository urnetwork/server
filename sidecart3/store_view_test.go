//go:build sidecar_localstack

// The oracle's store view on T3 (TEST-M2-01): the fixture T2 decodes on its fakes, written to a T3 Net's real
// clusters and read back through the same view, a look under an armed plan firing nothing, and server's tables
// read only.

package sidecart3

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestStoreViewDecodesEachFamilyOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		ctx := context.Background()
		fixture := fault.NewStoreViewFixture()
		if err := fixture.Write(ctx, net); err != nil {
			t.Fatal(err)
		}
		if _, err := net.Faults().Add(&fault.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: "head/", Result: sidecartest.FailBeforeApply}); err != nil {
			t.Fatal(err)
		}
		view := fault.NewStoreView(net, nil)
		if err := fixture.Check(ctx, view); err != nil {
			t.Fatal(err)
		}
		if unfired := net.Faults().Unfired(); len(unfired) != 1 {
			t.Fatalf("a look on T3 fired a plan: %d left unfired", len(unfired))
		}
		rows, err := view.Postgres(ctx, "SELECT 1")
		if err != nil || len(rows) != 1 {
			t.Fatalf("server's Postgres through the view: %v, %v", rows, err)
		}
		if _, err := view.Postgres(ctx, "CREATE TABLE store_view_write_refused (id int)"); err == nil {
			t.Fatal("the view's Postgres took a write")
		}
	})
}
