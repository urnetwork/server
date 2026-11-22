//go:build sidecar_localstack && sidecar_pending

// CC3's leaks into a sidecar table on T3 (capture_stores_test.go has the rest): these plant a row in MG1's
// sidecar_identity, which only the pending groups create, so the file builds only with sidecar_pending, as the gates
// and sidecar-t3.sh run it; without them each fails at its first planted row.

package sidecart3

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// Inserts value as an identity's public key, as a bug storing it would.
func plantPostgres(t *testing.T, value []byte) {
	t.Helper()
	server.Tx(context.Background(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			context.Background(),
			`INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time) VALUES ($1, $2, $3, 'ed25519', 'active', now())`,
			server.NewId(), server.NewId(), value,
		))
	})
}

// Leaks into an object's body, an object's key, a Redis value, a Redis key's name and a sidecar Postgres column are
// each caught, in their class; nothing planted, nothing found.
func TestT3CaptureStoresCatchesALeakInEachStore(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		net, report, ctx := newCapturedNet(t, nil)
		capture := net.Capture()
		if findings := capture.Scan(); len(findings) != 0 {
			t.Fatalf("an empty Net's stores: %v", findings)
		}
		cluster, err := net.Host().Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		objectBody := capture.Canary("object body")
		if _, err := cluster.PutIfNoneMatch(ctx, "head/"+runKey("cap")+"/_", objectBody, nil); err != nil {
			t.Fatal(err)
		}
		objectKey := capture.CanaryText("object key")
		if _, err := cluster.PutIfNoneMatch(ctx, "head/"+objectKey+"/_", []byte("v"), nil); err != nil {
			t.Fatal(err)
		}
		tag := runKey("t3cap_")
		redisValue := capture.Canary("redis value")
		if err := net.Host().Redis.Set(ctx, "{"+tag+"}v", redisValue, time.Hour); err != nil {
			t.Fatal(err)
		}
		redisName := capture.CanaryText("redis key name")
		if err := net.Host().Redis.Set(ctx, "{"+tag+"}"+redisName, []byte("v"), time.Hour); err != nil {
			t.Fatal(err)
		}
		plantPostgres(t, capture.Canary("postgres column"))
		findings := capture.Scan()
		for _, want := range []struct {
			label string
			class sidecartest.PrivacyClass
		}{
			{label: "object body", class: sidecartest.PrivacyClassObject},
			{label: "object key", class: sidecartest.PrivacyClassObject},
			{label: "redis value", class: sidecartest.PrivacyClassRedis},
			{label: "redis key name", class: sidecartest.PrivacyClassRedis},
			{label: "postgres column", class: sidecartest.PrivacyClassPostgres},
		} {
			if !caughtIn(findings, want.label, want.class) {
				t.Errorf("the %s leak was not caught in %s: %v", want.label, want.class, findings)
			}
		}
		if errs := report.Errors(); len(errs) != 0 {
			t.Errorf("stores not read: %v", errs)
		}
		objects, keys, rows := report.Counts()
		if objects < 2 || keys < 2 || rows < 1 {
			t.Errorf("read %d objects, %d keys and %d rows", objects, keys, rows)
		}
	})
}

// Every part of a Redis key and every page of a table is read as it streams, never only the first: with ranges of 2
// elements, string chunks of 64 bytes and Postgres pages of 1 row, a canary in a hash's fifth field, a list's seventh
// element, a set and a sorted set of 9, a stream's fifth entry, across a string's chunk boundary and in a table's
// third planted row is each caught.
func TestT3CaptureStoresStreamsEveryRangeAndPage(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		settings := DefaultCaptureStoresSettings()
		settings.RedisRangeCount = 2
		settings.RedisStringChunkByteCount = 64
		settings.PostgresPageRowCount = 1
		net, report, ctx := newCapturedNet(t, settings)
		capture := net.Capture()
		var client server.RedisClient
		server.Redis(ctx, func(redisClient server.RedisClient) {
			client = redisClient
		})
		tag := runKey("t3pages_")
		hashed := capture.CanaryText("fifth hash field")
		for index := range 5 {
			value := fmt.Sprintf("an ordinary value %d", index)
			if index == 4 {
				value = hashed
			}
			if err := client.HSet(ctx, "{"+tag+"}hash", fmt.Sprintf("field %d", index), value).Err(); err != nil {
				t.Fatal(err)
			}
		}
		listed := capture.CanaryText("seventh list element")
		elements := []any{"one", "two", "three", "four", "five", "six", listed}
		if err := client.RPush(ctx, "{"+tag+"}list", elements...).Err(); err != nil {
			t.Fatal(err)
		}
		member := capture.CanaryText("a set member among 9")
		scored := capture.CanaryText("a sorted member among 9")
		for index := range 8 {
			if err := client.SAdd(ctx, "{"+tag+"}set", fmt.Sprintf("member %d", index)).Err(); err != nil {
				t.Fatal(err)
			}
			if err := client.ZAdd(ctx, "{"+tag+"}zset", redis.Z{Score: float64(index), Member: fmt.Sprintf("member %d", index)}).Err(); err != nil {
				t.Fatal(err)
			}
		}
		if err := client.SAdd(ctx, "{"+tag+"}set", member).Err(); err != nil {
			t.Fatal(err)
		}
		if err := client.ZAdd(ctx, "{"+tag+"}zset", redis.Z{Score: 9, Member: scored}).Err(); err != nil {
			t.Fatal(err)
		}
		streamed := capture.CanaryText("fifth stream entry")
		for index := range 5 {
			value := fmt.Sprintf("an ordinary entry %d", index)
			if index == 4 {
				value = streamed
			}
			if err := client.XAdd(ctx, &redis.XAddArgs{Stream: "{" + tag + "}stream", Values: map[string]any{"field": value}}).Err(); err != nil {
				t.Fatal(err)
			}
		}
		// a 52-byte text canary starting at byte 40, so chunk 1 holds its first 24 bytes and chunk 2 the rest
		straddled := capture.CanaryText("a string across a chunk boundary")
		if err := client.Set(ctx, "{"+tag+"}string", strings.Repeat("p", 40)+straddled+strings.Repeat("q", 100), time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
		plantPostgres(t, []byte("an ordinary key 1"))
		plantPostgres(t, []byte("an ordinary key 2"))
		plantPostgres(t, capture.Canary("the third planted row"))

		findings := capture.Scan()
		for _, want := range []struct {
			label string
			class sidecartest.PrivacyClass
		}{
			{label: "fifth hash field", class: sidecartest.PrivacyClassRedis},
			{label: "seventh list element", class: sidecartest.PrivacyClassRedis},
			{label: "a set member among 9", class: sidecartest.PrivacyClassRedis},
			{label: "a sorted member among 9", class: sidecartest.PrivacyClassRedis},
			{label: "fifth stream entry", class: sidecartest.PrivacyClassRedis},
			{label: "a string across a chunk boundary", class: sidecartest.PrivacyClassRedis},
			{label: "the third planted row", class: sidecartest.PrivacyClassPostgres},
		} {
			if !caughtIn(findings, want.label, want.class) {
				t.Errorf("the %s was not caught in %s: %v", want.label, want.class, findings)
			}
		}
		if errs := report.Errors(); len(errs) != 0 {
			t.Errorf("stores not read: %v", errs)
		}
		if _, _, rows := report.Counts(); rows < 3 {
			t.Errorf("read %d rows a page at a time", rows)
		}
	})
}
