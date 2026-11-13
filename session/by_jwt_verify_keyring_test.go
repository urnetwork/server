// The verify-only keyring (plan-SOCK-client-auth, QD-040) on a synthetic vault
// and an explicit clock: it verifies what the issuer's keyring signs from the
// exported jwk set alone, rereads for a kid it lacks as the issuer's keyring
// does, and refuses a document it cannot trust.
package session

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// A verify-only keyring reading whatever *document holds, counting its reads.
type byJwtVerifyTestDocument struct {
	document  []byte
	err       error
	readCount int
}

func newByJwtVerifyTestKeyring(source *byJwtVerifyTestDocument, clock *byJwtTestClock) *ByJwtVerifyKeyring {
	return NewByJwtVerifyKeyring(&ByJwtVerifyKeyringSettings{
		Document: func() ([]byte, error) {
			source.readCount += 1
			return source.document, source.err
		},
		RereadInterval: byJwtTestRereadInterval,
		RsaMinBitCount: 2048,
		Now:            clock.now,
	})
}

// The jwk set an operations command would export from the synthetic vault now.
func byJwtVerifyTestExport(t *testing.T, vault *byJwtTestVault, clock *byJwtTestClock) []byte {
	t.Helper()
	keyring := newByJwtTestKeyring(vault, clock)
	document, err := exportByJwtPublicJwks(keyring.heldKeys())
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// A token the issuer's keyring signs verifies against the exported public keys,
// with every claim intact, and the export holds no private part.
func TestByJwtVerifyKeyringVerifiesWhatTheIssuerSigns(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-3*time.Hour))
	issuer := newByJwtTestKeyring(vault, clock)
	document := byJwtVerifyTestExport(t, vault, clock)
	if strings.Contains(string(document), `"d"`) {
		t.Fatalf("the export holds a private part: %s", document)
	}

	claims := newByJwtTestClaims()
	sidecarId := claims.NetworkId
	claims.SidecarId = &sidecarId
	signed := signWithKeyring(issuer, claims)
	verifier := newByJwtVerifyTestKeyring(&byJwtVerifyTestDocument{document: document}, clock)
	parsed, unknownKid, err := verifier.parse(signed)
	if err != nil || unknownKid {
		t.Fatalf("parse: %v, unknown kid %v", err, unknownKid)
	}
	if parsed.NetworkId != claims.NetworkId || parsed.UserId != claims.UserId || parsed.SidecarId == nil || *parsed.SidecarId != sidecarId {
		t.Fatalf("claims %+v", parsed)
	}
}

// A token under a kid the document lacks rereads it at most once per interval,
// as the issuer's keyring rereads the vault, and verifies once the export holds
// the key; until then it names an unknown kid, which a verifier tells apart.
func TestByJwtVerifyKeyringRereadsForAKidItLacks(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	source := &byJwtVerifyTestDocument{document: byJwtVerifyTestExport(t, vault, clock)}
	verifier := newByJwtVerifyTestKeyring(source, clock)
	if _, err := verifier.heldKeys(); err != nil {
		t.Fatal(err)
	}

	// the vault gains a version the mounted export does not hold yet
	secondKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time)
	signed := signByJwtWithKey(t, newByJwtTestClaims(), secondKey, true)
	if _, unknownKid, err := verifier.parse(signed); err == nil || !unknownKid {
		t.Fatalf("a kid the export lacks: %v, unknown kid %v", err, unknownKid)
	}
	if source.readCount != 2 {
		t.Fatalf("reads = %d, want 2", source.readCount)
	}

	// the export reaches the mount, but within the interval nothing rereads
	source.document = byJwtVerifyTestExport(t, vault, clock)
	clock.time = clock.time.Add(byJwtTestRereadInterval - time.Nanosecond)
	if _, _, err := verifier.parse(signed); err == nil {
		t.Fatal("a kid verified within the reread interval")
	}
	if source.readCount != 2 {
		t.Fatalf("reads within the interval = %d, want 2", source.readCount)
	}

	clock.time = clock.time.Add(time.Nanosecond)
	if _, unknownKid, err := verifier.parse(signed); err != nil || unknownKid {
		t.Fatalf("after the interval: %v, unknown kid %v", err, unknownKid)
	}
	if source.readCount != 3 {
		t.Fatalf("reads = %d, want 3", source.readCount)
	}

	// a key the next export drops keeps verifying, as a retired key does
	firstKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	firstSigned := signByJwtWithKey(t, newByJwtTestClaims(), firstKey, true)
	source.document = byJwtVerifyTestExport(t, vault, clock)
	clock.time = clock.time.Add(byJwtTestRereadInterval)
	if _, _, err := verifier.parse(firstSigned); err != nil {
		t.Fatalf("the rewritten first version: %v", err)
	}
	if _, _, err := verifier.parse(signed); err != nil {
		t.Fatalf("a key the export no longer lists: %v", err)
	}
}

// A forgery under a kid the keyring holds is no unknown kid, and causes no
// reread; a malformed token neither.
func TestByJwtVerifyKeyringRefusesAForgeryUnderAHeldKid(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	heldKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	source := &byJwtVerifyTestDocument{document: byJwtVerifyTestExport(t, vault, clock)}
	verifier := newByJwtVerifyTestKeyring(source, clock)

	foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spoofed := signByJwtWithKeyAndKid(t, foreignKey, byJwtTestKid(t, heldKey))
	if _, unknownKid, err := verifier.parse(spoofed); err == nil || unknownKid {
		t.Fatalf("a foreign signature under a held kid: %v, unknown kid %v", err, unknownKid)
	}
	if _, unknownKid, err := verifier.parse("not.a.token"); err == nil || unknownKid {
		t.Fatalf("a malformed token: %v, unknown kid %v", err, unknownKid)
	}
	if source.readCount != 1 {
		t.Fatalf("reads = %d, want 1", source.readCount)
	}
}

// A block whose document cannot be read holds no key: every parse answers
// ErrByJwtVerifyKeysUnavailable, the document is read again at most once per
// interval, and once a read succeeds tokens verify.
func TestByJwtVerifyKeyringWithNoDocumentIsUnavailable(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	key := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	signed := signByJwtWithKey(t, newByJwtTestClaims(), key, true)
	source := &byJwtVerifyTestDocument{err: errors.New("no jwt-jwks.json in this block's secret_files")}
	verifier := newByJwtVerifyTestKeyring(source, clock)
	for range 3 {
		if _, _, err := verifier.parse(signed); !errors.Is(err, ErrByJwtVerifyKeysUnavailable) {
			t.Fatalf("no document: %v", err)
		}
	}
	if source.readCount != 1 {
		t.Fatalf("reads within the interval = %d, want 1", source.readCount)
	}

	source.document, source.err = byJwtVerifyTestExport(t, vault, clock), nil
	clock.time = clock.time.Add(byJwtTestRereadInterval)
	if _, _, err := verifier.parse(signed); err != nil {
		t.Fatalf("after the document arrived: %v", err)
	}
}

// The export an operations command writes from a vault's jwt.yml holds every
// listed version's public key, and a verify-only keyring on it alone verifies
// what the process signs, whichever version signed.
func TestExportByJwtPublicJwksVerifiesTheVaultsTokens(t *testing.T) {
	if os.Getenv(byJwtRotationChildEnv) == t.Name() {
		vaultRoot := os.Getenv("WARP_VAULT_HOME")
		document, err := ExportByJwtPublicJwks()
		if err != nil {
			t.Fatal(err)
		}
		clock := newByJwtTestClock()
		verifier := newByJwtVerifyTestKeyring(&byJwtVerifyTestDocument{document: document}, clock)
		for _, version := range []string{"1.0.0", "2.0.0"} {
			key := readByJwtKeyFile(t, byJwtRotationKeyFile(vaultRoot, version))
			if _, unknownKid, err := verifier.parse(signByJwtWithKey(t, newByJwtTestClaims(), key, true)); err != nil || unknownKid {
				t.Fatalf("version %s: %v, unknown kid %v", version, err, unknownKid)
			}
		}
		if _, _, err := verifier.parse(newByJwtTestClaims().Testing_Sign()); err != nil {
			t.Fatalf("what the process signs: %v", err)
		}
		return
	}

	vaultRoot := newByJwtRotationVault(t)
	writeByJwtRotationKey(t, vaultRoot, "1.0.0")
	writeByJwtRotationKey(t, vaultRoot, "2.0.0")
	runByJwtRotationChild(t, t.Name(), vaultRoot)
}

// Any key the document cannot vouch for refuses the whole document: a private
// key, a kid that is not the key's, an alg that is not the key's, an rsa key
// below the minimum, a curve no by jwt uses, and an empty set.
func TestByJwtVerifyKeyringRefusesADocumentItCannotTrust(t *testing.T) {
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherEcdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	smallRsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	kid := func(key any) string {
		value, err := jwtKid(key)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	document := func(keys ...jose.JSONWebKey) []byte {
		data, err := json.Marshal(&jose.JSONWebKeySet{Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	good := jose.JSONWebKey{Key: &ecdsaKey.PublicKey, KeyID: kid(&ecdsaKey.PublicKey), Algorithm: "ES256", Use: "sig"}
	if keys, err := parseByJwtPublicJwks(document(good), 2048); err != nil || len(keys) != 1 {
		t.Fatalf("a good document: %v %v", keys, err)
	}
	for name, data := range map[string][]byte{
		"a private key":           document(good, jose.JSONWebKey{Key: otherEcdsaKey, KeyID: kid(&otherEcdsaKey.PublicKey), Algorithm: "ES256"}),
		"another key's kid":       document(jose.JSONWebKey{Key: &otherEcdsaKey.PublicKey, KeyID: kid(&ecdsaKey.PublicKey), Algorithm: "ES256"}),
		"another alg":             document(jose.JSONWebKey{Key: &ecdsaKey.PublicKey, KeyID: kid(&ecdsaKey.PublicKey), Algorithm: "ES384"}),
		"no alg":                  document(jose.JSONWebKey{Key: &ecdsaKey.PublicKey, KeyID: kid(&ecdsaKey.PublicKey)}),
		"a small rsa key": document(jose.JSONWebKey{Key: &smallRsaKey.PublicKey, KeyID: kid(&smallRsaKey.PublicKey), Algorithm: "RS512"}),
		"a curve no by jwt signs": []byte(`{"keys":[{"kty":"EC","crv":"P-224","alg":"ES256","kid":"synthetic",` +
			`"x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","y":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}`),
		"a symmetric key": []byte(`{"keys":[{"kty":"oct","k":"c3ludGhldGlj","alg":"HS256","kid":"synthetic"}]}`),
		"no key":          []byte(`{"keys":[]}`),
		"not a jwk set":   []byte(`[]`),
	} {
		if keys, err := parseByJwtPublicJwks(data, 2048); err == nil {
			t.Errorf("%s: parsed %d keys", name, len(keys))
		}
	}
}
