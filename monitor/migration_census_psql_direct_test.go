// The optional direct-owned carrier runs the real image psql, never a SQL or
// output substitute. It requires a private network namespace and read-only,
// hash-pinned image, proof and launcher mounts supplied by the external owner.
//
// Set URNETWORK_MIGRATION_TEST_PSQL_BACKEND=direct-owned and supply the absolute
// URNETWORK_MIGRATION_TEST_PSQL_DIRECT_PROOF path and its _SHA256. The strict
// JSON schema below pins the complete image receipt and FILES manifest plus
// the exact executable/startup-library closure. Physical paths in Files, Loader, Psql
// and LibraryDirectories are relative to Rootfs; the remaining paths are
// absolute. The host namespace handle must be a read-only nsfs bind mount.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Omission retains the original Docker carrier; direct mode is never fallback.
type migrationCensusNativeBackend struct {
	container string
	direct    *migrationCensusNativeDirect
}

// The externally pinned proof joins immutable whole-image evidence, the
// executable closure and the physical isolation of this particular fixture.
type migrationCensusNativeDirectProof struct {
	Schema               int               `json:"schema"`
	Rootfs               string            `json:"rootfs"`
	ImageReceipt         string            `json:"image_receipt"`
	ImageReceiptSha256   string            `json:"image_receipt_sha256"`
	ImageFiles           string            `json:"image_files"`
	ImageFilesSha256     string            `json:"image_files_sha256"`
	Loader               string            `json:"loader"`
	Psql                 string            `json:"psql"`
	LibraryDirectories   []string          `json:"library_directories"`
	Files                map[string]string `json:"files"`
	Launcher             string            `json:"launcher"`
	HostNetworkNamespace string            `json:"host_network_namespace"`
}

// All state belongs to this test fixture; no process PATH, loader or pool hook
// is changed. The child receives no inherited secrets or PostgreSQL defaults.
type migrationCensusNativeDirect struct {
	proof     migrationCensusNativeDirectProof
	loader    string
	psql      string
	libraries []string
	home      string
	authority string
}

// Pure selection is exercised in ordinary non-root tests before any spawn.
func migrationCensusNativeBackendKind(backend, container, proof, digest string) (string, error) {
	hexDigest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	switch backend {
	case "", "docker":
		if !hexDigest.MatchString(container) || proof != "" || digest != "" {
			return "", errors.New("native Docker fixture requires only its exact owned container id")
		}
		return "docker", nil
	case "direct-owned":
		if container != "" || !migrationCensusNativeAbsolutePath(proof) || !hexDigest.MatchString(digest) {
			return "", errors.New("direct native fixture requires an exclusive absolute pinned proof")
		}
		return "direct-owned", nil
	default:
		return "", errors.New("unknown native psql fixture backend")
	}
}

// Physical paths and image-relative names have no traversal or alias spelling.
func migrationCensusNativeAbsolutePath(path string) bool {
	return path != "/" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n:")
}

// Selected aliases may resolve only into this root, never through image
// absolute symlinks into the observer's host libraries or executable paths.
func migrationCensusNativeImagePath(root, relative string) (string, error) {
	if relative == "." || !filepath.IsLocal(relative) || filepath.Clean(relative) != relative || strings.ContainsAny(relative, "\x00\r\n:") {
		return "", errors.New("native image path is not a canonical relative name")
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", errors.New("native image path escaped its pinned root")
	}
	return path, nil
}

// Bound external metadata before decoding; unknown or trailing fields fail.
func migrationCensusNativeProof(raw []byte) (migrationCensusNativeDirectProof, error) {
	var proof migrationCensusNativeDirectProof
	if len(raw) > 1024*1024 {
		return proof, errors.New("direct native proof exceeds its metadata bound")
	}
	fields, objectErr := migrationCensusFrame(string(raw))
	_, filesErr := migrationCensusFrame(string(fields["files"]))
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if objectErr != nil || filesErr != nil || decoder.Decode(&proof) != nil || decoder.Decode(new(any)) != io.EOF || proof.Schema != 1 {
		return proof, errors.New("invalid direct native proof schema")
	}
	hexDigest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, path := range []string{proof.Rootfs, proof.ImageReceipt, proof.ImageFiles, proof.Launcher, proof.HostNetworkNamespace} {
		if !migrationCensusNativeAbsolutePath(path) {
			return proof, errors.New("direct native proof contains an invalid absolute path")
		}
	}
	if proof.ImageReceipt == proof.ImageFiles || !hexDigest.MatchString(proof.ImageReceiptSha256) || !hexDigest.MatchString(proof.ImageFilesSha256) || len(proof.LibraryDirectories) == 0 || len(proof.Files) < 2 ||
		proof.Files[proof.Loader] == "" || proof.Files[proof.Psql] == "" || proof.Loader == proof.Psql {
		return proof, errors.New("direct native proof lacks its whole-image or executable closure")
	}
	for path, digest := range proof.Files {
		if !filepath.IsLocal(path) || path == "." || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n:") || !hexDigest.MatchString(digest) {
			return proof, errors.New("direct native proof contains an invalid closure entry")
		}
	}
	directories := map[string]bool{}
	for _, path := range proof.LibraryDirectories {
		if path == "." || !filepath.IsLocal(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n:") || directories[path] {
			return proof, errors.New("direct native proof contains an invalid library directory")
		}
		directories[path] = true
	}
	return proof, nil
}

// Kernel mount metadata, not mode bits or a proof's boolean, establishes RO.
type migrationCensusNativeMount struct {
	path     string
	readOnly bool
	kind     string
}

// Select the effective mount, including any more-specific writable override.
func migrationCensusNativeMountAt(raw, path string) (migrationCensusNativeMount, error) {
	var selected migrationCensusNativeMount
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			return selected, errors.New("invalid native mount metadata")
		}
		fields, filesystem := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(fields) < 6 || len(filesystem) < 3 {
			return selected, errors.New("incomplete native mount metadata")
		}
		mountPath := unescape.Replace(fields[4])
		if (path == mountPath || strings.HasPrefix(path, strings.TrimSuffix(mountPath, "/")+"/")) && len(selected.path) < len(mountPath) {
			selected = migrationCensusNativeMount{path: mountPath, readOnly: strings.Contains(","+fields[5]+",", ",ro,"), kind: filesystem[0]}
		}
	}
	if selected.path == "" || !selected.readOnly {
		return selected, errors.New("native evidence is not on its effective read-only mount")
	}
	return selected, nil
}

// Every mapped image file must be a regular, hash-pinned member of the closure.
func migrationCensusNativeFileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("native pinned file is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("native pinned file is not regular")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", errors.New("native pinned file could not be hashed")
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

// Metadata cannot allocate without limit before its hash or schema is checked.
func migrationCensusNativeRead(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("native metadata is unavailable")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		return nil, errors.New("native metadata exceeds its read bound")
	}
	return raw, nil
}

// This wrapper only execs the real pinned psql with unchanged production argv.
func (self *migrationCensusNativeDirect) launcher() string {
	return "#!/bin/sh\nexec " + shellSingleQuote(self.loader) + " --inhibit-cache --library-path " + shellSingleQuote(strings.Join(self.libraries, ":")) + " " + shellSingleQuote(self.psql) + " \"$@\"\n"
}

// No inherited HOME, PATH, libpq service, password file, TLS, GSS or loader
// configuration can redirect a private loopback connection or load host code.
func (self *migrationCensusNativeDirect) environment() []string {
	return []string{"PATH=" + filepath.Dir(self.proof.Launcher), "HOME=" + self.home, "LC_ALL=C", "LANG=C", "PGHOSTADDR=127.0.0.1", "PGSSLMODE=disable", "PGGSSENCMODE=disable", "PGPASSFILE=/nonexistent", "PGSERVICE=", "PGSERVICEFILE=/nonexistent", "PGSYSCONFDIR=" + self.home, "PSQL_HISTORY=/nonexistent", "OPENSSL_CONF=/nonexistent"}
}

// An explicit portable owner is required even before disposable DB creation.
func migrationCensusNativeDirectOwner(environment, root, usePortable, allowUnmanaged, authority string) error {
	host, port, err := net.SplitHostPort(authority)
	portNumber, portErr := strconv.Atoi(port)
	if environment != "local" || !migrationCensusNativeAbsolutePath(root) || usePortable != "1" || allowUnmanaged != "1" ||
		err != nil || host != "127.0.0.1" || portErr != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
		return errors.New("direct native psql requires the exact private portable TestEnv owner")
	}
	return nil
}

// Refuse arbitrary databases, host endpoints, alternate ports or missing
// portable-owner flags before the first actual psql child may connect.
func migrationCensusNativeDirectPort(host string, port uint16, database, authority string) (int, error) {
	expectedHost, expectedPort, err := net.SplitHostPort(authority)
	if err != nil || expectedHost != "127.0.0.1" || expectedPort != strconv.Itoa(int(port)) || host != expectedHost || port == 0 ||
		!regexp.MustCompile(`^test_[0-9]+_[0-9a-f]{32}$`).MatchString(database) {
		return 0, errors.New("direct native psql is not bound to the exact private TestEnv database and authority")
	}
	return int(port), nil
}

// Require the loader's actual result, not a supplied list claiming host DSOs
// are image-owned. The kernel's live maps are checked again at the writer gate.
func (self *migrationCensusNativeDirect) validateLibraries(paths []string, complete bool) error {
	seen := map[string]bool{}
	for _, path := range paths {
		if !migrationCensusNativeAbsolutePath(path) {
			return errors.New("native loader returned a nonphysical library path")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(resolved, self.proof.Rootfs+"/") {
			return errors.New("native loader or process mapped a host library")
		}
		relative, err := filepath.Rel(self.proof.Rootfs, resolved)
		if err != nil || self.proof.Files[relative] == "" {
			return errors.New("native loader or process escaped the pinned closure")
		}
		seen[relative] = true
	}
	if len(seen) == 0 {
		return errors.New("native library observation was empty")
	}
	if complete {
		for relative := range self.proof.Files {
			if relative != self.proof.Psql && !seen[relative] {
				return errors.New("native loader did not resolve the complete pinned closure")
			}
		}
	} else if !seen[self.proof.Psql] {
		return errors.New("live native process did not map the pinned psql executable")
	}
	return nil
}

// The Linux virtual DSO has no file; every other loader result must name one
// real image file. Missing libraries and unrecognized diagnostics are failures.
func migrationCensusNativeLoaderPaths(output string) ([]string, error) {
	paths := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "linux-vdso.so.1" && strings.HasPrefix(fields[1], "(0x") {
			continue
		}
		switch {
		case len(fields) == 4 && fields[1] == "=>" && strings.HasPrefix(fields[3], "(0x"):
			paths = append(paths, fields[2])
		case len(fields) == 2 && strings.HasPrefix(fields[1], "(0x"):
			paths = append(paths, fields[0])
		default:
			return nil, errors.New("native loader closure was missing or malformed")
		}
	}
	return paths, nil
}

// Inspect the actual live psql only after its real first responses, without
// changing stdin pacing, the writer barrier, SQL, snapshot or physical pid.
func (self *migrationCensusNativeBackend) validateProcess(pid int) error {
	if self.direct == nil {
		return nil
	}
	raw, err := migrationCensusNativeRead(fmt.Sprintf("/proc/%d/maps", pid), 4*1024*1024)
	if err != nil {
		return errors.New("native psql process mappings were unavailable")
	}
	paths := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || len(fields) > 6 {
			return errors.New("native psql process mapping was malformed")
		}
		if len(fields) == 6 && !strings.HasPrefix(fields[5], "[") {
			paths = append(paths, fields[5])
		}
	}
	return self.direct.validateLibraries(paths, false)
}

// Both existing observations use this carrier; Docker argv and behavior stay
// unchanged, and direct mode executes the byte-identical production command.
func (self *migrationCensusNativeBackend) command(ctx context.Context, command string) *exec.Cmd {
	var child *exec.Cmd
	if self.direct == nil {
		child = exec.CommandContext(ctx, "docker", "exec", "-i", self.container, "/bin/sh", "-c", command)
	} else {
		child = exec.CommandContext(ctx, "/bin/sh", "-c", command)
		child.Env, child.Dir = self.direct.environment(), self.direct.home
	}
	child.WaitDelay = 5 * time.Second
	return child
}

// Kernel nsfs metadata and distinct physical handles are both mandatory.
func migrationCensusNativePrivateNamespace(mount migrationCensusNativeMount, path string, private, host os.FileInfo) error {
	if mount.path != path || !mount.readOnly || mount.kind != "nsfs" || private == nil || host == nil || os.SameFile(private, host) {
		return errors.New("direct native psql lacks a distinct physical private network namespace")
	}
	return nil
}

// Opt-in fixture construction authenticates all external evidence before a
// TestEnv may create its disposable database. Invalid bindings never skip.
func migrationCensusNativeFixture(t *testing.T) *migrationCensusNativeBackend {
	t.Helper()
	if os.Getenv("URNETWORK_MIGRATION_CENSUS_NATIVE_PSQL") != "1" {
		t.Skip("requires explicitly owned native psql migration-census fixture")
	}
	container := os.Getenv("URNETWORK_MIGRATION_TEST_PG_CONTAINER")
	proofPath, proofDigest := os.Getenv("URNETWORK_MIGRATION_TEST_PSQL_DIRECT_PROOF"), os.Getenv("URNETWORK_MIGRATION_TEST_PSQL_DIRECT_PROOF_SHA256")
	kind, err := migrationCensusNativeBackendKind(os.Getenv("URNETWORK_MIGRATION_TEST_PSQL_BACKEND"), container, proofPath, proofDigest)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &migrationCensusNativeBackend{container: container}
	if kind == "docker" {
		return fixture
	}
	if err := migrationCensusNativeDirectOwner(os.Getenv("WARP_ENV"), os.Getenv("WARP_TEST_ENV_PORTABLE_ROOT"), os.Getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES"),
		os.Getenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES"), os.Getenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY")); err != nil {
		t.Fatal(err)
	}
	mountRaw, err := migrationCensusNativeRead("/proc/self/mountinfo", 4*1024*1024)
	if err != nil {
		t.Fatal("direct native mount isolation is unavailable")
	}
	checkReadOnly := func(path string) {
		t.Helper()
		physical, err := filepath.EvalSymlinks(path)
		if err != nil || physical != path {
			t.Fatal("direct native evidence path is not physical")
		}
		if _, err := migrationCensusNativeMountAt(string(mountRaw), path); err != nil {
			t.Fatal(err)
		}
	}
	checkReadOnly(proofPath)
	raw, err := migrationCensusNativeRead(proofPath, 1024*1024)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != proofDigest {
		t.Fatal("direct native psql proof does not match its external pin")
	}
	proof, err := migrationCensusNativeProof(raw)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(proof.Rootfs)
	if err != nil || root != proof.Rootfs {
		t.Fatal("direct native image root is not physical")
	}
	direct := &migrationCensusNativeDirect{proof: proof, home: t.TempDir(), authority: os.Getenv("WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY")}
	for _, path := range []string{proof.Rootfs, proof.ImageReceipt, proof.ImageFiles, proof.Launcher} {
		checkReadOnly(path)
	}
	namespaceMount, err := migrationCensusNativeMountAt(string(mountRaw), proof.HostNetworkNamespace)
	selfNamespace, selfErr := os.Stat("/proc/self/ns/net")
	hostNamespace, hostErr := os.Stat(proof.HostNetworkNamespace)
	if err != nil || selfErr != nil || hostErr != nil || migrationCensusNativePrivateNamespace(namespaceMount, proof.HostNetworkNamespace, selfNamespace, hostNamespace) != nil {
		t.Fatal("direct native psql lacks a distinct physical private network namespace")
	}
	for _, pinned := range []struct{ path, digest string }{
		{path: proof.ImageReceipt, digest: proof.ImageReceiptSha256}, {path: proof.ImageFiles, digest: proof.ImageFilesSha256},
	} {
		if digest, err := migrationCensusNativeFileHash(pinned.path); err != nil || digest != pinned.digest {
			t.Fatal("direct native whole-image evidence does not match its pin")
		}
	}
	for relative, expected := range proof.Files {
		path, err := migrationCensusNativeImagePath(proof.Rootfs, relative)
		if err != nil || path != filepath.Join(proof.Rootfs, relative) {
			t.Fatal("direct native closure requires physical image files")
		}
		checkReadOnly(path)
		if digest, err := migrationCensusNativeFileHash(path); err != nil || digest != expected {
			t.Fatal("direct native executable or library does not match its pin")
		}
	}
	direct.loader, direct.psql = filepath.Join(proof.Rootfs, proof.Loader), filepath.Join(proof.Rootfs, proof.Psql)
	for _, executable := range []string{direct.loader, direct.psql, proof.Launcher} {
		info, err := os.Lstat(executable)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Fatal("direct native executable is not a physical executable file")
		}
	}
	for _, relative := range proof.LibraryDirectories {
		path, err := migrationCensusNativeImagePath(proof.Rootfs, relative)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatal("direct native library root is not a directory")
		}
		checkReadOnly(path)
		direct.libraries = append(direct.libraries, path)
	}
	launcher, err := migrationCensusNativeRead(proof.Launcher, 64*1024)
	if err != nil || string(launcher) != direct.launcher() || filepath.Base(proof.Launcher) != "psql" {
		t.Fatal("direct native launcher does not exec exactly the pinned loader and psql")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	loader := exec.CommandContext(ctx, direct.loader, "--inhibit-cache", "--library-path", strings.Join(direct.libraries, ":"), "--list", direct.psql)
	loader.Env, loader.Dir, loader.WaitDelay = direct.environment(), direct.home, 5*time.Second
	output, err := loader.Output()
	if err != nil {
		t.Fatal("direct native image loader did not close its actual startup dependencies")
	}
	paths, err := migrationCensusNativeLoaderPaths(string(output))
	if err != nil || direct.validateLibraries(paths, true) != nil {
		t.Fatal("direct native image loader resolved outside its complete pinned closure")
	}
	fixture.direct = direct
	return fixture
}
