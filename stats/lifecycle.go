package stats

// per-stream retention. A "stream" is the string name for a series of samples
// (the name passed to Append, e.g. "findproviders2"). Each stream has a default
// TTL defined here; ApplyStreamRetention translates the registry into blob-store
// lifecycle rules keyed by the stream's object prefix (<blob prefix>/<env>/
// <stream>/), which the store enforces — MinIO server-side via ILM, the local
// backend via its reaper (see server/blob.go).

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Stream names with managed retention. A stream is the string series-name
// passed to Append.
const (
	StreamFindProviders2 = "findproviders2"
)

// defaultStreamTTLs is the default retention for each known stream. Registered
// at init (below); override at runtime with RegisterStreamTTL, or set ttl<=0 to
// keep a stream forever. This is the single place to define per-stream defaults.
var defaultStreamTTLs = map[string]time.Duration{
	StreamFindProviders2: 7 * 24 * time.Hour,
}

func init() {
	for stream, ttl := range defaultStreamTTLs {
		RegisterStreamTTL(stream, ttl)
	}
}

var streamTTLMu sync.Mutex
var streamTTLs = map[string]time.Duration{}

// RegisterStreamTTL sets the retention TTL for a stats stream. Objects for the
// stream older than ttl are deleted by the store's lifecycle. Last registration
// wins; a ttl <= 0 disables retention for the stream (kept indefinitely).
func RegisterStreamTTL(stream string, ttl time.Duration) {
	streamTTLMu.Lock()
	defer streamTTLMu.Unlock()
	streamTTLs[stream] = ttl
}

// StreamTTLs returns a snapshot of the registered stream -> TTL mappings.
func StreamTTLs() map[string]time.Duration {
	streamTTLMu.Lock()
	defer streamTTLMu.Unlock()
	out := make(map[string]time.Duration, len(streamTTLs))
	for stream, ttl := range streamTTLs {
		out[stream] = ttl
	}
	return out
}

// ApplyStreamRetention installs the registered per-stream TTLs as blob-store
// lifecycle rules (MinIO ILM, or the local reaper bound to ctx). Call once at
// process init in a process with the perms to manage retention — the taskworker.
// Best-effort and independent of whether this process produces samples: it logs
// (never fails) so it cannot block startup, and no-ops when no blob store or no
// stream has retention.
func ApplyStreamRetention(ctx context.Context) {
	store, ok := server.LoadBlobStore()
	if !ok {
		glog.Infof("[stats]retention: no blob store configured\n")
		return
	}
	env, _ := server.Env()
	rules := streamLifecycleRules(store, env)
	if len(rules) == 0 {
		glog.Infof("[stats]retention: no streams with a TTL\n")
		return
	}
	if err := store.SetLifecycle(ctx, rules); err != nil {
		glog.Infof("[stats]retention apply err=%s (not enforced by this process)\n", err)
		return
	}
	glog.Infof("[stats]retention set for %d stream(s) -> %s/%s\n", len(rules), store.Authority(), store.Bucket())
}

// streamLifecycleRules builds the blob lifecycle rules for the registered
// streams under env, each keyed <prefix>/<env>/<stream>/.
func streamLifecycleRules(store server.BlobStore, env string) []server.BlobLifecycleRule {
	rules := []server.BlobLifecycleRule{}
	for stream, ttl := range StreamTTLs() {
		if ttl <= 0 {
			continue
		}
		rules = append(rules, server.BlobLifecycleRule{
			KeyPrefix: store.Prefix() + "/" + env + "/" + stream + "/",
			TTL:       ttl,
		})
	}
	return rules
}
