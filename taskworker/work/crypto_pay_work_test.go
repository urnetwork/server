package work

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A coin's chains never end (the kill switch gates selling only, mmm/ZCASH-CONTRACT.md
// §3.13.1): while its daemon is not configured they wait longer each run, up
// to cryptoPayIdleMaxDelay, and pick a vault fix up within minutes.
func TestCryptoPayIdleDelayBacksOff(t *testing.T) {
	interval := 20 * time.Second
	connect.AssertEqual(t, cryptoPayIdleDelay(interval, 0), interval)
	connect.AssertEqual(t, cryptoPayIdleDelay(interval, 1), 40*time.Second)
	connect.AssertEqual(t, cryptoPayIdleDelay(interval, 3), 160*time.Second)
	connect.AssertEqual(t, cryptoPayIdleDelay(interval, 5), cryptoPayIdleMaxDelay)
	connect.AssertEqual(t, cryptoPayIdleDelay(interval, 1000), cryptoPayIdleMaxDelay)
	connect.AssertEqual(t, cryptoPayIdleDelay(cryptoAddressRefillInterval, 2), 4*time.Minute)
}
