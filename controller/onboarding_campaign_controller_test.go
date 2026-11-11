// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNormalizeLocale(t *testing.T) {
	cases := map[string]string{
		"de-DE": "de-DE", "de_de": "de-DE", "EN": "en", "zh-Hant-HK": "zh-HK", "es-419": "es-419",
		"": "", "*": "", "1x": "", "toolongtoolongtoolongtoolongtoolong-XX": "", "pt-br": "pt-BR",
	}
	for in, want := range cases {
		if got := normalizeLocale(in); got != want {
			t.Errorf("normalizeLocale(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlatformFromDeviceSpec(t *testing.T) {
	cases := map[string]string{
		"iPhone15,2 iOS 26.0": "ios", "iPad": "ios", "Pixel 8 Android 15": "android", "macOS 26.0 arm64": "macos",
		"Windows 11": "windows", "Ubuntu 24.04": "linux", "Mozilla/5.0 (X11; Linux x86_64) Chrome": "linux",
		"Mozilla/5.0 Chrome/140": "web", "": "", "toaster": "",
	}
	for in, want := range cases {
		if got := platformFromDeviceSpec(in); got != want {
			t.Errorf("platformFromDeviceSpec(%q) = %q, want %q", in, got, want)
		}
	}
	if platformDeviceName("ios") != "iPhone" || platformDeviceName("") != "device" {
		t.Error("device names")
	}
}

// The specs the apps actually send on auth-client (device_spec on Main,
// 2026-11-10). The Android app sends "<os version> <make model>", and sent
// "<os version> <Build.FINGERPRINT>" before; neither names Android, so every
// Android network was left without a platform: its onboarding mail rendered
// "your device" and the web-billed branch with USD prices (store_billed off),
// though Google Play bills it. 1,832 Android networks got 5,465 onboarding
// sends in the 30 days to 2026-11-10 that way, 1,526 of them an offer email.
func TestPlatformFromDeviceSpecNamesTheAppsSpecs(t *testing.T) {
	cases := map[string]string{
		// Android, "<os version> <make model>" (android MainApplication.deviceSpec)
		"16 Samsung Galaxy S24 Ultra": "android",
		"17.1 Pixel 8 Pro":            "android",
		"10 Samsung SM-A125F":         "android",
		"8.1.0 Xiaomi Redmi 5 Plus":   "android",
		// Android, "<os version> <Build.FINGERPRINT>" (older app versions)
		"14 TECNO/LH7n-RU/TECNO-LH7n:14/UP1A.231005.007/250416V782:user/release-keys":                      "android",
		"11 Droidlogic/franklin/franklin:11/RD2A.211001.002/eng.ugoos.20231103.070631:userdebug/test-keys": "android",
		// a fingerprint whose build id contains "ios" is still Android
		"15 nubia/PQ82A04-UN/PQ82A04:15/AQ3A.240812.002/NebulaAIOS1.0.3_NX725J_GB:user/release-keys": "android",
		// iOS, "<os version> <model>" and the bare machine id
		"18.1 iPhone 16 Pro": "ios",
		"17.1 iPhone":        "ios",
		"iPhone14,5":         "ios",
		"iPad11,1":           "ios",
		// the other apps and the automation keep their answers
		"windows amd64":  "windows",
		"Windows 10 x64": "windows",
		"linux amd64":    "linux",
		"linux/amd64":    "linux",
		"macos":          "macos",
		"macOS Sonoma 14.6.1 (23G93) | Apple M3 8-Core | 24GB Unified Memory | 512GB Storage": "macos",
		"socks5":                  "",
		"resident":                "",
		"urnetwork/egress-prober": "",
		// the macOS app, "<os version> <model>" (mmm/ONBOARDING-RETENTION.md
		// P6): see TestPlatformFromDeviceSpecNamesTheMacApp
		"15.7.9 MacBook Air (M1, 2020)": "macos",
	}
	for in, want := range cases {
		if got := platformFromDeviceSpec(in); got != want {
			t.Errorf("platformFromDeviceSpec(%q) = %q, want %q", in, got, want)
		}
	}
}

// mmm/ONBOARDING-RETENTION.md P6: the macOS app's spec names only the Mac
// ("<os version> <retail name>", or the hardware model id the app's table
// has no name for), so every Mac app network was left without a platform. It
// then got the offer emails with card prices and no App Store rule, while a
// Mac browser's sign-up got the App Store's rule. A Mac app is macos now: the
// App Store build and the direct-download build send the same spec, and only
// the App Store build has a stable release.
func TestPlatformFromDeviceSpecNamesTheMacApp(t *testing.T) {
	cases := map[string]string{
		// the retail names of apple DeviceModelNames
		"15.7.9 MacBook Air (M1, 2020)":           "macos",
		"15.5 MacBook Pro (16-inch, Nov 2023)":    "macos",
		"26.1 Mac mini (2024)":                    "macos",
		"15.0 iMac (24-inch, 2021)":               "macos",
		"14.6 Mac Studio (2023)":                  "macos",
		"15.1 Mac Pro (2023)":                     "macos",
		"13.6 iMac Pro (2017)":                    "macos",
		"12.7 MacBook (Retina, 12-inch, 2017)":    "macos",
		"26.0 MacBook Pro (14-inch, M5, 2025)":    "macos",
		"15.7 MacBook Air (15-inch, M3, 2024)":    "macos",
		"14.7.1 MacBook Pro (13-inch, M2, 2022)":  "macos",
		"26.1 MacBook Air (13-inch, M4, 2025)":    "macos",
		"15.6.1 Mac Studio (2025)":                "macos",
		"11.7.10 MacBook Pro (15-inch, 2017)":     "macos",
		"10.15.7 iMac (Retina 5K, 27-inch, 2019)": "macos",
		// the hardware model id, when the table has no name for it
		"26.1 Mac16,10":         "macos",
		"14.6 MacBookPro18,3":   "macos",
		"13.6 MacBookAir10,1":   "macos",
		"15.0 iMac21,1":         "macos",
		"13.6 iMacPro1,1":       "macos",
		"12.7 Macmini9,1":       "macos",
		"12.7 MacPro7,1":        "macos",
		"12.7 MacBook10,1":      "macos",
		"15.7 Mac":              "macos",
		"18.1 iPhone 16 Pro":    "ios",
		"17.0 iPad Pro (M4)":    "ios",
		"16 Samsung Galaxy S24": "android",
		// not a Mac: an Android model that only mentions one, a bare word
		"14 Xiaomi Macro Edition": "",
		"Mac":                     "",
		"MacBook Air":             "",
	}
	for in, want := range cases {
		if got := platformFromDeviceSpec(in); got != want {
			t.Errorf("platformFromDeviceSpec(%q) = %q, want %q", in, got, want)
		}
	}
}

// mmm/ONBOARDING-RETENTION.md P14 and P6: a sign-up's platform. A browser
// sign-up was recorded as the visitor's OS: a Mac browser as macos (the App
// Store's offer rule and store-billed copy for a card buyer on the web), a
// Windows or Linux browser as the desktop app's platform. A desktop browser is
// web now; a phone's browser keeps the phone (the site sends it to the app's
// store); an app's declared device type decides first.
func TestPlatformFromRequest(t *testing.T) {
	request := func(deviceType string, userAgent string) *session.ClientSession {
		s := &session.ClientSession{Header: map[string][]string{}}
		if userAgent != "" {
			s.Header["User-Agent"] = []string{userAgent}
		}
		s.ClientInfo = session.UnknownClientInfo()
		if deviceType != "" {
			s.ClientInfo.DeviceType = deviceType
		}
		return s
	}
	const (
		macSafari     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15"
		macChrome     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
		windowsEdge   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0"
		linuxFirefox  = "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:144.0) Gecko/20100101 Firefox/144.0"
		chromebook    = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
		iphoneSafari  = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1"
		ipadSafari    = "Mozilla/5.0 (iPad; CPU OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1"
		androidChrome = "Mozilla/5.0 (Linux; Android 15; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36"
		goSdk         = "Go-http-client/2.0"
	)
	cases := []struct {
		name       string
		deviceType string
		userAgent  string
		want       string
	}{
		{"a Mac's Safari", "", macSafari, "web"},
		{"a Mac's Chrome", "", macChrome, "web"},
		{"a Windows browser", "", windowsEdge, "web"},
		{"a Linux browser", "", linuxFirefox, "web"},
		{"a Chromebook", "", chromebook, "web"},
		{"an iPhone's browser", "", iphoneSafari, "ios"},
		{"an iPad's browser", "", ipadSafari, "ios"},
		{"an Android phone's browser", "", androidChrome, "android"},
		// the apps declare their device type through their sdk
		{"the macOS app", "macos", goSdk, "macos"},
		{"the Android app", "android", goSdk, "android"},
		{"the iOS app", "ios", goSdk, "ios"},
		{"the Windows app", "windows", goSdk, "windows"},
		{"the Linux app", "linux", goSdk, "linux"},
		{"ur.io's sdk host", "web", macSafari, "web"},
		// a declared type outside the campaign's platforms falls through
		{"the cli", "cli", goSdk, ""},
		{"an unknown client", "unknown", goSdk, ""},
		{"no user agent", "", "", ""},
		{"a non-browser client names its platform as a spec does", "", "urnetwork windows amd64", "windows"},
	}
	for _, c := range cases {
		if got, distribution := platformFromRequest(request(c.deviceType, c.userAgent)); got != c.want || distribution != "" {
			t.Errorf("%s: platformFromRequest = %q, %q, want %q", c.name, got, distribution, c.want)
		}
	}
	if got, distribution := platformFromRequest(nil); got != "" || distribution != "" {
		t.Errorf("no session: %q, %q", got, distribution)
	}

	// a device authenticating: its app's declared type, else its spec
	if got, _ := devicePlatform(request("macos", goSdk), "15.7.9 MacBook Air (M1, 2020)"); got != "macos" {
		t.Errorf("the macOS app's device: %q", got)
	}
	if got, _ := devicePlatform(request("", goSdk), "16 Samsung Galaxy S24 Ultra"); got != "android" {
		t.Errorf("an older Android app's device: %q", got)
	}
	if got, _ := devicePlatform(request("unknown", ""), "socks5"); got != "" {
		t.Errorf("an automation's device: %q", got)
	}
}

// P6: the App Store and direct-download Mac builds send the same device type,
// spec and version, and their X-UR-ClientInfo distribution is what tells them
// apart. A sign-up and a device authenticating take it with the platform their
// app declares, from the header as the apps' sdk sends it; a browser sign-up
// and a device known only by its spec have no distribution.
func TestPlatformDistributionFromClientInfo(t *testing.T) {
	request := func(clientInfo string, userAgent string) *session.ClientSession {
		header := http.Header{}
		if clientInfo != "" {
			header.Set(session.ClientInfoHeader, clientInfo)
		}
		if userAgent != "" {
			header.Set("User-Agent", userAgent)
		}
		// as session.NewClientSessionFromRequest reads a request
		return &session.ClientSession{Header: header, ClientInfo: session.ClientInfoFromHeader(header)}
	}
	const (
		goSdk     = "Go-http-client/2.0"
		macSafari = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15"
		macSpec   = "15.7.9 MacBook Air (M1, 2020)"
	)
	cases := []struct {
		name             string
		clientInfo       string
		userAgent        string
		wantPlatform     string
		wantDistribution string
	}{
		{"the direct-download Mac", `{"v":1,"device_type":"macos","app_version":"2026.11.11-1","distribution":"mac_direct"}`, goSdk, "macos", session.ClientDistributionMacDirect},
		{"the App Store Mac", `{"v":1,"device_type":"macos","app_version":"2026.11.11-1","distribution":"app_store"}`, goSdk, "macos", session.ClientDistributionAppStore},
		{"a Mac app from before the field", `{"v":1,"device_type":"macos","app_version":"2026.11.10-1"}`, goSdk, "macos", ""},
		{"the F-Droid build", `{"v":1,"device_type":"android","app_version":"2026.11.11-1","distribution":"android_direct"}`, goSdk, "android", session.ClientDistributionAndroidDirect},
		{"the Linux AppImage", `{"v":1,"device_type":"linux","app_version":"2026.11.11-1","distribution":"linux_appimage"}`, goSdk, "linux", session.ClientDistributionLinuxAppimage},
		{"the extension", `{"v":1,"device_type":"web","app_version":"0.1.8","distribution":"extension_chrome"}`, macSafari, "web", session.ClientDistributionExtensionChrome},
		// a distribution that does not pair with the declared type is unknown
		{"an iPhone that claims the DMG", `{"v":1,"device_type":"ios","app_version":"1","distribution":"mac_direct"}`, goSdk, "ios", ""},
		// a type outside the campaign's platforms declares nothing
		{"the cli", `{"v":1,"device_type":"cli","app_version":"1","distribution":"linux"}`, goSdk, "", ""},
		{"a Mac's browser", "", macSafari, "web", ""},
	}
	for _, c := range cases {
		if platform, distribution := platformFromRequest(request(c.clientInfo, c.userAgent)); platform != c.wantPlatform || distribution != c.wantDistribution {
			t.Errorf("%s: sign-up %q, %q, want %q, %q", c.name, platform, distribution, c.wantPlatform, c.wantDistribution)
		}
	}
	if platform, distribution := devicePlatform(request(`{"v":1,"device_type":"macos","app_version":"1","distribution":"mac_direct"}`, goSdk), macSpec); platform != "macos" || distribution != session.ClientDistributionMacDirect {
		t.Errorf("the direct-download Mac's device: %q, %q", platform, distribution)
	}
	if platform, distribution := devicePlatform(request("", goSdk), macSpec); platform != "macos" || distribution != "" {
		t.Errorf("a Mac known by its spec: %q, %q", platform, distribution)
	}
}

func TestWebhookTags(t *testing.T) {
	if tags := webhookTags(&BrevoWebhookArgs{Tags: []string{"onboarding", "e1_connect", "default"}}); len(tags) != 3 {
		t.Errorf("array form: %v", tags)
	}
	if tags := webhookTags(&BrevoWebhookArgs{Tag: `["onboarding","e2_widget","activated"]`}); len(tags) != 3 || tags[1] != "e2_widget" {
		t.Errorf("legacy json string form: %v", tags)
	}
	if tags := webhookTags(&BrevoWebhookArgs{Tag: "plain"}); len(tags) != 1 || tags[0] != "plain" {
		t.Errorf("plain string form: %v", tags)
	}
	if tags := webhookTags(&BrevoWebhookArgs{}); tags != nil {
		t.Errorf("none: %v", tags)
	}
}

func TestDecideAppleOfferCodeTopUp(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	cfg := model.OnboardingAppleOfferCodesConfig{OfferCodeId: "abc", BatchSize: 500, MinAvailable: 200, ExpiryDays: 5}
	if d := DecideAppleOfferCodeTopUp(model.OnboardingAppleOfferCodesConfig{}, true, 0, now); d.Generate || d.Reason == "" {
		t.Errorf("no offer code id: %+v", d)
	}
	if d := DecideAppleOfferCodeTopUp(cfg, false, 0, now); d.Generate || d.Reason == "" {
		t.Errorf("no credentials: %+v", d)
	}
	if d := DecideAppleOfferCodeTopUp(cfg, true, 200, now); d.Generate {
		t.Errorf("at the floor: %+v", d)
	}
	d := DecideAppleOfferCodeTopUp(cfg, true, 199, now)
	if !d.Generate || d.BatchSize != 500 || !d.ExpiresAt.Equal(now.Add(5*24*time.Hour)) {
		t.Errorf("below the floor: %+v", d)
	}
	// Apple's minimum batch and the defaults
	d = DecideAppleOfferCodeTopUp(model.OnboardingAppleOfferCodesConfig{OfferCodeId: "abc", BatchSize: 10}, true, 0, now)
	if !d.Generate || d.BatchSize != 500 || !d.ExpiresAt.Equal(now.Add(5*24*time.Hour)) {
		t.Errorf("defaults: %+v", d)
	}
}

func TestParseAppleOfferCodeValues(t *testing.T) {
	expires := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	codes := parseAppleOfferCodeValues("Code\r\nABCDEF\r\nGHIJKL,extra\n\n", expires)
	if len(codes) != 2 || codes[0].Code != "ABCDEF" || codes[1].Code != "GHIJKL" || !codes[1].ExpiresAt.Equal(expires) {
		t.Errorf("values: %+v", codes)
	}
}

func TestOnboardingScheduleFromConfig(t *testing.T) {
	// with no onboarding.yml in the test config the plan's table applies
	s := onboardingSchedule()
	if s.E1Day <= 0 || s.E5bDay <= s.E4bDay {
		t.Errorf("schedule: %+v", s)
	}
	w := onboardingSendWindow()
	if w.StartHour != 8 || w.EndHour != 21 {
		t.Errorf("window: %+v", w)
	}
}
