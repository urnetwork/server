package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The label of a client's app version: one bounded word for a metric, whatever
// the client sent.
const (
	AppVersionLabelUnknown = "unknown"
	AppVersionLabelOther   = "other"

	// releases this recent keep their day, so a rollout can be followed
	// release by release while it happens; older releases fold into their
	// month. The window and the date checks bound the labels.
	AppVersionLabelDayWindow = 62 * 24 * time.Hour
	// a release dated after now by more than this is not a real release
	appVersionLabelFutureSlack = 48 * time.Hour
	// app versions are calendar releases since 2025.3.31
	appVersionLabelMinYear = 2025
)

// AppVersionLabel normalizes a client's app version to a bounded label. The apps
// send their calendar release with an optional build code:
// "2026.10.1-1060587890" (Android, Windows) or "2026.10.1" (Apple). The build
// code is dropped. A release within AppVersionLabelDayWindow of now keeps its
// day, "2026.10.1"; an older one is its month, "2025.12". No version is unknown;
// anything else, including a date in the future, is other, so the label set
// stays bounded whatever a client sends.
func AppVersionLabel(appVersion string, now time.Time) string {
	appVersion = strings.TrimSpace(appVersion)
	if appVersion == "" {
		return AppVersionLabelUnknown
	}
	appVersion = strings.TrimPrefix(appVersion, "v")
	if i := strings.IndexAny(appVersion, "-+ _"); 0 <= i {
		appVersion = appVersion[:i]
	}
	parts := strings.Split(appVersion, ".")
	if len(parts) != 3 {
		return AppVersionLabelOther
	}
	var numbers [3]int
	for i, part := range parts {
		if part == "" || 4 < len(part) || strings.Trim(part, "0123456789") != "" {
			return AppVersionLabelOther
		}
		numbers[i], _ = strconv.Atoi(part)
	}
	year, month, day := numbers[0], numbers[1], numbers[2]
	release := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if year < appVersionLabelMinYear ||
		release.Year() != year || release.Month() != time.Month(month) || release.Day() != day ||
		now.Add(appVersionLabelFutureSlack).Before(release) {
		return AppVersionLabelOther
	}
	if now.Sub(release) <= AppVersionLabelDayWindow {
		return fmt.Sprintf("%d.%d.%d", year, month, day)
	}
	return fmt.Sprintf("%d.%d", year, month)
}
