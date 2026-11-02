package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	//
	// "github.com/urnetwork/server/session"
)

func TestProductUpdates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fakeUserId := server.NewId()

		productUpdates, marketingSuppressed, userEmails := GetProductUpdateUserEmailsForUser(ctx, fakeUserId)
		connect.AssertEqual(t, productUpdates, false)
		connect.AssertEqual(t, marketingSuppressed, false)
		connect.AssertEqual(t, len(userEmails), 0)

		userEmailUserIds := GetUserEmailsForProductUpdatesSync(ctx)
		connect.AssertEqual(t, len(userEmailUserIds), 0)

		userIdProductUpdatesSync := map[server.Id]bool{}
		SetProductUpdatesSyncForUsers(ctx, userIdProductUpdatesSync)

		userEmailNetworkIds := GetNetworkUserEmailsForProductUpdatesSync(ctx)
		connect.AssertEqual(t, len(userEmailNetworkIds), 0)

		networkIdProductUpdatesSync := map[server.Id]bool{}
		SetNetworkProductUpdatesSyncForUsers(ctx, networkIdProductUpdatesSync)

	})
}

// The sync reads the suppression with the preference. The initial sync never
// adds a suppressed network's logins and adds them once a lift clears the
// suppression; the first suppression's reason stays, and a bounce or
// complaint is not liftable.
func TestProductUpdatesHonorMarketingSuppression(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, userId := server.NewId(), server.NewId()
		email := Testing_CreateNetwork(ctx, networkId, "synthetic-suppression-sync", userId)
		AccountPreferencesSetForNetwork(ctx, networkId, true)
		liftable := []string{"sunset", "switched"}
		now := server.NowUtc().Truncate(time.Second)

		expect := func(label string, productUpdates bool, suppressed bool, reason string) {
			t.Helper()
			readProductUpdates, readSuppressed, userEmails := GetProductUpdateUserEmailsForUser(ctx, userId)
			if readProductUpdates != productUpdates || readSuppressed != suppressed || len(userEmails) != 1 || userEmails[0] != email {
				t.Fatalf("%s: sync read (%t, %t, %d logins), want (%t, %t, 1)", label, readProductUpdates, readSuppressed, len(userEmails), productUpdates, suppressed)
			}
			state := GetNetworkMailState(ctx, networkId)
			if (state.SuppressedAt != nil) != suppressed || state.SuppressedReason != reason {
				t.Fatalf("%s: suppression %v %q, want %t %q", label, state.SuppressedAt, state.SuppressedReason, suppressed, reason)
			}
			_, userPending := GetUserEmailsForProductUpdatesSync(ctx)[email]
			_, networkPending := GetNetworkUserEmailsForProductUpdatesSync(ctx)[email]
			subscribed := productUpdates && !suppressed
			if userPending != subscribed || networkPending != subscribed {
				t.Fatalf("%s: initial sync would add (%t, %t), want %t", label, userPending, networkPending, subscribed)
			}
		}
		suppress := func(reason string, at time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				SuppressNetworkMarketingInTx(tx, ctx, networkId, reason, at)
			})
		}
		lift := func() (lifted bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				lifted = LiftNetworkMarketingSuppressionInTx(tx, ctx, networkId, liftable)
			})
			return
		}

		expect("subscribed", true, false, "")

		suppress("sunset", now)
		expect("sunset", true, true, "sunset")
		// the first suppression's time and reason stay
		suppress("complained", now.Add(time.Hour))
		expect("second suppression", true, true, "sunset")
		if at := GetNetworkMailState(ctx, networkId).SuppressedAt; at == nil || !at.Equal(now) {
			t.Fatalf("the first suppression's time changed: %v", at)
		}

		if !lift() {
			t.Fatal("a sunset was not lifted")
		}
		expect("lifted", true, false, "")
		if lift() {
			t.Fatal("a replayed lift lifted again")
		}

		for _, reason := range []string{"bounced", "complained"} {
			suppress(reason, now)
			if lift() {
				t.Fatalf("a %s suppression was lifted", reason)
			}
			expect(reason, true, true, reason)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_preferences SET marketing_suppressed_at = NULL, marketing_suppressed_reason = '' WHERE network_id = $1`, networkId))
			})
		}

		// an opt-out reads as such with or without a suppression
		AccountPreferencesSetForNetwork(ctx, networkId, false)
		expect("opted out", false, false, "")
		suppress("switched", now)
		expect("opted out and suppressed", false, true, "switched")
		if !lift() {
			t.Fatal("a switched answer was not lifted")
		}
		expect("opted out after the lift", false, false, "")

		var adminUserId server.Id
		var ok, unknownOk bool
		server.Tx(ctx, func(tx server.PgTx) {
			adminUserId, ok = GetNetworkAdminUserIdInTx(tx, ctx, networkId)
			_, unknownOk = GetNetworkAdminUserIdInTx(tx, ctx, server.NewId())
		})
		if !ok || adminUserId != userId || unknownOk {
			t.Fatalf("admin lookup (%s, %t), unknown network %t", adminUserId, ok, unknownOk)
		}
	})
}
