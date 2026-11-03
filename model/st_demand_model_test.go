package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// stDemandDbSetup prepares a database test's database. The migration list creates the
// tables, so it does nothing here. Until the branch merges, a harness overlay creates
// them in the test's scratch database instead.
var stDemandDbSetup = func(context.Context) {}

// The state is a compare and swap: a writer that lost never overwrites the
// journal of the one that won, including on the first write.
func TestStDemandStateSaveIsCompareAndSwap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		stDemandDbSetup(ctx)
		key := StDeploymentKey("mainnet-demand-test")
		if state, version := GetStDemandState(ctx, key); state != nil || version != 0 {
			t.Fatal(string(state), version)
		}
		next, ok := SaveStDemandState(ctx, key, []byte(`{"epoch":1}`), 0)
		if !ok || next != 1 {
			t.Fatal(next, ok)
		}
		if _, ok := SaveStDemandState(ctx, key, []byte(`{"epoch":99}`), 0); ok {
			t.Fatal("a second first write won")
		}
		next, ok = SaveStDemandState(ctx, key, []byte(`{"epoch":2}`), 1)
		if !ok || next != 2 {
			t.Fatal(next, ok)
		}
		if _, ok := SaveStDemandState(ctx, key, []byte(`{"epoch":99}`), 1); ok {
			t.Fatal("a stale write won")
		}
		state, version := GetStDemandState(ctx, key)
		if version != 2 || string(state) != `{"epoch": 2}` {
			t.Fatal(string(state), version)
		}
		other := StDeploymentKey("another-deployment")
		if state, version := GetStDemandState(ctx, other); state != nil || version != 0 {
			t.Fatal("deployments share a state", string(state), version)
		}
		AddStDemandEvent(ctx, key, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), 7, "swapped", map[string]any{"usdc_micro": 5})
		var kind string
		var epoch int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT kind, epoch FROM st_demand_event WHERE deployment_key = $1`, string(key)).Scan(&kind, &epoch))
		})
		if kind != "swapped" || epoch != 7 {
			t.Fatal(kind, epoch)
		}
	})
}

// The last verified price sheet is one row per deployment, replaced whole.
func TestStDemandPriceSheetIsOneRowPerDeployment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		stDemandDbSetup(ctx)
		key, other := StDeploymentKey("mainnet-demand-test"), StDeploymentKey("another-deployment")
		if raw := GetStDemandPriceSheet(ctx, key); raw != nil {
			t.Fatal(string(raw))
		}
		SaveStDemandPriceSheet(ctx, key, []byte("sn: 25\n"))
		SaveStDemandPriceSheet(ctx, key, []byte("sn: 25\n# next\n"))
		if raw := GetStDemandPriceSheet(ctx, key); !bytes.Equal(raw, []byte("sn: 25\n# next\n")) {
			t.Fatal(string(raw))
		}
		if raw := GetStDemandPriceSheet(ctx, other); raw != nil {
			t.Fatal("deployments share a sheet", string(raw))
		}
	})
}

// An epoch's users are written once, whatever is counted later, and its statement is
// recorded once. The totals keep a uint64's whole range.
func TestStLedgerV2EpochUsageIsWriteOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		stDemandDbSetup(ctx)
		key := StDeploymentKey("mainnet-demand-test")
		if usage := GetStLedgerV2EpochUsage(ctx, key, 1, 6); usage != nil {
			t.Fatalf("%+v", usage)
		}
		if err := RecordStLedgerV2EpochStatement(ctx, key, 1, 6, 1, 1, "sha256:a", []byte("{}")); err == nil {
			t.Fatal("a statement with no captured users was recorded")
		}
		if got := CaptureStLedgerV2EpochUsers(ctx, key, 1, 6, 12); got != 12 {
			t.Fatal(got)
		}
		if got := CaptureStLedgerV2EpochUsers(ctx, key, 1, 6, 99); got != 12 {
			t.Fatal("recounted to", got)
		}
		if usage := GetStLedgerV2EpochUsage(ctx, key, 1, 6); usage == nil || usage.TotalUsers != 12 || usage.Statement != nil || usage.ContentHash != "" {
			t.Fatalf("%+v", usage)
		}
		if latest := GetLatestStLedgerV2EpochStatement(ctx, key, 1); latest != nil {
			t.Fatalf("%+v", latest)
		}
		const huge = ^uint64(0)
		statement := []byte(`{"epoch":6}`)
		if err := RecordStLedgerV2EpochStatement(ctx, key, 1, 6, huge, 3, "sha256:a", statement); err != nil {
			t.Fatal(err)
		}
		usage := GetStLedgerV2EpochUsage(ctx, key, 1, 6)
		if usage.TotalUsageBytes != huge || usage.ProviderRows != 3 || usage.ContentHash != "sha256:a" || !bytes.Equal(usage.Statement, statement) || usage.StatementTime == nil || usage.TotalUsers != 12 {
			t.Fatalf("%+v", usage)
		}
		if err := RecordStLedgerV2EpochStatement(ctx, key, 1, 6, huge, 3, "sha256:a", statement); err != nil {
			t.Fatal(err)
		}
		if err := RecordStLedgerV2EpochStatement(ctx, key, 1, 6, 5, 3, "sha256:b", []byte(`{"epoch":7}`)); !errors.Is(err, ErrStLedgerV2EpochUsageFixed) {
			t.Fatal(err)
		}
		CaptureStLedgerV2EpochUsers(ctx, key, 1, 8, 1)
		CaptureStLedgerV2EpochUsers(ctx, key, 1, 7, 1)
		if err := RecordStLedgerV2EpochStatement(ctx, key, 1, 7, 5, 3, "sha256:c", []byte("{}")); err != nil {
			t.Fatal(err)
		}
		if latest := GetLatestStLedgerV2EpochStatement(ctx, key, 1); latest == nil || latest.Epoch != 7 {
			t.Fatalf("the newest with a statement: %+v", latest)
		}
	})
}
