package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The seed-phrase sign-up budgets of a source network and of an autonomous
// system (owner, 2026-11-13, "accept a+b recommended fixes", fix a: limit
// seed-phrase sign-ups per source). A seed-phrase account needs no email,
// phone, wallet or SSO identity, so the caller's address is all a sign-up says
// about who makes it. Three budgets count a seed-phrase sign-up
// (CheckNetworkCreateRateLimit):
//   - the address budget: NetworkCreateDailyLimit per address bucket (ipv4 /29,
//     ipv6 /56, server.ClientIpHashForAddr) per rolling 24 h, in PostgreSQL;
//   - the autonomous system budget here: the autonomous system that announces
//     the address (GeoLite2 ASN, server.GetIpAsn) may make AsnShortLimit
//     sign-ups per AsnShortWindow and AsnLongLimit per AsnLongWindow, in Redis;
//   - the source network budget here: the ipv4 /24 or ipv6 /48 that holds the
//     address may make ShortLimit sign-ups per ShortWindow and LongLimit per
//     LongWindow, in Redis.
//
// A /24 holds 32 address buckets and a /48 holds 256, so one caller holding a
// /24 could make 160 accounts a day and one holding a /48 1,280 within the
// address budget alone. The source network budget closes that rotation.
//
// A caller holding many /24s of one autonomous system stays within every
// source network budget. On 2026-11-14 a sign-up farm did that: from 11:00Z
// one autonomous system made 940 to 1,530 seed-phrase sign-ups an hour over
// many /24s while the /24 and /48 budgets refused at most 6 an hour. The
// autonomous system budget closes that spread. It is shared by every customer
// of an internet provider, so it sits far above what real users of one
// provider make (see AsnShortLimit), and while a farm holds an autonomous
// system's budget spent, that provider's real users are refused with it.
//
// What none of these stop: a pool spread over many autonomous systems, like the
// 2026-11-13 wave (11,905 seed-phrase accounts from 18:00 to 22:30Z, each with
// five hosted proxies, from 10,213 address buckets in 143 countries, almost
// every bucket used once an hour, the shape of a residential proxy pool).
// urnetwork_seedphrase_signup_source_signups records, for every admitted
// sign-up, how many sign-ups its address bucket, its source network and its
// autonomous system had made in their windows, so each wave shows what a
// tighter budget at each scope would refuse.
//
// Counting: the windows are fixed, aligned to the unix epoch (whole UTC hours
// and days). Only an admitted sign-up is counted: a refused attempt changes no
// counter, so a caller retrying a refusal does not extend it. Both counters of a
// source share one Redis hash slot, and one script checks and counts them
// together, so concurrent sign-ups cannot overrun a budget. The autonomous
// system is checked first, and a sign-up its source network then refuses is
// taken back out of the autonomous system's counters. A key names the keyed
// (peppered) hash of the masked network address, never the address; an
// autonomous system's key names its public number.
//
// A Redis failure admits the sign-up: each budget fails open, as the hosted
// proxy create rate does, and the budgets that could be read still apply.
type NetworkCreateSourceLimitSettings struct {
	// The prefix length, in bits, of the ipv4 network that holds a caller's
	// address. 24: the smallest block routed between networks, 32 address
	// buckets.
	Ipv4PrefixLength int
	// The prefix length, in bits, of the ipv6 network that holds a caller's
	// address. 48: the usual allocation to one site, 256 address buckets.
	Ipv6PrefixLength int
	// The short budget's fixed window.
	ShortWindow time.Duration
	// Admitted sign-ups a source network may make in one short window.
	// Non-positive disables the short budget. 20, four whole address budgets
	// in an hour: on 2026-11-12, 91% of the address buckets that signed up
	// made one sign-up all day, and none made more than 5 in an hour. How
	// real sign-ups spread over /24s and /48s was not measurable (only
	// address bucket hashes are kept), so the network budgets are multiples
	// of the address budget with room for a shared egress, until the
	// histogram shows the spread.
	ShortLimit int
	// The long budget's fixed window.
	LongWindow time.Duration
	// Admitted sign-ups a source network may make in one long window.
	// Non-positive disables the long budget. 50, ten whole address budgets a
	// day: on 2026-11-12, 99.4% of the address buckets that signed up made at
	// most 4.
	LongLimit int
	// The autonomous system short budget's fixed window. Under a millisecond,
	// or a long window under a millisecond, makes the autonomous system budget
	// unreadable, and it admits (fail open).
	AsnShortWindow time.Duration
	// Admitted sign-ups an autonomous system may make in one short window.
	// Non-positive refuses nothing; the sign-ups are still counted. 200: on
	// 2026-11-12 the busiest country made at most 88 seed-phrase sign-ups in an
	// hour, and all countries together at most 229 (no-email onboarding
	// starts; 115 to 280 in the busiest hour of each ordinary day from
	// 2026-10-24 to 11-12). A provider's real users are a part of one
	// country's, so 200 is more than twice the most one provider can have
	// made. From 08:00 to 16:00Z on 2026-11-14 it would have refused 5,445 of
	// the 10,991 admitted sign-ups, from one to three autonomous systems an
	// hour.
	AsnShortLimit int
	// The autonomous system long budget's fixed window.
	AsnLongWindow time.Duration
	// Admitted sign-ups an autonomous system may make in one long window.
	// Non-positive refuses nothing; the sign-ups are still counted. 1,500,
	// seven and a half hours at the short budget: all countries together made
	// 1,390 to 3,870 seed-phrase sign-ups a day from 2026-10-24 to 11-12 (6,800
	// on 10-28), and one provider a part of one country's. A farm holding one
	// autonomous system for a whole day gets 1,500 accounts from it, not the
	// 4,800 the short budget alone admits.
	AsnLongLimit int
}

// The production budgets. Each call returns a new instance.
func DefaultNetworkCreateSourceLimitSettings() *NetworkCreateSourceLimitSettings {
	return &NetworkCreateSourceLimitSettings{
		Ipv4PrefixLength: 24,
		Ipv6PrefixLength: 48,
		ShortWindow:      time.Hour,
		ShortLimit:       20,
		LongWindow:       24 * time.Hour,
		LongLimit:        50,
		AsnShortWindow:   time.Hour,
		AsnShortLimit:    200,
		AsnLongWindow:    24 * time.Hour,
		AsnLongLimit:     1500,
	}
}

// What a source's budgets decided for one sign-up.
type networkCreateSourceDecision struct {
	// the budget that refused the sign-up, "" when it was admitted
	limited networkCreateSourceBudget
	// seconds until the refusing budget's window ends; 0 when admitted
	retryAfterSeconds int
	// the source's admitted sign-ups in each window, this one included when it
	// was admitted
	shortCount int64
	longCount  int64
}

type networkCreateSourceBudget string

const (
	networkCreateSourceBudgetShort networkCreateSourceBudget = "short"
	networkCreateSourceBudgetLong  networkCreateSourceBudget = "long"
)

// Checks both budgets of a source and counts the sign-up in both only when both
// have room. Returns {status, short count, long count}: status 0 admitted, 1
// refused by the short budget, 2 by the long one. A limit of 0 or less never
// refuses.
//
// KEYS[1] the short window's counter, KEYS[2] the long window's (one hash slot)
// ARGV[1] short limit, ARGV[2] long limit, ARGV[3] short ttl ms, ARGV[4] long ttl ms
var networkCreateSourceLimitScript = redis.NewScript(`
local short_count = tonumber(redis.call('GET', KEYS[1]) or '0')
local long_count = tonumber(redis.call('GET', KEYS[2]) or '0')
local short_limit = tonumber(ARGV[1])
local long_limit = tonumber(ARGV[2])
if 0 < short_limit and short_limit <= short_count then
    return {1, short_count, long_count}
end
if 0 < long_limit and long_limit <= long_count then
    return {2, short_count, long_count}
end
short_count = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
long_count = redis.call('INCR', KEYS[2])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return {0, short_count, long_count}
`)

// Takes one admitted sign-up back out of both counters of a source, never below
// zero, keeping their expiry. Returns 1.
//
// KEYS[1] the short window's counter, KEYS[2] the long window's (one hash slot)
var networkCreateSourceUncountScript = redis.NewScript(`
for i = 1, 2 do
    if 0 < tonumber(redis.call('GET', KEYS[i]) or '0') then
        redis.call('DECR', KEYS[i])
    end
end
return 1
`)

// The fixed window of length `window` that `now` falls in: its index since the
// unix epoch and its end.
func networkCreateSourceWindow(now time.Time, window time.Duration) (index int64, end time.Time) {
	windowMillis := window.Milliseconds()
	index = now.UnixMilli() / windowMillis
	end = time.UnixMilli((index + 1) * windowMillis)
	return
}

// Whole seconds from now until end, rounded up so that a retry after them is
// never early, and at least 1.
func networkCreateSourceRetryAfterSeconds(now time.Time, end time.Time) int {
	seconds := int((end.Sub(now) + time.Second - 1) / time.Second)
	return max(1, seconds)
}

// The hash tag of the source network that holds addr: the keyed hash of the
// masked network address, so a key never carries the address.
func networkCreateSourceNetworkTag(addr netip.Addr, settings *NetworkCreateSourceLimitSettings) string {
	networkHash := server.ClientIpHashForAddrPrefix(addr, settings.Ipv4PrefixLength, settings.Ipv6PrefixLength)
	return "ncsrc_" + hex.EncodeToString(networkHash[:])
}

// The counters of a source with hash tag `tag` for the windows `now` falls in.
// A key names its window's length and index, so a changed window length never
// reads another length's counter.
func networkCreateSourceKey(tag string, now time.Time, window time.Duration) (key string, end time.Time) {
	index, end := networkCreateSourceWindow(now, window)
	key = fmt.Sprintf("{%s}w%d_%d", tag, window.Milliseconds(), index)
	return
}

// The counters of the source network that holds addr for the windows `now`
// falls in, and the windows' ends.
func networkCreateSourceNetworkKeys(
	addr netip.Addr,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (shortKey string, shortEnd time.Time, longKey string, longEnd time.Time) {
	tag := networkCreateSourceNetworkTag(addr, settings)
	shortKey, shortEnd = networkCreateSourceKey(tag, now, settings.ShortWindow)
	longKey, longEnd = networkCreateSourceKey(tag, now, settings.LongWindow)
	return
}

// The counters of autonomous system `asn` for the windows `now` falls in, and
// the windows' ends. The hourly key is the one the histogram counted in before
// the budget, so the count carries over a deploy.
func networkCreateSourceAsnKeys(
	asn uint32,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (shortKey string, shortEnd time.Time, longKey string, longEnd time.Time) {
	tag := fmt.Sprintf("ncsrc_asn_%d", asn)
	shortKey, shortEnd = networkCreateSourceKey(tag, now, settings.AsnShortWindow)
	longKey, longEnd = networkCreateSourceKey(tag, now, settings.AsnLongWindow)
	return
}

func (self *NetworkCreateSourceLimitSettings) valid() bool {
	return self != nil &&
		0 < self.Ipv4PrefixLength && self.Ipv4PrefixLength <= 32 &&
		0 < self.Ipv6PrefixLength && self.Ipv6PrefixLength <= 128 &&
		time.Millisecond <= self.ShortWindow && time.Millisecond <= self.LongWindow
}

func (self *NetworkCreateSourceLimitSettings) asnValid() bool {
	return self != nil && time.Millisecond <= self.AsnShortWindow && time.Millisecond <= self.AsnLongWindow
}

// Checks one sign-up against the two budgets of the source whose counters are
// shortKey and longKey, and counts it when both admit it.
func checkNetworkCreateSourceBudgets(
	ctx context.Context,
	now time.Time,
	shortKey string,
	shortEnd time.Time,
	shortLimit int,
	shortWindow time.Duration,
	longKey string,
	longEnd time.Time,
	longLimit int,
	longWindow time.Duration,
) (decision networkCreateSourceDecision, returnErr error) {
	var values []int64
	server.Redis(ctx, func(r server.RedisClient) {
		values, returnErr = networkCreateSourceLimitScript.Run(
			ctx,
			r,
			[]string{shortKey, longKey},
			shortLimit,
			longLimit,
			shortWindow.Milliseconds(),
			longWindow.Milliseconds(),
		).Int64Slice()
	})
	if returnErr != nil {
		return
	}
	if len(values) != 3 {
		returnErr = fmt.Errorf("seed-phrase source budget returned %d values", len(values))
		return
	}
	decision.shortCount = values[1]
	decision.longCount = values[2]
	switch values[0] {
	case 0:
	case 1:
		decision.limited = networkCreateSourceBudgetShort
		decision.retryAfterSeconds = networkCreateSourceRetryAfterSeconds(now, shortEnd)
	case 2:
		decision.limited = networkCreateSourceBudgetLong
		decision.retryAfterSeconds = networkCreateSourceRetryAfterSeconds(now, longEnd)
	default:
		returnErr = fmt.Errorf("seed-phrase source budget returned status %d", values[0])
	}
	return
}

// Checks one seed-phrase sign-up from addr against its source network's budgets
// at `now`, and counts it when both admit it. An error means the budget could
// not be read; the caller admits the sign-up (fail open).
func checkNetworkCreateSourceLimit(
	ctx context.Context,
	addr netip.Addr,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (decision networkCreateSourceDecision, returnErr error) {
	if !settings.valid() {
		returnErr = fmt.Errorf("invalid seed-phrase source network settings")
		return
	}
	if !addr.IsValid() {
		returnErr = fmt.Errorf("no source address")
		return
	}
	addr = addr.Unmap()
	shortKey, shortEnd, longKey, longEnd := networkCreateSourceNetworkKeys(addr, now, settings)
	return checkNetworkCreateSourceBudgets(
		ctx,
		now,
		shortKey, shortEnd, settings.ShortLimit, settings.ShortWindow,
		longKey, longEnd, settings.LongLimit, settings.LongWindow,
	)
}

// Checks one seed-phrase sign-up from autonomous system `asn` against its
// budgets at `now`, and counts it when both admit it. An error means the budget
// could not be read; the caller goes on without it (fail open).
func checkNetworkCreateSourceAsnLimit(
	ctx context.Context,
	asn uint32,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (decision networkCreateSourceDecision, returnErr error) {
	if !settings.asnValid() {
		returnErr = fmt.Errorf("invalid seed-phrase autonomous system settings")
		return
	}
	shortKey, shortEnd, longKey, longEnd := networkCreateSourceAsnKeys(asn, now, settings)
	return checkNetworkCreateSourceBudgets(
		ctx,
		now,
		shortKey, shortEnd, settings.AsnShortLimit, settings.AsnShortWindow,
		longKey, longEnd, settings.AsnLongLimit, settings.AsnLongWindow,
	)
}

// Takes a sign-up that a later budget refused back out of the counters of the
// autonomous system that admitted it at `now`. An error leaves it counted, so
// the system's budget refuses one sign-up early, never late.
func uncountNetworkCreateSourceAsn(
	ctx context.Context,
	asn uint32,
	now time.Time,
	settings *NetworkCreateSourceLimitSettings,
) (returnErr error) {
	if !settings.asnValid() {
		return fmt.Errorf("invalid seed-phrase autonomous system settings")
	}
	shortKey, _, longKey, _ := networkCreateSourceAsnKeys(asn, now, settings)
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = networkCreateSourceUncountScript.Run(ctx, r, []string{shortKey, longKey}).Err()
	})
	return
}

// The refusal of a sign-up over its source network's budget. Like the address
// budget's refusal, it says the budget is shared, so a person refused for what
// others on their network did is not told it was their own doing; the scope is
// the address range, not the one address.
func maxNetworkCreateSourceAttemptsError(retryAfterSeconds int) error {
	return &rateLimitError{
		message: "429 Too many accounts have been created recently from your network's " +
			"address range. This limit is shared with everyone connecting from the same " +
			"range, so this may not be your own activity. Please try again later or " +
			"from a different connection.",
		retryAfterSeconds: retryAfterSeconds,
	}
}

// The refusal of a sign-up over its autonomous system's budget. The scope is
// the whole internet provider, and the person refused is almost never the one
// who spent it, so it says so.
func maxNetworkCreateSourceAsnAttemptsError(retryAfterSeconds int) error {
	return &rateLimitError{
		message: "429 Too many accounts have been created recently from your internet " +
			"provider's network. This limit is shared with everyone on the same provider, " +
			"so this is almost certainly not your own activity. Please try again later or " +
			"from a different connection.",
		retryAfterSeconds: retryAfterSeconds,
	}
}

// The outcome of one seed-phrase sign-up's admission, a closed set.
type seedphraseSignupOutcome string

const (
	// admitted by the address budget and every source budget that applies
	seedphraseSignupAllowed seedphraseSignupOutcome = "allowed"
	// admitted by the address budget; the source budgets cannot apply to a
	// session that kept only its address hash
	seedphraseSignupAddressOnly seedphraseSignupOutcome = "address_only"
	// admitted by the address budget; the source network or autonomous system
	// budget could not be read, so it admitted the sign-up (fail open), and the
	// budgets that could be read admitted it too
	seedphraseSignupUnavailable seedphraseSignupOutcome = "unavailable"
	// no source budget applies: an excluded infrastructure address, a tests.yml
	// acceptance source, or no client address at all
	seedphraseSignupUnscoped seedphraseSignupOutcome = "unscoped"
	// refused by the address budget
	seedphraseSignupLimitedAddress seedphraseSignupOutcome = "limited_address"
	// refused by the source network's short budget
	seedphraseSignupLimitedNetworkShort seedphraseSignupOutcome = "limited_network_short"
	// refused by the source network's long budget
	seedphraseSignupLimitedNetworkLong seedphraseSignupOutcome = "limited_network_long"
	// refused by the autonomous system's short budget
	seedphraseSignupLimitedAsnShort seedphraseSignupOutcome = "limited_asn_short"
	// refused by the autonomous system's long budget
	seedphraseSignupLimitedAsnLong seedphraseSignupOutcome = "limited_asn_long"
)

var seedphraseSignupOutcomes = []seedphraseSignupOutcome{
	seedphraseSignupAllowed,
	seedphraseSignupAddressOnly,
	seedphraseSignupUnavailable,
	seedphraseSignupUnscoped,
	seedphraseSignupLimitedAddress,
	seedphraseSignupLimitedNetworkShort,
	seedphraseSignupLimitedNetworkLong,
	seedphraseSignupLimitedAsnShort,
	seedphraseSignupLimitedAsnLong,
}

// The address family of a caller, a closed set: "4", "6", or "unknown" for a
// session without a usable address.
func seedphraseSignupFamily(addr netip.Addr) string {
	switch server.IpVersionForAddr(addr) {
	case 4:
		return "4"
	case 6:
		return "6"
	default:
		return "unknown"
	}
}

var seedphraseSignupFamilies = []string{"4", "6", "unknown"}

// The scopes the histogram counts a sign-up's source at, a closed set.
const (
	// the address bucket (ipv4 /29, ipv6 /56) over the rolling 24 h
	seedphraseSignupScopeAddress = "address"
	// the source network (ipv4 /24, ipv6 /48) over the short window
	seedphraseSignupScopeNetworkShort = "network_short"
	// the source network over the long window
	seedphraseSignupScopeNetworkLong = "network_long"
	// the autonomous system over the short window (hourly), the scope's name
	// from when it was counted and not limited
	seedphraseSignupScopeAsn = "asn"
	// the autonomous system over the long window
	seedphraseSignupScopeAsnLong = "asn_long"
)

var seedphraseSignupAdmissionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "seedphrase_signup",
		Name:      "admissions_total",
		Help:      "Seed-phrase sign-up admissions by outcome (allowed, address_only, unavailable, unscoped, or the budget that refused: limited_address, limited_network_short, limited_network_long, limited_asn_short, limited_asn_long) and the caller's address family",
	},
	[]string{"outcome", "family"},
)

var seedphraseSignupSourceSignupsHistogram = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "seedphrase_signup",
		Name:      "source_signups",
		Help:      "For each admitted seed-phrase sign-up, the admitted sign-ups its source made in the scope's window, this one included: address (ipv4 /29, ipv6 /56, rolling 24 h), network_short and network_long (ipv4 /24, ipv6 /48, the source network budget's windows), asn and asn_long (autonomous system, the autonomous system budget's hourly and daily windows)",
		Buckets:   []float64{1, 2, 3, 5, 10, 20, 50, 100, 200, 500, 1000},
	},
	[]string{"scope", "family"},
)

func init() {
	// materialize the closed partition, so a fleet that refuses nothing is
	// told apart from one that does not report
	for _, family := range seedphraseSignupFamilies {
		for _, outcome := range seedphraseSignupOutcomes {
			seedphraseSignupAdmissionsCounter.WithLabelValues(string(outcome), family)
		}
	}
	prometheus.MustRegister(seedphraseSignupAdmissionsCounter, seedphraseSignupSourceSignupsHistogram)
}

func noteSeedphraseSignupAdmission(outcome seedphraseSignupOutcome, family string) {
	seedphraseSignupAdmissionsCounter.WithLabelValues(string(outcome), family).Inc()
}

func observeSeedphraseSignupSource(scope string, family string, count int64) {
	seedphraseSignupSourceSignupsHistogram.WithLabelValues(scope, family).Observe(float64(count))
}
