// Config-version ordering and the hand-back rule, without a database.
package work

import (
	"testing"
	"time"
)

// Replaces this process's config version for one test.
func withTestConfigVersion(t testing.TB, version string) {
	t.Helper()
	previous := currentConfigGeneration
	currentConfigGeneration = func() string { return version }
	t.Cleanup(func() { currentConfigGeneration = previous })
}

// Release order first, then build metadata within a release, as warp picks
// the latest config; unknown versions have no order in either direction. A
// docker tag form, with the build code in the prerelease, has no order within
// its release, and a '+' form does not outrank it there.
func TestProviderEgressProbeGenerationOrdersLikeWarp(t *testing.T) {
	cases := []struct {
		candidate string
		reference string
		want      bool
	}{
		{candidate: testNewerConfigVersion, reference: testOlderConfigVersion, want: true},
		{candidate: testOlderConfigVersion, reference: testNewerConfigVersion, want: false},
		{candidate: testNewerConfigVersion, reference: testNewerConfigVersion, want: false},
		{candidate: "2026.10.13-synthetic+1000000201", reference: "2026.10.13-synthetic+1000000200", want: true},
		{candidate: "2026.10.13+1000000201", reference: "2026.10.13-synthetic+1000000200", want: true},
		{candidate: "2026.10.9-synthetic+1000000300", reference: "2026.10.13-synthetic+1000000200", want: false},
		{candidate: "2026.11.1-synthetic+1", reference: "2026.10.13-synthetic+1000000200", want: true},
		{candidate: "", reference: testOlderConfigVersion, want: false},
		{candidate: testNewerConfigVersion, reference: "", want: false},
		{candidate: "not-a-version", reference: testOlderConfigVersion, want: false},
		{candidate: testNewerConfigVersion, reference: "0.0.0-local", want: true},
		{candidate: "2026.10.13-synthetic-1000000200", reference: "2026.10.13-synthetic-1000000100", want: false},
		{candidate: "2026.10.13-synthetic-1000000100", reference: "2026.10.13-synthetic-1000000200", want: false},
		{candidate: "2026.10.13-synthetic+1000000100", reference: "2026.10.13-synthetic-1000000200", want: false},
		{candidate: "2026.10.13-synthetic-1000000200", reference: "2026.10.13-synthetic+1000000100", want: false},
		{candidate: "2026.10.13-synthetic-1000000100", reference: "2026.10.12-synthetic+1000000000", want: true},
		{candidate: "2026.10.12-synthetic+1000000000", reference: "2026.10.13-synthetic-1000000100", want: false},
	}
	for _, c := range cases {
		if got := configGenerationNewer(c.candidate, c.reference); got != c.want {
			t.Errorf("newer(%q, %q) = %t, want %t", c.candidate, c.reference, got, c.want)
		}
	}
}

// Only a newer config's geometry that this instance would retire or rewrite
// is foreign to it. The same geometry from a newer config runs here unless
// this config disables probing, and older or unstamped snapshots keep their
// convergence.
func TestProviderEgressProbeNewerForeignGeometry(t *testing.T) {
	withTestConfigVersion(t, testOlderConfigVersion)
	local := testUrlGeometrySettings(4, 8)
	disabled := testUrlGeometrySettings(4, 8)
	disabled.Enabled = false
	newer := testUrlGeometrySettings(8, 64)
	stamp := func(settings providerEgressProbeSettings, shardIndex int, version string) *ProviderEgressProbeArgs {
		args := providerEgressProbeArgs(settings, shardIndex)
		args.ConfigStamp = ConfigStamp{ConfigGeneration: version, ConfigWrittenAt: time.Date(2026, 10, 13, 19, 52, 0, 0, time.UTC)}
		return args
	}
	cases := []struct {
		name     string
		args     *ProviderEgressProbeArgs
		settings providerEgressProbeSettings
		want     bool
	}{
		{name: "newer shard beyond the local count", args: stamp(newer, 5, testNewerConfigVersion), settings: local, want: true},
		{name: "newer shard within the local count", args: stamp(newer, 1, testNewerConfigVersion), settings: local, want: true},
		{name: "newer geometry on a disabled older config", args: stamp(newer, 1, testNewerConfigVersion), settings: disabled, want: true},
		{name: "same geometry from a newer config on a disabled older config", args: stamp(local, 1, testNewerConfigVersion), settings: disabled, want: true},
		{name: "same geometry from a newer config", args: stamp(local, 1, testNewerConfigVersion), settings: local, want: false},
		{name: "older snapshot", args: stamp(newer, 1, "2026.10.11-synthetic+1000000000"), settings: local, want: false},
		{name: "unstamped snapshot", args: stamp(newer, 5, ""), settings: local, want: false},
		{name: "no snapshot", args: nil, settings: local, want: false},
	}
	for _, c := range cases {
		if got := providerEgressProbeNewerForeignGeometry(c.args, c.settings); got != c.want {
			t.Errorf("%s: foreign %t, want %t", c.name, got, c.want)
		}
	}
	withTestConfigVersion(t, "")
	if providerEgressProbeNewerForeignGeometry(stamp(newer, 5, testNewerConfigVersion), local) {
		t.Error("a process without a config version ordered a snapshot")
	}
}

// A newer stamp stays live for twice the newer config's own max time and idle
// delay, within the minimum and maximum. A fixed 30 minutes expired the stamps
// of a config whose passes run longer while its instances were still running.
func TestProviderEgressProbeStampLiveFollowsTheNewerArguments(t *testing.T) {
	now := time.Date(2026, 10, 13, 19, 52, 0, 0, time.UTC)
	withTestConfigVersion(t, testOlderConfigVersion)
	stamp := func(maxTimeSeconds int, version string, age time.Duration) *ProviderEgressProbeArgs {
		settings := testUrlGeometrySettings(8, 64)
		settings.MaxTimeSeconds = maxTimeSeconds
		args := providerEgressProbeArgs(settings, 3)
		args.ConfigGeneration = version
		if 0 <= age {
			args.ConfigWrittenAt = now.Add(-age)
		}
		return args
	}
	// the defaults: 900 s max time and 5 s idle delay
	defaultTimeout := 2 * 905 * time.Second
	cases := []struct {
		name string
		args *ProviderEgressProbeArgs
		want bool
	}{
		{name: "fresh", args: stamp(900, testNewerConfigVersion, 0), want: true},
		{name: "just inside the default timeout", args: stamp(900, testNewerConfigVersion, defaultTimeout-time.Second), want: true},
		{name: "at the default timeout", args: stamp(900, testNewerConfigVersion, defaultTimeout), want: false},
		{name: "an hour-long pass, written 45 minutes ago", args: stamp(3600, testNewerConfigVersion, 45*time.Minute), want: true},
		{name: "at an hour-long pass's timeout", args: stamp(3600, testNewerConfigVersion, 2*3605*time.Second), want: false},
		{name: "a minute-long pass keeps the minimum", args: stamp(60, testNewerConfigVersion, 29*time.Minute), want: true},
		{name: "at the minimum", args: stamp(60, testNewerConfigVersion, 30*time.Minute), want: false},
		{name: "a day-long pass keeps the maximum", args: stamp(86400, testNewerConfigVersion, 23*time.Hour), want: true},
		{name: "at the maximum", args: stamp(86400, testNewerConfigVersion, 24*time.Hour), want: false},
		{name: "no write time", args: stamp(900, testNewerConfigVersion, -1), want: false},
		{name: "older generation", args: stamp(900, "2026.10.11-synthetic+1000000000", 0), want: false},
		{name: "no snapshot", args: nil, want: false},
	}
	for _, c := range cases {
		if got := providerEgressProbeStampLive(c.args, now); got != c.want {
			t.Errorf("%s: live %t, want %t", c.name, got, c.want)
		}
	}
}

// Stamping copies the snapshot and does not change its pass geometry.
func TestProviderEgressProbeStampIsNotPassGeometry(t *testing.T) {
	withTestConfigVersion(t, testNewerConfigVersion)
	settings := testUrlGeometrySettings(8, 64)
	args := providerEgressProbeArgs(settings, 3)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.FixedZone("synthetic", -5*60*60))
	stamped := stampProviderEgressProbeArgs(args, now)
	if args.ConfigGeneration != "" || !args.ConfigWrittenAt.IsZero() {
		t.Fatal("stamping mutated the source snapshot")
	}
	if stamped.ConfigGeneration != testNewerConfigVersion || !stamped.ConfigWrittenAt.Equal(now) || stamped.ConfigWrittenAt.Location() != time.UTC {
		t.Fatalf("stamp = %q at %s", stamped.ConfigGeneration, stamped.ConfigWrittenAt)
	}
	if !providerEgressProbeArgsMatchSettings(stamped, settings) {
		t.Fatal("a stamped snapshot no longer matches its own settings")
	}
}
