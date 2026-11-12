//go:build sidecar_localstack

// CC3 on T3 (capture_stores.go) and the M2 stored-state asserts (sidecartest's privacy_stored.go) on real stores: the
// local stack's MinIO, the private Redis and the test's Postgres with server's migrations, the pending groups' among
// them under sidecar_pending. Like net_test.go these fail rather than skip without the stack.

package sidecart3

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The private stores with server's schema, the pending sidecar groups' tables included under sidecar_pending.
func captureTestEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: true}
}

// A T3 Net with its stores captured, closed at the test's end.
func newCapturedNet(t *testing.T, settings *CaptureStoresSettings) (*sidecartest.Net, *CaptureStoresReport, context.Context) {
	t.Helper()
	net := NewNet(t, nil)
	t.Cleanup(net.Close)
	report, err := CaptureStores(net, settings)
	if err != nil {
		t.Fatal(err)
	}
	return net, report, net.Actors().Actor(net.Context(), "writerA").Context()
}

// Whether the scan found the needle labeled label in class.
func caughtIn(findings []*sidecartest.PrivacyFinding, label string, class sidecartest.PrivacyClass) bool {
	return slices.ContainsFunc(findings, func(finding *sidecartest.PrivacyFinding) bool {
		return finding.Needle == label && finding.Class == class
	})
}

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

// DUMP's payload of a long list is compressed, so a canary among its elements is not in it as written, and the read
// by type is what catches it; so does a hash field's value, a set's member, a sorted set's member and a stream
// entry's field.
func TestT3CaptureStoresReadsWhatDumpHides(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		net, report, ctx := newCapturedNet(t, nil)
		capture := net.Capture()
		var client server.RedisClient
		server.Redis(ctx, func(redisClient server.RedisClient) {
			client = redisClient
		})
		tag := runKey("t3dump_")
		listed := capture.CanaryText("list element")
		elements := []any{}
		for index := range 400 {
			elements = append(elements, fmt.Sprintf("padding padding padding padding %d", index%7))
		}
		elements = append(elements, listed)
		if err := client.RPush(ctx, "{"+tag+"}list", elements...).Err(); err != nil {
			t.Fatal(err)
		}
		dump, err := client.Dump(ctx, "{"+tag+"}list").Result()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(dump, listed) {
			t.Logf("DUMP holds the element as written, so this run's payload was not compressed")
		}
		hashed := capture.CanaryText("hash value")
		if err := client.HSet(ctx, "{"+tag+"}hash", "field", hashed).Err(); err != nil {
			t.Fatal(err)
		}
		member := capture.CanaryText("set member")
		if err := client.SAdd(ctx, "{"+tag+"}set", member).Err(); err != nil {
			t.Fatal(err)
		}
		scored := capture.CanaryText("sorted member")
		if err := client.ZAdd(ctx, "{"+tag+"}zset", redis.Z{Score: 1, Member: scored}).Err(); err != nil {
			t.Fatal(err)
		}
		streamed := capture.CanaryText("stream field")
		if err := client.XAdd(ctx, &redis.XAddArgs{Stream: "{" + tag + "}stream", Values: map[string]any{"field": streamed}}).Err(); err != nil {
			t.Fatal(err)
		}
		findings := capture.Scan()
		for _, label := range []string{"list element", "hash value", "set member", "sorted member", "stream field"} {
			if !caughtIn(findings, label, sidecartest.PrivacyClassRedis) {
				t.Errorf("the %s was not caught: %v", label, findings)
			}
		}
		if errs := report.Errors(); len(errs) != 0 {
			t.Errorf("stores not read: %v", errs)
		}
	})
}

// Every object is read, never a sample: with a LIST of 50 keys a page, a canary in the 160th object of a cluster is
// caught.
func TestT3CaptureStoresReadsEveryObject(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		settings := DefaultCaptureStoresSettings()
		settings.ListPageCount = 50
		net, report, ctx := newCapturedNet(t, settings)
		capture := net.Capture()
		cluster, err := net.Host().Blobs.SegmentCluster()
		if err != nil {
			t.Fatal(err)
		}
		for index := range 160 {
			body := []byte("an ordinary segment")
			if index == 159 {
				body = capture.Canary("the last object")
			}
			if _, err := cluster.PutIfNoneMatch(ctx, fmt.Sprintf("seg/r/%04d", index), body, nil); err != nil {
				t.Fatal(err)
			}
		}
		if findings := capture.Scan(); !caughtIn(findings, "the last object", sidecartest.PrivacyClassObject) {
			t.Errorf("the last object's canary was not caught: %v", findings)
		}
		if objects, _, _ := report.Counts(); objects != 160 {
			t.Errorf("read %d objects", objects)
		}
	})
}

// §22.7's first stored-state assert on T3's stores: a member's PrivateMessage commit and an external commit stored
// as gkm stores them pass, and a handshake blob in the clear fails, naming its object.
func TestT3StoredCommitsArePrivateMessages(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		net, _, ctx := newCapturedNet(t, nil)
		cluster, err := net.Host().Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		world := sidecartest.NewFakeMlsWorld()
		creatorEngine := world.Engine(sidecar.NewId(), []byte{1})
		joiner := world.Engine(sidecar.NewId(), []byte{2})
		group, err := creatorEngine.CreateGroup(sidecar.NewId(), &sidecar.MlsUrGroup{RepoId: sidecar.NewId(), Kind: protocol.RepoKind_REPO_KIND_GENERAL})
		if err != nil {
			t.Fatal(err)
		}
		staged, err := group.Commit(&sidecar.MlsCommitArgs{Path: true, AuthenticatedData: []byte{}})
		if err != nil {
			t.Fatal(err)
		}
		groupInfo, err := group.GroupInfo()
		if err != nil {
			t.Fatal(err)
		}
		external, err := joiner.JoinExternal(groupInfo, &sidecar.MlsCommitArgs{Path: true, AuthenticatedData: []byte{}})
		if err != nil {
			t.Fatal(err)
		}
		groupKey := "gkm/" + sidecar.EvsKeyId(group.GroupId()) + "/epoch/"
		put := func(epoch int, commit []byte) string {
			body, err := proto.Marshal(&protocol.GkmEpochObject{Commit: commit, Header: &protocol.GkmCommitHeader{Generation: 1}})
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s%020d", groupKey, epoch)
			if _, err := cluster.PutIfNoneMatch(ctx, key, body, nil); err != nil {
				t.Fatal(err)
			}
			return key
		}
		put(1, staged.Message())
		put(2, external.Message())
		findings, err := sidecartest.CheckStoredCommits(ctx, net.Host().Blobs, nil, true)
		if err != nil || len(findings) != 0 {
			t.Fatalf("honest commits: %q, %v", findings, err)
		}
		blob := put(3, []byte(`{"commit":"add","leaves":["a member in the clear"]}`))
		findings, err = sidecartest.CheckStoredCommits(ctx, net.Host().Blobs, nil, true)
		if err != nil || len(findings) != 1 || !strings.Contains(findings[0], blob) {
			t.Errorf("a plaintext handshake blob: %q, %v", findings, err)
		}
	})
}

// evs and gkm touch no Postgres: on T3 the host's Tasks writes server's Postgres, and evs's call of it through a host
// StoredWriters gave evs fails the assert, while taskworker's passes.
func TestT3APlantedPostgresTouchFromEvsFails(t *testing.T) {
	captureTestEnv().Run(t, func(tb testing.TB) {
		net, _, ctx := newCapturedNet(t, nil)
		writers := sidecartest.NewStoredWriters(nil)
		if err := writers.Host("taskworker", net.Host()).Tasks.Schedule(ctx, &sidecar.TaskRequest{Name: "t3.capture.taskworker", OnceKey: runKey("once")}); err != nil {
			t.Fatal(err)
		}
		if findings := writers.Check(); len(findings) != 0 {
			t.Fatalf("taskworker's: %q", findings)
		}
		if err := writers.Host("evs", net.Host()).Tasks.Schedule(ctx, &sidecar.TaskRequest{Name: "t3.capture.evs", OnceKey: runKey("once")}); err != nil {
			t.Fatal(err)
		}
		findings := writers.Check()
		if len(findings) != 1 || !strings.Contains(findings[0], "evs touched Postgres: Tasks.Schedule t3.capture.evs") {
			t.Errorf("findings %q", findings)
		}
		if err := writers.Host("gkm", net.Host()).Redis.Set(ctx, "{"+runKey("t3nofam_")+"}k", []byte("v"), time.Minute); err != nil {
			t.Fatal(err)
		}
		if findings := writers.Check(); len(findings) != 2 {
			t.Errorf("a write to an unlisted family on T3's Redis: %q", findings)
		}
	})
}

// §22.7's self-test on T3: each CC3 class's leak, sidecartest's for objects and Redis and this package's for
// Postgres, put through its real path on a T3 Net, is caught by CaptureStores. It runs under sidecar_leaktest alone,
// which links the leaks, as test.sh's leak leg builds.
func TestT3EveryStoreClassCatchesItsLeak(t *testing.T) {
	if !sidecartest.LeaksLinked {
		t.Skip("the leaks link only under sidecar_leaktest, which the leak leg builds")
	}
	captureTestEnv().Run(t, func(tb testing.TB) {
		leaks := sidecartest.LeakClasses()
		for _, class := range []sidecartest.PrivacyClass{sidecartest.PrivacyClassObject, sidecartest.PrivacyClassRedis, sidecartest.PrivacyClassPostgres} {
			leak := leaks[class]
			if leak == nil {
				t.Errorf("%s has no leak", class)
				continue
			}
			func() {
				// one Net at a time: each arms the process's pg-commit hook while it is open
				net := NewNet(t, nil)
				defer net.Close()
				if _, err := CaptureStores(net, nil); err != nil {
					t.Fatal(err)
				}
				text := net.Capture().CanaryText("leak")
				leak.Put(t, net, []byte(text))
				if !caughtIn(net.Capture().Scan(), "leak", class) {
					t.Errorf("%s did not catch its leak on T3", class)
				}
			}()
		}
	})
}
