package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/amendment"
)

type ctfTestAmendmentStore struct {
	raw []byte
}

func (self *ctfTestAmendmentStore) Load(ctx context.Context) ([]byte, error) {
	return self.raw, nil
}

func (self *ctfTestAmendmentStore) Save(ctx context.Context, raw []byte) error {
	self.raw = append([]byte(nil), raw...)
	return nil
}

func ctfTestAmendmentConfig(lines ...string) *StConfig {
	cfg := ledgerV2TestConfig()
	cfg.Netuid = 25
	cfg.Ctf = StCtfConfig{Mode: StCtfModeLive, CarryAddress: ctfTestCarry, AmendmentLines: lines}
	return cfg
}

func resetCtfAmendments(t *testing.T) {
	stCtfAmendmentHolders.Clear()
	signer, url, storer := stCtfAmendmentSigner, stCtfAmendmentUrl, stCtfAmendmentStorer
	t.Cleanup(func() {
		stCtfAmendmentHolders.Clear()
		stCtfAmendmentSigner, stCtfAmendmentUrl, stCtfAmendmentStorer = signer, url, storer
	})
}

// §2.4: E_c is the signed entry's; before it capture the flag is not in force,
// and a configured line stands in only where no deployment pins a signer.
func TestStCtfInForceFromTheEntry(t *testing.T) {
	resetCtfAmendments(t)
	alerts := captureLedgerV2Alerts(t)
	stCtfAmendmentSigner = func(uint64, uint16) (common.Address, bool) { return common.Address{}, false }
	cfg := ctfTestAmendmentConfig(ctfTestEntryLine(t, 8))
	for epoch, inForce := range map[uint64]bool{6: false, 7: false, 8: true, 9: true} {
		entry, err := stCtfInForce(t.Context(), cfg, epoch)
		if err != nil || (entry != nil) != inForce {
			t.Fatalf("epoch %d: %v %v", epoch, entry, err)
		}
		if entry != nil && (entry.FromEpoch != 8 || stCtfCarryAddress(cfg, entry) != ctfTestCarry) {
			t.Fatalf("epoch %d: %+v", epoch, entry)
		}
	}
	// without a line nothing is in force
	if entry, err := stCtfInForce(t.Context(), ctfTestAmendmentConfig(), 9); err != nil || entry != nil {
		t.Fatalf("no line: %v %v", entry, err)
	}
	// an entry for another carry than the configured one is refused
	other, err := amendment.NewCaptureTheFlag(945, 25, ctfTestNoId, 8, common.HexToAddress("0x00000000000000000000000000000000000000d8")).Line()
	if err != nil {
		t.Fatal(err)
	}
	if entry, err := stCtfInForce(t.Context(), ctfTestAmendmentConfig(other), 8); !errors.Is(err, errStCtfTerms) || entry != nil {
		t.Fatalf("another carry: %v %v", entry, err)
	}
	if len(*alerts) != 1 || (*alerts)[0] != StCtfAlertAmendment+" epoch 8" {
		t.Fatalf("alerts %v", *alerts)
	}
	// an entry of another operator is not this pool's
	otherNo, err := amendment.NewCaptureTheFlag(945, 25, 2, 8, ctfTestCarry).Line()
	if err != nil {
		t.Fatal(err)
	}
	if entry, err := stCtfInForce(t.Context(), ctfTestAmendmentConfig(otherNo), 8); err != nil || entry != nil {
		t.Fatalf("another operator: %v %v", entry, err)
	}
}

// §2.4: with a pinned signer the published file decides, verified against it;
// its last verified copy outlives an outage and a restart, and with neither
// the terms are unknown, never "not in force".
func TestStCtfInForceFromThePublishedFile(t *testing.T) {
	resetCtfAmendments(t)
	captureLedgerV2Alerts(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	entry := amendment.NewCaptureTheFlag(945, 25, ctfTestNoId, 8, ctfTestCarry)
	if err := entry.Sign(key); err != nil {
		t.Fatal(err)
	}
	line, err := entry.Line()
	if err != nil {
		t.Fatal(err)
	}
	published := fmt.Sprintf("sn: 25\nsigned_amendments:\n  - line: %q\n    signature: \"0x%s\"\n", line, hex.EncodeToString(entry.Signature))
	up := true
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(published))
	}))
	t.Cleanup(site.Close)
	store := &ctfTestAmendmentStore{}
	stCtfAmendmentSigner = func(uint64, uint16) (common.Address, bool) { return crypto.PubkeyToAddress(key.PublicKey), true }
	stCtfAmendmentUrl = site.URL
	stCtfAmendmentStorer = func(*StConfig) amendment.Store { return store }

	// a configured line never stands in for a published file
	other, _ := amendment.NewCaptureTheFlag(945, 25, ctfTestNoId, 6, ctfTestCarry).Line()
	cfg := ctfTestAmendmentConfig(other)
	if got, err := stCtfInForce(t.Context(), cfg, 8); err != nil || got == nil || got.Configured() || got.FromEpoch != 8 {
		t.Fatalf("published: %v %v", got, err)
	}
	if got, err := stCtfInForce(t.Context(), cfg, 7); err != nil || got == nil || !got.Configured() {
		t.Fatalf("a configured line before the signed entry: %v %v", got, err)
	}
	if !strings.Contains(string(store.raw), line) {
		t.Fatalf("stored %q", store.raw)
	}

	// an outage, then a restart: the stored copy stands
	up = false
	stCtfAmendmentHolders.Clear()
	if got, err := stCtfInForce(t.Context(), cfg, 8); err != nil || got == nil || got.FromEpoch != 8 {
		t.Fatalf("stored: %v %v", got, err)
	}
	// nothing stored and nothing published: unknown
	stCtfAmendmentHolders.Clear()
	store.raw = nil
	if got, err := stCtfInForce(t.Context(), cfg, 8); !errors.Is(err, amendment.ErrUnknown) || got != nil {
		t.Fatalf("unknown: %v %v", got, err)
	}
	// a file signed by anyone else is not taken
	up = true
	stCtfAmendmentHolders.Clear()
	otherKey, _ := crypto.GenerateKey()
	stCtfAmendmentSigner = func(uint64, uint16) (common.Address, bool) { return crypto.PubkeyToAddress(otherKey.PublicKey), true }
	if got, err := stCtfInForce(t.Context(), cfg, 8); !errors.Is(err, amendment.ErrUnknown) || got != nil {
		t.Fatalf("another signer: %v %v", got, err)
	}
}
