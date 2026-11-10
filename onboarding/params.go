// SPDX-License-Identifier: MPL-2.0

package onboarding

// Template params (mmm/onboarding/templates/<step>/<variant>/manifest.json).
// Every value is a preformatted string: Brevo templates only interpolate, they
// never format. The builder is pure so the exact param set per template is
// unit tested against the manifests.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/number"
)

// ParamsInput is the data a send renders from.
type ParamsInput struct {
	Template string
	Variant  string
	Opening  string

	// device name the server knows for the network ("iPhone", "Android", ...),
	// already in the reader's language (DeviceName)
	Platform string
	// the template's locale (the store locale the Brevo template was rendered
	// in); dates and times are formatted for it (localdate.go). "" = en
	Locale string
	// free daily data in GiB (pro.yml free.data / model.Gib)
	DailyDataGb float64
	// days since sign-up
	DaysActive int
	// days with at least one connection
	ConnectDays     int
	ProvidersOnline int
	CountriesOnline int

	// the welcome offer, when one exists
	OfferExpiresAt *time.Time
	// the offer may not be emailed to the network (Decision.NoOfferEmail):
	// E1 leaves out its offer line
	NoOfferEmail   bool
	OfferPriceUsd  float64
	RegularYearUsd float64
	TrialDays      int
	// the App Store or Google Play bills the network: the offer emails carry
	// no price (the store charges its own local price; REVIEW.md finding 7)
	StoreBilled bool

	// the network's zone for the local date/time strings
	Location *time.Location
	// now, for the trial end
	Now time.Time

	// landing token (cta_url) and feedback token (rating/reason urls)
	Token string
	// the site the links point at, https://ur.io
	SiteUrl string

	CompanyLine string
}

// Landing url shapes (PLAN.md "Landing pages"): every CTA goes through the
// landing page, which records the click and routes into the app.
const (
	landingPath  = "/o/"
	feedbackPath = "/f/"
)

// StepLandingSlug is the landing page slug per template (the destination the
// landing page routes to).
func StepLandingSlug(template string) string {
	switch template {
	case TemplateE1Connect:
		return "connect"
	case TemplateE2Widget:
		return "widgets"
	case TemplateE3LastChance, TemplateE4bOffer:
		return "offer"
	case TemplateE4Feedback:
		return "feedback"
	}
	return "connect"
}

// The landing pages' slugs (ur.io /o/<slug>): onboarding's four, and the
// re-engagement program's update (the store listing or the download page),
// keep (records the choice and shows a thank-you page), learn (the threat
// model) and winback (the plan screen).
const (
	SlugConnect  = "connect"
	SlugWidgets  = "widgets"
	SlugOffer    = "offer"
	SlugFeedback = "feedback"
	SlugUpdate   = "update"
	SlugKeep     = "keep"
	SlugLearn    = "learn"
	SlugWinback  = "winback"
)

// IsLandingSlug reports whether s is a landing page slug.
func IsLandingSlug(s string) bool {
	switch s {
	case SlugConnect, SlugWidgets, SlugOffer, SlugFeedback, SlugUpdate, SlugKeep, SlugLearn, SlugWinback:
		return true
	default:
		return false
	}
}

// LandingSlug is the slug a token's step routes to: re-engagement tokens carry
// the slug itself; onboarding tokens carry the template, mapped by
// StepLandingSlug. The landing page routes by this value, so it must never be
// a template name (REVIEW.md finding 14: e4b_offer used to fall back to
// Connect).
func LandingSlug(step string) string {
	if IsLandingSlug(step) {
		return step
	}
	return StepLandingSlug(step)
}

// The one-tap "what got in the way?" reasons of the onboarding feedback email,
// as the apps' feedback screen names them. The links used to carry 1..4,
// which the apps did not recognize; old links still resolve through
// FeedbackReasonCode.
const (
	ReasonNotNeeded  = "not_needed"
	ReasonNotWorking = "not_working"
	ReasonTrust      = "trust"
	ReasonOther      = "other"
)

var onboardingReasonCodes = []string{ReasonNotNeeded, ReasonNotWorking, ReasonTrust, ReasonOther}

// FeedbackReasonCode normalizes a reason from a feedback link: the numeric
// form of older onboarding links (1..4) to its code; a code passes through.
func FeedbackReasonCode(reason string) string {
	switch strings.TrimSpace(reason) {
	case "1":
		return ReasonNotNeeded
	case "2":
		return ReasonNotWorking
	case "3":
		return ReasonTrust
	case "4":
		return ReasonOther
	}
	return strings.TrimSpace(reason)
}

// CtaUrl is https://ur.io/o/<slug>?t=<token>.
func CtaUrl(siteUrl string, template string, token string) string {
	return strings.TrimRight(siteUrl, "/") + landingPath + StepLandingSlug(template) + "?t=" + token
}

// FeedbackUrl is https://ur.io/f/<token>?r=<rating> or ?why=<reason>.
func FeedbackUrl(siteUrl string, token string, query string) string {
	return strings.TrimRight(siteUrl, "/") + feedbackPath + token + "?" + query
}

// FormatUsd renders a USD amount the way the plan's strings do: "$29.99",
// "$39.99", and whole amounts without cents ("$4").
func FormatUsd(usd float64) string {
	if usd < 0 {
		usd = 0
	}
	cents := math.Round(usd * 100)
	if math.Mod(cents, 100) == 0 {
		return "$" + strconv.FormatInt(int64(cents/100), 10)
	}
	return fmt.Sprintf("$%.2f", cents/100)
}

// FormatCount renders an integer with the thousands grouping of `locale`, the
// template's locale: "4,812" (en, zh, ja, es-419), "4.812" (de, es, it, pt),
// "4 812" with a no-break space (fr, ru, cs, pl, sv, uk, pt-PT). The symbols
// are CLDR's, as golang.org/x/text has them, always in Latin digits as the
// rest of the copy writes numbers (the Arabic copy too). "" or a malformed
// locale is English.
func FormatCount(n int, locale string) string {
	tag := language.English
	if l := strings.ReplaceAll(strings.TrimSpace(locale), "_", "-"); l != "" {
		if parsed, err := language.Parse(l); err == nil {
			tag = parsed
		}
	}
	if latin, err := tag.SetTypeForKey("nu", "latn"); err == nil {
		tag = latin
	}
	return message.NewPrinter(tag).Sprint(number.Decimal(n))
}

// formatKnownCount is FormatCount for a live count, "" when it is not known
// (0): the templates hide a line whose count is empty.
func formatKnownCount(n int, locale string) string {
	if n <= 0 {
		return ""
	}
	return FormatCount(n, locale)
}

// FormatGb renders an amount of data in GiB, "30 GiB" (whole) or "2.5 GiB":
// the callers pass bytes / model.Gib, the free grant is 30 GiB a day
// (config/all/pro.yml free.data: 30gib), and the apps, ur.io and the Pro line
// ("10 TiB a month") all use the binary units.
func FormatGb(gb float64) string {
	if gb == math.Trunc(gb) {
		return fmt.Sprintf("%d GiB", int64(gb))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", gb), "0"), ".") + " GiB"
}

// FormatLocalDateTime renders a deadline in the network's zone in English, e.g.
// "Sep 14, 2026 at 3:05 PM PDT". The templates use FormatDateTimeLocale.
func FormatLocalDateTime(at time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return at.In(loc).Format("Jan 2, 2006 at 3:04 PM MST")
}

// FormatLocalDate renders "Sep 23, 2026".
func FormatLocalDate(at time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return at.In(loc).Format("Jan 2, 2006")
}

// BuildParams is the param map for one send, exactly the manifest's keys for
// the template and variant (unknown template/variant = the union, so a typo
// over-supplies rather than under-supplies).
func BuildParams(in ParamsInput) map[string]string {
	p := map[string]string{
		"company_line": in.CompanyLine,
	}
	offerExpires := ""
	if in.OfferExpiresAt != nil {
		offerExpires = FormatDateTimeLocale(*in.OfferExpiresAt, in.Location, in.Locale)
	}
	trialEnd := FormatDateLocale(in.Now.Add(time.Duration(in.TrialDays)*24*time.Hour), in.Location, in.Locale)
	daysActive := in.DaysActive
	if daysActive < 1 {
		daysActive = 1
	}

	switch in.Template {
	case TemplateE1Connect:
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		p["daily_data_gb"] = FormatGb(in.DailyDataGb)
		p["platform"] = in.Platform
		// path A only: the deadline of the in-app offer; empty hides the line.
		// Never where the offer may not be emailed (no-offer countries, an
		// unknown country): the offer steps stand down there too
		p["offer_expires_at"] = offerExpires
		if in.NoOfferEmail {
			p["offer_expires_at"] = ""
		}
	case TemplateE2Widget:
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		// the live counts, or "" while they are unknown (the count query has
		// not succeeded yet): the template drops the line on an empty
		// providers_online rather than say "0 providers are online". The
		// activated line quotes both counts, so it needs both.
		providers, countries := in.ProvidersOnline, in.CountriesOnline
		if providers <= 0 || (in.Variant == VariantActivated && countries <= 0) {
			providers, countries = 0, 0
		}
		p["providers_online"] = formatKnownCount(providers, in.Locale)
		if in.Variant == VariantActivated {
			if daysActive < 2 {
				daysActive = 2
			}
			p["connect_days"] = FormatCount(connectDaysOfFirst(in.ConnectDays, daysActive), in.Locale)
			p["days_active"] = FormatCount(daysActive, in.Locale)
			p["platform"] = in.Platform
			p["countries_online"] = formatKnownCount(countries, in.Locale)
		}
	case TemplateE3LastChance:
		p["opening"] = in.Opening
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		p["store_billed"] = storeBilledFlag(in.StoreBilled)
		// the Pro value line, as in E4b (REVIEW.md finding 11)
		p["daily_data_gb"] = FormatGb(in.DailyDataGb)
		p["offer_price"] = FormatUsd(in.OfferPriceUsd)
		p["regular_price"] = FormatUsd(in.RegularYearUsd)
		p["offer_expires_at"] = offerExpires
		p["trial_end_at"] = trialEnd
	case TemplateE4bOffer:
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		// the Pro value line (email_pro_value: "10 TiB a month instead of
		// {daily_data_gb} a day"; REVIEW.md finding 11)
		p["daily_data_gb"] = FormatGb(in.DailyDataGb)
		p["store_billed"] = storeBilledFlag(in.StoreBilled)
		p["offer_price"] = FormatUsd(in.OfferPriceUsd)
		p["regular_price"] = FormatUsd(in.RegularYearUsd)
		p["offer_expires_at"] = offerExpires
		p["trial_end_at"] = trialEnd
	case TemplateE4Feedback:
		if in.Variant == VariantEngaged {
			p["connect_days"] = FormatCount(connectDaysOfFirst(in.ConnectDays, daysActive), in.Locale)
			p["days_active"] = FormatCount(daysActive, in.Locale)
			for r := 1; r <= 5; r++ {
				p["rating_url_"+strconv.Itoa(r)] = FeedbackUrl(in.SiteUrl, in.Token, "r="+strconv.Itoa(r))
			}
		} else {
			if daysActive < 5 {
				daysActive = 5
			}
			p["days_active"] = FormatCount(daysActive, in.Locale)
			for i, code := range onboardingReasonCodes {
				p["reason_url_"+strconv.Itoa(i+1)] = FeedbackUrl(in.SiteUrl, in.Token, "why="+code)
			}
		}
	}
	return p
}

// connectDaysOfFirst is the connect_days that the activated and engaged texts
// quote as "You've connected on {connect_days} of your first {days_active}
// days". Connection days are UTC calendar days, while days_active counts whole
// days since sign-up, so a network that signed up late in a UTC day and
// connects every day has more connection days than whole days. It connected
// on every one of its first days then, and never on more days than it has had.
func connectDaysOfFirst(connectDays int, daysActive int) int {
	return min(connectDays, daysActive)
}

// storeBilledFlag is the templates' `{% if params.store_billed %}` value.
func storeBilledFlag(on bool) string {
	if on {
		return "1"
	}
	return ""
}

// StoreBilledPlatform reports whether a store bills the platform's purchases:
// the App Store on ios and macos, Google Play on android.
func StoreBilledPlatform(platform string) bool {
	switch platform {
	case "ios", "macos", "android":
		return true
	}
	return false
}
