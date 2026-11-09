package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/dnshost"
)

// The state directory's names. The KSK's private key stays at the top, which neither role mounts: only
// the api command signs windows with it, and the edge reads its public half as each root's anchor.
const (
	testDnsKskKeyFileName        = "ksk.key"
	testDnsKskPrivateFileName    = "ksk.private"
	testDnsZskKeyFileName        = "zsk.key"
	testDnsApiDirectoryName      = "api"
	testDnsEdgeDirectoryName     = "edge"
	testDnsSnapshotDirectoryName = "snapshots"
)

// The edge's warp service, whose bearer file it reads (internal-<service>.yml) and whose name its
// verify entry carries.
const testDnsEdgeService = "dnsedge-1"

// The bearer's length (plan-DNS-internal-route-credential: 256 random bits) and its text.
const testDnsBearerByteCount = 32

var testDnsBearerEncoding = base64.RawURLEncoding

// Owner-only, as warp mounts secret_files.
const (
	testDnsDirectoryMode = 0o700
	testDnsFileMode      = 0o600
)

// How the local KSK releases windows.
type TestDnsSettings struct {
	// A window's life: §12.6's 14 days.
	WindowLength time.Duration
	// How far apart windows start. One is released each step, so the one served has over
	// WindowLength - WindowStep left: 7 d keeps DNS-M5-17's check, over 7 d, clear.
	WindowStep time.Duration
	// How long before its step a window's signatures start, for a reader whose clock runs behind.
	WindowLead time.Duration
	// The DNSKEY RRset's TTL in seconds, which the KSK's signature covers.
	DnskeyTtl uint32
}

func DefaultTestDnsSettings() *TestDnsSettings {
	return &TestDnsSettings{
		WindowLength: 14 * 24 * time.Hour,
		WindowStep:   7 * 24 * time.Hour,
		WindowLead:   time.Hour,
		DnskeyTtl:    3600,
	}
}

// The key set's files, by path below the state directory.
func testDnsKeySetFiles() []string {
	return []string{
		testDnsKskKeyFileName,
		testDnsKskPrivateFileName,
		testDnsZskKeyFileName,
		filepath.Join(testDnsEdgeDirectoryName, dnshost.HostZskFileName),
		filepath.Join(testDnsEdgeDirectoryName, testDnsEdgeCredentialFileName()),
		filepath.Join(testDnsApiDirectoryName, dnshost.HostVerifyFileName),
	}
}

// The edge's bearer file in its secret directory.
func testDnsEdgeCredentialFileName() string {
	return "internal-" + testDnsEdgeService + ".yml"
}

// Makes state when it is missing, an absolute directory only its owner can enter, and refuses one that
// is a link, another user's, or open to anyone else, since it holds the edge's bearer and the KSK.
func checkTestDnsState(state string) error {
	if !filepath.IsAbs(state) {
		return fmt.Errorf("the state directory %q is not absolute", state)
	}
	if err := os.MkdirAll(state, testDnsDirectoryMode); err != nil {
		return err
	}
	info, err := os.Lstat(state)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("the state directory %s is not a directory", state)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the state directory %s is open to others (%v); it must be owner-only", state, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("the state directory %s belongs to another user", state)
	}
	for _, name := range []string{testDnsApiDirectoryName, testDnsEdgeDirectoryName, testDnsSnapshotDirectoryName} {
		if err := os.MkdirAll(filepath.Join(state, name), testDnsDirectoryMode); err != nil {
			return err
		}
	}
	return nil
}

// A zone key of the one algorithm §12.6 signs with, and its private key. miekg/dns will not sign under
// a key whose tag is 0, and about one random key in 65536 has that tag, so such a key is drawn again.
func newTestDnsKey(flags uint16, ttl uint32) (*dns.DNSKEY, crypto.Signer, error) {
	for {
		record := &dns.DNSKEY{
			Hdr:       dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: ttl},
			Flags:     flags,
			Protocol:  3,
			Algorithm: dns.ECDSAP256SHA256,
		}
		private, err := record.Generate(256)
		if err != nil {
			return nil, nil, err
		}
		signer, ok := private.(crypto.Signer)
		if !ok {
			return nil, nil, errors.New("the key generated is not a signer")
		}
		if record.KeyTag() != 0 {
			return record, signer, nil
		}
	}
}

// Makes the key set in state, or keeps the one there: a KSK and a ZSK, the ZSK's private key in the
// edge's secret directory, and a bearer for the edge with its digest in the API's. A state holding only
// some of the set, as a write a crash cut short leaves, is refused rather than mended.
func makeTestDnsKeys(state string, settings *TestDnsSettings) error {
	if err := checkTestDnsState(state); err != nil {
		return err
	}
	present := 0
	for _, name := range testDnsKeySetFiles() {
		if _, err := os.Lstat(filepath.Join(state, name)); err == nil {
			present += 1
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	switch present {
	case len(testDnsKeySetFiles()):
		// kept, once it reads whole
		_, err := loadTestDnsKeys(state, settings)
		return err
	case 0:
	default:
		return fmt.Errorf("%s holds part of a key set; remove it to start over", state)
	}

	ksk, kskSigner, err := newTestDnsKey(dns.ZONE|dns.SEP, settings.DnskeyTtl)
	if err != nil {
		return err
	}
	zsk, zskSigner, err := newTestDnsKey(dns.ZONE, settings.DnskeyTtl)
	if err != nil {
		return err
	}
	bearerBytes := make([]byte, testDnsBearerByteCount)
	if _, err := rand.Read(bearerBytes); err != nil {
		return err
	}
	bearer := testDnsBearerEncoding.EncodeToString(bearerBytes)
	digest := sha256.Sum256([]byte(bearer))

	files := []struct {
		name    string
		content string
	}{
		{testDnsKskKeyFileName, ksk.String() + "\n"},
		{testDnsKskPrivateFileName, ksk.PrivateKeyString(kskSigner)},
		{testDnsZskKeyFileName, zsk.String() + "\n"},
		{filepath.Join(testDnsEdgeDirectoryName, dnshost.HostZskFileName), zsk.PrivateKeyString(zskSigner)},
		{filepath.Join(testDnsEdgeDirectoryName, testDnsEdgeCredentialFileName()), fmt.Sprintf("current: %s\n", bearer)},
		{filepath.Join(testDnsApiDirectoryName, dnshost.HostVerifyFileName), fmt.Sprintf("%s: {class: %s, sha256: [%s]}\n", testDnsEdgeService, dnsservice.DnsCallerEdge, hex.EncodeToString(digest[:]))},
	}
	for _, file := range files {
		// O_EXCL: a file that appeared since the count above is another run's
		handle, err := os.OpenFile(filepath.Join(state, file.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, testDnsFileMode)
		if err != nil {
			return err
		}
		_, writeErr := handle.WriteString(file.content)
		syncErr := handle.Sync()
		closeErr := handle.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return err
		}
	}
	_, err = loadTestDnsKeys(state, settings)
	return err
}

// Reads one DNSKEY record file.
func readTestDnsKey(path string) (*dns.DNSKEY, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	record, err := dns.NewRR(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := record.(*dns.DNSKEY)
	if !ok || key.Algorithm != dns.ECDSAP256SHA256 || key.Flags&dns.ZONE == 0 {
		return nil, fmt.Errorf("%s is not a P-256 zone key", path)
	}
	return key, nil
}

// The public half of the key set, which is all the edge reads of it: each root's anchor is the KSK.
type testDnsPublicKeys struct {
	ksk *dns.DNSKEY
	zsk *dns.DNSKEY
}

func loadTestDnsPublicKeys(state string) (*testDnsPublicKeys, error) {
	ksk, err := readTestDnsKey(filepath.Join(state, testDnsKskKeyFileName))
	if err != nil {
		return nil, err
	}
	if ksk.Flags&dns.SEP == 0 {
		return nil, errors.New("the KSK is not a key-signing key")
	}
	zsk, err := readTestDnsKey(filepath.Join(state, testDnsZskKeyFileName))
	if err != nil {
		return nil, err
	}
	if zsk.Flags&dns.SEP != 0 {
		return nil, errors.New("the ZSK is a key-signing key")
	}
	return &testDnsPublicKeys{ksk: ksk, zsk: zsk}, nil
}

// One key's RDATA in presentation form, as dns.yml's trust_anchors and a window hold it.
func testDnsRdata(record dns.RR) string {
	return strings.TrimSpace(strings.TrimPrefix(record.String(), record.Header().String()))
}

// dns.yml's settings with the KSK as every root's trust anchor. A dns.yml that already names anchors
// for a root is refused: this KSK signs every window here, so another anchor could never serve.
func (self *testDnsPublicKeys) anchored(settings *dnsservice.DnsSettings) (*dnsservice.DnsSettings, error) {
	anchored := *settings
	anchored.TrustAnchors = map[string][]string{}
	for _, root := range settings.Roots {
		if 0 < len(settings.TrustAnchors[root]) {
			return nil, fmt.Errorf("dns.yml names trust anchors for %s; the test dns edge serves only under its own KSK", root)
		}
		anchored.TrustAnchors[root] = []string{testDnsRdata(self.ksk)}
	}
	if err := anchored.Validate(); err != nil {
		return nil, err
	}
	return &anchored, nil
}

// The whole key set, as the API's windows need it.
type testDnsKeys struct {
	*testDnsPublicKeys
	settings  *TestDnsSettings
	kskSigner crypto.Signer

	stateLock sync.Mutex
	// By root and window start, each signed once, so every feed answer in a step names one window.
	windows map[string]map[int64]*dnsservice.DnsKeyWindow
}

// Reads the key set from state, checking its parts agree: the ZSK the edge signs with is the one the
// windows publish, the KSK's private key is the anchor's, and the edge's bearer passes the API's check.
func loadTestDnsKeys(state string, settings *TestDnsSettings) (*testDnsKeys, error) {
	public, err := loadTestDnsPublicKeys(state)
	if err != nil {
		return nil, err
	}
	kskPath := filepath.Join(state, testDnsKskPrivateFileName)
	kskData, err := os.ReadFile(kskPath)
	if err != nil {
		return nil, err
	}
	kskPrivate, err := public.ksk.ReadPrivateKey(bytes.NewReader(kskData), kskPath)
	if err != nil {
		return nil, err
	}
	kskSigner, ok := kskPrivate.(crypto.Signer)
	if !ok || !testDnsSamePublicKey(public.ksk, kskSigner.Public()) {
		return nil, errors.New("the KSK's private key is not the anchor's")
	}
	zskData, err := os.ReadFile(filepath.Join(state, testDnsEdgeDirectoryName, dnshost.HostZskFileName))
	if err != nil {
		return nil, err
	}
	zsk, err := dnsservice.LoadZsk(zskData)
	if err != nil {
		return nil, err
	}
	if !testDnsSamePublicKey(public.zsk, zsk.Public()) {
		return nil, errors.New("the edge's ZSK is not the one the windows publish")
	}
	credentialData, err := os.ReadFile(filepath.Join(state, testDnsEdgeDirectoryName, testDnsEdgeCredentialFileName()))
	if err != nil {
		return nil, err
	}
	bearer, err := dnsservice.LoadDnsInternalCredential(credentialData)
	if err != nil {
		return nil, err
	}
	verifyData, err := os.ReadFile(filepath.Join(state, testDnsApiDirectoryName, dnshost.HostVerifyFileName))
	if err != nil {
		return nil, err
	}
	verifier, err := dnsservice.NewDnsInternalVerifier(verifyData)
	if err != nil {
		return nil, err
	}
	request := &http.Request{Header: http.Header{"Authorization": []string{"Bearer " + bearer}}}
	if class, ok := verifier.Authenticate(request); !ok || class != dnsservice.DnsCallerEdge {
		return nil, errors.New("the API's verify file does not take the edge's bearer")
	}
	return &testDnsKeys{
		testDnsPublicKeys: public,
		settings:          settings,
		kskSigner:         kskSigner,
		windows:           map[string]map[int64]*dnsservice.DnsKeyWindow{},
	}, nil
}

// Whether a DNSKEY carries public: a P-256 key's X and Y, RFC 6605 §4.
func testDnsSamePublicKey(record *dns.DNSKEY, public crypto.PublicKey) bool {
	key, ok := public.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return false
	}
	// the uncompressed point, 0x04 then X then Y
	point, err := key.Bytes()
	if err != nil || len(point) == 0 {
		return false
	}
	return base64.StdEncoding.EncodeToString(point[1:]) == record.PublicKey
}

// The window released for root at now, as dnshost.HostDeps.KeyWindow takes it: the DNSKEY RRset, the
// KSK and the ZSK, signed by the KSK from WindowLead before now's step for WindowLength. nil when the
// KSK cannot sign, which leaves an edge on the keys it serves.
func (self *testDnsKeys) Window(root string, now time.Time) *dnsservice.DnsKeyWindow {
	root = dns.CanonicalName(root)
	start := now.Truncate(self.settings.WindowStep)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	rootWindows, ok := self.windows[root]
	if !ok {
		rootWindows = map[int64]*dnsservice.DnsKeyWindow{}
		self.windows[root] = rootWindows
	}
	if window, ok := rootWindows[start.UnixNano()]; ok {
		return window
	}
	notBefore := start.Add(-self.settings.WindowLead)
	notAfter := start.Add(self.settings.WindowLength)
	rrset := []dns.RR{}
	for _, key := range []*dns.DNSKEY{self.ksk, self.zsk} {
		record := dns.Copy(key).(*dns.DNSKEY)
		record.Hdr.Name = root
		record.Hdr.Ttl = self.settings.DnskeyTtl
		rrset = append(rrset, record)
	}
	rrsig := &dns.RRSIG{
		Hdr:        dns.RR_Header{Name: root, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: self.settings.DnskeyTtl},
		Algorithm:  dns.ECDSAP256SHA256,
		KeyTag:     self.ksk.KeyTag(),
		SignerName: root,
		Inception:  uint32(notBefore.Unix()),
		Expiration: uint32(notAfter.Unix()),
	}
	if err := rrsig.Sign(self.kskSigner, rrset); err != nil {
		return nil
	}
	window := &dnsservice.DnsKeyWindow{
		Root:      root,
		NotBefore: notBefore,
		NotAfter:  notAfter,
		Dnskeys:   []string{testDnsRdata(rrset[0]), testDnsRdata(rrset[1])},
		Rrsigs:    []string{testDnsRdata(rrsig)},
	}
	rootWindows[start.UnixNano()] = window
	return window
}
