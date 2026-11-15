// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"testing"

	"github.com/urnetwork/server/session"
)

// Every offer email decides who bills the reader on the reader's app as the
// facts resolve it (Facts.Platform and Facts.Distribution), so the send names
// no price to a reader the App Store or Google Play bills (REVIEW.md finding
// 7). The send used to decide on the onboarding row alone, which is often
// recorded without a platform for an Android app reader.
func TestOfferDecisionsCarryTheReadersBilling(t *testing.T) {
	for _, c := range []struct {
		name         string
		platform     string
		distribution string
		storeBilled  bool
	}{
		{"android, distribution unknown (the Play build)", "android", "", true},
		{"android, play", "android", session.ClientDistributionPlay, true},
		{"android, outside Play", "android", session.ClientDistributionAndroidDirect, false},
		{"ios", "ios", "", true},
		{"macos, direct download", "macos", session.ClientDistributionMacDirect, false},
		{"web", PlatformWeb, "", false},
		{"windows", "windows", "", false},
		{"unknown", "", "", false},
	} {
		facts := func(path string, offerState string) Facts {
			f := healthy(path)
			f.Platform, f.Distribution = c.platform, c.distribution
			f.OfferState = offerState
			f.OfferEndsSoon = offerState == OfferStateActive
			return f
		}
		for _, s := range []struct {
			name  string
			step  string
			facts Facts
		}{
			{"e3a", StepE3, facts(PathA, OfferStateActive)},
			{"e4b", StepE4, facts(PathB, OfferStateNone)},
			{"e5b", StepE5, facts(PathB, OfferStateActive)},
		} {
			d := Decide(s.step, s.facts)
			if d.Action != ActionSend {
				t.Fatalf("%s %s: %+v, want a send", c.name, s.name, d)
			}
			if d.StoreBilled != c.storeBilled {
				t.Errorf("%s %s: store billed %t, want %t", c.name, s.name, d.StoreBilled, c.storeBilled)
			}
		}
	}
}
