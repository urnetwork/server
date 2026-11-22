package nginx

// Session ticket keys shared by every front that reads the same secrets
// (LBALTOPT2 R22 (a); LBALT.md D6c, amended 2026-11-20). Off by default:
// with TlsSettings.SessionTicketSecrets empty, each front keeps Go's automatic
// per-process keys, as D6c decided on 2026-11-18.
//
// On, the keys follow the UTC day. Each secret and day derive one key, the
// HKDF-SHA256 of the secret with the day's date in its info
// (tlsTicketKey), and a front installs on both base configs, with
// SetSessionTicketKeys:
//   - the first secret's key of today, which encrypts every new ticket;
//   - the first secret's keys of yesterday and tomorrow, and each other
//     secret's of the three days, which only decrypt.
//
// So a ticket resumes on every front with the same secrets, on any block or
// host and across restarts, until the end of the day after the one it was
// issued on: 24 to 48 hours, where per-process keys accept a ticket for 7
// days but only in the process that issued it. Tomorrow's key keeps a front
// whose clock is behind a day boundary able to open the tickets of a front
// whose clock is past it. A second secret lets a change of the vault secret
// roll through the blocks: the new one first and the old one after, until
// every block has restarted with the new one.
//
// The keys change at a front's first handshake of each UTC day (refresh), on
// both base configs, which crypto/tls allows while they serve.
//
// While the keys are shared, a TCP client that offers no TLS 1.3 gets no
// ticket: the config its handshake gets disables tickets (newTlsConfigs). A
// TLS 1.2 ticket holds its session's master secret, so a leaked secret would
// decrypt every recorded TLS 1.2 session that got a ticket. A TLS 1.3 ticket
// resumes only with a new (EC)DHE exchange (psk_dhe_ke, the only mode
// crypto/tls's server accepts), so recorded sessions stay forward secret,
// and a leaked secret lets an active attacker resume as the server to a
// client holding a ticket of those days. QUIC is TLS 1.3 only.

import (
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// The fewest bytes a session ticket secret may have.
const TlsTicketSecretMinBytes = 32

// The most secrets a front takes. Each adds three keys that a ticket from
// another front may be tried against.
const TlsTicketSecretMaxCount = 4

// Refuses secrets that can't key tickets: more than TlsTicketSecretMaxCount,
// or one shorter than TlsTicketSecretMinBytes. No error quotes a secret.
func checkTlsTicketSecrets(secrets [][]byte) error {
	if TlsTicketSecretMaxCount < len(secrets) {
		return errors.New("lbalt front: more session ticket secrets than the front takes")
	}
	for _, secret := range secrets {
		if len(secret) < TlsTicketSecretMinBytes {
			return errors.New("lbalt front: a session ticket secret is shorter than 32 bytes")
		}
	}
	return nil
}

// The seconds of a ticket key's day.
const tlsTicketDaySeconds = 24 * 60 * 60

// The UTC day of now, in days since the Unix epoch.
func tlsTicketDay(now time.Time) int64 {
	seconds := now.Unix()
	day := seconds / tlsTicketDaySeconds
	if seconds%tlsTicketDaySeconds < 0 {
		day -= 1
	}
	return day
}

// The key secret derives for day: HKDF-SHA256 with no salt, over the
// secret, with the day's date in the info. Every front of every version must
// derive the same key, or tickets stop resuming across a rolling deploy, so
// TestTlsTicketKeyDerivationIsPinned pins it.
func tlsTicketKey(secret []byte, day int64) [32]byte {
	date := time.Unix(day*tlsTicketDaySeconds, 0).UTC().Format(time.DateOnly)
	keyBytes, err := hkdf.Key(sha256.New, secret, nil, "urnetwork lbalt session ticket key "+date, 32)
	if err != nil {
		// a 32 byte key is within what HKDF-SHA256 can expand
		panic(err)
	}
	var key [32]byte
	copy(key[:], keyBytes)
	return key
}

// The keys a front holds on day, the one that encrypts first: each secret's
// keys of day, the day before and the day after, the first secret's first.
func tlsTicketKeysOfDay(secrets [][]byte, day int64) [][32]byte {
	keys := make([][32]byte, 0, 3*len(secrets))
	for _, secret := range secrets {
		for _, keyDay := range []int64{day, day - 1, day + 1} {
			keys = append(keys, tlsTicketKey(secret, keyDay))
		}
	}
	return keys
}

// The shared keys of one front's base configs, kept to the day of its clock.
// Safe for concurrent use once installed.
type tlsTicketKeys struct {
	secrets [][]byte
	// the clock the day is read from: time.Now, or a test's
	now func() time.Time

	// the base configs, set once by install, before they serve
	tlsConfigs []*tls.Config
	// the day whose keys the configs hold; math.MinInt64 before install
	installedDay atomic.Int64
	stateLock    sync.Mutex
}

// The keys of secrets on now's days, or nil when there are no secrets and
// each front keeps Go's per-process keys.
func newTlsTicketKeys(secrets [][]byte, now func() time.Time) *tlsTicketKeys {
	if len(secrets) == 0 {
		return nil
	}
	ticketKeys := &tlsTicketKeys{
		secrets: make([][]byte, 0, len(secrets)),
		now:     now,
	}
	for _, secret := range secrets {
		ticketKeys.secrets = append(ticketKeys.secrets, append([]byte(nil), secret...))
	}
	ticketKeys.installedDay.Store(math.MinInt64)
	return ticketKeys
}

// Installs today's keys on tlsConfigs, which refresh keeps current from then
// on. Called once, before the configs serve.
func (self *tlsTicketKeys) install(tlsConfigs ...*tls.Config) {
	self.tlsConfigs = tlsConfigs
	self.setKeys(tlsTicketDay(self.now()))
}

// Installs the keys of the current day unless the configs hold them already,
// which costs a handshake one clock read and one atomic load.
func (self *tlsTicketKeys) refresh() {
	day := tlsTicketDay(self.now())
	if self.installedDay.Load() == day {
		return
	}
	self.setKeys(day)
}

// Sets the keys of day on every config, once per change of day.
func (self *tlsTicketKeys) setKeys(day int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.installedDay.Load() == day {
		return
	}
	keys := tlsTicketKeysOfDay(self.secrets, day)
	for _, tlsConfig := range self.tlsConfigs {
		tlsConfig.SetSessionTicketKeys(keys)
	}
	self.installedDay.Store(day)
}
