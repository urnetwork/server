// SPDX-License-Identifier: MPL-2.0

package controller

// Which readers draw an App Store one-time offer code, through the paths that
// draw one: the app's offer call, step E4b's email issue and the plan
// response. On 2026-11-12, 451 of the 509 codes drawn on Main went to Android
// and web networks: every offer drew one, whatever its reader, and the email
// offer drew one at the send. These tests use only entry points that predate
// the fix, so they run against the code before it too.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The X-UR-ClientInfo each app's sdk sends.
const (
	appleOfferReaderIos         = `{"v":1,"device_type":"ios","app_version":"2026.11.11-1","distribution":"app_store"}`
	appleOfferReaderIosOlder    = `{"v":1,"device_type":"ios","app_version":"2026.11.10-1"}`
	appleOfferReaderMacAppStore = `{"v":1,"device_type":"macos","app_version":"2026.11.11-1","distribution":"app_store"}`
	appleOfferReaderMacOlder    = `{"v":1,"device_type":"macos","app_version":"2026.11.10-1"}`
	appleOfferReaderMacDirect   = `{"v":1,"device_type":"macos","app_version":"2026.11.11-1","distribution":"mac_direct"}`
	appleOfferReaderPlay        = `{"v":1,"device_type":"android","app_version":"2026.11.11-1","distribution":"play"}`
	appleOfferReaderAndroid     = `{"v":1,"device_type":"android","app_version":"2026.11.10-1"}`
	appleOfferReaderWeb         = `{"v":1,"device_type":"web","app_version":"2026.11.11-1","distribution":"web"}`
	appleOfferReaderWindows     = `{"v":1,"device_type":"windows","app_version":"2026.11.11-1","distribution":"windows"}`
	appleOfferReaderLinux       = `{"v":1,"device_type":"linux","app_version":"2026.11.11-1","distribution":"linux_flatpak"}`
)

// Runs the welcome offer as on Main with the App Store code pool on, a
// synthetic Play offer tag and Stripe coupon, and no experiments, so no
// network is held out of the in-app offer.
func setAppleOfferReaderTestConfig(t testing.TB) {
	setAppleOfferTestConfig(t, model.OnboardingAppleOfferCodesConfig{})
	config := model.Onboarding()
	config.Offer.StripeCouponId = "synthetic-coupon"
	config.Offer.PlayOfferTag = "synthetic-play-offer"
	config.Experiments = nil
}

// Puts count synthetic one-time codes in the pool, each outliving any offer
// the test issues.
func loadAppleOfferReaderTestCodes(t testing.TB, ctx context.Context, prefix string, count int) {
	codes := []*model.AppleOfferCode{}
	expiresAt := server.NowUtc().Add(30 * 24 * time.Hour)
	for i := 0; i < count; i++ {
		codes = append(codes, &model.AppleOfferCode{Code: fmt.Sprintf("%s-%d", prefix, i), ExpiresAt: expiresAt})
	}
	added, err := model.LoadAppleOfferCodePool(ctx, codes)
	if err != nil || added != count {
		t.Fatalf("could not load the pool: added %d of %d, err %v", added, count, err)
	}
}

// The codes left that an offer issued now can take.
func appleOfferReaderPoolLeft(ctx context.Context) int {
	return model.CountAvailableAppleOfferCodes(ctx, server.NowUtc())
}

// A synthetic network for the reader tests.
func newAppleOfferReaderNetwork(ctx context.Context) (networkId server.Id, userId server.Id) {
	networkId = server.NewId()
	userId = server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("appleofferreader%s", networkId), userId)
	return
}

// An app's authenticated session on the network, as a request from it is
// read: clientInfo is the X-UR-ClientInfo its sdk sends ("" for an app that
// sends none), and a deviceSpec registers the session's client with that
// device spec. Each session has its own documentation address, so the
// offer call's per-address limit never refuses a test.
func appleOfferReaderSession(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id, clientInfo string, deviceSpec string) *session.ClientSession {
	byJwt := &session.ByJwt{NetworkId: networkId, UserId: userId}
	if deviceSpec != "" {
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic device", deviceSpec)
		byJwt.ClientId = &clientId
	}
	address := appleOfferReaderAddresses.Add(1)%250 + 1
	clientSession := session.NewLocalClientSession(ctx, fmt.Sprintf("192.0.2.%d:443", address), byJwt)
	t.Cleanup(clientSession.Cancel)
	header := http.Header{}
	if clientInfo != "" {
		header.Set(session.ClientInfoHeader, clientInfo)
	}
	clientSession.ClientInfo = session.ClientInfoFromHeader(header)
	return clientSession
}

// The last documentation address a reader session took.
var appleOfferReaderAddresses atomic.Int64

// Issues the welcome offer from the app's session, as the app's offer screen
// does.
func issueAppleOfferReaderOffer(t testing.TB, clientSession *session.ClientSession) *OnboardingOfferResult {
	result, err := OnboardingOfferIssue(&OnboardingOfferIssueArgs{Surface: model.OnboardingOfferSurfaceIntroStep}, clientSession)
	if err != nil || result == nil || result.Error != nil || result.Offer == nil {
		t.Fatalf("could not issue the welcome offer: result %+v, err %v", result, err)
	}
	return result.Offer
}

// The welcome offer on the plan response, as the app reads it.
func readAppleOfferReaderPlan(t testing.TB, clientSession *session.ClientSession) *OnboardingOfferResult {
	result := &SubscriptionBalanceResult{}
	DecoratePlan(result, "", clientSession)
	if result.OnboardingOffer == nil {
		t.Fatalf("the plan response has no welcome offer")
	}
	return result.OnboardingOffer
}

// The app's offer call draws an App Store code only for a reader on an App
// Store build: the iOS app and the Mac App Store build, including builds from
// before the apps reported their distribution. Every other reader gets the
// same offer with its own store's handle (the Play offer tag, the Stripe
// coupon) and leaves the pool as it was.
func TestWelcomeOfferDrawsAppleCodeOnlyForAnAppStoreReader(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		setAppleOfferReaderTestConfig(t)
		loadAppleOfferReaderTestCodes(t, ctx, "SYNTHETIC-READER", 4)

		for _, c := range []struct {
			name       string
			clientInfo string
		}{
			{name: "the Play app", clientInfo: appleOfferReaderPlay},
			{name: "an Android app from before the distribution field", clientInfo: appleOfferReaderAndroid},
			{name: "ur.io", clientInfo: appleOfferReaderWeb},
			{name: "the Windows app", clientInfo: appleOfferReaderWindows},
			{name: "the Linux app", clientInfo: appleOfferReaderLinux},
			{name: "the direct-download Mac", clientInfo: appleOfferReaderMacDirect},
		} {
			networkId, userId := newAppleOfferReaderNetwork(ctx)
			offer := issueAppleOfferReaderOffer(t, appleOfferReaderSession(t, ctx, networkId, userId, c.clientInfo, ""))
			if offer.AppleOfferCode != "" {
				t.Errorf("%s: its offer took App Store code %q, which it can never redeem", c.name, offer.AppleOfferCode)
			}
			if offer.PlayOfferTag != "synthetic-play-offer" || offer.StripeCouponId != "synthetic-coupon" {
				t.Errorf("%s: its offer lost its own store's handle: play %q, stripe %q", c.name, offer.PlayOfferTag, offer.StripeCouponId)
			}
			if offer.State != model.OnboardingOfferStateActive || offer.PercentOff != 25 {
				t.Errorf("%s: the offer is not the welcome offer: %+v", c.name, offer)
			}
		}
		if left := appleOfferReaderPoolLeft(ctx); left != 4 {
			t.Errorf("readers who cannot redeem an App Store code drew %d of the pool's 4", 4-left)
		}

		for _, c := range []struct {
			name       string
			clientInfo string
		}{
			{name: "the iOS app", clientInfo: appleOfferReaderIos},
			{name: "an iOS app from before the distribution field", clientInfo: appleOfferReaderIosOlder},
			{name: "the Mac App Store build", clientInfo: appleOfferReaderMacAppStore},
			{name: "a Mac app from before the distribution field", clientInfo: appleOfferReaderMacOlder},
		} {
			networkId, userId := newAppleOfferReaderNetwork(ctx)
			offer := issueAppleOfferReaderOffer(t, appleOfferReaderSession(t, ctx, networkId, userId, c.clientInfo, ""))
			if !strings.HasPrefix(offer.AppleOfferCode, "SYNTHETIC-READER-") {
				t.Errorf("%s: its offer got no one-time App Store code (%q)", c.name, offer.AppleOfferCode)
			}
		}
		if left := appleOfferReaderPoolLeft(ctx); left != 0 {
			t.Errorf("%d codes left after four App Store readers, want 0", left)
		}
	})
}

// An app whose sdk sends no X-UR-ClientInfo is known by the device spec of
// its client: an iPhone draws a code, an Android phone or a session without
// a client does not.
func TestWelcomeOfferKnowsAnOlderAppByItsDeviceSpec(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		setAppleOfferReaderTestConfig(t)
		loadAppleOfferReaderTestCodes(t, ctx, "SYNTHETIC-SPEC", 3)

		for _, c := range []struct {
			name       string
			deviceSpec string
		}{
			{name: "an older Android app", deviceSpec: "14 synthetic phone a54"},
			{name: "an automation's client", deviceSpec: "socks5"},
			{name: "a session without a client", deviceSpec: ""},
		} {
			networkId, userId := newAppleOfferReaderNetwork(ctx)
			offer := issueAppleOfferReaderOffer(t, appleOfferReaderSession(t, ctx, networkId, userId, "", c.deviceSpec))
			if offer.AppleOfferCode != "" {
				t.Errorf("%s: its offer took App Store code %q", c.name, offer.AppleOfferCode)
			}
		}
		if left := appleOfferReaderPoolLeft(ctx); left != 3 {
			t.Errorf("readers who cannot redeem an App Store code drew %d of the pool's 3", 3-left)
		}

		networkId, userId := newAppleOfferReaderNetwork(ctx)
		offer := issueAppleOfferReaderOffer(t, appleOfferReaderSession(t, ctx, networkId, userId, "", "17.6.1 iphone 15"))
		if !strings.HasPrefix(offer.AppleOfferCode, "SYNTHETIC-SPEC-") {
			t.Errorf("an older iOS app's offer got no one-time App Store code (%q)", offer.AppleOfferCode)
		}
	})
}

// Step E4b issues the offer by email without drawing a code: most of its
// readers redeem through Google Play or Stripe. The code is drawn the first
// time an App Store build reads the offer (the plan the app reads when the
// email's link opens it, or its offer call), once, and other readers never
// draw one.
func TestEmailOfferDrawsAppleCodeWhenAnAppStoreBuildReadsIt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		setAppleOfferReaderTestConfig(t)
		loadAppleOfferReaderTestCodes(t, ctx, "SYNTHETIC-EMAIL", 2)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()

		networkId, userId := newAppleOfferReaderNetwork(ctx)
		issued, created, err := IssueOnboardingOfferByEmail(owner, networkId, "standard")
		if err != nil || !created || issued == nil {
			t.Fatalf("could not issue the email offer: created %t, err %v", created, err)
		}
		if issued.AppleOfferCode != nil {
			t.Errorf("the email offer drew App Store code %q at the send", *issued.AppleOfferCode)
		}
		if left := appleOfferReaderPoolLeft(ctx); left != 2 {
			t.Fatalf("the email send drew %d of the pool's 2", 2-left)
		}

		// the reader opens the link on the Play app: its plan and its offer
		// call show the offer with no App Store code, and draw none
		play := appleOfferReaderSession(t, ctx, networkId, userId, appleOfferReaderPlay, "")
		if offer := readAppleOfferReaderPlan(t, play); offer.AppleOfferCode != "" || offer.PlayOfferTag != "synthetic-play-offer" {
			t.Errorf("the Play app's plan shows App Store code %q, play offer %q", offer.AppleOfferCode, offer.PlayOfferTag)
		}
		if offer := issueAppleOfferReaderOffer(t, play); offer.AppleOfferCode != "" {
			t.Errorf("the Play app's offer call shows App Store code %q", offer.AppleOfferCode)
		}
		if left := appleOfferReaderPoolLeft(ctx); left != 2 {
			t.Fatalf("the Play app's reads drew %d of the pool's 2", 2-left)
		}

		// then on the iOS app: its plan draws the code
		ios := appleOfferReaderSession(t, ctx, networkId, userId, appleOfferReaderIos, "")
		code := readAppleOfferReaderPlan(t, ios).AppleOfferCode
		if !strings.HasPrefix(code, "SYNTHETIC-EMAIL-") {
			t.Fatalf("the iOS app read the email offer without an App Store code (%q)", code)
		}
		if left := appleOfferReaderPoolLeft(ctx); left != 1 {
			t.Errorf("%d codes left after the iOS app's read, want 1", left)
		}
		stored := model.GetOnboardingOffer(ctx, networkId)
		if stored == nil || stored.AppleOfferCode == nil || *stored.AppleOfferCode != code {
			t.Errorf("the offer row does not hold the code the iOS app read: %+v", stored)
		}

		// later reads, by any app, show the same code and draw no other
		if again := issueAppleOfferReaderOffer(t, ios).AppleOfferCode; again != code {
			t.Errorf("the iOS app's offer call shows %q, want the drawn %q", again, code)
		}
		mac := appleOfferReaderSession(t, ctx, networkId, userId, appleOfferReaderMacAppStore, "")
		if again := readAppleOfferReaderPlan(t, mac).AppleOfferCode; again != code {
			t.Errorf("the Mac App Store build's plan shows %q, want the drawn %q", again, code)
		}
		if left := appleOfferReaderPoolLeft(ctx); left != 1 {
			t.Errorf("%d codes left after the repeated reads, want 1", left)
		}
	})
}

// An App Store reader whose offer was issued while the pool was empty takes
// a code at its next read once the pool has one: an offer issued without a
// code used to keep none for its whole life.
func TestAppStoreReaderTakesAppleCodeOnceThePoolRefills(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		setAppleOfferReaderTestConfig(t)

		networkId, userId := newAppleOfferReaderNetwork(ctx)
		ios := appleOfferReaderSession(t, ctx, networkId, userId, appleOfferReaderIos, "")
		if offer := issueAppleOfferReaderOffer(t, ios); offer.AppleOfferCode != "" {
			t.Fatalf("an empty pool gave code %q", offer.AppleOfferCode)
		}

		loadAppleOfferReaderTestCodes(t, ctx, "SYNTHETIC-REFILL", 1)
		if code := readAppleOfferReaderPlan(t, ios).AppleOfferCode; !strings.HasPrefix(code, "SYNTHETIC-REFILL-") {
			t.Errorf("the iOS app's plan shows no App Store code after the refill (%q)", code)
		}
	})
}

// Only an active offer takes a code at a read: an expired or a redeemed one
// never draws.
func TestInactiveOfferTakesNoAppleCodeAtARead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		setAppleOfferReaderTestConfig(t)
		loadAppleOfferReaderTestCodes(t, ctx, "SYNTHETIC-INACTIVE", 1)

		// offers issued without a code, as the email offer is
		issueWithoutCode := func(networkId server.Id, validity time.Duration) {
			if _, created, err := model.IssueOnboardingOffer(ctx, &model.IssueOnboardingOfferArgs{
				NetworkId:  networkId,
				IssuedBy:   model.OnboardingOfferIssuedByEmail,
				Surface:    model.OnboardingOfferSurfaceEmailLink,
				Tier:       "standard",
				PercentOff: 25,
				MonthsFree: 3,
				Validity:   validity,
			}); err != nil || !created {
				t.Fatalf("could not issue the offer: created %t, err %v", created, err)
			}
		}

		expiredNetworkId, expiredUserId := newAppleOfferReaderNetwork(ctx)
		// expired an hour ago
		issueWithoutCode(expiredNetworkId, -time.Hour)
		expired := appleOfferReaderSession(t, ctx, expiredNetworkId, expiredUserId, appleOfferReaderIos, "")
		if offer := readAppleOfferReaderPlan(t, expired); offer.AppleOfferCode != "" || offer.State != model.OnboardingOfferStateExpired {
			t.Errorf("an expired offer read as %s with code %q", offer.State, offer.AppleOfferCode)
		}

		redeemedNetworkId, redeemedUserId := newAppleOfferReaderNetwork(ctx)
		issueWithoutCode(redeemedNetworkId, model.Onboarding().OfferValidity())
		if !model.RedeemOnboardingOffer(ctx, redeemedNetworkId, model.OnboardingStoreStripe, server.NowUtc()) {
			t.Fatal("could not redeem the offer")
		}
		redeemed := appleOfferReaderSession(t, ctx, redeemedNetworkId, redeemedUserId, appleOfferReaderIos, "")
		if offer := readAppleOfferReaderPlan(t, redeemed); offer.AppleOfferCode != "" || offer.State != model.OnboardingOfferStateRedeemed {
			t.Errorf("a redeemed offer read as %s with code %q", offer.State, offer.AppleOfferCode)
		}

		if left := appleOfferReaderPoolLeft(ctx); left != 1 {
			t.Errorf("inactive offers drew %d of the pool's 1", 1-left)
		}
	})
}
