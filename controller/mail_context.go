// SPDX-License-Identifier: MPL-2.0

package controller

// The mail context of a campaign email's reader, for both programs
// (onboarding and re-engagement): when in the reader's day a send goes and
// which language it is written in (the campaign review of 2026-11-13, item
// B). Every app has sent the device's zone and locale on auth-client since
// its 2026-09-09 build, but only a still-running onboarding row kept them; a
// network that finished onboarding, was a holdout, signed up through the
// device path or before 2026-09-15 was mailed at 10:00 UTC (02:00-07:00 in
// the Americas) and in English.
//
// Writing: every auth-client, sign-up and sign-in records what it reports in
// the network's durable mail context (model.NetworkMailContext), whatever the
// onboarding state: the device's zone, its ordered language list (the
// client's list or locale, then a browser's whole Accept-Language), and the
// app build it names. Only the latest values are kept.
//
// Reading, at send time (resolveMailReader), each step only when the one
// before gave nothing usable:
//
//   - the time zone: the newest device zone; the zone the onboarding row
//     recorded at sign-up (the sign-up IP's, by GeoLite2), kept only when the
//     sign-up IP's country agrees with the App Store, Play or Stripe country,
//     since a VPN user's IP is often an exit; the country's zone
//     (onboarding.CountrySendZone), the store or billing country before the
//     IP's, which keeps every populated zone of the country inside the send
//     window; then the time of day the reader was last active (the last
//     connection or the sign-up), in a frame that places a send at that time
//     of day.
//   - the language: the reader's own list, the onboarding row's locale and
//     the newest client event's, each matched in order against the email's
//     template locales; then the country's language under a strict rule
//     (onboarding.CountryLanguage), the store or billing country before the
//     IP's; then English.
//
// Nothing here makes an IP lookup: the onboarding row's zone and country were
// recorded once at enrollment.

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
	"github.com/urnetwork/server/session"
)

// usableReportedTimeZone is a zone a client reported when mail can be placed
// in it: a name the time package loads, of at most 64 bytes, that is not UTC.
// A device that reports UTC is a server, an emulator or a privacy-hardened
// browser (Tor Browser, Firefox's resistFingerprinting) far more often than a
// reader, whose own zone has a name (Atlantic/Reykjavik, Africa/Abidjan);
// keeping it would stop the IP zone and the country from placing the
// reader's mail. "" otherwise.
func usableReportedTimeZone(timeZone string) string {
	timeZone = strings.TrimSpace(timeZone)
	if timeZone == "" || 64 < len(timeZone) {
		return ""
	}
	switch strings.ToUpper(timeZone) {
	case "UTC", "ETC/UTC", "ETC/UCT", "UCT", "GMT", "ETC/GMT", "ETC/GMT+0", "ETC/GMT-0", "ETC/GMT0", "GMT0", "GMT+0", "GMT-0",
		"ETC/UNIVERSAL", "UNIVERSAL", "ETC/ZULU", "ZULU", "ETC/GREENWICH", "GREENWICH", "FACTORY", "LOCAL":
		return ""
	}
	loc, err := time.LoadLocation(timeZone)
	if err != nil || loc == time.UTC {
		return ""
	}
	return timeZone
}

// mailReportLanguages is the ordered language list a request reports: the
// client's own list, else its single locale, then the whole Accept-Language
// list of a browser (the apps send none), each tag once.
func mailReportLanguages(clientSession *session.ClientSession, locales []string, locale string, settings *onboarding.MailContextSettings) []string {
	reported := []string{}
	reported = append(reported, locales...)
	if locale != "" {
		reported = append(reported, locale)
	}
	if clientSession != nil {
		reported = append(reported, onboarding.AcceptLanguages(clientSession.Header["Accept-Language"], settings.MaxLanguages)...)
	}
	return onboarding.NormalizeLanguages(reported, settings.MaxLanguages)
}

// mailReportClient is the app build a request names: the platform and
// distribution its X-UR-ClientInfo declares with the app version, else the
// platform its device spec names; nil when it names no platform.
func mailReportClient(clientSession *session.ClientSession, deviceSpec string) *model.NetworkMailClient {
	platform, distribution := devicePlatform(clientSession, deviceSpec)
	if platform == "" {
		return nil
	}
	client := &model.NetworkMailClient{Platform: platform, Distribution: distribution}
	if clientSession != nil {
		if version := strings.TrimSpace(clientSession.ClientInfo.AppVersion); version != "" && len(version) <= 64 {
			client.AppVersion = version
		}
	}
	return client
}

// RecordNetworkMailContext keeps what a request reported for the network:
// its zone (usableReportedTimeZone), its languages (mailReportLanguages) and
// the app build it names. Called by auth-client, sign-up and sign-in, after
// the network's own write committed. Never fails the caller.
func RecordNetworkMailContext(
	clientSession *session.ClientSession,
	networkId server.Id,
	timeZone string,
	locales []string,
	locale string,
	deviceSpec string,
) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[mail]context failed for network %s: %v\n", networkId, r)
		}
	}()
	if clientSession == nil {
		return
	}
	settings := onboarding.DefaultMailContextSettings()
	report := &model.NetworkMailContextReport{
		TimeZone:  usableReportedTimeZone(timeZone),
		Languages: mailReportLanguages(clientSession, locales, locale, settings),
		Client:    mailReportClient(clientSession, deviceSpec),
	}
	model.RecordNetworkMailContext(clientSession.Ctx, networkId, report, server.NowUtc(), settings)
}

// recordSignInMailContext keeps what a sign-up or sign-in reported for the
// network its token names, once the sign-in committed: the request's zone
// and languages (the args the apps and the web app send) and its app build.
// A token that does not parse records nothing.
func recordSignInMailContext(clientSession *session.ClientSession, signedByJwt string, timeZone string, locales []string) {
	if clientSession == nil || signedByJwt == "" {
		return
	}
	runPostPrimaryOnboarding(clientSession, func(postSession *session.ClientSession) {
		byJwt, err := session.ParseByJwt(postSession.Ctx, signedByJwt)
		if err != nil {
			return
		}
		RecordNetworkMailContext(postSession, byJwt.NetworkId, timeZone, locales, "", "")
	})
}

// mailReaderSources is what a reader's send zone and languages are
// resolved from.
type mailReaderSources struct {
	// what the network's apps and browsers last reported; nil when nothing
	MailContext *model.NetworkMailContext
	// the onboarding row; nil for a network that never entered the program
	Onboarding *model.NetworkOnboarding
	// the countries of the network's purchases, as recorded (normalized
	// here): the latest App Store storefront and Play region, the Stripe
	// card's billing country
	AppStoreCountry string
	PlayCountry     string
	StripeCountry   string
	// the sign-up IP's country when no onboarding row carries it (a
	// re-engagement row's country)
	IpCountry string
	// the newest client event's locale; "" when none or not read
	EventLocale string
	// when the reader was last seen active: the last connection, else the
	// sign-up; nil when unknown
	ActivityAt *time.Time
}

// mailReader is a reader's resolved send zone and languages, with where
// each came from (reengagement.ContextSource*).
type mailReader struct {
	SendZone onboarding.SendZone
	// the IANA zone sends are placed in, "" for an activity frame or none:
	// what an episode row records and the tooling shows
	TimeZone       string
	TimeZoneSource string
	Languages      onboarding.MailLanguages
}

// localeCountry is the country a reader's zone and language fall back to:
// the App Store storefront, the Play region, the Stripe card's country, then
// the sign-up IP's. A store or billing country outranks the IP's, which for
// a VPN product is often an exit.
func (self *mailReaderSources) localeCountry() string {
	if country := self.purchaseCountry(); country != "" {
		return country
	}
	if self.Onboarding != nil {
		if country := model.CountryCodeFromStorefront(self.Onboarding.Country); country != "" {
			return country
		}
	}
	return model.CountryCodeFromStorefront(self.IpCountry)
}

// purchaseCountry is the first of the purchase countries that is a country.
func (self *mailReaderSources) purchaseCountry() string {
	for _, country := range []string{self.AppStoreCountry, self.PlayCountry, self.StripeCountry} {
		if country := model.CountryCodeFromStorefront(country); country != "" {
			return country
		}
	}
	return ""
}

// resolveMailReader takes the send zone and the languages from the sources,
// in the orders the file comment gives. frameHour and frameMinute are the
// local time an activity frame reads the reader's last activity as: the
// program's send hour (re-engagement), or the middle of the window
// (onboarding, whose sends keep the sign-up's time of day).
func resolveMailReader(s mailReaderSources, frameHour int, frameMinute int) mailReader {
	r := mailReader{}
	country := s.localeCountry()

	zoneSet := false
	if s.MailContext != nil {
		if timeZone := usableReportedTimeZone(s.MailContext.TimeZone); timeZone != "" {
			if loc, err := time.LoadLocation(timeZone); err == nil {
				r.SendZone, r.TimeZone, r.TimeZoneSource = onboarding.ZoneSendZone(loc), timeZone, reengagement.ContextSourceClient
				zoneSet = true
			}
		}
	}
	if !zoneSet && s.Onboarding != nil {
		// the sign-up IP's zone stands only where no purchase names another
		// country than the IP's: a VPN user signs up from the exit's country
		purchase := s.purchaseCountry()
		ipCountry := model.CountryCodeFromStorefront(s.Onboarding.Country)
		if timeZone := strings.TrimSpace(s.Onboarding.TimeZone); timeZone != "" && (purchase == "" || ipCountry == "" || purchase == ipCountry) {
			if loc, err := time.LoadLocation(timeZone); err == nil {
				r.SendZone, r.TimeZone, r.TimeZoneSource = onboarding.ZoneSendZone(loc), timeZone, reengagement.ContextSourceOnboarding
				zoneSet = true
			}
		}
	}
	if !zoneSet && country != "" {
		if zone, ok := onboarding.CountrySendZone(country); ok {
			r.SendZone, r.TimeZone, r.TimeZoneSource = zone, onboarding.CountryZoneName(country), reengagement.ContextSourceCountry
			zoneSet = true
		}
	}
	if !zoneSet && s.ActivityAt != nil {
		r.SendZone, r.TimeZoneSource = onboarding.ActivitySendZone(*s.ActivityAt, frameHour, frameMinute), reengagement.ContextSourceActivity
	}

	candidates := []onboarding.LanguageCandidate{}
	if s.MailContext != nil {
		for _, tag := range s.MailContext.Languages {
			candidates = append(candidates, onboarding.LanguageCandidate{Tag: tag, Source: reengagement.ContextSourceClient})
		}
	}
	if s.Onboarding != nil {
		if locale := normalizeLocale(s.Onboarding.Locale); locale != "" {
			candidates = append(candidates, onboarding.LanguageCandidate{Tag: locale, Source: reengagement.ContextSourceOnboarding})
		}
	}
	if locale := normalizeLocale(s.EventLocale); locale != "" {
		candidates = append(candidates, onboarding.LanguageCandidate{Tag: locale, Source: reengagement.ContextSourceClientEvent})
	}
	r.Languages = onboarding.MailLanguages{Candidates: candidates, Country: country}
	if country != "" {
		r.Languages.CountrySource = reengagement.ContextSourceCountry
	}
	return r
}

// mailReaderNeedsPurchaseCountries reports whether the purchase countries
// could change the resolution: the zone did not come from a device (the
// sign-up IP's zone stands only where they agree with it, and they outrank
// the IP's country), or no language the reader reported has a template.
func mailReaderNeedsPurchaseCountries(s mailReaderSources, settings *onboarding.MailLanguageSettings) bool {
	if s.MailContext == nil || usableReportedTimeZone(s.MailContext.TimeZone) == "" {
		return true
	}
	return !mailReaderServesALanguage(s, settings)
}

// mailReaderServesALanguage reports whether a language the reader reported
// (the mail context's, the onboarding row's, the event's) has a template.
func mailReaderServesALanguage(s mailReaderSources, settings *onboarding.MailLanguageSettings) bool {
	languages := []string{}
	if s.MailContext != nil {
		languages = append(languages, s.MailContext.Languages...)
	}
	if s.Onboarding != nil && s.Onboarding.Locale != "" {
		languages = append(languages, s.Onboarding.Locale)
	}
	if s.EventLocale != "" {
		languages = append(languages, s.EventLocale)
	}
	return onboarding.ServesAnyLanguage(languages, settings)
}

// loadMailReaderSources reads a network's sources: its mail context, and
// only where they could change the resolution, its purchase countries and
// its newest client event's locale. onb is the onboarding row the caller
// read (nil without one); ipCountry stands in for its country when it has
// none.
func loadMailReaderSources(ctx context.Context, networkId server.Id, onb *model.NetworkOnboarding, ipCountry string, activityAt *time.Time) mailReaderSources {
	settings := model.Onboarding().EffectiveMailLanguageSettings()
	s := mailReaderSources{
		MailContext: model.GetNetworkMailContext(ctx, networkId),
		Onboarding:  onb,
		IpCountry:   ipCountry,
		ActivityAt:  activityAt,
	}
	if mailReaderNeedsPurchaseCountries(s, settings) {
		s.AppStoreCountry, s.PlayCountry = model.NetworkStoreCountries(ctx, networkId)
		if s.AppStoreCountry == "" && s.PlayCountry == "" {
			s.StripeCountry = model.NetworkStripeBillingCountry(ctx, networkId)
		}
	}
	if !mailReaderServesALanguage(s, settings) {
		s.EventLocale = model.GetNetworkClientContext(ctx, networkId).EventLocale
	}
	return s
}
