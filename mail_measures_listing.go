package server

// Whether one of URnetwork's own sending addresses is on Spamhaus SBL or CSS, one of the measures §21.5's breakers
// read (ABUSE-M9-03; D-ABUSE-2, R-ABUSE-4, owner decisions 111 and 124, §2.8 ext-4, I-13).
//
// The check is local: it reads the licensed feed's mirror that §15.4's listings already keep (MAIL-M7-20's
// mxedgehost.FeedMirrorSource, an mxedgeservice.MxListingSource) and matches the sending addresses against it in
// this process, so no address, URnetwork's or a customer's, leaves in a query, and no public DNSBL resolver is asked.
// A zone is the mirror's category, its file's name: "sbl" and "css" by default. Only the sending addresses the caller names are matched, which are
// the platform's own from smtpedge.yml, never a visitor's, a sender's or a network's.
//
// The answer is the listed addresses or unknown, never unlisted for want of an answer. Unknown is no mirror, a
// mirror that did not load, one with no file for a zone this client reads, and a copy older than MirrorMaxAge: a
// stale copy cannot say an address has been delisted, nor that it is still clean. The feed and its credential are
// MAIL-M7-20's and external. Safe for concurrent use if the mirror is.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar/mxedgeservice"
)

// The mirror gave no usable answer: see the file header.
var ErrSpamhausListingSource = errors.New("spamhaus listing: the mirror gave no usable answer")

// Tunables of the client.
type SpamhausListingClientSettings struct {
	// The mirror's zones a sending address counts as listed on: §21.5's SBL and CSS. Each must be in the copy, or
	// the answer is unknown.
	Zones []string
	// How old a copy may be and still answer: an hour, twelve of the feed's 5-minute refreshes.
	MirrorMaxAge time.Duration
}

func DefaultSpamhausListingClientSettings() *SpamhausListingClientSettings {
	return &SpamhausListingClientSettings{
		Zones:        []string{"sbl", "css"},
		MirrorMaxAge: time.Hour,
	}
}

// See the file header.
type SpamhausListingClient struct {
	mirror   mxedgeservice.MxListingSource
	settings *SpamhausListingClientSettings
}

// mirror nil answers unknown; settings nil takes the defaults.
func NewSpamhausListingClient(mirror mxedgeservice.MxListingSource, settings *SpamhausListingClientSettings) *SpamhausListingClient {
	if settings == nil {
		settings = DefaultSpamhausListingClientSettings()
	}
	return &SpamhausListingClient{mirror: mirror, settings: settings}
}

// Of addresses, those a zone lists, sorted and each once, and whether the mirror answered at now (see the file
// header).
func (self *SpamhausListingClient) ListedAddresses(ctx context.Context, addresses []netip.Addr, now time.Time) (listed []netip.Addr, known bool) {
	listed, err := self.listedAddresses(ctx, addresses, now)
	if err != nil {
		return nil, false
	}
	return listed, true
}

func (self *SpamhausListingClient) listedAddresses(ctx context.Context, addresses []netip.Addr, now time.Time) ([]netip.Addr, error) {
	if self.mirror == nil {
		return nil, fmt.Errorf("%w: no mirror", ErrSpamhausListingSource)
	}
	if len(self.settings.Zones) == 0 {
		return nil, fmt.Errorf("%w: no zones", ErrSpamhausListingSource)
	}
	zones := map[string]bool{}
	for _, zone := range self.settings.Zones {
		zones[strings.ToLower(zone)] = false
	}
	own := []netip.Addr{}
	for _, address := range addresses {
		if address = address.Unmap(); address.IsValid() && !slices.Contains(own, address) {
			own = append(own, address)
		}
	}
	listed := []netip.Addr{}
	copyTime, err := self.mirror.Load(ctx, func(prefix netip.Prefix, zone string) error {
		zone = strings.ToLower(zone)
		if _, ok := zones[zone]; !ok {
			return nil
		}
		zones[zone] = true
		if !prefix.IsValid() {
			return nil
		}
		for _, address := range own {
			if prefix.Contains(address) && !slices.Contains(listed, address) {
				listed = append(listed, address)
			}
		}
		return nil
	})
	if err != nil {
		// the mirror's error may name a path or a line
		return nil, fmt.Errorf("%w: the mirror did not load", ErrSpamhausListingSource)
	}
	if copyTime.IsZero() || self.settings.MirrorMaxAge < now.Sub(copyTime) {
		return nil, fmt.Errorf("%w: a stale copy", ErrSpamhausListingSource)
	}
	for _, seen := range zones {
		if !seen {
			return nil, fmt.Errorf("%w: a zone with no entries in the copy", ErrSpamhausListingSource)
		}
	}
	slices.SortFunc(listed, func(a netip.Addr, b netip.Addr) int {
		return a.Compare(b)
	})
	return listed, nil
}
