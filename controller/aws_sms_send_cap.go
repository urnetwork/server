package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nyaruka/phonenumbers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The send caps of account texts (owner decision 2026-11-25).
//
// The server texts a phone only for account security (smsTemplateNames), and
// the price of a text depends on where it goes: most texts go to the US and
// Canada, and a text to many other countries costs many times as much. The
// limits before this one are per email or phone (model/auth_code_send_limit.go)
// and per client address (model/auth_model_attempt.go), so codes asked for many
// different numbers never meet them, and unexpected high-cost SMS volume could
// spend the month's send budget in hours. Every text now also passes two caps
// right before its Pinpoint call (AWSMessageSender.sendSms), on every path that
// sends one: the verification and password reset codes, and the
// password-changed notice.
//   - The global cap: AccountSmsSendGlobalCap texts in any AccountSmsSendCapWindow,
//     across every service.
//   - The destination country's cap: AccountSmsSendCountryCap texts in the
//     window, or the country's own cap in accountSmsSendCountryCaps.
//
// The caps are several times the busiest hour of normal sends, so they bind
// only on a burst. A text a cap refuses is not sent and spends neither cap. The
// refusal is a rate limit, the same kind the daily code budget of an email or
// phone answers: a 429 with the wait until the cap that refused has room again
// (Retry-After, and `retry_after_seconds` in a verify send result). Each
// refusal is counted by cap (urnetwork_account_message_sms_cap_refusals_total)
// and logged with the destination country, never the number. Caps that cannot
// be read refuse the text as a failed send, so no text goes out uncounted.
//
// The counts live in Redis, as the auth attempt limits do. Each cap is a
// sliding window of the texts it admitted, each scored by when it leaves the
// window, and every key shares one hash tag, so one script reads both caps of
// a text and spends both or neither.

// The trailing window each cap counts texts over.
const AccountSmsSendCapWindow = time.Hour

// The most account texts all services together send in a window.
const AccountSmsSendGlobalCap = 30

// The most account texts sent in a window to one destination country that has
// no cap of its own.
const AccountSmsSendCountryCap = 10

// The destination countries with a cap of their own, by ISO 3166-1 region code.
// Most normal sends go to these.
var accountSmsSendCountryCaps = map[string]int{
	"US": 20,
	"CA": 20,
}

// The cap the texts to a destination country count against.
func accountSmsSendCountryCap(country string) int {
	if countryCap, ok := accountSmsSendCountryCaps[country]; ok {
		return countryCap
	}
	return AccountSmsSendCountryCap
}

// The caps, as a refusal names them and the refusal metric labels them.
const (
	smsSendCapGlobal  = "global"
	smsSendCapCountry = "country"
)

// The destination country of a number that names no one country: one that does
// not parse, or one that matches no country of a calling code several countries
// share. Such texts share one country cap.
const smsUnknownCountry = phonenumbers.UNKNOWN_REGION

// A region code: two letters, or "001" for the non-geographic calling codes.
var smsCountryPattern = regexp.MustCompile(`^(?:[A-Z]{2}|001)$`)

// The destination country of a text to phoneNumber, as an ISO 3166-1 region
// code: the country of the number's calling code, and for a calling code that
// several countries share (+1 has the US, Canada and two dozen others) the
// country the number is numbered in. A number must be in international form, as
// a normal phone user auth is (model.NormalUserAuth): one without its calling
// code names no country.
func smsDestinationCountry(phoneNumber string) string {
	number, err := phonenumbers.Parse(phoneNumber, phonenumbers.UNKNOWN_REGION)
	if err != nil {
		return smsUnknownCountry
	}
	country := phonenumbers.GetRegionCodeForNumber(number)
	if !smsCountryPattern.MatchString(country) {
		return smsUnknownCountry
	}
	return country
}

// What a client is told about a text the caps refused, unless it shows its own
// words for a rate limit.
const smsSendCapRefusedMessage = "Too many texts are being sent right now. Please try again later, or use an email address instead."

// A text the caps refused. It is a rate limit, like the code budgets' refusals:
// Error carries the "429 " prefix router.RaiseHttpError turns into the status,
// and RetryAfterSeconds the wait it turns into a Retry-After header.
type smsSendCapRefusal struct {
	// smsSendCapGlobal or smsSendCapCountry
	capKind string
	// the cap that refused, in texts per window
	capLimit int
	// the destination country (smsDestinationCountry)
	country string
	// until the cap that refused has room again
	retryAfter time.Duration
}

func (self *smsSendCapRefusal) Error() string {
	return "429 " + smsSendCapRefusedMessage
}

// Whole seconds, rounded up so that trying again after them is never early.
func (self *smsSendCapRefusal) RetryAfterSeconds() int {
	return int(max((self.retryAfter+time.Second-1)/time.Second, 1))
}

// The verify send result error of a code whose text the caps refused: a rate
// limit with its wait, as the code budgets answer, rather than a failed send.
// Nil for any other send error.
func smsSendCapVerifySendError(sendErr error) *AuthVerifySendError {
	var refusal *smsSendCapRefusal
	if !errors.As(sendErr, &refusal) {
		return nil
	}
	return &AuthVerifySendError{
		Code:              model.AuthVerifySendErrorCodeRateLimited,
		Message:           smsSendCapRefusedMessage,
		RetryAfterSeconds: refusal.RetryAfterSeconds(),
	}
}

// The caps a text passes before its Pinpoint call.
type smsSendCaps interface {
	// Admits a text to phoneNumber and spends it from the caps. A refusal is a
	// *smsSendCapRefusal; any other error means the caps could not be read.
	admitSms(ctx context.Context, phoneNumber string) error
}

// The production caps, kept in Redis.
type redisSmsSendCaps struct {
	// nil reads the server clock; tests move time without waiting
	now func() time.Time
}

// The caps of the production sender, AWSMessageSender's zero value.
var defaultSmsSendCaps smsSendCaps = &redisSmsSendCaps{}

const smsSendCapKeyPrefix = "account_sms_send_cap."

// Every key of the caps shares this hash tag, so one script can read and spend
// the global cap and a country's cap together.
const smsSendCapHashTag = "account_sms_send_cap"

func smsSendCapGlobalKey() string {
	return fmt.Sprintf("%s{%s}.global", smsSendCapKeyPrefix, smsSendCapHashTag)
}

func smsSendCapCountryKey(country string) string {
	return fmt.Sprintf("%s{%s}.country.%s", smsSendCapKeyPrefix, smsSendCapHashTag, country)
}

// Admits one text by its global cap (KEYS[1]) and its country cap (KEYS[2]),
// or refuses it. Each cap is a sorted set of the texts it admitted, scored by
// when they leave the window. A refused text is added to neither. Returns
// {0, 0} for an admitted text, or the cap with no room (1 global, 2 country)
// and the milliseconds until it has room: when neither has room, the one with
// the longer wait, the country on a tie. ARGV: now, when this text leaves the
// window, the text's member, the global cap, the country cap, the window (all
// times in milliseconds).
var smsSendCapScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local waits = {0, 0}
for i = 1, 2 do
    redis.call('ZREMRANGEBYSCORE', KEYS[i], '-inf', now)
    if tonumber(ARGV[3 + i]) <= redis.call('ZCARD', KEYS[i]) then
        local oldest = redis.call('ZRANGE', KEYS[i], 0, 0, 'WITHSCORES')
        waits[i] = math.max(tonumber(oldest[2]) - now, 1)
    end
end
if waits[2] < waits[1] then
    return {1, waits[1]}
end
if 0 < waits[2] then
    return {2, waits[2]}
end
for i = 1, 2 do
    redis.call('ZADD', KEYS[i], ARGV[2], ARGV[3])
    redis.call('PEXPIRE', KEYS[i], ARGV[6])
end
return {0, 0}
`)

func (self *redisSmsSendCaps) admitSms(ctx context.Context, phoneNumber string) error {
	now := server.NowUtc()
	if self.now != nil {
		now = self.now()
	}
	country := smsDestinationCountry(phoneNumber)
	countryLimit := accountSmsSendCountryCap(country)
	var values []int64
	var err error
	server.Redis(ctx, func(r server.RedisClient) {
		// the error is returned rather than raised: a raised connection error
		// would run the script again, and a run whose answer was lost would
		// count the text twice
		values, err = smsSendCapScript.Run(
			ctx,
			r,
			[]string{smsSendCapGlobalKey(), smsSendCapCountryKey(country)},
			now.UnixMilli(),
			now.Add(AccountSmsSendCapWindow).UnixMilli(),
			server.NewId().String(),
			AccountSmsSendGlobalCap,
			countryLimit,
			AccountSmsSendCapWindow.Milliseconds(),
		).Int64Slice()
	})
	if err != nil {
		return fmt.Errorf("the sms send caps could not be read: %w", err)
	}
	if len(values) != 2 {
		return fmt.Errorf("the sms send caps answered %v", values)
	}
	retryAfter := time.Duration(values[1]) * time.Millisecond
	switch values[0] {
	case 0:
		return nil
	case 1:
		return &smsSendCapRefusal{
			capKind:    smsSendCapGlobal,
			capLimit:   AccountSmsSendGlobalCap,
			country:    country,
			retryAfter: retryAfter,
		}
	case 2:
		return &smsSendCapRefusal{
			capKind:    smsSendCapCountry,
			capLimit:   countryLimit,
			country:    country,
			retryAfter: retryAfter,
		}
	default:
		return fmt.Errorf("the sms send caps answered %v", values)
	}
}

// Counts the account texts the caps refused before their Pinpoint call, by the
// cap that refused them.
var smsSendCapRefusalCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "account_message",
		Name:      "sms_cap_refusals_total",
		Help:      "Account texts the hourly send caps refused before their Pinpoint SMS call, by cap (global, country)",
	},
	[]string{"cap"},
)

// Registers the refusal metric with both caps at zero, so a first refusal is an
// increase.
func init() {
	prometheus.MustRegister(smsSendCapRefusalCounter)
	smsSendCapRefusalCounter.WithLabelValues(smsSendCapGlobal)
	smsSendCapRefusalCounter.WithLabelValues(smsSendCapCountry)
}

// The default-level report of caps that could not be read. Every text asks the
// caps, so a line per failure would let an outage fill the logs.
var smsSendCapFailures = newBoundedReport(accountMessageSendReportInterval)

// The caps this sender's texts pass: its own, or the production caps.
func (self *AWSMessageSender) sendCaps() smsSendCaps {
	if self.smsCaps != nil {
		return self.smsCaps
	}
	return defaultSmsSendCaps
}

// Admits a text of the template to phoneNumber by the caps, before its Pinpoint
// call. A refusal is counted by its cap and logged, one line each, with the
// destination country and never the number; caps that cannot be read are
// reported at most once an interval. Either way the text is not sent and the
// error is returned.
func (self *AWSMessageSender) admitSms(ctx context.Context, phoneNumber string, templateName string) error {
	err := self.sendCaps().admitSms(ctx, phoneNumber)
	if err == nil {
		return nil
	}
	var refusal *smsSendCapRefusal
	if errors.As(err, &refusal) {
		smsSendCapRefusalCounter.WithLabelValues(refusal.capKind).Inc()
		glog.Infof(
			"[aws]account sms %s to %s refused by the %s send cap (%d texts in %d minutes); room again in %s\n",
			templateName,
			refusal.country,
			refusal.capKind,
			refusal.capLimit,
			int(AccountSmsSendCapWindow/time.Minute),
			refusal.retryAfter.Round(time.Second),
		)
		return err
	}
	if suppressedCount, ok := smsSendCapFailures.Allow(time.Now()); ok {
		glog.Infof(
			"[aws]account sms %s not sent: %s; %d more since the last report\n",
			templateName,
			err,
			suppressedCount,
		)
	}
	return err
}
