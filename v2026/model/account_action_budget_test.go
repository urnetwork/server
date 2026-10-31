package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
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
