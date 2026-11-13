package model

// The disclosure ledger under the conditions connect/EXTENDER-UPDATE2.md §1.2
// names, on the real tables: concurrent admissions from replicas of their own,
// retries and changes of country or family, records newly blocked or
// removed, probation, country capacity and the epoch's end. Each release reads
// the gated fleet from the tables as the route does, so a removal is what the
// route would see. The policy's clock is a fake one aligned to an epoch; the
// blocked state reads the server clock, as in production.

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The policy clock of these tests.
type testReleaseClock struct {
	stateLock sync.Mutex
	now       time.Time
}

// An hour into the epoch that holds the server's now, so every epoch and
// probation boundary in a test is a known distance away.
func newTestReleaseClock() *testReleaseClock {
	epochMs := connect.ExtenderReleaseEpochTimeout.Milliseconds()
	epochStart := time.UnixMilli(server.NowUtc().UnixMilli() / epochMs * epochMs).UTC()
	return &testReleaseClock{now: epochStart.Add(time.Hour)}
}

func (self *testReleaseClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testReleaseClock) advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// When the clock's epoch ends.
func (self *testReleaseClock) epochEnd() time.Time {
	epochMs := connect.ExtenderReleaseEpochTimeout.Milliseconds()
	return time.UnixMilli((self.Now().UnixMilli()/epochMs + 1) * epochMs).UTC()
}

// One active gated extender with an active address per family, each reached
// by the operator's probe just now, so it is healthy for the blocked state.
func testGatedReleaseExtender(
	t testing.TB,
	ctx context.Context,
	index int,
	canaryChannel string,
	ipVersions ...int,
) (string, server.Id) {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	now := server.NowUtc()
	addresses := []*NetworkExtenderAddress{}
	for _, ipVersion := range ipVersions {
		ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 1+index%250))
		if ipVersion == 6 {
			ip = netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", 1+index))
		}
		addresses = append(addresses, &NetworkExtenderAddress{
			IpVersion:            ipVersion,
			Ip:                   ip,
			Carriers:             []string{connect.ExtenderCarrierTcp},
			ActivateTime:         now,
			LastProbeTime:        &now,
			LastProbeSuccessTime: &now,
			Active:               true,
		})
	}
	extenderId := server.NewId()
	Testing_CreateNetworkExtender(ctx, &NetworkExtender{
		ExtenderId:  extenderId,
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   publicKey,
		CreateTime:  now,
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "us",
		Active:      true,
	}, addresses)
	if err := SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierGated, false); err != nil {
		t.Fatal(err)
	}
	if canaryChannel != "" {
		if err := SetNetworkExtenderCanaryChannel(ctx, extenderId, canaryChannel, false); err != nil {
			t.Fatal(err)
		}
	}
	return hex.EncodeToString(publicKey), extenderId
}

// Releases as the route does, on a replica of its own: the gated fleet as the
// tables hold it now, a fresh ledger and blocked source, and the policy.
type testReleaseRoute struct {
	ctx      context.Context
	secret   []byte
	settings *connect.ExtenderReleaseSettings
}

func newTestReleaseRoute(ctx context.Context, clock *testReleaseClock, configure func(*connect.ExtenderReleaseSettings)) *testReleaseRoute {
	settings := connect.DefaultExtenderReleaseSettings()
	settings.Now = clock.Now
	if configure != nil {
		configure(settings)
	}
	return &testReleaseRoute{
		ctx:      ctx,
		secret:   []byte("synthetic-release-ledger-secret"),
		settings: settings,
	}
}

// The gated fleet's keys, sorted, and the key map the ledger takes.
func (self *testReleaseRoute) fleet() ([]string, map[string]server.Id) {
	extenderIds := NetworkExtenderIdsByKeyHex(GetActiveGatedNetworkExtenders(self.ctx))
	keyHexes := []string{}
	for keyHex := range extenderIds {
		keyHexes = append(keyHexes, keyHex)
	}
	slices.Sort(keyHexes)
	return keyHexes, extenderIds
}

func (self *testReleaseRoute) release(request *connect.ExtenderReleaseRequest) (*connect.ExtenderReleaseResult, error) {
	keyHexes, extenderIds := self.fleet()
	policy := connect.NewExtenderReleasePolicy(
		self.secret,
		NewNetworkExtenderReleaseLedger(self.ctx, extenderIds),
		NewNetworkExtenderBlockedSource(self.ctx, extenderIds, nil),
		self.settings,
	)
	return policy.Release(request, keyHexes)
}

// The epoch's order of the identity's partition of the current fleet.
func (self *testReleaseRoute) order(identity []byte) []string {
	keyHexes, _ := self.fleet()
	members, _, _ := connect.ExtenderPartitionMembers(self.secret, connect.ExtenderChannelGated, identity, keyHexes)
	epoch := connect.ExtenderEpoch(self.settings.Now(), self.settings.EpochTimeout)
	return connect.ExtenderPartitionOrder(self.secret, connect.ExtenderChannelGated, identity, epoch, members)
}

// The distinct extenders the ledger has released to the identity in the epoch
// of `now`, which is the disclosure the budget counts.
func testLedgerDisclosureCount(t testing.TB, ctx context.Context, identity []byte, now time.Time) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(DISTINCT extender_id) FROM network_extender_release WHERE identity = $1 AND epoch = $2`,
			identity,
			int64(connect.ExtenderEpoch(now, connect.ExtenderReleaseEpochTimeout)),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// Root cause guarded (§1.2): replicas that each read the budget before any
// writes it would together disclose more than the epoch's count. Six replicas
// ask at once for the same identity, each eligible for a different record of
// its partition, so nothing but the shared ledger stands between them and six
// disclosures. A trusted identity is disclosed exactly three, a new one exactly
// one, and every other answer is the spent epoch's.
func TestExtenderReleaseConcurrentReplicasHoldTheEpochCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestReleaseClock()
		route := newTestReleaseRoute(ctx, clock, nil)
		for i := range 6 {
			testGatedReleaseExtender(t, ctx, i, "", 4)
		}
		keyHexes, extenderIds := route.fleet()
		if len(keyHexes) != 6 || connect.ExtenderPartitionCount(len(keyHexes)) != 1 {
			t.Fatalf("fleet of %d in %d partitions, want 6 in one", len(keyHexes), connect.ExtenderPartitionCount(len(keyHexes)))
		}
		for _, c := range []struct {
			name       string
			createTime time.Time
			count      int
			spent      string
		}{
			{name: "trusted", createTime: clock.Now().Add(-60 * 24 * time.Hour), count: 3, spent: connect.ExtenderReleaseOutcomeEpochSpent},
			{name: "new", createTime: time.Time{}, count: 1, spent: connect.ExtenderReleaseOutcomeProbation},
		} {
			identity := []byte("synthetic-concurrent-identity-" + c.name)
			var arrived sync.WaitGroup
			arrived.Add(len(keyHexes))
			proceed := make(chan struct{})
			type answer struct {
				result *connect.ExtenderReleaseResult
				err    error
			}
			answers := make(chan answer, len(keyHexes))
			for i := range keyHexes {
				go func() {
					only := keyHexes[i]
					ledger := &testReleaseReplicaLedger{
						NetworkExtenderReleaseLedger: NewNetworkExtenderReleaseLedger(ctx, extenderIds),
						arrived:                      &arrived,
						proceed:                      proceed,
					}
					policy := connect.NewExtenderReleasePolicy(route.secret, ledger, nil, route.settings)
					result, err := policy.Release(&connect.ExtenderReleaseRequest{
						Identity:           identity,
						IdentityCreateTime: c.createTime,
						Vantage:            fmt.Sprintf("198.51.100.%d/32", i),
						CountryCode:        "aa",
						Eligible:           func(keyHex string) bool { return keyHex == only },
					}, keyHexes)
					answers <- answer{result: result, err: err}
				}()
			}
			arrived.Wait()
			close(proceed)
			disclosed := map[string]bool{}
			for range keyHexes {
				answer := <-answers
				if answer.err != nil {
					t.Fatalf("%s: %v", c.name, answer.err)
				}
				for _, keyHex := range answer.result.KeyHexes {
					disclosed[keyHex] = true
				}
				if len(answer.result.KeyHexes) == 0 && answer.result.Outcome != c.spent {
					t.Fatalf("%s: an empty answer was %q, want %q", c.name, answer.result.Outcome, c.spent)
				}
			}
			if len(disclosed) != c.count {
				t.Fatalf("%s: concurrent replicas disclosed %d records, want the count of %d", c.name, len(disclosed), c.count)
			}
			if count := testLedgerDisclosureCount(t, ctx, identity, clock.Now()); count != c.count {
				t.Fatalf("%s: the ledger holds %d disclosures, want %d", c.name, count, c.count)
			}
		}
	})
}

// Concurrent identities in one country never put a record past its per-country
// cap, across every record of the partition at once.
func TestExtenderReleaseConcurrentReplicasHoldTheCountryCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestReleaseClock()
		route := newTestReleaseRoute(ctx, clock, func(settings *connect.ExtenderReleaseSettings) {
			settings.MaxClientsPerExtenderPerCountry = 2
		})
		for i := range 3 {
			testGatedReleaseExtender(t, ctx, i, "", 4)
		}
		keyHexes, extenderIds := route.fleet()
		const identityCount = 8
		var arrived sync.WaitGroup
		arrived.Add(identityCount)
		proceed := make(chan struct{})
		type answer struct {
			result *connect.ExtenderReleaseResult
			err    error
		}
		answers := make(chan answer, identityCount)
		for i := range identityCount {
			go func() {
				ledger := &testReleaseReplicaLedger{
					NetworkExtenderReleaseLedger: NewNetworkExtenderReleaseLedger(ctx, extenderIds),
					arrived:                      &arrived,
					proceed:                      proceed,
				}
				policy := connect.NewExtenderReleasePolicy(route.secret, ledger, nil, route.settings)
				result, err := policy.Release(&connect.ExtenderReleaseRequest{
					Identity:           []byte(fmt.Sprintf("synthetic-country-identity-%d", i)),
					IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
					Vantage:            fmt.Sprintf("198.51.100.%d/32", i),
					CountryCode:        "aa",
				}, keyHexes)
				answers <- answer{result: result, err: err}
			}()
		}
		arrived.Wait()
		close(proceed)
		identitiesByKey := map[string]int{}
		for range identityCount {
			answer := <-answers
			if answer.err != nil {
				t.Fatal(answer.err)
			}
			for _, keyHex := range answer.result.KeyHexes {
				identitiesByKey[keyHex] += 1
			}
			if len(answer.result.KeyHexes) == 0 && answer.result.Outcome != connect.ExtenderReleaseOutcomeCountryCapacity {
				t.Fatalf("an identity left with nothing answered %q, want country_capacity", answer.result.Outcome)
			}
		}
		for _, keyHex := range keyHexes {
			if identitiesByKey[keyHex] != 2 {
				t.Fatalf("record released to %d identities in the country, want the cap of 2: %v", identitiesByKey[keyHex], identitiesByKey)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT COUNT(DISTINCT identity) FROM network_extender_release
				WHERE country_code = 'aa' GROUP BY extender_id
			`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var count int
					server.Raise(result.Scan(&count))
					if count != 2 {
						t.Fatalf("the ledger holds %d identities for a record in the country, want 2", count)
					}
				}
			})
		})
	})
}

// Root cause guarded (§1.2): retrying, or asking from another country or for
// another family, must neither refund the epoch's disclosures nor add one.
// The identity sees its three records and no other, however it asks, and a
// spent epoch says when it ends.
func TestExtenderReleaseRequestChangesNeverRefund(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestReleaseClock()
		route := newTestReleaseRoute(ctx, clock, func(settings *connect.ExtenderReleaseSettings) {
			settings.MaxClientsPerExtenderPerCountry = 1
		})
		for i := range 6 {
			testGatedReleaseExtender(t, ctx, i, "", 4)
		}
		identity := []byte("synthetic-change-identity")
		request := func(countryCode string, eligible func(string) bool) *connect.ExtenderReleaseRequest {
			return &connect.ExtenderReleaseRequest{
				Identity:           identity,
				IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
				Vantage:            "198.51.100.0/24",
				CountryCode:        countryCode,
				Eligible:           eligible,
			}
		}
		seen := map[string]bool{}
		release := func(countryCode string, eligible func(string) bool) *connect.ExtenderReleaseResult {
			t.Helper()
			result, err := route.release(request(countryCode, eligible))
			if err != nil {
				t.Fatal(err)
			}
			for _, keyHex := range result.KeyHexes {
				seen[keyHex] = true
			}
			return result
		}

		first := release("aa", nil)
		order := route.order(identity)
		if !slices.Equal(first.KeyHexes, order[:3]) || first.Outcome != connect.ExtenderReleaseOutcomeOk {
			t.Fatalf("first release = %+v, want the epoch's first three of %v", first, order)
		}
		issued := slices.Clone(first.KeyHexes)
		for range 3 {
			if retry := release("aa", nil); !slices.Equal(retry.KeyHexes, issued) {
				t.Fatalf("a retry released %v, want %v", retry.KeyHexes, issued)
			}
		}
		if moved := release("bb", nil); !slices.Equal(moved.KeyHexes, issued) {
			t.Fatalf("another country released %v, want %v", moved.KeyHexes, issued)
		}
		// another family, whose records are only those it was not given
		otherFamily := release("aa", func(keyHex string) bool { return !slices.Contains(issued, keyHex) })
		if len(otherFamily.KeyHexes) != 0 || otherFamily.Outcome != connect.ExtenderReleaseOutcomeEpochSpent {
			t.Fatalf("another family = %+v, want epoch_spent", otherFamily)
		}
		if want := clock.epochEnd().Sub(clock.Now()); otherFamily.RetryAfter != want {
			t.Fatalf("retry after %s, want the epoch's end in %s", otherFamily.RetryAfter, want)
		}
		// a country where another identity holds each of its records at the cap
		holder := &connect.ExtenderReleaseRequest{
			Identity:           []byte("synthetic-change-holder"),
			IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
			Vantage:            "203.0.113.0/24",
			CountryCode:        "cc",
			Eligible:           func(keyHex string) bool { return slices.Contains(issued, keyHex) },
		}
		if held, err := route.release(holder); err != nil || len(held.KeyHexes) != 3 {
			t.Fatalf("holder: %+v, %v", held, err)
		}
		full := release("cc", nil)
		if len(full.KeyHexes) != 0 || full.Outcome != connect.ExtenderReleaseOutcomeEpochSpent {
			t.Fatalf("a full country = %+v, want epoch_spent", full)
		}
		if len(seen) != 3 || testLedgerDisclosureCount(t, ctx, identity, clock.Now()) != 3 {
			t.Fatalf("request changes disclosed %d records (ledger %d), want 3", len(seen), testLedgerDisclosureCount(t, ctx, identity, clock.Now()))
		}

		// the next epoch renews the budget, dealt again
		clock.advance(clock.epochEnd().Sub(clock.Now()))
		next := release("aa", nil)
		if next.Outcome != connect.ExtenderReleaseOutcomeOk || len(next.KeyHexes) != 3 {
			t.Fatalf("the next epoch = %+v", next)
		}
	})
}

// Root cause guarded (§1.2): a record blocked or removed after it was disclosed
// must not hand the identity a replacement within the epoch, which would turn
// every block report or removal into a fresh draw. Each way a record leaves
// the release leaves the identity's epoch spent, and a blocked record still
// answers where it is not blocked.
func TestExtenderReleaseRemovalAndBlockingNeverRefund(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestReleaseClock()
		route := newTestReleaseRoute(ctx, clock, func(settings *connect.ExtenderReleaseSettings) {
			// one record per identity, so the removal of that one is the
			// whole question
			settings.TrustedIdentityCount = 1
		})
		for i := range 5 {
			testGatedReleaseExtender(t, ctx, i, "", 4)
		}
		removals := []struct {
			name   string
			remove func(extenderId server.Id)
		}{
			{name: "blocked in the country", remove: func(extenderId server.Id) {
				for range connect.DefaultExtenderBlockedStateSettings().ReportThreshold {
					if !RecordNetworkExtenderBlockReport(ctx, extenderId, Testing_CreateBlockReportClient(ctx, server.NewId()), "aa", server.NowUtc()) {
						t.Fatal("a report against a probed extender was not recorded")
					}
				}
			}},
			{name: "revoked", remove: func(extenderId server.Id) {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender SET active = false WHERE extender_id = $1`, extenderId))
				})
			}},
			{name: "its last address deactivated", remove: func(extenderId server.Id) {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender_address SET active = false WHERE extender_id = $1`, extenderId))
				})
			}},
			{name: "moved to the open tier", remove: func(extenderId server.Id) {
				if err := SetNetworkExtenderDirectoryTier(ctx, extenderId, connect.ExtenderDirectoryTierOpen, false); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "deleted", remove: func(extenderId server.Id) {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_extender WHERE extender_id = $1`, extenderId))
				})
			}},
		}
		for _, removal := range removals {
			// a fresh record for each removal, so the fleet never runs out
			testGatedReleaseExtender(t, ctx, 100+len(removal.name), "", 4)
			identity := []byte("synthetic-removal-" + removal.name)
			request := func(countryCode string) *connect.ExtenderReleaseRequest {
				return &connect.ExtenderReleaseRequest{
					Identity:           identity,
					IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
					Vantage:            "198.51.100.0/24",
					CountryCode:        countryCode,
				}
			}
			first, err := route.release(request("aa"))
			if err != nil || len(first.KeyHexes) != 1 {
				t.Fatalf("%s: first release %+v, %v", removal.name, first, err)
			}
			_, extenderIds := route.fleet()
			removal.remove(extenderIds[first.KeyHexes[0]])

			after, err := route.release(request("aa"))
			if err != nil {
				t.Fatal(err)
			}
			if len(after.KeyHexes) != 0 || after.Outcome != connect.ExtenderReleaseOutcomeEpochSpent || after.Probation {
				t.Fatalf("%s: after removal %+v, want epoch_spent with no replacement", removal.name, after)
			}
			if count := testLedgerDisclosureCount(t, ctx, identity, clock.Now()); count != 1 {
				t.Fatalf("%s: the ledger holds %d disclosures, want 1", removal.name, count)
			}
			if removal.name == "blocked in the country" {
				elsewhere, err := route.release(request("bb"))
				if err != nil || !slices.Equal(elsewhere.KeyHexes, first.KeyHexes) {
					t.Fatalf("a record blocked in one country: elsewhere %+v, %v, want %v", elsewhere, err, first.KeyHexes)
				}
			}
		}
	})
}

// Probation, country capacity and the epoch's end over the database ledger:
// a new identity that spent its one record waits for the sooner of its
// probation's end and the epoch's, and is dealt the rest of its count when the
// probation ends; a record at its country cap answers country_capacity there
// alone; the next epoch renews every budget.
func TestExtenderReleaseProbationCapacityAndEpochOutcomes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestReleaseClock()
		route := newTestReleaseRoute(ctx, clock, func(settings *connect.ExtenderReleaseSettings) {
			settings.MaxClientsPerExtenderPerCountry = 1
		})
		for i := range 4 {
			testGatedReleaseExtender(t, ctx, i, "", 4)
		}

		identity := []byte("synthetic-probation-identity")
		createTime := clock.Now().Add(-connect.ExtenderReleaseProbationTimeout + 24*time.Hour)
		request := func(eligible func(string) bool) *connect.ExtenderReleaseRequest {
			return &connect.ExtenderReleaseRequest{
				Identity:           identity,
				IdentityCreateTime: createTime,
				Vantage:            "198.51.100.0/24",
				CountryCode:        "aa",
				Eligible:           eligible,
			}
		}
		first, err := route.release(request(nil))
		if err != nil || len(first.KeyHexes) != 1 || !first.Probation || first.Count != 1 {
			t.Fatalf("first: %+v, %v", first, err)
		}
		given := first.KeyHexes[0]
		spent, err := route.release(request(func(keyHex string) bool { return keyHex != given }))
		if err != nil {
			t.Fatal(err)
		}
		if spent.Outcome != connect.ExtenderReleaseOutcomeProbation || len(spent.KeyHexes) != 0 || spent.RetryAfter != 24*time.Hour {
			t.Fatalf("spent in probation = %+v, want probation retrying in a day", spent)
		}
		clock.advance(spent.RetryAfter)
		trusted, err := route.release(request(nil))
		if err != nil || trusted.Probation || trusted.Outcome != connect.ExtenderReleaseOutcomeOk || len(trusted.KeyHexes) != 3 || trusted.KeyHexes[0] != given {
			t.Fatalf("after probation = %+v, %v, want its first record and two more", trusted, err)
		}

		// the cap of one: the second identity in the country waits on the first
		only := func(keyHex string) bool { return keyHex == given }
		other := &connect.ExtenderReleaseRequest{
			Identity:           []byte("synthetic-capacity-identity"),
			IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
			Vantage:            "203.0.113.0/24",
			CountryCode:        "aa",
			Eligible:           only,
		}
		full, err := route.release(other)
		if err != nil || full.Outcome != connect.ExtenderReleaseOutcomeCountryCapacity || full.RetryAfter != 0 || len(full.KeyHexes) != 0 {
			t.Fatalf("at the cap = %+v, %v", full, err)
		}
		other.CountryCode = "bb"
		if elsewhere, err := route.release(other); err != nil || !slices.Equal(elsewhere.KeyHexes, []string{given}) {
			t.Fatalf("another country = %+v, %v", elsewhere, err)
		}

		// the next epoch renews the first identity's budget
		clock.advance(clock.epochEnd().Sub(clock.Now()))
		renewed, err := route.release(request(func(keyHex string) bool { return keyHex != given }))
		if err != nil || renewed.Outcome != connect.ExtenderReleaseOutcomeOk || len(renewed.KeyHexes) == 0 || slices.Contains(renewed.KeyHexes, given) {
			t.Fatalf("the next epoch = %+v, %v", renewed, err)
		}
	})
}
