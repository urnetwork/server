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
)

// ParamsInput is the data a send renders from.
type ParamsInput struct {
	Template string
	Variant  string
	Opening  string

	// device name the server knows for the network ("iPhone", "Android", ...)
	Platform string
	// free daily data in GB (pro.yml free.data)
	DailyDataGb float64
	// days since sign-up
	DaysActive int
	// days with at least one connection
	ConnectDays     int
	ProvidersOnline int
	CountriesOnline int

	// the welcome offer, when one exists
	OfferExpiresAt *time.Time
	OfferPriceUsd  float64
	RegularYearUsd float64
	TrialDays      int

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

// FormatCount renders an integer with thousands grouping ("4,812").
func FormatCount(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 || n < 0 {
		return s
	}
	var b strings.Builder
	head := len(s) % 3
	if head > 0 {
		b.WriteString(s[:head])
	}
	for i := head; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// FormatGb renders "30 GB" (whole) or "2.5 GB".
func FormatGb(gb float64) string {
	if gb == math.Trunc(gb) {
		return fmt.Sprintf("%d GB", int64(gb))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", gb), "0"), ".") + " GB"
}

// FormatLocalDateTime renders a deadline in the network's zone, e.g.
// "Sep 14, 2026 at 3:05 PM PDT" (English; the templates are localized in
// their copy, the deadline is a number-like token).
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
		offerExpires = FormatLocalDateTime(*in.OfferExpiresAt, in.Location)
	}
	trialEnd := FormatLocalDate(in.Now.Add(time.Duration(in.TrialDays)*24*time.Hour), in.Location)
	daysActive := in.DaysActive
	if daysActive < 1 {
		daysActive = 1
	}

	switch in.Template {
	case TemplateE1Connect:
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		p["daily_data_gb"] = FormatGb(in.DailyDataGb)
		p["platform"] = in.Platform
		// path A only: the deadline of the in-app offer; empty hides the line
		p["offer_expires_at"] = offerExpires
	case TemplateE2Widget:
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		p["providers_online"] = FormatCount(in.ProvidersOnline)
		if in.Variant == VariantActivated {
			p["connect_days"] = FormatCount(in.ConnectDays)
			if daysActive < 2 {
				daysActive = 2
			}
			p["days_active"] = FormatCount(daysActive)
			p["platform"] = in.Platform
			p["countries_online"] = FormatCount(in.CountriesOnline)
		}
	case TemplateE3LastChance:
		p["opening"] = in.Opening
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		p["offer_price"] = FormatUsd(in.OfferPriceUsd)
		p["regular_price"] = FormatUsd(in.RegularYearUsd)
		p["offer_expires_at"] = offerExpires
		p["trial_end_at"] = trialEnd
	case TemplateE4bOffer:
		p["cta_url"] = CtaUrl(in.SiteUrl, in.Template, in.Token)
		p["offer_price"] = FormatUsd(in.OfferPriceUsd)
		p["regular_price"] = FormatUsd(in.RegularYearUsd)
		p["offer_expires_at"] = offerExpires
		p["trial_end_at"] = trialEnd
	case TemplateE4Feedback:
		if in.Variant == VariantEngaged {
			p["connect_days"] = FormatCount(in.ConnectDays)
			p["days_active"] = FormatCount(daysActive)
			for r := 1; r <= 5; r++ {
				p["rating_url_"+strconv.Itoa(r)] = FeedbackUrl(in.SiteUrl, in.Token, "r="+strconv.Itoa(r))
			}
		} else {
			if daysActive < 5 {
				daysActive = 5
			}
			p["days_active"] = FormatCount(daysActive)
			for r := 1; r <= 4; r++ {
				p["reason_url_"+strconv.Itoa(r)] = FeedbackUrl(in.SiteUrl, in.Token, "why="+strconv.Itoa(r))
			}
		}
	}
	return p
}
