// URL pass placement follows the configured geometry without entering the
// durable pass snapshot.
package work

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Production URL settings with one geometry and instance concurrency.
func testUrlPlacementSettings(shardCount int, concurrency int, instanceConcurrency int) providerEgressProbeSettings {
	settings := defaultProviderEgressProbeSettings("example.test")
	settings.ShardCount = shardCount
	settings.UrlProbe.Limit, settings.UrlProbe.Concurrency = concurrency, concurrency
	settings.InstanceUrlConcurrency = instanceConcurrency
	return settings
}

// Whole passes fit in the instance concurrency, bounded by the shard count;
// a pass that alone exceeds it still runs.
func TestUrlProbeInstancePassLimitFitsInstanceConcurrency(t *testing.T) {
	cases := []struct {
		shardCount          int
		concurrency         int
		instanceConcurrency int
		want                int
	}{
		{shardCount: 8, concurrency: 64, instanceConcurrency: 128, want: 2},
		{shardCount: 8, concurrency: 64, instanceConcurrency: 191, want: 2},
		{shardCount: 8, concurrency: 64, instanceConcurrency: 192, want: 3},
		{shardCount: 6, concurrency: 96, instanceConcurrency: 128, want: 1},
		{shardCount: 4, concurrency: 8, instanceConcurrency: 128, want: 4},
		{shardCount: 1, concurrency: 8, instanceConcurrency: 128, want: 1},
		{shardCount: 2, concurrency: 200, instanceConcurrency: 128, want: 1},
	}
	for _, c := range cases {
		settings := testUrlPlacementSettings(c.shardCount, c.concurrency, c.instanceConcurrency)
		if err := settings.validate(); err != nil {
			t.Fatalf("fixture %d x %d is invalid: %v", c.shardCount, c.concurrency, err)
		}
		if got := providerUrlProbeInstancePassLimit(settings); got != c.want {
			t.Errorf("%d shards x %d workers in %d: limit %d, want %d", c.shardCount, c.concurrency, c.instanceConcurrency, got, c.want)
		}
	}
}

// Disabled and legacy-only settings keep one pass per instance.
func TestUrlProbeInstancePassLimitKeepsOneWithoutUrlGeometry(t *testing.T) {
	disabled := testUrlPlacementSettings(8, 8, 128)
	disabled.Enabled = false
	legacy := testUrlPlacementSettings(8, 8, 128)
	legacy.UrlProbe = nil
	for _, settings := range []providerEgressProbeSettings{disabled, legacy} {
		if got := providerUrlProbeInstancePassLimit(settings); got != 1 {
			t.Errorf("settings without URL geometry allowed %d passes", got)
		}
	}
}

// The default is two historical 64-worker passes, and a nonpositive value is
// a configuration error rather than an unlimited instance.
func TestUrlProbeInstanceConcurrencyDefaultAndValidation(t *testing.T) {
	settings := defaultProviderEgressProbeSettings("example.test")
	if settings.InstanceUrlConcurrency != 128 || settings.validate() != nil {
		t.Fatalf("default instance concurrency %d is not a valid 128", settings.InstanceUrlConcurrency)
	}
	for _, value := range []int{0, -1} {
		invalid := testUrlPlacementSettings(8, 64, value)
		if invalid.validate() == nil {
			t.Errorf("instance concurrency %d validated", value)
		}
	}
	legacy := testProviderEgressProbeSettings(8)
	legacy.InstanceUrlConcurrency = 0
	if err := legacy.validate(); err != nil {
		t.Fatalf("legacy settings now require URL placement: %v", err)
	}
}

// Placement is not pass geometry: changing it must not retire pending
// shard passes as stale snapshots.
func TestUrlProbeInstanceConcurrencyIsNotPassGeometry(t *testing.T) {
	settings := testUrlPlacementSettings(8, 64, 128)
	args := providerEgressProbeArgs(settings, 3)
	settings.InstanceUrlConcurrency = 256
	if !providerEgressProbeArgsMatchSettings(args, settings) {
		t.Fatal("instance concurrency made a pending pass snapshot stale")
	}
}

// The exported limit reads the current settings, publishes its value, and
// keeps one pass when settings cannot be read.
func TestUrlProbeInstancePassLimitPublishesSettingsValue(t *testing.T) {
	withProviderEgressProbeSettings(t, testUrlPlacementSettings(8, 64, 128))
	if got := ProviderEgressProbeInstancePassLimit(); got != 2 || testutil.ToFloat64(urlProbeInstancePassLimit) != 2 {
		t.Fatalf("limit %d published %v, want 2", got, testutil.ToFloat64(urlProbeInstancePassLimit))
	}
	previous := getProviderEgressProbeSettings
	getProviderEgressProbeSettings = func() (providerEgressProbeSettings, error) {
		return providerEgressProbeSettings{}, errors.New("synthetic settings failure")
	}
	t.Cleanup(func() { getProviderEgressProbeSettings = previous })
	if got := ProviderEgressProbeInstancePassLimit(); got != 1 || testutil.ToFloat64(urlProbeInstancePassLimit) != 1 {
		t.Fatalf("unreadable settings gave limit %d, published %v", got, testutil.ToFloat64(urlProbeInstancePassLimit))
	}
}
