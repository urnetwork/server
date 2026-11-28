package nginx

// Tests of the session ticket keys fronts share (LBALTOPT2 R22 (a);
// tls_ticket.go): the pinned derivation, the day and the order of the keys,
// resumption across two fronts holding the same secrets and none with the
// setting off or other secrets, over TCP and QUIC, the daily rotation that
// keeps the previous day's and the next day's keys, and no ticket for a
// client that offers no TLS 1.3.

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// A secret for the tests: 32 bytes from fill.
func testTlsTicketSecret(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, TlsTicketSecretMinBytes)
}

// The derivation is pinned to vectors computed apart from crypto/hkdf (HKDF
// by hand over HMAC-SHA256, RFC 5869, with no salt): every front of every
// version must derive the same keys, or tickets stop resuming across a
// rolling deploy.
func TestTlsTicketKeyDerivationIsPinned(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	day := tlsTicketDay(time.Date(2026, 11, 20, 15, 4, 5, 0, time.UTC))
	if day != 20777 {
		t.Fatalf("2026-11-20 is day %d, want 20777", day)
	}
	for _, c := range []struct {
		day int64
		key string
	}{
		{day: 20777, key: "fd899b9fd28edaa00745155bbca9dd3e458c1aed53f653bc86aca7b8d9d1d43e"},
		{day: 20776, key: "5947eef4bf5042e4e41febb9cf4529a8ae85838297741e8179a840feb2191a4f"},
		{day: 20778, key: "0592932841cef3445d2cb08ab348daa97a6507052271d28fbb95f979daf03955"},
	} {
		key := tlsTicketKey(secret, c.day)
		if got := hex.EncodeToString(key[:]); got != c.key {
			t.Errorf("day %d: %s, want %s", c.day, got, c.key)
		}
	}
}

// A day runs from one UTC midnight to the next, whatever the zone a time is
// read in, and before the epoch too.
func TestTlsTicketDayFollowsUtcMidnight(t *testing.T) {
	midnight := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	if before, after := tlsTicketDay(midnight.Add(-time.Second)), tlsTicketDay(midnight); after != before+1 {
		t.Errorf("midnight moves the day from %d to %d", before, after)
	}
	if tlsTicketDay(midnight.In(time.FixedZone("west", -8*60*60))) != tlsTicketDay(midnight) {
		t.Error("the zone changed the day")
	}
	if day := tlsTicketDay(time.Unix(-1, 0)); day != -1 {
		t.Errorf("the second before the epoch is day %d, want -1", day)
	}
}

// A front holds each secret's keys of the day, the day before and the day
// after, the first secret's key of the day first, since it encrypts; every
// key differs.
func TestTlsTicketKeysOfDayPutTheEncryptingKeyFirst(t *testing.T) {
	secrets := [][]byte{testTlsTicketSecret(1), testTlsTicketSecret(2)}
	day := int64(20777)
	keys := tlsTicketKeysOfDay(secrets, day)
	want := [][32]byte{
		tlsTicketKey(secrets[0], day),
		tlsTicketKey(secrets[0], day-1),
		tlsTicketKey(secrets[0], day+1),
		tlsTicketKey(secrets[1], day),
		tlsTicketKey(secrets[1], day-1),
		tlsTicketKey(secrets[1], day+1),
	}
	if len(keys) != len(want) {
		t.Fatalf("%d keys, want %d", len(keys), len(want))
	}
	seen := map[[32]byte]bool{}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key %d is not the expected one", i)
		}
		if seen[keys[i]] {
			t.Errorf("key %d repeats an earlier one", i)
		}
		seen[keys[i]] = true
	}
}

// A front refuses secrets it can't key tickets with, without quoting them.
func TestNewFrontRefusesSessionTicketSecretsItCantUse(t *testing.T) {
	vault := newTestTlsVault(t)
	shortSecret := []byte("a-short-session-ticket-secret")
	for _, c := range []struct {
		what    string
		secrets [][]byte
		refused bool
	}{
		{what: "none", secrets: nil},
		{what: "one of 32 bytes", secrets: [][]byte{testTlsTicketSecret(1)}},
		{what: "one shorter than 32 bytes", secrets: [][]byte{testTlsTicketSecret(1), shortSecret}, refused: true},
		{what: "more than the front takes", secrets: [][]byte{testTlsTicketSecret(1), testTlsTicketSecret(2), testTlsTicketSecret(3), testTlsTicketSecret(4), testTlsTicketSecret(5)}, refused: true},
	} {
		settings := DefaultFrontSettings()
		settings.Tls.SessionTicketSecrets = c.secrets
		front, err := NewFront(t.Context(), vault.routeTable, "test-revision", vault.load(t), &testConnectHandler{}, "1.2.3", settings)
		if front != nil {
			front.Close()
		}
		if refused := err != nil; refused != c.refused {
			t.Errorf("%s: refused %t (%v), want %t", c.what, refused, err, c.refused)
		}
		if err != nil && strings.Contains(err.Error(), string(shortSecret)) {
			t.Errorf("%s: the error quotes the secret: %v", c.what, err)
		}
	}
}

// Whether a session a client got from issuer resumes on target, a new
// client session each time, over TCP with the client's version up to
// maxVersion.
func testTlsTicketResumes(t *testing.T, vault *testTlsVault, issuer *tls.Config, target *tls.Config, maxVersion uint16) bool {
	t.Helper()
	sessionCache := newTestNotifyingSessionCache()
	clientConfig := &tls.Config{
		ServerName:         "test-api.service.example",
		RootCAs:            vault.authority.pool(),
		ClientSessionCache: sessionCache,
		MaxVersion:         maxVersion,
	}
	state, err := listenTestTls(t, issuer).exchange(t, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if state.DidResume {
		t.Fatal("a new client session resumed")
	}
	sessionCache.awaitSession(t)
	state, err = listenTestTls(t, target).exchange(t, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	return state.DidResume
}

// Two fronts holding the same secrets resume each other's sessions, as two
// blocks or hosts would, over TCP and QUIC. With the setting off, the
// default, or with other secrets, they don't: each holds its own keys.
func TestFrontsSharingTicketSecretsResumeEachOthersSessions(t *testing.T) {
	vault := newTestTlsVault(t)
	certificateStore := vault.load(t)
	newFront := func(secrets ...[]byte) *Front {
		settings := DefaultFrontSettings()
		settings.Tls.SessionTicketSecrets = secrets
		return newTestCloseFront(t, vault.routeTable, certificateStore, settings)
	}
	shared := testTlsTicketSecret(1)
	first := newFront(shared)
	second := newFront(shared)
	other := newFront(testTlsTicketSecret(2))
	off := newFront()
	alsoOff := newFront()

	for _, c := range []struct {
		what    string
		issuer  *Front
		target  *Front
		resumes bool
	}{
		{what: "fronts sharing a secret", issuer: first, target: second, resumes: true},
		{what: "the other way", issuer: second, target: first, resumes: true},
		{what: "fronts with other secrets", issuer: first, target: other, resumes: false},
		{what: "fronts with the setting off", issuer: off, target: alsoOff, resumes: false},
		{what: "a front with the setting off and one sharing a secret", issuer: off, target: first, resumes: false},
	} {
		if resumes := testTlsTicketResumes(t, vault, c.issuer.TlsConfig(), c.target.TlsConfig(), tls.VersionTLS13); resumes != c.resumes {
			t.Errorf("TCP, %s: resumed %t, want %t", c.what, resumes, c.resumes)
		}

		sessionCache := newTestNotifyingSessionCache()
		clientConfig := &tls.Config{
			ServerName:         "test-api.service.example",
			RootCAs:            vault.authority.pool(),
			NextProtos:         []string{"h3"},
			ClientSessionCache: sessionCache,
		}
		state, err := listenTestQuic(t, c.issuer.QuicTlsConfig()).exchange(t, clientConfig, sessionCache)
		if err != nil {
			t.Fatal(err)
		}
		if state.TLS.DidResume {
			t.Fatalf("QUIC, %s: a new client session resumed", c.what)
		}
		state, err = listenTestQuic(t, c.target.QuicTlsConfig()).exchange(t, clientConfig, nil)
		if err != nil {
			t.Fatal(err)
		}
		if state.TLS.DidResume != c.resumes {
			t.Errorf("QUIC, %s: resumed %t, want %t", c.what, state.TLS.DidResume, c.resumes)
		}
	}
}

// A ticket opens on a front whose clock reads the day it was issued, the day
// after or the day before, and on no later day; a second secret's keys open
// tickets too, so a secret change rolls through the blocks. A front keeps
// its keys to the day of its clock at its first handshake of a day, so its
// own earlier ticket stops opening two days on, and the ticket it then
// issues opens on a front a day further.
func TestSharedTicketKeysRotateDailyKeepingThePreviousDay(t *testing.T) {
	vault := newTestTlsVault(t)
	secret := testTlsTicketSecret(1)
	nextSecret := testTlsTicketSecret(2)
	day := 24 * time.Hour
	// the configs of a front whose clock reads offset from the issuer's
	frontAt := func(offset time.Duration, secrets ...[]byte) (*tls.Config, *testClock) {
		clock := newTestClock()
		clock.advance(offset)
		settings := DefaultTlsSettings()
		settings.SessionTicketSecrets = secrets
		tlsConfig, _ := vault.newTlsConfigsAt(t, settings, newFrontMetrics(prometheus.NewRegistry()), clock.now)
		return tlsConfig, clock
	}
	issuer, issuerClock := frontAt(0, secret)

	for _, c := range []struct {
		what    string
		offset  time.Duration
		secrets [][]byte
		resumes bool
	}{
		{what: "a front on the same day", offset: 11 * time.Hour, secrets: [][]byte{secret}, resumes: true},
		{what: "a front on the next day", offset: day, secrets: [][]byte{secret}, resumes: true},
		{what: "a front two days on", offset: 2 * day, secrets: [][]byte{secret}, resumes: false},
		{what: "a front whose clock is a day behind", offset: -day, secrets: [][]byte{secret}, resumes: true},
		{what: "a front two days behind", offset: -2 * day, secrets: [][]byte{secret}, resumes: false},
		{what: "a front with a new secret first and the old one after", offset: day, secrets: [][]byte{nextSecret, secret}, resumes: true},
		{what: "a front with only a new secret", offset: 0, secrets: [][]byte{nextSecret}, resumes: false},
	} {
		target, _ := frontAt(c.offset, c.secrets...)
		if resumes := testTlsTicketResumes(t, vault, issuer, target, tls.VersionTLS13); resumes != c.resumes {
			t.Errorf("%s: resumed %t, want %t", c.what, resumes, c.resumes)
		}
	}

	// the issuer's own clock moves on: a ticket of the first day, presented
	// as it was issued each time, still opens on the next day, as
	// yesterday's, and no longer two days on. A resumption issues a new
	// ticket under the day's key, which the frozen cache never keeps
	firstDayCache := newTestFrozenSessionCache()
	clientConfig := &tls.Config{
		ServerName:         "test-api.service.example",
		RootCAs:            vault.authority.pool(),
		ClientSessionCache: firstDayCache,
	}
	listener := listenTestTls(t, issuer)
	if _, err := listener.exchange(t, clientConfig); err != nil {
		t.Fatal(err)
	}
	firstDayCache.awaitSession(t)
	issuerClock.advance(day)
	state, err := listener.exchange(t, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !state.DidResume {
		t.Error("the first day's ticket didn't open on the issuer a day on")
	}
	issuerClock.advance(day)
	state, err = listener.exchange(t, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if state.DidResume {
		t.Error("a ticket two days old opened on the issuer")
	}

	// the issuer now seals under its new day's key, which a front a day
	// further opens
	sessionCache := newTestNotifyingSessionCache()
	clientConfig.ClientSessionCache = sessionCache
	if _, err := listener.exchange(t, clientConfig); err != nil {
		t.Fatal(err)
	}
	sessionCache.awaitSession(t)
	later, _ := frontAt(3*day, secret)
	state, err = listenTestTls(t, later).exchange(t, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !state.DidResume {
		t.Error("the rotated issuer's ticket didn't open on a front a day further")
	}
}

// A client session cache that keeps the first session of each key and
// ignores every later one and every eviction, so a client presents its first
// ticket on every connection, and reports the first.
type testFrozenSessionCache struct {
	*testNotifyingSessionCache
	stateLock sync.Mutex
	kept      map[string]bool
}

// An empty cache.
func newTestFrozenSessionCache() *testFrozenSessionCache {
	return &testFrozenSessionCache{
		testNotifyingSessionCache: newTestNotifyingSessionCache(),
		kept:                      map[string]bool{},
	}
}

// Keeps the first session of sessionKey.
func (self *testFrozenSessionCache) Put(sessionKey string, clientSessionState *tls.ClientSessionState) {
	if clientSessionState == nil {
		return
	}
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.kept[sessionKey] {
			return false
		}
		self.kept[sessionKey] = true
		return true
	}()
	if first {
		self.testNotifyingSessionCache.Put(sessionKey, clientSessionState)
	}
}

// While the keys are shared, a TCP client that offers no TLS 1.3 gets no
// ticket and doesn't resume, so no TLS 1.2 session's master secret is ever
// sealed under a shared key; a TLS 1.3 client of the same fronts resumes.
// With the setting off, a TLS 1.2 client still resumes on its front.
func TestSharedTicketKeysGiveTls12ClientsNoTicket(t *testing.T) {
	vault := newTestTlsVault(t)
	settings := DefaultTlsSettings()
	settings.SessionTicketSecrets = [][]byte{testTlsTicketSecret(1)}
	shared, _ := vault.newTlsConfigs(t, settings)
	unshared, _ := vault.newTlsConfigs(t, DefaultTlsSettings())

	for _, serverName := range []string{"test-api.service.example", "connect.service.example", ""} {
		sessionCache := tls.NewLRUClientSessionCache(0)
		clientConfig := &tls.Config{
			ServerName:         serverName,
			RootCAs:            vault.authority.pool(),
			InsecureSkipVerify: serverName == "",
			ClientSessionCache: sessionCache,
			MaxVersion:         tls.VersionTLS12,
		}
		listener := listenTestTls(t, shared)
		for range 2 {
			state, err := listener.exchange(t, clientConfig)
			if err != nil {
				t.Fatalf("%q: %v", serverName, err)
			}
			if state.Version != tls.VersionTLS12 || state.DidResume {
				t.Fatalf("%q: version %#x resumed %t, want TLS 1.2 without resumption", serverName, state.Version, state.DidResume)
			}
		}
	}

	if !testTlsTicketResumes(t, vault, shared, shared, tls.VersionTLS13) {
		t.Error("a TLS 1.3 client didn't resume on the shared keys")
	}
	if !testTlsTicketResumes(t, vault, unshared, unshared, tls.VersionTLS12) {
		t.Error("a TLS 1.2 client didn't resume with the setting off")
	}
}
