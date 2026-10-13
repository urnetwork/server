// Keyring rereads and signing-key selection, driven by an explicit clock and a
// synthetic vault (see by_jwt_rotation_test.go for the vault layout).
package session

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

const byJwtTestRereadInterval = 10 * time.Second
const byJwtTestSigningDelay = 1 * time.Hour

type byJwtTestClock struct {
	time time.Time
}

func (self *byJwtTestClock) now() time.Time {
	return self.time
}

// A synthetic vault root, its read count (each read lists the key paths once),
// and a switch that fails key path resolution.
type byJwtTestVault struct {
	root        string
	readCount   int
	failResolve bool
}

func newByJwtTestVault(t *testing.T) *byJwtTestVault {
	return &byJwtTestVault{
		root: t.TempDir(),
	}
}

func newByJwtTestKeyring(vault *byJwtTestVault, clock *byJwtTestClock) *byJwtKeyring {
	return newByJwtKeyring(&byJwtKeyringSettings{
		keyPaths: func() ([]string, error) {
			vault.readCount += 1
			return []string{byJwtRotationKeyPath}, nil
		},
		resourcePaths: func(keyPath string) ([]string, error) {
			if vault.failResolve {
				return nil, errors.New("synthetic resolve failure")
			}
			return server.ResolveResourcePaths(vault.root, byJwtRotationEnv, keyPath)
		},
		now:            clock.now,
		rereadInterval: byJwtTestRereadInterval,
		signingDelay:   byJwtTestSigningDelay,
	})
}

func newByJwtTestClock() *byJwtTestClock {
	return &byJwtTestClock{
		time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// Writes a key version modified at modifyTime.
func writeByJwtTestKey(t *testing.T, vault *byJwtTestVault, version string, modifyTime time.Time) *ecdsa.PrivateKey {
	t.Helper()
	key := writeByJwtRotationKey(t, vault.root, version)
	if err := os.Chtimes(byJwtRotationKeyFile(vault.root, version), modifyTime, modifyTime); err != nil {
		t.Fatal(err)
	}
	return key
}

func byJwtTestKid(t testing.TB, key *ecdsa.PrivateKey) string {
	t.Helper()
	kid, err := jwtKid(publicKey(key))
	if err != nil {
		t.Fatal(err)
	}
	return kid
}

func newByJwtTestClaims() *ByJwt {
	return NewByJwt(server.NewId(), server.NewId(), "test", false, false)
}

func TestByJwtKeyringRereadIsSpacedByInterval(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.heldKeys()

	// the first reread is admitted at once
	secondKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time)
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), secondKey, true)); err != nil {
		t.Fatalf("first unknown kid: %s", err)
	}
	if vault.readCount != 2 {
		t.Fatalf("reads = %d, want 2", vault.readCount)
	}

	// within the interval no reread is admitted, so a newer version is refused
	thirdKey := writeByJwtTestKey(t, vault, "3.0.0", clock.time)
	clock.time = clock.time.Add(byJwtTestRereadInterval - time.Nanosecond)
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), thirdKey, true)); err == nil {
		t.Fatalf("unknown kid within the reread interval verified")
	}
	if vault.readCount != 2 {
		t.Fatalf("reads within the interval = %d, want 2", vault.readCount)
	}

	// at the interval the next reread is admitted
	clock.time = clock.time.Add(time.Nanosecond)
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), thirdKey, true)); err != nil {
		t.Fatalf("unknown kid after the reread interval: %s", err)
	}
	if vault.readCount != 3 {
		t.Fatalf("reads = %d, want 3", vault.readCount)
	}
}

// A kid names the exact key, so a token that fails the held key it names is not
// a rotation and causes no reread. Neither does a token that does not parse.
func TestByJwtKeyringHeldKidMismatchDoesNotReread(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	heldKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.heldKeys()

	foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged := signByJwtWithKey(t, newByJwtTestClaims(), foreignKey, false)
	// a token signed by the foreign key that claims the held key's kid
	spoofed := signByJwtWithKeyAndKid(t, foreignKey, byJwtTestKid(t, heldKey))
	if _, err := parseByJwtSigned(keyring, spoofed); err == nil {
		t.Fatalf("token claiming a held kid with a foreign signature verified")
	}
	if _, err := parseByJwtSigned(keyring, "not.a.token"); err == nil {
		t.Fatalf("malformed token verified")
	}
	if vault.readCount != 1 {
		t.Fatalf("reads = %d, want 1", vault.readCount)
	}

	// a foreign token without a kid fails every held key, and is reread for
	if _, err := parseByJwtSigned(keyring, forged); err == nil {
		t.Fatalf("token without a kid signed by a foreign key verified")
	}
	if vault.readCount != 2 {
		t.Fatalf("reads after a kidless failure = %d, want 2", vault.readCount)
	}
}

// Signs synthetic claims with key under an explicit kid header.
func signByJwtWithKeyAndKid(t testing.TB, key *ecdsa.PrivateKey, kid string) string {
	t.Helper()
	token := gojwt.NewWithClaims(gojwt.SigningMethodES256, newByJwtTestClaims())
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// A token without a kid, signed with a version the vault gained after first use,
// verifies after a reread.
func TestByJwtKeyringKidlessTokenRereads(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.heldKeys()

	rotatedKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time)
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), rotatedKey, false)); err != nil {
		t.Fatalf("kidless token signed with the added version: %s", err)
	}
}

func TestByJwtKeyringSignsWithSettledVersion(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	settledKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-3*time.Hour))
	newKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time.Add(-30*time.Minute))
	keyring := newByJwtTestKeyring(vault, clock)

	signed := signWithKeyring(keyring, newByJwtTestClaims())
	if kid := byJwtTokenKid(t, signed); kid != byJwtTestKid(t, settledKey) {
		t.Fatalf("signed with %q before the new version settled, want the settled version", kid)
	}
	if _, err := parseByJwtSigned(keyring, signed); err != nil {
		t.Fatal(err)
	}

	// the new version settles the signing delay after it was written
	clock.time = clock.time.Add(30*time.Minute - time.Nanosecond)
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, settledKey) {
		t.Fatalf("signed with %q just before the new version settled", kid)
	}
	clock.time = clock.time.Add(time.Nanosecond)
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, newKey) {
		t.Fatalf("signed with %q once the new version settled, want the new version", kid)
	}
}

// With no settled version (a first deployment, or every key just written), the
// newest signs, as before the delay.
func TestByJwtKeyringSignsNewestWhenNoneSettled(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-10*time.Minute))
	newestKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time.Add(-5*time.Minute))
	keyring := newByJwtTestKeyring(vault, clock)

	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, newestKey) {
		t.Fatalf("signed with %q, want the newest version", kid)
	}
}

// Ecdsa before rsa, settled before newest, retired never.
func TestByJwtKeysSigningKeyOrder(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	settledTime := now.Add(-2 * byJwtTestSigningDelay)
	youngTime := now.Add(-byJwtTestSigningDelay / 2)
	newKey := func(name string, privateKey any, modifyTime time.Time, retired bool) *byJwtKey {
		return &byJwtKey{
			path:       name,
			modifyTime: modifyTime,
			privateKey: privateKey,
			retired:    retired,
		}
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	type signingCase struct {
		keys []*byJwtKey
		want string
	}
	cases := []signingCase{
		{
			keys: []*byJwtKey{newKey("young ecdsa", ecdsaKey, youngTime, false), newKey("settled ecdsa", ecdsaKey, settledTime, false)},
			want: "settled ecdsa",
		},
		{
			keys: []*byJwtKey{newKey("young ecdsa", ecdsaKey, youngTime, false), newKey("settled rsa", rsaKey, settledTime, false)},
			want: "settled rsa",
		},
		{
			keys: []*byJwtKey{newKey("settled rsa", rsaKey, settledTime, false), newKey("settled ecdsa", ecdsaKey, settledTime, false)},
			want: "settled ecdsa",
		},
		{
			keys: []*byJwtKey{newKey("young rsa", rsaKey, youngTime, false), newKey("young ecdsa", ecdsaKey, youngTime, false)},
			want: "young ecdsa",
		},
		{
			keys: []*byJwtKey{newKey("newer young ecdsa", ecdsaKey, youngTime, false), newKey("older young ecdsa", ecdsaKey, youngTime.Add(-time.Minute), false)},
			want: "newer young ecdsa",
		},
		{
			keys: []*byJwtKey{newKey("young ecdsa", ecdsaKey, youngTime, false), newKey("retired settled ecdsa", ecdsaKey, settledTime, true)},
			want: "young ecdsa",
		},
		{
			keys: []*byJwtKey{newKey("settled exactly", ecdsaKey, now.Add(-byJwtTestSigningDelay), false), newKey("settled ecdsa", ecdsaKey, settledTime, false)},
			want: "settled exactly",
		},
	}
	for _, c := range cases {
		keys := &byJwtKeys{keys: c.keys}
		signingKey := keys.signingKey(now, byJwtTestSigningDelay)
		if signingKey == nil || signingKey.path != c.want {
			t.Errorf("signing key = %v, want %q", signingKey, c.want)
		}
	}

	retiredOnly := &byJwtKeys{keys: []*byJwtKey{newKey("retired", ecdsaKey, settledTime, true)}}
	if signingKey := retiredOnly.signingKey(now, byJwtTestSigningDelay); signingKey != nil {
		t.Errorf("retired key signs: %q", signingKey.path)
	}
}

// A version the vault drops keeps verifying and no longer signs.
func TestByJwtKeyringRetiresKeysTheVaultDrops(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	settledKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-3*time.Hour))
	droppedKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time.Add(-2*time.Hour))
	keyring := newByJwtTestKeyring(vault, clock)
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, droppedKey) {
		t.Fatalf("signed with %q, want the newest settled version", kid)
	}

	// the vault drops the version, and gains a new one that is not yet settled
	if err := os.RemoveAll(filepath.Join(vault.root, "all", "tls", "2.0.0")); err != nil {
		t.Fatal(err)
	}
	newKey := writeByJwtTestKey(t, vault, "3.0.0", clock.time)
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), newKey, true)); err != nil {
		t.Fatalf("token signed with the added version: %s", err)
	}

	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), droppedKey, true)); err != nil {
		t.Fatalf("token signed with the dropped version: %s", err)
	}
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, settledKey) {
		t.Fatalf("signed with %q after the vault dropped the signing version, want the remaining settled version", kid)
	}
}

// A reread that cannot resolve a key path keeps every held key, so it cannot
// retire the signing key.
func TestByJwtKeyringRereadKeepsKeysWhenLookupFails(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	heldKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-3*time.Hour))
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.heldKeys()

	vault.failResolve = true
	rotatedKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time.Add(-3*time.Hour))
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), rotatedKey, true)); err == nil {
		t.Fatalf("token signed with a version the failed reread could not see verified")
	}
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), heldKey, true)); err != nil {
		t.Fatalf("held version after a failed reread: %s", err)
	}
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, heldKey) {
		t.Fatalf("signed with %q after a failed reread, want the held version", kid)
	}

	vault.failResolve = false
	clock.time = clock.time.Add(byJwtTestRereadInterval)
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), rotatedKey, true)); err != nil {
		t.Fatalf("token signed with the added version once the vault resolves: %s", err)
	}
}

// A key file rewritten in place is read again: its new key verifies and signs,
// and its old key keeps verifying.
func TestByJwtKeyringRereadsRewrittenKeyFile(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	oldKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-3*time.Hour))
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.heldKeys()

	rewrittenKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-2*time.Hour))
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), rewrittenKey, true)); err != nil {
		t.Fatalf("token signed with the rewritten key: %s", err)
	}
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), oldKey, true)); err != nil {
		t.Fatalf("token signed with the key the file held before: %s", err)
	}
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, rewrittenKey) {
		t.Fatalf("signed with %q, want the rewritten key", kid)
	}
}

// A file that does not parse is skipped without failing the read, and is read
// again once rewritten.
func TestByJwtKeyringSkipsUnparseableKeyFile(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	heldKey := writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-3*time.Hour))
	unparseablePath := byJwtRotationKeyFile(vault.root, "2.0.0")
	if err := os.MkdirAll(filepath.Dir(unparseablePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unparseablePath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring := newByJwtTestKeyring(vault, clock)
	if keys := keyring.heldKeys(); len(keys.keys) != 1 {
		t.Fatalf("held keys = %d, want 1", len(keys.keys))
	}
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, heldKey) {
		t.Fatalf("signed with %q, want the parseable version", kid)
	}

	rotatedKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time.Add(-2*time.Hour))
	if _, err := parseByJwtSigned(keyring, signByJwtWithKey(t, newByJwtTestClaims(), rotatedKey, true)); err != nil {
		t.Fatalf("token signed with the rewritten file's key: %s", err)
	}
}

// A process without a key can neither sign nor verify.
func TestByJwtKeyringFirstReadRequiresKey(t *testing.T) {
	vault := newByJwtTestVault(t)
	keyring := newByJwtTestKeyring(vault, newByJwtTestClock())
	defer func() {
		if recover() == nil {
			t.Fatalf("first read without a key did not panic")
		}
	}()
	keyring.heldKeys()
}

// Derived keys put the signing key first, then every other held key.
func TestByJwtKeysPrivateKeysSigningFirst(t *testing.T) {
	keys := &byJwtKeys{
		keys: []*byJwtKey{
			{path: "newest", privateKey: "newest"},
			{path: "signing", privateKey: "signing"},
			{path: "retired", privateKey: "retired", retired: true},
		},
	}
	privateKeys := keys.privateKeysSigningFirst(keys.keys[1])
	want := []string{"signing", "newest", "retired"}
	if len(privateKeys) != len(want) {
		t.Fatalf("private keys = %v, want %v", privateKeys, want)
	}
	for i, privateKey := range privateKeys {
		if privateKey != want[i] {
			t.Fatalf("private keys = %v, want %v", privateKeys, want)
		}
	}
}

// Concurrent first uses read the vault once.
func TestByJwtKeyringConcurrentFirstUseReadsOnce(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestKeyring(vault, clock)

	var waitGroup sync.WaitGroup
	for range 32 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			keyring.heldKeys()
		}()
	}
	waitGroup.Wait()
	if vault.readCount != 1 {
		t.Fatalf("reads = %d, want 1", vault.readCount)
	}
}

// Concurrent tokens naming a version the vault gained after first use all
// verify: one reread is admitted, and every caller the interval refuses sees
// the key it gained.
func TestByJwtKeyringConcurrentUnknownKidsShareOneReread(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time)
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.heldKeys()

	rotatedKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time)
	signedTokens := []string{}
	for range 32 {
		signedTokens = append(signedTokens, signByJwtWithKey(t, newByJwtTestClaims(), rotatedKey, true))
	}
	errs := make([]error, len(signedTokens))
	var waitGroup sync.WaitGroup
	for i, signed := range signedTokens {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, errs[i] = parseByJwtSigned(keyring, signed)
		}()
	}
	waitGroup.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("token %d signed with the added version: %s", i, err)
		}
	}
	if vault.readCount != 2 {
		t.Fatalf("reads = %d, want 2", vault.readCount)
	}
}

// A version the vault drops and lists again is held once, listed, and signs
// again.
func TestByJwtKeyringRelistedVersionIsHeldOnce(t *testing.T) {
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-3*time.Hour))
	relistedKey := writeByJwtTestKey(t, vault, "2.0.0", clock.time.Add(-2*time.Hour))
	keyring := newByJwtTestKeyring(vault, clock)
	keyring.heldKeys()

	relistedPath := byJwtRotationKeyFile(vault.root, "2.0.0")
	keyPem, err := os.ReadFile(relistedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(vault.root, "all", "tls", "2.0.0")); err != nil {
		t.Fatal(err)
	}
	keyring.reread(keyring.heldKeys())
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid == byJwtTestKid(t, relistedKey) {
		t.Fatalf("the dropped version signs")
	}

	if err := os.MkdirAll(filepath.Dir(relistedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(relistedPath, keyPem, 0o600); err != nil {
		t.Fatal(err)
	}
	relistedTime := clock.time.Add(-2 * time.Hour)
	if err := os.Chtimes(relistedPath, relistedTime, relistedTime); err != nil {
		t.Fatal(err)
	}
	clock.time = clock.time.Add(byJwtTestRereadInterval)
	keyring.reread(keyring.heldKeys())

	keys := keyring.heldKeys()
	if len(keys.keys) != 2 {
		t.Fatalf("held keys = %d, want 2", len(keys.keys))
	}
	for _, key := range keys.keys {
		if key.retired {
			t.Fatalf("relisted key %s is retired", key.path)
		}
	}
	if kid := byJwtTokenKid(t, signWithKeyring(keyring, newByJwtTestClaims())); kid != byJwtTestKid(t, relistedKey) {
		t.Fatalf("signed with %q, want the relisted version", kid)
	}
}
