package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The referral terms (network_referral_terms.go). Until 2026-11-24 every link
// paid both its sides from the run after it was made, for as long as it
// stood: no app use was asked of the referred network, nothing limited the
// period, and a re-point (the set-referral upsert) restarted the link's
// create_time.

const referralTermsTestMib = ByteCount(1024 * 1024)

// Each case is one link at the grant window's start, under the default terms
// or the ones the case names.
func TestReferralLinkPays(t *testing.T) {
	startTime := time.Date(2026, 12, 20, 11, 36, 0, 0, time.UTC)
	day := 24 * time.Hour
	limitFrom := time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)
	withLimit := DefaultReferralTermsSettings()
	withLimit.ExistingLinkLimitFrom = limitFrom
	laterLimit := DefaultReferralTermsSettings()
	laterLimit.ExistingLinkLimitFrom = startTime.Add(time.Hour)
	noAppUse := DefaultReferralTermsSettings()
	noAppUse.AppUseDayCount = 0
	forLife := DefaultReferralTermsSettings()
	forLife.LinkDuration = 0
	for _, c := range []struct {
		name  string
		link  referralLink
		terms ReferralTermsSettings
		pays  bool
	}{
		{name: "a new link without app use", link: referralLink{createTime: startTime.Add(-day)}, terms: DefaultReferralTermsSettings(), pays: false},
		{name: "a new link once qualified", link: referralLink{createTime: startTime.Add(-day), qualified: true}, terms: DefaultReferralTermsSettings(), pays: true},
		{name: "a new link without app use, when none is asked", link: referralLink{createTime: startTime.Add(-day)}, terms: noAppUse, pays: true},
		{name: "a qualified new link on its last day", link: referralLink{createTime: startTime.Add(-90*day + time.Second), qualified: true}, terms: DefaultReferralTermsSettings(), pays: true},
		{name: "a qualified new link at 90 days", link: referralLink{createTime: startTime.Add(-90 * day), qualified: true}, terms: DefaultReferralTermsSettings(), pays: false},
		{name: "a qualified new link past 90 days, for life", link: referralLink{createTime: startTime.Add(-200 * day), qualified: true}, terms: forLife, pays: true},
		{name: "an existing link without app use", link: referralLink{createTime: startTime.Add(-day), existingLink: true}, terms: DefaultReferralTermsSettings(), pays: true},
		{name: "an existing link past 90 days, no date set", link: referralLink{createTime: startTime.Add(-200 * day), existingLink: true}, terms: DefaultReferralTermsSettings(), pays: true},
		{name: "an existing link past 90 days, before the date", link: referralLink{createTime: startTime.Add(-200 * day), existingLink: true}, terms: laterLimit, pays: true},
		{name: "an existing link past 90 days, from the date", link: referralLink{createTime: startTime.Add(-200 * day), existingLink: true}, terms: withLimit, pays: false},
		{name: "an existing link under 90 days, from the date", link: referralLink{createTime: startTime.Add(-30 * day), existingLink: true}, terms: withLimit, pays: true},
	} {
		if pays := referralLinkPays(c.link, startTime, &c.terms); pays != c.pays {
			t.Errorf("%s: pays %t, want %t", c.name, pays, c.pays)
		}
	}
}

// A referrer counts its paying links only, and a referred network on a link
// that does not pay is in no referrer's list.
func TestPayingReferralsCountOnlyPayingLinks(t *testing.T) {
	startTime := time.Date(2026, 12, 20, 11, 36, 0, 0, time.UTC)
	terms := DefaultReferralTermsSettings()
	referrerId := server.NewId()
	qualifiedId, waitingId, existingId, pastId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	referrals := payingReferrals([]referralLink{
		{networkId: qualifiedId, referralNetworkId: referrerId, createTime: startTime.Add(-time.Hour), qualified: true},
		{networkId: waitingId, referralNetworkId: referrerId, createTime: startTime.Add(-time.Hour)},
		{networkId: existingId, referralNetworkId: referrerId, createTime: startTime.Add(-400 * 24 * time.Hour), existingLink: true},
		{networkId: pastId, referralNetworkId: referrerId, createTime: startTime.Add(-91 * 24 * time.Hour), qualified: true},
	}, startTime, &terms)
	connect.AssertEqual(t, referrals[referrerId], []server.Id{qualifiedId, existingId})
	connect.AssertEqual(t, len(referrals), 1)
}

// An absent key keeps its default, and each key overrides it. A malformed
// date refuses to load rather than lift the limit.
func TestParseReferralTerms(t *testing.T) {
	connect.AssertEqual(t, parseReferralTerms(proReferralYaml{}), DefaultReferralTermsSettings())
	defaults := DefaultReferralTermsSettings()
	connect.AssertEqual(t, defaults.LinkDuration, 90*24*time.Hour)
	connect.AssertEqual(t, defaults.AppUseDailyByteCount, 10*referralTermsTestMib)
	connect.AssertEqual(t, defaults.AppUseDayCount, 2)
	connect.AssertEqual(t, defaults.ExistingLinkLimitFrom.IsZero(), true)

	zero := 0
	terms := parseReferralTerms(proReferralYaml{
		LinkDuration:          "720h",
		ExistingLinkLimitFrom: "2026-12-03T00:00:00Z",
		AppUseDailyBytes:      "1mib",
		AppUseDays:            &zero,
	})
	connect.AssertEqual(t, terms.LinkDuration, 720*time.Hour)
	connect.AssertEqual(t, terms.ExistingLinkLimitFrom, time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, terms.AppUseDailyByteCount, referralTermsTestMib)
	connect.AssertEqual(t, terms.AppUseDayCount, 0)
	connect.AssertEqual(t, parseReferralTerms(proReferralYaml{ExistingLinkLimitFrom: "2026-12-03"}).ExistingLinkLimitFrom, time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC))

	refused := func() (refused bool) {
		defer func() {
			refused = recover() != nil
		}()
		parseReferralTerms(proReferralYaml{ExistingLinkLimitFrom: "next week"})
		return
	}
	connect.AssertEqual(t, refused(), true)
}

// A network with one app client: a device and its top-level client.
func referralTermsTestNetwork(ctx context.Context, name string) (networkId server.Id, clientId server.Id) {
	networkId = server.NewId()
	Testing_CreateNetwork(ctx, networkId, name+"-"+networkId.String()[24:], server.NewId())
	clientId = server.NewId()
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic device", "synthetic spec")
	return
}

// Meters byteCount as paid by the client in the usage block of usageTime and
// drains that block, as the rollup does. Each call names its own block, so no
// drain finds a block already applied.
func referralTermsTestUse(ctx context.Context, clientId server.Id, byteCount ByteCount, usageTime time.Time) {
	RecordClientDataUsage(ctx, clientId, byteCount, usageTime)
	drainClientDataUsageShard(ctx, clientDataUsageBlockNumber(usageTime), clientDataUsageShard(clientId))
}

// The network's recorded days of app use.
func referralTermsTestAppDays(ctx context.Context, networkId server.Id) map[time.Time]ByteCount {
	days := map[time.Time]ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT day, app_byte_count FROM network_referral_app_day WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var day time.Time
				var byteCount ByteCount
				server.Raise(result.Scan(&day, &byteCount))
				days[day.UTC()] = byteCount
			}
		})
	})
	return days
}

// The link's create time and whether it is an existing link.
func referralTermsTestLink(ctx context.Context, networkId server.Id) (createTime time.Time, existingLink bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT create_time, existing_link FROM network_referral WHERE network_id = $1`,
			networkId,
		).Scan(&createTime, &existingLink))
	})
	return
}

// Moves the link's create time back to createTime, as if made then, and makes
// it an existing link or not.
func referralTermsTestSetLink(ctx context.Context, networkId server.Id, createTime time.Time, existingLink bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_referral SET create_time = $2, existing_link = $3 WHERE network_id = $1`,
			networkId,
			createTime,
			existingLink,
		))
		if tag.RowsAffected() != 1 {
			panic("no link to set")
		}
	})
}

// The pro.yml amounts and terms of these tests, restored after the test.
func referralTermsTestConfig(t testing.TB) *ProConfig {
	config := Pro()
	previous := *config
	t.Cleanup(func() { *config = previous })
	config.MaxReferrals = 20
	config.seekerDataMultiplier = 1
	config.ReferralTerms = DefaultReferralTermsSettings()
	return config
}

// A new referred network with no app use earns nothing for either side.
// One day of 10 MiB is not enough, nor is 20 MiB on one day; on the second
// distinct day of 10 MiB both sides earn, from the next run. A past day short
// of 10 MiB is dropped, and the days of a qualified network go once it has
// qualified.
func TestReferralBonusWaitsForTheReferredNetworkToUseTheApp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		referralTermsTestConfig(t)
		referrerId, _ := referralTermsTestNetwork(ctx, "referrer")
		refereeId, refereeClientId := referralTermsTestNetwork(ctx, "referee")
		if CreateNetworkReferral(ctx, refereeId, CreateNetworkReferralCode(ctx, referrerId).ReferralCode) == nil {
			t.Fatal("synthetic referral rejected")
		}

		now := server.NowUtc()
		today := auditDeviceDayStart(now)
		yesterday := today.Add(-24 * time.Hour)
		twoDaysAgo := today.Add(-48 * time.Hour)
		grant := func() map[server.Id]ByteCount {
			startTime, endTime := ReferralGrantWindow(now)
			return AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		}
		assertNothing := func(added map[server.Id]ByteCount) {
			for _, networkId := range []server.Id{referrerId, refereeId} {
				if _, granted := added[networkId]; granted {
					t.Fatalf("granted %d before the referred network qualified", added[networkId])
				}
			}
		}

		assertNothing(grant())

		// 3 MiB two days ago, 12 MiB yesterday in two blocks, 9 MiB today
		referralTermsTestUse(ctx, refereeClientId, 3*referralTermsTestMib, twoDaysAgo.Add(time.Hour))
		referralTermsTestUse(ctx, refereeClientId, 6*referralTermsTestMib, yesterday.Add(time.Hour))
		referralTermsTestUse(ctx, refereeClientId, 6*referralTermsTestMib, yesterday.Add(2*time.Hour))
		referralTermsTestUse(ctx, refereeClientId, 9*referralTermsTestMib, today.Add(time.Minute))
		connect.AssertEqual(t, referralTermsTestAppDays(ctx, refereeId), map[time.Time]ByteCount{
			twoDaysAgo: 3 * referralTermsTestMib,
			yesterday:  12 * referralTermsTestMib,
			today:      9 * referralTermsTestMib,
		})
		assertNothing(grant())
		// the past day short of 10 MiB can count for nothing
		connect.AssertEqual(t, referralTermsTestAppDays(ctx, refereeId), map[time.Time]ByteCount{
			yesterday: 12 * referralTermsTestMib,
			today:     9 * referralTermsTestMib,
		})

		// 2 MiB more today makes the second day
		referralTermsTestUse(ctx, refereeClientId, 2*referralTermsTestMib, today.Add(2*time.Minute))
		added := grant()
		connect.AssertEqual(t, added[referrerId], ByteCount(100))
		connect.AssertEqual(t, added[refereeId], ByteCount(10))
		connect.AssertEqual(t, len(referralTermsTestAppDays(ctx, refereeId)), 0)

		// qualified for good: the next run pays again, with no more app use
		added = grant()
		connect.AssertEqual(t, added[referrerId], ByteCount(100))
		connect.AssertEqual(t, added[refereeId], ByteCount(10))
	})
}

// A referrer's bonus counts only its paying links: a qualified new referral
// and an existing link, not a new referral with one day of app use or none.
// The cap still applies to the count.
func TestReferralBonusCountsOnlyQualifyingReferrals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := referralTermsTestConfig(t)
		now := server.NowUtc()
		today := auditDeviceDayStart(now)
		referrerId, _ := referralTermsTestNetwork(ctx, "referrer")
		code := CreateNetworkReferralCode(ctx, referrerId).ReferralCode
		link := func(name string) (server.Id, server.Id) {
			networkId, clientId := referralTermsTestNetwork(ctx, name)
			if CreateNetworkReferral(ctx, networkId, code) == nil {
				t.Fatal("synthetic referral rejected")
			}
			return networkId, clientId
		}
		qualifiedId, qualifiedClientId := link("qualified")
		oneDayId, oneDayClientId := link("one-day")
		noUseId, _ := link("no-use")
		existingId, _ := link("existing")
		referralTermsTestSetLink(ctx, existingId, now.Add(-10*24*time.Hour), true)

		referralTermsTestUse(ctx, qualifiedClientId, 10*referralTermsTestMib, today.Add(-24*time.Hour))
		referralTermsTestUse(ctx, qualifiedClientId, 10*referralTermsTestMib, today.Add(time.Minute))
		referralTermsTestUse(ctx, oneDayClientId, 50*referralTermsTestMib, today.Add(2*time.Minute))

		startTime, endTime := ReferralGrantWindow(now)
		added := AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		connect.AssertEqual(t, added[referrerId], ByteCount(200))
		connect.AssertEqual(t, added[qualifiedId], ByteCount(10))
		connect.AssertEqual(t, added[existingId], ByteCount(10))
		for _, networkId := range []server.Id{oneDayId, noUseId} {
			if _, granted := added[networkId]; granted {
				t.Fatalf("a referral that has not qualified was granted %d", added[networkId])
			}
		}

		// the cap counts paying links
		config.MaxReferrals = 1
		startTime, endTime = ReferralGrantWindow(now.Add(time.Hour))
		added = AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		connect.AssertEqual(t, added[referrerId], ByteCount(100))
	})
}

// A link pays for 90 days from its creation: a qualified new link of 89 days
// pays both sides, one of 91 days neither.
func TestReferralBonusEndsAfterTheLinkPeriod(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		referralTermsTestConfig(t)
		now := server.NowUtc()
		day := 24 * time.Hour
		referrerId, _ := referralTermsTestNetwork(ctx, "referrer")
		code := CreateNetworkReferralCode(ctx, referrerId).ReferralCode
		link := func(name string, age time.Duration) server.Id {
			networkId, _ := referralTermsTestNetwork(ctx, name)
			if CreateNetworkReferral(ctx, networkId, code) == nil {
				t.Fatal("synthetic referral rejected")
			}
			referralTermsTestSetLink(ctx, networkId, now.Add(-age), false)
			Testing_QualifyReferralReferee(ctx, networkId, now.Add(-age))
			return networkId
		}
		currentId := link("current", 89*day)
		pastId := link("past", 91*day)

		startTime, endTime := ReferralGrantWindow(now)
		added := AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		connect.AssertEqual(t, added[referrerId], ByteCount(100))
		connect.AssertEqual(t, added[currentId], ByteCount(10))
		if _, granted := added[pastId]; granted {
			t.Fatalf("a link past its period was granted %d", added[pastId])
		}
	})
}

// A link that existed when the terms began pays with no app use, and its
// period starts to count only at existing_link_limit_from: until the date is
// set, and before it, a link of 120 days pays; from the date it pays nothing,
// and a link of 30 days still pays until its own 90 days.
func TestReferralExistingLinksKeepPayingUntilTheSetDate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := referralTermsTestConfig(t)
		now := server.NowUtc()
		day := 24 * time.Hour
		referrerId, _ := referralTermsTestNetwork(ctx, "referrer")
		code := CreateNetworkReferralCode(ctx, referrerId).ReferralCode
		link := func(name string, age time.Duration) server.Id {
			networkId, _ := referralTermsTestNetwork(ctx, name)
			if CreateNetworkReferral(ctx, networkId, code) == nil {
				t.Fatal("synthetic referral rejected")
			}
			referralTermsTestSetLink(ctx, networkId, now.Add(-age), true)
			return networkId
		}
		oldId := link("old", 120*day)
		youngId := link("young", 30*day)
		grant := func(at time.Time) map[server.Id]ByteCount {
			startTime, endTime := ReferralGrantWindow(at)
			return AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		}

		for _, limitFrom := range []time.Time{{}, now.Add(day)} {
			config.ReferralTerms.ExistingLinkLimitFrom = limitFrom
			added := grant(now)
			connect.AssertEqual(t, added[referrerId], ByteCount(200))
			connect.AssertEqual(t, added[oldId], ByteCount(10))
			connect.AssertEqual(t, added[youngId], ByteCount(10))
		}

		config.ReferralTerms.ExistingLinkLimitFrom = now.Add(-time.Hour)
		added := grant(now.Add(time.Hour))
		connect.AssertEqual(t, added[referrerId], ByteCount(100))
		connect.AssertEqual(t, added[youngId], ByteCount(10))
		if _, granted := added[oldId]; granted {
			t.Fatalf("an existing link past its period was granted %d after the set date", added[oldId])
		}
	})
}

// A re-point keeps the link's create time, so the period still counts from
// the first link; so does a link made again after an unlink. A re-point to
// the same referrer is no re-point.
func TestReferralRepointKeepsTheLinkPeriod(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		referralTermsTestConfig(t)
		now := server.NowUtc()
		firstLinkTime := now.Add(-60 * 24 * time.Hour)
		referrerAId, _ := referralTermsTestNetwork(ctx, "referrer-a")
		referrerBId, _ := referralTermsTestNetwork(ctx, "referrer-b")
		codeA := CreateNetworkReferralCode(ctx, referrerAId).ReferralCode
		codeB := CreateNetworkReferralCode(ctx, referrerBId).ReferralCode
		refereeId, _ := referralTermsTestNetwork(ctx, "referee")
		if CreateNetworkReferral(ctx, refereeId, codeA) == nil {
			t.Fatal("synthetic referral rejected")
		}
		referralTermsTestSetLink(ctx, refereeId, firstLinkTime, false)

		assertFirstLinkTime := func(step string) {
			createTime, _ := referralTermsTestLink(ctx, refereeId)
			if !createTime.Equal(firstLinkTime) {
				t.Fatalf("%s: the link's create time is %s, want the first link's %s", step, createTime, firstLinkTime)
			}
		}
		connect.AssertNotEqual(t, CreateNetworkReferral(ctx, refereeId, codeA), nil)
		assertFirstLinkTime("a re-point to the same referrer")
		connect.AssertNotEqual(t, CreateNetworkReferral(ctx, refereeId, codeB), nil)
		connect.AssertEqual(t, GetReferralNetworkByChildNetworkId(ctx, refereeId).Id, referrerBId)
		assertFirstLinkTime("a re-point")

		UnlinkReferralNetwork(ctx, refereeId)
		connect.AssertEqual(t, GetReferralNetworkByChildNetworkId(ctx, refereeId), nil)
		connect.AssertNotEqual(t, CreateNetworkReferral(ctx, refereeId, codeA), nil)
		connect.AssertEqual(t, GetReferralNetworkByChildNetworkId(ctx, refereeId).Id, referrerAId)
		assertFirstLinkTime("a link made again after an unlink")
	})
}

// An existing link that is re-pointed to another referrer is a new referral:
// it pays nothing until the referred network qualifies by its app use, and
// then pays the new referrer. A re-point to the same referrer keeps it an
// existing link.
func TestReferralRepointedExistingLinkNeedsAppUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		referralTermsTestConfig(t)
		now := server.NowUtc()
		today := auditDeviceDayStart(now)
		referrerAId, _ := referralTermsTestNetwork(ctx, "referrer-a")
		referrerBId, _ := referralTermsTestNetwork(ctx, "referrer-b")
		codeA := CreateNetworkReferralCode(ctx, referrerAId).ReferralCode
		codeB := CreateNetworkReferralCode(ctx, referrerBId).ReferralCode
		refereeId, refereeClientId := referralTermsTestNetwork(ctx, "referee")
		if CreateNetworkReferral(ctx, refereeId, codeA) == nil {
			t.Fatal("synthetic referral rejected")
		}
		referralTermsTestSetLink(ctx, refereeId, now.Add(-10*24*time.Hour), true)
		runCount := 0
		grant := func() map[server.Id]ByteCount {
			runCount += 1
			startTime, endTime := ReferralGrantWindow(now.Add(time.Duration(runCount) * time.Minute))
			return AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		}

		added := grant()
		connect.AssertEqual(t, added[referrerAId], ByteCount(100))
		connect.AssertEqual(t, added[refereeId], ByteCount(10))

		connect.AssertNotEqual(t, CreateNetworkReferral(ctx, refereeId, codeA), nil)
		_, existingLink := referralTermsTestLink(ctx, refereeId)
		connect.AssertEqual(t, existingLink, true)

		connect.AssertNotEqual(t, CreateNetworkReferral(ctx, refereeId, codeB), nil)
		_, existingLink = referralTermsTestLink(ctx, refereeId)
		connect.AssertEqual(t, existingLink, false)
		added = grant()
		for _, networkId := range []server.Id{referrerAId, referrerBId, refereeId} {
			if _, granted := added[networkId]; granted {
				t.Fatalf("a re-pointed link was granted %d before the referred network qualified", added[networkId])
			}
		}

		referralTermsTestUse(ctx, refereeClientId, 10*referralTermsTestMib, today.Add(-24*time.Hour))
		referralTermsTestUse(ctx, refereeClientId, 10*referralTermsTestMib, today.Add(time.Minute))
		added = grant()
		connect.AssertEqual(t, added[referrerBId], ByteCount(100))
		connect.AssertEqual(t, added[refereeId], ByteCount(10))
		_, granted := added[referrerAId]
		connect.AssertEqual(t, granted, false)
	})
}

// Data through a hosted proxy is not app use: neither the proxy's nor its
// windows', nor a proxy's an app client minted, nor that proxy's windows'.
// The same data through the app client qualifies the network.
func TestReferralHostedProxyUseDoesNotQualify(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		referralTermsTestConfig(t)
		now := server.NowUtc()
		today := auditDeviceDayStart(now)
		referrerId, _ := referralTermsTestNetwork(ctx, "referrer")
		refereeId, appClientId := referralTermsTestNetwork(ctx, "referee")
		if CreateNetworkReferral(ctx, refereeId, CreateNetworkReferralCode(ctx, referrerId).ReferralCode) == nil {
			t.Fatal("synthetic referral rejected")
		}

		// a top-level hosted proxy and its window, and a hosted proxy the app
		// client minted with its window
		proxyClientId, proxyWindowId := server.NewId(), server.NewId()
		childProxyClientId, childProxyWindowId := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			client := func(clientId server.Id, sourceClientId *server.Id) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_client (client_id, network_id, description, create_time, auth_time, source_client_id)
						VALUES ($1, $2, 'synthetic', $3, $3, $4)
					`,
					clientId,
					refereeId,
					now,
					sourceClientId,
				))
			}
			proxy := func(clientId server.Id) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO proxy_device_config (proxy_id, client_id, instance_id, config_json) VALUES ($1, $2, $3, '{}')`,
					server.NewId(),
					clientId,
					server.NewId(),
				))
			}
			client(proxyClientId, nil)
			proxy(proxyClientId)
			client(proxyWindowId, &proxyClientId)
			client(childProxyClientId, &appClientId)
			proxy(childProxyClientId)
			client(childProxyWindowId, &childProxyClientId)
		})

		grant := func(at time.Time) map[server.Id]ByteCount {
			startTime, endTime := ReferralGrantWindow(at)
			return AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		}
		blockTime := func(day time.Time, i int) time.Time {
			return day.Add(time.Duration(i) * ClientDataUsageBlockDuration)
		}
		for i, clientId := range []server.Id{proxyClientId, proxyWindowId, childProxyClientId, childProxyWindowId} {
			for _, day := range []time.Time{today.Add(-24 * time.Hour), today} {
				referralTermsTestUse(ctx, clientId, 20*referralTermsTestMib, blockTime(day, i))
			}
		}
		connect.AssertEqual(t, len(referralTermsTestAppDays(ctx, refereeId)), 0)
		added := grant(now)
		for _, networkId := range []server.Id{referrerId, refereeId} {
			if _, granted := added[networkId]; granted {
				t.Fatalf("hosted proxy use qualified a referral: granted %d", added[networkId])
			}
		}

		for _, day := range []time.Time{today.Add(-24 * time.Hour), today} {
			referralTermsTestUse(ctx, appClientId, 10*referralTermsTestMib, blockTime(day, 10))
		}
		added = grant(now.Add(time.Minute))
		connect.AssertEqual(t, added[referrerId], ByteCount(100))
		connect.AssertEqual(t, added[refereeId], ByteCount(10))
	})
}

// A drained block's app use counts once: a hash that reappears for a block
// already applied adds nothing.
func TestReferralAppUseCountsAReplayedBlockOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		referralTermsTestConfig(t)
		now := server.NowUtc()
		today := auditDeviceDayStart(now)
		referrerId, _ := referralTermsTestNetwork(ctx, "referrer")
		refereeId, refereeClientId := referralTermsTestNetwork(ctx, "referee")
		if CreateNetworkReferral(ctx, refereeId, CreateNetworkReferralCode(ctx, referrerId).ReferralCode) == nil {
			t.Fatal("synthetic referral rejected")
		}
		usageTime := today.Add(time.Minute)
		referralTermsTestUse(ctx, refereeClientId, 6*referralTermsTestMib, usageTime)
		referralTermsTestUse(ctx, refereeClientId, 6*referralTermsTestMib, usageTime)
		connect.AssertEqual(t, referralTermsTestAppDays(ctx, refereeId), map[time.Time]ByteCount{
			today: 6 * referralTermsTestMib,
		})
	})
}

// The exclusions hold for qualified referrals made under the terms: a Seeker
// holder's referral data is doubled, on either side, and a deleted network
// and a lag.dev guest get nothing and count toward no referrer's bonus.
func TestReferralExclusionsHoldForQualifiedReferrals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := referralTermsTestConfig(t)
		config.seekerDataMultiplier = 2
		now := server.NowUtc()
		seekerReferrerId, seekerReferrerClientId := referralTermsTestNetwork(ctx, "seeker-referrer")
		seekerRefereeId, seekerRefereeClientId := referralTermsTestNetwork(ctx, "seeker-referee")
		plainRefereeId, _ := referralTermsTestNetwork(ctx, "plain-referee")
		guest := CreateLagGuestNetwork(ctx, now, now.Add(24*time.Hour))
		deletedRefereeId := server.NewId()
		for i, seeker := range []struct {
			networkId server.Id
			clientId  server.Id
		}{
			{networkId: seekerReferrerId, clientId: seekerReferrerClientId},
			{networkId: seekerRefereeId, clientId: seekerRefereeClientId},
		} {
			seekerSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: seeker.networkId, ClientId: &seeker.clientId})
			if err := MarkWalletSeekerHolder("synthetic-seeker-wallet-"+string(rune('a'+i)), seekerSession); err != nil {
				t.Fatal(err)
			}
			seekerSession.Cancel()
		}
		code := CreateNetworkReferralCode(ctx, seekerReferrerId).ReferralCode
		for _, networkId := range []server.Id{seekerRefereeId, plainRefereeId, guest.NetworkId} {
			if CreateNetworkReferral(ctx, networkId, code) == nil {
				t.Fatal("synthetic referral rejected")
			}
		}
		// the deleted referee's link and qualification remain
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_referral (network_id, referral_network_id, create_time) VALUES ($1, $2, $3)`,
				deletedRefereeId,
				seekerReferrerId,
				now,
			))
		})
		for _, networkId := range []server.Id{seekerRefereeId, plainRefereeId, guest.NetworkId, deletedRefereeId} {
			Testing_QualifyReferralReferee(ctx, networkId, now)
		}

		startTime, endTime := ReferralGrantWindow(now)
		added := AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		// two paying referrals, doubled
		connect.AssertEqual(t, added[seekerReferrerId], ByteCount(400))
		connect.AssertEqual(t, added[seekerRefereeId], ByteCount(20))
		connect.AssertEqual(t, added[plainRefereeId], ByteCount(10))
		for _, networkId := range []server.Id{guest.NetworkId, deletedRefereeId} {
			if _, granted := added[networkId]; granted {
				t.Fatalf("an excluded network was granted %d", added[networkId])
			}
		}
	})
}
