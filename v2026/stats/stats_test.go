package stats

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/stats/sample"
)

// end to end: enable stats against a temp site home, append samples, close,
// then read the segment back and confirm the frames round-trip.
func TestStatsAppendRoundTrip(t *testing.T) {
	siteHome := t.TempDir()
	t.Setenv("WARP_SITE_HOME", siteHome)
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_SERVICE", "teststats")
	t.Setenv("WARP_HOST", "testhost")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newStats(ctx, &Config{MaxSegmentAge: time.Hour})
	connect.AssertEqual(t, s.Enabled(), true)

	const count = 5
	for i := 0; i < count; i += 1 {
		s.Append("findproviders2", &sample.FindProviders2Sample{
			TimeUnixMilli:     uint64(i),
			CallerCountryCode: "zz",
			RankMode:          "quality",
			PoolCount:         int32(i),
		})
	}

	s.Close()

	// find the one segment written under <root>/findproviders2/
	streamDir := filepath.Join(s.root, "findproviders2")
	entries, err := os.ReadDir(streamDir)
	connect.AssertEqual(t, err, nil)

	segments := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) == ".zst" { // .pb.zst final, never .partial
			segments = append(segments, filepath.Join(streamDir, name))
		}
	}
	connect.AssertEqual(t, len(segments), 1)

	got := 0
	err = ReadSegment(segments[0], func(frame []byte) error {
		var s sample.FindProviders2Sample
		if err := proto.Unmarshal(frame, &s); err != nil {
			return err
		}
		connect.AssertEqual(t, s.CallerCountryCode, "zz")
		got += 1
		return nil
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, got, count)
}

// a disabled Stats (missing site home) must silently no-op.
func TestStatsDisabledWhenNoSiteHome(t *testing.T) {
	t.Setenv("WARP_SITE_HOME", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("WARP_ENV", "local")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newStats(ctx, nil)
	connect.AssertEqual(t, s.Enabled(), false)
	// must not panic or write anything
	s.Append("findproviders2", &sample.FindProviders2Sample{})
	s.Close()
}

// non-local env with no salt disables (fail-safe), so raw ids are never
// written to a would-be-published stream.
func TestStatsDisabledWhenNoSaltNonLocal(t *testing.T) {
	siteHome := t.TempDir()
	t.Setenv("WARP_SITE_HOME", siteHome)
	t.Setenv("WARP_ENV", "testenv-no-salt")
	t.Setenv("WARP_SERVICE", "teststats")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newStats(ctx, nil)
	connect.AssertEqual(t, s.Enabled(), false)
}

// anonymization: raw in local, keyed+stable otherwise.
func TestAnonymizer(t *testing.T) {
	id := server.NewId()

	raw, ok := newAnonymizer(true, nil)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, raw.anonymize(id), id.Bytes())

	_, ok = newAnonymizer(false, nil)
	connect.AssertEqual(t, ok, false)

	keyed, ok := newAnonymizer(false, []byte("salt-value"))
	connect.AssertEqual(t, ok, true)
	a := keyed.anonymize(id)
	b := keyed.anonymize(id)
	connect.AssertEqual(t, len(a), 16)
	connect.AssertEqual(t, a, b) // stable
	connect.AssertNotEqual(t, a, id.Bytes())

	// a different salt yields a different pseudonym
	other, _ := newAnonymizer(false, []byte("other-salt"))
	connect.AssertNotEqual(t, other.anonymize(id), a)
}
