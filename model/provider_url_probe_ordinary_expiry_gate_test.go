// The ordinary expiry interval is a per-shard record that fails open.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Restores a shard's interval to unswept, as on a fresh deployment.
func testingClearUrlProbeOrdinaryExpiry(t testing.TB, shardIndex, shardCount int) {
	t.Helper()
	server.Redis(t.Context(), func(client server.RedisClient) {
		server.Raise(client.Del(t.Context(), providerUrlProbeOrdinaryExpiryKey(shardIndex, shardCount)).Err())
	})
}

// Only a readable record of a sweep earlier in the same interval skips one.
func TestUrlDueOrdinaryExpiryDecision(t *testing.T) {
	previousRead := readProviderUrlProbeOrdinaryExpiry
	defer func() { readProviderUrlProbeOrdinaryExpiry = previousRead }()
	now := time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC)
	cases := []struct {
		sweptAt time.Time
		present bool
		err     error
		due     bool
	}{
		{present: false, due: true},
		{sweptAt: now, present: true, err: errors.New("synthetic redis failure"), due: true},
		{sweptAt: now, present: true, due: false},
		{sweptAt: now.Add(-providerUrlProbeOrdinaryExpiryInterval + time.Microsecond), present: true, due: false},
		{sweptAt: now.Add(-providerUrlProbeOrdinaryExpiryInterval), present: true, due: true},
		{sweptAt: now.Add(-time.Hour), present: true, due: true},
		// another clock recorded a later sweep: this admission cannot place it
		{sweptAt: now.Add(time.Microsecond), present: true, due: true},
	}
	for index, c := range cases {
		var readKey string
		readProviderUrlProbeOrdinaryExpiry = func(_ context.Context, key string) (time.Time, bool, error) {
			readKey = key
			return c.sweptAt, c.present, c.err
		}
		if due := providerUrlProbeOrdinaryExpiryDue(context.Background(), now, 1, 4); due != c.due {
			t.Errorf("case %d: due=%t want %t", index, due, c.due)
		}
		if readKey != providerUrlProbeOrdinaryExpiryKey(1, 4) {
			t.Errorf("case %d read %q", index, readKey)
		}
	}
	keys := map[string]bool{}
	for _, geometry := range [][2]int{{0, 1}, {0, 4}, {1, 4}, {0, 8}, {4, 8}} {
		keys[providerUrlProbeOrdinaryExpiryKey(geometry[0], geometry[1])] = true
	}
	if len(keys) != 5 {
		t.Fatalf("shard geometries share an interval record: %v", keys)
	}
}

// The record holds the committing admission's clock for its own shard only.
func TestUrlDueOrdinaryExpiryRecordsAdmissionClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		if err := writeProviderUrlProbeOrdinaryExpiry(ctx, providerUrlProbeOrdinaryExpiryKey(1, 4), now); err != nil {
			t.Fatal(err)
		}
		sweptAt, present, err := readProviderUrlProbeOrdinaryExpiry(ctx, providerUrlProbeOrdinaryExpiryKey(1, 4))
		if err != nil || !present || !sweptAt.Equal(now) {
			t.Fatalf("record=%s present=%t err=%v want %s", sweptAt, present, err, now)
		}
		for _, geometry := range [][2]int{{0, 4}, {1, 8}} {
			if _, present, err := readProviderUrlProbeOrdinaryExpiry(ctx, providerUrlProbeOrdinaryExpiryKey(geometry[0], geometry[1])); err != nil || present {
				t.Fatalf("shard %d/%d inherited another shard's record: present=%t err=%v", geometry[0], geometry[1], present, err)
			}
		}
		var ttl time.Duration
		server.Redis(ctx, func(client server.RedisClient) {
			ttl = client.PTTL(ctx, providerUrlProbeOrdinaryExpiryKey(1, 4)).Val()
		})
		if ttl <= providerUrlProbeOrdinaryExpiryInterval || 2*providerUrlProbeOrdinaryExpiryInterval < ttl {
			t.Fatalf("record ttl=%s, want (%s,%s]", ttl, providerUrlProbeOrdinaryExpiryInterval, 2*providerUrlProbeOrdinaryExpiryInterval)
		}
	})
}

// An unreadable or unwritable record never skips maintenance: every claim
// then sweeps as before the interval existed.
func TestUrlDueOrdinaryExpiryFailsOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		shard, order := testingUrlProbeDueShardOrder(t, ctx, now, 64, 6)
		previousRead, previousWrite := readProviderUrlProbeOrdinaryExpiry, writeProviderUrlProbeOrdinaryExpiry
		defer func() {
			readProviderUrlProbeOrdinaryExpiry, writeProviderUrlProbeOrdinaryExpiry = previousRead, previousWrite
		}()
		failures := []struct {
			read, write bool
		}{{read: true}, {write: true}}
		claimed := 0
		for _, failure := range failures {
			readProviderUrlProbeOrdinaryExpiry, writeProviderUrlProbeOrdinaryExpiry = previousRead, previousWrite
			if failure.read {
				readProviderUrlProbeOrdinaryExpiry = func(context.Context, string) (time.Time, bool, error) {
					return time.Time{}, false, errors.New("synthetic redis read failure")
				}
			}
			if failure.write {
				writeProviderUrlProbeOrdinaryExpiry = func(context.Context, string, time.Time) error {
					return errors.New("synthetic redis write failure")
				}
			}
			testingClearUrlProbeOrdinaryExpiry(t, shard, 4)
			claims := 3
			sweeps := testingCountUrlProbeExpirySweeps(t, ctx, func() {
				for range claims {
					due := ClaimProviderUrlProbeDue(ctx, now.Add(time.Duration(claimed)*time.Microsecond), 1, shard, 4)
					if len(due) != 1 || due[0].ClientId != order[claimed] {
						t.Fatalf("claim %d changed ordinary selection: got=%+v want=%s", claimed, due, order[claimed])
					}
					claimed++
				}
			})
			if sweeps != int64(claims) {
				t.Fatalf("failure %+v: %d claims sent %d expiry sweeps, want %d", failure, claims, sweeps, claims)
			}
		}
	})
}
