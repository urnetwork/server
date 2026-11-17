// SPDX-License-Identifier: MPL-2.0

package onboarding

// Which app builds have the home screen widget that E2's activated email
// (mmm/onboarding/templates/e2_widget/activated) points at. The platform alone
// does not say: the App Store is on 2026.8.17, which has no widgets, so an
// iPhone reader was told to "Add the widget" in an app without one (the
// 2026-11-13 review). The controller judges the network's newest active
// client: its platform (from its device spec), the app version it last
// connected with, and the distribution the onboarding row recorded for that
// platform.

import (
	"strconv"
	"strings"

	"github.com/urnetwork/server/session"
)

// WidgetSettings are the app builds that have the home screen widget.
type WidgetSettings struct {
	// MinVersions maps "<platform>/<distribution>" (session.ClientDistribution*,
	// or "<platform>/" for a build that reported no distribution, as every
	// build did before the field) to the first app version, a calendar release
	// such as "2026.9.3", whose build carries the widget. A build of a pair
	// without an entry has no widget. Only phones count (WidgetPlatform): the
	// email is about the home screen.
	MinVersions map[string]string
}

// DefaultWidgetSettings are the release records of 2026-11-13. Android: the
// home screen widgets (android 843f21cec, 2026-09-02) are in the main source
// set, so every flavor's build has them from the first release tag after
// them, v2026.9.3; Google Play's release 2026.10.15 carries them. iOS: no App
// Store release has them yet. The App Store is on 2026.8.17, and Apple's
// widgets (apple 315d6bb2, 2026-09-02) are first in v2026.9.3; add
// "ios/app_store" with the first App Store release that ships them (the
// widget extension needs iOS 17, the app iOS 16). A Mac is never a widget
// platform: its desktop has no home screen.
func DefaultWidgetSettings() *WidgetSettings {
	return &WidgetSettings{
		MinVersions: map[string]string{
			"android/" + session.ClientDistributionPlay:          "2026.9.3",
			"android/" + session.ClientDistributionAndroidDirect: "2026.9.3",
			"android/" + session.ClientDistributionSolanaDapp:    "2026.9.3",
			"android/" + session.ClientDistributionEthosDapp:     "2026.9.3",
			"android/": "2026.9.3",
		},
	}
}

// HasWidget reports whether an app build has the home screen widget: a phone
// (WidgetPlatform) whose platform and distribution have a first version, on
// that version or a later one. An unknown or malformed version has none.
func (self *WidgetSettings) HasWidget(platform string, distribution string, appVersion string) bool {
	if !WidgetPlatform(platform) {
		return false
	}
	minVersion, ok := self.MinVersions[platform+"/"+distribution]
	if !ok {
		return false
	}
	return AppVersionAtLeast(appVersion, minVersion)
}

// AppVersionAtLeast reports whether appVersion is minVersion or a later
// release. Both are calendar releases, year.month.day, as the apps send them
// with an optional build code: "2026.10.15-1072765730" (Android, Windows) or
// "2026.8.17" (Apple); a leading "v" is allowed. A version of any other shape
// is not at least anything.
func AppVersionAtLeast(appVersion string, minVersion string) bool {
	parse := func(version string) (parts [3]int, ok bool) {
		version = strings.TrimPrefix(strings.TrimSpace(version), "v")
		if i := strings.IndexAny(version, "-+ _"); 0 <= i {
			version = version[:i]
		}
		fields := strings.Split(version, ".")
		if len(fields) != 3 {
			return
		}
		for i, field := range fields {
			if field == "" || strings.Trim(field, "0123456789") != "" {
				return
			}
			n, err := strconv.Atoi(field)
			if err != nil {
				return
			}
			parts[i] = n
		}
		ok = true
		return
	}
	version, ok := parse(appVersion)
	if !ok {
		return false
	}
	floor, ok := parse(minVersion)
	if !ok {
		return false
	}
	for i := range version {
		if version[i] != floor[i] {
			return floor[i] < version[i]
		}
	}
	return true
}
