// Completed-run ordering counts actual finished turns, not leases or URL credit.
package model

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The same resource parser used by an API request controls activation. No test
// replaces the readiness function or directly enables a different SQL branch.
func testingUrlCompletionPriority(t testing.TB, since time.Time) {
	t.Helper()
	pop := server.Config.PushSimpleResource(ProviderEgressProbeResourceName,
		[]byte(fmt.Sprintf("url_completed_run_priority_since: %s\n", since.Format(time.RFC3339Nano))))
	currentProviderEgressRules.Store(nil)
	t.Cleanup(func() {
		pop()
		currentProviderEgressRules.Store(nil)
	})
}

type testingUrlCompletionCycle struct {
	count     int64
	ordinal   int64
	next      time.Time
	expiry    *time.Time
	ready     bool
	successes int
	errors    int
	history   int
}

func testingReadUrlCompletionCycle(t testing.TB, ctx context.Context, clientId server.Id) testingUrlCompletionCycle {
	t.Helper()
	cycle := testingUrlCompletionCycle{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT completed_run_count,claim_ordinal,next_attempt_at,
			completed_next_expiry_at,completed_priority_ready,success_count,error_count,
			(SELECT COUNT(*) FROM provider_egress_health_history WHERE client_id=$1)
			FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId).Scan(
			&cycle.count, &cycle.ordinal, &cycle.next, &cycle.expiry, &cycle.ready,
			&cycle.successes, &cycle.errors, &cycle.history))
	})
	return cycle
}

// Each fixture turn obtains a real server claim before producing its receipt.
// Moving the selected fixture's deadline only arranges which turn is due.
func testingClaimUrlCompletion(t testing.TB, ctx context.Context, clientId server.Id, at time.Time) ProviderUrlProbeDue {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=
			CASE WHEN client_id=$1 THEN $2::timestamp ELSE $2::timestamp+interval '1 day' END`, clientId, at))
	})
	result := ClaimProviderUrlProbeDueWithStatus(ctx, at, 1, 0, 1)
	if result.PriorityMaintenancePending || len(result.Providers) != 1 || result.Providers[0].ClientId != clientId {
		t.Fatalf("missing arranged URL turn: %+v", result)
	}
	due := result.Providers[0]
	if due.ClaimOrdinal <= 0 || !due.ClaimedAt.Equal(at) {
		t.Fatalf("claim lacks its server-issued identity: %+v", due)
	}
	return due
}

func testingUrlCompletionClients(t testing.TB, now time.Time, count int) []server.Id {
	t.Helper()
	testingSeedUrlProbeFleet(t, now, count)
	clients := []server.Id{}
	server.Db(t.Context(), func(conn server.PgConn) {
		rows, err := conn.Query(t.Context(), `SELECT client_id FROM provider_egress_probe_cycle ORDER BY client_id`)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				server.Raise(rows.Scan(&clientId))
				clients = append(clients, clientId)
			}
		})
	})
	if len(clients) != count {
		t.Fatalf("fixture clients=%d want=%d", len(clients), count)
	}
	return clients
}

func testingCompleteUrlClaim(t testing.TB, ctx context.Context, due ProviderUrlProbeDue, at time.Time, failure string) *ProviderUrlProbeCompletionReceipt {
	t.Helper()
	receipt, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
		ClientId: due.ClientId, ClaimOrdinal: due.ClaimOrdinal, CompletedAt: at,
		ProbeFailure: failure, AllowPacing: true,
	}, at)
	if err != nil || receipt == nil || receipt.Replay || !receipt.CompletedAt.Equal(at) {
		t.Fatalf("first completion=%+v error=%v", receipt, err)
	}
	return receipt
}

// A zero-completion provider wins over older due providers with one or two
// completed turns. Setup failures and measured failures count like successes;
// none of these diagnostic receipts fabricates URL quality/quota evidence.
func TestUrlCompletedPriorityZeroThenLowestThenOldestDue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		clients := testingUrlCompletionClients(t, now.Add(-3*time.Hour), 5)
		for index, failure := range []string{"", "tunnel_failed", "health_not_run"} {
			clientId := clients[index+1]
			for turn := range index + 1 {
				at := now.Add(-2*time.Hour + time.Duration(turn)*time.Minute)
				due := testingClaimUrlCompletion(t, ctx, clientId, at)
				testingCompleteUrlClaim(t, ctx, due, at.Add(time.Second), failure)
			}
			cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
			if cycle.count != int64(index+1) || cycle.history != 0 || cycle.successes != 0 || cycle.errors != 0 {
				t.Fatalf("completed turns changed quota/evidence: %+v", cycle)
			}
		}
		// The last provider also has zero completions but is not paced due.
		server.Tx(ctx, func(tx server.PgTx) {
			for index, clientId := range clients {
				dueAt := now.Add(-time.Duration(index) * time.Minute)
				if index == 4 {
					dueAt = now.Add(time.Second)
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, clientId, dueAt))
			}
		})
		// Limit one is smaller than the due population: sorting a legacy
		// oldest-due head after LIMIT must not masquerade as global fairness.
		first := ClaimProviderUrlProbeDueWithStatus(ctx, now, 1, 0, 1)
		if len(first.Providers) != 1 || first.Providers[0].ClientId != clients[0] ||
			first.Providers[0].CompletedRunCount == nil || *first.Providers[0].CompletedRunCount != 0 {
			t.Fatalf("bounded claim hid the due zero-completion provider: %+v", first)
		}
		result := ClaimProviderUrlProbeDueWithStatus(ctx, now, 10, 0, 1)
		if !result.CompletedRunPriorityReady || result.PriorityMaintenancePending || len(result.Providers) != 3 {
			t.Fatalf("missing ready completion cohort: %+v", result)
		}
		for index, due := range result.Providers {
			if due.ClientId != clients[index+1] || due.CompletedRunCount == nil || *due.CompletedRunCount != int64(index+1) {
				t.Fatalf("completed-run order ignored zero/lowest count: index=%d due=%+v", index, due)
			}
		}
		if duplicate := ClaimProviderUrlProbeDue(ctx, now, 10, 0, 1); len(duplicate) != 0 {
			t.Fatal("reservation was claimed twice or a future zero-count row bypassed pacing")
		}
		// Equal count uses due time before client ID, independent of insertion.
		server.Tx(ctx, func(tx server.PgTx) {
			for index, clientId := range []server.Id{clients[0], clients[4]} {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`,
					clientId, now.Add(-time.Duration(index)*time.Minute)))
			}
		})
		tied := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 1)
		if len(tied) != 2 || tied[0].ClientId != clients[4] || tied[1].ClientId != clients[0] {
			t.Fatalf("equal-count oldest-due tie broke incorrectly: %+v", tied)
		}
	})
}

// An acknowledged replay keeps the first completion time and class, even when
// the caller retries with a later clock or a newer claim is already leased.
func TestUrlCompletedReceiptReplayAndNewClaimIsolation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := testingUrlCompletionClients(t, now, 1)[0]
		due := testingClaimUrlCompletion(t, ctx, clientId, now)
		finishedAt := now.Add(time.Second)
		receipt := testingCompleteUrlClaim(t, ctx, due, finishedAt, "tunnel_failed")
		cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
		if cycle.count != 1 || cycle.next.Sub(finishedAt) < 54*time.Second || cycle.next.Sub(finishedAt) > 66*time.Second {
			t.Fatalf("first failure did not count once with normal retry pacing: %+v", cycle)
		}
		newDue := ClaimProviderUrlProbeDue(ctx, cycle.next, 1, 0, 1)
		if len(newDue) != 1 || newDue[0].ClaimOrdinal != due.ClaimOrdinal+1 {
			t.Fatalf("new paced claim lost durable ordinal: %+v", newDue)
		}
		leased := testingReadUrlCompletionCycle(t, ctx, clientId)
		replayed, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
			ClientId: clientId, ClaimOrdinal: due.ClaimOrdinal, CompletedAt: now.Add(2 * time.Hour),
			ProbeFailure: "", AllowPacing: true,
		}, now.Add(2*time.Hour))
		if err != nil || replayed == nil || !replayed.Replay || !replayed.CompletedAt.Equal(receipt.CompletedAt) || !replayed.ReceivedAt.Equal(receipt.ReceivedAt) {
			t.Fatalf("retry moved the first receipt: %+v error=%v", replayed, err)
		}
		after := testingReadUrlCompletionCycle(t, ctx, clientId)
		if after.count != 1 || !after.next.Equal(leased.next) || after.ordinal != leased.ordinal {
			t.Fatalf("old retry released or counted a new claim: before=%+v after=%+v", leased, after)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var failure string
			var at time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT probe_failure,attempt_at FROM provider_egress_probe_attempt WHERE client_id=$1`, clientId).Scan(&failure, &at))
			if failure != "tunnel_failed" || !at.Equal(finishedAt) {
				t.Fatalf("replay rewrote the legacy diagnostic projection: failure=%q at=%s", failure, at)
			}
		})
	})
}

// Counts use a trailing open/closed window (now-4h, now]. The exact expiry
// instant loses one turn, without resetting the durable claim sequence.
func TestUrlCompletedPriorityFourHourBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		clientId := testingUrlCompletionClients(t, now, 1)[0]
		due := testingClaimUrlCompletion(t, ctx, clientId, now)
		at := now.Add(time.Second)
		testingCompleteUrlClaim(t, ctx, due, at, "health_not_run")
		boundary := at.Add(ProviderUrlProbeCompletedWindow)
		before := ClaimProviderUrlProbeDueWithStatus(ctx, boundary.Add(-time.Microsecond), 1, 0, 1)
		if len(before.Providers) != 1 || before.Providers[0].CompletedRunCount == nil || *before.Providers[0].CompletedRunCount != 1 {
			t.Fatalf("completion expired before four-hour boundary: %+v", before)
		}
		// Keep the existing reservation: maintenance must expire even a row
		// which is not due, and must not shorten its lease to manufacture work.
		leased := testingReadUrlCompletionCycle(t, ctx, clientId)
		atBoundary := ClaimProviderUrlProbeDueWithStatus(ctx, boundary, 1, 0, 1)
		after := testingReadUrlCompletionCycle(t, ctx, clientId)
		if len(atBoundary.Providers) != 0 || after.count != 0 || after.expiry != nil || !after.next.Equal(leased.next) || after.ordinal != leased.ordinal {
			t.Fatalf("four-hour expiry changed reservation or retained aged count: result=%+v cycle=%+v", atBoundary, after)
		}
		if replay, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
			ClientId: clientId, ClaimOrdinal: due.ClaimOrdinal, CompletedAt: at, ProbeFailure: "health_not_run", AllowPacing: true,
		}, boundary); err != nil || replay == nil || !replay.Replay || testingReadUrlCompletionCycle(t, ctx, clientId).count != 0 {
			t.Fatalf("expired replay resurrected a count: %+v error=%v", replay, err)
		}
	})
}

// Setup completion can be reported with small clock skew, not arbitrary past
// or future time. A delayed first report older than the window adds no count.
func TestUrlCompletedReceiptValidatesClockAndRetainsRawTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clients := testingUrlCompletionClients(t, now, 4)
		for index, clientId := range clients {
			due := testingClaimUrlCompletion(t, ctx, clientId, now)
			receivedAt := now.Add(time.Second)
			reportedAt := now.Add(-time.Minute)
			effectiveAt := now
			if index == 1 {
				reportedAt, effectiveAt = receivedAt.Add(time.Minute), receivedAt
			}
			if index == 2 {
				reportedAt = receivedAt.Add(providerUrlProbeCompletionSkew + time.Microsecond)
			}
			if index == 3 {
				reportedAt, effectiveAt = now, now
				receivedAt = now.Add(ProviderUrlProbeCompletedWindow)
			}
			receipt, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
				ClientId: clientId, ClaimOrdinal: due.ClaimOrdinal, CompletedAt: reportedAt, ProbeFailure: "tunnel_failed", AllowPacing: false,
			}, receivedAt)
			if index == 2 {
				if err == nil || receipt != nil || testingReadUrlCompletionCycle(t, ctx, clientId).count != 0 {
					t.Fatalf("unbounded future completion accepted: %+v error=%v", receipt, err)
				}
				continue
			}
			if err != nil || receipt == nil || !receipt.CompletedAt.Equal(effectiveAt) {
				t.Fatalf("bounded source skew/delay was not normalized: %+v error=%v", receipt, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var storedRaw time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT reported_completed_at FROM provider_url_probe_run WHERE client_id=$1 AND claim_ordinal=$2`, clientId, due.ClaimOrdinal).Scan(&storedRaw))
				if !storedRaw.Equal(reportedAt) {
					t.Fatalf("source timestamp audit was lost: got=%s want=%s", storedRaw, reportedAt)
				}
			})
			cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
			wantCount := int64(1)
			if index == 3 {
				wantCount = 0
			}
			if cycle.count != wantCount {
				t.Fatalf("completion clock changed count: %+v", cycle)
			}
			if index == 3 {
				if !cycle.next.Equal(now.Add(ProviderEgressProbeAttemptBackoff)) {
					t.Fatalf("late local completion extended its expired lease: %+v", cycle)
				}
			} else if delay := cycle.next.Sub(receivedAt); delay < 54*time.Second || delay > 66*time.Second {
				t.Fatalf("bounded-skew local completion did not schedule receipt-owned retry: %+v", cycle)
			}
		}
		if receipt, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
			ClientId: clients[0], ClaimOrdinal: 999, CompletedAt: now, AllowPacing: true,
		}, now); err == nil || receipt != nil {
			t.Fatal("a caller fabricated a server-issued claim")
		}
	})
}

// Until every old writer has retired and four hours have elapsed, the API
// explicitly reports unknown priority counts and preserves legacy due order.
func TestUrlCompletedPriorityWriterEpochIsNotObservedZero(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now)
		clients := testingUrlCompletionClients(t, now, 2)
		for _, at := range []time.Time{now, now.Add(4*time.Hour - time.Microsecond), now.Add(4 * time.Hour)} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1`, at))
			})
			result := ClaimProviderUrlProbeDueWithStatus(ctx, at, 2, 0, 1)
			ready := !at.Before(now.Add(4 * time.Hour))
			if result.CompletedRunPriorityReady != ready || result.CompletedRunPrioritySince == nil || !result.CompletedRunPrioritySince.Equal(now) || len(result.Providers) != len(clients) {
				t.Fatalf("writer epoch boundary changed: at=%s result=%+v", at, result)
			}
			for _, due := range result.Providers {
				if (due.CompletedRunCount != nil) != ready {
					t.Fatalf("unknown history was presented as observed zero: ready=%t due=%+v", ready, due)
				}
			}
		}
	})
}

// Concurrent replays serialize on the same cycle/run rows. A later independent
// claim is disjoint; its completion may count once but may not be mistaken for
// the first turn merely because the measurement timestamp is identical.
func TestUrlCompletedConcurrentReceiptsAndClaims(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		clients := testingUrlCompletionClients(t, now, 16)
		first := ClaimProviderUrlProbeDue(ctx, now, 16, 0, 1)
		if len(first) != len(clients) {
			t.Fatalf("initial claim=%d", len(first))
		}
		var wait sync.WaitGroup
		errors := make(chan error, 32)
		start := make(chan struct{})
		for range 32 {
			wait.Go(func() {
				<-start
				_, err := CompleteProviderUrlProbeRun(ctx, ProviderUrlProbeCompletion{
					ClientId: first[0].ClientId, ClaimOrdinal: first[0].ClaimOrdinal,
					CompletedAt: now.Add(time.Second), ProbeFailure: "tunnel_failed", AllowPacing: true,
				}, now.Add(time.Second))
				errors <- err
			})
		}
		close(start)
		wait.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		if cycle := testingReadUrlCompletionCycle(t, ctx, first[0].ClientId); cycle.count != 1 {
			t.Fatalf("concurrent receipt replay counted %d turns", cycle.count)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1`, now.Add(2*time.Minute)))
		})
		claimed := make(chan []ProviderUrlProbeDue, 4)
		for range 4 {
			wait.Go(func() {
				claimed <- ClaimProviderUrlProbeDue(ctx, now.Add(2*time.Minute), 4, 0, 1)
			})
		}
		wait.Wait()
		close(claimed)
		seen := []server.Id{}
		for providers := range claimed {
			for _, due := range providers {
				if slices.Contains(seen, due.ClientId) {
					t.Fatalf("overlapping workers duplicated claim %s/%d", due.ClientId, due.ClaimOrdinal)
				}
				seen = append(seen, due.ClientId)
			}
		}
		// A concurrent maintenance owner may legitimately postpone admission;
		// a sequential tail must still reach every unreserved row exactly once.
		for _, due := range ClaimProviderUrlProbeDue(ctx, now.Add(2*time.Minute), 16, 0, 1) {
			if slices.Contains(seen, due.ClientId) {
				t.Fatal("sequential tail reclaimed a concurrent reservation")
			}
			seen = append(seen, due.ClientId)
		}
		if len(seen) != len(clients) {
			t.Fatalf("concurrent claim maintenance stranded work: got=%d want=%d", len(seen), len(clients))
		}
	})
}
