package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Main's score exporter commits the native census 186-374 seconds after its
// source completes (arindbctl/ARINDB.md), and the observer's snapshot exists
// only after that commit. These cases inject the clock and the committed
// census: no Redis, no sleeps and no dependence on the wall clock.
var shadowScoreTestCompletedAt = time.Date(2026, 10, 6, 3, 47, 15, 684629000, time.UTC)

type shadowScoreTestPublication struct {
	capture  *ArinShadowScoreCapture
	snapshot *ArinShadowScoreSnapshot
	census   *ClientScoreNativeCensus
	now      time.Time
}

// Publishes one observed provider through the real observer path. tail is the
// export interval from source completion to the committed publication.
func shadowScoreTestPublish(t testing.TB, c *ArinShadowScoreCapture, a *arinShadowScoreAttempt, tail time.Duration) *shadowScoreTestPublication {
	t.Helper()
	p := &shadowScoreTestPublication{capture: c}
	c.clock = func() time.Time { return p.now }
	c.latest = func(context.Context) (*ClientScoreNativeCensus, error) { return p.census, nil }
	country := "us"
	passing := true
	a.observe(shadowScoreTestRow(server.NewId()), &country, &providerEgressFacts{egressQuality: &passing}, true)
	p.census = shadowScoreTestCensus(shadowScoreTestCompletedAt.Add(-112*time.Second), 0, 0, 0)
	p.census.SourceCompletedAt = shadowScoreTestCompletedAt
	p.census.PublishedAt = shadowScoreTestCompletedAt.Add(tail)
	p.now = p.census.PublishedAt
	a.publish(p.census)
	if p.snapshot = c.Snapshot(); p.snapshot == nil {
		t.Fatal("committed publication was not observed")
	}
	return p
}

func TestArinShadowScoreLeaseWindowStartsAtCommittedPublication(t *testing.T) {
	for _, test := range []struct {
		name          string
		tail, elapsed time.Duration // source completion to publication, publication to lease request
		leased        bool
	}{
		// The defect: every documented Main publication was born unleasable.
		{"main shortest export tail at publication", 186 * time.Second, 0, true},
		{"main longest export tail at publication", 374 * time.Second, 0, true},
		{"main longest export tail at window end", 374 * time.Second, server.ArinShadowCaptureMaxAge, true},
		{"source exactly at the census limit", 810 * time.Second, server.ArinShadowCaptureMaxAge, true},
		// Controls: the original fresh case, a stale publication and a stale
		// source whose publication alone is fresh all keep their outcomes.
		{"immediate publication", time.Millisecond, 0, true},
		{"stale publication", 186 * time.Second, server.ArinShadowCaptureMaxAge + time.Microsecond, false},
		{"source beyond the census limit with a fresh publication", 850 * time.Second, 50*time.Second + time.Microsecond, false},
		{"source beyond the census limit when published", 900*time.Second + time.Microsecond, 0, false},
		{"clock behind the publication", 186 * time.Second, -time.Microsecond, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, a := shadowScoreTestSource(t, 4)
			p := shadowScoreTestPublish(t, c, a, test.tail)
			p.now = p.census.PublishedAt.Add(test.elapsed)
			ctx := context.Background()
			lease, err := p.snapshot.AcquireCaptureLease(ctx)
			if (err == nil) != test.leased {
				t.Fatalf("lease granted %t, want %t (source age %s, publication age %s)", err == nil, test.leased, p.now.Sub(p.census.SourceCompletedAt), test.elapsed)
			}
			if p.snapshot.Validate(ctx) != test.leased {
				t.Fatal("validation disagrees with lease admission")
			}
			if lease == nil {
				return
			}
			defer lease.Close()
			if !lease.startedAt.Equal(p.now) || !lease.expiresAt.Equal(p.now.Add(server.ArinShadowCaptureMaxAge)) {
				t.Fatal("lease is not the bounded window from its own start")
			}
			if pinned, ok := lease.current(ctx); !ok || pinned != p.snapshot {
				t.Fatal("granted lease does not pin the committed generation")
			}
			p.now = lease.expiresAt
			if _, ok := lease.current(ctx); ok {
				t.Fatal("lease outlived its bounded window")
			}
		})
	}
}

// publish never admits these, but Validate must not rely on that.
func TestArinShadowScoreSnapshotRefusesMissingInvertedOrReplacedPublication(t *testing.T) {
	ctx := context.Background()
	c, a := shadowScoreTestSource(t, 4)
	p := shadowScoreTestPublish(t, c, a, 186*time.Second)
	if !p.snapshot.Validate(ctx) {
		t.Fatal("fresh committed publication refused")
	}
	committed := p.snapshot.publishedAt
	for _, publishedAt := range []time.Time{{}, p.census.SourceCompletedAt.Add(-time.Microsecond)} {
		p.snapshot.publishedAt = publishedAt
		p.now = p.census.SourceCompletedAt
		if p.snapshot.Validate(ctx) {
			t.Fatal("snapshot without a committed publication clock validated")
		}
	}
	p.snapshot.publishedAt, p.now = committed, committed
	// A republished census for the same source is a different generation.
	republished := *p.census
	republished.PublicationId = server.NewId().String()
	p.census = &republished
	if _, err := p.snapshot.AcquireCaptureLease(ctx); err == nil {
		t.Fatal("superseded generation leased")
	}
}

// The operator selects the publisher with native_status immediately before
// native_acquire; a Main-shaped publication must be selectable and leasable.
func TestArinShadowNativeRPCServesMainShapedPublication(t *testing.T) {
	ctx := context.Background()
	handler, closeOwner, err := NewArinShadowNativeRPC(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOwner()
	p := shadowScoreTestPublish(t, currentArinShadowScoreCapture.Load(), beginArinShadowScoreCapture(), 374*time.Second)
	p.now = p.census.PublishedAt.Add(30 * time.Second)
	value, err := handler(ctx, "native_status", json.RawMessage(`{}`))
	if status, ok := value.(ArinShadowNativeStatus); err != nil || !ok || !status.Available || !status.SourceCompletedAt.Equal(p.census.SourceCompletedAt) {
		t.Fatal("Main-shaped publication unavailable to the operator", err)
	}
	value, err = handler(ctx, "native_acquire", json.RawMessage(`{}`))
	lease, ok := value.(ArinShadowNativeLeaseInfo)
	if err != nil || !ok || !lease.PublishedAt.Equal(p.census.PublishedAt) || !lease.SourceCompletedAt.Equal(p.census.SourceCompletedAt) || !lease.ExpiresAt.Equal(p.now.Add(server.ArinShadowCaptureMaxAge)) {
		t.Fatal("Main-shaped publication not leased", err)
	}
	request, _ := json.Marshal(struct {
		Token server.Id `json:"token"`
	}{lease.Token})
	if _, err := handler(ctx, "native_release", request); err != nil {
		t.Fatal(err)
	}
	p.now = p.census.PublishedAt.Add(server.ArinShadowCaptureMaxAge + time.Microsecond)
	if value, err := handler(ctx, "native_status", json.RawMessage(`{}`)); err != nil || value.(ArinShadowNativeStatus).Available {
		t.Fatal("stale publication still offered to the operator")
	}
}
