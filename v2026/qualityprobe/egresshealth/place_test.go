package egresshealth

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Tests of places: matching, destinations left out for a provider's place,
// short classes, canaries and the places' wire format.

// Holds the matching rule: an entry without a region covers
// its whole country, an entry with one covers that region only, both sides
// compared without regard to case or stray space, and a provider whose
// country is unknown is covered by nothing.
func TestPlaceCovers(t *testing.T) {
	for _, tc := range []struct {
		entry    Place
		provider Place
		want     bool
	}{
		{entry: Place{Country: "us"}, provider: Place{Country: "us", Region: "California"}, want: true},
		{entry: Place{Country: "us"}, provider: Place{Country: "us"}, want: true},
		{entry: Place{Country: "us"}, provider: Place{Country: "US"}, want: true},
		{entry: Place{Country: "us"}, provider: Place{Country: "ca"}, want: false},
		{entry: Place{Country: "us", Region: "Texas"}, provider: Place{Country: "us", Region: "texas "}, want: true},
		{entry: Place{Country: "us", Region: "Texas"}, provider: Place{Country: "us", Region: "California"}, want: false},
		{entry: Place{Country: "us", Region: "Texas"}, provider: Place{Country: "us"}, want: false},
		{entry: Place{Country: "us", Region: "Texas"}, provider: Place{Country: "mx", Region: "Texas"}, want: false},
		{entry: Place{Country: "us"}, provider: Place{}, want: false},
		{entry: Place{}, provider: Place{}, want: false},
	} {
		if got := tc.entry.covers(tc.provider); got != tc.want {
			t.Errorf("%+v covers %+v = %t, want %t", tc.entry, tc.provider, got, tc.want)
		}
	}
}

// Records every path requested.
type placeServer struct {
	stateLock sync.Mutex
	seen      map[string]int
	*httptest.Server
}

// Starts a place server: dns paths answer a DoH answer, conn paths 204 and
// the rest a body. Closed when the test ends.
func newPlaceServer(t *testing.T) *placeServer {
	t.Helper()
	s := &placeServer{seen: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.stateLock.Lock()
		s.seen[r.URL.Path]++
		s.stateLock.Unlock()
		if strings.HasPrefix(r.URL.Path, "/dns") {
			_, _ = w.Write([]byte(`{"Answer":[{"data":"192.0.2.1"}]}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/conn") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(s.Close)
	return s
}

// Reports whether path was requested.
func (self *placeServer) requested(path string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < self.seen[path]
}

// A small pool whose classes are all under their sample sizes,
// so every compatible destination is always drawn and what is left out is left
// out by place, not by the draw.
func placePool(url string) []Destination {
	pool := []Destination{}
	for i := 0; i < 3; i++ {
		pool = append(pool,
			Destination{Name: fmt.Sprintf("dns-%d", i), Class: ClassDns, Url: fmt.Sprintf("%s/dns-%d", url, i), Verify: BodyCheck{Kind: BodyCheckDnsJson}},
			Destination{Name: fmt.Sprintf("conn-%d", i), Class: ClassConnectivity, Url: fmt.Sprintf("%s/conn-%d", url, i), Expect: ExpectStatus, Status: http.StatusNoContent},
			Destination{Name: fmt.Sprintf("cdn-%d", i), Class: ClassCdn, Url: fmt.Sprintf("%s/cdn-%d", url, i)},
			Destination{Name: fmt.Sprintf("site-%d", i), Class: ClassSite, Url: fmt.Sprintf("%s/site-%d", url, i)},
		)
	}
	pool = append(pool,
		Destination{Name: "blocked-in-us", Class: ClassSite, Url: url + "/blocked-in-us", Incompatible: []Place{{Country: "us"}}},
		Destination{Name: "blocked-in-texas", Class: ClassSite, Url: url + "/blocked-in-texas", Incompatible: []Place{{Country: "us", Region: "Texas"}}},
	)
	return pool
}

// Runs pool, with {server} in its urls pointed at a new place server, for a
// provider at place.
func runForPlace(t *testing.T, pool []Destination, place Place) (*Result, *placeServer) {
	t.Helper()
	srv := newPlaceServer(t)
	for i := range pool {
		pool[i].Url = strings.Replace(pool[i].Url, "{server}", srv.URL, 1)
	}
	opts := fastOptions()
	opts.Destinations = pool
	opts.ProviderPlace = place
	opts.Rand = rand.New(rand.NewSource(1))
	res, err := Check(context.Background(), http.DefaultClient, opts)
	if err != nil {
		t.Fatalf("Check err = %v", err)
	}
	return res, srv
}

// A site known not to
// work from the provider's country is never asked for -- not scored, not
// failed, not requested at all -- while one incompatible only with a
// different region of the same country is loaded as usual.
func TestIncompatibleDestinationIsNeverLoadedFromItsPlace(t *testing.T) {
	for _, place := range []Place{
		{Country: "us", Region: "California"},
		{Country: "us"},
	} {
		res, srv := runForPlace(t, placePool("{server}"), place)
		if srv.requested("/blocked-in-us") {
			t.Errorf("%+v: a destination incompatible with the provider's country was loaded", place)
		}
		for _, c := range res.Checks {
			if c.Name == "blocked-in-us" {
				t.Errorf("%+v: blocked-in-us is among the run's checks", place)
			}
		}
		if !srv.requested("/blocked-in-texas") {
			t.Errorf("%+v: a destination incompatible only with Texas was not loaded", place)
		}
		if res.OkCount != res.Total {
			t.Errorf("%+v: Summary = %s", place, res.Summary())
		}
	}

	// ...and from Texas it is the other way round for the regional entry.
	_, srv := runForPlace(t, placePool("{server}"), Place{Country: "us", Region: "Texas"})
	if srv.requested("/blocked-in-texas") || srv.requested("/blocked-in-us") {
		t.Error("from Texas, a destination incompatible with Texas or with the US was loaded")
	}

	// A provider elsewhere loads both.
	_, srv = runForPlace(t, placePool("{server}"), Place{Country: "de"})
	if !srv.requested("/blocked-in-texas") || !srv.requested("/blocked-in-us") {
		t.Error("from Germany, a destination incompatible only with the US was not loaded")
	}
}

// When too few destinations of a class
// are compatible with the provider's place, the run takes the ones that are
// and says the class was short. It never pads the sample with sites known not
// to work there: that would charge the place's exits with them.
func TestShortClassIsReportedAndNotPadded(t *testing.T) {
	srv := newPlaceServer(t)
	var pool []Destination
	for i := 0; i < 7; i++ {
		d := Destination{Name: fmt.Sprintf("dns-%d", i), Class: ClassDns, Url: fmt.Sprintf("%s/dns-%d", srv.URL, i), Verify: BodyCheck{Kind: BodyCheckDnsJson}}
		if i < 3 {
			d.Incompatible = []Place{{Country: "cn"}}
		}
		pool = append(pool, d)
	}
	for _, c := range []Class{ClassConnectivity, ClassCdn, ClassSite} {
		for i := 0; i < sampleSizes[c]+1; i++ {
			d := Destination{Name: fmt.Sprintf("%s-%d", c, i), Class: c, Url: fmt.Sprintf("%s/%s-%d", srv.URL, c, i)}
			if c == ClassConnectivity {
				d.Expect, d.Status = ExpectStatus, http.StatusNoContent
			}
			pool = append(pool, d)
		}
	}
	opts := fastOptions()
	opts.Destinations = pool
	opts.ProviderPlace = Place{Country: "cn"}
	opts.Rand = rand.New(rand.NewSource(2))
	res, err := Check(context.Background(), http.DefaultClient, opts)
	if err != nil {
		t.Fatalf("Check err = %v", err)
	}
	if len(res.ShortClasses) != 1 || res.ShortClasses[0] != ClassDns {
		t.Fatalf("ShortClasses = %v, want [dns]: four compatible DoH endpoints cannot fill a sample of %d", res.ShortClasses, sampleSizes[ClassDns])
	}
	if got := res.ByClass[ClassDns].Total; got != 4 {
		t.Errorf("dns loads = %d, want the 4 compatible ones and no padding", got)
	}
	for i := 0; i < 3; i++ {
		if srv.requested(fmt.Sprintf("/dns-%d", i)) {
			t.Errorf("dns-%d, incompatible with cn, was loaded to pad the class", i)
		}
	}
	if !strings.Contains(res.Summary(), "short=dns") {
		t.Errorf("Summary = %q, want the short class on the line", res.Summary())
	}

	// The same pool from elsewhere is not short.
	opts.ProviderPlace = Place{Country: "de"}
	res, err = Check(context.Background(), http.DefaultClient, opts)
	if err != nil {
		t.Fatalf("Check err = %v", err)
	}
	if len(res.ShortClasses) != 0 {
		t.Errorf("ShortClasses = %v from a place every destination works from", res.ShortClasses)
	}
}

// A destination the server marked as
// a canary is loaded from the place it is incompatible with, but kept out of
// every count and reported apart; from a place it is compatible with it is an
// ordinary scored load.
func TestCanaryIsLoadedUnscoredFromItsPlace(t *testing.T) {
	pool := placePool("{server}")
	pool = append(pool, Destination{Name: "canary-de", Class: ClassSite, Url: "{server}/canary-de", Incompatible: []Place{{Country: "de"}}, Canary: true})
	res, srv := runForPlace(t, pool, Place{Country: "de"})
	if !srv.requested("/canary-de") {
		t.Fatal("the canary was not loaded from the place it is a canary for")
	}
	var canary *CheckResult
	for i := range res.Checks {
		if res.Checks[i].Name == "canary-de" {
			canary = &res.Checks[i]
		}
	}
	if canary == nil || !canary.Canary {
		t.Fatalf("canary result = %+v, want it reported with Canary", canary)
	}
	if got := res.CanaryPassedNames(); len(got) != 1 || got[0] != "canary-de" {
		t.Errorf("CanaryPassedNames = %v", got)
	}
	if res.ByClass[ClassSite].Total != 5 || res.Total != 14 {
		t.Errorf("site total = %d, run total = %d; the canary must be in no count", res.ByClass[ClassSite].Total, res.Total)
	}
	if !strings.Contains(res.Summary(), "canary=1/1") {
		t.Errorf("Summary = %q", res.Summary())
	}

	// From anywhere it works, it is an ordinary load.
	pool = placePool("{server}")
	pool = append(pool, Destination{Name: "canary-de", Class: ClassSite, Url: "{server}/canary-de", Incompatible: []Place{{Country: "de"}}, Canary: true})
	res, _ = runForPlace(t, pool, Place{Country: "fr"})
	for _, c := range res.Checks {
		if c.Name == "canary-de" && c.Canary {
			t.Error("from a compatible place the canary-marked destination was treated as a canary")
		}
	}
	if len(res.CanaryPassedNames())+len(res.CanaryFailedNames()) != 0 {
		t.Error("canary names reported from a compatible place")
	}
}

// The check draws from the
// connectivity destinations that work from the provider's place, never loads
// a canary, and says when too few are left to draw its three.
func TestBlackholeDrawsOnlyCompatibleConnectivity(t *testing.T) {
	srv := newPlaceServer(t)
	dests := []Destination{
		{Name: "conn-ok", Class: ClassConnectivity, Url: srv.URL + "/conn-ok", Expect: ExpectStatus, Status: http.StatusNoContent},
		{Name: "conn-ru", Class: ClassConnectivity, Url: srv.URL + "/conn-ru", Expect: ExpectStatus, Status: http.StatusNoContent, Incompatible: []Place{{Country: "ru"}}},
		{Name: "conn-ru-canary", Class: ClassConnectivity, Url: srv.URL + "/conn-ru-canary", Expect: ExpectStatus, Status: http.StatusNoContent, Incompatible: []Place{{Country: "ru"}}, Canary: true},
	}
	res := blackhole(context.Background(), http.DefaultClient, dests, Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep, ProviderPlace: Place{Country: "ru"}})
	if !res.Ok {
		t.Fatalf("result = %+v", res)
	}
	if srv.requested("/conn-ru") || srv.requested("/conn-ru-canary") {
		t.Error("the check loaded a connectivity destination incompatible with the provider's place")
	}
	if len(res.Results) != 1 || len(res.ShortClasses) != 1 || res.ShortClasses[0] != ClassConnectivity {
		t.Errorf("drew %d, short = %v; want the one compatible destination and connectivity short", len(res.Results), res.ShortClasses)
	}
}

// Pins the json the server serves for a
// destination's places and canary mark.
func TestDestinationPlacesWireFormat(t *testing.T) {
	d := Destination{
		Name:         "site",
		Class:        ClassSite,
		Url:          "https://site.example",
		MaxBytes:     maxAssetBytes,
		Incompatible: []Place{{Country: "cn"}, {Country: "us", Region: "Texas"}},
		Canary:       true,
	}
	buf, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}
	const want = `{"name":"site","class":"site","url":"https://site.example","expect":"body","max_bytes":1024,"incompatible":[{"country":"cn"},{"country":"us","region":"Texas"}],"canary":true}`
	if string(buf) != want {
		t.Fatalf("json = %s\nwant   %s", buf, want)
	}
	var back Destination
	if err := json.Unmarshal(buf, &back); err != nil || back.Incompatible[1].Region != "Texas" || !back.Canary {
		t.Fatalf("round trip = %+v (%v)", back, err)
	}
	plain, _ := json.Marshal(Destination{Name: "x", Class: ClassSite, Url: "https://x.example"})
	if strings.Contains(string(plain), "incompatible") || strings.Contains(string(plain), "canary") {
		t.Errorf("a destination with no places serialised them: %s", plain)
	}
}

// An incompatible place the prober could
// not match would charge that place's exits with a site known not to work
// there, so a pool carrying one is refused.
func TestPoolRefusesAPlaceItCannotMatch(t *testing.T) {
	for _, place := range []Place{{Country: "USA"}, {Country: "US"}, {Country: ""}, {Country: "u1"}, {Region: "Texas"}} {
		pool := BuiltinPool()
		pool.Destinations[0].Incompatible = []Place{place}
		err := ValidateDestinations(pool.Destinations)
		if err == nil || !strings.Contains(err.Error(), "alpha-2") {
			t.Errorf("incompatible place %+v was accepted: %v", place, err)
		}
	}
	pool := BuiltinPool()
	pool.Destinations[0].Incompatible = []Place{{Country: "us", Region: "Texas"}, {Country: "cn"}}
	if err := ValidateDestinations(pool.Destinations); err != nil {
		t.Errorf("valid places were refused: %v", err)
	}
}
