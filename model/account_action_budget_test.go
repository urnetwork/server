package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A refusal by a budget that knows when it has room again names that wait,
// rounded up so that trying again after it is never early, rather than
// "later": an extender activation refused by its hourly budget can be tried
// again in minutes, not tomorrow (support inbox 8083).
func TestAccountActionRefusalNamesTheWait(t *testing.T) {
	cases := []struct {
		retryAfter time.Duration
		want       string
	}{
		{retryAfter: time.Second, want: "Please try again in 1 minute."},
		{retryAfter: time.Minute, want: "Please try again in 1 minute."},
		{retryAfter: time.Minute + time.Second, want: "Please try again in 2 minutes."},
		{retryAfter: 23*time.Minute + 10*time.Second, want: "Please try again in 24 minutes."},
		{retryAfter: time.Hour, want: "Please try again in 60 minutes."},
		{retryAfter: 2 * time.Hour, want: "Please try again in 2 hours."},
		{retryAfter: 23*time.Hour + time.Minute, want: "Please try again in 24 hours."},
	}
	for _, c := range cases {
		message := maxAccountActionAttemptsRetryError(
			AccountActionExtenderActivateV4,
			time.Hour,
			c.retryAfter,
		).Error()
		want := "You have reached the maximum number of attempts for activating an extender over IPv4 " +
			"in the last hour. " + c.want
		if message != want {
			t.Errorf("wait %s: the refusal is %q, want %q", c.retryAfter, message, want)
		}
	}
}

// The budgets of one action are spent together or not at all (support inbox
// 8083). A client refused by its account's budget keeps its own budget whole,
// where before its own was spent first and the account's checked after, so
// the refusal cost it an attempt of an activation that never ran. The refusal
// names the budget that has no room, its window, and the wait.
func TestCheckAndRecordAccountActionBudgetsSpendsAllOrNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		firstClientId := server.NewId()
		secondClientId := server.NewId()

		budgets := func(clientId server.Id, clientAction string, accountAction string) []AccountActionBudget {
			return []AccountActionBudget{
				{Id: clientId, Action: clientAction, Limit: 2, Window: time.Hour},
				{Id: userId, Action: accountAction, Limit: 2, Window: time.Hour},
			}
		}
		v4Budgets := func(clientId server.Id) []AccountActionBudget {
			return budgets(clientId, AccountActionExtenderActivateV4, AccountActionExtenderActivateAccountV4)
		}

		// the first client spends the account's budget, and its own
		for i := range 2 {
			if err := CheckAndRecordAccountActionBudgets(ctx, v4Budgets(firstClientId)...); err != nil {
				t.Fatalf("attempt %d was refused: %s", i+1, err)
			}
		}

		// both are spent: the refusal names the first of them
		err := CheckAndRecordAccountActionBudgets(ctx, v4Budgets(firstClientId)...)
		if err == nil {
			t.Fatalf("the third attempt was not refused")
		}
		for _, want := range []string{
			"for activating an extender over IPv4 in the last hour.",
			"Please try again in ",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the client refusal %q does not say %q", err.Error(), want)
			}
		}

		// the second client is refused by the account's budget
		err = CheckAndRecordAccountActionBudgets(ctx, v4Budgets(secondClientId)...)
		if err == nil {
			t.Fatalf("the second client was not refused by the account's budget")
		}
		for _, want := range []string{
			"for activating your account's extenders over IPv4 in the last hour.",
			"Please try again in ",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the account refusal %q does not say %q", err.Error(), want)
			}
		}
		// and that refusal spent none of its own budget: both of its attempts
		// still fit it
		for i := range 2 {
			if err := CheckAndRecordAccountActionRateLimit(
				ctx,
				secondClientId,
				AccountActionExtenderActivateV4,
				2,
				time.Hour,
			); err != nil {
				t.Fatalf("the account's refusal spent the second client's own budget (attempt %d): %s", i+1, err)
			}
		}

		// the other family's budgets are their own
		for i := range 2 {
			if err := CheckAndRecordAccountActionBudgets(
				ctx,
				budgets(firstClientId, AccountActionExtenderActivateV6, AccountActionExtenderActivateAccountV6)...,
			); err != nil {
				t.Fatalf("ipv6 attempt %d was refused by the ipv4 budgets: %s", i+1, err)
			}
		}
	})
}

// A budget with no room has room again when the attempt whose leaving makes
// room leaves the window (connect#230): that wait, at least a second and at
// most the window, and the window when no attempt can make room.
func TestAccountActionBudgetRetryAfterIsWhenTheAttemptThatMakesRoomLeaves(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *time.Time {
		boundaryTime := now.Add(-ago)
		return &boundaryTime
	}
	cases := []struct {
		boundaryTime *time.Time
		want         time.Duration
	}{
		{boundaryTime: at(10 * time.Minute), want: 50 * time.Minute},
		{boundaryTime: at(59*time.Minute + 30*time.Second), want: 30 * time.Second},
		{boundaryTime: at(time.Hour), want: time.Second},
		{boundaryTime: at(2 * time.Hour), want: time.Second},
		{boundaryTime: at(0), want: time.Hour},
		{boundaryTime: at(-time.Minute), want: time.Hour},
		{boundaryTime: nil, want: time.Hour},
	}
	for _, c := range cases {
		if got := AccountActionBudgetRetryAfter(c.boundaryTime, time.Hour, now); got != c.want {
			t.Errorf("boundary %v: retry after %s, want %s", c.boundaryTime, got, c.want)
		}
	}
}

// A budget whose limit is below its count -- an account's limit that shrank,
// as its proven hosts age out (connect#230) -- has room again when enough
// attempts have left for the count to be under the limit, not when the oldest
// leaves. Ten attempts five minutes apart, the oldest 55 minutes ago, against
// a limit of 8: room comes when the third oldest leaves, in 15 minutes. Before,
// the refusal named the oldest's 5 minutes, and a caller that waited it out
// was refused again.
func TestAccountActionBudgetRefusalWaitsForTheAttemptThatMakesRoom(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			for i := range 10 {
				insertAccountActionAttemptInTx(
					ctx,
					tx,
					userId,
					AccountActionExtenderActivateAccountV4,
					now.Add(-55*time.Minute+time.Duration(i)*5*time.Minute),
				)
			}
		})
		err := CheckAndRecordAccountActionBudgets(ctx, AccountActionBudget{
			Id:     userId,
			Action: AccountActionExtenderActivateAccountV4,
			Limit:  8,
			Window: time.Hour,
		})
		if err == nil {
			t.Fatalf("a budget of 8 holding 10 attempts had room")
		}
		if !strings.Contains(err.Error(), "Please try again in 15 minutes.") {
			t.Fatalf("the refusal %q does not wait for the third oldest attempt, 15 minutes", err.Error())
		}
	})
}

// A budget's extended limit (ExtendLimitInTx, connect#230) is read only once
// its count has reached the floor, in the spending transaction, and a value
// below the floor leaves the floor. The refusal it makes is an
// *AccountActionBudgetRefusal with the wait, which an extender activation
// answers as retry_after_seconds.
func TestCheckAndRecordAccountActionBudgetsReadsTheExtendedLimitPastTheFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extendedLimit := 3
		extendCount := 0
		budget := AccountActionBudget{
			Id:     server.NewId(),
			Action: AccountActionExtenderActivateAccountV4,
			Limit:  2,
			Window: time.Hour,
			ExtendLimitInTx: func(ctx context.Context, tx server.PgTx) int {
				extendCount += 1
				return extendedLimit
			},
		}
		for i := range 2 {
			if err := CheckAndRecordAccountActionBudgets(ctx, budget); err != nil {
				t.Fatalf("spend %d under the floor was refused: %s", i+1, err)
			}
		}
		connect.AssertEqual(t, extendCount, 0)

		if err := CheckAndRecordAccountActionBudgets(ctx, budget); err != nil {
			t.Fatalf("the spend past the floor was refused under the extended limit: %s", err)
		}
		connect.AssertEqual(t, extendCount, 1)

		err := CheckAndRecordAccountActionBudgets(ctx, budget)
		var refusal *AccountActionBudgetRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("the spend past the extended limit was not refused by the budget: %v", err)
		}
		connect.AssertEqual(t, refusal.Action, AccountActionExtenderActivateAccountV4)
		connect.AssertEqual(t, refusal.Window, time.Hour)
		if refusal.RetryAfter <= 59*time.Minute || time.Hour < refusal.RetryAfter {
			t.Errorf("the refusal waits %s, want the rest of the hour", refusal.RetryAfter)
		}
		connect.AssertEqual(t, extendCount, 2)

		// an extended limit below the floor leaves the floor
		extendedLimit = 0
		lowBudget := budget
		lowBudget.Id = server.NewId()
		for i := range 2 {
			if err := CheckAndRecordAccountActionBudgets(ctx, lowBudget); err != nil {
				t.Fatalf("spend %d under the floor was refused with a low extended limit: %s", i+1, err)
			}
		}
		if err := CheckAndRecordAccountActionBudgets(ctx, lowBudget); err == nil {
			t.Fatalf("the spend past the floor had room with a low extended limit")
		}
	})
}

// A spend waits in the database for another process's spend of the same
// budget, then reads what that spend spent (connect#230). The spends of one
// process wait for each other in the process, but two processes meet only in
// the database; without its lock a spend in each could both read a budget one
// short of its limit and both spend it.
func TestCheckAndRecordAccountActionBudgetsWaitsForAnotherProcessesSpend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// the spend waits as long as the test holds it, rather than racing the
		// main pool's 2 s lock bound while the test looks for its wait
		previousLockTimeout := accountActionBudgetsLockTimeout
		accountActionBudgetsLockTimeout = 30 * time.Second
		defer func() {
			accountActionBudgetsLockTimeout = previousLockTimeout
		}()
		budget := AccountActionBudget{
			Id:     server.NewId(),
			Action: AccountActionExtenderActivateAddressV4,
			Limit:  2,
			Window: time.Hour,
		}
		if err := CheckAndRecordAccountActionBudgets(ctx, budget); err != nil {
			t.Fatal(err)
		}

		// the other process's spend holds the budget's database lock, and
		// spends the last attempt once let go
		otherHolding := make(chan struct{})
		otherRelease := make(chan struct{})
		otherDone := make(chan struct{})
		go func() {
			defer close(otherDone)
			server.Tx(ctx, func(tx server.PgTx) {
				lockAccountActionBudgetsInTx(ctx, tx, accountActionBudgetLockKeys([]AccountActionBudget{budget}))
				close(otherHolding)
				<-otherRelease
				insertAccountActionAttemptInTx(ctx, tx, budget.Id, budget.Action, server.NowUtc())
			}, server.TxReadCommitted)
		}()
		<-otherHolding

		spendDone := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					spendDone <- fmt.Errorf("the spend raised: %v", r)
				}
			}()
			spendDone <- CheckAndRecordAccountActionBudgets(ctx, budget)
		}()

		// this process's spend waits for the other's lock in the database
		waiting := false
		for deadline := time.Now().Add(30 * time.Second); !waiting && time.Now().Before(deadline); {
			select {
			case err := <-spendDone:
				close(otherRelease)
				<-otherDone
				t.Fatalf("the spend did not wait for the other process's spend of its budget, and returned %v", err)
			default:
			}
			server.Db(ctx, func(conn server.PgConn) {
				var waitingCount int
				server.Raise(conn.QueryRow(
					ctx,
					`
					SELECT COUNT(*) FROM pg_locks
					WHERE locktype = 'advisory' AND NOT granted
					  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
					`,
				).Scan(&waitingCount))
				waiting = 0 < waitingCount
			})
			if !waiting {
				time.Sleep(10 * time.Millisecond)
			}
		}
		close(otherRelease)
		<-otherDone
		err := <-spendDone
		if !waiting {
			t.Fatalf("the spend was never seen waiting for the other process's lock (it returned %v)", err)
		}
		var refusal *AccountActionBudgetRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("the spend after the other process spent the last attempt was not refused: %v", err)
		}
	})
}

// A spend that waits past its bound for a budget another spend holds ends with
// ErrAccountActionBudgetsBusy and spends nothing (connect#230). Before, the
// wait's 55P03 raised out of the spend, and an extender activation answered
// it as an unexpected 500.
func TestCheckAndRecordAccountActionBudgetsAnswersBusyPastTheLockBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		previousLockTimeout := accountActionBudgetsLockTimeout
		accountActionBudgetsLockTimeout = 200 * time.Millisecond
		defer func() {
			accountActionBudgetsLockTimeout = previousLockTimeout
		}()
		budget := AccountActionBudget{
			Id:     server.NewId(),
			Action: AccountActionExtenderActivateAddressV4,
			Limit:  2,
			Window: time.Hour,
		}

		// another process's spend holds the budget until this spend has ended
		held := make(chan struct{})
		release := make(chan struct{})
		released := make(chan struct{})
		go func() {
			defer close(released)
			server.Tx(ctx, func(tx server.PgTx) {
				lockAccountActionBudgetsInTx(ctx, tx, accountActionBudgetLockKeys([]AccountActionBudget{budget}))
				close(held)
				<-release
			}, server.TxReadCommitted)
		}()
		<-held

		var err error
		raised := server.HandleError(func() {
			err = CheckAndRecordAccountActionBudgets(ctx, budget)
		})
		close(release)
		<-released
		if raised != nil {
			t.Fatalf("the spend raised past the lock bound: %v", raised)
		}
		if !errors.Is(err, ErrAccountActionBudgetsBusy) {
			t.Fatalf("the spend past the lock bound ended %v, want ErrAccountActionBudgetsBusy", err)
		}
		var attemptCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM network_user_action_attempt WHERE user_id = $1`,
				budget.Id,
			).Scan(&attemptCount))
		})
		connect.AssertEqual(t, attemptCount, 0)
	})
}
