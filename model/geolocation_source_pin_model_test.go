package model

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestSetGeolocationSourcePinStoresAndReplacesPerHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		observedAt := server.NowUtc().Truncate(time.Millisecond)

		// first observation of a host: nothing replaced
		previous := SetGeolocationSourcePin(ctx, &GeolocationSourcePin{
			Host:             "ipinfo.io",
			LeafSpki:         "leaf-ipinfo-1",
			IntermediateSpki: "int-ipinfo-1",
			ObservedAt:       observedAt,
		})
		if previous != nil {
			t.Fatalf("expected no previous pin on first observation, got %+v", previous)
		}

		// a second host is stored independently
		SetGeolocationSourcePin(ctx, &GeolocationSourcePin{
			Host:             "api.i.pn",
			LeafSpki:         "leaf-ipn-1",
			IntermediateSpki: "int-ipn-1",
			ObservedAt:       observedAt,
		})

		stored := GetGeolocationSourcePin(ctx, "ipinfo.io")
		if stored == nil {
			t.Fatal("expected a stored pin for ipinfo.io, got nil")
		}
		connect.AssertEqual(t, stored.Host, "ipinfo.io")
		connect.AssertEqual(t, stored.LeafSpki, "leaf-ipinfo-1")
		connect.AssertEqual(t, stored.IntermediateSpki, "int-ipinfo-1")
		connect.AssertEqual(t, stored.ObservedAt.UTC().Equal(observedAt.UTC()), true)

		// re-observing one host REPLACES that host's row and returns what it
		// replaced -- the return value is what makes a rotation loggable with
		// both old and new
		rotatedAt := observedAt.Add(6 * time.Hour)
		previous = SetGeolocationSourcePin(ctx, &GeolocationSourcePin{
			Host:             "ipinfo.io",
			LeafSpki:         "leaf-ipinfo-2",
			IntermediateSpki: "int-ipinfo-2",
			ObservedAt:       rotatedAt,
		})
		if previous == nil {
			t.Fatal("expected the replaced pin to be returned, got nil")
		}
		connect.AssertEqual(t, previous.LeafSpki, "leaf-ipinfo-1")
		connect.AssertEqual(t, previous.IntermediateSpki, "int-ipinfo-1")

		pins := GetGeolocationSourcePins(ctx)
		connect.AssertEqual(t, len(pins), 2)
		connect.AssertEqual(t, pins["ipinfo.io"].LeafSpki, "leaf-ipinfo-2")
		connect.AssertEqual(t, pins["ipinfo.io"].IntermediateSpki, "int-ipinfo-2")
		// replacing one host must not touch another: a per-host write that
		// disturbed its neighbours would be the same class of bug as a failing
		// host blanking the whole set
		connect.AssertEqual(t, pins["api.i.pn"].LeafSpki, "leaf-ipn-1")
		connect.AssertEqual(t, pins["api.i.pn"].IntermediateSpki, "int-ipn-1")

		// a host that has never been observed reads as absent, not as an empty
		// pin -- the consumer's correct response to absent is to refuse to
		// probe, and a zero-valued row would take that decision away from it
		if GetGeolocationSourcePin(ctx, "free.freeipapi.com") != nil {
			t.Fatal("expected nil for a host that has never been observed")
		}
		if _, ok := pins["free.freeipapi.com"]; ok {
			t.Fatal("expected an unobserved host to be absent from the pin map")
		}
	})
}

var sourceUrlPattern = regexp.MustCompile(`URL:\s*"https://([^/"]+)`)

// TestGeolocationSourceHostsMatchProberSourcesWhenCheckedOutAlongside is the
// only mechanism available for linking GeolocationSourceHosts to its
// counterpart in the operator-proxy repo. The prober is a separate Go module in
// a separate repository that depends on this server, so the list genuinely
// cannot be imported -- see the GeolocationSourceHosts comment.
//
// What this covers: a machine that has both repositories checked out as
// siblings (or names one via URNETWORK_OPERATOR_PROXY) will fail this test the
// moment the two lists disagree, which is the case that matters -- whoever
// changes a source endpoint is working in both trees.
//
// What it does NOT cover: CI, or any checkout without the prober beside it. It
// skips there rather than passing vacuously. The real backstop for drift is at
// runtime and fails closed: the prober treats a source host with no served pin
// as a hard error and refuses to probe.
func TestGeolocationSourceHostsMatchProberSourcesWhenCheckedOutAlongside(t *testing.T) {
	// Both directory names are accepted: a checkout cloned from
	// github.com/urnetwork/operator-proxy lands in `operator-proxy`, while
	// older checkouts predating the move sit in `urnetwork-operator-proxy`.
	// URNETWORK_OPERATOR_PROXY overrides both for any other layout.
	candidates := []string{
		os.Getenv("URNETWORK_OPERATOR_PROXY"),
		filepath.Join("..", "..", "operator-proxy"),
		filepath.Join("..", "..", "urnetwork-operator-proxy"),
	}

	var sourcesPath string
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		path := filepath.Join(candidate, "geolocate", "sources.go")
		if _, err := os.Stat(path); err == nil {
			sourcesPath = path
			break
		}
	}
	if sourcesPath == "" {
		t.Skip("operator-proxy checkout not found beside this one; set URNETWORK_OPERATOR_PROXY to enable this check")
	}

	b, err := os.ReadFile(sourcesPath)
	if err != nil {
		t.Fatalf("read %s: %v", sourcesPath, err)
	}

	matches := sourceUrlPattern.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		// the file exists but no longer looks the way this test assumes. Fail
		// rather than skip: a silently-inert drift check is worse than none,
		// because it reads as coverage.
		t.Fatalf("no source URLs found in %s -- the prober's source table has changed shape and this check needs updating", sourcesPath)
	}

	seen := map[string]bool{}
	proberHosts := []string{}
	for _, match := range matches {
		host := match[1]
		if seen[host] {
			continue
		}
		seen[host] = true
		proberHosts = append(proberHosts, host)
	}

	serverHosts := append([]string{}, GeolocationSourceHosts...)
	sort.Strings(proberHosts)
	sort.Strings(serverHosts)

	if len(proberHosts) != len(serverHosts) {
		t.Fatalf("host lists disagree: prober %v (%s), server %v", proberHosts, sourcesPath, serverHosts)
	}
	for i := range proberHosts {
		if proberHosts[i] != serverHosts[i] {
			t.Fatalf("host lists disagree: prober %v (%s), server %v", proberHosts, sourcesPath, serverHosts)
		}
	}
}
