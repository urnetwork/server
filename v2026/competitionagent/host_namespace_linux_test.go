// Namespace verification reads only synthetic private proc-shaped files.
// Real daemon/process/runtime qualification remains a separate native gate.
package competitionagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostNamespaceRequiresOneCanonicalDeclaredNonRootRange(t *testing.T) {
	for _, valid := range []string{"0 200000 65536\n", "         0     200000      65536\n"} {
		if err := validateHostNamespaceMap([]byte(valid), 200000, 65536); err != nil {
			t.Fatal("matching complete namespace range was refused", err)
		}
	}
	for _, invalid := range []string{"", "0 0 65536", "0 200000 65535", "0 0200000 65536", "0 200000 65536\n65536 400000 65536\n",
		"-1 200000 65536", "0 200000 65536 extra", "0 4294967295 65536", "0 200000 65536\n0 200000 65536"} {
		if err := validateHostNamespaceMap([]byte(invalid), 200000, 65536); err == nil {
			t.Error("ambiguous or unapproved namespace range was accepted")
		}
	}
}

func TestHostNamespaceCapturesExactCgroupAndKernelMapHashes(t *testing.T) {
	root := t.TempDir()
	expected := "/synthetic.slice/docker-" + strings.Repeat("c", 64) + ".scope"
	uidMap, gidMap := []byte("0 200000 65536\n"), []byte("0 300000 65536\n")
	for name, content := range map[string][]byte{"cgroup": []byte("0::" + expected + "\n"), "uid_map": uidMap, "gid_map": gidMap} {
		if err := os.WriteFile(filepath.Join(root, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan := HostFilesPlan{ContainerRootUid: 200000, ContainerRootGid: 300000, ContainerIdMapLength: 65536}
	mapping, err := readHostNamespace(t.Context(), root, expected, plan, DefaultHostFilesSettings())
	if err != nil || mapping.ContainerRootUid != 200000 || mapping.ContainerRootGid != 300000 || mapping.ContainerIdMapLength != 65536 ||
		mapping.UidMapSha256 != digestDockerBytes(uidMap) || mapping.GidMapSha256 != digestDockerBytes(gidMap) || mapping.CgroupPath != expected {
		t.Fatal("namespace proof lost exact scoped kernel bytes", err)
	}
	if _, err := readHostNamespace(t.Context(), root, expected+"-foreign", plan, DefaultHostFilesSettings()); err == nil {
		t.Fatal("another process cgroup was accepted as this container")
	}
	outside := filepath.Join(t.TempDir(), "map")
	if err := os.WriteFile(outside, uidMap, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "uid_map")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "uid_map")); err != nil {
		t.Fatal(err)
	}
	if _, err := readHostNamespace(t.Context(), root, expected, plan, DefaultHostFilesSettings()); err == nil {
		t.Fatal("namespace property followed a synthetic replacement symlink")
	}
}
