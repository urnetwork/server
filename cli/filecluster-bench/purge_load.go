package main

// §22.8's M10 versionless purges at year two's rate as a load beside a window (EVSB-M10-08; §11.13's purge row, §19.6's
// versionless files row): new objects put at the purges' rates, each deleted the readers' allowance after its put,
// unconditionally as the Purger deletes a replaced version's content, with the Purger's HEAD for its stored size and
// the sweep's re-check beside each DELETE. On a segment cluster the objects are kind-5 chunks and the index's shard
// rewrites, streamed as uploads are; on a metadata cluster they are slots. A DELETE still to come when the window
// ends is left to the run's cleanup. Held by the run's pause, when it has one.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

// One kind of object the purges churn: its size and how many are put a second.
type PurgeLoadRate struct {
	Name            string
	ObjectByteCount int64
	PerSecond       float64
}

// What the purge load runs. The Default...PurgeLoadSettings constructors are its only source.
type PurgeLoadSettings struct {
	Rates []*PurgeLoadRate
	// How long after its put an object is deleted: 60 s, the readers' allowance (§11.13's purge row).
	Allowance time.Duration
	// The HEADs each DELETE takes: 2, the Purger's for the size it reports and the sweep's re-check.
	HeadsPerDelete int
	// The most PUTs in flight, and the distinct failure texts kept: 256 and 16.
	MaxInFlight     int
	MaxFailureKinds int
}

// Year two's purges on a segment cluster (§19.6): 1 TiB a day of 4 MiB-chunk churn, ~3 PUTs and ~3 DELETEs a
// second, and the search index's shard rewrites, ~20-70 each a second at year two, here 45 of 512 KiB.
func DefaultSegmentPurgeLoadSettings() *PurgeLoadSettings {
	chunkByteCount := int64(4 * 1024 * 1024)
	churnBytesPerDay := float64(1024 * 1024 * 1024 * 1024)
	return &PurgeLoadSettings{
		Rates: []*PurgeLoadRate{
			{Name: "chunk", ObjectByteCount: chunkByteCount, PerSecond: churnBytesPerDay / (24 * time.Hour).Seconds() / float64(chunkByteCount)},
			{Name: "index-shard", ObjectByteCount: 512 * 1024, PerSecond: 45},
		},
		Allowance:       60 * time.Second,
		HeadsPerDelete:  2,
		MaxInFlight:     256,
		MaxFailureKinds: 16,
	}
}

// Year two's slot purges on one metadata set: 0.2 new slots a push (§11.13's inputs) at ~360 pushes a second a set,
// each replacing one, so ~72 slot DELETEs a second, each slot 12 KiB.
func DefaultSlotPurgeLoadSettings() *PurgeLoadSettings {
	return &PurgeLoadSettings{
		Rates:           []*PurgeLoadRate{{Name: "slot", ObjectByteCount: 12 * 1024, PerSecond: 72}},
		Allowance:       60 * time.Second,
		HeadsPerDelete:  1,
		MaxInFlight:     256,
		MaxFailureKinds: 16,
	}
}

// The PUTs a second the load offers.
func (self *PurgeLoadSettings) perSecond() float64 {
	total := 0.0
	for _, rate := range self.Rates {
		total += rate.PerSecond
	}
	return total
}

// What a purge load did: its PUTs offered over its own window, and every attempt that did not complete, the
// driver's refusals past MaxInFlight among them.
type PurgeLoadCounts struct {
	OfferedPutsPerSecond float64 `json:"offered_puts_per_second"`
	Seconds              float64 `json:"seconds"`
	Puts                 int64   `json:"puts"`
	PutByteCount         int64   `json:"put_byte_count"`
	Deletes              int64   `json:"deletes"`
	Heads                int64   `json:"heads"`
	Failed               int64   `json:"failed"`
}

// Runs the purge load on cluster for duration, its keys keyOf's, held by pause: see the file header.
func runPurgeLoad(ctx context.Context, clock sidecar.Clock, cluster sidecar.BlobCluster, segment bool, settings *PurgeLoadSettings, duration time.Duration, keyOf func(n int64) string, pause *loadPause, notes *benchNotes) *PurgeLoadCounts {
	counts := &PurgeLoadCounts{OfferedPutsPerSecond: settings.perSecond(), Seconds: duration.Seconds()}
	if counts.OfferedPutsPerSecond <= 0 || duration <= 0 {
		return counts
	}
	var puts, putBytes, deletes, heads, failed atomic.Int64
	// the DELETEs still to come, stopped when the window ends
	stateLock := sync.Mutex{}
	stops := []func() bool{}
	ended := false
	var deleting sync.WaitGroup
	purge := func(key string) {
		defer deleting.Done()
		if ctx.Err() != nil {
			return
		}
		pause.wait(ctx)
		for range settings.HeadsPerDelete {
			if _, err := cluster.Head(ctx, key); err != nil {
				failed.Add(1)
				return
			}
			heads.Add(1)
		}
		if err := cluster.Delete(ctx, key); err != nil {
			failed.Add(1)
			return
		}
		deletes.Add(1)
	}
	driver := load.NewDriver(&load.DriverSettings{
		Name:            "purges",
		Rate:            counts.OfferedPutsPerSecond,
		Steady:          duration,
		MaxInFlight:     settings.MaxInFlight,
		MaxFailureKinds: settings.MaxFailureKinds,
		Clock:           clock,
	}, func(ctx context.Context, arrival int64) error {
		pause.wait(ctx)
		// arrival n is of the rate whose share of the total its place in each second falls in
		position := float64(arrival) - float64(int64(float64(arrival)/counts.OfferedPutsPerSecond))*counts.OfferedPutsPerSecond
		rate := settings.Rates[len(settings.Rates)-1]
		for _, candidate := range settings.Rates {
			if position < candidate.PerSecond {
				rate = candidate
				break
			}
			position -= candidate.PerSecond
		}
		key := keyOf(arrival)
		body := opsBody(rate.ObjectByteCount, arrival, 0)
		var err error
		if segment {
			checksum := sha256.Sum256(body)
			_, err = cluster.PutStream(ctx, key, strings.NewReader(string(body)), int64(len(body)), checksum[:])
		} else {
			_, err = cluster.PutIfNoneMatch(ctx, key, body, nil)
		}
		if err != nil {
			return err
		}
		puts.Add(1)
		putBytes.Add(int64(len(body)))
		stateLock.Lock()
		defer stateLock.Unlock()
		if !ended {
			deleting.Add(1)
			var stop func() bool
			stop = clock.AfterFunc(settings.Allowance, func() { go purge(key) })
			stops = append(stops, func() bool {
				if stop() {
					deleting.Done()
					return true
				}
				return false
			})
		}
		return nil
	})
	report := driver.Run(ctx)
	for kind, count := range report.FailureKinds {
		notes.add(fmt.Sprintf("purges: %d failed: %s", count, kind))
	}
	// the window has ended: DELETEs not yet due are the cleanup's, those under way finish
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		ended = true
		for _, stop := range stops {
			stop()
		}
	}()
	deleting.Wait()
	counts.Puts = puts.Load()
	counts.PutByteCount = putBytes.Load()
	counts.Deletes = deletes.Load()
	counts.Heads = heads.Load()
	// a PUT's failure, or an arrival the driver refused, is in its report; a DELETE's or a HEAD's in failed
	counts.Failed = report.Failed + failed.Load()
	return counts
}
