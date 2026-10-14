package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestArinNativeLeasePinsStartCurrentThroughRolloverAndReleases(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		capture, attempt := shadowScoreTestSource(t, 4)
		id := server.NewId()
		row := shadowScoreTestRow(id)
		country := "us"
		passing := true
		attempt.observe(row, &country, &providerEgressFacts{egressQuality: &passing}, true)
		row.PassesMinimums = map[string]bool{RankModeQuality: true, RankModeSpeed: true}
		row.Online = true
		target := map[server.Id]map[server.Id]*ClientScore{server.NewId(): {id: row}}
		now := server.NowUtc()
		first := newClientScoreNativeCensus(now.Add(-time.Second), now, now, map[server.Id]ProviderEgressHealthCounts{}, target)
		server.Raise(writeClientScoreNativeCensus(ctx, first, time.Minute))
		attempt.publish(first, target)
		snapshot := capture.Snapshot()
		lease, err := snapshot.AcquireCaptureLease(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		if _, err := snapshot.AcquireCaptureLease(ctx); err == nil {
			t.Fatal("competing capture retained another population")
		}
		replacement := *first
		replacement.PublicationId = server.NewId().String()
		server.Raise(writeClientScoreNativeCensus(ctx, &replacement, time.Minute))
		attempt.publish(&replacement, target)
		if snapshot.Validate(ctx) {
			t.Fatal("old snapshot claimed latest publication")
		}
		pinned, ok := lease.current(ctx)
		if !ok || pinned != snapshot || !pinned.member(id, 1).ActiveQuality {
			t.Fatal("healthy publication invalidated or changed pinned membership")
		}
		if _, err = snapshot.AcquireCaptureLease(ctx); err == nil {
			t.Fatal("stale source acquired new lease")
		}
		lease.Close()
		if _, ok = lease.current(ctx); ok || lease.snapshot != nil {
			t.Fatal("close retained population")
		}
		short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		next, err := capture.Snapshot().AcquireCaptureLease(short)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			capture.mu.Lock()
			released := next.snapshot == nil && capture.lease == nil
			capture.mu.Unlock()
			if released {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("expired lease retained population without another request")
	})
}
