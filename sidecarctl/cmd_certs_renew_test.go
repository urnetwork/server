// Tests for `sidecarctl certs renew`. Each runs renewCertificates against a builder vault that is a
// real git checkout of a bare origin, on a fixed clock, with lego, the CA's ARI and the swap
// replaced: lego by certsRenewFakeLego, which writes what an order would, signed by a test CA, the
// ARI by a local server, and the swap by the real swapTlsCurrent against a test lb host's vault. No
// test reaches docker, a CA or a host.

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The clock every renewal test runs at.
var certsRenewTestNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// The version a renewal at certsRenewTestNow writes.
const certsRenewTestVersion = "2026.11.8"

// The life of every test certificate, Let's Encrypt's from 2028.
const certsRenewTestLife = 45 * 24 * time.Hour

// What the test services.yml gives the env main, by registered domain, in its order.
var (
	certsRenewTestComHostnames = []string{"example.com", "*.example.com", "main-lb.example.com", "main-api.example.com"}
	certsRenewTestNetHostnames = []string{"example.net", "main-lb.example.net", "main-api.example.net"}
)

// A services.yml whose env main has a route53 domain with a wildcard and a cloudflare domain.
const certsRenewTestServicesYml = `domain: example.com
domains:
  example.com: route53
  example.net: cloudflare
expose_aliases:
  - "*.example.com"
versions:
  - services:
      api: {}
`

// The token the test home's cloudflare credentials hold for example.net.
const certsRenewTestCloudflareToken = "synthetic-test-token-not-a-credential"

// A CA that signs the test certificates, so that each carries the authority key id ARI reads.
type certsRenewTestCa struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPem []byte
}

// A new CA, valid around the test clock.
func newCertsRenewTestCa(t *testing.T) *certsRenewTestCa {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "certs renew test ca"},
		NotBefore:             certsRenewTestNow.Add(-365 * 24 * time.Hour),
		NotAfter:              certsRenewTestNow.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	certDer, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDer)
	if err != nil {
		t.Fatal(err)
	}
	return &certsRenewTestCa{
		cert:    cert,
		key:     key,
		certPem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDer}),
	}
}

// A leaf for hostnames, valid from notBefore to notAfter, with its key, both as pem.
func (self *certsRenewTestCa) issue(t *testing.T, hostnames []string, notBefore time.Time, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostnames[0]},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     hostnames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDer, err := x509.CreateCertificate(rand.Reader, template, self.cert, key.Public(), self.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDer, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDer}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDer})
}

// The test's builder vault, a checkout of a bare origin, with the CA its certificates come from and
// a git that reads none of the machine's configuration.
type certsRenewTestVault struct {
	t          *testing.T
	home       string
	originHome string
	gitEnv     []string
	ca         *certsRenewTestCa
	// holds .aws and .cloudflare/credentials, with a synthetic token
	userHome string
}

// A vault holding the test services.yml for main and no certificate, pushed to its origin.
func newCertsRenewTestVault(t *testing.T) *certsRenewTestVault {
	t.Helper()
	gitHome := t.TempDir()
	emptyConfigPath := filepath.Join(gitHome, "gitconfig")
	if err := os.WriteFile(emptyConfigPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	gitEnv := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, "HOME=") {
			gitEnv = append(gitEnv, entry)
		}
	}
	gitEnv = append(gitEnv,
		"HOME="+gitHome,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+emptyConfigPath,
		"GIT_AUTHOR_NAME=certs renew test",
		"GIT_AUTHOR_EMAIL=renew@example.com",
		"GIT_COMMITTER_NAME=certs renew test",
		"GIT_COMMITTER_EMAIL=renew@example.com",
	)
	userHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userHome, ".aws"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(userHome, ".cloudflare"), 0700); err != nil {
		t.Fatal(err)
	}
	credentials := fmt.Sprintf("cloudflare_dns_api_tokens:\n  example.net: %s\n", certsRenewTestCloudflareToken)
	if err := os.WriteFile(filepath.Join(userHome, ".cloudflare", "credentials"), []byte(credentials), 0600); err != nil {
		t.Fatal(err)
	}
	vault := &certsRenewTestVault{
		t:          t,
		home:       filepath.Join(t.TempDir(), "vault"),
		originHome: filepath.Join(t.TempDir(), "origin.git"),
		gitEnv:     gitEnv,
		ca:         newCertsRenewTestCa(t),
		userHome:   userHome,
	}
	vault.runGit("", "init", "-q", "-b", "main", vault.home)
	vault.writeFile("main/services.yml", certsRenewTestServicesYml)
	vault.commitAll("Add services.yml")
	vault.runGit("", "init", "-q", "--bare", "-b", "main", vault.originHome)
	vault.runGit(vault.home, "remote", "add", "origin", vault.originHome)
	vault.runGit(vault.home, "push", "-q", "-u", "origin", "main")
	return vault
}

// Runs git in dir, or with no -C for an empty dir, and returns what it printed.
func (self *certsRenewTestVault) runGit(dir string, args ...string) string {
	self.t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = self.gitEnv
	output, err := cmd.CombinedOutput()
	if err != nil {
		self.t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

// Writes a file in the vault, relative to its top.
func (self *certsRenewTestVault) writeFile(relativePath string, content string) {
	self.t.Helper()
	path := filepath.Join(self.home, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		self.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		self.t.Fatal(err)
	}
}

// Commits everything in the vault and pushes it once there is an origin.
func (self *certsRenewTestVault) commitAll(message string) {
	self.t.Helper()
	self.runGit(self.home, "add", "-A")
	self.runGit(self.home, "commit", "-q", "-m", message)
	if _, err := os.Stat(self.originHome); err == nil {
		self.runGit(self.home, "push", "-q")
	}
}

// Writes one certificate for hostnames into tlsHome/version, each name's directory holding it as
// `warpctl certs issue` files it, positioned so that the test clock is lifeFraction of its life.
// Returns the leaf.
func (self *certsRenewTestVault) writeVersion(tlsHome string, version string, hostnames []string, lifeFraction float64) *x509.Certificate {
	self.t.Helper()
	notBefore := certsRenewTestNow.Add(-time.Duration(lifeFraction * float64(certsRenewTestLife)))
	certPem, keyPem := self.ca.issue(self.t, hostnames, notBefore, notBefore.Add(certsRenewTestLife))
	for _, host := range hostnames {
		name := certsRenewCertName(host)
		self.writeFile(filepath.Join(tlsHome, version, name, name+".crt"), string(certPem))
		self.writeFile(filepath.Join(tlsHome, version, name, "ca.crt"), string(self.ca.certPem))
		self.writeFile(filepath.Join(tlsHome, version, name, name+".key"), string(keyPem))
		self.writeFile(filepath.Join(tlsHome, version, name, name+".pem"), string(certPem)+string(self.ca.certPem))
	}
	block, _ := pem.Decode(certPem)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		self.t.Fatal(err)
	}
	return leaf
}

// The commit subjects reachable from a ref, newest first.
func (self *certsRenewTestVault) subjects(dir string, ref string) []string {
	self.t.Helper()
	return strings.Split(strings.TrimSpace(self.runGit(dir, "log", "--format=%s", ref)), "\n")
}

// A run's settings against this vault: the test clock, lego and git, a CA that offers no ARI, and a
// swap that answers nothing until a test gives it one. The log is kept for tests that read it.
func (self *certsRenewTestVault) settings(lego *certsRenewFakeLego, acme *certsRenewTestAcme, log io.Writer) *certsRenewSettings {
	settings := defaultCertsRenewSettings()
	settings.now = func() time.Time {
		return certsRenewTestNow
	}
	settings.acmeDirectoryUrl = acme.server.URL + "/directory"
	settings.userHome = self.userHome
	settings.log = log
	settings.runLego = lego.run
	settings.git = func(vaultHome string, args []string) error {
		cmd := exec.Command("git", append([]string{"-C", vaultHome}, args...)...)
		cmd.Env = self.gitEnv
		cmd.Stdout = log
		cmd.Stderr = log
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}
	return settings
}

// Stands in for docker running lego. It reads an order's lego home and names from its arguments
// and writes what lego would, a certificate signed by the test CA and valid from the test clock,
// unless the order's first name fails.
type certsRenewFakeLego struct {
	t  *testing.T
	ca *certsRenewTestCa
	// the arguments of each run, in order
	runArgs [][]string
	// the names of each order that wrote a certificate
	orderedHostnames [][]string
	// how many more runs fail for an order's first name, -1 for every run
	firstNameFailCounts map[string]int
	// edits what a successful run writes, from the names it asked for: the names the certificate
	// covers, its validity, and whether its key is another's
	certify func(hostnames []string) (certHostnames []string, notBefore time.Time, notAfter time.Time, otherKey bool)
	// what a cloudflare order's env file held while it ran
	envFileContents []string
}

// A lego whose every order succeeds.
func newCertsRenewFakeLego(t *testing.T, ca *certsRenewTestCa) *certsRenewFakeLego {
	return &certsRenewFakeLego{
		t:                   t,
		ca:                  ca,
		firstNameFailCounts: map[string]int{},
		certify: func(hostnames []string) ([]string, time.Time, time.Time, bool) {
			return hostnames, certsRenewTestNow.Add(-time.Hour), certsRenewTestNow.Add(certsRenewTestLife), false
		},
	}
}

// One `docker run` of lego.
func (self *certsRenewFakeLego) run(ctx context.Context, args []string) error {
	self.runArgs = append(self.runArgs, args)
	legoHome := ""
	hostnames := []string{}
	for index, arg := range args {
		if index+1 == len(args) {
			break
		}
		next := args[index+1]
		switch {
		case arg == "-v" && strings.HasSuffix(next, ":/.lego:Z"):
			legoHome = strings.TrimSuffix(next, ":/.lego:Z")
		case arg == "--domains":
			hostnames = append(hostnames, next)
		case arg == "--env-file":
			content, err := os.ReadFile(next)
			if err != nil {
				return err
			}
			self.envFileContents = append(self.envFileContents, string(content))
		}
	}
	if legoHome == "" || len(hostnames) == 0 || args[len(args)-1] != "run" {
		return fmt.Errorf("the fake lego cannot read %v", args)
	}
	if failCount := self.firstNameFailCounts[hostnames[0]]; failCount != 0 {
		self.firstNameFailCounts[hostnames[0]] = failCount - 1
		return errors.New("the fake lego failed its order")
	}
	certHostnames, notBefore, notAfter, otherKey := self.certify(hostnames)
	certPem, keyPem := self.ca.issue(self.t, certHostnames, notBefore, notAfter)
	if otherKey {
		_, keyPem = self.ca.issue(self.t, certHostnames, notBefore, notAfter)
	}
	fileStem := strings.ReplaceAll(hostnames[0], "*", "_")
	fileNameContents := map[string][]byte{
		fileStem + ".crt":        append(slices.Clone(certPem), self.ca.certPem...),
		fileStem + ".issuer.crt": self.ca.certPem,
		fileStem + ".key":        keyPem,
	}
	if err := os.MkdirAll(filepath.Join(legoHome, "certificates"), 0700); err != nil {
		return err
	}
	for fileName, content := range fileNameContents {
		if err := os.WriteFile(filepath.Join(legoHome, "certificates", fileName), content, 0600); err != nil {
			return err
		}
	}
	self.orderedHostnames = append(self.orderedHostnames, hostnames)
	return nil
}

// Whether an order for the domain whose first name is firstName wrote a certificate.
func (self *certsRenewFakeLego) ordered(firstName string) bool {
	return slices.ContainsFunc(self.orderedHostnames, func(hostnames []string) bool {
		return hostnames[0] == firstName
	})
}

// A CA's ACME directory and ARI for the reader alone: no ARI unless window is set, which answers
// every certificate the same.
type certsRenewTestAcme struct {
	server *httptest.Server
	window *ariWindow
	// the ARI ids asked for, in order
	requestedIds []string
}

// A directory server, stopped when the test ends.
func newCertsRenewTestAcme(t *testing.T) *certsRenewTestAcme {
	acme := &certsRenewTestAcme{}
	acme.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/directory":
			directory := map[string]string{"newOrder": acme.server.URL + "/new-order"}
			if acme.window != nil {
				directory["renewalInfo"] = acme.server.URL + "/renewal-info"
			}
			json.NewEncoder(writer).Encode(directory)
		case strings.HasPrefix(request.URL.Path, "/renewal-info/") && acme.window != nil:
			acme.requestedIds = append(acme.requestedIds, strings.TrimPrefix(request.URL.Path, "/renewal-info/"))
			fmt.Fprintf(writer, `{"suggestedWindow": {"start": %q, "end": %q}}`, acme.window.start.Format(time.RFC3339), acme.window.end.Format(time.RFC3339))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(acme.server.Close)
	return acme
}

// Copies a directory tree of plain files.
func copyCertsRenewTree(t *testing.T, sourceHome string, destinationHome string) {
	t.Helper()
	err := filepath.WalkDir(sourceHome, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relativePath, err := filepath.Rel(sourceHome, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(destinationHome, relativePath), 0700)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(destinationHome, relativePath), content, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An lb host's flat vault, its tls/current links on the builder vault's version, and a swap that
// makes a version live on it as the job's would: the vault deploy merges the version from
// tls.pending, then certs swap moves the links, here with no lb to reload.
type certsRenewTestHost struct {
	t         *testing.T
	vault     *certsRenewTestVault
	vaultHome string
	// the versions swapped, in order
	swappedVersions []string
}

// A host whose links point at the builder vault's all/tls/<version>.
func newCertsRenewTestHost(t *testing.T, vault *certsRenewTestVault, version string) *certsRenewTestHost {
	t.Helper()
	host := &certsRenewTestHost{
		t:         t,
		vault:     vault,
		vaultHome: t.TempDir(),
	}
	copyCertsRenewTree(t, filepath.Join(vault.home, "all", "tls", version), filepath.Join(host.vaultHome, "tls", version))
	if err := swapTlsCurrent(host.vaultHome, "main", version, host.swapSettings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	return host
}

// A swap with no lb, on the test clock.
func (self *certsRenewTestHost) swapSettings() *certsSwapSettings {
	settings := defaultCertsSwapSettings()
	settings.noReload = true
	settings.now = func() time.Time {
		return certsRenewTestNow
	}
	return settings
}

// The settings' swapVersion.
func (self *certsRenewTestHost) swap(env string, version string) error {
	self.swappedVersions = append(self.swappedVersions, version)
	copyCertsRenewTree(self.t, filepath.Join(self.vault.home, "all", "tls.pending", version), filepath.Join(self.vaultHome, "tls", version))
	return swapTlsCurrent(self.vaultHome, env, version, self.swapSettings(), io.Discard)
}

// Each name's link target on the host.
func (self *certsRenewTestHost) linkTargets() map[string]string {
	self.t.Helper()
	entries, err := os.ReadDir(filepath.Join(self.vaultHome, "tls", tlsCurrentDirName))
	if err != nil {
		self.t.Fatal(err)
	}
	nameTargets := map[string]string{}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(self.vaultHome, "tls", tlsCurrentDirName, entry.Name()))
		if err != nil {
			self.t.Fatal(err)
		}
		nameTargets[entry.Name()] = target
	}
	return nameTargets
}

// The certificates of both test domains at the given shares of their life, in all/tls/2026.1.1,
// committed and pushed.
func setupCertsRenewVault(t *testing.T, comLifeFraction float64, netLifeFraction float64) *certsRenewTestVault {
	t.Helper()
	vault := newCertsRenewTestVault(t)
	vault.writeVersion("all/tls", "2026.1.1", certsRenewTestComHostnames, comLifeFraction)
	vault.writeVersion("all/tls", "2026.1.1", certsRenewTestNetHostnames, netLifeFraction)
	vault.commitAll("Add certificates 2026.1.1")
	return vault
}

// Fails unless each name of hostnames has a pair in tlsHome/version that loads, is valid at the
// test clock and covers the name.
func requireCertsRenewVersionServes(t *testing.T, vaultHome string, tlsHome string, version string, hostnames []string) {
	t.Helper()
	for _, host := range hostnames {
		name := certsRenewCertName(host)
		nameHome := filepath.Join(vaultHome, tlsHome, version, name)
		pair, err := tls.LoadX509KeyPair(filepath.Join(nameHome, name+".pem"), filepath.Join(nameHome, name+".key"))
		if err != nil {
			t.Errorf("%s does not load: %v", nameHome, err)
			continue
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if certsRenewTestNow.Before(leaf.NotBefore) || !certsRenewTestNow.Before(leaf.NotAfter) {
			t.Errorf("%s is valid from %s to %s", nameHome, leaf.NotBefore, leaf.NotAfter)
		}
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("%s: %v", nameHome, err)
		}
	}
}

// The warnings a run printed.
func certsRenewWarnings(out string) []string {
	warnings := []string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, certsRenewWarningPrefix) {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

// D-OPS-12's two thirds on the clock: a certificate at 60% of its life is not due, and one at 67%
// or 90% is, which orders its domain, here the route53 one, and no other.
func TestCertsRenewRenewsAtTwoThirdsOfLife(t *testing.T) {
	cases := []struct {
		lifeFraction float64
		wantOrdered  bool
	}{
		{lifeFraction: 0.60, wantOrdered: false},
		{lifeFraction: 0.67, wantOrdered: true},
		{lifeFraction: 0.90, wantOrdered: true},
	}
	for _, c := range cases {
		vault := setupCertsRenewVault(t, c.lifeFraction, 0.10)
		lego := newCertsRenewFakeLego(t, vault.ca)
		out := &bytes.Buffer{}
		err := renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), out)

		if ordered := lego.ordered("example.com"); ordered != c.wantOrdered {
			t.Errorf("at %.0f%% of life: ordered %v, want %v\n%s", c.lifeFraction*100, ordered, c.wantOrdered, out)
		}
		if lego.ordered("example.net") {
			t.Errorf("at %.0f%% of life: ordered example.net, which is at 10%%", c.lifeFraction*100)
		}
		// an order lands in tls.pending, where the default swap leaves it, which warns
		if (err != nil) != c.wantOrdered {
			t.Errorf("at %.0f%% of life: error %v", c.lifeFraction*100, err)
		}
	}
}

// A CA's window before two thirds pulls the renewal forward, as a CA asks for a certificate it
// means to revoke, and the reader asks for the certificate by its RFC 9773 id.
func TestCertsRenewTakesAnAriWindowBeforeTwoThirds(t *testing.T) {
	vault := newCertsRenewTestVault(t)
	comLeaf := vault.writeVersion("all/tls", "2026.1.1", certsRenewTestComHostnames, 0.50)
	vault.writeVersion("all/tls", "2026.1.1", certsRenewTestNetHostnames, 0.10)
	vault.commitAll("Add certificates 2026.1.1")
	acme := newCertsRenewTestAcme(t)
	acme.window = &ariWindow{start: certsRenewTestNow.Add(-time.Hour), end: certsRenewTestNow.Add(time.Hour)}
	lego := newCertsRenewFakeLego(t, vault.ca)
	out := &bytes.Buffer{}
	renewCertificates(vault.home, "main", vault.settings(lego, acme, io.Discard), out)

	if !lego.ordered("example.com") {
		t.Fatalf("a window around now did not order example.com\n%s", out)
	}
	comId, err := ariCertificateId(comLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(acme.requestedIds, comId) {
		t.Errorf("asked ARI for %v, not example.com's %s", acme.requestedIds, comId)
	}
	if !strings.Contains(out.String(), "in its CA's renewal window") {
		t.Errorf("the run does not say why it renewed:\n%s", out)
	}
}

// A window whose chosen time comes after the next run waits for a later run, with no warning.
func TestCertsRenewWaitsForAnAriWindowAfterTheNextRun(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.50, 0.10)
	acme := newCertsRenewTestAcme(t)
	acme.window = &ariWindow{start: certsRenewTestNow.Add(48 * time.Hour), end: certsRenewTestNow.Add(72 * time.Hour)}
	lego := newCertsRenewFakeLego(t, vault.ca)
	out := &bytes.Buffer{}
	err := renewCertificates(vault.home, "main", vault.settings(lego, acme, io.Discard), out)

	if err != nil || 0 < len(lego.runArgs) {
		t.Fatalf("a window two days off ordered %v, error %v\n%s", lego.orderedHostnames, err, out)
	}
	if len(acme.requestedIds) == 0 {
		t.Errorf("the run never asked the CA's ARI")
	}
}

// A window after two thirds holds nothing back: a renewal has to be tried by two thirds for a
// failure to warn with a third of the life left.
func TestCertsRenewRenewsPastTwoThirdsWhateverAriSays(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.70, 0.10)
	acme := newCertsRenewTestAcme(t)
	acme.window = &ariWindow{start: certsRenewTestNow.Add(10 * 24 * time.Hour), end: certsRenewTestNow.Add(11 * 24 * time.Hour)}
	lego := newCertsRenewFakeLego(t, vault.ca)
	renewCertificates(vault.home, "main", vault.settings(lego, acme, io.Discard), io.Discard)

	if !lego.ordered("example.com") {
		t.Fatalf("a late window held back a certificate past two thirds")
	}
}

// RFC 9773 section 4.1's example: the key identifier 69:88:5B:6B:87:46:40:41:E1:B3:7B:84:7B:A0:AE:2C:
// DE:01:C8:D4 and the serial 0x87654321, whose high bit takes a zero octet in DER.
func TestCertsRenewAriIdIsTheRfcs(t *testing.T) {
	authorityKeyId, err := hex.DecodeString("69885B6B87464041E1B37B847BA0AE2CDE01C8D4")
	if err != nil {
		t.Fatal(err)
	}
	ariId, err := ariCertificateId(&x509.Certificate{
		AuthorityKeyId: authorityKeyId,
		SerialNumber:   big.NewInt(0x87654321),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ariId != "aYhba4dGQEHhs3uEe6CuLN4ByNQ.AIdlQyE" {
		t.Errorf("ARI id %s, want the RFC's aYhba4dGQEHhs3uEe6CuLN4ByNQ.AIdlQyE", ariId)
	}
	if _, err := ariCertificateId(&x509.Certificate{SerialNumber: big.NewInt(1)}); err == nil {
		t.Errorf("a certificate with no authority key id has an ARI id")
	}
}

// The chosen time sits inside the window and is the same on every run, so that runs agree with no
// state between them, while two certificates spread over it.
func TestCertsRenewChoosesOneTimeInsideTheWindow(t *testing.T) {
	window := &ariWindow{start: certsRenewTestNow, end: certsRenewTestNow.Add(24 * time.Hour)}
	chosenTimes := map[time.Time]bool{}
	for index := 0; index < 16; index += 1 {
		ariId := fmt.Sprintf("aYhba4dGQEHhs3uEe6CuLN4ByNQ.%d", index)
		chosenTime := window.chooseTime(ariId)
		if chosenTime.Before(window.start) || !chosenTime.Before(window.end) {
			t.Errorf("%s chose %s, outside %s to %s", ariId, chosenTime, window.start, window.end)
		}
		if !window.chooseTime(ariId).Equal(chosenTime) {
			t.Errorf("%s chose two times", ariId)
		}
		chosenTimes[chosenTime] = true
	}
	if len(chosenTimes) < 8 {
		t.Errorf("16 certificates chose only %d times", len(chosenTimes))
	}
}

// The whole path: the due domain is ordered once and verified, added to all/tls.pending as one
// version and pushed, swapped live on the host through certs swap, then moved to all/tls and
// pushed again. The domain that was not due keeps its links.
func TestCertsRenewAddsAVersionAndSwapsIt(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.10)
	host := newCertsRenewTestHost(t, vault, "2026.1.1")
	lego := newCertsRenewFakeLego(t, vault.ca)
	settings := vault.settings(lego, newCertsRenewTestAcme(t), io.Discard)
	settings.swapVersion = host.swap
	out := &bytes.Buffer{}
	if err := renewCertificates(vault.home, "main", settings, out); err != nil {
		t.Fatalf("renewal error %v\n%s", err, out)
	}

	if len(lego.runArgs) != 1 || !lego.ordered("example.com") {
		t.Fatalf("lego ran %d times for %v, want once for example.com", len(lego.runArgs), lego.orderedHostnames)
	}
	if !slices.Equal(host.swappedVersions, []string{certsRenewTestVersion}) {
		t.Errorf("swapped %v, want %s", host.swappedVersions, certsRenewTestVersion)
	}
	requireCertsRenewVersionServes(t, vault.home, "all/tls", certsRenewTestVersion, certsRenewTestComHostnames)
	if _, err := os.Stat(filepath.Join(vault.home, "all", "tls.pending", certsRenewTestVersion)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the version is still in tls.pending: %v", err)
	}
	for _, host := range certsRenewTestNetHostnames {
		if _, err := os.Stat(filepath.Join(vault.home, "all", "tls", certsRenewTestVersion, certsRenewCertName(host))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the version holds %s, which was not due", host)
		}
	}
	wantSubjects := []string{
		"Move platform certificates 2026.11.8 out of tls.pending now every lb serves them",
		"Add renewed platform certificates 2026.11.8",
		"Add certificates 2026.1.1",
		"Add services.yml",
	}
	if subjects := vault.subjects(vault.originHome, "main"); !slices.Equal(subjects, wantSubjects) {
		t.Errorf("the origin holds %q, want %q", subjects, wantSubjects)
	}
	if status := vault.runGit(vault.home, "status", "--porcelain"); status != "" {
		t.Errorf("the run left the checkout dirty:\n%s", status)
	}
	linkTargets := host.linkTargets()
	for _, comHost := range certsRenewTestComHostnames {
		name := certsRenewCertName(comHost)
		if linkTargets[name] != "../2026.11.8/"+name {
			t.Errorf("the host's %s links to %q", name, linkTargets[name])
		}
	}
	for _, netHost := range certsRenewTestNetHostnames {
		name := certsRenewCertName(netHost)
		if linkTargets[name] != "../2026.1.1/"+name {
			t.Errorf("the host's %s links to %q, though it was not due", name, linkTargets[name])
		}
	}
}

// A failed order adds no version, commits nothing, swaps nothing and leaves every host's
// tls/current where it was, and it warns.
func TestCertsRenewLegoFailureLeavesTlsCurrentAlone(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.10)
	host := newCertsRenewTestHost(t, vault, "2026.1.1")
	linkTargetsBefore := host.linkTargets()
	lego := newCertsRenewFakeLego(t, vault.ca)
	lego.firstNameFailCounts["example.com"] = -1
	settings := vault.settings(lego, newCertsRenewTestAcme(t), io.Discard)
	settings.swapVersion = host.swap
	out := &bytes.Buffer{}
	err := renewCertificates(vault.home, "main", settings, out)

	if err == nil {
		t.Fatalf("a failed renewal exited 0\n%s", out)
	}
	if len(lego.runArgs) != settings.orderAttemptCount {
		t.Errorf("lego ran %d times, want %d", len(lego.runArgs), settings.orderAttemptCount)
	}
	if len(host.swappedVersions) != 0 {
		t.Errorf("a failed renewal swapped %v", host.swappedVersions)
	}
	if linkTargets := host.linkTargets(); !maps.Equal(linkTargets, linkTargetsBefore) {
		t.Errorf("the host's links moved from %v to %v", linkTargetsBefore, linkTargets)
	}
	if _, err := os.Stat(filepath.Join(vault.home, "all", "tls.pending")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed renewal wrote tls.pending: %v", err)
	}
	if subjects := vault.subjects(vault.originHome, "main"); len(subjects) != 2 {
		t.Errorf("a failed renewal pushed %q", subjects)
	}
	warnings := certsRenewWarnings(out.String())
	if !slices.ContainsFunc(warnings, func(warning string) bool { return strings.Contains(warning, "example.com: the renewal failed") }) {
		t.Errorf("no warning names the failed domain: %q", warnings)
	}
	if !slices.ContainsFunc(warnings, func(warning string) bool {
		return strings.Contains(warning, "example.com: 4 names are past their renewal point") && strings.Contains(warning, "nothing has replaced them")
	}) {
		t.Errorf("no warning says the names are still past their renewal point: %q", warnings)
	}
}

// A run retries a failed order up to its count, and a later attempt that succeeds renews.
func TestCertsRenewRetriesAFailedOrder(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.10)
	lego := newCertsRenewFakeLego(t, vault.ca)
	lego.firstNameFailCounts["example.com"] = 2
	renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), io.Discard)

	if len(lego.runArgs) != 3 || !lego.ordered("example.com") {
		t.Fatalf("lego ran %d times and ordered %v, want 3 runs ordering example.com", len(lego.runArgs), lego.orderedHostnames)
	}
	requireCertsRenewVersionServes(t, vault.home, "all/tls.pending", certsRenewTestVersion, certsRenewTestComHostnames)
}

// With no swap to make it live, which is production's state (QD-022), the version waits in
// all/tls.pending, committed and pushed, and every run warns of it, ordering nothing again, until a
// person finishes it with xops/certs/renew.md.
func TestCertsRenewStopsAtTlsPendingUntilSomethingSwaps(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.10)
	lego := newCertsRenewFakeLego(t, vault.ca)
	out := &bytes.Buffer{}
	err := renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), out)

	if err == nil || !strings.Contains(err.Error(), "xops/certs/renew.md") {
		t.Fatalf("error %v, want one sending a person to xops/certs/renew.md\n%s", err, out)
	}
	requireCertsRenewVersionServes(t, vault.home, "all/tls.pending", certsRenewTestVersion, certsRenewTestComHostnames)
	if subjects := vault.subjects(vault.originHome, "main"); subjects[0] != "Add renewed platform certificates 2026.11.8" {
		t.Errorf("the origin's newest commit is %q", subjects[0])
	}
	if _, err := os.Stat(filepath.Join(vault.home, "all", "tls", certsRenewTestVersion)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unswapped version moved to all/tls: %v", err)
	}
	waiting := func(warning string) bool {
		return strings.Contains(warning, "example.com: 4 names are past their renewal point") && strings.Contains(warning, "version 2026.11.8 waits in tls.pending")
	}
	if warnings := certsRenewWarnings(out.String()); !slices.ContainsFunc(warnings, waiting) {
		t.Errorf("no warning says the version waits: %q", warnings)
	}

	out.Reset()
	err = renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), out)
	if len(lego.runArgs) != 1 {
		t.Errorf("the next run ordered again: lego ran %d times", len(lego.runArgs))
	}
	if warnings := certsRenewWarnings(out.String()); err == nil || !slices.ContainsFunc(warnings, waiting) {
		t.Errorf("the next run did not warn of the waiting version: %v, %q", err, warnings)
	}
}

// A version is written once, so with today's already in the vault the run orders nothing and warns.
func TestCertsRenewOrdersNothingIntoATakenVersion(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.10)
	vault.writeVersion("all/tls.pending", certsRenewTestVersion, []string{"other.example.org"}, 0.01)
	vault.commitAll("Add a version issued today")
	lego := newCertsRenewFakeLego(t, vault.ca)
	out := &bytes.Buffer{}
	err := renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), out)

	if err == nil || len(lego.runArgs) != 0 {
		t.Fatalf("a taken version: error %v, lego ran %d times", err, len(lego.runArgs))
	}
	if !slices.ContainsFunc(certsRenewWarnings(out.String()), func(warning string) bool {
		return strings.Contains(warning, "version 2026.11.8 already exists")
	}) {
		t.Errorf("no warning names the taken version:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(vault.home, "all", "tls.pending", certsRenewTestVersion, "example.com")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run wrote into a version that existed: %v", err)
	}
}

// A certificate lego wrote that could not serve stays out of the vault: a key that is not its
// certificate's, a certificate missing a name, and one already expired.
func TestCertsRenewRefusesALegoCertificateThatCannotServe(t *testing.T) {
	cases := []struct {
		label   string
		certify func(hostnames []string) ([]string, time.Time, time.Time, bool)
	}{
		{
			label: "another key",
			certify: func(hostnames []string) ([]string, time.Time, time.Time, bool) {
				return hostnames, certsRenewTestNow.Add(-time.Hour), certsRenewTestNow.Add(certsRenewTestLife), true
			},
		},
		{
			label: "a missing name",
			certify: func(hostnames []string) ([]string, time.Time, time.Time, bool) {
				return hostnames[:1], certsRenewTestNow.Add(-time.Hour), certsRenewTestNow.Add(certsRenewTestLife), false
			},
		},
		{
			label: "an expired certificate",
			certify: func(hostnames []string) ([]string, time.Time, time.Time, bool) {
				return hostnames, certsRenewTestNow.Add(-2 * time.Hour), certsRenewTestNow.Add(-time.Hour), false
			},
		},
	}
	for _, c := range cases {
		vault := setupCertsRenewVault(t, 0.80, 0.10)
		lego := newCertsRenewFakeLego(t, vault.ca)
		lego.certify = c.certify
		log := &bytes.Buffer{}
		out := &bytes.Buffer{}
		err := renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), log), out)

		if err == nil {
			t.Errorf("%s: the run exited 0\n%s", c.label, out)
		}
		if _, err := os.Stat(filepath.Join(vault.home, "all", "tls.pending")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the certificate reached tls.pending: %v", c.label, err)
		}
		if !strings.Contains(log.String(), "[example.com]renewal failed") {
			t.Errorf("%s: the log does not say why:\n%s", c.label, log)
		}
	}
}

// One domain's failure costs only itself: the other's renewal lands as the version, and the
// failure warns.
func TestCertsRenewOneFailedDomainCostsOnlyItself(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.80)
	lego := newCertsRenewFakeLego(t, vault.ca)
	lego.firstNameFailCounts["example.net"] = -1
	out := &bytes.Buffer{}
	renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), out)

	requireCertsRenewVersionServes(t, vault.home, "all/tls.pending", certsRenewTestVersion, certsRenewTestComHostnames)
	if _, err := os.Stat(filepath.Join(vault.home, "all", "tls.pending", certsRenewTestVersion, "example.net")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed domain reached the version: %v", err)
	}
	if !slices.ContainsFunc(certsRenewWarnings(out.String()), func(warning string) bool {
		return strings.Contains(warning, "example.net: the renewal failed")
	}) {
		t.Errorf("no warning names the failed domain:\n%s", out)
	}
}

// A vault that will not pull stops the run before it orders anything, since an old checkout would
// order again what another run renewed.
func TestCertsRenewOrdersNothingWhenTheVaultWillNotPull(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.10)
	if err := os.RemoveAll(vault.originHome); err != nil {
		t.Fatal(err)
	}
	lego := newCertsRenewFakeLego(t, vault.ca)
	out := &bytes.Buffer{}
	err := renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), out)

	if err == nil || len(lego.runArgs) != 0 {
		t.Fatalf("a vault that will not pull: error %v, lego ran %d times", err, len(lego.runArgs))
	}
	if !slices.ContainsFunc(certsRenewWarnings(out.String()), func(warning string) bool {
		return strings.Contains(warning, "the vault will not pull")
	}) {
		t.Errorf("no warning says the vault will not pull:\n%s", out)
	}
}

// Every file and directory under root, with its mode, size, time and content's hash, or a link's
// target.
func snapshotCertsRenewTree(t *testing.T, root string) map[string]string {
	t.Helper()
	pathStates := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		state := fmt.Sprintf("%s %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			state += " -> " + target
		case info.Mode().IsRegular():
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state += fmt.Sprintf(" %x", sha256.Sum256(content))
		}
		pathStates[path] = state
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return pathStates
}

// A dry run says what is due and why, and touches no vault file: no pull, no lego, no write.
func TestCertsRenewDryRunTouchesNoVaultFile(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.10)
	before := snapshotCertsRenewTree(t, vault.home)
	lego := newCertsRenewFakeLego(t, vault.ca)
	settings := vault.settings(lego, newCertsRenewTestAcme(t), io.Discard)
	settings.dryRun = true
	gitArgs := [][]string{}
	settings.git = func(vaultHome string, args []string) error {
		gitArgs = append(gitArgs, args)
		return nil
	}
	out := &bytes.Buffer{}
	if err := renewCertificates(vault.home, "main", settings, out); err != nil {
		t.Fatalf("dry run error %v", err)
	}

	if len(lego.runArgs) != 0 || len(gitArgs) != 0 {
		t.Errorf("the dry run ran lego %v and git %v", lego.runArgs, gitArgs)
	}
	after := snapshotCertsRenewTree(t, vault.home)
	for path, state := range before {
		if after[path] != state {
			t.Errorf("the dry run changed %s: %q to %q", path, state, after[path])
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("the dry run added %s", path)
		}
	}
	if !strings.Contains(out.String(), "[example.com]due, ") || !strings.Contains(out.String(), "[example.net]not due") {
		t.Errorf("the dry run does not say what is due:\n%s", out)
	}
}

// The arguments are certs issue's for the pinned v4 image, with the CA named: lego v5 rejects
// `--accept-tos`, so a different image needs a different list, and this pins the two together. A
// cloudflare token rides in an env file, never in an argument.
func TestCertsRenewLegoArgsAreTheImagesCli(t *testing.T) {
	if !strings.HasPrefix(certsRenewLegoImage, "goacme/lego:v4.") || strings.Count(certsRenewLegoImage, ".") != 2 {
		t.Fatalf("certs renew runs %q; it must pin an exact lego v4 release (goacme/lego:v4.X.Y)", certsRenewLegoImage)
	}
	settings := defaultCertsRenewSettings()
	route53Args, err := certsRenewLegoArgs(&certsRenewDomain{domain: "example.com", registrar: "route53", hostnames: []string{"example.com", "*.example.com"}}, "/vault/.lego/renew.1", "/home/builder", "", settings)
	if err != nil {
		t.Fatal(err)
	}
	wantRoute53Args := []string{
		"--rm",
		"-e", "AWS_PROPAGATION_TIMEOUT=600",
		"-e", "AWS_POLLING_INTERVAL=30",
		"-e", "AWS_MAX_RETRIES=8",
		"-v", "/home/builder/.aws:/root/.aws:z",
		"-v", "/vault/.lego/renew.1:/.lego:Z",
		"goacme/lego:v4.35.2",
		"--accept-tos",
		"--server", "https://acme-v02.api.letsencrypt.org/directory",
		"--key-type", "ec256",
		"--dns", "route53",
		"--email", "admin@example.com",
		"--dns.resolvers", "1.1.1.1:53",
		"--dns.propagation-disable-ans",
		"--domains", "example.com",
		"--domains", "*.example.com",
		"run",
	}
	if !slices.Equal(route53Args, wantRoute53Args) {
		t.Errorf("route53 arguments\n%q\nwant\n%q", route53Args, wantRoute53Args)
	}
	cloudflareArgs, err := certsRenewLegoArgs(&certsRenewDomain{domain: "example.net", registrar: "cloudflare", hostnames: []string{"example.net"}}, "/vault/.lego/renew.1", "/home/builder", "/tmp/certs-renew-1/cloudflare.env", settings)
	if err != nil {
		t.Fatal(err)
	}
	wantCloudflareArgs := []string{
		"--rm",
		"--env-file", "/tmp/certs-renew-1/cloudflare.env",
		"-v", "/vault/.lego/renew.1:/.lego:Z",
		"goacme/lego:v4.35.2",
		"--accept-tos",
		"--server", "https://acme-v02.api.letsencrypt.org/directory",
		"--key-type", "ec256",
		"--dns", "cloudflare",
		"--email", "admin@example.net",
		"--dns.resolvers", "1.1.1.1:53",
		"--dns.propagation-wait", "20s",
		"--domains", "example.net",
		"run",
	}
	if !slices.Equal(cloudflareArgs, wantCloudflareArgs) {
		t.Errorf("cloudflare arguments\n%q\nwant\n%q", cloudflareArgs, wantCloudflareArgs)
	}
	if _, err := certsRenewLegoArgs(&certsRenewDomain{domain: "example.org", registrar: "", hostnames: []string{"example.org"}}, "/l", "/h", "", settings); err == nil {
		t.Errorf("a domain with no registrar has arguments")
	}
}

// certs renew runs the lego release `warpctl certs issue` runs, read from warpctl's source, so that
// moving one image fails here until the other moves with it.
func TestCertsRenewRunsTheLegoReleaseCertsIssueRuns(t *testing.T) {
	warpctlMainPath := filepath.Join("..", "..", "warp", "warpctl", "main.go")
	file, err := parser.ParseFile(token.NewFileSet(), warpctlMainPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	legoImage := ""
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec := spec.(*ast.ValueSpec)
			for index, name := range valueSpec.Names {
				if name.Name != "legoImage" || len(valueSpec.Values) <= index {
					continue
				}
				if literal, ok := valueSpec.Values[index].(*ast.BasicLit); ok {
					legoImage, _ = strconv.Unquote(literal.Value)
				}
			}
		}
	}
	if legoImage != certsRenewLegoImage {
		t.Fatalf("warpctl's certs issue runs %q and certs renew %q", legoImage, certsRenewLegoImage)
	}
}

// A cloudflare order reads its domain's token into an env file only it can read, which is gone
// once the order ends, and the token is in no argument.
func TestCertsRenewKeepsTheCloudflareTokenOffTheCommandLine(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.10, 0.80)
	lego := newCertsRenewFakeLego(t, vault.ca)
	renewCertificates(vault.home, "main", vault.settings(lego, newCertsRenewTestAcme(t), io.Discard), io.Discard)

	if len(lego.runArgs) != 1 || !lego.ordered("example.net") {
		t.Fatalf("lego ran %d times for %v, want once for example.net", len(lego.runArgs), lego.orderedHostnames)
	}
	for _, arg := range lego.runArgs[0] {
		if strings.Contains(arg, certsRenewTestCloudflareToken) {
			t.Errorf("an argument holds the token: %q", arg)
		}
	}
	if !slices.Equal(lego.envFileContents, []string{"CF_DNS_API_TOKEN=" + certsRenewTestCloudflareToken + "\n"}) {
		t.Errorf("the env files held %q", lego.envFileContents)
	}
	envFileIndex := slices.Index(lego.runArgs[0], "--env-file")
	envFilePath := lego.runArgs[0][envFileIndex+1]
	if strings.HasPrefix(envFilePath, vault.home) {
		t.Errorf("the env file %s is inside the vault checkout", envFilePath)
	}
	if _, err := os.Stat(envFilePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the env file outlived its order: %v", err)
	}
}

// The command finds the vault as warpctl does and takes a dry run, and refuses a line it cannot
// take. Every name here is past two thirds, so no ARI is asked of a real CA.
func TestCertsRenewCommandDryRunFindsTheVault(t *testing.T) {
	vault := setupCertsRenewVault(t, 0.80, 0.90)
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("WARP_VAULT_HOME", vault.home)
	out := &bytes.Buffer{}
	if err := runCertsRenew([]string{"main", "--dry-run"}, out); err != nil {
		t.Fatalf("dry run error %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "[example.com]due, ") || !strings.Contains(out.String(), "[example.net]due, ") {
		t.Errorf("the dry run printed\n%s", out)
	}

	for _, args := range [][]string{{}, {"main", "staging"}, {"main", "--force"}} {
		err := runCertsRenew(args, io.Discard)
		usageErr := &sidecarctlUsageError{}
		if !errors.As(err, &usageErr) {
			t.Errorf("%q: error %v, want a usage mistake", args, err)
		}
	}
	t.Setenv("WARP_HOME", "")
	if err := runCertsRenew([]string{"main", "--dry-run"}, io.Discard); err == nil || !strings.Contains(err.Error(), "WARP_HOME") {
		t.Errorf("with no WARP_HOME: error %v", err)
	}
}

// The binary's own registry holds certs renew, which its cmd file's init registers.
func TestSidecarctlRegistersCertsRenew(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"certs", "renew", "main", "--dry-run"})
	if command == nil || command.words != "certs renew" {
		t.Fatalf("the binary has no certs renew: %v", command)
	}
	if command.usage != "<env> [--dry-run]" || !slices.Equal(args, []string{"main", "--dry-run"}) {
		t.Fatalf("certs renew's usage is %q, with %v", command.usage, args)
	}
}
