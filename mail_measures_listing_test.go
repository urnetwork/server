package server

import (
	"bufio"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/mxedgeservice"
)

// A mirror over testdata/mail_measures/mirror: each *.rbldnsd file's prefixes under its base name, as MAIL-M7-20's
// mirror loads them, one prefix a line. Load takes no address, so nothing asked about can reach a mirror.
type testListingMirror struct {
	t        testing.TB
	copyTime time.Time
	err      error
	// a zone left out of the copy
	skip string
}

var _ mxedgeservice.MxListingSource = (*testListingMirror)(nil)

func (self *testListingMirror) Load(ctx context.Context, add mxedgeservice.MxListingAdd) (time.Time, error) {
	if self.err != nil {
		return time.Time{}, self.err
	}
	paths, err := filepath.Glob(filepath.Join("testdata", "mail_measures", "mirror", "*.rbldnsd"))
	if err != nil || len(paths) == 0 {
		self.t.Fatalf("no mirror fixture: %v", err)
	}
	for _, path := range paths {
		zone := strings.TrimSuffix(filepath.Base(path), ".rbldnsd")
		if zone == self.skip {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			self.t.Fatal(err)
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			if err := add(netip.MustParsePrefix(strings.TrimSpace(scanner.Text())), zone); err != nil {
				file.Close()
				return time.Time{}, err
			}
		}
		file.Close()
	}
	return self.copyTime, nil
}

var testListingNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// Only the platform's own sending addresses are matched, against SBL and CSS alone, in this process.
func TestSpamhausListingClientMatchesOwnAddressesLocally(t *testing.T) {
	mirror := &testListingMirror{t: t, copyTime: testListingNow.Add(-5 * time.Minute)}
	client := NewSpamhausListingClient(mirror, nil)
	own := []netip.Addr{
		// on another zone only, which is no SBL or CSS listing
		netip.MustParseAddr("192.0.2.10"),
		// on SBL by its own /32
		netip.MustParseAddr("::ffff:192.0.2.11"),
		netip.MustParseAddr("192.0.2.12"),
		// on CSS inside a /48
		netip.MustParseAddr("2001:db8:1::25"),
		netip.MustParseAddr("192.0.2.11"),
	}
	listed, known := client.ListedAddresses(context.Background(), own, testListingNow)
	if !known || !slices.Equal(listed, []netip.Addr{netip.MustParseAddr("192.0.2.11"), netip.MustParseAddr("2001:db8:1::25")}) {
		t.Fatalf("listed %v, known %t", listed, known)
	}
	// prefixes the mirror lists that are no address of ours are in no answer
	for _, address := range listed {
		if !slices.Contains(own, address) {
			t.Errorf("%s is no address that was asked about", address)
		}
	}
	none, known := client.ListedAddresses(context.Background(), []netip.Addr{netip.MustParseAddr("192.0.2.12")}, testListingNow)
	if !known || len(none) != 0 {
		t.Errorf("a clean address: %v, known %t", none, known)
	}
}

// No mirror, a mirror that fails, a stale copy and a copy missing a zone read unknown, never unlisted, and the
// error names no path.
func TestSpamhausListingClientAnswersUnknownNotUnlisted(t *testing.T) {
	own := []netip.Addr{netip.MustParseAddr("192.0.2.11")}
	cases := []struct {
		name   string
		client *SpamhausListingClient
	}{
		{"no mirror", NewSpamhausListingClient(nil, nil)},
		{"a mirror never synced", NewSpamhausListingClient(&testListingMirror{t: t, err: errors.New("open /var/lib/feed/sbl.rbldnsd: no such file")}, nil)},
		{"a stale copy", NewSpamhausListingClient(&testListingMirror{t: t, copyTime: testListingNow.Add(-61 * time.Minute)}, nil)},
		{"a copy with no time", NewSpamhausListingClient(&testListingMirror{t: t}, nil)},
		{"a copy missing CSS", NewSpamhausListingClient(&testListingMirror{t: t, copyTime: testListingNow, skip: "css"}, nil)},
		{"no zones to read", NewSpamhausListingClient(&testListingMirror{t: t, copyTime: testListingNow}, &SpamhausListingClientSettings{MirrorMaxAge: time.Hour})},
	}
	for _, c := range cases {
		if listed, known := c.client.ListedAddresses(context.Background(), own, testListingNow); known || listed != nil {
			t.Errorf("%s: %v, known %t", c.name, listed, known)
		}
		_, err := c.client.listedAddresses(context.Background(), own, testListingNow)
		if !errors.Is(err, ErrSpamhausListingSource) {
			t.Errorf("%s: %v", c.name, err)
		} else if strings.Contains(err.Error(), "/var/lib") || strings.Contains(err.Error(), "192.0.2.") {
			t.Errorf("%s: the error names a path or an address: %v", c.name, err)
		}
	}
	// a copy at the age bound still answers
	atBound := NewSpamhausListingClient(&testListingMirror{t: t, copyTime: testListingNow.Add(-time.Hour)}, nil)
	if listed, known := atBound.ListedAddresses(context.Background(), own, testListingNow); !known || len(listed) != 1 {
		t.Errorf("a copy at the bound: %v, known %t", listed, known)
	}
}
