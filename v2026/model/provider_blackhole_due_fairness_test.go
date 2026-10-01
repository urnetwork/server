// The real due selector must leave room for first checks without starving
// overdue recovery checks. Fixtures bypass publication to test its predicate.
package model

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type testBlackholeDueFleet struct {
	ctx     context.Context
	now     time.Time
	retries []server.Id
	first   []server.Id
}

func newTestBlackholeDueFleet(t testing.TB, retryCount, firstCount int) testBlackholeDueFleet {
	t.Helper()
	fleet := testBlackholeDueFleet{ctx: context.Background(), now: server.NowUtc()}
	city := &Location{LocationType: LocationTypeCity, City: "Synthetic Harbor", Region: "Synthetic Region", Country: "United States", CountryCode: "us"}
	CreateLocation(fleet.ctx, city)
	networkId := server.NewId()
	for index := range retryCount + firstCount {
		id := testingCreateProviderClient(fleet.ctx, networkId, nil, true)
		testingInsertProviderLocationReliability(fleet.ctx, id, networkId, city)
		if index < retryCount {
			firstFailed := fleet.now.Add(-time.Hour)
			due := fleet.now.Add(-10 * time.Minute)
			SetProviderBlackholeCheck(fleet.ctx, &ProviderBlackholeCheck{ClientId: id,
				CheckedAt: firstFailed, OK: false, Failure: "all_destinations_failed", ConsecutiveFailures: 1,
				FirstFailedAt: &firstFailed, NextDueAt: &due})
			fleet.retries = append(fleet.retries, id)
		} else {
			fleet.first = append(fleet.first, id)
		}
	}
	slices.SortFunc(fleet.retries, func(a, b server.Id) int {
		if a.Less(b) {
			return -1
		}
		if b.Less(a) {
			return 1
		}
		return 0
	})
	slices.SortFunc(fleet.first, func(a, b server.Id) int {
		if a.Less(b) {
			return -1
		}
		if b.Less(a) {
			return 1
		}
		return 0
	})
	return fleet
}

func TestBlackholeDueRetryBacklogDoesNotStarveFirstChecks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fleet := newTestBlackholeDueFleet(t, 4, 3)
		for _, first := range fleet.first {
			due := GetProviderBlackholeCheckDue(fleet.ctx, fleet.now, 2, 0, 1)
			if !slices.Equal(due, []server.Id{fleet.retries[0], first}) {
				t.Fatalf("retry backlog excluded first-check share: got=%v want retry then first=%v", due, first)
			}
			// Existing retries intentionally remain due. Completing one first
			// check must advance the other class on the next bounded request.
			SetProviderBlackholeCheck(fleet.ctx, &ProviderBlackholeCheck{ClientId: first, CheckedAt: fleet.now, OK: true})
		}
	})
}

func TestBlackholeDueFairInterleavePreservesLimitAndPrefix(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fleet := newTestBlackholeDueFleet(t, 4, 4)
		var want []server.Id
		for index := range 4 {
			want = append(want, fleet.retries[index], fleet.first[index])
		}
		for limit := 1; limit <= len(want)+2; limit++ {
			due := GetProviderBlackholeCheckDue(fleet.ctx, fleet.now, limit, 0, 1)
			if !slices.Equal(due, want[:min(limit, len(want))]) {
				t.Fatalf("limit=%d changed fair deterministic prefix: got=%v want=%v", limit, due, want[:min(limit, len(want))])
			}
		}
	})
}

func TestBlackholeDueFairnessLendsUnusedClassShare(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fleet := newTestBlackholeDueFleet(t, 4, 1)
		want := []server.Id{fleet.retries[0], fleet.first[0], fleet.retries[1], fleet.retries[2]}
		if got := GetProviderBlackholeCheckDue(fleet.ctx, fleet.now, 4, 0, 1); !slices.Equal(got, want) {
			t.Fatalf("scarce first checks wasted retry capacity: %v want=%v", got, want)
		}
		for _, retry := range fleet.retries {
			SetProviderBlackholeCheck(fleet.ctx, &ProviderBlackholeCheck{ClientId: retry, CheckedAt: fleet.now, OK: true})
		}
		if got := GetProviderBlackholeCheckDue(fleet.ctx, fleet.now, 4, 0, 1); !slices.Equal(got, fleet.first) {
			t.Fatalf("single remaining class lost capacity: %v want=%v", got, fleet.first)
		}
	})
}

// Fixed synthetic ids cover both signs of PostgreSQL's hash in every shard;
// neither class may silently drop the negative half of the hash space.
func TestBlackholeDueFairShareCoversEverySignedHashShard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		candidates := make([]server.Id, 256)
		for index := range candidates {
			candidates[index][0] = 0xf1
			binary.BigEndian.PutUint32(candidates[index][12:], uint32(index+1))
		}
		var byShard [4][2][]server.Id
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT id, ((hashtext(id::text) % 4) + 4) % 4, hashtext(id::text) < 0
				FROM unnest($1::uuid[]) AS ids(id) ORDER BY id`, candidates)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					var shard int
					var negative bool
					server.Raise(rows.Scan(&id, &shard, &negative))
					sign := 0
					if negative {
						sign = 1
					}
					byShard[shard][sign] = append(byShard[shard][sign], id)
				}
			})
		})
		city := &Location{LocationType: LocationTypeCity, City: "Synthetic Shard City", Region: "Synthetic Region", Country: "United States", CountryCode: "us"}
		CreateLocation(ctx, city)
		networkId := server.NewId()
		var retries, first [4][]server.Id
		for shard := range byShard {
			for _, ids := range byShard[shard] {
				if len(ids) < 2 {
					t.Fatal("fixed synthetic corpus does not cover a signed hash shard")
				}
				for lane, id := range ids[:2] {
					Testing_CreateDevice(ctx, networkId, server.NewId(), id, "", "")
					SetProvide(ctx, id, map[ProvideMode][]byte{ProvideModePublic: []byte("synthetic-public-key")})
					testingInsertProviderLocationReliability(ctx, id, networkId, city)
					if lane == 0 {
						SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{ClientId: id, CheckedAt: now.Add(-3 * time.Hour), OK: true})
						retries[shard] = append(retries[shard], id)
					} else {
						first[shard] = append(first[shard], id)
					}
				}
			}
		}
		seen := map[server.Id]bool{}
		for shard := range byShard {
			due := GetProviderBlackholeCheckDue(ctx, now, 4, shard, 4)
			if len(due) != 4 {
				t.Fatalf("shard=%d selected=%d want=4", shard, len(due))
			}
			for index, id := range due {
				want := retries[shard]
				if index%2 == 1 {
					want = first[shard]
				}
				if !slices.Contains(want, id) || seen[id] {
					t.Fatalf("shard=%d leaked/duplicated class at index=%d", shard, index)
				}
				seen[id] = true
			}
		}
		if len(seen) != 16 {
			t.Fatalf("fair shard coverage=%d want=16", len(seen))
		}
	})
}
