// Tests for `sidecarctl abuse mailbox-keygen` on files in a scratch directory, run through the binary's own command
// line as the owner runs it: what it writes reads back through controller's parsers, a record sealed to the public
// key opens with the key the private file parses to, both files are 0600, a file that exists is never written over,
// a refused run leaves nothing behind, and neither key reaches stdout or stderr. The parsers' own refusals are
// controller's (abuse_mailbox_seal_test.go). Every key here is drawn by the test run and discarded with its scratch
// directory.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Runs a command line of the binary's own registry, as the owner would, and returns its exit code and output.
func runAbuseMailboxKeygenTest(args ...string) (int, string, string) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := sidecarctlCommands.run(append([]string{"abuse", "mailbox-keygen"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// Runs the command under settings, through a command set of its own, for the tests that replace the random source.
func runAbuseMailboxKeygenTestWithSettings(settings *abuseMailboxKeygenSettings, args ...string) (int, string, string) {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "abuse mailbox-keygen",
		usage:   "<files>",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runAbuseMailboxKeygenWithSettings(args, settings, out)
		},
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := commands.run(append([]string{"abuse", "mailbox-keygen"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// A fresh scratch directory and the two paths a run writes in it.
func newAbuseMailboxKeygenTestPaths(t *testing.T) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	return directory, filepath.Join(directory, "abuse-mailbox.yml"), filepath.Join(directory, "abuse-mailbox-private.yml")
}

// The names in directory, which a refused run must leave as it found them.
func abuseMailboxKeygenTestNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// The keys a run wrote, read back through controller's parsers.
func readAbuseMailboxKeygenTestKeys(t *testing.T, outPath string, privateOutPath string) (*controller.AbuseMailboxKeys, []byte) {
	t.Helper()
	keyFile, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := controller.ParseAbuseMailboxKeys(keyFile, controller.DefaultAbuseMailboxSettings())
	if err != nil {
		t.Fatalf("abuse-mailbox.yml does not parse: %v", err)
	}
	privateKeyFile, err := os.ReadFile(privateOutPath)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := controller.ParseAbuseMailboxSealPrivateKey(privateKeyFile)
	if err != nil {
		t.Fatalf("the private file does not parse: %v", err)
	}
	seed, err := privateKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return keys, seed
}

// What abuse-mailbox.yml holds passes controller.ParseAbuseMailboxKeys, the private file is QD-832's one line, and a
// record sealed to the public key, mailed and decoded as the mailer and the restore do, opens with the key the
// private file parses to, its MAC verifying under the written MAC key.
func TestAbuseMailboxKeygenWritesFilesThatReadBack(t *testing.T) {
	_, outPath, privateOutPath := newAbuseMailboxKeygenTestPaths(t)
	code, stdout, stderr := runAbuseMailboxKeygenTest("--out="+outPath, "--private-out="+privateOutPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"wrote " + outPath, "wrote " + privateOutPath, "offline beside §19.10's restore runbook", "rotation"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q: %s", want, stdout)
		}
	}

	keyFile, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	settings := controller.DefaultAbuseMailboxSettings()
	keys, err := controller.ParseAbuseMailboxKeys(keyFile, settings)
	if err != nil {
		t.Fatalf("abuse-mailbox.yml does not parse: %v", err)
	}
	if !bytes.Equal(keyFile, controller.AbuseMailboxKeyFile(keys)) {
		t.Fatal("abuse-mailbox.yml is not AbuseMailboxKeyFile's bytes for its keys")
	}
	privateKeyFile, err := os.ReadFile(privateOutPath)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^abuse_mailbox_seal_private_key: [A-Za-z0-9+/]{43}=\n$`).Match(privateKeyFile) {
		t.Fatalf("the private file is not one line holding a 32-byte seed: %d bytes", len(privateKeyFile))
	}
	privateKey, err := controller.ParseAbuseMailboxSealPrivateKey(privateKeyFile)
	if err != nil {
		t.Fatalf("the private file does not parse: %v", err)
	}
	if !bytes.Equal(privateKey.PublicKey().Bytes(), keys.SealPublicKey.Bytes()) {
		t.Fatal("the private file's key is not the pair of abuse-mailbox.yml's public key")
	}

	networkId := server.NewId()
	until := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	record := &controller.AbuseRecord{
		ActionId:  server.NewId(),
		CaseId:    server.NewId(),
		Category:  model.AbuseCategoryPhishing,
		NetworkId: &networkId,
		Kind:      model.AbuseActionKindHostKill,
		Target:    "login.keygen.example",
		Until:     &until,
		Time:      time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
	}
	plaintext, err := controller.EncodeAbuseRecord(record, keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := controller.SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := controller.DecodeAbuseMailboxBody(controller.EncodeAbuseMailboxBody(sealed, settings), settings)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := controller.OpenAbuseMailboxRecord(privateKey, decoded, settings)
	if err != nil {
		t.Fatalf("a record sealed to the written public key does not open with the written private key: %v", err)
	}
	parsed, err := controller.ParseAbuseRecord(opened, keys.MacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ActionId != record.ActionId || parsed.Target != record.Target || !parsed.Time.Equal(record.Time) {
		t.Fatalf("the record opens as %+v", parsed)
	}
}

// Both files are created 0600: their owner alone reads them.
func TestAbuseMailboxKeygenFilesAreOwnerOnly(t *testing.T) {
	_, outPath, privateOutPath := newAbuseMailboxKeygenTestPaths(t)
	if code, _, stderr := runAbuseMailboxKeygenTest("--out="+outPath, "--private-out="+privateOutPath); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, path := range []string{outPath, privateOutPath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want a regular file of 0600", filepath.Base(path), info.Mode())
		}
	}
}

// Nothing the run prints, on stdout or stderr, holds a key: not the MAC key, the public key or the seed, in base64,
// hex or raw, nor the start of one. A refused run's stderr is its one line, so it holds none either.
func TestAbuseMailboxKeygenPrintsNoKey(t *testing.T) {
	directory, outPath, privateOutPath := newAbuseMailboxKeygenTestPaths(t)
	code, stdout, stderr := runAbuseMailboxKeygenTest("--out="+outPath, "--private-out="+privateOutPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	keys, seed := readAbuseMailboxKeygenTestKeys(t, outPath, privateOutPath)
	for _, key := range []struct {
		name  string
		value []byte
	}{
		{name: "the MAC key", value: keys.MacKey},
		{name: "the public key", value: keys.SealPublicKey.Bytes()},
		{name: "the seed", value: seed},
	} {
		for _, form := range []string{
			base64.StdEncoding.EncodeToString(key.value),
			base64.StdEncoding.EncodeToString(key.value)[:16],
			base64.RawURLEncoding.EncodeToString(key.value)[:16],
			hex.EncodeToString(key.value)[:16],
			string(key.value),
		} {
			for _, output := range []struct {
				name string
				text string
			}{
				{name: "stdout", text: stdout},
				{name: "stderr", text: stderr},
			} {
				if strings.Contains(output.text, form) {
					t.Errorf("%s holds %s", output.name, key.name)
				}
			}
		}
	}

	// a second run onto the same files is refused with only its message
	code, stdout, stderr = runAbuseMailboxKeygenTest("--out="+filepath.Join(directory, "other.yml"), "--private-out="+privateOutPath)
	if code != 1 || stdout != "" {
		t.Fatalf("a refused run exited %d and printed %q", code, stdout)
	}
	want := "sidecarctl abuse mailbox-keygen: nothing was written: --private-out=" + privateOutPath + " exists, and this never writes over a file\n"
	if stderr != want {
		t.Fatalf("a refused run's stderr is %q, want %q", stderr, want)
	}
}

// A path that exists is never written over, whichever flag names it, a symlink included, even one to nowhere; the
// run creates nothing, so the other path stays absent and the existing file keeps its bytes and mode.
func TestAbuseMailboxKeygenNeverWritesOverAFile(t *testing.T) {
	for _, c := range []struct {
		name string
		// which of the two paths exists before the run: "out" or "private-out"
		existing string
		// whether it is a symlink, and to a file that exists
		symlink       bool
		symlinkTarget bool
	}{
		{name: "an abuse-mailbox.yml that exists", existing: "out"},
		{name: "a private file that exists", existing: "private-out"},
		{name: "a symlink to a file at --out", existing: "out", symlink: true, symlinkTarget: true},
		{name: "a symlink to nowhere at --private-out", existing: "private-out", symlink: true},
		{name: "a symlink to nowhere at --out", existing: "out", symlink: true},
	} {
		directory, outPath, privateOutPath := newAbuseMailboxKeygenTestPaths(t)
		existingPath, otherPath := outPath, privateOutPath
		if c.existing == "private-out" {
			existingPath, otherPath = privateOutPath, outPath
		}
		sentinel := []byte("an earlier file the run must not touch\n")
		targetPath := filepath.Join(directory, "target")
		if c.symlink {
			if c.symlinkTarget {
				if err := os.WriteFile(targetPath, sentinel, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(targetPath, existingPath); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(existingPath, sentinel, 0o644); err != nil {
			t.Fatal(err)
		}
		readPath := existingPath
		if c.symlink {
			readPath = targetPath
		}
		var beforeMode os.FileMode
		if info, err := os.Stat(readPath); err == nil {
			beforeMode = info.Mode()
		}
		before := abuseMailboxKeygenTestNames(t, directory)

		code, stdout, stderr := runAbuseMailboxKeygenTest("--out="+outPath, "--private-out="+privateOutPath)
		if code != 1 || !strings.Contains(stderr, "exists, and this never writes over a file") || stdout != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.name, code, stdout, stderr)
		}
		if after := abuseMailboxKeygenTestNames(t, directory); !slices.Equal(after, before) {
			t.Errorf("%s: the directory held %v and holds %v", c.name, before, after)
		}
		if _, err := os.Lstat(otherPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the other path was created: %v", c.name, err)
		}
		data, err := os.ReadFile(readPath)
		switch {
		case c.symlink && !c.symlinkTarget:
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: the symlink's target was created: %v", c.name, err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.name, err)
		case !bytes.Equal(data, sentinel):
			t.Errorf("%s: the existing file was written over", c.name)
		default:
			if info, err := os.Stat(readPath); err != nil || info.Mode() != beforeMode {
				t.Errorf("%s: the existing file's mode changed from %v: %v", c.name, beforeMode, err)
			}
		}
	}
}

// A file the run created is removed when the next cannot be: with --out in a directory that does not exist, the
// private file, created first, does not stay behind.
func TestAbuseMailboxKeygenLeavesNothingWhenACreateFails(t *testing.T) {
	directory, _, privateOutPath := newAbuseMailboxKeygenTestPaths(t)
	outPath := filepath.Join(directory, "missing", "abuse-mailbox.yml")
	code, stdout, stderr := runAbuseMailboxKeygenTest("--out="+outPath, "--private-out="+privateOutPath)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written: --out:") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if names := abuseMailboxKeygenTestNames(t, directory); len(names) != 0 {
		t.Fatalf("the run left %v behind", names)
	}
}

// A command line the command cannot take exits 2 with the usage and writes nothing: a flag missing or empty, one file
// named twice in any spelling, a positional, a value written apart from its flag, a value that looks like a flag and
// a flag the command does not have.
func TestAbuseMailboxKeygenRefusesCommandLines(t *testing.T) {
	directory := t.TempDir()
	// the relative paths below resolve in the scratch directory, never in the package's own
	t.Chdir(directory)
	for _, c := range []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "no flags", args: []string{}, wantStderr: "--out names no file"},
		{name: "no --private-out", args: []string{"--out=a.yml"}, wantStderr: "--private-out names no file"},
		{name: "no --out", args: []string{"--private-out=b.yml"}, wantStderr: "--out names no file"},
		{name: "an empty --out", args: []string{"--out=", "--private-out=b.yml"}, wantStderr: "--out names no file"},
		{name: "one file twice", args: []string{"--out=a.yml", "--private-out=a.yml"}, wantStderr: "name one file"},
		{name: "one file twice, spelled apart", args: []string{"--out=a.yml", "--private-out=./sub/../a.yml"}, wantStderr: "name one file"},
		{name: "one file twice, absolute and relative", args: []string{"--out=" + filepath.Join(directory, "a.yml"), "--private-out=a.yml"}, wantStderr: "name one file"},
		{name: "a positional", args: []string{"--out=a.yml", "--private-out=b.yml", "c.yml"}, wantStderr: "takes flags only"},
		{name: "values apart from their flags", args: []string{"-out", "a.yml", "-private-out", "b.yml"}, wantStderr: "takes flags only"},
		{name: "a value apart from its flag, last", args: []string{"--out=a.yml", "--private-out", "b.yml"}, wantStderr: "flag needs an argument"},
		// flag takes the next argument for -out's value, though it begins with -
		{name: "a flag taken for a value", args: []string{"-out", "-private-out=b.yml"}, wantStderr: "looks like a flag"},
		{name: "a value that looks like a flag", args: []string{"--out=-private-out", "--private-out=b.yml"}, wantStderr: "looks like a flag"},
		{name: "a flag the command lacks", args: []string{"--out=a.yml", "--private-out=b.yml", "--force"}, wantStderr: "flag provided but not defined"},
	} {
		code, stdout, stderr := runAbuseMailboxKeygenTest(c.args...)
		if code != 2 || stdout != "" {
			t.Errorf("%s: exit %d, stdout %q", c.name, code, stdout)
		}
		if !strings.Contains(stderr, c.wantStderr) || !strings.Contains(stderr, "sidecarctl abuse mailbox-keygen --out=<abuse-mailbox.yml> --private-out=<file>") {
			t.Errorf("%s: stderr %q, want %q and the usage", c.name, stderr, c.wantStderr)
		}
		if names := abuseMailboxKeygenTestNames(t, directory); len(names) != 0 {
			t.Fatalf("%s: the run wrote %v", c.name, names)
		}
	}
}

// A MAC key the check refuses is never written: from a random source of zeros, abuse-mailbox.yml would hold an
// all-zero MAC key, which ParseAbuseMailboxKeys refuses, so the run stops before either file exists.
func TestAbuseMailboxKeygenWritesNothingTheCheckRefuses(t *testing.T) {
	directory, outPath, privateOutPath := newAbuseMailboxKeygenTestPaths(t)
	settings := defaultAbuseMailboxKeygenSettings()
	settings.random = bytes.NewReader(make([]byte, 64))
	code, stdout, stderr := runAbuseMailboxKeygenTestWithSettings(settings, "--out="+outPath, "--private-out="+privateOutPath)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written") || !strings.Contains(stderr, "all zero") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if names := abuseMailboxKeygenTestNames(t, directory); len(names) != 0 {
		t.Fatalf("the run wrote %v", names)
	}
}

// A random source that fails gives no keys and no files.
func TestAbuseMailboxKeygenWritesNothingWithoutRandomBytes(t *testing.T) {
	directory, outPath, privateOutPath := newAbuseMailboxKeygenTestPaths(t)
	settings := defaultAbuseMailboxKeygenSettings()
	// fewer bytes than a MAC key
	settings.random = bytes.NewReader(make([]byte, 8))
	code, stdout, stderr := runAbuseMailboxKeygenTestWithSettings(settings, "--out="+outPath, "--private-out="+privateOutPath)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written: no MAC key was drawn") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if names := abuseMailboxKeygenTestNames(t, directory); len(names) != 0 {
		t.Fatalf("the run wrote %v", names)
	}
}

// Each run draws its own keys: two runs share no MAC key, public key or seed, as a rotation needs.
func TestAbuseMailboxKeygenDrawsFreshKeys(t *testing.T) {
	_, firstOutPath, firstPrivateOutPath := newAbuseMailboxKeygenTestPaths(t)
	_, secondOutPath, secondPrivateOutPath := newAbuseMailboxKeygenTestPaths(t)
	for _, paths := range [][]string{{firstOutPath, firstPrivateOutPath}, {secondOutPath, secondPrivateOutPath}} {
		if code, _, stderr := runAbuseMailboxKeygenTest("--out="+paths[0], "--private-out="+paths[1]); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	}
	firstKeys, firstSeed := readAbuseMailboxKeygenTestKeys(t, firstOutPath, firstPrivateOutPath)
	secondKeys, secondSeed := readAbuseMailboxKeygenTestKeys(t, secondOutPath, secondPrivateOutPath)
	if bytes.Equal(firstKeys.MacKey, secondKeys.MacKey) ||
		bytes.Equal(firstKeys.SealPublicKey.Bytes(), secondKeys.SealPublicKey.Bytes()) ||
		bytes.Equal(firstSeed, secondSeed) {
		t.Fatal("two runs share a key")
	}
}

// The binary's own registry holds the command under the usage its doc gives, and the binary's help says where the
// private file is kept, that a rotation is the same command, and that old files are kept until their records' ttl.
func TestSidecarctlRegistersAbuseMailboxKeygen(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"abuse", "mailbox-keygen", "--out=a.yml"})
	if command == nil || command.words != "abuse mailbox-keygen" {
		t.Fatalf("the binary has no abuse mailbox-keygen: %v", command)
	}
	if command.usage != "--out=<abuse-mailbox.yml> --private-out=<file>" || !slices.Equal(args, []string{"--out=a.yml"}) {
		t.Fatalf("abuse mailbox-keygen's usage is %q, with %v", command.usage, args)
	}
	stdout := &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"--help"}, stdout, io.Discard); code != 0 {
		t.Fatalf("--help exited %d", code)
	}
	for _, want := range []string{
		"sidecarctl abuse mailbox-keygen --out=<abuse-mailbox.yml> --private-out=<file>",
		"keeps offline beside §19.10's restore runbook and never puts in vault",
		"Both are created 0600, never over a file, and neither key is printed",
		"A rotation is the same command with new files",
		"until the last record sealed to them has passed its ttl",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the help lacks %q", want)
		}
	}
}
