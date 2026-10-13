// Config-version ordering and the hand-back rule, without a database.
package work

import (
	"testing"
	"time"
)

// Replaces this process's config version for one test.
func withTestConfigVersion(t testing.TB, version string) {
	t.Helper()
	previous := providerEgressProbeConfigVersion
	providerEgressProbeConfigVersion = func() string { return version }
	t.Cleanup(func() { providerEgressProbeConfigVersion = previous })
}

// Release order first, then build metadata within a release, as warp picks
// the latest config; unknown versions have no order in either direction.
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
	}
	for _, c := range cases {
		if got := providerEgressProbeGenerationNewer(c.candidate, c.reference); got != c.want {
			t.Errorf("newer(%q, %q) = %t, want %t", c.candidate, c.reference, got, c.want)
		}
	}
}

// Only a newer, live geometry this instance would retire or rewrite is handed
// back. The same geometry from a newer config runs, and older, unstamped or
// withdrawn snapshots keep their convergence.
func TestProviderEgressProbeHandsBackOnlyNewerDifferentGeometry(t *testing.T) {
	now := time.Date(2026, 10, 13, 19, 52, 0, 0, time.UTC)
	withTestConfigVersion(t, testOlderConfigVersion)
	local := testUrlGeometrySettings(4, 8)
	disabled := testUrlGeometrySettings(4, 8)
	disabled.Enabled = false
	newer := testUrlGeometrySettings(8, 64)
	stamp := func(settings providerEgressProbeSettings, shardIndex int, version string, writtenAt time.Time) *ProviderEgressProbeArgs {
		args := providerEgressProbeArgs(settings, shardIndex)
		args.GeometryGeneration, args.GeometryWrittenAt = version, writtenAt
		return args
	}
	cases := []struct {
		name     string
		args     *ProviderEgressProbeArgs
		settings providerEgressProbeSettings
		want     bool
	}{
		{name: "newer shard beyond the local count", args: stamp(newer, 5, testNewerConfigVersion, now), settings: local, want: true},
		{name: "newer shard within the local count", args: stamp(newer, 1, testNewerConfigVersion, now), settings: local, want: true},
		{name: "newer geometry on a disabled older config", args: stamp(newer, 1, testNewerConfigVersion, now), settings: disabled, want: true},
		{name: "same geometry from a newer config on a disabled older config", args: stamp(local, 1, testNewerConfigVersion, now), settings: disabled, want: true},
		{name: "same geometry from a newer config", args: stamp(local, 1, testNewerConfigVersion, now), settings: local, want: false},
		{name: "older snapshot", args: stamp(newer, 1, "2026.10.11-synthetic+1000000000", now), settings: local, want: false},
		{name: "unstamped snapshot", args: stamp(newer, 5, "", time.Time{}), settings: local, want: false},
		{name: "newer snapshot without a write time", args: stamp(newer, 5, testNewerConfigVersion, time.Time{}), settings: local, want: false},
		{name: "newer snapshot nobody rewrote", args: stamp(newer, 5, testNewerConfigVersion, now.Add(-providerEgressProbeNewerGenerationTimeout)), settings: local, want: false},
		{name: "newer snapshot just inside the timeout", args: stamp(newer, 5, testNewerConfigVersion, now.Add(-providerEgressProbeNewerGenerationTimeout+time.Second)), settings: local, want: true},
		{name: "no snapshot", args: nil, settings: local, want: false},
	}
	for _, c := range cases {
		if got := providerEgressProbeHandsBack(c.args, c.settings, now); got != c.want {
			t.Errorf("%s: hands back %t, want %t", c.name, got, c.want)
		}
	}
	withTestConfigVersion(t, "")
	if providerEgressProbeHandsBack(stamp(newer, 5, testNewerConfigVersion, now), local, now) {
		t.Error("a process without a config version ordered a snapshot")
	}
}

// Stamping copies the snapshot and does not change its pass geometry.
func TestProviderEgressProbeStampIsNotPassGeometry(t *testing.T) {
	withTestConfigVersion(t, testNewerConfigVersion)
	settings := testUrlGeometrySettings(8, 64)
	args := providerEgressProbeArgs(settings, 3)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.FixedZone("synthetic", -5*60*60))
	stamped := stampProviderEgressProbeArgs(args, now)
	if args.GeometryGeneration != "" || !args.GeometryWrittenAt.IsZero() {
		t.Fatal("stamping mutated the source snapshot")
	}
	if stamped.GeometryGeneration != testNewerConfigVersion || !stamped.GeometryWrittenAt.Equal(now) || stamped.GeometryWrittenAt.Location() != time.UTC {
		t.Fatalf("stamp = %q at %s", stamped.GeometryGeneration, stamped.GeometryWrittenAt)
	}
	if !providerEgressProbeArgsMatchSettings(stamped, settings) {
		t.Fatal("a stamped snapshot no longer matches its own settings")
	}
}
