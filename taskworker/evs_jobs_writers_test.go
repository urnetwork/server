package taskworker

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The writers taskworker installs beside evs's jobs spend the process's write budget (installEvsJobWriters): on a
// first cluster evsservice.BudgetedBlobs wraps, which refuses a write no budget passed, the hold/ writer's create,
// renewal and DELETE each land and spend §11.13's create, overwrite and delete rows as P0 of kind lifecycle, and the
// rev/ expiry sweep's DELETE the delete row as P0 of kind revocation, under the table a budget built with the default
// settings prices with. Before this change the hold writer and the sweep had no budget there, so none of it spent.
func TestEvsJobWritersSpendTheProcessBudget(t *testing.T) {
	ctx := t.Context()
	// the worker's sweep reads real time, as in production, so the store's clock stands an hour before it: what the
	// sweep finds is an hour old whenever the test runs
	clock := sidecartest.NewFakeClock(time.Now().Add(-time.Hour))
	blobs := evsservice.NewBudgetedBlobs(sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings()))
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	budget := sidecartest.NewFakeWriteBudget(nil)
	host := &sidecar.ServiceHost{Blobs: blobs, Budget: budget, Clock: clock}
	closeWriters := installEvsJobWriters(host, first, nil)
	defer closeWriters()
	prices := evsservice.DefaultWriteBudgetSettings().PriceTable
	price := func(operation evsservice.WriteOperationKind, key string) int64 {
		objectByteCount := int64(0)
		if operation != evsservice.WriteOperationDelete {
			info, err := first.Head(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			objectByteCount = info.ByteCount
		}
		priced, err := prices.Price(evsservice.WriteOperation{Kind: operation, ObjectByteCount: objectByteCount, Count: 1})
		if err != nil {
			t.Fatal(err)
		}
		return priced
	}
	spent := func(kind sidecar.WriteP0Kind) int64 {
		if violations := budget.Violations(); len(violations) != 0 {
			t.Errorf("the budget's violations: %v", violations)
		}
		return budget.P0ByteCount(first.ClusterId(), kind)
	}

	store, err := model.CurrentAbuseHoldStore()
	if err != nil {
		t.Fatal(err)
	}
	placeTime := clock.Now().UTC().Truncate(time.Millisecond)
	record := &sidecar.HoldRecord{
		NetworkId:  sidecar.NewId(),
		CaseId:     sidecar.NewId(),
		Kind:       sidecar.HoldKindPreserve2703f,
		Scope:      sidecar.HoldScopeNetwork(),
		PlaceTime:  placeTime,
		ExpireTime: placeTime.AddDate(0, 0, model.LegalHoldPreserveDays),
	}
	key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)
	if _, err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	want := price(evsservice.WriteOperationCreate, key)
	if got := spent(sidecar.WriteP0KindLifecycle); got != want {
		t.Errorf("the hold's create spent %d, and §11.13's create row prices it at %d", got, want)
	}
	renewed := *record
	renewed.ExpireTime = model.LegalHoldRenewedDeadline(record.PlaceTime)
	if err := store.Rewrite(ctx, &renewed); err != nil {
		t.Fatal(err)
	}
	want += price(evsservice.WriteOperationOverwrite, key)
	if got := spent(sidecar.WriteP0KindLifecycle); got != want {
		t.Errorf("the hold's create and renewal spent %d, and §11.13's rows price them at %d", got, want)
	}
	if err := store.Delete(ctx, server.Id(record.NetworkId), server.Id(record.CaseId)); err != nil {
		t.Fatal(err)
	}
	want += price(evsservice.WriteOperationDelete, key)
	if got := spent(sidecar.WriteP0KindLifecycle); got != want {
		t.Errorf("the hold's create, renewal and DELETE spent %d, and §11.13's rows price them at %d", got, want)
	}

	// an expired rev/ object, as the issuer left it; the sweep reads only its key and age
	revKey := sidecar.RevocationKey(sidecar.NewId(), sidecar.RevocationKindClient, sidecar.NewId())
	fixtureCtx := sidecartest.NewFakeWriteBudget(nil).IssueP0(ctx, first.ClusterId(), sidecar.WriteP0KindRevocation, 0)
	if _, err := first.PutIfNoneMatch(fixtureCtx, revKey, []byte("a revocation"), nil); err != nil {
		t.Fatal(err)
	}
	if deletedCount, err := model.SweepSidecarRevocations(ctx); err != nil || deletedCount != 1 {
		t.Fatalf("the sweep deleted %d: %v", deletedCount, err)
	}
	if got, want := spent(sidecar.WriteP0KindRevocation), price(evsservice.WriteOperationDelete, revKey); got != want {
		t.Errorf("the sweep's DELETE spent %d, and §11.13's delete row prices it at %d", got, want)
	}
}
