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

		productUpdates, marketingSuppressed, country, userEmails := GetProductUpdateUserEmailsForUser(ctx, fakeUserId)
		connect.AssertEqual(t, productUpdates, false)
		connect.AssertEqual(t, marketingSuppressed, false)
		connect.AssertEqual(t, country, "")
		connect.AssertEqual(t, len(userEmails), 0)

		userEmailUserIds := GetUserEmailsForProductUpdatesSync(ctx, nil)
		connect.AssertEqual(t, len(userEmailUserIds), 0)

		userIdProductUpdatesSync := map[server.Id]bool{}
		SetProductUpdatesSyncForUsers(ctx, userIdProductUpdatesSync)

		userEmailNetworkIds := GetNetworkUserEmailsForProductUpdatesSync(ctx, nil)
		connect.AssertEqual(t, len(userEmailNetworkIds), 0)

		networkIdProductUpdatesSync := map[server.Id]bool{}
		SetNetworkProductUpdatesSyncForUsers(ctx, networkIdProductUpdatesSync)

	})
}

// The sync reads the suppression with the preference. The initial sync never
// adds a suppressed network's logins and adds them once a lift clears the
// suppression. The first suppression's time stays, and so does its reason
// unless a reason no connection lifts follows a liftable one; a bounce or
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
			readProductUpdates, readSuppressed, country, userEmails := GetProductUpdateUserEmailsForUser(ctx, userId)
			if readProductUpdates != productUpdates || readSuppressed != suppressed || country != "" || len(userEmails) != 1 || userEmails[0] != email {
				t.Fatalf("%s: sync read (%t, %t, %q, %d logins), want (%t, %t, \"\", 1)", label, readProductUpdates, readSuppressed, country, len(userEmails), productUpdates, suppressed)
			}
			state := GetNetworkMailState(ctx, networkId)
			if (state.SuppressedAt != nil) != suppressed || state.SuppressedReason != reason {
				t.Fatalf("%s: suppression %v %q, want %t %q", label, state.SuppressedAt, state.SuppressedReason, suppressed, reason)
			}
			_, userPending := GetUserEmailsForProductUpdatesSync(ctx, nil)[email]
			_, networkPending := GetNetworkUserEmailsForProductUpdatesSync(ctx, nil)[email]
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
		clearSuppression := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_preferences SET marketing_suppressed_at = NULL, marketing_suppressed_reason = '' WHERE network_id = $1`, networkId))
			})
		}
		expectFirstTime := func(label string) {
			t.Helper()
			if at := GetNetworkMailState(ctx, networkId).SuppressedAt; at == nil || !at.Equal(now) {
				t.Fatalf("%s: the first suppression's time changed: %v", label, at)
			}
		}

		expect("subscribed", true, false, "")

		suppress("sunset", now)
		expect("sunset", true, true, "sunset")
		// a replay keeps the first suppression's time and reason
		suppress("sunset", now.Add(time.Hour))
		expect("replayed sunset", true, true, "sunset")
		expectFirstTime("replayed sunset")
		// a liftable reason never replaces another
		suppress("switched", now.Add(time.Hour))
		expect("switched after a sunset", true, true, "sunset")

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
			clearSuppression()
		}

		// a reason no connection lifts, after a liftable one, replaces it and
		// keeps the first time. Before 2026-11-13 the sunset's reason stayed,
		// and the next connection lifted the complaint, the bounce or the
		// operator's suppression with it.
		for _, reason := range []string{"complained", "bounced", "sanctions", "automated_signup"} {
			suppress("sunset", now)
			suppress(reason, now.Add(time.Hour))
			expect(reason+" after a sunset", true, true, reason)
			expectFirstTime(reason + " after a sunset")
			if lift() {
				t.Fatalf("a %s suppression recorded after a sunset was lifted", reason)
			}
			expect(reason+" after a connection", true, true, reason)
			// a liftable reason after it changes nothing, nor does another
			// reason no connection lifts
			suppress("switched", now.Add(2*time.Hour))
			expect("switched after "+reason, true, true, reason)
			suppress("unsubscribe", now.Add(2*time.Hour))
			expect("another reason after "+reason, true, true, reason)
			clearSuppression()
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

// The product-updates queries leave out a network whose sign-up country gets
// no marketing: the initial sync would add neither list, the per-user sync
// reads the country, and the mirror's listing pages through exactly those
// networks. A network without an onboarding row has no country and stays in.
func TestProductUpdatesLeaveOutNoMarketingCountries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		type fixture struct {
			networkId server.Id
			userId    server.Id
			email     string
		}
		create := func(name string, country string, onboardingRow bool) fixture {
			f := fixture{networkId: server.NewId(), userId: server.NewId()}
			f.email = Testing_CreateNetwork(ctx, f.networkId, name, f.userId)
			AccountPreferencesSetForNetwork(ctx, f.networkId, true)
			if onboardingRow {
				CreateNetworkOnboarding(ctx, &NetworkOnboarding{NetworkId: f.networkId, CreatedAt: now, Email: true, Country: country})
			}
			return f
		}
		iran := create("synthetic-country-a", "IR", true)
		syria := create("synthetic-country-b", "SY", true)
		unitedStates := create("synthetic-country-c", "US", true)
		withoutRow := create("synthetic-country-d", "", false)
		noMarketingCountries := []string{"IR", "SY", "CU", "KP"}

		for f, want := range map[fixture]string{iran: "IR", syria: "SY", unitedStates: "US", withoutRow: ""} {
			if _, _, country, _ := GetProductUpdateUserEmailsForUser(ctx, f.userId); country != want {
				t.Fatalf("the sync read country %q, want %q", country, want)
			}
		}

		users := GetUserEmailsForProductUpdatesSync(ctx, noMarketingCountries)
		networks := GetNetworkUserEmailsForProductUpdatesSync(ctx, noMarketingCountries)
		for _, f := range []fixture{iran, syria} {
			if _, ok := users[f.email]; ok {
				t.Fatal("the initial sync would add a no-marketing country's login to network_users")
			}
			if _, ok := networks[f.email]; ok {
				t.Fatal("the initial sync would add a no-marketing country's login to new_networks")
			}
		}
		for _, f := range []fixture{unitedStates, withoutRow} {
			if users[f.email] != f.userId || networks[f.email] != f.networkId {
				t.Fatal("the initial sync left out a login whose country gets marketing")
			}
		}
		// without a list every subscribed login is added, as before
		if _, ok := GetUserEmailsForProductUpdatesSync(ctx, nil)[iran.email]; !ok {
			t.Fatal("no list left out a login")
		}
		if _, ok := GetNetworkUserEmailsForProductUpdatesSync(ctx, []string{})[iran.email]; !ok {
			t.Fatal("an empty list left out a login")
		}

		// the mirror's listing, one network a page
		want := map[server.Id]server.Id{iran.networkId: iran.userId, syria.networkId: syria.userId}
		got := map[server.Id]server.Id{}
		var after *server.Id
		for pages := 0; ; pages += 1 {
			if 3 < pages {
				t.Fatal("the listing did not end")
			}
			admins := ListNoMarketingCountryNetworkAdmins(ctx, noMarketingCountries, after, 1)
			if len(admins) == 0 {
				break
			}
			if len(admins) != 1 {
				t.Fatal("a page held more than its limit", len(admins))
			}
			got[admins[0].NetworkId] = admins[0].AdminUserId
			after = &admins[0].NetworkId
		}
		if len(got) != len(want) || got[iran.networkId] != iran.userId || got[syria.networkId] != syria.userId {
			t.Fatal("the listing", got)
		}
		if admins := ListNoMarketingCountryNetworkAdmins(ctx, nil, nil, 10); len(admins) != 0 {
			t.Fatal("no list listed networks", len(admins))
		}
	})
}

// The operator's batch suppression writes each existing network that is not
// suppressed for good, keeps the first suppression's time when it replaces a
// liftable reason, leaves a reason no connection lifts and an id that names
// no network alone, and writes nothing on a repeat.
func TestSuppressNetworksMarketingInTx(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		earlier := server.NowUtc().Add(-24 * time.Hour).Truncate(time.Second)
		now := server.NowUtc().Truncate(time.Second)
		create := func(name string) (networkId server.Id, userId server.Id) {
			networkId, userId = server.NewId(), server.NewId()
			Testing_CreateNetwork(ctx, networkId, name, userId)
			return
		}
		suppress := func(networkId server.Id, reason string) {
			server.Tx(ctx, func(tx server.PgTx) {
				SuppressNetworkMarketingInTx(tx, ctx, networkId, reason, earlier)
			})
		}
		// no preference row, a subscriber, a sunset, a complaint, a missing id
		freshNetworkId, freshUserId := create("synthetic-batch-fresh")
		subscribedNetworkId, subscribedUserId := create("synthetic-batch-subscribed")
		AccountPreferencesSetForNetwork(ctx, subscribedNetworkId, true)
		sunsetNetworkId, sunsetUserId := create("synthetic-batch-sunset")
		suppress(sunsetNetworkId, "sunset")
		complainedNetworkId, _ := create("synthetic-batch-complained")
		suppress(complainedNetworkId, "complained")
		missingNetworkId := server.NewId()
		networkIds := []server.Id{freshNetworkId, subscribedNetworkId, sunsetNetworkId, complainedNetworkId, missingNetworkId}

		states := GetNetworksMarketingSuppression(ctx, networkIds)
		if _, ok := states[missingNetworkId]; ok || len(states) != 4 {
			t.Fatal("states of", len(states), "networks")
		}
		if states[freshNetworkId].SuppressedAt != nil || states[subscribedNetworkId].SuppressedAt != nil ||
			states[sunsetNetworkId].SuppressedReason != "sunset" || states[complainedNetworkId].SuppressedReason != "complained" {
			t.Fatal("states before")
		}

		var written []*ProductUpdatesNetworkAdmin
		server.Tx(ctx, func(tx server.PgTx) {
			if inTx := GetNetworksMarketingSuppressionInTx(tx, ctx, networkIds); len(inTx) != 4 {
				t.Fatal("states in the transaction", len(inTx))
			}
			written = SuppressNetworksMarketingInTx(tx, ctx, networkIds, "sanctions", now)
		})
		wantAdmins := map[server.Id]server.Id{freshNetworkId: freshUserId, subscribedNetworkId: subscribedUserId, sunsetNetworkId: sunsetUserId}
		if len(written) != len(wantAdmins) {
			t.Fatal("wrote", len(written), "networks")
		}
		for i, admin := range written {
			if wantAdmins[admin.NetworkId] != admin.AdminUserId {
				t.Fatal("wrote an unexpected network or admin")
			}
			if 0 < i && !written[i-1].NetworkId.Less(admin.NetworkId) {
				t.Fatal("not in network id order")
			}
		}

		states = GetNetworksMarketingSuppression(ctx, networkIds)
		for _, networkId := range []server.Id{freshNetworkId, subscribedNetworkId} {
			if state := states[networkId]; state.SuppressedAt == nil || !state.SuppressedAt.Equal(now) || state.SuppressedReason != "sanctions" {
				t.Fatal("a new suppression", state.SuppressedAt, state.SuppressedReason)
			}
		}
		if state := states[sunsetNetworkId]; !state.SuppressedAt.Equal(earlier) || state.SuppressedReason != "sanctions" {
			t.Fatal("the sunset was not replaced with its first time kept", state.SuppressedAt, state.SuppressedReason)
		}
		if state := states[complainedNetworkId]; !state.SuppressedAt.Equal(earlier) || state.SuppressedReason != "complained" {
			t.Fatal("a complaint was replaced", state.SuppressedAt, state.SuppressedReason)
		}
		// a network without a preference row is subscribed and suppressed, as
		// SuppressNetworkMarketingInTx leaves it
		if mail := GetNetworkMailState(ctx, freshNetworkId); !mail.ProductUpdates || mail.SuppressedAt == nil {
			t.Fatal("the fresh network's preference row")
		}

		// a repeat writes nothing, and no connection lifts what it wrote
		server.Tx(ctx, func(tx server.PgTx) {
			if again := SuppressNetworksMarketingInTx(tx, ctx, networkIds, "sanctions", now.Add(time.Hour)); len(again) != 0 {
				t.Fatal("a repeat wrote", len(again), "networks")
			}
			for _, networkId := range []server.Id{freshNetworkId, subscribedNetworkId, sunsetNetworkId} {
				if LiftNetworkMarketingSuppressionInTx(tx, ctx, networkId, []string{"sunset", "switched"}) {
					t.Fatal("a connection lifted an operator's suppression")
				}
			}
		})
		if state := GetNetworksMarketingSuppression(ctx, networkIds)[freshNetworkId]; !state.SuppressedAt.Equal(now) {
			t.Fatal("a repeat moved the suppression's time", state.SuppressedAt)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if none := SuppressNetworksMarketingInTx(tx, ctx, nil, "sanctions", now); len(none) != 0 {
				t.Fatal("no networks wrote", len(none))
			}
		})
	})
}
