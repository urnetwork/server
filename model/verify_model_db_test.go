package model

// verify_model_db_test.go — pg/redis-backed integration tests for the
// `/verify` state model: the redis behaviors the pure controller tests only
// reason about (per-trail lock, egress bijection, rate/eligibility counters,
// trail lifecycle + reaper) and the pg trail/rollup persistence. Run with the
// local services from test.sh (see st_model_db_test.go header).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// testVerifySetProvideModes marks a client as providing (the §5.1 eligibility
// requirement), writing the same redis value the provide-key layer maintains.
func testVerifySetProvideModes(ctx context.Context, clientId server.Id) {
	modesJson, err := json.Marshal([]ProvideMode{ProvideModePublic})
	if err != nil {
		panic(err)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, provideModesKey(clientId), modesJson, 0).Err())
	})
}

// testVerifyEligible reports membership in the eligible-provider set.
func testVerifyEligible(ctx context.Context, clientId server.Id) bool {
	var member bool
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		member, err = r.SIsMember(ctx, verifyEligibleKey, clientId.String()).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
	})
	return member
}

// testVerifyReapScore returns the reap-registry score for a trail, or ok=false
// when the trail has no registry entry.
func testVerifyReapScore(ctx context.Context, trailId server.Id) (score float64, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		score, err = r.ZScore(ctx, verifyReapKey, trailId.String()).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		ok = true
	})
	return
}

func TestVerifyEgressBijection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()

		c1, c2, c3, c4 := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		ip1 := netip.MustParseAddr("203.0.113.10")
		ip2 := netip.MustParseAddr("203.0.113.20")
		ip3 := netip.MustParseAddr("203.0.113.30")
		ip4 := netip.MustParseAddr("203.0.113.40")

		// clean claim resolves both ways
		FeedVerifyEgress(ctx, c1, ip1, settings)
		if got := ResolveVerifyEgress(ctx, ip1, settings); got == nil || *got != c1 {
			t.Fatalf("resolve(ip1) = %v, want c1", got)
		}

		// a second client observed on the same ip downgrades it to ambiguous —
		// it resolves to NOTHING (never guess), and re-feeding the original
		// claimant does not re-claim while the marker lives
		FeedVerifyEgress(ctx, c2, ip1, settings)
		if got := ResolveVerifyEgress(ctx, ip1, settings); got != nil {
			t.Fatalf("ambiguous ip resolved to %v", got)
		}
		FeedVerifyEgress(ctx, c1, ip1, settings)
		if got := ResolveVerifyEgress(ctx, ip1, settings); got != nil {
			t.Fatalf("ambiguous ip re-claimed to %v", got)
		}

		// a client with two live egress ips fails the exactly-one reverse check
		FeedVerifyEgress(ctx, c3, ip2, settings)
		FeedVerifyEgress(ctx, c3, ip3, settings)
		if got := ResolveVerifyEgress(ctx, ip2, settings); got != nil {
			t.Fatalf("two-ip client resolved to %v", got)
		}

		// release hook: removing the client clears forward + reverse state
		FeedVerifyEgress(ctx, c4, ip4, settings)
		if got := ResolveVerifyEgress(ctx, ip4, settings); got == nil || *got != c4 {
			t.Fatalf("resolve(ip4) = %v, want c4", got)
		}
		RemoveVerifyEgressForClient(ctx, c4)
		if got := ResolveVerifyEgress(ctx, ip4, settings); got != nil {
			t.Fatalf("released ip still resolves to %v", got)
		}

		// ClearVerifyEgress drops a single (client, ip) pair
		c5 := server.NewId()
		ip5 := netip.MustParseAddr("203.0.113.50")
		FeedVerifyEgress(ctx, c5, ip5, settings)
		ClearVerifyEgress(ctx, c5, ip5)
		if got := ResolveVerifyEgress(ctx, ip5, settings); got != nil {
			t.Fatalf("cleared ip still resolves to %v", got)
		}
	})
}

func TestVerifyEligibleMembership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()

		providing, silent := server.NewId(), server.NewId()
		testVerifySetProvideModes(ctx, providing)

		FeedVerifyEgress(ctx, providing, netip.MustParseAddr("198.51.100.10"), settings)
		FeedVerifyEgress(ctx, silent, netip.MustParseAddr("198.51.100.20"), settings)

		if !testVerifyEligible(ctx, providing) {
			t.Fatal("providing client with one clean egress must be eligible")
		}
		if testVerifyEligible(ctx, silent) {
			t.Fatal("client with no provide modes must not be eligible")
		}

		// losing the bijection revokes eligibility
		FeedVerifyEgress(ctx, providing, netip.MustParseAddr("198.51.100.30"), settings)
		if testVerifyEligible(ctx, providing) {
			t.Fatal("two live egress ips must revoke eligibility")
		}
	})
}

func TestVerifyTrailLockMutualExclusion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		trailId := server.NewId()

		if !AcquireVerifyTrailLock(ctx, trailId, 30*time.Second) {
			t.Fatal("first acquire must succeed")
		}
		if AcquireVerifyTrailLock(ctx, trailId, 30*time.Second) {
			t.Fatal("second acquire must be excluded (V3)")
		}
		ReleaseVerifyTrailLock(ctx, trailId)
		if !AcquireVerifyTrailLock(ctx, trailId, 200*time.Millisecond) {
			t.Fatal("acquire after release must succeed")
		}
		// the ttl self-heals a crashed holder
		time.Sleep(400 * time.Millisecond)
		if !AcquireVerifyTrailLock(ctx, trailId, time.Second) {
			t.Fatal("acquire after ttl expiry must succeed")
		}
	})
}

func TestVerifyRateMeters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()

		// ips in distinct /29s (ClientIpHash buckets ipv4 by /29)
		ipA, ipB := "10.0.0.1", "10.0.1.1"
		vpkA, vpkB := []byte("vpk-a"), []byte("vpk-b")

		if ip, vpk := IncrVerifySeedRates(ctx, ipA, vpkA, settings); ip != 1 || vpk != 1 {
			t.Fatalf("first seed = (%d, %d)", ip, vpk)
		}
		if ip, vpk := IncrVerifySeedRates(ctx, ipA, vpkA, settings); ip != 2 || vpk != 2 {
			t.Fatalf("second seed = (%d, %d)", ip, vpk)
		}
		// rotating the vpk resets only the (best-effort) vpk counter — the
		// source-ip counter is the real limit (V4)
		if ip, vpk := IncrVerifySeedRates(ctx, ipA, vpkB, settings); ip != 3 || vpk != 1 {
			t.Fatalf("rotated-vpk seed = (%d, %d)", ip, vpk)
		}
		if ip, _ := IncrVerifySeedRates(ctx, ipB, vpkA, settings); ip != 1 {
			t.Fatalf("second ip seed = %d", ip)
		}

		// the EXTEND meter is per source ip only
		if got := IncrVerifyExtendRate(ctx, ipA, settings); got != 1 {
			t.Fatalf("extend = %d", got)
		}
		if got := IncrVerifyExtendRate(ctx, ipA, settings); got != 2 {
			t.Fatalf("extend = %d", got)
		}

		// active-trail slots: incr counts, decr floors at zero
		if got := IncrVerifyActiveTrails(ctx, vpkA, settings); got != 1 {
			t.Fatalf("active = %d", got)
		}
		DecrVerifyActiveTrails(ctx, vpkA)
		DecrVerifyActiveTrails(ctx, vpkA) // double release must not go negative
		if got := IncrVerifyActiveTrails(ctx, vpkA, settings); got != 1 {
			t.Fatalf("active after floor = %d, want 1", got)
		}
	})
}

func TestVerifyEligibilityTokens(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings() // EligibilityBurst = 2

		// D26: the bucket defaults OFF (EligibilityInterval 0) — every provider
		// always holds a token past the burst, so each validator sets its own
		// measurement rate.
		clientOff := server.NewId()
		if !CheckVerifyEligibilityToken(ctx, clientOff, settings) ||
			!SpendVerifyEligibilityToken(ctx, clientOff, settings) ||
			!SpendVerifyEligibilityToken(ctx, clientOff, settings) ||
			!SpendVerifyEligibilityToken(ctx, clientOff, settings) ||
			!CheckVerifyEligibilityToken(ctx, clientOff, settings) {
			t.Fatal("with the bucket off (interval 0) tokens never exhaust (D26)")
		}

		// a positive interval re-enables the §5.3 bucket (still configurable)
		settings.EligibilityInterval = 60 * time.Second
		clientId := server.NewId()

		if !CheckVerifyEligibilityToken(ctx, clientId, settings) {
			t.Fatal("unspent client must hold a token")
		}
		if !SpendVerifyEligibilityToken(ctx, clientId, settings) {
			t.Fatal("spend 1/2 must succeed")
		}
		if !CheckVerifyEligibilityToken(ctx, clientId, settings) {
			t.Fatal("1/2 spent must still hold")
		}
		if !SpendVerifyEligibilityToken(ctx, clientId, settings) {
			t.Fatal("spend 2/2 must succeed")
		}
		if CheckVerifyEligibilityToken(ctx, clientId, settings) {
			t.Fatal("burst exhausted must not hold")
		}
		if SpendVerifyEligibilityToken(ctx, clientId, settings) {
			t.Fatal("spend over burst must fail")
		}
	})
}

// testVerifyBuildTrail builds an active trail with a seed hop and a pending
// assignment at `assignedMs`.
func testVerifyBuildTrail(poison bool, assignedMs uint64) *VerifyTrail {
	nowMs := assignedMs
	return &VerifyTrail{
		TrailId:     server.NewId(),
		ClientId:    server.NewId(),
		Vpk:         []byte(fmt.Sprintf("vpk-%s", server.NewId())),
		ServerNonce: []byte("nonce-1234567890"),
		M:           3,
		ServerKeyId: 1,
		Status:      VerifyTrailStatusActive,
		Poison:      poison,
		CreateMs:    nowMs,
		ActivityMs:  nowMs,
		Hops: []*VerifyTrailHop{
			// D27: the seed hop carries an egress-IP-hash (here a fixed marker)
			// that must round-trip through the redis hop record
			{ClientId: server.NewId(), ConfirmedMs: nowMs, Seed: true, EgressIpHash: [32]byte{0xEE, 0x01}},
		},
		Pending: &VerifyTrailHop{ClientId: server.NewId(), AssignedMs: assignedMs, AssignN: 4},
	}
}

func TestVerifyTrailLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		nowMs := uint64(server.NowUtc().UnixMilli())

		trail := testVerifyBuildTrail(false, nowMs)
		CreateVerifyTrail(ctx, trail, `{"resp":1}`, settings)

		got := GetVerifyTrail(ctx, trail.TrailId)
		if got == nil {
			t.Fatal("created trail must load")
		}
		if got.ClientId != trail.ClientId || string(got.Vpk) != string(trail.Vpk) ||
			got.M != 3 || got.ServerKeyId != 1 || got.Poison ||
			got.Status != VerifyTrailStatusActive {
			t.Fatalf("header roundtrip = %+v", got)
		}
		if len(got.Hops) != 1 || !got.Hops[0].Seed || got.Pending == nil ||
			got.Pending.ClientId != trail.Pending.ClientId ||
			got.Hops[0].EgressIpHash != trail.Hops[0].EgressIpHash {
			t.Fatalf("hops/pending roundtrip = %+v / %+v", got.Hops, got.Pending)
		}
		if resp, ok := GetVerifyTrailResponse(ctx, trail.TrailId); !ok || resp != `{"resp":1}` {
			t.Fatalf("cached response = %q %v", resp, ok)
		}

		// confirm the pending hop and assign the next
		confirmed := &VerifyTrailHop{
			ClientId: trail.Pending.ClientId, AssignedMs: trail.Pending.AssignedMs,
			ConfirmedMs: nowMs + 100, AssignN: trail.Pending.AssignN,
			EgressIpHash: [32]byte{0xEE, 0x02},
		}
		newPending := &VerifyTrailHop{ClientId: server.NewId(), AssignedMs: nowMs + 100, AssignN: 5}
		ConfirmVerifyHopAndAssign(ctx, trail, confirmed, newPending, `{"resp":2}`, settings)

		got = GetVerifyTrail(ctx, trail.TrailId)
		if len(got.Hops) != 2 || got.Hops[1].ConfirmedMs != nowMs+100 ||
			got.Hops[1].EgressIpHash != confirmed.EgressIpHash ||
			got.Pending == nil || got.Pending.ClientId != newPending.ClientId ||
			got.ActivityMs != nowMs+100 {
			t.Fatalf("after confirm = %+v / %+v", got.Hops, got.Pending)
		}
		if resp, _ := GetVerifyTrailResponse(ctx, trail.TrailId); resp != `{"resp":2}` {
			t.Fatalf("response after confirm = %q", resp)
		}
		wantDeadline := float64(newPending.AssignedMs) + float64((settings.StepTimeout+settings.StepTimeoutGrace)/time.Millisecond)
		if score, ok := testVerifyReapScore(ctx, trail.TrailId); !ok || score != wantDeadline {
			t.Fatalf("reap score = %f %v, want %f", score, ok, wantDeadline)
		}

		// complete: pending cleared, reap entry dropped, final response cached
		final := &VerifyTrailHop{
			ClientId: newPending.ClientId, AssignedMs: newPending.AssignedMs,
			ConfirmedMs: nowMs + 200, AssignN: newPending.AssignN,
		}
		CompleteVerifyTrail(ctx, trail, final, `{"resp":3}`, settings)
		got = GetVerifyTrail(ctx, trail.TrailId)
		if got.Status != VerifyTrailStatusComplete || got.Pending != nil || len(got.Hops) != 3 {
			t.Fatalf("after complete = %+v", got)
		}
		if resp, _ := GetVerifyTrailResponse(ctx, trail.TrailId); resp != `{"resp":3}` {
			t.Fatalf("final response = %q (idempotent replay source)", resp)
		}
		if _, ok := testVerifyReapScore(ctx, trail.TrailId); ok {
			t.Fatal("completed trail must leave the reap registry")
		}

		if GetVerifyTrail(ctx, server.NewId()) != nil {
			t.Fatal("unknown trail must be nil")
		}
	})
}

func TestVerifyTrailRowInsertIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		completeTime := server.NowUtc()
		row := &VerifyTrailRow{
			TrailId: server.NewId(), Vpk: []byte("vpk-row"), ServerKeyId: 2,
			ServerNonce: []byte("nonce"), Depth: 3, Status: VerifyTrailRowStatusComplete,
			HopsJson: `[{"client_id":"x"}]`, FinalSig: []byte("fsig"), VerifierSig: []byte("vsig"),
			CreateTime: completeTime.Add(-time.Minute), CompleteTime: &completeTime,
		}
		InsertVerifyTrail(ctx, row)
		// the reaper/inline-failure race retries with different content — the
		// first durable record must win
		dup := *row
		dup.Depth = 99
		InsertVerifyTrail(ctx, &dup)

		got := GetVerifyTrailRow(ctx, row.TrailId)
		if got == nil || got.Depth != 3 || got.Status != VerifyTrailRowStatusComplete ||
			string(got.FinalSig) != "fsig" || string(got.VerifierSig) != "vsig" ||
			got.CompleteTime == nil {
			t.Fatalf("row roundtrip = %+v", got)
		}
		if GetVerifyTrailRow(ctx, server.NewId()) != nil {
			t.Fatal("unknown row must be nil")
		}
	})
}

func TestSweepExpiredVerifyTrails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		now := server.NowUtc()
		nowMs := uint64(now.UnixMilli())
		deadlineMs := uint64((settings.StepTimeout + settings.StepTimeoutGrace) / time.Millisecond)

		// a real trail whose pending step deadline passed → expired + persisted
		expired := testVerifyBuildTrail(false, nowMs-deadlineMs-1000)
		CreateVerifyTrail(ctx, expired, `{}`, settings)
		IncrVerifyActiveTrails(ctx, expired.Vpk, settings)

		// a poison trail in the same state → expired but never persisted (§9)
		poison := testVerifyBuildTrail(true, nowMs-deadlineMs-1000)
		CreateVerifyTrail(ctx, poison, `{}`, settings)

		// a live trail with a stale registry score → re-scored, not expired
		live := testVerifyBuildTrail(false, nowMs)
		CreateVerifyTrail(ctx, live, `{}`, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.ZAdd(ctx, verifyReapKey, redis.Z{
				Score:  float64(nowMs - 1000),
				Member: live.TrailId.String(),
			}).Err())
		})

		swept := SweepExpiredVerifyTrails(ctx, now, settings)
		if swept != 2 {
			t.Fatalf("swept = %d, want 2 (real + poison)", swept)
		}

		if got := GetVerifyTrail(ctx, expired.TrailId); got.Status != VerifyTrailStatusExpired {
			t.Fatalf("expired trail status = %s", got.Status)
		}
		row := GetVerifyTrailRow(ctx, expired.TrailId)
		if row == nil || row.Status != VerifyTrailRowStatusExpired || row.Depth != 1 ||
			row.CompleteTime != nil {
			t.Fatalf("expired durable row = %+v", row)
		}
		// the expired trail released its active slot (decr floors at the sweep)
		if got := IncrVerifyActiveTrails(ctx, expired.Vpk, settings); got != 1 {
			t.Fatalf("active slots after sweep = %d, want 1", got)
		}

		if GetVerifyTrailRow(ctx, poison.TrailId) != nil {
			t.Fatal("poison trail must never be durably persisted")
		}
		if got := GetVerifyTrail(ctx, poison.TrailId); got.Status != VerifyTrailStatusExpired {
			t.Fatalf("poison trail status = %s", got.Status)
		}

		// the live trail survived with its registry entry moved to the real deadline
		if got := GetVerifyTrail(ctx, live.TrailId); got.Status != VerifyTrailStatusActive {
			t.Fatalf("live trail status = %s", got.Status)
		}
		wantDeadline := float64(live.Pending.AssignedMs) + float64(deadlineMs)
		if score, ok := testVerifyReapScore(ctx, live.TrailId); !ok || score != wantDeadline {
			t.Fatalf("re-scored deadline = %f %v, want %f", score, ok, wantDeadline)
		}

		// a terminal trail left in the registry is dropped without processing
		completed := testVerifyBuildTrail(false, nowMs)
		CreateVerifyTrail(ctx, completed, `{}`, settings)
		CompleteVerifyTrail(ctx, completed, completed.Pending, `{}`, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.ZAdd(ctx, verifyReapKey, redis.Z{
				Score:  float64(nowMs - 1000),
				Member: completed.TrailId.String(),
			}).Err())
		})
		if swept := SweepExpiredVerifyTrails(ctx, now, settings); swept != 0 {
			t.Fatalf("terminal sweep = %d, want 0", swept)
		}
		if _, ok := testVerifyReapScore(ctx, completed.TrailId); ok {
			t.Fatal("terminal trail's stale registry entry must be dropped")
		}
		if GetVerifyTrailRow(ctx, completed.TrailId) != nil {
			t.Fatal("the sweep must not persist a row for a terminal trail")
		}
	})
}

func TestVerifyStatsRollupIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings() // StatsPeriod = 15m
		clientId := server.NewId()
		base := time.Date(2026, 7, 1, 10, 7, 0, 0, time.UTC) // inside [10:00, 10:15)

		for range 3 {
			RecordVerifyAssignment(ctx, clientId, base, settings)
		}
		RecordVerifyConfirmation(ctx, clientId, 120, base, settings)
		RecordVerifyConfirmation(ctx, clientId, 480, base, settings)

		RollupVerifyProviderStats(ctx, base, settings)
		rows := GetVerifyProviderStats(ctx, clientId)
		if len(rows) != 1 || rows[0].Assignments != 3 || rows[0].Confirmations != 2 {
			t.Fatalf("rollup rows = %+v", rows)
		}
		if rows[0].LatencyP50Ms == nil || rows[0].LatencyP90Ms == nil || rows[0].LatencyP99Ms == nil ||
			*rows[0].LatencyP50Ms > *rows[0].LatencyP90Ms || *rows[0].LatencyP90Ms > *rows[0].LatencyP99Ms {
			t.Fatalf("latency percentiles = %v %v %v", rows[0].LatencyP50Ms, rows[0].LatencyP90Ms, rows[0].LatencyP99Ms)
		}
		wantStart := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		if !rows[0].PeriodStart.UTC().Equal(wantStart) {
			t.Fatalf("period start = %v, want %v", rows[0].PeriodStart, wantStart)
		}

		// re-running the rollup writes the same absolute values (idempotent)
		RollupVerifyProviderStats(ctx, base, settings)
		if rows = GetVerifyProviderStats(ctx, clientId); len(rows) != 1 || rows[0].Assignments != 3 {
			t.Fatalf("second rollup rows = %+v (must not double-count)", rows)
		}

		// more activity in the same period updates the absolute row
		RecordVerifyConfirmation(ctx, clientId, 200, base.Add(time.Minute), settings)
		RollupVerifyProviderStats(ctx, base.Add(time.Minute), settings)
		if rows = GetVerifyProviderStats(ctx, clientId); rows[0].Confirmations != 3 {
			t.Fatalf("updated rollup = %+v", rows)
		}

		// the next period rolls up alongside the previous one, ordered ascending
		next := base.Add(settings.StatsPeriod)
		RecordVerifyAssignment(ctx, clientId, next, settings)
		RollupVerifyProviderStats(ctx, next, settings)
		rows = GetVerifyProviderStats(ctx, clientId)
		if len(rows) != 2 || rows[0].Assignments != 3 || rows[1].Assignments != 1 {
			t.Fatalf("two-period rows = %+v", rows)
		}

		// once both redis windows age out, the client leaves the pending set
		// (the pg rows stay)
		RollupVerifyProviderStats(ctx, base.Add(3*settings.StatsPeriod), settings)
		var pending bool
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			pending, err = r.SIsMember(ctx, verifyStatClientsKey, clientId.String()).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
		})
		if pending {
			t.Fatal("aged-out client must leave the pending rollup set")
		}
		if rows = GetVerifyProviderStats(ctx, clientId); len(rows) != 2 {
			t.Fatalf("durable rows after ageout = %+v", rows)
		}
	})
}

func TestSampleVerifyNextHop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings() // EligibilityBurst = 2
		// D26: the default EligibilityInterval of 0 disables the token bucket
		// (every provider always assignable). Force the bucket ON so this test
		// keeps covering the §5.3 burst accounting.
		settings.EligibilityInterval = time.Minute

		providerA, providerB := server.NewId(), server.NewId()
		testVerifySetProvideModes(ctx, providerA)
		testVerifySetProvideModes(ctx, providerB)
		FeedVerifyEgress(ctx, providerA, netip.MustParseAddr("192.0.2.10"), settings)
		FeedVerifyEgress(ctx, providerB, netip.MustParseAddr("192.0.2.20"), settings)
		if !testVerifyEligible(ctx, providerA) || !testVerifyEligible(ctx, providerB) {
			t.Fatal("both providers must be eligible")
		}

		// excluding A leaves only B drawable, with n = |set| - |excluded∩set|
		for draw := 0; draw < 2; draw++ {
			nextHop, n := SampleVerifyNextHop(ctx, []server.Id{providerA}, settings)
			if nextHop == nil || *nextHop != providerB || n != 1 {
				t.Fatalf("draw %d = %v n=%d, want B n=1", draw, nextHop, n)
			}
		}
		// B's eligibility burst (2) is spent: the sample finds no assignable hop
		if nextHop, _ := SampleVerifyNextHop(ctx, []server.Id{providerA}, settings); nextHop != nil {
			t.Fatalf("token-exhausted draw = %v, want nil", nextHop)
		}
		// excluding everyone yields n = 0
		if nextHop, n := SampleVerifyNextHop(ctx, []server.Id{providerA, providerB}, settings); nextHop != nil || n != 0 {
			t.Fatalf("fully excluded draw = %v n=%d", nextHop, n)
		}

		// the poison-path pad reports the set size without touching real tokens:
		// A (never drawn above) still holds its full burst afterwards
		if n := PadVerifySample(ctx, settings); n != 2 {
			t.Fatalf("pad n = %d, want 2", n)
		}
		if !CheckVerifyEligibilityToken(ctx, providerA, settings) {
			t.Fatal("pad must not spend a real provider's tokens")
		}
	})
}
