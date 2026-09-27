package work

import (
	"context"
	"fmt"
	"hash/fnv"
	"iter"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/geo"
	"github.com/urnetwork/server/v2026/geo/solve"
	"github.com/urnetwork/server/v2026/model"
)

// The derive phase at scale (connect/GEOMAP.md §5.3 "Scale", §5.8 item 2): the
// day read over parallel cursors and aggregated as it streams, with memory
// that follows the pairs and not the rows, and terms that do not depend on
// the cursor count; the planner's projection; and the peers a node is
// expected to measure (D26). The whole job over a generated day at a tenth of
// the target is in derive_location_scale_target_test.go, behind
// GEOMAP_SCALE=1.

// A generated day of co-signed direct pings, produced rather than stored, so a
// day of a million rows costs no memory of its own. The extenders ping in
// rounds -- each pinger's pairs once a round, pinger by pinger -- so the rows
// of every pair interleave with every other pair's as a day's do, and a
// pinger takes part in as many rounds as its pairs have samples; the
// providers probe in the day's last round, so the last hours of the day hold
// every pair. A round trip is its pair's base plus the round's noise, both
// hashed from the seed, so the day is the same rows however often it is
// walked.
type deriveTestScaleDay struct {
	extenderIds []server.Id
	providerIds []server.Id
	// the extenders each extender pings, and each provider probes
	extenderPeerCount int
	providerPeerCount int
	// the samples each extender pinger takes of each of its pairs, by pinger
	extenderSamples []int
	seed            uint64
	// a node's ping to itself and a ping of a kind that is no node, in the
	// first round: rows the cursors read and drop
	withDroppedRows bool
}

// A day of `extenderCount` extenders pinging `extenderPeerCount` peers each,
// `samples(pinger)` times a pair, and `providerCount` providers probing
// `providerPeerCount` extenders once.
func newDeriveTestScaleDay(
	extenderCount int,
	extenderPeerCount int,
	samples func(pinger int) int,
	providerCount int,
	providerPeerCount int,
	seed uint64,
) *deriveTestScaleDay {
	day := &deriveTestScaleDay{
		extenderPeerCount: extenderPeerCount,
		providerPeerCount: providerPeerCount,
		seed:              seed,
	}
	for i := range extenderCount {
		day.extenderIds = append(day.extenderIds, server.NewId())
		day.extenderSamples = append(day.extenderSamples, samples(i))
	}
	for range providerCount {
		day.providerIds = append(day.providerIds, server.NewId())
	}
	return day
}

// The rows the day holds.
func (self *deriveTestScaleDay) rowCount() int {
	rows := len(self.providerIds) * self.providerPeerCount
	for _, samples := range self.extenderSamples {
		rows += samples * self.extenderPeerCount
	}
	if self.withDroppedRows {
		rows += 2
	}
	return rows
}

// The ordered pairs the day's co-signed pings make.
func (self *deriveTestScaleDay) pairCount() int {
	return len(self.extenderIds)*self.extenderPeerCount + len(self.providerIds)*self.providerPeerCount
}

// The day's rows in the order they were created, `rounds` rounds of them, or
// all of them for a negative count.
func (self *deriveTestScaleDay) rows(rounds int) iter.Seq[*model.NetworkPingTerm] {
	return func(yield func(*model.NetworkPingTerm) bool) {
		// splitmix64 over the seed and the coordinates
		hash := func(values ...uint64) uint64 {
			h := self.seed
			for _, value := range values {
				h += value + 0x9e3779b97f4a7c15
				h = (h ^ (h >> 30)) * 0xbf58476d1ce4e5b9
				h = (h ^ (h >> 27)) * 0x94d049bb133111eb
				h ^= h >> 31
			}
			return h
		}
		dayRounds := 0
		for _, samples := range self.extenderSamples {
			dayRounds = max(dayRounds, samples)
		}
		maxRounds := dayRounds
		if 0 <= rounds {
			maxRounds = min(maxRounds, rounds)
		}
		extenderCount := len(self.extenderIds)
		for round := 0; round < maxRounds; round += 1 {
			if round == 0 && self.withDroppedRows {
				if !yield(&model.NetworkPingTerm{
					PingerKind:       model.NetworkPingPingerKindExtender,
					PingerId:         self.extenderIds[0],
					TargetExtenderId: self.extenderIds[0],
					RttMs:            1,
				}) {
					return
				}
				if !yield(&model.NetworkPingTerm{
					PingerKind:       3,
					PingerId:         server.NewId(),
					TargetExtenderId: self.extenderIds[0],
					RttMs:            1,
				}) {
					return
				}
			}
			for i, pingerId := range self.extenderIds {
				if self.extenderSamples[i] <= round {
					continue
				}
				for j := 1; j <= self.extenderPeerCount; j += 1 {
					target := (i + j) % extenderCount
					base := 5 + hash(1, uint64(i), uint64(target))%150
					noise := hash(2, uint64(i), uint64(target), uint64(round)) % 7
					if !yield(&model.NetworkPingTerm{
						PingerKind:       model.NetworkPingPingerKindExtender,
						PingerId:         pingerId,
						TargetExtenderId: self.extenderIds[target],
						RttMs:            int(base + noise),
					}) {
						return
					}
				}
			}
			if round != dayRounds-1 {
				continue
			}
			for i, pingerId := range self.providerIds {
				// consecutive extenders from a hashed start: distinct pairs
				start := int(hash(3, uint64(i)) % uint64(extenderCount))
				for j := range self.providerPeerCount {
					if !yield(&model.NetworkPingTerm{
						PingerKind:       model.NetworkPingPingerKindProvider,
						PingerId:         pingerId,
						TargetExtenderId: self.extenderIds[(start+j)%extenderCount],
						RttMs:            int(5 + hash(4, uint64(i), uint64(j))%150),
					}) {
						return
					}
				}
			}
		}
	}
}

// The cursor a pinger's rows go to when the day is split into `hashRanges`
// in Go: the production split hashes in the database (uuid_hash_extended),
// this one with FNV-1a, over the same cut of the unsigned space
// (model.NetworkPingHashRanges). Either keeps every pair whole, since a pair's
// pinger alone decides its range.
func deriveTestCursorIndex(pingerId server.Id, hashRanges []model.NetworkPingHashRange) int {
	h := fnv.New64a()
	h.Write(pingerId[:])
	value := int64(h.Sum64() ^ (1 << 63))
	for i, hashRange := range hashRanges {
		if hashRange.Lo <= value && value <= hashRange.Hi {
			return i
		}
	}
	panic(fmt.Errorf("hash %d is in no range", value))
}

// The day read over `cursorCount` cursors split in Go, each cursor reading its
// pairs in the day's order, and merged as the job merges them.
func deriveTestIngestInGo(settings *solve.Settings, day *deriveTestScaleDay, cursorCount int) *deriveInputs {
	hashRanges := model.NetworkPingHashRanges(cursorCount)
	nodeIds := newDeriveNodeIdTable(4 * len(hashRanges))
	cursorInputs := make([]*deriveCursorInputs, len(hashRanges))
	for i := range cursorInputs {
		cursorInputs[i] = newDeriveCursorInputs(settings, nodeIds)
	}
	for row := range day.rows(-1) {
		cursorInputs[deriveTestCursorIndex(row.PingerId, hashRanges)].addTerm(row)
	}
	for _, cursor := range cursorInputs {
		cursor.finish()
	}
	return mergeDeriveInputs(cursorInputs)
}

// Fails the test at the first way two inputs differ: every term bit for bit,
// every node, every party's counts, and every counter.
func requireDeriveTestInputsEqual(t testing.TB, name string, got *deriveInputs, want *deriveInputs) {
	t.Helper()
	if len(got.terms) != len(want.terms) {
		t.Fatalf("%s: %d terms, want %d", name, len(got.terms), len(want.terms))
	}
	for i := range want.terms {
		if got.terms[i] != want.terms[i] {
			t.Fatalf("%s: term %d is %+v, want %+v", name, i, got.terms[i], want.terms[i])
		}
	}
	if len(got.nodeKeys) != len(want.nodeKeys) {
		t.Fatalf("%s: %d nodes, want %d", name, len(got.nodeKeys), len(want.nodeKeys))
	}
	for nodeId, key := range want.nodeKeys {
		if got.nodeKeys[nodeId] != key {
			t.Fatalf("%s: node %s is %+v, want %+v", name, nodeId, got.nodeKeys[nodeId], key)
		}
	}
	if len(got.refusals) != len(want.refusals) {
		t.Fatalf("%s: counts of %d parties, want %d", name, len(got.refusals), len(want.refusals))
	}
	for nodeId, counts := range want.refusals {
		if gotCounts, ok := got.refusals[nodeId]; !ok || *gotCounts != *counts {
			t.Fatalf("%s: counts of %s are %+v, want %+v", name, nodeId, gotCounts, *counts)
		}
	}
	connect.AssertEqual(t, got.cosignedPings, want.cosignedPings)
	connect.AssertEqual(t, got.droppedPings, want.droppedPings)
	connect.AssertEqual(t, got.refusalCount, want.refusalCount)
	connect.AssertEqual(t, got.ignoredRefusals, want.ignoredRefusals)
}

// The merged inputs are the same for any number of cursors, bit for bit: at 1,
// 4 and 16 cursors the terms, the nodes and every party's counts are
// identical. The split keeps each pair whole -- a pair's pinger decides its
// cursor, as the production split by pinger-id hash does -- and each cursor
// reads a pair's rows in the day's order, which is what makes the merge
// exact: each pair's aggregate, reservoir and all, is the one a single cursor
// makes. Half the pairs have more samples than the reservoir keeps, so which
// samples the reservoir kept is part of what is compared. Pure.
func TestDeriveInputsAreTheSameForAnyCursorCount(t *testing.T) {
	settings := solve.DefaultSettings()
	day := newDeriveTestScaleDay(200, 8, func(pinger int) int {
		if pinger%2 == 0 {
			return 40
		}
		return 8
	}, 50, 16, 1)
	day.withDroppedRows = true

	want := deriveTestIngestInGo(settings, day, 1)
	connect.AssertEqual(t, want.cosignedPings, day.rowCount()-1)
	connect.AssertEqual(t, want.droppedPings, 2)
	beyondReservoir := 0
	for _, term := range want.terms {
		if settings.PairSampleReservoir < term.Samples {
			beyondReservoir += 1
		}
	}
	connect.AssertEqual(t, beyondReservoir, 100*8)
	for _, cursorCount := range []int{4, 16} {
		got := deriveTestIngestInGo(settings, day, cursorCount)
		requireDeriveTestInputsEqual(t, fmt.Sprintf("%d cursors", cursorCount), got, want)
	}
	t.Logf("%d rows, %d terms (%d beyond the reservoir of %d), %d nodes: identical at 1, 4 and 16 cursors", day.rowCount(), len(want.terms), beyondReservoir, settings.PairSampleReservoir, len(want.nodeKeys))
}

// A split that does not keep pairs whole cannot be merged: two cursors'
// aggregates of one pair are not one aggregate, and the merge refuses them
// rather than solving on two terms for one pair. Pure.
func TestMergeDeriveInputsRefusesAPairReadByTwoCursors(t *testing.T) {
	settings := solve.DefaultSettings()
	nodeIds := newDeriveNodeIdTable(1)
	cursorInputs := []*deriveCursorInputs{
		newDeriveCursorInputs(settings, nodeIds),
		newDeriveCursorInputs(settings, nodeIds),
	}
	pingerId := server.NewId()
	targetId := server.NewId()
	// the pair's rows alternate between the two cursors
	for i := range 4 {
		cursorInputs[i%2].addTerm(&model.NetworkPingTerm{
			PingerKind:       model.NetworkPingPingerKindExtender,
			PingerId:         pingerId,
			TargetExtenderId: targetId,
			RttMs:            10 + i,
		})
	}
	for _, cursor := range cursorInputs {
		cursor.finish()
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "read by two cursors") {
			t.Fatalf("the merge of a split pair recovered %v", r)
		}
	}()
	mergeDeriveInputs(cursorInputs)
}

// The live heap a function's result holds, as the collector sees it: the heap
// after a collection with the result alive, less the heap after one before.
func deriveTestLiveBytes[T any](build func() T) (T, int64) {
	stats := &runtime.MemStats{}
	runtime.GC()
	runtime.ReadMemStats(stats)
	before := int64(stats.HeapAlloc)
	result := build()
	runtime.GC()
	runtime.ReadMemStats(stats)
	after := int64(stats.HeapAlloc)
	runtime.KeepAlive(result)
	return result, after - before
}

// A day of a million rows streamed through sixteen cursors holds memory that
// follows its pairs, not its rows: the live heap of the cursors' aggregation,
// measured with every row read and before the terms are made, is the same for
// the million rows as for their first quarter over the same pairs, and a few
// hundred bytes a pair; it is at most what the rows would take as round trips
// alone. Pure.
func TestDeriveCursorMemoryFollowsPairsNotRows(t *testing.T) {
	settings := solve.DefaultSettings()
	// 625 extenders pinging 25 peers 64 times: 15 625 pairs, a million rows
	day := newDeriveTestScaleDay(625, 25, func(int) int { return 64 }, 0, 0, 2)
	connect.AssertEqual(t, day.rowCount(), 1000000)
	hashRanges := model.NetworkPingHashRanges(16)

	measure := func(rounds int) (rows int, liveBytes int64) {
		_, liveBytes = deriveTestLiveBytes(func() []*deriveCursorInputs {
			nodeIds := newDeriveNodeIdTable(4 * len(hashRanges))
			cursorInputs := make([]*deriveCursorInputs, len(hashRanges))
			for i := range cursorInputs {
				cursorInputs[i] = newDeriveCursorInputs(settings, nodeIds)
			}
			for row := range day.rows(rounds) {
				cursorInputs[deriveTestCursorIndex(row.PingerId, hashRanges)].addTerm(row)
				rows += 1
			}
			return cursorInputs
		})
		return rows, liveBytes
	}
	allRows, allBytes := measure(-1)
	quarterRows, quarterBytes := measure(16)
	pairs := int64(day.pairCount())
	t.Logf(
		"live aggregation state over 16 cursors: %d rows %d bytes (%.0f a pair, %.1f a row); %d rows %d bytes (%.0f a pair); %d pairs",
		allRows, allBytes, float64(allBytes)/float64(pairs), float64(allBytes)/float64(allRows),
		quarterRows, quarterBytes, float64(quarterBytes)/float64(pairs),
		pairs,
	)
	connect.AssertEqual(t, allRows, 1000000)
	connect.AssertEqual(t, quarterRows, 250000)
	// four times the rows, the same pairs, the same memory
	if !(float64(allBytes) < 1.25*float64(quarterBytes)) {
		t.Fatalf("a million rows hold %d bytes against %d for a quarter of them: memory follows the rows", allBytes, quarterBytes)
	}
	if !(allBytes < 1024*pairs) {
		t.Fatalf("%d bytes for %d pairs is %.0f a pair", allBytes, pairs, float64(allBytes)/float64(pairs))
	}
	// the round trips alone, one float64 a row, would be 8 MB
	if !(allBytes < 8*int64(allRows)) {
		t.Fatalf("%d bytes for %d rows", allBytes, allRows)
	}
}

// Stores `rowCount` generated co-signed rows with COPY, their create times
// spread evenly over the `span` before `now` in the order generated, so the
// cursors' order (create_time, ping_id) is the generator's. The ingest's
// one-row inserts would take minutes for a million rows; the rows are the
// same. Returns the rows stored.
func deriveTestCopyPings(t testing.TB, ctx context.Context, rows iter.Seq[*model.NetworkPingTerm], rowCount int, now time.Time, span time.Duration) int64 {
	t.Helper()
	next, stop := iter.Pull(rows)
	defer stop()
	index := 0
	var copied int64
	server.Db(ctx, func(conn server.PgConn) {
		var err error
		copied, err = conn.CopyFrom(
			ctx,
			pgx.Identifier{"network_ping"},
			[]string{
				"ping_id",
				"pinger_kind",
				"pinger_id",
				"target_extender_id",
				"probe_nonce",
				"rtt_ms",
				"probe_time",
				"cosign",
				"cosign_reason",
				"pinger_signature",
				"cosignature",
				"hop_count",
				"create_time",
			},
			pgx.CopyFromFunc(func() ([]any, error) {
				row, ok := next()
				if !ok {
					return nil, nil
				}
				// a share of the span, in floating point: the span in
				// nanoseconds times a row index overflows int64 past about
				// a hundred thousand rows
				createTime := now.Add(-span + time.Duration(float64(span)*(float64(index)/float64(rowCount)))).UTC()
				index += 1
				return []any{
					server.NewId(),
					row.PingerKind,
					row.PingerId,
					row.TargetExtenderId,
					server.NewId().Bytes(),
					row.RttMs,
					createTime,
					model.NetworkPingCosignCosigned,
					0,
					[]byte("pinger-signature"),
					[]byte("cosignature"),
					0,
					createTime,
				}, nil
			}),
		)
		server.Raise(err)
	})
	return copied
}

// A day of a million rows in the database, read over 1, 4 and 16 cursors by
// the job's own ingest: the inputs are identical at every cursor count. The
// production split by pinger-id hash keeps each pair whole and each cursor
// reads a pair in the day's order, which is what makes the merge exact, and
// every pair has more samples than the reservoir keeps, so the reservoir is
// part of what is compared. The cursors' live aggregation state follows the
// pairs, not the rows: the whole day holds what its last quarter does. The
// time each cursor count takes is logged, for the scaling.
func TestDeriveIngestOfAMillionRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := solve.DefaultSettings()
		now := server.NowUtc()
		minCreateTime := now.Add(-controller.DefaultExtenderPingReportSettings().Retention)
		// 625 extenders pinging 25 peers 64 times, and 400 providers probing
		// 16 extenders once
		day := newDeriveTestScaleDay(625, 25, func(int) int { return 64 }, 400, 16, 3)
		copyStart := time.Now()
		connect.AssertEqual(t, deriveTestCopyPings(t, ctx, day.rows(-1), day.rowCount(), now, 23*time.Hour), int64(day.rowCount()))
		t.Logf("copied %d rows in %.1fs", day.rowCount(), time.Since(copyStart).Seconds())

		var want *deriveInputs
		for _, cursorCount := range []int{1, 4, 16} {
			jobSettings := DefaultDeriveLocationsSettings()
			jobSettings.DeriveReadCursors = cursorCount
			start := time.Now()
			inputs := ingestDeriveInputs(ctx, settings, jobSettings, minCreateTime)
			seconds := time.Since(start).Seconds()
			t.Logf("%2d cursors: %d rows into %d terms in %.2fs (%.0f rows a second)", cursorCount, inputs.cosignedPings, len(inputs.terms), seconds, float64(inputs.cosignedPings)/seconds)
			if want == nil {
				want = inputs
				connect.AssertEqual(t, inputs.cosignedPings, day.rowCount())
				connect.AssertEqual(t, len(inputs.terms), day.pairCount())
				for _, term := range inputs.terms {
					if strings.HasPrefix(term.Source, deriveExtenderNodeIdPrefix) && !(settings.PairSampleReservoir < term.Samples) {
						t.Fatalf("extender pair %s to %s has %d samples, no more than the reservoir", term.Source, term.Target, term.Samples)
					}
				}
				continue
			}
			requireDeriveTestInputsEqual(t, fmt.Sprintf("%d cursors", cursorCount), inputs, want)
		}

		// the cursors' state with every row read, before the terms are made
		measure := func(minCreateTime time.Time) (rows int, liveBytes int64) {
			hashRanges := model.NetworkPingHashRanges(16)
			_, liveBytes = deriveTestLiveBytes(func() []*deriveCursorInputs {
				nodeIds := newDeriveNodeIdTable(4 * len(hashRanges))
				cursorInputs := make([]*deriveCursorInputs, len(hashRanges))
				for i, hashRange := range hashRanges {
					cursorInputs[i] = newDeriveCursorInputs(settings, nodeIds)
					model.GetNetworkPingTermRange(ctx, minCreateTime, hashRange, cursorInputs[i].addTerm)
					rows += cursorInputs[i].cosignedPings
				}
				return cursorInputs
			})
			return rows, liveBytes
		}
		allRows, allBytes := measure(minCreateTime)
		// the last quarter of the rounds, the providers' round with them:
		// every pair, a quarter of the rows
		quarterRows, quarterBytes := measure(now.Add(-23 * time.Hour / 4))
		pairs := int64(day.pairCount())
		t.Logf(
			"live aggregation state over 16 cursors: %d rows %d bytes (%.0f a pair, %.1f a row); %d rows %d bytes (%.0f a pair); %d pairs",
			allRows, allBytes, float64(allBytes)/float64(pairs), float64(allBytes)/float64(allRows),
			quarterRows, quarterBytes, float64(quarterBytes)/float64(pairs),
			pairs,
		)
		if !(float64(allBytes) < 1.25*float64(quarterBytes)) {
			t.Fatalf("the whole day holds %d bytes against %d for its last quarter: memory follows the rows", allBytes, quarterBytes)
		}
		if !(allBytes < 1024*pairs) {
			t.Fatalf("%d bytes for %d pairs is %.0f a pair", allBytes, pairs, float64(allBytes)/float64(pairs))
		}
	})
}

// A node's coverage is measured against the sample its pinger draws (D26): an
// extender is expected to measure every other extender of the window up to
// ExtenderPeerSampleSize, a provider every extender up to
// ProviderProbeSampleSize, the four its probe pass is designed to reach, and
// the solve's nodes carry those expectations. Pure.
func TestDeriveNodesExpectThePeersOfTheirSample(t *testing.T) {
	settings := deriveTestSettings()
	jobSettings := DefaultDeriveLocationsSettings()
	places := loadDeriveTestPlaces(t)
	west := deriveTestCity(t, places, 9001)
	for _, test := range []struct {
		extenderCount int
		wantExtender  int
		wantProvider  int
	}{
		{extenderCount: 200, wantExtender: 64, wantProvider: 4},
		{extenderCount: 30, wantExtender: 29, wantProvider: 4},
		{extenderCount: 3, wantExtender: 2, wantProvider: 3},
	} {
		day := newDeriveTestScaleDay(test.extenderCount, 3, func(int) int { return 2 }, 5, 3, 4)
		inputs := deriveTestIngestInGo(settings, day, 4)
		extenderPeers, providerPeers := inputs.setExpectedPeers(jobSettings)
		connect.AssertEqual(t, extenderPeers, test.wantExtender)
		connect.AssertEqual(t, providerPeers, test.wantProvider)

		geneses := map[string]*deriveGenesis{}
		for nodeId := range inputs.nodeKeys {
			geneses[nodeId] = &deriveGenesis{
				source:          deriveGenesisSourceActivation,
				level:           solve.GenesisLevelCity,
				position:        deriveTestLatLon(west),
				radiusKm:        1000,
				countryCode:     west.CountryCode,
				region:          west.Region,
				regionGeonameId: west.RegionGeonameId,
			}
		}
		plan := planDerivation(inputs, geneses, map[string]*model.DerivedLocation{}, places, settings)
		connect.AssertEqual(t, len(plan.nodes), test.extenderCount+5)
		for _, node := range plan.nodes {
			want := test.wantExtender
			if inputs.nodeKeys[node.Id].nodeKind == model.DerivedLocationNodeKindProvider {
				want = test.wantProvider
			}
			if node.ExpectedPeers != want {
				t.Fatalf("%d extenders: %s expects %d peers, want %d", test.extenderCount, node.Id, node.ExpectedPeers, want)
			}
		}
	}
}

// A provider that measured the four extenders its probe pass is designed to
// reach has full coverage and full weight, and one that measured a single
// extender is marked down on coverage: the expectation is the probe window,
// not the pass's candidate cap. Every round trip is the geometry's exactly,
// so nothing but coverage separates the two. Pure.
func TestDeriveProviderCoverageIsItsProbeWindow(t *testing.T) {
	settings := deriveTestSettings()
	jobSettings := DefaultDeriveLocationsSettings()
	places := loadDeriveTestPlaces(t)
	cityIds := []uint32{9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009}

	inputs := &deriveInputs{
		nodeKeys: map[string]deriveNodeKey{},
		refusals: map[string]*solve.Refusals{},
	}
	geneses := map[string]*deriveGenesis{}
	positions := map[string]solve.LatLon{}
	addNode := func(nodeKind int, place *geo.Place) string {
		id := server.NewId()
		nodeId := deriveNodeId(nodeKind, id)
		inputs.nodeKeys[nodeId] = deriveNodeKey{nodeKind: nodeKind, id: id}
		positions[nodeId] = deriveTestLatLon(place)
		geneses[nodeId] = &deriveGenesis{
			source:          deriveGenesisSourceActivation,
			level:           solve.GenesisLevelCity,
			position:        deriveTestLatLon(place),
			radiusKm:        25,
			countryCode:     place.CountryCode,
			region:          place.Region,
			regionGeonameId: place.RegionGeonameId,
		}
		return nodeId
	}
	addTerm := func(source string, target string) {
		inputs.terms = append(inputs.terms, solve.Term{
			Source:  source,
			Target:  target,
			RttMs:   solve.DistanceKm(positions[source], positions[target])/settings.KmPerMs + settings.OverheadMs,
			Samples: 8,
		})
	}
	extenderIds := []string{}
	for _, cityId := range cityIds {
		extenderIds = append(extenderIds, addNode(model.DerivedLocationNodeKindExtender, deriveTestCity(t, places, cityId)))
	}
	for _, source := range extenderIds {
		for _, target := range extenderIds {
			if source != target {
				addTerm(source, target)
			}
		}
	}
	window := addNode(model.DerivedLocationNodeKindProvider, deriveTestCity(t, places, 9002))
	for _, target := range extenderIds[:4] {
		addTerm(window, target)
	}
	single := addNode(model.DerivedLocationNodeKindProvider, deriveTestCity(t, places, 9008))
	addTerm(single, extenderIds[0])
	slices.SortFunc(inputs.terms, func(a solve.Term, b solve.Term) int {
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return strings.Compare(a.Target, b.Target)
	})

	extenderPeers, providerPeers := inputs.setExpectedPeers(jobSettings)
	connect.AssertEqual(t, extenderPeers, len(extenderIds)-1)
	connect.AssertEqual(t, providerPeers, 4)
	plan := planDerivation(inputs, geneses, map[string]*model.DerivedLocation{}, places, settings)

	windowResult := plan.result.Node(window)
	singleResult := plan.result.Node(single)
	t.Logf("four targets: coverage %.3f z %.3f q %.6f; one target: coverage %.3f z %.3f q %.6f", windowResult.Coverage, windowResult.CoverageZ, windowResult.Q, singleResult.Coverage, singleResult.CoverageZ, singleResult.Q)
	connect.AssertEqual(t, windowResult.Coverage, 1.0)
	connect.AssertEqual(t, windowResult.Q, 1.0)
	connect.AssertEqual(t, singleResult.Coverage, 0.25)
	if !(singleResult.Q < 1) {
		t.Fatalf("a provider with one target of four kept q %.6f", singleResult.Q)
	}
	// coverage lowers a weight but never excludes (§5.5)
	if singleResult.Excluded {
		t.Fatal("a provider with one target was excluded")
	}
}

// The planner projects the next run from the last run's record at the cores
// the host has now, and, with no usable record, from the window's exact
// count at the settings' costs; a run projected from its own measurement at
// its own cores is its solve time times the safety factor, and its peak.
// Pure.
func TestPlanDeriveRunProjectsFromTheLastRun(t *testing.T) {
	jobSettings := DefaultDeriveLocationsSettings()
	counted := 0
	count := func() (int64, int64) {
		counted += 1
		return 72000000, 2000000
	}
	// a tenth of the target, measured on ten cores at the settings' costs
	last := &model.DeriveLocationsRun{
		Terms:               7200000,
		Nodes:               200000,
		Sweeps:              36,
		Cores:               10,
		SecondsPerTermSweep: 0.5e-6,
		SecondsPerNodeSweep: 3e-6,
		BytesPerTerm:        100,
		BytesPerNode:        2 * 1024,
	}
	for _, test := range []struct {
		cores       int
		wantSeconds float64
	}{
		// 36 × (7.2M × 0.5 µs + 200k × 3 µs) × 3
		{cores: 10, wantSeconds: 453.6},
		{cores: 20, wantSeconds: 226.8},
		{cores: 5, wantSeconds: 907.2},
	} {
		projection := planDeriveRun(jobSettings, []*model.DeriveLocationsRun{last}, test.cores, count)
		connect.AssertEqual(t, projection.source, "run")
		if 1e-6 < math.Abs(projection.seconds-test.wantSeconds) {
			t.Fatalf("at %d cores projected %.6fs, want %.6fs", test.cores, projection.seconds, test.wantSeconds)
		}
		// 7.2M × 100 + 200k × 2 KiB
		connect.AssertEqual(t, projection.bytes, int64(1129600000))
	}
	connect.AssertEqual(t, counted, 0)

	// a first run, and a run recorded before the costs were, project from the
	// count, as if measured on one core
	for _, history := range [][]*model.DeriveLocationsRun{
		{},
		{{Terms: 10, Nodes: 5}},
	} {
		projection := planDeriveRun(jobSettings, history, 96, count)
		connect.AssertEqual(t, projection.source, "count")
		connect.AssertEqual(t, projection.terms, 72000000)
		connect.AssertEqual(t, projection.sweeps, 36)
		// 36 × (72M × 0.5 µs + 2M × 3 µs) × 3 / 96
		if 1e-6 < math.Abs(projection.seconds-47.25) {
			t.Fatalf("projected %.6fs from the count, want 47.25s", projection.seconds)
		}
		connect.AssertEqual(t, projection.bytes, int64(72000000*100+2000000*2*1024))
	}
	connect.AssertEqual(t, counted, 2)

	// the measured costs keep the settings' proportion, so a run projected
	// from itself at its own cores is its time times the safety factor
	costs := measureDeriveRunCosts(jobSettings, 33000, 1100, 40, 8, 2.5, 180*1024*1024)
	self := projectDeriveRun(jobSettings, costs, 8)
	if 1e-9 < math.Abs(self.seconds-2.5*jobSettings.SweepSafetyFactor) {
		t.Fatalf("a run projected from itself takes %.9fs, want %.9fs", self.seconds, 2.5*jobSettings.SweepSafetyFactor)
	}
	if 1 < math.Abs(float64(self.bytes-180*1024*1024)) {
		t.Fatalf("a run projected from itself holds %d bytes, want %d", self.bytes, 180*1024*1024)
	}
	if 1e-12 < math.Abs(costs.secondsPerNodeSweep/costs.secondsPerTermSweep-jobSettings.SecondsPerNodeSweep/jobSettings.SecondsPerTermSweep) {
		t.Fatalf("the measured costs changed the settings' proportion: %+v", costs)
	}
	// a run that measured nothing keeps the settings' costs
	empty := measureDeriveRunCosts(jobSettings, 0, 0, 0, 8, 0, 0)
	connect.AssertEqual(t, empty.secondsPerTermSweep, jobSettings.SecondsPerTermSweep)
	connect.AssertEqual(t, empty.bytesPerNode, jobSettings.BytesPerNode)
}

// The heap sampler reports the peak above where it started, stops on finish
// and on its context, and does not spin without an interval.
func TestDeriveMemoryPeakMeasuresTheRunsHeap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	memoryPeak := startDeriveMemoryPeak(ctx, time.Millisecond)
	held := make([]byte, 64*1024*1024)
	for i := range held {
		held[i] = byte(i)
	}
	peakBytes := memoryPeak.finish()
	runtime.KeepAlive(held)
	if peakBytes < 64*1024*1024 {
		t.Fatalf("a 64 MiB allocation peaked at %d bytes", peakBytes)
	}
	// a second finish only reads the peak
	connect.AssertEqual(t, memoryPeak.finish(), peakBytes)

	// no interval: the sampler goroutine ends at once
	still := startDeriveMemoryPeak(ctx, 0)
	<-still.done
	connect.AssertEqual(t, 0 <= still.finish(), true)

	// the context ends the sampler
	stopped, stop := context.WithCancel(ctx)
	sampler := startDeriveMemoryPeak(stopped, time.Hour)
	stop()
	<-sampler.done
	sampler.finish()
}
