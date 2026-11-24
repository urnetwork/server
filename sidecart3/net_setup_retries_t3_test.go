//go:build sidecar_localstack

// T3 Nets built through resets of the local stack's MinIO, and a T3 Net's rereads of admin info as its clock steps
// (QD-1395). A store in front of the stack (net_setup_retries_test.go's resettingStore) resets the first creates of a
// bucket and the first admin info reads, each before any answer: a Net with the default setup retries is built
// through them and its stores answer, and its twin with none is not, failing on its first admin read as every T3 Net
// did on a reset before. Small steps of a T3 Net's clock, summing to AdminInfoRereadAfter, reread nothing and leave
// every write answered on admin info still fresh, while the step past it and a jump past CheckStaleAfter each reread
// once. Like net_test.go these fail rather than skip without the stack.

package sidecart3

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A handler sending each request on to the store at address as it came, its Host header kept, since the stack's
// MinIO checks a request's signature against the host its client signed for.
func newStoreForward(t *testing.T, address string) http.Handler {
	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	forward := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: address})
	forward.Transport = transport
	return forward
}

// The settings of a T3 Net reaching the stack's MinIO through a store that resets the first two creates of a bucket,
// which minio-go sends again itself, and the first four admin info reads. Those four are the clusters' own first
// checks, two reads, and the Net's first admin read, whose check's two reads are always among them: the clusters'
// loop reads again only once the Net's clock has moved a CheckInterval, and nothing moves it while the Net is built.
// retryCount is SetupRetryCount.
func resetSetupSettings(t *testing.T, retryCount int) (*Settings, *resettingStore) {
	t.Helper()
	settings := DefaultSettings()
	settings.SetupRetryCount = retryCount
	settings.SetupRetryWait = 10 * time.Millisecond
	store := newResettingStore(t, newStoreForward(t, settings.MinioEndpoint))
	store.reset(storeRouteMakeBucket, 2)
	store.reset(storeRouteAdminInfo, 4)
	settings.MinioEndpoint = store.address
	return settings, store
}

// A T3 Net is built through resets of its setup calls, as the local stack's MinIO resets new connections under load,
// and its stores answer a write and its read.
func TestT3ANetIsBuiltThroughResetsOfItsSetupCalls(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		settings, store := resetSetupSettings(t, DefaultSettings().SetupRetryCount)
		net := NewNet(t, settings)
		defer net.Close()
		for route, expectResetCount := range map[string]int{storeRouteMakeBucket: 2, storeRouteAdminInfo: 4} {
			if _, resetCount := store.counts(route); resetCount != expectResetCount {
				t.Errorf("the store reset %d %s requests, not %d", resetCount, route, expectResetCount)
			}
		}
		cluster, err := net.Host().Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		key := runKey("setup/")
		body := []byte("built through resets")
		if _, err := cluster.PutIfNoneMatch(net.Context(), key, body, nil); err != nil {
			t.Fatalf("a write to a Net built through resets read %v", err)
		}
		object, err := cluster.Get(net.Context(), key)
		if err != nil {
			t.Fatalf("a read from a Net built through resets read %v", err)
		}
		defer object.Body.Close()
		readBody, err := io.ReadAll(object.Body)
		if err != nil || !bytes.Equal(readBody, body) {
			t.Errorf("a read from a Net built through resets read %q, %v", readBody, err)
		}
	})
}

// The twin: a T3 Net with no setup retries is not built through the same resets, its first admin read failing on
// one, as every T3 Net's did before QD-1395. Its buckets are removed as it fails.
func TestT3ANetWithNoSetupRetriesIsNotBuiltThroughResets(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		settings, _ := resetSetupSettings(t, 0)
		recovered := func() (recovered any) {
			defer func() {
				recovered = recover()
			}()
			NewNet(t, settings).Close()
			return nil
		}()
		err, _ := recovered.(error)
		if err == nil || !strings.Contains(err.Error(), "gave no admin info") {
			t.Fatalf("a Net with no setup retries, through resets of its first admin reads, ended with %v", recovered)
		}
	})
}

// Small steps of a T3 Net's clock, summing to AdminInfoRereadAfter, reread no admin info and leave every write
// answered on what the first request read, still fresh on the real clusters; the step past the threshold rereads once,
// and so does a jump past CheckStaleAfter.
func TestT3SmallStepsRereadOncePerThresholdAndAJumpRereads(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		settings := DefaultSettings()
		driven := newDrivenCatchUps("")
		net, cluster := newDrivenT3Net(t, settings, driven)
		defer net.Close()
		rereads := driven.workCount("admin info reread")
		stepAndWrite := func(step time.Duration, what string) {
			t.Helper()
			if err := Advance(net.Context(), net, step); err != nil {
				t.Fatal(err)
			}
			if _, err := cluster.PutIfNoneMatch(net.Context(), runKey("reread/"), []byte(what), nil); err != nil {
				t.Fatalf("a write after %s read %v", what, err)
			}
		}
		expectRereads := func(count int, after string) {
			t.Helper()
			if got := driven.workCount("admin info reread") - rereads; got != count {
				t.Errorf("after %s, admin info was reread %d times, not %d", after, got, count)
			}
		}
		step := settings.AdminInfoRereadAfter / 10
		for range 10 {
			stepAndWrite(step, "a step")
		}
		expectRereads(0, "steps summing to AdminInfoRereadAfter")
		stepAndWrite(step, "a step past AdminInfoRereadAfter")
		expectRereads(1, "a step past AdminInfoRereadAfter")
		stepAndWrite(settings.FileClusters.CheckStaleAfter+time.Second, "a jump past CheckStaleAfter")
		expectRereads(2, "a jump past CheckStaleAfter")
	})
}
