// Tests for `sidecarctl internal bearer-keygen` on files in a scratch directory, run through the binary's own command
// line as the owner runs it. A first draw's files load through internal_gate.go's own loaders from a vault holding
// them, and api's verifier takes the bearer as its class's; a rotation keeps the old bearer as previous and api takes
// both, and the next drops the first; every other entry of the verify file reads as it did; both files are 0600; no
// bearer or digest is printed; a class internal_gate.go does not know, a command line the command cannot take, files
// that are no pair and files it cannot use are refused with nothing written; a run that cannot put its second file in
// place puts the first back; and the platform listener's class (QD-1121), drawn beside the edge class's, passes api's
// role-mail route's gate and not the edge-client route's, as the edge bearer passes the edge-client route's alone.
// Every bearer here is drawn by the test run, or made of test-chosen bytes, and discarded with its scratch directory.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/controller"
)

// Runs a command line of the binary's own registry, as the owner would, and returns its exit code and output.
func runInternalBearerKeygenTest(args ...string) (int, string, string) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := sidecarctlCommands.run(append([]string{"internal", "bearer-keygen"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// Runs the command under settings, through a command set of its own, for the tests that replace the random source or
// set the hook.
func runInternalBearerKeygenTestWithSettings(settings *internalBearerKeygenSettings, args ...string) (int, string, string) {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "internal bearer-keygen",
		usage:   "<files>",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runInternalBearerKeygenWithSettings(args, settings, out)
		},
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := commands.run(append([]string{"internal", "bearer-keygen"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// The flags of a run of class over the two paths.
func internalBearerKeygenTestArgs(class server.InternalGateCaller, outPath string, verifyPath string) []string {
	return []string{"--class=" + string(class), "--out=" + outPath, "--verify=" + verifyPath}
}

// A fresh scratch directory and, in it, the paths of class's credential file and api's verify file under their
// vault names.
func newInternalBearerKeygenTestPaths(t *testing.T, class server.InternalGateCaller) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	return directory, filepath.Join(directory, server.InternalGateCredentialFileName(class)), filepath.Join(directory, server.InternalGateVerifyFileName)
}

// A bearer of the gate's format drawn here, and its digest in hex, for files a test writes itself.
func newInternalBearerKeygenTestBearer(t *testing.T) (string, string) {
	t.Helper()
	raw := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	bearer := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(bearer))
	return bearer, hex.EncodeToString(digest[:])
}

// A bearer's digest in hex, worked out here rather than by the command.
func internalBearerKeygenTestDigest(bearer string) string {
	digest := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(digest[:])
}

// The bearers a credential file holds, current and previous, as YAML gives them.
func readInternalBearerKeygenTestCredential(t *testing.T, path string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	credential := &internalBearerKeygenCredentialYaml{}
	if err := yaml.Unmarshal(data, credential); err != nil {
		t.Fatalf("the credential file does not parse: %v", err)
	}
	return credential.Current, credential.Previous
}

// The entries of a verify file, by caller name, as api decodes them.
func readInternalBearerKeygenTestVerify(t *testing.T, path string) map[string]*internalBearerKeygenVerifyEntryYaml {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	callerEntries := map[string]*internalBearerKeygenVerifyEntryYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&callerEntries); err != nil {
		t.Fatalf("the verify file does not parse: %v", err)
	}
	return callerEntries
}

// What class's services and api load from a vault holding the two files a run wrote: the bearer the credential file
// sends, through server.LoadInternalGateCredential, and api's verifier, through server.LoadInternalGateVerifier.
func loadInternalBearerKeygenTestFiles(t *testing.T, class server.InternalGateCaller, outPath string, verifyPath string) (string, *server.InternalGateVerifier) {
	t.Helper()
	credentialData, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	verifyData, err := os.ReadFile(verifyPath)
	if err != nil {
		t.Fatal(err)
	}
	popCredential := server.Vault.PushSimpleResource(server.InternalGateCredentialFileName(class), credentialData)
	defer popCredential()
	popVerify := server.Vault.PushSimpleResource(server.InternalGateVerifyFileName, verifyData)
	defer popVerify()
	bearer, err := server.LoadInternalGateCredential(class)
	if err != nil {
		t.Fatalf("class %s's services do not load its file: %v", class, err)
	}
	verifier, err := server.LoadInternalGateVerifier()
	if err != nil {
		t.Fatalf("api does not load its verify file: %v", err)
	}
	return bearer, verifier
}

// The class api's verifier takes a bearer as, and whether it takes it.
func authenticateInternalBearerKeygenTest(verifier *server.InternalGateVerifier, bearer string) (server.InternalGateCaller, bool) {
	request := httptest.NewRequest(http.MethodGet, server.InternalGatePathPrefix+"over-limit", nil)
	request.Header.Set("Authorization", "Bearer "+bearer)
	return verifier.Authenticate(request)
}

// What directory holds, by name: each regular file's mode and bytes, each symlink's target and each directory, so a
// refused run can be shown to leave it as it found it.
func internalBearerKeygenTestDirectory(t *testing.T, directory string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	nameContents := map[string]string{}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			nameContents[entry.Name()] = "a symlink to " + target
		case info.IsDir():
			nameContents[entry.Name()] = "a directory"
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			nameContents[entry.Name()] = info.Mode().String() + " " + string(data)
		}
	}
	return nameContents
}

// A first draw writes the class's first credential file and a verify file of its one entry. Loaded from a vault
// holding them, as the class's services and api load them, the credential file sends a 256-bit bearer in unpadded
// base64url, and api's verifier takes it as the class's and takes no other bearer.
func TestInternalBearerKeygenFirstDrawReadsBack(t *testing.T) {
	_, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	code, stdout, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"drew a new bearer for class edge: 256 random bits, printed nowhere",
		"wrote --out=" + outPath + ", readable by its owner alone: class edge's first credential file",
		"wrote --verify=" + verifyPath + ", readable by its owner alone: api's verify file, whose entry edge, of class edge",
		"it holds no other class's entry",
		"in vault as internal-gate-verify.yml and deploy api first",
		"in vault as internal-gate-edge.yml and deploy the services that mount it",
		"a rotation is this command again",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q: %s", want, stdout)
		}
	}

	bearer, verifier := loadInternalBearerKeygenTestFiles(t, server.InternalGateCallerEdge, outPath, verifyPath)
	if raw, err := base64.RawURLEncoding.DecodeString(bearer); err != nil || len(raw) != 32 {
		t.Fatalf("the bearer is not 256 bits in unpadded base64url: %d bytes, %v", len(raw), err)
	}
	if current, previous := readInternalBearerKeygenTestCredential(t, outPath); current != bearer || previous != "" {
		t.Fatal("a first credential file holds more than its current bearer")
	}
	if caller, ok := authenticateInternalBearerKeygenTest(verifier, bearer); !ok || caller != server.InternalGateCallerEdge {
		t.Fatalf("api's verifier takes the bearer as %q %t", caller, ok)
	}
	stranger, _ := newInternalBearerKeygenTestBearer(t)
	if _, ok := authenticateInternalBearerKeygenTest(verifier, stranger); ok {
		t.Fatal("api's verifier takes a bearer the run did not draw")
	}
	want := map[string]*internalBearerKeygenVerifyEntryYaml{
		"edge": {Class: "edge", Sha256: []string{internalBearerKeygenTestDigest(bearer)}},
	}
	if callerEntries := readInternalBearerKeygenTestVerify(t, verifyPath); !reflect.DeepEqual(callerEntries, want) {
		t.Fatalf("the verify file holds %d entries, not the class's one with its bearer's digest", len(callerEntries))
	}
}

// A run on the files a first draw wrote is a rotation: the new bearer is current and the first previous, and api's
// verifier takes both as the class's, so the services move with no downtime. The next run drops the first bearer from
// both files, and api no longer takes it.
func TestInternalBearerKeygenRotationKeepsThePrevious(t *testing.T) {
	_, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	args := internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)
	bearers := []string{}
	for runIndex := range 3 {
		code, stdout, stderr := runInternalBearerKeygenTest(args...)
		if code != 0 {
			t.Fatalf("run %d: exit %d: %s", runIndex, code, stderr)
		}
		bearer, verifier := loadInternalBearerKeygenTestFiles(t, server.InternalGateCallerEdge, outPath, verifyPath)
		if slices.Contains(bearers, bearer) {
			t.Fatalf("run %d drew a bearer an earlier run drew", runIndex)
		}
		bearers = append(bearers, bearer)
		current, previous := readInternalBearerKeygenTestCredential(t, outPath)
		wantDigests := []string{internalBearerKeygenTestDigest(bearer)}
		if 0 < runIndex {
			if current != bearer || previous != bearers[runIndex-1] {
				t.Fatalf("run %d: the credential file does not hold the new bearer as current and the one before as previous", runIndex)
			}
			if !strings.Contains(stdout, "the new bearer its current and the one it replaces its previous") || !strings.Contains(stdout, "takes the new bearer and the one it replaces") {
				t.Errorf("run %d: stdout does not say the rotation: %s", runIndex, stdout)
			}
			wantDigests = append(wantDigests, internalBearerKeygenTestDigest(bearers[runIndex-1]))
		}
		if dropped := strings.Contains(stdout, "the bearer that was previous is dropped"); dropped != (runIndex == 2) {
			t.Errorf("run %d: stdout says a previous bearer is dropped: %t", runIndex, dropped)
		}
		for index, sentBearer := range bearers {
			caller, ok := authenticateInternalBearerKeygenTest(verifier, sentBearer)
			wantOk := runIndex-1 <= index
			if ok != wantOk || (ok && caller != server.InternalGateCallerEdge) {
				t.Errorf("run %d: api's verifier takes the bearer of run %d as %q %t, want %t", runIndex, index, caller, ok, wantOk)
			}
		}
		want := map[string]*internalBearerKeygenVerifyEntryYaml{"edge": {Class: "edge", Sha256: wantDigests}}
		if callerEntries := readInternalBearerKeygenTestVerify(t, verifyPath); !reflect.DeepEqual(callerEntries, want) {
			t.Fatalf("run %d: the class's entry does not hold the new bearer's digest and the one before's alone", runIndex)
		}
	}
}

// Every other entry is kept as it was, comments with it, in flow or block style, with upper-case digests or under a
// caller name that is not its class's, and its bearers verify as before. A rotation of a class whose entry is under
// such a name keeps the name.
func TestInternalBearerKeygenKeepsOtherEntries(t *testing.T) {
	directory, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	evsCurrent, evsCurrentDigest := newInternalBearerKeygenTestBearer(t)
	evsPrevious, evsPreviousDigest := newInternalBearerKeygenTestBearer(t)
	tlsedgeBearer, tlsedgeDigest := newInternalBearerKeygenTestBearer(t)
	verifyFile := "# api's verify file, made by the test\n" +
		"evs: {class: evs, sha256: [" + evsCurrentDigest + ", " + evsPreviousDigest + "]} # the evs blocks\n" +
		"tlsedge-1:\n  class: tlsedge\n  sha256:\n    - " + strings.ToUpper(tlsedgeDigest) + "\n"
	if err := os.WriteFile(verifyPath, []byte(verifyFile), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readInternalBearerKeygenTestVerify(t, verifyPath)

	code, stdout, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "its other entries, of classes evs, tlsedge, read as they did") {
		t.Errorf("stdout does not name the other entries' classes: %s", stdout)
	}
	afterDraw := readInternalBearerKeygenTestVerify(t, verifyPath)
	for _, callerName := range []string{"evs", "tlsedge-1"} {
		if !reflect.DeepEqual(afterDraw[callerName], before[callerName]) {
			t.Errorf("entry %s changed", callerName)
		}
	}
	if len(afterDraw) != 3 {
		t.Fatalf("the verify file holds %d entries, want evs, tlsedge-1 and edge", len(afterDraw))
	}
	data, err := os.ReadFile(verifyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, comment := range []string{"# api's verify file, made by the test", "# the evs blocks"} {
		if !bytes.Contains(data, []byte(comment)) {
			t.Errorf("the comment %q was dropped", comment)
		}
	}
	edgeBearer, verifier := loadInternalBearerKeygenTestFiles(t, server.InternalGateCallerEdge, outPath, verifyPath)
	for _, c := range []struct {
		bearer string
		class  server.InternalGateCaller
	}{
		{bearer: evsCurrent, class: server.InternalGateCallerEvs},
		{bearer: evsPrevious, class: server.InternalGateCallerEvs},
		{bearer: tlsedgeBearer, class: server.InternalGateCallerTlsedge},
		{bearer: edgeBearer, class: server.InternalGateCallerEdge},
	} {
		if caller, ok := authenticateInternalBearerKeygenTest(verifier, c.bearer); !ok || caller != c.class {
			t.Errorf("api's verifier takes a bearer of %s as %q %t", c.class, caller, ok)
		}
	}

	// tlsedge's rotation, its entry under the name tlsedge-1
	tlsedgeOutPath := filepath.Join(directory, server.InternalGateCredentialFileName(server.InternalGateCallerTlsedge))
	if err := os.WriteFile(tlsedgeOutPath, []byte("current: "+tlsedgeBearer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerTlsedge, tlsedgeOutPath, verifyPath)...); code != 0 {
		t.Fatalf("tlsedge's rotation: exit %d: %s", code, stderr)
	}
	newTlsedgeBearer, verifier := loadInternalBearerKeygenTestFiles(t, server.InternalGateCallerTlsedge, tlsedgeOutPath, verifyPath)
	afterRotation := readInternalBearerKeygenTestVerify(t, verifyPath)
	if _, ok := afterRotation["tlsedge"]; ok || len(afterRotation) != 3 {
		t.Fatal("tlsedge's rotation added an entry where it should edit tlsedge-1")
	}
	wantTlsedge := &internalBearerKeygenVerifyEntryYaml{Class: "tlsedge", Sha256: []string{internalBearerKeygenTestDigest(newTlsedgeBearer), tlsedgeDigest}}
	if !reflect.DeepEqual(afterRotation["tlsedge-1"], wantTlsedge) {
		t.Fatal("tlsedge-1 does not hold the new bearer's digest and the old one's")
	}
	for _, callerName := range []string{"evs", "edge"} {
		if !reflect.DeepEqual(afterRotation[callerName], afterDraw[callerName]) {
			t.Errorf("tlsedge's rotation changed entry %s", callerName)
		}
	}
	for _, sentBearer := range []string{newTlsedgeBearer, tlsedgeBearer} {
		if caller, ok := authenticateInternalBearerKeygenTest(verifier, sentBearer); !ok || caller != server.InternalGateCallerTlsedge {
			t.Errorf("api's verifier takes a tlsedge bearer as %q %t", caller, ok)
		}
	}
}

// Every class internal_gate.go knows is drawn into one verify file, and api's verifier takes each class's bearer as
// that class's.
func TestInternalBearerKeygenDrawsEveryClass(t *testing.T) {
	directory := t.TempDir()
	verifyPath := filepath.Join(directory, server.InternalGateVerifyFileName)
	classes := server.InternalGateCallers()
	for _, class := range classes {
		outPath := filepath.Join(directory, server.InternalGateCredentialFileName(class))
		if code, _, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(class, outPath, verifyPath)...); code != 0 {
			t.Fatalf("class %s: exit %d: %s", class, code, stderr)
		}
	}
	if callerEntries := readInternalBearerKeygenTestVerify(t, verifyPath); len(callerEntries) != len(classes) {
		t.Fatalf("the verify file holds %d entries for %d classes", len(callerEntries), len(classes))
	}
	for _, class := range classes {
		bearer, verifier := loadInternalBearerKeygenTestFiles(t, class, filepath.Join(directory, server.InternalGateCredentialFileName(class)), verifyPath)
		if caller, ok := authenticateInternalBearerKeygenTest(verifier, bearer); !ok || caller != class {
			t.Errorf("api's verifier takes class %s's bearer as %q %t", class, caller, ok)
		}
	}
}

// Both files are 0600 after a first draw, and after a rotation of files others could read before.
func TestInternalBearerKeygenFilesAreOwnerOnly(t *testing.T) {
	_, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	args := internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)
	for runIndex := range 2 {
		if code, _, stderr := runInternalBearerKeygenTest(args...); code != 0 {
			t.Fatalf("run %d: exit %d: %s", runIndex, code, stderr)
		}
		for _, path := range []string{outPath, verifyPath} {
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Errorf("run %d: %s has mode %v, want a regular file of 0600", runIndex, filepath.Base(path), info.Mode())
			}
			// the rotation replaces files others could read
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Nothing a run prints, on stdout or stderr, holds a bearer or a digest, whole or in part: not after a first draw or a
// rotation, and not from a refused run, whose files hold bearers, among them one written alone in a credential file,
// whose start YAML's own message would quote.
func TestInternalBearerKeygenPrintsNoBearer(t *testing.T) {
	directory, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	outputs := []string{}
	for runIndex := range 2 {
		code, stdout, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)...)
		if code != 0 {
			t.Fatalf("run %d: exit %d: %s", runIndex, code, stderr)
		}
		outputs = append(outputs, stdout, stderr)
	}
	current, previous := readInternalBearerKeygenTestCredential(t, outPath)

	// a credential file whose bearer the verify file does not hold, so no pair
	stranger, _ := newInternalBearerKeygenTestBearer(t)
	strangerPath := filepath.Join(directory, "stranger.yml")
	if err := os.WriteFile(strangerPath, []byte("current: "+stranger+"\nprevious: "+current+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, strangerPath, verifyPath)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "so the two are no pair") {
		t.Fatalf("a run on files that are no pair exited %d: %q %q", code, stdout, stderr)
	}
	outputs = append(outputs, stdout, stderr)
	// a bearer written alone, which YAML's message would quote the start of
	barePath := filepath.Join(directory, "bare.yml")
	if err := os.WriteFile(barePath, []byte(stranger+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, barePath, verifyPath)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "does not parse as a credential file") {
		t.Fatalf("a run on a bare bearer exited %d: %q %q", code, stdout, stderr)
	}
	outputs = append(outputs, stdout, stderr)

	for _, bearer := range []string{current, previous, stranger} {
		raw, err := base64.RawURLEncoding.DecodeString(bearer)
		if err != nil {
			t.Fatal(err)
		}
		digest := internalBearerKeygenTestDigest(bearer)
		for _, form := range []string{
			bearer,
			// as YAML's message quotes a value: its first 7 characters
			bearer[:7],
			bearer[len(bearer)-7:],
			digest[:16],
			base64.StdEncoding.EncodeToString(raw)[:16],
			hex.EncodeToString(raw)[:16],
			string(raw),
		} {
			for _, output := range outputs {
				if strings.Contains(output, form) {
					t.Fatalf("the output holds a bearer or its digest: %q", output)
				}
			}
		}
	}
}

// A class internal_gate.go does not know is refused, as a usage mistake, before any file is read or written, and the
// message names the classes it knows: another service's name, a class's name in another case or with a space, a
// path, the verify file's own part of its name, and none at all.
func TestInternalBearerKeygenRefusesUnknownClasses(t *testing.T) {
	directory := t.TempDir()
	outPath := filepath.Join(directory, "out.yml")
	verifyPath := filepath.Join(directory, "verify.yml")
	known := []string{}
	for _, class := range server.InternalGateCallers() {
		known = append(known, string(class))
	}
	for _, class := range []string{"dns", "EDGE", "edge ", "../pg", "verify", ""} {
		code, stdout, stderr := runInternalBearerKeygenTest("--class="+class, "--out="+outPath, "--verify="+verifyPath)
		if code != 2 || stdout != "" {
			t.Errorf("--class=%q: exit %d, stdout %q", class, code, stdout)
		}
		if !strings.Contains(stderr, "one of "+strings.Join(known, ", ")) || !strings.Contains(stderr, "sidecarctl internal bearer-keygen --class=<") {
			t.Errorf("--class=%q: stderr %q, want the known classes and the usage", class, stderr)
		}
		if names := internalBearerKeygenTestDirectory(t, directory); len(names) != 0 {
			t.Fatalf("--class=%q: the run wrote %v", class, slices.Collect(maps.Keys(names)))
		}
	}
}

// A command line the command cannot take exits 2 with the usage and writes nothing: a flag missing or empty, a flag
// given twice, one file named twice in any spelling, a positional, a value written apart from its flag, a value that
// looks like a flag, a flag the command does not have, and a file named as another file is in vault.
func TestInternalBearerKeygenRefusesCommandLines(t *testing.T) {
	directory := t.TempDir()
	// the relative paths below resolve in the scratch directory, never in the package's own
	t.Chdir(directory)
	for _, c := range []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "no flags", args: []string{}, wantStderr: "--class names no class"},
		{name: "no --out", args: []string{"--class=edge", "--verify=v.yml"}, wantStderr: "--out names no file"},
		{name: "no --verify", args: []string{"--class=edge", "--out=o.yml"}, wantStderr: "--verify names no file"},
		{name: "an empty --out", args: []string{"--class=edge", "--out=", "--verify=v.yml"}, wantStderr: "--out names no file"},
		{name: "a flag given twice", args: []string{"--class=edge", "--out=o.yml", "--out=p.yml", "--verify=v.yml"}, wantStderr: "given twice"},
		{name: "a class given twice", args: []string{"--class=edge", "--class=evs", "--out=o.yml", "--verify=v.yml"}, wantStderr: "given twice"},
		{name: "one file twice", args: []string{"--class=edge", "--out=a.yml", "--verify=a.yml"}, wantStderr: "name one file"},
		{name: "one file twice, spelled apart", args: []string{"--class=edge", "--out=a.yml", "--verify=./sub/../a.yml"}, wantStderr: "name one file"},
		{name: "one file twice, absolute and relative", args: []string{"--class=edge", "--out=" + filepath.Join(directory, "a.yml"), "--verify=a.yml"}, wantStderr: "name one file"},
		{name: "a positional", args: []string{"--class=edge", "--out=o.yml", "--verify=v.yml", "c.yml"}, wantStderr: "takes flags only"},
		{name: "values apart from their flags", args: []string{"--class=edge", "-out", "o.yml", "-verify", "v.yml"}, wantStderr: "takes flags only"},
		{name: "a value apart from its flag, last", args: []string{"--class=edge", "--out=o.yml", "--verify", "v.yml"}, wantStderr: "flag needs an argument"},
		// flag takes the next argument for -out's value, though it begins with -
		{name: "a flag taken for a value", args: []string{"--class=edge", "-out", "-verify=v.yml"}, wantStderr: "looks like a flag"},
		{name: "a value that looks like a flag", args: []string{"--class=edge", "--out=o.yml", "--verify=-v.yml"}, wantStderr: "looks like a flag"},
		{name: "a flag the command lacks", args: []string{"--class=edge", "--out=o.yml", "--verify=v.yml", "--force"}, wantStderr: "flag provided but not defined"},
		{name: "--out named as the verify file", args: []string{"--class=edge", "--out=sub/internal-gate-verify.yml", "--verify=v.yml"}, wantStderr: "is named as api's verify file is in vault"},
		{name: "--out named as another class's file", args: []string{"--class=edge", "--out=internal-gate-tlsedge.yml", "--verify=v.yml"}, wantStderr: "is named as class tlsedge's credential file is in vault"},
		{name: "--verify named as a class's file", args: []string{"--class=edge", "--out=o.yml", "--verify=internal-gate-edge.yml"}, wantStderr: "is named as class edge's credential file is in vault"},
	} {
		code, stdout, stderr := runInternalBearerKeygenTest(c.args...)
		if code != 2 || stdout != "" {
			t.Errorf("%s: exit %d, stdout %q", c.name, code, stdout)
		}
		if !strings.Contains(stderr, c.wantStderr) || !strings.Contains(stderr, "\n    sidecarctl internal bearer-keygen --class=<") {
			t.Errorf("%s: stderr %q, want %q and the usage", c.name, stderr, c.wantStderr)
		}
		if names := internalBearerKeygenTestDirectory(t, directory); len(names) != 0 {
			t.Fatalf("%s: the run wrote %v", c.name, slices.Collect(maps.Keys(names)))
		}
	}
}

// Files that are no pair are refused, and the run leaves the directory as it found it, each file's bytes and mode
// included: a credential file with no verify file, or with one holding no entry of its class or not its bearer's
// digest; a verify file holding the class's entry and no credential file; two entries of the class; another class's
// entry under the class's name; and a bearer whose digest another class's entry holds too.
func TestInternalBearerKeygenRefusesFilesThatAreNoPair(t *testing.T) {
	edgeBearer, edgeDigest := newInternalBearerKeygenTestBearer(t)
	_, otherDigest := newInternalBearerKeygenTestBearer(t)
	credentialFile := "current: " + edgeBearer + "\n"
	for _, c := range []struct {
		name string
		// each file's bytes, "" when none stands there
		credential string
		verify     string
		wantStderr string
	}{
		{name: "a credential file and no verify file", credential: credentialFile, wantStderr: "so this is a rotation, but no file stands at --verify"},
		{name: "no entry of the class", credential: credentialFile, verify: "evs: {class: evs, sha256: [" + edgeDigest + "]}\n", wantStderr: "holds no entry of class edge"},
		{name: "an entry without the bearer's digest", credential: credentialFile, verify: "edge: {class: edge, sha256: [" + otherDigest + "]}\n", wantStderr: "does not hold the digest of the bearer"},
		{name: "an entry of the class and no credential file", verify: "edge: {class: edge, sha256: [" + edgeDigest + "]}\n", wantStderr: "so the class has a bearer, but no file stands at --out"},
		{name: "two entries of the class", credential: credentialFile, verify: "edge: {class: edge, sha256: [" + edgeDigest + "]}\nedge-2: {class: edge, sha256: [" + otherDigest + "]}\n", wantStderr: "holds entries edge, edge-2, all of class edge"},
		{name: "another class's entry under the class's name", verify: "edge: {class: tlsedge, sha256: [" + otherDigest + "]}\n", wantStderr: "holds an entry named edge of class tlsedge"},
		{name: "the bearer's digest in another class's entry", credential: credentialFile, verify: "edge: {class: edge, sha256: [" + edgeDigest + "]}\ntlsedge: {class: tlsedge, sha256: [" + strings.ToUpper(edgeDigest) + "]}\n", wantStderr: "entry tlsedge of class tlsedge also holds the digest"},
	} {
		directory, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
		for _, file := range []struct {
			path string
			data string
		}{
			{path: outPath, data: c.credential},
			{path: verifyPath, data: c.verify},
		} {
			if file.data == "" {
				continue
			}
			// a mode the run would change to 0600 if it wrote
			if err := os.WriteFile(file.path, []byte(file.data), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		before := internalBearerKeygenTestDirectory(t, directory)
		code, stdout, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written") || !strings.Contains(stderr, c.wantStderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want %q", c.name, code, stdout, stderr, c.wantStderr)
		}
		if after := internalBearerKeygenTestDirectory(t, directory); !maps.Equal(after, before) {
			t.Errorf("%s: the run changed the directory", c.name)
		}
	}
}

// A file either of internal_gate.go's parsers refuses, or one an edit could not keep faithfully, is refused with
// nothing written: a credential file that does not parse, is empty, holds a field the format lacks or a bearer of
// another size; a verify file that does not parse, that api's verifier refuses, or that holds two documents or an
// alias; a symlink or a directory at either path; the two paths one file by a hard link; and a file past the most
// bytes read.
func TestInternalBearerKeygenRefusesFilesItCannotUse(t *testing.T) {
	edgeBearer, edgeDigest := newInternalBearerKeygenTestBearer(t)
	credentialFile := "current: " + edgeBearer + "\n"
	verifyFile := "edge: {class: edge, sha256: [" + edgeDigest + "]}\n"
	short := base64.RawURLEncoding.EncodeToString(make([]byte, server.DefaultInternalGateSettings().BearerByteCount-1))
	for _, c := range []struct {
		name string
		// makes the files of the case in directory, at the two paths
		setup      func(t *testing.T, directory string, outPath string, verifyPath string)
		wantStderr string
	}{
		{
			name: "a credential file that does not parse",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, "current: ["+edgeBearer+"\n", verifyPath, verifyFile)
			},
			wantStderr: "does not parse as a credential file",
		},
		{
			name: "an empty credential file",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, "", verifyPath, verifyFile)
			},
			wantStderr: "does not parse as a credential file",
		},
		{
			name: "a credential file with a field the format lacks",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile+"next: "+edgeBearer+"\n", verifyPath, verifyFile)
			},
			wantStderr: "does not parse as a credential file",
		},
		{
			name: "a credential file with a bearer of another size",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile+"previous: "+short+"\n", verifyPath, verifyFile)
			},
			wantStderr: "a bearer that is not 256 bits in base64url",
		},
		{
			name: "a verify file that does not parse",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile, verifyPath, "edge: {class: edge, sha256: ["+edgeDigest+"\n")
			},
			wantStderr: "does not parse as api's verify file",
		},
		{
			name: "a verify file api's verifier refuses",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile, verifyPath, verifyFile+"dns: {class: dns, sha256: ["+edgeDigest+"]}\n")
			},
			wantStderr: "is refused by api's verifier",
		},
		{
			name: "a verify file of two documents",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile, verifyPath, verifyFile+"---\nevs: {class: evs, sha256: ["+edgeDigest+"]}\n")
			},
			wantStderr: "holds 2 YAML documents",
		},
		{
			name: "a verify file with an alias",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile, verifyPath, "edge: &entry {class: edge, sha256: ["+edgeDigest+"]}\nedge-copy: *entry\n")
			},
			wantStderr: "anchors or aliases",
		},
		{
			name: "a symlink at --out",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, filepath.Join(directory, "target.yml"), credentialFile, verifyPath, verifyFile)
				if err := os.Symlink(filepath.Join(directory, "target.yml"), outPath); err != nil {
					t.Fatal(err)
				}
			},
			wantStderr: "--out=" + server.InternalGateCredentialFileName(server.InternalGateCallerEdge) + " is a symlink",
		},
		{
			name: "a symlink to nowhere at --verify",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				if err := os.Symlink(filepath.Join(directory, "nowhere.yml"), verifyPath); err != nil {
					t.Fatal(err)
				}
			},
			wantStderr: "--verify=" + server.InternalGateVerifyFileName + " is a symlink",
		},
		{
			name: "a directory at --out",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				if err := os.Mkdir(outPath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantStderr: "is not a regular file",
		},
		{
			name: "one file at both paths by a hard link",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile, "", "")
				if err := os.Link(outPath, verifyPath); err != nil {
					t.Fatal(err)
				}
			},
			wantStderr: "are one file",
		},
		{
			name: "a verify file past the most bytes read",
			setup: func(t *testing.T, directory string, outPath string, verifyPath string) {
				large := verifyFile + "#" + strings.Repeat("x", int(defaultInternalBearerKeygenSettings().maxFileByteCount)) + "\n"
				writeInternalBearerKeygenTestFiles(t, outPath, credentialFile, verifyPath, large)
			},
			wantStderr: "holds more than the 65536 bytes",
		},
	} {
		directory := t.TempDir()
		// the paths as given relative to the scratch directory, as messages then name them
		t.Chdir(directory)
		outPath := server.InternalGateCredentialFileName(server.InternalGateCallerEdge)
		verifyPath := server.InternalGateVerifyFileName
		c.setup(t, directory, outPath, verifyPath)
		before := internalBearerKeygenTestDirectory(t, directory)
		code, stdout, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written") || !strings.Contains(stderr, c.wantStderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want %q", c.name, code, stdout, stderr, c.wantStderr)
		}
		if after := internalBearerKeygenTestDirectory(t, directory); !maps.Equal(after, before) {
			t.Errorf("%s: the run changed the directory", c.name)
		}
	}
}

// Writes each file named, with its bytes; an empty path is skipped. A file of no bytes is still written.
func writeInternalBearerKeygenTestFiles(t *testing.T, outPath string, outData string, verifyPath string, verifyData string) {
	t.Helper()
	for _, file := range []struct {
		path string
		data string
	}{
		{path: outPath, data: outData},
		{path: verifyPath, data: verifyData},
	} {
		if file.path == "" {
			continue
		}
		if err := os.WriteFile(file.path, []byte(file.data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A random source that fails, gives all zeros or gives the bearer --out already sends gives no bearer, and the run
// writes nothing.
func TestInternalBearerKeygenWritesNothingWithoutAFreshBearer(t *testing.T) {
	seed := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	for _, c := range []struct {
		name   string
		random io.Reader
		// whether a first draw from the same bytes stands at the paths before the run
		drawnBefore bool
		wantStderr  string
	}{
		{name: "a source that fails", random: bytes.NewReader(make([]byte, 8)), wantStderr: "nothing was written: no bearer was drawn"},
		{name: "a source of zeros", random: bytes.NewReader(make([]byte, 64)), wantStderr: "nothing was written: the random source gave all zeros"},
		{name: "a source that repeats", random: bytes.NewReader(seed), drawnBefore: true, wantStderr: "nothing was written: the random source gave the bearer --out already sends"},
	} {
		directory, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
		args := internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)
		if c.drawnBefore {
			settings := defaultInternalBearerKeygenSettings()
			settings.random = bytes.NewReader(seed)
			if code, _, stderr := runInternalBearerKeygenTestWithSettings(settings, args...); code != 0 {
				t.Fatalf("%s: the first draw: exit %d: %s", c.name, code, stderr)
			}
		}
		before := internalBearerKeygenTestDirectory(t, directory)
		settings := defaultInternalBearerKeygenSettings()
		settings.random = c.random
		code, stdout, stderr := runInternalBearerKeygenTestWithSettings(settings, args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, c.wantStderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.name, code, stdout, stderr)
		}
		if after := internalBearerKeygenTestDirectory(t, directory); !maps.Equal(after, before) {
			t.Errorf("%s: the run changed the directory", c.name)
		}
	}
}

// A bearer or a digest of digits alone, which YAML would read back as a number if written plain, reads back as the
// text it is: bytes that encode to a bearer of zeros give files internal_gate.go reads, and an entry of a digest of
// digits alone parses in api's verifier with that digest.
func TestInternalBearerKeygenQuotesValuesThatLookLikeNumbers(t *testing.T) {
	// 0xd3 0x4d 0x34 encode as 0000 in base64url, so these bytes give a bearer of 43 zeros
	raw := bytes.Repeat([]byte{0xd3, 0x4d, 0x34}, 11)[:server.DefaultInternalGateSettings().BearerByteCount]
	zeros := strings.Repeat("0", 43)
	if bearer := base64.RawURLEncoding.EncodeToString(raw); bearer != zeros {
		t.Fatalf("the bytes encode as %q", bearer)
	}
	_, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	settings := defaultInternalBearerKeygenSettings()
	settings.random = bytes.NewReader(raw)
	if code, _, stderr := runInternalBearerKeygenTestWithSettings(settings, internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)...); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	bearer, verifier := loadInternalBearerKeygenTestFiles(t, server.InternalGateCallerEdge, outPath, verifyPath)
	if bearer != zeros {
		t.Fatal("the credential file does not send the bearer of zeros")
	}
	if caller, ok := authenticateInternalBearerKeygenTest(verifier, bearer); !ok || caller != server.InternalGateCallerEdge {
		t.Fatalf("api's verifier takes the bearer as %q %t", caller, ok)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	node := &yaml.Node{}
	if err := yaml.Unmarshal(data, node); err != nil {
		t.Fatal(err)
	}
	if value := node.Content[0].Content[1]; value.Tag != "!!str" || value.Style == 0 {
		t.Fatalf("the bearer is written as %s in style %d, not a quoted string", value.Tag, value.Style)
	}

	digits := strings.Repeat("0123456789", 7)[:64]
	verifyFile := &internalBearerKeygenVerifyFile{
		document:      &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}},
		callerEntries: map[string]*internalBearerKeygenVerifyEntryYaml{},
	}
	verifyData, err := verifyFile.withEntry("edge", internalBearerKeygenVerifyEntryNode(server.InternalGateCallerEdge, []string{digits}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.NewInternalGateVerifier(verifyData); err != nil {
		t.Fatalf("api's verifier refuses an entry of a digest of digits: %v", err)
	}
	verifyNode := &yaml.Node{}
	if err := yaml.Unmarshal(verifyData, verifyNode); err != nil {
		t.Fatal(err)
	}
	digestNode := verifyNode.Content[0].Content[1].Content[3].Content[0]
	if digestNode.Value != digits || digestNode.Tag != "!!str" || digestNode.Style == 0 {
		t.Fatalf("the digest is written as %s in style %d, not a quoted string", digestNode.Tag, digestNode.Style)
	}
}

// A run that cannot put the credential file, its second, in place puts the verify file back as it found it: a first
// draw whose --out appears meanwhile leaves no verify file, and a rotation whose --out turns into a directory
// meanwhile leaves the verify file's bytes as they were. Neither leaves a file beside.
func TestInternalBearerKeygenPutsTheVerifyFileBack(t *testing.T) {
	directory, outPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	sentinel := "a file that appeared during the run\n"
	settings := defaultInternalBearerKeygenSettings()
	settings.beforePlace = func(file *internalBearerKeygenFile) {
		if file.flagName == "--out" {
			if err := os.WriteFile(outPath, []byte(sentinel), 0o644); err != nil {
				t.Error(err)
			}
		}
	}
	code, stdout, stderr := runInternalBearerKeygenTestWithSettings(settings, internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written: --out="+outPath+" appeared during the run") {
		t.Fatalf("a first draw whose --out appeared: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	want := map[string]string{filepath.Base(outPath): "-rw-r--r-- " + sentinel}
	if after := internalBearerKeygenTestDirectory(t, directory); !maps.Equal(after, want) {
		t.Fatalf("a first draw whose --out appeared left %v", slices.Collect(maps.Keys(after)))
	}

	directory, outPath, verifyPath = newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	args := internalBearerKeygenTestArgs(server.InternalGateCallerEdge, outPath, verifyPath)
	if code, _, stderr := runInternalBearerKeygenTest(args...); code != 0 {
		t.Fatalf("the first draw: exit %d: %s", code, stderr)
	}
	verifyBefore, err := os.ReadFile(verifyPath)
	if err != nil {
		t.Fatal(err)
	}
	settings.beforePlace = func(file *internalBearerKeygenFile) {
		if file.flagName != "--out" {
			return
		}
		// a directory that is not empty, which no rename replaces
		if err := os.Remove(outPath); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(outPath, 0o700); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(outPath, "kept"), nil, 0o600); err != nil {
			t.Error(err)
		}
	}
	code, stdout, stderr = runInternalBearerKeygenTestWithSettings(settings, args...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing was written: --out:") {
		t.Fatalf("a rotation whose --out turned into a directory: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	verifyAfter, err := os.ReadFile(verifyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(verifyAfter, verifyBefore) {
		t.Fatal("the verify file was not put back")
	}
	if names := slices.Sorted(maps.Keys(internalBearerKeygenTestDirectory(t, directory))); !slices.Equal(names, []string{filepath.Base(outPath), filepath.Base(verifyPath)}) {
		t.Fatalf("the rotation left %v", names)
	}
}

// The platform listener's class (QD-1121) is drawn like any class internal_gate.go knows, beside the edge class's in
// one verify file. The listener's services load its file from a vault holding it, and api, loading its verify file
// from the same vault, takes the bearer as mxedge-platform's: the role-mail notice route (AbuseNoticeGatePath) lets it
// past its gate, to a body it refuses 400 with nothing filed, and the edge-client route (EdgeClientMintPath) refuses
// it 403, while the edge bearer passes the edge-client route's gate alone.
func TestInternalBearerKeygenDrawsThePlatformListenersClass(t *testing.T) {
	directory, edgeOutPath, verifyPath := newInternalBearerKeygenTestPaths(t, server.InternalGateCallerEdge)
	platformOutPath := filepath.Join(directory, server.InternalGateCredentialFileName(server.InternalGateCallerMxedgePlatform))
	if code, _, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerEdge, edgeOutPath, verifyPath)...); code != 0 {
		t.Fatalf("the edge class: exit %d: %s", code, stderr)
	}
	code, stdout, stderr := runInternalBearerKeygenTest(internalBearerKeygenTestArgs(server.InternalGateCallerMxedgePlatform, platformOutPath, verifyPath)...)
	if code != 0 {
		t.Fatalf("the platform listener's class: exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"wrote --out=" + platformOutPath + ", readable by its owner alone: class mxedge-platform's first credential file",
		"whose entry mxedge-platform, of class mxedge-platform",
		"its other entries, of classes edge, read as they did",
		"in vault as internal-gate-mxedge-platform.yml",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q: %s", want, stdout)
		}
	}
	platformBearer, verifier := loadInternalBearerKeygenTestFiles(t, server.InternalGateCallerMxedgePlatform, platformOutPath, verifyPath)
	edgeBearer, _ := loadInternalBearerKeygenTestFiles(t, server.InternalGateCallerEdge, edgeOutPath, verifyPath)
	if caller, ok := authenticateInternalBearerKeygenTest(verifier, platformBearer); !ok || caller != server.InternalGateCallerMxedgePlatform {
		t.Fatalf("api's verifier takes the platform listener's bearer as %q %t", caller, ok)
	}
	if caller, ok := authenticateInternalBearerKeygenTest(verifier, edgeBearer); !ok || caller != server.InternalGateCallerEdge {
		t.Fatalf("api's verifier takes the edge bearer as %q %t", caller, ok)
	}

	abuseNotice := handlers.NewInternalGateAbuseNoticeHandlers(&handlers.InternalGateAbuseNoticeDeps{
		Verifier: verifier,
		File: func(ctx context.Context, recipientLocalPart string, root string, verdicts []controller.AbuseNoticeMailVerdict) (*controller.AbuseNoticeResult, error) {
			t.Error("a notice was filed")
			return nil, errors.New("no notice is filed in this test")
		},
	})
	edgeClient := handlers.NewInternalGateEdgeClientHandlers(&handlers.InternalGateEdgeClientDeps{
		Verifier: verifier,
		Config: func() (*server.EdgeClientsConfig, error) {
			t.Error("edge.yml was read")
			return nil, errors.New("no edge.yml in this test")
		},
	})
	for _, c := range []struct {
		name    string
		handle  func(w http.ResponseWriter, r *http.Request)
		path    string
		bearer  string
		want    int
		wantGot string
	}{
		{name: "the platform listener's bearer on the role-mail route", handle: abuseNotice.PostAbuseNotice, path: server.AbuseNoticeGatePath, bearer: platformBearer, want: http.StatusBadRequest},
		{name: "the edge bearer on the role-mail route", handle: abuseNotice.PostAbuseNotice, path: server.AbuseNoticeGatePath, bearer: edgeBearer, want: http.StatusForbidden},
		{name: "the platform listener's bearer on the edge-client route", handle: edgeClient.PostEdgeClient, path: server.EdgeClientMintPath, bearer: platformBearer, want: http.StatusForbidden},
		{name: "the edge bearer on the edge-client route", handle: edgeClient.PostEdgeClient, path: server.EdgeClientMintPath, bearer: edgeBearer, want: http.StatusBadRequest},
	} {
		// a body no route takes, so a bearer past the gate is refused 400 and one at it 403, with nothing done
		request := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader("{"))
		request.Header.Set("Authorization", "Bearer "+c.bearer)
		recorder := httptest.NewRecorder()
		c.handle(recorder, request)
		if recorder.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, recorder.Code, c.want)
		}
	}
}

// The binary's own registry holds the command under the usage its doc gives, offering every class internal_gate.go
// knows, and the binary's help says where each file goes in vault, that no bearer is printed, and what deploys first.
func TestSidecarctlRegistersInternalBearerKeygen(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"internal", "bearer-keygen", "--class=edge"})
	if command == nil || command.words != "internal bearer-keygen" || !slices.Equal(args, []string{"--class=edge"}) {
		t.Fatalf("the binary has no internal bearer-keygen: %v", command)
	}
	classNames := []string{}
	for _, class := range server.InternalGateCallers() {
		classNames = append(classNames, string(class))
	}
	wantUsage := "--class=<" + strings.Join(classNames, "|") + "> --out=<file> --verify=<file>"
	if command.usage != wantUsage || !strings.Contains(command.usage, "|edge|") || !strings.Contains(command.usage, "mxedge-platform") {
		t.Fatalf("internal bearer-keygen's usage is %q", command.usage)
	}
	stdout := &bytes.Buffer{}
	if code := sidecarctlCommands.run([]string{"--help"}, stdout, io.Discard); code != 0 {
		t.Fatalf("--help exited %d", code)
	}
	for _, want := range []string{
		"sidecarctl internal bearer-keygen " + wantUsage,
		"internal-gate-<class>.yml for vault",
		"api's internal-gate-verify.yml",
		"Both are written 0600 and no bearer is printed",
		"place the verify file and deploy api first",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the help lacks %q", want)
		}
	}
}
