//go:build sidecar_pending

package computehost

// compute_catalog_version's SQL (INFER-M12-01; DESIGN-COMPUTE §6.3, §14.4; compute-plan-catalog-versions, QD-1237) on
// the private test stack's Postgres with MG-C2 applied, so this builds under sidecar_pending and runs through
// .stack/server-test.sh (server.DefaultTestEnv): a version published one past the newest, a second publisher of one
// number refused, the newest two read back whole, and exposures set only forward in time.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/computeservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
)

// One version of the catalog for the store, published at publishedTime.
func testComputeCatalogVersion(version uint64, publishedTime time.Time, exposures []*computeservice.ComputeDirectExposure) *computeservice.ComputeCatalogVersion {
	return &computeservice.ComputeCatalogVersion{
		Version:       version,
		PublishedTime: publishedTime,
		Answer: &protocol.InferCatalogAnswer{
			Version:     version,
			PublishedMs: publishedTime.UnixMilli(),
			Models: []*protocol.InferCatalogModel{{
				Provider: "engy", Model: "deepseek-v4.1-flash", Kind: protocol.InferCatalogModelKind_INFER_CATALOG_MODEL_KIND_DIRECT,
				Label: "none", Efforts: []string{"off"}, DefaultEffort: "off", ContextWindow: 1024, MaxOutput: 256,
				Modalities: []string{"text"}, InputNanoUsdPerMillion: 1_200_000_000,
			}},
		},
		Exposures:    exposures,
		ExposureTime: publishedTime,
	}
}

// Each version publishes one past the newest and reads back whole, newest first; a second publisher of a number, and
// one that skips a number, are refused; a version's exposures move forward in time alone.
func TestComputeCatalogStoreOnThePrivateStack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		store := computeservice.NewComputeCatalogStore(core.NewDb(), computeservice.DefaultComputeBrokerSettings())
		versions, err := store.Newest(ctx, 2)
		if err != nil || len(versions) != 0 {
			tb.Fatalf("before any version: %v, %v", versions, err)
		}
		now := server.NowUtc().Truncate(time.Millisecond)
		exposure := &computeservice.ComputeDirectExposure{Provider: computeservice.ComputeProviderIdEngy, ModelId: "deepseek-v4.1-flash-1017", WorstCaseNanoUsd: 2_584_800_000}
		if err := store.Publish(ctx, testComputeCatalogVersion(1, now, []*computeservice.ComputeDirectExposure{exposure})); err != nil {
			tb.Fatal(err)
		}
		for _, version := range []uint64{1, 3} {
			if err := store.Publish(ctx, testComputeCatalogVersion(version, now, nil)); !errors.Is(err, computeservice.ErrComputeCatalogRace) {
				tb.Fatalf("publishing %d after 1: %v", version, err)
			}
		}
		later := now.Add(time.Hour)
		if err := store.Publish(ctx, testComputeCatalogVersion(2, later, nil)); err != nil {
			tb.Fatal(err)
		}
		versions, err = store.Newest(ctx, 2)
		if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
			tb.Fatalf("the newest two: %v, %v", versions, err)
		}
		first := versions[1]
		if !first.PublishedTime.Equal(now) || first.Answer.GetVersion() != 1 || len(first.Answer.GetModels()) != 1 ||
			first.Answer.GetModels()[0].GetInputNanoUsdPerMillion() != 1_200_000_000 {
			tb.Fatalf("version 1 read back as %+v", first)
		}
		if got := first.Exposure(computeservice.ComputeProviderIdEngy); got == nil || *got != *exposure {
			tb.Fatalf("version 1's exposure: %+v", got)
		}
		risen := &computeservice.ComputeDirectExposure{Provider: computeservice.ComputeProviderIdEngy, ModelId: "deepseek-v4.1-pro", WorstCaseNanoUsd: 9_000_000_000}
		if err := store.SetExposure(ctx, 2, []*computeservice.ComputeDirectExposure{risen}, later.Add(time.Hour)); err != nil {
			tb.Fatal(err)
		}
		// an hourly read older than the one stored changes nothing
		if err := store.SetExposure(ctx, 2, nil, later); err != nil {
			tb.Fatal(err)
		}
		versions, err = store.Newest(ctx, 1)
		if err != nil || len(versions) != 1 || !versions[0].ExposureTime.Equal(later.Add(time.Hour)) {
			tb.Fatalf("after the exposures: %v, %v", versions, err)
		}
		if got := versions[0].Exposure(computeservice.ComputeProviderIdEngy); got == nil || *got != *risen {
			tb.Fatalf("version 2's exposure: %+v", got)
		}
	})
}
