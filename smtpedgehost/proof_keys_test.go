// T2 for the block's proof verifier (OPS-M9-02; DESIGN-DETAILS 3.2.7): smtpedge.yml's proof_key_sets become the
// sets smtpproof's verifier holds, each directory opened against its id; a block started with two sets accepts
// a preamble naming either and answers a third with the error the engine turns into KEYS; a set past its
// not_after is dropped at start; and a directory that is not its id's leaves the block with no verifier and
// not ready. Every key set here is a throwaway no ceremony made, which names no shape and holds no key.

package smtpedgehost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"
)

const testProofKeysConfig = `
root: root.example
report_host: rpt.report.example
helo_domain: out.report.example
sending_addresses:
  - {address: 192.0.2.10, helo: o1.out.report.example}
`

// A throwaway key set's directory under home: a keyset.json of this build's version and domain size with one
// phase 1 entry whose file does not exist and no shape, so it opens and holds no key. It returns the set's id
// in hex, keyset.json's SHA-256.
func writeThrowawayKeySet(t *testing.T, home string, name string) string {
	t.Helper()
	contribution := sha256.Sum256([]byte("no such contribution: " + name))
	keySetJson := fmt.Sprintf(`{"version": 2, "beacon": "%s", "phase1": [{"domain_size": 1048576, "contributions": [{"name": "%s-phase1-0000", "sha256": "%s", "byte_count": 1}]}], "shapes": []}`,
		hex.EncodeToString([]byte("throwaway "+name)), name, hex.EncodeToString(contribution[:]))
	directory := filepath.Join(home, "smtpproof-keys", name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "keyset.json"), []byte(keySetJson), 0o644); err != nil {
		t.Fatal(err)
	}
	id := sha256.Sum256([]byte(keySetJson))
	return hex.EncodeToString(id[:])
}

func testProofPreamble(keySetId string) *sidecar.SmtpProofPreamble {
	id, err := hex.DecodeString(keySetId)
	if err != nil {
		panic(err)
	}
	return &sidecar.SmtpProofPreamble{
		RecipientDomain:  "recipient.example",
		RecipientCount:   1,
		MessageId:        "message-1@ur.local.example",
		HeaderFromDomain: "sender.root.example",
		DkimSelector:     "s1",
		MessageByteCount: 1024,
		HeloName:         "o1.out.report.example",
		// in the forms the edge issues them in: 36 and 40 characters, synthetic
		XUrRef:         "1.aeibceqtcqkrmfyydenbxifbukr2jjngu4",
		FeedbackId:     "n:c:gaytemzugu3doobzhi5typj6h4:urnetwork",
		EnvelopeDomain: "bounce.root.example",
		MxHost:         "mx.recipient.example",
		ProofKeySet:    id,
	}
}

// A block started with two key sets holds both: its verifier accepts a preamble naming either, with a session
// under that set, and answers a preamble naming a third, one the file does not list, the error the engine
// answers KEYS for. The verifier is no longer among what the block says it lacks.
func TestBlockVerifierHoldsItsTwoKeySetsAndRefusesAThird(t *testing.T) {
	home := t.TempDir()
	first := writeThrowawayKeySet(t, home, "first")
	second := writeThrowawayKeySet(t, home, "second")
	third := writeThrowawayKeySet(t, home, "third")
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(testProofKeysConfig+"proof_key_sets:\n"+
		"  - {id: "+first+", directory: smtpproof-keys/first}\n"+
		"  - {id: "+second+", directory: "+filepath.Join(home, "smtpproof-keys", "second")+"}\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	verifier, err := NewProofVerifier(config, home, now)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	settings := DefaultHostSettings(config)
	settings.ConfigHome = home
	block, err := NewBlockHost(ctx, settings, &HostDeps{
		Quota:    host.Quota,
		Redis:    host.Redis,
		Gates:    host.Gates,
		Clock:    clock,
		Log:      &testLog{},
		Verifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	if slices.ContainsFunc(block.Missing(), func(text string) bool { return strings.Contains(text, "verifier") }) {
		t.Fatalf("a block with its verifier still lacks %q", block.Missing())
	}

	var blockVerifier sidecar.SmtpProofVerifier = block.deps.Verifier
	for name, id := range map[string]string{"first": first, "second": second} {
		preamble := testProofPreamble(id)
		if !blockVerifier.HoldsKeySet(preamble.ProofKeySet) {
			t.Errorf("the block does not hold the %s set", name)
		}
		session, err := blockVerifier.NewSmtpProofSession(preamble)
		if err != nil || session == nil {
			t.Errorf("a preamble naming the %s set: %v", name, err)
		}
	}
	// a third set, a well-formed one the file does not list, and no set at all
	for name, preamble := range map[string]*sidecar.SmtpProofPreamble{
		"a third set": testProofPreamble(third),
		"no set":      func() *sidecar.SmtpProofPreamble { p := testProofPreamble(first); p.ProofKeySet = nil; return p }(),
		"a short id": func() *sidecar.SmtpProofPreamble {
			p := testProofPreamble(first)
			p.ProofKeySet = p.ProofKeySet[:31]
			return p
		}(),
	} {
		if blockVerifier.HoldsKeySet(preamble.ProofKeySet) {
			t.Errorf("the block holds %s", name)
		}
		if session, err := blockVerifier.NewSmtpProofSession(preamble); !errors.Is(err, sidecar.ErrSmtpProofKeySet) || session != nil {
			t.Errorf("a preamble naming %s: %v", name, err)
		}
	}
}

// A set past its not_after is dropped at start, its directory unread, so a preamble that names it answers as
// one naming a set never listed; a set whose not_after is ahead is held until then and no longer; and a file
// that lists no set gives a verifier that holds none.
func TestBlockVerifierDropsASetPastItsNotAfter(t *testing.T) {
	home := t.TempDir()
	kept := writeThrowawayKeySet(t, home, "kept")
	retiring := writeThrowawayKeySet(t, home, "retiring")
	// the retired set's directory is gone: a set past its not_after needs none
	retiredId := sha256.Sum256([]byte("a retired set's keyset.json"))
	retired := hex.EncodeToString(retiredId[:])
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(testProofKeysConfig+"proof_key_sets:\n"+
		"  - {id: "+kept+", directory: smtpproof-keys/kept}\n"+
		"  - {id: "+retiring+", directory: smtpproof-keys/retiring, not_after: 2026-12-01T00:00:00Z}\n"+
		"  - {id: "+retired+", directory: smtpproof-keys/removed, not_after: 2026-11-01T00:00:00Z}\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewProofVerifier(config, home, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{kept: true, retiring: true, retired: false} {
		preamble := testProofPreamble(id)
		if verifier.HoldsKeySet(preamble.ProofKeySet) != want {
			t.Errorf("holds %s = %v, want %v", id[:8], !want, want)
		}
		if _, err := verifier.NewSmtpProofSession(preamble); errors.Is(err, sidecar.ErrSmtpProofKeySet) == want {
			t.Errorf("a preamble naming %s: %v", id[:8], err)
		}
	}
	// a block started after the retiring set's not_after holds the kept set alone
	later, err := NewProofVerifier(config, home, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !later.HoldsKeySet(testProofPreamble(kept).ProofKeySet) || later.HoldsKeySet(testProofPreamble(retiring).ProofKeySet) {
		t.Error("a start at the retiring set's not_after did not drop it alone")
	}

	none, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(testProofKeysConfig), 0)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := NewProofVerifier(none, home, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	if err != nil || empty == nil {
		t.Fatalf("a file with no proof_key_sets: %v", err)
	}
	if empty.HoldsKeySet(testProofPreamble(kept).ProofKeySet) {
		t.Error("a verifier with no set holds one")
	}
	if _, err := empty.NewSmtpProofSession(testProofPreamble(kept)); !errors.Is(err, sidecar.ErrSmtpProofKeySet) {
		t.Errorf("a verifier with no set: %v", err)
	}
}

// A listed set that does not open gives no verifier at all, and the block built without one is not ready and
// says the verifier is what it lacks: a directory holding another set's keyset.json, one with none, a
// keyset.json that hashes to its id and is no key set, and a relative directory with no config home. No error
// names a path.
func TestBlockHasNoVerifierWhenAListedSetDoesNotOpen(t *testing.T) {
	home := t.TempDir()
	good := writeThrowawayKeySet(t, home, "good")
	writeThrowawayKeySet(t, home, "other")
	notASet := filepath.Join(home, "smtpproof-keys", "not-a-set")
	if err := os.MkdirAll(notASet, 0o755); err != nil {
		t.Fatal(err)
	}
	notASetJson := []byte(`{"version": 1, "shapes": []}`)
	if err := os.WriteFile(filepath.Join(notASet, "keyset.json"), notASetJson, 0o644); err != nil {
		t.Fatal(err)
	}
	notASetId := sha256.Sum256(notASetJson)
	for name, entry := range map[string]struct {
		entries string
		home    string
	}{
		"another set's keyset.json":     {"  - {id: " + good + ", directory: smtpproof-keys/good}\n  - {id: " + good[:62] + "00, directory: smtpproof-keys/other}\n", home},
		"no keyset.json":                {"  - {id: " + good + ", directory: smtpproof-keys/absent}\n", home},
		"a keyset.json that is not one": {"  - {id: " + hex.EncodeToString(notASetId[:]) + ", directory: smtpproof-keys/not-a-set}\n", home},
		"no config home":                {"  - {id: " + good + ", directory: smtpproof-keys/good}\n", ""},
	} {
		config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(testProofKeysConfig+"proof_key_sets:\n"+entry.entries), 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		verifier, err := NewProofVerifier(config, entry.home, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
		if err == nil || verifier != nil {
			t.Errorf("%s: a verifier was built", name)
			continue
		}
		if strings.Contains(err.Error(), home) || strings.Contains(err.Error(), "smtpproof-keys") {
			t.Errorf("%s: the error names a path: %v", name, err)
		}
	}

	// the block, as blockDeps leaves it after such an error: no verifier, not ready, and it says so
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(testProofKeysConfig+"proof_key_sets:\n  - {id: "+good+", directory: smtpproof-keys/other}\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	deps := &HostDeps{Log: &testLog{}}
	if verifier, err := NewProofVerifier(config, home, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)); err == nil {
		deps.Verifier = verifier
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	deps.Quota, deps.Redis, deps.Gates, deps.Clock = host.Quota, host.Redis, host.Gates, clock
	block, err := NewBlockHost(ctx, DefaultHostSettings(config), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	if block.Ready() || !slices.ContainsFunc(block.Missing(), func(text string) bool { return strings.Contains(text, "the proof verifier") }) {
		t.Fatalf("a block whose key set did not open is ready, or lacks only %q", block.Missing())
	}
}

// config/local's smtpedge.yml lists one throwaway key set beside it, which opens against its id: the local
// block's verifier holds that set and no other, and the set names no shape, so nothing verifies under it.
func TestLocalSmtpEdgeConfigKeySetOpens(t *testing.T) {
	configHome := filepath.Join("..", "..", "config", "local")
	data, err := os.ReadFile(filepath.Join(configHome, "smtpedge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := smtpedgeservice.LoadSmtpEdgeConfig(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ProofKeySets) != 1 || filepath.IsAbs(config.ProofKeySets[0].Directory) || !config.ProofKeySets[0].NotAfter.IsZero() {
		t.Fatalf("config/local lists %+v", config.ProofKeySets)
	}
	keySetJson, err := os.ReadFile(filepath.Join(configHome, config.ProofKeySets[0].Directory, "keyset.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(keySetJson), `"shapes": []`) {
		t.Error("config/local's key set names a shape: a throwaway set holds no key")
	}
	verifier, err := NewProofVerifier(config, configHome, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	local := hex.EncodeToString(config.ProofKeySets[0].Id)
	if !verifier.HoldsKeySet(testProofPreamble(local).ProofKeySet) {
		t.Error("the local verifier does not hold config/local's set")
	}
	other := sha256.Sum256([]byte("another set"))
	if _, err := verifier.NewSmtpProofSession(testProofPreamble(hex.EncodeToString(other[:]))); !errors.Is(err, sidecar.ErrSmtpProofKeySet) {
		t.Errorf("the local verifier takes another set: %v", err)
	}
}
