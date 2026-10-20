// Recovery controls use real PG/Redis custody. Only retry time and deliberately
// lost acknowledgements are injected; a fresh public request performs recovery.
package model

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Retain the actual v2 Redis marker after the SQL owner rolls back. The next
// public caller receives neither this context nor any in-memory recovery state.
func redisRecoveryAbandonedTestRequest(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, amount ByteCount) server.Id {
	t.Helper()
	request := withRedisContractAdmission(ctx)
	conn := acquireContractLifecycleTestConnection(t, ctx)
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	server.Raise(err)
	defer tx.Rollback(context.Background())
	escrow, _, err := createTransferEscrowInTx(request, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, amount, nil)
	if err != nil || escrow == nil {
		t.Fatal("abandoned request did not reach real SQL publication", err)
	}
	server.Raise(tx.Rollback(ctx))
	redisRecoveryRequireMarker(t, ctx, f.balanceId, escrow.ContractId, true)
	return escrow.ContractId
}

func redisRecoveryRequireMarker(t testing.TB, ctx context.Context, balance, contract server.Id, present bool) {
	t.Helper()
	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.ZScore(ctx, redisContractReservationKeys(balance)[4], contract.String()).Result()
		if (err == nil) != present || err != nil && !errors.Is(err, redis.Nil) {
			t.Fatalf("retained recovery marker present=%v want=%v err=%v", err == nil, present, err)
		}
	})
}

func TestRedisRecoveryPublicRequestResumesRetainedAbandonedMarker(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("retained abandoned reservation fixture differs", got)
		}
		resumed := createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 46 {
			t.Fatal("fresh public caller did not recover retained marker", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
		server.Redis(ctx, func(client server.RedisClient) {
			values, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
			server.Raise(err)
			if len(values) != 2 || values[neighbor.ContractId.String()] != "17" || values[resumed.ContractId.String()] != "29" {
				t.Fatal("restart recovery changed another request's reservation", values)
			}
		})
	})
}

func TestRedisRecoveryExhaustedCleanupRetainsExactMarkerForNextCaller(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		now := time.Now()
		var attempts int
		cleanup := redisReservationCleanup{now: func() time.Time { return now },
			recover: func(owner context.Context, balance, contract server.Id) error {
				attempts++
				deadline, ok := owner.Deadline()
				if !ok || time.Until(deadline) < time.Minute || balance != f.balanceId || contract != abandoned {
					t.Fatal("cleanup lost its single 300-second owner or original request")
				}
				return syscall.ECONNRESET
			},
			wait: func(context.Context, time.Duration) error {
				// Accelerated logical outage, not a 65-second wall-clock sleep.
				now = now.Add(65 * time.Second)
				return nil
			}}
		err := cleanup.run(ctx, abandoned, []server.Id{f.balanceId})
		if !errors.Is(err, errRedisReservationCleanupPending) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, syscall.ECONNRESET) || attempts != 5 {
			t.Fatal("exhausted cleanup was reported successful or had multiplied budgets", attempts, err)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 23 {
			t.Fatal("failed cleanup erased retained obligation", got)
		}
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 29 {
			t.Fatal("next caller left exhausted cleanup to lease expiry", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
	})
}

func TestRedisRecoveryLostReleaseAcknowledgementRetriesExactRequest(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		now := time.Now()
		var attempts int
		cleanup := redisReservationCleanup{now: func() time.Time { return now },
			recover: func(owner context.Context, balance, contract server.Id) error {
				attempts++
				if owner.Err() != nil || balance != f.balanceId || contract != abandoned {
					t.Fatal("canceled caller or retry replaced cleanup ownership", owner.Err())
				}
				if err := recoverRedisReservationRequest(owner, balance, contract); err != nil {
					return err
				}
				if attempts == 1 {
					return io.EOF // The actual exact-token release already succeeded.
				}
				return nil
			}, wait: func(context.Context, time.Duration) error {
				now = now.Add(65 * time.Second)
				server.Tx(ctx, func(tx server.PgTx) {
					var joined bool
					server.Raise(tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, redisContractAdmissionLock(abandoned)).Scan(&joined))
					if !joined {
						t.Fatal("cleanup retry retained SQL ownership during wait")
					}
				})
				return nil
			}}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := cleanup.run(canceled, abandoned, []server.Id{f.balanceId}); err != nil || attempts != 2 {
			t.Fatal("lost release acknowledgement did not recover after logical65-second outage", attempts, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("release acknowledgement retry changed a live neighbor", got)
		}
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

func TestRedisRecoveryLostReserveAcknowledgementSurvivesCallerState(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		request := withRedisContractAdmission(ctx)
		id := redisAdmissionFromContext(request).contractId
		value := server.HandleError(func() {
			server.Tx(request, func(tx server.PgTx) {
				wrapped := &redisRefusalTestTx{PgTx: tx, beforeFence: func() {
					redisRecoveryRequireMarker(t, ctx, f.balanceId, id, true)
					panic(io.EOF) // Real reserve succeeded; caller loses its reply/state.
				}}
				_, _, _ = createTransferEscrowInTx(request, wrapped, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if value != io.EOF {
			t.Fatal("lost reserve fixture did not reach its real acknowledgement boundary", value)
		}
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 29 {
			t.Fatal("fresh caller needed lost in-memory reserve outcome", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, id, false)
	})
}

func TestRedisRecoveryLivePublicationAndLostCommitStayReserved(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		// Share the funded balance but not the deliberately held source row.
		// The public peer's post-commit usage stamp must remain independent
		// of the original writer's client lifecycle FOR SHARE fence.
		peer := f
		peer.sourceId = server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{peer.sourceId: peer.sourceNetworkId})
		request := withRedisContractAdmission(ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.Background())
		first, _, err := createTransferEscrowInTx(request, held, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
		if err != nil || first == nil {
			t.Fatal("held publication fixture failed", err)
		}
		_ = createRedisAdmissionTest(ctx, peer, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 52 {
			t.Fatal("concurrent recovery released an uncommitted writer", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, first.ContractId, true)
		server.Raise(held.Commit(ctx)) // Treat its success reply as lost to the caller.
		if err := recoverRedisReservationRequest(ctx, f.balanceId, first.ContractId); err != nil {
			t.Fatal("actual SQL custody could not reconcile lost commit acknowledgement", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 52 {
			t.Fatal("lost commit acknowledgement became absent SQL", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, first.ContractId, false)
		if repeated, err := CreateTransferEscrow(request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23); err == nil || repeated != nil || !strings.Contains(err.Error(), "already has SQL custody") {
			t.Fatal("same request replaced original committed authority", repeated, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 52 {
			t.Fatal("same-request refusal compensated a prior committed token", got)
		}
	})
}

func TestRedisRecoveryMissingMarkerAndForeignTokenRemainUntouched(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		foreign := server.NewId()
		amount, err := redisContractReservation(ctx, "reserve", f.balanceId, foreign, 1000, 17, redisContractReservationLease)
		if err != nil || amount != 17 {
			t.Fatal("unmarked legacy fixture failed", amount, err)
		}
		lost := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.ZRem(ctx, redisContractReservationKeys(f.balanceId)[4], lost.String()).Err())
		})
		if err := recoverRedisReservationRequest(ctx, f.balanceId, lost); err == nil || !strings.Contains(err.Error(), "missing owned reservation marker") {
			t.Fatal("missing recovery marker was enrolled or released", err)
		}
		if err := recoverRedisReservationRequest(ctx, f.balanceId, foreign); err == nil || !strings.Contains(err.Error(), "missing owned reservation marker") {
			t.Fatal("legacy foreign token acquired v2 cleanup authority", err)
		}
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 69 {
			t.Fatal("unknown custody was reset to available balance", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, lost, false)
	})
}

func TestRedisRecoveryUnknownSqlAndMalformedMarkerDoNotBlockValidPeer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		original := createRedisAdmissionTest(ctx, f, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		keys := redisContractReservationKeys(f.balanceId)
		server.Redis(ctx, func(client server.RedisClient) {
			// Controlled cache corruption: SQL still binds seventeen. Neither
			// the mismatching nineteen nor an invalid identity may be released.
			server.Raise(client.HSet(ctx, keys[1], original.ContractId.String(), "19", "synthetic-invalid-id", "1").Err())
			server.Raise(client.Set(ctx, keys[0], "43", 25*time.Hour).Err())
			server.Raise(client.ZAdd(ctx, keys[4], redis.Z{Score: 0, Member: original.ContractId.String()}, redis.Z{Score: 1, Member: "synthetic-invalid-id"}).Err())
		})
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 49 {
			t.Fatal("unknown earlier marker blocked or contaminated valid recovery", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, original.ContractId, true)
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
		server.Db(ctx, func(conn server.PgConn) {
			var amount ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2`, original.ContractId, f.balanceId).Scan(&amount))
			if amount != 17 {
				t.Fatal("recovery rewrote SQL authority to match corrupt cache", amount)
			}
		})
	})
}

// Earlier active requests rotate behind a later abandoned one. No global
// balance scan or unbounded marker read is required for the next caller.
func TestRedisRecoveryBoundedPageDoesNotStarveLaterAbandonedRequest(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		busy := make([]server.Id, redisReservationRecoveryBatch)
		for index := range busy {
			busy[index] = server.NewId()
			server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, redisContractAdmissionLock(busy[index])))
			amount, err := redisContractReservation(ctx, "reserve-owned", f.balanceId, busy[index], 1000, 1, redisContractReservationLease)
			if err != nil || amount != 1 {
				t.Fatal("bounded live request fixture failed", index, amount, err)
			}
		}
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		server.Redis(ctx, func(client server.RedisClient) {
			for index, id := range busy {
				server.Raise(client.ZAdd(ctx, redisContractReservationKeys(f.balanceId)[4], redis.Z{Score: float64(index), Member: id.String()}).Err())
			}
			server.Raise(client.ZAdd(ctx, redisContractReservationKeys(f.balanceId)[4], redis.Z{Score: 100, Member: abandoned.String()}).Err())
		})
		_ = createRedisAdmissionTest(ctx, f, 29)
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 84 {
			t.Fatal("first bounded page crossed live ownership", got)
		}
		_ = createRedisAdmissionTest(ctx, f, 31)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 92 {
			t.Fatal("bounded page starved the later abandoned request", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
		for _, id := range busy {
			redisRecoveryRequireMarker(t, ctx, f.balanceId, id, true)
		}
	})
}

func TestRedisRecoveryPublicInsufficientBalanceResumesSameRequest(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 980)
		request := withRedisContractAdmission(ctx)
		id := redisAdmissionFromContext(request).contractId
		escrow, err := CreateTransferEscrow(request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 29)
		if err != nil || escrow == nil || escrow.ContractId != id {
			t.Fatal("known insufficient balance did not resume the same public request", escrow, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 29 {
			t.Fatal("insufficient recovery duplicated or dropped reservation", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
	})
}

func TestRedisRecoveryCanceledSqlPageKeepsSuccessfulPrimaryAndMarkers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		primary := createRedisAdmissionTest(ctx, f, 17)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE`))
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		page, stopPage := context.WithCancel(ctx)
		defer stopPage()
		owner := &redisContractAdmission{contractId: server.NewId(), attemptedBalanceIds: []server.Id{f.balanceId}}
		done := make(chan bool, 1)
		go func() { done <- owner.recoverRetainedPage(page) }()
		requireContractLifecycleBlockedBy(t, ctx, held, pid)
		stopPage()
		select {
		case progress := <-done:
			if progress {
				t.Fatal("canceled SQL observation claimed recovery progress")
			}
		case <-ctx.Done():
			t.Fatal("optional SQL page did not join cancellation", ctx.Err())
		}
		server.Raise(held.Rollback(ctx))
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("unavailable SQL changed primary or unobserved reservation", got)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exists bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, primary.ContractId).Scan(&exists))
			if !exists {
				t.Fatal("optional recovery aborted successful foreground custody")
			}
		})
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 46 {
			t.Fatal("canceled page poisoned a later healthy foreground transaction", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
	})
}

func TestRedisRecoveryExpiredPagePerformsNoSqlOrRedisMutation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		page, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		owner := &redisContractAdmission{contractId: server.NewId(), attemptedBalanceIds: []server.Id{f.balanceId}}
		if owner.recoverRetainedPage(page) {
			t.Fatal("expired page invented recovery")
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 23 {
			t.Fatal("expired page changed retained reservation", got)
		}
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 29 {
			t.Fatal("expired page stopped later bounded recovery", got)
		}
	})
}

type redisRecoveryTestReply string

func (self redisRecoveryTestReply) Error() string { return string(self) }
func (redisRecoveryTestReply) RedisError()        {}

func TestRedisRecoveryHardErrorDominatesTransientCleanup(t *testing.T) {
	for _, item := range []struct {
		cause error
		want  bool
	}{
		{cause: syscall.ECONNRESET, want: true}, {cause: io.EOF, want: true},
		{cause: redisRecoveryTestReply("LOADING synthetic restart"), want: true},
		{cause: &pgconn.PgError{Code: "57014"}, want: true},
		{cause: errors.Join(errRedisReservationRecoveryIdentity, context.DeadlineExceeded)},
		{cause: errors.Join(&pgconn.PgError{Code: "42P01"}, context.DeadlineExceeded)},
		{cause: errors.Join(redisRecoveryTestReply("NOAUTH synthetic refusal"), io.EOF)},
		{cause: errors.New("invalid reservation counter")},
	} {
		if got := redisReservationRecoveryRetryable(item.cause); got != item.want {
			t.Errorf("cleanup cause %v retry=%v want=%v", item.cause, got, item.want)
		}
	}
	first, second := server.NewId(), server.NewId()
	var firstAttempts, secondAttempts, waits int
	cleanup := redisReservationCleanup{now: time.Now,
		recover: func(_ context.Context, id, _ server.Id) error {
			if id == first {
				firstAttempts++
				return errors.Join(errRedisReservationRecoveryIdentity, context.DeadlineExceeded)
			}
			secondAttempts++
			return nil
		}, wait: func(context.Context, time.Duration) error { waits++; return nil }}
	err := cleanup.run(t.Context(), server.NewId(), []server.Id{first, second})
	if !errors.Is(err, errRedisReservationCleanupPending) || !errors.Is(err, errRedisReservationRecoveryIdentity) || firstAttempts != 1 || secondAttempts != 1 || waits != 0 {
		t.Fatal("hard custody refusal retried or blocked its healthy peer", firstAttempts, secondAttempts, waits, err)
	}
}
