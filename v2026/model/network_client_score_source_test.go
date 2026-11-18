package model

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Model the predecessor's three LEFT JOINs independently. Empty locations
// contribute one NULL to the product, while output maps absorb duplicate IDs.
func scoreSourceProduct(memberships [3][]server.Id, visit func([]server.Id)) {
	rows := [3][]*server.Id{}
	for level, ids := range memberships {
		if len(ids) == 0 {
			rows[level] = []*server.Id{nil}
		} else {
			for i := range ids {
				rows[level] = append(rows[level], &ids[i])
			}
		}
	}
	for _, city := range rows[0] {
		for _, region := range rows[1] {
			for _, country := range rows[2] {
				visit(distinctIds(city, region, country))
			}
		}
	}
}

func TestClientScoreGroupMembershipProductParity(t *testing.T) {
	ids := []server.Id{cohortTestId(101), cohortTestId(102), cohortTestId(103), cohortTestId(104)}
	for masks := 0; masks < 1<<12; masks++ {
		var memberships [3][]server.Id
		for level := range memberships {
			for i, id := range ids {
				if masks&(1<<(4*level+i)) != 0 {
					memberships[level] = append(memberships[level], id)
				}
			}
		}
		wantMap := map[server.Id]bool{}
		rows := 0
		scoreSourceProduct(memberships, func(groupIds []server.Id) {
			rows++
			for _, id := range groupIds {
				wantMap[id] = true
			}
		})
		got := clientScoreDistinctGroupIds(memberships[:]...)
		if len(got) != len(wantMap) {
			t.Fatalf("membership count differs for masks=%d", masks)
		}
		for _, id := range got {
			if !wantMap[id] {
				t.Fatalf("membership differs for masks=%d", masks)
			}
			delete(wantMap, id)
		}
		if len(wantMap) != 0 || rows == 0 {
			t.Fatal("lost membership or the no-group source row")
		}
	}
	t.Log("all 4096 three-level membership combinations retain the exact union, including empty, country-only and overlapping groups")
}

// Exercise the actual rank calculation once for each old/new source row.
// The same provider/lookback values are intentionally shared across its group
// rows, as they are within one production SELECT snapshot.
func scoreSourceRankFixture(client, lookback int) *ClientScore {
	score := &ClientScore{
		LookbackIndex:                lookback,
		ReliabilityWeight:            float64(97+client%4) / 100,
		IndependentReliabilityWeight: float64(80+client%21) / 100,
		MinRelativeLatencyMillis:     client % 225,
		MaxBytesPerSecond:            ByteCount(1+client%60) * Mib,
		HasLatencyTest:               client%13 != 0,
		HasSpeedTest:                 client%17 != 0,
		Scores:                       map[string]int{}, Tiers: map[string]int{},
	}
	var egressIndex *int
	if client%2 == 0 {
		index := client % 4
		egressIndex = &index
	}
	bases, minimums := clientScoreRankModeBases(client%3, client%2, egressIndex)
	setClientScoreRanks(score, bases, minimums, score.MinRelativeLatencyMillis, score.MaxBytesPerSecond, score.HasLatencyTest, score.HasSpeedTest)
	return score
}

func scoreSourceRankControl(client int, memberships [3][]server.Id, aggregate bool) (map[server.Id]map[int]*ClientScore, int) {
	out := map[server.Id]map[int]*ClientScore{}
	rows := 0
	for lookback := 0; lookback < 3; lookback++ {
		load := func(ids []server.Id) {
			rows++
			score := scoreSourceRankFixture(client, lookback)
			for _, id := range ids {
				if out[id] == nil {
					out[id] = map[int]*ClientScore{}
				}
				out[id][lookback] = score
			}
		}
		if aggregate {
			load(clientScoreDistinctGroupIds(memberships[:]...))
		} else {
			scoreSourceProduct(memberships, load)
		}
	}
	return out, rows
}

func TestClientScoreGroupRankingParity(t *testing.T) {
	a, b, c, d := cohortTestId(101), cohortTestId(102), cohortTestId(103), cohortTestId(104)
	for _, memberships := range [][3][]server.Id{
		{}, {{a}, nil, nil}, {{a, b}, {a, b}, {a, b}}, {{a, b}, {a, c}, {a, d}},
	} {
		for client := 0; client < 512; client++ {
			before, oldRows := scoreSourceRankControl(client, memberships, false)
			after, newRows := scoreSourceRankControl(client, memberships, true)
			if !reflect.DeepEqual(before, after) || newRows > oldRows {
				t.Fatal("membership aggregation changed scores or increased rank work")
			}
		}
	}
}

// This is a source-row CPU/allocation comparison, not an end-to-end Main
// timing model. The client cardinality is from the retained score attempt;
// two memberships per location and three lookbacks are synthetic controls.
func BenchmarkClientScoreGroupSourceRanking(b *testing.B) {
	a, c, d, e := cohortTestId(101), cohortTestId(102), cohortTestId(103), cohortTestId(104)
	memberships := [3][]server.Id{{a, c}, {a, d}, {a, e}}
	for _, aggregate := range []bool{false, true} {
		b.Run(fmt.Sprintf("aggregate=%t/clients=86499", aggregate), func(b *testing.B) {
			b.ReportAllocs()
			rows := 0
			checksum := 0
			for run := 0; run < b.N; run++ {
				for client := 0; client < 86499; client++ {
					values, count := scoreSourceRankControl(client, memberships, aggregate)
					rows += count
					for _, lookbacks := range values {
						for _, score := range lookbacks {
							checksum += score.Scores[RankModeQuality] + score.Scores[RankModeSpeed]
						}
					}
				}
			}
			if checksum <= 0 {
				b.Fatal("rank work disappeared")
			}
			b.ReportMetric(float64(rows)/float64(b.N), "rank_rows/op")
		})
	}
}

func TestClientScoreGroupDedupDoesNotMutateMemberships(t *testing.T) {
	a, b := cohortTestId(101), cohortTestId(102)
	members := []server.Id{b, a, b}
	before := slices.Clone(members)
	got := clientScoreDistinctGroupIds(members, members)
	if !reflect.DeepEqual(members, before) || !reflect.DeepEqual(got, []server.Id{b, a}) {
		t.Fatal("membership inputs were mutated or duplicates survived")
	}
}
