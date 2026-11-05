// These composed file-boundary tests use private regular files, the current
// test ELF and fake ownership/join facts. No host Docker/cgroup operation runs.
package competitionagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// All paths are owner-derived; tests cannot choose another broker root.
func newHostTrackedBuildTest(t *testing.T) *hostFilesTest {
	t.Helper()
	fixture := newHostFilesTest(t)
	for _, operation := range []string{"prepare-build-check-cache", "prepare-build-compile-cache", "prepare-build-output"} {
		if err := os.MkdirAll(filepath.Join(fixture.broker.plan.ContextRoot, hostBuildRelativePath(operation)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(fixture.broker.plan.ContextRoot, ".evidence-runtime", "tracked-build", "assembly"), 0700); err != nil {
		t.Fatal(err)
	}
	fixture.output = filepath.Join(fixture.broker.plan.ContextRoot, hostBuildRelativePath("prepare-build-output"))
	fixture.broker.buildJoined = func(context.Context) error { return nil }
	fixture.verify(t)
	return fixture
}

// A real Go ELF exercises the standard build-info parser without invoking a
// compiler or candidate executable from this test process.
func copyHostTrackedTestBinary(t *testing.T, destination string) []byte {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	if err := errors.Join(copyErr, output.Close()); err != nil {
		t.Fatal(err)
	}
	return hash.Sum(nil)
}

// No path, object identity or alternate operation enlarges the fixed role.
func TestHostTrackedBuildRejectsCallerSelectedTargets(t *testing.T) {
	fixture := newHostTrackedBuildTest(t)
	for _, operation := range []string{"prepare-build-check-cache", "prepare-build-compile-cache", "prepare-build-output", "reclaim-build-output"} {
		for _, mutate := range []func(*HostFilesRequest){func(request *HostFilesRequest) { request.Path = fixture.output },
			func(request *HostFilesRequest) { request.ContainerId = strings.Repeat("a", 64) },
			func(request *HostFilesRequest) { request.NetworkId = strings.Repeat("b", 64) },
			func(request *HostFilesRequest) { request.RunId = strings.Repeat("c", 64) }} {
			request := fixture.request(operation, "")
			mutate(&request)
			if result := fixture.broker.Execute(t.Context(), request); result.ExitCode == 0 || fixture.chowns != 0 {
				t.Fatal("caller-selected build authority reached an ownership mutation", operation, result)
			}
		}
	}
}

// A daemon census alone is insufficient: the independent kernel join refusal
// must precede both mapped ownership and binary receipt publication.
func TestHostTrackedBuildRequiresKernelJoinBeforeEveryFileHandoff(t *testing.T) {
	for _, operation := range []string{"prepare-build-check-cache", "prepare-build-compile-cache", "prepare-build-output", "reclaim-build-output"} {
		fixture := newHostTrackedBuildTest(t)
		fixture.broker.buildJoined = func(context.Context) error { return errors.New("synthetic retained compiler") }
		if result := fixture.broker.Execute(t.Context(), fixture.request(operation, "")); result.ExitCode == 0 ||
			result.ErrorCode != "build_process_join_unverified" || fixture.chowns != 0 || result.BuildBinary != nil || result.BuildCache != nil {
			t.Fatal("file handoff outran the compiler join", operation, result)
		}
	}
}

// Unknown and populated kernel replies cannot be confused with a completed
// compiler, even when another well-formed cgroup counter is present.
func TestHostTrackedBuildJoinRequiresOneExplicitEmptyPopulation(t *testing.T) {
	if err := validateBuildOutputJoin([]byte("populated 0\nfrozen 0\n")); err != nil {
		t.Fatal("finite empty kernel population was refused", err)
	}
	for _, reply := range []string{"", "frozen 1\n", "populated 1\nfrozen 0\n", "populated 0\npopulated 0\n",
		"populated 00\n", "populated -1\n", "populated 0 extra\n", "populated 0\nfrozen unknown\n"} {
		if err := validateBuildOutputJoin([]byte(reply)); err == nil {
			t.Fatal("unknown or live compiler acquired an empty join", reply)
		}
	}
}

// Independent initial receipts are equal, but a check mutation cannot alter
// the compilation inode or its initial trusted cache receipt.
func TestHostTrackedBuildCachesHaveIndependentIdenticalInitialInputs(t *testing.T) {
	fixture := newHostTrackedBuildTest(t)
	var receipts []*HostBuildCache
	var paths []string
	for _, operation := range []string{"prepare-build-check-cache", "prepare-build-compile-cache"} {
		path := filepath.Join(fixture.broker.plan.ContextRoot, hostBuildRelativePath(operation), "v0.0.0-synthetic.mod")
		if err := os.WriteFile(path, []byte("module synthetic.example/module\n"), 0600); err != nil {
			t.Fatal(err)
		}
		result := fixture.broker.Execute(t.Context(), fixture.request(operation, ""))
		if result.ExitCode != 0 || result.BuildCache == nil || result.BuildCache.Bytes == 0 || result.BuildCache.Files != 1 {
			t.Fatal("finite trusted cache did not cross its fixed handoff", operation, result)
		}
		receipts, paths = append(receipts, result.BuildCache), append(paths, path)
	}
	if receipts[0].Stage != "check" || receipts[1].Stage != "compile" || receipts[0].Sha256 != receipts[1].Sha256 {
		t.Fatal("separate stage inputs did not retain equal initial content", receipts)
	}
	if err := os.WriteFile(paths[0], []byte("candidate-owned check mutation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(paths[1])
	if err != nil || string(content) != "module synthetic.example/module\n" {
		t.Fatal("check writes reached the fresh compilation cache", err)
	}
}

// The finite metadata leaf cannot smuggle a special inode, shared hardlink,
// recursive directory, excessive bytes or excessive enumeration to root.
func TestHostTrackedBuildCachesRejectExpandedInodesAndBounds(t *testing.T) {
	for _, fault := range []string{"symlink", "hardlink", "directory", "bytes", "entries"} {
		fixture := newHostTrackedBuildTest(t)
		cache := filepath.Join(fixture.broker.plan.ContextRoot, hostBuildRelativePath("prepare-build-check-cache"))
		path := filepath.Join(cache, "v0.0.0-synthetic.mod")
		if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "symlink":
			if err := os.Symlink("v0.0.0-synthetic.mod", filepath.Join(cache, "alias")); err != nil {
				t.Fatal(err)
			}
		case "hardlink":
			if err := os.Link(path, filepath.Join(cache, "alias")); err != nil {
				t.Fatal(err)
			}
		case "directory":
			if err := os.Mkdir(filepath.Join(cache, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
		case "bytes":
			fixture.broker.settings.MaximumBuildCacheBytes = 1
		case "entries":
			fixture.broker.settings.MaximumBuildCacheEntries = 1
			if err := os.WriteFile(filepath.Join(cache, "second"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if result := fixture.broker.Execute(t.Context(), fixture.request("prepare-build-check-cache", "")); result.ExitCode == 0 || result.BuildCache != nil {
			t.Fatal("unsafe metadata acquired a successful cache receipt", fault, result)
		}
	}
}

// An actual displayed-directory substitution cannot redirect the cache read
// or root metadata mutation after its directory fd was captured.
func TestHostTrackedBuildCacheDirectorySubstitutionUsesHeldDescriptor(t *testing.T) {
	fixture := newHostTrackedBuildTest(t)
	cache := filepath.Join(fixture.broker.plan.ContextRoot, hostBuildRelativePath("prepare-build-check-cache"))
	outside := t.TempDir()
	for _, directory := range []string{cache, outside} {
		if err := os.WriteFile(filepath.Join(directory, "synthetic.mod"), []byte(directory), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.broker.beforeOpen = func(_ *os.File, _ string) {
		fixture.broker.beforeOpen = nil
		if err := os.Rename(cache, cache+"-held"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, cache); err != nil {
			t.Fatal(err)
		}
	}
	outsideInfo, err := os.Stat(filepath.Join(outside, "synthetic.mod"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.broker.chown = func(file *os.File, _, _ uint32) error {
		info, err := file.Stat()
		if err != nil || os.SameFile(info, outsideInfo) {
			t.Fatal("display path substitution reached outside metadata", err)
		}
		return nil
	}
	if result := fixture.broker.Execute(t.Context(), fixture.request("prepare-build-check-cache", "")); result.ExitCode != 0 || result.BuildCache == nil {
		t.Fatal("held cache directory did not survive display path substitution", result)
	}
	content, err := os.ReadFile(filepath.Join(outside, "synthetic.mod"))
	if err != nil || string(content) != outside {
		t.Fatal("outside cache content changed", err)
	}
}

// Only a new copied inode enters assembly. No candidate process or retained
// source reference can alter its bytes after the joined readback succeeds.
func TestHostTrackedBuildReclaimsOneBinaryIntoIndependentSealedAssembly(t *testing.T) {
	fixture := newHostTrackedBuildTest(t)
	original := filepath.Join(fixture.output, "sim-latency")
	want := copyHostTrackedTestBinary(t, original)
	result := fixture.broker.Execute(t.Context(), fixture.request("reclaim-build-output", ""))
	if result.ExitCode != 0 || result.BuildBinary == nil || result.BuildBinary.GoVersion == "" || result.BuildBinary.Sha256 != hex.EncodeToString(want) {
		t.Fatal("real regular Go ELF did not acquire its joined binary receipt", result)
	}
	assembled := filepath.Join(filepath.Dir(fixture.output), "assembly", "sim-latency")
	first, firstErr := os.Stat(original)
	second, secondErr := os.Stat(assembled)
	if firstErr != nil || secondErr != nil || os.SameFile(first, second) || second.Mode().Perm() != 0400 || result.BuildBinary.Bytes != second.Size() {
		t.Fatal("assembly retained an executable/shared candidate inode", firstErr, secondErr)
	}
	if duplicate := fixture.broker.Execute(t.Context(), fixture.request("reclaim-build-output", "")); duplicate.ExitCode == 0 {
		t.Fatal("duplicate handoff overwrote the sealed assembly inode")
	}
	if err := os.WriteFile(original, []byte("post-join source mutation"), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(assembled)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil || hex.EncodeToString(hash.Sum(nil)) != result.BuildBinary.Sha256 {
		t.Fatal("old candidate reference changed sealed assembly", err)
	}
}

// A leaf alias is rejected before parsing, copying or changing outside data.
func TestHostTrackedBuildRejectsBinarySymlinksAndExpandedOutput(t *testing.T) {
	for _, fault := range []string{"symlink", "hardlink", "directory", "extra", "size", "not-go"} {
		fixture := newHostTrackedBuildTest(t)
		path := filepath.Join(fixture.output, "sim-latency")
		switch fault {
		case "symlink":
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		case "directory":
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		default:
			if fault == "hardlink" {
				copyHostTrackedTestBinary(t, path)
			} else {
				if err := os.WriteFile(path, []byte("not an ELF"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "hardlink" {
				outside := filepath.Join(filepath.Dir(fixture.output), "shared-hardlink")
				if err := os.Link(path, outside); err != nil {
					t.Fatal(err)
				}
				originalInfo, originalErr := os.Stat(path)
				outsideInfo, outsideErr := os.Stat(outside)
				stat, ok := infoSysStat(originalInfo)
				if originalErr != nil || outsideErr != nil || !ok || stat.Nlink != 2 || !os.SameFile(originalInfo, outsideInfo) {
					t.Fatal("hardlink fixture did not establish one shared inode on its owned filesystem", originalErr, outsideErr)
				}
			}
			if fault == "extra" {
				if err := os.WriteFile(filepath.Join(fixture.output, "unexpected"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "size" {
				fixture.broker.settings.MaximumBuildBinaryBytes = 1
			}
		}
		if result := fixture.broker.Execute(t.Context(), fixture.request("reclaim-build-output", "")); result.ExitCode == 0 || result.BuildBinary != nil || fixture.chowns != 0 {
			t.Fatal("unsafe candidate output reached sealed assembly", fault, result)
		}
	}
}

// The source file is opened through the original output directory, not its
// display path, when the precise lookup barrier substitutes that whole path.
func TestHostTrackedBuildBinaryDirectorySubstitutionKeepsOriginalInode(t *testing.T) {
	fixture := newHostTrackedBuildTest(t)
	want := copyHostTrackedTestBinary(t, filepath.Join(fixture.output, "sim-latency"))
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sim-latency"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.broker.beforeOpen = func(_ *os.File, _ string) {
		fixture.broker.beforeOpen = nil
		if err := os.Rename(fixture.output, fixture.output+"-held"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, fixture.output); err != nil {
			t.Fatal(err)
		}
	}
	result := fixture.broker.Execute(t.Context(), fixture.request("reclaim-build-output", ""))
	if result.ExitCode != 0 || result.BuildBinary == nil || result.BuildBinary.Sha256 != hex.EncodeToString(want) {
		t.Fatal("binary reclaim followed the substituted display path", result)
	}
	content, err := os.ReadFile(filepath.Join(outside, "sim-latency"))
	if err != nil || string(content) != "outside" {
		t.Fatal("outside binary content changed", err)
	}
}
