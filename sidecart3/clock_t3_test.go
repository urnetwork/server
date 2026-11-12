//go:build sidecar_localstack

// T3's store clock (clock.go) on real stores: the local stack's MinIO, whose LastModified is its own real time, and
// the private Redis, whose ttls run on real time, each translated onto a T3 Net's fake clock. Like net_test.go these
// fail rather than skip without the stack: sidecar-t3.sh's nightly cadence runs them, and its suites cadence when the
// stack answers.

package sidecart3

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// Settings with the store clock on, aging the Redis families patterns name.
func storeClockSettings(patterns ...string) *Settings {
	settings := DefaultSettings()
	settings.StoreClock = DefaultStoreClockSettings()
	settings.StoreClock.RedisFamilies = patterns
	return settings
}

// An object written after a 25 h Advance reads as new, and one written before reads 25 h old, through Head, Get and
// List, on MinIO's own clock, which moved a few milliseconds; with the store clock off the same object reads the
// store's real time, which is what the store clock is for.
func TestT3StoreClockReadsAgesOnTheScheduleClock(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, storeClockSettings())
		defer net.Close()
		ctx := net.Actors().Actor(net.Context(), "writerA").Context()
		cluster, err := net.Host().Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		prefix := runKey("clock/") + "/"
		start := net.Clock().Now()
		if _, err := cluster.PutIfNoneMatch(ctx, prefix+"before/_", []byte("before"), nil); err != nil {
			t.Fatal(err)
		}
		if err := Advance(ctx, net, 25*time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := cluster.PutIfNoneMatch(ctx, prefix+"after/_", []byte("after"), nil); err != nil {
			t.Fatal(err)
		}
		now := net.Clock().Now()
		before, err := cluster.Head(ctx, prefix+"before/_")
		if err != nil {
			t.Fatal(err)
		}
		if !before.LastModified.Equal(start) || now.Sub(before.LastModified) != 25*time.Hour {
			t.Errorf("the object written before reads %s, %s old", before.LastModified, now.Sub(before.LastModified))
		}
		object, err := cluster.Get(ctx, prefix+"after/_")
		if err != nil {
			t.Fatal(err)
		}
		object.Body.Close()
		if !object.LastModified.Equal(now) {
			t.Errorf("the object written after reads %s, not %s", object.LastModified, now)
		}
		objects, _, err := cluster.List(ctx, prefix, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(objects) != 2 || !objects[0].LastModified.Equal(now) || !objects[1].LastModified.Equal(start) {
			t.Errorf("List reads %+v", objects)
		}
		if problems := storeClockProblems(net); len(problems) != 0 {
			t.Errorf("problems %q", problems)
		}
	})
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		ctx := net.Actors().Actor(net.Context(), "writerA").Context()
		cluster, err := net.Host().Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		key := runKey("clock/real/") + "/_"
		if _, err := cluster.PutIfNoneMatch(ctx, key, []byte("real"), nil); err != nil {
			t.Fatal(err)
		}
		info, err := cluster.Head(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if info.LastModified.Sub(net.Clock().Now()) < 24*time.Hour {
			t.Errorf("with the store clock off the object reads %s beside the schedule's %s", info.LastModified, net.Clock().Now())
		}
	})
}

// A ttl passes on Advance: a key of a family the row names and a hash field ttl in it shorten with the schedule's
// clock, at once on Advance and at the next Redis call after a step's own move, and go when the clock passes them;
// a key of another family keeps the store's real time.
func TestT3AnAdvancePassesATtl(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		tag := runKey("t3clock_")
		otherTag := runKey("t3other_")
		net := NewNet(t, storeClockSettings("{"+tag+"}*"))
		defer net.Close()
		ctx := net.Actors().Actor(net.Context(), "engineA").Context()
		redis := net.Host().Redis
		aged := "{" + tag + "}k"
		hash := "{" + tag + "}h"
		other := "{" + otherTag + "}k"
		if err := redis.Set(ctx, aged, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := redis.Set(ctx, other, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := redis.HashSet(ctx, hash, map[string][]byte{"lease": []byte("l"), "kept": []byte("k")}); err != nil {
			t.Fatal(err)
		}
		if err := redis.HashExpire(ctx, hash, []string{"lease"}, time.Hour); err != nil {
			t.Fatal(err)
		}
		var client server.RedisClient
		server.Redis(ctx, func(redisClient server.RedisClient) {
			client = redisClient
		})
		ttl := func(key string) time.Duration {
			value, err := client.PTTL(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		if err := Advance(ctx, net, 30*time.Minute); err != nil {
			t.Fatal(err)
		}
		if got := ttl(aged); 30*time.Minute < got || got < 29*time.Minute {
			t.Errorf("the named family's ttl after 30 min: %s", got)
		}
		if got := ttl(other); got < 59*time.Minute {
			t.Errorf("another family's ttl after 30 min: %s", got)
		}
		fieldTtls, err := client.HPTTL(ctx, hash, "lease").Result()
		if err != nil {
			t.Fatal(err)
		}
		if len(fieldTtls) != 1 || 30*60*1000 < fieldTtls[0] || fieldTtls[0] < 29*60*1000 {
			t.Errorf("the field's ttl after 30 min: %v ms", fieldTtls)
		}
		// a step's own move, as fault's rules make it: the next call ages first
		net.Clock().Advance(31 * time.Minute)
		if _, found, err := redis.Get(ctx, aged); err != nil || found {
			t.Errorf("the named family's key past its ttl: found %t, %v", found, err)
		}
		if _, found, err := redis.HashGet(ctx, hash, "lease"); err != nil || found {
			t.Errorf("the field past its ttl: found %t, %v", found, err)
		}
		if _, found, err := redis.HashGet(ctx, hash, "kept"); err != nil || !found {
			t.Errorf("a field with no ttl: found %t, %v", found, err)
		}
		if _, found, err := redis.Get(ctx, other); err != nil || !found {
			t.Errorf("another family's key: found %t, %v", found, err)
		}
		if errs := StoreClockOf(net).Errors(); len(errs) != 0 {
			t.Errorf("aging failed: %v", errs)
		}
	})
}

// The store clock runs as a T3 Net's own: two Nets one after another each age only from their own start.
func TestT3EachNetHasItsOwnStoreClock(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		for range 2 {
			func() {
				net := NewNet(t, storeClockSettings())
				defer net.Close()
				storeClock := StoreClockOf(net)
				if storeClock == nil {
					t.Fatal("a Net with the store clock on has none")
				}
				if _, ok := storeClock.WriteTime(metadataClusterId, "head/none/_"); ok {
					t.Error("a new Net's store clock holds a write")
				}
			}()
		}
		if StoreClockOf(&sidecartest.Net{}) != nil {
			t.Error("no Net's store clock was found")
		}
	})
}
