// Tests for `sidecarctl dns ceremony prepare|assemble|verify` on files in a scratch directory: a quarter's round
// trip with a software key standing in for the HSM, and each refusal leaving nothing written. The stand-in KSK
// lives in this test file only, so no KSK-shaped code ships (DNS-M5-19). What a plan and a window file must hold is
// dnsservice's (ceremony_test.go, key_window_test.go).

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"
)

// The root the ceremony tests sign for.
const dnsCeremonyTestRoot = "ceremony.example."

// The time assemble is judged at: before the quarter the tests prepare.
var dnsCeremonyTestNow = time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)

// A ceremony's files in a scratch directory, and the stand-in for the HSM.
type dnsCeremonyTest struct {
	directory string
	// the software KSK, which signs as the HSM would
	kskKey *ecdsa.PrivateKey
	// its DNSKEY RDATA
	ksk string
}

// A fresh P-256 key and its DNSKEY RDATA with flags.
func newDnsCeremonyTestKey(t testing.TB, flags uint16) (*ecdsa.PrivateKey, string) {
	t.Helper()
	record := &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: dnsCeremonyTestRoot, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600},
		Flags:     flags,
		Protocol:  3,
		Algorithm: dns.ECDSAP256SHA256,
	}
	for {
		private, err := record.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		// miekg/dns signs under no key whose tag is 0, and neither does the ceremony
		if record.KeyTag() != 0 {
			return private.(*ecdsa.PrivateKey), strings.TrimSpace(strings.TrimPrefix(record.String(), record.Header().String()))
		}
	}
}

// dns.yml for the test root with anchor as its only trust anchor.
func dnsCeremonyTestYaml(anchor string) string {
	return "roots: [ceremony.example]\nns: [ns1, ns2]\nsignatures_per_second: 500\nrrl: {responses_per_second: 20}\ntcp: {connections_per_source: 8}\n" +
		"trust_anchors:\n  ceremony.example: ['" + anchor + "']\n"
}

// A scratch directory holding dns.yml anchored at a fresh KSK, the KSK's DNSKEY and three ZSKs' DNSKEYs.
func newDnsCeremonyTest(t testing.TB) *dnsCeremonyTest {
	t.Helper()
	self := &dnsCeremonyTest{directory: t.TempDir()}
	self.kskKey, self.ksk = newDnsCeremonyTestKey(t, dns.ZONE|dns.SEP)
	self.write(t, "dns.yml", []byte(dnsCeremonyTestYaml(self.ksk)))
	self.write(t, "ksk.dnskey", []byte(self.ksk+"\n"))
	for _, name := range []string{"zsk-current.dnskey", "zsk-next.dnskey", "zsk-standby.dnskey"} {
		_, dnskey, err := dnsservice.GenerateDnsZsk()
		if err != nil {
			t.Fatal(err)
		}
		self.write(t, name, []byte("\n"+dnskey+"\n\n"))
	}
	return self
}

// A file's path in the scratch directory.
func (self *dnsCeremonyTest) path(name string) string {
	return filepath.Join(self.directory, name)
}

// Writes a file in the scratch directory.
func (self *dnsCeremonyTest) write(t testing.TB, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(self.path(name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Whether the scratch directory holds the file.
func (self *dnsCeremonyTest) exists(name string) bool {
	_, err := os.Stat(self.path(name))
	return err == nil
}

// Runs one ceremony command with the clock at dnsCeremonyTestNow, or at now when given, and returns its exit code
// and output.
func (self *dnsCeremonyTest) run(settings *dnsCeremonySettings, args ...string) (int, string, string) {
	if settings == nil {
		settings = defaultDnsCeremonySettings()
		settings.now = func() time.Time { return dnsCeremonyTestNow }
	}
	commands := newSidecarctlCommandSet()
	for words, run := range map[string]func([]string, *dnsCeremonySettings, io.Writer) error{
		"dns ceremony prepare":  runDnsCeremonyPrepareWithSettings,
		"dns ceremony assemble": runDnsCeremonyAssembleWithSettings,
		"dns ceremony verify":   runDnsCeremonyVerifyWithSettings,
	} {
		commands.add(&sidecarctlCommand{
			words: words, usage: "<files>", summary: "the command under test",
			run: func(args []string, out io.Writer) error {
				return run(args, settings, out)
			},
		})
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := commands.run(append([]string{"dns", "ceremony"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// prepare's arguments for the first quarter of 2027, writing planName.
func (self *dnsCeremonyTest) prepareArgs(planName string) []string {
	return []string{
		"prepare", "--dns-yml=" + self.path("dns.yml"), "--ksk=" + self.path("ksk.dnskey"),
		"--zsk-current=" + self.path("zsk-current.dnskey"), "--zsk-next=" + self.path("zsk-next.dnskey"), "--zsk-standby=" + self.path("zsk-standby.dnskey"),
		"--from=2027-01-01T00:00:00Z", "--until=2027-04-01T00:00:00Z", "--out=" + self.path(planName),
	}
}

// Prepares the quarter's plan as plan.json and returns it.
func (self *dnsCeremonyTest) prepare(t testing.TB) *dnsservice.DnsCeremonyPlan {
	t.Helper()
	if code, _, stderr := self.run(nil, self.prepareArgs("plan.json")...); code != 0 {
		t.Fatalf("prepare exited %d: %s", code, stderr)
	}
	data, err := os.ReadFile(self.path("plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := dnsservice.ParseDnsCeremonyPlan(data)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// Signs each window of plan with key as the HSM would, alternating RFC 6605's r||s and DER, into one file a
// window, and returns the files' paths in the plan's order.
func (self *dnsCeremonyTest) sign(t testing.TB, plan *dnsservice.DnsCeremonyPlan, key *ecdsa.PrivateKey) []string {
	t.Helper()
	paths := []string{}
	for index, window := range plan.Windows {
		digest := sha256.Sum256(window.SigningInput)
		signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		if index%2 == 0 {
			var value struct {
				R *big.Int
				S *big.Int
			}
			if _, err := asn1.Unmarshal(signature, &value); err != nil {
				t.Fatal(err)
			}
			signature = make([]byte, 64)
			value.R.FillBytes(signature[:32])
			value.S.FillBytes(signature[32:])
		}
		name := fmt.Sprintf("window-%02d.sig", index)
		self.write(t, name, signature)
		paths = append(paths, self.path(name))
	}
	return paths
}

// assemble's arguments over plan.json, writing outName.
func (self *dnsCeremonyTest) assembleArgs(outName string, signaturePaths []string) []string {
	return append([]string{"assemble", "--dns-yml=" + self.path("dns.yml"), "--plan=" + self.path("plan.json"), "--out=" + self.path(outName)}, signaturePaths...)
}

// A quarter round trip: prepare writes a plan whose digests it prints, the stand-in HSM signs each window, assemble
// writes a window file that verifies against dns.yml's anchor with no gap, and verify rereads it.
func TestDnsCeremonyRoundTripsAQuarter(t *testing.T) {
	ceremony := newDnsCeremonyTest(t)
	code, stdout, stderr := ceremony.run(nil, ceremony.prepareArgs("plan.json")...)
	if code != 0 {
		t.Fatalf("prepare exited %d: %s", code, stderr)
	}
	planData, err := os.ReadFile(ceremony.path("plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := dnsservice.ParseDnsCeremonyPlan(planData)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Root != dnsCeremonyTestRoot || len(plan.Windows) < 10 || plan.Ksk != ceremony.ksk {
		t.Fatalf("the plan is for %s with %d windows under %q", plan.Root, len(plan.Windows), plan.Ksk)
	}
	for index, window := range plan.Windows {
		digest := sha256.Sum256(window.SigningInput)
		if want := fmt.Sprintf("window %d: %s to %s, signing input sha256 %x\n", index, window.NotBefore.UTC().Format(time.RFC3339), window.NotAfter.UTC().Format(time.RFC3339), digest); !strings.Contains(stdout, want) {
			t.Errorf("prepare printed %q, want %q in it", stdout, want)
		}
	}
	if info, err := os.Stat(ceremony.path("plan.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the plan's mode is %v, %v, want its owner's alone", info.Mode(), err)
	}

	signaturePaths := ceremony.sign(t, plan, ceremony.kskKey)
	code, stdout, stderr = ceremony.run(nil, ceremony.assembleArgs("windows.json", signaturePaths)...)
	if code != 0 {
		t.Fatalf("assemble exited %d: %s", code, stderr)
	}
	for _, want := range []string{
		fmt.Sprintf("%d windows, %d of them from this plan", len(plan.Windows), len(plan.Windows)),
		"gaps: none",
		"it goes to vault as dns-key-windows-ceremony.example.json",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("assemble printed %q, want %q in it", stdout, want)
		}
	}
	windowData, err := os.ReadFile(ceremony.path("windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := dnsservice.ParseDnsKeyWindowFile(windowData)
	if err != nil {
		t.Fatal(err)
	}
	dnsSettings, err := dnsservice.LoadDnsSettings([]byte(dnsCeremonyTestYaml(ceremony.ksk)))
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := dnsservice.VerifyDnsKeyWindowFile(dnsSettings, file)
	if err != nil || len(coverage.Gaps) != 0 || len(file.Windows) != len(plan.Windows) {
		t.Fatalf("the window file holds %d windows with coverage %+v: %v", len(file.Windows), coverage, err)
	}

	code, stdout, stderr = ceremony.run(nil, "verify", "--dns-yml="+ceremony.path("dns.yml"), ceremony.path("windows.json"))
	if code != 0 {
		t.Fatalf("verify exited %d: %s", code, stderr)
	}
	for _, want := range []string{
		fmt.Sprintf("verifies: %d windows for %s", len(file.Windows), dnsCeremonyTestRoot),
		"served from " + coverage.From.UTC().Format(time.RFC3339),
		"must be released by " + coverage.Until.UTC().Format(time.RFC3339),
		"gaps: none",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("verify printed %q, want %q in it", stdout, want)
		}
	}

	// the next quarter's ceremony keeps this file's windows that have not ended
	nextArgs := ceremony.prepareArgs("plan-next.json")
	for index, arg := range nextArgs {
		switch {
		case strings.HasPrefix(arg, "--from="):
			nextArgs[index] = "--from=" + plan.Windows[len(plan.Windows)-1].NotBefore.Add(6*24*time.Hour).UTC().Format(time.RFC3339)
		case strings.HasPrefix(arg, "--until="):
			nextArgs[index] = "--until=2027-07-01T00:00:00Z"
		}
	}
	if code, _, stderr := ceremony.run(nil, nextArgs...); code != 0 {
		t.Fatalf("the next prepare exited %d: %s", code, stderr)
	}
	nextData, err := os.ReadFile(ceremony.path("plan-next.json"))
	if err != nil {
		t.Fatal(err)
	}
	nextPlan, err := dnsservice.ParseDnsCeremonyPlan(nextData)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDnsCeremonySettings()
	settings.now = func() time.Time { return time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC) }
	nextSignaturePaths := ceremony.sign(t, nextPlan, ceremony.kskKey)
	nextAssemble := append([]string{"assemble", "--dns-yml=" + ceremony.path("dns.yml"), "--plan=" + ceremony.path("plan-next.json"), "--previous=" + ceremony.path("windows.json"), "--out=" + ceremony.path("windows-next.json")}, nextSignaturePaths...)
	code, stdout, stderr = ceremony.run(settings, nextAssemble...)
	if code != 0 {
		t.Fatalf("the next assemble exited %d: %s", code, stderr)
	}
	nextWindowData, err := os.ReadFile(ceremony.path("windows-next.json"))
	if err != nil {
		t.Fatal(err)
	}
	nextFile, err := dnsservice.ParseDnsKeyWindowFile(nextWindowData)
	if err != nil {
		t.Fatal(err)
	}
	if len(nextFile.Windows) <= len(nextPlan.Windows) || !strings.Contains(stdout, "gaps: none") {
		t.Errorf("the next file holds %d windows for a plan of %d, and assemble printed %q", len(nextFile.Windows), len(nextPlan.Windows), stdout)
	}
}

// assemble writes nothing for a signature that is not the KSK's, a signature under the wrong window, a KSK
// dns.yml does not anchor, a file read after its windows have passed, or a count of signatures that is not the
// plan's; and it never replaces a file that exists.
func TestDnsCeremonyAssembleWritesNothingItCannotVerify(t *testing.T) {
	ceremony := newDnsCeremonyTest(t)
	plan := ceremony.prepare(t)
	signaturePaths := ceremony.sign(t, plan, ceremony.kskKey)
	otherKey, otherKsk := newDnsCeremonyTestKey(t, dns.ZONE|dns.SEP)
	otherDirectory := t.TempDir()
	otherSignaturePaths := []string{}
	for index, window := range plan.Windows {
		digest := sha256.Sum256(window.SigningInput)
		signature, err := ecdsa.SignASN1(rand.Reader, otherKey, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(otherDirectory, fmt.Sprintf("window-%02d.sig", index))
		if err := os.WriteFile(path, signature, 0o600); err != nil {
			t.Fatal(err)
		}
		otherSignaturePaths = append(otherSignaturePaths, path)
	}
	swappedPaths := append([]string{signaturePaths[1], signaturePaths[0]}, signaturePaths[2:]...)
	ceremony.write(t, "dns-other-anchor.yml", []byte(dnsCeremonyTestYaml(otherKsk)))
	ceremony.write(t, "garbage.sig", []byte("not a signature"))
	garbagePaths := append([]string{ceremony.path("garbage.sig")}, signaturePaths[1:]...)
	past := defaultDnsCeremonySettings()
	past.now = func() time.Time { return time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC) }

	cases := []struct {
		name     string
		settings *dnsCeremonySettings
		args     []string
		wantCode int
		want     string
	}{
		{name: "another key's signatures", args: ceremony.assembleArgs("refused.json", otherSignaturePaths), wantCode: 1, want: "no window file was written"},
		{name: "two windows' signatures swapped", args: ceremony.assembleArgs("refused.json", swappedPaths), wantCode: 1, want: "no window file was written"},
		{name: "a file that is no signature", args: ceremony.assembleArgs("refused.json", garbagePaths), wantCode: 1, want: "no window file was written"},
		{name: "a KSK dns.yml does not anchor", args: append([]string{"assemble", "--dns-yml=" + ceremony.path("dns-other-anchor.yml"), "--plan=" + ceremony.path("plan.json"), "--out=" + ceremony.path("refused.json")}, signaturePaths...), wantCode: 1, want: "no window file was written"},
		{name: "windows that have passed", settings: past, args: ceremony.assembleArgs("refused.json", signaturePaths), wantCode: 1, want: "no window file was written"},
		{name: "one signature short", args: ceremony.assembleArgs("refused.json", signaturePaths[1:]), wantCode: 2, want: "want one a window in the plan's order"},
		{name: "no signatures", args: ceremony.assembleArgs("refused.json", nil), wantCode: 2, want: "want one a window in the plan's order"},
		{name: "a signature file that is not there", args: ceremony.assembleArgs("refused.json", append([]string{ceremony.path("absent.sig")}, signaturePaths[1:]...)), wantCode: 1, want: "window 0's signature"},
		{name: "no plan", args: append([]string{"assemble", "--dns-yml=" + ceremony.path("dns.yml"), "--out=" + ceremony.path("refused.json")}, signaturePaths...), wantCode: 2, want: "--plan names no file"},
		{name: "no out", args: append([]string{"assemble", "--dns-yml=" + ceremony.path("dns.yml"), "--plan=" + ceremony.path("plan.json")}, signaturePaths...), wantCode: 2, want: "--out names the window file"},
		{name: "a previous file that does not read", args: append([]string{"assemble", "--dns-yml=" + ceremony.path("dns.yml"), "--plan=" + ceremony.path("plan.json"), "--previous=" + ceremony.path("plan.json"), "--out=" + ceremony.path("refused.json")}, signaturePaths...), wantCode: 1, want: "--previous"},
	}
	for _, c := range cases {
		code, stdout, stderr := ceremony.run(c.settings, c.args...)
		if code != c.wantCode || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exited %d with %q, want %d and %q", c.name, code, stderr, c.wantCode, c.want)
		}
		if stdout != "" || ceremony.exists("refused.json") {
			t.Errorf("%s: printed %q, and a window file exists: %t", c.name, stdout, ceremony.exists("refused.json"))
		}
	}

	ceremony.write(t, "windows.json", []byte("what vault holds\n"))
	code, stdout, stderr := ceremony.run(nil, ceremony.assembleArgs("windows.json", signaturePaths)...)
	if code != 1 || !strings.Contains(stderr, "the window file was not written") || stdout != "" {
		t.Errorf("over a file that exists: exited %d with %q and printed %q", code, stderr, stdout)
	}
	if data, err := os.ReadFile(ceremony.path("windows.json")); err != nil || string(data) != "what vault holds\n" {
		t.Errorf("the file that existed now holds %q, %v", data, err)
	}
}

// prepare writes no plan for a command line it cannot take, a key file that is not one DNSKEY, a key that is not
// what its flag says, or an input over the bound, and never replaces a plan that exists.
func TestDnsCeremonyPrepareWritesNoPlanItCannotMake(t *testing.T) {
	ceremony := newDnsCeremonyTest(t)
	ceremony.write(t, "two.dnskey", []byte(ceremony.ksk+"\n"+ceremony.ksk+"\n"))
	ceremony.write(t, "empty.dnskey", []byte("\n\n"))
	replace := func(prefix string, value string) []string {
		args := ceremony.prepareArgs("refused.json")
		for index, arg := range args {
			if strings.HasPrefix(arg, prefix) {
				args[index] = prefix + value
			}
		}
		return args
	}
	drop := func(prefix string) []string {
		args := []string{}
		for _, arg := range ceremony.prepareArgs("refused.json") {
			if !strings.HasPrefix(arg, prefix) {
				args = append(args, arg)
			}
		}
		return args
	}
	small := defaultDnsCeremonySettings()
	small.inputMaxByteCount = 16

	cases := []struct {
		name     string
		settings *dnsCeremonySettings
		args     []string
		wantCode int
		want     string
	}{
		{name: "no --from", args: drop("--from="), wantCode: 2, want: "--from=\"\" is not an RFC 3339 time"},
		{name: "a date for --until", args: replace("--until=", "2027-04-01"), wantCode: 2, want: "is not an RFC 3339 time"},
		{name: "no --out", args: drop("--out="), wantCode: 2, want: "--out names the plan file"},
		{name: "no --ksk", args: drop("--ksk="), wantCode: 2, want: "--ksk names no file"},
		{name: "no --zsk-standby", args: drop("--zsk-standby="), wantCode: 2, want: "--zsk-standby names no file"},
		{name: "no --dns-yml", args: drop("--dns-yml="), wantCode: 2, want: "--dns-yml names no file"},
		{name: "a stray word", args: append(ceremony.prepareArgs("refused.json"), "now"), wantCode: 2, want: "takes flags only"},
		{name: "a root dns.yml does not serve", args: append(ceremony.prepareArgs("refused.json"), "--root=elsewhere.example"), wantCode: 2, want: "is not a root dns.yml serves"},
		{name: "two keys in a file", args: replace("--ksk=", ceremony.path("two.dnskey")), wantCode: 1, want: "holds 2 lines, want one DNSKEY RDATA"},
		{name: "no key in a file", args: replace("--zsk-next=", ceremony.path("empty.dnskey")), wantCode: 1, want: "holds 0 lines, want one DNSKEY RDATA"},
		{name: "a ZSK given as the KSK", args: replace("--ksk=", ceremony.path("zsk-current.dnskey")), wantCode: 1, want: "no plan was written"},
		{name: "the KSK given as a ZSK", args: replace("--zsk-current=", ceremony.path("ksk.dnskey")), wantCode: 1, want: "no plan was written"},
		{name: "a span that ends before it starts", args: replace("--until=", "2026-12-01T00:00:00Z"), wantCode: 1, want: "no plan was written"},
		{name: "a dns.yml that does not parse", args: replace("--dns-yml=", ceremony.path("ksk.dnskey")), wantCode: 1, want: "--dns-yml"},
		{name: "an input over the bound", settings: small, args: ceremony.prepareArgs("refused.json"), wantCode: 1, want: "holds more than 16 bytes"},
	}
	for _, c := range cases {
		code, stdout, stderr := ceremony.run(c.settings, c.args...)
		if code != c.wantCode || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exited %d with %q, want %d and %q", c.name, code, stderr, c.wantCode, c.want)
		}
		if stdout != "" || ceremony.exists("refused.json") {
			t.Errorf("%s: printed %q, and a plan exists: %t", c.name, stdout, ceremony.exists("refused.json"))
		}
	}

	ceremony.write(t, "plan.json", []byte("what the HSM signed\n"))
	code, stdout, stderr := ceremony.run(nil, ceremony.prepareArgs("plan.json")...)
	if code != 1 || !strings.Contains(stderr, "the plan was not written") || stdout != "" {
		t.Errorf("over a plan that exists: exited %d with %q and printed %q", code, stderr, stdout)
	}
	if data, err := os.ReadFile(ceremony.path("plan.json")); err != nil || string(data) != "what the HSM signed\n" {
		t.Errorf("the plan that existed now holds %q, %v", data, err)
	}
}

// verify exits 1 and prints nothing for a file whose signatures another KSK made, one that was changed after it
// was assembled, and one that is no window file.
func TestDnsCeremonyVerifyRefusesAFileItsAnchorsDidNotSign(t *testing.T) {
	ceremony := newDnsCeremonyTest(t)
	plan := ceremony.prepare(t)
	if code, _, stderr := ceremony.run(nil, ceremony.assembleArgs("windows.json", ceremony.sign(t, plan, ceremony.kskKey))...); code != 0 {
		t.Fatalf("assemble exited %d: %s", code, stderr)
	}
	_, otherKsk := newDnsCeremonyTestKey(t, dns.ZONE|dns.SEP)
	ceremony.write(t, "dns-other-anchor.yml", []byte(dnsCeremonyTestYaml(otherKsk)))
	windowData, err := os.ReadFile(ceremony.path("windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := dnsservice.ParseDnsKeyWindowFile(windowData)
	if err != nil {
		t.Fatal(err)
	}
	// a window whose validity was stretched after signing
	file.Windows[len(file.Windows)-1].NotAfter = file.Windows[len(file.Windows)-1].NotAfter.Add(24 * time.Hour)
	stretched, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	ceremony.write(t, "windows-stretched.json", stretched)

	cases := []struct {
		name     string
		args     []string
		wantCode int
		want     string
	}{
		{name: "another anchor", args: []string{"verify", "--dns-yml=" + ceremony.path("dns-other-anchor.yml"), ceremony.path("windows.json")}, wantCode: 1, want: "does not verify"},
		{name: "a stretched window", args: []string{"verify", "--dns-yml=" + ceremony.path("dns.yml"), ceremony.path("windows-stretched.json")}, wantCode: 1, want: "does not verify"},
		{name: "a plan given as a window file", args: []string{"verify", "--dns-yml=" + ceremony.path("dns.yml"), ceremony.path("plan.json")}, wantCode: 1, want: "does not verify"},
		{name: "no file", args: []string{"verify", "--dns-yml=" + ceremony.path("dns.yml")}, wantCode: 2, want: "want one window file"},
		{name: "two files", args: []string{"verify", "--dns-yml=" + ceremony.path("dns.yml"), ceremony.path("windows.json"), ceremony.path("windows.json")}, wantCode: 2, want: "want one window file"},
		{name: "no dns.yml", args: []string{"verify", ceremony.path("windows.json")}, wantCode: 2, want: "--dns-yml names no file"},
		{name: "a file that is not there", args: []string{"verify", "--dns-yml=" + ceremony.path("dns.yml"), ceremony.path("absent.json")}, wantCode: 1, want: "the window file"},
	}
	for _, c := range cases {
		code, stdout, stderr := ceremony.run(nil, c.args...)
		if code != c.wantCode || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exited %d with %q, want %d and %q", c.name, code, stderr, c.wantCode, c.want)
		}
		if stdout != "" {
			t.Errorf("%s: printed %q", c.name, stdout)
		}
	}
}

// The binary's own registry holds the three commands, each under its words.
func TestSidecarctlRegistersDnsCeremony(t *testing.T) {
	for _, words := range []string{"dns ceremony prepare", "dns ceremony assemble", "dns ceremony verify"} {
		command, args := sidecarctlCommands.find(append(strings.Fields(words), "--dns-yml=dns.yml"))
		if command == nil || command.words != words || len(args) != 1 {
			t.Errorf("%s chose %v with %v", words, command, args)
		}
	}
}
