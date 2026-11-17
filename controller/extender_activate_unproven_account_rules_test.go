package controller

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The rules of the longer wait an account whose hosts never pass is asked for
// (research extender-health-2026-11-11 §5 item 3), without a database.

// The numbers: at most six hours, which is the most an activator holds a family
// whatever it is told (connect/net_extender_activate.go MaxBackoff), over the
// account's attempts of the last day, which the attempts' 48 hour retention
// keeps.
func TestExtenderActivateSettingsHoldTheUnprovenAccountNumbers(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	connect.AssertEqual(t, settings.UnprovenAccountRetryAfterMax, 6*time.Hour)
	connect.AssertEqual(t, settings.UnprovenAccountAttemptLookback, 24*time.Hour)
	if model.AccountActionAttemptRetention < settings.UnprovenAccountAttemptLookback {
		t.Fatalf("the lookback %s reaches past the attempts' retention %s", settings.UnprovenAccountAttemptLookback, model.AccountActionAttemptRetention)
	}
	if connect.DefaultExtenderActivatorSettings().MaxBackoff < settings.UnprovenAccountRetryAfterMax {
		t.Fatalf("the longest wait %s is longer than the activator holds a family", settings.UnprovenAccountRetryAfterMax)
	}
}

// The wait doubles from the window for each window's worth of the account's
// floor it has spent over the lookback, and stops at six hours; the count is
// read no further than the four windows' worth that reach it.
func TestExtenderActivateUnprovenAccountWaitGrowsToSixHours(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	for _, c := range []struct {
		attemptCount int
		want         time.Duration
	}{
		{attemptCount: 0, want: time.Hour},
		{attemptCount: 59, want: time.Hour},
		{attemptCount: 60, want: time.Hour},
		{attemptCount: 119, want: time.Hour},
		{attemptCount: 120, want: 2 * time.Hour},
		{attemptCount: 180, want: 4 * time.Hour},
		{attemptCount: 239, want: 4 * time.Hour},
		{attemptCount: 240, want: 6 * time.Hour},
		{attemptCount: 1440, want: 6 * time.Hour},
		{attemptCount: 1 << 30, want: 6 * time.Hour},
	} {
		if got := settings.unprovenAccountRetryAfter(c.attemptCount); got != c.want {
			t.Errorf("%d attempts ask for %s, want %s", c.attemptCount, got, c.want)
		}
	}
	connect.AssertEqual(t, settings.unprovenAccountAttemptCountLimit(), 240)
	if got := settings.unprovenAccountRetryAfter(settings.unprovenAccountAttemptCountLimit()); got != settings.UnprovenAccountRetryAfterMax {
		t.Fatalf("the count's limit asks for %s, want the most", got)
	}

	// off, every account keeps the wait until its budget has room
	settings.UnprovenAccountRetryAfterMax = 0
	connect.AssertEqual(t, settings.unprovenAccountRetryAfter(1440), time.Duration(0))
	connect.AssertEqual(t, settings.unprovenAccountAttemptCountLimit(), 0)
}

// Any host of the family among the account's candidates is a pass: the
// candidates are only hosts whose required test passed within the lookback,
// with no least age, so a host that passed a minute ago counts. A host of the
// other family does not.
func TestExtenderActivateAccountPassedRules(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	candidate := func(ip string, age time.Duration) *model.NetworkExtenderProvenHostCandidate {
		return &model.NetworkExtenderProvenHostCandidate{
			ExtenderId:        server.NewId(),
			Ip:                netip.MustParseAddr(ip),
			FirstActivateTime: now.Add(-age),
		}
	}
	connect.AssertEqual(t, settings.accountPassed(nil, 4), false)
	connect.AssertEqual(t, settings.accountPassed([]*model.NetworkExtenderProvenHostCandidate{
		candidate("192.0.2.10", time.Minute),
	}, 4), true)
	connect.AssertEqual(t, settings.accountPassed([]*model.NetworkExtenderProvenHostCandidate{
		candidate("::ffff:192.0.2.10", 6*24*time.Hour),
	}, 4), true)
	connect.AssertEqual(t, settings.accountPassed([]*model.NetworkExtenderProvenHostCandidate{
		candidate("2001:db8::10", time.Minute),
	}, 4), false)
	connect.AssertEqual(t, settings.accountPassed([]*model.NetworkExtenderProvenHostCandidate{
		candidate("2001:db8::10", time.Minute),
	}, 6), true)
}

// The answer to each budget's refusal: the budget's own wait for every budget
// and for an account whose hosts pass, with its words unchanged, and the longer
// wait for an account whose hosts never pass, counted as unproven_account,
// with words that start as the account's always did and then say why. The
// account's attempts are counted over the last day only for the account
// whose hosts never pass.
func TestExtenderActivateBudgetRefusalAnswers(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	settings.BudgetRetryAfterJitter = 0
	service := newExtenderActivateService(settings)
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	service.now = func() time.Time {
		return now
	}

	clientAction, addressAction, accountAction := extenderActivateRateLimitActions(4)
	counts := []time.Time{}
	countAttempts := func(attemptCount int) func(minTime time.Time, maxCount int) int {
		return func(minTime time.Time, maxCount int) int {
			counts = append(counts, minTime)
			connect.AssertEqual(t, maxCount, 240)
			return min(attemptCount, maxCount)
		}
	}
	refusal := func(action string) model.AccountActionBudgetRefusal {
		return model.AccountActionBudgetRefusal{
			Action:     action,
			Window:     time.Hour,
			RetryAfter: 59 * time.Minute,
		}
	}

	for _, c := range []struct {
		name            string
		action          string
		accountUnproven bool
		attemptCount    int
		wantOutcome     string
		wantRetryAfter  time.Duration
		wantWhy         bool
	}{
		{name: "the client's", action: clientAction, wantOutcome: "client", wantRetryAfter: 59 * time.Minute},
		{name: "the address's", action: addressAction, wantOutcome: "address", wantRetryAfter: 59 * time.Minute},
		{name: "an account whose hosts pass", action: accountAction, attemptCount: 1440, wantOutcome: "account", wantRetryAfter: 59 * time.Minute},
		{name: "a new account whose hosts never pass", action: accountAction, accountUnproven: true, attemptCount: 60, wantOutcome: "unproven_account", wantRetryAfter: time.Hour, wantWhy: true},
		{name: "an account whose hosts never pass all day", action: accountAction, accountUnproven: true, attemptCount: 1440, wantOutcome: "unproven_account", wantRetryAfter: 6 * time.Hour, wantWhy: true},
	} {
		counts = counts[:0]
		outcome, retryAfter, message := service.budgetRefusalAnswer(refusal(c.action), 4, c.accountUnproven, countAttempts(c.attemptCount))
		connect.AssertEqual(t, outcome, c.wantOutcome)
		connect.AssertEqual(t, retryAfter, c.wantRetryAfter)
		wantStart := refusal(c.action)
		wantStart.RetryAfter = c.wantRetryAfter
		if !strings.HasPrefix(message, wantStart.Error()) {
			t.Errorf("%s: the refusal %q does not start %q", c.name, message, wantStart.Error())
		}
		why := strings.Contains(message, "None of your account's extenders has passed activation over IPv4 in the last 7 days")
		if why != c.wantWhy {
			t.Errorf("%s: the refusal %q says why it waits longer: %t, want %t", c.name, message, why, c.wantWhy)
		}
		if c.accountUnproven {
			if len(counts) != 1 || !counts[0].Equal(now.Add(-24*time.Hour)) {
				t.Errorf("%s: the attempts were counted since %v, want once since a day ago", c.name, counts)
			}
		} else if len(counts) != 0 {
			t.Errorf("%s: the attempts were counted for a refusal that does not wait longer", c.name)
		}
	}

	// off, the account whose hosts never pass is answered as any account
	settings.UnprovenAccountRetryAfterMax = 0
	outcome, retryAfter, message := service.budgetRefusalAnswer(refusal(accountAction), 4, true, countAttempts(1440))
	connect.AssertEqual(t, outcome, "account")
	connect.AssertEqual(t, retryAfter, 59*time.Minute)
	accountRefusal := refusal(accountAction)
	connect.AssertEqual(t, message, accountRefusal.Error())
}

// The lookback in the refusal's words.
func TestExtenderActivateLookbackText(t *testing.T) {
	connect.AssertEqual(t, extenderActivateLookbackText(7*24*time.Hour), "7 days")
	connect.AssertEqual(t, extenderActivateLookbackText(24*time.Hour), "day")
	connect.AssertEqual(t, extenderActivateLookbackText(36*time.Hour), "36 hours")
}
