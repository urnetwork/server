// Tests for `sidecarctl certs swap`. Most run swapTlsCurrent against fakeLbDocker, a few run a
// real nginx through the paths an lb reads, and the last group runs the command itself, through
// runCertsSwap and as a process, for the wiring and the exit status a job relies on.

package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// What `docker top` prints for an lb draining its last connection, as a real nginx printed it
// after a QUIT.
const drainingLbTop = `PID                 COMMAND
1                   /usr/local/sbin/lb
7                   nginx: master process nginx -g daemon off; -c /srv/warp/nginx.conf/edge-0.example.com-eth0.conf
8                   nginx: worker process is shutting down
`

// Stands in for the docker cli as a swap's settings.docker, so that no test here can reach a real
// daemon. It answers the census with censusLines, and each container as a healthy lb reading the
// fixture's names through tls/current, unless told otherwise. A reload that takes gives the
// container a new worker pid.
type fakeLbDocker struct {
	t *testing.T
	// the vault an lb mounts, unless told otherwise
	vaultHome string
	// what `docker ps` prints, one `<id> <block>` per line
	censusLines []string
	// fails the census
	censusErr error
	// the tls lines grep finds in a container's config
	containerIdTlsLines map[string][]string
	// the worker count line grep finds there, where "" is none, in place of one worker
	containerIdWorkerProcessesLines map[string]string
	// fails the read of a container's tls paths
	containerIdTlsReadErrs map[string]error
	// what `docker top` prints for a container, in place of its one worker
	containerIdTopOutputs map[string]string
	// what successive `docker top` calls print for a container, each taken once, ahead of
	// everything else
	containerIdTopSequences map[string][]string
	// answers `docker top` for a container ahead even of those, as from a real process table
	containerIdTopFuncs map[string]func() ([]byte, error)
	// fails `docker top` of a container
	containerIdTopErrs map[string]error
	// what `readlink` prints in a container, in place of the links in vaultHome
	containerIdLinkOutputs map[string]string
	// fails `readlink` in a container
	containerIdLinkErrs map[string]error
	// containers that answer a reload and start no worker, as a draining nginx does
	deafContainerIds map[string]bool
	// how many looks at a container's workers pass before a reload's new worker shows
	slowReloadLooks int
	// fails the reload of a container
	containerIdReloadErrs map[string]error
	// runs at each reload, and answers it when it fails
	onReload func(containerId string) error

	censusArgs  [][]string
	tlsReadArgs [][]string
	reloadArgs  [][]string
	// the reloads that took
	containerIdReloadCounts map[string]int
	// the looks each container's last reload still waits for before it shows in `docker top`
	containerIdPendingLooks map[string]int
	// every docker command run, for the tests that must run none
	commandCount int
}

// Answers one docker command the way the real cli would.
func (self *fakeLbDocker) answer(ctx context.Context, args []string) ([]byte, error) {
	self.commandCount += 1
	switch args[0] {
	case "ps":
		self.censusArgs = append(self.censusArgs, args)
		if self.censusErr != nil {
			return nil, self.censusErr
		}
		return []byte(strings.Join(self.censusLines, "\n") + "\n"), nil
	case "top":
		containerId := args[1]
		if err := self.containerIdTopErrs[containerId]; err != nil {
			return nil, err
		}
		if topFunc := self.containerIdTopFuncs[containerId]; topFunc != nil {
			return topFunc()
		}
		if sequence := self.containerIdTopSequences[containerId]; 0 < len(sequence) {
			self.containerIdTopSequences[containerId] = sequence[1:]
			return []byte(sequence[0]), nil
		}
		if output, ok := self.containerIdTopOutputs[containerId]; ok {
			return []byte(output), nil
		}
		reloadCount := self.containerIdReloadCounts[containerId]
		if 0 < self.containerIdPendingLooks[containerId] {
			self.containerIdPendingLooks[containerId] -= 1
			reloadCount -= 1
		}
		top := "PID                 COMMAND\n"
		top += "1                   /usr/local/sbin/lb\n"
		top += "7                   nginx: master process nginx -g daemon off; -c /srv/warp/nginx.conf/edge-0.example.com-eth0.conf\n"
		top += fmt.Sprintf("%-20dnginx: worker process\n", 100+reloadCount)
		return []byte(top), nil
	case "exec":
		containerId := args[1]
		switch args[2] {
		case "grep":
			self.tlsReadArgs = append(self.tlsReadArgs, args)
			if err := self.containerIdTlsReadErrs[containerId]; err != nil {
				return nil, err
			}
			lines, ok := self.containerIdTlsLines[containerId]
			if !ok {
				lines = tlsCurrentLines("example.com", "star.example.com")
			}
			workerProcessesLine, ok := self.containerIdWorkerProcessesLines[containerId]
			if !ok {
				workerProcessesLine = "        worker_processes 1;"
			}
			if workerProcessesLine != "" {
				lines = append([]string{workerProcessesLine}, lines...)
			}
			return []byte(strings.Join(lines, "\n") + "\n"), nil
		case "readlink":
			if err := self.containerIdLinkErrs[containerId]; err != nil {
				return nil, err
			}
			if output, ok := self.containerIdLinkOutputs[containerId]; ok {
				return []byte(output), nil
			}
			targets := []string{}
			for _, linkPath := range args[3:] {
				target, err := os.Readlink(filepath.Join(self.vaultHome, strings.TrimPrefix(linkPath, lbVaultMountPath)))
				if err != nil {
					return nil, err
				}
				targets = append(targets, target)
			}
			return []byte(strings.Join(targets, "\n") + "\n"), nil
		case "nginx":
			self.reloadArgs = append(self.reloadArgs, args)
			if self.onReload != nil {
				if err := self.onReload(containerId); err != nil {
					return nil, err
				}
			}
			if err := self.containerIdReloadErrs[containerId]; err != nil {
				return nil, err
			}
			if !self.deafContainerIds[containerId] {
				self.containerIdReloadCounts[containerId] += 1
				self.containerIdPendingLooks[containerId] = self.slowReloadLooks
			}
			return nil, nil
		}
	}
	self.t.Errorf("unexpected docker command %v", args)
	return nil, errors.New("unexpected docker command")
}

// The default settings, with this fake as their docker.
func (self *fakeLbDocker) settings() *certsSwapSettings {
	settings := defaultCertsSwapSettings()
	settings.docker = self.answer
	return settings
}

// One healthy lb on the host, mounting vaultHome.
func newFakeLbDocker(t *testing.T, vaultHome string) *fakeLbDocker {
	t.Helper()
	return &fakeLbDocker{
		t:                       t,
		vaultHome:               vaultHome,
		censusLines:             []string{"aaaa11 edge-0.example.com-eth0"},
		containerIdReloadCounts: map[string]int{},
		containerIdPendingLooks: map[string]int{},
	}
}

// The lines `warpctl lb create-config` writes for names read through tls/current, as grep
// finds them.
func tlsCurrentLines(names ...string) []string {
	lines := []string{}
	for _, name := range names {
		lines = append(
			lines,
			fmt.Sprintf("                ssl_certificate     /srv/warp/vault/tls/current/%[1]s/%[1]s.pem;", name),
			fmt.Sprintf("                ssl_certificate_key /srv/warp/vault/tls/current/%[1]s/%[1]s.key;", name),
		)
	}
	return lines
}

// The lines it writes for a name read from one version, which is what every lb reads today.
func tlsVersionLines(version string, name string) []string {
	return []string{
		fmt.Sprintf("                ssl_certificate     /srv/warp/vault/tls/%[1]s/%[2]s/%[2]s.pem;", version, name),
		fmt.Sprintf("                ssl_certificate_key /srv/warp/vault/tls/%[1]s/%[2]s/%[2]s.key;", version, name),
	}
}

// Writes a self-signed pair under versionHome/<dirName>/, named <dirName>.pem and .key, covering
// dnsNames and valid from now for a day.
func writeTestTlsPair(t *testing.T, versionHome string, dirName string, dnsNames []string, key crypto.Signer) {
	t.Helper()
	certDir := filepath.Join(versionHome, dirName)
	if err := os.MkdirAll(certDir, 0700); err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     dnsNames,
	}
	certDer, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDer, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		dirName + ".pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDer}),
		dirName + ".key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDer}),
	}
	for fileName, content := range files {
		if err := os.WriteFile(filepath.Join(certDir, fileName), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Writes the pair a vault holds for a domain's top name, or with wildcard for star.<domain>,
// which covers the domain and *.<domain>, as `warpctl certs issue` files them.
func generateTestTlsFiles(t *testing.T, versionHome string, domain string, wildcard bool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if wildcard {
		writeTestTlsPair(t, versionHome, "star."+domain, []string{domain, "*." + domain}, key)
	} else {
		writeTestTlsPair(t, versionHome, domain, []string{domain}, key)
	}
}

// A flat vault home, as a host holds it, with versions 1.0.0 and 2.0.0 of the wildcard and top
// names.
func setupCertsSwapVault(t *testing.T) string {
	t.Helper()
	vaultHome := t.TempDir()
	for _, version := range []string{"1.0.0", "2.0.0"} {
		versionHome := filepath.Join(vaultHome, "tls", version)
		generateTestTlsFiles(t, versionHome, "example.com", true)
		generateTestTlsFiles(t, versionHome, "example.com", false)
	}
	return vaultHome
}

// Where a swap puts a name's link.
func tlsCurrentLinkPath(vaultHome string, name string) string {
	return filepath.Join(vaultHome, "tls", tlsCurrentDirName, name)
}

// Fails unless the name's link holds exactly this target.
func requireTlsLink(t *testing.T, vaultHome string, name string, wantTarget string) {
	t.Helper()
	target, err := os.Readlink(tlsCurrentLinkPath(vaultHome, name))
	if err != nil {
		t.Fatal(err)
	}
	if target != wantTarget {
		t.Fatalf("%s links to %q, want %q", name, target, wantTarget)
	}
}

// Fails unless each name's pair loads through its link, as an lb start would load it.
func requireTlsLinksLoad(t *testing.T, vaultHome string, names ...string) {
	t.Helper()
	for _, name := range names {
		linkPath := tlsCurrentLinkPath(vaultHome, name)
		if _, err := tls.LoadX509KeyPair(filepath.Join(linkPath, name+".pem"), filepath.Join(linkPath, name+".key")); err != nil {
			t.Errorf("%s does not load through its link: %v", name, err)
		}
	}
}

// Replaces a name's key in a version with one that does not match its certificate.
func mismatchTlsPair(t *testing.T, vaultHome string, version string, name string, otherVersion string) {
	t.Helper()
	otherKey, err := os.ReadFile(filepath.Join(vaultHome, "tls", otherVersion, name, name+".key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vaultHome, "tls", version, name, name+".key"), otherKey, 0600); err != nil {
		t.Fatal(err)
	}
}

// The state a refused swap must leave: nothing moved and nothing reloaded.
func requireNothingSwapped(t *testing.T, vaultHome string, fake *fakeLbDocker) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(vaultHome, "tls", tlsCurrentDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tls/current exists after a refused swap: %v", err)
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("a refused swap reloaded nginx: %v", fake.reloadArgs)
	}
}

// Each name's link ends at that version's pair, read through the link as nginx reads it.
func TestCertsSwapPointsCurrentAtVersion(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "2.0.0", name))
		for _, fileName := range []string{name + ".pem", name + ".key"} {
			want, err := os.ReadFile(filepath.Join(vaultHome, "tls", "2.0.0", name, fileName))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(tlsCurrentLinkPath(vaultHome, name), fileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("tls/current/%s/%s is not version 2.0.0's", name, fileName)
			}
		}
	}
	requireTlsLinksLoad(t, vaultHome, "example.com", "star.example.com")
}

// The vault mounts at another path in the container, so a link that named the host's path would
// dangle there, and nginx would fail to load it.
func TestCertsSwapLinksAreRelative(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	mountedHome := filepath.Join(t.TempDir(), "mounted")
	if err := os.Rename(vaultHome, mountedHome); err != nil {
		t.Fatal(err)
	}
	requireTlsLinksLoad(t, mountedHome, "example.com", "star.example.com")
}

// W5's order: the reload must find every link already moved.
func TestCertsSwapMovesLinksBeforeReload(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.reloadArgs = nil

	reloadCount := 0
	fake.onReload = func(containerId string) error {
		reloadCount += 1
		for _, name := range []string{"example.com", "star.example.com"} {
			requireTlsLink(t, vaultHome, name, filepath.Join("..", "2.0.0", name))
		}
		return nil
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if reloadCount != 1 {
		t.Fatalf("reloaded %d times, want 1", reloadCount)
	}
}

// A pair moves in one rename: a reader sees the old pair up to that call and the new pair after
// it, and never one of each.
func TestCertsSwapMovesEachLinkInOneStep(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	settings := fake.settings()
	movedNames := []string{}
	settings.rename = func(oldPath string, newPath string) error {
		name := filepath.Base(newPath)
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "1.0.0", name))
		if filepath.Dir(oldPath) != filepath.Dir(newPath) {
			t.Errorf("the temporary link %s is not beside %s, so a rename may not be atomic", oldPath, newPath)
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "2.0.0", name))
		movedNames = append(movedNames, name)
		return nil
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com", "star.example.com"}; !slices.Equal(movedNames, want) {
		t.Fatalf("moved %v, want %v", movedNames, want)
	}

	entries, err := os.ReadDir(filepath.Join(vaultHome, "tls", tlsCurrentDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("left %s behind", entry.Name())
		}
	}
}

// `-c` must name the block's own config, because nginx reads the pid file from it and every block
// on a host has its own master.
func TestCertsSwapReloadsEachBlockWithItsOwnConfig(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth1",
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	want := [][]string{
		{"exec", "aaaa11", "nginx", "-c", "/srv/warp/nginx.conf/edge-0.example.com-eth0.conf", "-s", "reload"},
		{"exec", "bbbb22", "nginx", "-c", "/srv/warp/nginx.conf/edge-0.example.com-eth1.conf", "-s", "reload"},
	}
	if !slices.EqualFunc(fake.reloadArgs, want, slices.Equal[[]string]) {
		t.Fatalf("reloads %v, want %v", fake.reloadArgs, want)
	}
}

// The census is how a swap finds its lbs, so its arguments are pinned: the labels `warpctl run`
// stamps, keeping another service's nginx and another env's lb out, the format that gives
// `<id> <block>`, and no -a, since a stopped lb takes no reload.
func TestCertsSwapFindsOnlyThisEnvsRunningLb(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	want := [][]string{{
		"ps",
		"--no-trunc",
		"--filter", "label=warp.env=main",
		"--filter", "label=warp.service=lb",
		"--format", `{{.ID}} {{.Label "warp.block"}}`,
	}}
	if !slices.EqualFunc(fake.censusArgs, want, slices.Equal[[]string]) {
		t.Fatalf("census %v, want %v", fake.censusArgs, want)
	}
}

// One key that does not match refuses the version, and names the pair.
func TestCertsSwapRefusesMismatchedPair(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	mismatchTlsPair(t, vaultHome, "2.0.0", "star.example.com", "1.0.0")

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "star.example.com") {
		t.Fatalf("swap error = %v, want one naming the bad pair", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// The check of every name comes before the first move, so the good names of a bad version stay
// where they were. The bad name sorts last to prove it.
func TestCertsSwapChecksEveryNameBeforeMovingAny(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.reloadArgs = nil
	mismatchTlsPair(t, vaultHome, "2.0.0", "star.example.com", "1.0.0")

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err == nil {
		t.Fatal("swap accepted a version with a bad pair")
	}

	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "1.0.0", name))
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("a refused swap reloaded nginx: %v", fake.reloadArgs)
	}
}

// nginx loads a certificate at any date, so only this check stops a version that every visitor
// would reject.
func TestCertsSwapRefusesCertificateOutsideItsValidity(t *testing.T) {
	cases := []struct {
		label  string
		offset time.Duration
	}{
		{label: "expired", offset: 48 * time.Hour},
		{label: "not yet valid", offset: -48 * time.Hour},
	}
	for _, c := range cases {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		settings := fake.settings()
		settings.now = func() time.Time { return time.Now().Add(c.offset) }

		if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard); err == nil {
			t.Errorf("swap accepted a certificate that is %s", c.label)
		}
		requireNothingSwapped(t, vaultHome, fake)
	}
}

// A mis-filed certificate parses and matches its key, and would still fail every visitor to the
// name once nginx reloaded it.
func TestCertsSwapRefusesCertificateForAnotherName(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	writeTestTlsPair(t, filepath.Join(vaultHome, "tls", "3.0.0"), "mine.example", []string{"other.example"}, key)

	err = swapTlsCurrent(vaultHome, "main", "3.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mine.example") {
		t.Fatalf("swap error = %v, want one naming the mis-filed name", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// Half a pair moved behind a link would stop nginx at its next reload, whichever half is missing.
func TestCertsSwapRefusesNameMissingAFile(t *testing.T) {
	for _, fileName := range []string{"example.com.pem", "example.com.key"} {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		if err := os.Remove(filepath.Join(vaultHome, "tls", "2.0.0", "example.com", fileName)); err != nil {
			t.Fatal(err)
		}

		if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err == nil {
			t.Errorf("swap accepted a name with no %s", fileName)
		}
		requireNothingSwapped(t, vaultHome, fake)
	}
}

// Every other check passes a pair whose RSA key nginx's tls library refuses, which with
// --no-reload nothing else would catch before the next lb start. The default's floor is the one
// that library sets, 2048 bits, and a non-positive floor takes any size, as its doc says.
func TestCertsSwapRefusesAnRsaKeyNginxRefuses(t *testing.T) {
	writeRsaVersion := func(t *testing.T, vaultHome string, version string, bitCount int) {
		key, err := rsa.GenerateKey(rand.Reader, bitCount)
		if err != nil {
			t.Fatal(err)
		}
		writeTestTlsPair(t, filepath.Join(vaultHome, "tls", version), "example.com", []string{"example.com"}, key)
	}
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	writeRsaVersion(t, vaultHome, "3.0.0", 1024)
	settings := fake.settings()
	settings.noReload = true

	err := swapTlsCurrent(vaultHome, "main", "3.0.0", settings, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("swap error = %v, want one naming the key's size", err)
	}
	requireNothingSwapped(t, vaultHome, fake)

	writeRsaVersion(t, vaultHome, "4.0.0", 2048)
	if err := swapTlsCurrent(vaultHome, "main", "4.0.0", settings, io.Discard); err != nil {
		t.Fatalf("swap refused a key of the floor's size: %v", err)
	}
	settings.minimumRsaKeyBits = 0
	if err := swapTlsCurrent(vaultHome, "main", "3.0.0", settings, io.Discard); err != nil {
		t.Fatalf("swap with no floor refused a small key: %v", err)
	}
}

// A version names one directory under tls/, as certs issue names one, so nothing here may walk
// out of it or name tls/current itself, and nothing but three numbers is taken for one.
func TestCertsSwapRefusesBadVersions(t *testing.T) {
	versions := []string{
		"", "current", "latest", "..", "../..", "2.0.0/../1.0.0", "/etc", "9.9.9",
		"2.0.0-rc.1", "2.0.0+build", "v2.0.0", "02.0.0", " 2.0.0", "2.0",
	}
	for _, version := range versions {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)

		if err := swapTlsCurrent(vaultHome, "main", version, fake.settings(), io.Discard); err == nil {
			t.Errorf("swap accepted version %q", version)
		}
		requireNothingSwapped(t, vaultHome, fake)
	}
}

// A swap that moves nothing must not read as a rotation.
func TestCertsSwapRefusesVersionHoldingNoNames(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := os.MkdirAll(filepath.Join(vaultHome, "tls", "3.0.0"), 0700); err != nil {
		t.Fatal(err)
	}

	if err := swapTlsCurrent(vaultHome, "main", "3.0.0", fake.settings(), io.Discard); err == nil {
		t.Fatal("swap accepted an empty version")
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// A name that is a link in a version would get no link of its own if passed over like a stray
// file, so that name would quietly not rotate. The swap refuses the version instead.
func TestCertsSwapRefusesANameThatIsALink(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := os.Symlink(filepath.Join("..", "1.0.0", "example.com"), filepath.Join(vaultHome, "tls", "2.0.0", "www.example.com")); err != nil {
		t.Fatal(err)
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "www.example.com") {
		t.Fatalf("swap error = %v, want one naming the link", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// A real directory cannot be replaced in one step, and something other than this command put it
// there, which the refusal says in those words.
func TestCertsSwapRefusesEntryThatIsNotALink(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	generateTestTlsFiles(t, filepath.Join(vaultHome, "tls", tlsCurrentDirName), "example.com", true)

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "star.example.com is not a link") {
		t.Fatalf("swap error = %v, want one saying the real directory is not a link", err)
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("a refused swap reloaded nginx: %v", fake.reloadArgs)
	}
	if _, err := os.Lstat(tlsCurrentLinkPath(vaultHome, "example.com")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused swap moved a link: %v", err)
	}
}

// With tls/current a link to a version, a swap would write its links into that version's
// directory, where server's resolver and the jwt keyring look. The version shares no name with the
// swap, so no check of a name can refuse it.
func TestCertsSwapRefusesCurrentThatIsALink(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	generateTestTlsFiles(t, filepath.Join(vaultHome, "tls", "3.0.0"), "other.example", false)
	if err := os.Symlink("3.0.0", filepath.Join(vaultHome, "tls", tlsCurrentDirName)); err != nil {
		t.Fatal(err)
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err == nil {
		t.Fatal("swap accepted a tls/current that is a link")
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("a refused swap reloaded nginx: %v", fake.reloadArgs)
	}
	entries, err := os.ReadDir(filepath.Join(vaultHome, "tls", "3.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the swap wrote into the version tls/current links to: %v", entries)
	}
}

// Moving links on a host with nothing to reload would read as a rotation that happened, so a swap
// on the wrong host, or under a mistyped env, changes nothing unless it asks for that. The refusal
// says how, since on a new host it is the right thing to ask.
func TestCertsSwapRefusesHostWithoutLbContainers(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = nil

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--no-reload") {
		t.Fatalf("swap error = %v, want one naming --no-reload", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// With no answer from docker a swap cannot know what it would reach.
func TestCertsSwapRefusesWhenTheCensusFails(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusErr = errors.New("daemon unavailable")

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, fake.censusErr) {
		t.Fatalf("swap error = %v, want the census failure", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// Without a block a reload has no config to name.
func TestCertsSwapRefusesCensusLineItCannotRead(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{"aaaa11"}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err == nil {
		t.Fatal("swap accepted a container with no block")
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// What every lb deployed today reads, by the owner's decision of 2026-11-06 (QD-022): each name
// from its newest version, as `warpctl lb create-config` writes it from a vault with no
// tls/current. A swap reaches none of it, so it refuses there and changes nothing.
func TestCertsSwapRefusesWhatEveryLbReadsToday(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTlsLines = map[string][]string{
		"aaaa11": append(tlsVersionLines("1.0.0", "star.example.com"), tlsVersionLines("1.0.0", "example.com")...),
	}
	output := &bytes.Buffer{}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), output)
	if err == nil || !strings.Contains(err.Error(), "edge-0.example.com-eth0") {
		t.Fatalf("swap error = %v, want one naming the lb", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
	if strings.Contains(output.String(), "Reloaded") {
		t.Errorf("a refused swap printed Reloaded: %q", output.String())
	}
}

// An image reading a name from anywhere but tls/current reloads happily and serves what it served.
// So would an image built from a vault holding tls/current for some names and not others. Every
// form of path the build can bake counts, read by itself or by a second server block, and nothing
// may move.
func TestCertsSwapRefusesLbReadingANameOutsideCurrent(t *testing.T) {
	cases := []struct {
		label    string
		tlsLines []string
	}{
		{
			label:    "from a version",
			tlsLines: tlsVersionLines("1.0.0", "star.example.com"),
		},
		{
			label: "from the top of the vault",
			tlsLines: []string{
				"ssl_certificate     /srv/warp/vault/star.example.com.pem;",
				"ssl_certificate_key /srv/warp/vault/star.example.com.key;",
			},
		},
		{
			label:    "from a version, beside another name in tls/current",
			tlsLines: append(tlsCurrentLines("example.com"), tlsVersionLines("1.0.0", "star.example.com")...),
		},
		{
			label:    "from tls/current and also a version",
			tlsLines: append(tlsCurrentLines("star.example.com"), tlsVersionLines("1.0.0", "star.example.com")...),
		},
	}
	for _, c := range cases {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		fake.containerIdTlsLines = map[string][]string{"aaaa11": c.tlsLines}

		err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "star.example.com") || !strings.Contains(err.Error(), "edge-0.example.com-eth0") {
			t.Errorf("a name read %s: swap error = %v, want one naming the lb and the name", c.label, err)
		}
		requireNothingSwapped(t, vaultHome, fake)
	}
}

// Reloading an lb that reads nothing this version holds changes nothing a client sees, which must
// not read as a rotation.
func TestCertsSwapRefusesLbReadingNoNameOfTheVersion(t *testing.T) {
	cases := [][]string{
		tlsCurrentLines("other.example"),
		tlsVersionLines("1.0.0", "other.example"),
	}
	for _, tlsLines := range cases {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		fake.containerIdTlsLines = map[string][]string{"aaaa11": tlsLines}

		err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "edge-0.example.com-eth0") {
			t.Errorf("swap error = %v, want one naming the lb", err)
		}
		requireNothingSwapped(t, vaultHome, fake)
	}
}

// One lb a swap cannot reach refuses the swap for the host, before any other lb reloads, as one
// bad pair refuses the version.
func TestCertsSwapChecksEveryLbBeforeMovingAnyLink(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth1",
	}
	fake.containerIdTlsLines = map[string][]string{"bbbb22": tlsVersionLines("1.0.0", "star.example.com")}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "edge-0.example.com-eth1") {
		t.Fatalf("swap error = %v, want one naming the second lb", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// Not knowing what an lb reads is not knowing the swap reaches it.
func TestCertsSwapRefusesLbWhosePathsItCannotRead(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	readErr := errors.New("no such file")
	fake.containerIdTlsReadErrs = map[string]error{"aaaa11": readErr}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, readErr) {
		t.Fatalf("swap error = %v, want the read failure", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// Every lb is read at the config its reload names, and with a read that prints only tls lines and
// the worker count, so that no more of a config than that reaches a log.
func TestCertsSwapReadsEachLbsOwnConfig(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth1",
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	want := [][]string{
		{"exec", "aaaa11", "grep", "-e", "ssl_certificate", "-e", "worker_processes", "/srv/warp/nginx.conf/edge-0.example.com-eth0.conf"},
		{"exec", "bbbb22", "grep", "-e", "ssl_certificate", "-e", "worker_processes", "/srv/warp/nginx.conf/edge-0.example.com-eth1.conf"},
	}
	if !slices.EqualFunc(fake.tlsReadArgs, want, slices.Equal[[]string]) {
		t.Fatalf("read %v, want %v", fake.tlsReadArgs, want)
	}
}

// The build writes its pair lines at two depths, one per kind of server block, and a line may be
// indented by tabs or end in a space before its semicolon. Each case holds one shape alone, so a
// shape the swap could not read would leave it reading no name, which it refuses.
func TestCertsSwapReadsEveryShapeOfPairLine(t *testing.T) {
	cases := [][]string{
		{
			"                        ssl_certificate     /srv/warp/vault/tls/current/star.example.com/star.example.com.pem;",
			"                        ssl_certificate_key /srv/warp/vault/tls/current/star.example.com/star.example.com.key;",
		},
		{
			"\t\tssl_certificate\t/srv/warp/vault/tls/current/star.example.com/star.example.com.pem;",
			"\t\tssl_certificate_key\t/srv/warp/vault/tls/current/star.example.com/star.example.com.key ;",
		},
	}
	for _, tlsLines := range cases {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		fake.containerIdTlsLines = map[string][]string{"aaaa11": tlsLines}

		if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
			t.Errorf("lines %q: %v", tlsLines, err)
		}
		if len(fake.reloadArgs) != 1 {
			t.Errorf("lines %q: reloaded %d times, want 1", tlsLines, len(fake.reloadArgs))
		}
	}
}

// A version can hold a name this lb has no server for, which it does not read and a link for which
// harms nothing.
func TestCertsSwapMovesNamesTheLbDoesNotRead(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTlsLines = map[string][]string{"aaaa11": tlsCurrentLines("star.example.com")}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "2.0.0", name))
	}
	if len(fake.reloadArgs) != 1 {
		t.Fatalf("reloaded %d times, want 1", len(fake.reloadArgs))
	}
}

// A name this version does not hold has nothing new to serve, so a versioned path for it does not
// stop the swap. No swap will ever move that path, though, so the operator is told.
func TestCertsSwapSaysWhichNamesItCanNeverMove(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTlsLines = map[string][]string{
		"aaaa11": append(tlsCurrentLines("star.example.com"), tlsVersionLines("1.0.0", "old.example")...),
	}
	output := &bytes.Buffer{}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), output); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), "edge-0.example.com-eth0 keeps reading old.example from /srv/warp/vault/tls/1.0.0/old.example/") {
		t.Fatalf("output %q does not name the path no swap moves", output.String())
	}
}

// One issuance need not hold every name, so a link it does not move stays where it is. That name
// has not rotated, which the operator is told, and a name that did move is not named.
func TestCertsSwapSaysWhichLinksItLeavesAlone(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTlsLines = map[string][]string{
		"aaaa11": tlsCurrentLines("star.example.com", "third.example"),
	}
	output := &bytes.Buffer{}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), output); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), "edge-0.example.com-eth0 keeps its third.example link: 2.0.0 does not hold that name") {
		t.Fatalf("output %q does not name the link left alone", output.String())
	}
	if strings.Contains(output.String(), "keeps its star.example.com link") {
		t.Fatalf("output %q names a link that moved", output.String())
	}
}

// A line nginx never acts on says nothing about what the lb reads.
func TestCertsSwapPassesOverACommentedOutPath(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTlsLines = map[string][]string{
		"aaaa11": append(
			tlsCurrentLines("star.example.com"),
			"# ssl_certificate /srv/warp/vault/tls/1.0.0/star.example.com/star.example.com.pem;",
		),
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
}

// After a deploy the old lb drains for up to an hour beside the new one. It takes no reload and no
// new connections, so the swap is for the new one alone, and the old one's image is no reason to
// refuse.
func TestCertsSwapPassesOverADrainingLb(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth0",
	}
	fake.containerIdTopOutputs = map[string]string{"aaaa11": drainingLbTop}
	fake.containerIdTlsLines = map[string][]string{"aaaa11": tlsVersionLines("1.0.0", "star.example.com")}
	output := &bytes.Buffer{}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), output); err != nil {
		t.Fatal(err)
	}

	want := [][]string{
		{"exec", "bbbb22", "nginx", "-c", "/srv/warp/nginx.conf/edge-0.example.com-eth0.conf", "-s", "reload"},
	}
	if !slices.EqualFunc(fake.reloadArgs, want, slices.Equal[[]string]) {
		t.Fatalf("reloads %v, want %v", fake.reloadArgs, want)
	}
	if count := strings.Count(output.String(), "Reloaded edge-0.example.com-eth0"); count != 1 {
		t.Errorf("printed Reloaded %d times, want 1: %q", count, output.String())
	}
	if !strings.Contains(output.String(), "Passing over edge-0.example.com-eth0") {
		t.Errorf("output %q does not name the draining lb", output.String())
	}
}

// nginx answers 0 to a reload while it drains, then ignores it. A host whose only lb is draining
// has nothing to reload, the same as a host with none.
func TestCertsSwapRefusesHostWhoseOnlyLbIsDraining(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTopOutputs = map[string]string{"aaaa11": drainingLbTop}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--no-reload") {
		t.Fatalf("swap error = %v, want one naming --no-reload", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// An lb whose nginx has no workers at all may be starting, and may read the links before or after
// they move, so a swap cannot pass over it as it does a drain.
func TestCertsSwapRefusesLbWithNoWorkers(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTopOutputs = map[string]string{"aaaa11": "PID COMMAND\n1 /usr/local/sbin/lb\n"}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "edge-0.example.com-eth0") {
		t.Fatalf("swap error = %v, want one naming the lb", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// An lb in a restart loop answers `docker top` with an error, and a swap cannot say what it would
// reach. --no-reload is the way out of that state.
func TestCertsSwapRefusesLbWhoseWorkersItCannotRead(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	topErr := errors.New("container is restarting")
	fake.containerIdTopErrs = map[string]error{"aaaa11": topErr}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, topErr) {
		t.Fatalf("swap error = %v, want the top failure", err)
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// Old workers finish their connections for up to an hour after a reload, so one shutting down says
// nothing about an lb that has another serving.
func TestCertsSwapTakesAnLbWithOldWorkersForAServingOne(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTopOutputs = map[string]string{"aaaa11": drainingLbTop + "9 nginx: worker process\n"}
	fake.onReload = func(containerId string) error {
		fake.containerIdTopOutputs[containerId] = drainingLbTop + "9 nginx: worker process is shutting down\n10 nginx: worker process\n"
		return nil
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(fake.reloadArgs) != 1 {
		t.Fatalf("reloaded %d times, want 1", len(fake.reloadArgs))
	}
}

// nginx exits 0 once the signal is sent, and a master that began draining after the census ignores
// it, so that exit proves nothing. `Reloaded` needs new workers, and a swap without them has not
// happened.
func TestCertsSwapFailsAReloadThatStartsNoWorker(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.deafContainerIds = map[string]bool{"aaaa11": true}
	output := &bytes.Buffer{}
	settings := fake.settings()
	settings.reloadCheckCount = 3
	settings.reloadCheckInterval = 0

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, output)
	if err == nil || !strings.Contains(err.Error(), "edge-0.example.com-eth0") {
		t.Fatalf("swap error = %v, want one naming the lb", err)
	}
	if strings.Contains(output.String(), "Reloaded edge-0") {
		t.Errorf("printed Reloaded with no new worker: %q", output.String())
	}
	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "1.0.0", name))
	}
}

// An lb mounting some other vault, such as a host's staged copy, reloads and shows new workers,
// and serves what it served, because the links moved somewhere it does not read. Its reload would
// prove nothing, so it never happens.
func TestCertsSwapFailsWhenAnLbDoesNotSeeTheLinks(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.reloadArgs = nil
	fake.containerIdLinkOutputs = map[string]string{"aaaa11": "../1.0.0/example.com\n../1.0.0/star.example.com\n"}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "edge-0.example.com-eth0") || !strings.Contains(err.Error(), vaultHome) {
		t.Fatalf("swap error = %v, want one naming the lb and this vault", err)
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("reloaded an lb that cannot see the links: %v", fake.reloadArgs)
	}
	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "1.0.0", name))
	}
}

// Not knowing what an lb sees is not knowing it sees the swap.
func TestCertsSwapFailsWhenItCannotReadAnLbsLinks(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	linkErr := errors.New("no such file")
	fake.containerIdLinkErrs = map[string]error{"aaaa11": linkErr}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, linkErr) {
		t.Fatalf("swap error = %v, want the read failure", err)
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("reloaded an lb whose links it could not read: %v", fake.reloadArgs)
	}
}

// Only the names an lb reads have to show there: one this lb has no server for may have no link in
// a vault it mounts, and every lb is asked only for its own.
func TestCertsSwapLooksOnlyAtTheLinksAnLbReads(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdTlsLines = map[string][]string{"aaaa11": tlsCurrentLines("star.example.com")}
	fake.containerIdLinkOutputs = map[string]string{"aaaa11": "../2.0.0/star.example.com\n"}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
}

// An lb can finish its drain and exit while a swap looks for its new worker. That is no reload
// either, and the failure says which step found it out.
func TestCertsSwapFailsAReloadItCannotConfirm(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	topErr := errors.New("container is not running")
	fake.onReload = func(containerId string) error {
		fake.containerIdTopErrs = map[string]error{containerId: topErr}
		return nil
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, topErr) || !strings.Contains(err.Error(), "after the signal") {
		t.Fatalf("swap error = %v, want the top failure, placed after the signal", err)
	}
	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "1.0.0", name))
	}
}

// Without the workers from before the signal no new one can be told from them, so that lb is not
// signalled, and the lb already reloaded goes back.
func TestCertsSwapSignalsNoLbWhoseWorkersItCannotReadFirst(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth1",
	}
	topErr := errors.New("container is not running")
	fake.onReload = func(containerId string) error {
		fake.containerIdTopErrs = map[string]error{"bbbb22": topErr}
		return nil
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, topErr) || !strings.Contains(err.Error(), "before the signal") {
		t.Fatalf("swap error = %v, want the top failure, placed before the signal", err)
	}
	reloadedIds := []string{}
	for _, reloadArgs := range fake.reloadArgs {
		reloadedIds = append(reloadedIds, reloadArgs[1])
	}
	if want := []string{"aaaa11", "aaaa11"}; !slices.Equal(reloadedIds, want) {
		t.Fatalf("reloaded %v, want %v", reloadedIds, want)
	}
}

// The master parses its config after the nginx that signalled it has exited, so a new worker can
// be some looks away, and the last look counts.
func TestCertsSwapLooksForTheNewWorkerUpToItsCount(t *testing.T) {
	cases := []struct {
		slowReloadLooks int
		wantErr         bool
	}{
		{slowReloadLooks: 0, wantErr: false},
		{slowReloadLooks: 2, wantErr: false},
		{slowReloadLooks: 3, wantErr: true},
	}
	for _, c := range cases {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		fake.slowReloadLooks = c.slowReloadLooks
		settings := fake.settings()
		settings.reloadCheckCount = 3
		settings.reloadCheckInterval = 0

		err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard)
		if (err != nil) != c.wantErr {
			t.Errorf("worker %d looks away: swap error = %v, want an error: %v", c.slowReloadLooks, err, c.wantErr)
		}
	}
}

// What `docker top` prints for an lb serving through these worker pids, and finishing its
// connections through the shutting ones.
func lbTopOutput(workerPids []string, shuttingWorkerPids []string) string {
	top := "PID                 COMMAND\n1                   /usr/local/sbin/lb\n"
	top += "7                   nginx: master process nginx -g daemon off; -c /srv/warp/nginx.conf/edge-0.example.com-eth0.conf\n"
	for _, workerPid := range workerPids {
		top += fmt.Sprintf("%-20snginx: worker process\n", workerPid)
	}
	for _, workerPid := range shuttingWorkerPids {
		top += fmt.Sprintf("%-20snginx: worker process is shutting down\n", workerPid)
	}
	return top
}

// A master forks a reload's workers one by one and retires the old ones only after the last. A
// worker the lb did not have before the signal is therefore no proof: here the first look after
// the signal shows one beside both old ones, which a real run showed can be a late worker of an
// earlier reload, holding the pair as it stood then. The reload counts only once none of the
// workers from before serves, so with looks for only that partial picture the swap fails, and
// with one more it reloads.
func TestCertsSwapCountsAReloadOnlyWhenEveryWorkerIsReplaced(t *testing.T) {
	afterSignal := []string{
		lbTopOutput([]string{"200", "201", "300"}, nil),
		lbTopOutput([]string{"200", "201", "300", "301"}, nil),
		lbTopOutput([]string{"300", "301"}, []string{"200", "201"}),
	}
	cases := []struct {
		reloadCheckCount int
		wantErr          bool
	}{
		{reloadCheckCount: 2, wantErr: true},
		{reloadCheckCount: 3, wantErr: false},
	}
	for _, c := range cases {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		fake.containerIdWorkerProcessesLines = map[string]string{"aaaa11": "worker_processes 2;"}
		// the census, then the look before the signal
		fake.containerIdTopSequences = map[string][]string{"aaaa11": {
			lbTopOutput([]string{"200", "201"}, nil),
			lbTopOutput([]string{"200", "201"}, nil),
		}}
		fake.onReload = func(containerId string) error {
			fake.containerIdTopSequences[containerId] = slices.Clone(afterSignal)
			return nil
		}
		output := &bytes.Buffer{}
		settings := fake.settings()
		settings.reloadCheckCount = c.reloadCheckCount
		settings.reloadCheckInterval = 0

		err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, output)
		if (err != nil) != c.wantErr {
			t.Errorf("%d looks: swap error = %v, want an error: %v", c.reloadCheckCount, err, c.wantErr)
		}
		if c.wantErr && strings.Contains(output.String(), "Reloaded edge-0.example.com-eth0\n") {
			t.Errorf("%d looks: printed Reloaded before the workers were replaced: %q", c.reloadCheckCount, output.String())
		}
	}
}

// A signal sent while an earlier reload is still forking would be taken with it, and its late
// workers would pass for this reload's. So the signal waits until the lb runs exactly its own count
// of workers, and an lb that never does is not signalled at all, and the links go back.
func TestCertsSwapSignalsOnlyALbRunningItsOwnCountOfWorkers(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.reloadArgs = nil
	midReload := lbTopOutput([]string{"200", "300"}, nil)
	// the census, then two looks before the signal mid-reload, then one with the reload done
	fake.containerIdTopSequences = map[string][]string{"aaaa11": {midReload, midReload, midReload, lbTopOutput([]string{"300"}, []string{"200"})}}
	topCountAtSignal := 0
	fake.onReload = func(containerId string) error {
		topCountAtSignal = 4 - len(fake.containerIdTopSequences[containerId])
		return nil
	}
	settings := fake.settings()
	settings.reloadCheckInterval = 0

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard); err != nil {
		t.Fatal(err)
	}
	if topCountAtSignal != 4 {
		t.Fatalf("signalled after %d looks at the workers, want 4: the census and three before the signal", topCountAtSignal)
	}

	fake.containerIdTopSequences = map[string][]string{"aaaa11": {midReload, midReload, midReload, midReload}}
	fake.reloadArgs = nil
	settings.reloadCheckCount = 3
	err := swapTlsCurrent(vaultHome, "main", "1.0.0", settings, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "never ran exactly its 1 workers") {
		t.Fatalf("swap error = %v, want one saying the lb never settled", err)
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("signalled an lb that never settled: %v", fake.reloadArgs)
	}
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "2.0.0", "star.example.com"))
}

// The worker count is what tells the lb's own reload from an earlier one's, so an lb config that
// sets its count in a way this cannot read, as auto, zero, twice, beside another directive or past
// what an int holds, is refused with nothing moved.
func TestCertsSwapRefusesAnLbWhoseWorkerCountItCannotRead(t *testing.T) {
	workerProcessesLines := []string{
		"worker_processes auto;",
		"worker_processes 0;",
		"worker_processes 1;\nworker_processes 2;",
		"events { worker_connections 64; } worker_processes 2;",
		"worker_processes 99999999999999999999;",
	}
	for _, workerProcessesLine := range workerProcessesLines {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		fake.containerIdWorkerProcessesLines = map[string]string{"aaaa11": workerProcessesLine}
		// a count read wrongly would fail at the wait before the signal, and without waiting
		settings := fake.settings()
		settings.reloadCheckInterval = 0

		err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "edge-0.example.com-eth0 sets its count of nginx workers") {
			t.Errorf("worker line %q: swap error = %v, want one naming the lb and its count", workerProcessesLine, err)
		}
		requireNothingSwapped(t, vaultHome, fake)
	}
}

// warpctl writes no worker count for a block without capacity sizing, and nginx then runs its
// default of one worker, so such an lb swaps as one that says 1 does: the signal waits for exactly
// one serving worker. A count in a comment sets nothing either. Here the lb is mid-reload at the
// census and the first look, so the signal comes at the third look at its workers.
func TestCertsSwapTakesNginxsOneWorkerForAnLbThatSetsNoCount(t *testing.T) {
	for _, workerProcessesLine := range []string{"", "# worker_processes 4;"} {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		fake.containerIdWorkerProcessesLines = map[string]string{"aaaa11": workerProcessesLine}
		midReload := lbTopOutput([]string{"200", "300"}, nil)
		fake.containerIdTopSequences = map[string][]string{"aaaa11": {
			midReload, midReload, lbTopOutput([]string{"300"}, []string{"200"}),
		}}
		topCountAtSignal := 0
		fake.onReload = func(containerId string) error {
			topCountAtSignal = 3 - len(fake.containerIdTopSequences[containerId])
			fake.containerIdTopSequences[containerId] = []string{lbTopOutput([]string{"400"}, []string{"300"})}
			return nil
		}
		output := &bytes.Buffer{}
		settings := fake.settings()
		settings.reloadCheckInterval = 0

		if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, output); err != nil {
			t.Errorf("worker line %q: %v", workerProcessesLine, err)
			continue
		}
		if topCountAtSignal != 3 {
			t.Errorf("worker line %q: signalled after %d looks at the workers, want 3: the census and two before the signal", workerProcessesLine, topCountAtSignal)
		}
		if !strings.Contains(output.String(), "Reloaded edge-0.example.com-eth0\n") {
			t.Errorf("worker line %q: output %q does not say it reloaded", workerProcessesLine, output.String())
		}
		requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "2.0.0", "star.example.com"))
	}
}

// A master that starts draining as the signal reaches it retires every worker, and serves with
// none. Every old worker has stopped serving, as at the end of a reload, but nothing reloaded.
func TestCertsSwapFailsAReloadWhenTheLbDrainsInstead(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.onReload = func(containerId string) error {
		fake.containerIdTopOutputs = map[string]string{containerId: drainingLbTop}
		return nil
	}
	output := &bytes.Buffer{}
	settings := fake.settings()
	settings.reloadCheckCount = 3
	settings.reloadCheckInterval = 0

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, output)
	if err == nil || !strings.Contains(err.Error(), "did not replace its workers") {
		t.Fatalf("swap error = %v, want one saying the lb replaced no workers", err)
	}
	if strings.Contains(output.String(), "Reloaded edge-0.example.com-eth0\n") {
		t.Errorf("printed Reloaded for a drain: %q", output.String())
	}
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "1.0.0", "star.example.com"))
}

// With no looks allowed no reload can be proven, which its doc says.
func TestCertsSwapFailsEveryReloadWithNoLooks(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	settings := fake.settings()
	settings.reloadCheckCount = 0

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard); err == nil {
		t.Fatal("swap succeeded with no look at the workers")
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("signalled with no look at the workers: %v", fake.reloadArgs)
	}
}

// A version nginx refuses must not stay in the links, because the next lb start reads them and
// fails. The failure reaches the caller, nothing moved stays moved, and nothing is left to reload
// back.
func TestCertsSwapPutsLinksBackWhenAReloadFails(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.reloadArgs = nil
	reloadErr := errors.New("nginx refused the reload")
	fake.containerIdReloadErrs = map[string]error{"aaaa11": reloadErr}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, reloadErr) {
		t.Fatalf("swap error = %v, want the reload failure", err)
	}
	if !strings.Contains(err.Error(), "edge-0.example.com-eth0") {
		t.Errorf("swap error = %v, want one naming the block", err)
	}
	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "1.0.0", name))
	}
	if len(fake.reloadArgs) != 1 {
		t.Errorf("attempted %d reloads, want 1", len(fake.reloadArgs))
	}
}

// A first swap has no links to go back to, so its links go.
func TestCertsSwapRemovesLinksItAddedWhenAReloadFails(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdReloadErrs = map[string]error{"aaaa11": errors.New("nginx refused the reload")}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err == nil {
		t.Fatal("swap hid a failed reload")
	}

	entries, err := os.ReadDir(filepath.Join(vaultHome, "tls", tlsCurrentDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed swap left %v", entries)
	}
}

// Links that move back must take any lb already reloaded with them, or it alone serves the pair
// the swap gave up on. The block after the failure never reloads.
func TestCertsSwapReloadsBackTheLbsAlreadyReloaded(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth1",
		"cccc33 edge-0.example.com-eth2",
	}
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.reloadArgs = nil
	reloadErr := errors.New("nginx refused the reload")
	fake.containerIdReloadErrs = map[string]error{"bbbb22": reloadErr}
	containerIdLinkTargets := map[string][]string{}
	fake.onReload = func(containerId string) error {
		target, err := os.Readlink(tlsCurrentLinkPath(vaultHome, "star.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		containerIdLinkTargets[containerId] = append(containerIdLinkTargets[containerId], target)
		return nil
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, reloadErr) {
		t.Fatalf("swap error = %v, want the reload failure", err)
	}

	newTarget := filepath.Join("..", "2.0.0", "star.example.com")
	oldTarget := filepath.Join("..", "1.0.0", "star.example.com")
	want := map[string][]string{
		"aaaa11": {newTarget, oldTarget},
		"bbbb22": {newTarget},
	}
	if !maps.EqualFunc(containerIdLinkTargets, want, slices.Equal[[]string]) {
		t.Fatalf("reloads read %v, want %v", containerIdLinkTargets, want)
	}
}

// A reload back needs a worker of its own: the first reload's new worker is no proof of the second,
// which the lb may ignore as it can any other.
func TestCertsSwapFailsAReloadBackThatStartsNoWorker(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth1",
	}
	reloadErr := errors.New("nginx refused the reload")
	fake.containerIdReloadErrs = map[string]error{"bbbb22": reloadErr}
	fake.onReload = func(containerId string) error {
		if containerId == "bbbb22" {
			fake.deafContainerIds = map[string]bool{"aaaa11": true}
		}
		return nil
	}
	output := &bytes.Buffer{}
	settings := fake.settings()
	settings.reloadCheckCount = 3
	settings.reloadCheckInterval = 0

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, output)
	if !errors.Is(err, reloadErr) || !strings.Contains(err.Error(), "reload edge-0.example.com-eth0 back") {
		t.Fatalf("swap error = %v, want the refused reload and the failed reload back", err)
	}
	if strings.Contains(output.String(), "Reloaded edge-0.example.com-eth0 back") {
		t.Errorf("claimed a reload back with no new worker: %q", output.String())
	}
}

// An lb that cannot be reloaded back serves the new pair until something reloads it, which the
// operator must hear of.
func TestCertsSwapReportsAnLbItCouldNotReloadBack(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = []string{
		"aaaa11 edge-0.example.com-eth0",
		"bbbb22 edge-0.example.com-eth1",
	}
	reloadErr := errors.New("nginx refused the reload")
	fake.containerIdReloadErrs = map[string]error{"bbbb22": reloadErr}
	reloadBackErr := errors.New("daemon unavailable")
	fake.onReload = func(containerId string) error {
		if containerId == "aaaa11" && 0 < fake.containerIdReloadCounts["aaaa11"] {
			return reloadBackErr
		}
		return nil
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if !errors.Is(err, reloadErr) || !errors.Is(err, reloadBackErr) {
		t.Fatalf("swap error = %v, want both failures", err)
	}
	if !strings.Contains(err.Error(), "edge-0.example.com-eth0") {
		t.Errorf("swap error = %v, want one naming the lb left on the new pair", err)
	}
}

// A retry after a failed reload is how an operator recovers, and it starts over.
func TestCertsSwapRetrySwapsAfterAFailure(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.containerIdReloadErrs = map[string]error{"aaaa11": errors.New("nginx refused the reload")}
	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err == nil {
		t.Fatal("swap hid a failed reload")
	}

	fake.containerIdReloadErrs = nil
	fake.reloadArgs = nil
	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(fake.reloadArgs) != 1 {
		t.Fatalf("retry reloaded %d times, want 1", len(fake.reloadArgs))
	}
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "2.0.0", "star.example.com"))
}

// A first lb start needs the links, and the lb is what reads them, so a new host has none running
// to reload. Nothing here touches docker, which a new host may not have started.
func TestCertsSwapNoReloadSeedsANewHost(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	fake.censusLines = nil
	settings := fake.settings()
	settings.noReload = true

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "2.0.0", name))
	}
	if fake.commandCount != 0 {
		t.Errorf("ran %d docker commands, want none", fake.commandCount)
	}
	requireTlsLinksLoad(t, vaultHome, "example.com", "star.example.com")
}

// Recovery has to run when the lb is down and docker with it: the links are all that stand between
// the host and its next lb start.
func TestCertsSwapNoReloadRollsBackWithDockerDown(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.censusErr = errors.New("daemon unavailable")
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err == nil {
		t.Fatal("a plain swap succeeded with docker down")
	}
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "2.0.0", "star.example.com"))

	settings := fake.settings()
	settings.noReload = true
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", settings, io.Discard); err != nil {
		t.Fatal(err)
	}

	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "1.0.0", "star.example.com"))
}

// Skipping the lb skips nothing else: the pairs are all that judge the version.
func TestCertsSwapNoReloadStillChecksEveryPair(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	mismatchTlsPair(t, vaultHome, "2.0.0", "star.example.com", "1.0.0")
	settings := fake.settings()
	settings.noReload = true

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard); err == nil {
		t.Fatal("swap accepted a bad pair")
	}
	requireNothingSwapped(t, vaultHome, fake)
}

// Links moved with nothing reloaded must not read as a rotation.
func TestCertsSwapNoReloadSaysNothingWasReloaded(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	output := &bytes.Buffer{}
	settings := fake.settings()
	settings.noReload = true

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, output); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), "Reloaded nothing") {
		t.Fatalf("output %q does not say nothing was reloaded", output.String())
	}
}

// Rollback is a swap to the older version.
func TestCertsSwapMovesBackToAnOlderVersion(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	for _, version := range []string{"2.0.0", "1.0.0"} {
		if err := swapTlsCurrent(vaultHome, "main", version, fake.settings(), io.Discard); err != nil {
			t.Fatal(err)
		}
	}

	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "1.0.0", "star.example.com"))
}

// One issuance need not hold every name, as with an exact certificate issued on its own, so the
// names it does not hold stay where they are.
func TestCertsSwapLeavesOtherNamesAlone(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	generateTestTlsFiles(t, filepath.Join(vaultHome, "tls", "3.0.0"), "example.com", false)

	if err := swapTlsCurrent(vaultHome, "main", "3.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	requireTlsLink(t, vaultHome, "example.com", filepath.Join("..", "3.0.0", "example.com"))
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "1.0.0", "star.example.com"))
}

// A swap that died between its two steps left a temporary link. The next one must finish, not fail
// on it.
func TestCertsSwapReplacesItsLeftoverTemporaryLink(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	currentHome := filepath.Join(vaultHome, "tls", tlsCurrentDirName)
	if err := os.MkdirAll(currentHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../1.0.0/example.com", filepath.Join(currentHome, ".example.com.swap")); err != nil {
		t.Fatal(err)
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	requireTlsLink(t, vaultHome, "example.com", filepath.Join("..", "2.0.0", "example.com"))
	if _, err := os.Lstat(filepath.Join(currentHome, ".example.com.swap")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leftover survived: %v", err)
	}
}

// A move that fails reaches the caller, reloads nothing and leaves no link.
func TestCertsSwapReportsAFailedMove(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	settings := fake.settings()
	renameErr := errors.New("disk failure")
	settings.rename = func(string, string) error { return renameErr }

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard)
	if !errors.Is(err, renameErr) {
		t.Fatalf("swap error = %v, want the rename failure", err)
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("a failed move reloaded nginx: %v", fake.reloadArgs)
	}
	entries, err := os.ReadDir(filepath.Join(vaultHome, "tls", tlsCurrentDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed move left %v", entries)
	}
}

// A move that fails midway must not leave a version half in place: the name before it goes back,
// so the pair of names still agree.
func TestCertsSwapPutsLinksBackWhenAMoveFails(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	fake.reloadArgs = nil
	settings := fake.settings()
	renameErr := errors.New("disk failure")
	settings.rename = func(oldPath string, newPath string) error {
		if filepath.Base(newPath) == "star.example.com" {
			return renameErr
		}
		return os.Rename(oldPath, newPath)
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard)
	if !errors.Is(err, renameErr) {
		t.Fatalf("swap error = %v, want the rename failure", err)
	}
	for _, name := range []string{"example.com", "star.example.com"} {
		requireTlsLink(t, vaultHome, name, filepath.Join("..", "1.0.0", name))
	}
	if len(fake.reloadArgs) != 0 {
		t.Errorf("a failed move reloaded nginx: %v", fake.reloadArgs)
	}
}

// A link that cannot go back leaves the host in a state only the operator can judge, so the
// failure names it.
func TestCertsSwapReportsALinkItCouldNotPutBack(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	reloadErr := errors.New("nginx refused the reload")
	fake.containerIdReloadErrs = map[string]error{"aaaa11": reloadErr}
	settings := fake.settings()
	putBackErr := errors.New("disk failure")
	renameCount := 0
	settings.rename = func(oldPath string, newPath string) error {
		renameCount += 1
		if 2 < renameCount {
			return putBackErr
		}
		return os.Rename(oldPath, newPath)
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard)
	if !errors.Is(err, reloadErr) || !errors.Is(err, putBackErr) {
		t.Fatalf("swap error = %v, want both failures", err)
	}
	if !strings.Contains(err.Error(), "star.example.com") {
		t.Errorf("swap error = %v, want one naming the link", err)
	}
}

// A stray file in a version is not a name, so it gets no link and fails no check.
func TestCertsSwapPassesOverFilesInAVersion(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := os.WriteFile(filepath.Join(vaultHome, "tls", "2.0.0", "NOTES"), []byte("issued by hand"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tlsCurrentLinkPath(vaultHome, "NOTES")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stray file got a link: %v", err)
	}
}

// tls/current leads to private keys, so a swap creates it for its owner alone.
func TestCertsSwapCreatesCurrentForItsOwnerAlone(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(vaultHome, "tls", tlsCurrentDirName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("tls/current has mode %o, want 700", info.Mode().Perm())
	}
}

// A call past the timeout is a failure whatever docker answered, and a timeout that is not
// positive is past already, as its doc says. It stops the swap at the census.
func TestCertsSwapRefusesAtTheCensusWithNoTimeLeft(t *testing.T) {
	for _, dockerTimeout := range []time.Duration{0, -time.Second} {
		vaultHome := setupCertsSwapVault(t)
		fake := newFakeLbDocker(t, vaultHome)
		settings := fake.settings()
		settings.dockerTimeout = dockerTimeout

		err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout %s: swap error = %v, want the deadline", dockerTimeout, err)
		}
		requireNothingSwapped(t, vaultHome, fake)
	}
}

// Writes a docker onto a new directory with a sudo that passes through, since the docker cli runs
// through sudo on linux, and returns that directory and the PATH that finds them first.
func dockerScriptPath(t *testing.T, dockerScript string) (binDir string, path string) {
	t.Helper()
	binDir = t.TempDir()
	scripts := map[string]string{
		"docker": dockerScript,
		"sudo":   "#!/bin/sh\nexec \"$@\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return binDir, binDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// A daemon that never answers fails the swap instead of hanging the job that ran it. The sleep
// ends by itself, so that without the bound this fails, not hangs.
func TestCertsSwapGivesUpOnAHungDocker(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	_, path := dockerScriptPath(t, "#!/bin/sh\nexec sleep 30\n")
	t.Setenv("PATH", path)
	settings := defaultCertsSwapSettings()
	settings.dockerTimeout = time.Millisecond

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("swap error = %v, want the deadline", err)
	}
	requireNothingSwapped(t, vaultHome, &fakeLbDocker{})
}

// A docker that has to be killed takes its children with it: sudo's docker on linux is one, and a
// child left holding the output open would keep the swap waiting. The grandchild here outlives any
// test, so only the kill of its process group ends it, and waitDelay, which would end the wait
// without it, is longer than the barrier ever needs.
func TestCertsSwapKillsAHungDockerAndItsChildren(t *testing.T) {
	binDir, path := dockerScriptPath(t, "#!/bin/sh\nsleep 600 &\necho $! > \"$(dirname \"$0\")/child.tmp\"\nmv \"$(dirname \"$0\")/child.tmp\" \"$(dirname \"$0\")/child\"\nwait\n")
	t.Setenv("PATH", path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := runDockerCli(ctx, time.Minute, []string{"ps"})
		result <- err
	}()

	childPath := filepath.Join(binDir, "child")
	deadline := time.Now().Add(30 * time.Second)
	childPid := 0
	for childPid == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(childPath); err == nil {
			childPid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if childPid == 0 {
		t.Fatal("the docker script never started its child")
	}
	t.Cleanup(func() { syscall.Kill(childPid, syscall.SIGKILL) })
	cancel()

	if err := <-result; err == nil {
		t.Fatal("a killed docker returned no error")
	}
	if processRuns(t, childPid) {
		t.Fatalf("the docker's child %d outlived the kill", childPid)
	}
}

// Whether a process has yet to exit. One that is gone has, and so has one that is dead and
// waiting for its parent to reap it, which an orphan in a container may do for good.
func processRuns(t *testing.T, pid int) bool {
	t.Helper()
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// the state is the first field after the command, which is in parentheses
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
		return 0 < len(fields) && fields[0] != "Z"
	}
	output, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return false
		}
		t.Fatalf("cannot tell whether %d runs: %v", pid, err)
	}
	state := strings.TrimSpace(string(output))
	return state != "" && !strings.HasPrefix(state, "Z")
}

// The real docker runner runs through sudo on linux as warpctl's does, and a failure carries what
// the cli printed to stderr, which names the daemon's reason.
func TestCertsSwapRunsTheDockerCliAndKeepsItsReason(t *testing.T) {
	binDir, path := dockerScriptPath(t, "#!/bin/sh\necho \"$@\" > \"$(dirname \"$0\")/args\"\necho 'Error: No such container: aaaa11' >&2\nexit 1\n")
	t.Setenv("PATH", path)

	_, err := runDockerCli(context.Background(), time.Second, []string{"top", "aaaa11", "-o", "pid,args"})
	if err == nil || !strings.Contains(err.Error(), "No such container: aaaa11") {
		t.Fatalf("docker error = %v, want one carrying the cli's reason", err)
	}
	args, readErr := os.ReadFile(filepath.Join(binDir, "args"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(args)) != "top aaaa11 -o pid,args" {
		t.Fatalf("docker ran with %q", args)
	}
}

// An nginx serving star.example.com from tls/current under vaultHome, as the lb does, with the
// block config a reload names. The two configs differ only in their listen lines, as `<block>.conf`
// differs from the `_host.conf` that a host-networked lb starts from.
type tlsTestNginx struct {
	binary          string
	vaultHome       string
	prefixDir       string
	address         string
	hostConfigPath  string
	blockConfigPath string
	output          *bytes.Buffer
	masterPid       int
}

// The installed nginx, which needs its ssl module for these tests, or a skip when there is none.
func tlsTestNginxBinary(t *testing.T) string {
	t.Helper()
	nginxBinary, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("no nginx on PATH")
	}
	output, err := exec.Command(nginxBinary, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "http_ssl_module") {
		t.Skipf("%s has no ssl module: %v %s", nginxBinary, err, output)
	}
	return nginxBinary
}

// Starts one on a free port and stops it when the test ends. A caller waits for its first leaf,
// because nginx answers some time after it starts.
func startTlsTestNginx(t *testing.T, vaultHome string) *tlsTestNginx {
	t.Helper()
	nginxBinary := tlsTestNginxBinary(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	prefixDir := t.TempDir()
	writeConfig := func(fileName string, listenAddress string) string {
		configPath := filepath.Join(prefixDir, fileName)
		config := fmt.Sprintf(`
pid %[1]s/nginx.pid;
error_log stderr;
events { worker_connections 64; }
http {
    access_log off;
    client_body_temp_path %[1]s/client_body;
    proxy_temp_path %[1]s/proxy;
    fastcgi_temp_path %[1]s/fastcgi;
    uwsgi_temp_path %[1]s/uwsgi;
    scgi_temp_path %[1]s/scgi;
    server {
        listen %[2]s ssl;
        server_name star.example.com;
        ssl_certificate     %[3]s/tls/current/star.example.com/star.example.com.pem;
        ssl_certificate_key %[3]s/tls/current/star.example.com/star.example.com.key;
    }
}
`, prefixDir, listenAddress, vaultHome)
		if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
			t.Fatal(err)
		}
		return configPath
	}
	hostConfigPath := writeConfig("block_host.conf", address)
	blockConfigPath := writeConfig("block.conf", "127.0.0.1:1")

	self := &tlsTestNginx{
		binary:          nginxBinary,
		vaultHome:       vaultHome,
		prefixDir:       prefixDir,
		address:         address,
		hostConfigPath:  hostConfigPath,
		blockConfigPath: blockConfigPath,
		output:          &bytes.Buffer{},
	}
	cmd := exec.Command(nginxBinary, "-g", "daemon off;", "-c", hostConfigPath, "-p", prefixDir)
	cmd.Stdout = self.output
	cmd.Stderr = self.output
	// workers inherit the output pipe, so Wait must not depend on them closing it
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	self.masterPid = cmd.Process.Pid
	t.Cleanup(func() {
		// the master takes its workers down, where a kill would orphan them
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	})
	return self
}

// What `docker top` prints for this nginx were it an lb: its master and the master's children,
// each by pid and title, read from the process table as docker top reads a container's.
func (self *tlsTestNginx) top() ([]byte, error) {
	output, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil, err
	}
	masterPid := strconv.Itoa(self.masterPid)
	top := "PID                 COMMAND\n"
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if 3 <= len(fields) && (fields[0] == masterPid || fields[1] == masterPid) {
			top += fmt.Sprintf("%-20s%s\n", fields[0], strings.Join(fields[2:], " "))
		}
	}
	return []byte(top), nil
}

// The lines the swap's grep finds in the config a reload names, with the vault at the path an lb
// mounts it at.
func (self *tlsTestNginx) swapGrepLines(t *testing.T) []string {
	t.Helper()
	config, err := os.ReadFile(self.blockConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, line := range strings.Split(string(config), "\n") {
		if strings.Contains(line, "ssl_certificate") || strings.Contains(line, "worker_processes") {
			lines = append(lines, strings.ReplaceAll(line, self.vaultHome, lbVaultMountPath))
		}
	}
	return lines
}

// The reload a swap asks for, run here rather than in a container.
func (self *tlsTestNginx) reload(string) error {
	output, err := exec.Command(self.binary, "-c", self.blockConfigPath, "-p", self.prefixDir, "-s", "reload").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}

// Whether an lb starting now would load what the links name. It is the check a start makes, run
// over the config this nginx started from.
func (self *tlsTestNginx) checkStart() error {
	output, err := exec.Command(self.binary, "-t", "-c", self.hostConfigPath, "-p", self.prefixDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}

// The leaf served now, nil while nginx is not answering.
func (self *tlsTestNginx) servedLeaf() *x509.Certificate {
	conn, err := tls.Dial("tcp", self.address, &tls.Config{ServerName: "star.example.com", InsecureSkipVerify: true})
	if err != nil {
		return nil
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0]
}

// A start and a reload finish after their callers return, so this retries a handshake until the
// wanted leaf answers. It ends at a deadline only on failure.
func (self *tlsTestNginx) waitForLeaf(t *testing.T, want *x509.Certificate) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if served := self.servedLeaf(); served != nil && served.Equal(want) {
			return
		}
	}
	t.Fatalf("nginx never served the wanted leaf:\n%s", self.output.String())
}

// The leaf a version holds for a name.
func tlsVersionLeaf(t *testing.T, vaultHome string, version string, name string) *x509.Certificate {
	t.Helper()
	pemBytes, err := os.ReadFile(filepath.Join(vaultHome, "tls", version, name, name+".pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// The whole of W5 against a real nginx, through the paths the lb reads: nginx serves the pair
// tls/current names, a moved link alone changes nothing, and the reload then serves the new pair,
// though nginx was started from another config than the one the reload names.
func TestCertsSwapServesTheNewPairThroughNginx(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	nginx := startTlsTestNginx(t, vaultHome)
	fake.onReload = nginx.reload

	oldLeaf := tlsVersionLeaf(t, vaultHome, "1.0.0", "star.example.com")
	newLeaf := tlsVersionLeaf(t, vaultHome, "2.0.0", "star.example.com")
	if oldLeaf.Equal(newLeaf) {
		t.Fatal("the fixture's two versions hold one certificate")
	}
	nginx.waitForLeaf(t, oldLeaf)

	settings := fake.settings()
	settings.rename = func(oldPath string, newPath string) error {
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		if served := nginx.servedLeaf(); served == nil || !served.Equal(oldLeaf) {
			t.Error("nginx stopped serving the old pair before any reload")
		}
		return nil
	}

	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, io.Discard); err != nil {
		t.Fatal(err)
	}
	nginx.waitForLeaf(t, newLeaf)
}

// nginx asks more of a pair than this command can, and proves a config loads before it signals. A
// pair that passed these checks is refused with the old pair still serving, the refusal reaches the
// caller, and the links go back, because a later lb start reads them and would fail where this
// reload did.
func TestCertsSwapSurfacesARefusedNginxReload(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	nginx := startTlsTestNginx(t, vaultHome)
	oldLeaf := tlsVersionLeaf(t, vaultHome, "1.0.0", "star.example.com")
	nginx.waitForLeaf(t, oldLeaf)

	fake.onReload = func(containerId string) error {
		mismatchTlsPair(t, vaultHome, "2.0.0", "star.example.com", "1.0.0")
		return nginx.reload(containerId)
	}

	err := swapTlsCurrent(vaultHome, "main", "2.0.0", fake.settings(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "SSL_CTX_use_PrivateKey") {
		t.Fatalf("swap error = %v, want nginx refusing the pair", err)
	}
	// refused before any signal, so this reads the settled state
	if served := nginx.servedLeaf(); served == nil || !served.Equal(oldLeaf) {
		t.Fatal("nginx stopped serving the old pair after refusing the reload")
	}
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "1.0.0", "star.example.com"))
	if err := nginx.checkStart(); err != nil {
		t.Fatalf("an lb could not start from the links a refused swap left: %v", err)
	}
}

// What a real nginx does with a config that sets no worker count, as warpctl writes for a block
// without capacity sizing: it runs exactly its default of one worker. The swap reads the config the
// reload names, takes that count, waits for it, signals, and sees the reload through nginx's own
// titles, read from the process table as docker top reads them, and nginx then serves the new
// pair.
func TestCertsSwapReloadsARealNginxThatSetsNoWorkerCount(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps on PATH to read nginx's workers with")
	}
	vaultHome := setupCertsSwapVault(t)
	fake := newFakeLbDocker(t, vaultHome)
	if err := swapTlsCurrent(vaultHome, "main", "1.0.0", fake.settings(), io.Discard); err != nil {
		t.Fatal(err)
	}

	nginx := startTlsTestNginx(t, vaultHome)
	nginx.waitForLeaf(t, tlsVersionLeaf(t, vaultHome, "1.0.0", "star.example.com"))
	grepLines := nginx.swapGrepLines(t)
	if lbWorkerProcessesLineRegex.MatchString(strings.Join(grepLines, "\n")) {
		t.Fatalf("the test nginx's config sets a worker count: %q", grepLines)
	}
	fake.containerIdTlsLines = map[string][]string{"aaaa11": grepLines}
	fake.containerIdWorkerProcessesLines = map[string]string{"aaaa11": ""}
	fake.containerIdTopFuncs = map[string]func() ([]byte, error){"aaaa11": nginx.top}
	fake.onReload = nginx.reload
	settings := fake.settings()
	settings.reloadCheckCount = 3000
	settings.reloadCheckInterval = 10 * time.Millisecond

	workerPids, _, err := readLbWorkers(settings, "aaaa11")
	if err != nil {
		t.Fatal(err)
	}
	if len(workerPids) != nginxDefaultWorkerCount {
		top, _ := nginx.top()
		t.Fatalf("nginx runs %d workers for a config that sets no count, want %d:\n%s", len(workerPids), nginxDefaultWorkerCount, top)
	}

	output := &bytes.Buffer{}
	if err := swapTlsCurrent(vaultHome, "main", "2.0.0", settings, output); err != nil {
		t.Fatalf("%v\n%s", err, nginx.output.String())
	}
	if !strings.Contains(output.String(), "Reloaded edge-0.example.com-eth0\n") {
		t.Errorf("output %q does not say it reloaded", output.String())
	}
	nginx.waitForLeaf(t, tlsVersionLeaf(t, vaultHome, "2.0.0", "star.example.com"))
}

// Answers for the docker a sidecarctl process runs: one lb, with the tls lines and worker in the
// files beside this script, which a reload changes. A file named refuse-reload makes nginx refuse.
const fakeDockerScript = `#!/bin/sh
home=$(dirname "$0")
case "$1" in
ps)
	echo "aaaa11 edge-0.example.com-eth0"
	;;
top)
	echo "PID COMMAND"
	echo "$(cat "$home/worker") nginx: worker process"
	;;
exec)
	case "$3" in
	grep)
		cat "$home/tls-lines"
		;;
	readlink)
		shift 3
		for link in "$@"; do
			readlink "$WARP_VAULT_HOME${link#/srv/warp/vault}"
		done
		;;
	nginx)
		if [ -e "$home/refuse-reload" ]; then
			echo "nginx: [emerg] refused by the fake" >&2
			exit 1
		fi
		echo 200 > "$home/worker"
		;;
	esac
	;;
*)
	exit 64
	;;
esac
`

// Not a test: it is main, for the process runSidecarctlProcess starts.
func TestSidecarctlMainHelper(t *testing.T) {
	args := os.Getenv("SIDECARCTL_MAIN_HELPER_ARGS")
	if args == "" {
		return
	}
	os.Args = append([]string{"sidecarctl"}, strings.Fields(args)...)
	main()
}

// Runs sidecarctl as a process against vaultHome, with the docker PATH finds, and returns all it
// printed with its exit status. The status is the whole of what a job sees of a swap, and only a
// process has one.
func runSidecarctlProcess(t *testing.T, path string, vaultHome string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSidecarctlMainHelper$")
	cmd.Env = append(
		os.Environ(),
		fmt.Sprintf("PATH=%s", path),
		fmt.Sprintf("WARP_VAULT_HOME=%s", vaultHome),
		fmt.Sprintf("SIDECARCTL_MAIN_HELPER_ARGS=%s", strings.Join(args, " ")),
	)
	output, err := cmd.CombinedOutput()
	exitErr := &exec.ExitError{}
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatal(err)
	}
	return string(output), cmd.ProcessState.ExitCode()
}

// A docker for runSidecarctlProcess whose one lb reads the fixture's names through tls/current,
// and the directory holding its files.
func setupSidecarctlDocker(t *testing.T) (binDir string, path string) {
	t.Helper()
	binDir, path = dockerScriptPath(t, fakeDockerScript)
	files := map[string]string{
		"worker":    "100",
		"tls-lines": strings.Join(append([]string{"worker_processes 1;"}, tlsCurrentLines("example.com", "star.example.com")...), "\n"),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return binDir, path
}

// The command through main, from the command line to the reload.
func TestCertsSwapCommandMovesLinksAndReloads(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	_, path := setupSidecarctlDocker(t)

	output, exitCode := runSidecarctlProcess(t, path, vaultHome, "certs", "swap", "main", "2.0.0")

	if exitCode != 0 {
		t.Fatalf("exited %d:\n%s", exitCode, output)
	}
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "2.0.0", "star.example.com"))
	if !strings.Contains(output, "Reloaded edge-0.example.com-eth0") {
		t.Fatalf("output does not say it reloaded:\n%s", output)
	}
}

// A swap nginx refused must exit non-zero, or the job that ran it reads a rotation that did not
// happen. The reason nginx gave reaches the output.
func TestCertsSwapCommandExitsNonZeroWhenNginxRefuses(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	binDir, path := setupSidecarctlDocker(t)
	if err := os.WriteFile(filepath.Join(binDir, "refuse-reload"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	output, exitCode := runSidecarctlProcess(t, path, vaultHome, "certs", "swap", "main", "2.0.0")

	if exitCode != 1 {
		t.Fatalf("a refused swap exited %d, want 1:\n%s", exitCode, output)
	}
	if !strings.Contains(output, "refused by the fake") {
		t.Errorf("output does not carry nginx's reason:\n%s", output)
	}
	if _, err := os.Lstat(tlsCurrentLinkPath(vaultHome, "star.example.com")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused swap left its link: %v", err)
	}
}

// The same for a swap an lb's paths refuse, which is every lb's state today.
func TestCertsSwapCommandExitsNonZeroOnAVersionedLb(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	binDir, path := setupSidecarctlDocker(t)
	tlsLines := strings.Join(append([]string{"worker_processes 1;"}, tlsVersionLines("1.0.0", "star.example.com")...), "\n")
	if err := os.WriteFile(filepath.Join(binDir, "tls-lines"), []byte(tlsLines), 0644); err != nil {
		t.Fatal(err)
	}

	output, exitCode := runSidecarctlProcess(t, path, vaultHome, "certs", "swap", "main", "2.0.0")

	if exitCode != 1 {
		t.Fatalf("a swap that reaches nothing exited %d, want 1:\n%s", exitCode, output)
	}
	if strings.Contains(output, "Reloaded") {
		t.Fatalf("a swap that reaches nothing printed Reloaded:\n%s", output)
	}
}

// The flag reaches the swap, wherever it stands: with a docker that fails everything, it alone can
// move the links.
func TestCertsSwapCommandNoReloadNeedsNoDocker(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	_, path := dockerScriptPath(t, "#!/bin/sh\nexit 1\n")

	output, exitCode := runSidecarctlProcess(t, path, vaultHome, "certs", "swap", "main", "2.0.0")
	if exitCode == 0 {
		t.Fatalf("a swap exited 0 with docker failing:\n%s", output)
	}

	output, exitCode = runSidecarctlProcess(t, path, vaultHome, "certs", "swap", "--no-reload", "main", "2.0.0")
	if exitCode != 0 {
		t.Fatalf("exited %d:\n%s", exitCode, output)
	}
	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "2.0.0", "star.example.com"))
}

// A command line the swap cannot take exits 2 with its usage, and touches nothing.
func TestCertsSwapCommandExitsTwoOnAMissingVersion(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	_, path := dockerScriptPath(t, "#!/bin/sh\nexit 1\n")

	output, exitCode := runSidecarctlProcess(t, path, vaultHome, "certs", "swap", "main", "--no-reload")

	if exitCode != 2 {
		t.Fatalf("exited %d, want 2:\n%s", exitCode, output)
	}
	if !strings.Contains(output, "sidecarctl certs swap <env> <version> [--no-reload]") {
		t.Errorf("output does not give the usage:\n%s", output)
	}
	if _, err := os.Lstat(filepath.Join(vaultHome, "tls", tlsCurrentDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused command line made tls/current: %v", err)
	}
}

// A line the command cannot take is a usage mistake, which exits 2, and it touches nothing: a
// missing version, a word more than it takes, and a flag it does not have.
func TestCertsSwapCommandRefusesALineItCannotTake(t *testing.T) {
	vaultHome := setupCertsSwapVault(t)
	t.Setenv("WARP_VAULT_HOME", vaultHome)
	for _, args := range [][]string{
		{"main", "--no-reload"},
		{"main", "2.0.0", "1.0.0", "--no-reload"},
		{"main", "2.0.0", "--no-reload", "--force"},
	} {
		err := runCertsSwap(args, io.Discard)
		usageErr := &sidecarctlUsageError{}
		if !errors.As(err, &usageErr) {
			t.Errorf("%v: error = %v, want a usage mistake", args, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(vaultHome, "tls", tlsCurrentDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused command line made tls/current: %v", err)
	}
}

// The vault is found as warpctl finds it: WARP_VAULT_HOME, else WARP_HOME's vault.
func TestCertsSwapCommandFindsTheVaultUnderWarpHome(t *testing.T) {
	warpHome := t.TempDir()
	vaultHome := filepath.Join(warpHome, "vault")
	for _, version := range []string{"1.0.0", "2.0.0"} {
		generateTestTlsFiles(t, filepath.Join(vaultHome, "tls", version), "example.com", true)
	}
	t.Setenv("WARP_VAULT_HOME", "")
	t.Setenv("WARP_HOME", warpHome)

	if err := runCertsSwap([]string{"main", "2.0.0", "--no-reload"}, io.Discard); err != nil {
		t.Fatal(err)
	}

	requireTlsLink(t, vaultHome, "star.example.com", filepath.Join("..", "2.0.0", "star.example.com"))
}

// With neither set there is no vault to swap in, which is a failure, not a swap of some default.
func TestCertsSwapCommandRefusesWithNoVault(t *testing.T) {
	t.Setenv("WARP_VAULT_HOME", "")
	t.Setenv("WARP_HOME", "")

	err := runCertsSwap([]string{"main", "2.0.0", "--no-reload"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "WARP_HOME") {
		t.Fatalf("command error = %v, want one naming the missing setting", err)
	}
}
