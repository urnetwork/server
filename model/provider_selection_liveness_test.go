// FindProviders2 and the provider liveness lease (provider_liveness_lease.go):
// a candidate without a current lease leaves every draw of the request, in
// every rank mode, though its connection row still reads live; a named one
// keeps its place in every mode. Every read that cannot judge fails open: a
// connect fleet with a live handler that does not stamp the writer set, a
// stale shard, a failed read. Every provider here is connected on a live
// handler, so the request's own live read admits each of them.
package model

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One location's providers as the score export publishes them, every one
// connected on a live handler.
type livenessTestFixture struct {
	city *Location
	// verified: Quality and Speed natives
	natives []server.Id
	// ARIN non-quality: Speed natives that Quality refuses
	speedOnly []server.Id
	// a failing URL ratio: online, in neither native bucket
	onlineOnly server.Id
}

func newLivenessTestFixture(ctx context.Context, t testing.TB, name string) *livenessTestFixture {
	t.Helper()
	fixture := &livenessTestFixture{
		city: egressTestCity(ctx, name+" City", name+" Region", "United States", "us"),
	}
	for range 2 {
		provider := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
		egressTestProbed(ctx, provider, fixture.city, 0, "us")
		fixture.natives = append(fixture.natives, provider.clientId)
	}
	for range 2 {
		fixture.speedOnly = append(fixture.speedOnly, egressTestOverLine(ctx, t, fixture.city, egressTestFast).clientId)
	}
	onlineOnly := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
	egressTestHealth(ctx, onlineOnly.clientId, server.NowUtc(), 3, 1)
	fixture.onlineOnly = onlineOnly.clientId
	egressTestPasses(ctx, t)

	published := egressTestCachedScores(ctx, t, fixture.city, RankModeQuality, false)
	for _, member := range []struct {
		clientId       server.Id
		quality, speed bool
	}{
		{fixture.natives[0], true, true},
		{fixture.natives[1], true, true},
		{fixture.speedOnly[0], false, true},
		{fixture.speedOnly[1], false, true},
		{fixture.onlineOnly, false, false},
	} {
		score := published[member.clientId]
		if score == nil || !score.Online || score.PassesMinimums[RankModeQuality] != member.quality || score.PassesMinimums[RankModeSpeed] != member.speed {
			t.Fatal("fixture bucket membership changed")
		}
	}
	return fixture
}

// Every provider of the fixture.
func (self *livenessTestFixture) all() []server.Id {
	return append(slices.Clone(self.natives), self.speedOnly[0], self.speedOnly[1], self.onlineOnly)
}

// Every provider at its tier in a Quality answer: the natives, then the Speed
// natives Quality refuses at the borrowed tier, then the online member at the
// Online fallback's.
func (self *livenessTestFixture) qualityAnswer() map[server.Id]int {
	offset := egressTestBackfillOffset()
	return map[server.Id]int{
		self.natives[0]:   0,
		self.natives[1]:   0,
		self.speedOnly[0]: offset,
		self.speedOnly[1]: offset,
		self.onlineOnly:   2 * offset,
	}
}

// The same in a Speed answer, where all four Speed natives are native.
func (self *livenessTestFixture) speedAnswer() map[server.Id]int {
	return map[server.Id]int{
		self.natives[0]:   0,
		self.natives[1]:   0,
		self.speedOnly[0]: 0,
		self.speedOnly[1]: 0,
		self.onlineOnly:   2 * egressTestBackfillOffset(),
	}
}

// Leases every provider but the leaseless ones, and stamps every live handler,
// as a connect fleet that runs the writer everywhere does. Main's shards each
// hold thousands of leases; a lease of some other client in every shard keeps
// each one as current here, so no leaseless provider is alone in a stale one.
func (self *livenessTestFixture) lease(ctx context.Context, t testing.TB, leaseless ...server.Id) {
	t.Helper()
	for shard := range providerLivenessLeaseShardCount {
		if err := RefreshProviderLivenessLease(ctx, livenessTestIdInShard(shard, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for _, clientId := range self.all() {
		if !slices.Contains(leaseless, clientId) {
			if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
				t.Fatal(err)
			}
		}
	}
	livenessTestStampLiveHandlers(ctx, t)
}

// A copy of want without the clientIds.
func livenessTestWithout(want map[server.Id]int, clientIds ...server.Id) map[server.Id]int {
	without := maps.Clone(want)
	for _, clientId := range clientIds {
		delete(without, clientId)
	}
	return without
}

// Fails unless the answer is exactly `want`, each at its tier, in the
// client's order.
func assertLivenessTestAnswer(t testing.TB, label string, providers []*FindProvidersProvider, want map[server.Id]int) {
	t.Helper()
	assertEgressTestNoRepeats(t, providers)
	assertEgressTestTiersKeepOrder(t, providers)
	got := map[server.Id]int{}
	for _, provider := range providers {
		got[provider.ClientId] = provider.Tier
	}
	if !maps.Equal(got, want) {
		missing, extra := 0, 0
		for clientId := range want {
			if _, ok := got[clientId]; !ok {
				missing += 1
			}
		}
		for clientId := range got {
			if _, ok := want[clientId]; !ok {
				extra += 1
			}
		}
		t.Fatalf("%s: answered %d providers, want %d: %d missing, %d not wanted, or a tier differs", label, len(got), len(want), missing, extra)
	}
}

// Fails unless the answer holds exactly the clientIds, in any order.
func assertLivenessTestSet(t testing.TB, label string, providers []*FindProvidersProvider, clientIds ...server.Id) {
	t.Helper()
	assertEgressTestNoRepeats(t, providers)
	if !slices.Equal(onlineTestSorted(egressTestIds(providers)...), onlineTestSorted(clientIds...)) {
		t.Fatalf("%s: answered %d providers, want %d", label, len(providers), len(clientIds))
	}
}

// Runs one test's setup the way every test here needs it.
func livenessTestSetup(t testing.TB) {
	t.Helper()
	enableSubscriberQualityPolicy(t)
	onlineTestFreshSubscriberCache(t)
	livenessTestFreshHandlers(t)
}

// A Quality request borrows a Speed native when its natives fall short. One
// whose connect announcement stopped refreshing its lease, its row still
// connected on a live handler, was borrowed at the borrowed tier before; it is
// now in no draw of the request.
func TestFindProviders2LeaselessSpeedNativeIsNotBorrowedByQuality(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Leaseless Borrowed")
		fixture.lease(ctx, t, fixture.speedOnly[1])
		offlineTestForEachReader(t, func(nativeReader bool) {
			offlineBefore := livenessTestVerdictCount(t, RankModeQuality, providerLivenessOffline)
			quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
			assertLivenessTestAnswer(t, fmt.Sprintf("reader=%t Quality", nativeReader), quality, livenessTestWithout(fixture.qualityAnswer(), fixture.speedOnly[1]))
			if livenessTestVerdictCount(t, RankModeQuality, providerLivenessOffline) <= offlineBefore {
				t.Fatalf("reader=%t: the leaseless provider was not counted offline", nativeReader)
			}
		})
	})
}

// With its natives and borrowed Speed natives short, a Quality request lends
// online members through the Online fallback. A member without a lease was
// lent there before; it is no longer.
func TestFindProviders2LeaselessOnlineMemberIsNotInTheOnlineFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Leaseless Fallback")
		fixture.lease(ctx, t, fixture.onlineOnly)
		offlineTestForEachReader(t, func(nativeReader bool) {
			quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
			assertLivenessTestAnswer(t, fmt.Sprintf("reader=%t Quality", nativeReader), quality, livenessTestWithout(fixture.qualityAnswer(), fixture.onlineOnly))
		})
	})
}

// Online requests, and forced Speed requests, read no live fact of their own,
// and answered every provider in the pool. A Speed native and an online member
// without a lease are in neither answer now.
func TestFindProviders2LeaselessProvidersLeaveOnlineAndForcedAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Leaseless Online")
		fixture.lease(ctx, t, fixture.natives[1], fixture.onlineOnly)
		leased := []server.Id{fixture.natives[0], fixture.speedOnly[0], fixture.speedOnly[1]}
		offlineTestForEachReader(t, func(nativeReader bool) {
			online := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeOnline, 10, false, server.NewId())
			assertLivenessTestSet(t, fmt.Sprintf("reader=%t online", nativeReader), online, leased...)
			forcedSpeed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, true, server.NewId())
			assertLivenessTestSet(t, fmt.Sprintf("reader=%t forced Speed", nativeReader), forcedSpeed, leased...)
		})
	})
}

// A phantom: the provider's connection row is connected on a live handler, so
// the request's own live read admits it as a Quality native, but its
// announcement no longer refreshes its lease. Quality answered it at tier 0;
// it is not answered now.
func TestFindProviders2PhantomQualityNativeIsNotAnswered(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Phantom Quality")
		fixture.lease(ctx, t, fixture.natives[1])
		if subscriberQualityTestRefused(ctx, t, fixture.natives[1]) {
			t.Fatal("fixture is wrong: the live read refuses the phantom")
		}
		offlineTestForEachReader(t, func(nativeReader bool) {
			quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
			assertLivenessTestAnswer(t, fmt.Sprintf("reader=%t Quality", nativeReader), quality, livenessTestWithout(fixture.qualityAnswer(), fixture.natives[1]))
		})
	})
}

// A short Speed request draws its natives, then reads the Quality page, whose
// live read revokes a drawn native it finds without a live connection. That
// later page cannot revoke a phantom, whose row reads live: before this change
// the phantom stayed in the answer. Its lease is read before any draw now, so
// it is never drawn, and nothing is left to revoke.
func TestFindProviders2PhantomSpeedNativeIsNotKeptByTheLaterPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Phantom Speed")
		fixture.lease(ctx, t, fixture.natives[1])
		offlineTestForEachReader(t, func(nativeReader bool) {
			speed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, false, server.NewId())
			assertLivenessTestAnswer(t, fmt.Sprintf("reader=%t Speed", nativeReader), speed, livenessTestWithout(fixture.speedAnswer(), fixture.natives[1]))
		})
	})
}

// The lease lapses within the window of a disconnect, and in that window the
// later page's live read still revokes a drawn Speed native it finds without a
// live connection: the lease adds refusals, and never undoes one.
func TestFindProviders2CurrentLeaseKeepsTheLaterPageRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Leased Offline")
		for _, clientId := range append(slices.Clone(fixture.natives), fixture.offline, fixture.speedOnly[0], fixture.speedOnly[1], fixture.onlineOnly) {
			if err := RefreshProviderLivenessLease(ctx, clientId); err != nil {
				t.Fatal(err)
			}
		}
		livenessTestStampLiveHandlers(ctx, t)
		offlineTestForEachReader(t, func(nativeReader bool) {
			speed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, false, server.NewId())
			assertOfflineTestAnswer(t, fmt.Sprintf("reader=%t Speed", nativeReader), speed, fixture.offline, fixture.speedAnswer())
		})
	})
}

// A named provider keeps its place whatever its lease, in every mode, by a
// request that names only it and by a discovery request that names it: a
// redial that names its exit gets one try, and a wrong offline verdict there
// changes the user's IP. In the same fleet, enforcing, a leaseless provider
// the discovery request did not name is left out; and a mixed fleet and an
// unreadable shard answer the named provider all the same.
func TestFindProviders2LeaselessNamedProviderIsAnsweredInEveryMode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Leaseless Named")
		leaseless := fixture.natives[1]
		unnamedLeaseless := fixture.speedOnly[1]
		fixture.lease(ctx, t, leaseless, unnamedLeaseless)
		named := []*ProviderSpec{{ClientId: &leaseless}}
		answersNamed := func(label string) {
			t.Helper()
			livenessTestForgetCoverage()
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
				if providers := egressTestFind(ctx, t, named, rankMode, 10, false, server.NewId()); !slices.Equal(egressTestIds(providers), []server.Id{leaseless}) {
					t.Fatalf("%s, %s: a request naming only the leaseless provider answered %d providers, want it", label, rankMode, len(providers))
				}
			}
		}

		answersNamed("enforcing")
		offlineTestForEachReader(t, func(nativeReader bool) {
			livenessTestForgetCoverage()
			mixed := egressTestFind(ctx, t, append(slices.Clone(named), egressTestLocationSpec(fixture.city)...), RankModeSpeed, 10, false, server.NewId())
			assertLivenessTestAnswer(t, fmt.Sprintf("reader=%t named and discovered Speed", nativeReader), mixed, livenessTestWithout(fixture.speedAnswer(), unnamedLeaseless))
			if len(mixed) == 0 || mixed[0].ClientId != leaseless {
				t.Fatalf("reader=%t: a Speed discovery request lost the provider it names", nativeReader)
			}
		})

		unstamped := CreateNetworkClientHandler(ctx)
		answersNamed("a mixed fleet")
		if err := PublishProviderLivenessWriter(ctx, unstamped); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			key := providerLivenessLeaseKey(providerLivenessLeaseShard(leaseless))
			if err := r.Del(ctx, key).Err(); err != nil {
				t.Fatal(err)
			}
			if err := r.Set(ctx, key, "test: not a sorted set", time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		answersNamed("an unreadable shard")
	})
}

// Every case where the lease cannot judge admits: no lease or stamp anywhere,
// as before connect runs the writer; a live connect handler without a stamp,
// a mixed fleet; and the leaseless providers' shards stale or unreadable. Each
// answers every provider, as before this change, and counts the candidates
// unknown. Between them, the same fleet with every handler stamped enforces.
func TestFindProviders2LeaseReadFailsOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Fail Open")
		leaseless := []server.Id{fixture.speedOnly[1], fixture.onlineOnly}
		find := func(label string) []*FindProvidersProvider {
			t.Helper()
			// a new handler or stamp is seen once the cached reads age out
			livenessTestForgetCoverage()
			return egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
		}
		admits := func(label string) {
			t.Helper()
			unknownBefore := livenessTestVerdictCount(t, RankModeQuality, providerLivenessUnknown)
			assertLivenessTestAnswer(t, label, find(label), fixture.qualityAnswer())
			if livenessTestVerdictCount(t, RankModeQuality, providerLivenessUnknown) <= unknownBefore {
				t.Fatalf("%s: no candidate was counted unknown", label)
			}
		}

		admits("no leases and no stamps")

		// a mixed fleet: every lease written and every shard current, but one
		// live handler unstamped
		fixture.lease(ctx, t, leaseless...)
		unstamped := CreateNetworkClientHandler(ctx)
		admits("a live handler without a stamp")
		if err := PublishProviderLivenessWriter(ctx, unstamped); err != nil {
			t.Fatal(err)
		}
		assertLivenessTestAnswer(t, "every live handler stamped", find("every live handler stamped"), livenessTestWithout(fixture.qualityAnswer(), leaseless...))

		// stale shards: the leaseless providers' shards hold only leases older
		// than the window
		leaselessShards := map[int]bool{}
		for _, clientId := range leaseless {
			leaselessShards[providerLivenessLeaseShard(clientId)] = true
		}
		nodeMillis := livenessTestNodeTime(ctx, t).UnixMilli()
		server.Redis(ctx, func(r server.RedisClient) {
			for shard := range leaselessShards {
				if err := r.Del(ctx, providerLivenessLeaseKey(shard)).Err(); err != nil {
					t.Fatal(err)
				}
			}
		})
		for _, clientId := range fixture.all() {
			if leaselessShards[providerLivenessLeaseShard(clientId)] {
				livenessTestScore(ctx, t, providerLivenessLeaseKey(providerLivenessLeaseShard(clientId)), map[string]int64{
					clientId.String(): nodeMillis - providerLivenessLeaseWindow.Milliseconds() - 1000,
				})
			}
		}
		admits("stale shards")

		// unreadable shards
		errorsBefore := livenessTestMetric(t, findProviders2LivenessReadErrors)
		server.Redis(ctx, func(r server.RedisClient) {
			for shard := range leaselessShards {
				if err := r.Del(ctx, providerLivenessLeaseKey(shard)).Err(); err != nil {
					t.Fatal(err)
				}
				if err := r.Set(ctx, providerLivenessLeaseKey(shard), "test: not a sorted set", time.Hour).Err(); err != nil {
					t.Fatal(err)
				}
			}
		})
		admits("unreadable shards")
		if livenessTestMetric(t, findProviders2LivenessReadErrors) <= errorsBefore {
			t.Fatal("the failed shard reads were not counted")
		}
	})
}

// The window to the millisecond: every shard's newest lease is the reference,
// a lease exactly the window older is borrowed, one a millisecond older is
// not. The reference is earlier than both the Redis node's clock and this
// process's, so neither clock moves it.
func TestFindProviders2LeaseWindowBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		livenessTestSetup(t)
		ctx := t.Context()
		fixture := newLivenessTestFixture(ctx, t, "Lease Window")
		reference := min(livenessTestNodeTime(ctx, t).UnixMilli(), server.NowUtc().UnixMilli()) - 5000
		window := providerLivenessLeaseWindow.Milliseconds()
		for shard := range providerLivenessLeaseShardCount {
			livenessTestScore(ctx, t, providerLivenessLeaseKey(shard), map[string]int64{"test-anchor": reference})
		}
		for _, clientId := range fixture.all() {
			livenessTestScore(ctx, t, providerLivenessLeaseKey(providerLivenessLeaseShard(clientId)), map[string]int64{
				clientId.String(): reference,
			})
		}
		atWindow, pastWindow := fixture.speedOnly[0], fixture.speedOnly[1]
		livenessTestScore(ctx, t, providerLivenessLeaseKey(providerLivenessLeaseShard(atWindow)), map[string]int64{
			atWindow.String(): reference - window,
		})
		livenessTestScore(ctx, t, providerLivenessLeaseKey(providerLivenessLeaseShard(pastWindow)), map[string]int64{
			pastWindow.String(): reference - window - 1,
		})
		livenessTestStampLiveHandlers(ctx, t)
		offlineTestForEachReader(t, func(nativeReader bool) {
			quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
			assertLivenessTestAnswer(t, fmt.Sprintf("reader=%t Quality", nativeReader), quality, livenessTestWithout(fixture.qualityAnswer(), pastWindow))
		})
	})
}
