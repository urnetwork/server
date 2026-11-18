// Tests for `sidecarctl sockets ticket-keygen` on files in a scratch directory, run through a command line as the
// owner runs it, at a time each test sets: a first file holds one key the sockets block signs with; a rotation keeps
// -in's live keys exactly and drops the expired, its expiry the boundary, in place too; the file is 0600; a path that
// exists is never written over unless -in names that file; a file -in names that the block would refuse, or a full
// one, refuses the run; a new file that does not read back is never written; and no seed or key reaches stdout or
// stderr. The key file's own rules are socketsservice's (ticket_keys_test.go). Every key here is drawn by the test
// run and discarded with its scratch directory.

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar/socketsservice"
)

// Where a test's runs start: a fixed date, so the times in the output repeat.
func socketsTicketKeygenTestStart() time.Time {
	return time.Date(2026, time.November, 17, 9, 0, 0, 0, time.UTC)
}

// Runs the command at now through a command set of its own, as the binary runs a command line, and returns its exit
// code and output.
func runSocketsTicketKeygenTest(now time.Time, args ...string) (int, string, string) {
	settings := defaultSocketsTicketKeygenSettings()
	settings.now = func() time.Time {
		return now
	}
	return runSocketsTicketKeygenTestWithSettings(settings, args...)
}

// Runs the command under settings.
func runSocketsTicketKeygenTestWithSettings(settings *socketsTicketKeygenSettings, args ...string) (int, string, string) {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "sockets ticket-keygen",
		usage:   "-out <file> [-in <file>]",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runSocketsTicketKeygenWithSettings(args, settings, out)
		},
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := commands.run(append([]string{"sockets", "ticket-keygen"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// Runs the command at now, failing the test unless it succeeds, and returns its stdout.
func requireSocketsTicketKeygenTest(t *testing.T, now time.Time, args ...string) string {
	t.Helper()
	code, stdout, stderr := runSocketsTicketKeygenTest(now, args...)
	if code != 0 || stderr != "" {
		t.Fatalf("%v exited %d: %s", args, code, stderr)
	}
	return stdout
}

// One key as the file holds it, its seed included, which only a test reads.
type socketsTicketKeygenTestKey struct {
	KeyId         string `yaml:"key_id"`
	Seed          string `yaml:"seed"`
	CreatedMillis int64  `yaml:"created_millis"`
	ExpireMillis  int64  `yaml:"expire_millis"`
}

// A key file as a test reads and plants it.
type socketsTicketKeygenTestFile struct {
	Keys []*socketsTicketKeygenTestKey `yaml:"keys"`
}

// The keys a file's bytes hold, in its order.
func parseSocketsTicketKeygenTestFile(t *testing.T, data []byte) []*socketsTicketKeygenTestKey {
	t.Helper()
	file := &socketsTicketKeygenTestFile{}
	if err := yaml.Unmarshal(data, file); err != nil {
		t.Fatalf("a key file does not parse: %v", err)
	}
	return file.Keys
}

// The keys the file at path holds, in its order.
func readSocketsTicketKeygenTestFile(t *testing.T, path string) []*socketsTicketKeygenTestKey {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return parseSocketsTicketKeygenTestFile(t, data)
}

// The bytes of a key file holding keys.
func socketsTicketKeygenTestFileBytes(t *testing.T, keys ...*socketsTicketKeygenTestKey) []byte {
	t.Helper()
	data, err := yaml.Marshal(&socketsTicketKeygenTestFile{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The sockets block's key set over the file at path, read at now.
func readSocketsTicketKeygenTestKeySet(t *testing.T, path string, now time.Time) *socketsservice.SocketTicketKeySet {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	read := func() ([]byte, error) {
		return os.ReadFile(path)
	}
	keySet, err := socketsservice.NewSocketTicketKeySet(ctx, read, &socketsTicketKeygenStoppedClock{now: now}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return keySet
}

// The ids of the keys a set verifies under, oldest first.
func socketsTicketKeygenTestVerifyKeyIds(keySet *socketsservice.SocketTicketKeySet) []string {
	keyIds := []string{}
	for _, key := range keySet.VerifyKeys() {
		keyIds = append(keyIds, key.KeyId)
	}
	return keyIds
}

// The ids of keys, in order.
func socketsTicketKeygenTestKeyIds(keys []*socketsTicketKeygenTestKey) []string {
	keyIds := []string{}
	for _, key := range keys {
		keyIds = append(keyIds, key.KeyId)
	}
	return keyIds
}

// The names in directory, sorted, which a refused run must leave as it found them.
func socketsTicketKeygenTestNames(t *testing.T, directory string) []string {
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

// A time as the output gives it.
func socketsTicketKeygenTestTimeText(millis int64) string {
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
}

// Through the binary's own registry at the real time, a first file holds one key, with a 32-byte seed and an expiry
// RotationInterval and KeyOverlap after its creation, which the sockets block verifies under and signs with at once;
// the output names the key and its times and where the file goes, and nothing is left beside the file.
func TestSocketsTicketKeygenWritesAFirstFile(t *testing.T) {
	directory := t.TempDir()
	outPath := filepath.Join(directory, "socket-ticket-keys.yml")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"sockets", "ticket-keygen", "-out", outPath}, stdout, stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	keys := readSocketsTicketKeygenTestFile(t, outPath)
	if len(keys) != 1 {
		t.Fatalf("a first file holds %d keys", len(keys))
	}
	key := keys[0]
	seed, err := base64.StdEncoding.DecodeString(key.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("the key's seed is no Ed25519 seed: %v", err)
	}
	settings := socketsservice.DefaultSocketTicketKeySettings()
	if lifetime := time.Duration(key.ExpireMillis-key.CreatedMillis) * time.Millisecond; lifetime != settings.RotationInterval+settings.KeyOverlap {
		t.Fatalf("the key verifies for %v", lifetime)
	}

	keySet := readSocketsTicketKeygenTestKeySet(t, outPath, time.UnixMilli(key.CreatedMillis))
	signingKeyId, privateKey, signs := keySet.SigningKey()
	if !signs || signingKeyId != key.KeyId || !bytes.Equal(privateKey.Seed(), seed) {
		t.Fatal("the block does not sign with a first file's key")
	}
	if keyIds := socketsTicketKeygenTestVerifyKeyIds(keySet); !slices.Equal(keyIds, []string{key.KeyId}) {
		t.Fatalf("the block verifies under %v", keyIds)
	}

	for _, want := range []string{
		"added ticket key " + key.KeyId + ": created " + socketsTicketKeygenTestTimeText(key.CreatedMillis) + ", it verifies until " + socketsTicketKeygenTestTimeText(key.ExpireMillis) + "\n",
		"the new key is the file's only one, so it signs as soon as a sockets block reads the file",
		"wrote " + outPath + ", readable by its owner alone",
		"place " + outPath + " in vault as socket-ticket-keys.yml: each sockets block rereads it every 1m0s and gives the new key to its listeners\n",
		"run this again by " + socketsTicketKeygenTestTimeText(key.CreatedMillis+settings.RotationInterval.Milliseconds()),
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q: %s", want, stdout)
		}
	}
	if strings.Contains(stdout.String(), "-in=") {
		t.Errorf("a first file's output speaks of -in: %s", stdout)
	}
	// a block settles a key from its own first sight of it (QD-1016), so the placement has no deadline
	if strings.Contains(stdout.String(), "well before") || strings.Contains(stdout.String(), "socket-ticket-keys.yml now") ||
		strings.Contains(stdout.String(), socketsTicketKeygenTestTimeText(key.CreatedMillis+settings.SettleTime.Milliseconds())) {
		t.Errorf("stdout gives a time to place the file by: %s", stdout)
	}
	if names := socketsTicketKeygenTestNames(t, directory); !slices.Equal(names, []string{"socket-ticket-keys.yml"}) {
		t.Fatalf("the run left %v", names)
	}
}

// A rotation keeps each key of -in that is live at the run's time exactly as it was (id, seed and times) and drops
// the expired, a key expiring at the run's millisecond counting as expired; the new key is the run's; the output
// lists the kept keys and never the dropped, and says the new key signs SettleTime after a block first sees it, with
// no time to place the file by; and a block first reading the new file verifies under every key in it and signs with
// the oldest that outlives SignMargin, the second key, since none has settled there.
func TestSocketsTicketKeygenKeepsLiveKeysAndDropsExpired(t *testing.T) {
	directory := t.TempDir()
	settings := socketsservice.DefaultSocketTicketKeySettings()
	start := socketsTicketKeygenTestStart()
	firstPath := filepath.Join(directory, "first.yml")
	secondPath := filepath.Join(directory, "second.yml")
	requireSocketsTicketKeygenTest(t, start, "-out", firstPath)
	first := readSocketsTicketKeygenTestFile(t, firstPath)

	// a rotation later the first key verifies on beside the second
	secondTime := start.Add(settings.RotationInterval)
	requireSocketsTicketKeygenTest(t, secondTime, "-in", firstPath, "-out", secondPath)
	second := readSocketsTicketKeygenTestFile(t, secondPath)
	if len(second) != 2 || *second[0] != *first[0] {
		t.Fatalf("the second file does not keep the first key as it was: %d keys", len(second))
	}
	if second[1].CreatedMillis != secondTime.UnixMilli() || second[1].ExpireMillis != secondTime.Add(settings.RotationInterval+settings.KeyOverlap).UnixMilli() {
		t.Fatal("the second key's times are not the run's")
	}

	firstExpireTime := time.UnixMilli(first[0].ExpireMillis)
	for _, c := range []struct {
		name     string
		at       time.Time
		wantKept []*socketsTicketKeygenTestKey
	}{
		{name: "a millisecond before the first key expires", at: firstExpireTime.Add(-time.Millisecond), wantKept: second},
		{name: "as the first key expires", at: firstExpireTime, wantKept: second[1:]},
	} {
		thirdPath := filepath.Join(directory, "third.yml")
		if err := os.Remove(thirdPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		stdout := requireSocketsTicketKeygenTest(t, c.at, "-in", secondPath, "-out", thirdPath)
		third := readSocketsTicketKeygenTestFile(t, thirdPath)
		if len(third) != len(c.wantKept)+1 {
			t.Fatalf("%s: the file holds %d keys, want %d", c.name, len(third), len(c.wantKept)+1)
		}
		for index, key := range c.wantKept {
			if *third[index] != *key {
				t.Errorf("%s: key %s is not kept as it was", c.name, key.KeyId)
			}
			if want := "kept ticket key " + key.KeyId + ", which verifies until " + socketsTicketKeygenTestTimeText(key.ExpireMillis); !strings.Contains(stdout, want) {
				t.Errorf("%s: stdout lacks %q", c.name, want)
			}
		}
		newKey := third[len(third)-1]
		if slices.Contains(socketsTicketKeygenTestKeyIds(second), newKey.KeyId) ||
			newKey.CreatedMillis != c.at.UnixMilli() || newKey.ExpireMillis != c.at.Add(settings.RotationInterval+settings.KeyOverlap).UnixMilli() {
			t.Errorf("%s: the new key is not the run's: %s", c.name, newKey.KeyId)
		}
		if want := fmt.Sprintf("kept the keys of -in=%s still live at %s, each as it was (%d in all); any expired were dropped", secondPath, socketsTicketKeygenTestTimeText(c.at.UnixMilli()), len(c.wantKept)); !strings.Contains(stdout, want) {
			t.Errorf("%s: stdout lacks %q: %s", c.name, want, stdout)
		}
		if !strings.Contains(stdout, "added ticket key "+newKey.KeyId) {
			t.Errorf("%s: stdout does not name the new key", c.name)
		}
		if len(c.wantKept) < len(second) && strings.Contains(stdout, first[0].KeyId) {
			t.Errorf("%s: stdout names the dropped key", c.name)
		}
		if strings.Contains(stdout, "no key of -in can sign") {
			t.Errorf("%s: stdout says no key of -in signs, though the second does", c.name)
		}
		if want := "each sockets block signs with the new key 10m0s after the first read in which it sees the file, and with a key of -in until then\n"; !strings.Contains(stdout, want) {
			t.Errorf("%s: stdout lacks %q: %s", c.name, want, stdout)
		}
		if strings.Contains(stdout, "well before") || strings.Contains(stdout, socketsTicketKeygenTestTimeText(c.at.Add(settings.SettleTime).UnixMilli())) {
			t.Errorf("%s: stdout gives a time to place the file by: %s", c.name, stdout)
		}

		keySet := readSocketsTicketKeygenTestKeySet(t, thirdPath, c.at)
		if keyIds := socketsTicketKeygenTestVerifyKeyIds(keySet); !slices.Equal(keyIds, socketsTicketKeygenTestKeyIds(third)) {
			t.Errorf("%s: the block verifies under %v", c.name, keyIds)
		}
		if signingKeyId, _, signs := keySet.SigningKey(); !signs || signingKeyId != second[1].KeyId {
			t.Errorf("%s: the block signs with %q, not the settled second key", c.name, signingKeyId)
		}
	}
}

// -in naming -out's file, in any spelling, rotates in place: the file at -out gains one key, keeps the others as
// they were, ends 0600 though it was readable by everyone, and nothing is left beside it.
func TestSocketsTicketKeygenRotatesInPlace(t *testing.T) {
	directory := t.TempDir()
	// the relative paths below resolve in the scratch directory, never in the package's own
	t.Chdir(directory)
	if err := os.Mkdir("sub", 0o700); err != nil {
		t.Fatal(err)
	}
	start := socketsTicketKeygenTestStart()
	requireSocketsTicketKeygenTest(t, start, "-out", "socket-ticket-keys.yml")
	for index, paths := range [][]string{
		{"socket-ticket-keys.yml", "socket-ticket-keys.yml"},
		{"./socket-ticket-keys.yml", "socket-ticket-keys.yml"},
		{"socket-ticket-keys.yml", "./socket-ticket-keys.yml"},
		{"sub/../socket-ticket-keys.yml", "socket-ticket-keys.yml"},
		{filepath.Join(directory, "socket-ticket-keys.yml"), "socket-ticket-keys.yml"},
	} {
		// a copy from elsewhere, readable by everyone
		if err := os.Chmod("socket-ticket-keys.yml", 0o644); err != nil {
			t.Fatal(err)
		}
		before := readSocketsTicketKeygenTestFile(t, "socket-ticket-keys.yml")
		at := start.Add(time.Duration(index+1) * time.Hour)
		requireSocketsTicketKeygenTest(t, at, "-in", paths[0], "-out", paths[1])
		after := readSocketsTicketKeygenTestFile(t, "socket-ticket-keys.yml")
		if len(after) != len(before)+1 || after[len(after)-1].CreatedMillis != at.UnixMilli() {
			t.Fatalf("-in=%s -out=%s: the file holds %d keys after %d", paths[0], paths[1], len(after), len(before))
		}
		for keyIndex, key := range before {
			if *after[keyIndex] != *key {
				t.Errorf("-in=%s -out=%s: key %s is not kept as it was", paths[0], paths[1], key.KeyId)
			}
		}
		info, err := os.Lstat("socket-ticket-keys.yml")
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("-in=%s -out=%s: the file is %v: %v", paths[0], paths[1], info.Mode(), err)
		}
		if names := socketsTicketKeygenTestNames(t, directory); !slices.Equal(names, []string{"socket-ticket-keys.yml", "sub"}) {
			t.Fatalf("-in=%s -out=%s: the directory holds %v", paths[0], paths[1], names)
		}
	}
}

// The file is a regular file of mode 0600, readable by its owner alone: a first file, one written from an -in that
// everyone may read, and a rotation in place over such a file.
func TestSocketsTicketKeygenFileIsOwnerOnly(t *testing.T) {
	directory := t.TempDir()
	start := socketsTicketKeygenTestStart()
	firstPath := filepath.Join(directory, "first.yml")
	secondPath := filepath.Join(directory, "second.yml")
	requireOwnerOnly := func(label string, path string) {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want a regular file of 0600", label, info.Mode())
		}
	}

	requireSocketsTicketKeygenTest(t, start, "-out", firstPath)
	requireOwnerOnly("a first file", firstPath)
	if err := os.Chmod(firstPath, 0o644); err != nil {
		t.Fatal(err)
	}
	requireSocketsTicketKeygenTest(t, start.Add(time.Hour), "-in", firstPath, "-out", secondPath)
	requireOwnerOnly("a file from an -in everyone reads", secondPath)
	requireSocketsTicketKeygenTest(t, start.Add(2*time.Hour), "-in", firstPath, "-out", firstPath)
	requireOwnerOnly("a rotation in place", firstPath)
}

// No run prints a seed, a private key or a public key, in base64, hex or raw, nor the start of one: not a first file's
// run, not a rotation's, which reads every kept key's seed, and not a refused run's.
func TestSocketsTicketKeygenPrintsNoSeed(t *testing.T) {
	directory := t.TempDir()
	settings := socketsservice.DefaultSocketTicketKeySettings()
	start := socketsTicketKeygenTestStart()
	firstPath := filepath.Join(directory, "first.yml")
	secondPath := filepath.Join(directory, "second.yml")
	outputNameTexts := map[string]string{}
	for _, run := range []struct {
		name     string
		at       time.Time
		args     []string
		wantCode int
	}{
		{name: "a first file", at: start, args: []string{"-out", firstPath}, wantCode: 0},
		{name: "a rotation", at: start.Add(settings.RotationInterval), args: []string{"-in", firstPath, "-out", secondPath}, wantCode: 0},
		{name: "a run refused over a file", at: start.Add(settings.RotationInterval), args: []string{"-in", firstPath, "-out", secondPath}, wantCode: 1},
		{name: "a rotation in place", at: start.Add(settings.RotationInterval + time.Hour), args: []string{"-in", secondPath, "-out", secondPath}, wantCode: 0},
	} {
		code, stdout, stderr := runSocketsTicketKeygenTest(run.at, run.args...)
		if code != run.wantCode {
			t.Fatalf("%s exited %d: %s", run.name, code, stderr)
		}
		outputNameTexts[run.name+"'s stdout"] = stdout
		outputNameTexts[run.name+"'s stderr"] = stderr
	}

	keys := append(readSocketsTicketKeygenTestFile(t, firstPath), readSocketsTicketKeygenTestFile(t, secondPath)...)
	if len(keys) != 4 {
		t.Fatalf("the files hold %d keys", len(keys))
	}
	for _, key := range keys {
		seed, err := base64.StdEncoding.DecodeString(key.Seed)
		if err != nil || len(seed) != ed25519.SeedSize {
			t.Fatalf("key %s's seed is no Ed25519 seed: %v", key.KeyId, err)
		}
		privateKey := ed25519.NewKeyFromSeed(seed)
		for _, value := range []struct {
			name  string
			bytes []byte
		}{
			{name: "seed", bytes: seed},
			{name: "private key", bytes: privateKey},
			{name: "public key", bytes: privateKey.Public().(ed25519.PublicKey)},
		} {
			for _, form := range []string{
				base64.StdEncoding.EncodeToString(value.bytes),
				base64.StdEncoding.EncodeToString(value.bytes)[:16],
				base64.RawURLEncoding.EncodeToString(value.bytes)[:16],
				hex.EncodeToString(value.bytes)[:16],
				string(value.bytes),
			} {
				for outputName, outputText := range outputNameTexts {
					if strings.Contains(outputText, form) {
						t.Errorf("%s holds key %s's %s", outputName, key.KeyId, value.name)
					}
				}
			}
		}
	}
}

// An -in the sockets block would refuse, or one it reads as no key file, refuses the run with exit 1 and a reason
// that repeats no seed: no file is created and -in keeps its bytes, whether -out is a new path or -in itself.
func TestSocketsTicketKeygenRefusesAnUnparsableIn(t *testing.T) {
	directory := t.TempDir()
	settings := socketsservice.DefaultSocketTicketKeySettings()
	start := socketsTicketKeygenTestStart()
	// a test-only seed, and one a byte short
	plantedSeed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
	shortSeed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize-1))
	createdMillis := start.Add(-time.Hour).UnixMilli()
	expireMillis := start.Add(time.Hour).UnixMilli()
	plantedKey := func(keyId string, seed string, expireMillis int64) *socketsTicketKeygenTestKey {
		return &socketsTicketKeygenTestKey{KeyId: keyId, Seed: seed, CreatedMillis: createdMillis, ExpireMillis: expireMillis}
	}
	for _, c := range []struct {
		name       string
		data       []byte
		wantStderr string
	}{
		{name: "no yaml", data: []byte("keys: [\n"), wantStderr: "socket ticket key file refused: it does not parse"},
		{name: "a field the format lacks", data: []byte(fmt.Sprintf("keys:\n  - key_id: k1\n    seed: %s\n    created_millis: %d\n    expire_millis: %d\n    note: planted\n", plantedSeed, createdMillis, expireMillis)), wantStderr: "it does not parse"},
		{name: "a seed a byte short", data: socketsTicketKeygenTestFileBytes(t, plantedKey("k1", shortSeed, expireMillis)), wantStderr: "key 0's seed is no Ed25519 seed"},
		{name: "a seed that is no base64", data: socketsTicketKeygenTestFileBytes(t, plantedKey("k1", "not base64!", expireMillis)), wantStderr: "key 0's seed is no Ed25519 seed"},
		{name: "a key named twice", data: socketsTicketKeygenTestFileBytes(t, plantedKey("k1", plantedSeed, expireMillis), plantedKey("k1", plantedSeed, expireMillis)), wantStderr: "key 1 has no id of its own"},
		{name: "a key with no id", data: socketsTicketKeygenTestFileBytes(t, plantedKey("", plantedSeed, expireMillis)), wantStderr: "key 0 has no id of its own"},
		{name: "a key expiring at its creation", data: socketsTicketKeygenTestFileBytes(t, plantedKey("k1", plantedSeed, createdMillis)), wantStderr: "key 0 expires before its creation"},
		{name: "an empty file", data: []byte{}, wantStderr: "is empty, which is no key file"},
		{name: "a blank file", data: []byte("\n \n"), wantStderr: "is empty, which is no key file"},
		{name: "a file past the bound", data: append([]byte("keys: []\n"), bytes.Repeat([]byte("#"), settings.MaxFileByteCount)...), wantStderr: fmt.Sprintf("holds more than the %d bytes a key file may", settings.MaxFileByteCount)},
	} {
		inPath := filepath.Join(directory, "in.yml")
		outPath := filepath.Join(directory, "out.yml")
		if err := os.WriteFile(inPath, c.data, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{outPath, inPath} {
			code, stdout, stderr := runSocketsTicketKeygenTest(start, "-in", inPath, "-out", target)
			if code != 1 || stdout != "" || !strings.Contains(stderr, "sidecarctl sockets ticket-keygen: nothing was written") || !strings.Contains(stderr, c.wantStderr) {
				t.Errorf("%s, -out=%s: exit %d, stdout %q, stderr %q, want %q", c.name, filepath.Base(target), code, stdout, stderr, c.wantStderr)
			}
			if strings.Contains(stderr, plantedSeed[:16]) || strings.Contains(stderr, shortSeed[:16]) {
				t.Errorf("%s: stderr repeats a seed", c.name)
			}
			if data, err := os.ReadFile(inPath); err != nil || !bytes.Equal(data, c.data) {
				t.Errorf("%s, -out=%s: -in was changed: %v", c.name, filepath.Base(target), err)
			}
			if names := socketsTicketKeygenTestNames(t, directory); !slices.Equal(names, []string{"in.yml"}) {
				t.Fatalf("%s, -out=%s: the run left %v", c.name, filepath.Base(target), names)
			}
		}
	}

	for _, c := range []struct {
		name       string
		inPath     string
		wantStderr string
	}{
		{name: "a missing file", inPath: filepath.Join(directory, "missing.yml"), wantStderr: "no such file or directory"},
		{name: "a directory", inPath: directory, wantStderr: "is a directory"},
	} {
		outPath := filepath.Join(directory, "out.yml")
		code, stdout, stderr := runSocketsTicketKeygenTest(start, "-in", c.inPath, "-out", outPath)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written: -in:") || !strings.Contains(stderr, c.wantStderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.name, code, stdout, stderr)
		}
		if _, err := os.Lstat(outPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: -out was created: %v", c.name, err)
		}
	}
}

// A path that exists is never written over unless -in names that same file: not a file with no -in, not one -in does
// not name, and not a symlink, even one to -in's file, or a directory. The refused run leaves the directory as it was,
// what stood at -out unchanged and -in's bytes too.
func TestSocketsTicketKeygenNeverWritesOverAFile(t *testing.T) {
	start := socketsTicketKeygenTestStart()
	sentinel := []byte("an earlier file the run must not touch\n")
	for _, c := range []struct {
		name string
		// lays out what stands at -out
		plant      func(directory string, inPath string, outPath string) error
		withIn     bool
		wantStderr string
	}{
		{
			name: "a file at -out and no -in",
			plant: func(directory string, inPath string, outPath string) error {
				return os.WriteFile(outPath, sentinel, 0o644)
			},
			wantStderr: "exists, and this writes over a file only when -in names it",
		},
		{
			name: "a file at -out that -in does not name",
			plant: func(directory string, inPath string, outPath string) error {
				return os.WriteFile(outPath, sentinel, 0o644)
			},
			withIn:     true,
			wantStderr: "exists, and this writes over a file only when -in names it",
		},
		{
			name:       "a symlink at -out to -in's file",
			plant:      func(directory string, inPath string, outPath string) error { return os.Symlink(inPath, outPath) },
			withIn:     true,
			wantStderr: "is a symlink; name the file itself",
		},
		{
			name: "a symlink at -out to nowhere",
			plant: func(directory string, inPath string, outPath string) error {
				return os.Symlink(filepath.Join(directory, "nowhere.yml"), outPath)
			},
			wantStderr: "is a symlink; name the file itself",
		},
		{
			name:       "a directory at -out",
			plant:      func(directory string, inPath string, outPath string) error { return os.Mkdir(outPath, 0o700) },
			withIn:     true,
			wantStderr: "is not a regular file",
		},
	} {
		directory := t.TempDir()
		inPath := filepath.Join(directory, "in.yml")
		outPath := filepath.Join(directory, "out.yml")
		requireSocketsTicketKeygenTest(t, start, "-out", inPath)
		inData, err := os.ReadFile(inPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.plant(directory, inPath, outPath); err != nil {
			t.Fatal(err)
		}
		// what stands at -out: its mode, and a file's bytes or a link's target
		outState := func() string {
			info, err := os.Lstat(outPath)
			if err != nil {
				return err.Error()
			}
			state := info.Mode().String()
			if info.Mode().IsRegular() {
				data, _ := os.ReadFile(outPath)
				state += " " + string(data)
			} else if target, err := os.Readlink(outPath); err == nil {
				state += " -> " + target
			}
			return state
		}
		beforeState := outState()
		beforeNames := socketsTicketKeygenTestNames(t, directory)

		args := []string{"-out", outPath}
		if c.withIn {
			args = append(args, "-in", inPath)
		}
		code, stdout, stderr := runSocketsTicketKeygenTest(start.Add(time.Hour), args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written: -out="+outPath) || !strings.Contains(stderr, c.wantStderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.name, code, stdout, stderr)
		}
		if afterNames := socketsTicketKeygenTestNames(t, directory); !slices.Equal(afterNames, beforeNames) {
			t.Errorf("%s: the directory held %v and holds %v", c.name, beforeNames, afterNames)
		}
		if afterState := outState(); afterState != beforeState {
			t.Errorf("%s: -out was %q and is %q", c.name, beforeState, afterState)
		}
		if data, err := os.ReadFile(inPath); err != nil || !bytes.Equal(data, inData) {
			t.Errorf("%s: -in was changed: %v", c.name, err)
		}
	}
}

// A command line the command cannot take exits 2 with the usage and writes nothing: -out missing or empty, -in empty,
// a positional, a flag given twice, a value that looks like a flag, a flag with no value and a flag the command does
// not have.
func TestSocketsTicketKeygenRefusesCommandLines(t *testing.T) {
	directory := t.TempDir()
	// the relative paths below resolve in the scratch directory, never in the package's own
	t.Chdir(directory)
	start := socketsTicketKeygenTestStart()
	for _, c := range []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "no flags", args: []string{}, wantStderr: "-out names no file"},
		{name: "an empty -out", args: []string{"-out="}, wantStderr: "-out names no file"},
		{name: "-in alone", args: []string{"-in", "a.yml"}, wantStderr: "-out names no file"},
		{name: "an empty -in", args: []string{"-out", "a.yml", "-in="}, wantStderr: "-in names no file; for a deployment's first file leave -in off"},
		{name: "a positional last", args: []string{"-out", "a.yml", "b.yml"}, wantStderr: "takes -out and -in alone"},
		{name: "a positional first", args: []string{"a.yml", "-out", "b.yml"}, wantStderr: "takes -out and -in alone"},
		{name: "-out twice", args: []string{"-out", "a.yml", "-out=b.yml"}, wantStderr: "given twice"},
		{name: "-in twice", args: []string{"-out", "a.yml", "-in", "b.yml", "-in", "c.yml"}, wantStderr: "given twice"},
		// flag takes the next argument for -out's value, though it begins with -
		{name: "a flag taken for a value", args: []string{"-out", "-in=b.yml"}, wantStderr: "looks like a flag"},
		{name: "a value that looks like a flag", args: []string{"-out=a.yml", "-in=-b.yml"}, wantStderr: "looks like a flag"},
		{name: "-out with no value", args: []string{"-out"}, wantStderr: "flag needs an argument"},
		{name: "a flag the command lacks", args: []string{"-out", "a.yml", "-force"}, wantStderr: "flag provided but not defined"},
	} {
		code, stdout, stderr := runSocketsTicketKeygenTest(start, c.args...)
		if code != 2 || stdout != "" {
			t.Errorf("%s: exit %d, stdout %q", c.name, code, stdout)
		}
		if !strings.Contains(stderr, c.wantStderr) || !strings.Contains(stderr, "sidecarctl sockets ticket-keygen -out <file> [-in <file>]") {
			t.Errorf("%s: stderr %q, want %q and the usage", c.name, stderr, c.wantStderr)
		}
		if names := socketsTicketKeygenTestNames(t, directory); len(names) != 0 {
			t.Fatalf("%s: the run wrote %v", c.name, names)
		}
	}
}

// A flag's value follows it apart, as the design writes the command, or after an =, as sidecarctl's other commands
// take theirs, with one dash or two, and the flags come in either order.
func TestSocketsTicketKeygenTakesEitherFlagSpelling(t *testing.T) {
	directory := t.TempDir()
	// the relative paths below resolve in the scratch directory, never in the package's own
	t.Chdir(directory)
	start := socketsTicketKeygenTestStart()
	requireSocketsTicketKeygenTest(t, start, "-out=first.yml")
	for index, c := range []struct {
		args         []string
		outPath      string
		wantKeyCount int
	}{
		{args: []string{"-out", "a.yml"}, outPath: "a.yml", wantKeyCount: 1},
		{args: []string{"--out", "b.yml"}, outPath: "b.yml", wantKeyCount: 1},
		{args: []string{"--out=c.yml"}, outPath: "c.yml", wantKeyCount: 1},
		{args: []string{"-in", "first.yml", "-out", "d.yml"}, outPath: "d.yml", wantKeyCount: 2},
		{args: []string{"-in=first.yml", "-out=e.yml"}, outPath: "e.yml", wantKeyCount: 2},
		{args: []string{"--in", "first.yml", "--out=f.yml"}, outPath: "f.yml", wantKeyCount: 2},
		{args: []string{"-out", "g.yml", "-in", "first.yml"}, outPath: "g.yml", wantKeyCount: 2},
	} {
		requireSocketsTicketKeygenTest(t, start.Add(time.Duration(index+1)*time.Minute), c.args...)
		if keys := readSocketsTicketKeygenTestFile(t, c.outPath); len(keys) != c.wantKeyCount {
			t.Errorf("%v: the file holds %d keys, want %d", c.args, len(keys), c.wantKeyCount)
		}
	}
}

// An -in already holding MaxKeyCount live keys refuses the run, writing nothing; once its oldest key has expired the
// same file takes the new key in its place.
func TestSocketsTicketKeygenRefusesAFullFile(t *testing.T) {
	directory := t.TempDir()
	settings := socketsservice.DefaultSocketTicketKeySettings()
	start := socketsTicketKeygenTestStart()
	var data []byte
	for index := 0; index < settings.MaxKeyCount; index += 1 {
		var err error
		if data, err = socketsservice.SocketTicketKeyFileAdd(data, start.Add(time.Duration(index)*time.Minute), settings); err != nil {
			t.Fatal(err)
		}
	}
	inPath := filepath.Join(directory, "in.yml")
	outPath := filepath.Join(directory, "out.yml")
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSocketsTicketKeygenTest(start.Add(time.Hour), "-in", inPath, "-out", outPath)
	if code != 1 || stdout != "" || !strings.Contains(stderr, fmt.Sprintf("nothing was written: -in=%s: socket ticket key file refused: %d live keys, the most a file holds", inPath, settings.MaxKeyCount)) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if names := socketsTicketKeygenTestNames(t, directory); !slices.Equal(names, []string{"in.yml"}) {
		t.Fatalf("the run left %v", names)
	}

	oldest := readSocketsTicketKeygenTestFile(t, inPath)[0]
	requireSocketsTicketKeygenTest(t, time.UnixMilli(oldest.ExpireMillis), "-in", inPath, "-out", outPath)
	keys := readSocketsTicketKeygenTestFile(t, outPath)
	if len(keys) != settings.MaxKeyCount || slices.Contains(socketsTicketKeygenTestKeyIds(keys), oldest.KeyId) {
		t.Fatalf("the file holds %d keys, the expired one among them: %v", len(keys), slices.Contains(socketsTicketKeygenTestKeyIds(keys), oldest.KeyId))
	}
}

// A new file that does not read back as the sockets block reads it, holding -in's live keys unchanged and exactly
// one new key with its expiry, is never written, to a new path or in place: an add that returns bytes the block
// refuses, drops a live key, changes a kept key's seed, expiry or id, adds two keys, or gives the new key another
// expiry.
func TestSocketsTicketKeygenWritesNothingTheReadBackRefuses(t *testing.T) {
	directory := t.TempDir()
	ticketKeySettings := socketsservice.DefaultSocketTicketKeySettings()
	start := socketsTicketKeygenTestStart()
	runTime := start.Add(time.Hour)
	inPath := filepath.Join(directory, "in.yml")
	outPath := filepath.Join(directory, "out.yml")
	requireSocketsTicketKeygenTest(t, start, "-out", inPath)
	inData, err := os.ReadFile(inPath)
	if err != nil {
		t.Fatal(err)
	}
	// a test-only seed
	otherSeed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, ed25519.SeedSize))
	edit := func(data []byte, change func(keys []*socketsTicketKeygenTestKey) []*socketsTicketKeygenTestKey) []byte {
		return socketsTicketKeygenTestFileBytes(t, change(parseSocketsTicketKeygenTestFile(t, data))...)
	}
	for _, c := range []struct {
		name string
		// what the faulty add returns, from the real add's bytes
		change     func(data []byte) []byte
		wantStderr string
	}{
		{
			name:       "bytes the block refuses",
			change:     func(data []byte) []byte { return []byte("keys: [\n") },
			wantStderr: "it holds a key count of 0 where 2 was due: -in's live keys and one new",
		},
		{
			name: "a live key dropped",
			change: func(data []byte) []byte {
				return edit(data, func(keys []*socketsTicketKeygenTestKey) []*socketsTicketKeygenTestKey { return keys[1:] })
			},
			wantStderr: "it holds a key count of 1 where 2 was due: -in's live keys and one new",
		},
		{
			name: "a kept key's seed changed",
			change: func(data []byte) []byte {
				return edit(data, func(keys []*socketsTicketKeygenTestKey) []*socketsTicketKeygenTestKey {
					keys[0].Seed = otherSeed
					return keys
				})
			},
			wantStderr: "of -in is not kept as it was",
		},
		{
			name: "a kept key's expiry moved",
			change: func(data []byte) []byte {
				return edit(data, func(keys []*socketsTicketKeygenTestKey) []*socketsTicketKeygenTestKey {
					keys[0].ExpireMillis += 1000
					return keys
				})
			},
			wantStderr: "of -in is not kept as it was",
		},
		{
			name: "a kept key's id changed",
			change: func(data []byte) []byte {
				return edit(data, func(keys []*socketsTicketKeygenTestKey) []*socketsTicketKeygenTestKey {
					keys[0].KeyId += "-other"
					return keys
				})
			},
			wantStderr: "of -in is not kept as it was",
		},
		{
			name: "two new keys",
			change: func(data []byte) []byte {
				twice, err := socketsservice.SocketTicketKeyFileAdd(data, runTime, nil)
				if err != nil {
					t.Fatal(err)
				}
				return twice
			},
			wantStderr: "it holds a key count of 3 where 2 was due: -in's live keys and one new",
		},
		{
			name: "the new key's expiry moved",
			change: func(data []byte) []byte {
				return edit(data, func(keys []*socketsTicketKeygenTestKey) []*socketsTicketKeygenTestKey {
					keys[len(keys)-1].ExpireMillis -= 1
					return keys
				})
			},
			wantStderr: fmt.Sprintf("the new key does not expire %s after %s", ticketKeySettings.RotationInterval+ticketKeySettings.KeyOverlap, socketsTicketKeygenTestTimeText(runTime.UnixMilli())),
		},
	} {
		settings := defaultSocketsTicketKeygenSettings()
		settings.now = func() time.Time {
			return runTime
		}
		settings.addKey = func(existing []byte, now time.Time, ticketKeySettings *socketsservice.SocketTicketKeySettings) ([]byte, error) {
			data, err := socketsservice.SocketTicketKeyFileAdd(existing, now, ticketKeySettings)
			if err != nil {
				return nil, err
			}
			return c.change(data), nil
		}
		for _, target := range []string{outPath, inPath} {
			code, stdout, stderr := runSocketsTicketKeygenTestWithSettings(settings, "-in", inPath, "-out", target)
			if code != 1 || stdout != "" ||
				!strings.Contains(stderr, "nothing was written: the new file does not read back as the sockets block reads it: ") ||
				!strings.Contains(stderr, c.wantStderr) {
				t.Errorf("%s, -out=%s: exit %d, stdout %q, stderr %q, want %q", c.name, filepath.Base(target), code, stdout, stderr, c.wantStderr)
			}
			if names := socketsTicketKeygenTestNames(t, directory); !slices.Equal(names, []string{"in.yml"}) {
				t.Fatalf("%s, -out=%s: the run left %v", c.name, filepath.Base(target), names)
			}
			if data, err := os.ReadFile(inPath); err != nil || !bytes.Equal(data, inData) {
				t.Errorf("%s, -out=%s: -in was changed: %v", c.name, filepath.Base(target), err)
			}
		}
	}
}

// When no key of -in can sign at the run's time, because each is past SignMargin of its expiry or expired, the
// output says the new key signs as soon as a block reads the file; the file still keeps a key that verifies on.
func TestSocketsTicketKeygenSaysWhenNoKeyOfInSigns(t *testing.T) {
	directory := t.TempDir()
	settings := socketsservice.DefaultSocketTicketKeySettings()
	start := socketsTicketKeygenTestStart()
	inPath := filepath.Join(directory, "in.yml")
	requireSocketsTicketKeygenTest(t, start, "-out", inPath)
	inKey := readSocketsTicketKeygenTestFile(t, inPath)[0]
	inExpireTime := time.UnixMilli(inKey.ExpireMillis)
	for _, c := range []struct {
		name         string
		at           time.Time
		wantKeyCount int
	}{
		{name: "within SignMargin of -in's key's expiry", at: inExpireTime.Add(-settings.SignMargin), wantKeyCount: 2},
		{name: "past -in's key's expiry", at: inExpireTime, wantKeyCount: 1},
	} {
		outPath := filepath.Join(directory, "out.yml")
		if err := os.Remove(outPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		stdout := requireSocketsTicketKeygenTest(t, c.at, "-in", inPath, "-out", outPath)
		if !strings.Contains(stdout, "no key of -in can sign any more, so the new key signs as soon as a sockets block reads the file") {
			t.Errorf("%s: stdout does not say the new key signs at once: %s", c.name, stdout)
		}
		if strings.Contains(stdout, "after the first read in which it sees the file") {
			t.Errorf("%s: stdout says the new key waits to settle, though nothing else can sign: %s", c.name, stdout)
		}
		if keys := readSocketsTicketKeygenTestFile(t, outPath); len(keys) != c.wantKeyCount {
			t.Errorf("%s: the file holds %d keys, want %d", c.name, len(keys), c.wantKeyCount)
		}
	}
}

// A file that cannot be written is not written, and nothing is left: -out in a directory that does not exist.
func TestSocketsTicketKeygenLeavesNothingWhenItCannotWrite(t *testing.T) {
	directory := t.TempDir()
	outPath := filepath.Join(directory, "missing", "socket-ticket-keys.yml")
	code, stdout, stderr := runSocketsTicketKeygenTest(socketsTicketKeygenTestStart(), "-out", outPath)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written: -out:") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if names := socketsTicketKeygenTestNames(t, directory); len(names) != 0 {
		t.Fatalf("the run left %v", names)
	}
}

// The clock a read back reads at stays where it stopped: a positive wait never fires and a call after one never
// comes, its stop answering true once and false after; a wait that is not positive fires at once, and its call comes.
func TestSocketsTicketKeygenStoppedClockNeverMoves(t *testing.T) {
	stoppedAt := socketsTicketKeygenTestStart()
	clock := &socketsTicketKeygenStoppedClock{now: stoppedAt}
	if !clock.Now().Equal(stoppedAt) {
		t.Fatalf("the clock reads %v", clock.Now())
	}
	select {
	case <-clock.After(time.Nanosecond):
		t.Fatal("a positive wait fired")
	default:
	}
	if fired := <-clock.After(0); !fired.Equal(stoppedAt) {
		t.Fatalf("a wait of 0 fired at %v", fired)
	}
	stop := clock.AfterFunc(time.Nanosecond, func() {
		t.Error("a call after a positive wait came")
	})
	if !stop() || stop() {
		t.Fatal("stop does not answer true once and then false")
	}
	called := make(chan struct{})
	clock.AfterFunc(0, func() {
		close(called)
	})
	<-called
}

// The binary's own registry holds the command under the design's usage, and the binary's help says what it keeps
// and drops, the file's mode, that no key is printed, how often to run it, where the file
// goes, and that a rotation's new key signs SettleTime after a block first sees it, with no time to place it by.
func TestSidecarctlRegistersSocketsTicketKeygen(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"sockets", "ticket-keygen", "-out", "a.yml"})
	if command == nil || command.words != "sockets ticket-keygen" {
		t.Fatalf("the binary has no sockets ticket-keygen: %v", command)
	}
	if command.usage != "-out <file> [-in <file>]" || !slices.Equal(args, []string{"-out", "a.yml"}) {
		t.Fatalf("sockets ticket-keygen's usage is %q, with %v", command.usage, args)
	}
	stdout := &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"--help"}, stdout, io.Discard); code != 0 {
		t.Fatalf("--help exited %d", code)
	}
	for _, want := range []string{
		"sidecarctl sockets ticket-keygen -out <file> [-in <file>]",
		"whose live keys are kept and expired keys dropped",
		"created 0600 and never over a file unless -in names it",
		"It prints the new key's id and expiry, never a key",
		"Run it every 30 days and place the file in vault as socket-ticket-keys.yml",
		"which each sockets block rereads every 1m0s, and a block signs with a rotation's new key 10m0s after the first read in which it sees it.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the help lacks %q", want)
		}
	}
	for _, deadline := range []string{"at once", "after the run"} {
		if strings.Contains(stdout.String(), deadline) {
			t.Errorf("the help still times the placement: %q", deadline)
		}
	}
}

// The name the command gives the file in vault is the one server/cli/sockets mounts and reads.
func TestSocketsTicketKeygenNamesTheFileTheSocketsHostReads(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "cli", "sockets", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "hostTicketKeyFileName = " + strconv.Quote(socketsTicketKeyResourceName); !strings.Contains(string(source), want) {
		t.Fatalf("server/cli/sockets/main.go does not read the key file as %s", socketsTicketKeyResourceName)
	}
}
