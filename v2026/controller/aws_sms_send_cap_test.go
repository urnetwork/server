package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The send caps of account texts (aws_sms_send_cap.go): the production sender,
// pointed at a local stand-in for Pinpoint so no test calls AWS, with the
// production caps in the test Redis and a clock the tests move.
//
// Root cause: the only limits on the texts of verification and password reset
// codes were per email or phone and per client address. Codes asked for many
// different numbers met neither, so every text of a burst reached Pinpoint, and
// unexpected high-cost SMS volume could spend the month's send budget in hours.
// The hourly caps alone still admitted volume spread over hours, each under
// the hourly caps: up to 24 hourly caps a day (owner decision 2026-11-26: daily
// caps and an alert before the AWS SMS limit is raised).

// The answer of the Pinpoint stand-in to a SendTextMessage call.
const (
	smsSendCapTestContentType = "application/x-amz-json-1.0"
	smsSendCapTestAnswer      = `{"MessageId":"synthetic-message-id"}`
)

// When the tests' clock starts: on the hour, a moment no window has seen.
var smsSendCapTestStart = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

// A clock the tests move by hand.
type smsSendCapTestClock struct {
	now time.Time
}

func (self *smsSendCapTestClock) Now() time.Time {
	return self.now
}

// A sender pointed at the stand-in whose texts pass the production caps, kept
// in the test Redis, reading the test clock.
func (self *awsEndpointFixture) cappedSender(clock *smsSendCapTestClock) *AWSMessageSender {
	sender := self.sender(5 * time.Second)
	sender.smsCaps = &redisSmsSendCaps{now: clock.Now}
	return sender
}

// The texts a number format makes in one country: two digits replace %02d.
var smsSendCapTestNumberFormats = map[string]string{
	"US": "+1 201-555-01%02d",
	"CA": "+1 613-555-01%02d",
	"GB": "+44 7400 1234%02d",
	"FR": "+33 6 12 34 56 %02d",
	"DE": "+49 1512 34567%02d",
	"NL": "+31 6 123456%02d",
}

// count distinct numbers in country, each checked to be in it.
func smsSendCapTestNumbers(t testing.TB, country string, count int) []string {
	format, ok := smsSendCapTestNumberFormats[country]
	if !ok || 100 < count {
		t.Fatalf("no %d test numbers in %s", count, country)
	}
	numbers := []string{}
	for i := range count {
		number := fmt.Sprintf(format, i)
		if got := smsDestinationCountry(number); got != country {
			t.Fatalf("test number %s is in %q, want %s", number, got, country)
		}
		numbers = append(numbers, number)
	}
	return numbers
}

// Sends a verification code text to number, a second of the test clock after
// the last text.
func sendSmsSendCapTestText(t testing.TB, sender *AWSMessageSender, clock *smsSendCapTestClock, number string) error {
	return sendSmsSendCapTestTextAt(t, sender, clock, clock.now.Add(time.Second), number)
}

// Sends a verification code text to number with the test clock at at.
func sendSmsSendCapTestTextAt(t testing.TB, sender *AWSMessageSender, clock *smsSendCapTestClock, at time.Time, number string) error {
	clock.now = at
	return sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate(number, &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
	})
}

// The trailing window of each cap, by the kind a refusal names.
var smsSendCapTestWindows = map[string]time.Duration{
	smsSendCapGlobal:       AccountSmsSendCapWindow,
	smsSendCapCountry:      AccountSmsSendCapWindow,
	smsSendCapGlobalDaily:  AccountSmsSendDailyCapWindow,
	smsSendCapCountryDaily: AccountSmsSendDailyCapWindow,
}

// The refusal of err by capKind, failing the test unless err is one: a rate
// limit (the 429 status and a wait no longer than the cap's window) to country.
func requireSmsSendCapRefusal(t testing.TB, err error, capKind string, country string) *smsSendCapRefusal {
	var refusal *smsSendCapRefusal
	if err == nil {
		t.Fatalf("the text was sent, want a refusal by the %s send cap", capKind)
	}
	if !errors.As(err, &refusal) {
		t.Fatalf("send error = %v, want a refusal by the %s send cap", err, capKind)
	}
	if refusal.capKind != capKind || refusal.country != country {
		t.Fatalf("refused by the %s cap for %s, want the %s cap for %s", refusal.capKind, refusal.country, capKind, country)
	}
	var rateLimit interface{ RetryAfterSeconds() int }
	if !strings.HasPrefix(err.Error(), "429 ") || !errors.As(err, &rateLimit) {
		t.Fatalf("refusal %q is not a rate limit", err)
	}
	window, ok := smsSendCapTestWindows[capKind]
	if !ok {
		t.Fatalf("no send cap %q", capKind)
	}
	if refusal.retryAfter <= 0 || window < refusal.retryAfter || rateLimit.RetryAfterSeconds() <= 0 {
		t.Fatalf("refusal waits %s (%d s), want a wait within the %s cap's %s window", refusal.retryAfter, rateLimit.RetryAfterSeconds(), capKind, window)
	}
	return refusal
}

// Texts to many different numbers in one country stop at the country's cap:
// Pinpoint is asked for no text past it, and texts to other countries still go.
func TestSmsSendCapStopsTextsToManyNumbersInOneCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapCountry))

		// the whole burst first, then what reached Pinpoint, then each answer
		const overCap = 5
		numbers := smsSendCapTestNumbers(t, "FR", AccountSmsSendCountryCap+overCap)
		sendErrs := []error{}
		for _, number := range numbers {
			sendErrs = append(sendErrs, sendSmsSendCapTestText(t, sender, clock, number))
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendCountryCap {
			t.Fatalf("Pinpoint was asked for %d of %d texts to different numbers in one country, want %d: the country cap must stop the rest before the call", calls, len(numbers), AccountSmsSendCountryCap)
		}
		for i, err := range sendErrs {
			if i < AccountSmsSendCountryCap {
				if err != nil {
					t.Fatalf("text %d to a new number under the country cap: send error = %v", i, err)
				}
				continue
			}
			refusal := requireSmsSendCapRefusal(t, err, smsSendCapCountry, "FR")
			// room again when the first text leaves the window
			if want := AccountSmsSendCapWindow - time.Duration(i)*time.Second; refusal.retryAfter != want {
				t.Fatalf("text %d: refusal waits %s, want %s", i, refusal.retryAfter, want)
			}
		}
		if refused := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapCountry)) - refusedBefore; refused != overCap {
			t.Fatalf("counted %v country cap refusals, want %d", refused, overCap)
		}

		// another country's cap is its own
		for _, number := range smsSendCapTestNumbers(t, "DE", 2) {
			if err := sendSmsSendCapTestText(t, sender, clock, number); err != nil {
				t.Fatalf("text to %s after another country's cap was spent: send error = %v", number, err)
			}
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendCountryCap+2 {
			t.Fatalf("Pinpoint was asked for %d texts, want %d", calls, AccountSmsSendCountryCap+2)
		}
	})
}

// The global cap binds across countries, each under its own cap, and a text a
// cap refuses spends neither cap: the texts the country cap refused leave the
// global cap's room to texts elsewhere.
func TestSmsSendCapGlobalBindsAcrossCountries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapGlobal))

		// a burst at one country, which its cap stops; then texts to several
		// countries in turn, none past its own cap. The whole burst first, then
		// what reached Pinpoint, then each answer.
		type sentText struct {
			country string
			err     error
		}
		sent := []sentText{}
		const refusedByCountry = 5
		for _, number := range smsSendCapTestNumbers(t, "GB", AccountSmsSendCountryCap+refusedByCountry) {
			sent = append(sent, sentText{country: "GB", err: sendSmsSendCapTestText(t, sender, clock, number)})
		}
		countries := []string{"US", "CA", "FR", "DE", "NL"}
		numbers := map[string][]string{}
		for _, country := range countries {
			numbers[country] = smsSendCapTestNumbers(t, country, 5)
		}
		for i := range 5 {
			for _, country := range countries {
				sent = append(sent, sentText{country: country, err: sendSmsSendCapTestText(t, sender, clock, numbers[country][i])})
			}
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendGlobalCap {
			t.Fatalf("Pinpoint was asked for %d of %d texts across %d countries, want the global cap %d", calls, len(sent), 1+len(countries), AccountSmsSendGlobalCap)
		}

		// the country cap admits its cap and refuses the rest, which must not
		// spend the global cap: the global cap then admits exactly its
		// remaining room
		admitted := 0
		globalRefusals := 0
		for i, text := range sent {
			switch {
			case text.country == "GB" && AccountSmsSendCountryCap <= i:
				requireSmsSendCapRefusal(t, text.err, smsSendCapCountry, "GB")
			case admitted < AccountSmsSendGlobalCap:
				if text.err != nil {
					t.Fatalf("text %d to %s with %d of the global cap spent: send error = %v", i, text.country, admitted, text.err)
				}
				admitted += 1
			default:
				requireSmsSendCapRefusal(t, text.err, smsSendCapGlobal, text.country)
				globalRefusals += 1
			}
		}
		if want := AccountSmsSendCountryCap + 5*len(countries) - AccountSmsSendGlobalCap; globalRefusals != want {
			t.Fatalf("the global cap refused %d texts, want %d", globalRefusals, want)
		}
		if refused := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapGlobal)) - refusedBefore; refused != float64(globalRefusals) {
			t.Fatalf("counted %v global cap refusals, want %d", refused, globalRefusals)
		}
	})
}

// A cap has room again once the window has passed its texts, with no wait in
// real time: the first text sent leaves first.
func TestSmsSendCapAdmitsAgainOnceTheWindowPasses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)

		numbers := smsSendCapTestNumbers(t, "NL", AccountSmsSendCountryCap+3)
		for i := range AccountSmsSendCountryCap {
			if err := sendSmsSendCapTestText(t, sender, clock, numbers[i]); err != nil {
				t.Fatalf("text %d under the country cap: send error = %v", i, err)
			}
		}
		// the first text was sent a second after the start
		firstSent := smsSendCapTestStart.Add(time.Second)
		requireSmsSendCapRefusal(t, sendSmsSendCapTestText(t, sender, clock, numbers[AccountSmsSendCountryCap]), smsSendCapCountry, "NL")

		// a millisecond before the first text leaves the window, still none
		clock.now = firstSent.Add(AccountSmsSendCapWindow - time.Millisecond)
		err := sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(numbers[AccountSmsSendCountryCap], &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		})
		if refusal := requireSmsSendCapRefusal(t, err, smsSendCapCountry, "NL"); refusal.retryAfter != time.Millisecond {
			t.Fatalf("refusal a millisecond before the window passes waits %s, want 1ms", refusal.retryAfter)
		}

		// once it has left, one more text has room, and the next does not
		clock.now = firstSent.Add(AccountSmsSendCapWindow)
		err = sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(numbers[AccountSmsSendCountryCap], &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		})
		if err != nil {
			t.Fatalf("text after the window passed the first text: send error = %v", err)
		}
		err = sendWithGuard(t, func() error {
			return sender.SendAccountMessageTemplate(numbers[AccountSmsSendCountryCap+1], &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		})
		requireSmsSendCapRefusal(t, err, smsSendCapCountry, "NL")

		// a full window after the last text, the cap is empty again
		clock.now = firstSent.Add(2 * AccountSmsSendCapWindow)
		for i := AccountSmsSendCountryCap + 1; i < len(numbers); i += 1 {
			if err := sendSmsSendCapTestText(t, sender, clock, numbers[i]); err != nil {
				t.Fatalf("text %d a window after the last: send error = %v", i, err)
			}
		}
		if calls, want := endpoint.requestCount.Load(), int64(AccountSmsSendCountryCap+1+2); calls != want {
			t.Fatalf("Pinpoint was asked for %d texts, want %d", calls, want)
		}
	})
}

// A country's daily cap binds on texts spread over hours with every hourly
// window under its caps: a text every 10 minutes to new numbers in one country,
// at most 6 in any hour where its hourly cap is 10, stops at its daily cap of
// 30. Pinpoint is asked for no text past it, each refusal names the daily cap
// and waits until the first text is 24 hours old, and texts to another country
// still go.
func TestSmsSendDailyCapBindsACountryAcrossHours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapCountryDaily))

		const spacing = 10 * time.Minute
		if perHour := int(AccountSmsSendCapWindow / spacing); AccountSmsSendCountryCap <= perHour {
			t.Fatalf("%d texts an hour reach the hourly country cap %d", perHour, AccountSmsSendCountryCap)
		}
		// the whole run first, then what reached Pinpoint, then each answer
		const overCap = 6
		numbers := smsSendCapTestNumbers(t, "FR", AccountSmsSendCountryDailyCap+overCap)
		sentAt := []time.Time{}
		sendErrs := []error{}
		for i, number := range numbers {
			at := smsSendCapTestStart.Add(time.Duration(i) * spacing)
			sentAt = append(sentAt, at)
			sendErrs = append(sendErrs, sendSmsSendCapTestTextAt(t, sender, clock, at, number))
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendCountryDailyCap {
			t.Fatalf("Pinpoint was asked for %d of %d texts to one country over %s, at most 6 in any hour, want the daily cap %d: the daily cap must stop the rest before the call", calls, len(numbers), sentAt[len(sentAt)-1].Sub(sentAt[0]), AccountSmsSendCountryDailyCap)
		}
		for i, err := range sendErrs {
			if i < AccountSmsSendCountryDailyCap {
				if err != nil {
					t.Fatalf("text %d at +%s, under every cap: send error = %v", i, sentAt[i].Sub(smsSendCapTestStart), err)
				}
				continue
			}
			refusal := requireSmsSendCapRefusal(t, err, smsSendCapCountryDaily, "FR")
			if refusal.capLimit != AccountSmsSendCountryDailyCap {
				t.Fatalf("text %d: refused by a cap of %d, want %d", i, refusal.capLimit, AccountSmsSendCountryDailyCap)
			}
			// room again when the first text is 24 hours old
			if want := sentAt[0].Add(AccountSmsSendDailyCapWindow).Sub(sentAt[i]); refusal.retryAfter != want {
				t.Fatalf("text %d: refusal waits %s, want %s", i, refusal.retryAfter, want)
			}
		}
		if refused := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapCountryDaily)) - refusedBefore; refused != overCap {
			t.Fatalf("counted %v country daily cap refusals, want %d", refused, overCap)
		}

		// another country's daily cap is its own
		last := sentAt[len(sentAt)-1]
		for i, number := range smsSendCapTestNumbers(t, "DE", 2) {
			if err := sendSmsSendCapTestTextAt(t, sender, clock, last.Add(time.Duration(i+1)*time.Second), number); err != nil {
				t.Fatalf("text to %s after another country's daily cap was spent: send error = %v", number, err)
			}
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendCountryDailyCap+2 {
			t.Fatalf("Pinpoint was asked for %d texts, want %d", calls, AccountSmsSendCountryDailyCap+2)
		}
	})
}

// The global daily cap binds on texts spread over hours and countries with
// every hourly window and every country's daily window under its cap: a round
// of one text to each of six countries every 15 minutes, at most 24 texts in
// any hour where the global hourly cap is 30 and at most 17 to a country in the
// day, stops at the global daily cap of 100. Pinpoint is asked for no text past
// it, and each refusal names the global daily cap and waits until the first
// text is 24 hours old.
func TestSmsSendDailyGlobalCapBindsAcrossCountries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapGlobalDaily))

		countries := []string{"US", "CA", "GB", "FR", "DE", "NL"}
		const rounds = 20
		const roundSpacing = 15 * time.Minute
		numbers := map[string][]string{}
		for _, country := range countries {
			numbers[country] = smsSendCapTestNumbers(t, country, rounds)
		}
		// the whole run first, then what reached Pinpoint, then each answer
		type sentText struct {
			country string
			at      time.Time
			err     error
		}
		sent := []sentText{}
		for round := range rounds {
			for i, country := range countries {
				at := smsSendCapTestStart.Add(time.Duration(round)*roundSpacing + time.Duration(i)*time.Second)
				sent = append(sent, sentText{country: country, at: at, err: sendSmsSendCapTestTextAt(t, sender, clock, at, numbers[country][round])})
			}
		}
		if calls := endpoint.requestCount.Load(); calls != AccountSmsSendGlobalDailyCap {
			t.Fatalf("Pinpoint was asked for %d of %d texts across %d countries over %s, at most 24 in any hour, want the global daily cap %d: the global daily cap must stop the rest before the call", calls, len(sent), len(countries), sent[len(sent)-1].at.Sub(sent[0].at), AccountSmsSendGlobalDailyCap)
		}
		for i, text := range sent {
			if i < AccountSmsSendGlobalDailyCap {
				if text.err != nil {
					t.Fatalf("text %d to %s at +%s, under every cap: send error = %v", i, text.country, text.at.Sub(smsSendCapTestStart), text.err)
				}
				continue
			}
			refusal := requireSmsSendCapRefusal(t, text.err, smsSendCapGlobalDaily, text.country)
			if refusal.capLimit != AccountSmsSendGlobalDailyCap {
				t.Fatalf("text %d: refused by a cap of %d, want %d", i, refusal.capLimit, AccountSmsSendGlobalDailyCap)
			}
			// room again when the first text is 24 hours old
			if want := sent[0].at.Add(AccountSmsSendDailyCapWindow).Sub(text.at); refusal.retryAfter != want {
				t.Fatalf("text %d: refusal waits %s, want %s", i, refusal.retryAfter, want)
			}
		}
		if refused, want := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(smsSendCapGlobalDaily))-refusedBefore, len(sent)-AccountSmsSendGlobalDailyCap; refused != float64(want) {
			t.Fatalf("counted %v global daily cap refusals, want %d", refused, want)
		}
	})
}

// A text a cap refuses spends none of the four windows, hourly or daily. Texts
// the hourly country cap refused leave the global hourly cap, the country's
// daily cap and the global daily cap their room; texts the country's daily cap
// refused leave the global hourly cap and the global daily cap theirs; texts
// the global daily cap refused leave the global and country hourly caps
// theirs. So each window fills with exactly the texts sent, and a text that
// finds several caps full names the one with the longest wait.
func TestSmsSendCapRefusedTextsSpendNoWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)

		numbers := map[string][]string{}
		for _, country := range []string{"US", "CA", "GB", "FR", "DE", "NL"} {
			numbers[country] = smsSendCapTestNumbers(t, country, 100)
		}
		nextNumber := map[string]int{}
		sentCount := int64(0)
		// From at, sends count texts to country a second apart: all of them
		// first, then what reached Pinpoint, then each answer. An empty
		// capKind wants every text sent, any other every text refused by that
		// cap. Returns the refusals.
		step := func(what string, at time.Time, country string, count int, capKind string) []*smsSendCapRefusal {
			sendErrs := []error{}
			for i := range count {
				number := numbers[country][nextNumber[country]%len(numbers[country])]
				nextNumber[country] += 1
				sendErrs = append(sendErrs, sendSmsSendCapTestTextAt(t, sender, clock, at.Add(time.Duration(i)*time.Second), number))
			}
			want := sentCount
			if capKind == "" {
				want += int64(count)
			}
			if calls := endpoint.requestCount.Load(); calls != want {
				t.Fatalf("%s: Pinpoint was asked for %d texts in all, want %d", what, calls, want)
			}
			sentCount = want
			refusals := []*smsSendCapRefusal{}
			for i, err := range sendErrs {
				if capKind == "" {
					if err != nil {
						t.Fatalf("%s: text %d to %s: send error = %v", what, i, country, err)
					}
					continue
				}
				var refusal *smsSendCapRefusal
				if !errors.As(err, &refusal) || refusal.capKind != capKind {
					got := "no refusal"
					if refusal != nil {
						got = "a refusal by the " + refusal.capKind + " cap"
					}
					t.Fatalf("%s: text %d to %s got %s (send error = %v), want a refusal by the %s cap", what, i, country, got, err, capKind)
				}
				refusals = append(refusals, requireSmsSendCapRefusal(t, err, capKind, country))
			}
			return refusals
		}
		// hours and minutes after the start. Each block of texts starts more
		// than an hour after the last, so no block's texts are in another's
		// hourly windows, except the refused texts at 23:50, which the hourly
		// windows of the texts at 24:35 must hold if they were counted
		at := func(hours int, minutes int) time.Time {
			return smsSendCapTestStart.Add(time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute)
		}

		// FR's hourly cap admits 10 and refuses 20, which leave the global
		// hourly cap its room, so 20 more to DE and NL fill it
		step("FR under its hourly cap", at(0, 0), "FR", AccountSmsSendCountryCap, "")
		step("FR past its hourly cap", at(0, 1), "FR", 20, smsSendCapCountry)
		step("DE after FR's refused texts", at(0, 2), "DE", AccountSmsSendCountryCap, "")
		step("NL after FR's refused texts", at(0, 3), "NL", AccountSmsSendCountryCap, "")
		step("GB past the global hourly cap", at(0, 4), "GB", 1, smsSendCapGlobal)

		// the 20 texts FR's hourly cap refused left its daily cap the room for
		// two more blocks of 10, which fill it
		step("FR in a second block", at(1, 30), "FR", AccountSmsSendCountryCap, "")
		step("FR in a third block", at(3, 0), "FR", AccountSmsSendCountryCap, "")
		// both of FR's caps are full, and the daily one waits longer
		step("FR past both its caps", at(3, 1), "FR", 1, smsSendCapCountryDaily)

		// 40 texts FR's daily cap refused leave the global hourly cap its
		// room, so 30 to five other countries fill it
		step("FR past its daily cap", at(4, 30), "FR", 40, smsSendCapCountryDaily)
		for i, country := range []string{"US", "CA", "GB", "DE", "NL"} {
			step(country+" after FR's daily refusals", at(4, 31+i), country, 6, "")
		}
		step("GB past the global hourly cap again", at(4, 40), "GB", 1, smsSendCapGlobal)

		// no refused text spent the global daily cap, so with 80 texts sent,
		// exactly 20 more fill it
		for i, country := range []string{"US", "CA", "GB", "DE", "NL"} {
			step(country+" up to the global daily cap", at(6, i), country, 4, "")
		}
		step("US past the global daily cap", at(6, 10), "US", 1, smsSendCapGlobalDaily)

		// 30 texts the global daily cap refused leave the hourly caps their
		// room: once the first block's texts are 24 hours old, 20 to the US
		// fill its hourly cap and 10 to FR the global hourly cap, and the
		// global daily cap fills again
		step("US past the global daily cap a day later", at(23, 50), "US", 30, smsSendCapGlobalDaily)
		step("US after the first block left the daily window", at(24, 35), "US", accountSmsSendCountryCap("US"), "")
		step("FR after the first block left the daily window", at(24, 36), "FR", AccountSmsSendCountryCap, "")
		// both global caps are full: the hourly one waits until its first text,
		// sent at 24:35, is an hour old, 58 minutes; the daily one until its
		// first, FR's second block at 1:30, is 24 hours old, 53 minutes
		refusals := step("NL past both global caps", at(24, 37), "NL", 1, smsSendCapGlobal)
		if want := at(24, 35).Add(AccountSmsSendCapWindow).Sub(at(24, 37)); refusals[0].retryAfter != want {
			t.Fatalf("a text past both global caps waits %s, want the hourly cap's %s", refusals[0].retryAfter, want)
		}
	})
}

// The daily window slides as the hourly one does: a country's daily cap has
// room again when its first text is 24 hours old, for one text, and the next
// has room when the second text is. Midnight UTC frees nothing.
func TestSmsSendDailyCapWindowSlides(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
		clock := &smsSendCapTestClock{now: smsSendCapTestStart}
		sender := endpoint.cappedSender(clock)

		// a text every 10 minutes, at most 6 in any hour, until NL's daily cap
		// is spent
		const spacing = 10 * time.Minute
		numbers := smsSendCapTestNumbers(t, "NL", AccountSmsSendCountryDailyCap+3)
		sentAt := []time.Time{}
		for i := range AccountSmsSendCountryDailyCap {
			at := smsSendCapTestStart.Add(time.Duration(i) * spacing)
			sentAt = append(sentAt, at)
			if err := sendSmsSendCapTestTextAt(t, sender, clock, at, numbers[i]); err != nil {
				t.Fatalf("text %d at +%s, under every cap: send error = %v", i, at.Sub(smsSendCapTestStart), err)
			}
		}
		sent := int64(AccountSmsSendCountryDailyCap)
		// what reached Pinpoint first, then the answer
		refusedAt := func(what string, at time.Time, number string, wantWait time.Duration) {
			err := sendSmsSendCapTestTextAt(t, sender, clock, at, number)
			if calls := endpoint.requestCount.Load(); calls != sent {
				t.Fatalf("%s: Pinpoint was asked for %d texts, want %d: NL's daily cap has no room", what, calls, sent)
			}
			if refusal := requireSmsSendCapRefusal(t, err, smsSendCapCountryDaily, "NL"); refusal.retryAfter != wantWait {
				t.Fatalf("%s: refusal waits %s, want %s", what, refusal.retryAfter, wantWait)
			}
		}
		sentAtTime := func(what string, at time.Time, number string) {
			if err := sendSmsSendCapTestTextAt(t, sender, clock, at, number); err != nil {
				t.Fatalf("%s: send error = %v", what, err)
			}
			sent += 1
			if calls := endpoint.requestCount.Load(); calls != sent {
				t.Fatalf("%s: Pinpoint was asked for %d texts, want %d", what, calls, sent)
			}
		}
		leaves := func(i int) time.Time {
			return sentAt[i].Add(AccountSmsSendDailyCapWindow)
		}

		// the next text, 10 minutes after the last, waits for the first to be
		// 24 hours old
		next := sentAt[len(sentAt)-1].Add(spacing)
		refusedAt("10 minutes after the last text", next, numbers[30], leaves(0).Sub(next))
		midnight := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
		if !next.Before(midnight) || !midnight.Before(leaves(0)) {
			t.Fatalf("midnight UTC %s is not between the last text and the first one's 24 hours", midnight)
		}
		refusedAt("at midnight UTC", midnight, numbers[30], leaves(0).Sub(midnight))
		refusedAt("a millisecond before the first text is 24 hours old", leaves(0).Add(-time.Millisecond), numbers[30], time.Millisecond)

		// one text has room once the first has left, and the next only once
		// the second has
		sentAtTime("once the first text is 24 hours old", leaves(0), numbers[30])
		refusedAt("the next text at the same moment", leaves(0), numbers[31], leaves(1).Sub(leaves(0)))
		sentAtTime("once the second text is 24 hours old", leaves(1), numbers[31])
		refusedAt("the next text at that moment", leaves(1), numbers[32], leaves(2).Sub(leaves(1)))
	})
}

// Send caps that refuse every text by one cap.
type refuseEverySms struct {
	capKind string
}

func (self *refuseEverySms) admitSms(ctx context.Context, phoneNumber string) error {
	return &smsSendCapRefusal{
		capKind:    self.capKind,
		capLimit:   1,
		country:    smsDestinationCountry(phoneNumber),
		retryAfter: 90 * time.Second,
	}
}

// A refused text never reaches Pinpoint: the sender returns the refusal, a
// rate limit, and counts it by its cap, the daily caps included. Every cap's
// count starts at zero, so the first refusal is an increase the alert sees.
// The production sender's caps are the Redis caps on the server clock. No
// Redis: the caps here are a fake.
func TestSmsSendCapRefusalStopsTheTextBeforePinpoint(t *testing.T) {
	if caps, ok := (&AWSMessageSender{}).sendCaps().(*redisSmsSendCaps); !ok || caps.now != nil {
		t.Fatalf("the production sender's caps are %T, want the Redis caps on the server clock", (&AWSMessageSender{}).sendCaps())
	}
	capKinds := []string{smsSendCapGlobal, smsSendCapCountry, smsSendCapGlobalDaily, smsSendCapCountryDaily}
	if count := testutil.CollectAndCount(smsSendCapRefusalCounter); count != len(capKinds) {
		t.Fatalf("the refusal metric has %d series, want one for each of the %d caps", count, len(capKinds))
	}

	endpoint := newAwsEndpoint(t, smsSendCapTestContentType, smsSendCapTestAnswer)
	for _, capKind := range capKinds {
		sender := endpoint.sender(5 * time.Second)
		sender.smsCaps = &refuseEverySms{capKind: capKind}
		refusedBefore := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(capKind))
		sentBefore := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelSms, "sent"))

		for _, template := range []Template{
			&AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"},
			&AuthPasswordResetTemplate{ResetCode: strings.Repeat("ab", 64)},
			&AuthPasswordSetTemplate{},
		} {
			err := sendWithGuard(t, func() error {
				return sender.SendAccountMessageTemplate("+1 201-555-0123", template)
			})
			refusal := requireSmsSendCapRefusal(t, err, capKind, "US")
			if refusal.RetryAfterSeconds() != 90 {
				t.Fatalf("%s: retry after %d s, want 90", template.Name(), refusal.RetryAfterSeconds())
			}
		}
		if calls := endpoint.requestCount.Load(); calls != 0 {
			t.Fatalf("Pinpoint was asked for %d refused texts", calls)
		}
		if refused := testutil.ToFloat64(smsSendCapRefusalCounter.WithLabelValues(capKind)) - refusedBefore; refused != 3 {
			t.Fatalf("counted %v %s cap refusals, want 3", refused, capKind)
		}
		if sent := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelSms, "sent")) - sentBefore; sent != 0 {
			t.Fatalf("counted %v refused texts as sent", sent)
		}
	}
}

// The destination country of a text: the number's calling code, and inside a
// shared calling code (+1, +44, +7) the country the number is in. A number
// that names no one country shares the unknown country's cap.
func TestSmsDestinationCountry(t *testing.T) {
	for _, c := range []struct {
		phoneNumber string
		country     string
	}{
		{"+1 201-555-0123", "US"},
		{"+12015550123", "US"},
		{"+1 (201) 555-0123", "US"},
		{"+1 613-555-0123", "CA"},
		{"+1 416-555-0199", "CA"},
		// another country of the +1 calling code is its own
		{"+1 876-555-0123", "JM"},
		{"+1 787-555-0123", "PR"},
		{"+44 7400 123456", "GB"},
		{"+33 6 12 34 56 78", "FR"},
		{"+49 1512 3456789", "DE"},
		{"+7 912 345-67-89", "RU"},
		{"+7 701 234 5678", "KZ"},
		{"+61 412 345 678", "AU"},
		{"+81 90-1234-5678", "JP"},
		// a non-geographic calling code
		{"+800 1234 5678", "001"},
		// numbers in no one country
		{"+1 555-555-0100", smsUnknownCountry},
		{"+44 7700 900123", smsUnknownCountry},
		{"+999 1234 5678", smsUnknownCountry},
		// a number without its calling code is not taken for a US one
		{"2015550123", smsUnknownCountry},
		{"not a number", smsUnknownCountry},
		{"", smsUnknownCountry},
	} {
		if country := smsDestinationCountry(c.phoneNumber); country != c.country {
			t.Errorf("destination country of %q = %q, want %q", c.phoneNumber, country, c.country)
		}
	}

	// the normal phone user auth, which is what the sender is handed
	for _, c := range []struct {
		userAuth string
		country  string
	}{
		{"+1 201 555 0123", "US"},
		{"+16135550123", "CA"},
		{"+33612345678", "FR"},
	} {
		normalUserAuth, userAuthType := model.NormalUserAuth(c.userAuth)
		if userAuthType != model.UserAuthTypePhone {
			t.Fatalf("%q is not a phone user auth", c.userAuth)
		}
		if country := smsDestinationCountry(normalUserAuth); country != c.country {
			t.Errorf("destination country of the user auth %q = %q, want %q", normalUserAuth, country, c.country)
		}
	}
}

// The caps leave the countries most normal texts go to more room than the
// rest, and no country more than every country together.
func TestAccountSmsSendCapsLeaveRoomForNormalSends(t *testing.T) {
	if AccountSmsSendCapWindow != time.Hour {
		t.Fatalf("the caps count texts over %s, want an hour", AccountSmsSendCapWindow)
	}
	if AccountSmsSendCountryCap <= 0 || AccountSmsSendGlobalCap < AccountSmsSendCountryCap {
		t.Fatalf("country cap %d, global cap %d", AccountSmsSendCountryCap, AccountSmsSendGlobalCap)
	}
	for _, country := range []string{"US", "CA"} {
		if countryCap := accountSmsSendCountryCap(country); countryCap <= AccountSmsSendCountryCap || AccountSmsSendGlobalCap < countryCap {
			t.Fatalf("%s cap %d, want more than the %d of other countries and at most the global %d", country, countryCap, AccountSmsSendCountryCap, AccountSmsSendGlobalCap)
		}
	}
	if countryCap := accountSmsSendCountryCap(smsUnknownCountry); countryCap != AccountSmsSendCountryCap {
		t.Fatalf("unknown country cap %d, want %d", countryCap, AccountSmsSendCountryCap)
	}
}

// The caps the owner set (2026-11-25 hourly, 2026-11-26 daily): 30 texts an
// hour and 100 a day across every service; 10 an hour and 30 a day to one
// country, the US and Canada 20 and 60, and numbers in no one country 10 and
// 30. Each daily cap is at least its hourly cap and under 24 of them, so it
// binds on volume the hourly cap admits.
func TestAccountSmsSendDailyCapsAreTheOwnersValues(t *testing.T) {
	if AccountSmsSendDailyCapWindow != 24*time.Hour {
		t.Fatalf("the daily caps count texts over %s, want 24 hours", AccountSmsSendDailyCapWindow)
	}
	if AccountSmsSendGlobalCap != 30 || AccountSmsSendGlobalDailyCap != 100 {
		t.Fatalf("global caps %d an hour and %d a day, want 30 and 100", AccountSmsSendGlobalCap, AccountSmsSendGlobalDailyCap)
	}
	for _, c := range []struct {
		country       string
		hourly, daily int
	}{
		{"US", 20, 60},
		{"CA", 20, 60},
		{"FR", 10, 30},
		{"GB", 10, 30},
		{smsUnknownCountry, 10, 30},
	} {
		hourly, daily := accountSmsSendCountryCap(c.country), accountSmsSendCountryDailyCap(c.country)
		if hourly != c.hourly || daily != c.daily {
			t.Errorf("%s caps %d an hour and %d a day, want %d and %d", c.country, hourly, daily, c.hourly, c.daily)
		}
	}
	for _, c := range []struct {
		name          string
		hourly, daily int
	}{
		{"global", AccountSmsSendGlobalCap, AccountSmsSendGlobalDailyCap},
		{"US", accountSmsSendCountryCap("US"), accountSmsSendCountryDailyCap("US")},
		{"another country", AccountSmsSendCountryCap, AccountSmsSendCountryDailyCap},
	} {
		if c.daily < c.hourly || 24*c.hourly <= c.daily {
			t.Errorf("%s daily cap %d, want at least its hourly cap %d and under 24 of them", c.name, c.daily, c.hourly)
		}
	}
}

// A code whose text the send caps refused reaches the client as a rate limit
// with the caps' wait and message, as the code budgets' refusals do, and never
// as a failed send: after sign-up and login (send_error), on
// /auth/verify-send and /auth/password-reset (error), and as the 429 with
// Retry-After an older client is answered. A daily cap's refusal maps the same
// way, with a wait that can be most of a day. No database or Redis.
func TestAuthCodeSendsReportAnSmsSendCapRefusalAsARateLimit(t *testing.T) {
	for _, c := range []struct {
		refusal               *smsSendCapRefusal
		wantRetryAfterSeconds int
	}{
		{
			refusal: &smsSendCapRefusal{
				capKind:    smsSendCapCountry,
				capLimit:   AccountSmsSendCountryCap,
				country:    "FR",
				retryAfter: 41*time.Minute + 500*time.Millisecond,
			},
			wantRetryAfterSeconds: 41*60 + 1,
		},
		{
			refusal: &smsSendCapRefusal{
				capKind:    smsSendCapCountryDaily,
				capLimit:   AccountSmsSendCountryDailyCap,
				country:    "FR",
				retryAfter: 19*time.Hour + 500*time.Millisecond,
			},
			wantRetryAfterSeconds: 19*60*60 + 1,
		},
	} {
		refusal := c.refusal
		wantRetryAfterSeconds := c.wantRetryAfterSeconds
		requireCapError := func(t *testing.T, path string, sendError *AuthVerifySendError) {
			path = refusal.capKind + " cap, " + path
			if sendError == nil || sendError.Code != model.AuthVerifySendErrorCodeRateLimited {
				t.Fatalf("%s: error = %+v, want %q", path, sendError, model.AuthVerifySendErrorCodeRateLimited)
			}
			if sendError.Message != smsSendCapRefusedMessage || sendError.RetryAfterSeconds != wantRetryAfterSeconds {
				t.Fatalf("%s: error = %+v, want the caps' message and a wait of %d s", path, sendError, wantRetryAfterSeconds)
			}
			legacyErr := legacyAuthVerifySendError(sendError)
			var rateLimit interface{ RetryAfterSeconds() int }
			if legacyErr.Error() != "429 "+smsSendCapRefusedMessage || !errors.As(legacyErr, &rateLimit) || rateLimit.RetryAfterSeconds() != wantRetryAfterSeconds {
				t.Fatalf("%s: older clients are answered %v, want the 429 with Retry-After %d", path, legacyErr, wantRetryAfterSeconds)
			}
		}

		sender := &fakeVerifySendMessageSender{err: refusal}
		verifyResult, err := authVerifySend(
			AuthVerifySendArgs{UserAuth: "+1 201-555-0123", ResultErrors: true},
			verifySendTestSession(),
			createCodeOk,
			sender,
		)
		if err != nil || sender.sendCount != 1 {
			t.Fatalf("verify send: err = %v, send count = %d", err, sender.sendCount)
		}
		requireCapError(t, "/auth/verify-send", verifyResult.Error)

		verifySend := func(args AuthVerifySendArgs, clientSession *session.ClientSession) (*AuthVerifySendResult, error) {
			return authVerifySend(args, clientSession, createCodeOk, &fakeVerifySendMessageSender{err: refusal})
		}
		loginResult, err := authLoginWithPassword(
			model.AuthLoginWithPasswordArgs{UserAuth: "+1 201-555-0123", Password: "password"},
			verifySendTestSession(),
			verificationRequiredLogin,
			verifySend,
		)
		if err != nil {
			t.Fatalf("login: err = %v", err)
		}
		requireCapError(t, "login verification_required.send_error", loginResult.VerificationRequired.SendError)

		resetCode := strings.Repeat("ab", 64)
		createResetCode := func(model.AuthPasswordResetCreateCodeArgs, *session.ClientSession) (*model.AuthPasswordResetCreateCodeResult, error) {
			return &model.AuthPasswordResetCreateCodeResult{ResetCode: &resetCode}, nil
		}
		sender = &fakeVerifySendMessageSender{err: refusal}
		resetResult, err := authPasswordReset(
			AuthPasswordResetArgs{UserAuth: "+1 201-555-0123", ResultErrors: true},
			verifySendTestSession(),
			createResetCode,
			sender,
		)
		if err != nil || sender.sendCount != 1 {
			t.Fatalf("password reset: err = %v, send count = %d", err, sender.sendCount)
		}
		requireCapError(t, "/auth/password-reset", resetResult.Error)
	}

	// any other send error is still a failed send
	sender := &fakeVerifySendMessageSender{err: fmt.Errorf("pinpoint: %w", errors.New("throttled"))}
	verifyResult, err := authVerifySend(
		AuthVerifySendArgs{UserAuth: "+1 201-555-0123", ResultErrors: true},
		verifySendTestSession(),
		createCodeOk,
		sender,
	)
	if err != nil || verifyResult.Error == nil || verifyResult.Error.Code != model.AuthVerifySendErrorCodeSendFailed {
		t.Fatalf("a failed text: result = %+v, err = %v, want %q", verifyResult, err, model.AuthVerifySendErrorCodeSendFailed)
	}
}
