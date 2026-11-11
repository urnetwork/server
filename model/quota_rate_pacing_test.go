package model

// FS-47's pacing clauses on the real {sq_} script (QUOTA-M2-03 and QUOTA-M2-14; §20.7's closure and wait
// rows as DESIGN-DETAILS 13.2.1 gives them), on a fake clock against the private stack's Redis: k erasers
// asking all the while each take one ?closure=1 upload every k/R s, none starved, the network at most R
// a second; erasers whose allowance ran out return together at 00:00 UTC, ask in one fixed order every
// time, and each is served in its turn; an eraser that obeys a retry_after_ms over a minute keeps its
// place against erasers re-asking every second, since its seat, not its asking, holds the place; the
// script answers as sidecar.QuotaClosurePacing does on fixed-seed runs; and only a pass is recorded.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// R at the fixed rate: evs_closure_uploads_per_s reads at least 4 (owner decision 146).
const testingPacingPerS = int64(4)

const testingPacingSpacing = time.Second / time.Duration(testingPacingPerS)

// One upload of byteCount by eraserId, as ChargeClosure counts it: nil when it took, else the refusal.
func testingAskClosureUpload(ctx context.Context, t testing.TB, network *testingRateNetwork, eraserId server.Id, byteCount int64) *sidecar.QuotaRefusal {
	refusal, err := network.count(ctx, testingChargeClosure(eraserId, byteCount))
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

// k erasers asking every fifth of a slot take turns k/R s apart, after the first eraser's two at once,
// none starved, the network never past R in a second.
func TestClosurePacingServesKErasersEveryKOverR(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		for _, k := range []int{4, 5, 8, 16} {
			clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
			network := newTestingRateNetwork(nil, clock, map[string]int64{"evs_closure_bytes_per_day": 64 * 1024 * 1024})
			eraserIds := []server.Id{}
			for range k {
				eraserIds = append(eraserIds, server.NewId())
			}
			duration := 24 * time.Second
			step := testingPacingSpacing / 5
			eraserTakeTimes := map[server.Id][]time.Time{}
			secondTakeCounts := map[int64]int64{}
			for i := range int(duration / step) {
				if 0 < i {
					clock.Advance(step)
				}
				now := clock.Now()
				for _, eraserId := range eraserIds {
					refusal := testingAskClosureUpload(ctx, t, network, eraserId, 1024)
					if refusal == nil {
						eraserTakeTimes[eraserId] = append(eraserTakeTimes[eraserId], now)
						secondTakeCounts[now.Unix()] += 1
					} else if refusal.LimitKey != "evs_closure_uploads_per_s" {
						t.Fatalf("k=%d: refused %+v", k, refusal)
					}
				}
			}
			for second, count := range secondTakeCounts {
				if testingPacingPerS < count {
					t.Fatalf("k=%d: second %d took %d", k, second, count)
				}
			}
			turn := time.Duration(k) * testingPacingSpacing
			for index, eraserId := range eraserIds {
				takeTimes := eraserTakeTimes[eraserId]
				if len(takeTimes) < int(duration/turn)-1 {
					t.Fatalf("k=%d: eraser %d took %d turns in %s", k, index, len(takeTimes), duration)
				}
				first := 1
				if index == 0 {
					first = 2
				}
				for i := first; i < len(takeTimes); i += 1 {
					if gap := takeTimes[i].Sub(takeTimes[i-1]); gap != turn {
						t.Fatalf("k=%d: eraser %d's turn %d came %s after the last, want %s", k, index, i, gap, turn)
					}
				}
			}
		}
	})
}

// Erasers whose allowances ran out before midnight, refused with retry_after_ms to 00:00 UTC, all come
// back at once: their allowances are whole again, the network takes at most R a second, and each gets
// its uploads within the turns its share of R allows. The askers of each instant ask in one fixed order,
// the same every time, the order a bucket admitting by arrival let the first four win every turn in
// (DESIGN-DETAILS 13.1 A.1): an allowance refusal gives no seat, and seats admit by place, not arrival.
func TestClosurePacingMidnightStampede(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		clock := sidecartest.NewFakeClock(time.Date(2026, time.May, 6, 23, 58, 0, 0, time.UTC))
		allowance := int64(4 * 1024 * 1024)
		upload := int64(64 * 1024)
		network := newTestingRateNetwork(nil, clock, map[string]int64{
			"evs_closure_bytes_per_day": allowance,
			"evs_closure_bytes_floor":   4 * 1024 * 1024,
		})
		eraserCount := 3 * int(testingPacingPerS)
		eraserIds := []server.Id{}
		for range eraserCount {
			eraserId := server.NewId()
			eraserIds = append(eraserIds, eraserId)
			// the day's allowance spent on CLOSURE pushes' inline records, which take no turn
			refusal, err := network.count(ctx, map[string]int64{sidecar.QuotaRateKeyClosureBytes(connect.Id(eraserId)): allowance})
			if err != nil || refusal != nil {
				t.Fatalf("the allowance: %+v, %v", refusal, err)
			}
		}
		clock.AdvanceTo(time.Date(2026, time.May, 6, 23, 59, 30, 0, time.UTC))
		midnight := time.Date(2026, time.May, 7, 0, 0, 0, 0, time.UTC)
		eraserNextAskTimes := map[server.Id]time.Time{}
		for _, eraserId := range eraserIds {
			refusal := testingAskClosureUpload(ctx, t, network, eraserId, upload)
			if refusal == nil || refusal.LimitKey != "evs_closure_bytes_per_day" {
				t.Fatalf("an upload past the allowance answered %+v", refusal)
			}
			if refusal.RetryAfterMs != sidecar.QuotaRetryAfterMs(clock.Now(), midnight) {
				t.Fatalf("retry_after_ms %d, want to 00:00 UTC", refusal.RetryAfterMs)
			}
			eraserNextAskTimes[eraserId] = clock.Now().Add(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
		}

		// every eraser obeys each retry_after_ms until it has its uploads
		uploadCount := 3
		eraserTakeCounts := map[server.Id]int{}
		eraserDoneTimes := map[server.Id]time.Time{}
		secondTakeCounts := map[int64]int64{}
		for range 10000 {
			if len(eraserDoneTimes) == eraserCount {
				break
			}
			var next time.Time
			for _, eraserId := range eraserIds {
				if _, done := eraserDoneTimes[eraserId]; done {
					continue
				}
				if askTime := eraserNextAskTimes[eraserId]; next.IsZero() || askTime.Before(next) {
					next = askTime
				}
			}
			clock.AdvanceTo(next)
			now := clock.Now()
			for _, eraserId := range eraserIds {
				if _, done := eraserDoneTimes[eraserId]; done || eraserNextAskTimes[eraserId].After(now) {
					continue
				}
				refusal := testingAskClosureUpload(ctx, t, network, eraserId, upload)
				if refusal == nil {
					secondTakeCounts[now.Unix()] += 1
					eraserTakeCounts[eraserId] += 1
					if eraserTakeCounts[eraserId] == uploadCount {
						eraserDoneTimes[eraserId] = now
					}
					eraserNextAskTimes[eraserId] = now
					continue
				}
				if refusal.LimitKey != "evs_closure_uploads_per_s" {
					t.Fatalf("after midnight an eraser was refused %+v", refusal)
				}
				eraserNextAskTimes[eraserId] = now.Add(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
			}
		}
		connect.AssertEqual(t, len(eraserDoneTimes), eraserCount)
		for second, count := range secondTakeCounts {
			if testingPacingPerS < count {
				t.Fatalf("second %d took %d", second, count)
			}
			if second < midnight.Unix() {
				t.Fatalf("an upload took before midnight, at second %d", second)
			}
		}
		// R a second serve every upload within its share, a second of slack for the stampede's first
		bound := midnight.Add(time.Duration(eraserCount*uploadCount/int(testingPacingPerS)+1) * time.Second)
		for index, eraserId := range eraserIds {
			if doneTime := eraserDoneTimes[eraserId]; bound.Before(doneTime) {
				t.Fatalf("eraser %d finished at %s, past %s", index, doneTime, bound)
			}
		}
	})
}

// An eraser that waits out a retry_after_ms of over a minute keeps its place: erasers re-asking every
// second neither pass it nor starve it, and it takes within a second of its turn.
func TestClosurePacingObedientEraserKeepsItsPlace(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		start := time.Date(2026, time.May, 6, 10, 0, 0, 0, time.UTC)
		clock := sidecartest.NewFakeClock(start)
		network := newTestingRateNetwork(nil, clock, map[string]int64{"evs_closure_bytes_per_day": 64 * 1024 * 1024})
		reAskerIds := []server.Id{}
		for range 250 {
			reAskerIds = append(reAskerIds, server.NewId())
		}
		reAsk := func() {
			for _, reAskerId := range reAskerIds {
				refusal := testingAskClosureUpload(ctx, t, network, reAskerId, 1024)
				if refusal != nil && refusal.LimitKey != "evs_closure_uploads_per_s" {
					t.Fatalf("a re-asker was refused %+v", refusal)
				}
			}
		}
		reAsk()
		obedientId := server.NewId()
		refusal := testingAskClosureUpload(ctx, t, network, obedientId, 1024)
		if refusal == nil || refusal.LimitKey != "evs_closure_uploads_per_s" {
			t.Fatalf("the obedient eraser's first ask answered %+v", refusal)
		}
		wait := time.Duration(refusal.RetryAfterMs) * time.Millisecond
		if wait <= time.Minute {
			t.Fatalf("a wait of %s, not over a minute", wait)
		}
		turnTime := clock.Now().Add(wait)
		obedientAskTime := turnTime
		obedientAskCount := 1
		taken := false
		for !taken {
			nextSecond := time.Unix(clock.Now().Unix()+1, 0).UTC()
			if obedientAskTime.Before(nextSecond) {
				clock.AdvanceTo(obedientAskTime)
				obedientAskCount += 1
				refusal := testingAskClosureUpload(ctx, t, network, obedientId, 1024)
				if refusal == nil {
					taken = true
					break
				}
				if refusal.LimitKey != "evs_closure_uploads_per_s" || time.Second < time.Duration(refusal.RetryAfterMs)*time.Millisecond {
					t.Fatalf("at its turn the obedient eraser was refused %+v", refusal)
				}
				obedientAskTime = clock.Now().Add(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
				continue
			}
			clock.AdvanceTo(nextSecond)
			// the re-askers ask first at the second's start, the order least kind to the obedient eraser
			reAsk()
			if obedientAskTime.Equal(clock.Now()) {
				obedientAskCount += 1
				if refusal := testingAskClosureUpload(ctx, t, network, obedientId, 1024); refusal == nil {
					taken = true
				} else {
					obedientAskTime = clock.Now().Add(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
				}
			}
			if turnTime.Add(2 * time.Second).Before(clock.Now()) {
				t.Fatalf("the obedient eraser had not taken by %s, its turn %s, after %d asks", clock.Now(), turnTime, obedientAskCount)
			}
		}
		if turnTime.Add(time.Second).Before(clock.Now()) {
			t.Fatalf("the obedient eraser took at %s, past a second after its turn %s", clock.Now(), turnTime)
		}
		if 3 < obedientAskCount {
			t.Fatal(fmt.Sprintf("the obedient eraser asked %d times", obedientAskCount))
		}
	})
}

// The script against sidecar.QuotaClosurePacing, which FakeQuota runs, on fixed-seed runs of paced uploads
// for R = 4, 6 and 7 (R reads at least 4), R not dividing 10^6 included, from two callers whose clocks are
// 2 ms apart: every
// answer, its used and retry_after_ms included, is the reference's (DESIGN-DETAILS 13.6). Each eraser's 60 a
// minute is judged first, as the script judges its counters before it asks the pacing, and its counter keeps
// one window, as the script's does.
func TestClosurePacingScriptFollowsTheReference(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		type minuteCount struct {
			window int64
			used   int64
		}
		for _, perS := range []int64{4, 6, 7} {
			limits := &sidecar.QuotaLimits{Tier: string(NetworkTierFree), Limits: map[string]int64{"evs_closure_uploads_per_s": perS}}
			counter := NewQuotaRateCounter(nil, nil)
			networkId := server.NewId()
			reference := sidecar.NewQuotaClosurePacing()
			random := rand.New(rand.NewPCG(uint64(perS), 657))
			eraserIds := []server.Id{}
			for range 6 {
				eraserIds = append(eraserIds, server.NewId())
			}
			eraserMinuteCounts := map[server.Id]*minuteCount{}
			takeCount := 0
			now := time.Date(2026, time.May, 6, 10, 0, 0, 123456*1000, time.UTC)
			for step := range 1500 {
				now = now.Add(time.Duration(random.IntN(150*1000)) * time.Microsecond)
				callerNow := now
				if random.IntN(3) == 0 {
					callerNow = now.Add(-2 * time.Millisecond)
				}
				eraserId := eraserIds[random.IntN(len(eraserIds))]
				refusal, err := counter.CountRate(ctx, networkId, limits, map[string]int64{sidecar.QuotaRateKeyClosureUpload(connect.Id(eraserId)): 1}, server.NewId(), callerNow)
				if err != nil {
					t.Fatal(err)
				}
				window := sidecar.QuotaRateWindowMinute.Index(callerNow)
				used := int64(0)
				if count, ok := eraserMinuteCounts[eraserId]; ok && count.window == window {
					used = count.used
				}
				var want *sidecar.QuotaRefusal
				if 60 <= used {
					want = &sidecar.QuotaRefusal{
						LimitKey:     "evs_closure_uploads_per_min",
						Limit:        60,
						Used:         used,
						RetryAfterMs: sidecar.QuotaRetryAfterMs(callerNow, sidecar.QuotaRateWindowMinute.End(window)),
					}
				} else if ok, seatTime, taken := reference.Ask(connect.Id(eraserId), callerNow, perS); !ok {
					want = sidecar.QuotaClosurePacingRefusal(limits, taken, reference.Clock(), seatTime)
				} else {
					reference.Take(connect.Id(eraserId), callerNow, perS)
					eraserMinuteCounts[eraserId] = &minuteCount{window: window, used: used + 1}
					takeCount += 1
				}
				if (refusal == nil) != (want == nil) || (refusal != nil && *refusal != *want) {
					t.Fatalf("R = %d, step %d: the script answered %+v, the reference %+v", perS, step, refusal, want)
				}
			}
			if takeCount == 0 {
				t.Fatalf("R = %d: nothing took", perS)
			}
		}
	})
}

// Only a pass is recorded under its debit id: an upload refused for its turn leaves nothing there, comes back
// at its seat under the same id and takes, and its replay counts nothing (QD-652; evsservice's one debit id
// an upload).
func TestClosurePacingRecordsOnlyAPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetEnforceSidecarLimits(true)()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		network := newTestingRateNetwork(nil, clock, map[string]int64{"evs_closure_bytes_per_day": 64 * 1024 * 1024})
		if refusal := testingAskClosureUpload(ctx, t, network, server.NewId(), 1024); refusal != nil {
			t.Fatalf("the first eraser: %+v", refusal)
		}
		eraserId := server.NewId()
		debitId := server.NewId()
		keyAmounts := testingChargeClosure(eraserId, 1024)
		refusal, err := network.counter.CountRate(ctx, network.networkId, network.limits, keyAmounts, debitId, clock.Now())
		connect.AssertEqual(t, err, nil)
		if refusal == nil || refusal.LimitKey != "evs_closure_uploads_per_s" || refusal.RetryAfterMs != 500 {
			t.Fatalf("the second eraser: %+v", refusal)
		}
		_, found := testingRedisGet(ctx, quotaRateDebitKey(network.networkId, debitId))
		connect.AssertEqual(t, found, false)
		clock.Advance(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
		for range 2 {
			refusal, err = network.counter.CountRate(ctx, network.networkId, network.limits, keyAmounts, debitId, clock.Now())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, refusal, (*sidecar.QuotaRefusal)(nil))
		}
		record, found := testingRedisGet(ctx, quotaRateDebitKey(network.networkId, debitId))
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, record, quotaRatePassedRecord)
		value, _ := testingRedisGet(ctx, quotaRateCounterKey(network.networkId, sidecar.QuotaRateKeyClosureBytes(connect.Id(eraserId))))
		connect.AssertEqual(t, strings.HasSuffix(value, ":1024"), true)
		// the replay took no second seat: the next upload waits one cell, not two
		refusal = testingAskClosureUpload(ctx, t, network, eraserId, 1024)
		if refusal == nil || refusal.LimitKey != "evs_closure_uploads_per_s" || refusal.RetryAfterMs != 250 {
			t.Fatalf("the next upload: %+v", refusal)
		}
	})
}
