// SPDX-License-Identifier: MPL-2.0

package controller

// A network's mail context for the re-engagement program
// (mmm/reengagement/PLAN.md "Server" 7 and 10): the locale its emails are
// rendered in, the time zone its sends are placed in, the platform its "what
// changed" items are chosen for, and the country the mail policy reads. The
// onboarding row decides every part it has. A network older than the
// onboarding program (signed up before 2026-09-15) has no row; its country
// comes from its purchases and its locale and platform from its own clients'
// reports (reengagement/context.go). Never an IP lookup: without a source a
// part stays unknown, and an unknown country is the mail policy's strictest
// case.

import (
	"context"
	"slices"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/reengagement"
)

// ReengagementContext is a network's resolved mail context, with where each
// part came from (reengagement.ContextSource*, "" when no source had it).
type ReengagementContext struct {
	Locale         string `json:"locale"`
	LocaleSource   string `json:"locale_source"`
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
}

// resolveReengagementContext takes each part from its first source with a
// usable value. The onboarding row comes first for every part. The country
// then falls back to the App Store storefront, the Google Play region and the
// Stripe card's billing country, in that order; the locale to the newest
// client event's; the platform to the newer of the newest client event's and
// the one the newest client's device spec names. The time zone has no
// fallback. Countries are normalized to alpha-2 and locales to the onboarding
// rows' form; a value that does not normalize is no value.
func resolveReengagementContext(s reengagementContextSources) ReengagementContext {
	c := ReengagementContext{}
	if onb := s.Onboarding; onb != nil {
		if locale := normalizeLocale(onb.Locale); locale != "" {
			c.Locale, c.LocaleSource = locale, reengagement.ContextSourceOnboarding
		}
		if timeZone := strings.TrimSpace(onb.TimeZone); timeZone != "" {
			c.TimeZone, c.TimeZoneSource = timeZone, reengagement.ContextSourceOnboarding
		}
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
	if client := s.Client; client != nil {
		if c.Locale == "" {
			if locale := normalizeLocale(client.EventLocale); locale != "" {
				c.Locale, c.LocaleSource = locale, reengagement.ContextSourceClientEvent
			}
		}
		if c.Platform == "" {
			c.Platform, c.PlatformSource = newestClientPlatform(client)
		}
	}
	return c
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
// platform (a Mac's "<os version> <model>" spec names only a model) is no report. On a
// tie the event wins: it names its platform outright.
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

// reengagementContext resolves the network's context, reading only what the
// onboarding row leaves open: a network with a complete row costs one read;
// one without a row reads its store countries (and its card's, when no store
// has one) and its client reports.
func reengagementContext(ctx context.Context, networkId server.Id) ReengagementContext {
	sources := reengagementContextSources{
		Onboarding: model.GetNetworkOnboarding(ctx, networkId),
	}
	c := resolveReengagementContext(sources)
	if c.Country == "" {
		sources.AppStoreCountry, sources.PlayCountry = model.NetworkStoreCountries(ctx, networkId)
		if sources.AppStoreCountry == "" && sources.PlayCountry == "" {
			sources.StripeCountry = model.NetworkStripeBillingCountry(ctx, networkId)
		}
	}
	if c.Locale == "" || c.Platform == "" {
		sources.Client = model.GetNetworkClientContext(ctx, networkId)
	}
	return resolveReengagementContext(sources)
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
