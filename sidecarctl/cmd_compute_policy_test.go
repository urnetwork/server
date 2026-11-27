// `sidecarctl compute policy build|sign|verify|publish|status|staging` on software test keys standing in for the
// holders' tokens (ATTEST-M12-02): a test release key certifies a compute-policy key and a staging key, the commands
// pin it in place of this build's release key, and a signer stands in for ssh-keygen with each key, so a policy is
// built, signed, verified and published round trip, a week's next built on it, and the staging policy signed under
// its own namespace; each refusal the commands name, and this build's own state, which pins no release key and so
// refuses every policy. One test runs the real ssh-keygen with a software key file, where one is installed.

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The test sitting: 2026-12-01 10:00 UTC.
var testComputePolicySitting = time.Date(2026, time.December, 1, 10, 0, 0, 0, time.UTC)

// A stream of bytes that repeats for its label: SHA-256 of it and a counter, block after block.
type testComputePolicyStream struct {
	label   string
	counter uint64
	buffer  []byte
}

// More of the stream.
func (self *testComputePolicyStream) Read(out []byte) (int, error) {
	for len(self.buffer) < len(out) {
		block := sha256.New()
		block.Write([]byte("urnetwork-server sidecarctl compute policy test " + self.label))
		binary.Write(block, binary.BigEndian, self.counter)
		self.counter += 1
		self.buffer = block.Sum(self.buffer)
	}
	count := copy(out, self.buffer)
	self.buffer = self.buffer[count:]
	return count, nil
}

// An Ed25519 key from label, the same every run.
func testComputePolicyKey(t *testing.T, label string) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	seed := sha256.Sum256([]byte("urnetwork-server sidecarctl compute policy test key " + label))
	private := ed25519.NewKeyFromSeed(seed[:])
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return private, signer
}

// key with an OpenSSH user certificate by authority naming principal alone, valid from validAfter until validBefore,
// as a signer whose public key is the certificate.
func testComputePolicyCertify(t *testing.T, key ssh.Signer, authority ssh.Signer, principal string, validAfter time.Time, validBefore time.Time) ssh.Signer {
	t.Helper()
	certificate := &ssh.Certificate{
		Key:             key.PublicKey(),
		Serial:          7,
		CertType:        ssh.UserCert,
		KeyId:           "sidecarctl test " + principal,
		ValidPrincipals: []string{principal},
		ValidAfter:      uint64(validAfter.Unix()),
		ValidBefore:     uint64(validBefore.Unix()),
		Permissions:     ssh.Permissions{CriticalOptions: map[string]string{}, Extensions: map[string]string{}},
	}
	if err := certificate.SignCert(&testComputePolicyStream{label: "certificate " + principal}, authority); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewCertSigner(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// A test's world: its directories, the test keys and their certificates' files, a fake clock, and settings pinning
// the test release key with a software signer for each certificate's file.
type testComputePolicyWorld struct {
	directory string
	// the serving tree, the staging serving tree, the release's copy's directory and the ticket host's store
	tree        string
	stagingTree string
	release     string
	store       string
	clock       *sidecartest.FakeClock
	settings    *computePolicyCommandSettings
	releaseKey  ssh.Signer
	// the certificates' files: the compute-policy key's, the staging key's, and one the release key did not sign
	policyCertificate      string
	stagingCertificate     string
	uncertifiedCertificate string
	// each certificate file's signer
	pathSigners map[string]ssh.Signer
	// how many signatures were asked for
	signCount int
}

// A world at the test sitting, its tree, release directory and store made and empty.
func newTestComputePolicyWorld(t *testing.T) *testComputePolicyWorld {
	t.Helper()
	directory := t.TempDir()
	world := &testComputePolicyWorld{
		directory:   directory,
		tree:        filepath.Join(directory, "tree"),
		stagingTree: filepath.Join(directory, "staging-tree"),
		release:     filepath.Join(directory, "attest_policy_release"),
		store:       filepath.Join(directory, "store"),
		clock:       sidecartest.NewFakeClock(testComputePolicySitting),
		pathSigners: map[string]ssh.Signer{},
	}
	for _, path := range []string{world.tree, world.stagingTree, world.release, world.store, filepath.Join(directory, "keys")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, release := testComputePolicyKey(t, "release")
	_, otherRelease := testComputePolicyKey(t, "other release")
	_, policyKey := testComputePolicyKey(t, "compute policy")
	_, stagingKey := testComputePolicyKey(t, "staging")
	world.releaseKey = release
	validAfter := testComputePolicySitting.Add(-30 * 24 * time.Hour)
	validBefore := validAfter.Add(364 * 24 * time.Hour)
	world.policyCertificate = world.writeCertificate(t, "policy",
		testComputePolicyCertify(t, policyKey, release, sidecar.ComputePolicyNamespace, validAfter, validBefore))
	world.stagingCertificate = world.writeCertificate(t, "staging",
		testComputePolicyCertify(t, stagingKey, release, sidecar.ComputePolicyStagingNamespace, validAfter, validBefore))
	world.uncertifiedCertificate = world.writeCertificate(t, "uncertified",
		testComputePolicyCertify(t, policyKey, otherRelease, sidecar.ComputePolicyNamespace, validAfter, validBefore))
	settings := defaultComputePolicyCommandSettings()
	settings.releaseKeys = []ssh.PublicKey{release.PublicKey()}
	settings.clock = world.clock
	settings.random = &testComputePolicyStream{label: "batch ids"}
	settings.sign = func(ctx context.Context, settings *computePolicyCommandSettings, certificatePath string, namespace string, message []byte) ([]byte, error) {
		world.signCount += 1
		signer, ok := world.pathSigners[certificatePath]
		if !ok {
			return nil, errors.New("the test token holds no key for that certificate")
		}
		return sidecar.ComputeSshsigSign(&testComputePolicyStream{label: "sshsig"}, signer, namespace, message)
	}
	world.settings = settings
	return world
}

// Writes signer's certificate as keys/<name>-cert.pub, the name --key takes, and answers its path.
func (self *testComputePolicyWorld) writeCertificate(t *testing.T, name string, signer ssh.Signer) string {
	t.Helper()
	path := filepath.Join(self.directory, "keys", name+computePolicyCertificateSuffix)
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644); err != nil {
		t.Fatal(err)
	}
	self.pathSigners[path] = signer
	return path
}

// Runs one of the policy's commands, by the words after compute policy, with the world's settings.
func (self *testComputePolicyWorld) run(words string, args ...string) (string, error) {
	out := &bytes.Buffer{}
	commands := map[string]func(args []string, settings *computePolicyCommandSettings, out io.Writer) error{
		"build":           runComputePolicyBuildWithSettings,
		"sign":            runComputePolicySignWithSettings,
		"verify":          runComputePolicyVerifyWithSettings,
		"publish":         runComputePolicyPublishWithSettings,
		"status":          runComputePolicyStatusWithSettings,
		"staging":         runComputePolicyStagingWithSettings,
		"tickets sign":    runComputePolicyTicketsSignWithSettings,
		"tickets release": runComputePolicyTicketsReleaseWithSettings,
	}
	err := commands[words](args, self.settings, out)
	return out.String(), err
}

// Runs a command that must succeed.
func (self *testComputePolicyWorld) must(t *testing.T, words string, args ...string) string {
	t.Helper()
	out, err := self.run(words, args...)
	if err != nil {
		t.Fatalf("compute policy %s %q: %v\n%s", words, args, err, out)
	}
	return out
}

// The first policy's change set: a TEE provider with a model and a reproduced profile, a direct provider, two
// disclosures and the support line, every value synthetic.
const testComputePolicyFirstChanges = `{
  "roots": {"intel_sgx": ["0101010101010101010101010101010101010101010101010101010101010101"], "amd_arks": [], "nvidia_device": [], "nvidia_nras": []},
  "providers": [{
    "id": "teetest", "cpus": ["tdx"], "schemes": ["chutes-e2e-v1"], "label": "AP", "roots": [],
    "models": [{"model": "test-model-27b", "provider_model_id": "chute-test-0001"}],
    "profiles": [{"id": "teetest-2026.12.1", "guest_version": "test-1", "cpu": "tdx",
      "mrtd": "%[1]s", "rtmr0": "%[1]s", "rtmr1": "%[1]s", "rtmr2": "%[1]s", "rtmr3": "%[1]s",
      "xfam": "", "measurement": "", "single_socket": false, "gpu_model": "", "gpu_count": 0, "payload_root_sha256": "",
      "expires_ms": 1830000000000, "replayed": [], "review": "reproduced", "review_list_sha256": ""}]
  }],
  "direct": {"providers": [{"slug": "directtest", "api_host": "api.directtest.example", "receiver_hotkey": "5Ctestreceiver", "netuid": 4201,
    "targets": [{"method": "POST", "path": "/v1/chat/completions"}],
    "models": [{"model": "test-model-35b", "provider_model_id": "test/test-model-35b", "aliases": [], "zero_retention": true, "upstreams": []}]}],
    "limited_use": ["teetest-test-model-27b"]},
  "support": {"host": "feedback.test.example", "subprocessors": [{"name": "Test triage", "purpose": "reads support's entries for triage", "retention": "30 days"}]},
  "disclosures": [
    {"change": "fact", "disclosure": {"id": "inference:teetest", "version": 1, "revision": 0, "grace_days": 14, "since_ms": 0,
      "locales": [{"locale": "en", "summary": "Synthetic test text one.", "exposed": ["Synthetic exposed one."], "private": ["Synthetic private one."]}]}},
    {"change": "fact", "disclosure": {"id": "support:urnetwork", "version": 1, "revision": 0, "grace_days": 0, "since_ms": 0,
      "locales": [{"locale": "en", "summary": "Synthetic support text one.", "exposed": ["Synthetic exposed one."], "private": ["Synthetic private one."]}]}}
  ]
}`

// Writes a change set into the world's directory as name, and answers its path.
func (self *testComputePolicyWorld) writeChanges(t *testing.T, name string, changes string) string {
	t.Helper()
	path := filepath.Join(self.directory, name)
	if err := os.WriteFile(path, []byte(changes), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The first policy's change set's file.
func (self *testComputePolicyWorld) firstChanges(t *testing.T) string {
	t.Helper()
	return self.writeChanges(t, "first-changes.json", fmt.Sprintf(testComputePolicyFirstChanges, strings.Repeat("ab", 48)))
}

// Builds, signs and publishes the first policy into the tree, and answers its working file's path.
func (self *testComputePolicyWorld) publishFirst(t *testing.T) string {
	t.Helper()
	path := filepath.Join(self.directory, "work-1.json")
	self.must(t, "build", path, "--changes="+self.firstChanges(t))
	self.must(t, "sign", path, "--key="+self.policyCertificate)
	self.must(t, "publish", path, "--tree="+self.tree, "--release="+self.release)
	return path
}

// The file at path.
func testComputePolicyRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Whether a file exists at path.
func testComputePolicyExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A policy built, signed under ur-compute-policy by the certified test key through the software signer, verified and
// published into the tree and the release's copy; the week after, the next built on the tree's copy with a listed
// profile and a revocation, signed and published above it, the release's copy moving with it; each verifying as a
// device of this build verifies it.
func TestComputePolicyCommandsRoundTrip(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	first := filepath.Join(world.directory, "work-1.json")
	out := world.must(t, "build", first, "--changes="+world.firstChanges(t))
	if !strings.Contains(out, "built version 1 into "+first) || !strings.Contains(out, "provider teetest added whole") ||
		!strings.Contains(out, "support line replaced") {
		t.Errorf("build printed:\n%s", out)
	}
	if testComputePolicyExists(first + ".sig") {
		t.Fatal("build signed")
	}
	out = world.must(t, "sign", first, "--key="+world.policyCertificate)
	if !strings.Contains(out, "signed version 1") || !strings.Contains(out, sidecar.ComputePolicyNamespace) || world.signCount != 1 {
		t.Errorf("sign printed:\n%s", out)
	}
	out = world.must(t, "verify", first)
	if !strings.Contains(out, "version 1 verifies") || !strings.Contains(out, "90.0 days from now") {
		t.Errorf("verify printed:\n%s", out)
	}
	world.must(t, "publish", first, "--tree="+world.tree, "--release="+world.release)
	document := testComputePolicyRead(t, first)
	if !bytes.Equal(testComputePolicyRead(t, filepath.Join(world.tree, "1.json")), document) ||
		!bytes.Equal(testComputePolicyRead(t, filepath.Join(world.tree, "1.json.sig")), testComputePolicyRead(t, first+".sig")) ||
		!bytes.Equal(testComputePolicyRead(t, filepath.Join(world.release, "compute-policy.json")), document) {
		t.Fatal("publish did not put the signed bytes into the tree and the release's copy")
	}

	world.clock.AdvanceTo(testComputePolicySitting.Add(7 * 24 * time.Hour))
	changes := world.writeChanges(t, "second-changes.json", fmt.Sprintf(`{
	  "profiles": [{"provider": "teetest", "profile": {"id": "teetest-2026.12.2", "guest_version": "test-2", "cpu": "tdx",
	    "mrtd": "%[1]s", "rtmr0": "%[1]s", "rtmr1": "%[1]s", "rtmr2": "%[1]s", "rtmr3": "%[1]s", "expires_ms": 1830000000000,
	    "replayed": [], "review": "listed", "review_list_sha256": "%[2]s"}}],
	  "revoke": {"profiles": ["teetest-2026.12.1"], "chips": [], "gpu_serials": []}
	}`, strings.Repeat("cd", 48), strings.Repeat("ef", 32)))
	second := filepath.Join(world.directory, "work-2.json")
	out = world.must(t, "build", second, "--previous="+filepath.Join(world.tree, "1.json"), "--changes="+changes)
	if !strings.Contains(out, "built version 2") || !strings.Contains(out, "profile teetest-2026.12.2 added under teetest") ||
		!strings.Contains(out, "profile teetest-2026.12.1 revoked") {
		t.Errorf("the second build printed:\n%s", out)
	}
	world.must(t, "sign", second, "--key="+world.policyCertificate)
	world.must(t, "publish", second, "--tree="+world.tree, "--release="+world.release)
	builder := newComputePolicyCommandBuilder(world.settings, false)
	policy, err := readComputePolicyVerified(builder, filepath.Join(world.tree, "2.json"), world.settings)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.ProfileRevoked("teetest-2026.12.1") || policy.Provider("teetest").Profile("teetest-2026.12.2") == nil ||
		!policy.NotAfter().Equal(testComputePolicySitting.Add(97*24*time.Hour)) {
		t.Errorf("version 2: %s", policy.Document())
	}
	copied, err := readComputePolicyVerified(builder, filepath.Join(world.release, "compute-policy.json"), world.settings)
	if err != nil || copied.Version != 2 {
		t.Fatalf("the release's copy: %v, %v", copied, err)
	}
	if !testComputePolicyExists(filepath.Join(world.tree, "1.json")) {
		t.Error("publish removed the version before it from the tree")
	}
}

// sign never reads a private key's file: a --key that is not a certificate's name is refused unread, as a usage
// mistake, before anything is signed. A certificate's name holding a bare key, the staging key's certificate, and a
// certificate the release key did not sign are refused too, and no .sig is written.
func TestComputePolicySignRefusesAKeyThatIsNotACertificate(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	path := filepath.Join(world.directory, "work-1.json")
	world.must(t, "build", path, "--changes="+world.firstChanges(t))
	unreadable := filepath.Join(world.directory, "keys", "holder")
	_, err := world.run("sign", path, "--key="+unreadable)
	usageErr := &sidecarctlUsageError{}
	if !errors.As(err, &usageErr) {
		t.Fatalf("a private key's name: %v", err)
	}
	_, policyKey := testComputePolicyKey(t, "compute policy")
	bare := filepath.Join(world.directory, "keys", "bare"+computePolicyCertificateSuffix)
	if err := os.WriteFile(bare, ssh.MarshalAuthorizedKey(policyKey.PublicKey()), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{bare, world.stagingCertificate, world.uncertifiedCertificate} {
		if _, err := world.run("sign", path, "--key="+key); !errors.Is(err, sidecar.ErrComputePolicyBuildRefused) {
			t.Errorf("%s: %v", filepath.Base(key), err)
		}
	}
	if world.signCount != 0 || testComputePolicyExists(path+".sig") {
		t.Errorf("%d signatures asked for, a .sig written %t", world.signCount, testComputePolicyExists(path+".sig"))
	}
	if _, err := world.run("sign", path); !errors.As(err, &usageErr) {
		t.Errorf("no --key: %v", err)
	}
}

// What a signer answers is checked before it is written: a token that signs with another certified key, or under
// another namespace, leaves no .sig.
func TestComputePolicySignChecksWhatCameBack(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	path := filepath.Join(world.directory, "work-1.json")
	world.must(t, "build", path, "--changes="+world.firstChanges(t))
	_, otherKey := testComputePolicyKey(t, "other compute policy")
	validAfter := testComputePolicySitting.Add(-30 * 24 * time.Hour)
	other := testComputePolicyCertify(t, otherKey, world.releaseKey, sidecar.ComputePolicyNamespace, validAfter, validAfter.Add(364*24*time.Hour))
	world.pathSigners[world.policyCertificate] = other
	if _, err := world.run("sign", path, "--key="+world.policyCertificate); !errors.Is(err, sidecar.ErrComputePolicyBuildRefused) {
		t.Errorf("another certified key: %v", err)
	}
	world.settings.sign = func(ctx context.Context, settings *computePolicyCommandSettings, certificatePath string, namespace string, message []byte) ([]byte, error) {
		return sidecar.ComputeSshsigSign(&testComputePolicyStream{label: "sshsig"}, world.pathSigners[world.stagingCertificate], sidecar.ComputePolicyStagingNamespace, message)
	}
	if _, err := world.run("sign", path, "--key="+world.policyCertificate); !errors.Is(err, sidecar.ErrComputePolicyRefused) {
		t.Errorf("another namespace: %v", err)
	}
	if testComputePolicyExists(path + ".sig") {
		t.Error("a refused signature was written")
	}
}

// This build pins no release key until the key ceremony: with the commands' own default, every check refuses, so
// nothing is signed, verified, published or released, which is the fail-closed answer.
func TestComputePolicyCommandsRefuseWithNoReleaseKeyPinned(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	published := world.publishFirst(t)
	if 0 < len(sidecar.ComputeReleaseKeys()) {
		t.Skip("this build pins a release key, so its pins are not the source build's")
	}
	world.settings.releaseKeys = nil
	path := filepath.Join(world.directory, "unpinned.json")
	world.must(t, "build", path, "--changes="+world.firstChanges(t))
	signs := world.signCount
	if _, err := world.run("sign", path, "--key="+world.policyCertificate); err == nil || !strings.Contains(err.Error(), "pins no release key") {
		t.Errorf("sign: %v", err)
	}
	for _, words := range []string{"verify", "publish"} {
		args := []string{published}
		if words == "publish" {
			args = append(args, "--tree="+world.tree)
		}
		if _, err := world.run(words, args...); !errors.Is(err, sidecar.ErrComputePolicyRefused) {
			t.Errorf("%s: %v", words, err)
		}
	}
	if _, err := world.run("build", filepath.Join(world.directory, "next.json"), "--previous="+filepath.Join(world.tree, "1.json")); err == nil {
		t.Error("build took a previous policy no release key verifies")
	}
	if out, err := world.run("status", world.tree); err == nil || !strings.Contains(out, "none in") {
		t.Errorf("status: %v\n%s", err, out)
	}
	if _, err := world.run("tickets sign", "--store="+world.store, "--key="+world.policyCertificate, "--policy=1"); err == nil {
		t.Error("tickets sign signed with no release key pinned")
	}
	if world.signCount != signs {
		t.Errorf("%d signatures asked for with no release key pinned", world.signCount-signs)
	}
}

// publish puts a version only above the tree's newest and the release's copy: an older version is refused, the same
// one again does nothing, the same version with other bytes is a conflict, and a policy past its not_after is not
// published.
func TestComputePolicyPublishRefusesAVersionNotAboveTheTree(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	first := world.publishFirst(t)
	out := world.must(t, "publish", first, "--tree="+world.tree, "--release="+world.release)
	if !strings.Contains(out, "already serves version 1") || !strings.Contains(out, "already version 1") {
		t.Errorf("publishing the same again printed:\n%s", out)
	}
	second := filepath.Join(world.directory, "work-2.json")
	world.must(t, "build", second, "--previous="+filepath.Join(world.tree, "1.json"))
	world.must(t, "sign", second, "--key="+world.policyCertificate)
	world.must(t, "publish", second, "--tree="+world.tree, "--release="+world.release)
	if _, err := world.run("publish", first, "--tree="+world.tree); err == nil || !strings.Contains(err.Error(), "not above the tree's newest, 2") {
		t.Errorf("an older version into the tree: %v", err)
	}
	if _, err := world.run("publish", first, "--release="+world.release); err == nil || !strings.Contains(err.Error(), "older than the release's copy") {
		t.Errorf("an older version into the release's copy: %v", err)
	}
	other := filepath.Join(world.directory, "work-2-other.json")
	world.must(t, "build", other, "--previous="+filepath.Join(world.tree, "1.json"), "--not-after="+testComputePolicySitting.Add(30*24*time.Hour).Format(time.RFC3339))
	world.must(t, "sign", other, "--key="+world.policyCertificate)
	if _, err := world.run("publish", other, "--tree="+world.tree); err == nil || !strings.Contains(err.Error(), "other bytes") {
		t.Errorf("version 2 with other bytes: %v", err)
	}
	world.clock.AdvanceTo(testComputePolicySitting.Add(91 * 24 * time.Hour))
	if _, err := world.run("publish", second, "--tree="+world.tree); err == nil || !strings.Contains(err.Error(), "past its not_after") {
		t.Errorf("a policy past its not_after: %v", err)
	}
	if _, err := world.run("publish", second); err == nil {
		t.Error("publish with neither --tree nor --release")
	}
}

// build takes a previous policy only as a device would: one with no .sig beside it, one signed by a key the release
// key did not certify, or a staging policy, is refused, and nothing is written. Each refusal of the builder's is the
// command's: a version not above the previous, and a not_after past 90 days.
func TestComputePolicyBuildRefusesAPreviousThatDoesNotVerify(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	world.publishFirst(t)
	unsigned := filepath.Join(world.directory, "unsigned.json")
	world.must(t, "build", unsigned, "--changes="+world.firstChanges(t))
	uncertified := filepath.Join(world.directory, "uncertified.json")
	world.must(t, "build", uncertified, "--changes="+world.firstChanges(t))
	signature, err := sidecar.ComputeSshsigSign(&testComputePolicyStream{label: "sshsig"}, world.pathSigners[world.uncertifiedCertificate],
		sidecar.ComputePolicyNamespace, testComputePolicyRead(t, uncertified))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uncertified+".sig", signature, 0o644); err != nil {
		t.Fatal(err)
	}
	world.must(t, "staging", world.stagingTree, "--key="+world.stagingCertificate)
	for _, previous := range []string{unsigned, uncertified, filepath.Join(world.stagingTree, "1.json")} {
		out := filepath.Join(world.directory, "next-"+filepath.Base(filepath.Dir(previous))+"-"+filepath.Base(previous))
		if _, err := world.run("build", out, "--previous="+previous); err == nil || testComputePolicyExists(out) {
			t.Errorf("a previous of %s: %v", previous, err)
		}
	}
	lower := filepath.Join(world.directory, "lower.json")
	if _, err := world.run("build", lower, "--previous="+filepath.Join(world.tree, "1.json"), "--version=1"); !errors.Is(err, sidecar.ErrComputePolicyBuildRefused) {
		t.Errorf("version 1 again: %v", err)
	}
	late := testComputePolicySitting.Add(91 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := world.run("build", lower, "--previous="+filepath.Join(world.tree, "1.json"), "--not-after="+late); !errors.Is(err, sidecar.ErrComputePolicyBuildRefused) {
		t.Errorf("not_after past 90 days: %v", err)
	}
	if testComputePolicyExists(lower) {
		t.Error("a refused build was written")
	}
	if _, err := world.run("build", filepath.Join(world.tree, "1.json"), "--changes="+world.firstChanges(t)); err == nil {
		t.Error("build wrote over a file that exists")
	}
}

// staging signs the week's staging policy under ur-compute-policy-staging by the staging key, expiring 7 days after
// its signing: a staging build's verifier takes it and a production build's refuses it, its release's copy is
// compute-policy-staging.json, the next week's is built on it, and the compute-policy key's certificate is refused.
func TestComputePolicyStagingSignsUnderTheStagingNamespace(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	out := world.must(t, "staging", world.stagingTree, "--key="+world.stagingCertificate, "--changes="+world.firstChanges(t), "--release="+world.release)
	if !strings.Contains(out, "built and signed staging version 1") || !strings.Contains(out, sidecar.ComputePolicyStagingNamespace) {
		t.Errorf("staging printed:\n%s", out)
	}
	published := filepath.Join(world.stagingTree, "1.json")
	out = world.must(t, "verify", published, "--staging")
	if !strings.Contains(out, "7.0 days from now") {
		t.Errorf("verify --staging printed:\n%s", out)
	}
	if _, err := world.run("verify", published); !errors.Is(err, sidecar.ErrComputePolicyRefused) {
		t.Errorf("a production build's verifier took the staging policy: %v", err)
	}
	if !bytes.Equal(testComputePolicyRead(t, filepath.Join(world.release, "compute-policy-staging.json")), testComputePolicyRead(t, published)) ||
		testComputePolicyExists(filepath.Join(world.release, "compute-policy.json")) {
		t.Error("the staging copy is not compute-policy-staging.json alone")
	}
	world.clock.AdvanceTo(testComputePolicySitting.Add(7 * 24 * time.Hour))
	world.must(t, "staging", world.stagingTree, "--key="+world.stagingCertificate, "--release="+world.release)
	builder := newComputePolicyCommandBuilder(world.settings, true)
	second, err := readComputePolicyVerified(builder, filepath.Join(world.stagingTree, "2.json"), world.settings)
	if err != nil || second.Provider("teetest") == nil || !second.NotAfter().Equal(testComputePolicySitting.Add(14*24*time.Hour)) {
		t.Fatalf("the next week's: %v, %v", second, err)
	}
	if _, err := world.run("staging", world.stagingTree, "--key="+world.policyCertificate); !errors.Is(err, sidecar.ErrComputePolicyBuildRefused) {
		t.Errorf("the compute-policy key for staging: %v", err)
	}
}

// status prints the days to the newest policy's not_after and to the newest ticket's horizon: none paging the day
// after a sitting, the horizon paging within 7 days once a sitting is missed, and status failing once it has passed.
func TestComputePolicyStatusReportsTheHorizon(t *testing.T) {
	world := newTestComputePolicyWorld(t)
	world.publishFirst(t)
	world.must(t, "tickets sign", "--store="+world.store, "--key="+world.policyCertificate, "--policy=1")
	world.clock.AdvanceTo(time.Date(2026, time.December, 2, 0, 0, 0, 0, time.UTC))
	world.must(t, "tickets release", "--store="+world.store, "--tree="+world.tree)
	out := world.must(t, "status", world.tree, "--store="+world.store)
	if !strings.Contains(out, "policy: version 1") || !strings.Contains(out, "89.4 days left") ||
		!strings.Contains(out, "released: the ticket from 2026-12-02T00:00:00Z") || !strings.Contains(out, "13.4 days left") ||
		!strings.Contains(out, "store: 13 unreleased tickets") || strings.Contains(out, "page:") {
		t.Errorf("the day after the sitting:\n%s", out)
	}
	world.clock.AdvanceTo(testComputePolicySitting.Add(8 * 24 * time.Hour))
	out = world.must(t, "status", world.tree, "--store="+world.store)
	if !strings.Contains(out, "6.0 days left") || !strings.Contains(out, "page: the newest signed ticket") {
		t.Errorf("a missed sitting:\n%s", out)
	}
	world.clock.AdvanceTo(testComputePolicySitting.Add(14 * 24 * time.Hour))
	out, err := world.run("status", world.tree, "--store="+world.store)
	if err == nil || !strings.Contains(out, "page: the newest signed ticket") {
		t.Errorf("past the horizon: %v\n%s", err, out)
	}
}

// The real ssh-keygen signs with a software key file standing in for a holder's token, named by its certificate,
// which it finds the private half of itself; what it answers verifies as a device verifies it. Skipped where no
// ssh-keygen is installed.
func TestComputePolicySignWithSshKeygen(t *testing.T) {
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen on PATH")
	}
	// the holder's agent, if one runs, holds no key of this test's
	t.Setenv("SSH_AUTH_SOCK", "")
	world := newTestComputePolicyWorld(t)
	world.settings.sign = computePolicySshKeygenSign
	world.settings.sshKeygen = sshKeygen
	private, holder := testComputePolicyKey(t, "holder")
	block, err := ssh.MarshalPrivateKey(private, "sidecarctl test holder")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(world.directory, "keys", "holder")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	validAfter := testComputePolicySitting.Add(-30 * 24 * time.Hour)
	certified := testComputePolicyCertify(t, holder, world.releaseKey, sidecar.ComputePolicyNamespace, validAfter, validAfter.Add(364*24*time.Hour))
	certificatePath := world.writeCertificate(t, "holder", certified)
	path := filepath.Join(world.directory, "work-1.json")
	world.must(t, "build", path, "--changes="+world.firstChanges(t))
	world.must(t, "sign", path, "--key="+certificatePath)
	builder := newComputePolicyCommandBuilder(world.settings, false)
	policy, err := readComputePolicyVerified(builder, path, world.settings)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Signer().Fingerprint != ssh.FingerprintSHA256(holder.PublicKey()) {
		t.Errorf("signed by %+v", policy.Signer())
	}
}
