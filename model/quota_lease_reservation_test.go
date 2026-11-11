package model

// QUOTA-M2-15: the reservation script's lease operations and the credit floor (DESIGN-DETAILS 13.2.2,
// 13.6's quota_lease_reservation_test.go row) against the private stack's Redis, through
// .stack/server-test.sh. A carve moves once whatever copies of it arrive; extend answers a token's
// amount; a reservation is clamped by the floor however old its reading, the twin LeaseTwin-NoFloor's
// overdraft refused; a floor is lowered, never raised, and set again after it expires; six-key callers
// read and write no floor; and every lease operation lies below the grant probe's cut. The flush halves
// are service_debit_flush_test.go's and TestTransferDebitFlushLowersTheFloor's here.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A lease host's token lifetime in these tests: DESIGN-DETAILS 13.2.2's 15 min.
const testingLeaseTokenTtl = 15 * time.Minute

// What one balance's reservation keys hold.
type testingLeaseReservationState struct {
	total        string
	tokenAmounts map[string]string
	tokenExpiry  map[string]float64
	floor        string
	floorTtl     time.Duration
}

func testingReadLeaseReservation(ctx context.Context, balanceId server.Id) *testingLeaseReservationState {
	keys := redisLeaseReservationKeys(balanceId)
	state := &testingLeaseReservationState{tokenExpiry: map[string]float64{}}
	server.Redis(ctx, func(client server.RedisClient) {
		total, err := client.Get(ctx, keys[0]).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		state.total = total
		state.tokenAmounts, err = client.HGetAll(ctx, keys[1]).Result()
		server.Raise(err)
		scores, err := client.ZRangeWithScores(ctx, keys[2], 0, -1).Result()
		server.Raise(err)
		for _, score := range scores {
			state.tokenExpiry[score.Member.(string)] = score.Score
		}
		floor, err := client.Get(ctx, keys[6]).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		state.floor = floor
		state.floorTtl, err = client.PTTL(ctx, keys[6]).Result()
		server.Raise(err)
	})
	return state
}

// The flush's release of token, with the balance its debit left.
func testingReleaseDebited(ctx context.Context, balanceId server.Id, token string, balanceByteCount ByteCount) {
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.Eval(ctx, redisContractReservationScript, redisLeaseReservationKeys(balanceId),
			"release-debited", token, balanceByteCount, "0", redisContractReservationLease.Milliseconds(),
			DefaultLeaseReservationSettings().FloorTtl.Milliseconds()).Err())
	})
}

// api's contract admission's reservation: six keys, its own reading.
func testingReserveSixKeys(ctx context.Context, balanceId server.Id, token string, credit ByteCount, requested ByteCount) (amount ByteCount) {
	server.Redis(ctx, func(client server.RedisClient) {
		value, err := client.Eval(ctx, redisContractReservationScript, redisContractReservationKeys(balanceId),
			"reserve", token, credit, requested, redisContractReservationLease.Milliseconds()).Int64()
		server.Raise(err)
		amount = value
	})
	return
}

func testingReserveLease(t testing.TB, ctx context.Context, redis QuotaRateRedis, balanceId server.Id, token string, credit ByteCount, requested ByteCount) ByteCount {
	t.Helper()
	amount, err := ReserveLease(ctx, redis, balanceId, token, credit, requested, testingLeaseTokenTtl, DefaultLeaseReservationSettings().FloorTtl)
	if err != nil {
		t.Fatal(err)
	}
	return amount
}

func testingCarveLease(t testing.TB, ctx context.Context, redis QuotaRateRedis, balanceId server.Id, usageId server.Id, parts ...*LeaseCarvePart) (bool, ByteCount) {
	t.Helper()
	moved, byteCount, err := CarveLease(ctx, redis, balanceId, usageId, parts, DefaultLeaseReservationSettings().CarvedTokenTtl, testingLeaseTokenTtl)
	if err != nil {
		t.Fatal(err)
	}
	return moved, byteCount
}

// A carve moves once: a replay finds its destination, a copy arriving after the destination's release
// finds its source below its expect, a source short of its expect moves nothing and writes nothing, and
// the total never changes (twin LeaseTwin-BlindCarve).
func TestCarveMovesOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		redis := NewQuotaRateServerRedis(10 * time.Second)
		balanceId := server.NewId()
		leaseId := server.NewId()
		first := QuotaLeaseToken(leaseId, 1)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, balanceId, first, 1000, 300), ByteCount(300))

		usageId := server.NewId()
		moved, byteCount := testingCarveLease(t, ctx, redis, balanceId, usageId, &LeaseCarvePart{Token: first, ByteCount: 100, ExpectByteCount: 300})
		connect.AssertEqual(t, moved, true)
		connect.AssertEqual(t, byteCount, ByteCount(100))
		state := testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.tokenAmounts, map[string]string{first: "200", QuotaCarveToken(usageId): "100"})
		connect.AssertEqual(t, state.total, "300")

		// the answer was lost: the same carve again finds its destination and moves nothing
		moved, byteCount = testingCarveLease(t, ctx, redis, balanceId, usageId, &LeaseCarvePart{Token: first, ByteCount: 100, ExpectByteCount: 300})
		connect.AssertEqual(t, moved, true)
		connect.AssertEqual(t, byteCount, ByteCount(100))
		connect.AssertEqual(t, testingReadLeaseReservation(ctx, balanceId).tokenAmounts[first], "200")

		// the flush released the carved token; a stray third copy finds the source below its expect
		testingReleaseDebited(ctx, balanceId, QuotaCarveToken(usageId), 900)
		moved, byteCount = testingCarveLease(t, ctx, redis, balanceId, usageId, &LeaseCarvePart{Token: first, ByteCount: 100, ExpectByteCount: 300})
		connect.AssertEqual(t, moved, false)
		connect.AssertEqual(t, byteCount, ByteCount(0))
		state = testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.tokenAmounts, map[string]string{first: "200"})
		connect.AssertEqual(t, state.total, "200")

		// a source short of its expect moves nothing and writes no destination
		shortId := server.NewId()
		moved, _ = testingCarveLease(t, ctx, redis, balanceId, shortId, &LeaseCarvePart{Token: first, ByteCount: 50, ExpectByteCount: 250})
		connect.AssertEqual(t, moved, false)
		connect.AssertEqual(t, testingReadLeaseReservation(ctx, balanceId).tokenAmounts, map[string]string{first: "200"})

		// two draws into one row: an emptied source leaves, the other shrinks, the total stays
		second := QuotaLeaseToken(leaseId, 2)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, balanceId, second, 900, 100), ByteCount(100))
		bothId := server.NewId()
		moved, byteCount = testingCarveLease(t, ctx, redis, balanceId, bothId,
			&LeaseCarvePart{Token: first, ByteCount: 200, ExpectByteCount: 200},
			&LeaseCarvePart{Token: second, ByteCount: 30, ExpectByteCount: 100},
		)
		connect.AssertEqual(t, moved, true)
		connect.AssertEqual(t, byteCount, ByteCount(230))
		state = testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.tokenAmounts, map[string]string{second: "70", QuotaCarveToken(bothId): "230"})
		connect.AssertEqual(t, state.total, "300")
		if _, ok := state.tokenExpiry[first]; ok {
			t.Fatal("an emptied source kept its expiry")
		}
		// the carved token lives its 24 h, the source the lease's 15 min
		if gap := time.Duration(state.tokenExpiry[QuotaCarveToken(bothId)]-state.tokenExpiry[second]) * time.Millisecond; gap < 23*time.Hour {
			t.Fatalf("the carved token outlives its source by %s", gap)
		}

		// a part that names one source twice, or expects less than it moves, is the caller's bug
		if _, _, err := CarveLease(ctx, redis, balanceId, server.NewId(), []*LeaseCarvePart{
			{Token: second, ByteCount: 10, ExpectByteCount: 70},
			{Token: second, ByteCount: 10, ExpectByteCount: 70},
		}, DefaultLeaseReservationSettings().CarvedTokenTtl, testingLeaseTokenTtl); err == nil {
			t.Fatal("a carve named one source twice")
		}
		if _, _, err := CarveLease(ctx, redis, balanceId, server.NewId(), []*LeaseCarvePart{{Token: second, ByteCount: 80, ExpectByteCount: 70}}, DefaultLeaseReservationSettings().CarvedTokenTtl, testingLeaseTokenTtl); err == nil {
			t.Fatal("a carve moved more than its expect")
		}
		connect.AssertEqual(t, testingReadLeaseReservation(ctx, balanceId).tokenAmounts[second], "70")
	})
}

// extend sets a token's expiry afresh and answers its amount, which settles a carve whose answer was
// lost, and 0 once the token is gone or Redis lost it.
func TestLeaseExtendAnswersItsAmount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		redis := NewQuotaRateServerRedis(10 * time.Second)
		balanceId := server.NewId()
		token := QuotaLeaseToken(server.NewId(), 1)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, balanceId, token, 1000, 400), ByteCount(400))
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.ZAdd(ctx, redisLeaseReservationKeys(balanceId)[2], testingRedisZ(1e15, token)).Err())
		})
		amount, err := ExtendLease(ctx, redis, balanceId, token, testingLeaseTokenTtl)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, amount, ByteCount(400))
		expiry := testingReadLeaseReservation(ctx, balanceId).tokenExpiry[token]
		now := time.Duration(time.Now().UnixMilli()) * time.Millisecond
		if gap := time.Duration(expiry)*time.Millisecond - now; gap < testingLeaseTokenTtl-time.Minute || testingLeaseTokenTtl+time.Minute < gap {
			t.Fatalf("the extended token expires in %s", gap)
		}

		// a carve whose answer was lost: the extend answers what the token holds now
		testingCarveLease(t, ctx, redis, balanceId, server.NewId(), &LeaseCarvePart{Token: token, ByteCount: 150, ExpectByteCount: 400})
		amount, err = ExtendLease(ctx, redis, balanceId, token, testingLeaseTokenTtl)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, amount, ByteCount(250))

		connect.AssertEqual(t, ReleaseLease(ctx, redis, balanceId, token, testingLeaseTokenTtl), nil)
		amount, err = ExtendLease(ctx, redis, balanceId, token, testingLeaseTokenTtl)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, amount, ByteCount(0))
		if _, ok := testingReadLeaseReservation(ctx, balanceId).tokenAmounts[token]; ok {
			t.Fatal("an extend of a released token wrote it again")
		}
	})
}

// A score for ZADD.
func testingRedisZ(score float64, member string) redis.Z {
	return redis.Z{Score: score, Member: member}
}

// Twin LeaseTwin-NoFloor: balance B holds 100; edge E1 reserves 64, spends and carves them, and the flush
// debits B to 36 and releases the carve, so Redis holds no reservation. Edge E2 asks for 64 with the 100 it
// read before the debit: the floor the release left clamps the grant to 36, where a six-key reservation on
// that reading grants what B no longer holds.
func TestLeaseReservationClampedByFloor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		redis := NewQuotaRateServerRedis(10 * time.Second)
		balanceId := server.NewId()
		e1 := QuotaLeaseToken(server.NewId(), 1)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, balanceId, e1, 100, 64), ByteCount(64))
		usageId := server.NewId()
		moved, _ := testingCarveLease(t, ctx, redis, balanceId, usageId, &LeaseCarvePart{Token: e1, ByteCount: 64, ExpectByteCount: 64})
		connect.AssertEqual(t, moved, true)
		testingReleaseDebited(ctx, balanceId, QuotaCarveToken(usageId), 36)
		state := testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.total, "0")
		connect.AssertEqual(t, state.floor, "36")

		e2 := QuotaLeaseToken(server.NewId(), 1)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, balanceId, e2, 100, 64), ByteCount(36))
		connect.AssertEqual(t, testingReadLeaseReservation(ctx, balanceId).floor, "36")
		// nothing more is left to grant on that reading
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, balanceId, QuotaLeaseToken(server.NewId(), 1), 100, 64), ByteCount(0))
		// the twin: a reservation that trusts the reading, as six keys do, grants 64 of the 36
		connect.AssertEqual(t, testingReserveSixKeys(ctx, balanceId, server.NewId().String(), 100, 64), ByteCount(64))
	})
}

// Twin LeaseTwin-FloorExpires: a floor is lowered to a lower reading, never raised by a higher one, and
// its ttl, FloorTtl (at least twice the 30 s a host uses a reading), is set only when it is lowered, so it
// outlives every reading taken before it was set; once it has expired the next caller sets it again from
// its own reading, as release-debited does from the balance a debit left.
func TestLeaseFloorOutlivesAReading(t *testing.T) {
	floorTtl := DefaultLeaseReservationSettings().FloorTtl
	if floorTtl < 2*30*time.Second {
		t.Fatalf("FloorTtl %s is under twice a host's 30 s MaxCreditAge", floorTtl)
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		redis := NewQuotaRateServerRedis(10 * time.Second)
		balanceId := server.NewId()
		testingReserveLease(t, ctx, redis, balanceId, QuotaLeaseToken(server.NewId(), 1), 500, 10)
		state := testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.floor, "500")
		if state.floorTtl < floorTtl-time.Minute || floorTtl < state.floorTtl {
			t.Fatalf("the floor lives %s", state.floorTtl)
		}
		// shorten the ttl as time passing would: a higher reading neither raises the floor nor resets it
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.PExpire(ctx, redisLeaseReservationKeys(balanceId)[6], 30*time.Second).Err())
		})
		testingReserveLease(t, ctx, redis, balanceId, QuotaLeaseToken(server.NewId(), 1), 800, 10)
		testingReleaseDebited(ctx, balanceId, QuotaLeaseToken(server.NewId(), 9), 700)
		state = testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.floor, "500")
		if 30*time.Second < state.floorTtl {
			t.Fatalf("a reading above the floor set its ttl to %s", state.floorTtl)
		}
		// a lower one lowers it and sets the ttl afresh
		testingReleaseDebited(ctx, balanceId, QuotaLeaseToken(server.NewId(), 9), 400)
		state = testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.floor, "400")
		if state.floorTtl < floorTtl-time.Minute {
			t.Fatalf("a lowered floor lives %s", state.floorTtl)
		}
		// expired, the floor is set again by the next caller's reading
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Del(ctx, redisLeaseReservationKeys(balanceId)[6]).Err())
		})
		testingReserveLease(t, ctx, redis, balanceId, QuotaLeaseToken(server.NewId(), 1), 300, 10)
		connect.AssertEqual(t, testingReadLeaseReservation(ctx, balanceId).floor, "300")
		// a negative balance is shown as 0, which refuses every reading
		amount, err := ReserveLease(ctx, redis, balanceId, QuotaLeaseToken(server.NewId(), 1), -5, 10, testingLeaseTokenTtl, floorTtl)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, amount, ByteCount(0))
		connect.AssertEqual(t, testingReadLeaseReservation(ctx, balanceId).floor, "0")
	})
}

// Callers that pass six keys behave as before: they neither read nor write a floor, whatever it holds.
func TestLeaseFloorLeavesSixKeyCallersAlone(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balanceId := server.NewId()
		floorKey := redisLeaseReservationKeys(balanceId)[6]
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Set(ctx, floorKey, "10", time.Minute).Err())
		})
		token := server.NewId().String()
		connect.AssertEqual(t, testingReserveSixKeys(ctx, balanceId, token, 1000, 500), ByteCount(500))
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Eval(ctx, redisContractReservationScript, redisContractReservationKeys(balanceId),
				"release-debited", token, "1", "0", redisContractReservationLease.Milliseconds(), 1000).Err())
		})
		state := testingReadLeaseReservation(ctx, balanceId)
		connect.AssertEqual(t, state.floor, "10")
		connect.AssertEqual(t, state.total, "0")
		if state.floorTtl < 50*time.Second {
			t.Fatalf("a six-key call touched the floor's ttl: %s", state.floorTtl)
		}
	})
}

// Every lease operation and the floor lie below the line the grant probe cuts its shared prelude at, so the
// probe stays the reservation's read-only twin.
func TestLeaseOperationsLieBelowTheProbeCut(t *testing.T) {
	prelude, body, found := strings.Cut(redisContractReservationScript, "local rawTotal=")
	if !found {
		t.Fatal("the reservation script lost its cut")
	}
	for _, word := range []string{"'extend'", "'carve'", "'release-debited'", "KEYS[7]", "lowerFloor"} {
		if strings.Contains(prelude, word) {
			t.Errorf("%s lies above the cut", word)
		}
		if !strings.Contains(body, word) {
			t.Errorf("%s is missing", word)
		}
		if strings.Contains(redisGrantProbeScript, word) {
			t.Errorf("the probe holds %s", word)
		}
	}
}

// The transfer flush releases with release-debited: after its commit each contract's token goes and the
// balance's floor is what the debit left, read in the debit's own statement.
func TestTransferDebitFlushLowersTheFloor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		server.RunPosts(ctx, posts...)
		connect.AssertEqual(t, testingReadLeaseReservation(ctx, f.balanceId).floor, "")
		applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || applied != 1 || released != 1 || busy {
			t.Fatal("the flush did not complete", applied, released, busy, err)
		}
		state := testingReadLeaseReservation(ctx, f.balanceId)
		connect.AssertEqual(t, state.floor, "989")
		if _, ok := state.tokenAmounts[contract.ContractId.String()]; ok {
			t.Fatal("the debited contract's token stayed")
		}
		// a lease with a reading from before the debit is clamped to what is left
		redis := NewQuotaRateServerRedis(10 * time.Second)
		connect.AssertEqual(t, testingReserveLease(t, ctx, redis, f.balanceId, QuotaLeaseToken(server.NewId(), 1), 1000, 2000), ByteCount(989))
	})
}
