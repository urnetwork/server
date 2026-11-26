// `sidecarctl compute models mirror|index|serve|status` over server's ComputeModelBlobStore on a local blob store and a
// fake hub serving sidecar/testdata/compute/models/draft.json's files: the copy, the index pins written into the draft
// as indexed.json has them, serve taken only under the release key's signed sums listing the manifest, and each file's
// state; the command lines refused, and an env other than WARP_ENV's refused before a store opens.

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/handler"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// sidecar's fixtures for the mirror, beside this checkout in the monoroot.
const computeModelsVectorDirectory = "../../sidecar/testdata/compute/models"

// A fake hub: each url's bytes, every request recorded.
type fakeComputeModelsHub struct {
	stateLock sync.Mutex
	urlBytes  map[string][]byte
	requests  []string
}

// The url's bytes from offset, the whole file's size declared, or a refusal for a url the hub does not hold.
func (self *fakeComputeModelsHub) Open(ctx context.Context, fileUrl string, offset int64) (io.ReadCloser, int64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, fmt.Sprintf("%s @%d", fileUrl, offset))
	data, ok := self.urlBytes[fileUrl]
	if !ok {
		return nil, 0, fmt.Errorf("%w: the hub answered 404", model.ErrComputeModelFetchRefused)
	}
	return io.NopCloser(bytes.NewReader(data[offset:])), int64(len(data)), nil
}

// How many requests the hub saw.
func (self *fakeComputeModelsHub) requestCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.requests)
}

// A file's bytes as the vectors' README gives them: byte i is (i + seed) mod 251.
func computeModelsTestBytes(byteCount int, seed int) []byte {
	data := make([]byte, byteCount)
	for i := range data {
		data[i] = byte((i + seed) % 251)
	}
	return data
}

// A test release key, the same every run.
func computeModelsTestReleaseKey(t *testing.T, label string) ssh.Signer {
	t.Helper()
	seed := sha256.Sum256([]byte("urnetwork-server test model release key " + label))
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// One test's world: a local blob store standing in for the env's, the fake hub, a draft in a temp directory, the
// release key the build pins in this world, and the commands over them.
type computeModelsTestWorld struct {
	hub         *fakeComputeModelsHub
	directory   string
	draftPath   string
	release     ssh.Signer
	releaseKeys []ssh.PublicKey
	opened      int
	commands    *sidecarctlCommandSet
}

// A world with the draft written, its files on the hub, and the commands over server's store.
func newComputeModelsTestWorld(t *testing.T) *computeModelsTestWorld {
	t.Helper()
	server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 64*1024*1024)
	world := &computeModelsTestWorld{
		hub:       &fakeComputeModelsHub{urlBytes: map[string][]byte{}},
		directory: t.TempDir(),
		release:   computeModelsTestReleaseKey(t, "release"),
	}
	world.releaseKeys = []ssh.PublicKey{world.release.PublicKey()}
	draft, err := os.ReadFile(filepath.Join(computeModelsVectorDirectory, "draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	world.draftPath = filepath.Join(world.directory, "infer_manifest.json")
	if err := os.WriteFile(world.draftPath, draft, 0o644); err != nil {
		t.Fatal(err)
	}
	settings := world.settings(t)
	manifest, err := model.ParseComputeModelDraftManifest(draft, settings.infer)
	if err != nil {
		t.Fatal(err)
	}
	sha256Bytes := map[string][]byte{}
	for _, data := range [][]byte{computeModelsTestBytes(2621440, 0), computeModelsTestBytes(1048577, 7)} {
		sha256Bytes[computeModelsSha256(data)] = data
	}
	for _, file := range model.ComputeModelManifestFiles(manifest) {
		world.hub.urlBytes[file.Url] = sha256Bytes[file.Sha256]
	}

	world.commands = newSidecarctlCommandSet()
	for _, command := range []*sidecarctlCommand{
		{words: "compute models mirror", usage: "<env> <draft-manifest>", run: func(args []string, out io.Writer) error {
			return runComputeModelsMirrorWithSettings(args, world.settings(t), out)
		}},
		{words: "compute models index", usage: "<env> <draft-manifest>", run: func(args []string, out io.Writer) error {
			return runComputeModelsIndexWithSettings(args, world.settings(t), out)
		}},
		{words: "compute models serve", usage: "<env> <manifest>", run: func(args []string, out io.Writer) error {
			return runComputeModelsServeWithSettings(args, world.settings(t), out)
		}},
		{words: "compute models status", usage: "<env> <manifest>", run: func(args []string, out io.Writer) error {
			return runComputeModelsStatusWithSettings(args, world.settings(t), out)
		}},
	} {
		world.commands.add(command)
	}
	return world
}

// The commands' settings in this world: the fake hub at hub.example, 1 MiB chunks, this world's release keys, and the
// env local, over server's store on the local blob store.
func (self *computeModelsTestWorld) settings(t *testing.T) *computeModelsCommandSettings {
	settings := defaultComputeModelsCommandSettings()
	settings.mirror.HubHost = "hub.example"
	settings.mirror.HubRetryInterval = 0
	settings.mirror.StagingDirectory = t.TempDir()
	settings.infer.ModelChunkByteCount = 1024 * 1024
	settings.releaseKeys = func() []ssh.PublicKey { return self.releaseKeys }
	settings.openMirror = func(ctx context.Context, env string, settings *computeModelsCommandSettings) (*model.ComputeModelMirror, error) {
		self.opened += 1
		if env != "local" {
			return nil, errors.New("this process runs as WARP_ENV=local")
		}
		return model.NewComputeModelMirror(server.NewComputeModelBlobStore(nil), self.hub, settings.mirror, settings.infer, nil, nil), nil
	}
	return settings
}

// Runs one command line: its exit code and output.
func (self *computeModelsTestWorld) run(args ...string) (int, string, string) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := self.commands.run(args, stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// Runs one command line that must succeed: its output.
func (self *computeModelsTestWorld) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := self.run(args...)
	if code != 0 {
		t.Fatalf("%q exited %d: %s%s", args, code, stdout, stderr)
	}
	return stdout
}

// The state line status prints for each file, by its SHA-256.
func (self *computeModelsTestWorld) states(t *testing.T, manifestPath string) map[string]string {
	t.Helper()
	sha256States := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(self.mustRun(t, "compute", "models", "status", "local", manifestPath)), "\n")[1:] {
		fields := strings.Fields(line)
		sha256States[fields[1]] = fields[0]
	}
	return sha256States
}

// The four commands are the binary's own.
func TestComputeModelsCommandsAreRegistered(t *testing.T) {
	for _, words := range []string{"compute models mirror", "compute models index", "compute models serve", "compute models status"} {
		command, _ := sidecarctlCommands.find(strings.Fields(words))
		if command == nil || command.words != words {
			t.Errorf("%q is not registered", words)
		}
	}
}

// A line no command takes exits 2 with its usage and opens no store: a positional missing or extra, an env that names
// no directory, an unknown flag, and serve without its sums or their signature.
func TestComputeModelsCommandsRefuseBadLines(t *testing.T) {
	world := newComputeModelsTestWorld(t)
	for _, args := range [][]string{
		{"compute", "models", "mirror", "local"},
		{"compute", "models", "mirror", "local", world.draftPath, "extra"},
		{"compute", "models", "mirror", "../main", world.draftPath},
		{"compute", "models", "mirror", "local", world.draftPath, "--unknown=x"},
		{"compute", "models", "index", world.draftPath},
		{"compute", "models", "status", "local"},
		{"compute", "models", "serve", "local", world.draftPath},
		{"compute", "models", "serve", "local", world.draftPath, "--sums=SHA256SUMS"},
		{"compute", "models", "serve", "local", world.draftPath, "--signature=SHA256SUMS.sig"},
	} {
		code, _, stderr := world.run(args...)
		if code != 2 || !strings.Contains(stderr, "Usage:") {
			t.Errorf("%q exited %d: %s", args, code, stderr)
		}
	}
	if world.opened != 0 {
		t.Fatalf("a refused line opened the store %d times", world.opened)
	}
	code, _, stderr := world.run("compute", "models", "mirror", "staging", world.draftPath)
	if code != 1 || !strings.Contains(stderr, "nothing was read or written in staging") || world.hub.requestCount() != 0 {
		t.Fatalf("another env: %d, %s", code, stderr)
	}
}

// The production open reaches only the env WARP_ENV names, and only with a blob store: no WARP_ENV and another env are
// refused before a store opens, and the env named opens server's mirror over the store it selects.
func TestComputeModelsOpenRefusesAnotherEnv(t *testing.T) {
	settings := defaultComputeModelsCommandSettings()
	ctx := context.Background()
	t.Setenv("WARP_ENV", "")
	if _, err := openServerComputeModelMirror(ctx, "local", settings); err == nil || !strings.Contains(err.Error(), "WARP_ENV is not set") {
		t.Fatalf("no WARP_ENV: %v", err)
	}
	t.Setenv("WARP_ENV", "local")
	if _, err := openServerComputeModelMirror(ctx, "staging", settings); err == nil || !strings.Contains(err.Error(), "WARP_ENV=local") {
		t.Fatalf("another env: %v", err)
	}
	server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1024*1024)
	mirror, err := openServerComputeModelMirror(ctx, "local", settings)
	if err != nil || mirror == nil {
		t.Fatalf("the env named: %v", err)
	}
	record, err := mirror.Status(ctx, computeModelsSha256([]byte("no such file")))
	if err != nil || record.State != model.ComputeModelFileStateAbsent {
		t.Fatalf("a file the store does not hold: %+v, %v", record, err)
	}
}

// mirror copies each file the draft names once, the file under two models among them, into server's store, never
// served; a second run copies nothing; --recopy copies a copied file again.
func TestComputeModelsMirrorCopiesTheDraft(t *testing.T) {
	world := newComputeModelsTestWorld(t)
	stdout := world.mustRun(t, "compute", "models", "mirror", "local", world.draftPath)
	if strings.Count(stdout, "copied  ") != 2 || !strings.Contains(stdout, "test-mirror-b-gpu.litertlm (test-mirror-a, test-mirror-b)") {
		t.Fatalf("mirror printed %s", stdout)
	}
	if world.hub.requestCount() != 2 {
		t.Fatalf("the hub saw %d requests", world.hub.requestCount())
	}
	for sha256Text, state := range world.states(t, world.draftPath) {
		if state != "copied" {
			t.Fatalf("%s reads %s", sha256Text, state)
		}
	}
	routes := server.NewComputeModelBlobStore(nil)
	for sha256Text := range world.states(t, world.draftPath) {
		record, err := routes.FileRecord(context.Background(), sha256Text)
		if err != nil || record.State != handler.ComputeModelFileStateCopied {
			t.Fatalf("the routes' store reads %s as %+v, %v", sha256Text, record, err)
		}
	}
	stdout = world.mustRun(t, "compute", "models", "mirror", "local", world.draftPath)
	if strings.Count(stdout, "already copied") != 2 || world.hub.requestCount() != 2 {
		t.Fatalf("a second run printed %s after %d requests", stdout, world.hub.requestCount())
	}
	stdout = world.mustRun(t, "compute", "models", "mirror", "local", world.draftPath, "--recopy")
	if strings.Count(stdout, "copied again") != 2 || world.hub.requestCount() != 4 {
		t.Fatalf("--recopy printed %s after %d requests", stdout, world.hub.requestCount())
	}
}

// index writes each file's index_sha256 into the draft, which then is indexed.json byte for byte and parses as a
// released manifest, each pin that of the index the routes' store holds; a second run leaves the file as it is; a
// draft whose file is not copied is left as it was, and so is one already pinning another index.
func TestSidecarctlComputeModelsWritesIndexPin(t *testing.T) {
	world := newComputeModelsTestWorld(t)
	draft, err := os.ReadFile(world.draftPath)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := world.run("compute", "models", "index", "local", world.draftPath)
	if code != 1 || !strings.Contains(stdout, "is not copied") || !strings.Contains(stderr, "left as it was") {
		t.Fatalf("index before mirror: %d, %s%s", code, stdout, stderr)
	}
	if after, _ := os.ReadFile(world.draftPath); !bytes.Equal(after, draft) {
		t.Fatal("a failed index changed the draft")
	}

	world.mustRun(t, "compute", "models", "mirror", "local", world.draftPath)
	stdout = world.mustRun(t, "compute", "models", "index", "local", world.draftPath)
	if strings.Count(stdout, "indexed  ") != 2 || !strings.Contains(stdout, "sign it with the release") {
		t.Fatalf("index printed %s", stdout)
	}
	pinned, err := os.ReadFile(world.draftPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(computeModelsVectorDirectory, "indexed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pinned, want) {
		t.Fatalf("the pinned draft is not indexed.json:\n%s", pinned)
	}
	settings := world.settings(t)
	manifest, err := sidecar.ParseInferManifest(pinned, settings.infer)
	if err != nil {
		t.Fatal(err)
	}
	routes := server.NewComputeModelBlobStore(nil)
	for _, file := range model.ComputeModelManifestFiles(manifest) {
		reader, err := routes.OpenIndex(context.Background(), file.Sha256)
		if err != nil {
			t.Fatal(err)
		}
		index, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sidecar.ParseInferModelIndex(index, file, settings.infer); err != nil {
			t.Fatalf("%s's stored index under the draft's pin: %v", file.Sha256, err)
		}
	}
	for sha256Text, state := range world.states(t, world.draftPath) {
		if state != "indexed" {
			t.Fatalf("%s reads %s", sha256Text, state)
		}
	}

	stdout = world.mustRun(t, "compute", "models", "index", "local", world.draftPath)
	if !strings.Contains(stdout, "already carries every pin") {
		t.Fatalf("a second index printed %s", stdout)
	}
	if after, _ := os.ReadFile(world.draftPath); !bytes.Equal(after, want) {
		t.Fatal("a second index changed the manifest")
	}

	moved := bytes.Replace(want, []byte(`"index_sha256": "2efd`), []byte(`"index_sha256": "3efd`), 1)
	if bytes.Equal(moved, want) {
		t.Fatal("the pin to move is not in indexed.json")
	}
	if err := os.WriteFile(world.draftPath, moved, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = world.run("compute", "models", "index", "local", world.draftPath)
	if code != 1 || !strings.Contains(stderr, "already pins the index") {
		t.Fatalf("a draft pinning another index: %d, %s", code, stderr)
	}
	if after, _ := os.ReadFile(world.draftPath); !bytes.Equal(after, moved) {
		t.Fatal("a refused index changed the draft")
	}
}

// The sums the release key signs over a manifest's line and others, as ursums writes them.
func computeModelsTestSums(manifestSha256 string) []byte {
	return []byte(computeModelsSha256([]byte("ur")) + "  ur-2026.11.26-1-linux-amd64.tar.gz\n" +
		manifestSha256 + "  infer_manifest.json\n" +
		computeModelsSha256([]byte("release")) + "  ur-release.json\n")
}

// Writes the sums and their signature by signer under namespace into the world's directory: their paths.
func (self *computeModelsTestWorld) writeSums(t *testing.T, sums []byte, signer ssh.Signer, namespace string) (string, string) {
	t.Helper()
	signature, err := sidecar.ComputeSshsigSign(bytes.NewReader(make([]byte, 64)), signer, namespace, sums)
	if err != nil {
		t.Fatal(err)
	}
	sumsPath := filepath.Join(self.directory, "SHA256SUMS")
	signaturePath := filepath.Join(self.directory, "SHA256SUMS.sig")
	if err := os.WriteFile(sumsPath, sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, signature, 0o644); err != nil {
		t.Fatal(err)
	}
	return sumsPath, signaturePath
}

// serve takes a released manifest only once SHA256SUMS, signed by the release key under ur-release, lists the
// manifest's SHA-256: with no release key pinned, by another key, under another namespace, over sums altered after
// signing, sums that do not list the manifest, and a signed draft that is no released manifest, nothing is served and
// no store opens; then the files serve, each checked, and the routes' store reads them served.
func TestSidecarctlComputeModelsServeChecksReleaseSums(t *testing.T) {
	world := newComputeModelsTestWorld(t)
	draft, err := os.ReadFile(world.draftPath)
	if err != nil {
		t.Fatal(err)
	}
	world.mustRun(t, "compute", "models", "mirror", "local", world.draftPath)
	world.mustRun(t, "compute", "models", "index", "local", world.draftPath)
	released, err := os.ReadFile(world.draftPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestSha256 := computeModelsSha256(released)
	serve := func(sumsPath string, signaturePath string) (int, string, string) {
		return world.run("compute", "models", "serve", "local", world.draftPath, "--sums="+sumsPath, "--signature="+signaturePath)
	}
	openedBefore := world.opened

	world.releaseKeys = nil
	sumsPath, signaturePath := world.writeSums(t, computeModelsTestSums(manifestSha256), world.release, "ur-release")
	code, _, stderr := serve(sumsPath, signaturePath)
	if code != 1 || !strings.Contains(stderr, "no release key exists to check") {
		t.Fatalf("no release key: %d, %s", code, stderr)
	}
	world.releaseKeys = []ssh.PublicKey{world.release.PublicKey()}

	refusals := map[string]func() (string, string){
		"another key": func() (string, string) {
			return world.writeSums(t, computeModelsTestSums(manifestSha256), computeModelsTestReleaseKey(t, "other"), "ur-release")
		},
		"another namespace": func() (string, string) {
			return world.writeSums(t, computeModelsTestSums(manifestSha256), world.release, sidecar.ComputePolicyNamespace)
		},
		"sums altered after signing": func() (string, string) {
			sumsPath, signaturePath := world.writeSums(t, computeModelsTestSums(manifestSha256), world.release, "ur-release")
			if err := os.WriteFile(sumsPath, computeModelsTestSums(computeModelsSha256([]byte("another manifest"))), 0o644); err != nil {
				t.Fatal(err)
			}
			return sumsPath, signaturePath
		},
		"sums not listing the manifest": func() (string, string) {
			return world.writeSums(t, computeModelsTestSums(computeModelsSha256(draft)), world.release, "ur-release")
		},
	}
	for name, write := range refusals {
		sumsPath, signaturePath := write()
		code, _, stderr := serve(sumsPath, signaturePath)
		if code != 1 || !strings.Contains(stderr, "nothing was served") {
			t.Fatalf("%s: %d, %s", name, code, stderr)
		}
	}
	// a draft the release key signed is still no released manifest: its pins are missing
	if err := os.WriteFile(world.draftPath, draft, 0o644); err != nil {
		t.Fatal(err)
	}
	sumsPath, signaturePath = world.writeSums(t, computeModelsTestSums(computeModelsSha256(draft)), world.release, "ur-release")
	code, _, stderr = serve(sumsPath, signaturePath)
	if code != 1 || !strings.Contains(stderr, "is no released manifest") {
		t.Fatalf("a signed draft: %d, %s", code, stderr)
	}
	if world.opened != openedBefore {
		t.Fatalf("a refused serve opened the store %d times", world.opened-openedBefore)
	}
	for sha256Text, state := range world.states(t, world.draftPath) {
		if state != "indexed" {
			t.Fatalf("%s reads %s after refusals", sha256Text, state)
		}
	}

	if err := os.WriteFile(world.draftPath, released, 0o644); err != nil {
		t.Fatal(err)
	}
	sumsPath, signaturePath = world.writeSums(t, computeModelsTestSums(manifestSha256), world.release, "ur-release")
	code, stdout, stderr := serve(sumsPath, signaturePath)
	if code != 0 || strings.Count(stdout, "served  ") != 2 || !strings.Contains(stdout, "lists "+manifestSha256+" as infer_manifest.json") {
		t.Fatalf("serve: %d, %s%s", code, stdout, stderr)
	}
	routes := server.NewComputeModelBlobStore(nil)
	manifest, err := sidecar.ParseInferManifest(released, world.settings(t).infer)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range model.ComputeModelManifestFiles(manifest) {
		record, err := routes.FileRecord(context.Background(), file.Sha256)
		if err != nil || record.State != handler.ComputeModelFileStateServed || record.IndexSha256 != file.IndexSha256 {
			t.Fatalf("the routes' store reads %s as %+v, %v", file.Sha256, record, err)
		}
	}
	code, stdout, stderr = serve(sumsPath, signaturePath)
	if code != 0 || strings.Count(stdout, "served  ") != 2 {
		t.Fatalf("serve again: %d, %s%s", code, stdout, stderr)
	}
}

// A chunk altered in the store after index fails serve for that file alone, which stays indexed and unserved while
// the other serves.
func TestSidecarctlComputeModelsServeChecksEveryStoredChunk(t *testing.T) {
	world := newComputeModelsTestWorld(t)
	world.mustRun(t, "compute", "models", "mirror", "local", world.draftPath)
	world.mustRun(t, "compute", "models", "index", "local", world.draftPath)
	released, err := os.ReadFile(world.draftPath)
	if err != nil {
		t.Fatal(err)
	}
	altered := computeModelsTestBytes(1024*1024, 0)
	altered[100] ^= 0x01
	alteredPath := filepath.Join(world.directory, "altered-chunk")
	if err := os.WriteFile(alteredPath, altered, 0o600); err != nil {
		t.Fatal(err)
	}
	fileA := "35aeff7e048974ee23c365c8faf6bcb868ca0529c69309a00f6cde98bfbf89ce"
	if err := server.NewComputeModelBlobStore(nil).PutChunk(context.Background(), fileA, 0, alteredPath); err != nil {
		t.Fatal(err)
	}
	sumsPath, signaturePath := world.writeSums(t, computeModelsTestSums(computeModelsSha256(released)), world.release, "ur-release")
	code, stdout, stderr := world.run("compute", "models", "serve", "local", world.draftPath, "--sums="+sumsPath, "--signature="+signaturePath)
	if code != 1 || !strings.Contains(stdout, "failed  "+fileA) || !strings.Contains(stdout, "chunk 0") || strings.Count(stdout, "served  ") != 1 {
		t.Fatalf("serve over an altered chunk: %d, %s%s", code, stdout, stderr)
	}
	states := world.states(t, world.draftPath)
	if states[fileA] != "indexed" || states["7c7a4592cb23381f1bb73356d5bff522b62e870243fa1d4bf64189ddd2cba5c7"] != "served" {
		t.Fatalf("the states %v", states)
	}
}

// status lists each file the manifest names once, with its state as the mirror moves it: absent, copied, indexed and
// served, an indexed or served file with its index pin and the others with none.
func TestSidecarctlComputeModelsStatus(t *testing.T) {
	world := newComputeModelsTestWorld(t)
	stdout := world.mustRun(t, "compute", "models", "status", "local", world.draftPath)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "STATE") {
		t.Fatalf("status printed %s", stdout)
	}
	for _, line := range lines[1:] {
		if fields := strings.Fields(line); fields[0] != "absent" || fields[3] != "-" {
			t.Fatalf("a file not mirrored reads %q", line)
		}
	}
	if world.hub.requestCount() != 0 {
		t.Fatal("status asked the hub")
	}
	world.mustRun(t, "compute", "models", "mirror", "local", world.draftPath)
	for _, state := range world.states(t, world.draftPath) {
		if state != "copied" {
			t.Fatalf("after mirror: %s", state)
		}
	}
	world.mustRun(t, "compute", "models", "index", "local", world.draftPath)
	released, err := os.ReadFile(world.draftPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout = world.mustRun(t, "compute", "models", "status", "local", world.draftPath)
	if !strings.Contains(stdout, "indexed  35aeff7e048974ee23c365c8faf6bcb868ca0529c69309a00f6cde98bfbf89ce  2621440  2efd2b8d5093e0523a4cf45604b02185378275dd76327b75085588cea23176f7  test-mirror-a.litertlm (test-mirror-a)") {
		t.Fatalf("after index: %s", stdout)
	}
	sumsPath, signaturePath := world.writeSums(t, computeModelsTestSums(computeModelsSha256(released)), world.release, "ur-release")
	world.mustRun(t, "compute", "models", "serve", "local", world.draftPath, "--sums="+sumsPath, "--signature="+signaturePath)
	for sha256Text, state := range world.states(t, world.draftPath) {
		if state != "served" {
			t.Fatalf("%s after serve: %s", sha256Text, state)
		}
	}
}
