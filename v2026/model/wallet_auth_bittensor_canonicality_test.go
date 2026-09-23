package model

import (
	"testing"

	"github.com/btcsuite/btcutil/base58"
	"golang.org/x/crypto/blake2b"
)

// Exhaustive canonicality sweep: how many distinct address STRINGS does one
// public key yield that IsValidBittensorAddress accepts? Must be exactly 1,
// because every account uniqueness check keys on the string.
func TestSS58ExhaustiveCanonicalitySweep(t *testing.T) {
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(7*i + 3)
	}
	encode := func(prefixBytes []byte) string {
		body := append(append([]byte{}, prefixBytes...), pub...)
		h, _ := blake2b.New512(nil)
		h.Write([]byte(ss58Prefix))
		h.Write(body)
		sum := h.Sum(nil)
		return base58.Encode(append(body, sum[0], sum[1]))
	}
	accepted := map[string][]byte{}
	for b0 := 0; b0 < 256; b0++ {
		if a := encode([]byte{byte(b0)}); IsValidBittensorAddress(a) {
			accepted[a] = []byte{byte(b0)}
		}
	}
	for b0 := 0; b0 < 256; b0++ {
		for b1 := 0; b1 < 256; b1++ {
			if a := encode([]byte{byte(b0), byte(b1)}); IsValidBittensorAddress(a) {
				accepted[a] = []byte{byte(b0), byte(b1)}
			}
		}
	}
	t.Logf("DISTINCT ACCEPTED ADDRESS STRINGS FOR ONE KEY: %d", len(accepted))
	for a, pb := range accepted {
		t.Logf("  prefixBytes=%v len=%d addr=%s", pb, len(a), a)
	}
	if len(accepted) != 1 {
		t.Fatalf("one key yields %d accepted address strings, want exactly 1 (sybil vector)", len(accepted))
	}
}
