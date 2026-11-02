// SPDX-License-Identifier: MPL-2.0

package onboarding

// Dates, times and device names in the reader's locale (mmm/onboarding/REVIEW.md
// finding 7). The templates are translated in the localizations store; the
// values the server fills in were English for every locale ("Sep 14, 2026 at
// 3:05 PM PDT" inside a Chinese sentence). The patterns follow each locale's
// usual numeric form (CLDR short/medium dates, 24-hour time except English),
// which avoids translating month names; the zone abbreviation stays because a
// network without a known zone is mailed in UTC.

import (
	"fmt"
	"strings"
	"time"
)

type localeStyle struct {
	// Go layouts
	date string
	// day and month without the year, for dates in the current year; "" =
	// always the full date
	dayMonth string
	clock    string
	// how a date and a time join: %[1]s is the date, %[2]s the time
	join string
	// the device word for a browser sign-up and for an unknown platform, in
	// the form the onboarding sentences ("on your {platform}") need
	browser string
	device  string
}

var localeStyles = map[string]localeStyle{
	"en":    {date: "Jan 2, 2006", dayMonth: "Jan 2", clock: "3:04 PM MST", join: "%[1]s at %[2]s", browser: "browser", device: "device"},
	"ar":    {date: "02/01/2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "المتصفح", device: "الجهاز"},
	"cs":    {date: "2. 1. 2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "prohlížeči", device: "zařízení"},
	"de":    {date: "02.01.2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "Browser", device: "Gerät"},
	"el":    {date: "2/1/2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "πρόγραμμα περιήγησης", device: "σύστημα"},
	"es":    {date: "02/01/2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "navegador", device: "dispositivo"},
	"fr":    {date: "02/01/2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "navigateur", device: "appareil"},
	"he":    {date: "2.1.2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "דפדפן", device: "מכשיר"},
	"hi":    {date: "2/1/2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "ब्राउज़र", device: "डिवाइस"},
	"id":    {date: "02/01/2006", clock: "15.04 MST", join: "%[1]s %[2]s", browser: "browser", device: "perangkat"},
	"it":    {date: "02/01/2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "browser", device: "dispositivo"},
	"ja":    {date: "2006/01/02", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "ブラウザ", device: "デバイス"},
	"ko":    {date: "2006. 1. 2.", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "브라우저", device: "기기"},
	"nl":    {date: "02-01-2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "browser", device: "apparaat"},
	"pl":    {date: "02.01.2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "urządzeniu", device: "urządzeniu"},
	"pt":    {date: "02/01/2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "navegador", device: "dispositivo"},
	"ru":    {date: "02.01.2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "браузере", device: "устройстве"},
	"sv":    {date: "2006-01-02", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "webbläsare", device: "enhet"},
	"sw":    {date: "02/01/2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "kompyuta", device: "kompyuta"},
	"th":    {date: "2/1/2006", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "เบราว์เซอร์", device: "อุปกรณ์"},
	"uk":    {date: "02.01.2006", clock: "15:04 MST", join: "%[1]s, %[2]s", browser: "браузері", device: "пристрої"},
	"vi":    {date: "02/01/2006", clock: "15:04 MST", join: "%[2]s, %[1]s", browser: "trình duyệt", device: "thiết bị"},
	"zh":    {date: "2006年1月2日", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "浏览器", device: "设备"},
	"zh-HK": {date: "2006年1月2日", clock: "15:04 MST", join: "%[1]s %[2]s", browser: "瀏覽器", device: "裝置"},
}

// EmailLocale resolves a recorded locale ("de-AT", "zh-TW", "pt-BR") to one of
// the email locales' styles: the exact tag, then Traditional Chinese regions to
// zh-HK, then the language, then English.
func EmailLocale(locale string) string {
	l := strings.TrimSpace(strings.ReplaceAll(locale, "_", "-"))
	if l == "" {
		return "en"
	}
	if _, ok := localeStyles[l]; ok {
		return l
	}
	parts := strings.Split(l, "-")
	lang := strings.ToLower(parts[0])
	if lang == "zh" {
		for _, p := range parts[1:] {
			switch strings.ToUpper(p) {
			case "HK", "TW", "MO", "HANT":
				return "zh-HK"
			}
		}
		return "zh"
	}
	if _, ok := localeStyles[lang]; ok {
		return lang
	}
	return "en"
}

func styleFor(locale string) localeStyle {
	return localeStyles[EmailLocale(locale)]
}

// FormatDateTimeLocale renders a moment in `loc` for `locale`, e.g. "Sep 14,
// 2026 at 3:05 PM PDT" (en), "14.09.2026, 15:05 CEST" (de), "2026年9月14日
// 15:05 CST" (zh).
func FormatDateTimeLocale(at time.Time, loc *time.Location, locale string) string {
	if loc == nil {
		loc = time.UTC
	}
	s := styleFor(locale)
	local := at.In(loc)
	return fmt.Sprintf(s.join, local.Format(s.date), local.Format(s.clock))
}

// FormatDateLocale renders a calendar date for `locale`, e.g. "Sep 23, 2026".
func FormatDateLocale(at time.Time, loc *time.Location, locale string) string {
	if loc == nil {
		loc = time.UTC
	}
	return at.In(loc).Format(styleFor(locale).date)
}

// FormatDayLocale renders a past or near date for running text: the day and
// month when it falls in `now`'s year and the locale has a short form ("Sep
// 28"), the full date otherwise.
func FormatDayLocale(at time.Time, loc *time.Location, locale string, now time.Time) string {
	if loc == nil {
		loc = time.UTC
	}
	s := styleFor(locale)
	local := at.In(loc)
	if s.dayMonth != "" && local.Year() == now.In(loc).Year() {
		return local.Format(s.dayMonth)
	}
	return local.Format(s.date)
}

// DeviceName is the `platform` template value: the product names as they are,
// and for a browser sign-up or an unknown platform the locale's word in the
// form the onboarding sentences need ("on your {platform}").
func DeviceName(platform string, locale string) string {
	switch platform {
	case "ios":
		return "iPhone"
	case "android":
		return "Android"
	case "macos":
		return "Mac"
	case "windows":
		return "Windows PC"
	case "linux":
		return "Linux"
	case "web":
		return styleFor(locale).browser
	}
	return styleFor(locale).device
}
