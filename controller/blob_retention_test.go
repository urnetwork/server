// Actual MinIO XML reads and writes exercise the complete taskworker rule set.
package controller

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/stats"
)

// Own every concurrent initializer through cancellation and its final return.
func startBlobRetentionTest(t *testing.T, calls ...func(context.Context)) (context.Context, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	var workers sync.WaitGroup
	for _, call := range calls {
		workers.Add(1)
		go func() {
			defer workers.Done()
			call(ctx)
		}()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("retention initializer did not join after cancellation")
		}
	})
	return ctx, cancel, done
}

func waitBlobRetentionTest(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("retention initializer exceeded its owned deadline", ctx.Err())
	}
}

func blobRetentionRulesTest(t *testing.T, raw []byte) map[string]lifecycle.Rule {
	t.Helper()
	config := lifecycle.NewConfiguration()
	if err := xml.Unmarshal(raw, config); err != nil {
		t.Fatal("retained lifecycle XML is invalid", err)
	}
	rules := map[string]lifecycle.Rule{}
	for _, rule := range config.Rules {
		if _, duplicate := rules[rule.ID]; duplicate {
			t.Fatal("retained lifecycle duplicated a rule", rule.ID)
		}
		rules[rule.ID] = rule
	}
	return rules
}

// Two independent initializers read the same old configuration before either
// may write. Every resulting write and the final bucket retain both owned sets.
func TestApplyBlobRetentionConcurrentInitializersKeepSharedRules(t *testing.T) {
	fake := newFakeMinio(t)
	foreign := lifecycle.Rule{ID: "synthetic-operator-rule", Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: "operator-archive/"}, Expiration: lifecycle.Expiration{Days: 31}}
	otherEnv := lifecycle.Rule{ID: "urnetwork-ttl-other-env-stream", Status: "Disabled", RuleFilter: lifecycle.Filter{Prefix: "other-env/stream/"}, Expiration: lifecycle.Expiration{Days: 19}}
	initial := lifecycle.NewConfiguration()
	initial.Rules = []lifecycle.Rule{foreign, otherEnv}
	raw, err := xml.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	fake.lifecycleConfigs = map[string][]byte{"blob": raw}
	reads := make(chan struct{}, 4)
	release := make(chan struct{})
	fake.afterLifecycleRead = func(ctx context.Context, bucket string) error {
		select {
		case reads <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "blob")))
	defer pop()
	ctx, _, done := startBlobRetentionTest(t, ApplyBlobRetention, ApplyBlobRetention)
	for range 2 {
		select {
		case <-reads:
		case <-ctx.Done():
			t.Fatal("independent initializers did not reach paired old lifecycle reads", ctx.Err())
		}
	}
	if len(fake.Writes()) != 0 {
		t.Fatal("lifecycle write escaped the paired-read barrier")
	}
	close(release)
	waitBlobRetentionTest(t, ctx, done)
	env, _ := server.Env()
	streamId := "urnetwork-ttl-blob-" + strings.ReplaceAll(env, "/", "-") + "-findproviders2"
	writes := fake.Writes()
	for _, write := range writes {
		rules := blobRetentionRulesTest(t, write.body)
		if rules["urnetwork-ttl-logs"].RuleFilter.Prefix != "logs/" || rules[streamId].RuleFilter.Prefix != "blob/"+env+"/findproviders2/" {
			t.Fatal("shared lifecycle write lost part of the complete desired set", rules)
		}
		for _, original := range []lifecycle.Rule{foreign, otherEnv} {
			before, _ := xml.Marshal(original)
			after, _ := xml.Marshal(rules[original.ID])
			if !bytes.Equal(before, after) {
				t.Fatal("complete lifecycle write changed a foreign rule", original.ID)
			}
		}
	}
	if len(writes) != 2 {
		t.Fatal("each initializer must write one complete shared configuration", len(writes))
	}
	fake.stateLock.Lock()
	final := bytes.Clone(fake.lifecycleConfigs["blob"])
	fake.stateLock.Unlock()
	if !bytes.Equal(final, writes[len(writes)-1].body) || len(blobRetentionRulesTest(t, final)) != 4 {
		t.Fatal("final bucket state lost a desired or foreign lifecycle rule")
	}
}

// A shared endpoint does not merge different buckets, and disabled feedback
// never adds a log rule to the ordinary stats target.
func TestApplyBlobRetentionKeepsDistinctBucketsAndDisabledFeedback(t *testing.T) {
	for _, feedbackBucket := range []string{"feedback-bucket", ""} {
		fake := newFakeMinio(t)
		pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, feedbackBucket)))
		ctx, _, done := startBlobRetentionTest(t, ApplyBlobRetention)
		waitBlobRetentionTest(t, ctx, done)
		pop()
		fake.stateLock.Lock()
		stream := bytes.Clone(fake.lifecycleConfigs["blob"])
		feedback := bytes.Clone(fake.lifecycleConfigs[feedbackBucket])
		count := len(fake.lifecycleConfigs)
		fake.stateLock.Unlock()
		streamRules := blobRetentionRulesTest(t, stream)
		if _, hasLogs := streamRules["urnetwork-ttl-logs"]; hasLogs || len(streamRules) != 1 {
			t.Fatal("ordinary bucket acquired feedback rules", streamRules)
		}
		if feedbackBucket == "" {
			if count != 1 {
				t.Fatal("disabled feedback wrote another lifecycle configuration", count)
			}
		} else if count != 2 || len(blobRetentionRulesTest(t, feedback)) != 1 || blobRetentionRulesTest(t, feedback)["urnetwork-ttl-logs"].RuleFilter.Prefix != "logs/" {
			t.Fatal("distinct feedback bucket lost its own lifecycle")
		}
	}
}

// Even an identical bucket name on two endpoints names independent storage.
// A denied read or write on one endpoint cannot cancel the other operation.
func TestApplyBlobRetentionDistinctAuthoritiesSurviveOneStoreFailure(t *testing.T) {
	for _, fault := range []string{"none", "read", "write"} {
		stream, feedback := newFakeMinio(t), newFakeMinio(t)
		if fault == "read" {
			stream.afterLifecycleRead = func(context.Context, string) error { return errors.New("synthetic denied read") }
		} else if fault == "write" {
			stream.DenyWrites()
		}
		newStore := func(fake *fakeMinio, prefix string) server.BlobStore {
			t.Helper()
			store, err := server.NewBlobStore(&server.BlobStoreConfig{Authority: fake.Authority(), Bucket: "same-bucket", Prefix: prefix, AccessKey: "synthetic-access", SecretKey: "synthetic-secret"})
			if err != nil {
				t.Fatal(err)
			}
			return store
		}
		streamStore, feedbackStore := newStore(stream, "stats"), newStore(feedback, "logs")
		ctx, _, done := startBlobRetentionTest(t, func(ctx context.Context) { applyBlobRetention(ctx, streamStore, feedbackStore) })
		waitBlobRetentionTest(t, ctx, done)
		writes := feedback.Writes()
		if len(writes) != 1 || len(blobRetentionRulesTest(t, writes[0].body)) != 1 || blobRetentionRulesTest(t, writes[0].body)["urnetwork-ttl-logs"].RuleFilter.Prefix != "logs/" {
			t.Fatal("distinct authority lost its feedback lifecycle", fault)
		}
		if fault == "none" && len(stream.Writes()) != 1 || fault != "none" && len(stream.Writes()) != 0 {
			t.Fatal("failed lifecycle read/write changed retained configuration", fault, len(stream.Writes()))
		}
	}
}

// One target can remain in a read while another finishes its real write.
// Parent cancellation then joins the stalled read without a blind replacement.
func TestApplyBlobRetentionCancellationJoinsIndependentBuckets(t *testing.T) {
	fake := newFakeMinio(t)
	blocked, written := make(chan struct{}), make(chan struct{})
	var blockOnce, writeOnce sync.Once
	fake.afterLifecycleRead = func(ctx context.Context, bucket string) error {
		if bucket == "blob" {
			blockOnce.Do(func() { close(blocked) })
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	fake.afterLifecycleWrite = func(bucket string) {
		if bucket == "feedback-bucket" {
			writeOnce.Do(func() { close(written) })
		}
	}
	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "feedback-bucket")))
	defer pop()
	ctx, cancel, done := startBlobRetentionTest(t, ApplyBlobRetention)
	for _, event := range []<-chan struct{}{blocked, written} {
		select {
		case <-event:
		case <-ctx.Done():
			t.Fatal("blocked target prevented independent lifecycle progress", ctx.Err())
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled retention operations did not join")
	}
	writes := fake.Writes()
	if len(writes) != 1 || strings.Trim(writes[0].path, "/") != "feedback-bucket" {
		t.Fatal("cancellation caused a blind write or lost the independent target", len(writes))
	}
}

// Disabled stream settings are not converted into an expiry during coalescing.
func TestApplyBlobRetentionLeavesDisabledStreamsUntouched(t *testing.T) {
	ttls := stats.StreamTTLs()
	for stream := range ttls {
		stats.RegisterStreamTTL(stream, 0)
	}
	t.Cleanup(func() {
		for stream, ttl := range ttls {
			stats.RegisterStreamTTL(stream, ttl)
		}
	})
	fake := newFakeMinio(t)
	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "")))
	ctx, _, done := startBlobRetentionTest(t, ApplyBlobRetention)
	waitBlobRetentionTest(t, ctx, done)
	pop()
	if fake.RequestCount() != 0 {
		t.Fatal("disabled retention performed storage I/O")
	}
	pop = server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "blob")))
	defer pop()
	ctx, _, done = startBlobRetentionTest(t, ApplyBlobRetention)
	waitBlobRetentionTest(t, ctx, done)
	writes := fake.Writes()
	if len(writes) != 1 || len(blobRetentionRulesTest(t, writes[0].body)) != 1 || blobRetentionRulesTest(t, writes[0].body)["urnetwork-ttl-logs"].RuleFilter.Prefix != "logs/" {
		t.Fatal("coalescing re-enabled a disabled stream or lost enabled feedback")
	}
}
