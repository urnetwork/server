// Fenced union publication rejects stale owners, partial batch success, and
// expired queued work without discarding another owner's metadata lifetime.
package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Execute one real publication operation, including the predecessor read.
func runModernFenceTestCommand(ctx context.Context, r server.RedisClient, publication *clientScoreModernPublication, set clientScoreRedisSet, expiresAt time.Time) error {
	set.modernPublication = publication
	if set.modernBegin {
		if err := publication.prepare(ctx, r); err != nil {
			return err
		}
	}
	pipe := r.Pipeline()
	command := publication.queue(ctx, pipe, set, expiresAt)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	return checkClientScoreModernWrite(command)
}

// Begin and commit may lose their successful replies. Replaying the same owner
// is safe, but even an equal-content successor must reject every older operation.
func TestModernPublicationFenceRetryAndSupersession(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		keys := newClientScoreTargetKeys(false, RankModeQuality, clientScoreTarget{id: server.NewId()})
		key := keys.modern(server.Id{})
		pageKey := keys.facetSample(server.Id{}, ipFamilyFacetV4Only, 0)
		expiresAt := server.NowUtc().Add(time.Hour)
		server.Redis(ctx, func(r server.RedisClient) {
			first := newClientScoreModernPublication(key)
			begin := clientScoreRedisSet{key: key, modernBegin: true}
			page := clientScoreRedisSet{key: pageKey, value: []byte("first page")}
			for range 2 {
				if err := runModernFenceTestCommand(ctx, r, first, begin, expiresAt); err != nil {
					t.Fatalf("same begin retry failed: %v", err)
				}
			}
			if err := runModernFenceTestCommand(ctx, r, first, page, expiresAt); err != nil {
				t.Fatal(err)
			}
			firstCommit := clientScoreRedisSet{key: key, value: gobEncodeForTest(t, clientScoreModernPartition{Generation: first.generation})}
			for range 2 {
				if err := runModernFenceTestCommand(ctx, r, first, firstCommit, expiresAt); err != nil {
					t.Fatalf("same commit retry failed: %v", err)
				}
			}
			waiting := newClientScoreModernPublication(key)
			if err := waiting.prepare(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err := runModernFenceTestCommand(ctx, r, first, page, expiresAt); err == nil {
				t.Fatal("a committed owner mutated its page")
			}
			second := newClientScoreModernPublication(key)
			if err := runModernFenceTestCommand(ctx, r, second, begin, expiresAt); err != nil {
				t.Fatal(err)
			}
			for _, stale := range []clientScoreRedisSet{begin, page, firstCommit} {
				if err := runModernFenceTestCommand(ctx, r, first, stale, expiresAt); err == nil {
					t.Fatal("an older operation displaced an incomplete successor")
				}
			}
			if value, err := r.Get(ctx, key).Bytes(); err != nil || !bytes.Equal(value, second.dirty) {
				t.Fatalf("older operations changed the successor's dirty token: %v", err)
			}
			secondCommit := clientScoreRedisSet{key: key, value: gobEncodeForTest(t, clientScoreModernPartition{Generation: second.generation})}
			if err := runModernFenceTestCommand(ctx, r, second, secondCommit, expiresAt); err != nil {
				t.Fatal(err)
			}
			if err := runModernFenceTestCommand(ctx, r, waiting, begin, expiresAt); err == nil {
				t.Fatal("equal-content replacement recreated a pending begin's predecessor")
			}
			for _, stale := range []clientScoreRedisSet{begin, page, firstCommit} {
				if err := runModernFenceTestCommand(ctx, r, first, stale, expiresAt); err == nil {
					t.Fatal("an older operation displaced a complete successor")
				}
			}
			if value, err := r.Get(ctx, key).Bytes(); err != nil || !bytes.Equal(value, secondCommit.value) {
				t.Fatalf("older operations changed completed successor metadata: %v", err)
			}
			// A first commit, not only its replay, must respect a later dirty owner.
			pending := newClientScoreModernPublication(key)
			if err := runModernFenceTestCommand(ctx, r, pending, begin, expiresAt); err != nil {
				t.Fatal(err)
			}
			if err := runModernFenceTestCommand(ctx, r, pending, page, expiresAt); err != nil {
				t.Fatal(err)
			}
			successor := newClientScoreModernPublication(key)
			if err := runModernFenceTestCommand(ctx, r, successor, begin, expiresAt); err != nil {
				t.Fatal(err)
			}
			pendingCommit := clientScoreRedisSet{key: key, value: gobEncodeForTest(t, clientScoreModernPartition{Generation: pending.generation})}
			if err := runModernFenceTestCommand(ctx, r, pending, pendingCommit, expiresAt); err == nil {
				t.Fatal("first commit cleared a newer dirty owner")
			}
			if value, err := r.Get(ctx, key).Bytes(); err != nil || !bytes.Equal(value, successor.dirty) {
				t.Fatalf("first commit changed the newer dirty token: %v", err)
			}
		})
	})
}

// The production encoder stamps each complete payload, even when its scores and
// page boundaries are unchanged, so a waiting begin cannot see an ABA predecessor.
func TestModernPublicationCompletedGenerationChangesForEqualPayload(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		key := clientScoreLocationModernKey(false, RankModeQuality, locationId, server.Id{})
		scores := modernTestScores(1, false, ipFamilyFacetV4Only, false)
		server.Redis(ctx, func(r server.RedisClient) {
			previousGeneration := ""
			for range 2 {
				if err := writeModernPublicationTestPayload(t, ctx, r, locationId, scores, nil); err != nil {
					t.Fatal(err)
				}
				metadata, err := r.Get(ctx, key).Bytes()
				if err != nil {
					t.Fatal(err)
				}
				var partition clientScoreModernPartition
				if err := gob.NewDecoder(bytes.NewReader(metadata)).Decode(&partition); err != nil {
					t.Fatal(err)
				}
				if partition.Generation == "" || partition.Generation == previousGeneration {
					t.Fatal("identical payload publications reused their completed generation")
				}
				previousGeneration = partition.Generation
			}
		})
	})
}

// A shorter overlapping export cannot expire dirty metadata while older pages
// still live; queued expired commands cannot shorten or replace its successor.
func TestModernPublicationFencePreservesDeadlines(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		keys := newClientScoreTargetKeys(false, RankModeQuality, clientScoreTarget{id: server.NewId()})
		key := keys.modern(server.Id{})
		now := server.NowUtc()
		previousExpiry := now.Add(2 * time.Hour)
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Do(ctx, "SET", key, "previous metadata", "PXAT", previousExpiry.UnixMilli()).Err(); err != nil {
				t.Fatal(err)
			}
			first := newClientScoreModernPublication(key)
			second := newClientScoreModernPublication(key)
			begin := clientScoreRedisSet{key: key, modernBegin: true}
			for _, publication := range []*clientScoreModernPublication{first, second} {
				if err := runModernFenceTestCommand(ctx, r, publication, begin, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if expiry, err := r.PExpireTime(ctx, key).Result(); err != nil || expiry.Milliseconds() != previousExpiry.UnixMilli() {
					t.Fatalf("dirty ownership shortened the predecessor deadline: %v %v", expiry, err)
				}
			}
			page := clientScoreRedisSet{key: keys.facetSample(server.Id{}, ipFamilyFacetV4Only, 0), value: []byte("expired page")}
			commit := clientScoreRedisSet{key: key, value: gobEncodeForTest(t, clientScoreModernPartition{Generation: second.generation})}
			for _, expired := range []clientScoreRedisSet{begin, page, commit} {
				err := runModernFenceTestCommand(ctx, r, second, expired, now.Add(-time.Minute))
				if err == nil || !strings.Contains(err.Error(), "client score export expired") {
					t.Fatalf("expired queued operation was not refused: %v", err)
				}
			}
			if value, err := r.Get(ctx, key).Bytes(); err != nil || !bytes.Equal(value, second.dirty) {
				t.Fatalf("expired operation changed the current token: %v", err)
			}
			if expiry, err := r.PExpireTime(ctx, key).Result(); err != nil || expiry.Milliseconds() != previousExpiry.UnixMilli() {
				t.Fatalf("expired operation changed the retained deadline: %v %v", expiry, err)
			}
			if exists, err := r.Exists(ctx, page.key).Result(); err != nil || exists != 0 {
				t.Fatalf("expired payload became visible: exists=%d err=%v", exists, err)
			}
		})
	})
}

// One payload command fails while siblings succeed and Exec masks the error.
// The owner must inspect the individual command and never enqueue its commit.
type modernPublicationFailureHook struct {
	key     string
	pageKey string
	failure error
	failed  bool
	commits int
}

// Keep the test environment's guarded transport.
func (self *modernPublicationFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// Predecessor reads are real Redis reads.
func (self *modernPublicationFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

// Only this fixture's exact payload key is refused; all other commands execute.
func (self *modernPublicationFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		good := []redis.Cmder{}
		failed := []redis.Cmder{}
		for _, command := range commands {
			args := command.Args()
			if len(args) >= 5 && command.Name() == "eval" && args[3] == self.key {
				if args[1] == clientScoreModernCommitScript {
					self.commits++
				}
				if args[1] == clientScoreModernSetScript && args[4] == self.pageKey {
					failed = append(failed, command)
					continue
				}
			}
			good = append(good, command)
		}
		if len(failed) == 0 {
			return next(ctx, commands)
		}
		if len(good) != 0 {
			if err := next(ctx, good); err != nil {
				return err
			}
		}
		self.failed = true
		for _, command := range failed {
			command.SetErr(self.failure)
		}
		return nil
	}
}

// Partial pipeline success cannot publish complete partition metadata.
func TestModernPublicationFenceRejectsMaskedPayloadFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		keys := newClientScoreTargetKeys(false, RankModeQuality, clientScoreTarget{id: locationId})
		key := keys.modern(server.Id{})
		failure := errors.New("synthetic masked payload failure")
		hook := &modernPublicationFailureHook{key: key, pageKey: keys.facetSample(server.Id{}, ipFamilyFacetV4Only, 0), failure: failure}
		server.Redis(ctx, func(r server.RedisClient) {
			r.AddHook(hook)
			err := writeModernPublicationTestPayload(t, ctx, r, locationId, modernTestScores(400, true, ipFamilyFacetV4Only, false), nil)
			if !hook.failed || !errors.Is(err, failure) || hook.commits != 0 {
				t.Fatalf("partial batch became complete: failed=%t commits=%d err=%v", hook.failed, hook.commits, err)
			}
			metadata, err := r.Get(ctx, key).Bytes()
			if err != nil || !bytes.HasPrefix(metadata, []byte("writing:")) {
				t.Fatalf("partial batch lost its dirty metadata: %q %v", metadata, err)
			}
			if exists, err := r.Exists(ctx, keys.facetSample(server.Id{}, ipFamilyFacetV4Only, 1)).Result(); err != nil || exists != 1 {
				t.Fatalf("fixture did not execute the successful sibling page: %d %v", exists, err)
			}
		})
	})
}

// A predecessor read error cannot be interpreted as an absent partition.
func TestModernPublicationFenceRefusesPredecessorReadError(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		keys := newClientScoreTargetKeys(false, RankModeQuality, clientScoreTarget{id: server.NewId()})
		key := keys.modern(server.Id{})
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.RPush(ctx, key, "preserved non-string value").Err(); err != nil {
				t.Fatal(err)
			}
			publication := newClientScoreModernPublication(key)
			err := runModernFenceTestCommand(ctx, r, publication, clientScoreRedisSet{key: key, modernBegin: true}, server.NowUtc().Add(time.Hour))
			if err == nil || !strings.HasPrefix(err.Error(), "WRONGTYPE ") {
				t.Fatalf("predecessor read error was not returned: %v", err)
			}
			if values, readErr := r.LRange(ctx, key, 0, -1).Result(); readErr != nil || len(values) != 1 || values[0] != "preserved non-string value" {
				t.Fatalf("failed predecessor read changed Redis state: %v %v", values, readErr)
			}
		})
	})
}

// The command budget includes fixed-size fence state, even before prepare reads
// the predecessor. Two payloads cannot share a batch that only fits their values.
func TestModernPublicationFenceBytesBoundTheStream(t *testing.T) {
	publication := newClientScoreModernPublication("{modern-fence}:partition")
	set := clientScoreRedisSet{key: "{modern-fence}:page", value: make([]byte, 128), modernPublication: publication}
	batchSizes := []int{}
	err := runClientScoreExportStream(context.Background(), 512, 2*(len(set.key)+len(set.value)), 1,
		func(emit func(clientScoreRedisSet) error) error {
			for range 2 {
				if err := emit(set); err != nil {
					return err
				}
			}
			return nil
		},
		func(batch []clientScoreRedisSet) error {
			batchSizes = append(batchSizes, len(batch))
			return nil
		},
		func(context.Context, int) error { return nil },
	)
	if err != nil || len(batchSizes) != 2 || batchSizes[0] != 1 || batchSizes[1] != 1 {
		t.Fatalf("fence retention escaped the byte budget: batches=%v err=%v", batchSizes, err)
	}
}
