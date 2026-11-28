package model

// The initial performance profile of a hosted device of a network on the free
// plan (FreeProxyPerformanceProfile): a stored fixed window is held to the
// free device's whole window, and every other profile is the stored one.

import (
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// A fixed window past the bound is held to it, as a copy that connect accepts
// and that keeps the window type and the direct and post-quantum choices. No
// profile, auto, a fixed window within the bound and a fixed window with no
// size are the stored profile itself, and a nonpositive bound bounds nothing.
func TestFreeProxyPerformanceProfileHoldsAFixedWindowToTheFreeWindow(t *testing.T) {
	const windowSizeMax = 3
	sized := func(windowType sdk.WindowType, windowSizeMin int, windowSizeMax int, windowSizeHardMax int) *sdk.PerformanceProfile {
		return &sdk.PerformanceProfile{
			WindowType: windowType,
			WindowSize: &sdk.WindowSizeSettings{
				WindowSizeMin:            windowSizeMin,
				WindowSizeMax:            windowSizeMax,
				WindowSizeHardMax:        windowSizeHardMax,
				WindowSizeReconnectScale: 1,
			},
			PostQuantumEncryption: true,
		}
	}

	for _, c := range []struct {
		name               string
		performanceProfile *sdk.PerformanceProfile
	}{
		{name: "auto", performanceProfile: sized(sdk.WindowTypeAuto, 8, 8, 8)},
		{name: "a fixed ip", performanceProfile: sized(sdk.WindowTypeQuality, 1, 1, 1)},
		{name: "a fixed window of the bound", performanceProfile: sized(sdk.WindowTypeSpeed, 3, 3, 3)},
		{name: "a fixed window with no size", performanceProfile: &sdk.PerformanceProfile{WindowType: sdk.WindowTypeQuality}},
	} {
		if got := FreeProxyPerformanceProfile(c.performanceProfile, windowSizeMax); got != c.performanceProfile {
			t.Errorf("%s: got %+v, want the stored profile", c.name, got)
		}
	}
	if got := FreeProxyPerformanceProfile(nil, windowSizeMax); got != nil {
		t.Errorf("no profile read back as %+v", got)
	}
	large := sized(sdk.WindowTypeQuality, 8, 8, 8)
	if got := FreeProxyPerformanceProfile(large, 0); got != large {
		t.Errorf("a bound of 0 changed the profile to %+v", got)
	}

	for _, c := range []struct {
		name               string
		performanceProfile *sdk.PerformanceProfile
		want               sdk.WindowSizeSettings
	}{
		{
			name:               "a fixed quality window of 8",
			performanceProfile: sized(sdk.WindowTypeQuality, 8, 8, 8),
			want:               sdk.WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 3, WindowSizeHardMax: 3, WindowSizeReconnectScale: 1},
		},
		{
			name:               "a speed window from 2 to 6 with no hard max",
			performanceProfile: sized(sdk.WindowTypeSpeed, 2, 6, 0),
			want:               sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 3, WindowSizeHardMax: 3, WindowSizeReconnectScale: 1},
		},
		{
			name:               "a window of 3 with a hard max of 10",
			performanceProfile: sized(sdk.WindowTypeQuality, 3, 3, 10),
			want:               sdk.WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 3, WindowSizeHardMax: 3, WindowSizeReconnectScale: 1},
		},
	} {
		stored := *c.performanceProfile.WindowSize
		got := FreeProxyPerformanceProfile(c.performanceProfile, windowSizeMax)
		if got == c.performanceProfile || got.WindowSize == c.performanceProfile.WindowSize {
			t.Fatalf("%s: the bound wrote the stored profile", c.name)
		}
		if *c.performanceProfile.WindowSize != stored {
			t.Errorf("%s: the stored size changed to %+v", c.name, *c.performanceProfile.WindowSize)
		}
		if got.WindowType != c.performanceProfile.WindowType || !got.PostQuantumEncryption || got.AllowDirect {
			t.Errorf("%s: the bound changed the profile's type or choices: %+v", c.name, got)
		}
		if *got.WindowSize != c.want {
			t.Errorf("%s: bounded to %+v, want %+v", c.name, *got.WindowSize, c.want)
		}
		if err := validatePerformanceProfile(got); err != nil {
			t.Errorf("%s: connect refuses the bounded profile: %s", c.name, err)
		}
	}
}
