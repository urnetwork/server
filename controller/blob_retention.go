// Taskworker retention owns one complete desired rule set per remote bucket.
package controller

import (
	"context"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/stats"
)

// ApplyBlobRetention joins stats and feedback rules before any lifecycle write.
// Concurrent workers with the same configuration therefore write the same full
// desired set, even if their bucket reads overlap. Retention remains best-effort
// and local reapers retain the caller's lifetime context.
func ApplyBlobRetention(ctx context.Context) {
	streamStore, _ := server.LoadBlobStore()
	feedbackStore, _ := LoadFeedbackLogStore()
	applyBlobRetention(ctx, streamStore, feedbackStore)
}

// Distinct targets proceed independently and every operation joins before
// return. This coordinates these code-owned rules, not external bucket writers.
func applyBlobRetention(ctx context.Context, streamStore, feedbackStore server.BlobStore) {
	var streamRules, feedbackRules []server.BlobLifecycleRule
	if streamStore != nil {
		env, _ := server.Env()
		streamRules = stats.StreamLifecycleRules(streamStore, env)
	}
	if feedbackStore != nil {
		feedbackRules = feedbackLogLifecycleRules(feedbackStore)
	}
	if server.BlobStoresShareLifecycle(streamStore, feedbackStore) {
		streamRules = append(streamRules, feedbackRules...)
		feedbackStore = nil
	}
	var workers sync.WaitGroup
	for _, target := range []struct {
		store server.BlobStore
		rules []server.BlobLifecycleRule
	}{
		{store: streamStore, rules: streamRules},
		{store: feedbackStore, rules: feedbackRules},
	} {
		if target.store == nil || len(target.rules) == 0 {
			continue
		}
		workers.Add(1)
		go server.HandleError(func() {
			defer workers.Done()
			if err := target.store.SetLifecycle(ctx, target.rules); err != nil {
				connect.DefaultLogger().Infof("[blob]retention apply %s/%s: %v (not enforced by this process)", target.store.Authority(), target.store.Bucket(), err)
				return
			}
			connect.DefaultLogger().Infof("[blob]retention set for %d prefixes -> %s/%s", len(target.rules), target.store.Authority(), target.store.Bucket())
		})
	}
	workers.Wait()
}
