// SPDX-License-Identifier: MPL-2.0

package onboarding

// The sign-up cohorts the experiment readouts are stratified by (owner,
// 2026-11-15): likely bot and likely human, so a decision rests on the human
// cohort while sign-up farms make most of the email sign-ups (from 2026-11-09
// about 1,400 to 5,300 a day, against about 100 before). The label decides
// nothing about mail: every network in either cohort is mailed the same.
//
// Two kinds of signal, both from the sign-up itself:
//   - the request (SignupCohort, at enrollment, recorded as the event
//     signup.likely_bot or signup.likely_human): a farm's user agent, or a
//     browser's user agent sent without an Accept-Language header, which every
//     browser sends and the farms' scripts did not (of the 2026-11-09..15
//     email sign-ups recorded as web with no language, 5,367, none redeemed
//     an offer or saw one in the app; of the 1,738 web sign-ups with a
//     language, 73 redeemed);
//   - a burst (SignatureBursts, at rollup time): at least BurstThreshold email
//     sign-ups in one UTC hour with the same platform, language and country.
//     Before the wave, 4,175 of 4,219 such hours held at most 4 sign-ups (the
//     99th percentile was 5); the wave's held up to 387.
// A burst outranks a request's human label. A sign-up recorded before the
// request labels existed is likely bot when it bursts or when a browser
// signed up without a language (the web platform with no locale), and
// otherwise unknown: it was never classified.

import (
	"strings"
	"time"
)

// The cohorts.
const (
	CohortHuman   = "human"
	CohortBot     = "bot"
	CohortUnknown = "unknown"
)

// Why a sign-up is likely bot.
const (
	CohortReasonFarmUserAgent    = "farm_user_agent"
	CohortReasonNoAcceptLanguage = "no_accept_language"
)

// CohortExperimentId is the results experiment id of one cohort's rows of a
// registry experiment: "<experiment>@<cohort>". The rollup writes them beside
// the experiment's own rows, under the same dimensions, so the results
// endpoint serves a cohort as it serves an experiment, and the guardrail
// check, which reads the registry ids, never mixes them in.
func CohortExperimentId(experimentId string, cohort string) string {
	return experimentId + "@" + cohort
}

// SignupCohortSettings are the classifier's tunables.
type SignupCohortSettings struct {
	// lower-case marks a farm's user agent contains anywhere
	FarmUserAgentMarks []string
	// lower-case prefixes a farm's user agent starts with
	FarmUserAgentPrefixes []string
	// email sign-ups in one UTC hour with the same platform, language and
	// country from which all of them are likely bot
	BurstThreshold int
}

// DefaultSignupCohortSettings: the farm agents of the 2026-11-14 wave
// (`urnet/1.1 (+…reformboss…/ur-network-farm)`, `synx/1.0`, `urnet-safe/1.0`)
// and a burst of 10, twice the busiest normal hour's 99th percentile.
func DefaultSignupCohortSettings() *SignupCohortSettings {
	return &SignupCohortSettings{
		FarmUserAgentMarks:    []string{"ur-network-farm", "reformboss"},
		FarmUserAgentPrefixes: []string{"urnet/", "urnet-safe/", "synx/"},
		BurstThreshold:        10,
	}
}

// SignupCohort classifies a sign-up by its request: likely bot for a farm's
// user agent, or for a browser's (every browser's starts "Mozilla/") sent with
// no Accept-Language; otherwise likely human. reason is "" for human. The apps
// send their own user agents and no Accept-Language on iOS, so the language
// rule never reads an app's sign-up.
func SignupCohort(userAgent string, acceptLanguage string, settings *SignupCohortSettings) (cohort string, reason string) {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	for _, mark := range settings.FarmUserAgentMarks {
		if mark != "" && strings.Contains(ua, mark) {
			return CohortBot, CohortReasonFarmUserAgent
		}
	}
	for _, prefix := range settings.FarmUserAgentPrefixes {
		if prefix != "" && strings.HasPrefix(ua, prefix) {
			return CohortBot, CohortReasonFarmUserAgent
		}
	}
	if strings.HasPrefix(ua, "mozilla/") && strings.TrimSpace(acceptLanguage) == "" {
		return CohortBot, CohortReasonNoAcceptLanguage
	}
	return CohortHuman, ""
}

// CohortSignup is what the burst rule and the cohort read of one sign-up.
type CohortSignup struct {
	Email     bool
	CreatedAt time.Time
	Platform  string
	Locale    string
	Country   string
	// the request's label (SignupCohort), "" when none was recorded
	RequestCohort string
}

// signatureHour is a burst's key: the sign-up's UTC hour, platform, language
// and country.
type signatureHour struct {
	hour     time.Time
	platform string
	locale   string
	country  string
}

func signatureOf(s CohortSignup) signatureHour {
	return signatureHour{
		hour:     s.CreatedAt.UTC().Truncate(time.Hour),
		platform: s.Platform,
		locale:   s.Locale,
		country:  s.Country,
	}
}

// SignatureBursts counts the email sign-ups per signature hour and returns, by
// index into signups, those in a signature hour with at least BurstThreshold:
// every one of them, the first included. Sign-ups without an email are not
// counted (the experiments' population is the email sign-ups).
func SignatureBursts(signups []CohortSignup, settings *SignupCohortSettings) map[int]bool {
	counts := map[signatureHour]int{}
	for _, s := range signups {
		if s.Email {
			counts[signatureOf(s)] += 1
		}
	}
	bursts := map[int]bool{}
	if settings.BurstThreshold <= 0 {
		return bursts
	}
	for i, s := range signups {
		if s.Email && settings.BurstThreshold <= counts[signatureOf(s)] {
			bursts[i] = true
		}
	}
	return bursts
}

// Cohort is a sign-up's cohort: likely bot when it bursts or its request was;
// likely human when its request was and it doesn't burst; for a sign-up from
// before the request labels, likely bot when a browser signed up without a
// language (the web platform with no locale), else unknown.
func Cohort(s CohortSignup, burst bool) string {
	switch {
	case burst || s.RequestCohort == CohortBot:
		return CohortBot
	case s.RequestCohort == CohortHuman:
		return CohortHuman
	case s.Platform == PlatformWeb && strings.TrimSpace(s.Locale) == "":
		return CohortBot
	}
	return CohortUnknown
}
