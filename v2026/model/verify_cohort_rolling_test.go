package model

// The verify cohort's rolling census (sn VERIFY-COHORT-V2.md §2.4-2.6) against
// the test environment's Redis and Postgres: it starts only once the
// validator's head summaries carry a census and it has folded a window since
// the first of them; it then frees the legacy members the validator no longer
// holds, and a top-ranked unclaimed network's representative enters; the
// interlock stops admissions at the validator's reported census; a stale report
// frees nothing and admits no new provider; the headroom holds room for claimed
// members; an expiry racing a re-draw keeps the member; and a Redis that lost
// the census restores it, counting every member as assigned that epoch.
// Identities are synthetic.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

const testVerifyCohortNetuid uint16 = 25

func testVerifyCohortHotkey() [32]byte {
	return [32]byte{0x98, 0x9e, 0x82}
}

// Stores one head summary of the test validator, as the ingest does.
func testVerifyCohortPutSummary(ctx context.Context, nativeEpoch uint64, settlementEpoch uint64, census *uint64, receivedAt time.Time) {
	PutSnHeadSummary(ctx, &SnHeadSummary{
		Netuid: testVerifyCohortNetuid, ValidatorHotkey: testVerifyCohortHotkey(), ValidatorUid: 1,
		NativeEpoch: nativeEpoch, NativeSnapshotBlock: 9_000_000 + nativeEpoch*360, EvmSnapshotBlock: 9_000_000 + nativeEpoch*360,
		SettlementEpoch: settlementEpoch, PolicyId: 1, MeasurementArtifactHash: "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		MaximumHeadFleets: 200, SignedAt: receivedAt, SigningHash: "sha256:0000000000000000000000000000000000000000000000000000000000000002",
		ReceivedAt: receivedAt, Body: []byte("{}"), CensusProviders: census,
	})
}

func testVerifyCohortCount(value uint64) *uint64 {
	return &value
}

// Reads the report and refreshes the rolling state, as the binding sync does.
func testVerifyCohortRefresh(t testing.TB, ctx context.Context, settings *VerifySettings, upcoming int, now time.Time) *VerifyCohortRollingState {
	t.Helper()
	report := GetVerifyCohortValidatorReport(ctx, testVerifyCohortNetuid, now)
	state := RefreshVerifyCohortState(ctx, settings, report, upcoming, now)
	if state == nil {
		t.Fatal("the rolling state was not refreshed")
	}
	return state
}

func testVerifyCohortDurableLastEpochs(ctx context.Context) map[server.Id]*int64 {
	rows := map[server.Id]*int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT client_id, last_epoch FROM verify_cohort_census WHERE deployment_key = $1`, string(testVerifyCohortDeployment))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var last *int64
				server.Raise(result.Scan(&clientId, &last))
				rows[clientId] = last
			}
		})
	})
	return rows
}

// Root cause: a full legacy census whose members went idle refuses a top-ranked
// unclaimed network's representative for good, so it can never become
// measurable. Once the validator's summaries show it folded a window with the
// retirement rule, the rolling census frees the idle members, keeps the one
// still measured, and admits the representative.
func TestVerifyCohortRollingAdmitsARankedNetworkPastAFullLegacyCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}
		now := server.NowUtc()

		// epoch 1: four providers fill the lifetime census
		testVerifyCohortSetEpoch(ctx, 1)
		legacy := addresses.providers(ctx, settings, 4)
		testVerifyCohortSample(t, ctx, settings, 8)
		if rotation := RotateVerifyCohort(ctx, settings); rotation == nil || rotation.Census != 4 {
			t.Fatal("rotation did not seed the census", rotation)
		}
		// epoch 3: three of them went away; one is still measured
		testVerifyCohortSetEpoch(ctx, 3)
		testVerifyCohortSetEligible(ctx, false, legacy[1:]...)
		if hop, _ := SampleVerifyNextHop(ctx, nil, settings); hop == nil || *hop != legacy[0] {
			t.Fatal("the measured member was not carried", hop)
		}
		representative := addresses.providers(ctx, settings, 1)[0]
		SetVerifyCohortRanked(ctx, settings.CohortDeploymentKey, []server.Id{representative})

		// the legacy census refuses it for good, and the board's ranked set
		// does not draw outside the rolling census
		for range 4 {
			if hop, _ := SampleVerifyNextHop(ctx, nil, settings); hop == nil || *hop == representative {
				t.Fatal("the legacy census admitted the representative", hop)
			}
		}
		if admitted, _ := testVerifyCohortAdmit(ctx, settings, 3, representative, false); admitted {
			t.Fatal("the legacy census admitted a new unclaimed provider past its limit")
		}
		// a summary without a census, and one census in the first epoch, start nothing
		testVerifyCohortPutSummary(ctx, 100, 3, nil, now.Add(-2*time.Hour))
		testVerifyCohortPutSummary(ctx, 101, 3, testVerifyCohortCount(4), now.Add(-time.Hour))
		if state := testVerifyCohortRefresh(t, ctx, settings, 0, now); state.Rolling {
			t.Fatal("the rolling census started before the validator folded a window with the rule", state)
		}
		if admitted, _ := testVerifyCohortAdmit(ctx, settings, 3, representative, false); admitted {
			t.Fatal("the representative entered before the rolling census started")
		}

		// the validator folded window 3 with the rule: epoch 4's summary
		testVerifyCohortSetEpoch(ctx, 4)
		testVerifyCohortPutSummary(ctx, 102, 4, testVerifyCohortCount(2), now)
		state := testVerifyCohortRefresh(t, ctx, settings, 0, now)
		if !state.Rolling || state.RollingFrom != 4 || !state.CensusKnown || state.Census != 2 || state.ValidatorEpoch != 4 {
			t.Fatal("rolling state", state)
		}
		expiry := ExpireVerifyCohort(ctx, settings, state)
		if expiry == nil || expiry.Expired != 3 || expiry.Members != 1 || expiry.Restored {
			t.Fatal("expiry", expiry)
		}
		durable := testVerifyCohortDurableLastEpochs(ctx)
		if len(durable) != 1 || durable[legacy[0]] == nil || *durable[legacy[0]] != 3 {
			t.Fatal("the durable rolling census", durable)
		}
		if hop, _ := SampleVerifyNextHop(ctx, nil, settings); hop == nil || *hop != representative {
			t.Fatal("the representative was not drawn first after claimed members", hop)
		}
		durable = testVerifyCohortDurableLastEpochs(ctx)
		if durable[representative] == nil || *durable[representative] != 4 {
			t.Fatal("the representative was not written through with its epoch", durable)
		}
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 4)
		if census := testVerifyCohortMembers(ctx, keys.census); len(census) != 2 || !census[representative.String()] || !census[legacy[0].String()] {
			t.Fatal("rolling census", census)
		}
	})
}

// Starts the rolling census at validator epoch 4 with a census of census, and
// runs the taskworker's first expiry, which readies the census in Redis.
func testVerifyCohortStartRolling(t testing.TB, ctx context.Context, settings *VerifySettings, census uint64, now time.Time) *VerifyCohortRollingState {
	t.Helper()
	testVerifyCohortPutSummary(ctx, 200, 3, testVerifyCohortCount(census), now.Add(-time.Hour))
	testVerifyCohortPutSummary(ctx, 201, 4, testVerifyCohortCount(census), now)
	testVerifyCohortSetEpoch(ctx, 4)
	state := testVerifyCohortRefresh(t, ctx, settings, 0, now)
	if !state.Rolling || !state.CensusKnown {
		t.Fatal("the rolling census did not start", state)
	}
	if expiry := ExpireVerifyCohort(ctx, settings, state); expiry == nil || expiry.Expired != 0 {
		t.Fatal("the first expiry", expiry)
	}
	return state
}

// The validator's reported census plus everything admitted since its last two
// reports stays under 3,584; a new report moves the count back, and a report
// without a census admits nobody new.
func TestVerifyCohortRollingInterlockStopsAtTheValidatorCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}
		now := server.NowUtc()
		testVerifyCohortStartRolling(t, ctx, settings, verifyCohortInterlockLimit-1, now)
		providers := addresses.providers(ctx, settings, 3)
		if admitted, _ := testVerifyCohortAdmit(ctx, settings, 4, providers[0], false); !admitted {
			t.Fatal("the first admission under the interlock was refused")
		}
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 4)
		var refusal verifyCohortRefusal
		server.Redis(ctx, func(r server.RedisClient) {
			_, _, _, refusal = admitVerifyCohortWithReason(ctx, r, keys, providers[1].String(), settings, verifyCohortAdmission{}, nil)
		})
		if refusal != verifyCohortRefusedInterlock {
			t.Fatal("the admission at the interlock was not refused by it", refusal)
		}
		// the next report counts that admission in its census: room again,
		// while the admission stays counted as previous until the report after
		testVerifyCohortPutSummary(ctx, 202, 4, testVerifyCohortCount(verifyCohortInterlockLimit-2), now)
		testVerifyCohortRefresh(t, ctx, settings, 0, now)
		if admitted, _ := testVerifyCohortAdmit(ctx, settings, 4, providers[1], false); !admitted {
			t.Fatal("a new report did not move the interlock")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			_, _, _, refusal = admitVerifyCohortWithReason(ctx, r, keys, providers[2].String(), settings, verifyCohortAdmission{}, nil)
		})
		if refusal != verifyCohortRefusedInterlock {
			t.Fatal("previous and since were not both counted", refusal)
		}
		// a validator that stops reporting its census admits nobody new
		testVerifyCohortPutSummary(ctx, 203, 4, nil, now)
		if state := testVerifyCohortRefresh(t, ctx, settings, 0, now); state.CensusKnown || !state.Rolling {
			t.Fatal("a report without a census", state)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			_, _, _, refusal = admitVerifyCohortWithReason(ctx, r, keys, providers[2].String(), settings, verifyCohortAdmission{priority: true}, nil)
		})
		if refusal != verifyCohortRefusedInterlock {
			t.Fatal("an unknown census admitted a new provider", refusal)
		}
	})
}

// A stale report frees nothing and admits nobody new; members already counted
// are still drawn.
func TestVerifyCohortRollingStaleReportFreesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}
		now := server.NowUtc()
		testVerifyCohortSetEpoch(ctx, 1)
		members := addresses.providers(ctx, settings, 2)
		testVerifyCohortSample(t, ctx, settings, 4)
		RotateVerifyCohort(ctx, settings)
		testVerifyCohortSetEligible(ctx, false, members[1])
		// the validator's last report is four hours old
		testVerifyCohortPutSummary(ctx, 300, 3, testVerifyCohortCount(2), now.Add(-5*time.Hour))
		testVerifyCohortPutSummary(ctx, 301, 6, testVerifyCohortCount(2), now.Add(-4*time.Hour))
		testVerifyCohortSetEpoch(ctx, 6)
		state := testVerifyCohortRefresh(t, ctx, settings, 0, now)
		if !state.Rolling || state.CensusKnown {
			t.Fatal("a stale report", state)
		}
		if expiry := ExpireVerifyCohort(ctx, settings, state); expiry == nil || expiry.Expired != 0 || expiry.Members != 2 {
			t.Fatal("a stale report freed members", expiry)
		}
		fresh := addresses.providers(ctx, settings, 1)[0]
		if admitted, _ := testVerifyCohortAdmit(ctx, settings, 6, fresh, true); admitted {
			t.Fatal("a stale report admitted a new provider")
		}
		if hop, _ := SampleVerifyNextHop(ctx, nil, settings); hop == nil || *hop != members[0] {
			t.Fatal("a counted member was not drawn", hop)
		}
	})
}

// The headroom holds room for claimed members: every other tier stops at the
// limit less the headroom, a claimed member at the limit.
func TestVerifyCohortRollingHeadroomHoldsRoomForClaimedMembers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}
		now := server.NowUtc()
		testVerifyCohortStartRolling(t, ctx, settings, 0, now)
		// one claimed member bound from the next epoch
		testVerifyCohortRefresh(t, ctx, settings, 1, now)
		providers := addresses.providers(ctx, settings, 5)
		for _, provider := range providers[:3] {
			if admitted, _ := testVerifyCohortAdmit(ctx, settings, 4, provider, false); !admitted {
				t.Fatal("admission under the headroom was refused")
			}
		}
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 4)
		var refusal verifyCohortRefusal
		server.Redis(ctx, func(r server.RedisClient) {
			_, _, _, refusal = admitVerifyCohortWithReason(ctx, r, keys, providers[3].String(), settings, verifyCohortAdmission{}, nil)
		})
		if refusal != verifyCohortRefusedRollingFull {
			t.Fatal("an unclaimed provider took the headroom", refusal)
		}
		if admitted, _ := testVerifyCohortAdmit(ctx, settings, 4, providers[3], true); !admitted {
			t.Fatal("a claimed member was refused the headroom held for it")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			_, _, _, refusal = admitVerifyCohortWithReason(ctx, r, keys, providers[4].String(), settings, verifyCohortAdmission{priority: true}, nil)
		})
		if refusal != verifyCohortRefusedRollingFull {
			t.Fatal("a claimed member passed the limit", refusal)
		}
	})
}

// An expiry racing a re-draw keeps the member: the script re-checks this
// epoch's and the next epoch's cohorts before it stops counting anyone.
func TestVerifyCohortRollingExpiryKeepsARedrawnMember(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 4)
		next := newVerifyCohortKeys(settings.CohortDeploymentKey, 5)
		redrawn, redrawnNext, idle := server.NewId().String(), server.NewId().String(), server.NewId().String()
		var removed []string
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, keys.census, redrawn, redrawnNext, idle).Err())
			server.Raise(r.SAdd(ctx, keys.lifetime, redrawn, redrawnNext, idle).Err())
			server.Raise(r.SAdd(ctx, keys.epoch, redrawn).Err())
			server.Raise(r.SAdd(ctx, next.epoch, redrawnNext).Err())
			var err error
			removed, err = r.Eval(ctx, verifyCohortExpireScript, []string{keys.census, keys.lifetime, keys.epoch, next.epoch}, redrawn, redrawnNext, idle).StringSlice()
			server.Raise(err)
		})
		if len(removed) != 1 || removed[0] != idle {
			t.Fatal("the expiry script removed", removed)
		}
		census := testVerifyCohortMembers(ctx, keys.census)
		lifetime := testVerifyCohortMembers(ctx, keys.lifetime)
		if len(census) != 2 || !census[redrawn] || !census[redrawnNext] || len(lifetime) != 2 || lifetime[idle] {
			t.Fatal("after expiry", census, lifetime)
		}
	})
}

// A Redis that lost the rolling census restores it from Postgres and counts
// every member as assigned this epoch, since its epoch sets are gone too: the
// run frees nothing, and the durable last epochs move up to it.
func TestVerifyCohortRollingRestoresConservatively(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		addresses := &testVerifyCohortAddresses{}
		now := server.NowUtc()
		testVerifyCohortSetEpoch(ctx, 1)
		members := addresses.providers(ctx, settings, 3)
		testVerifyCohortSample(t, ctx, settings, 6)
		RotateVerifyCohort(ctx, settings)
		testVerifyCohortSetEligible(ctx, false, members...)
		// the rolling census starts, and every member was last assigned in epoch 1
		testVerifyCohortPutSummary(ctx, 400, 5, testVerifyCohortCount(3), now.Add(-time.Hour))
		testVerifyCohortPutSummary(ctx, 401, 7, testVerifyCohortCount(3), now)
		testVerifyCohortSetEpoch(ctx, 7)
		state := testVerifyCohortRefresh(t, ctx, settings, 0, now)
		if !state.Rolling || !state.CensusKnown {
			t.Fatal("rolling", state)
		}
		// Redis loses the cohort before the first expiry
		server.Redis(ctx, func(r server.RedisClient) {
			for epoch := uint64(0); epoch <= 8; epoch++ {
				keys := newVerifyCohortKeys(settings.CohortDeploymentKey, epoch)
				server.Raise(r.Del(ctx, keys.epoch, keys.draws).Err())
			}
			keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 7)
			server.Raise(r.Del(ctx, keys.census, keys.lifetime, keys.censusReady).Err())
		})
		expiry := ExpireVerifyCohort(ctx, settings, state)
		if expiry == nil || !expiry.Restored || expiry.Expired != 0 || expiry.Members != 3 {
			t.Fatal("restore", expiry)
		}
		for clientId, last := range testVerifyCohortDurableLastEpochs(ctx) {
			if last == nil || *last != 7 {
				t.Fatal("a restored member's last epoch", clientId, last)
			}
		}
	})
}

// The head summary keeps what the validator reported, and the report reads it.
func TestSnHeadSummaryKeepsTheReportedCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		testVerifyCohortPutSummary(ctx, 500, 6, testVerifyCohortCount(1403), now)
		summary := GetLatestSnHeadSummary(ctx, testVerifyCohortNetuid, nil)
		if summary == nil || summary.CensusProviders == nil || *summary.CensusProviders != 1403 || summary.BoundClients != nil {
			t.Fatal("stored summary", summary)
		}
		report := GetVerifyCohortValidatorReport(ctx, testVerifyCohortNetuid, now)
		if report == nil || report.Hotkeys != 1 || report.SettlementEpoch != 6 || report.Census == nil || *report.Census != 1403 || report.FirstCensusEpoch == nil || *report.FirstCensusEpoch != 6 {
			t.Fatal("report", report)
		}
	})
}

// The admission script as it was before the rolling census, kept to prove the
// new script's legacy mode is the same rule: during the api roll both run on
// one Redis, before the rolling census can start.
const testVerifyCohortAdmitScriptBeforeRolling = `
local member = redis.call('SISMEMBER', KEYS[1], ARGV[1]) == 1
local size = redis.call('SCARD', KEYS[1])
local carried = redis.call('SISMEMBER', KEYS[3], ARGV[1]) == 1
local lifetime = redis.call('SCARD', KEYS[3])
local ready = redis.call('EXISTS', KEYS[5]) == 1
local counted = redis.call('SISMEMBER', KEYS[4], ARGV[1]) == 1
local census = redis.call('SCARD', KEYS[4])
local raw = redis.call('GET', KEYS[2])
if raw and not string.match(raw, '^%d+$') then
  return redis.error_reply('verify cohort draw counter is not an integer')
end
local draws = tonumber(raw or '0')
local new = 0
if not member then
  if size >= tonumber(ARGV[2]) then return {0, size, 0} end
  if not carried then
    if lifetime >= tonumber(ARGV[3]) then return {0, size, 0} end
    if not (ready and counted) then
      if ARGV[9] == '1' and not ready then return {0, size, 0} end
      local ever = lifetime
      local bound = tonumber(ARGV[3])
      if ready then
        ever = census
        if ARGV[8] == '1' then bound = tonumber(ARGV[7]) end
      end
      if ever >= bound then return {0, size, 0} end
      if ARGV[5] ~= '1' and draws < tonumber(ARGV[4]) * size then return {0, size, 0} end
      if ready then new = 1 end
    end
    redis.call('SADD', KEYS[3], ARGV[1])
    redis.call('SADD', KEYS[4], ARGV[1])
  end
  redis.call('SADD', KEYS[1], ARGV[1])
end
redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[1], ARGV[6])
redis.call('EXPIRE', KEYS[2], ARGV[6])
local n = redis.call('SCARD', KEYS[1])
for i = 10, #ARGV do
  if redis.call('SISMEMBER', KEYS[1], ARGV[i]) == 1 then n = n - 1 end
end
return {1, n, new}
`

// One admission by the script as it was before the rolling census.
func testVerifyCohortAdmitBeforeRolling(ctx context.Context, r server.RedisClient, keys verifyCohortKeys, candidate string, settings *VerifySettings, admission verifyCohortAdmission) []int64 {
	flag := func(value bool) string {
		if value {
			return "1"
		}
		return "0"
	}
	values, err := r.Eval(ctx, testVerifyCohortAdmitScriptBeforeRolling, []string{keys.epoch, keys.draws, keys.lifetime, keys.census, keys.censusReady},
		candidate, settings.CohortSize, settings.CohortLifetimeLimit, settings.ReliabilityAMin, flag(admission.skipGrowthGate),
		int64(verifyCohortEpochTtl/time.Second), max(settings.CohortCensusLimit, settings.CohortLifetimeLimit), flag(admission.priority), flag(admission.censusRequired),
	).Int64Slice()
	server.Raise(err)
	return values
}

// Before the rolling census starts, an api on the new script and one still on
// the old interleave on one Redis exactly as the old script alone: the same
// admissions, refusals, cohort sizes and census entries, across the growth
// gate, both limits, the claimed path and a ready census.
func TestVerifyCohortLegacyModeMatchesTheScriptBeforeRolling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyCohortRotationSettings()
		settings.ReliabilityAMin = 1
		candidates := make([]string, 12)
		for i := range candidates {
			candidates[i] = server.NewId().String()
		}
		type step struct {
			candidate int
			admission verifyCohortAdmission
		}
		steps := []step{}
		for i := range candidates {
			steps = append(steps, step{candidate: i, admission: verifyCohortAdmission{skipGrowthGate: i%3 == 0, priority: 6 <= i && i%2 == 0}})
			steps = append(steps, step{candidate: i / 2, admission: verifyCohortAdmission{}})
		}
		run := func(deploymentKey StDeploymentKey, mixed bool) [][]int64 {
			keys := newVerifyCohortKeys(deploymentKey, 1)
			results := [][]int64{}
			server.Redis(ctx, func(r server.RedisClient) {
				// a ready census, as after rotation's first run
				server.Raise(r.Set(ctx, keys.censusReady, "1", 0).Err())
				for index, step := range steps {
					if mixed && index%2 == 1 {
						admitted, n, entered, _ := admitVerifyCohortWithReason(ctx, r, keys, candidates[step.candidate], settings, step.admission, nil)
						result := []int64{0, int64(n), 0}
						if admitted {
							result[0] = 1
						}
						if entered {
							result[2] = 1
						}
						results = append(results, result)
					} else {
						results = append(results, testVerifyCohortAdmitBeforeRolling(ctx, r, keys, candidates[step.candidate], settings, step.admission))
					}
				}
			})
			return results
		}
		before := run("964:0x00000000000000000000000000000000000000b1", false)
		mixed := run("964:0x00000000000000000000000000000000000000b2", true)
		if len(before) != len(mixed) {
			t.Fatal("runs differ in length")
		}
		for index := range before {
			if len(before[index]) != 3 || before[index][0] != mixed[index][0] || before[index][1] != mixed[index][1] || before[index][2] != mixed[index][2] {
				t.Fatalf("step %d: the old script alone %v, interleaved with the new %v", index, before[index], mixed[index])
			}
		}
		beforeKeys := newVerifyCohortKeys("964:0x00000000000000000000000000000000000000b1", 1)
		mixedKeys := newVerifyCohortKeys("964:0x00000000000000000000000000000000000000b2", 1)
		for _, pair := range [][2]string{{beforeKeys.lifetime, mixedKeys.lifetime}, {beforeKeys.census, mixedKeys.census}, {beforeKeys.epoch, mixedKeys.epoch}} {
			left, right := testVerifyCohortMembers(ctx, pair[0]), testVerifyCohortMembers(ctx, pair[1])
			if len(left) != len(right) || len(left) == 0 {
				t.Fatalf("%s holds %d, %s holds %d", pair[0], len(left), pair[1], len(right))
			}
			for member := range left {
				if !right[member] {
					t.Fatalf("%s holds %s and %s does not", pair[0], member, pair[1])
				}
			}
		}
	})
}
