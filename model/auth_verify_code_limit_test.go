package model

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The decisions of the verification code limits alone, without a database or
// Redis (auth_verify_code_limit_db_test.go drives them through AuthVerify).

// No lockout after an address's first strike, so a person who mistyped asks
// for a new code at once; then longer each time, up to a day.
func TestVerifyCodeLockoutLadder(t *testing.T) {
	for strikes, want := range []time.Duration{
		0,
		0,
		15 * time.Minute,
		time.Hour,
		4 * time.Hour,
		24 * time.Hour,
		24 * time.Hour,
		24 * time.Hour,
	} {
		if got := verifyCodeLockout(strikes); got != want {
			t.Errorf("lockout after %d strikes = %s, want %s", strikes, got, want)
		}
	}
}

// A code takes VerifyCodeTryLimit tries. Its last try, when wrong, kills it and
// strikes the address; a try past them is never compared, not even a right
// one.
func TestDecideVerifyCodeTry(t *testing.T) {
	const code = "123456"
	const other = "abcdef01"
	for _, c := range []struct {
		name   string
		codes  []string
		tries  []int64
		guess  string
		match  int
		spent  []int
		strike bool
	}{
		{"right first try", []string{code}, []int64{1}, code, 0, nil, false},
		{"right after four wrong tries", []string{code}, []int64{5}, code, 0, nil, false},
		{"first wrong try", []string{code}, []int64{1}, "123457", -1, nil, false},
		{"fourth wrong try", []string{code}, []int64{4}, "123457", -1, nil, false},
		{"fifth wrong try kills the code", []string{code}, []int64{5}, "123457", -1, []int{0}, true},
		{"right code after five tries", []string{code}, []int64{6}, code, -1, []int{0}, false},
		{"wrong try past the tries", []string{code}, []int64{9}, "123457", -1, []int{0}, false},
		{"a guess of another length", []string{code}, []int64{1}, "1234567", -1, nil, false},
		{"an empty guess", []string{code}, []int64{1}, "", -1, nil, false},
		{"no live code", []string{}, []int64{}, code, -1, nil, false},
		{"right guess of a second live code", []string{other, code}, []int64{5, 3}, code, 1, []int{0}, false},
		{"wrong guess of two live codes", []string{other, code}, []int64{2, 5}, "123457", -1, []int{1}, true},
		{"a spent code is not compared beside a live one", []string{code, other}, []int64{6, 1}, code, -1, []int{0}, false},
	} {
		outcome := decideVerifyCodeTry(c.codes, c.tries, c.guess)
		if outcome.match != c.match {
			t.Errorf("%s: match = %d, want %d", c.name, outcome.match, c.match)
		}
		if !reflect.DeepEqual(outcome.spent, c.spent) {
			t.Errorf("%s: spent = %v, want %v", c.name, outcome.spent, c.spent)
		}
		if outcome.strike != c.strike {
			t.Errorf("%s: strike = %t, want %t", c.name, outcome.strike, c.strike)
		}
	}
}

// Every refusal is a 429 that names its wait and carries it as Retry-After,
// rounded up to whole seconds and never zero.
func TestVerifyCodeRefusalsCarryTheirWait(t *testing.T) {
	type retryAfter interface{ RetryAfterSeconds() int }
	for _, c := range []struct {
		name    string
		err     error
		text    string
		seconds int
	}{
		{"address", verifyCodeAddressLockedError(15 * time.Minute), "for this email or phone. Please try again in 15 minutes.", 900},
		{"address, a day", verifyCodeAddressLockedError(24 * time.Hour), "Please try again in 24 hours.", 86400},
		{"source", verifyCodeSourceRefusedError(30 * time.Minute), "from your network address. Please try again in 30 minutes.", 1800},
		{"network", verifyCodeNetworkRefusedError(time.Hour), "from this account. Please try again in 60 minutes.", 3600},
		{"part of a second", verifyCodeAddressLockedError(1500 * time.Millisecond), "Please try again in 1 minute.", 2},
		{"no wait left", verifyCodeSourceRefusedError(0), "Please try again in 1 minute.", 1},
	} {
		if !strings.HasPrefix(c.err.Error(), "429 ") || !strings.HasSuffix(c.err.Error(), c.text) {
			t.Errorf("%s: message = %q, want a 429 ending %q", c.name, c.err.Error(), c.text)
		}
		var rateLimit retryAfter
		if !errors.As(c.err, &rateLimit) || rateLimit.RetryAfterSeconds() != c.seconds {
			t.Errorf("%s: retry after = %+v, want %d seconds", c.name, rateLimit, c.seconds)
		}
	}
}

// The answer to the try that killed a code names the wait for a new one when
// the strike locked the address out.
func TestAuthVerifyCodeSpentLockedMessage(t *testing.T) {
	for lockout, want := range map[time.Duration]string{
		15 * time.Minute: "Too many wrong codes. This code no longer works. Please request a new code in 15 minutes.",
		4 * time.Hour:    "Too many wrong codes. This code no longer works. Please request a new code in 4 hours.",
	} {
		if got := authVerifyCodeSpentLockedMessage(lockout); got != want {
			t.Errorf("message for a lockout of %s = %q, want %q", lockout, got, want)
		}
	}
	if strings.Contains(AuthVerifyCodeSpentMessage, " in ") {
		t.Errorf("the message of a strike with no lockout names a wait: %q", AuthVerifyCodeSpentMessage)
	}
}

// The limits read the clock a test puts in the request's context, and the
// server's clock otherwise.
func TestVerifyCodeLimitNowReadsTheContextClock(t *testing.T) {
	fixed := time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)
	ctx := context.WithValue(context.Background(), verifyCodeLimitClockKey{}, func() time.Time { return fixed })
	if got := verifyCodeLimitNow(ctx); !got.Equal(fixed) {
		t.Fatalf("now = %s, want the context clock %s", got, fixed)
	}
	before := server.NowUtc()
	got := verifyCodeLimitNow(context.Background())
	if got.Before(before) || server.NowUtc().Before(got) {
		t.Fatalf("now = %s, want the server's clock", got)
	}
}
