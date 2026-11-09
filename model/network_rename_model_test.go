package model

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §7.12's one rename (RenameNetwork).

// A rename with the dependencies a test sets.
func testNetworkRenamer(now func() time.Time) *networkRenamer {
	renamer := newNetworkRenamer()
	renamer.now = now
	return renamer
}

// A network the test owns, and the session of its owner.
func testRenameNetwork(t testing.TB, ctx context.Context, name string) (server.Id, *session.ClientSession) {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, name, userId)
	return networkId, session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
}

func requireRenameRefused(t testing.TB, result *RenameNetworkResult, code string) {
	t.Helper()
	if result.Error == nil || result.Error.Code != code {
		t.Fatalf("rename answered %+v (error %+v), want a %s refusal", result, result.Error, code)
	}
}

func requireRenamed(t testing.TB, ctx context.Context, result *RenameNetworkResult, networkId server.Id, name string) {
	t.Helper()
	if result.Error != nil || result.NetworkName != name {
		t.Fatalf("rename answered %+v (error %+v), want %q", result, result.Error, name)
	}
	if stored := networkNameOf(ctx, networkId); stored != name {
		t.Fatalf("the network is named %q, want %q", stored, name)
	}
}

// The tombstone's hold on a name, or zero for none.
func reclaimCoolDownUntil(ctx context.Context, name string) (coolDownUntil time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT cool_down_until FROM network_name_reclaim WHERE old_name = $1`, name)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&coolDownUntil))
			}
		})
	})
	return
}

// A released name is held for no one for exactly 24 hours: refused a moment
// before the hold ends, taken when it ends.
func TestRenameHoldsAReleasedName24Hours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		t0 := server.NowUtc()
		now := t0
		renamer := testNetworkRenamer(func() time.Time { return now })

		firstNetworkId, firstSession := testRenameNetwork(t, ctx, "hold-released-first")
		requireRenamed(t, ctx, renamer.rename(firstSession, &RenameNetworkArgs{NewName: "hold-released-moved"}), firstNetworkId, "hold-released-moved")
		if until := reclaimCoolDownUntil(ctx, "hold-released-first"); !until.Equal(t0.Add(24 * time.Hour)) {
			t.Fatalf("the released name is held until %s, want %s", until, t0.Add(24*time.Hour))
		}

		secondNetworkId, secondSession := testRenameNetwork(t, ctx, "hold-released-second")
		now = t0.Add(24*time.Hour - time.Microsecond)
		requireRenameRefused(t, renamer.rename(secondSession, &RenameNetworkArgs{NewName: "hold-released-first"}), RenameNetworkRefusalNotAvailable)
		now = t0.Add(24 * time.Hour)
		requireRenamed(t, ctx, renamer.rename(secondSession, &RenameNetworkArgs{NewName: "hold-released-first"}), secondNetworkId, "hold-released-first")
	})
}

// Two renames a day: the third in the window is refused and spends nothing, and
// one is allowed again once the window passes the first. A claim has its own
// budget.
func TestRenameAllowsTwoADay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		t0 := server.NowUtc()
		now := t0
		renamer := testNetworkRenamer(func() time.Time { return now })
		networkId, clientSession := testRenameNetwork(t, ctx, "budget-network-zero")

		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "budget-network-one"}), networkId, "budget-network-one")
		now = t0.Add(time.Hour)
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "budget-network-two"}), networkId, "budget-network-two")
		now = t0.Add(24 * time.Hour)
		requireRenameRefused(t, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "budget-network-three"}), RenameNetworkRefusalRateLimited)
		// the claim budget is apart
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "budget-network-claim", Claim: true}), networkId, "budget-network-claim")
		// the first change leaves the window
		now = t0.Add(24*time.Hour + time.Microsecond)
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "budget-network-three"}), networkId, "budget-network-three")
	})
}

// Another network's name one edit away refuses a name, before any budget is
// spent; two edits away does not, and neither does the network's own name.
func TestRenameRefusesANameOneEditFromAnotherNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		inMemorySearch, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		t0 := server.NowUtc()
		renamer := testNetworkRenamer(func() time.Time { return t0 })

		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "nearby-network", server.NewId())
		inMemorySearch.Add(ctx, "nearby-network", otherNetworkId, 0)

		networkId, clientSession := testRenameNetwork(t, ctx, "faraway-name")
		inMemorySearch.Add(ctx, "faraway-name", networkId, 0)

		requireRenameRefused(t, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "nearby-networx"}), RenameNetworkRefusalNotAvailable)
		requireRenameRefused(t, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "nearby-network"}), RenameNetworkRefusalNotAvailable)
		// refusals spent nothing: two renames still go through
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "nearby-netwoyz"}), networkId, "nearby-netwoyz")
		inMemorySearch.Add(ctx, "nearby-netwoyz", networkId, 0)
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "nearby-netwoyx"}), networkId, "nearby-netwoyx")
	})
}

// §12.5's label rules beyond ValidateNetworkName.
func TestRenameAppliesTheLabelRules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		renamer := testNetworkRenamer(server.NowUtc)
		networkId, clientSession := testRenameNetwork(t, ctx, "label-rules-network")

		for _, name := range []string{"-label-rules", "label-rules-", "la--belrules", "xn--labelrule", "abc"} {
			requireRenameRefused(t, renamer.rename(clientSession, &RenameNetworkArgs{NewName: name}), RenameNetworkRefusalInvalid)
		}
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "lab--elrules"}), networkId, "lab--elrules")
	})
}

// §12.5's reserved list and brand rule, through sidecar's ValidateDnsName
// (DNS-M0-01): a reserved label, a reserved pattern and a brand-confusable
// token are refused as invalid, with a sentence about the network name.
func TestRenameRefusesReservedAndBrandNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		renamer := testNetworkRenamer(server.NowUtc)
		networkId, clientSession := testRenameNetwork(t, ctx, "reserved-rules-network")

		for _, name := range []string{"postmaster", "main-reserved", "rules-lb", "urnetw0rk-team", "paypa1-help"} {
			result := renamer.rename(clientSession, &RenameNetworkArgs{NewName: name})
			requireRenameRefused(t, result, RenameNetworkRefusalInvalid)
			if !strings.HasPrefix(result.Error.Message, "Network name ") {
				t.Fatalf("%q refused with %q", name, result.Error.Message)
			}
		}
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "reserved-rules-moved"}), networkId, "reserved-rules-moved")
	})
}

// Holds every name a test names, as dns_label will.
type testNetworkLabelHolder map[string]bool

func (self testNetworkLabelHolder) HoldsNameInTx(ctx context.Context, query server.PgCanQuery, name string) bool {
	return self[name]
}

// A name dns_label holds, as a label or alias in any state, is refused; the
// holder before dns_label exists holds none and never errs.
func TestRenameRefusesANameDnsLabelHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		renamer := testNetworkRenamer(server.NowUtc)
		renamer.labelHolder = testNetworkLabelHolder{"held-by-a-label": true}
		networkId, clientSession := testRenameNetwork(t, ctx, "label-holder-network")

		requireRenameRefused(t, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "held-by-a-label"}), RenameNetworkRefusalNotAvailable)
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "held-by-no-label"}), networkId, "held-by-no-label")

		server.Db(ctx, func(conn server.PgConn) {
			if defaultNetworkLabelHolder().HoldsNameInTx(ctx, conn, "held-by-a-label") {
				t.Fatal("the holder before dns_label holds a name")
			}
		})
	})
}

// Only the network's owner renames it.
func TestRenameRefusesAnyoneButTheOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		networkId, _ := testRenameNetwork(t, ctx, "owner-only-network")
		stranger := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			UserId:    server.NewId(),
		})
		requireRenameRefused(t, RenameNetwork(stranger, &RenameNetworkArgs{NewName: "owner-only-renamed"}), RenameNetworkRefusalNotOwner)
		if stored := networkNameOf(ctx, networkId); stored != "owner-only-network" {
			t.Fatalf("the network is named %q", stored)
		}
	})
}

// A name the network already has is no change: nothing is written, released or
// spent.
func TestRenameToTheSameNameChangesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		_, clientSession := testRenameNetwork(t, ctx, "same-name-network")
		for range 3 {
			result := RenameNetwork(clientSession, &RenameNetworkArgs{NewName: "Same Name Network"})
			if result.Error != nil || !result.Unchanged || result.NetworkName != "same-name-network" {
				t.Fatalf("rename answered %+v (error %+v)", result, result.Error)
			}
		}
		if !reclaimCoolDownUntil(ctx, "same-name-network").IsZero() {
			t.Fatal("an unchanged name was released")
		}
	})
}

// Deleting a network holds its name for 24 hours, so neither a create nor a
// rename can take it at once.
func TestNetworkDeletionHoldsItsName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "deleted-network-name", userId)
		before := server.NowUtc()
		if removed, _ := RemoveNetwork(ctx, networkId, &userId); !removed {
			t.Fatal("the network was not removed")
		}
		until := reclaimCoolDownUntil(ctx, "deleted-network-name")
		if until.Before(before.Add(24*time.Hour)) || server.NowUtc().Add(24*time.Hour).Before(until) {
			t.Fatalf("the deleted name is held until %s", until)
		}

		password := "synthetic-password-1"
		userAuth := "deleted-name-taker@example.com"
		created := networkCreateUserAuth(
			ctx,
			&NetworkCreateArgs{
				UserName:    "deleted-name-taker",
				UserAuth:    &userAuth,
				Password:    &password,
				NetworkName: "deleted-network-name",
				Terms:       true,
			},
			&userAuth,
			"deleted-network-name",
			false,
			false,
		)
		if created.Created || created.refusalStatus != http.StatusConflict || created.refusalMessage != networkNameNotAvailableMessage {
			t.Fatalf("a create took the deleted name: %+v", created)
		}

		_, clientSession := testRenameNetwork(t, ctx, "deleted-name-renamer")
		requireRenameRefused(t, RenameNetwork(clientSession, &RenameNetworkArgs{NewName: "deleted-network-name"}), RenameNetworkRefusalNotAvailable)
	})
}
