// The Fast application stack must derive readiness from its real exchange
// environment before any TUN, provider, socket or TestEnv fixture is created.
package perfvar

import (
	"reflect"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

// The canonical fast path retains its original budget; asymmetric access and
// internal links prove setup cannot replace the real environment with defaults.
func TestSyntheticFastApplicationSettingsUseActualExchangeProfile(t *testing.T) {
	profile := initialNetworkProfiles(7919)["clean-lan"]
	profile.Forward = newLinkProfile(2_000_000_000, 20*time.Millisecond, 0, 0, 20*time.Millisecond)
	profile.Forward.QueueByteCount = min(2*1024*1024, profile.Forward.QueueByteCount)
	profile.Reverse = profile.Forward
	providerProfile := initialNetworkProfiles(7927)["clean-lan"]
	providerProfile.Forward = newLinkProfile(2_000_000_000, 0, 0, 0, 20*time.Millisecond)
	providerProfile.Forward.QueueByteCount = min(2*1024*1024, providerProfile.Forward.QueueByteCount)
	providerProfile.Reverse = providerProfile.Forward
	resources := defaultTunResourceProfile()
	canonicalTimeout := time.Minute
	if perfvarRaceEnabled {
		canonicalTimeout = 4 * time.Minute
	}
	for _, asymmetric := range []bool{false, true} {
		environment := &routeEnvironment{profile: profile,
			deviceAccessProfile: profile, providerAccessProfile: providerProfile}
		wantTimeout := canonicalTimeout
		if asymmetric {
			environment.deviceAccessProfile.Forward.RateBitsPerSecond = 32768
			environment.deviceAccessProfile.Forward.BaseDelay = 90 * time.Millisecond
			environment.deviceAccessProfile.Forward.ProcessingDelay = 10 * time.Millisecond
			environment.deviceAccessProfile.Reverse.BaseDelay = 180 * time.Millisecond
			environment.deviceAccessProfile.Reverse.ProcessingDelay = 20 * time.Millisecond
			environment.providerAccessProfile.Forward.BaseDelay = 270 * time.Millisecond
			environment.providerAccessProfile.Forward.ProcessingDelay = 30 * time.Millisecond
			environment.providerAccessProfile.Reverse.BaseDelay = 360 * time.Millisecond
			environment.providerAccessProfile.Reverse.ProcessingDelay = 40 * time.Millisecond
			internal := providerProfile
			internal.Forward.BaseDelay, internal.Forward.ProcessingDelay = 450*time.Millisecond, 50*time.Millisecond
			internal.Reverse.BaseDelay, internal.Reverse.ProcessingDelay = 540*time.Millisecond, 60*time.Millisecond
			environment.internalExchangeProfile = &internal
			// Six modeled links contribute 2.1s RTT: 126s directional. The
			// 16KiB upload at 32768bit/s takes 4s: 240s. Their sum is 366s,
			// above both normal and race floors, without waiting or dialing.
			wantTimeout = 366 * time.Second
		}
		fixture := &syntheticFastNativeFixture{ctx: t.Context(), environment: environment, profile: profile}
		settings := fixture.applicationTunSettings(resources)
		want := clientconnect.DefaultTunSettingsWithBufferSize(resources.ChannelSize)
		want.Mtu, want.DialTimeout = 1280, wantTimeout
		if perfvarRaceEnabled {
			want.DialRaceTimeout = wantTimeout / time.Duration(want.DialRace)
		}
		if !reflect.DeepEqual(settings, want) {
			t.Fatalf("asymmetric=%t application settings=%+v, want unchanged resources and profile budget=%+v", asymmetric, settings, want)
		}
	}
}
