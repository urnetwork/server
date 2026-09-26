package egresshealth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the server's pool: its wire format, fetching and refusing it, and
// runs drawn from it with its profile.

// Serves body at PoolPath with status, and records the secret it
// was asked with.
func poolServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var stateLock sync.Mutex
	secret := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		*secret = r.Header.Get("X-UR-Operator-Secret")
		stateLock.Unlock()
		if r.URL.Path != PoolPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, secret
}

// A pool as the server would serve it.
func marshalPool(t *testing.T, pool *Pool) string {
	t.Helper()
	buf, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %s", err)
	}
	return string(buf)
}

// A pool the server serves in the documented
// shape comes back whole -- destinations, contracts, body checks, version and
// profile -- fetched under the operator secret.
func TestFetchPoolDecodesTheServedPool(t *testing.T) {
	served := BuiltinPool()
	served.Version = 17
	served.GeneratedAt = time.Date(2026, 9, 23, 6, 0, 0, 0, time.UTC)
	served.Profile = RequestProfile{UserAgent: "Mozilla/5.0 pool-agent", Headers: map[string]string{"Accept-Language": "de-DE,de;q=0.9"}}
	srv, secret := poolServer(t, http.StatusOK, marshalPool(t, served))

	pool, err := FetchPool(context.Background(), srv.Client(), srv.URL+PoolPath, "s3cret")
	if err != nil {
		t.Fatalf("FetchPool: %s", err)
	}
	if *secret != "s3cret" {
		t.Errorf("operator secret header = %q", *secret)
	}
	if pool.Version != 17 || !pool.GeneratedAt.Equal(served.GeneratedAt) {
		t.Errorf("version/generated_at = %d/%s, want 17/%s", pool.Version, pool.GeneratedAt, served.GeneratedAt)
	}
	if !reflect.DeepEqual(pool.Profile, served.Profile) {
		t.Errorf("profile = %+v, want %+v", pool.Profile, served.Profile)
	}
	// The built-in table survives the wire unchanged -- which is what lets the
	// server seed its pool from Destinations() and serve it back.
	if !reflect.DeepEqual(pool.Destinations, Destinations()) {
		t.Fatal("the built-in table did not round-trip through the pool's wire format unchanged")
	}
}

// Pins the names the server stores and serves. A rename on
// either side would make every pool fail to decode, and the prober would fall
// back to the built-in table forever while looking healthy.
func TestPoolWireFormat(t *testing.T) {
	pool := &Pool{
		Version:     3,
		GeneratedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		Destinations: []Destination{
			{Name: "doh", Class: ClassDns, Url: "https://doh.example/dns-query?name=example.com&type=A", Headers: map[string]string{"Accept": acceptDnsJson}, MaxBytes: maxDnsBytes, Verify: BodyCheck{Kind: BodyCheckDnsJson}},
			{Name: "gen204", Class: ClassConnectivity, Url: "https://c.example/generate_204", Expect: ExpectStatus, Status: 204, MaxBytes: maxConnectivityBytes},
			{Name: "portal", Class: ClassConnectivity, Url: "https://p.example/hotspot.html", MaxBytes: maxConnectivityBytes, Verify: BodyCheck{Kind: BodyCheckContains, Text: "Success"}},
			{Name: "geo-site", Class: ClassSite, Url: "https://s.example", Expect: ExpectReachable, MaxBytes: maxAssetBytes},
		},
		Profile: DefaultRequestProfile(),
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(marshalPool(t, pool)), &doc); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}
	for _, key := range []string{"version", "generated_at", "destinations", "profile"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("pool json has no %q: %v", key, doc)
		}
	}
	profile := doc["profile"].(map[string]any)
	if _, ok := profile["user_agent"]; !ok {
		t.Errorf("profile json has no user_agent: %v", profile)
	}
	if _, ok := profile["headers"]; !ok {
		t.Errorf("profile json has no headers: %v", profile)
	}
	dests := doc["destinations"].([]any)
	doh := dests[0].(map[string]any)
	for key, want := range map[string]any{
		"name": "doh", "class": "dns", "expect": "body", "max_bytes": float64(maxDnsBytes),
	} {
		if doh[key] != want {
			t.Errorf("destination %q = %v, want %v", key, doh[key], want)
		}
	}
	if verify := doh["verify"].(map[string]any); verify["kind"] != "dns_json" {
		t.Errorf("verify = %v, want kind dns_json", verify)
	}
	if gen := dests[1].(map[string]any); gen["expect"] != "status" || gen["status"] != float64(204) {
		t.Errorf("status destination = %v, want expect status and status 204", gen)
	}
	if _, present := dests[1].(map[string]any)["verify"]; present {
		t.Error("a destination with no body check serialised one; the zero check must be omitted")
	}
	if portal := dests[2].(map[string]any)["verify"].(map[string]any); portal["kind"] != "contains" || portal["text"] != "Success" {
		t.Errorf("contains check = %v", portal)
	}
	if reach := dests[3].(map[string]any); reach["expect"] != "reachable" {
		t.Errorf("reachable destination = %v", reach)
	}
}

// Every way the server can fail to
// hand over a runnable pool is ErrPoolUnavailable, which the caller answers by
// running the built-in table. None of them may come back as a pool.
func TestFetchPoolFailsOnEveryUnusableAnswer(t *testing.T) {
	valid := marshalPool(t, BuiltinPool())
	withDestinations := func(dests ...Destination) string {
		pool := BuiltinPool()
		pool.Destinations = append(pool.Destinations, dests...)
		return marshalPool(t, pool)
	}
	onlySites := &Pool{Destinations: []Destination{{Name: "s", Class: ClassSite, Url: "https://s.example"}}}

	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"401":                {status: http.StatusUnauthorized, body: valid, want: "operator secret"},
		"404":                {status: http.StatusNotFound, body: valid, want: PoolPath},
		"500":                {status: http.StatusInternalServerError, body: "boom", want: "500"},
		"not json":           {status: http.StatusOK, body: "<html>maintenance</html>", want: "decoding"},
		"null":               {status: http.StatusOK, body: "null", want: "at least one destination"},
		"empty":              {status: http.StatusOK, body: `{"version":1,"destinations":[]}`, want: "at least one destination"},
		"a class missing":    {status: http.StatusOK, body: marshalPool(t, onlySites), want: "has no destination"},
		"plaintext url":      {status: http.StatusOK, body: withDestinations(Destination{Name: "plain", Class: ClassSite, Url: "http://plain.example"}), want: "https"},
		"another port":       {status: http.StatusOK, body: withDestinations(Destination{Name: "port", Class: ClassSite, Url: "https://port.example:8443"}), want: "443"},
		"unknown class":      {status: http.StatusOK, body: withDestinations(Destination{Name: "rep", Class: Class("reputation"), Url: "https://rep.example"}), want: "reputation"},
		"duplicate name":     {status: http.StatusOK, body: withDestinations(Destination{Name: "google", Class: ClassSite, Url: "https://google.example"}), want: "repeats"},
		"unverified dns":     {status: http.StatusOK, body: withDestinations(Destination{Name: "doh2", Class: ClassDns, Url: "https://doh2.example/dns-query"}), want: "dns_json"},
		"unverified portal":  {status: http.StatusOK, body: withDestinations(Destination{Name: "portal2", Class: ClassConnectivity, Url: "https://portal2.example"}), want: "captive portal"},
		"refusal declared":   {status: http.StatusOK, body: withDestinations(Destination{Name: "refused", Class: ClassSite, Url: "https://refused.example", Expect: ExpectStatus, Status: 403}), want: "2xx or 3xx"},
		"reachable off site": {status: http.StatusOK, body: withDestinations(Destination{Name: "reach", Class: ClassCdn, Url: "https://reach.example", Expect: ExpectReachable}), want: "confined"},
		"unknown expect":     {status: http.StatusOK, body: strings.Replace(valid, `"expect":"body"`, `"expect":"sometimes"`, 1), want: "unknown expect"},
		"numeric expect":     {status: http.StatusOK, body: strings.Replace(valid, `"expect":"body"`, `"expect":0`, 1), want: "decoding"},
		"unknown check":      {status: http.StatusOK, body: strings.Replace(valid, `"kind":"ip_text"`, `"kind":"looks_fine"`, 1), want: "unknown body check"},
		"empty contains":     {status: http.StatusOK, body: strings.Replace(valid, `"text":"Success"`, `"text":""`, 1), want: "names no text"},
	} {
		srv, _ := poolServer(t, tc.status, tc.body)
		pool, err := FetchPool(context.Background(), srv.Client(), srv.URL+PoolPath, "s3cret")
		if err == nil {
			t.Fatalf("%s: FetchPool returned a pool of %d destination(s) and no error", name, len(pool.Destinations))
		}
		if pool != nil {
			t.Errorf("%s: FetchPool returned both an error and a pool", name)
		}
		if !errors.Is(err, ErrPoolUnavailable) {
			t.Errorf("%s: err = %v, want it to wrap ErrPoolUnavailable", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to say %q", name, err, tc.want)
		}
	}

	// A server that cannot be reached: the context has already ended, so
	// nothing is dialed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchPool(ctx, http.DefaultClient, "http://192.0.2.1:1"+PoolPath, "s3cret"); !errors.Is(err, ErrPoolUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("unreachable server: err = %v, want ErrPoolUnavailable carrying context.Canceled", err)
	}
	if _, err := FetchPool(context.Background(), nil, "http://192.0.2.1:1", "s"); !errors.Is(err, ErrNilClient) {
		t.Errorf("nil client: err = %v, want ErrNilClient", err)
	}
}

// The server keeps per-site fields the
// prober has no use for (category, region, retirement). They must not stop a
// pool from decoding, or every server-side addition would need a prober
// release.
func TestFetchPoolIgnoresServerBookkeeping(t *testing.T) {
	body := strings.Replace(marshalPool(t, BuiltinPool()), `"name":"google"`, `"category":"search","region":"us","retire_count":2,"name":"google"`, 1)
	body = strings.Replace(body, `{"version":0`, `{"etag":"abc","version":0`, 1)
	srv, _ := poolServer(t, http.StatusOK, body)
	if _, err := FetchPool(context.Background(), srv.Client(), srv.URL+PoolPath, "s"); err != nil {
		t.Fatalf("a pool carrying server bookkeeping was refused: %s", err)
	}
}

// The built-in table is the seed of the server's
// pool and the fallback, so it has to pass the very validation a served pool
// does.
func TestBuiltinTableIsAValidPool(t *testing.T) {
	if err := ValidateDestinations(Destinations()); err != nil {
		t.Fatalf("the built-in table is not a valid pool: %s", err)
	}
	pool := BuiltinPool()
	if pool.Version != 0 || len(pool.Destinations) != len(destinations) || pool.Profile.UserAgent != DefaultRequestProfile().UserAgent {
		t.Errorf("BuiltinPool = version %d, %d destinations, agent %q", pool.Version, len(pool.Destinations), pool.Profile.UserAgent)
	}
}

// A run with Options.Destinations draws its
// sample from that table and nothing else, and says how big it was.
func TestPoolIsWhatARunDrawsFrom(t *testing.T) {
	var stateLock sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		seen[r.URL.Path] = true
		stateLock.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var pool []Destination
	for _, c := range Classes {
		for i := 0; i < 4; i++ {
			name := string(c) + "-" + string(rune('a'+i))
			pool = append(pool, Destination{Name: name, Class: c, Url: srv.URL + "/" + name})
		}
	}
	opts := fastOptions()
	opts.Destinations = pool
	res, err := Check(context.Background(), http.DefaultClient, opts)
	if err != nil {
		t.Fatalf("Check err = %v", err)
	}
	if res.TableTotal != len(pool) {
		t.Errorf("TableTotal = %d, want the pool's %d", res.TableTotal, len(pool))
	}
	if res.Total != SamplePerRunOf(pool) || res.OkCount != res.Total {
		t.Errorf("OkCount/Total = %d/%d, want every one of the pool's %d sampled loads", res.OkCount, res.Total, SamplePerRunOf(pool))
	}
	inPool := map[string]bool{}
	for _, d := range pool {
		inPool[d.Name] = true
	}
	for _, c := range res.Checks {
		if !inPool[c.Name] {
			t.Errorf("the run loaded %q, which is not in the pool", c.Name)
		}
	}
}

// The profile the pool carries is what every request
// is shaped with -- the server can move it to a newer browser without a
// prober release -- and a profile with no user agent falls back to the
// default rather than going out as Go's own agent.
func TestPoolProfileIsApplied(t *testing.T) {
	var stateLock sync.Mutex
	var agents, languages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		agents = append(agents, r.Header.Get("User-Agent"))
		languages = append(languages, r.Header.Get("Accept-Language"))
		stateLock.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	served := &Pool{
		Destinations: []Destination{{Name: "site-a", Class: ClassSite, Url: srv.URL + "/a"}},
		Profile: RequestProfile{
			UserAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:156.0) Gecko/20100101 Firefox/156.0",
			Headers:   map[string]string{"Accept-Language": "fr-FR,fr;q=0.8", "Range": "bytes=0-1"},
		},
	}
	opts := fastOptions()
	opts.Destinations = served.Destinations
	opts.Profile = &served.Profile
	if _, err := Check(context.Background(), http.DefaultClient, opts); err != nil {
		t.Fatalf("Check err = %v", err)
	}
	opts.Profile = &RequestProfile{Headers: map[string]string{"Accept-Language": "xx"}}
	if _, err := Check(context.Background(), http.DefaultClient, opts); err != nil {
		t.Fatalf("Check err = %v", err)
	}

	stateLock.Lock()
	defer stateLock.Unlock()
	if len(agents) != 2 {
		t.Fatalf("saw %d requests, want 2", len(agents))
	}
	if agents[0] != served.Profile.UserAgent || languages[0] != "fr-FR,fr;q=0.8" {
		t.Errorf("first run sent agent %q language %q, want the pool's profile", agents[0], languages[0])
	}
	if agents[1] != DefaultRequestProfile().UserAgent || languages[1] != DefaultRequestProfile().Headers["Accept-Language"] {
		t.Errorf("a profile with no agent sent agent %q language %q, want the whole default profile", agents[1], languages[1])
	}
}
