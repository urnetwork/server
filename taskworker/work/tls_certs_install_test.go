package work

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// taskworker's certs install (QD-846; tls_certs_install.go) over fakes and
// fixture files: with sidecar.yml's switch off nothing is built or read and
// the chains run empty; with it on and the files there the chains run on the
// engine with its DNS client and ACME client; a missing or refused file leaves
// them uninstalled, named in the log, and no secret's value is said.

const (
	tlsInstallTestRoot        = "root.example"
	tlsInstallTestAccountFile = "certs-account-letsencrypt.yml"
	tlsInstallTestGtsFile     = "certs-account-gts.yml"
)

// The install's world: the fixture files, the switch, and what was read and
// said.
type tlsChainsInstallTestWorld struct {
	t           *testing.T
	ctx         context.Context
	host        *sidecartest.FakeHost
	store       *sidecartest.FakeCertsStore
	on          bool
	configFiles map[string][]byte
	secretFiles map[string][]byte
	// each secret value a fixture file holds, which no line may carry
	secrets     []string
	configReads []string
	secretReads []string
	hostBuilds  int
	lines       []string
	install     *server.SidecarSwitchInstall
}

// An account file's text for ca with a new key, an EAB key when eab, and the
// secret values in it.
func tlsInstallTestAccount(t *testing.T, ca string, eab bool) (text string, secrets []string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPem := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	text = "ca: " + ca + "\nprivate_key: |\n"
	for _, line := range strings.Split(strings.TrimSuffix(keyPem, "\n"), "\n") {
		text += "  " + line + "\n"
		if !strings.HasPrefix(line, "-----") {
			secrets = append(secrets, line)
		}
	}
	text += "key_create_time: 2026-01-02T03:04:05Z\n"
	if eab {
		macKey := make([]byte, 32)
		if _, err := rand.Read(macKey); err != nil {
			t.Fatal(err)
		}
		macKeyText := base64.RawURLEncoding.EncodeToString(macKey)
		text += "eab_key_id: fixture-eab-key-id\neab_mac_key: " + macKeyText + "\n"
		secrets = append(secrets, macKeyText)
	}
	return text, secrets
}

func newTlsChainsInstallTestWorld(t *testing.T) *tlsChainsInstallTestWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
	sidecartest.RegisterCertsBudgetTwins(host.Redis)
	world := &tlsChainsInstallTestWorld{
		t:           t,
		ctx:         ctx,
		host:        host,
		store:       sidecartest.NewFakeCertsStore(nil),
		configFiles: map[string][]byte{},
		secretFiles: map[string][]byte{},
	}
	world.configFiles[tlsCertChainsConfigFileName] = []byte(`root: ` + tlsInstallTestRoot + `
dns_url: https://dns.fixture.example
cas:
  letsencrypt:
    directory_url: https://acme.fixture.example/directory
    account_file: ` + tlsInstallTestAccountFile + `
  gts:
    directory_url: https://gts.fixture.example/directory
    account_file: ` + tlsInstallTestGtsFile + `
`)
	account, secrets := tlsInstallTestAccount(t, "letsencrypt", false)
	world.secretFiles[tlsInstallTestAccountFile] = []byte(account)
	world.secrets = append(world.secrets, secrets...)
	account, secrets = tlsInstallTestAccount(t, "gts", true)
	world.secretFiles[tlsInstallTestGtsFile] = []byte(account)
	world.secrets = append(world.secrets, secrets...)
	bearer := make([]byte, 32)
	if _, err := rand.Read(bearer); err != nil {
		t.Fatal(err)
	}
	bearerText := base64.RawURLEncoding.EncodeToString(bearer)
	world.secretFiles[server.CertsDnsCredentialFileName] = []byte("current: " + bearerText + "\n")
	world.secrets = append(world.secrets, bearerText)

	// the chains start with nothing installed, as a process does
	SetTlsCertChains(nil)
	read := func(mount string, files map[string][]byte, reads *[]string) func(name string) ([]byte, error) {
		return func(name string) ([]byte, error) {
			*reads = append(*reads, name)
			data, ok := files[name]
			if !ok {
				return nil, fmt.Errorf("%w: %s holds no %s", server.ErrSidecarInstallFile, mount, name)
			}
			return data, nil
		}
	}
	world.install = NewTlsCertChainsInstall(&TlsCertChainsInstallDeps{
		SwitchOn: func() bool {
			return world.on
		},
		ReadConfigFile: read("config", world.configFiles, &world.configReads),
		ReadSecretFile: read("vault", world.secretFiles, &world.secretReads),
		Host: func(ctx context.Context) (*sidecar.ServiceHost, error) {
			world.hostBuilds += 1
			return world.host.ServiceHost(), nil
		},
		Store: world.store,
		// the sweep's Postgres source is not this test's: a source that finds no cause
		RevocationSource: tlsInstallTestNoCauses{},
	}, nil)
	world.install.Testing_SetLog(func(format string, arguments ...any) {
		world.lines = append(world.lines, fmt.Sprintf(format, arguments...))
	})
	t.Cleanup(func() {
		world.install.Close()
		SetTlsCertChains(nil)
	})
	return world
}

type tlsInstallTestNoCauses struct{}

func (tlsInstallTestNoCauses) RevocationCauses(ctx context.Context, candidates []*certsservice.CertsRevocationCandidate) (map[sidecar.Id]certsservice.CertsRevocationCause, error) {
	return map[sidecar.Id]certsservice.CertsRevocationCause{}, nil
}

// Whether a pass of each chain is idle: nothing installed, or a binary with no
// tls tables.
func (self *tlsChainsInstallTestWorld) idle() (orders bool, renewals bool, revocations bool) {
	self.t.Helper()
	clientSession := session.NewLocalClientSession(self.ctx, "", nil)
	defer clientSession.Cancel()
	ordersResult, err := TlsCertOrders(&TlsCertOrdersArgs{}, clientSession)
	if err != nil {
		self.t.Fatal(err)
	}
	renewalsResult, err := TlsCertRenewals(&TlsCertRenewalsArgs{}, clientSession)
	if err != nil {
		self.t.Fatal(err)
	}
	revocationsResult, err := TlsCertRevocations(&TlsCertRevocationsArgs{}, clientSession)
	if err != nil {
		self.t.Fatal(err)
	}
	return ordersResult.Idle, renewalsResult.Idle, revocationsResult.Idle
}

// No line said carries a fixture secret's value.
func (self *tlsChainsInstallTestWorld) requireNoSecretSaid() {
	self.t.Helper()
	for _, line := range self.lines {
		for _, secret := range self.secrets {
			if strings.Contains(line, secret) {
				self.t.Fatalf("a secret's value is in the log line %q", line)
			}
		}
	}
}

// With the switch off nothing is built, no file is read, and each chain runs
// an empty pass.
func TestTlsCertChainsInstallOffInstallsNothing(t *testing.T) {
	world := newTlsChainsInstallTestWorld(t)
	for range 2 {
		if world.install.Sync(world.ctx) || world.install.Installed() {
			t.Fatal("installed with the switch off")
		}
	}
	if tlsCertChainDeps.Load() != nil {
		t.Fatal("the chains hold deps with the switch off")
	}
	if len(world.configReads) != 0 || len(world.secretReads) != 0 || world.hostBuilds != 0 || len(world.lines) != 0 {
		t.Fatalf("the switch off read %v and %v, built %d hosts and said %q", world.configReads, world.secretReads, world.hostBuilds, world.lines)
	}
	if orders, renewals, revocations := world.idle(); !orders || !renewals || !revocations {
		t.Fatalf("a pass with the switch off is not idle: %t %t %t", orders, renewals, revocations)
	}
}

// With the switch on and the fixture files the chains run on the engine: its
// DNS client on certs.yml's dns_url, its ACME client on the account files, its
// hint writer. Off again, they run empty passes; on again, the files are read
// anew.
func TestTlsCertChainsInstallOnInstallsTheEngine(t *testing.T) {
	world := newTlsChainsInstallTestWorld(t)
	world.on = true
	if !world.install.Sync(world.ctx) || !world.install.Installed() {
		t.Fatalf("not installed with the switch on: %q", world.lines)
	}
	deps := tlsCertChainDeps.Load()
	if deps == nil || deps.Service == nil || deps.RevocationSource == nil {
		t.Fatal("the chains hold no deps with the switch on")
	}
	if _, ok := deps.Service.Dns().(*server.CertsDnsClient); !ok {
		t.Fatalf("the engine's DNS client is %T", deps.Service.Dns())
	}
	if _, ok := deps.Service.Acme().(*certsservice.CertsAcmeClient); !ok {
		t.Fatalf("the engine's ACME client is %T", deps.Service.Acme())
	}
	if !slices.Equal(world.configReads, []string{tlsCertChainsConfigFileName}) {
		t.Fatalf("config files read: %v", world.configReads)
	}
	// each account file certs.yml names, in CA order, then the dns bearer
	if !slices.Equal(world.secretReads, []string{tlsInstallTestGtsFile, tlsInstallTestAccountFile, server.CertsDnsCredentialFileName}) {
		t.Fatalf("secret files read: %v", world.secretReads)
	}
	// a pass runs on the engine wherever the binary links the tls tables
	orders, renewals, revocations := world.idle()
	if want := !server.SidecarMg5Linked(); orders != want || renewals != want || revocations != want {
		t.Fatalf("a pass with the switch on: idle %t %t %t, want %t", orders, renewals, revocations, want)
	}
	// steps while on build nothing again
	world.install.Sync(world.ctx)
	if world.hostBuilds != 1 || len(world.configReads) != 1 {
		t.Fatalf("a second step built again: %d hosts, %d config reads", world.hostBuilds, len(world.configReads))
	}

	world.on = false
	if world.install.Sync(world.ctx) || tlsCertChainDeps.Load() != nil {
		t.Fatal("still installed with the switch off again")
	}
	if orders, renewals, revocations := world.idle(); !orders || !renewals || !revocations {
		t.Fatal("a pass with the switch off again is not idle")
	}

	world.on = true
	if !world.install.Sync(world.ctx) || world.hostBuilds != 2 || len(world.configReads) != 2 {
		t.Fatalf("the second on: %d hosts built", world.hostBuilds)
	}
	if again := tlsCertChainDeps.Load(); again == nil || again == deps {
		t.Fatal("the second on did not build the engine anew")
	}
	world.requireNoSecretSaid()
}

// A certs.yml with no dns_url reads no bearer and leaves the engine on the
// closed DNS client; a CA with no account file has no account read.
func TestTlsCertChainsInstallWithoutDnsUrlReachesNoZone(t *testing.T) {
	world := newTlsChainsInstallTestWorld(t)
	world.configFiles[tlsCertChainsConfigFileName] = []byte(`root: ` + tlsInstallTestRoot + `
cas:
  letsencrypt:
    account_file: ` + tlsInstallTestAccountFile + `
`)
	delete(world.secretFiles, server.CertsDnsCredentialFileName)
	delete(world.secretFiles, tlsInstallTestGtsFile)
	world.on = true
	if !world.install.Sync(world.ctx) {
		t.Fatalf("not installed: %q", world.lines)
	}
	deps := tlsCertChainDeps.Load()
	if _, ok := deps.Service.Dns().(certsservice.ClosedCertsDns); !ok {
		t.Fatalf("the engine's DNS client with no dns_url is %T", deps.Service.Dns())
	}
	if !slices.Equal(world.secretReads, []string{tlsInstallTestAccountFile}) {
		t.Fatalf("secret files read: %v", world.secretReads)
	}
}

// A missing file leaves the chains uninstalled and is named in the log once:
// certs.yml, an account file it names, internal-certs.yml. When the file is
// there the next step installs.
func TestTlsCertChainsInstallMissingFileLeavesThemUninstalled(t *testing.T) {
	for _, c := range []struct {
		name   string
		config bool
	}{
		{tlsCertChainsConfigFileName, true},
		{tlsInstallTestAccountFile, false},
		{tlsInstallTestGtsFile, false},
		{server.CertsDnsCredentialFileName, false},
	} {
		world := newTlsChainsInstallTestWorld(t)
		files := world.secretFiles
		if c.config {
			files = world.configFiles
		}
		kept := files[c.name]
		delete(files, c.name)
		world.on = true
		for range 3 {
			if world.install.Sync(world.ctx) || world.install.Installed() {
				t.Fatalf("installed with no %s", c.name)
			}
		}
		if tlsCertChainDeps.Load() != nil {
			t.Fatalf("the chains hold deps with no %s", c.name)
		}
		if len(world.lines) != 1 || !strings.Contains(world.lines[0], c.name) || !strings.Contains(world.lines[0], "stays uninstalled") {
			t.Fatalf("no %s said %q", c.name, world.lines)
		}
		if world.hostBuilds != 0 {
			t.Fatalf("a host was built with no %s", c.name)
		}
		if orders, renewals, revocations := world.idle(); !orders || !renewals || !revocations {
			t.Fatalf("a pass with no %s is not idle", c.name)
		}
		world.requireNoSecretSaid()

		files[c.name] = kept
		if !world.install.Sync(world.ctx) || tlsCertChainDeps.Load() == nil {
			t.Fatalf("not installed once %s is there: %q", c.name, world.lines)
		}
		world.requireNoSecretSaid()
		world.install.Close()
	}
}

// A file that is there and refused leaves the chains uninstalled, named, with
// nothing the file holds in the log: an account key that is no key, an account
// file that does not parse, one that holds another CA's account, an EAB key
// that is not base64url, a bearer that is not 256 bits, a credential file that
// does not parse, and a certs.yml with an unknown key.
func TestTlsCertChainsInstallRefusedFilesSayNoSecret(t *testing.T) {
	const marker = "5ecr3t-fixture-value-that-must-not-be-said"
	otherCa, otherSecrets := tlsInstallTestAccount(t, "zerossl", false)
	for _, c := range []struct {
		name   string
		config bool
		text   string
		// whether a parser's own text may quote the file, which is no secret
		quotes bool
	}{
		{tlsInstallTestAccountFile, false, "ca: letsencrypt\nprivate_key: " + marker + "\n", false},
		{tlsInstallTestAccountFile, false, "ca: letsencrypt\nprivate_key: [" + marker + "\n", false},
		{tlsInstallTestAccountFile, false, otherCa, false},
		{tlsInstallTestGtsFile, false, string(mustTlsInstallTestAccountWithEab(t, marker+"!")), false},
		{server.CertsDnsCredentialFileName, false, "current: " + marker + "\n", false},
		{server.CertsDnsCredentialFileName, false, "current: [" + marker + "\n", false},
		{server.CertsDnsCredentialFileName, false, "bearer: " + marker + "\n", false},
		{tlsCertChainsConfigFileName, true, "root: " + tlsInstallTestRoot + "\nbudget_cap_typo: 5\n", true},
	} {
		world := newTlsChainsInstallTestWorld(t)
		world.secrets = append(world.secrets, otherSecrets...)
		if !c.quotes {
			world.secrets = append(world.secrets, marker)
		}
		if c.config {
			world.configFiles[c.name] = []byte(c.text)
		} else {
			world.secretFiles[c.name] = []byte(c.text)
		}
		world.on = true
		if world.install.Sync(world.ctx) || tlsCertChainDeps.Load() != nil {
			t.Fatalf("installed on a refused %s (%q)", c.name, c.text)
		}
		if len(world.lines) != 1 || !strings.Contains(world.lines[0], c.name) {
			t.Fatalf("a refused %s said %q", c.name, world.lines)
		}
		world.requireNoSecretSaid()
		world.install.Close()
	}
}

// A gts account file whose EAB MAC key is macKey.
func mustTlsInstallTestAccountWithEab(t *testing.T, macKey string) []byte {
	t.Helper()
	text, _ := tlsInstallTestAccount(t, "gts", false)
	return []byte(text + "eab_key_id: fixture-eab-key-id\neab_mac_key: \"" + macKey + "\"\n")
}

// The install as taskworker starts it reads the process's own sidecar.yml,
// config mount and vault mount: off in sidecar.yml nothing is installed, and
// on, with the fixture files pushed into the mounts, the engine is.
func TestTlsCertChainsInstallFollowsSidecarYmlAndTheMounts(t *testing.T) {
	world := newTlsChainsInstallTestWorld(t)
	for name, data := range world.configFiles {
		defer server.Config.PushSimpleResource(name, data)()
	}
	for name, data := range world.secretFiles {
		defer server.Vault.PushSimpleResource(name, data)()
	}
	install := NewTlsCertChainsInstall(&TlsCertChainsInstallDeps{
		Host: func(ctx context.Context) (*sidecar.ServiceHost, error) {
			return world.host.ServiceHost(), nil
		},
		Store:            world.store,
		RevocationSource: tlsInstallTestNoCauses{},
	}, nil)
	install.Testing_SetLog(func(format string, arguments ...any) {
		world.lines = append(world.lines, fmt.Sprintf(format, arguments...))
	})
	defer install.Close()

	func() {
		defer server.Testing_PushSidecarConfig("sidecar:\n  certs: {enabled: false}\n  tlsedge: {enabled: true, percent: 100}\n")()
		if install.Sync(world.ctx) || tlsCertChainDeps.Load() != nil {
			t.Fatal("installed with sidecar.yml's certs off")
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig("sidecar:\n  certs: {enabled: true}\n")()
		if !install.Sync(world.ctx) || tlsCertChainDeps.Load() == nil {
			t.Fatalf("not installed with sidecar.yml's certs on: %q", world.lines)
		}
	}()
	func() {
		defer server.Testing_PushSidecarConfig("sidecar: {}\n")()
		if install.Sync(world.ctx) || tlsCertChainDeps.Load() != nil {
			t.Fatal("still installed once sidecar.yml names no certs switch")
		}
	}()
	world.requireNoSecretSaid()
}
