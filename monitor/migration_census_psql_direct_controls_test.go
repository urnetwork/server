// Ordinary non-root controls validate the opt-in carrier without running psql,
// Docker, a loader, namespaces or a database. All evidence here is synthetic.
package monitor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Explicit direct selection never borrows a Docker identity or silently falls
// back when its proof is missing, malformed, relative or ambiguously supplied.
func TestMigrationCensusNativeDirectSelectionPreservesDockerAndFailsClosed(t *testing.T) {
	container, digest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, c := range []struct {
		backend, container, proof, digest, want string
	}{
		{container: container, want: "docker"},
		{backend: "docker", container: container, want: "docker"},
		{backend: "direct-owned", proof: "/synthetic/proof.json", digest: digest, want: "direct-owned"},
		{}, {container: "synthetic-container-name"},
		{container: container, proof: "/synthetic/proof.json", digest: digest},
		{backend: "direct-owned", container: container, proof: "/synthetic/proof.json", digest: digest},
		{backend: "direct-owned", proof: "synthetic/proof.json", digest: digest},
		{backend: "direct-owned", proof: "/synthetic/../proof.json", digest: digest},
		{backend: "direct-owned", proof: "/synthetic/proof.json"},
		{backend: "direct-owned", proof: "/synthetic/proof.json", digest: strings.ToUpper(digest)},
		{backend: "automatic", container: container},
	} {
		kind, err := migrationCensusNativeBackendKind(c.backend, c.container, c.proof, c.digest)
		if kind != c.want || (err == nil) != (c.want != "") {
			t.Fatal("backend selection changed its exact ownership boundary", c.backend, kind, c.want, err)
		}
	}
}

// Direct mode cannot accept a live environment, an incomplete portable escape,
// a hostname, IPv6, another port spelling or an arbitrary non-TestEnv database.
func TestMigrationCensusNativeDirectRequiresExactPortableOwnerAndDatabase(t *testing.T) {
	for _, c := range []struct {
		environment, root, use, allow, authority string
		want                                     bool
	}{
		{environment: "local", root: "/synthetic/portable", use: "1", allow: "1", authority: "127.0.0.1:15432", want: true},
		{environment: "main", root: "/synthetic/portable", use: "1", allow: "1", authority: "127.0.0.1:15432"},
		{environment: "local", use: "1", allow: "1", authority: "127.0.0.1:15432"},
		{environment: "local", root: "/synthetic/portable", allow: "1", authority: "127.0.0.1:15432"},
		{environment: "local", root: "/synthetic/portable", use: "1", authority: "127.0.0.1:15432"},
		{environment: "local", root: "/synthetic/portable", use: "1", allow: "1", authority: "localhost:15432"},
		{environment: "local", root: "/synthetic/portable", use: "1", allow: "1", authority: "127.0.0.1:015432"},
		{environment: "local", root: "/synthetic/portable", use: "1", allow: "1", authority: "[::1]:15432"},
	} {
		if err := migrationCensusNativeDirectOwner(c.environment, c.root, c.use, c.allow, c.authority); (err == nil) != c.want {
			t.Fatal("portable ownership boundary changed", c.environment, c.authority, err)
		}
	}
	database := "test_1_" + strings.Repeat("a", 32)
	for _, c := range []struct {
		host, database, authority string
		port                      uint16
		want                      int
	}{
		{host: "127.0.0.1", database: database, authority: "127.0.0.1:15432", port: 15432, want: 15432},
		{host: "127.0.0.1", database: database, authority: "127.0.0.1:15432", port: 5432},
		{host: "localhost", database: database, authority: "127.0.0.1:15432", port: 15432},
		{host: "192.0.2.40", database: database, authority: "127.0.0.1:15432", port: 15432},
		{host: "127.0.0.1", database: "synthetic_persistent_database", authority: "127.0.0.1:15432", port: 15432},
		{host: "127.0.0.1", database: database, authority: "127.0.0.1:015432", port: 15432},
	} {
		port, err := migrationCensusNativeDirectPort(c.host, c.port, c.database, c.authority)
		if port != c.want || (err == nil) != (c.want != 0) {
			t.Fatal("direct connection escaped its exact generated database and actual port", c.host, port, err)
		}
	}
}

// Proof metadata is pinned whole-image provenance plus a closed executable
// inventory; unknown, duplicate, trailing, missing and traversal inputs refuse.
func TestMigrationCensusNativeDirectProofIsStrictAndComplete(t *testing.T) {
	proof := migrationCensusNativeDirectProof{Schema: 1, Rootfs: "/synthetic/image", ImageReceipt: "/synthetic/proof/image.json", ImageReceiptSha256: strings.Repeat("a", 64),
		ImageFiles: "/synthetic/proof/FILES.sha256", ImageFilesSha256: strings.Repeat("b", 64), Loader: "lib/loader", Psql: "bin/psql",
		LibraryDirectories: []string{"lib"}, Files: map[string]string{"lib/loader": strings.Repeat("c", 64), "bin/psql": strings.Repeat("d", 64)},
		Launcher: "/synthetic/launcher/psql", HostNetworkNamespace: "/synthetic/host-netns"}
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := migrationCensusNativeProof(raw); err != nil || !reflect.DeepEqual(got, proof) {
		t.Fatal("complete synthetic proof was not parsed exactly", err)
	}
	for _, invalid := range []string{
		strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(string(raw), `"schema":1`, `"schema":2`, 1),
		strings.Replace(string(raw), `"schema":1`, `"schema":1,"unexpected":true`, 1),
		string(raw) + `{}`, strings.Replace(string(raw), `"library_directories":["lib"]`, `"library_directories":[]`, 1),
		strings.Replace(string(raw), `"library_directories":["lib"]`, `"library_directories":["../lib"]`, 1),
		strings.Replace(string(raw), `"library_directories":["lib"]`, `"library_directories":["lib","lib"]`, 1),
		strings.Replace(string(raw), `"loader":"lib/loader"`, `"loader":"../loader"`, 1),
		strings.Replace(string(raw), `"files":{`, `"files":{"bin/psql":"`+strings.Repeat("d", 64)+`",`, 1),
		strings.Replace(string(raw), strings.Repeat("a", 64), "not-a-digest", 1),
	} {
		if _, err := migrationCensusNativeProof([]byte(invalid)); err == nil {
			t.Fatal("malformed or incomplete synthetic proof was accepted")
		}
	}
}

// A kernel-reported nested writable mount defeats a read-only parent. A
// distinct namespace additionally needs its exact RO nsfs physical handle.
func TestMigrationCensusNativeDirectChecksEffectiveMountAndPhysicalNamespace(t *testing.T) {
	const mounts = "1 0 0:1 / / rw - overlay overlay rw\n2 1 0:2 / /synthetic ro - ext4 fixture rw\n3 2 0:3 / /synthetic/writable rw - tmpfs tmpfs rw\n4 2 0:4 net:[42] /synthetic/host-netns ro - nsfs nsfs rw\n"
	for _, c := range []struct {
		path string
		want bool
	}{
		{path: "/synthetic/image/psql", want: true}, {path: "/synthetic/host-netns", want: true},
		{path: "/synthetic/writable/psql"}, {path: "/synthetic-escape/psql"}, {path: "/host/psql"},
	} {
		if _, err := migrationCensusNativeMountAt(mounts, c.path); (err == nil) != c.want {
			t.Fatal("effective mount ownership changed", c.path, err)
		}
	}
	if _, err := migrationCensusNativeMountAt("malformed", "/synthetic"); err == nil {
		t.Fatal("malformed mount evidence was accepted")
	}
	directory := t.TempDir()
	infos := []os.FileInfo{}
	for _, name := range []string{"private", "host"} {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("synthetic namespace comparison only"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, info)
	}
	mount, err := migrationCensusNativeMountAt(mounts, "/synthetic/host-netns")
	if err != nil || migrationCensusNativePrivateNamespace(mount, mount.path, infos[0], infos[1]) != nil ||
		migrationCensusNativePrivateNamespace(mount, mount.path, infos[0], infos[0]) == nil {
		t.Fatal("physical namespace comparison lost equality or distinction", err)
	}
	mount.kind = "ext4"
	if migrationCensusNativePrivateNamespace(mount, mount.path, infos[0], infos[1]) == nil {
		t.Fatal("ordinary file was accepted as a physical namespace handle")
	}
}

// Real filesystem aliases are safe only when their canonical targets remain
// inside the pinned image. All files and hashes here are synthetic, not DSOs.
func TestMigrationCensusNativeDirectRejectsHostAliasesAndUnpinnedLibraries(t *testing.T) {
	root := t.TempDir()
	direct := &migrationCensusNativeDirect{proof: migrationCensusNativeDirectProof{Rootfs: root, Loader: "loader", Psql: "psql", Files: map[string]string{}}}
	for _, name := range []string{"loader", "psql", "library", "unlisted"} {
		body := []byte("synthetic image file " + name)
		if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "unlisted" {
			direct.proof.Files[name] = fmt.Sprintf("%x", sha256.Sum256(body))
		}
	}
	if err := os.Symlink("library", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "host-library")
	if err := os.WriteFile(outside, []byte("synthetic outside image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	path, err := migrationCensusNativeImagePath(root, "alias")
	if err != nil || path != filepath.Join(root, "library") {
		t.Fatal("in-image relative alias did not resolve to its physical member", err)
	}
	for _, relative := range []string{"escape", "../host-library", "/absolute", "./library"} {
		if _, err := migrationCensusNativeImagePath(root, relative); err == nil {
			t.Fatal("unsafe image alias was accepted", relative)
		}
	}
	if err := direct.validateLibraries([]string{filepath.Join(root, "loader"), filepath.Join(root, "alias")}, true); err != nil {
		t.Fatal("complete synthetic closure was rejected", err)
	}
	for _, paths := range [][]string{nil, {outside}, {filepath.Join(root, "unlisted")}, {filepath.Join(root, "library")}} {
		if err := direct.validateLibraries(paths, true); err == nil {
			t.Fatal("empty, host, unpinned or incomplete library closure was accepted")
		}
	}
	if direct.validateLibraries([]string{filepath.Join(root, "library")}, false) == nil ||
		direct.validateLibraries([]string{filepath.Join(root, "psql"), filepath.Join(root, "library")}, false) != nil {
		t.Fatal("live mapping proof did not require the actual pinned psql executable")
	}
	if digest, err := migrationCensusNativeFileHash(filepath.Join(root, "library")); err != nil || digest != direct.proof.Files["library"] {
		t.Fatal("physical synthetic library hash changed", err)
	}
	if _, err := migrationCensusNativeRead(filepath.Join(root, "library"), 2); err == nil {
		t.Fatal("bounded metadata read accepted oversized content")
	}
}

// Loader diagnostics are not executable identities; only complete absolute
// native resolutions survive parsing and the separate image-closure check.
func TestMigrationCensusNativeDirectLoaderOutputCannotHideMissingLibraries(t *testing.T) {
	paths, err := migrationCensusNativeLoaderPaths("linux-vdso.so.1 (0x1)\nlibnative.so => /synthetic/image/library (0x2)\n/synthetic/image/loader (0x3)\n")
	if err != nil || !reflect.DeepEqual(paths, []string{"/synthetic/image/library", "/synthetic/image/loader"}) {
		t.Fatal("complete synthetic native loader output was not parsed exactly", paths, err)
	}
	for _, output := range []string{"", "libnative.so => not found", "loader error", "linux-vdso.so.1 (0x1)\nextra diagnostic"} {
		if _, err := migrationCensusNativeLoaderPaths(output); err == nil {
			t.Fatal("missing or malformed loader output was accepted")
		}
	}
}

// Construction never starts a subprocess. Docker preserves its former argv;
// direct mode changes only the carrier and has no inherited credential knobs.
func TestMigrationCensusNativeDirectCarrierPreservesProductionArgvAndStdinProtocol(t *testing.T) {
	command := "IFS= read -r PGPASSWORD; export PGPASSWORD; export PGOPTIONS='-c statement_timeout=30000 -c default_transaction_read_only=on'; exec psql -h localhost -p 15432 -U synthetic_user test_1_" + strings.Repeat("a", 32) + " -X -q --csv -t -v ON_ERROR_STOP=1 -f -"
	container := strings.Repeat("a", 64)
	docker := (&migrationCensusNativeBackend{container: container}).command(t.Context(), command)
	if !reflect.DeepEqual(docker.Args, []string{"docker", "exec", "-i", container, "/bin/sh", "-c", command}) || docker.Env != nil {
		t.Fatal("default Docker carrier changed its original command or environment")
	}
	t.Setenv("PGPASSWORD", "synthetic-inherited-must-not-pass")
	t.Setenv("LD_PRELOAD", "synthetic-inherited-loader-must-not-pass")
	direct := &migrationCensusNativeDirect{proof: migrationCensusNativeDirectProof{Launcher: "/synthetic/launcher/psql"}, loader: "/synthetic/image/loader", psql: "/synthetic/image/psql", libraries: []string{"/synthetic/image/lib"}, home: t.TempDir()}
	child := (&migrationCensusNativeBackend{direct: direct}).command(t.Context(), command)
	if !reflect.DeepEqual(child.Args, []string{"/bin/sh", "-c", command}) || child.Dir != direct.home || child.WaitDelay != docker.WaitDelay ||
		strings.Contains(strings.Join(child.Env, "\n"), "synthetic-inherited") {
		t.Fatal("direct carrier changed production argv, join bound or inherited host secrets")
	}
	for _, required := range []string{"PATH=/synthetic/launcher", "HOME=" + direct.home, "PGHOSTADDR=127.0.0.1", "PGSSLMODE=disable", "PGPASSFILE=/nonexistent", "PGSERVICE="} {
		found := false
		for _, value := range child.Env {
			found = found || value == required
		}
		if !found {
			t.Fatal("direct child lost its exact private environment", required)
		}
	}
	if direct.launcher() != "#!/bin/sh\nexec '/synthetic/image/loader' --inhibit-cache --library-path '/synthetic/image/lib' '/synthetic/image/psql' \"$@\"\n" ||
		!strings.Contains(child.Args[len(child.Args)-1], "IFS= read -r PGPASSWORD;") || !strings.HasSuffix(child.Args[len(child.Args)-1], " -f -") {
		t.Fatal("launcher stopped execing the real pinned psql or changed file-reader/password custody")
	}
}
