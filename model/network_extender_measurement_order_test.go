package model

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The order the weekly cap issues new measurement certificates in
// (lag.dev/PLAN.md C7, amendment 24): each goes where it least crowds the
// more crowded of its country and its network, counting the extenders that
// serve and the requests picked before it in the same read.

// A capped request of an extender in `countryCode`, activated by the network
// `networkId`, waiting since `pendingTime`.
func testMeasurementOrderCandidate(countryCode string, networkId server.Id, pendingTime time.Time) *networkExtenderMeasurementCandidate {
	return &networkExtenderMeasurementCandidate{
		extenderId:  server.NewId(),
		pendingTime: pendingTime,
		countryCode: countryCode,
		networkId:   networkId,
	}
}

// The picks as "network/country", by the networks' names.
func testMeasurementOrderLabels(
	picks []server.Id,
	candidates []*networkExtenderMeasurementCandidate,
	networkNames map[server.Id]string,
) []string {
	labels := map[server.Id]string{}
	for _, candidate := range candidates {
		labels[candidate.extenderId] = networkNames[candidate.networkId] + "/" + candidate.countryCode
	}
	pickLabels := []string{}
	for _, extenderId := range picks {
		pickLabels = append(pickLabels, labels[extenderId])
	}
	return pickLabels
}

// The queue's shape on Main: a big network A, which serves in four countries
// and asks from them and from three where nothing serves; a network B with
// none serving that asks only from the US, and many times; and two small
// networks, C and D. A joined first, then B, then C and D, so longest waiting
// first would give A and B everything. Instead:
//
//   - C and D get in at their level: D's request from za, where nothing
//     serves, first, then C's and D's where two serve;
//   - B is held by the US's three serving extenders, and gets in at 4. Each
//     pick then raises the US for the next, the k-th pick seeing the k-1
//     before it: C's second de request goes at 4, after C's first raised de;
//   - A's network serves ten, so its requests wait for the rest to reach 11,
//     and from there A and B alternate, each pick raising its own level by
//     one, and a tie going to A, whose countries are the thinner;
//   - A's picks go to its thinnest countries first -- br and in, where nothing
//     serves, before za, where D's took a place, and hk and the US last --
//     though its hk requests waited longest.
func TestPickNetworkExtenderMeasurementCandidatesSpreadsTheCap(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a := server.NewId()
	b := server.NewId()
	c := server.NewId()
	d := server.NewId()
	networkNames := map[server.Id]string{a: "A", b: "B", c: "C", d: "D"}

	countryCounts := map[string]int{"hk": 4, "us": 3, "de": 2, "jp": 2}
	networkCounts := map[server.Id]int{a: 10, c: 1}

	candidates := []*networkExtenderMeasurementCandidate{}
	ask := func(countryCode string, networkId server.Id, minutes ...int) {
		for _, minute := range minutes {
			candidates = append(candidates, testMeasurementOrderCandidate(countryCode, networkId, t0.Add(time.Duration(minute)*time.Minute)))
		}
	}
	ask("hk", a, 0, 1, 2, 3)
	ask("us", a, 4, 5)
	ask("jp", a, 6)
	ask("br", a, 7, 8)
	ask("in", a, 9, 10)
	ask("za", a, 11)
	ask("us", b, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29)
	ask("de", c, 40, 41)
	ask("jp", d, 50)
	ask("za", d, 51)
	countryCountsBefore := maps.Clone(countryCounts)
	networkCountsBefore := maps.Clone(networkCounts)

	picks := pickNetworkExtenderMeasurementCandidates(candidates, countryCounts, networkCounts, len(candidates))
	labels := testMeasurementOrderLabels(picks, candidates, networkNames)
	want := []string{
		// level 1, then 3, 3 and 4
		"D/za", "C/de", "D/jp", "B/us", "C/de",
		// B alone, at 5 to 10
		"B/us", "B/us", "B/us", "B/us", "B/us", "B/us",
		// A and B, from 11
		"A/br", "B/us", "A/in", "B/us", "A/br", "B/us",
		// A alone, its thinnest countries first
		"A/in", "A/za", "A/jp", "A/hk", "A/hk", "A/hk", "A/hk", "A/us", "A/us",
	}
	if !slices.Equal(labels, want) {
		t.Fatalf("picks = %v, want %v", labels, want)
	}

	// a read that picks fewer takes the head of the same order
	if few := pickNetworkExtenderMeasurementCandidates(candidates, countryCounts, networkCounts, 6); !slices.Equal(few, picks[:6]) {
		t.Fatalf("six picks = %v, want %v", testMeasurementOrderLabels(few, candidates, networkNames), want[:6])
	}
	// the counts passed in are left as they were
	if !maps.Equal(countryCounts, countryCountsBefore) || !maps.Equal(networkCounts, networkCountsBefore) {
		t.Fatalf("the counts changed: %v, %v", countryCounts, networkCounts)
	}
}

// The k-th pick sees the k-1 before it, in its country and in its network.
// Three requests where nothing serves would all be at level 1; once the first
// is picked, the second from its country, or from its network, is at 2, and
// the one from elsewhere goes before it although it waited less.
func TestPickNetworkExtenderMeasurementCandidatesCountsEachPick(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	sameCountry := []*networkExtenderMeasurementCandidate{
		testMeasurementOrderCandidate("za", server.NewId(), t0),
		testMeasurementOrderCandidate("za", server.NewId(), t0.Add(time.Minute)),
		testMeasurementOrderCandidate("fr", server.NewId(), t0.Add(2*time.Minute)),
	}
	picks := pickNetworkExtenderMeasurementCandidates(sameCountry, map[string]int{}, map[server.Id]int{}, 3)
	want := []server.Id{sameCountry[0].extenderId, sameCountry[2].extenderId, sameCountry[1].extenderId}
	if !slices.Equal(picks, want) {
		t.Fatalf("picks from one country = %v, want %v", picks, want)
	}

	network := server.NewId()
	sameNetwork := []*networkExtenderMeasurementCandidate{
		testMeasurementOrderCandidate("nz", network, t0),
		testMeasurementOrderCandidate("cl", network, t0.Add(time.Minute)),
		testMeasurementOrderCandidate("ar", server.NewId(), t0.Add(2*time.Minute)),
	}
	picks = pickNetworkExtenderMeasurementCandidates(sameNetwork, map[string]int{}, map[server.Id]int{}, 3)
	want = []server.Id{sameNetwork[0].extenderId, sameNetwork[2].extenderId, sameNetwork[1].extenderId}
	if !slices.Equal(picks, want) {
		t.Fatalf("picks from one network = %v, want %v", picks, want)
	}

	// no more than asked for, and no more than there are
	connect.AssertEqual(t, len(pickNetworkExtenderMeasurementCandidates(sameNetwork, map[string]int{}, map[server.Id]int{}, 0)), 0)
	connect.AssertEqual(t, len(pickNetworkExtenderMeasurementCandidates(sameNetwork, map[string]int{}, map[server.Id]int{}, 10)), 3)
	connect.AssertEqual(t, len(pickNetworkExtenderMeasurementCandidates(nil, map[string]int{}, map[server.Id]int{}, 10)), 0)
}

// At one level the lower sum of the two counts goes first, then the longest
// waiting, then the lower extender id.
func TestPickNetworkExtenderMeasurementCandidatesBreaksTies(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	crowded := server.NewId()
	countryCounts := map[string]int{"us": 2, "de": 2, "jp": 2}
	networkCounts := map[server.Id]int{crowded: 2}

	// all at level 3: one in de from a crowded network, waiting longest,
	// sums 4; the others sum 2, the two in jp waiting as long as each other
	inDe := testMeasurementOrderCandidate("de", crowded, t0)
	inJp := testMeasurementOrderCandidate("jp", server.NewId(), t0.Add(time.Minute))
	inJpToo := testMeasurementOrderCandidate("jp", server.NewId(), t0.Add(time.Minute))
	if inJpToo.extenderId.Less(inJp.extenderId) {
		inJp, inJpToo = inJpToo, inJp
	}
	inUs := testMeasurementOrderCandidate("us", server.NewId(), t0.Add(2*time.Minute))
	candidates := []*networkExtenderMeasurementCandidate{inDe, inJpToo, inUs, inJp}

	picks := pickNetworkExtenderMeasurementCandidates(candidates, countryCounts, networkCounts, len(candidates))
	want := []server.Id{
		// the lower id of the two that waited longest of those that sum 2
		inJp.extenderId,
		// jp is at 4 now; us sums 2, so it goes before de, which waited
		// longest and sums 4
		inUs.extenderId,
		inDe.extenderId,
		inJpToo.extenderId,
	}
	if !slices.Equal(picks, want) {
		t.Fatalf("picks = %v, want %v", picks, want)
	}
}

// One activated open tier extender with a v4 address, in `countryCode`,
// activated by the network `networkId`.
func testMeasurementOrderActivate(t testing.TB, ctx context.Context, index int, countryCode string, networkId server.Id) server.Id {
	t.Helper()
	sign, _ := testRecordSigner()
	activation := testExtenderActivation(
		[]byte(fmt.Sprintf("extender-public-key-measurement-order-%03d", index)),
		4,
		fmt.Sprintf("198.51.100.%d", index),
	)
	activation.CountryCode = countryCode
	activation.NetworkId = networkId
	activated := ActivateNetworkExtender(ctx, activation, sign)
	if activated == nil {
		t.Fatalf("the activation was not stored")
	}
	return activated.Extender.ExtenderId
}

// The extender's request for its name under `key`, made at `requestTime`.
func testMeasurementOrderRequest(ctx context.Context, extenderId server.Id, key string, requestTime time.Time) string {
	return SetNetworkExtenderMeasurementRequest(ctx, extenderId, []byte("csr-"+key), []byte(key), testMeasurementNames(extenderId), requestTime)
}

// The queue as read (C7, amendment 24): the requests the weekly cap does not
// hold first, however recent; then the capped ones as the cap is to issue for
// them, weighed against the extenders that hold a live certificate -- one that
// is away among them, but not one whose certificate was withdrawn or expired.
// The request of an extender that is not active is left out of either part,
// and that of an extender that is gone is read, for the pass to refuse.
// `limit` bounds the two parts together.
func TestNetworkExtenderMeasurementPendingReadsTheCapsOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour

		// serves in de, and rotates its key: a request the cap does not hold,
		// and the most recent one read
		rotating := testMeasurementOrderActivate(t, ctx, 1, "de", server.NewId())
		testMeasurementOrderRequest(ctx, rotating, "key-a", now.Add(-2*day))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(rotating, []byte("key-a"), now.Add(-2*day))), true)
		connect.AssertEqual(t, testMeasurementOrderRequest(ctx, rotating, "key-b", now.Add(-time.Hour)), NetworkExtenderMeasurementStatePending)

		// serves in hk
		hkServing := testMeasurementOrderActivate(t, ctx, 2, "hk", server.NewId())
		testMeasurementOrderRequest(ctx, hkServing, "key-hk", now.Add(-2*day))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(hkServing, []byte("key-hk"), now.Add(-2*day))), true)

		// three in the US that do not serve, of one network
		formerNetwork := server.NewId()
		// away, with a live certificate, and a rotated key's request from
		// before it went
		away := testMeasurementOrderActivate(t, ctx, 3, "us", formerNetwork)
		testMeasurementOrderRequest(ctx, away, "key-away-a", now.Add(-3*day))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(away, []byte("key-away-a"), now.Add(-3*day))), true)
		connect.AssertEqual(t, testMeasurementOrderRequest(ctx, away, "key-away-b", now.Add(-30*time.Minute)), NetworkExtenderMeasurementStatePending)
		RecordNetworkExtenderProbeResult(ctx, away, 4, false, now.Add(-20*time.Minute), 1, testRevocationSigner())
		// active, its certificate withdrawn
		withdrawn := testMeasurementOrderActivate(t, ctx, 4, "us", formerNetwork)
		testMeasurementOrderRequest(ctx, withdrawn, "key-withdrawn", now.Add(-3*day))
		withdrawnCert := testMeasurementCert(withdrawn, []byte("key-withdrawn"), now.Add(-3*day))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, withdrawnCert), true)
		connect.AssertEqual(t, WithdrawNetworkExtenderMeasurementCert(ctx, withdrawnCert.CertId, "the extender's owner opted out of measurement", now.Add(-2*day)), true)
		// active, its certificate expired
		expired := testMeasurementOrderActivate(t, ctx, 5, "us", formerNetwork)
		testMeasurementOrderRequest(ctx, expired, "key-expired", now.Add(-100*day))
		connect.AssertEqual(t, AddNetworkExtenderMeasurementCert(ctx, testMeasurementCert(expired, []byte("key-expired"), now.Add(-100*day))), true)

		// capped: one in the US, where nothing serves, and one in hk, where
		// one does
		usNetwork := server.NewId()
		inUs := testMeasurementOrderActivate(t, ctx, 6, "us", usNetwork)
		testMeasurementOrderRequest(ctx, inUs, "key-us", now.Add(-5*time.Hour))
		inHk := testMeasurementOrderActivate(t, ctx, 7, "hk", server.NewId())
		testMeasurementOrderRequest(ctx, inHk, "key-in-hk", now.Add(-4*time.Hour))
		// one in the US that waited longest, and is not active
		inactive := testMeasurementOrderActivate(t, ctx, 8, "us", usNetwork)
		testMeasurementOrderRequest(ctx, inactive, "key-inactive", now.Add(-6*time.Hour))
		RecordNetworkExtenderProbeResult(ctx, inactive, 4, false, now.Add(-20*time.Minute), 1, testRevocationSigner())
		// the request of an extender that is gone: no country, no network
		gone := server.NewId()
		testMeasurementOrderRequest(ctx, gone, "key-gone", now.Add(-3*time.Hour))

		pendingIds := func(limit int) []server.Id {
			extenderIds := []server.Id{}
			for _, target := range GetPendingNetworkExtenderMeasurements(ctx, now, limit) {
				extenderIds = append(extenderIds, target.Measurement.ExtenderId)
			}
			return extenderIds
		}

		// the US holds one certificate, the away extender's, and hk one, so
		// the gone extender's request goes first, at level 1, then the US's
		// and hk's at level 2, the US's waiting longer
		pending := GetPendingNetworkExtenderMeasurements(ctx, now, 10)
		want := []server.Id{rotating, gone, inUs, inHk}
		got := []server.Id{}
		for _, target := range pending {
			got = append(got, target.Measurement.ExtenderId)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("pending = %v, want %v", got, want)
		}
		connect.AssertEqual(t, pending[0].Extender.ExtenderId, rotating)
		connect.AssertEqual(t, pending[1].Extender == nil, true)
		connect.AssertEqual(t, pending[2].Extender.CountryCode, "us")
		connect.AssertEqual(t, len(pending[2].Addresses), 1)
		connect.AssertEqual(t, pending[3].Extender.CountryCode, "hk")

		// the limit takes the head of the same order, the uncapped first
		if got := pendingIds(2); !slices.Equal(got, want[:2]) {
			t.Fatalf("pending with a limit of 2 = %v, want %v", got, want[:2])
		}
		if got := pendingIds(1); !slices.Equal(got, want[:1]) {
			t.Fatalf("pending with a limit of 1 = %v, want %v", got, want[:1])
		}

		// both back: the away extender's request is read with the uncapped,
		// and its certificate still counts in the US, so the US's requests are
		// at 2, behind the gone extender's. The one back from inactive is the
		// US's first, its longest waiting, and puts the other at 3, behind
		// hk's
		testMeasurementOrderActivate(t, ctx, 3, "us", formerNetwork)
		testMeasurementOrderActivate(t, ctx, 8, "us", usNetwork)
		want = []server.Id{rotating, away, gone, inactive, inHk, inUs}
		if got := pendingIds(10); !slices.Equal(got, want) {
			t.Fatalf("pending once both are back = %v, want %v", got, want)
		}
	})
}
