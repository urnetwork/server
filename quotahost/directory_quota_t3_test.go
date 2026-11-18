//go:build sidecar_pending

package quotahost_test

// D-QUOTA-11's accounting on T3 (§20.6, §20.7, owner decision 10; QUOTA-M6-06): the team directory is a repo
// like any other to quota. evs names no repo kind in a usage batch, so what stands here is what the batch of a
// directory's bytes does on real rows: its stored bytes count in the owning network's stored count, a read
// debits the reader's network and never the directory's owner, the directory's bytes take the network over its
// storage limit and into Quota.OverLimit as any repo's do, a read is debited while the owner is over, and a
// delete frees the count. FI-13 (a) and (c) are read after every step.
//
// And its request limits (§16.11): the sidecar_abuse: directory_* keys reach GET /sidecar/limits and a host's
// Quota.Limits, and a §21.7 tightening row reaches both, for the one network, until its lift. The requesting
// daemon and the directory's readers apply them (sidecar's directory_limits.go), since no server sees a
// request's content. It is this package's external test because the handlers import quotahost.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func newDirectoryQuotaT3Target(t *testing.T, ctx context.Context) *quotasuite.Target {
	return quotahost.NewT3TargetOn(ctx, func(t *testing.T, clock sidecar.Clock) quotahost.QuotaSource {
		return quotahost.NewLocalQuotaSource(clock)
	})(t)
}

// The network's stored count, 0 where it holds no row.
func directoryQuotaStored(t testing.TB, ctx context.Context, networkId connect.Id) int64 {
	t.Helper()
	state, err := model.GetNetworkQuotaState(ctx, server.Id(networkId))
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		return 0
	}
	return state.StoredByteCount
}

func TestDirectoryCountsLikeAnyRepoT3(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := t.Context()
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// both networks read as free here: 1000 bytes of storage
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"storage": 1000})()
		target := newDirectoryQuotaT3Target(t, ctx)
		oracle := quotahost.NewT3Oracle(ctx)

		// the network that holds the directory's repo, and a reader's
		ownerNetworkId, readerNetworkId := connect.NewId(), connect.NewId()
		ownerBalanceId, readerBalanceId := connect.NewId(), connect.NewId()
		for networkId, balanceId := range map[connect.Id]connect.Id{ownerNetworkId: ownerBalanceId, readerNetworkId: readerBalanceId} {
			if err := target.SetBalances(networkId, []*sidecartest.QuotaBalance{{BalanceId: balanceId, ByteCount: 1000}}); err != nil {
				t.Fatal(err)
			}
			oracle.Grant(networkId, 1000)
		}
		balance := func(networkId connect.Id, balanceId connect.Id) int64 {
			t.Helper()
			balances, err := target.ReadBalances(networkId)
			if err != nil {
				t.Fatal(err)
			}
			for _, balance := range balances {
				if balance.BalanceId == balanceId {
					return balance.ByteCount
				}
			}
			t.Fatalf("network %s holds no balance %s", networkId, balanceId)
			return 0
		}
		report := func(what string, events ...*sidecar.UsageEvent) *sidecar.UsageBatch {
			t.Helper()
			batch := &sidecar.UsageBatch{BatchId: connect.NewId(), Producer: sidecar.QuotaProducerEvs, Events: events}
			for _, event := range events {
				if 0 < event.TransferByteCount {
					oracle.Meter(event.NetworkId, event.Meter, event.TransferByteCount)
				}
			}
			if err := target.Quota.ReportUsage(ctx, batch); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if err := oracle.Check(ctx); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if err := target.Settle(); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if err := oracle.CheckSettled(ctx); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			return batch
		}
		checkStored := func(what string, networkId connect.Id, segmentByteCount int64) {
			t.Helper()
			if err := oracle.CheckStored(ctx, networkId, &sidecartest.QuotaStoredListing{SegmentByteCount: segmentByteCount}); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		overStorage := func() map[connect.Id]*sidecar.QuotaRefusal {
			t.Helper()
			overLimit, err := target.Quota.OverLimit(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return overLimit.StorageNetworkIdRefusals
		}

		// The directory's entries are written, 600 bytes, by a member who moved 100 through evs, and the reader's
		// sidecar downloads 250 of them.
		written := report(
			"the directory's entries and a read",
			&sidecar.UsageEvent{NetworkId: ownerNetworkId, Meter: sidecar.QuotaMeterEvs, TransferByteCount: 100, StoredByteDelta: 600},
			&sidecar.UsageEvent{NetworkId: readerNetworkId, Meter: sidecar.QuotaMeterEvs, TransferByteCount: 250},
		)
		connect.AssertEqual(t, directoryQuotaStored(t, ctx, ownerNetworkId), int64(600))
		connect.AssertEqual(t, directoryQuotaStored(t, ctx, readerNetworkId), int64(0))
		checkStored("the directory's bytes", ownerNetworkId, 600)
		// the read debits the reader's network, the write its writer's, and neither the other
		connect.AssertEqual(t, balance(readerNetworkId, readerBalanceId), int64(750))
		connect.AssertEqual(t, balance(ownerNetworkId, ownerBalanceId), int64(900))
		connect.AssertEqual(t, len(overStorage()), 0)

		// the batch again, as a producer repeats an unanswered one: counted and debited once
		if err := target.Quota.ReportUsage(ctx, written); err != nil {
			t.Fatal(err)
		}
		if err := target.Settle(); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, directoryQuotaStored(t, ctx, ownerNetworkId), int64(600))
		connect.AssertEqual(t, balance(readerNetworkId, readerBalanceId), int64(750))
		if err := oracle.CheckSettled(ctx); err != nil {
			t.Fatal(err)
		}

		// No exemption: 500 more directory bytes take the owner past its 1000, and it is over its storage limit as
		// for any repo's bytes; the reader's network is not.
		report("the directory grows past the limit", &sidecar.UsageEvent{NetworkId: ownerNetworkId, Meter: sidecar.QuotaMeterEvs, StoredByteDelta: 500})
		connect.AssertEqual(t, directoryQuotaStored(t, ctx, ownerNetworkId), int64(1100))
		checkStored("the directory past the limit", ownerNetworkId, 1100)
		refusals := overStorage()
		if refusals[ownerNetworkId] == nil || refusals[ownerNetworkId].LimitKey != "storage" {
			t.Fatalf("the directory's network is not over its storage limit: %+v", refusals[ownerNetworkId])
		}
		if refusals[readerNetworkId] != nil {
			t.Fatalf("the reader's network is over a limit it holds no bytes against: %+v", refusals[readerNetworkId])
		}

		// a read while the owner is over still debits the reader, and only the reader
		report("a read while the owner is over", &sidecar.UsageEvent{NetworkId: readerNetworkId, Meter: sidecar.QuotaMeterEvs, TransferByteCount: 50})
		connect.AssertEqual(t, balance(readerNetworkId, readerBalanceId), int64(700))
		connect.AssertEqual(t, balance(ownerNetworkId, ownerBalanceId), int64(900))

		// entries deleted free their bytes, and the network is under its limit again
		report("entries deleted", &sidecar.UsageEvent{NetworkId: ownerNetworkId, Meter: sidecar.QuotaMeterEvs, StoredByteDelta: -500})
		connect.AssertEqual(t, directoryQuotaStored(t, ctx, ownerNetworkId), int64(600))
		checkStored("the directory after the delete", ownerNetworkId, 600)
		connect.AssertEqual(t, len(overStorage()), 0)
	})
}

// One GET /sidecar/limits on authorization: its decoded body.
func directoryQuotaLimitsCall(t testing.TB, ctx context.Context, authorization string) *sidecar.QuotaLimits {
	t.Helper()
	apiRouter := router.NewRouter(ctx, []*router.Route{
		router.NewRoute("GET", "/sidecar/limits", handlers.GetSidecarLimits),
	})
	request := httptest.NewRequest("GET", "/sidecar/limits", strings.NewReader(""))
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("Authorization", authorization)
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /sidecar/limits answered %d", w.Code)
	}
	limits := &sidecar.QuotaLimits{}
	if err := json.Unmarshal(w.Body.Bytes(), limits); err != nil {
		t.Fatalf("the limits answered no json: %q", w.Body.String())
	}
	return limits
}

func TestDirectoryLimitTighteningReachesTheLimitsReadsT3(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := t.Context()
		directoryLimits := map[string]int64{
			sidecar.DirectoryLimitRequestsPending: 20,
			sidecar.DirectoryLimitRequestsPerDay:  50,
			sidecar.DirectoryLimitRequestsPerRun:  5,
			sidecar.DirectoryLimitTargetPending:   100,
		}
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, directoryLimits)()
		defer model.Testing_SetEnforceSidecarLimits(true)()

		// two networks, each with a daemon's bearer
		networkIds := []server.Id{server.NewId(), server.NewId()}
		bearers := []string{}
		for _, networkId := range networkIds {
			userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "directory-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), userId)
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "daemon", "directory")
			byJwt := session.NewByJwt(networkId, userId, "directory", false, false)
			bearers = append(bearers, "Bearer "+byJwt.Client(deviceId, clientId).Testing_Sign())
		}
		// what a host reads, on a Quota that has read nothing yet
		hostLimits := func(networkId server.Id) *sidecar.QuotaLimits {
			t.Helper()
			limits, err := newDirectoryQuotaT3Target(t, ctx).Quota.Limits(ctx, connect.Id(networkId))
			if err != nil {
				t.Fatal(err)
			}
			return limits
		}
		checkLimits := func(what string, index int, want map[string]int64) {
			t.Helper()
			daemon := directoryQuotaLimitsCall(t, ctx, bearers[index])
			host := hostLimits(networkIds[index])
			connect.AssertEqual(t, daemon.Enforced, true)
			for limitKey, limit := range want {
				if daemon.Limits[limitKey] != limit {
					t.Fatalf("%s: GET /sidecar/limits reads %s as %d, want %d", what, limitKey, daemon.Limits[limitKey], limit)
				}
				if host.Limits[limitKey] != limit {
					t.Fatalf("%s: Quota.Limits reads %s as %d, want %d", what, limitKey, host.Limits[limitKey], limit)
				}
			}
		}
		checkLimits("the plan's keys", 0, directoryLimits)
		checkLimits("the plan's keys", 1, directoryLimits)

		// §21.7's tightening of one network's day
		caseId := server.NewId()
		target := model.AbuseLimitTightenTarget(sidecar.DirectoryLimitRequestsPerDay, 3)
		if err := model.SetAbuseLimitRow(ctx, networkIds[0], caseId, target); err != nil {
			t.Fatal(err)
		}
		tightened := map[string]int64{
			sidecar.DirectoryLimitRequestsPending: 20,
			sidecar.DirectoryLimitRequestsPerDay:  3,
			sidecar.DirectoryLimitRequestsPerRun:  5,
			sidecar.DirectoryLimitTargetPending:   100,
		}
		checkLimits("tightened", 0, tightened)
		checkLimits("the other network", 1, directoryLimits)

		// a key §21 does not tighten takes no row
		err := model.SetAbuseLimitRow(ctx, networkIds[0], caseId, model.AbuseLimitTightenTarget("directory_card_bytes", 1))
		if !errors.Is(err, model.ErrAbuseLimitRefused) {
			t.Fatalf("a tightening of directory_card_bytes answered %v", err)
		}
		checkLimits("after the refused row", 0, tightened)

		// the lift puts the plan's number back
		cleared, err := model.ClearAbuseLimitRow(ctx, networkIds[0], caseId, target)
		if err != nil || !cleared {
			t.Fatalf("the lift: %v, %v", cleared, err)
		}
		checkLimits("lifted", 0, directoryLimits)
	})
}
