// SPDX-License-Identifier: MPL-2.0

package controller

// A network's mail context for the re-engagement program
// (mmm/reengagement/PLAN.md "Server" 7 and 10): the locale its emails are
// rendered in, the time zone its sends are placed in, the platform its "what
// changed" items are chosen for, and the country the mail policy reads. The
// onboarding row decides the country and the platform when it has them; a
// network older than the onboarding program (signed up before 2026-09-15)
// has no row, and its country comes from its purchases and its platform from
// its own clients' reports (reengagement/context.go). The time zone and the
// locale follow the chain of controller/mail_context.go, which reads first
// what the network's apps and browsers last reported. Never an IP lookup:
// without a source a part stays unknown, and an unknown country is the mail
// policy's strictest case.

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/reengagement"
)

// ReengagementContext is a network's resolved mail context, with where each
// part came from (reengagement.ContextSource*, "" when no source had it).
type ReengagementContext struct {
	// the email locale a send would be written in (onboarding.EmailLocales);
	// "" when no source names one, which is English
	Locale       string `json:"locale"`
	LocaleSource string `json:"locale_source"`
	// the IANA zone sends are placed in; "" when none, or an activity frame
	// (TimeZoneSource activity)
	TimeZone       string `json:"time_zone"`
	TimeZoneSource string `json:"time_zone_source"`
	Platform       string `json:"platform"`
	PlatformSource string `json:"platform_source"`
	// upper-case ISO 3166-1 alpha-2
	Country       string `json:"country"`
	CountrySource string `json:"country_source"`
}

// reengagementContextSources is what a context is resolved from.
type reengagementContextSources struct {
	// nil for a network that never entered the onboarding program
	Onboarding *model.NetworkOnboarding
	// the countries of the network's latest App Store and Google Play
	// subscription rows and of its Stripe card, as recorded (alpha-2, or the
	// App Store's alpha-3); normalized here
	AppStoreCountry string
	PlayCountry     string
	StripeCountry   string
	// what the network's clients last reported; nil when not read
	Client *model.NetworkClientContext
	// what the network's apps and browsers last reported on auth-client,
	// sign-up and sign-in; nil when nothing
	MailContext *model.NetworkMailContext
	// when the reader was last active: the last connection, else the
	// sign-up; nil when unknown
	ActivityAt *time.Time
	// the hour an activity frame places sends at; 0 is the default schedule's
	SendHour int
	// nil is onboarding.DefaultMailLanguageSettings
	LanguageSettings *onboarding.MailLanguageSettings
}

// mailReaderSources are the mail chain's view of the sources.
func (self *reengagementContextSources) mailReaderSources() mailReaderSources {
	s := mailReaderSources{
		MailContext:     self.MailContext,
		Onboarding:      self.Onboarding,
		AppStoreCountry: self.AppStoreCountry,
		PlayCountry:     self.PlayCountry,
		StripeCountry:   self.StripeCountry,
		ActivityAt:      self.ActivityAt,
	}
	if self.Client != nil {
		s.EventLocale = self.Client.EventLocale
	}
	return s
}

func (self *reengagementContextSources) languageSettings() *onboarding.MailLanguageSettings {
	if self.LanguageSettings != nil {
		return self.LanguageSettings
	}
	return onboarding.DefaultMailLanguageSettings()
}

func (self *reengagementContextSources) sendHour() int {
	if 0 < self.SendHour {
		return self.SendHour
	}
	return reengagement.DefaultSchedule.SendHour
}

// resolveReengagementContext takes each part from its first source with a
// usable value. The country: the onboarding row's, then the App Store
// storefront, the Google Play region and the Stripe card's billing country,
// in that order. The platform: the onboarding row's, then the newer of the
// newest client event's and the one the newest client's device spec names.
// The time zone and the locale: the mail chain (resolveMailReader), the
// locale as the email locale it picks among the shipped ones. Countries are
// normalized to alpha-2; a value that does not normalize is no value.
func resolveReengagementContext(s reengagementContextSources) ReengagementContext {
	c, _ := resolveReengagementContextReader(s)
	return c
}

// resolveReengagementContextReader is resolveReengagementContext with the
// mail chain's resolution, whose send zone places the episode's steps.
func resolveReengagementContextReader(s reengagementContextSources) (ReengagementContext, mailReader) {
	c := ReengagementContext{}
	if onb := s.Onboarding; onb != nil {
		if platform := reengagementEventPlatform(onb.Platform); platform != "" {
			c.Platform, c.PlatformSource = platform, reengagement.ContextSourceOnboarding
		}
		if country := model.CountryCodeFromStorefront(onb.Country); country != "" {
			c.Country, c.CountrySource = country, reengagement.ContextSourceOnboarding
		}
	}
	if c.Country == "" {
		for _, candidate := range []struct {
			country string
			source  string
		}{
			{s.AppStoreCountry, reengagement.ContextSourceAppStore},
			{s.PlayCountry, reengagement.ContextSourcePlay},
			{s.StripeCountry, reengagement.ContextSourceStripe},
		} {
			if country := model.CountryCodeFromStorefront(candidate.country); country != "" {
				c.Country, c.CountrySource = country, candidate.source
				break
			}
		}
	}
	if client := s.Client; client != nil && c.Platform == "" {
		c.Platform, c.PlatformSource = newestClientPlatform(client)
	}

	reader := resolveMailReader(s.mailReaderSources(), s.sendHour(), 0)
	c.TimeZone, c.TimeZoneSource = reader.TimeZone, reader.TimeZoneSource
	if locale, source := onboarding.ChooseTemplateLocale(reader.Languages, onboarding.EmailLocales, s.languageSettings()); source != "" {
		c.Locale, c.LocaleSource = locale, source
	}
	return c, reader
}

// reengagementEventPlatform is the platform as the events and the highlights
// name it (model.EventPlatforms), or "".
func reengagementEventPlatform(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if slices.Contains(model.EventPlatforms, platform) {
		return platform
	}
	return ""
}

// newestClientPlatform is the platform of the more recent of the two client
// reports that name one: the newest client event, or the device spec of the
// newest client as of that client's last authentication. A spec that names no
// platform (an automation's "socks5") is no report; a Mac's "<os version>
// <model>" spec names macos (macAppModelSpec). On a tie the event wins: it
// names its platform outright.
func newestClientPlatform(client *model.NetworkClientContext) (platform string, source string) {
	eventPlatform := reengagementEventPlatform(client.EventPlatform)
	devicePlatform := platformFromDeviceSpec(client.DeviceSpec)
	deviceNewer := client.DeviceSpecAt != nil && (client.EventPlatformAt == nil || client.DeviceSpecAt.After(*client.EventPlatformAt))
	switch {
	case devicePlatform != "" && (eventPlatform == "" || deviceNewer):
		return devicePlatform, reengagement.ContextSourceDevice
	case eventPlatform != "":
		return eventPlatform, reengagement.ContextSourceClientEvent
	}
	return "", ""
}

// reengagementContext resolves the network's context, reading only what can
// change it: the onboarding row and the mail context always; the purchase
// countries when the row names no country or the mail chain could fall back
// to them; the clients' reports when the platform is unknown or no reported
// language has a template. lastConnect is the network's last connection, the
// activity an unknown zone falls back to (else the sign-up the row records).
func reengagementContext(ctx context.Context, networkId server.Id, lastConnect *time.Time) ReengagementContext {
	c, _ := reengagementContextReader(ctx, networkId, lastConnect)
	return c
}

// reengagementContextReader is reengagementContext with the mail chain's
// resolution, whose send zone places the episode's first step.
func reengagementContextReader(ctx context.Context, networkId server.Id, lastConnect *time.Time) (ReengagementContext, mailReader) {
	cfg := model.Reengagement()
	languageSettings := model.Onboarding().EffectiveMailLanguageSettings()
	sources := reengagementContextSources{
		Onboarding:       model.GetNetworkOnboarding(ctx, networkId),
		MailContext:      model.GetNetworkMailContext(ctx, networkId),
		ActivityAt:       lastConnect,
		SendHour:         cfg.ScheduleValue().SendHour,
		LanguageSettings: languageSettings,
	}
	if sources.ActivityAt == nil && sources.Onboarding != nil {
		createdAt := sources.Onboarding.CreatedAt
		sources.ActivityAt = &createdAt
	}
	c := resolveReengagementContext(sources)
	if c.Country == "" || mailReaderNeedsPurchaseCountries(sources.mailReaderSources(), languageSettings) {
		sources.AppStoreCountry, sources.PlayCountry = model.NetworkStoreCountries(ctx, networkId)
		if sources.AppStoreCountry == "" && sources.PlayCountry == "" {
			sources.StripeCountry = model.NetworkStripeBillingCountry(ctx, networkId)
		}
	}
	if c.Platform == "" || !mailReaderServesALanguage(sources.mailReaderSources(), languageSettings) {
		sources.Client = model.GetNetworkClientContext(ctx, networkId)
	}
	return resolveReengagementContextReader(sources)
}

// reengagementMailReader resolves an episode's send zone and languages at
// send time, so a zone or language a device reported after the episode
// opened places and writes its later steps. The episode's country stands in
// for the onboarding row's when the network has none; its last connection,
// else its start, is the activity an unknown zone falls back to.
func reengagementMailReader(ctx context.Context, row *model.NetworkReengagement) mailReader {
	activityAt := row.StartedAt
	if row.LastConnectAt != nil {
		activityAt = *row.LastConnectAt
	}
	onb := model.GetNetworkOnboarding(ctx, row.NetworkId)
	sources := loadMailReaderSources(ctx, row.NetworkId, onb, row.Country, &activityAt)
	return resolveMailReader(sources, model.Reengagement().ScheduleValue().SendHour, 0)
}

// countReengagementContextSources records where a scan candidate's context
// came from, part by part: the program's reach by source.
func countReengagementContextSources(stream string, c ReengagementContext) {
	for _, part := range []struct {
		name   string
		source string
	}{
		{"country", c.CountrySource},
		{"locale", c.LocaleSource},
		{"platform", c.PlatformSource},
		{"time_zone", c.TimeZoneSource},
	} {
		source := part.source
		if source == "" {
			source = reengagement.ContextSourceUnknown
		}
		reengagementContextSourcesTotal.WithLabelValues(stream, part.name, source).Inc()
	}
}
