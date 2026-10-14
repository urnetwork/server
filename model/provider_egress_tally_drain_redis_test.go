// Native read-script coverage of a multi-page drain: reads past the durable
// cursor acknowledge nothing, and cleanup never passes that cursor.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A drain's later calls read after a speculative cursor while every deletion
// stays at or before the durable cursor. Once that cursor is committed, the
// successor's cleanup-only calls delete exactly the committed prefix.
func TestProviderEgressTallyQueueReadPastDurableCursorAcknowledgesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic drain"}}
		shard := ProviderEgressTallyShard(run.Place)
		generation := "synthetic-owner"
		testingOpenTallyShard(t, ctx, shard, generation)
		writer := NewProviderEgressTallyWriter()
		for range 600 {
			if err := writer.Record(ctx, time.Now(), run, []ProviderEgressSiteLoad{{Name: "synthetic.example", Ok: true}}); err != nil {
				t.Fatal(err)
			}
		}
		retained := func() (int64, int64) {
			var count, length int64
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				count, err = r.HGet(ctx, providerEgressTallyKeys(shard)[0], "count").Int64()
				server.Raise(err)
				length, err = r.XLen(ctx, providerEgressTallyKeys(shard)[1]).Result()
				server.Raise(err)
			})
			return count, length
		}
		first, err := readProviderEgressTallyCall(ctx, shard, generation, "0-0", "0-0", providerEgressTallyPageLength, false, "")
		if err != nil || len(first.records) != 256 || !first.more || first.dirty {
			t.Fatalf("first call=%+v err=%v", first, err)
		}
		second, err := readProviderEgressTallyCall(ctx, shard, generation, "0-0", first.nextCursor, providerEgressTallyPageLength, false, "")
		if err != nil || len(second.records) != 256 || !second.more || second.dirty {
			t.Fatalf("speculative call=%+v err=%v", second, err)
		}
		if order, _ := CompareProviderEgressTallyCursor(second.nextCursor, first.nextCursor); order <= 0 {
			t.Fatal("speculative page did not follow the first page")
		}
		if count, length := retained(); count != 600 || length != 600 || second.backlog.Records != 600 || second.backlog.PendingSince.IsZero() {
			t.Fatalf("speculative read acknowledged records: count=%d length=%d backlog=%+v", count, length, second.backlog)
		}
		if _, err := readProviderEgressTallyCall(ctx, shard, generation, "0-0", "99999999999999-0", providerEgressTallyPageLength, false, ""); err == nil {
			t.Fatal("read cursor past the retained tail accepted")
		}
		if _, err := readProviderEgressTallyCall(ctx, shard, generation, second.nextCursor, first.nextCursor, providerEgressTallyPageLength, false, ""); err == nil {
			t.Fatal("read cursor before the durable cursor accepted")
		}
		// Commit 512: cleanup deletes one page per call and nothing after it.
		ack := second.nextCursor
		cleanup, err := readProviderEgressTallyCall(ctx, shard, generation, ack, ack, 0, false, "")
		if err != nil || len(cleanup.records) != 0 || !cleanup.dirty || !cleanup.more || cleanup.backlog.Records != 600-256 {
			t.Fatalf("first cleanup call=%+v err=%v", cleanup, err)
		}
		cleanup, err = readProviderEgressTallyCall(ctx, shard, generation, ack, ack, 0, false, "")
		if err != nil || cleanup.dirty || cleanup.backlog.Records != 600-512 {
			t.Fatalf("second cleanup call=%+v err=%v", cleanup, err)
		}
		if count, length := retained(); count != 88 || length != 88 {
			t.Fatalf("cleanup passed the durable cursor: count=%d length=%d", count, length)
		}
		rest, err := readProviderEgressTallyPage(ctx, shard, generation, ack, false)
		if err != nil || len(rest.Records) != 88 {
			t.Fatalf("unacknowledged suffix lost: %+v %v", rest, err)
		}
	})
}
