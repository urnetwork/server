package taskworker

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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

// A compact JWS shape around payload, as oauth.SignRevocation writes one: what the writer reads back of an object.
func testEvsJobWritersSignRevocation(payload []byte) ([]byte, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"test","typ":"ur-rev+jwt"}`))
	return []byte(header + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString([]byte("test"))), nil
}

// all's process installs api's parts, its signing rev/ store among them, before taskworker's runtime installs these
// writers (§19.4's writers that share a process; owner decision 788): the worker's sweep store keeps out of use beside
// api's, so a removal's write task still publishes its object, and the sweep, reading whichever store is current,
// deletes it through api's 20 minutes on, spending api's budget and none of the worker's. The writers' removal leaves
// api's store current, and api's own removal then leaves none. Before, the worker's store replaced api's: the write
// task failed as unsigned, and the writers' removal left no store at all.
func TestEvsJobWritersKeepApisSigningRevStore(t *testing.T) {
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := evsservice.NewBudgetedBlobs(sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings()))
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	model.SetSidecarRevStore(nil)
	defer model.SetSidecarRevStore(nil)
	// api's signing store, as installApiRevocations makes it, on api's own budget
	apiBudget := sidecartest.NewFakeWriteBudget(nil)
	apiSettings := model.DefaultSidecarRevSettings()
	apiSettings.Clock = clock
	apiStore := model.NewSidecarRevStore(first, testEvsJobWritersSignRevocation, apiSettings)
	apiStore.SetWriteBudget(apiBudget, nil)
	removeApi := model.InstallSidecarRevStore(apiStore)
	defer removeApi()
	workerBudget := sidecartest.NewFakeWriteBudget(nil)
	host := &sidecar.ServiceHost{Blobs: blobs, Budget: workerBudget, Clock: clock}
	closeWriters := installEvsJobWriters(host, first, nil)
	defer closeWriters()

	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	write := func() (string, error) {
		revocation := &model.SidecarRevocation{
			NetworkId: server.NewId(),
			Kind:      sidecar.RevocationKindClient,
			SubjectId: server.NewId(),
			NotBefore: sidecar.RevocationNotBefore(clock.Now()),
			Reason:    sidecar.RevocationReasonRemoved,
		}
		_, err := model.WriteSidecarRevTask(&model.WriteSidecarRevArgs{Revocations: []*model.SidecarRevocation{revocation}}, clientSession)
		return sidecar.RevocationKey(sidecar.Id(revocation.NetworkId), revocation.Kind, sidecar.Id(revocation.SubjectId)), err
	}
	published := func(key string) bool {
		_, err := first.Head(ctx, key)
		return err == nil
	}
	spent := func(budget *sidecartest.FakeWriteBudget) int64 {
		if violations := budget.Violations(); len(violations) != 0 {
			t.Errorf("the budget's violations: %v", violations)
		}
		return budget.P0ByteCount(first.ClusterId(), sidecar.WriteP0KindRevocation)
	}

	key, err := write()
	if err != nil || !published(key) {
		t.Fatalf("api's store, beside the worker's writers, did not publish: %v", err)
	}
	created := spent(apiBudget)
	if created <= 0 {
		t.Fatalf("api's create spent %d", created)
	}
	clock.Advance(apiSettings.DeleteAfter + time.Minute)
	if deletedCount, err := model.SweepSidecarRevocations(ctx); err != nil || deletedCount != 1 {
		t.Fatalf("the sweep deleted %d: %v", deletedCount, err)
	}
	if published(key) {
		t.Fatal("the sweep left the expired object")
	}
	if swept := spent(apiBudget); swept <= created {
		t.Errorf("api's budget spent %d after the sweep and %d before it: the sweep did not go through api's store", swept, created)
	}
	if got := spent(workerBudget); got != 0 {
		t.Errorf("the worker's budget spent %d of rev/ while api's store was current", got)
	}

	// the writers' removal leaves api's store, and api's own then none
	closeWriters()
	if key, err := write(); err != nil || !published(key) {
		t.Fatalf("api's store after the writers' removal did not publish: %v", err)
	}
	removeApi()
	if _, err := write(); err == nil {
		t.Fatal("a write landed with neither store installed")
	}
}
