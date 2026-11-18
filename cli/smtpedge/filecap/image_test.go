// The release check over a docker command line that answers from files: each platform's binary is read out of
// docker cp's archive and held to the helper's bytes, an image whose binary lacks them is refused naming the
// platform and what it holds, every container created is removed on every path, and a docker that fails or
// answers nonsense fails the check. A test writes the archives as containerd's differ and moby's docker cp did on
// 2026-11-18: one regular file under its base name, the attribute in a SCHILY.xattr PAX record.

package filecap

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// docker's ids for the two platforms' containers, as the fake answers them
var testContainerIds = map[string]string{
	"linux/amd64": strings.Repeat("a", 64),
	"linux/arm64": strings.Repeat("b", 64),
}

// An archive of name holding value as its security.capability record, or no record when value is nil.
func testArchive(t *testing.T, name string, value []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := []byte("#!/bin/false\n")
	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0755,
		Size:     int64(len(content)),
		ModTime:  time.Unix(1700000000, 0),
		Format:   tar.FormatPAX,
	}
	if value != nil {
		header.PAXRecords = map[string]string{paxXattrPrefix + AttributeName: string(value)}
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// A docker that logs each command line and answers from dir: create prints the platform's id, or fails when
// dir/create-fail exists, or prints nonsense when dir/create-junk does; cp writes dir/<id>.tar, or fails when
// dir/<id>.cp-fail exists; rm fails when dir/rm-fail exists.
func testDocker(t *testing.T) (*ImageCheckSettings, string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
dir='` + dir + `'
printf '%s\n' "$*" >> "$dir/log"
case "$1" in
create)
    if [ -e "$dir/create-fail" ]; then echo "manifest unknown" >&2; exit 1; fi
    if [ -e "$dir/create-junk" ]; then echo "Unable to find image locally"; exit 0; fi
    platform=""; previous=""
    for arg in "$@"; do
        if [ "$previous" = "--platform" ]; then platform="$arg"; fi
        previous="$arg"
    done
    case "$platform" in
    linux/amd64) echo ` + testContainerIds["linux/amd64"] + ` ;;
    linux/arm64) echo ` + testContainerIds["linux/arm64"] + ` ;;
    *) echo "no such platform" >&2; exit 1 ;;
    esac
    ;;
cp)
    id="${2%%:*}"
    if [ -e "$dir/$id.cp-fail" ]; then echo "Could not find the file" >&2; exit 1; fi
    cat "$dir/$id.tar"
    ;;
rm)
    if [ -e "$dir/rm-fail" ]; then echo "removal refused" >&2; exit 1; fi
    echo "$2"
    ;;
*)
    exit 2
    ;;
esac
`
	command := filepath.Join(dir, "docker")
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	settings := DefaultImageCheckSettings()
	settings.DockerCommand = command
	settings.CommandTimeout = time.Minute
	return settings, dir
}

func testWrite(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

// The command lines the fake ran, in order.
func testDockerLog(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

const testImage = "registry.example/review/main-smtpedge:2026.11.18-1"

func TestCheckImageReadsEachPlatformsBinary(t *testing.T) {
	settings, dir := testDocker(t)
	value, err := NetAdminFileCapability().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range testContainerIds {
		testWrite(t, filepath.Join(dir, id+".tar"), testArchive(t, "bringyour-smtpedge", value))
	}
	lines, err := CheckImage(context.Background(), testImage, settings)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"linux/amd64 /usr/local/sbin/bringyour-smtpedge cap_net_admin=ep",
		"linux/arm64 /usr/local/sbin/bringyour-smtpedge cap_net_admin=ep",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("answered %q", lines)
	}
	amd64, arm64 := testContainerIds["linux/amd64"], testContainerIds["linux/arm64"]
	wantLog := []string{
		"create --quiet --pull always --platform linux/amd64 " + testImage,
		"cp " + amd64 + ":/usr/local/sbin/bringyour-smtpedge -",
		"rm " + amd64,
		"create --quiet --pull always --platform linux/arm64 " + testImage,
		"cp " + arm64 + ":/usr/local/sbin/bringyour-smtpedge -",
		"rm " + arm64,
	}
	if got := testDockerLog(t, dir); !slices.Equal(got, wantLog) {
		t.Fatalf("docker ran %q, want %q", got, wantLog)
	}
}

// One platform's binary without the attribute, or with another, refuses the image and names it; the containers
// created are removed all the same.
func TestCheckImageRefusesABinaryWithoutTheCapability(t *testing.T) {
	namespaced, err := FileCapability{Revision: 3, Effective: true, Permitted: 1 << CapNetAdmin, RootId: 100000}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	good, err := NetAdminFileCapability().Encode()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		arm64 []byte
		say   string
	}{
		{arm64: nil, say: "linux/arm64 /usr/local/sbin/bringyour-smtpedge: no security.capability attribute"},
		{arm64: namespaced, say: "linux/arm64 /usr/local/sbin/bringyour-smtpedge: security.capability holds revision 3 cap_net_admin=ep [rootid=100000]"},
		{arm64: []byte{1, 0, 0, 2}, say: "linux/arm64 /usr/local/sbin/bringyour-smtpedge: security.capability does not decode"},
	}
	for _, c := range cases {
		settings, dir := testDocker(t)
		testWrite(t, filepath.Join(dir, testContainerIds["linux/amd64"]+".tar"), testArchive(t, "bringyour-smtpedge", good))
		testWrite(t, filepath.Join(dir, testContainerIds["linux/arm64"]+".tar"), testArchive(t, "bringyour-smtpedge", c.arm64))
		lines, err := CheckImage(context.Background(), testImage, settings)
		if err == nil || !strings.Contains(err.Error(), c.say) {
			t.Errorf("answered %v, want it to say %q", err, c.say)
		}
		if !slices.Equal(lines, []string{"linux/amd64 /usr/local/sbin/bringyour-smtpedge cap_net_admin=ep"}) {
			t.Errorf("answered lines %q", lines)
		}
		log := testDockerLog(t, dir)
		for _, id := range testContainerIds {
			if !slices.Contains(log, "rm "+id) {
				t.Errorf("container %s was not removed: %q", id, log)
			}
		}
	}
}

// A platform the image does not hold, a docker that fails, an id that is not one, an archive without the
// binary, or a container that cannot be removed fails the check, and nothing is created that is not removed.
func TestCheckImageFailsClosedOnDockerFailures(t *testing.T) {
	good, err := NetAdminFileCapability().Encode()
	if err != nil {
		t.Fatal(err)
	}
	amd64 := testContainerIds["linux/amd64"]
	cases := []struct {
		name   string
		files  map[string][]byte
		say    string
		wantRm bool
	}{
		{name: "create fails", files: map[string][]byte{"create-fail": nil}, say: "docker create: exit status 1: manifest unknown", wantRm: false},
		{name: "create answers no id", files: map[string][]byte{"create-junk": nil}, say: `docker create answered "Unable to find image locally", not a container id`, wantRm: false},
		{name: "cp fails", files: map[string][]byte{amd64 + ".cp-fail": nil}, say: "docker cp: exit status 1: Could not find the file", wantRm: true},
		{name: "no binary in the archive", files: map[string][]byte{amd64 + ".tar": testArchive(t, "bringyour-other", good)}, say: "the archive holds no file bringyour-smtpedge", wantRm: true},
		{name: "rm fails", files: map[string][]byte{amd64 + ".tar": testArchive(t, "bringyour-smtpedge", good), "rm-fail": nil}, say: "remove the check's container: docker rm: exit status 1: removal refused", wantRm: true},
	}
	for _, c := range cases {
		settings, dir := testDocker(t)
		for name, content := range c.files {
			testWrite(t, filepath.Join(dir, name), content)
		}
		lines, err := CheckImage(context.Background(), testImage, settings)
		if err == nil || !strings.Contains(err.Error(), "linux/amd64 /usr/local/sbin/bringyour-smtpedge: "+c.say) {
			t.Errorf("%s: answered %v, want it to say %q", c.name, err, c.say)
		}
		if len(lines) != 0 {
			t.Errorf("%s: answered lines %q", c.name, lines)
		}
		log := testDockerLog(t, dir)
		if removed := slices.Contains(log, "rm "+amd64); removed != c.wantRm {
			t.Errorf("%s: removed %t: %q", c.name, removed, log)
		}
		// the check stops at the first platform that fails
		if slices.ContainsFunc(log, func(line string) bool { return strings.Contains(line, "linux/arm64") }) {
			t.Errorf("%s: went on to arm64: %q", c.name, log)
		}
	}
	// a platform the image does not hold
	settings, dir := testDocker(t)
	settings.Platforms = []string{"linux/riscv64"}
	if _, err := CheckImage(context.Background(), testImage, settings); err == nil || !strings.Contains(err.Error(), "no such platform") {
		t.Errorf("an absent platform answered %v", err)
	}
	if log := testDockerLog(t, dir); len(log) != 1 {
		t.Errorf("an absent platform ran %q", log)
	}
	// nothing to check, or a binary path that is not one, checks nothing
	for _, change := range []func(*ImageCheckSettings){
		func(settings *ImageCheckSettings) { settings.Platforms = nil },
		func(settings *ImageCheckSettings) { settings.BinaryPath = "bringyour-smtpedge" },
	} {
		settings, dir := testDocker(t)
		change(settings)
		if _, err := CheckImage(context.Background(), testImage, settings); err == nil {
			t.Errorf("%+v passed", settings)
		}
		if log := testDockerLog(t, dir); len(log) != 0 {
			t.Errorf("%+v ran %q", settings, log)
		}
	}
}

// A docker command still running when the check's context ends is killed and fails the check, and the
// container is still removed. The fake's cp opens the fifo started for writing, which waits for the test to open
// it, then reads the fifo never, which nothing writes, so the test ends the context only once cp runs.
func TestCheckImageEndsACommandWithItsContext(t *testing.T) {
	settings, dir := testDocker(t)
	amd64 := testContainerIds["linux/amd64"]
	script, err := os.ReadFile(settings.DockerCommand)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(script), `    cat "$dir/$id.tar"`, `    exec 3>"$dir/started"; exec cat "$dir/never"`, 1)
	if changed == string(script) {
		t.Fatal("the fake's cp did not change")
	}
	testWrite(t, settings.DockerCommand, []byte(changed))
	if err := os.Chmod(settings.DockerCommand, 0700); err != nil {
		t.Fatal(err)
	}
	for _, fifo := range []string{"started", "never"} {
		if output, err := exec.Command("mkfifo", filepath.Join(dir, fifo)).CombinedOutput(); err != nil {
			t.Skipf("no fifo here: %v: %s", err, output)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// returns once cp has opened it
		started, err := os.Open(filepath.Join(dir, "started"))
		if err == nil {
			started.Close()
		}
		cancel()
	}()
	_, err = CheckImage(ctx, testImage, settings)
	if err == nil || !strings.Contains(err.Error(), "docker cp: signal: killed") {
		t.Fatalf("answered %v", err)
	}
	if log := testDockerLog(t, dir); !slices.Contains(log, "rm "+amd64) {
		t.Fatalf("the container was not removed: %q", log)
	}
}

// The entry's record, under its base name or ./ before it; another type of entry of that name, or another
// name, is not the binary.
func TestReadTarFileCapabilityFindsTheEntrysRecord(t *testing.T) {
	value, err := NetAdminFileCapability().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bringyour-smtpedge", "./bringyour-smtpedge"} {
		found, err := ReadTarFileCapability(bytes.NewReader(testArchive(t, name, value)), "bringyour-smtpedge")
		if err != nil || !bytes.Equal(found, value) {
			t.Errorf("%s: found %x, %v", name, found, err)
		}
	}
	if _, err := ReadTarFileCapability(bytes.NewReader(testArchive(t, "bringyour-smtpedge", nil)), "bringyour-smtpedge"); !errors.Is(err, ErrNoFileCapability) {
		t.Errorf("an entry without the record answered %v", err)
	}
	// a link of that name comes first and is passed over for the file
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "bringyour-smtpedge", Linkname: "elsewhere", Format: tar.FormatPAX,
		PAXRecords: map[string]string{paxXattrPrefix + AttributeName: string(value)}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTarFileCapability(bytes.NewReader(archive.Bytes()), "bringyour-smtpedge"); err == nil || errors.Is(err, ErrNoFileCapability) {
		t.Errorf("a link answered %v", err)
	}
	if _, err := ReadTarFileCapability(strings.NewReader("not an archive"), "bringyour-smtpedge"); err == nil {
		t.Error("nonsense passed")
	}
}
