package stats

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// the central default (defaultStreamTTLs) is registered at package init
func TestDefaultStreamTTLRegistered(t *testing.T) {
	ttls := StreamTTLs()
	if got := ttls[StreamFindProviders2]; got != 7*24*time.Hour {
		t.Fatalf("findproviders2 default TTL = %v, want 168h", got)
	}
}

// streamLifecycleRules keys each stream by <prefix>/<env>/<stream>/ with its TTL
func TestStreamLifecycleRules(t *testing.T) {
	store := server.NewLocalBlobStore("/tmp/blob-test", "stats")
	rules := streamLifecycleRules(store, "main")

	var found *server.BlobLifecycleRule
	for i := range rules {
		if rules[i].KeyPrefix == "stats/main/"+StreamFindProviders2+"/" {
			found = &rules[i]
		}
	}
	if found == nil {
		t.Fatalf("no rule for findproviders2 in %+v", rules)
	}
	if found.TTL != 7*24*time.Hour {
		t.Fatalf("findproviders2 rule TTL = %v, want 168h", found.TTL)
	}

	// a ttl<=0 stream is excluded (kept forever)
	RegisterStreamTTL("keepforever", 0)
	defer RegisterStreamTTL("keepforever", 0) // leave disabled
	for _, r := range streamLifecycleRules(store, "main") {
		if r.KeyPrefix == "stats/main/keepforever/" {
			t.Fatal("ttl<=0 stream should not produce a lifecycle rule")
		}
	}
}
