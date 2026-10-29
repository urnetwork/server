package controller

// The validator head summary ingest (sn docs/spec/head-summary-v1.md): a
// signed canonical summary decodes and verifies; a non-canonical,
// inconsistent or wrongly signed one is refused; only an accepted validator's
// summary is stored; and a tempo keeps the summary with the latest snapshot,
// so a replay changes nothing. Keys and identities are synthetic.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A synthetic sr25519 validator key.
type snHeadSummaryTestKey struct {
	secret *schnorrkel.SecretKey
	hotkey [32]byte
}

func newSnHeadSummaryTestKey(t testing.TB, seed [32]byte) *snHeadSummaryTestKey {
	t.Helper()
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		t.Fatal(err)
	}
	secret := mini.ExpandEd25519()
	public, err := secret.Public()
	if err != nil {
		t.Fatal(err)
	}
	return &snHeadSummaryTestKey{secret: secret, hotkey: public.Encode()}
}

func (self *snHeadSummaryTestKey) hex() string {
	return "0x" + hex.EncodeToString(self.hotkey[:])
}

// Signs the summary as the validator does and returns its canonical body.
func (self *snHeadSummaryTestKey) sign(t testing.TB, summary *SnHeadSummary) []byte {
	t.Helper()
	digest := SnHeadSummaryDigest(summary)
	signature, err := self.secret.Sign(schnorrkel.NewSigningContext([]byte("substrate"), digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	encoded := signature.Encode()
	summary.SigningHash = "sha256:" + hex.EncodeToString(digest[:])
	summary.Signature = "0x" + hex.EncodeToString(encoded[:])
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func snHeadSummaryTestHash(b byte) string {
	return "0x" + strings.Repeat(fmt.Sprintf("%02x", b), 32)
}

// Three eligible UIDs with two head seats: UID 42 first; UIDs 7 and 9 tie,
// so the lower UID ranks first; UID 7 has two generations of one fleet.
func snHeadSummaryTestEnvelope(validator string) *SnHeadSummary {
	return &SnHeadSummary{
		Schema: SnHeadSummarySchemaV1, Netuid: 25, NativeEpoch: 51234, NativeSnapshotBlock: 6_123_456, NativeSnapshotHash: snHeadSummaryTestHash(0x6d),
		EvmSnapshotBlock: 6_123_450, EvmSnapshotHash: snHeadSummaryTestHash(0x2a), SettlementEpoch: 7, PolicyId: 1, PolicyHash: snHeadSummaryTestHash(0x9c),
		MeasurementArtifactHash: "sha256:" + strings.Repeat("41", 32), ValidatorHotkey: validator, ValidatorUid: 1, MaximumHeadFleets: 2,
		Entries: []SnHeadSummaryEntry{
			{Rank: 1, Uid: 42, Hotkey: snHeadSummaryTestHash(0x8f), Score: SnHeadSummaryRational{Numerator: "7", Denominator: "4"}, Selected: true,
				Fleets: []SnHeadSummaryFleet{{FleetId: snHeadSummaryTestHash(0x11), Generation: 3}}},
			{Rank: 2, Uid: 7, Hotkey: snHeadSummaryTestHash(0x87), Score: SnHeadSummaryRational{Numerator: "1", Denominator: "1"}, Selected: true,
				Fleets: []SnHeadSummaryFleet{{FleetId: snHeadSummaryTestHash(0x12), Generation: 1}, {FleetId: snHeadSummaryTestHash(0x12), Generation: 2}}},
			{Rank: 3, Uid: 9, Hotkey: snHeadSummaryTestHash(0x89), Score: SnHeadSummaryRational{Numerator: "1", Denominator: "1"}, Selected: false,
				Fleets: []SnHeadSummaryFleet{{FleetId: snHeadSummaryTestHash(0x13), Generation: 1}}},
		},
		SelectedUids: []uint16{42, 7}, RejectedUids: []uint16{9},
		SignedAt: "2026-10-29T18:04:05.123456789Z", SignatureScheme: "sr25519",
	}
}

func requireSnHeadSummaryInvalid(t testing.TB, err error, label string) {
	t.Helper()
	if !errors.Is(err, ErrSnHeadSummaryInvalid) {
		t.Fatalf("%s: %v, want an invalid summary", label, err)
	}
}

// A signed canonical summary decodes and verifies, and its digest is SHA-256
// over the domain, a zero byte and the encoding with an empty signing hash
// and signature.
func TestSnHeadSummaryDecodesAndVerifiesASignedSummary(t *testing.T) {
	key := newSnHeadSummaryTestKey(t, [32]byte{0x01})
	body := key.sign(t, snHeadSummaryTestEnvelope(key.hex()))
	summary, err := DecodeSnHeadSummary(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySnHeadSummarySignature(summary); err != nil {
		t.Fatal(err)
	}
	unsigned := *summary
	unsigned.SigningHash, unsigned.Signature = "", ""
	encoded, _ := json.Marshal(&unsigned)
	if !bytes.Contains(encoded, []byte(`"signing_hash":"","signature":""}`)) {
		t.Fatalf("the unsigned encoding keeps the members in place: %s", encoded)
	}
	want := sha256.Sum256(append(append([]byte("urnetwork/validator/head-summary/v1"), 0), encoded...))
	if SnHeadSummaryDigest(summary) != want || summary.SigningHash != "sha256:"+hex.EncodeToString(want[:]) {
		t.Fatal("the digest differs from the spec's")
	}
	// an empty head is a valid summary with three empty arrays
	empty := snHeadSummaryTestEnvelope(key.hex())
	empty.Entries, empty.SelectedUids, empty.RejectedUids = []SnHeadSummaryEntry{}, []uint16{}, []uint16{}
	emptyBody := key.sign(t, empty)
	if !bytes.Contains(emptyBody, []byte(`"entries":[],"selected_uids":[],"rejected_uids":[]`)) {
		t.Fatalf("empty arrays: %s", emptyBody)
	}
	if summary, err := DecodeSnHeadSummary(emptyBody); err != nil || VerifySnHeadSummarySignature(summary) != nil {
		t.Fatal("the empty head", err)
	}
}

// The body must be exactly the canonical encoding: whitespace, a trailing
// newline, reordered or unknown or duplicate members, a null array, trailing
// data, uppercase hex and an unknown schema are each refused.
func TestSnHeadSummaryRefusesNonCanonicalBodies(t *testing.T) {
	key := newSnHeadSummaryTestKey(t, [32]byte{0x02})
	body := key.sign(t, snHeadSummaryTestEnvelope(key.hex()))
	text := string(body)
	for name, mutated := range map[string]string{
		"whitespace":       strings.Replace(text, `"netuid":25`, `"netuid": 25`, 1),
		"trailing newline": text + "\n",
		"member order":     strings.Replace(text, `{"schema":"urnetwork-validator-head-summary-v1","netuid":25,`, `{"netuid":25,"schema":"urnetwork-validator-head-summary-v1",`, 1),
		"unknown member":   strings.Replace(text, `"netuid":25,`, `"netuid":25,"extra":1,`, 1),
		"duplicate member": strings.Replace(text, `"netuid":25,`, `"netuid":25,"netuid":25,`, 1),
		"null array":       strings.Replace(text, `"rejected_uids":[9]`, `"rejected_uids":null`, 1),
		"trailing data":    text + `{}`,
		"uppercase hex":    strings.Replace(text, snHeadSummaryTestHash(0x6d), "0x"+strings.ToUpper(snHeadSummaryTestHash(0x6d)[2:]), 1),
		"key case":         strings.Replace(text, `"netuid":25`, `"Netuid":25`, 1),
		"unknown schema":   strings.Replace(text, SnHeadSummarySchemaV1, "urnetwork-validator-head-summary-v2", 1),
		"exponent":         strings.Replace(text, `"settlement_epoch":7`, `"settlement_epoch":7e0`, 1),
	} {
		if mutated == text {
			t.Fatalf("%s did not change the body", name)
		}
		_, err := DecodeSnHeadSummary([]byte(mutated))
		requireSnHeadSummaryInvalid(t, err, name)
	}
	if _, err := DecodeSnHeadSummary(append(body, make([]byte, SnHeadSummaryMaxBytes)...)); err == nil {
		t.Fatal("a body over 16 MiB was decoded")
	}
}

// Every value is checked against the spec's tables, the rank order, the
// selection and the UID partitions, even when the summary is signed.
func TestSnHeadSummaryRefusesInconsistentValues(t *testing.T) {
	key := newSnHeadSummaryTestKey(t, [32]byte{0x03})
	for name, change := range map[string]func(*SnHeadSummary){
		"rank gap":    func(s *SnHeadSummary) { s.Entries[2].Rank = 4 },
		"score order": func(s *SnHeadSummary) { s.Entries[1].Score = SnHeadSummaryRational{Numerator: "2", Denominator: "1"} },
		"tie order": func(s *SnHeadSummary) {
			s.Entries[1].Uid, s.Entries[2].Uid, s.SelectedUids, s.RejectedUids = 9, 7, []uint16{42, 9}, []uint16{7}
		},
		"repeated uid": func(s *SnHeadSummary) { s.Entries[2].Uid, s.RejectedUids = 42, []uint16{42} },
		"selection": func(s *SnHeadSummary) {
			s.Entries[2].Selected, s.SelectedUids, s.RejectedUids = true, []uint16{42, 7, 9}, []uint16{}
		},
		"selected uids":          func(s *SnHeadSummary) { s.SelectedUids = []uint16{7, 42} },
		"rejected uids":          func(s *SnHeadSummary) { s.RejectedUids = []uint16{} },
		"unreduced score":        func(s *SnHeadSummary) { s.Entries[0].Score = SnHeadSummaryRational{Numerator: "14", Denominator: "8"} },
		"zero score":             func(s *SnHeadSummary) { s.Entries[2].Score = SnHeadSummaryRational{Numerator: "0", Denominator: "1"} },
		"leading zero":           func(s *SnHeadSummary) { s.Entries[0].Score = SnHeadSummaryRational{Numerator: "07", Denominator: "4"} },
		"zero hotkey":            func(s *SnHeadSummary) { s.Entries[0].Hotkey = snHeadSummaryTestHash(0) },
		"zero validator":         func(s *SnHeadSummary) { s.ValidatorHotkey = snHeadSummaryTestHash(0) },
		"fleet order":            func(s *SnHeadSummary) { s.Entries[1].Fleets[0].Generation, s.Entries[1].Fleets[1].Generation = 2, 1 },
		"duplicate fleet":        func(s *SnHeadSummary) { s.Entries[1].Fleets[1].Generation = 1 },
		"zero generation":        func(s *SnHeadSummary) { s.Entries[0].Fleets[0].Generation = 0 },
		"zero fleet":             func(s *SnHeadSummary) { s.Entries[0].Fleets[0].FleetId = snHeadSummaryTestHash(0) },
		"no fleet":               func(s *SnHeadSummary) { s.Entries[0].Fleets = []SnHeadSummaryFleet{} },
		"trailing fraction zero": func(s *SnHeadSummary) { s.SignedAt = "2026-10-29T18:04:05.120Z" },
		"local offset":           func(s *SnHeadSummary) { s.SignedAt = "2026-10-29T18:04:05+01:00" },
		"zero netuid":            func(s *SnHeadSummary) { s.Netuid = 0 },
		"zero policy":            func(s *SnHeadSummary) { s.PolicyId = 0 },
		"zero head seats":        func(s *SnHeadSummary) { s.MaximumHeadFleets = 0 },
		"zero snapshot":          func(s *SnHeadSummary) { s.NativeSnapshotBlock = 0 },
		"artifact address":       func(s *SnHeadSummary) { s.MeasurementArtifactHash = "sha256:41" },
		"scheme":                 func(s *SnHeadSummary) { s.SignatureScheme = "ed25519" },
		"too many entries": func(s *SnHeadSummary) {
			s.Entries, s.SelectedUids, s.RejectedUids = []SnHeadSummaryEntry{}, []uint16{}, []uint16{}
			for i := range 4097 {
				s.Entries = append(s.Entries, SnHeadSummaryEntry{Rank: uint32(i + 1), Uid: uint16(i), Hotkey: snHeadSummaryTestHash(0x77),
					Score: SnHeadSummaryRational{Numerator: "1", Denominator: "1"}, Selected: i < 2, Fleets: []SnHeadSummaryFleet{{FleetId: snHeadSummaryTestHash(0x11), Generation: 1}}})
				if i < 2 {
					s.SelectedUids = append(s.SelectedUids, uint16(i))
				} else {
					s.RejectedUids = append(s.RejectedUids, uint16(i))
				}
			}
		},
	} {
		summary := snHeadSummaryTestEnvelope(key.hex())
		change(summary)
		_, err := DecodeSnHeadSummary(key.sign(t, summary))
		requireSnHeadSummaryInvalid(t, err, name)
	}
}

// The signature binds every member: a member changed after signing breaks
// the signing hash; a recomputed hash with another key's signature, or the
// right key's signature over another digest, does not verify.
func TestSnHeadSummaryRefusesWrongSignatures(t *testing.T) {
	key, other := newSnHeadSummaryTestKey(t, [32]byte{0x04}), newSnHeadSummaryTestKey(t, [32]byte{0x05})
	signed := snHeadSummaryTestEnvelope(key.hex())
	key.sign(t, signed)

	tampered := *signed
	tampered.SettlementEpoch = 8
	body, _ := json.Marshal(&tampered)
	summary, err := DecodeSnHeadSummary(body)
	if err != nil {
		t.Fatal(err)
	}
	requireSnHeadSummaryInvalid(t, VerifySnHeadSummarySignature(summary), "tampered member")

	// signed by another key under this hotkey
	impostor := snHeadSummaryTestEnvelope(key.hex())
	other.sign(t, impostor)
	requireSnHeadSummaryInvalid(t, VerifySnHeadSummarySignature(impostor), "another key")

	// this key's signature over another summary's digest
	replayed := snHeadSummaryTestEnvelope(key.hex())
	replayed.NativeEpoch = 51235
	digest := SnHeadSummaryDigest(replayed)
	replayed.SigningHash, replayed.Signature = "sha256:"+hex.EncodeToString(digest[:]), signed.Signature
	requireSnHeadSummaryInvalid(t, VerifySnHeadSummarySignature(replayed), "a signature of another digest")

	// a well-formed signature of random bytes
	random := *signed
	var noise [64]byte
	rand.Read(noise[:])
	noise[63] |= 0x80
	random.Signature = "0x" + hex.EncodeToString(noise[:])
	requireSnHeadSummaryInvalid(t, VerifySnHeadSummarySignature(&random), "a random signature")

	if err := VerifySnHeadSummarySignature(signed); err != nil {
		t.Fatal("the signed summary", err)
	}
}

// The operator accepts the hotkeys st.yml lists, and only those; without a
// list, the hotkey that holds UID 1 with a validator permit at the newest
// incentive census, read in this tempo or the one before.
func TestSnHeadSummaryAcceptsTheConfiguredValidator(t *testing.T) {
	key, other := newSnHeadSummaryTestKey(t, [32]byte{0x06}), newSnHeadSummaryTestKey(t, [32]byte{0x07})
	summary := snHeadSummaryTestEnvelope(key.hex())
	holder := &model.SnIncentiveSnapshot{SubnetEpochIndex: 51233, Uids: []*model.SnIncentiveUid{
		{Uid: 0}, {Uid: 1, ValidatorPermit: true, HolderRead: true, Hotkey: key.hotkey},
	}}
	configured := &StConfig{Netuid: 25, ValidatorHotkeys: [][32]byte{other.hotkey}}
	if snHeadSummaryAccepted(configured, holder, summary, key.hotkey) || !snHeadSummaryAccepted(configured, nil, snHeadSummaryTestEnvelope(other.hex()), other.hotkey) {
		t.Fatal("a configured list accepts exactly its hotkeys")
	}
	unconfigured := &StConfig{Netuid: 25}
	if !snHeadSummaryAccepted(unconfigured, holder, summary, key.hotkey) {
		t.Fatal("UID 1's permitted holder was refused")
	}
	for name, test := range map[string]struct {
		cfg      *StConfig
		snapshot *model.SnIncentiveSnapshot
		change   func(*SnHeadSummary)
		hotkey   [32]byte
	}{
		"no census":       {cfg: unconfigured, hotkey: key.hotkey},
		"another hotkey":  {cfg: unconfigured, snapshot: holder, hotkey: other.hotkey},
		"another uid":     {cfg: unconfigured, snapshot: holder, change: func(s *SnHeadSummary) { s.ValidatorUid = 2 }, hotkey: key.hotkey},
		"another subnet":  {cfg: unconfigured, snapshot: holder, change: func(s *SnHeadSummary) { s.Netuid = 26 }, hotkey: key.hotkey},
		"an older census": {cfg: unconfigured, snapshot: &model.SnIncentiveSnapshot{SubnetEpochIndex: 51232, Uids: holder.Uids}, hotkey: key.hotkey},
		"no permit": {cfg: unconfigured, snapshot: &model.SnIncentiveSnapshot{SubnetEpochIndex: 51234, Uids: []*model.SnIncentiveUid{
			{Uid: 0}, {Uid: 1, HolderRead: true, Hotkey: key.hotkey},
		}}, hotkey: key.hotkey},
	} {
		summary := snHeadSummaryTestEnvelope(key.hex())
		if test.change != nil {
			test.change(summary)
		}
		if snHeadSummaryAccepted(test.cfg, test.snapshot, summary, test.hotkey) {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// Storage keeps one summary per (netuid, validator, native epoch): the first
// is stored, an identical replay and an earlier snapshot are kept out, a later
// snapshot replaces it, and the board reads each fleet identity's score, rank
// and selection. Another validator's summary is refused and stores nothing.
func TestSnUploadHeadSummaryKeepsOnePerTempo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var seed [32]byte
		rand.Read(seed[:])
		key := newSnHeadSummaryTestKey(t, seed)
		cfg := &StConfig{ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000027"), Netuid: 25, ValidatorHotkeys: [][32]byte{key.hotkey}}
		now := time.Date(2026, 10, 29, 18, 5, 0, 0, time.UTC)

		first := key.sign(t, snHeadSummaryTestEnvelope(key.hex()))
		if result, err := snUploadHeadSummary(ctx, cfg, nil, first, now); err != nil || result != SnHeadSummaryStored {
			t.Fatal("first upload", result, err)
		}
		stored := model.GetLatestSnHeadSummary(ctx, 25, &key.hotkey)
		if stored == nil || stored.NativeEpoch != 51234 || stored.NativeSnapshotBlock != 6_123_456 || stored.EntryCount != 3 || stored.SelectedCount != 2 ||
			!bytes.Equal(stored.Body, first) || len(stored.Fleets) != 4 || !stored.ReceivedAt.Equal(now) {
			t.Fatal("stored summary", stored)
		}
		var fleet7 [32]byte
		copy(fleet7[:], common.FromHex(snHeadSummaryTestHash(0x12)))
		var hotkey7 [32]byte
		copy(hotkey7[:], common.FromHex(snHeadSummaryTestHash(0x87)))
		score, ok := stored.FleetScore(model.SnFleetKey{FleetId: fleet7, Hotkey: hotkey7, Generation: 2, Uid: 7})
		if !ok || score.Rank != 2 || score.Score != 1 || !score.Selected || score.ScoreNumerator != "1" {
			t.Fatal("UID 7's second generation", score, ok)
		}
		if score := stored.Fleets[0]; score.Key.Uid != 42 || score.Rank != 1 || score.Score != 1.75 || score.ScoreNumerator != "7" || score.ScoreDenominator != "4" {
			t.Fatal("UID 42", score)
		}

		// an identical replay is answered and changes nothing
		if result, err := snUploadHeadSummary(ctx, cfg, nil, first, now.Add(time.Minute)); err != nil || result != SnHeadSummaryKept {
			t.Fatal("replay", result, err)
		}
		// a later snapshot of the same tempo replaces it
		later := snHeadSummaryTestEnvelope(key.hex())
		later.NativeSnapshotBlock = 6_123_470
		later.Entries = later.Entries[:1]
		later.SelectedUids, later.RejectedUids = []uint16{42}, []uint16{}
		laterBody := key.sign(t, later)
		if result, err := snUploadHeadSummary(ctx, cfg, nil, laterBody, now.Add(2*time.Minute)); err != nil || result != SnHeadSummaryStored {
			t.Fatal("later snapshot", result, err)
		}
		// an earlier snapshot is kept out
		earlier := snHeadSummaryTestEnvelope(key.hex())
		earlier.NativeSnapshotBlock = 6_123_460
		if result, err := snUploadHeadSummary(ctx, cfg, nil, key.sign(t, earlier), now.Add(3*time.Minute)); err != nil || result != SnHeadSummaryKept {
			t.Fatal("earlier snapshot", result, err)
		}
		stored = model.GetLatestSnHeadSummary(ctx, 25, &key.hotkey)
		if stored.NativeSnapshotBlock != 6_123_470 || stored.EntryCount != 1 || len(stored.Fleets) != 1 || !bytes.Equal(stored.Body, laterBody) {
			t.Fatal("the later snapshot did not replace the stored one", stored)
		}
		// the next tempo is the latest
		next := snHeadSummaryTestEnvelope(key.hex())
		next.NativeEpoch, next.NativeSnapshotBlock = 51235, 6_123_816
		if result, err := snUploadHeadSummary(ctx, cfg, nil, key.sign(t, next), now.Add(72*time.Minute)); err != nil || result != SnHeadSummaryStored {
			t.Fatal("next tempo", result, err)
		}
		if latest := model.GetLatestSnHeadSummary(ctx, 25, nil); latest == nil || latest.NativeEpoch != 51235 || latest.ValidatorHotkey != key.hotkey {
			t.Fatal("latest", latest)
		}

		// another validator, validly signed, is refused and stores nothing
		var otherSeed [32]byte
		rand.Read(otherSeed[:])
		other := newSnHeadSummaryTestKey(t, otherSeed)
		if _, err := snUploadHeadSummary(ctx, cfg, nil, other.sign(t, snHeadSummaryTestEnvelope(other.hex())), now); !errors.Is(err, ErrSnHeadSummaryNotAccepted) {
			t.Fatal("another validator", err)
		}
		if model.GetLatestSnHeadSummary(ctx, 25, &other.hotkey) != nil {
			t.Fatal("a refused summary was stored")
		}
		// a malformed body is refused before anything else
		if _, err := snUploadHeadSummary(ctx, cfg, nil, append([]byte(" "), first...), now); !errors.Is(err, ErrSnHeadSummaryInvalid) {
			t.Fatal("malformed", err)
		}
		// past the hotkey's hourly limit, uploads are rate limited
		limited := 0
		for range snHeadSummaryHotkeyLimit + 1 {
			if _, err := snUploadHeadSummary(ctx, cfg, nil, first, now); errors.Is(err, ErrSnHeadSummaryRateLimited) {
				limited++
			}
		}
		if limited == 0 {
			t.Fatal("the hotkey was never rate limited")
		}
		// summaries past the retention are removed
		if removed := model.RemoveSnHeadSummariesBefore(ctx, now.Add(8*24*time.Hour)); removed != 2 || model.GetLatestSnHeadSummary(ctx, 25, nil) != nil {
			t.Fatal("retention", removed)
		}
	})
}
