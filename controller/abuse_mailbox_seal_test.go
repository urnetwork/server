package controller

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The mailboxes' seal and key file (abuse_mailbox_seal.go). Pure: every key is
// synthetic, generated or derived here, and no test reads vault.

// A synthetic X-Wing pair, as the owner's offline pair would be, and the keys
// an abuse-mailbox.yml naming its public half holds.
func newTestAbuseMailboxKeys(t testing.TB) (hpke.PrivateKey, *AbuseMailboxKeys) {
	t.Helper()
	privateKey, err := hpke.MLKEM768X25519().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	macKey := make([]byte, 32)
	if _, err := rand.Read(macKey); err != nil {
		t.Fatal(err)
	}
	return privateKey, &AbuseMailboxKeys{
		MacKey:        macKey,
		SealPublicKey: privateKey.PublicKey(),
	}
}

// The suite is QD-441's, the framing QD-726's: X-Wing (0x647a), HKDF-SHA256 and
// AES-128-GCM, a 1,216-byte public key, and u8(1) || enc (1,120 B) || ct with
// AES-128-GCM's 16-byte tag.
func TestAbuseMailboxSealSuite(t *testing.T) {
	if id := hpke.MLKEM768X25519().ID(); id != 0x647a {
		t.Fatalf("KEM %#04x", id)
	}
	if id := hpke.HKDFSHA256().ID(); id != 0x0001 {
		t.Fatalf("KDF %#04x", id)
	}
	if id := hpke.AES128GCM().ID(); id != 0x0001 {
		t.Fatalf("AEAD %#04x", id)
	}
	if abuseMailboxSealInfo != "UR abuse 1.0 record" || abuseMailboxSealVersion != 1 {
		t.Fatalf("info %q, version %d", abuseMailboxSealInfo, abuseMailboxSealVersion)
	}
	_, keys := newTestAbuseMailboxKeys(t)
	if n := len(keys.SealPublicKey.Bytes()); n != 1216 {
		t.Fatalf("a public key of %d bytes", n)
	}
	plaintext := []byte(`{"a":1}`)
	sealed, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, DefaultAbuseMailboxSettings())
	if err != nil {
		t.Fatal(err)
	}
	if sealed[0] != 1 || len(sealed) != 1+1120+len(plaintext)+16 {
		t.Fatalf("sealed is %d bytes, version %d", len(sealed), sealed[0])
	}
}

// A sealed record opens with the private key to the bytes the mailer MACed,
// whose MAC verifies, directly and through the mail's body.
func TestAbuseMailboxSealOpensWithThePrivateKey(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, keys := newTestAbuseMailboxKeys(t)
	record := testAbuseRecord()
	plaintext, err := EncodeAbuseRecord(record, keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened %s, sealed %s", opened, plaintext)
	}

	body := EncodeAbuseMailboxBody(sealed, settings)
	decoded, err := DecodeAbuseMailboxBody(body, settings)
	if err != nil {
		t.Fatal(err)
	}
	opened, err = OpenAbuseMailboxRecord(privateKey, decoded, settings)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAbuseRecord(opened, keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ActionId != record.ActionId || parsed.CaseId != record.CaseId || parsed.Category != record.Category ||
		*parsed.NetworkId != *record.NetworkId || parsed.Kind != record.Kind || parsed.Target != record.Target ||
		!parsed.Until.Equal(*record.Until) || !parsed.Time.Equal(record.Time) {
		t.Fatalf("parsed %+v, want %+v", parsed, record)
	}
}

// Every seal draws its own encapsulation, so one record sealed twice gives two
// unrelated sealed forms, each of which opens.
func TestAbuseMailboxSealIsFreshEachTime(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, keys := newTestAbuseMailboxKeys(t)
	plaintext, err := EncodeAbuseRecord(testAbuseRecord(), keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	first, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first[1:1121], second[1:1121]) || bytes.Equal(first[1121:], second[1121:]) {
		t.Fatal("two seals share an encapsulation or a ciphertext")
	}
	for _, sealed := range [][]byte{first, second} {
		if opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings); err != nil || !bytes.Equal(opened, plaintext) {
			t.Fatalf("a seal opens as %q: %v", opened, err)
		}
	}
}

// A sealed record changed anywhere does not open, nor one opened with another
// key, nor a body that is not what a mailer wrote; each answers only that it
// does not open.
func TestAbuseMailboxSealRefusesTampering(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, keys := newTestAbuseMailboxKeys(t)
	otherPrivateKey, _ := newTestAbuseMailboxKeys(t)
	plaintext, err := EncodeAbuseRecord(testAbuseRecord(), keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(index int) []byte {
		changed := bytes.Clone(sealed)
		changed[index] ^= 0x01
		return changed
	}
	for _, c := range []struct {
		name   string
		sealed []byte
		key    hpke.PrivateKey
	}{
		{name: "another version", sealed: flip(0), key: privateKey},
		{name: "a changed encapsulation", sealed: flip(1 + 500), key: privateKey},
		{name: "the encapsulation's X25519 share changed", sealed: flip(1120), key: privateKey},
		{name: "a changed ciphertext", sealed: flip(1 + 1120 + 10), key: privateKey},
		{name: "a changed tag", sealed: flip(len(sealed) - 1), key: privateKey},
		{name: "one byte short", sealed: sealed[:len(sealed)-1], key: privateKey},
		{name: "one byte more", sealed: append(bytes.Clone(sealed), 0), key: privateKey},
		{name: "the header alone", sealed: sealed[:1+1120], key: privateKey},
		{name: "nothing", sealed: nil, key: privateKey},
		{name: "another key", sealed: sealed, key: otherPrivateKey},
		{name: "no key", sealed: sealed, key: nil},
	} {
		if opened, err := OpenAbuseMailboxRecord(c.key, c.sealed, settings); !errors.Is(err, ErrAbuseMailboxSealOpen) || opened != nil {
			t.Errorf("%s opens as %q: %v", c.name, opened, err)
		}
	}

	// the mail's body: a changed character, a character outside base64, a cut
	body := EncodeAbuseMailboxBody(sealed, settings)
	middle := len(body) / 2
	for body[middle] == '\n' || body[middle] == 'A' {
		middle += 1
	}
	changedBody := body[:middle] + "A" + body[middle+1:]
	for _, c := range []struct {
		name string
		body string
	}{
		{name: "a changed character", body: changedBody},
		{name: "a character outside base64", body: strings.Replace(body, body[3:4], "*", 1)},
		{name: "the last line cut", body: body[:strings.LastIndex(strings.TrimSuffix(body, "\n"), "\n")+1]},
		{name: "an empty body", body: ""},
	} {
		decoded, err := DecodeAbuseMailboxBody(c.body, settings)
		if err == nil {
			decoded, err = OpenAbuseMailboxRecord(privateKey, decoded, settings)
		}
		if !errors.Is(err, ErrAbuseMailboxSealOpen) {
			t.Errorf("%s opens as %q: %v", c.name, decoded, err)
		}
	}
}

// Anyone holding the public key can seal, so a record sealed by someone without
// the MAC key opens but does not verify, and a restore applies nothing from it.
func TestAbuseMailboxForgeryOpensButDoesNotVerify(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, keys := newTestAbuseMailboxKeys(t)
	forgedRecord := testAbuseRecord()
	forgedRecord.Kind = model.AbuseActionKindLift
	forged, err := EncodeAbuseRecord(forgedRecord, []byte("a forger's synthetic mac key"))
	if err != nil {
		t.Fatal(err)
	}
	unmaced, err := forgedRecord.canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, plaintext := range [][]byte{forged, unmaced} {
		sealed, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings)
		if err != nil {
			t.Fatal(err)
		}
		if parsed, err := ParseAbuseRecord(opened, keys.MacKey); !errors.Is(err, ErrAbuseRecordMac) {
			t.Errorf("a forgery parses as %+v: %v", parsed, err)
		}
	}
}

// The body is standard base64 in lines of at most 76 characters, each ending
// in a newline, and reads back however a mailbox rewraps it.
func TestAbuseMailboxBodyLines(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	sealed := make([]byte, 1+1120+300+16)
	if _, err := rand.Read(sealed); err != nil {
		t.Fatal(err)
	}
	body := EncodeAbuseMailboxBody(sealed, settings)
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("the body does not end its last line")
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	encodedCharCount := base64.StdEncoding.EncodedLen(len(sealed))
	if want := (encodedCharCount + 75) / 76; len(lines) != want {
		t.Fatalf("%d lines, want %d", len(lines), want)
	}
	for _, line := range lines {
		if len(line) == 0 || 76 < len(line) {
			t.Fatalf("a line of %d characters", len(line))
		}
		if strings.Trim(line, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") != "" {
			t.Fatalf("a line outside base64: %q", line)
		}
	}
	rewrapped := "  " + strings.ReplaceAll(body, "\n", "\r\n\t")
	for _, form := range []string{body, rewrapped, strings.ReplaceAll(body, "\n", "")} {
		decoded, err := DecodeAbuseMailboxBody(form, settings)
		if err != nil || !bytes.Equal(decoded, sealed) {
			t.Fatalf("a rewrapped body decodes to %d bytes: %v", len(decoded), err)
		}
	}

	oneLine := *settings
	oneLine.BodyLineCharCount = 0
	if body := EncodeAbuseMailboxBody(sealed, &oneLine); strings.Count(body, "\n") != 1 {
		t.Fatalf("a non-positive line length wrote %d lines", strings.Count(body, "\n"))
	}
}

// A record past MaxSealedByteCount is not sealed and a body past it is not
// decoded; a key of another suite neither seals nor opens.
func TestAbuseMailboxSealBounds(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, keys := newTestAbuseMailboxKeys(t)
	fits := make([]byte, settings.MaxSealedByteCount-1-1120-16)
	sealed, err := SealAbuseMailboxRecord(keys.SealPublicKey, fits, settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != settings.MaxSealedByteCount {
		t.Fatalf("sealed %d bytes", len(sealed))
	}
	if _, err := OpenAbuseMailboxRecord(privateKey, sealed, settings); err != nil {
		t.Fatal(err)
	}
	if _, err := SealAbuseMailboxRecord(keys.SealPublicKey, append(fits, 0), settings); !errors.Is(err, ErrAbuseMailboxSeal) {
		t.Fatalf("a record one byte past the bound seals: %v", err)
	}
	tooLong := strings.Repeat("A", base64.StdEncoding.EncodedLen(settings.MaxSealedByteCount+1))
	if _, err := DecodeAbuseMailboxBody(tooLong, settings); !errors.Is(err, ErrAbuseMailboxSealOpen) {
		t.Fatalf("a body past the bound decodes: %v", err)
	}

	x25519PrivateKey, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SealAbuseMailboxRecord(x25519PrivateKey.PublicKey(), []byte("{}"), settings); !errors.Is(err, ErrAbuseMailboxSeal) {
		t.Fatalf("an X25519 key seals: %v", err)
	}
	if _, err := SealAbuseMailboxRecord(nil, []byte("{}"), settings); !errors.Is(err, ErrAbuseMailboxSeal) {
		t.Fatalf("no key seals: %v", err)
	}
	if _, err := OpenAbuseMailboxRecord(x25519PrivateKey, sealed, settings); !errors.Is(err, ErrAbuseMailboxSealOpen) {
		t.Fatalf("an X25519 key opens: %v", err)
	}
}

// The key file AbuseMailboxKeyFile writes reads back to its keys, and so does
// one whose values a YAML block scalar has wrapped.
func TestAbuseMailboxKeyFileParses(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	_, keys := newTestAbuseMailboxKeys(t)
	parsed, err := ParseAbuseMailboxKeys(AbuseMailboxKeyFile(keys), settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.MacKey, keys.MacKey) || !bytes.Equal(parsed.SealPublicKey.Bytes(), keys.SealPublicKey.Bytes()) {
		t.Fatal("the key file does not read back to its keys")
	}

	encodedPublicKey := base64.StdEncoding.EncodeToString(keys.SealPublicKey.Bytes())
	wrapped := &strings.Builder{}
	wrapped.WriteString("# the abuse and legal-request mailboxes' keys\n")
	wrapped.WriteString(abuseMailboxMacKeyName + ": " + base64.StdEncoding.EncodeToString(keys.MacKey) + "\n")
	wrapped.WriteString(abuseMailboxSealPublicKeyName + ": |\n")
	for 0 < len(encodedPublicKey) {
		take := min(64, len(encodedPublicKey))
		wrapped.WriteString("    " + encodedPublicKey[:take] + "\n")
		encodedPublicKey = encodedPublicKey[take:]
	}
	parsed, err = ParseAbuseMailboxKeys([]byte(wrapped.String()), settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.SealPublicKey.Bytes(), keys.SealPublicKey.Bytes()) {
		t.Fatal("a wrapped public key does not read back")
	}
}

// A key file is refused unless it holds exactly its two keys, well formed: a
// private key beside them refuses the whole file. No refusal names a value.
func TestAbuseMailboxKeyFileRefusals(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, keys := newTestAbuseMailboxKeys(t)
	privateKeyBytes, err := privateKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	macKey := base64.StdEncoding.EncodeToString(keys.MacKey)
	publicKey := base64.StdEncoding.EncodeToString(keys.SealPublicKey.Bytes())
	encodedPrivateKey := base64.StdEncoding.EncodeToString(privateKeyBytes)
	randomPublicKey := make([]byte, 1216)
	if _, err := rand.Read(randomPublicKey); err != nil {
		t.Fatal(err)
	}
	line := func(name string, value string) string {
		return name + ": " + value + "\n"
	}
	macLine := line(abuseMailboxMacKeyName, macKey)
	publicLine := line(abuseMailboxSealPublicKeyName, publicKey)
	for _, c := range []struct {
		name string
		file string
	}{
		{name: "an empty file", file: ""},
		{name: "a file that is not a mapping", file: "- " + macKey + "\n"},
		{name: "a file that does not parse", file: macLine + "\t:" + publicKey + "\n  - ["},
		{name: "no mac key", file: publicLine},
		{name: "an empty mac key", file: line(abuseMailboxMacKeyName, "") + publicLine},
		{name: "a mac key that is a list", file: abuseMailboxMacKeyName + ": [" + macKey + "]\n" + publicLine},
		{name: "a mac key that is not base64", file: line(abuseMailboxMacKeyName, "not*base64") + publicLine},
		{name: "a mac key in base64url", file: line(abuseMailboxMacKeyName, strings.TrimRight(base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)), "=")) + publicLine},
		{name: "a 31-byte mac key", file: line(abuseMailboxMacKeyName, base64.StdEncoding.EncodeToString(keys.MacKey[:31])) + publicLine},
		{name: "a 33-byte mac key", file: line(abuseMailboxMacKeyName, base64.StdEncoding.EncodeToString(append(bytes.Clone(keys.MacKey), 1))) + publicLine},
		{name: "an all-zero mac key", file: line(abuseMailboxMacKeyName, base64.StdEncoding.EncodeToString(make([]byte, 32))) + publicLine},
		{name: "no seal key", file: macLine},
		{name: "a seal key that is not base64", file: macLine + line(abuseMailboxSealPublicKeyName, "not*base64")},
		{name: "a seal key one byte short", file: macLine + line(abuseMailboxSealPublicKeyName, base64.StdEncoding.EncodeToString(keys.SealPublicKey.Bytes()[:1215]))},
		{name: "1,216 random bytes", file: macLine + line(abuseMailboxSealPublicKeyName, base64.StdEncoding.EncodeToString(randomPublicKey))},
		{name: "the private key in place of the public", file: macLine + line(abuseMailboxSealPublicKeyName, encodedPrivateKey)},
		{name: "the private key beside them", file: macLine + publicLine + line("abuse_mailbox_seal_private_key", encodedPrivateKey)},
		{name: "a value misplaced as a key", file: macLine + publicLine + line(encodedPrivateKey, "x")},
	} {
		parsed, err := ParseAbuseMailboxKeys([]byte(c.file), settings)
		if !errors.Is(err, ErrAbuseMailboxKeys) || parsed != nil {
			t.Errorf("%s parses as %+v: %v", c.name, parsed, err)
			continue
		}
		for _, value := range []string{macKey, publicKey, encodedPrivateKey, hex.EncodeToString(keys.MacKey)} {
			if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), value[:16]) {
				t.Errorf("%s: the refusal names a value: %v", c.name, err)
			}
		}
	}
}

// A process reads the file from its vault by name, and one without the file
// holds no keys.
func TestAbuseMailboxKeysLoadFromTheVault(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	if _, err := LoadAbuseMailboxKeys(settings); !errors.Is(err, ErrAbuseMailboxKeys) {
		t.Fatalf("no file answered %v", err)
	}
	_, keys := newTestAbuseMailboxKeys(t)
	pop := server.Vault.PushSimpleResource(AbuseMailboxKeysResourceName, AbuseMailboxKeyFile(keys))
	loaded, err := LoadAbuseMailboxKeys(settings)
	if err != nil {
		pop()
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.MacKey, keys.MacKey) || !bytes.Equal(loaded.SealPublicKey.Bytes(), keys.SealPublicKey.Bytes()) {
		pop()
		t.Fatal("the vault's file does not read back to its keys")
	}
	pop()
	if _, err := LoadAbuseMailboxKeys(settings); !errors.Is(err, ErrAbuseMailboxKeys) {
		t.Fatalf("a removed file answered %v", err)
	}
}

// A sealed record mailed by an earlier build still opens: testdata's vector
// pins the version byte, info, suite, framing, body and MAC form, against a
// pair derived from a synthetic seed. UPDATE_ABUSE_MAILBOX_VECTOR=1 writes a
// new vector, which only a deliberate format change does.
type testAbuseMailboxVector struct {
	Comment string `json:"comment"`
	// the canonical JSON with its MAC, as mailed before the seal
	Plaintext string `json:"plaintext"`
	// the mail's text/plain body
	Body string `json:"body"`
}

const testAbuseMailboxVectorPath = "testdata/abuse_mailbox_seal_vector.json"

// The vector's pair and MAC key: derived from fixed synthetic bytes, so the
// test needs no private key on disk.
func testAbuseMailboxVectorKeys(t testing.TB) (hpke.PrivateKey, []byte) {
	t.Helper()
	privateKey, err := hpke.MLKEM768X25519().DeriveKeyPair([]byte("synthetic abuse mailbox seal ikm, test only"))
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, []byte("synthetic abuse mailbox mac key!")
}

// The vector's record: synthetic ids and times.
func testAbuseMailboxVectorRecord(t testing.TB) *AbuseRecord {
	t.Helper()
	parse := func(id string) server.Id {
		parsed, err := server.ParseId(id)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	networkId := parse("01900000-0000-7000-8000-00000000a003")
	until := time.Date(2026, 12, 1, 9, 0, 0, 500*int(time.Millisecond), time.UTC)
	return &AbuseRecord{
		ActionId:  parse("01900000-0000-7000-8000-00000000a001"),
		CaseId:    parse("01900000-0000-7000-8000-00000000a002"),
		Category:  model.AbuseCategoryPhishing,
		NetworkId: &networkId,
		Kind:      model.AbuseActionKindHostKill,
		Target:    "login.vector.example",
		Until:     &until,
		Time:      time.Date(2026, 11, 14, 18, 30, 0, 125*int(time.Millisecond), time.UTC),
	}
}

func TestAbuseMailboxSealVectorOpens(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, macKey := testAbuseMailboxVectorKeys(t)
	record := testAbuseMailboxVectorRecord(t)
	plaintext, err := EncodeAbuseRecord(record, macKey)
	if err != nil {
		t.Fatal(err)
	}

	if os.Getenv("UPDATE_ABUSE_MAILBOX_VECTOR") == "1" {
		sealed, err := SealAbuseMailboxRecord(privateKey.PublicKey(), plaintext, settings)
		if err != nil {
			t.Fatal(err)
		}
		vector := &testAbuseMailboxVector{
			Comment:   "A sealed §21.10 record (QD-726) under a synthetic X-Wing pair and MAC key, which abuse_mailbox_seal_test.go derives; nothing here is a real key.",
			Plaintext: string(plaintext),
			Body:      EncodeAbuseMailboxBody(sealed, settings),
		}
		out, err := json.MarshalIndent(vector, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(testAbuseMailboxVectorPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(testAbuseMailboxVectorPath, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(testAbuseMailboxVectorPath)
	if err != nil {
		t.Fatal(err)
	}
	vector := &testAbuseMailboxVector{}
	if err := json.Unmarshal(data, vector); err != nil {
		t.Fatal(err)
	}
	if vector.Plaintext != string(plaintext) {
		t.Fatalf("the record's canonical form moved:\n%s\nwant\n%s", plaintext, vector.Plaintext)
	}
	sealed, err := DecodeAbuseMailboxBody(vector.Body, settings)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAbuseMailboxRecord(privateKey, sealed, settings)
	if err != nil {
		t.Fatalf("the vector does not open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("the vector opens to %s", opened)
	}
	parsed, err := ParseAbuseRecord(opened, macKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ActionId != record.ActionId || parsed.Target != record.Target || !parsed.Time.Equal(record.Time) {
		t.Fatalf("the vector parses as %+v", parsed)
	}
	if EncodeAbuseMailboxBody(sealed, settings) != vector.Body {
		t.Fatal("the vector's body is not in the body's form")
	}
}

// The private key file AbuseMailboxSealPrivateKeyFile writes is QD-832's one
// line, the seed in standard base64, and reads back to a key that opens a
// record sealed to the public key, its MAC verifying. So does a file an owner
// has reformatted: a comment, quotes, a wrapped value, no final newline, CRLF.
func TestAbuseMailboxSealPrivateKeyFileParses(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, keys := newTestAbuseMailboxKeys(t)
	seed, err := privateKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) != 32 {
		t.Fatalf("a seed of %d bytes", len(seed))
	}
	file, err := AbuseMailboxSealPrivateKeyFile(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encodedSeed := base64.StdEncoding.EncodeToString(seed)
	if string(file) != "abuse_mailbox_seal_private_key: "+encodedSeed+"\n" {
		t.Fatalf("the private key file is not its one line: %d bytes", len(file))
	}

	plaintext, err := EncodeAbuseRecord(testAbuseRecord(), keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{
		string(file),
		"# the abuse mailboxes' private key, kept offline\n" + string(file),
		abuseMailboxSealPrivateKeyName + ": \"" + encodedSeed + "\"\n",
		abuseMailboxSealPrivateKeyName + ": |\n    " + encodedSeed[:20] + "\n    " + encodedSeed[20:] + "\n",
		abuseMailboxSealPrivateKeyName + ": " + encodedSeed,
		abuseMailboxSealPrivateKeyName + ": " + encodedSeed + "\r\n",
	} {
		parsed, err := ParseAbuseMailboxSealPrivateKey([]byte(form))
		if err != nil {
			t.Fatalf("a form of %d bytes: %v", len(form), err)
		}
		parsedSeed, err := parsed.Bytes()
		if err != nil || !bytes.Equal(parsedSeed, seed) {
			t.Fatalf("a form of %d bytes reads back to another seed: %v", len(form), err)
		}
		if !bytes.Equal(parsed.PublicKey().Bytes(), keys.SealPublicKey.Bytes()) {
			t.Fatalf("a form of %d bytes reads back to another pair", len(form))
		}
		opened, err := OpenAbuseMailboxRecord(parsed, sealed, settings)
		if err != nil || !bytes.Equal(opened, plaintext) {
			t.Fatalf("a form of %d bytes does not open the record: %v", len(form), err)
		}
		if _, err := ParseAbuseRecord(opened, keys.MacKey); err != nil {
			t.Fatal(err)
		}
	}
}

// A private key file is refused unless it is one document holding exactly its
// one key, the base64 of a 32-byte seed: abuse-mailbox.yml's keys beside it, a
// second document, the public key in its place and every malformed value
// included. No refusal names a value.
func TestAbuseMailboxSealPrivateKeyFileRefusals(t *testing.T) {
	privateKey, keys := newTestAbuseMailboxKeys(t)
	seed, err := privateKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	encodedSeed := base64.StdEncoding.EncodeToString(seed)
	macKey := base64.StdEncoding.EncodeToString(keys.MacKey)
	publicKey := base64.StdEncoding.EncodeToString(keys.SealPublicKey.Bytes())
	line := func(name string, value string) string {
		return name + ": " + value + "\n"
	}
	seedLine := line(abuseMailboxSealPrivateKeyName, encodedSeed)
	for _, c := range []struct {
		name string
		file string
	}{
		{name: "an empty file", file: ""},
		{name: "a comment alone", file: "# nothing here\n"},
		{name: "a file that is not a mapping", file: "- " + encodedSeed + "\n"},
		{name: "a file that does not parse", file: seedLine + "\t:" + encodedSeed + "\n  - ["},
		{name: "an empty value", file: line(abuseMailboxSealPrivateKeyName, "")},
		{name: "a value that is a list", file: abuseMailboxSealPrivateKeyName + ": [" + encodedSeed + "]\n"},
		{name: "a value that is a mapping", file: abuseMailboxSealPrivateKeyName + ": {seed: " + encodedSeed + "}\n"},
		{name: "the seed under another key", file: line("abuse_mailbox_seal_seed", encodedSeed)},
		{name: "the mac key beside it", file: seedLine + line(abuseMailboxMacKeyName, macKey)},
		{name: "the public key beside it", file: seedLine + line(abuseMailboxSealPublicKeyName, publicKey)},
		{name: "abuse-mailbox.yml in its place", file: string(AbuseMailboxKeyFile(keys))},
		{name: "a value misplaced as a key", file: seedLine + line(encodedSeed, "x")},
		{name: "the key twice", file: seedLine + seedLine},
		{name: "a second document", file: seedLine + "---\n" + seedLine},
		{name: "a second, empty document", file: seedLine + "---\n"},
		{name: "a value that is not base64", file: line(abuseMailboxSealPrivateKeyName, "not*base64")},
		{name: "a value in base64url", file: line(abuseMailboxSealPrivateKeyName, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)))},
		{name: "a value in hex", file: line(abuseMailboxSealPrivateKeyName, hex.EncodeToString(seed))},
		{name: "a 31-byte seed", file: line(abuseMailboxSealPrivateKeyName, base64.StdEncoding.EncodeToString(seed[:31]))},
		{name: "a 33-byte seed", file: line(abuseMailboxSealPrivateKeyName, base64.StdEncoding.EncodeToString(append(bytes.Clone(seed), 1)))},
		{name: "the public key in place of the seed", file: line(abuseMailboxSealPrivateKeyName, publicKey)},
	} {
		parsed, err := ParseAbuseMailboxSealPrivateKey([]byte(c.file))
		if !errors.Is(err, ErrAbuseMailboxSealPrivateKey) || parsed != nil {
			t.Errorf("%s parses: %v", c.name, err)
			continue
		}
		for _, value := range []string{encodedSeed, hex.EncodeToString(seed), macKey, publicKey} {
			if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), value[:16]) {
				t.Errorf("%s: the refusal names a value: %v", c.name, err)
			}
		}
	}
}

// A key that is not X-Wing's, or one crypto/hpke assembled from its parts and
// so holds no seed, gives no private key file: there is nothing a restore
// could read back.
func TestAbuseMailboxSealPrivateKeyFileRefusesKeysWithoutASeed(t *testing.T) {
	x25519PrivateKey, err := hpke.DHKEM(ecdh.X25519()).GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	decapsulationKey, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatal(err)
	}
	exchangeKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	partsKey, err := hpke.NewHybridPrivateKey(decapsulationKey, exchangeKey)
	if err != nil {
		t.Fatal(err)
	}
	if partsKey.KEM().ID() != hpke.MLKEM768X25519().ID() {
		t.Fatalf("the assembled key is KEM %#04x", partsKey.KEM().ID())
	}
	for _, c := range []struct {
		name string
		key  hpke.PrivateKey
	}{
		{name: "no key", key: nil},
		{name: "an X25519 key", key: x25519PrivateKey},
		{name: "an X-Wing key with no seed", key: partsKey},
	} {
		if file, err := AbuseMailboxSealPrivateKeyFile(c.key); !errors.Is(err, ErrAbuseMailboxSealPrivateKey) || file != nil {
			t.Errorf("%s gives a file of %d bytes: %v", c.name, len(file), err)
		}
	}
}

// The seal vector's pair, written as a private key file and read back, opens
// the record an earlier build mailed, so the file's form is pinned against the
// seal's own vector with no private key on disk.
func TestAbuseMailboxSealPrivateKeyFileOpensTheVector(t *testing.T) {
	settings := DefaultAbuseMailboxSettings()
	privateKey, macKey := testAbuseMailboxVectorKeys(t)
	file, err := AbuseMailboxSealPrivateKeyFile(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAbuseMailboxSealPrivateKey(file)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(testAbuseMailboxVectorPath)
	if err != nil {
		t.Fatal(err)
	}
	vector := &testAbuseMailboxVector{}
	if err := json.Unmarshal(data, vector); err != nil {
		t.Fatal(err)
	}
	sealed, err := DecodeAbuseMailboxBody(vector.Body, settings)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAbuseMailboxRecord(parsed, sealed, settings)
	if err != nil {
		t.Fatalf("the vector does not open with the key the file reads back to: %v", err)
	}
	if string(opened) != vector.Plaintext {
		t.Fatalf("the vector opens to %s", opened)
	}
	if _, err := ParseAbuseRecord(opened, macKey); err != nil {
		t.Fatal(err)
	}
}
