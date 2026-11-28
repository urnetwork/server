package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The seed block, the origin's store and the api's answer
// (connect/EXTENDER-UPDATE2.md §2.3). The publisher's invariants are pinned in
// taskworker/work/extender_seed_publish_test.go; these pin what the store and
// the origin do with whatever bytes they are given.

func TestExtenderSeedMirrorRoutes(t *testing.T) {
	config := &ExtenderSeedConfig{Mirrors: []ExtenderSeedMirrorConfig{
		{Name: "origin", Url: "https://api.ur.example/v1/seed.bin", DialAddresses: []string{"203.0.113.10", "2001:db8::10"}},
		{Name: "m2", Url: "https://seed.example.org/v1/seed.bin"},
	}}
	routes, err := config.MirrorRoutes()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, route := range routes {
		got = append(got, route.Mirror+"/"+route.Route)
		if route.Url.Hostname() == "" || strings.Contains(route.Mirror+route.Route, ".") {
			t.Fatalf("route %v", route)
		}
	}
	if want := "origin/dns origin/ipv4 origin/ipv6 m2/dns"; strings.Join(got, " ") != want {
		t.Fatalf("routes %q, want %q", strings.Join(got, " "), want)
	}
	if routes[1].DialAddress != netip.MustParseAddr("203.0.113.10") {
		t.Fatalf("dial address %s", routes[1].DialAddress)
	}

	for name, mirror := range map[string]ExtenderSeedMirrorConfig{
		"an address as a name":     {Name: "203.0.113.10", Url: "https://a.example/v1/seed.bin"},
		"a host as a name":         {Name: "a.example", Url: "https://a.example/v1/seed.bin"},
		"an upper case name":       {Name: "Origin", Url: "https://a.example/v1/seed.bin"},
		"a long name":              {Name: strings.Repeat("a", 33), Url: "https://a.example/v1/seed.bin"},
		"plain http":               {Name: "a", Url: "http://a.example/v1/seed.bin"},
		"an address as the host":   {Name: "a", Url: "https://203.0.113.10/v1/seed.bin"},
		"credentials in the url":   {Name: "a", Url: "https://user:pass@a.example/v1/seed.bin"},
		"two addresses per family": {Name: "a", Url: "https://a.example/v1/seed.bin", DialAddresses: []string{"203.0.113.10", "203.0.113.11"}},
		"an unspecified address":   {Name: "a", Url: "https://a.example/v1/seed.bin", DialAddresses: []string{"0.0.0.0"}},
		"an unparsable address":    {Name: "a", Url: "https://a.example/v1/seed.bin", DialAddresses: []string{"a.example"}},
	} {
		if _, err := (&ExtenderSeedConfig{Mirrors: []ExtenderSeedMirrorConfig{mirror}}).MirrorRoutes(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	duplicate := &ExtenderSeedConfig{Mirrors: []ExtenderSeedMirrorConfig{
		{Name: "a", Url: "https://a.example/v1/seed.bin"},
		{Name: "a", Url: "https://b.example/v1/seed.bin"},
	}}
	if _, err := duplicate.MirrorRoutes(); err == nil {
		t.Errorf("a repeated name was accepted")
	}
	many := &ExtenderSeedConfig{}
	for i := range ExtenderSeedMaxMirrorCount + 1 {
		many.Mirrors = append(many.Mirrors, ExtenderSeedMirrorConfig{Name: fmt.Sprintf("m%d", i), Url: "https://a.example/v1/seed.bin"})
	}
	if _, err := many.MirrorRoutes(); err == nil {
		t.Errorf("more than %d mirrors were accepted", ExtenderSeedMaxMirrorCount)
	}
}

func TestExtenderSeedRecordLifetime(t *testing.T) {
	for hours, want := range map[int]time.Duration{
		0:     ExtenderRecordExpireTimeout,
		-3:    ExtenderRecordExpireTimeout,
		1:     2 * ExtenderSeedRefreshTimeout,
		24:    24 * time.Hour,
		168:   168 * time.Hour,
		10000: connect.ExtenderSeedMaxValidity,
	} {
		if got := (&ExtenderSeedConfig{RecordLifetimeHours: hours}).RecordLifetime(); got != want {
			t.Errorf("%d hours: %s, want %s", hours, got, want)
		}
	}
}

func TestNormalizeExtenderSeedNetworkHost(t *testing.T) {
	for in, want := range map[string]string{
		"ur.example":    "ur.example",
		" UR.Example. ": "ur.example",
	} {
		if got, err := NormalizeExtenderSeedNetworkHost(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", ".", "../x", "a/b", "a..b", "-a.example", "a b", "a_b.example"} {
		if _, err := NormalizeExtenderSeedNetworkHost(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
}

// A store whose next claim triggers a hook, standing in for a concurrent
// publish.
type testExtenderSeedRacingStore struct {
	server.BlobStore
	afterClaim func()
}

func (self *testExtenderSeedRacingStore) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	created, err := self.BlobStore.PutIfAbsent(ctx, key, localPath, contentType)
	if hook := self.afterClaim; hook != nil {
		self.afterClaim = nil
		hook()
	}
	return created, err
}

// The sequence an artifact of these tests carries, in its text.
func testExtenderSeedArtifactSequence(artifact []byte) (uint64, bool) {
	var sequence uint64
	if _, err := fmt.Sscanf(string(artifact), "seq-%d", &sequence); err != nil {
		return 0, false
	}
	return sequence, true
}

func testExtenderSeedArtifact(sequence uint64) []byte {
	return []byte(fmt.Sprintf("seq-%d", sequence))
}

func TestWriteExtenderSeedClaimsBeforeReplacing(t *testing.T) {
	ctx := context.Background()
	store := server.NewLocalBlobStore(t.TempDir(), "blob")
	host := "ur.example"
	write := func(store server.BlobStore, sequence uint64) error {
		return WriteExtenderSeed(ctx, store, host, sequence, testExtenderSeedArtifact(sequence), testExtenderSeedArtifactSequence)
	}
	latest := func() uint64 {
		t.Helper()
		artifact, err := ReadExtenderSeedLatest(ctx, store, host)
		if err != nil {
			t.Fatal(err)
		}
		sequence, _ := testExtenderSeedArtifactSequence(artifact)
		return sequence
	}

	if artifact, err := ReadExtenderSeedLatest(ctx, store, host); artifact != nil || err != nil {
		t.Fatalf("an empty store: %v %v", artifact, err)
	}
	if err := write(store, 100); err != nil {
		t.Fatal(err)
	}
	if got := latest(); got != 100 {
		t.Fatalf("latest %d", got)
	}
	if !strings.HasPrefix(ExtenderSeedLatestKey(store, host), "blob/extender-seed/ur.example/") {
		t.Fatalf("key %s", ExtenderSeedLatestKey(store, host))
	}

	// the same sequence again is someone else's claim
	if err := write(store, 100); !errors.Is(err, ErrExtenderSeedSequenceClaimed) {
		t.Fatalf("a second write at one sequence: %v", err)
	}
	// a lower sequence never replaces latest
	if err := write(store, 50); !errors.Is(err, ErrExtenderSeedSequenceSuperseded) {
		t.Fatalf("a lower write: %v", err)
	}
	if got := latest(); got != 100 {
		t.Fatalf("latest %d after a lower write", got)
	}

	// a concurrent publish claims a higher sequence after this one's claim
	racing := &testExtenderSeedRacingStore{BlobStore: store}
	racing.afterClaim = func() {
		if err := write(store, 300); err != nil {
			t.Fatal(err)
		}
	}
	if err := write(racing, 200); !errors.Is(err, ErrExtenderSeedSequenceSuperseded) {
		t.Fatalf("a write overtaken by a higher claim: %v", err)
	}
	if got := latest(); got != 300 {
		t.Fatalf("latest %d, want the higher publish's 300", got)
	}
	floor, err := ExtenderSeedClaimedSequenceFloor(ctx, store, host)
	if err != nil || floor != 300 {
		t.Fatalf("floor %d %v", floor, err)
	}

	// an artifact past the envelope cap is neither written nor read
	if err := WriteExtenderSeed(ctx, store, host, 400, make([]byte, ExtenderSeedMaxArtifactBytes+1), testExtenderSeedArtifactSequence); !errors.Is(err, ErrExtenderSeedArtifactTooLarge) {
		t.Fatalf("an oversized write: %v", err)
	}
	stage := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(stage, make([]byte, ExtenderSeedMaxArtifactBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, ExtenderSeedLatestKey(store, host), stage, ExtenderSeedContentType); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExtenderSeedLatest(ctx, store, host); !errors.Is(err, ErrExtenderSeedArtifactTooLarge) {
		t.Fatalf("an oversized read: %v", err)
	}
}

// One signed seed of the test configuration's host, with a record of a
// synthetic open extender, issued at issueTime.
func testExtenderSeedEnvelopeBytes(t *testing.T, config *ExtenderConfig, rootPrivateKey ed25519.PrivateKey, sequence uint64, issueTime time.Time) []byte {
	t.Helper()
	publicKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey)
	record, _, err := SignExtenderRecord(config, rootPrivateKey, &model.NetworkExtender{
		ExtenderId: server.NewId(),
		PublicKey:  publicKey,
		TcpPort:    443,
		Active:     true,
	}, []*model.NetworkExtenderAddress{{
		IpVersion: 4,
		Ip:        netip.MustParseAddr("203.0.113.7"),
		Carriers:  []string{connect.ExtenderCarrierTcp},
		Active:    true,
	}}, issueTime)
	if err != nil {
		t.Fatal(err)
	}
	body, err := connect.NewExtenderSeedBody(config.NetworkHost, sequence, issueTime, issueTime.Add(ExtenderRecordExpireTimeout), []*protocol.ExtenderRecord{record}, nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := connect.SignExtenderSeed(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := connect.MarshalExtenderSeedEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func installTestExtenderSeedConfig(t *testing.T, enabled bool) (*ExtenderConfig, ed25519.PrivateKey) {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	installTestExtenderConfigYaml(t, strings.Join([]string{
		fmt.Sprintf("root_private_key_hex: %s", connect.ExtenderKeySeedHex(seed)),
		"root_public_keys_hex:",
		fmt.Sprintf("  - %s", hex.EncodeToString(rootPrivateKey.Public().(ed25519.PublicKey))),
		fmt.Sprintf("network_host: %s", testExtenderNetworkHost),
		"seed:",
		fmt.Sprintf("  enabled: %t", enabled),
	}, "\n"))
	config, err := EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	Testing_ResetExtenderSeedOrigin()
	t.Cleanup(Testing_ResetExtenderSeedOrigin)
	return config, rootPrivateKey
}

func putTestExtenderSeed(t *testing.T, artifact []byte) {
	t.Helper()
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("no blob store")
	}
	stage := filepath.Join(t.TempDir(), "seed.bin")
	if err := os.WriteFile(stage, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), ExtenderSeedLatestKey(store, testExtenderNetworkHost), stage, ExtenderSeedContentType); err != nil {
		t.Fatal(err)
	}
}

func serveTestExtenderSeed(t *testing.T, now time.Time, header http.Header) (*httptest.ResponseRecorder, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, ExtenderSeedPath, nil)
	for name, values := range header {
		request.Header[name] = values
	}
	response := httptest.NewRecorder()
	outcome := serveExtenderSeed(response, request, now)
	return response, outcome
}

// A read of the store is the process's, not a request's: a client that goes away
// does not cancel it, it has its own bound, and while it runs every other request
// is answered at once from the copy at hand, with no second read. A stalled store
// must not queue this public route's requests.
func TestExtenderSeedOriginReadHoldsUpNoOtherRequest(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	config := &ExtenderConfig{}
	good := []byte("a verified artifact")

	type read struct {
		ctx     context.Context
		release chan error
	}
	reads := make(chan *read, 4)
	cache := &extenderSeedOriginCache{
		readStored: func(ctx context.Context, _ *ExtenderConfig) ([]byte, bool, error) {
			r := &read{ctx: ctx, release: make(chan error, 1)}
			reads <- r
			if err := <-r.release; err != nil {
				return nil, false, err
			}
			return good, false, nil
		},
	}

	// a request already gone still gets its read through, under its own bound
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	type answer struct {
		copy    *extenderSeedOriginCopy
		outcome string
	}
	getAsync := func(ctx context.Context, at time.Time) chan answer {
		answers := make(chan answer, 1)
		go func() {
			copy, outcome := cache.get(ctx, config, at)
			answers <- answer{copy: copy, outcome: outcome}
		}()
		return answers
	}
	first := getAsync(gone, now)
	r := <-reads
	if r.ctx.Err() != nil {
		t.Fatalf("the read ended with the request that found it due: %v", r.ctx.Err())
	}
	deadline, ok := r.ctx.Deadline()
	if !ok || extenderSeedOriginReadTimeout < time.Until(deadline) {
		t.Fatalf("the read has no bound of its own: %v %t", deadline, ok)
	}
	r.release <- nil
	if a := <-first; a.copy == nil || !bytes.Equal(a.copy.artifact, good) {
		t.Fatalf("the first read: %+v", a)
	}

	// the next read stalls: only the request that found it due waits for it
	due := now.Add(extenderSeedOriginCacheTimeout)
	claimer := getAsync(context.Background(), due)
	r = <-reads
	other := getAsync(context.Background(), due)
	select {
	case a := <-other:
		if a.copy == nil || !bytes.Equal(a.copy.artifact, good) {
			t.Fatalf("another request during the read: %+v", a)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("a stalled read held up another request")
	}
	if 0 < len(reads) {
		t.Fatalf("another request started a second read")
	}

	// the stalled read fails: the copy at hand stands, and the next read is a
	// cache timeout away
	r.release <- errors.New("synthetic stalled store")
	if a := <-claimer; a.copy == nil || !bytes.Equal(a.copy.artifact, good) {
		t.Fatalf("after a failed read: %+v", a)
	}
	if a := <-getAsync(context.Background(), due.Add(time.Second)); a.copy == nil || 0 < len(reads) {
		t.Fatalf("a read inside the cache timeout: %+v, %d reads", a, len(reads))
	}
}

func TestServeExtenderSeed(t *testing.T) {
	server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1<<20)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	// off: nothing is served, whatever is stored
	installTestExtenderSeedConfig(t, false)
	if response, outcome := serveTestExtenderSeed(t, now, nil); response.Code != http.StatusNotFound || outcome != extenderSeedOriginDisabled {
		t.Fatalf("off: %d %s", response.Code, outcome)
	}

	config, rootPrivateKey := installTestExtenderSeedConfig(t, true)
	if response, outcome := serveTestExtenderSeed(t, now, nil); response.Code != http.StatusNotFound || outcome != extenderSeedOriginAbsent {
		t.Fatalf("absent: %d %s", response.Code, outcome)
	}

	artifact := testExtenderSeedEnvelopeBytes(t, config, rootPrivateKey, 1000, now.Add(-time.Hour))
	putTestExtenderSeed(t, artifact)
	// the absence is remembered until the copy is read again
	now = now.Add(extenderSeedOriginCacheTimeout)
	response, outcome := serveTestExtenderSeed(t, now, nil)
	if response.Code != http.StatusOK || outcome != extenderSeedOriginServed || !bytes.Equal(response.Body.Bytes(), artifact) {
		t.Fatalf("stored: %d %s", response.Code, outcome)
	}
	digest := sha256.Sum256(artifact)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	if response.Header().Get("Content-Type") != ExtenderSeedContentType ||
		response.Header().Get("Cache-Control") != "public, max-age=300" ||
		response.Header().Get("ETag") != etag {
		t.Fatalf("headers %v", response.Header())
	}
	if response, outcome := serveTestExtenderSeed(t, now, http.Header{"If-None-Match": {etag}}); response.Code != http.StatusNotModified || outcome != extenderSeedOriginNotModified || response.Body.Len() != 0 {
		t.Fatalf("conditional: %d %s", response.Code, outcome)
	}

	// a stored object that does not verify is never served; the last good
	// copy is, while it is recent
	forgerSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	forgerKey, err := connect.ExtenderPrivateKeyFromSeed(forgerSeed)
	if err != nil {
		t.Fatal(err)
	}
	putTestExtenderSeed(t, testExtenderSeedEnvelopeBytes(t, config, forgerKey, 2000, now))
	now = now.Add(extenderSeedOriginCacheTimeout)
	if response, _ := serveTestExtenderSeed(t, now, nil); response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), artifact) {
		t.Fatalf("a forged replacement: %d, want the last good copy", response.Code)
	}
	now = now.Add(ExtenderSeedRefreshTimeout)
	if response, outcome := serveTestExtenderSeed(t, now, nil); response.Code != http.StatusServiceUnavailable || outcome != extenderSeedOriginUnavailable {
		t.Fatalf("a forged replacement past the last good copy: %d %s", response.Code, outcome)
	}

	// an expired but valid bundle is served: freshness is the client's to judge
	expired := testExtenderSeedEnvelopeBytes(t, config, rootPrivateKey, 3000, now.Add(-3*ExtenderRecordExpireTimeout))
	putTestExtenderSeed(t, expired)
	now = now.Add(extenderSeedOriginCacheTimeout)
	if response, _ := serveTestExtenderSeed(t, now, nil); response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), expired) {
		t.Fatalf("an expired bundle: %d", response.Code)
	}
}
